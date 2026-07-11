package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// task_context — the one JSON document a worker needs to start. Side effect:
// when worker_id matches the task's active claim AND status is claimed or
// needs_feedback, the status flips to in_progress (fetching context IS the
// moment work (re)starts — no separate task_start tool). Pure read otherwise.

type contextArgs struct {
	TaskID   int64  `json:"task_id"`
	WorkerID string `json:"worker_id,omitempty"`
}

func validateContext(args []byte) error {
	var a contextArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID == 0 {
		return errors.New("missing task_id")
	}
	return nil
}

func taskContext(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a contextArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	doc := map[string]any{}

	// task + project
	var (
		projectID                              int64
		subproject, body, autonomy, workerType *string
		parentID                               *int64
		title, status, assigneeType            string
		priority                               int
		pName, pSlug, pClient                  string
		pRepoPath                              *string
		pExecution, pDelivery                  string
	)
	err := pool.QueryRow(ctx,
		`SELECT t.project_id, t.subproject, t.parent_id, t.title, t.body, t.assignee_type,
		        t.worker_type, t.status, t.autonomy, t.priority,
		        p.name, p.slug, p.client, p.repo_path, p.execution, p.delivery
		 FROM tasks t JOIN projects p ON p.id = t.project_id
		 WHERE t.id = $1`, a.TaskID).Scan(
		&projectID, &subproject, &parentID, &title, &body, &assigneeType,
		&workerType, &status, &autonomy, &priority,
		&pName, &pSlug, &pClient, &pRepoPath, &pExecution, &pDelivery)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("task %d not found", a.TaskID)
	}
	if err != nil {
		return nil, fmt.Errorf("select task %d: %w", a.TaskID, err)
	}

	// holder transition: claimed|needs_feedback -> in_progress. The check and
	// the flip share one tx with the task row locked, so a concurrent release
	// or double-fetch cannot interleave.
	if a.WorkerID != "" && (status == "claimed" || status == "needs_feedback") {
		err := inTx(ctx, pool, func(tx pgx.Tx) error {
			var cur string
			if err := tx.QueryRow(ctx,
				`SELECT status FROM tasks WHERE id=$1 FOR UPDATE`, a.TaskID).Scan(&cur); err != nil {
				return fmt.Errorf("lock task %d: %w", a.TaskID, err)
			}
			if cur != "claimed" && cur != "needs_feedback" {
				return nil // raced with another transition — stay a pure read
			}
			if _, err := activeClaim(ctx, tx, a.TaskID, a.WorkerID); err != nil {
				return nil // not the holder — pure read
			}
			if _, err := tx.Exec(ctx,
				`UPDATE tasks SET status='in_progress', updated_at=now() WHERE id=$1`, a.TaskID); err != nil {
				return fmt.Errorf("flip task %d to in_progress: %w", a.TaskID, err)
			}
			if _, err := insertTaskEvent(ctx, tx, a.TaskID, "status_changed",
				map[string]any{"from": cur, "to": "in_progress", "worker_id": a.WorkerID}); err != nil {
				return err
			}
			status = "in_progress"
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	doc["task"] = map[string]any{
		"id": a.TaskID, "title": title, "body": deref(body), "status": status,
		"subproject": deref(subproject), "assignee_type": assigneeType,
		"worker_type": deref(workerType), "autonomy": deref(autonomy),
		"priority": priority, "parent_id": parentID,
	}
	doc["project"] = map[string]any{
		"name": pName, "slug": pSlug, "client": pClient,
		"repo_path": derefP(pRepoPath), "execution": pExecution, "delivery": pDelivery,
	}

	// decisions — ALL project decisions, injected into every task context
	decisions, err := collectRows(ctx, pool,
		`SELECT title, COALESCE(body,''), COALESCE(created_by,''), created_at::text
		 FROM decisions WHERE project_id=$1 ORDER BY created_at, id`, []any{projectID},
		"title", "body", "created_by", "created_at")
	if err != nil {
		return nil, fmt.Errorf("collect decisions: %w", err)
	}
	doc["decisions"] = decisions

	// parent summary
	if parentID != nil {
		var pt, ps string
		if err := pool.QueryRow(ctx,
			`SELECT title, status FROM tasks WHERE id=$1`, *parentID).Scan(&pt, &ps); err == nil {
			doc["parent"] = map[string]any{"id": *parentID, "title": pt, "status": ps}
		}
	} else {
		doc["parent"] = nil
	}

	// children summaries
	children, err := collectRows(ctx, pool,
		`SELECT id::text, title, status FROM tasks WHERE parent_id=$1 ORDER BY id`, []any{a.TaskID},
		"id", "title", "status")
	if err != nil {
		return nil, fmt.Errorf("collect children: %w", err)
	}
	doc["children"] = children

	// dependencies
	deps, err := collectRows(ctx, pool,
		`SELECT d.depends_on_task_id::text, t.title, t.status
		 FROM task_dependencies d JOIN tasks t ON t.id = d.depends_on_task_id
		 WHERE d.task_id=$1 ORDER BY d.depends_on_task_id`, []any{a.TaskID},
		"id", "title", "status")
	if err != nil {
		return nil, fmt.Errorf("collect dependencies: %w", err)
	}
	doc["dependencies"] = deps

	// feedback requests
	feedback, err := collectRows(ctx, pool,
		`SELECT id::text, question, COALESCE(answer,''), status
		 FROM feedback_requests WHERE task_id=$1 ORDER BY id`, []any{a.TaskID},
		"id", "question", "answer", "status")
	if err != nil {
		return nil, fmt.Errorf("collect feedback: %w", err)
	}
	doc["feedback"] = feedback

	// last 50 events, oldest first for readability
	events, err := collectRows(ctx, pool,
		`SELECT event_type, payload::text, created_at::text FROM (
		   SELECT event_type, payload, created_at, id FROM task_events
		   WHERE task_id=$1 ORDER BY id DESC LIMIT 50
		 ) sub ORDER BY id ASC`, []any{a.TaskID},
		"event_type", "payload", "created_at")
	if err != nil {
		return nil, fmt.Errorf("collect events: %w", err)
	}
	doc["events"] = events

	return marshalResult(doc)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefP(s *string) string { return deref(s) }

// collectRows scans uniform text rows into []map[string]string.
func collectRows(ctx context.Context, q querier, sql string, args []any, cols ...string) ([]map[string]string, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	out := []map[string]string{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]*string, len(cols))
		for i := range vals {
			ptrs[i] = new(string)
			vals[i] = ptrs[i]
		}
		if err := rows.Scan(vals...); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		m := map[string]string{}
		for i, c := range cols {
			m[c] = *ptrs[i]
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
