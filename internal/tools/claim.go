package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
)

// ClaimTTL is stamped on task_claims.expires_at. Enforcement (reaping expired
// claims) is the step-5 orchestrator's job; the constant lives here so it
// inherits from the claim code, not folklore.
const ClaimTTL = 2 * time.Hour

type claimArgs struct {
	TaskID   int64  `json:"task_id"`
	WorkerID string `json:"worker_id"`
}

func validateClaim(args []byte) error {
	var a claimArgs
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

// claimTask is the jobagent SKIP LOCKED idiom applied by-id: a concurrent
// claimer never blocks, it fails fast and re-peeks.
func claimTask(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a claimArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	var claimID int64
	var expiresAt time.Time
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		var id int64
		var assignee string
		err := tx.QueryRow(ctx,
			`SELECT id, assignee_type FROM tasks WHERE id = $1 AND status = 'ready' FOR UPDATE SKIP LOCKED`,
			a.TaskID).Scan(&id, &assignee)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("task %d cannot be claimed (not ready, already claimed, or locked)", a.TaskID)
		}
		if err != nil {
			return fmt.Errorf("lock task %d: %w", a.TaskID, err)
		}
		// SWT-38 (Codex review): task_get_next only routes claude tasks, but a
		// claim by id would let a worker console take a HUMAN task — including one
		// a user-scope session created from content read in another repo — and
		// load its body into a worker prompt. Over MCP, a non-human identity may
		// claim only claude tasks; human sessions (mcp:manual:…) and in-process
		// callers keep claiming anything, as before.
		if assignee != "claude" && executor.ViaMCP(ctx) && !policy.HumanActor(executor.ActorFrom(ctx)) {
			return fmt.Errorf("task %d is assigned to %s; a worker console claims only claude tasks (task_get_next routes them)",
				a.TaskID, assignee)
		}

		if _, err := tx.Exec(ctx,
			`UPDATE tasks SET status = 'claimed', updated_at = now() WHERE id = $1`, a.TaskID); err != nil {
			return fmt.Errorf("mark task %d claimed: %w", a.TaskID, err)
		}

		err = tx.QueryRow(ctx,
			`INSERT INTO task_claims (task_id, worker_id, expires_at)
			 VALUES ($1, $2, now() + $3::interval) RETURNING id, expires_at`,
			a.TaskID, a.WorkerID, ClaimTTL.String()).Scan(&claimID, &expiresAt)
		if err != nil {
			return fmt.Errorf("insert claim: %w", err)
		}

		_, err = insertTaskEvent(ctx, tx, a.TaskID, "claimed",
			map[string]any{"worker_id": a.WorkerID, "claim_id": claimID})
		return err
	})
	if err != nil {
		return nil, err
	}

	return marshalResult(map[string]any{
		"claim_id":   claimID,
		"task_id":    a.TaskID,
		"expires_at": expiresAt.Format(time.RFC3339),
	})
}
