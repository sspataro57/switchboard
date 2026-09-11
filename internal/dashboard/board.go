package dashboard

// SWT-10 dashboard slices: the full board (/tasks — queues are FILTERS on the
// one tasks table, never tables), task detail, briefs, plan-import review
// (/plans) and the deterministic exports. Reads are direct SQL (dashboard
// idiom); the ONLY actions are approve/reject_plan_import, both through the
// executor (invariant 3).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sspataro57/switchboard/internal/orchestrator"
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
}

type statusColumn struct {
	Status string
	Tasks  []taskRow
}

type boardData struct {
	Columns  []statusColumn
	Projects []string
	Filters  map[string]string
	Flash    string
	// OrchAlert is set only when the orchestrator verdict is not ok (SWT-41
	// D5): the board is where Salvador looks, /funnel is where he investigates.
	// A failing health query leaves it nil — the board never breaks on health.
	OrchAlert *orchestrator.HealthState
}

// boardStatusOrder pins the column order to the status machine.
var boardStatusOrder = []string{
	"holding", "ready", "blocked", "claimed", "in_progress", "needs_feedback",
	"pr_open", "awaiting_ci", "awaiting_merge", "done_locally", "delivered", "closed",
}

// boardQuery builds the filtered board select (shared by /tasks and the
// exports — same filters, id ASC).
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
		conds = append(conds, "t.status <> 'closed'") // closed hidden by default
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

	byStatus := map[string][]taskRow{}
	for _, t := range rows {
		tr := taskRow{
			ID: t.ID, Project: t.Project, Subproject: t.Subproject,
			Title: t.Title, Status: t.Status, AssigneeType: t.AssigneeType,
			WorkerType: t.WorkerType, Priority: t.Priority, UpdatedAt: t.UpdatedAt,
			ReopenedAfterDismissal: markers[t.ID],
		}
		if t.ParentID != nil {
			tr.ParentID = fmt.Sprintf("%d", *t.ParentID)
		}
		if t.PlanOrder != nil {
			tr.PlanOrder = fmt.Sprintf("%d", *t.PlanOrder)
		}
		byStatus[t.Status] = append(byStatus[t.Status], tr)
	}
	data := boardData{Flash: r.URL.Query().Get("flash"), Filters: map[string]string{
		"project":       r.URL.Query().Get("project"),
		"status":        r.URL.Query().Get("status"),
		"assignee_type": r.URL.Query().Get("assignee_type"),
		"subproject":    r.URL.Query().Get("subproject"),
	}}
	for _, st := range boardStatusOrder {
		if len(byStatus[st]) > 0 {
			data.Columns = append(data.Columns, statusColumn{Status: st, Tasks: byStatus[st]})
			delete(byStatus, st)
		}
	}
	for st, ts := range byStatus { // unknown statuses still render
		data.Columns = append(data.Columns, statusColumn{Status: st, Tasks: ts})
	}

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

	if h, err := orchestrator.Health(r.Context(), s.pool, time.Now()); err == nil && h.Verdict != orchestrator.VerdictOK {
		data.OrchAlert = &h
	}

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
	back := url.Values{}
	for _, k := range []string{"project", "status", "assignee_type", "subproject"} {
		if v := r.PostFormValue(k); v != "" {
			back.Set(k, v)
		}
	}
	s.executeTask(w, r, "task_dismiss", string(raw), taskID, back)
}

// planAction runs approve/reject_plan_import through the executor with the
// session actor and redirects back to the plan list.
func (s *Server) planAction(tool string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		args := fmt.Sprintf(`{"plan_import_id":%s}`, r.PathValue("id"))
		s.executeTo(w, r, tool, args, "/plans")
	})
}
