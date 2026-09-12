// Package dashboard is the SWT-8 deliveries slice (SPEC 08-draft-deliveries):
// a single Go+HTMX page for approve/edit/send. Reads are direct SQL; every
// ACTION goes through the executor (invariant 3). The full board is step 10.
package dashboard

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
)

//go:embed templates/*.html
var templateFS embed.FS

// Exec is the executor seam.
type Exec interface {
	Execute(ctx context.Context, call executor.Call) (executor.Result, error)
}

type Server struct {
	pool *pgxpool.Pool
	ex   Exec
	auth *Auth
	tmpl *template.Template
}

func NewServer(pool *pgxpool.Pool, ex Exec, auth *Auth) (*Server, error) {
	tmpl, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Server{pool: pool, ex: ex, auth: auth, tmpl: tmpl}, nil
}

// Handler builds the mux. All /deliveries* routes require a session.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	s.auth.Routes(mux)

	mux.Handle("GET /", s.auth.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/tasks", http.StatusFound)
	})))
	// SWT-10: full board, task detail, briefs, plan review, exports.
	mux.Handle("GET /tasks", s.auth.Require(http.HandlerFunc(s.listTasks)))
	mux.Handle("GET /tasks/{id}", s.auth.Require(http.HandlerFunc(s.showTask)))
	mux.Handle("GET /briefs", s.auth.Require(http.HandlerFunc(s.listBriefs)))
	// Ingestion visibility, split across two read-only pages (SWT-29):
	// /sources answers "how much is stored, per account" (lifetime totals,
	// channels); /funnel answers "is it moving, and what happened to it" —
	// connector freshness (incl. the calendar's real AVAIL_MAX_SYNC_AGE
	// verdict), the intake trend, capture attribution and the classify shadow
	// summary. Together they distinguish a quiet board from a dead connector
	// while triage is still in shadow mode. Both read the raw/normalized
	// tables; neither executes anything.
	mux.Handle("GET /sources", s.auth.Require(http.HandlerFunc(s.listSources)))
	mux.Handle("GET /funnel", s.auth.Require(http.HandlerFunc(s.showFunnel)))
	mux.Handle("GET /plans", s.auth.Require(http.HandlerFunc(s.listPlans)))
	mux.Handle("GET /plans/{id}", s.auth.Require(http.HandlerFunc(s.showPlan)))
	// SWT-31: the board's first verb. Auth-required like every POST; the
	// handler (board.go) rebuilds the filter query itself — criterion 17.
	mux.Handle("POST /tasks/{id}/dismiss", s.auth.Require(http.HandlerFunc(s.dismissTaskAction)))
	mux.Handle("POST /plans/{id}/approve", s.auth.Require(s.planAction("approve_plan_import")))
	mux.Handle("POST /plans/{id}/reject", s.auth.Require(s.planAction("reject_plan_import")))
	mux.Handle("GET /export/tasks.csv", s.auth.Require(http.HandlerFunc(s.exportCSV)))
	mux.Handle("GET /export/tasks.json", s.auth.Require(http.HandlerFunc(s.exportJSON)))
	mux.Handle("GET /deliveries", s.auth.Require(http.HandlerFunc(s.listDeliveries)))
	mux.Handle("POST /deliveries/{id}/edit", s.auth.Require(http.HandlerFunc(s.actionEdit)))
	mux.Handle("POST /deliveries/{id}/approve", s.auth.Require(s.action("approve_delivery")))
	// SWT-43: Deny / Redo, one form, the redraft bit chosen by the button.
	mux.Handle("POST /deliveries/{id}/reject", s.auth.Require(http.HandlerFunc(s.actionReject)))
	mux.Handle("POST /deliveries/{id}/send", s.auth.Require(s.action("send_delivery")))
	mux.Handle("POST /deliveries/{id}/mark-sent", s.auth.Require(s.action("mark_delivery_sent")))
	// Resolves a stuck slack_reply 'sending' row the other way: a human looked in
	// Slack and the message is NOT there (SWT-12 criterion 12).
	mux.Handle("POST /deliveries/{id}/mark-failed", s.auth.Require(s.action("mark_delivery_failed")))
	mux.Handle("POST /flags/sending-frozen", s.auth.Require(http.HandlerFunc(s.actionFreeze)))
	return mux
}

type deliveryRow struct {
	ID          int64
	TaskID      int64
	TaskTitle   string
	Channel     string
	Status      string
	Subject     string
	Body        string
	CreatedBy   string
	SentAt      string
	ConfirmedAt string
	Error       string
	// StartsAt/EndsAt are the booked interval, calendar rows only (SWT-28).
	// Under the auto tier the dashboard is the review surface: a human
	// checking what was booked must see WHEN.
	StartsAt string
	EndsAt   string
	// SWT-43: set on rejected rows only.
	RejectionNote    string
	RedraftRequested bool
}

type pageData struct {
	Deliveries []deliveryRow
	Status     string
	Frozen     bool
	Flash      string
}

func (s *Server) listDeliveries(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	q := `SELECT d.id, d.task_id, COALESCE(t.title,''), d.channel, d.status,
	             COALESCE(d.subject,''), COALESCE(d.body,''), COALESCE(d.created_by,''),
	             COALESCE(d.sent_at::text,''), COALESCE(d.confirmed_at::text,''), COALESCE(d.error,''),
	             COALESCE(d.starts_at::text,''), COALESCE(d.ends_at::text,''),
	             COALESCE(d.rejection_note,''), d.redraft_requested_at IS NOT NULL
	      FROM deliveries d LEFT JOIN tasks t ON t.id = d.task_id`
	args := []any{}
	if status != "" {
		q += ` WHERE d.status = $1`
		args = append(args, status)
	}
	q += ` ORDER BY d.id DESC LIMIT 100`

	rows, err := s.pool.Query(r.Context(), q, args...)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	data := pageData{Status: status, Flash: r.URL.Query().Get("flash")}
	for rows.Next() {
		var d deliveryRow
		if err := rows.Scan(&d.ID, &d.TaskID, &d.TaskTitle, &d.Channel, &d.Status,
			&d.Subject, &d.Body, &d.CreatedBy, &d.SentAt, &d.ConfirmedAt, &d.Error,
			&d.StartsAt, &d.EndsAt, &d.RejectionNote, &d.RedraftRequested); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		data.Deliveries = append(data.Deliveries, d)
	}

	var frozen *bool
	_ = s.pool.QueryRow(r.Context(),
		`SELECT (value->>'frozen')::boolean FROM ops_flags WHERE name='sending_frozen'`).Scan(&frozen)
	data.Frozen = frozen != nil && *frozen

	if err := s.tmpl.ExecuteTemplate(w, "deliveries.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// action runs a delivery-id tool through the executor with the session actor.
func (s *Server) action(tool string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		args := fmt.Sprintf(`{"delivery_id":%s}`, id)
		s.execute(w, r, tool, args)
	})
}

func (s *Server) actionEdit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	payload := map[string]any{"delivery_id": jsonNum(r.PathValue("id"))}
	if v := r.PostFormValue("body"); v != "" {
		payload["body"] = v
	}
	if v := r.PostFormValue("subject"); v != "" {
		payload["subject"] = v
	}
	raw, _ := json.Marshal(payload)
	s.execute(w, r, "update_delivery", string(raw))
}

// actionReject is Deny (redraft=false) and Redo (redraft=true). The note is
// free text, so the args are built with json.Marshal, never Sprintf.
func (s *Server) actionReject(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	payload := map[string]any{
		"delivery_id": jsonNum(r.PathValue("id")),
		"redraft":     r.PostFormValue("redraft") == "true",
	}
	if v := r.PostFormValue("note"); v != "" {
		payload["note"] = v
	}
	raw, _ := json.Marshal(payload)
	s.execute(w, r, "reject_delivery", string(raw))
}

func (s *Server) actionFreeze(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	frozen := r.PostFormValue("frozen") == "true"
	s.execute(w, r, "set_sending_frozen", fmt.Sprintf(`{"frozen":%v}`, frozen))
}

func (s *Server) execute(w http.ResponseWriter, r *http.Request, tool, args string) {
	s.executeTo(w, r, tool, args, "/deliveries")
}

// executeTask runs a task-scoped tool with executor.Call.TaskID set — the
// audit start/complete rows then carry the task (SWT-31 criterion 15; plain
// executeTo sets none) — and redirects to /tasks carrying the given
// query values plus the flash.
func (s *Server) executeTask(w http.ResponseWriter, r *http.Request, tool, args string, taskID int64, back url.Values) {
	actor := "dashboard:" + s.auth.User(r)
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	_, err := s.ex.Execute(ctx, executor.Call{Tool: tool, Actor: actor, Args: []byte(args), TaskID: &taskID})
	flash := tool + " ok"
	if err != nil {
		flash = err.Error()
	}
	back.Set("flash", flash)
	http.Redirect(w, r, "/tasks?"+back.Encode(), http.StatusSeeOther)
}

// executeTo runs a tool through the executor with the session actor and
// redirects to the given page with a flash.
func (s *Server) executeTo(w http.ResponseWriter, r *http.Request, tool, args, back string) {
	actor := "dashboard:" + s.auth.User(r)
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	_, err := s.ex.Execute(ctx, executor.Call{Tool: tool, Actor: actor, Args: []byte(args)})
	flash := tool + " ok"
	if err != nil {
		flash = err.Error()
	}
	http.Redirect(w, r, back+"?flash="+template.URLQueryEscaper(flash), http.StatusSeeOther)
}

func jsonNum(s string) json.Number { return json.Number(s) }
