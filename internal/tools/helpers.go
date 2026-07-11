package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// querier is satisfied by both *pgxpool.Pool and pgx.Tx so helpers work inside
// and outside transactions.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// insertTaskEvent writes one task_events row. The event-type vocabulary this
// step ships: claimed, status_changed, log, session, feedback_requested,
// feedback_answered, done_local, child_created, released.
func insertTaskEvent(ctx context.Context, q querier, taskID int64, eventType string, payload map[string]any) (int64, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshal %s payload: %w", eventType, err)
	}
	var id int64
	err = q.QueryRow(ctx,
		`INSERT INTO task_events (task_id, event_type, payload) VALUES ($1, $2, $3) RETURNING id`,
		taskID, eventType, raw).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert %s event: %w", eventType, err)
	}
	return id, nil
}

// activeClaim verifies worker_id holds the unreleased claim on the task and
// returns the claim id.
func activeClaim(ctx context.Context, q querier, taskID int64, workerID string) (int64, error) {
	var claimID int64
	err := q.QueryRow(ctx,
		`SELECT id FROM task_claims
		 WHERE task_id=$1 AND worker_id=$2 AND released_at IS NULL
		 ORDER BY id DESC LIMIT 1`,
		taskID, workerID).Scan(&claimID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("worker %q holds no active claim on task %d", workerID, taskID)
	}
	if err != nil {
		return 0, fmt.Errorf("look up claim for task %d: %w", taskID, err)
	}
	return claimID, nil
}

// inTx runs fn inside a transaction.
func inTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

func marshalResult(v any) (json.RawMessage, error) {
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal result: %w", err)
	}
	return out, nil
}
