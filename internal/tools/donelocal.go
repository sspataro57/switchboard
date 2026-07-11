package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// mark_done_local (agent-facing) and task_release (spine-facing — parking is
// the wrapper's business, un-choosing work is choosing).

type doneLocalArgs struct {
	TaskID   int64  `json:"task_id"`
	WorkerID string `json:"worker_id"`
	Summary  string `json:"summary,omitempty"`
}

func validateDoneLocal(args []byte) error {
	var a doneLocalArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID == 0 {
		return errors.New("missing task_id")
	}
	if a.WorkerID == "" {
		return errors.New("missing worker_id")
	}
	return nil
}

func markDoneLocal(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a doneLocalArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		claimID, err := activeClaim(ctx, tx, a.TaskID, a.WorkerID)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx,
			`UPDATE tasks SET status='done_locally', updated_at=now()
			 WHERE id=$1 AND status IN ('in_progress','claimed')`, a.TaskID)
		if err != nil {
			return fmt.Errorf("mark task %d done_locally: %w", a.TaskID, err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("task %d is not in progress; cannot mark done", a.TaskID)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE task_claims SET released_at=now() WHERE id=$1`, claimID); err != nil {
			return fmt.Errorf("release claim %d: %w", claimID, err)
		}
		_, err = insertTaskEvent(ctx, tx, a.TaskID, "done_local",
			map[string]any{"summary": a.Summary, "worker_id": a.WorkerID})
		return err
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"task_id": a.TaskID, "status": "done_locally"})
}

type releaseArgs struct {
	TaskID   int64  `json:"task_id"`
	WorkerID string `json:"worker_id"`
	Reason   string `json:"reason"`
}

func validateRelease(args []byte) error {
	var a releaseArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID == 0 {
		return errors.New("missing task_id")
	}
	if a.WorkerID == "" {
		return errors.New("missing worker_id")
	}
	if a.Reason == "" {
		return errors.New("missing reason")
	}
	return nil
}

// releaseTask puts a failed task back to ready on the SAME task (CLAUDE.md's
// retry shape — never a new one).
func releaseTask(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a releaseArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		claimID, err := activeClaim(ctx, tx, a.TaskID, a.WorkerID)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx,
			`UPDATE tasks SET status='ready', updated_at=now()
			 WHERE id=$1 AND status IN ('claimed','in_progress','needs_feedback')`, a.TaskID)
		if err != nil {
			return fmt.Errorf("release task %d: %w", a.TaskID, err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("task %d is not in a releasable status", a.TaskID)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE task_claims SET released_at=now() WHERE id=$1`, claimID); err != nil {
			return fmt.Errorf("release claim %d: %w", claimID, err)
		}
		_, err = insertTaskEvent(ctx, tx, a.TaskID, "released",
			map[string]any{"reason": a.Reason, "worker_id": a.WorkerID})
		return err
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"task_id": a.TaskID, "status": "ready"})
}
