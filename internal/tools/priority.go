package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// task_set_priority (SWT-38 C5): reorder any task. humanOnly in policy (C6):
// no spine caller writes priority after creation, so every automated caller —
// worker consoles and the orchestrator alike — is refused ("never choose your
// own work"). A future spine caller must move it to mcpHumanOnly deliberately.

// The priority scale (SWT-38 C7), spelled ONCE: triage's words
// (internal/triage/prompt.go), index = value. Higher runs first
// (taskQueueOrder). The MCP schema's minimum/maximum and its description's
// level names are asserted equal to these (internal/mcpserver
// TestTaskSetPrioritySchema).
const (
	PriorityMin = 0
	PriorityMax = 3
)

// PriorityLevels names each priority value: PriorityLevels[p] is p's name.
var PriorityLevels = []string{"normal", "elevated", "high", "urgent"}

// checkPriorityRange is the one range check both create_task and
// task_set_priority run.
func checkPriorityRange(p int) error {
	if p < PriorityMin || p > PriorityMax {
		return fmt.Errorf("priority %d: must be %d..%d (0 normal, 1 elevated, 2 high, 3 urgent)",
			p, PriorityMin, PriorityMax)
	}
	return nil
}

type setPriorityArgs struct {
	TaskID int64 `json:"task_id"`
	// A pointer: a missing (or null) priority is REFUSED, never read as 0 —
	// "swb prioritize 412" with a dropped argument must not demote 412.
	Priority *int   `json:"priority"`
	Reason   string `json:"reason,omitempty"`
}

func validateSetPriority(args []byte) error {
	var a setPriorityArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID <= 0 {
		return errors.New("missing or zero task_id")
	}
	if a.Priority == nil {
		return errors.New("missing priority")
	}
	return checkPriorityRange(*a.Priority)
}

// setPriority locks the task row, and either no-ops (the value is unchanged:
// the spine idempotence convention, no event) or updates priority + updated_at
// and writes one priority_changed {from,to,reason} event, atomically. It never
// touches status, a claim, plan_order or a delivery: priority is ordering only.
func setPriority(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a setPriorityArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	if a.Priority == nil {
		return nil, errors.New("missing priority")
	}
	to := *a.Priority

	var from int
	changed := false
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		from, changed, err = applyPriority(ctx, tx, a.TaskID, to, a.Reason)
		return err
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"task_id": a.TaskID, "from": from, "to": to, "changed": changed})
}

// priorityChangedEvent is the task_events type applyPriority writes; the
// `reviewed` event (requeue.go) reuses the word as its priority-changed flag.
const priorityChangedEvent = "priority_changed"

// applyPriority is the ONE spelling of the priority write (SWT-72 criterion
// 20), shared by task_set_priority and task_requeue: lock the row, no-op when
// the value is unchanged (no event), else update priority + updated_at and
// write one priority_changed {from,to,reason} event. Re-taking a row lock the
// transaction already holds is a no-op, so a caller under lockTask is fine.
func applyPriority(ctx context.Context, tx pgx.Tx, taskID int64, to int, reason string) (from int, changed bool, err error) {
	if err := tx.QueryRow(ctx,
		`SELECT priority FROM tasks WHERE id=$1 FOR UPDATE`, taskID).Scan(&from); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, fmt.Errorf("task %d not found", taskID)
		}
		return 0, false, fmt.Errorf("lock task %d: %w", taskID, err)
	}
	if from == to {
		return from, false, nil // idempotent: no update, no event
	}
	if _, err := tx.Exec(ctx,
		`UPDATE tasks SET priority=$2, updated_at=now() WHERE id=$1`, taskID, to); err != nil {
		return from, false, fmt.Errorf("set priority of task %d: %w", taskID, err)
	}
	if _, err := insertTaskEvent(ctx, tx, taskID, priorityChangedEvent,
		map[string]any{"from": from, "to": to, "reason": reason}); err != nil {
		return from, false, err
	}
	return from, true, nil
}
