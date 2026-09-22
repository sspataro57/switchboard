package tools

// task_requeue (SWT-72 D6): the third review verb beside Dismiss and Done. A
// task that activity put into INCOMING (task_mark_activity) is reviewed by
// sending it back to the queue: reviewed_at = now(), a holding task lifted to
// ready, an optional new priority. humanOnly in policy — an interactive
// session (mcp:manual:salvo), the dashboard and opsctl pass; a worker console
// never chooses its own work and is refused.
//
// One transaction under lockTask, in this order:
//   1. a closed task is refused by name (task_reopen is the verb for that);
//   2. reviewed_at = now(), unconditional — a double-tap is a clean no-op;
//   3. holding -> ready, with the dependency.go status_changed spelling; the
//      ONLY transition this verb can make;
//   4. the priority, if given and different, through applyPriority (shared with
//      task_set_priority: one spelling of the write and its event). Omitted
//      means unchanged — a dropped argument must never demote a task;
//   5. one `reviewed` task_events row.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type requeueArgs struct {
	TaskID   int64  `json:"task_id"`
	Priority *int   `json:"priority"`
	Note     string `json:"note,omitempty"`
}

func validateRequeue(args []byte) error {
	var a requeueArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID <= 0 {
		return errors.New("missing or zero task_id")
	}
	if a.Priority != nil {
		return checkPriorityRange(*a.Priority)
	}
	return nil
}

func requeueTask(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a requeueArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	var (
		fromStatus, toStatus string
		prioFrom, prioTo     int
		prioChanged          bool
	)
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		status, err := lockTask(ctx, tx, a.TaskID)
		if err != nil {
			return err
		}
		if status == "closed" {
			return fmt.Errorf("task %d is closed; task_requeue reviews an open task — task_reopen is the verb for a closed one", a.TaskID)
		}
		fromStatus, toStatus = status, status

		var hadUnreviewed bool
		if err := tx.QueryRow(ctx,
			`SELECT activity_at IS NOT NULL AND (reviewed_at IS NULL OR activity_at > reviewed_at), priority
			   FROM tasks WHERE id=$1`, a.TaskID).Scan(&hadUnreviewed, &prioFrom); err != nil {
			return fmt.Errorf("read review state of task %d: %w", a.TaskID, err)
		}
		prioTo = prioFrom

		if _, err := tx.Exec(ctx, `UPDATE tasks SET reviewed_at = now() WHERE id = $1`, a.TaskID); err != nil {
			return fmt.Errorf("review task %d: %w", a.TaskID, err)
		}
		if status == "holding" {
			toStatus = "ready"
			if _, err := tx.Exec(ctx,
				`UPDATE tasks SET status='ready', updated_at=now() WHERE id=$1`, a.TaskID); err != nil {
				return fmt.Errorf("lift task %d to ready: %w", a.TaskID, err)
			}
			if _, err := insertTaskEvent(ctx, tx, a.TaskID, "status_changed",
				map[string]any{"from": "holding", "to": "ready", "rule": "requeue", "reason": a.Note}); err != nil {
				return err
			}
		}
		if a.Priority != nil {
			prioTo = *a.Priority
			if prioFrom, prioChanged, err = applyPriority(ctx, tx, a.TaskID, prioTo, a.Note); err != nil {
				return err
			}
		}
		_, err = insertTaskEvent(ctx, tx, a.TaskID, "reviewed", map[string]any{
			"note":                    a.Note,
			"from_status":             fromStatus,
			"to_status":               toStatus,
			"priority_from":           prioFrom,
			"priority_to":             prioTo,
			priorityChangedEvent:      prioChanged,
			"had_unreviewed_activity": hadUnreviewed,
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{
		"task_id":  a.TaskID,
		"status":   toStatus,
		"reviewed": true,
		"priority": map[string]any{"from": prioFrom, "to": prioTo, "changed": prioChanged},
	})
}
