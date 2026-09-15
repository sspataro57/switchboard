package dashboard

// SWT-10 dashboard slices: the full board (/tasks — queues are FILTERS on the
// one tasks table, never tables), task detail, briefs, plan-import review
// (/plans) and the deterministic exports. Reads are direct SQL (dashboard
// idiom); the ONLY actions are approve/reject_plan_import, both through the
// executor (invariant 3).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sspataro57/switchboard/internal/orchestrator"
	"github.com/sspataro57/switchboard/internal/tools"
)

// ---- /tasks board -------------------------------------------------------------

type taskRow struct {
	ID           int64
	Project      string
	Subproject   string
	ParentID     string
	Title        string
	Status       string
	AssigneeType string
	WorkerType   string
	Priority     int
	PlanOrder    string
	UpdatedAt    string
	// ReopenedAfterDismissal is the D12 marker's reason code (SWT-36): set
	// when the task is not closed and its NEWEST dismissal was overtaken by
	// an inbound message. Board-only — never an export column.
	ReopenedAfterDismissal string
	// Light is the row's status light (SWT-52 D1), computed in Go by lightFor
	// from the status and boardLightFacts' separate read. Board-only.
	Light light
	// QueueRank orders the queue section (SWT-57 L2) and Updated is the short
	// updated stamp the row shows (L8); both come from boardLightFacts.
	// Board-only — never export columns.
	QueueRank int
	Updated   string
}

type boardData struct {
	// Sections are the rows grouped by their light (SWT-57 L1), in
	// boardSectionOrder, empty ones omitted.
	Sections []boardSection
	// AdvancedFilters are the active non-project filters, shown on the first
	// line (L5); ClearAdvancedURL drops them, keeping project and refresh.
	AdvancedFilters  []boardFilter
	ClearAdvancedURL string
	Projects         []string
	Filters          map[string]string
	Flash            string
	// OrchAlert is set only when the orchestrator verdict is not ok (SWT-41
	// D5): the board is where Salvador looks, /funnel is where he investigates.
	// A failing health query leaves it nil — the board never breaks on health.
	OrchAlert *orchestrator.HealthState
	// SWT-52 D15, the opt-in auto-refresh. AutoRefresh is set iff the query
	// carries refresh=on; RefreshSeconds comes from boardRefreshInterval, never
	// from the request. RefreshToggleURL and ReloadURL are rebuilt from
	// boardKeys (never flash); RenderedAt is the DB clock's HH:MM:SS in
	// BoardTimeZone, returned by boardLightFacts' first statement.
	AutoRefresh      bool
	RefreshSeconds   int
	RefreshToggleURL string
	ReloadURL        string
	RenderedAt       string
}

// BoardTimeZone is Salvador's day for the board (SWT-52 D5): a task closed
// since local midnight here stays on the default board. Bound as a parameter,
// evaluated on the Postgres clock — the pods run UTC (the SWT-48 lesson) — and
// deliberately not AVAIL_TZ, which is availability's knob.
const BoardTimeZone = "America/New_York"

// boardRefreshInterval is the auto-refresh period (D15), fixed here and never a
// URL value: a caller-chosen refresh=0.1 would turn one tab into a query flood
// against the shared pg-main.
const boardRefreshInterval = 5 * time.Second

// boardKeys is the ONE list of board URL keys (D15, criterion 30): the four
// filters plus refresh. boardBack rebuilds a verb's redirect from the POSTed
// form over it, and boardRefreshURLs rebuilds the toggle and reload URLs from
// the query over it, so a key cannot survive one round trip and not another.
// boardQuery still reads only the four filters: refresh never reaches SQL.
var boardKeys = []string{"project", "status", "assignee_type", "subproject", "refresh"}

// boardDayStart is the ONE spelling of today's local midnight on the DB clock
// (criterion 10): p is the bind parameter holding BoardTimeZone.
func boardDayStart(p string) string {
	return "date_trunc('day', now() AT TIME ZONE " + p + ") AT TIME ZONE " + p
}

// boardStatusOrder is the status machine's order. Since SWT-57 it no longer
// orders the board (the light-derived sections do); it is the within-section
// tiebreak, so in-flight rows read along the pipeline (L2, L10).
var boardStatusOrder = []string{
	"holding", "ready", "blocked", "claimed", "in_progress", "needs_feedback",
	"pr_open", "awaiting_ci", "awaiting_merge", "done_locally", "delivered", "closed",
}

// boardQuery builds the filtered board select (shared by /tasks and the
// exports — same filters, id ASC). With no status filter it shows every open
// task plus the tasks closed since today's local midnight that carry no OPEN
// dismissal (SWT-52 D5): a dismissal leaves at once, a Done lingers until
// midnight. The close instant is COALESCE(closed_at, updated_at), the 0030
// fallback. Its select list — the exports' columns — is unchanged.
func boardQuery(r *http.Request) (string, []any) {
	q := `SELECT t.id, COALESCE(p.slug,''), COALESCE(t.subproject,''), t.parent_id,
	             t.title, t.status, t.assignee_type, COALESCE(t.worker_type,''),
	             t.priority, t.plan_order,
	             COALESCE(t.created_at::text,''), COALESCE(t.updated_at::text,'')
	      FROM tasks t JOIN projects p ON p.id = t.project_id`
	var conds []string
	var args []any
	add := func(cond string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}
	if v := r.URL.Query().Get("project"); v != "" {
		add("p.slug = $%d", v)
	}
	if v := r.URL.Query().Get("status"); v != "" {
		add("t.status = $%d", v)
	} else {
		args = append(args, BoardTimeZone)
		conds = append(conds, `(t.status <> 'closed'
		 OR (COALESCE(t.closed_at, t.updated_at) >= `+boardDayStart(fmt.Sprintf("$%d", len(args)))+`
		     AND NOT EXISTS (SELECT 1 FROM task_dismissals d
		                      WHERE d.task_id = t.id AND d.reopened_at IS NULL)))`)
	}
	if v := r.URL.Query().Get("assignee_type"); v != "" {
		add("t.assignee_type = $%d", v)
	}
	if v := r.URL.Query().Get("subproject"); v != "" {
		add("t.subproject = $%d", v)
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY t.id ASC"
	return q, args
}

func (s *Server) boardRows(r *http.Request) ([]TaskExportRow, error) {
	q, args := boardQuery(r)
	rows, err := s.pool.Query(r.Context(), q, args...)
	if err != nil {
		return nil, fmt.Errorf("select board: %w", err)
	}
	defer rows.Close()
	var out []TaskExportRow
	for rows.Next() {
		var t TaskExportRow
		if err := rows.Scan(&t.ID, &t.Project, &t.Subproject, &t.ParentID, &t.Title,
			&t.Status, &t.AssigneeType, &t.WorkerType, &t.Priority, &t.PlanOrder,
			&t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan board row: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	rows, err := s.boardRows(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	markers, err := s.reopenMarkers(r, rows)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// SWT-52: the lights' own read (D3), which also returns the render time the
	// auto-refresh indicator shows (D15). One refresh render is exactly one
	// ordinary render: the same statements, whatever the refresh key says.
	facts, renderedAt, err := s.boardLightFacts(r.Context(), rows)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	trs := make([]taskRow, 0, len(rows))
	for _, t := range rows {
		f := facts[t.ID]
		tr := taskRow{
			ID: t.ID, Project: t.Project, Subproject: t.Subproject,
			Title: t.Title, Status: t.Status, AssigneeType: t.AssigneeType,
			WorkerType: t.WorkerType, Priority: t.Priority, UpdatedAt: t.UpdatedAt,
			ReopenedAfterDismissal: markers[t.ID],
			Light:                  lightFor(t.Status, f),
			QueueRank:              f.QueueRank,
			Updated:                f.UpdatedStamp,
		}
		if tr.Updated == "" {
			tr.Updated = t.UpdatedAt // never a blank cell for a row that has a value
		}
		if t.ParentID != nil {
			tr.ParentID = fmt.Sprintf("%d", *t.ParentID)
		}
		if t.PlanOrder != nil {
			tr.PlanOrder = fmt.Sprintf("%d", *t.PlanOrder)
		}
		trs = append(trs, tr)
	}
	// D15: only refresh=on turns auto-refresh on; the interval is the const.
	autoRefresh := r.URL.Query().Get("refresh") == "on"
	refreshKey := ""
	if autoRefresh {
		refreshKey = "on" // the verb forms' and the filter form's hidden refresh input
	}
	toggle, reload := boardRefreshURLs(r.URL.Query())
	data := boardData{Flash: r.URL.Query().Get("flash"), Filters: map[string]string{
		"project":       r.URL.Query().Get("project"),
		"status":        r.URL.Query().Get("status"),
		"assignee_type": r.URL.Query().Get("assignee_type"),
		"subproject":    r.URL.Query().Get("subproject"),
		"refresh":       refreshKey,
	},
		AutoRefresh:      autoRefresh,
		RefreshSeconds:   int(boardRefreshInterval / time.Second),
		RefreshToggleURL: toggle,
		ReloadURL:        reload,
		RenderedAt:       renderedAt,
	}
	// SWT-57: the rows grouped by their light (unknown statuses land in
	// "other"), and the first line's advanced-filter marker.
	data.Sections = boardSections(trs)
	data.AdvancedFilters, data.ClearAdvancedURL = boardAdvanced(r.URL.Query())

	prows, err := s.pool.Query(r.Context(), `SELECT slug FROM projects ORDER BY slug`)
	if err == nil {
		defer prows.Close()
		for prows.Next() {
			var slug string
			if prows.Scan(&slug) == nil {
				data.Projects = append(data.Projects, slug)
			}
		}
	}

	// Bounded: a slow or hung health read degrades to "no line", never a
	// stalled board (SWT-41 review).
	hctx, hcancel := context.WithTimeout(r.Context(), 2*time.Second)
	if h, err := orchestrator.Health(hctx, s.pool, time.Now()); err == nil && h.Verdict != orchestrator.VerdictOK {
		data.OrchAlert = &h
	}
	hcancel()

	if err := s.tmpl.ExecuteTemplate(w, "tasks.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// reopenMarkers is D12's separate read (SWT-36): for the board's NOT-closed
// rows, the reason code of each task whose NEWEST dismissal was overtaken by an
// inbound message (reopened_by_message_id IS NOT NULL). Keyed on the message
// column, not reopened_at: a human's plain reopen (the mis-click undo) is not
// "reopened after dismissal". Deliberately NOT in boardQuery, which the CSV and
// JSON exports share and whose header is pinned.
func (s *Server) reopenMarkers(r *http.Request, rows []TaskExportRow) (map[int64]string, error) {
	out := map[int64]string{}
	var ids []int64
	for _, t := range rows {
		if t.Status != "closed" {
			ids = append(ids, t.ID)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	q, err := s.pool.Query(r.Context(),
		`SELECT n.task_id, n.reason_code
		   FROM (SELECT DISTINCT ON (d.task_id) d.task_id, d.reason_code, d.reopened_by_message_id
		           FROM task_dismissals d
		          WHERE d.task_id = ANY($1)
		          ORDER BY d.task_id, d.id DESC) n
		  WHERE n.reopened_by_message_id IS NOT NULL`, ids)
	if err != nil {
		return nil, fmt.Errorf("select reopen markers: %w", err)
	}
	defer q.Close()
	for q.Next() {
		var id int64
		var code string
		if err := q.Scan(&id, &code); err != nil {
			return nil, fmt.Errorf("scan reopen marker: %w", err)
		}
		out[id] = code
	}
	return out, q.Err()
}

// boardLightFacts is the lights' SEPARATE read (SWT-52 D3, criterion 5) — the
// reopenMarkers precedent: boardQuery feeds the exports, whose header is
// pinned, so none of these columns enters TaskExportRow. At most two
// statements per render (D15's cost statement):
//
//  1. the row facts, for the union of the displayed ids and every ready task,
//     plus the render time for the auto-refresh indicator (one row even when no
//     task matches: the facts are LEFT JOINed onto a one-row select). The
//     session state and its time; staleness against tools.WorkingLease; and,
//     for closed rows, the newest OPEN dismissal's code and whether the close
//     instant is since today's local midnight. All on the DB clock, in
//     BoardTimeZone.
//  2. the queue-head candidates: every ready task in the database, in
//     tools.TaskQueueOrder (D2) — never only the displayed rows, so a filter
//     can hide a queue's first task but never make the second one blue. Skipped
//     when no task is ready.
//
// A candidate is eligible iff lightFor with QueueHead=false gives it the
// neutral light: a red or yellow task is not "next". It reads neither
// feedback_requests nor task_events.
func (s *Server) boardLightFacts(ctx context.Context, rows []TaskExportRow) (map[int64]lightFacts, string, error) {
	ids := make([]int64, 0, len(rows))
	for _, t := range rows {
		ids = append(ids, t.ID)
	}
	facts := map[int64]lightFacts{}
	statusOf := map[int64]string{}
	var renderedAt string
	q, err := s.pool.Query(ctx,
		`SELECT to_char(now() AT TIME ZONE $2, 'HH24:MI:SS'),
		        f.id, f.status, f.state, f.state_at, f.state_today, f.stale, f.dismissal, f.closed_today, f.session,
		        f.updated
		   FROM (SELECT 1) one
		   LEFT JOIN (
		     SELECT t.id, t.status,
		            COALESCE(t.working_state, '') AS state,
		            COALESCE(t.working_session, '') AS session,
		            COALESCE(CASE WHEN t.updated_at >= `+boardDayStart("$2")+`
		                          THEN to_char(t.updated_at AT TIME ZONE $2, 'HH24:MI')
		                          ELSE to_char(t.updated_at AT TIME ZONE $2, 'YYYY-MM-DD') END, '') AS updated,
		            COALESCE(to_char(t.working_state_at AT TIME ZONE $2, 'YYYY-MM-DD HH24:MI'), '') AS state_at,
		            COALESCE(t.working_state_at >= `+boardDayStart("$2")+`, false) AS state_today,
		            COALESCE(t.working_state = 'working'
		                     AND t.working_state_at < now() - make_interval(secs => $3), false) AS stale,
		            CASE WHEN t.status = 'closed' THEN
		                 COALESCE((SELECT d.reason_code FROM task_dismissals d
		                            WHERE d.task_id = t.id AND d.reopened_at IS NULL
		                            ORDER BY d.id DESC LIMIT 1), '')
		                 ELSE '' END AS dismissal,
		            (t.status = 'closed' AND COALESCE(t.closed_at, t.updated_at) >= `+boardDayStart("$2")+`) AS closed_today
		       FROM tasks t
		      WHERE t.id = ANY($1) OR t.status = 'ready') f ON true`,
		ids, BoardTimeZone, tools.WorkingLease.Seconds())
	if err != nil {
		return nil, "", fmt.Errorf("select light facts: %w", err)
	}
	defer q.Close()
	var ready []int64
	for q.Next() {
		var id *int64
		var status, state, stateAt, dismissal, session, updated *string
		var stateToday, stale, closedToday *bool
		if err := q.Scan(&renderedAt, &id, &status, &state, &stateAt, &stateToday, &stale, &dismissal, &closedToday,
			&session, &updated); err != nil {
			return nil, "", fmt.Errorf("scan light facts: %w", err)
		}
		if id == nil {
			continue // the one row of an empty match: render time only
		}
		facts[*id] = lightFacts{
			OpenDismissalCode: *dismissal, ClosedToday: *closedToday,
			State: *state, StateAt: *stateAt, StateToday: *stateToday, Stale: *stale,
			Session: *session, UpdatedStamp: *updated,
		}
		statusOf[*id] = *status
		if *status == "ready" {
			ready = append(ready, *id)
		}
	}
	if err := q.Err(); err != nil {
		return nil, "", fmt.Errorf("read light facts: %w", err)
	}
	q.Close()
	if len(ready) == 0 {
		return facts, renderedAt, nil
	}

	c, err := s.pool.Query(ctx,
		`SELECT t.id, t.assignee_type, t.project_id, COALESCE(p.slug,''), COALESCE(p.client,''), COALESCE(t.subproject,'')
		   FROM tasks t JOIN projects p ON p.id = t.project_id
		  WHERE t.status = 'ready'
		  ORDER BY `+tools.TaskQueueOrder)
	if err != nil {
		return nil, "", fmt.Errorf("select queue candidates: %w", err)
	}
	defer c.Close()
	var cands []headCandidate
	for c.Next() {
		var h headCandidate
		if err := c.Scan(&h.ID, &h.AssigneeType, &h.ProjectID, &h.ProjectSlug, &h.Client, &h.Subproject); err != nil {
			return nil, "", fmt.Errorf("scan queue candidate: %w", err)
		}
		cands = append(cands, h)
	}
	if err := c.Err(); err != nil {
		return nil, "", fmt.Errorf("read queue candidates: %w", err)
	}
	eligible := func(id int64) bool {
		if statusOf[id] != "ready" {
			return false // became ready between the two statements: no facts, not a head
		}
		f := facts[id]
		f.QueueHead = false
		return lightFor("ready", f).Class == "none"
	}
	for id, lane := range pickQueueHeads(cands, eligible) {
		f := facts[id]
		f.QueueHead, f.Lane = true, lane
		facts[id] = f
	}
	// SWT-57 L2: the queue section's order is this statement's order
	// (tools.TaskQueueOrder, never a second spelling): each ready task's 1-based
	// position among the candidates.
	for i, h := range cands {
		if statusOf[h.ID] != "ready" {
			continue // became ready between the two statements: no facts, unranked
		}
		f := facts[h.ID]
		f.QueueRank = i + 1
		facts[h.ID] = f
	}
	return facts, renderedAt, nil
}

// ---- /tasks/{id} detail ---------------------------------------------------------

type taskDetail struct {
	taskRow
	Body       string
	Autonomy   string
	ParentLink string
	Parent     *taskRow
	Children   []taskRow
	Deps       []taskRow // dependencies with their statuses
	Events     []eventRow
	Feedback   []feedbackRow
	Deliveries []deliveryRow
	Refs       []refRow
}

type eventRow struct {
	ID      int64
	Type    string
	Payload string
	At      string
}

type feedbackRow struct {
	ID       int64
	Question string
	Answer   string
	Status   string
}

type refRow struct {
	System string
	Key    string
	URL    string
}

func (s *Server) showTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var d taskDetail
	var parentID *int64
	var planOrder *int
	err := s.pool.QueryRow(r.Context(),
		`SELECT t.id, COALESCE(p.slug,''), COALESCE(t.subproject,''), t.parent_id, t.title,
		        COALESCE(t.body,''), t.status, t.assignee_type, COALESCE(t.worker_type,''),
		        COALESCE(t.autonomy,''), t.priority, t.plan_order, COALESCE(t.updated_at::text,'')
		 FROM tasks t JOIN projects p ON p.id = t.project_id WHERE t.id = $1`, id).
		Scan(&d.ID, &d.Project, &d.Subproject, &parentID, &d.Title, &d.Body, &d.Status,
			&d.AssigneeType, &d.WorkerType, &d.Autonomy, &d.Priority, &planOrder, &d.UpdatedAt)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if parentID != nil {
		d.ParentLink = fmt.Sprintf("%d", *parentID)
	}
	if planOrder != nil {
		d.PlanOrder = fmt.Sprintf("%d", *planOrder)
	}

	scanTasks := func(q string, args ...any) []taskRow {
		rows, err := s.pool.Query(r.Context(), q, args...)
		if err != nil {
			return nil
		}
		defer rows.Close()
		var out []taskRow
		for rows.Next() {
			var t taskRow
			if rows.Scan(&t.ID, &t.Title, &t.Status, &t.AssigneeType) == nil {
				out = append(out, t)
			}
		}
		return out
	}
	d.Children = scanTasks(`SELECT id, title, status, assignee_type FROM tasks WHERE parent_id=$1 ORDER BY plan_order NULLS LAST, id`, d.ID)
	d.Deps = scanTasks(`SELECT t.id, t.title, t.status, t.assignee_type
	                    FROM task_dependencies dep JOIN tasks t ON t.id = dep.depends_on_task_id
	                    WHERE dep.task_id=$1 ORDER BY t.id`, d.ID)
	if parentID != nil {
		if p := scanTasks(`SELECT id, title, status, assignee_type FROM tasks WHERE id=$1`, *parentID); len(p) == 1 {
			d.Parent = &p[0]
		}
	}

	if rows, err := s.pool.Query(r.Context(),
		`SELECT id, event_type, payload::text, COALESCE(created_at::text,'')
		 FROM task_events WHERE task_id=$1 ORDER BY id DESC LIMIT 50`, d.ID); err == nil {
		defer rows.Close()
		for rows.Next() {
			var e eventRow
			if rows.Scan(&e.ID, &e.Type, &e.Payload, &e.At) == nil {
				d.Events = append(d.Events, e)
			}
		}
	}
	if rows, err := s.pool.Query(r.Context(),
		`SELECT id, question, COALESCE(answer,''), status FROM feedback_requests
		 WHERE task_id=$1 ORDER BY id DESC`, d.ID); err == nil {
		defer rows.Close()
		for rows.Next() {
			var f feedbackRow
			if rows.Scan(&f.ID, &f.Question, &f.Answer, &f.Status) == nil {
				d.Feedback = append(d.Feedback, f)
			}
		}
	}
	if rows, err := s.pool.Query(r.Context(),
		`SELECT id, channel, status, COALESCE(subject,''), COALESCE(body,''), COALESCE(sent_at::text,'')
		 FROM deliveries WHERE task_id=$1 ORDER BY id DESC`, d.ID); err == nil {
		defer rows.Close()
		for rows.Next() {
			var dl deliveryRow
			if rows.Scan(&dl.ID, &dl.Channel, &dl.Status, &dl.Subject, &dl.Body, &dl.SentAt) == nil {
				d.Deliveries = append(d.Deliveries, dl)
			}
		}
	}
	if rows, err := s.pool.Query(r.Context(),
		`SELECT system, external_key, COALESCE(external_url,'') FROM external_refs
		 WHERE task_id=$1 ORDER BY id`, d.ID); err == nil {
		defer rows.Close()
		for rows.Next() {
			var ref refRow
			if rows.Scan(&ref.System, &ref.Key, &ref.URL) == nil {
				d.Refs = append(d.Refs, ref)
			}
		}
	}

	if err := s.tmpl.ExecuteTemplate(w, "task.html", d); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// ---- /briefs -------------------------------------------------------------------

type briefRow struct {
	ID    int64
	Title string
	Body  string
	At    string
}

func (s *Server) listBriefs(w http.ResponseWriter, r *http.Request) {
	// The title predicate is exactly the key R7 dedups on — it cannot drift
	// from the producer without the producer changing first.
	rows, err := s.pool.Query(r.Context(),
		`SELECT id, title, COALESCE(body,''), COALESCE(created_at::text,'')
		 FROM tasks WHERE title LIKE 'Morning brief %' ORDER BY id DESC LIMIT 60`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var briefs []briefRow
	for rows.Next() {
		var b briefRow
		if err := rows.Scan(&b.ID, &b.Title, &b.Body, &b.At); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		briefs = append(briefs, b)
	}
	if err := s.tmpl.ExecuteTemplate(w, "briefs.html", briefs); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// ---- exports -------------------------------------------------------------------

func (s *Server) exportCSV(w http.ResponseWriter, r *http.Request) {
	rows, err := s.boardRows(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="tasks.csv"`)
	if err := WriteTasksCSV(w, rows); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) exportJSON(w http.ResponseWriter, r *http.Request) {
	rows, err := s.boardRows(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := WriteTasksJSON(w, rows); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// ---- /plans review --------------------------------------------------------------

type planListRow struct {
	ID         int64
	Project    string
	SourcePath string
	Status     string
	CreatedAt  string
	DecidedBy  string
}

type planNode struct {
	Ref           string   `json:"ref"`
	ParentRef     *string  `json:"parent_ref"`
	Title         string   `json:"title"`
	Body          string   `json:"body"`
	AssigneeType  string   `json:"assignee_type"`
	Subproject    *string  `json:"subproject"`
	WorkerType    *string  `json:"worker_type"`
	Priority      int      `json:"priority"`
	DependsOnRefs []string `json:"depends_on_refs"`
	Confidence    float64  `json:"confidence"`
	Notes         string   `json:"notes"`
	PlanOrder     int      `json:"plan_order"`
	Depth         int      `json:"-"`
}

type planDetail struct {
	planListRow
	Summary    string
	Validation []string
	Nodes      []planNode
	Flash      string
}

func (s *Server) listPlans(w http.ResponseWriter, r *http.Request) {
	q := `SELECT pi.id, COALESCE(p.slug,''), pi.source_path, pi.status,
	             COALESCE(pi.created_at::text,''), COALESCE(pi.decided_by,'')
	      FROM plan_imports pi JOIN projects p ON p.id = pi.project_id`
	args := []any{}
	if v := r.URL.Query().Get("status"); v != "" {
		q += ` WHERE pi.status = $1`
		args = append(args, v)
	}
	q += ` ORDER BY pi.id DESC LIMIT 100`
	rows, err := s.pool.Query(r.Context(), q, args...)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var plans []planListRow
	for rows.Next() {
		var p planListRow
		if err := rows.Scan(&p.ID, &p.Project, &p.SourcePath, &p.Status, &p.CreatedAt, &p.DecidedBy); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		plans = append(plans, p)
	}
	if err := s.tmpl.ExecuteTemplate(w, "plans.html", map[string]any{
		"Plans": plans, "Flash": r.URL.Query().Get("flash"),
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) showPlan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var d planDetail
	var fields []byte
	err := s.pool.QueryRow(r.Context(),
		`SELECT pi.id, COALESCE(p.slug,''), pi.source_path, pi.status,
		        COALESCE(pi.created_at::text,''), COALESCE(pi.decided_by,''), e.fields
		 FROM plan_imports pi
		 JOIN projects p ON p.id = pi.project_id
		 JOIN ai_extractions e ON e.id = pi.ai_extraction_id
		 WHERE pi.id = $1`, id).
		Scan(&d.ID, &d.Project, &d.SourcePath, &d.Status, &d.CreatedAt, &d.DecidedBy, &fields)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var doc struct {
		Summary    string     `json:"summary"`
		Tasks      []planNode `json:"tasks"`
		Validation []string   `json:"validation"`
	}
	if err := json.Unmarshal(fields, &doc); err != nil {
		http.Error(w, "corrupt extraction fields: "+err.Error(), http.StatusInternalServerError)
		return
	}
	d.Summary, d.Validation = doc.Summary, doc.Validation
	d.Flash = r.URL.Query().Get("flash")

	// Indentation from parent refs (tree is parents-first).
	depth := map[string]int{}
	for _, n := range doc.Tasks {
		if n.ParentRef != nil {
			n.Depth = depth[*n.ParentRef] + 1
		}
		depth[n.Ref] = n.Depth
		d.Nodes = append(d.Nodes, n)
	}

	if err := s.tmpl.ExecuteTemplate(w, "plan.html", d); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// dismissTaskAction is POST /tasks/{id}/dismiss (SWT-31): the board's dismiss
// verb. One executor call (task_dismiss — humanOnly, closes the task and writes
// the typed task_dismissals label in one transaction), no SQL of its own
// (invariant 3). The redirect target is REBUILT from the four known filter keys
// via url.Values — never echoed from a caller-supplied string (the safeNext
// lesson in auth.go) — so a dismissal from a filtered board lands back on the
// same filtered board, minus the dismissed row.
func (s *Server) dismissTaskAction(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	taskID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || taskID <= 0 {
		http.Error(w, "bad task id", http.StatusBadRequest)
		return
	}
	raw, err := json.Marshal(map[string]any{
		"task_id":     taskID,
		"reason_code": r.PostFormValue("reason_code"),
		"note":        r.PostFormValue("note"),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.executeTask(w, r, "task_dismiss", string(raw), taskID, boardBack(r))
}

// closeTaskAction is POST /tasks/{id}/close (SWT-51): the board's Done verb.
// One executor call (the existing task_close, as dashboard:{user}), no SQL of
// its own (invariant 3), and no dismissal label — a finished task is not a
// labelled negative. The optional note folds into task_close's required
// reason (D1). The template renders the button on human rows only (D2); the
// executor's closeTransition owns the refusable status set (D3).
func (s *Server) closeTaskAction(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	taskID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || taskID <= 0 {
		http.Error(w, "bad task id", http.StatusBadRequest)
		return
	}
	reason := "done on the board"
	if note := strings.TrimSpace(r.PostFormValue("note")); note != "" {
		reason += ": " + note
	}
	raw, err := json.Marshal(map[string]any{"task_id": taskID, "reason": reason})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.executeTask(w, r, "task_close", string(raw), taskID, boardBack(r))
}

// boardBack is D5's one spelling of the board redirect's keys: boardKeys (the
// four filters plus SWT-52's refresh, so a Done or Dismiss made with
// auto-refresh on lands back on an auto-refreshing board), re-encoded from the
// POSTED form via url.Values, empty ones omitted — never echoed from the
// request's query string or any other caller-supplied string (the safeNext
// lesson in auth.go). Every board verb calls it, so a key cannot survive one
// verb's redirect and not another's.
func boardBack(r *http.Request) url.Values {
	back := url.Values{}
	for _, k := range boardKeys {
		if v := r.PostFormValue(k); v != "" {
			back.Set(k, v)
		}
	}
	return back
}

// boardRefreshURLs is the GET side of boardKeys (SWT-52 D15, criterion 30): it
// re-encodes the non-empty boardKeys values of the board's query via
// url.Values. toggle flips refresh (on → absent, anything else → on); reload
// keeps refresh=on. Neither carries flash (a verb's flash shows once) or any
// key outside boardKeys. Pure.
func boardRefreshURLs(q url.Values) (toggle, reload string) {
	tv, rv := url.Values{}, url.Values{}
	for _, k := range boardKeys {
		if k == "refresh" {
			if q.Get(k) != "on" {
				tv.Set(k, "on")
			}
			rv.Set(k, "on")
			continue
		}
		if v := q.Get(k); v != "" {
			tv.Set(k, v)
			rv.Set(k, v)
		}
	}
	return boardURL(tv), boardURL(rv)
}

// boardFilter is one active advanced filter, shown on the board's first line.
type boardFilter struct{ Key, Value string }

// boardAdvanced is the first line's advanced-filter marker (SWT-57 L5), a third
// iterator of boardKeys beside boardBack and boardRefreshURLs: every key but
// project and refresh is advanced, so a filter key added to boardKeys later is
// advanced automatically. active lists the non-empty ones in boardKeys order;
// clearURL drops them, keeping project and refresh=on (only when on). Neither
// carries flash or any key outside boardKeys. Pure.
func boardAdvanced(q url.Values) (active []boardFilter, clearURL string) {
	keep := url.Values{}
	for _, k := range boardKeys {
		v := q.Get(k)
		switch k {
		case "project":
			if v != "" {
				keep.Set(k, v)
			}
		case "refresh":
			if v == "on" {
				keep.Set(k, v)
			}
		default:
			if v != "" {
				active = append(active, boardFilter{Key: k, Value: v})
			}
		}
	}
	if len(active) == 0 {
		return nil, ""
	}
	return active, boardURL(keep)
}

func boardURL(v url.Values) string {
	if len(v) == 0 {
		return "/tasks"
	}
	return "/tasks?" + v.Encode()
}

// planAction runs approve/reject_plan_import through the executor with the
// session actor and redirects back to the plan list.
func (s *Server) planAction(tool string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		args := fmt.Sprintf(`{"plan_import_id":%s}`, r.PathValue("id"))
		s.executeTo(w, r, tool, args, "/plans")
	})
}
