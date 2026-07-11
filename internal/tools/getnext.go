package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// task_get_next — peek, read-only. Peek and claim are separate on purpose:
// the losing racer's claim fails cleanly and it re-peeks.

type getNextArgs struct {
	Client     string `json:"client"`
	Subproject string `json:"subproject,omitempty"`
}

func validateGetNext(args []byte) error {
	var a getNextArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.Client == "" {
		return errors.New("missing client")
	}
	return nil
}

type nextTask struct {
	ID         int64  `json:"id"`
	Project    string `json:"project"`
	Subproject string `json:"subproject"`
	Title      string `json:"title"`
	Priority   int    `json:"priority"`
}

func getNext(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a getNextArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	var nt nextTask
	var subproject *string
	err := pool.QueryRow(ctx,
		`SELECT t.id, p.slug, t.subproject, t.title, t.priority
		 FROM tasks t JOIN projects p ON p.id = t.project_id
		 WHERE p.client = $1
		   AND t.status = 'ready'
		   AND t.assignee_type = 'claude'
		   AND ($2 = '' OR t.subproject = $2)
		 ORDER BY t.priority DESC, t.plan_order ASC NULLS LAST, t.created_at ASC, t.id ASC
		 LIMIT 1`,
		a.Client, a.Subproject).Scan(&nt.ID, &nt.Project, &subproject, &nt.Title, &nt.Priority)
	if errors.Is(err, pgx.ErrNoRows) {
		return marshalResult(map[string]any{"task": nil})
	}
	if err != nil {
		return nil, fmt.Errorf("select next task: %w", err)
	}
	if subproject != nil {
		nt.Subproject = *subproject
	}
	return marshalResult(map[string]any{"task": nt})
}
