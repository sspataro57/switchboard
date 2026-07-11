package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// task_close and record_orchestration — spine-facing (SPEC 05-orchestrator-loop).

type closeArgs struct {
	TaskID int64  `json:"task_id"`
	Reason string `json:"reason"`
}

func validateClose(args []byte) error {
	var a closeArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID == 0 {
		return errors.New("missing task_id")
	}
	if a.Reason == "" {
		return errors.New("missing reason")
	}
	return nil
}

// closeTask is the terminal verb: -> closed from holding | ready | blocked |
// done_locally | delivered. Refuses from claimed | in_progress |
// needs_feedback (never close work out from under a holder). Already-closed
// is an idempotent success so orchestrator replays never stall.
func closeTask(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a closeArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx,
			`SELECT status FROM tasks WHERE id=$1 FOR UPDATE`, a.TaskID).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("task %d not found", a.TaskID)
			}
			return fmt.Errorf("lock task %d: %w", a.TaskID, err)
		}
		switch status {
		case "closed":
			return nil // idempotent
		case "claimed", "in_progress", "needs_feedback":
			return fmt.Errorf("task %d is %s; refusing to close active work", a.TaskID, status)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE tasks SET status='closed', updated_at=now() WHERE id=$1`, a.TaskID); err != nil {
			return fmt.Errorf("close task %d: %w", a.TaskID, err)
		}
		_, err := insertTaskEvent(ctx, tx, a.TaskID, "status_changed",
			map[string]any{"from": status, "to": "closed", "reason": a.Reason})
		return err
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"task_id": a.TaskID, "status": "closed"})
}

type recordOrchestrationArgs struct {
	TaskID         int64          `json:"task_id"`
	Rule           string         `json:"rule"`
	TriggerEventID int64          `json:"trigger_event_id"`
	Payload        map[string]any `json:"payload,omitempty"`
}

func validateRecordOrchestration(args []byte) error {
	var a recordOrchestrationArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID == 0 {
		return errors.New("missing task_id")
	}
	if a.Rule == "" {
		return errors.New("missing rule")
	}
	return nil
}

// recordOrchestration writes the orchestrator's decision record: an
// 'orchestrated' task_events row on the triggering task. It doubles as the
// replay-dedup key and surfaces in task_context.
func recordOrchestration(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a recordOrchestrationArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM tasks WHERE id=$1)`, a.TaskID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("verify task %d: %w", a.TaskID, err)
	}
	if !exists {
		return nil, fmt.Errorf("task %d not found", a.TaskID)
	}

	payload := map[string]any{"rule": a.Rule, "trigger_event_id": a.TriggerEventID}
	for k, v := range a.Payload {
		payload[k] = v
	}
	eventID, err := insertTaskEvent(ctx, pool, a.TaskID, "orchestrated", payload)
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"event_id": eventID})
}
