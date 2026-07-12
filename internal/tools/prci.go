package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SWT-9 tools: link_external_ref (agent-facing), record_pr_event /
// record_ci_event / task_pr_transition (spine-facing — hooksd and the
// orchestrator are the callers; agents never move PR states themselves).

// ---- link_external_ref ---------------------------------------------------------

type linkExternalRefArgs struct {
	TaskID      int64  `json:"task_id"`
	System      string `json:"system"`
	ExternalKey string `json:"external_key"`
	ExternalURL string `json:"external_url,omitempty"`
}

func validateLinkExternalRef(args []byte) error {
	var a linkExternalRefArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID == 0 {
		return errors.New("missing task_id")
	}
	switch a.System {
	case "jira", "github", "upwork_crm":
	default:
		return fmt.Errorf("system %q: must be jira, github, or upwork_crm", a.System)
	}
	if a.ExternalKey == "" {
		return errors.New("missing external_key")
	}
	return nil
}

func linkExternalRef(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a linkExternalRefArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	tag, err := pool.Exec(ctx,
		`INSERT INTO external_refs (task_id, system, external_key, external_url)
		 VALUES ($1, $2, $3, NULLIF($4,''))
		 ON CONFLICT (task_id, system, external_key) DO NOTHING`,
		a.TaskID, a.System, a.ExternalKey, a.ExternalURL)
	if err != nil {
		return nil, fmt.Errorf("insert external ref: %w", err)
	}
	return marshalResult(map[string]any{"linked": tag.RowsAffected() > 0})
}

// ---- record_pr_event -------------------------------------------------------------

type recordPREventArgs struct {
	TaskID int64  `json:"task_id"`
	Action string `json:"action"` // opened | merged | closed
	PR     int64  `json:"pr"`
	URL    string `json:"url,omitempty"`
}

func validateRecordPREvent(args []byte) error {
	var a recordPREventArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID == 0 {
		return errors.New("missing task_id")
	}
	switch a.Action {
	case "opened", "merged", "closed":
	default:
		return fmt.Errorf("action %q: must be opened, merged, or closed", a.Action)
	}
	if a.PR == 0 {
		return errors.New("missing pr")
	}
	return nil
}

// recordPREvent inserts a pr_{action} task_event — idempotent on (task, pr,
// action): webhook redeliveries and poller overlap replay safely.
func recordPREvent(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a recordPREventArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	eventType := "pr_" + a.Action

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM task_events
		 WHERE task_id=$1 AND event_type=$2 AND (payload->>'pr')::bigint = $3)`,
		a.TaskID, eventType, a.PR).Scan(&exists); err != nil {
		return nil, fmt.Errorf("dedup check: %w", err)
	}
	if exists {
		return marshalResult(map[string]any{"recorded": false})
	}
	if _, err := insertTaskEvent(ctx, pool, a.TaskID, eventType,
		map[string]any{"pr": a.PR, "url": a.URL}); err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"recorded": true})
}

// ---- record_ci_event -------------------------------------------------------------

type recordCIEventArgs struct {
	TaskID     int64  `json:"task_id"`
	Phase      string `json:"phase"` // started | completed
	Conclusion string `json:"conclusion,omitempty"`
	RunID      int64  `json:"run_id"`
	RunURL     string `json:"run_url,omitempty"`
}

func validateRecordCIEvent(args []byte) error {
	var a recordCIEventArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID == 0 {
		return errors.New("missing task_id")
	}
	switch a.Phase {
	case "started", "completed":
	default:
		return fmt.Errorf("phase %q: must be started or completed", a.Phase)
	}
	if a.RunID == 0 {
		return errors.New("missing run_id")
	}
	return nil
}

// recordCIEvent maps phase+conclusion → ci_started | ci_passed | ci_failed
// task_events — idempotent on (task, run_id, resulting event type).
func recordCIEvent(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a recordCIEventArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	eventType := "ci_started"
	if a.Phase == "completed" {
		if a.Conclusion == "success" {
			eventType = "ci_passed"
		} else {
			eventType = "ci_failed"
		}
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM task_events
		 WHERE task_id=$1 AND event_type=$2 AND (payload->>'run_id')::bigint = $3)`,
		a.TaskID, eventType, a.RunID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("dedup check: %w", err)
	}
	if exists {
		return marshalResult(map[string]any{"recorded": false})
	}
	if _, err := insertTaskEvent(ctx, pool, a.TaskID, eventType,
		map[string]any{"run_id": a.RunID, "run_url": a.RunURL, "conclusion": a.Conclusion}); err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"recorded": true, "event_type": eventType})
}

// ---- task_pr_transition ------------------------------------------------------------

type prTransitionArgs struct {
	TaskID  int64  `json:"task_id"`
	To      string `json:"to"`
	Summary string `json:"summary,omitempty"`
}

// prTransitionLegal maps target statuses to their legal sources.
var prTransitionLegal = map[string][]string{
	"pr_open":        {"ready", "claimed", "in_progress"},
	"awaiting_ci":    {"pr_open", "awaiting_merge"},
	"awaiting_merge": {"awaiting_ci", "pr_open"},
	"done_locally":   {"awaiting_merge", "awaiting_ci", "pr_open"},
	"ready":          {"pr_open", "awaiting_ci", "awaiting_merge"},
}

func validatePRTransition(args []byte) error {
	var a prTransitionArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID == 0 {
		return errors.New("missing task_id")
	}
	if _, ok := prTransitionLegal[a.To]; !ok {
		return fmt.Errorf("to %q: not a PR-lifecycle target status", a.To)
	}
	return nil
}

// taskPRTransition moves a task along the PR half of the status machine.
// Same-status is an idempotent no-op (orchestrator replay discipline);
// to=done_locally emits a done_local event so R3 (Deliver task) chains.
func taskPRTransition(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a prTransitionArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	status := ""
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT status FROM tasks WHERE id=$1 FOR UPDATE`, a.TaskID).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("task %d not found", a.TaskID)
			}
			return fmt.Errorf("lock task %d: %w", a.TaskID, err)
		}
		if status == a.To {
			return nil // idempotent replay
		}
		legal := false
		for _, from := range prTransitionLegal[a.To] {
			if status == from {
				legal = true
				break
			}
		}
		if !legal {
			return fmt.Errorf("task %d is %s; cannot transition to %s", a.TaskID, status, a.To)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE tasks SET status=$2, updated_at=now() WHERE id=$1`, a.TaskID, a.To); err != nil {
			return fmt.Errorf("transition task %d: %w", a.TaskID, err)
		}
		if a.To == "done_locally" {
			// done_local so R3 (Deliver task) chains exactly as a worker finish.
			if _, err := insertTaskEvent(ctx, tx, a.TaskID, "done_local",
				map[string]any{"summary": a.Summary, "via": "pr_transition"}); err != nil {
				return err
			}
		} else {
			if _, err := insertTaskEvent(ctx, tx, a.TaskID, "status_changed",
				map[string]any{"from": status, "to": a.To, "reason": a.Summary}); err != nil {
				return err
			}
		}
		status = a.To
		return nil
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"task_id": a.TaskID, "status": status})
}
