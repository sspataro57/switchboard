package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// task_append_log — append a task_events row, no status change. kind "session"
// is the wrapper's dedicated event after every claude -p run: the event_type
// becomes 'session' and a JSON-object message is merged into the payload so
// resume can read payload->>'session_id'. Everything still flows through the
// executor — no new tool name for what is one INSERT with a different tag.

type appendLogArgs struct {
	TaskID   int64  `json:"task_id"`
	Message  string `json:"message"`
	Kind     string `json:"kind,omitempty"`
	WorkerID string `json:"worker_id,omitempty"`
	// RequireAssigneeType is the SWT-38 profile pin (C4): the user-scope MCP
	// adapter force-sets it to "human", so a session in another repo can log
	// only on Salvador's-lane tasks. A log line on a claude task is text inside
	// a future worker prompt (task_context returns recent event payloads).
	// Handler-enforced, under the task row lock, atomically with the insert.
	// Absent from every MCP schema; it only narrows.
	RequireAssigneeType string `json:"require_assignee_type,omitempty"`
}

func validateAppendLog(args []byte) error {
	var a appendLogArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID == 0 {
		return errors.New("missing task_id")
	}
	if a.Message == "" {
		return errors.New("missing message")
	}
	return nil
}

func appendLog(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a appendLogArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	if a.Kind == "" {
		a.Kind = "log"
	}

	eventType := "log"
	payload := map[string]any{"message": a.Message, "kind": a.Kind, "worker_id": a.WorkerID}
	if a.Kind == "session" {
		eventType = "session"
		var sessionFields map[string]any
		if err := json.Unmarshal([]byte(a.Message), &sessionFields); err == nil {
			for k, v := range sessionFields {
				payload[k] = v
			}
		}
	}

	if a.RequireAssigneeType == "" {
		eventID, err := insertTaskEvent(ctx, pool, a.TaskID, eventType, payload)
		if err != nil {
			return nil, err
		}
		return marshalResult(map[string]any{"event_id": eventID})
	}

	// SWT-38 C4: check-then-insert in ONE transaction, the check holding a
	// SHARE lock on the task row so the assignee cannot change between them.
	var eventID int64
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		var assignee string
		if err := tx.QueryRow(ctx,
			`SELECT assignee_type FROM tasks WHERE id=$1 FOR SHARE`, a.TaskID).Scan(&assignee); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("task %d not found", a.TaskID)
			}
			return fmt.Errorf("read task %d: %w", a.TaskID, err)
		}
		if assignee != a.RequireAssigneeType {
			return fmt.Errorf("task %d is assigned to %s; logging on it from this session is refused", a.TaskID, assignee)
		}
		id, err := insertTaskEvent(ctx, tx, a.TaskID, eventType, payload)
		eventID = id
		return err
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"event_id": eventID})
}
