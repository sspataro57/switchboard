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
	"io/fs"
	"mime"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/tools"
)

//go:embed templates/*.html
var templateFS embed.FS

// staticFS carries the board's vendored assets (SWT-67 B17): the fonts with
// their licences, the icons and the web app manifest. Embedded, so the binary
// still carries everything and nothing new lands in the image build.
//
//go:embed static
var staticFS embed.FS

func init() {
	// Go's mime table has neither extension; without them the file server
	// would sniff the fonts as octet-stream and the manifest as text/plain.
	_ = mime.AddExtensionType(".woff2", "font/woff2")
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

// Exec is the executor seam.
type Exec interface {
	Execute(ctx context.Context, call executor.Call) (executor.Result, error)
}

type Server struct {
	pool *pgxpool.Pool
	ex   Exec
	auth *Auth
	tmpl *template.Template
	// live is the process's board hub (SWT-89); nil answers the stream 503 and
	// open boards poll in place.
	live *BoardHub
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

	// The vendored assets are served WITHOUT a session, deliberately: a browser
	// fetches a manifest, its icons and @font-face files without credentials,
	// so s.auth.Require would 302 them to the login page and silently break the
	// fonts and the install path. The bytes are public font files, two icons
	// and a static manifest — no task data. fs.Sub over an embedded FS cannot
	// traverse out of static/.
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(fmt.Sprintf("dashboard: static sub-FS: %v", err)) // embed guarantees the directory
	}
	mux.Handle("GET /static/{path...}", http.StripPrefix("/static/", http.FileServerFS(staticSub)))

	mux.Handle("GET /", s.auth.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/tasks", http.StatusFound)
	})))
	// SWT-10: full board, task detail, briefs, plan review, exports.
	mux.Handle("GET /tasks", s.auth.Require(http.HandlerFunc(s.listTasks)))
	// SWT-89: the board's change stream. The literal wins over {id} (Go 1.22 mux).
	mux.Handle("GET /tasks/stream", s.auth.Require(http.HandlerFunc(s.boardStream)))
	mux.Handle("GET /tasks/{id}", s.auth.Require(http.HandlerFunc(s.showTask)))
	// SWT-67 B21: the full-screen shell around the board (kiosk.go).
	mux.Handle("GET /kiosk", s.auth.Require(http.HandlerFunc(s.showKiosk)))
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
	mux.Handle("POST /tasks/{id}/close", s.auth.Require(http.HandlerFunc(s.closeTaskAction)))
	mux.Handle("POST /tasks/{id}/requeue", s.auth.Require(http.HandlerFunc(s.requeueTaskAction)))
	mux.Handle("POST /tasks/{id}/attach", s.auth.Require(http.HandlerFunc(s.attachTaskAction)))
	mux.Handle("POST /plans/{id}/approve", s.auth.Require(s.planAction("approve_plan_import")))
	mux.Handle("POST /plans/{id}/reject", s.auth.Require(s.planAction("reject_plan_import")))
	mux.Handle("GET /export/tasks.csv", s.auth.Require(http.HandlerFunc(s.exportCSV)))
	mux.Handle("GET /export/tasks.json", s.auth.Require(http.HandlerFunc(s.exportJSON)))
	mux.Handle("GET /deliveries", s.auth.Require(http.HandlerFunc(s.listDeliveries)))
	mux.Handle("POST /deliveries/{id}/edit", s.auth.Require(http.HandlerFunc(s.actionEdit)))
	mux.Handle("POST /deliveries/{id}/approve", s.auth.Require(http.HandlerFunc(s.approveAction)))
	// SWT-43: Deny / Redo, one form, the redraft bit chosen by the button.
	mux.Handle("POST /deliveries/{id}/reject", s.auth.Require(http.HandlerFunc(s.actionReject)))
	mux.Handle("POST /deliveries/{id}/send", s.auth.Require(s.action("send_delivery")))
	mux.Handle("POST /deliveries/{id}/mark-sent", s.auth.Require(s.action("mark_delivery_sent")))
	// Resolves a stuck slack_reply 'sending' row the other way: a human looked in
	// Slack and the message is NOT there (SWT-12 criterion 12).
	mux.Handle("POST /deliveries/{id}/mark-failed", s.auth.Require(s.action("mark_delivery_failed")))
	mux.Handle("POST /flags/sending-frozen", s.auth.Require(http.HandlerFunc(s.actionFreeze)))
	return staticCacheHeaders(mux, staticSub)
}

// staticCacheHeaders guards the /static/ prefix and passes every other request
// through untouched. Only a real embedded FILE is served, with an immutable
// Cache-Control (nothing under /static/ changes under its name: a font's
// weight is in its filename and the set is replaced together). Anything else
// under the prefix — a directory, a miss — is a plain 404 that carries no cache
// header, so a probed-then-added name is never a year-long cached miss, and the
// open route lists nothing.
func staticCacheHeaders(next http.Handler, static fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The board frames itself (/kiosk) and nothing else should: its POST
		// verbs are one click each. SAMEORIGIN, never DENY — DENY breaks /kiosk.
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		if name, ok := strings.CutPrefix(r.URL.Path, "/static/"); ok {
			fi, err := fs.Stat(static, name)
			if err != nil || fi.IsDir() {
				http.NotFound(w, r)
				return
			}
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
		}
		next.ServeHTTP(w, r)
	})
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
	// Cc (SWT-69) is the gmail row's carbon-copy list, shown beside From/To so
	// Salvador approves the recipients he sees. CcText is the same list as the
	// edit form's one text input.
	Cc     []string
	CcText string
	// SentCopyIngested (SWT-71): the message this row composed, by its own
	// Message-ID, is in the ingested mailbox — the proof send_delivery needs to
	// FINISH a row stuck in `sending`. A body-prefix confirmation is not it.
	SentCopyIngested bool
	// QueuedAt / QueueJobID (SWT-76): the leaf ACCEPTED this send (202) while
	// its browser was busy and will click it in the next gap; the row is
	// 'sending' with its attempt unsettled, and the next export confirms it.
	// LeaseHeld is true while that attempt is younger than the send lease:
	// mark_delivery_failed refuses inside it, so "Not in Slack" is not offered
	// (a button that can only error teaches distrust); "It's in Slack" stays,
	// because recording a send that happened is always safe.
	QueuedAt   string
	QueueJobID string
	LeaseHeld  bool
	// ContentHash is tools.DeliveryContentHash of the Subject/Body this page
	// renders. The Approve form posts it back as expect_content_hash, so an
	// edit made after the page loaded (a session's update_delivery) makes the
	// approve refuse instead of passing words Salvador never saw (SWT-44).
	ContentHash string
	// Where the send would go, shown before approval (SWT-44 review). Gmail:
	// From / To / ThreadSubject from tools.ResolveGmailRoute — the send path's
	// own resolution, never a second spelling. Any other channel: TargetRef.
	// Unresolvable parts read "(unresolved)".
	From, To, ThreadSubject string
	TargetRef               string
}

const unresolved = "(unresolved)"

// sendable are the statuses a gmail send can still start from (approve covers
// failed-without-id). A sent row's route is NOT recomputed: the thread may
// have a newer inbound message now, and showing it would misstate where the
// mail went.
var sendable = map[string]bool{"drafted": true, "approved": true, "failed": true}

// resolveDestination fills d's destination fields (SWT-44 review).
func (s *Server) resolveDestination(ctx context.Context, d *deliveryRow, fromAcct, threadID *int64) {
	if d.Channel != "gmail" {
		if d.TargetRef == "" {
			d.TargetRef = unresolved
		}
		return
	}
	if !sendable[d.Status] {
		return
	}
	d.From, d.To, d.ThreadSubject = unresolved, unresolved, unresolved
	if fromAcct == nil || threadID == nil {
		return
	}
	// On error the route still carries what resolved before the failure, so a
	// thread with nothing inbound still shows its From and subject.
	r, _ := tools.ResolveGmailRoute(ctx, s.pool, *fromAcct, *threadID)
	if r.From != "" {
		d.From = r.From
	}
	if r.To != "" {
		d.To = r.To
	}
	if r.Subject != "" {
		d.ThreadSubject = r.Subject
	}
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
	             d.from_account_id, d.thread_id, COALESCE(d.target_ref,''),
	             COALESCE(d.rejection_note,''), d.redraft_requested_at IS NOT NULL, d.cc,
	             EXISTS (SELECT 1 FROM normalized_messages nm
	                      WHERE nm.channel = 'gmail' AND nm.external_message_id = d.sent_external_id
	                        AND nm.direction = 'outbound'),
	             COALESCE(d.send_queued_at::text,''), COALESCE(d.send_queue_job_id,''),
	             d.send_attempted_at, d.send_settled_at
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
	type routeRef struct{ fromAcct, threadID *int64 }
	var refs []routeRef
	for rows.Next() {
		var d deliveryRow
		var ref routeRef
		var attemptedAt, settledAt *time.Time
		if err := rows.Scan(&d.ID, &d.TaskID, &d.TaskTitle, &d.Channel, &d.Status,
			&d.Subject, &d.Body, &d.CreatedBy, &d.SentAt, &d.ConfirmedAt, &d.Error,
			&d.StartsAt, &d.EndsAt, &ref.fromAcct, &ref.threadID, &d.TargetRef,
			&d.RejectionNote, &d.RedraftRequested, &d.Cc, &d.SentCopyIngested,
			&d.QueuedAt, &d.QueueJobID, &attemptedAt, &settledAt); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// The lease comparison lives here, beside the query: a template cannot
		// do time arithmetic, and the constant is the send path's own.
		d.LeaseHeld = settledAt == nil && attemptedAt != nil && time.Since(*attemptedAt) < tools.SendAttemptLease
		d.CcText = strings.Join(d.Cc, ", ")
		d.ContentHash = tools.DeliveryContentHash(d.Subject, d.Body, d.Cc)
		data.Deliveries = append(data.Deliveries, d)
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rows.Close() // release the connection before the per-row route reads
	for i := range data.Deliveries {
		s.resolveDestination(r.Context(), &data.Deliveries[i], refs[i].fromAcct, refs[i].threadID)
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

// approveAction approves a delivery bound to the words this page rendered
// (SWT-44): the form's content_hash goes through as expect_content_hash, and
// approve_delivery refuses if the row changed since. json.Marshal, not
// Sprintf: a form value is caller text and must not be able to add or replace
// keys (delivery_id included). The dashboard is the review surface, so the
// hash is REQUIRED here (SWT-44 second review): a POST without one — a page
// rendered before the deploy, or a crafted POST — is refused before the
// executor rather than becoming an approve bound to nothing. approve_delivery
// itself keeps the hash optional for opsctl and the full-profile MCP.
func (s *Server) approveAction(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h := strings.TrimSpace(r.PostFormValue("content_hash"))
	if h == "" {
		flash := "approve refused: this page did not say which words you reviewed; reload the page and review it again"
		http.Redirect(w, r, "/deliveries?flash="+template.URLQueryEscaper(flash), http.StatusSeeOther)
		return
	}
	payload := map[string]any{"delivery_id": jsonNum(r.PathValue("id")), "expect_content_hash": h}
	raw, err := json.Marshal(payload)
	if err != nil { // a non-numeric id is not a json.Number
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.execute(w, r, "approve_delivery", string(raw))
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
	// SWT-69 D10: unlike body and subject above, cc is forwarded whenever the
	// POST CONTAINS the key, and an emptied box means "clear". For words, an
	// empty box keeping the old value is a known residual (SWT-61); for a
	// recipient list it would be worse — Salvador deletes Katie from the box,
	// the save succeeds, and the email goes to her anyway.
	if r.PostForm.Has("cc") {
		payload["cc"] = splitCcInput(r.PostFormValue("cc"))
	}
	raw, _ := json.Marshal(payload)
	s.execute(w, r, "update_delivery", string(raw))
}

// splitCcInput turns the edit form's one text box into cc entries. A proper
// address list ("Name <a@x>, b@y") is parsed as one; anything else is split on
// commas, semicolons and line breaks and handed over as typed, so the executor's
// own validation names what is wrong. Always non-nil: an empty box is "clear".
func splitCcInput(v string) []string {
	out := []string{}
	if strings.TrimSpace(v) == "" {
		return out
	}
	// An empty parse is NOT a clear: RFC 5322 group syntax
	// ("undisclosed-recipients:;") parses to zero addresses with no error, and
	// forwarding that as [] would wipe the list behind a success flash.
	if list, err := mail.ParseAddressList(v); err == nil && len(list) > 0 {
		for _, a := range list {
			out = append(out, a.Address)
		}
		return out
	}
	for _, part := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ';' || r == '\n' || r == '\r' }) {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// actionReject is Deny (redraft=false) and Redo (redraft=true). The note is
// free text, so the args are built with json.Marshal, never Sprintf.
//
// Like approveAction, the verdict is bound to the words this page rendered
// (SWT-43 review): the form's content_hash goes through as
// expect_content_hash, and a POST without one is refused before the executor.
// reject_delivery keeps the hash optional for opsctl.
//
// The note reaches the drafts worker's prompt (quoted as data, see
// drafts.renderUser). It needs no authorization beyond reject_delivery's
// humanOnly: this route sits behind Keycloak OIDC, and the note is Salvador's
// own text.
func (s *Server) actionReject(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h := strings.TrimSpace(r.PostFormValue("content_hash"))
	if h == "" {
		flash := "reject refused: this page did not say which words you reviewed; reload the page and review it again"
		http.Redirect(w, r, "/deliveries?flash="+template.URLQueryEscaper(flash), http.StatusSeeOther)
		return
	}
	payload := map[string]any{
		"delivery_id":         jsonNum(r.PathValue("id")),
		"redraft":             r.PostFormValue("redraft") == "true",
		"expect_content_hash": h,
	}
	if v := r.PostFormValue("note"); v != "" {
		payload["note"] = v
	}
	raw, err := json.Marshal(payload)
	if err != nil { // a non-numeric id is not a json.Number
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
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

	out, err := s.ex.Execute(ctx, executor.Call{Tool: tool, Actor: actor, Args: []byte(args)})
	flash := tool + " ok"
	if err != nil {
		flash = err.Error()
	} else if tool == "send_delivery" {
		flash = sendFlash(out.Output, flash)
	}
	http.Redirect(w, r, back+"?flash="+template.URLQueryEscaper(flash), http.StatusSeeOther)
}

// sendFlash reads send_delivery's result: a Slack send the leaf QUEUED (SWT-76)
// is a success that leaves the row in 'sending', and "send_delivery ok" would
// not say why. Any other result keeps the default.
func sendFlash(output []byte, fallback string) string {
	var r struct {
		DeliveryID int64  `json:"delivery_id"`
		Queued     bool   `json:"queued"`
		JobID      string `json:"job_id"`
	}
	if err := json.Unmarshal(output, &r); err != nil || !r.Queued {
		return fallback
	}
	return fmt.Sprintf("delivery %d queued on the bridge (job %s): the browser was busy; the leaf clicks it in "+
		"the next gap and the next export confirms it", r.DeliveryID, r.JobID)
}

func jsonNum(s string) json.Number { return json.Number(s) }
