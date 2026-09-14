//go:build integration

package dashboard_test

// Integration tests for SWT-51 (docs/tickets/board-done-button_SPEC.md) —
// acceptance criteria 10-18. The REAL dashboard.Server runs under httptest with
// dev-login session auth, the REAL executor registry and the REAL policy matrix;
// the close is made by POSTing the board's Done form. Build-tagged `integration`
// AND env-gated on DATABASE_URL. NO LLM, NO network, NO orchestrator running
// (dependents are asserted UNTOUCHED — unblocking is the orchestrator's drain).
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_isodone?sslmode=disable' \
//	  TZ=UTC go test -tags integration -p 1 -count=1 -run BoardDone ./internal/dashboard/
//
// GREENFIELD NOTE — EXPECTED RED: POST /tasks/{id}/close is not registered, so
// the POST is a 405 (the path matches only the GET-only `GET /` pattern) and
// dnDone fails before any assertion; the render test finds no /close action.
//
// CLEANUP PACT. Joins the dashboard suite's shape (dashGuard / dashPool /
// newDashServer / get / snippet in dashboard_integration_test.go; bdInsID /
// bdCount in board_dismiss_integration_test.go — same package) with its own
// test-owned prefix `itest-done-%`, FK-ordered and rerunnable.
// audit_events.task_id FKs tasks with no cascade, and this route FILLS it
// (criterion 10), so policy_decisions → audit_events go before tasks (the
// SWT-31/SWT-37 landmine). task_dismissals cascades from tasks but is deleted
// explicitly anyway.
//
// MUTATIONS THAT MUST TURN THIS FILE RED:
//   - the handler calls task_dismiss → ClosesAudits (a task_dismissals row, the tool name).
//   - executeTo instead of executeTask → ClosesAudits (no audit row with the task id).
//   - drop the template conditional → OnlyHumanRowsRenderDone.
//   - the handler touches dependents → DependentsAreTheOrchestratorsJob.

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/promote"
)

const (
	dnSlug   = "itest-done-proj"
	dnClient = "itest-done-client"
	dnAcct   = "itest-done-a@local"
	dnModel  = "itest-done-model"
	dnThread = "itest-done:thread:1"
	dnWorker = "itest-done-worker"
	dnActor  = "dashboard:salvo"
)

func cleanupDone(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const projs = `(SELECT id FROM projects WHERE slug = '` + dnSlug + `')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	const raws = `(SELECT id FROM raw_source_items WHERE external_id LIKE 'itest-done%')`
	const msgs = `(SELECT id FROM normalized_messages WHERE raw_source_item_id IN ` + raws + `)`
	for _, q := range []string{
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM classify_promotions WHERE task_id IN ` + tasksOf + ` OR normalized_message_id IN ` + msgs,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_dependencies WHERE task_id IN ` + tasksOf + ` OR depends_on_task_id IN ` + tasksOf,
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + `)`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM ai_extractions WHERE ai_run_id IN (SELECT id FROM ai_runs WHERE model='` + dnModel + `')`,
		`DELETE FROM ai_runs WHERE model='` + dnModel + `'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE thread_key = '` + dnThread + `'`,
		`DELETE FROM raw_source_items WHERE external_id LIKE 'itest-done%'`,
		`DELETE FROM projects WHERE slug = '` + dnSlug + `'`,
		`DELETE FROM source_accounts WHERE account_email = '` + dnAcct + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func dnProject(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	return bdInsID(t, ctx, pool,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,$2,'manual','dashboard','/tmp/itest-done','any') RETURNING id`, dnSlug, dnClient)
}

func dnTask(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID int64, title, assignee, status string) int64 {
	t.Helper()
	return bdInsID(t, ctx, pool,
		`INSERT INTO tasks (project_id, title, body, assignee_type, status, priority)
		 VALUES ($1,$2,'',$3,$4,0) RETURNING id`, projectID, title, assignee, status)
}

// dnDone posts the board's Done form and returns the URL the 303 landed on.
// Nothing is faked: the route, the executor, the policy matrix and task_close.
func dnDone(t *testing.T, client *http.Client, base string, taskID int64, form url.Values) *url.URL {
	t.Helper()
	resp, err := client.PostForm(base+"/tasks/"+strconv.FormatInt(taskID, 10)+"/close", form)
	if err != nil {
		t.Fatalf("POST /tasks/%d/close: %v", taskID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasSuffix(resp.Request.URL.Path, "/tasks") {
		t.Fatalf("POST /tasks/%d/close landed on %s with status %d, want /tasks (200 after the 303). "+
			"Criteria 5 + 8: the route is POST /tasks/{id}/close on the AUTH-REQUIRED mux, redirecting 303 "+
			"back to the filtered board with a flash", taskID, resp.Request.URL, resp.StatusCode)
	}
	return resp.Request.URL
}

func dnStatusEvents(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64) int {
	t.Helper()
	return bdCount(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='status_changed'`, taskID)
}

// ---- criteria 10, 11, 12, 15 (+ 8 end to end) -----------------------------------

func TestBoardDone_Integration_ClosesAuditsAndWritesNoLabel(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupDone(t, ctx, pool)
	defer cleanupDone(t, ctx, pool)

	proj := dnProject(t, ctx, pool)
	task := dnTask(t, ctx, pool, proj, "DONE ready human task", "human", "ready")

	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	// ---- criterion 15, before: on the default board ---------------------------
	_, before := get(t, client, ts.URL+"/tasks?project="+dnSlug)
	if !strings.Contains(before, "DONE ready human task") {
		t.Fatalf("/tasks?project=%s does not show the seeded task before Done\n%s", dnSlug, snippet(before))
	}
	if n := bdCount(t, ctx, pool, `SELECT count(*) FROM task_dismissals WHERE task_id=$1`, task); n != 0 {
		t.Fatalf("fixture invalid: %d task_dismissals rows before Done, want 0", n)
	}

	const note = "shipped in 1.4"
	landed := dnDone(t, client, ts.URL, task, url.Values{
		"note": {note}, "project": {dnSlug}, "assignee_type": {"human"},
	})

	// ---- criterion 8 end to end: the filters and the flash round-trip ---------
	q := landed.Query()
	if q.Get("flash") != "task_close ok" {
		t.Errorf("Done on a ready human task flashed %q, want `task_close ok`", q.Get("flash"))
	}
	if q.Get("project") != dnSlug || q.Get("assignee_type") != "human" {
		t.Errorf("redirect query %q lost the posted filters, want project=%s and assignee_type=human "+
			"(criterion 8 / D5)", landed.RawQuery, dnSlug)
	}
	if _, present := q["status"]; present {
		t.Errorf("redirect query %q carries an empty status key; unset filters are omitted", landed.RawQuery)
	}

	// ---- criterion 11: closed, the close record, one status_changed -----------
	var status, closedFrom string
	var closedAtSet bool
	if err := pool.QueryRow(ctx,
		`SELECT status, COALESCE(closed_from_status,''), closed_at IS NOT NULL FROM tasks WHERE id=$1`, task).
		Scan(&status, &closedFrom, &closedAtSet); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if status != "closed" || closedFrom != "ready" || !closedAtSet {
		t.Errorf("after Done: (status %q, closed_from_status %q, closed_at set %v), want (closed, ready, true). "+
			"Criterion 11: task_close's closeTransition writes the close record", status, closedFrom, closedAtSet)
	}
	if n := dnStatusEvents(t, ctx, pool, task); n != 1 {
		t.Errorf("status_changed events after Done = %d, want exactly 1", n)
	}
	var from, to, reason string
	var keys []string
	if err := pool.QueryRow(ctx,
		`SELECT payload->>'from', payload->>'to', payload->>'reason',
		        ARRAY(SELECT jsonb_object_keys(payload) ORDER BY 1)
		   FROM task_events WHERE task_id=$1 AND event_type='status_changed' ORDER BY id DESC LIMIT 1`,
		task).Scan(&from, &to, &reason, &keys); err != nil {
		t.Fatalf("no status_changed event for the closed task: %v", err)
	}
	if from != "ready" || to != "closed" {
		t.Errorf("status_changed payload = (from %q, to %q), want (ready, closed)", from, to)
	}
	if strings.Join(keys, ",") != "from,reason,to" {
		t.Errorf("status_changed payload keys = %v, want exactly [from reason to] (criterion 11)", keys)
	}
	if reason != "done on the board: "+note {
		t.Errorf("status_changed reason = %q, want %q (D1: the note folds into task_close's reason)",
			reason, "done on the board: "+note)
	}

	// ---- criterion 12: NO dismissal label -------------------------------------
	if n := bdCount(t, ctx, pool, `SELECT count(*) FROM task_dismissals WHERE task_id=$1`, task); n != 0 {
		t.Errorf("task_dismissals rows after Done = %d, want 0. Criterion 12 / invariant 2: a finished task is "+
			"not a labelled negative — InquiryOutcome counts it a true positive precisely because the row is absent", n)
	}

	// ---- criterion 10: the audit row carries the task and the dashboard actor --
	if n := bdCount(t, ctx, pool,
		`SELECT count(*) FROM audit_events WHERE tool='task_close' AND task_id=$1`, task); n != 1 {
		t.Fatalf("audit_events rows for task_close with task_id=%d = %d, want 1. Criterion 10: the handler goes "+
			"through executeTask, which sets Call.TaskID — executeTo would leave task_id NULL", task, n)
	}
	var auditID int64
	var actor, auditStatus string
	if err := pool.QueryRow(ctx,
		`SELECT id, actor, status FROM audit_events WHERE tool='task_close' AND task_id=$1`, task).
		Scan(&auditID, &actor, &auditStatus); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if actor != dnActor || auditStatus != "ok" {
		t.Errorf("audit row = (actor %q, status %q), want (%q, ok)", actor, auditStatus, dnActor)
	}
	// Criterion 9 through the real matrix: mcpHumanOnly → Decide does not deny
	// a dashboard actor → static allow-list.
	rows, err := pool.Query(ctx,
		`SELECT decision, COALESCE(rule,'') FROM policy_decisions WHERE audit_event_id=$1`, auditID)
	if err != nil {
		t.Fatalf("read policy_decisions: %v", err)
	}
	var nDec int
	sawStatic := false
	for rows.Next() {
		var decision, rule string
		if err := rows.Scan(&decision, &rule); err != nil {
			rows.Close()
			t.Fatalf("scan policy_decisions: %v", err)
		}
		nDec++
		if decision != "allow" {
			t.Errorf("policy decision for the board close = %q (rule %q), want allow", decision, rule)
		}
		if rule == "static-default" {
			sawStatic = true
		}
	}
	rows.Close()
	if nDec == 0 || !sawStatic {
		t.Errorf("policy_decisions for audit %d: %d row(s), static-default seen %v; want allow / static-default "+
			"(criterion 10)", auditID, nDec, sawStatic)
	}

	// ---- criterion 15, after: hidden by default, present under ?status=closed --
	_, after := get(t, client, ts.URL+"/tasks?project="+dnSlug)
	if strings.Contains(after, "DONE ready human task") {
		t.Errorf("the closed task is still on the default board (criterion 15: `t.status <> 'closed'` hides it)\n%s",
			snippet(after))
	}
	_, closed := get(t, client, ts.URL+"/tasks?project="+dnSlug+"&status=closed")
	if !strings.Contains(closed, "DONE ready human task") {
		t.Errorf("/tasks?status=closed does not show the closed task (criterion 15: the lane is a FILTER)\n%s",
			snippet(closed))
	}
}

// ---- criterion 13: task_close's own refusal, as the flash ------------------------

func TestBoardDone_Integration_RefusesActiveWork(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupDone(t, ctx, pool)
	defer cleanupDone(t, ctx, pool)

	proj := dnProject(t, ctx, pool)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	for _, st := range []string{"claimed", "in_progress", "needs_feedback"} {
		t.Run(st, func(t *testing.T) {
			task := dnTask(t, ctx, pool, proj, "DONE active human task "+st, "human", st)
			// The unreleased claim production has for these statuses (a manual
			// `/task N` session holding a human task — D3).
			claim := bdInsID(t, ctx, pool,
				`INSERT INTO task_claims (task_id, worker_id, expires_at)
				 VALUES ($1,$2, now() + interval '2 hours') RETURNING id`, task, dnWorker)
			var expiresBefore string
			if err := pool.QueryRow(ctx, `SELECT expires_at::text FROM task_claims WHERE id=$1`, claim).
				Scan(&expiresBefore); err != nil {
				t.Fatalf("read claim: %v", err)
			}

			flash := dnDone(t, client, ts.URL, task, url.Values{"note": {"trying anyway"}}).Query().Get("flash")
			if !strings.Contains(flash, "refusing to close active work") {
				t.Errorf("Done on a %s task flashed %q, want task_close's own refusal `task %d is %s; refusing to "+
					"close active work` (criterion 13 / D3: one spelling of the status list, in closeTransition)",
					st, flash, task, st)
			}
			var status string
			if err := pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, task).Scan(&status); err != nil {
				t.Fatalf("read task: %v", err)
			}
			if status != st {
				t.Errorf("refused task status = %q, want %q (unchanged)", status, st)
			}
			if n := dnStatusEvents(t, ctx, pool, task); n != 0 {
				t.Errorf("status_changed events on the refused task = %d, want 0", n)
			}
			var released bool
			var expiresAfter, worker string
			if err := pool.QueryRow(ctx,
				`SELECT released_at IS NOT NULL, expires_at::text, worker_id FROM task_claims WHERE id=$1`, claim).
				Scan(&released, &expiresAfter, &worker); err != nil {
				t.Fatalf("read claim after: %v", err)
			}
			if released || expiresAfter != expiresBefore || worker != dnWorker {
				t.Errorf("the claim was touched by a refused Done: (released %v, expires %q→%q, worker %q), want "+
					"untouched (criterion 13)", released, expiresBefore, expiresAfter, worker)
			}
			if n := bdCount(t, ctx, pool, `SELECT count(*) FROM task_dismissals WHERE task_id=$1`, task); n != 0 {
				t.Errorf("task_dismissals rows for the refused task = %d, want 0", n)
			}
			// Invariant 3: the refusal is an AUDITED refusal, carrying the task.
			if n := bdCount(t, ctx, pool,
				`SELECT count(*) FROM audit_events WHERE tool='task_close' AND task_id=$1 AND actor=$2 AND status='error'`,
				task, dnActor); n != 1 {
				t.Errorf("audit_events error rows for the refused task_close = %d, want 1 (the executor audits "+
					"the refusal, with the task id)", n)
			}
		})
	}
}

// ---- criterion 14: a stale page / double submit is a no-op success ---------------

func TestBoardDone_Integration_AlreadyClosedIsANoOp(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupDone(t, ctx, pool)
	defer cleanupDone(t, ctx, pool)

	proj := dnProject(t, ctx, pool)
	task := dnTask(t, ctx, pool, proj, "DONE double submit task", "human", "ready")

	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	if f := dnDone(t, client, ts.URL, task, url.Values{"note": {"first"}}).Query().Get("flash"); f != "task_close ok" {
		t.Fatalf("the first Done flashed %q, want `task_close ok`", f)
	}
	var closedAt1, from1 string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(closed_at::text,''), COALESCE(closed_from_status,'') FROM tasks WHERE id=$1`, task).
		Scan(&closedAt1, &from1); err != nil {
		t.Fatalf("read close record: %v", err)
	}
	events1 := dnStatusEvents(t, ctx, pool, task)

	if f := dnDone(t, client, ts.URL, task, url.Values{"note": {"second"}}).Query().Get("flash"); f != "task_close ok" {
		t.Errorf("a second Done on an already-closed task flashed %q, want `task_close ok` (criterion 14: a stale "+
			"page and a double submit are the same request twice)", f)
	}
	var status, closedAt2, from2 string
	if err := pool.QueryRow(ctx,
		`SELECT status, COALESCE(closed_at::text,''), COALESCE(closed_from_status,'') FROM tasks WHERE id=$1`, task).
		Scan(&status, &closedAt2, &from2); err != nil {
		t.Fatalf("read close record after: %v", err)
	}
	if status != "closed" || closedAt2 != closedAt1 || from2 != from1 {
		t.Errorf("the replay moved the close record: (status %q, closed_at %q→%q, closed_from_status %q→%q), want "+
			"unchanged (criterion 14 — the revive guard compares against closed_at)", status, closedAt1, closedAt2, from1, from2)
	}
	if n := dnStatusEvents(t, ctx, pool, task); n != events1 {
		t.Errorf("status_changed events after the replay = %d, want %d (unchanged: nothing transitioned, and "+
			"each event NOTIFYs the orchestrator's drain)", n, events1)
	}
	if n := bdCount(t, ctx, pool, `SELECT count(*) FROM task_dismissals WHERE task_id=$1`, task); n != 0 {
		t.Errorf("task_dismissals rows after two Dones = %d, want 0", n)
	}
}

// ---- criterion 16: dependents are the orchestrator's job ------------------------

func TestBoardDone_Integration_DependentsAreTheOrchestratorsJob(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupDone(t, ctx, pool)
	defer cleanupDone(t, ctx, pool)

	proj := dnProject(t, ctx, pool)
	prereq := dnTask(t, ctx, pool, proj, "DONE prerequisite task", "human", "ready")
	dependent := dnTask(t, ctx, pool, proj, "DONE blocked dependent task", "claude", "blocked")
	if _, err := pool.Exec(ctx,
		`INSERT INTO task_dependencies (task_id, depends_on_task_id) VALUES ($1,$2)`, dependent, prereq); err != nil {
		t.Fatalf("seed dependency: %v", err)
	}
	var depUpdated string
	if err := pool.QueryRow(ctx, `SELECT updated_at::text FROM tasks WHERE id=$1`, dependent).Scan(&depUpdated); err != nil {
		t.Fatalf("read dependent: %v", err)
	}
	depEvents := bdCount(t, ctx, pool, `SELECT count(*) FROM task_events WHERE task_id=$1`, dependent)

	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	if f := dnDone(t, client, ts.URL, prereq, url.Values{}).Query().Get("flash"); f != "task_close ok" {
		t.Fatalf("Done on the prerequisite flashed %q, want `task_close ok`", f)
	}

	// The input Evaluate routes to ruleUnblockDependents: the prerequisite's
	// newest event is status_changed {to: closed}.
	var evType, to string
	if err := pool.QueryRow(ctx,
		`SELECT event_type, COALESCE(payload->>'to','') FROM task_events WHERE task_id=$1 ORDER BY id DESC LIMIT 1`,
		prereq).Scan(&evType, &to); err != nil {
		t.Fatalf("no event on the prerequisite after Done: %v", err)
	}
	if evType != "status_changed" || to != "closed" {
		t.Errorf("the prerequisite's newest event = (%q, to %q), want (status_changed, closed) — criterion 16: "+
			"that event is exactly what the orchestrator's R5 consumes", evType, to)
	}

	// And the dashboard did not do the orchestrator's job.
	var depStatus, depUpdatedAfter string
	if err := pool.QueryRow(ctx, `SELECT status, updated_at::text FROM tasks WHERE id=$1`, dependent).
		Scan(&depStatus, &depUpdatedAfter); err != nil {
		t.Fatalf("read dependent after: %v", err)
	}
	if depStatus != "blocked" || depUpdatedAfter != depUpdated {
		t.Errorf("the dependent changed on the POST: (status %q, updated_at %q→%q), want still blocked and "+
			"untouched — criterion 16: unblocking is the orchestrator's job on its next drain (invariant 7)",
			depStatus, depUpdated, depUpdatedAfter)
	}
	if n := bdCount(t, ctx, pool, `SELECT count(*) FROM task_events WHERE task_id=$1`, dependent); n != depEvents {
		t.Errorf("task_events on the dependent went %d → %d; the board close must write nothing for it", depEvents, n)
	}
	if n := bdCount(t, ctx, pool,
		`SELECT count(*) FROM task_dependencies WHERE task_id=$1 AND depends_on_task_id=$2`, dependent, prereq); n != 1 {
		t.Errorf("task_dependencies rows for the pair = %d, want 1 (the dashboard touches no dependency row)", n)
	}
}

// ---- criterion 17: the inquiry lane's true positive ------------------------------

func TestBoardDone_Integration_InquiryTruePositive(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupDone(t, ctx, pool)
	defer cleanupDone(t, ctx, pool)

	// The table is global: measure a baseline BEFORE seeding, so the delta is
	// exactly this test's promoted task.
	base, err := promote.InquiryOutcomes(ctx, pool, 0)
	if err != nil {
		t.Fatalf("InquiryOutcomes (baseline): %v", err)
	}

	proj := dnProject(t, ctx, pool)
	acct := bdInsID(t, ctx, pool,
		`INSERT INTO source_accounts (provider, account_email) VALUES ('upwork_crm',$1)
		 ON CONFLICT (provider, account_email) DO UPDATE SET account_email=EXCLUDED.account_email
		 RETURNING id`, dnAcct)
	thread := bdInsID(t, ctx, pool,
		`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,'','[]') RETURNING id`, dnThread)
	raw := bdInsID(t, ctx, pool,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,'itest-done-inquiry','{}','itest-done-hash-inquiry', now()) RETURNING id`, acct)
	msg := bdInsID(t, ctx, pool,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		    body_text, subject, sender, channel)
		 VALUES ($1,$2,'inbound','itest-done-msg-inquiry', now() - interval '20 minutes',
		         'Can you look at the export?','','Mario Cruz','upwork_chat') RETURNING id`, raw, thread)
	run := bdInsID(t, ctx, pool,
		`INSERT INTO ai_runs (worker_type, provider, model, input, output, status)
		 VALUES ('classify_inquiry','itest-done',$1,'{"itest":"done"}'::jsonb,'{}'::jsonb,'ok') RETURNING id`, dnModel)
	extr := bdInsID(t, ctx, pool,
		`INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields) VALUES ($1,$2,$3::jsonb) RETURNING id`,
		run, raw, `{"actionable":true,"kind":"inquiry","title":"Look at the export","reason":"itest-done verdict"}`)
	task := dnTask(t, ctx, pool, proj, "DONE inquiry-promoted task", "human", "ready")
	// action 'task', NOT 'attached': an attached promotion counts excluded
	// whatever the task's status, so it would prove nothing.
	if _, err := pool.Exec(ctx,
		`INSERT INTO classify_promotions
		   (normalized_message_id, raw_source_item_id, ai_extraction_id, project_id, kind, action, task_id, reason)
		 VALUES ($1,$2,$3,$4,'inquiry','task',$5,'itest-done fixture promotion')`,
		msg, raw, extr, proj, task); err != nil {
		t.Fatalf("seed classify_promotion: %v", err)
	}

	delta := func(a, b promote.OutcomeCounts) promote.OutcomeCounts {
		return promote.OutcomeCounts{
			FalsePositive: a.FalsePositive - b.FalsePositive,
			TruePositive:  a.TruePositive - b.TruePositive,
			MisClick:      a.MisClick - b.MisClick,
			Excluded:      a.Excluded - b.Excluded,
		}
	}

	// Control: the fixture is inside InquiryOutcomes' join, and while open it
	// counts excluded. Without this, a zero delta below could mean "not joined".
	mid, err := promote.InquiryOutcomes(ctx, pool, 0)
	if err != nil {
		t.Fatalf("InquiryOutcomes (seeded): %v", err)
	}
	if d := delta(mid, base); d != (promote.OutcomeCounts{Excluded: 1}) {
		t.Fatalf("fixture invalid: seeding the open inquiry-promoted task moved InquiryOutcomes by %+v, want "+
			"{Excluded:1} (still open)", d)
	}

	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	if f := dnDone(t, client, ts.URL, task, url.Values{"note": {"answered"}}).Query().Get("flash"); f != "task_close ok" {
		t.Fatalf("Done on the inquiry-promoted task flashed %q, want `task_close ok`", f)
	}

	after, err := promote.InquiryOutcomes(ctx, pool, 0)
	if err != nil {
		t.Fatalf("InquiryOutcomes (after): %v", err)
	}
	if d := delta(after, base); d != (promote.OutcomeCounts{TruePositive: 1}) {
		t.Errorf("InquiryOutcomes delta after a board Done = %+v, want exactly {TruePositive:1}. Criterion 17: "+
			"closed with no dismissal is a true positive; a Done that wrote a task_dismissals row (or called "+
			"task_dismiss) would move FalsePositive/Excluded instead", d)
	}
}

// ---- criterion 18: Done renders on human rows only ------------------------------

func TestBoardDone_Integration_OnlyHumanRowsRenderDone(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupDone(t, ctx, pool)
	defer cleanupDone(t, ctx, pool)

	proj := dnProject(t, ctx, pool)
	human := dnTask(t, ctx, pool, proj, "DONE human row", "human", "ready")
	claude := dnTask(t, ctx, pool, proj, "DONE claude row", "claude", "ready")

	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	code, body := get(t, client, ts.URL+"/tasks?project="+dnSlug)
	if code != http.StatusOK {
		t.Fatalf("GET /tasks?project=%s = %d\n%s", dnSlug, code, snippet(body))
	}
	action := func(id int64, verb string) string {
		return `action="/tasks/` + strconv.FormatInt(id, 10) + `/` + verb + `"`
	}
	if !strings.Contains(body, action(human, "close")) {
		t.Errorf("the human row has no %s. Criterion 18: a human task renders the Done form", action(human, "close"))
	}
	if strings.Contains(body, action(claude, "close")) {
		t.Errorf("the claude row renders %s. D2: Done renders only on assignee_type=human rows — task_close accepts "+
			"pr_open/awaiting_* where a worker's claim is still live", action(claude, "close"))
	}
	for _, id := range []int64{human, claude} {
		if !strings.Contains(body, action(id, "dismiss")) {
			t.Errorf("row %d lost its Dismiss form (%s). SWT-31 D6: Dismiss renders on every row", id, action(id, "dismiss"))
		}
	}
}
