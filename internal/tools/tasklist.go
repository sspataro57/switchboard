package tools

// Read-only queue tools (SWT-35, docs/tickets/task-list-mcp_SPEC.md).
//
// task_list(project, …) reads ONE project's work still in play, in exactly the
// order task_get_next hands work to a worker, with per-status counts over the
// full filtered set. project_list is the directory a session confirms a slug
// against before memorising it as its repo's queue.
//
// Both write nothing but their audit row, so — like mail_search — they are not
// humanOnly and not snapshotGated, and nothing in either handler branches on
// who is calling: cross-client privacy is not a goal (Salvador, 2026-09-10), and
// by his decision of the same day local_only projects are listed like any
// other — no project column but the id is read. What makes that
// acceptable is the ROW SHAPE: a row carries
// id, title, status, priority and assignee_type (plus subproject / parent_id
// when set) and never a body — task_context stays the audited per-task read.
//
// "No junk and wasted tokens" (Salvador) is the acceptance test for the output:
// the default set hides closed AND delivered work (unlike the board, which
// hides only closed), the default limit is one screenful, and a key appears in
// a row only when it carries information.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	taskListDefaultLimit = 25  // one screenful; the counts carry the rest (L9)
	taskListMaxLimit     = 200 // clamped, not refused (mail_search's shape)
)

// taskListInPlay names the default status set in the echoed filter. Not "open":
// openStatuses (close.go) already means task_close's source set.
const taskListInPlay = "in_play"

// inPlayPredicate is the ONE spelling of "work still in play" (L4), used by
// task_list's default and by project_list's in_play count, so the directory can
// never report a count task_list will not return.
const inPlayPredicate = `t.status NOT IN ('closed','delivered')`

// taskStatuses is the tasks.status CHECK (migrations/0001_initial.sql). An
// unknown status is a validation error, never an empty list: `status=redy` must
// not read as "nothing ready". Pinned to the live CHECK by an integration test.
var taskStatuses = []string{
	"holding", "ready", "claimed", "in_progress", "needs_feedback",
	"pr_open", "awaiting_ci", "awaiting_merge", "done_locally",
	"delivered", "closed", "blocked",
}

// ---- task_list ----------------------------------------------------------------

type taskListArgs struct {
	Project      string `json:"project"`
	Status       string `json:"status,omitempty"`
	AssigneeType string `json:"assignee_type,omitempty"`
	Subproject   string `json:"subproject,omitempty"`
	Limit        int    `json:"limit,omitempty"`
}

// validateTaskList tolerates unknown keys on purpose: every MCP call arrives
// carrying an injected worker_id.
func validateTaskList(args []byte) error {
	var a taskListArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if strings.TrimSpace(a.Project) == "" {
		return errors.New("missing project: the caller's project slug is required (call project_list for valid slugs)")
	}
	if a.Status != "" && !slices.Contains(taskStatuses, a.Status) {
		return fmt.Errorf("status %q is not a task status (want one of %s)", a.Status, strings.Join(taskStatuses, ", "))
	}
	switch a.AssigneeType {
	case "", "human", "claude":
	default:
		return fmt.Errorf("assignee_type %q is not human or claude", a.AssigneeType)
	}
	if a.Limit < 0 {
		return fmt.Errorf("limit %d is negative", a.Limit)
	}
	return nil
}

// taskListFilter is the filter echoed back: status is always present
// ("in_play" when not given), limit is the APPLIED value, and assignee_type /
// subproject appear only when given.
type taskListFilter struct {
	Status       string `json:"status"`
	AssigneeType string `json:"assignee_type,omitempty"`
	Subproject   string `json:"subproject,omitempty"`
	Limit        int    `json:"limit"`
}

func newTaskListFilter(a taskListArgs) taskListFilter {
	f := taskListFilter{
		Status:       taskListInPlay,
		AssigneeType: a.AssigneeType,
		Subproject:   strings.TrimSpace(a.Subproject),
		Limit:        a.Limit,
	}
	if a.Status != "" {
		f.Status = a.Status
	}
	if f.Limit <= 0 {
		f.Limit = taskListDefaultLimit
	}
	if f.Limit > taskListMaxLimit {
		f.Limit = taskListMaxLimit
	}
	return f
}

// taskListRow is one compact row. No body (the largest token cost, and
// task_context is the audited per-task read), no plan_order (row position is
// the order), no timestamps. subproject / parent_id are OMITTED when NULL.
type taskListRow struct {
	ID           int64   `json:"id"`
	Title        string  `json:"title"`
	Status       string  `json:"status"`
	Priority     int     `json:"priority"`
	AssigneeType string  `json:"assignee_type"`
	Subproject   *string `json:"subproject,omitempty"`
	ParentID     *int64  `json:"parent_id,omitempty"`
}

type taskListResult struct {
	Project   string         `json:"project"`
	Filter    taskListFilter `json:"filter"`
	Counts    map[string]int `json:"counts"`
	Total     int            `json:"total"`
	Truncated bool           `json:"truncated"`
	Tasks     []taskListRow  `json:"tasks"`
}

// taskListWhere builds the WHERE fragment ONCE, for both the rows and the
// counts query (L8) — a counts query with its own WHERE would be a second
// spelling of the filter. Every value is a bound parameter; the fragment is
// assembled from fixed strings only.
func taskListWhere(projectID int64, f taskListFilter) (string, []any) {
	clauses := []string{"t.project_id = $1"}
	params := []any{projectID}
	if f.Status == taskListInPlay {
		clauses = append(clauses, inPlayPredicate)
	} else {
		params = append(params, f.Status)
		clauses = append(clauses, fmt.Sprintf("t.status = $%d", len(params)))
	}
	if f.AssigneeType != "" {
		params = append(params, f.AssigneeType)
		clauses = append(clauses, fmt.Sprintf("t.assignee_type = $%d", len(params)))
	}
	if f.Subproject != "" {
		params = append(params, f.Subproject)
		clauses = append(clauses, fmt.Sprintf("t.subproject = $%d", len(params)))
	}
	return strings.Join(clauses, " AND "), params
}

func taskList(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a taskListArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	slug := strings.TrimSpace(a.Project)

	// ONE read-only snapshot for the resolve, the page and the counts: separate
	// statements on the pool could straddle a claim or a close and return a row
	// its own counts do not include. ReadOnly also makes Postgres refuse a write.
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin task_list snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var projectID int64
	var resolved string
	err = tx.QueryRow(ctx, `SELECT id, slug FROM projects WHERE slug = $1`, slug).Scan(&projectID, &resolved)
	if errors.Is(err, pgx.ErrNoRows) {
		// An error, never an empty list: {"tasks":[]} for a slug that does not
		// exist reads as "nothing in my queue".
		return nil, fmt.Errorf("project %q not found; call project_list for valid slugs", slug)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve project %q: %w", slug, err)
	}

	f := newTaskListFilter(a)
	where, params := taskListWhere(projectID, f)

	// limit+1 so truncated is reported honestly, not whenever a page is full.
	rowParams := append(append([]any(nil), params...), f.Limit+1)
	rows, err := tx.Query(ctx,
		`SELECT t.id, t.title, t.status, t.priority, t.assignee_type, t.subproject, t.parent_id
		   FROM tasks t
		  WHERE `+where+`
		  ORDER BY `+taskQueueOrder+`
		  LIMIT `+fmt.Sprintf("$%d", len(rowParams)), rowParams...)
	if err != nil {
		return nil, fmt.Errorf("list tasks for %q: %w", resolved, err)
	}
	out := taskListResult{Project: resolved, Filter: f, Counts: map[string]int{}, Tasks: make([]taskListRow, 0, f.Limit)}
	for rows.Next() {
		if len(out.Tasks) == f.Limit {
			out.Truncated = true
			break
		}
		var r taskListRow
		if err := rows.Scan(&r.ID, &r.Title, &r.Status, &r.Priority, &r.AssigneeType, &r.Subproject, &r.ParentID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan task row: %w", err)
		}
		out.Tasks = append(out.Tasks, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task rows: %w", err)
	}

	// Counts over the FULL filtered set, independent of limit (L8).
	crows, err := tx.Query(ctx,
		`SELECT t.status, count(*) FROM tasks t WHERE `+where+` GROUP BY t.status`, params...)
	if err != nil {
		return nil, fmt.Errorf("count tasks for %q: %w", resolved, err)
	}
	defer crows.Close()
	for crows.Next() {
		var status string
		var n int
		if err := crows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("scan task count: %w", err)
		}
		out.Counts[status] = n
		out.Total += n
	}
	if err := crows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task counts: %w", err)
	}
	return marshalResult(out)
}

// ---- project_list -------------------------------------------------------------

// validateProjectList accepts no arguments: an empty body, {}, or an object
// carrying only the injected worker_id. project_list is the one registered tool
// whose empty args are legal. Anything that is not a JSON object is malformed.
func validateProjectList(args []byte) error {
	if len(bytes.TrimSpace(args)) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return fmt.Errorf("parse args: project_list takes no arguments, and its args must be a JSON object: %w", err)
	}
	return nil
}

// projectListRow is one directory row: client only when set. There is no
// local_only field — nothing refuses a local_only project, so the flag would
// carry no information a caller can act on.
type projectListRow struct {
	Slug   string  `json:"slug"`
	Name   string  `json:"name"`
	Client *string `json:"client,omitempty"`
	InPlay int     `json:"in_play"`
}

func projectList(ctx context.Context, pool *pgxpool.Pool, _ []byte) ([]byte, error) {
	// Every project, none hidden: a project with nothing in play is still a slug
	// a repo may memorise. in_play uses the SAME predicate as task_list's default.
	rows, err := pool.Query(ctx,
		`SELECT p.slug, COALESCE(p.name, ''), p.client,
		        count(t.id) FILTER (WHERE `+inPlayPredicate+`)
		   FROM projects p
		   LEFT JOIN tasks t ON t.project_id = p.id
		  GROUP BY p.id, p.slug, p.name, p.client
		  ORDER BY p.slug`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	projects := make([]projectListRow, 0)
	for rows.Next() {
		var r projectListRow
		if err := rows.Scan(&r.Slug, &r.Name, &r.Client, &r.InPlay); err != nil {
			return nil, fmt.Errorf("scan project row: %w", err)
		}
		projects = append(projects, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate projects: %w", err)
	}
	return marshalResult(map[string]any{"projects": projects})
}
