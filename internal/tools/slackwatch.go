package tools

// The Slack watch list (SWT-75, slack-watch-sweep D2): which conversations the
// resident watcher reads about every minute, as DATA in `slack_watch`. Three
// tools are the table's ONLY writers and readers-through-the-executor:
//
//	slack_watch_add         {workspace_id, conversation_id, label?}
//	slack_watch_set_enabled {id, enabled}
//	slack_watch_list        {enabled?}
//
// All three are policy.humanOnly and deliberately NOT in
// internal/mcpserver/schemas.go — the capture_rule_add shape, for the same
// reason: an agent must not be able to point the shared browser at a
// conversation of its choosing, and browser time on the Mac mini is the
// scarcest resource in this system. opsctl (`opsctl slack-watch …`) and the
// /sources panel are the human surfaces.
//
// The id shapes are NOT validated here beyond presence: migration 0041's two
// CHECKs are the leaf's own rules, and a Go copy could drift from them. A bad
// id fails in Postgres, loudly, with the constraint's name.
//
// A row is turned OFF, never deleted (the capture_rules rule): the /sources
// "last read" column joins sync_runs to a row that must still exist.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type slackWatchAddArgs struct {
	WorkspaceID    string `json:"workspace_id"`
	ConversationID string `json:"conversation_id"`
	Label          string `json:"label,omitempty"`
}

func validateSlackWatchAdd(args []byte) error {
	var a slackWatchAddArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.WorkspaceID == "" {
		return errors.New("missing workspace_id")
	}
	if a.ConversationID == "" {
		return errors.New("missing conversation_id")
	}
	return nil
}

// slackWatchAdd upserts on (workspace_id, conversation_id): a repeated add
// updates the label and RE-ENABLES the row rather than erroring, so a re-run
// seed command succeeds and a paused watch is resumed with the same command
// that created it (the owner works the board concurrently).
func slackWatchAdd(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a slackWatchAddArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	var (
		id      int64
		label   string
		enabled bool
	)
	if err := pool.QueryRow(ctx,
		`INSERT INTO slack_watch (workspace_id, conversation_id, label)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (workspace_id, conversation_id) DO UPDATE
		   SET label = EXCLUDED.label, enabled = true, updated_at = now()
		 RETURNING id, label, enabled`,
		a.WorkspaceID, a.ConversationID, a.Label).Scan(&id, &label, &enabled); err != nil {
		return nil, fmt.Errorf("add slack watch %s/%s: %w", a.WorkspaceID, a.ConversationID, err)
	}
	return marshalResult(map[string]any{
		"id": id, "workspace_id": a.WorkspaceID, "conversation_id": a.ConversationID,
		"label": label, "enabled": enabled,
	})
}

type slackWatchSetEnabledArgs struct {
	ID int64 `json:"id"`
	// A pointer: an omitted or null flag is refused, never read as false —
	// capture_rule_set_enabled's rule.
	Enabled *bool `json:"enabled"`
}

func validateSlackWatchSetEnabled(args []byte) error {
	var a slackWatchSetEnabledArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.ID <= 0 {
		return errors.New("missing or zero id")
	}
	if a.Enabled == nil {
		return errors.New("missing enabled")
	}
	return nil
}

// slackWatchSetEnabled is idempotent: setting the value a row already has is a
// success that changes nothing (updated_at moves only on a real change).
func slackWatchSetEnabled(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a slackWatchSetEnabledArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	var enabled bool
	err := pool.QueryRow(ctx,
		`UPDATE slack_watch
		    SET enabled = $2,
		        updated_at = CASE WHEN enabled = $2 THEN updated_at ELSE now() END
		  WHERE id = $1 RETURNING enabled`, a.ID, *a.Enabled).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("slack watch row %d not found", a.ID)
	}
	if err != nil {
		return nil, fmt.Errorf("set slack watch %d enabled=%t: %w", a.ID, *a.Enabled, err)
	}
	return marshalResult(map[string]any{"id": a.ID, "enabled": enabled})
}

type slackWatchListArgs struct {
	// A pointer: omitted means no filter (the /sources and opsctl call).
	Enabled *bool `json:"enabled"`
}

func validateSlackWatchList(args []byte) error {
	var a slackWatchListArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	return nil
}

// slackWatchList reads every row, with `last_read_at` — the start of the
// latest completed ROTATION run (phase slack_web) whose stats.read lists the
// conversation — from sync_runs, not from a column slack_watch would have to
// maintain. Watch-phase runs are excluded on purpose: they read the row every
// minute, and "last read by the rotation" is the number the rotation's own
// ordering uses (SWT-75 D6, criterion 19).
func slackWatchList(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a slackWatchListArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	rows, err := pool.Query(ctx, `
		WITH last_read AS (
		  SELECT lower(a.account_email) AS account_email, rd.conversation_id, max(r.started_at) AS at
		    FROM sync_runs r
		    JOIN source_accounts a ON a.id = r.source_account_id AND a.provider = 'slack_web'
		    CROSS JOIN LATERAL jsonb_array_elements_text(r.stats->'read') AS rd(conversation_id)
		   WHERE r.status IN ('ok','partial') AND COALESCE(r.stats->>'phase','slack_web') = 'slack_web'
		     AND jsonb_typeof(r.stats->'read') = 'array'
		     AND r.started_at > now() - interval '30 days'
		   GROUP BY 1, 2
		)
		SELECT w.id, w.workspace_id, w.conversation_id, w.label, w.enabled, lr.at
		  FROM slack_watch w
		  LEFT JOIN last_read lr ON lr.account_email = lower(w.workspace_id) || '@slack-web.local'
		                        AND lr.conversation_id = w.conversation_id
		 WHERE $1::boolean IS NULL OR w.enabled = $1
		 ORDER BY w.workspace_id, w.conversation_id`, a.Enabled)
	if err != nil {
		return nil, fmt.Errorf("list slack watch: %w", err)
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var (
			id              int64
			ws, conv, label string
			enabled         bool
			at              *time.Time
		)
		if err := rows.Scan(&id, &ws, &conv, &label, &enabled, &at); err != nil {
			return nil, fmt.Errorf("scan slack watch row: %w", err)
		}
		row := map[string]any{"id": id, "workspace_id": ws, "conversation_id": conv,
			"label": label, "enabled": enabled, "last_read_at": nil}
		if at != nil {
			row["last_read_at"] = at.UTC().Format(time.RFC3339)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read slack watch rows: %w", err)
	}
	return marshalResult(map[string]any{"rows": out})
}
