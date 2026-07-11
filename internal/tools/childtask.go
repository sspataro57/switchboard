package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// create_child_task — new discovered work becomes a child row, never a plan
// edit. Cross-boundary coordination passes subproject "main" +
// worker_type "coordination".

type childTaskArgs struct {
	ParentTaskID int64  `json:"parent_task_id"`
	Title        string `json:"title"`
	Body         string `json:"body,omitempty"`
	AssigneeType string `json:"assignee_type,omitempty"`
	Priority     *int   `json:"priority,omitempty"`
	Subproject   string `json:"subproject,omitempty"`
	WorkerType   string `json:"worker_type,omitempty"`
}

func validateChildTask(args []byte) error {
	var a childTaskArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.ParentTaskID == 0 {
		return errors.New("missing parent_task_id")
	}
	if a.Title == "" {
		return errors.New("missing title")
	}
	if a.AssigneeType != "" && a.AssigneeType != "human" && a.AssigneeType != "claude" {
		return fmt.Errorf("assignee_type %q: must be human or claude", a.AssigneeType)
	}
	return nil
}

func createChildTask(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a childTaskArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	if a.AssigneeType == "" {
		a.AssigneeType = "claude"
	}
	priority := 0
	if a.Priority != nil {
		priority = *a.Priority
	}

	var childID int64
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		var projectID int64
		var parentSubproject *string
		err := tx.QueryRow(ctx,
			`SELECT project_id, subproject FROM tasks WHERE id=$1`, a.ParentTaskID).
			Scan(&projectID, &parentSubproject)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("parent task %d not found", a.ParentTaskID)
		}
		if err != nil {
			return fmt.Errorf("select parent task %d: %w", a.ParentTaskID, err)
		}

		subproject := a.Subproject
		if subproject == "" && parentSubproject != nil {
			subproject = *parentSubproject
		}

		err = tx.QueryRow(ctx,
			`INSERT INTO tasks (project_id, subproject, parent_id, title, body,
			                    assignee_type, worker_type, status, priority)
			 VALUES ($1, NULLIF($2,''), $3, $4, NULLIF($5,''), $6, NULLIF($7,''), 'ready', $8)
			 RETURNING id`,
			projectID, subproject, a.ParentTaskID, a.Title, a.Body,
			a.AssigneeType, a.WorkerType, priority).Scan(&childID)
		if err != nil {
			return fmt.Errorf("insert child task: %w", err)
		}

		_, err = insertTaskEvent(ctx, tx, a.ParentTaskID, "child_created",
			map[string]any{"child_task_id": childID})
		return err
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"task_id": childID})
}
