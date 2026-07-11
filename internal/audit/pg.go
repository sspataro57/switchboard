package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// pgStore writes audit_events and policy_decisions rows.
type pgStore struct {
	pool *pgxpool.Pool
}

func NewPGStore(pool *pgxpool.Pool) Store {
	return &pgStore{pool: pool}
}

func (s *pgStore) Start(ctx context.Context, ev Event) (int64, error) {
	args := ev.Args
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO audit_events (actor, tool, args, status, task_id)
		 VALUES ($1, $2, $3, 'started', $4) RETURNING id`,
		ev.Actor, ev.Tool, args, ev.TaskID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert audit_events: %w", err)
	}
	return id, nil
}

func (s *pgStore) RecordPolicy(ctx context.Context, auditEventID int64, d PolicyDecision) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO policy_decisions (audit_event_id, tool, decision, rule, reason)
		 VALUES ($1, $2, $3, $4, $5)`,
		auditEventID, d.Tool, d.Decision, d.Rule, d.Reason)
	if err != nil {
		return fmt.Errorf("insert policy_decisions: %w", err)
	}
	return nil
}

func (s *pgStore) Complete(ctx context.Context, id int64, status, errMsg string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE audit_events
		 SET status = $2, error = NULLIF($3, ''), completed_at = now()
		 WHERE id = $1`,
		id, status, errMsg)
	if err != nil {
		return fmt.Errorf("update audit_events %d: %w", id, err)
	}
	return nil
}
