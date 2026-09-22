package tools

// task_mark_activity (SWT-72 D3): records that an inbound message landed on an
// OPEN task as a log line — a Jira comment, a direct email, a Slack message —
// so the board can put the task in INCOMING for review. The human (or the
// interactive Claude session) clears it with task_requeue or by closing.
//
// SPINE-FACING, deliberately NOT in internal/mcpserver/schemas.go — the
// task_mark_surfaced shape. The MCP adapter passes arguments through (F7), so
// absence from every profile is the only boundary: a worker that could reach
// this tool could push any task onto the board with any inbound message. Not
// humanOnly either: capture (capture:{connector}) and the promoter
// (promote:{lane}) are its callers. No policy rule of its own; it falls through
// to the static allow-list.
//
// Deliberately a parallel pair to surfaced_at (D1): the Jira ticket-status
// reconciler reads that one and would hold every commented-on closed ticket's
// task open forever.
//
// Semantics, under the tasks row lock:
//   - a missing or non-inbound message is an ERROR (invariant 5 at the verb);
//   - a closed task is skipped (SWT-45's revive and SWT-36's reopen run AFTER
//     this call, so a closed ticket's task stays theirs);
//   - the same message twice is a no-op, so a replay never re-surfaces a row
//     the human already reviewed;
//   - otherwise activity_at = now() and activity_by_message_id = the message,
//     and NOTHING else moves: no event (the caller appended the log line one
//     statement earlier), no timestamp the board's `updated` cell reads.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type markActivityArgs struct {
	TaskID    int64  `json:"task_id"`
	MessageID int64  `json:"message_id"`
	Reason    string `json:"reason"`
}

func validateMarkActivity(args []byte) error {
	var a markActivityArgs
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

// Skip answers. A skip is a success with marked:false.
const (
	skipActivityClosed = "task_closed"
	skipAlreadyMarked  = "already_marked_by_message"
)

func markActivity(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a markActivityArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	result := map[string]any{"task_id": a.TaskID, "marked": false}
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
			return fmt.Errorf("message_id %d is %s; only an INBOUND message is activity on a task "+
				"(our own sends re-enter via ingestion — invariant 5)", a.MessageID, direction)
		}
		if status == "closed" {
			result["skipped"] = skipActivityClosed
			return nil
		}
		var current *int64
		if err := tx.QueryRow(ctx,
			`SELECT activity_by_message_id FROM tasks WHERE id=$1`, a.TaskID).Scan(&current); err != nil {
			return fmt.Errorf("read activity of task %d: %w", a.TaskID, err)
		}
		if current != nil && *current == a.MessageID {
			result["skipped"] = skipAlreadyMarked
			return nil
		}
		if _, err := tx.Exec(ctx,
			`UPDATE tasks SET activity_at = now(), activity_by_message_id = $2 WHERE id = $1`,
			a.TaskID, a.MessageID); err != nil {
			return fmt.Errorf("mark activity on task %d: %w", a.TaskID, err)
		}
		result["marked"] = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(result)
}
