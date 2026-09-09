//go:build integration

package dashboard_test

// Integration tests for SWT-31 (docs/tickets/board-dismissals_SPEC.md) Part 2 —
// acceptance criteria 11, 12, 13, 14, 15, 18 and 19. The REAL dashboard.Server
// runs under httptest with dev-login session auth, the REAL executor registry and
// the REAL policy matrix; the dismissal is made by POSTing the board's form.
// Build-tagged `integration` AND env-gated on DATABASE_URL. NO LLM, NO network.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run BoardDismiss ./internal/dashboard/
//
// GREENFIELD NOTE — EXPECTED RED, twice over: migration 0022 has not created
// task_dismissals (so every fixture read errors), `task_dismiss` is not
// registered, and POST /tasks/{id}/dismiss is a 405 (the path matches only
// GET-registered patterns).
//
// CLEANUP PACT. Joins the dashboard suite's shape (dashGuard / dashPool /
// newDashServer / get / snippet live in dashboard_integration_test.go, same
// package) with its own test-owned prefix `itest-dismiss-%`, FK-ordered and
// rerunnable. Two orderings are load-bearing and are the reason this suite has
// its own cleanup rather than reusing cleanupDash:
//
//   - audit_events.task_id FKs tasks, and criterion 15 is precisely that the
//     dismiss call SETS it — so audit_events (and policy_decisions, which FKs
//     audit_events) must be deleted BEFORE tasks or cleanup fails with an FK
//     violation on this suite's own successful run.
//   - task_dismissals cascades from tasks, but it is deleted explicitly anyway:
//     a cleanup that silently depends on a cascade stops proving that the
//     cascade is there.

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	bdSlug    = "itest-dismiss-proj"
	bdClient  = "itest-dismiss-client"
	bdAcct    = "itest-dismiss-a@local"
	bdModel   = "itest-dismiss-model"
	bdActor   = "dashboard:salvo"
	bdThread  = "upwork_crm:7700:room:1"
	bdThread2 = "upwork_crm:7700:room:2"
)

// The two queries a future precision ticket runs, copied VERBATIM from the
// SPEC's "Labelled-data contract" section. Criterion 19 pins them: they are
// plain typed SQL — no jsonb predicate on task_events, no LIKE, no payload
// grepping — and if a schema change breaks one, it breaks here rather than in
// six months in a psql session.
const bdClassifierLabelQuery = `SELECT d.reason_code, d.note, d.dismissed_by, d.created_at,
       cp.normalized_message_id, cp.kind AS promoted_kind, cp.action,
       e.fields->>'kind'   AS verdict_kind,
       e.fields->>'title'  AS verdict_title,
       e.fields->>'reason' AS verdict_reason
  FROM task_dismissals d
  JOIN classify_promotions cp ON cp.task_id = d.task_id
  JOIN ai_extractions e       ON e.id = cp.ai_extraction_id
 WHERE d.reason_code = 'not_actionable'
 ORDER BY d.created_at`

const bdCaptureLabelQuery = `SELECT r.id AS rule_id, r.criteria_type, r.pattern, p.slug AS project,
       d.reason_code, count(*)
  FROM task_dismissals d
  JOIN capture_decisions cd ON cd.task_id = d.task_id AND cd.mode = 'live'
  JOIN capture_rules r      ON r.id = cd.matched_rule_id
  JOIN projects p           ON p.id = r.project_id
 GROUP BY 1,2,3,4,5
 ORDER BY 6 DESC`

type bdSeed struct {
	projectID int64

	// promoted is a task the SWT-30 promoter created from a stored classify
	// verdict: classify_promotions -> ai_extractions. The classifier-side label
	// join.
	promotedTask int64
	promotedMsg  int64
	extractionID int64

	// captured is a task the capture-rules engine created: capture_decisions ->
	// capture_rules. The capture-side label join.
	capturedTask int64
	captureRule  int64

	// activeTask is in_progress — criterion 11's refusal.
	activeTask int64
	// closedTask is already closed and carries no dismissal row — criterion 14.
	closedTask int64
}

// bdRequireDismissalsTable turns "relation task_dismissals does not exist" —
// which surfaces from inside CLEANUP and reads like a broken pact — into the one
// sentence that is actually true before this ticket lands.
func bdRequireDismissalsTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var present bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('task_dismissals') IS NOT NULL`).Scan(&present); err != nil {
		t.Fatalf("probe for task_dismissals: %v", err)
	}
	if !present {
		t.Fatalf("task_dismissals does not exist in this database. Criterion 7: migration " +
			"0022_task_dismissals.sql creates it, and `make integration` applies migrations to the " +
			"compose db on :5433 before running. Merging a migration is not applying it")
	}
}

func cleanupDismiss(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	bdRequireDismissalsTable(t, ctx, pool)
	const projs = `(SELECT id FROM projects WHERE slug = '` + bdSlug + `')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	const raws = `(SELECT id FROM raw_source_items WHERE external_id LIKE 'itest-dismiss%')`
	const msgs = `(SELECT id FROM normalized_messages WHERE raw_source_item_id IN ` + raws + `)`
	for _, q := range []string{
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM classify_promotions WHERE task_id IN ` + tasksOf + ` OR normalized_message_id IN ` + msgs,
		`DELETE FROM capture_decisions WHERE task_id IN ` + tasksOf + ` OR message_id IN ` + msgs,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_dependencies WHERE task_id IN ` + tasksOf,
		// audit_events.task_id FKs tasks — criterion 15 fills it, so this is not
		// optional. policy_decisions FKs audit_events and goes first.
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + `)`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM capture_rules WHERE project_id IN ` + projs,
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM ai_extractions WHERE ai_run_id IN (SELECT id FROM ai_runs WHERE model='` + bdModel + `')`,
		`DELETE FROM ai_runs WHERE model='` + bdModel + `'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE thread_key IN ('` + bdThread + `','` + bdThread2 + `')`,
		`DELETE FROM raw_source_items WHERE external_id LIKE 'itest-dismiss%'`,
		`DELETE FROM projects WHERE slug = '` + bdSlug + `'`,
		`DELETE FROM source_accounts WHERE account_email = '` + bdAcct + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func bdInsID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, q string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", q, err)
	}
	return id
}

func bdCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

// seedDismiss builds both provenance shapes and both refusal shapes. Fixtures
// are written directly, not through the executor: a fixture is not a production
// write, and invariant 3 is asserted against the DISMISS call's audit rows.
func seedDismiss(t *testing.T, ctx context.Context, pool *pgxpool.Pool) bdSeed {
	t.Helper()
	var s bdSeed

	s.projectID = bdInsID(t, ctx, pool,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,$2,'manual','dashboard','/tmp/itest-dismiss','any') RETURNING id`, bdSlug, bdClient)

	acct := bdInsID(t, ctx, pool,
		`INSERT INTO source_accounts (provider, account_email) VALUES ('upwork_crm',$1)
		 ON CONFLICT (provider, account_email) DO UPDATE SET account_email=EXCLUDED.account_email
		 RETURNING id`, bdAcct)

	threadA := bdInsID(t, ctx, pool,
		`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,'','[]') RETURNING id`, bdThread)
	threadB := bdInsID(t, ctx, pool,
		`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,'','[]') RETURNING id`, bdThread2)

	newMsg := func(label string, thread int64, sender string) (int64, int64) {
		raw := bdInsID(t, ctx, pool,
			`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
			 VALUES ($1,$2,'{}','itest-dismiss-hash-'||$3, now()) RETURNING id`,
			acct, "itest-dismiss-"+label, label)
		msg := bdInsID(t, ctx, pool,
			`INSERT INTO normalized_messages
			   (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
			    body_text, subject, sender, channel)
			 VALUES ($1,$2,'inbound',$3, now() - interval '20 minutes',
			         'Hi Salvador, about the invoice','',$4,'upwork_chat') RETURNING id`,
			raw, thread, "itest-dismiss-msg-"+label, sender)
		return raw, msg
	}

	// ---- the classifier-side fixture: a promoted task -------------------------
	rawP, msgP := newMsg("promoted", threadA, "Mario Cruz")
	s.promotedMsg = msgP
	runID := bdInsID(t, ctx, pool,
		`INSERT INTO ai_runs (worker_type, provider, model, input, output, status)
		 VALUES ('classify','itest-dismiss',$1,'{"itest":"dismiss"}'::jsonb,'{}'::jsonb,'ok') RETURNING id`, bdModel)
	s.extractionID = bdInsID(t, ctx, pool,
		`INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields) VALUES ($1,$2,$3::jsonb) RETURNING id`,
		runID, rawP,
		`{"actionable":true,"kind":"payment_due","title":"An invoice the model thought was actionable",`+
			`"reason":"itest-dismiss stored verdict","sender":"Mario Cruz","subject":""}`)
	s.promotedTask = bdInsID(t, ctx, pool,
		`INSERT INTO tasks (project_id, title, body, assignee_type, status, priority)
		 VALUES ($1,'DISMISS promoted task','','human','ready',0) RETURNING id`, s.projectID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO classify_promotions
		   (normalized_message_id, raw_source_item_id, ai_extraction_id, project_id, kind, action, task_id, reason)
		 VALUES ($1,$2,$3,$4,'payment_due','task',$5,'itest-dismiss fixture promotion')`,
		msgP, rawP, s.extractionID, s.projectID, s.promotedTask); err != nil {
		t.Fatalf("seed classify_promotion: %v", err)
	}

	// ---- the capture-side fixture: a rule-created task ------------------------
	rawC, msgC := newMsg("captured", threadB, "")
	s.captureRule = bdInsID(t, ctx, pool,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, priority, enabled, note)
		 VALUES ($1,'thread_key_prefix','upwork_crm:7700:room:','upwork_crm',200,true,'itest-dismiss rule')
		 RETURNING id`, s.projectID)
	s.capturedTask = bdInsID(t, ctx, pool,
		`INSERT INTO tasks (project_id, title, body, assignee_type, status, priority)
		 VALUES ($1,'DISMISS captured task','','human','ready',0) RETURNING id`, s.projectID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO capture_decisions
		   (message_id, raw_source_item_id, mode, matched_rule_id, project_id, action,
		    external_system, external_key, task_id)
		 VALUES ($1,$2,'live',$3,$4,'task','upwork_crm',$5,$6)`,
		msgC, rawC, s.captureRule, s.projectID, bdThread2, s.capturedTask); err != nil {
		t.Fatalf("seed capture_decision: %v", err)
	}

	// ---- the two refusal / edge shapes ---------------------------------------
	s.activeTask = bdInsID(t, ctx, pool,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'DISMISS active task','claude','in_progress') RETURNING id`, s.projectID)
	s.closedTask = bdInsID(t, ctx, pool,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'DISMISS already closed task','claude','closed') RETURNING id`, s.projectID)
	return s
}

// bdDismiss posts the board's per-row form and returns the flash text the
// redirect carries. Nothing is faked: this is the route, the executor, the
// policy matrix and the handler.
func bdDismiss(t *testing.T, client *http.Client, base string, taskID int64, code, note string) string {
	t.Helper()
	resp, err := client.PostForm(base+"/tasks/"+strconv.FormatInt(taskID, 10)+"/dismiss",
		url.Values{"reason_code": {code}, "note": {note}})
	if err != nil {
		t.Fatalf("POST /tasks/%d/dismiss: %v", taskID, err)
	}
	defer resp.Body.Close()
	// The client follows the 303, so a routed request lands on /tasks with the
	// flash as a query parameter. Anything else means the route is missing: an
	// unregistered POST under a GET-only pattern is a 404 or a 405, and falling
	// through to `GET /`'s redirect loses the flash entirely.
	if resp.StatusCode != http.StatusOK || !strings.HasSuffix(resp.Request.URL.Path, "/tasks") {
		t.Fatalf("POST /tasks/%d/dismiss landed on %s with status %d, want /tasks (200 after the 303). "+
			"Criterion 15: the route is POST /tasks/{id}/dismiss on the AUTH-REQUIRED mux, redirecting "+
			"303 back to the filtered board with a flash", taskID, resp.Request.URL, resp.StatusCode)
	}
	return resp.Request.URL.Query().Get("flash")
}

// ---- criteria 11-15, 18: the verb ---------------------------------------------

func TestBoardDismiss_Integration_RefusesActiveWorkAndLeavesNoLabel(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupDismiss(t, ctx, pool)
	defer cleanupDismiss(t, ctx, pool)
	sd := seedDismiss(t, ctx, pool)

	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	flash := bdDismiss(t, client, ts.URL, sd.activeTask, "not_actionable", "")

	// Criterion 11: the SHARED refusal, word for word — `closeTask` and
	// `dismissTask` call one unexported helper, so the message cannot differ.
	if !strings.Contains(flash, "refusing to close active work") {
		t.Errorf("dismissing an in_progress task flashed %q, want the shared refusal "+
			"`task %d is in_progress; refusing to close active work`. D3: the refusal is NOT re-encoded "+
			"— a second spelling drifts from closeTask's silently, and the divergence closes work out "+
			"from under a running worker", flash, sd.activeTask)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, sd.activeTask).Scan(&status); err != nil {
		t.Fatalf("read task status: %v", err)
	}
	if status != "in_progress" {
		t.Errorf("the refused task's status = %q, want in_progress", status)
	}
	// The half that only a database can prove: the refusal left NO label. A
	// dismissal that refused the transition but wrote the row anyway would put a
	// judgement about a live task into the training set.
	if n := bdCount(t, ctx, pool, `SELECT count(*) FROM task_dismissals WHERE task_id=$1`, sd.activeTask); n != 0 {
		t.Errorf("task_dismissals rows for the REFUSED task = %d, want 0 (criterion 11: the refusal "+
			"leaves no row — the status update, the event and the label are one transaction)", n)
	}
}

func TestBoardDismiss_Integration_ClosesRecordsAndAudits(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupDismiss(t, ctx, pool)
	defer cleanupDismiss(t, ctx, pool)
	sd := seedDismiss(t, ctx, pool)

	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	// ---- criterion 18, before: the row is on the default board ---------------
	_, before := get(t, client, ts.URL+"/tasks?project="+bdSlug)
	if !strings.Contains(before, "DISMISS captured task") {
		t.Fatalf("/tasks?project=%s does not show the seeded task before dismissal\n%s", bdSlug, snippet(before))
	}

	const note = "duplicate of the Tuesday thread"
	flash := bdDismiss(t, client, ts.URL, sd.capturedTask, "duplicate", note)
	if !strings.Contains(flash, "task_dismiss ok") {
		t.Fatalf("dismissing a ready task flashed %q, want `task_dismiss ok`", flash)
	}

	// ---- criterion 12: status + event + label, one transaction ---------------
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, sd.capturedTask).Scan(&status); err != nil {
		t.Fatalf("read task status: %v", err)
	}
	if status != "closed" {
		t.Errorf("dismissed task status = %q, want closed", status)
	}

	var code, gotNote, by string
	if err := pool.QueryRow(ctx,
		`SELECT reason_code, COALESCE(note,''), dismissed_by FROM task_dismissals WHERE task_id=$1`,
		sd.capturedTask).Scan(&code, &gotNote, &by); err != nil {
		t.Fatalf("no task_dismissals row for the dismissed task: %v", err)
	}
	if code != "duplicate" || gotNote != note {
		t.Errorf("task_dismissals row = (%q, %q), want (duplicate, %q)", code, gotNote, note)
	}
	// dismissed_by duplicates the executor actor already in audit_events,
	// deliberately: the label is training data and must be readable with ONE
	// typed query, not by parsing audit_events.args.
	if by != bdActor {
		t.Errorf("dismissed_by = %q, want %q (dashboard:{session user})", by, bdActor)
	}

	// The event: the SAME {from,to,reason} payload shape every other close
	// writes, with NO NEW KEY. The reason is PROSE composed from the code and
	// the note — asserted by its ingredients, not by an exact string, because
	// the wording is the implementer's (the SPEC says "e.g.").
	var from, to, reason string
	var keys []string
	if err := pool.QueryRow(ctx,
		`SELECT payload->>'from', payload->>'to', payload->>'reason',
		        ARRAY(SELECT jsonb_object_keys(payload) ORDER BY 1)
		   FROM task_events WHERE task_id=$1 AND event_type='status_changed' ORDER BY id DESC LIMIT 1`,
		sd.capturedTask).Scan(&from, &to, &reason, &keys); err != nil {
		t.Fatalf("no status_changed event for the dismissed task: %v", err)
	}
	if from != "ready" || to != "closed" {
		t.Errorf("status_changed payload = (from %q, to %q), want (ready, closed)", from, to)
	}
	if len(keys) != 3 || keys[0] != "from" || keys[1] != "reason" || keys[2] != "to" {
		t.Errorf("status_changed payload keys = %v, want exactly [from reason to]. Criterion 12: the same "+
			"payload shape every other close writes, with no new key — the LABEL lives in "+
			"task_dismissals (D2), and a reason_code in this payload is the untyped predicate that "+
			"whole decision refuses", keys)
	}
	for _, want := range []string{"duplicate", note} {
		if !strings.Contains(reason, want) {
			t.Errorf("status_changed reason = %q, which does not contain %q. The prose is composed from "+
				"the code and the note so a human reading the task sees WHY, while the machine-readable "+
				"copy stays in task_dismissals", reason, want)
		}
	}

	// ---- criterion 15: the audit row carries the task id ---------------------
	var auditTask *int64
	var actor, auditStatus string
	if err := pool.QueryRow(ctx,
		`SELECT actor, status, task_id FROM audit_events
		  WHERE tool='task_dismiss' AND task_id=$1 ORDER BY id DESC LIMIT 1`,
		sd.capturedTask).Scan(&actor, &auditStatus, &auditTask); err != nil {
		t.Fatalf("no audit_events row for task_dismiss with task_id=%d: %v.\nCriterion 15: the dashboard "+
			"passes executor.Call.TaskID, so audit start/complete rows carry the task. Today's executeTo "+
			"sets none — extend it or add a sibling", sd.capturedTask, err)
	}
	if actor != bdActor {
		t.Errorf("audit actor = %q, want %q", actor, bdActor)
	}
	if auditStatus != "ok" {
		t.Errorf("audit status = %q, want ok", auditStatus)
	}
	if auditTask == nil {
		t.Errorf("audit_events.task_id is NULL for the dismiss call (criterion 15)")
	}

	// ---- criterion 18, after: gone by default, present under ?status=closed --
	_, after := get(t, client, ts.URL+"/tasks?project="+bdSlug)
	if strings.Contains(after, "DISMISS captured task") {
		t.Errorf("the dismissed task is still on the default board. Criterion 18: `t.status <> 'closed'` "+
			"already hides it — boardQuery does not change\n%s", snippet(after))
	}
	_, closed := get(t, client, ts.URL+"/tasks?project="+bdSlug+"&status=closed")
	if !strings.Contains(closed, "DISMISS captured task") {
		t.Errorf("/tasks?status=closed does not show the dismissed task; the lane is a FILTER on the one "+
			"tasks table, not a table (invariant 2)\n%s", snippet(closed))
	}

	// ---- criterion 13: the second dismiss is a no-op success -----------------
	eventsBefore := bdCount(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='status_changed'`, sd.capturedTask)
	flash2 := bdDismiss(t, client, ts.URL, sd.capturedTask, "not_actionable", "a different note")
	if !strings.Contains(flash2, "task_dismiss ok") {
		t.Errorf("a second dismiss flashed %q, want success. Criterion 13: a stale page and a double "+
			"submit are the same request twice, and the operator must not have to reason about which "+
			"one landed", flash2)
	}
	if n := bdCount(t, ctx, pool, `SELECT count(*) FROM task_dismissals WHERE task_id=$1`, sd.capturedTask); n != 1 {
		t.Errorf("task_dismissals rows after a second dismiss = %d, want 1. The TOTAL unique index on "+
			"(task_id) is what makes `ON CONFLICT (task_id) DO NOTHING` need no restated predicate — "+
			"the index IS criterion 13", n)
	}
	// And the first label is the one that survives: DO NOTHING, not DO UPDATE.
	if err := pool.QueryRow(ctx,
		`SELECT reason_code, COALESCE(note,'') FROM task_dismissals WHERE task_id=$1`,
		sd.capturedTask).Scan(&code, &gotNote); err != nil {
		t.Fatalf("read task_dismissals after replay: %v", err)
	}
	if code != "duplicate" || gotNote != note {
		t.Errorf("the replay overwrote the label: (%q, %q), want the FIRST one (duplicate, %q). "+
			"ON CONFLICT DO NOTHING — a replay is not a correction", code, gotNote, note)
	}
	if n := bdCount(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='status_changed'`,
		sd.capturedTask); n != eventsBefore {
		t.Errorf("status_changed events after a second dismiss = %d, want %d (unchanged). The task was "+
			"already closed, so there was no transition to record — and each event NOTIFYs the "+
			"orchestrator's drain", n, eventsBefore)
	}
}

// Criterion 14: "Dismissing a task that is ALREADY closed but has no dismissal
// row records the label and emits no status_changed event."
//
// Reachable by a stale page, a double submit, or a task the orchestrator closed.
// The label is a human's judgement about a task that should not have existed;
// refusing would LOSE it, and the row makes no claim about the transition.
func TestBoardDismiss_Integration_AlreadyClosedTaskStillRecordsTheLabel(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupDismiss(t, ctx, pool)
	defer cleanupDismiss(t, ctx, pool)
	sd := seedDismiss(t, ctx, pool)

	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	if n := bdCount(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='status_changed'`,
		sd.closedTask); n != 0 {
		t.Fatalf("fixture invalid: the already-closed task starts with %d status_changed events, want 0", n)
	}

	flash := bdDismiss(t, client, ts.URL, sd.closedTask, "wrong_kind", "the orchestrator closed it first")
	if !strings.Contains(flash, "task_dismiss ok") {
		t.Fatalf("dismissing an already-closed task flashed %q, want success (criterion 14)", flash)
	}

	var code string
	if err := pool.QueryRow(ctx,
		`SELECT reason_code FROM task_dismissals WHERE task_id=$1`, sd.closedTask).Scan(&code); err != nil {
		t.Fatalf("no task_dismissals row for the already-closed task: %v. Criterion 14: refusing would "+
			"lose a human's judgement about a task that should not have existed", err)
	}
	if code != "wrong_kind" {
		t.Errorf("reason_code = %q, want wrong_kind", code)
	}
	if n := bdCount(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='status_changed'`,
		sd.closedTask); n != 0 {
		t.Errorf("status_changed events on the already-closed task = %d, want 0. Nothing transitioned, so "+
			"nothing is recorded as a transition — and the orchestrator's drain must not be woken for "+
			"a close that did not happen", n)
	}
}

// ---- criterion 19: both label joins, run verbatim ------------------------------

// "Both label joins work, proven by an integration test that seeds one promoted
// task (classify_promotions -> ai_extractions) and one capture-rule task
// (capture_decisions -> capture_rules), dismisses both through the real handler,
// and runs the two queries in Labelled-data contract below verbatim."
func TestBoardDismiss_Integration_BothLabelJoinsReturnTheDismissal(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupDismiss(t, ctx, pool)
	defer cleanupDismiss(t, ctx, pool)
	sd := seedDismiss(t, ctx, pool)

	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	// The classifier query filters reason_code = 'not_actionable', so the
	// promoted task is dismissed with exactly that.
	if f := bdDismiss(t, client, ts.URL, sd.promotedTask, "not_actionable", "the model was wrong"); !strings.Contains(f, "ok") {
		t.Fatalf("dismissing the promoted task flashed %q", f)
	}
	if f := bdDismiss(t, client, ts.URL, sd.capturedTask, "wrong_kind", "rule is too broad"); !strings.Contains(f, "ok") {
		t.Fatalf("dismissing the captured task flashed %q", f)
	}

	// ---- classifier side ------------------------------------------------------
	rows, err := pool.Query(ctx, bdClassifierLabelQuery)
	if err != nil {
		t.Fatalf("the classifier label query failed: %v\n%s", err, bdClassifierLabelQuery)
	}
	found := false
	for rows.Next() {
		var reasonCode, note, by string
		var createdAt any
		var msgID int64
		var promotedKind, action string
		var vKind, vTitle, vReason *string
		if err := rows.Scan(&reasonCode, &note, &by, &createdAt, &msgID, &promotedKind, &action,
			&vKind, &vTitle, &vReason); err != nil {
			rows.Close()
			t.Fatalf("scan classifier label row: %v", err)
		}
		if msgID != sd.promotedMsg {
			continue // another suite's row; the query is global by design
		}
		found = true
		if reasonCode != "not_actionable" || by != bdActor {
			t.Errorf("classifier label row = (%q, by %q), want (not_actionable, %q)", reasonCode, by, bdActor)
		}
		if promotedKind != "payment_due" || action != "task" {
			t.Errorf("classifier label row carries (kind %q, action %q), want (payment_due, task)",
				promotedKind, action)
		}
		// The point of the join: the dismissal comes back ALONGSIDE what the
		// model said, so a precision ticket can compare them.
		if vKind == nil || *vKind != "payment_due" {
			t.Errorf("verdict_kind = %v, want payment_due (from ai_extractions.fields)", vKind)
		}
		if vTitle == nil || *vTitle == "" {
			t.Errorf("verdict_title is empty; the stored verdict is what makes the label useful")
		}
		if vReason == nil || *vReason == "" {
			t.Errorf("verdict_reason is empty")
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate classifier label rows: %v", err)
	}
	if !found {
		t.Errorf("the classifier label query returned no row for message %d. This query IS the "+
			"deliverable: `SELECT ... FROM task_dismissals JOIN classify_promotions ...` returning the "+
			"dismissal alongside the stored verdict that caused it is what 'usable alone' means",
			sd.promotedMsg)
	}

	// ---- capture side ---------------------------------------------------------
	crows, err := pool.Query(ctx, bdCaptureLabelQuery)
	if err != nil {
		t.Fatalf("the capture label query failed: %v\n%s", err, bdCaptureLabelQuery)
	}
	foundRule := false
	for crows.Next() {
		var ruleID int64
		var criteriaType, pattern, project, reasonCode string
		var n int64
		if err := crows.Scan(&ruleID, &criteriaType, &pattern, &project, &reasonCode, &n); err != nil {
			crows.Close()
			t.Fatalf("scan capture label row: %v", err)
		}
		if ruleID != sd.captureRule {
			continue
		}
		foundRule = true
		if criteriaType != "thread_key_prefix" || project != bdSlug {
			t.Errorf("capture label row = (%q, project %q), want (thread_key_prefix, %q)",
				criteriaType, project, bdSlug)
		}
		if reasonCode != "wrong_kind" || n != 1 {
			t.Errorf("capture label row = (%q, count %d), want (wrong_kind, 1)", reasonCode, n)
		}
	}
	crows.Close()
	if err := crows.Err(); err != nil {
		t.Fatalf("iterate capture label rows: %v", err)
	}
	if !foundRule {
		t.Errorf("the capture label query returned no row for rule %d. 'Which rules produce tasks that "+
			"get thrown away' is the report a future precision ticket builds on, and a rule whose tasks "+
			"are all dismissed is a rule to disable", sd.captureRule)
	}

	// The stated fact the reader must know, asserted so it stays true: a
	// dismissal joins ZERO rows on both sides for a hand-created task (opsctl,
	// plan import). task_dismissals is complete on its own; the joins are
	// provenance, and a LEFT JOIN is the honest shape for a mixed report.
	handmade := bdInsID(t, ctx, pool,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'DISMISS hand-created task','human','ready') RETURNING id`, sd.projectID)
	if f := bdDismiss(t, client, ts.URL, handmade, "handled_elsewhere", ""); !strings.Contains(f, "ok") {
		t.Fatalf("dismissing a hand-created task flashed %q", f)
	}
	if n := bdCount(t, ctx, pool, `SELECT count(*) FROM task_dismissals WHERE task_id=$1`, handmade); n != 1 {
		t.Errorf("task_dismissals rows for a hand-created task = %d, want 1 — the label needs no "+
			"provenance row to exist (D7: the join key is task_id and nothing else)", n)
	}
}
