package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// record_decision — project-scoped decisions, injected into every task_context.

type decisionArgs struct {
	Project  string `json:"project"`
	Title    string `json:"title"`
	Body     string `json:"body,omitempty"`
	WorkerID string `json:"worker_id,omitempty"`
}

func validateDecision(args []byte) error {
	var a decisionArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.Project == "" {
		return errors.New("missing project")
	}
	if a.Title == "" {
		return errors.New("missing title")
	}
	return nil
}

func recordDecision(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a decisionArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	var projectID int64
	err := pool.QueryRow(ctx, `SELECT id FROM projects WHERE slug=$1`, a.Project).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("project %q not found", a.Project)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve project %q: %w", a.Project, err)
	}

	createdBy := "unspecified"
	if a.WorkerID != "" {
		createdBy = "worker:" + a.WorkerID
	}

	var decisionID int64
	err = pool.QueryRow(ctx,
		`INSERT INTO decisions (project_id, title, body, created_by)
		 VALUES ($1, $2, NULLIF($3,''), $4) RETURNING id`,
		projectID, a.Title, a.Body, createdBy).Scan(&decisionID)
	if err != nil {
		return nil, fmt.Errorf("insert decision: %w", err)
	}
	return marshalResult(map[string]any{"decision_id": decisionID})
}
