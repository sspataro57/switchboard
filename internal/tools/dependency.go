package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Spine-facing dependency tools (SPEC 05-orchestrator-loop): agents must not
// gate their own dependencies, so none of these joins the MCP allowlist.

// depUnsatisfiedPredicate negates the SPEC's "dep satisfied" vocabulary
// (satisfied = done_locally | delivered | closed).
const depUnsatisfiedPredicate = `NOT IN ('done_locally','delivered','closed')`

type addDependencyArgs struct {
	TaskID          int64 `json:"task_id"`
	DependsOnTaskID int64 `json:"depends_on_task_id"`
}

func validateAddDependency(args []byte) error {
	var a addDependencyArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID == 0 {
		return errors.New("missing task_id")
	}
	if a.DependsOnTaskID == 0 {
		return errors.New("missing depends_on_task_id")
	}
	if a.TaskID == a.DependsOnTaskID {
		return errors.New("a task cannot depend on itself")
	}
	return nil
}

func addDependency(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a addDependencyArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	added := false
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM tasks WHERE id IN ($1, $2)`, a.TaskID, a.DependsOnTaskID).Scan(&n); err != nil {
			return fmt.Errorf("verify tasks exist: %w", err)
		}
		if n != 2 {
			return fmt.Errorf("task %d and/or %d not found", a.TaskID, a.DependsOnTaskID)
		}
		tag, err := tx.Exec(ctx,
			`INSERT INTO task_dependencies (task_id, depends_on_task_id)
			 VALUES ($1, $2) ON CONFLICT DO NOTHING`, a.TaskID, a.DependsOnTaskID)
		if err != nil {
			return fmt.Errorf("insert dependency: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil // already present — idempotent
		}
		added = true
		_, err = insertTaskEvent(ctx, tx, a.TaskID, "dependency_added",
			map[string]any{"depends_on_task_id": a.DependsOnTaskID})
		return err
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"added": added})
}

type blockArgs struct {
	TaskID int64  `json:"task_id"`
	Reason string `json:"reason,omitempty"`
}

func validateBlockUnblock(args []byte) error {
	var a blockArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID == 0 {
		return errors.New("missing task_id")
	}
	return nil
}

// blockTask flips ready -> blocked, re-verifying an unsatisfied dependency
// exists (the rule decides WHEN; the handler guarantees the transition is
// valid). Any other current status is an idempotent no-op — the orchestrator
// replays events and must never stall on a raced transition.
func blockTask(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a blockArgs
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
		if status != "ready" {
			return nil // only ready -> blocked; anything else is a no-op
		}
		var unmet bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM task_dependencies d
			   JOIN tasks dt ON dt.id = d.depends_on_task_id
			   WHERE d.task_id=$1 AND dt.status `+depUnsatisfiedPredicate+`)`, a.TaskID).Scan(&unmet); err != nil {
			return fmt.Errorf("verify unmet deps: %w", err)
		}
		if !unmet {
			return nil // defense in depth: no unmet dep, no block
		}
		if _, err := tx.Exec(ctx,
			`UPDATE tasks SET status='blocked', updated_at=now() WHERE id=$1`, a.TaskID); err != nil {
			return fmt.Errorf("block task %d: %w", a.TaskID, err)
		}
		if _, err := insertTaskEvent(ctx, tx, a.TaskID, "status_changed",
			map[string]any{"from": "ready", "to": "blocked", "rule": "dependency", "reason": a.Reason}); err != nil {
			return err
		}
		status = "blocked"
		return nil
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"task_id": a.TaskID, "status": status})
}

// unblockTask flips blocked -> ready, re-verifying ALL deps satisfied.
func unblockTask(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a blockArgs
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
		if status != "blocked" {
			return nil
		}
		var unmet bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM task_dependencies d
			   JOIN tasks dt ON dt.id = d.depends_on_task_id
			   WHERE d.task_id=$1 AND dt.status `+depUnsatisfiedPredicate+`)`, a.TaskID).Scan(&unmet); err != nil {
			return fmt.Errorf("verify deps satisfied: %w", err)
		}
		if unmet {
			return nil // a dep was added between snapshot and apply — stay blocked
		}
		if _, err := tx.Exec(ctx,
			`UPDATE tasks SET status='ready', updated_at=now() WHERE id=$1`, a.TaskID); err != nil {
			return fmt.Errorf("unblock task %d: %w", a.TaskID, err)
		}
		if _, err := insertTaskEvent(ctx, tx, a.TaskID, "status_changed",
			map[string]any{"from": "blocked", "to": "ready", "rule": "dependency", "reason": a.Reason}); err != nil {
			return err
		}
		status = "ready"
		return nil
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"task_id": a.TaskID, "status": status})
}
