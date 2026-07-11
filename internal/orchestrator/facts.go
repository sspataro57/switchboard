package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// facts.go — the read-only SQL side of the purity boundary. Loaders SELECT,
// never mutate.

const depsUnsatisfiedExists = `EXISTS (
  SELECT 1 FROM task_dependencies d
  JOIN tasks dt ON dt.id = d.depends_on_task_id
  WHERE d.task_id = %s AND dt.status NOT IN ('done_locally','delivered','closed')
)`

func (e *Engine) loadEventFacts(ctx context.Context, ev Event) (Facts, error) {
	var f Facts

	// The triggering task + project.
	err := e.pool.QueryRow(ctx,
		`SELECT t.id, p.slug, p.delivery, t.title, t.status,
		        `+fmt.Sprintf(depsUnsatisfiedExists, "t.id")+`
		 FROM tasks t JOIN projects p ON p.id = t.project_id
		 WHERE t.id = $1`, ev.TaskID).Scan(
		&f.Task.ID, &f.Task.ProjectSlug, &f.Task.ProjectDelivery,
		&f.Task.Title, &f.Task.Status, &f.Task.HasUnmetDep)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, nil // task deleted — rules see empty facts and no-op
	}
	if err != nil {
		return f, fmt.Errorf("load task facts: %w", err)
	}

	// Prior orchestrator decisions on this task (dedup facts).
	rows, err := e.pool.Query(ctx,
		`SELECT COALESCE(payload->>'rule',''),
		        COALESCE((payload->>'feedback_request_id')::bigint, 0),
		        COALESCE((payload->>'created_task_id')::bigint, 0)
		 FROM task_events WHERE task_id=$1 AND event_type='orchestrated' ORDER BY id`, ev.TaskID)
	if err != nil {
		return f, fmt.Errorf("load orchestrations: %w", err)
	}
	for rows.Next() {
		o := Orchestration{TaskID: ev.TaskID}
		if err := rows.Scan(&o.Rule, &o.FeedbackRequestID, &o.CreatedTaskID); err != nil {
			rows.Close()
			return f, fmt.Errorf("scan orchestration: %w", err)
		}
		f.Orchestrations = append(f.Orchestrations, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return f, fmt.Errorf("iterate orchestrations: %w", err)
	}

	// Active claim holder.
	err = e.pool.QueryRow(ctx,
		`SELECT worker_id FROM task_claims
		 WHERE task_id=$1 AND released_at IS NULL ORDER BY id DESC LIMIT 1`,
		ev.TaskID).Scan(&f.ActiveClaimWorkerID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return f, fmt.Errorf("load active claim: %w", err)
	}

	// Dependents of this task and whether their deps are now all satisfied.
	rows, err = e.pool.Query(ctx,
		`SELECT t.id, t.status, NOT `+fmt.Sprintf(depsUnsatisfiedExists, "t.id")+`
		 FROM task_dependencies d JOIN tasks t ON t.id = d.task_id
		 WHERE d.depends_on_task_id = $1 ORDER BY t.id`, ev.TaskID)
	if err != nil {
		return f, fmt.Errorf("load dependents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d DependentTask
		if err := rows.Scan(&d.ID, &d.Status, &d.AllDepsSatisfied); err != nil {
			return f, fmt.Errorf("scan dependent: %w", err)
		}
		f.Dependents = append(f.Dependents, d)
	}
	return f, rows.Err()
}

func (e *Engine) loadTickFacts(ctx context.Context, now time.Time) (Facts, error) {
	var f Facts

	// Expired unreleased claims joined to their task status; the pure rule
	// exempts needs_feedback, so it is loaded, not filtered here.
	rows, err := e.pool.Query(ctx,
		`SELECT c.task_id, c.worker_id, t.status
		 FROM task_claims c JOIN tasks t ON t.id = c.task_id
		 WHERE c.released_at IS NULL AND c.expires_at < now()
		   AND t.status IN ('claimed','in_progress','needs_feedback')
		 ORDER BY c.id`)
	if err != nil {
		return f, fmt.Errorf("load expired claims: %w", err)
	}
	for rows.Next() {
		var c ExpiredClaim
		if err := rows.Scan(&c.TaskID, &c.WorkerID, &c.Status); err != nil {
			rows.Close()
			return f, fmt.Errorf("scan expired claim: %w", err)
		}
		f.ExpiredClaims = append(f.ExpiredClaims, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return f, fmt.Errorf("iterate expired claims: %w", err)
	}

	if e.cfg.BriefProject != "" {
		title := "Morning brief " + now.Format("2006-01-02")
		if err := e.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM tasks t JOIN projects p ON p.id=t.project_id
			 WHERE p.slug=$1 AND t.title=$2)`, e.cfg.BriefProject, title).Scan(&f.BriefExists); err != nil {
			return f, fmt.Errorf("check brief existence: %w", err)
		}
		if !f.BriefExists {
			counts, err := e.loadBriefCounts(ctx)
			if err != nil {
				return f, err
			}
			f.BriefCounts = counts
		}
	}
	return f, nil
}

func (e *Engine) loadBriefCounts(ctx context.Context) ([]ProjectCounts, error) {
	rows, err := e.pool.Query(ctx,
		`SELECT p.slug,
		        count(*) FILTER (WHERE t.status='ready'),
		        count(*) FILTER (WHERE t.status='blocked'),
		        count(*) FILTER (WHERE t.status='needs_feedback'),
		        count(*) FILTER (WHERE t.status='done_locally'),
		        (SELECT count(*) FROM feedback_requests fr
		         JOIN tasks ft ON ft.id=fr.task_id
		         WHERE ft.project_id=p.id AND fr.status='open')
		 FROM projects p JOIN tasks t ON t.project_id = p.id
		 GROUP BY p.id, p.slug
		 HAVING count(*) FILTER (WHERE t.status IN
		   ('ready','blocked','needs_feedback','done_locally')) > 0
		 ORDER BY p.slug`)
	if err != nil {
		return nil, fmt.Errorf("load brief counts: %w", err)
	}
	defer rows.Close()

	var out []ProjectCounts
	for rows.Next() {
		var c ProjectCounts
		if err := rows.Scan(&c.ProjectSlug, &c.Ready, &c.Blocked, &c.NeedsFeedback, &c.DoneLocally, &c.OpenFeedback); err != nil {
			return nil, fmt.Errorf("scan brief counts: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
