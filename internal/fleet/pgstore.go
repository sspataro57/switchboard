package fleet

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgStore persists heartbeats in worker_heartbeats.
type pgStore struct {
	pool *pgxpool.Pool
}

func NewPGStore(pool *pgxpool.Pool) Store {
	return &pgStore{pool: pool}
}

func (s *pgStore) Existing(ctx context.Context, workerID string) (Heartbeat, bool, error) {
	hb := Heartbeat{WorkerID: workerID}
	var client *string
	err := s.pool.QueryRow(ctx,
		`SELECT client, state, task_id FROM worker_heartbeats WHERE worker_id=$1`,
		workerID).Scan(&client, &hb.State, &hb.TaskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Heartbeat{}, false, nil
	}
	if err != nil {
		return Heartbeat{}, false, fmt.Errorf("select heartbeat %s: %w", workerID, err)
	}
	if client != nil {
		hb.Client = *client
	}
	return hb, true, nil
}

func (s *pgStore) Upsert(ctx context.Context, hb Heartbeat) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO worker_heartbeats (worker_id, client, state, task_id, last_seen)
		 VALUES ($1, $2, $3, $4, now())
		 ON CONFLICT (worker_id) DO UPDATE SET
		   client=EXCLUDED.client, state=EXCLUDED.state,
		   task_id=EXCLUDED.task_id, last_seen=now()`,
		hb.WorkerID, hb.Client, hb.State, hb.TaskID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return fmt.Errorf("upsert heartbeat %s: %w", hb.WorkerID, ErrTaskFK)
		}
		return fmt.Errorf("upsert heartbeat %s: %w", hb.WorkerID, err)
	}
	return nil
}
