package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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

	eventID, err := insertTaskEvent(ctx, pool, a.TaskID, eventType, payload)
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"event_id": eventID})
}
