package tools

// task_attach (SWT-74 D7): route a comm task onto the task it belongs with,
// and close the comm — the second half of "I'll route to the tasks or
// dismiss". humanOnly: it closes a task. One transaction, the LOWER task id
// locked first (two concurrent attaches in opposite directions cannot
// deadlock), in this order:
//
//  1. refuse task_id == target_task_id (validated too);
//  2. refuse a CLOSED target by name — task_reopen is the verb for that;
//  3. dedup on the (source, target) pair: attached:false, skipped
//     "already_attached" — a second, DIFFERENT target is legal;
//  4. on the TARGET one `log` row, IDS ONLY:
//     attached: task #<source> (message <M>)   (the message omitted when the
//     source has none). The NOTE never reaches the target: task_append_log is
//     pinned to human tasks on the user profile precisely because a claude
//     task's log feeds a worker prompt, and a note is words a session may have
//     composed from untrusted input — so the verb is safe on any target with
//     no assignee branch;
//  5. the SOURCE closes through closeTransition — the ONE writer of
//     the closed state, so the active-work refusal, the in-flight-send fence,
//     closed_at, closed_from_status, the session-state clear and SWT-72's
//     reviewed_at stamp all come for free — with reason
//     `routed to task #<target>: <note>`;
//  6. one `attached` event on the SOURCE, payload {target_task_id, note}.
//
// Not a dismissal (a routed comm was CORRECT, it just belongs elsewhere), and
// the target is NOT surfaced: he just looked at the comm and decided.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const attachNoteMax = 500

type attachArgs struct {
	TaskID       int64  `json:"task_id"`
	TargetTaskID int64  `json:"target_task_id"`
	Note         string `json:"note,omitempty"`
}

func validateAttach(args []byte) error {
	var a attachArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID <= 0 {
		return errors.New("missing or zero task_id")
	}
	if a.TargetTaskID <= 0 {
		return errors.New("missing or zero target_task_id")
	}
	if a.TaskID == a.TargetTaskID {
		return fmt.Errorf("task %d cannot be routed onto itself", a.TaskID)
	}
	if len([]rune(a.Note)) > attachNoteMax {
		return fmt.Errorf("note is %d characters; must be at most %d", len([]rune(a.Note)), attachNoteMax)
	}
	return nil
}

func attachTask(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a attachArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID == a.TargetTaskID {
		return nil, fmt.Errorf("task %d cannot be routed onto itself", a.TaskID)
	}
	note := strings.TrimSpace(a.Note)
	result := map[string]any{"task_id": a.TaskID, "target_task_id": a.TargetTaskID, "attached": false, "closed": false}
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		// Lock order: the lower id first.
		first, second := a.TaskID, a.TargetTaskID
		if second < first {
			first, second = second, first
		}
		statuses := map[int64]string{}
		for _, id := range []int64{first, second} {
			st, err := lockTask(ctx, tx, id)
			if err != nil {
				return err
			}
			statuses[id] = st
		}
		if statuses[a.TargetTaskID] == "closed" {
			return fmt.Errorf("target task %d is closed; routing live work onto a closed task is a mistake — "+
				"task_reopen is the verb for that", a.TargetTaskID)
		}
		// Dedup on the pair: the source's own `attached` events name the target.
		var already bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM task_events WHERE task_id = $1 AND event_type = 'attached'
			                 AND payload->'target_task_id' = to_jsonb($2::bigint))`, a.TaskID, a.TargetTaskID).Scan(&already); err != nil {
			return fmt.Errorf("check attach of %d onto %d: %w", a.TaskID, a.TargetTaskID, err)
		}
		if already {
			result["skipped"] = "already_attached"
			return nil
		}
		// The target's pointer, ids only.
		var messageID *int64
		if err := tx.QueryRow(ctx, `SELECT activity_by_message_id FROM tasks WHERE id = $1`, a.TaskID).Scan(&messageID); err != nil {
			return fmt.Errorf("read the source's activity message: %w", err)
		}
		pointer := fmt.Sprintf("attached: task #%d", a.TaskID)
		if messageID != nil {
			pointer = fmt.Sprintf("attached: task #%d (message %d)", a.TaskID, *messageID)
		}
		if _, err := insertTaskEvent(ctx, tx, a.TargetTaskID, "log", map[string]any{"kind": "log", "message": pointer}); err != nil {
			return err
		}
		// The source closes through the one writer of the closed state.
		reason := fmt.Sprintf("routed to task #%d", a.TargetTaskID)
		if note != "" {
			reason += ": " + note
		}
		_, transitioned, err := closeTransition(ctx, tx, a.TaskID, "closed", reason)
		if err != nil {
			return err
		}
		if _, err := insertTaskEvent(ctx, tx, a.TaskID, "attached",
			map[string]any{"target_task_id": a.TargetTaskID, "note": note}); err != nil {
			return err
		}
		result["attached"], result["closed"] = true, transitioned
		return nil
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(result)
}
