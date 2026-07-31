package slackweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/textmatch"
)

type PGSink struct {
	pool *pgxpool.Pool
}

func NewSink(pool *pgxpool.Pool) *PGSink { return &PGSink{pool: pool} }

func (s *PGSink) EnsureAccount(ctx context.Context, workspace Workspace) (int64, error) {
	scopes := make([]string, 0, len(workspace.Conversations))
	for _, conversation := range workspace.Conversations {
		scopes = append(scopes, conversation.ID)
	}
	accountEmail := strings.ToLower(workspace.ID) + "@slack-web.local"
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO source_accounts
		   (provider, account_email, domain_default, scopes, send_enabled, calendar_in_availability)
		 VALUES ($1,$2,$3,$4,false,false)
		 ON CONFLICT (provider, account_email) DO UPDATE SET
		   domain_default=EXCLUDED.domain_default, scopes=EXCLUDED.scopes
		 RETURNING id`,
		Provider, accountEmail, workspace.URL, scopes).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert Slack source account %s: %w", workspace.ID, err)
	}
	return id, nil
}

func (s *PGSink) StartRun(ctx context.Context, accountID int64, startedAt time.Time) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO sync_runs (source_account_id, started_at, status, stats)
		 VALUES ($1, $2, 'running', '{"phase":"slack_web"}') RETURNING id`, accountID, startedAt).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert Slack sync run: %w", err)
	}
	return id, nil
}

func (s *PGSink) FinishRun(ctx context.Context, runID int64, status string, stats Stats, errMsg string) error {
	rawStats, err := json.Marshal(stats)
	if err != nil {
		return fmt.Errorf("marshal Slack sync stats: %w", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE sync_runs SET finished_at=now(), status=$2,
		        stats=stats || $3::jsonb, error=NULLIF($4,'') WHERE id=$1`,
		runID, status, rawStats, errMsg); err != nil {
		return fmt.Errorf("finish Slack sync run %d: %w", runID, err)
	}
	return nil
}

func (s *PGSink) RawHash(ctx context.Context, accountID int64, externalID string) (string, bool, error) {
	var hash string
	err := s.pool.QueryRow(ctx,
		`SELECT content_hash FROM raw_source_items WHERE source_account_id=$1 AND external_id=$2`,
		accountID, externalID).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("select Slack raw hash: %w", err)
	}
	return hash, true, nil
}

func (s *PGSink) InsertRaw(ctx context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1,$2,$3,$4)`, accountID, externalID, raw, hash); err != nil {
		return fmt.Errorf("insert Slack raw item: %w", err)
	}
	return nil
}

func (s *PGSink) UpdateRaw(ctx context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE raw_source_items SET raw_json=$3, content_hash=$4,
		        ingested_at=now(), normalized_at=NULL
		 WHERE source_account_id=$1 AND external_id=$2`,
		accountID, externalID, raw, hash); err != nil {
		return fmt.Errorf("update Slack raw item: %w", err)
	}
	return nil
}

type rawItem struct {
	id         int64
	externalID string
	raw        json.RawMessage
}

func (s *PGSink) pendingRaw(ctx context.Context, all bool) ([]rawItem, error) {
	query := `SELECT r.id, r.external_id, r.raw_json
	          FROM raw_source_items r
	          JOIN source_accounts a ON a.id=r.source_account_id
	          WHERE a.provider='slack_web'`
	if !all {
		query += ` AND r.normalized_at IS NULL`
	}
	query += ` ORDER BY r.external_id, r.id`
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("select Slack raw items: %w", err)
	}
	defer rows.Close()
	var items []rawItem
	for rows.Next() {
		var item rawItem
		if err := rows.Scan(&item.id, &item.externalID, &item.raw); err != nil {
			return nil, fmt.Errorf("scan Slack raw item: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Slack raw items: %w", err)
	}
	return items, nil
}

func (s *PGSink) upsertConversation(ctx context.Context, rawItemID int64, thread NormalizedThread) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO normalized_threads (raw_source_item_id, thread_key, subject, participants)
		 VALUES ($1,$2,$3,'[]')
		 ON CONFLICT (thread_key) WHERE thread_key IS NOT NULL DO UPDATE SET
		   raw_source_item_id=EXCLUDED.raw_source_item_id, subject=EXCLUDED.subject`,
		rawItemID, thread.ThreadKey, thread.Subject); err != nil {
		return fmt.Errorf("upsert Slack conversation %s: %w", thread.ThreadKey, err)
	}
	return nil
}

func (s *PGSink) upsertMessage(ctx context.Context, rawItemID int64, thread NormalizedThread, message NormalizedMessage) error {
	var threadID int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO normalized_threads (raw_source_item_id, thread_key, subject, participants)
		 VALUES ($1,$2,$3,'[]')
		 ON CONFLICT (thread_key) WHERE thread_key IS NOT NULL DO UPDATE SET
		   subject=EXCLUDED.subject
		 RETURNING id`, rawItemID, thread.ThreadKey, thread.Subject).Scan(&threadID)
	if err != nil {
		return fmt.Errorf("upsert Slack thread %s: %w", thread.ThreadKey, err)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		    body_text, subject, sender, channel)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		 ON CONFLICT (raw_source_item_id) DO UPDATE SET
		   thread_id=EXCLUDED.thread_id, direction=EXCLUDED.direction,
		   external_message_id=EXCLUDED.external_message_id, sent_at=EXCLUDED.sent_at,
		   body_text=EXCLUDED.body_text, subject=EXCLUDED.subject,
		   sender=EXCLUDED.sender, channel=EXCLUDED.channel`,
		rawItemID, threadID, message.Direction, message.ExternalMessageID, message.SentAt,
		message.BodyText, message.Subject, message.Sender, message.Channel); err != nil {
		return fmt.Errorf("upsert Slack message for raw item %d: %w", rawItemID, err)
	}
	if message.Direction == "outbound" {
		if err := s.confirmDelivery(ctx, message); err != nil {
			return err
		}
	}
	return nil
}

const slackMatchPrefixLen = 120

func (s *PGSink) confirmDelivery(ctx context.Context, message NormalizedMessage) error {
	if message.ExternalMessageID == "" || message.BodyText == "" || message.TargetRef == "" {
		return nil
	}
	// Refuse to claim a message another delivery already owns. The unique index
	// on (channel, sent_external_id) would otherwise turn a prefix collision into
	// a failed normalization run rather than a skipped confirmation.
	var claimed bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM deliveries
		   WHERE channel='slack_reply' AND sent_external_id=$1)`,
		message.ExternalMessageID).Scan(&claimed); err != nil {
		return fmt.Errorf("check Slack external id claim: %w", err)
	}
	if claimed {
		return nil
	}

	// 'sending' as well as 'sent' (SWT-12): a crash between the click and the
	// 'sent' write leaves a row switchboard cannot classify, and nothing is
	// allowed to retry it. Matching it here is what makes that window
	// self-healing — the message is in Slack, so the row becomes 'sent'.
	//
	// The time floor matters as much as the prefix. On target plus a 120-char
	// prefix alone, a replay (--all) would happily bind a months-old message — or
	// one a human typed by hand — to a newly stuck row and promote a send that
	// never happened. A message that predates the click cannot be that click's
	// message.
	//
	// The floor applies only where the instant is actually known. A switchboard
	// dispatch records send_attempted_at, so the comparison is sound. An assisted
	// row has no attempt instant, and created_at is NOT a safe substitute: the
	// row's creation says nothing about when a human typed the message, and using
	// it would refuse legitimate confirmations. Those rows keep the SWT-13
	// behaviour, so the historical-collision risk remains for the assisted tier
	// alone — bounded by the fact that nothing auto-promotes an assisted row's
	// status.
	//
	// The skew allowance is not decoration: send_attempted_at is Postgres now() on
	// pg-main, while message.SentAt comes from Slack's clock via the leaf. A
	// database even a second fast would refuse a legitimate confirmation
	// PERMANENTLY and flag the row instead — a silent failure with the same shape
	// as SWT-13's non-canonical target_ref landmine. Two minutes still kills the
	// months-old-replay collision this floor exists for.
	rows, err := s.pool.Query(ctx,
		// COALESCE because body is nullable (0001) and scanning NULL into a string
		// errors, which would fail the export rather than just this match (SWT-16).
		`SELECT id, task_id, COALESCE(body,'') FROM deliveries
		 WHERE channel='slack_reply' AND status IN ('sending','sent')
		   AND sent_external_id IS NULL
		   AND confirmed_at IS NULL AND target_ref=$1
		   AND (send_attempted_at IS NULL OR send_attempted_at - interval '2 minutes' <= $2)
		 ORDER BY id DESC`, message.TargetRef, message.SentAt)
	if err != nil {
		return fmt.Errorf("select Slack deliveries to confirm: %w", err)
	}
	defer rows.Close()
	var deliveryID, taskID int64
	matches := 0
	messagePrefix := normalizedPrefix(message.BodyText, slackMatchPrefixLen)
	for rows.Next() {
		var id, task int64
		var body string
		if err := rows.Scan(&id, &task, &body); err != nil {
			return fmt.Errorf("scan Slack delivery candidate: %w", err)
		}
		if normalizedPrefix(body, slackMatchPrefixLen) == messagePrefix {
			matches++
			if deliveryID == 0 {
				deliveryID, taskID = id, task
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate Slack delivery candidates: %w", err)
	}
	if deliveryID == 0 {
		return nil
	}
	if matches > 1 {
		// Two pending deliveries to the same conversation share a 120-char
		// prefix, which short repeated replies make realistic. Guessing would
		// stamp a real message onto the wrong row and mark the other unsent
		// forever; the reconciler flags both for a human instead.
		return nil
	}
	// Promote a crashed 'sending' row to 'sent' and backfill sent_at, while
	// leaving an already-'sent' row's timestamp alone. The guards are unchanged:
	// sent_external_id IS NULL means this never overwrites an id, so re-running
	// (including --all) is idempotent and emits no duplicate event.
	tag, err := s.pool.Exec(ctx,
		`UPDATE deliveries
		    SET sent_external_id=$2, confirmed_at=now(),
		        status='sent',
		        sent_at=COALESCE(sent_at, now()),
		        updated_at=now()
		 WHERE id=$1 AND sent_external_id IS NULL AND confirmed_at IS NULL`,
		deliveryID, message.ExternalMessageID)
	if err != nil {
		return fmt.Errorf("confirm Slack delivery %d: %w", deliveryID, err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	payload, _ := json.Marshal(map[string]any{
		"delivery_id": deliveryID, "matched_message_id": message.ExternalMessageID,
	})
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO task_events (task_id, event_type, payload)
		 VALUES ($1,'delivery_confirmed',$2)`, taskID, payload); err != nil {
		return fmt.Errorf("insert Slack delivery_confirmed event: %w", err)
	}
	return nil
}

// normalizedPrefix delegates to the shared spelling (SWT-16). Kept as a local
// alias so the call sites above read unchanged; the rule itself now lives in one
// place, because three copies of a comparison that must agree exactly is how a
// matcher silently stops matching.
func normalizedPrefix(value string, limit int) string {
	return textmatch.NormalizedPrefix(value, limit)
}

func (s *PGSink) markNormalized(ctx context.Context, rawItemID int64) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE raw_source_items SET normalized_at=now() WHERE id=$1`, rawItemID); err != nil {
		return fmt.Errorf("mark Slack raw item normalized: %w", err)
	}
	return nil
}

func Normalize(ctx context.Context, sink *PGSink, cfg Config) (Stats, error) {
	var stats Stats
	items, err := sink.pendingRaw(ctx, cfg.All)
	if err != nil {
		return stats, err
	}
	for _, item := range items {
		switch {
		case strings.HasPrefix(item.externalID, "conversation:"):
			thread, err := NormalizeConversation(item.raw)
			if err != nil {
				return stats, fmt.Errorf("normalize %s: %w", item.externalID, err)
			}
			if err := sink.upsertConversation(ctx, item.id, thread); err != nil {
				return stats, fmt.Errorf("apply %s: %w", item.externalID, err)
			}
		case strings.HasPrefix(item.externalID, "message:"):
			thread, message, err := NormalizeMessage(item.raw)
			if err != nil {
				return stats, fmt.Errorf("normalize %s: %w", item.externalID, err)
			}
			if err := sink.upsertMessage(ctx, item.id, thread, message); err != nil {
				return stats, fmt.Errorf("apply %s: %w", item.externalID, err)
			}
		default:
			return stats, fmt.Errorf("Slack raw item %d has unknown external_id %q", item.id, item.externalID)
		}
		if err := sink.markNormalized(ctx, item.id); err != nil {
			return stats, fmt.Errorf("stamp %s normalized: %w", item.externalID, err)
		}
		stats.Normalized++
	}
	return stats, nil
}
