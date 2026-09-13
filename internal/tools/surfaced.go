package tools

// task_mark_surfaced (SWT-45 J7): records that a task CREATED by an overriding
// capture rule was put on the board by activity, so the Jira reconciler holds
// it open instead of closing a done ticket's brand-new task in the same tick.
//
// SPINE-FACING, deliberately NOT in internal/mcpserver/schemas.go — the
// task_set_source_thread shape. The MCP adapter passes arguments through (F7),
// so absence from every profile is the only boundary: a worker that could reach
// this tool could pin any task open against the reconciler. It is NOT
// humanOnly either: capture (capture:{connector}) is its caller. No policy rule
// of its own; it falls through to the static allow-list.
//
// Why a tool and not a create_task argument: F7 again — a create_task argument
// is settable by any MCP caller, schema or not.
//
// Semantics, under the tasks row lock:
//   - a missing or non-inbound message is an ERROR (invariant 5 at the verb);
//   - a closed task is skipped (the crash window between creation and this
//     call degrades to today's behaviour: the next overriding email revives it);
//   - the same message twice is a no-op, so surfaced_at never moves on a replay
//     (a moved instant would read as a NEW surfacing to the reconciler);
//   - otherwise surfaced_at = now() and surfaced_by_message_id = the message.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type markSurfacedArgs struct {
	TaskID    int64  `json:"task_id"`
	MessageID int64  `json:"message_id"`
	Reason    string `json:"reason"`
}

func validateMarkSurfaced(args []byte) error {
	var a markSurfacedArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID <= 0 {
		return errors.New("missing or zero task_id")
	}
	if a.MessageID <= 0 {
		return fmt.Errorf("message_id must be > 0 (got %d)", a.MessageID)
	}
	if a.Reason == "" {
		return errors.New("missing reason")
	}
	return nil
}

// Skip answers. A skip is a success with surfaced:false.
const (
	skipSurfacedClosed  = "task_closed"
	skipAlreadySurfaced = "already_surfaced_by_message"
)

func markSurfaced(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a markSurfacedArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	result := map[string]any{"task_id": a.TaskID, "surfaced": false}
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		status, err := lockTask(ctx, tx, a.TaskID)
		if err != nil {
			return err
		}
		var direction string
		if err := tx.QueryRow(ctx,
			`SELECT direction FROM normalized_messages WHERE id=$1`, a.MessageID).Scan(&direction); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("message_id %d: no such normalized message", a.MessageID)
			}
			return fmt.Errorf("read message %d: %w", a.MessageID, err)
		}
		if direction != "inbound" {
			return fmt.Errorf("message_id %d is %s; only an INBOUND message can surface a task "+
				"(our own sends re-enter via ingestion — invariant 5)", a.MessageID, direction)
		}
		if status == "closed" {
			result["skipped"] = skipSurfacedClosed
			return nil
		}
		var current *int64
		if err := tx.QueryRow(ctx,
			`SELECT surfaced_by_message_id FROM tasks WHERE id=$1`, a.TaskID).Scan(&current); err != nil {
			return fmt.Errorf("read surfacing of task %d: %w", a.TaskID, err)
		}
		if current != nil && *current == a.MessageID {
			result["skipped"] = skipAlreadySurfaced
			return nil
		}
		if _, err := tx.Exec(ctx,
			`UPDATE tasks SET surfaced_at = now(), surfaced_by_message_id = $2 WHERE id = $1`,
			a.TaskID, a.MessageID); err != nil {
			return fmt.Errorf("surface task %d: %w", a.TaskID, err)
		}
		result["surfaced"] = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(result)
}
