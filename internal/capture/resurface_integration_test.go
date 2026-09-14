//go:build integration

package capture_test

// chat-on-closed-task (SWT-53, docs/tickets/chat-on-closed-task_SPEC.md)
// against a real database, the CAPTURE path: criteria 2 (the CHECK), 5 (the
// notifier list is COLUMN-fed, both directions), 6 (every case SWT-45 / SWT-36
// / J3 / a not-closed status owns writes resurface=false and still logs), 7
// (shadow writes the same flag and calls nothing), 8 (the gate path writes the
// default false, CC8) and 9 (RulesStats.Resurfaced, both modes) — plus Q1's
// answer: an Avviato DM that names a key follows rule 10's attribution and
// resurfaces like a Treetop one (T13).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_isochat?sslmode=disable \
//	  TZ=UTC go test -tags integration -p 1 -count=1 -run CaptureResurface ./internal/capture/
//
// Build-tagged `integration` AND env-gated on DATABASE_URL, FATAL on
// 192.168.50.49. NO LLM, NO network. USE AN ISOLATED DATABASE (IK "Test
// infrastructure", 2026-09-12): this suite deletes capture_decisions
// WHOLESALE at start and end (the rules_integration_test.go precedent;
// EvaluateRules' pending set is global).
//
// "TEST THE COLUMN, NOT THE FIXTURE": the notifier list is seeded through
// projects.notifier_senders, the rule is a capture_rules row in rule 10's shape
// (body_regex, a PREFIX group, external_system jira, NO key_regex — K3), the
// bucket task is CREATED by a first live pass (external_refs written by
// link_external_ref) and closed through task_close, and resurface is read
// back from the column capture wrote. MUTATIONS named inline.
//
// ---- IMPOSED SURFACE ----------------------------------------------------------
//
//	migrations/0034_chat_on_closed_task.sql:
//	  projects.notifier_senders TEXT[] NOT NULL DEFAULT '{}'
//	  capture_decisions.resurface BOOLEAN NOT NULL DEFAULT false
//	  CONSTRAINT capture_decisions_resurface_is_task_log CHECK (NOT resurface OR action = 'task_log')
//	type RulesStats struct { ...; Resurfaced int } // decisions written resurface=true, BOTH modes
//	insertDecision writes resurface; loadRules selects p.notifier_senders; a
//	notifier exclusion's decision reason names the notifier list.
//
// RED TODAY: RulesStats.Resurfaced does not exist, so the capture integration
// build does not compile; once it does, every test FATALs in rsfRequire0034
// until 0034 is applied.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	rsfSlug      = "itest-capcc"
	rsfProvider  = "itest-capcc-src"
	rsfTreetop   = "titestcapcc@slack-web.local"
	rsfAvviato   = "titestcapccavv@slack-web.local"
	rsfSubject   = "itest-capcc"
	rsfTreetopWS = "TITESTCAPCC"
	rsfAvviatoWS = "TITESTCAPAVV"
	rsfJiraPfx   = "jira:capcc.jira.com:"
	// Rule 10's SHAPE with this suite's own letters: a prefix group, no
	// key_regex, so the key is the first group (CCW / CCA / CCO) — a bucket.
	rsfRule10 = `(CCW|CCA|CCO)-[0-9]+`
	rsfActor  = "capture:itest-capcc"
	rsfCloser = "opsctl:itest-capcc"
	rsfHuman  = "dashboard:itest-capcc"
)

type rsfSuite struct {
	pool             *pgxpool.Pool
	ex               *executor.Executor
	project          int64
	treetop, avviato int64
	threads          map[string]int64
}

// rsfRequire0034 turns "column resurface does not exist" (raised from inside
// the first pass) into the one sentence that is true before 0034 is applied.
func rsfRequire0034(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE (table_name='projects' AND column_name='notifier_senders')
		     OR (table_name='capture_decisions' AND column_name='resurface')`).Scan(&n); err != nil {
		t.Fatalf("probe 0034's columns: %v", err)
	}
	if n != 2 {
		t.Fatalf("found %d of the 2 columns migration 0034 adds (projects.notifier_senders, "+
			"capture_decisions.resurface); apply migrations/0034_chat_on_closed_task.sql "+
			"(make migrate LOCAL_DB_URL=...)", n)
	}
}

func newRSFSuite(t *testing.T, ctx context.Context) *rsfSuite {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db (cleanup deletes capture_decisions " +
			"wholesale); use an isolated compose db on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	rsfRequire0034(t, ctx, pool)
	rsfCleanup(t, ctx, pool)
	t.Cleanup(func() { rsfCleanup(t, context.Background(), pool) })

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	s := &rsfSuite{pool: pool, ex: executor.New(reg, checker, audit.NewPGStore(pool)), threads: map[string]int64{}}

	// collaboratory's shape: gate off, client set. notifier_senders is left at
	// its DEFAULT here; the tests that need a list set it through the column.
	s.project = s.insID(t, ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality, ticket_assignee_gate)
		 VALUES ($1,$1,'itest-capcc-client','manual','dashboard','/tmp/itest-capcc','any',false) RETURNING id`, rsfSlug)
	s.insID(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, priority, enabled, note)
		 VALUES ($1,'body_regex',$2,'jira',NULL,90,true,'itest-capcc rule10') RETURNING id`, s.project, rsfRule10)
	// Two workspaces, like Treetop and Avviato: rule 10 names no workspace, so
	// both attribute to the same project (Q1).
	s.treetop = s.insID(t, ctx,
		`INSERT INTO source_accounts (provider, account_email, scopes, send_enabled, calendar_in_availability)
		 VALUES ($1,$2,'{}',false,false) RETURNING id`, rsfProvider, rsfTreetop)
	s.avviato = s.insID(t, ctx,
		`INSERT INTO source_accounts (provider, account_email, scopes, send_enabled, calendar_in_availability)
		 VALUES ($1,$2,'{}',false,false) RETURNING id`, rsfProvider, rsfAvviato)
	return s
}

func rsfCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE provider='` + rsfProvider + `')`
	const projs = `(SELECT id FROM projects WHERE slug='` + rsfSlug + `')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	const actors = `('` + rsfActor + `','` + rsfCloser + `','` + rsfHuman + `')`
	for _, q := range []string{
		`DELETE FROM capture_decisions`, // wholesale: see the header
		`DELETE FROM ticket_status_syncs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM external_refs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM deliveries WHERE task_id IN ` + tasksOf,
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor IN ` + actors + `)`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor IN ` + actors,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM capture_rules WHERE project_id IN ` + projs,
		`DELETE FROM projects WHERE slug='` + rsfSlug + `'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN
		   (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`,
		`DELETE FROM normalized_threads WHERE subject = '` + rsfSubject + `'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider='` + rsfProvider + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

// ---- helpers ------------------------------------------------------------------------

func (s *rsfSuite) insID(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", q, err)
	}
	return id
}

func (s *rsfSuite) count(t *testing.T, ctx context.Context, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

func (s *rsfSuite) exec(t *testing.T, ctx context.Context, q string, args ...any) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func (s *rsfSuite) execute(t *testing.T, ctx context.Context, tool, actor string, taskID int64, args string) {
	t.Helper()
	if _, err := s.ex.Execute(ctx, executor.Call{Tool: tool, Actor: actor, Args: []byte(args), TaskID: &taskID}); err != nil {
		t.Fatalf("%s(%s) as %s: %v", tool, args, actor, err)
	}
}

// dm is a 1:1 DM conversation key in the normalizer's shape, built in Go.
func rsfDM(ws, label string) string { return "slack:" + ws + ":D0RSF" + strings.ToUpper(label) }

func (s *rsfSuite) thread(t *testing.T, ctx context.Context, key string) int64 {
	t.Helper()
	if id, ok := s.threads[key]; ok {
		return id
	}
	id := s.insID(t, ctx, `INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,$2,'[]') RETURNING id`,
		key, rsfSubject)
	s.threads[key] = id
	return id
}

// message seeds one INBOUND message the capture pass has not decided yet.
func (s *rsfSuite) message(t *testing.T, ctx context.Context, label, key, channel, sender, body string, acct int64) int64 {
	t.Helper()
	raw := s.insID(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,'{}',$3, now()) RETURNING id`, acct, "itest-capcc-"+label, "itest-capcc-h-"+label)
	return s.insID(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1,$2,'inbound',$3, clock_timestamp(), $4, '', $5, $6) RETURNING id`,
		raw, s.thread(t, ctx, key), "itest-capcc-ext-"+label, body, sender, channel)
}

// human is a person's Treetop-workspace DM naming the key.
func (s *rsfSuite) human(t *testing.T, ctx context.Context, label, sender, body string) int64 {
	t.Helper()
	return s.message(t, ctx, label, rsfDM(rsfTreetopWS, label), "slack", sender, body, s.treetop)
}

func (s *rsfSuite) pass(t *testing.T, ctx context.Context, mode string) capture.RulesStats {
	t.Helper()
	st, err := capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{Mode: mode, Actor: rsfActor})
	if err != nil {
		t.Fatalf("EvaluateRules(%s): %v", mode, err)
	}
	return st
}

// createdBy runs seed (one message naming key), then a live pass, and returns
// the task that pass created and linked — the ref is link_external_ref's.
func (s *rsfSuite) createdBy(t *testing.T, ctx context.Context, key string, seed func() int64) int64 {
	t.Helper()
	seed()
	s.pass(t, ctx, capture.RulesModeLive)
	var task int64
	if err := s.pool.QueryRow(ctx,
		`SELECT r.task_id FROM external_refs r JOIN tasks t ON t.id = r.task_id
		  WHERE r.system='jira' AND r.external_key=$1 AND t.project_id=$2`, key, s.project).Scan(&task); err != nil {
		t.Fatalf("setup: the first live pass created no task for %s: %v", key, err)
	}
	if st := s.status(t, ctx, task); st != "ready" {
		t.Fatalf("setup: the capture-created task for %s is %q, want ready", key, st)
	}
	return task
}

// bucket is rule 10's case: the key IS the prefix (K3), so the task is a bucket.
func (s *rsfSuite) bucket(t *testing.T, ctx context.Context, prefix string) int64 {
	t.Helper()
	return s.createdBy(t, ctx, prefix, func() int64 {
		return s.human(t, ctx, "setup-"+prefix, "Setup Human", prefix+"-1 kickoff")
	})
}

func (s *rsfSuite) closeTask(t *testing.T, ctx context.Context, task int64) {
	t.Helper()
	s.execute(t, ctx, "task_close", rsfCloser, task, fmt.Sprintf(`{"task_id":%d,"reason":"itest-capcc done"}`, task))
	if st := s.status(t, ctx, task); st != "closed" {
		t.Fatalf("setup: task %d is %q after task_close", task, st)
	}
}

func (s *rsfSuite) status(t *testing.T, ctx context.Context, task int64) string {
	t.Helper()
	var st string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, task).Scan(&st); err != nil {
		t.Fatalf("read task %d: %v", task, err)
	}
	return st
}

func (s *rsfSuite) logs(t *testing.T, ctx context.Context, task int64) int {
	t.Helper()
	return s.count(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='log'`, task)
}

type rsfDecision struct {
	action    string
	taskID    *int64
	reason    string
	resurface bool
}

func (s *rsfSuite) decision(t *testing.T, ctx context.Context, msg int64, mode string) (rsfDecision, bool) {
	t.Helper()
	var d rsfDecision
	err := s.pool.QueryRow(ctx,
		`SELECT action, task_id, COALESCE(reason,''), resurface FROM capture_decisions
		  WHERE message_id=$1 AND mode=$2 ORDER BY id DESC LIMIT 1`, msg, mode).
		Scan(&d.action, &d.taskID, &d.reason, &d.resurface)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return d, false
		}
		t.Fatalf("read %s decision for message %d: %v", mode, msg, err)
	}
	return d, true
}

// logged asserts the message was decided task_log onto task (the log line is
// still appended, as today) and that its resurface column reads want.
func (s *rsfSuite) logged(t *testing.T, ctx context.Context, msg int64, mode string, task int64, want bool, why string) rsfDecision {
	t.Helper()
	d, ok := s.decision(t, ctx, msg, mode)
	if !ok || d.action != "task_log" || d.taskID == nil || *d.taskID != task {
		t.Fatalf("message %d: %s decision = %+v (found=%v), want task_log on task %d (the log line stays; "+
			"resurface is a column, not an action)", msg, mode, d, ok, task)
	}
	if d.resurface != want {
		t.Errorf("message %d: capture_decisions.resurface = %v, want %v — %s (reason %q)", msg, d.resurface, want, why, d.reason)
	}
	return d
}

// ---- criterion 2: the CHECK pins resurface to task_log ---------------------------------

// MUTATION: drop capture_decisions_resurface_is_task_log from 0034 → the
// attributed insert succeeds and this goes red.
func TestCaptureResurface_Integration_TheCheckPinsResurfaceToTaskLog(t *testing.T) {
	ctx := context.Background()
	s := newRSFSuite(t, ctx)
	b := s.bucket(t, ctx, "CCW")
	m := s.human(t, ctx, "check", "Dana Ruiz", "no key here")

	_, err := s.pool.Exec(ctx,
		`INSERT INTO capture_decisions (message_id, mode, action, project_id, resurface, reason)
		 VALUES ($1,'shadow','attributed',$2,true,'itest-capcc check')`, m, s.project)
	if err == nil || !strings.Contains(err.Error(), "capture_decisions_resurface_is_task_log") {
		t.Errorf("inserting resurface=true on an `attributed` row: err = %v, want a violation of "+
			"capture_decisions_resurface_is_task_log (criterion 2: NOT resurface OR action = 'task_log')", err)
	}

	// Positive control: a task_log may carry it, so the refusal above is the CHECK's.
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO capture_decisions (message_id, mode, action, project_id, external_system, external_key, task_id,
		                                resurface, reason)
		 VALUES ($1,'shadow','task_log',$2,'jira','CCW',$3,true,'itest-capcc check ok')`, m, s.project, b); err != nil {
		t.Fatalf("POSITIVE CONTROL: a task_log row with resurface=true was refused: %v", err)
	}
	// The default is false (DEFAULT false: every writer that does not name it — the gate, the route stage).
	s.exec(t, ctx, `INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
	                VALUES ($1,'shadow','attributed',$2,'itest-capcc default')`, m, s.project)
	var def bool
	if err := s.pool.QueryRow(ctx, `SELECT resurface FROM capture_decisions WHERE message_id=$1 ORDER BY id DESC LIMIT 1`,
		m).Scan(&def); err != nil {
		t.Fatalf("read the default: %v", err)
	}
	if def {
		t.Errorf("a decision written without resurface reads true; 0034 declares DEFAULT false")
	}
}

// ---- criterion 5: the notifier list is column-fed, both directions ---------------------

// T2. MUTATION: select '{}'::text[] instead of p.notifier_senders in loadRules
// → the Jira app's message resurfaces and this goes red.
func TestCaptureResurface_Integration_ANotifierIsExcludedThroughTheColumn(t *testing.T) {
	ctx := context.Background()
	s := newRSFSuite(t, ctx)
	s.exec(t, ctx, `UPDATE projects SET notifier_senders = '{Jira}' WHERE id=$1`, s.project)
	b := s.bucket(t, ctx, "CCO")
	s.closeTask(t, ctx, b)
	before := s.logs(t, ctx, b)

	m := s.human(t, ctx, "jira-app", "Jira", "CCO-123 moved to Done")
	st := s.pass(t, ctx, capture.RulesModeLive)

	d := s.logged(t, ctx, m, capture.RulesModeLive, b, false,
		"the sender EQUALS an entry of projects.notifier_senders ('Jira'): bot traffic keeps logging silently (T2)")
	if !strings.Contains(strings.ToLower(d.reason), "notifier") {
		t.Errorf("decision reason %q does not name the notifier list (criterion 5); Verification 4 reads it", d.reason)
	}
	if got := s.logs(t, ctx, b); got != before+1 {
		t.Errorf("log events on the bucket went %d -> %d, want +1: the notifier's log line is today's behaviour", before, got)
	}
	if got := s.status(t, ctx, b); got != "closed" {
		t.Errorf("the bucket is %q; nothing reopens it", got)
	}
	if st.Resurfaced != 0 || st.Appended != 1 {
		t.Errorf("RulesStats = %+v, want Resurfaced 0, Appended 1", st)
	}
}

// T1 and T13 (Q1: an Avviato DM naming a key follows rule 10's attribution).
// MUTATION: a literal false for resurface in insertDecision → red.
func TestCaptureResurface_Integration_AHumanOnAClosedTaskResurfaces(t *testing.T) {
	ctx := context.Background()
	s := newRSFSuite(t, ctx)
	// The list is SET and still does not swallow a human: equality, not substring.
	s.exec(t, ctx, `UPDATE projects SET notifier_senders = '{Jira}' WHERE id=$1`, s.project)
	b := s.bucket(t, ctx, "CCW")
	s.closeTask(t, ctx, b)
	before := s.logs(t, ctx, b)

	treetop := s.human(t, ctx, "asunda45", "asunda45", "can you check CCW-10355?")
	avviato := s.message(t, ctx, "jose", rsfDM(rsfAvviatoWS, "jose"), "slack", "Jose Garcia",
		"any news on CCW-10400?", s.avviato)
	st := s.pass(t, ctx, capture.RulesModeLive)

	s.logged(t, ctx, treetop, capture.RulesModeLive, b, true,
		"T1: a human's message logged onto a CLOSED task, not activity, no open dismissal, not the connector's "+
			"copy, not a notifier — capture records resurface=true (CC3)")
	s.logged(t, ctx, avviato, capture.RulesModeLive, b, true,
		"T13 / Q1: an Avviato DM that names the key is attributed by rule 10 like a Treetop one and resurfaces "+
			"in the same project; no workspace special case")
	if got := s.logs(t, ctx, b); got != before+2 {
		t.Errorf("log events on the bucket went %d -> %d, want +2: the log line is still appended (today's behaviour)", before, got)
	}
	if got := s.status(t, ctx, b); got != "closed" {
		t.Errorf("the bucket is %q; CC2: the closed task stays closed (a reopen resurrects a catch-all, K3)", got)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool='task_reopen'`, b); n != 0 {
		t.Errorf("task_reopen was called %d time(s) on the bucket; capture makes NO new executor call", n)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, s.project); n != 1 {
		t.Errorf("%d tasks in the project, want 1: capture creates nothing for a resurfaced message (promote does, later)", n)
	}
	if st.Resurfaced != 2 || st.Appended != 2 {
		t.Errorf("RulesStats = %+v, want Resurfaced 2, Appended 2 (criterion 9)", st)
	}
}

// ---- criterion 6: everything another path owns writes false and still logs -----------

// T6/T11: a ready, an in_progress and a delivered task.
func TestCaptureResurface_Integration_ANotClosedTaskNeverResurfaces(t *testing.T) {
	ctx := context.Background()
	s := newRSFSuite(t, ctx)
	tasks := map[string]int64{}
	for prefix, status := range map[string]string{"CCW": "ready", "CCA": "in_progress", "CCO": "delivered"} {
		task := s.bucket(t, ctx, prefix)
		if status != "ready" {
			s.exec(t, ctx, `UPDATE tasks SET status=$2 WHERE id=$1`, task, status) // fixture: the status column's value
		}
		tasks[status] = task
	}
	before := map[string]int{}
	msgs := map[string]int64{}
	for status, task := range tasks {
		before[status] = s.logs(t, ctx, task)
	}
	msgs["ready"] = s.human(t, ctx, "on-ready", "asunda45", "news on CCW-5?")
	msgs["in_progress"] = s.human(t, ctx, "on-inprogress", "asunda45", "news on CCA-5?")
	msgs["delivered"] = s.human(t, ctx, "on-delivered", "asunda45", "news on CCO-5?")
	st := s.pass(t, ctx, capture.RulesModeLive)

	for status, task := range tasks {
		s.logged(t, ctx, msgs[status], capture.RulesModeLive, task, false,
			"the task is "+status+", not closed: its log line is on the board already (T6, T11)")
		if got := s.logs(t, ctx, task); got != before[status]+1 {
			t.Errorf("%s task: log events %d -> %d, want +1", status, before[status], got)
		}
	}
	if st.Resurfaced != 0 {
		t.Errorf("RulesStats.Resurfaced = %d, want 0", st.Resurfaced)
	}
}

// T10: SWT-36's guarded reopen still runs (rules_reopen_integration_test.go unmodified).
func TestCaptureResurface_Integration_AnOpenDismissalStaysSWT36s(t *testing.T) {
	ctx := context.Background()
	s := newRSFSuite(t, ctx)
	b := s.bucket(t, ctx, "CCW")
	s.execute(t, ctx, "task_dismiss", rsfHuman, b, fmt.Sprintf(`{"task_id":%d,"reason_code":"not_actionable"}`, b))
	before := s.logs(t, ctx, b)

	m := s.human(t, ctx, "after-dismiss", "asunda45", "still need CCW-77")
	st := s.pass(t, ctx, capture.RulesModeLive)

	d := s.logged(t, ctx, m, capture.RulesModeLive, b, false,
		"the task has an OPEN dismissal: SWT-36's guarded reopen owns it (T10)")
	if !strings.Contains(d.reason, "was dismissed") {
		t.Errorf("decision reason %q lost SWT-36's dismissal clause", d.reason)
	}
	if got := s.status(t, ctx, b); got != "ready" {
		t.Errorf("the dismissed task is %q, want ready: SWT-36's reopen is still called", got)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool='task_reopen' AND actor=$2`,
		b, rsfActor); n != 1 {
		t.Errorf("task_reopen audit rows = %d, want 1", n)
	}
	if got := s.logs(t, ctx, b); got != before+1 {
		t.Errorf("log events %d -> %d, want +1", before, got)
	}
	if st.Reopened != 1 || st.Resurfaced != 0 {
		t.Errorf("RulesStats = %+v, want Reopened 1, Resurfaced 0", st)
	}
}

// T8: a reviving rule on a closed task stays SWT-45's (revive, own_action, blind).
func TestCaptureResurface_Integration_ARevivingRuleStaysSWT45s(t *testing.T) {
	ctx := context.Background()
	s := newRSFSuite(t, ctx)
	s.insID(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, priority, enabled,
		                            revive, note)
		 VALUES ($1,'body_regex','CCR-[0-9]+','jira','(CCR-[0-9]+)',95,true,true,'itest-capcc revive') RETURNING id`,
		s.project)
	task := s.createdBy(t, ctx, "CCR-7", func() int64 {
		return s.human(t, ctx, "rev-setup", "Dana Ruiz", "please look at CCR-7")
	})
	s.closeTask(t, ctx, task)
	before := s.logs(t, ctx, task)

	m := s.human(t, ctx, "rev-follow", "asunda45", "any update on CCR-7?")
	st := s.pass(t, ctx, capture.RulesModeLive)

	s.logged(t, ctx, m, capture.RulesModeLive, task, false,
		"the winner is an ACTIVITY rule (revive): the revive / own_action / blind outcomes are SWT-45's (T8)")
	if got := s.status(t, ctx, task); got == "closed" {
		t.Errorf("the reviving rule's closed task stayed closed; the SWT-45 path changed (setup is wrong or the revive broke)")
	}
	if got := s.logs(t, ctx, task); got != before+1 {
		t.Errorf("log events %d -> %d, want +1", before, got)
	}
	if st.Revived != 1 || st.Resurfaced != 0 {
		t.Errorf("RulesStats = %+v, want Revived 1, Resurfaced 0", st)
	}
}

// T9: the Jira connector's own comment copy (rules 3–5's shape, channel 'jira').
func TestCaptureResurface_Integration_TheJiraConnectorCopyNeverResurfaces(t *testing.T) {
	ctx := context.Background()
	s := newRSFSuite(t, ctx)
	s.insID(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, priority, enabled, note)
		 VALUES ($1,'thread_key_prefix',$2,'jira','[A-Z]+-[0-9]+$',50,true,'itest-capcc rule4') RETURNING id`,
		s.project, rsfJiraPfx)
	key := rsfJiraPfx + "CCJ-1"
	task := s.createdBy(t, ctx, "CCJ-1", func() int64 {
		return s.message(t, ctx, "jira-setup", key, "jira", "Katie Evans", "a comment on the ticket", s.treetop)
	})
	s.closeTask(t, ctx, task)
	before := s.logs(t, ctx, task)

	m := s.message(t, ctx, "jira-follow", key, "jira", "Katie Evans", "another comment", s.treetop)
	st := s.pass(t, ctx, capture.RulesModeLive)

	s.logged(t, ctx, m, capture.RulesModeLive, task, false,
		"pm.channel == jira.Channel: the poller reads whole projects, so its copy never resurfaces (SWT-45 J3, T9)")
	if got := s.logs(t, ctx, task); got != before+1 {
		t.Errorf("log events %d -> %d, want +1", before, got)
	}
	if st.Resurfaced != 0 {
		t.Errorf("RulesStats.Resurfaced = %d, want 0", st.Resurfaced)
	}
}

// ---- criteria 7 and 9: shadow records the same flag and calls nothing -----------------

func TestCaptureResurface_Integration_ShadowRecordsTheSameFlagAndCallsNothing(t *testing.T) {
	ctx := context.Background()
	s := newRSFSuite(t, ctx)
	b := s.bucket(t, ctx, "CCW")
	s.closeTask(t, ctx, b)
	auditBefore := s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE actor=$1`, rsfActor)
	logsBefore := s.logs(t, ctx, b)

	m := s.human(t, ctx, "shadow", "asunda45", "can you check CCW-10355?")
	st := s.pass(t, ctx, capture.RulesModeShadow)

	s.logged(t, ctx, m, capture.RulesModeShadow, b, true,
		"criterion 7: the decision is mode-free; shadow records the same resurface value")
	if st.Resurfaced != 1 {
		t.Errorf("RulesStats.Resurfaced in SHADOW = %d, want 1 (CC7: counted in both modes, a recorded fact)", st.Resurfaced)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE actor=$1`, rsfActor); n != auditBefore {
		t.Errorf("a SHADOW pass made %d executor call(s); shadow calls nothing", n-auditBefore)
	}
	if got := s.logs(t, ctx, b); got != logsBefore {
		t.Errorf("a SHADOW pass appended %d log line(s)", got-logsBefore)
	}

	st2 := s.pass(t, ctx, capture.RulesModeLive)
	s.logged(t, ctx, m, capture.RulesModeLive, b, true, "the live row carries the same value the shadow row did")
	if st2.Resurfaced != 1 {
		t.Errorf("RulesStats.Resurfaced in LIVE = %d, want 1", st2.Resurfaced)
	}
	if got := s.logs(t, ctx, b); got != logsBefore+1 {
		t.Errorf("the live pass: log events %d -> %d, want +1", logsBefore, got)
	}
}

// ---- criterion 8: the gate path writes the default false (CC8) --------------------------

// gate_scope_integration_test.go's shape: a held jira match on a gated project
// whose ref task is CLOSED, resolved by the gate into a task_log. The gate
// path only logs (SWT-45's Part D section) and writes resurface=false — the
// recorded residual; gated projects are not inquiry-armed today.
func TestCaptureResurface_Integration_TheGatePathWritesResurfaceFalse(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	rsfRequire0034(t, ctx, s.pool)
	task := s.taskWithRef(t, ctx, "GTE-930")
	if _, err := s.ex.Execute(ctx, executor.Call{Tool: "task_close", Actor: cgHuman, TaskID: &task,
		Args: []byte(fmt.Sprintf(`{"task_id":%d,"reason":"itest-capgate done"}`, task))}); err != nil {
		t.Fatalf("setup task_close: %v", err)
	}
	s.fake.put(cgIssue{"GTE-930", "indeterminate", "In Progress", cgOwnID})
	m := s.mention(t, ctx, "resurface-gate", "GTE-930", time.Minute)
	s.live(t, ctx)
	s.gate(t, ctx)

	g, ok := s.decision(t, ctx, m, "gate")
	if !ok || g.action != "task_log" || g.taskID == nil || *g.taskID != task {
		t.Fatalf("gate row = %+v (found %v), want task_log on the closed task %d", g, ok, task)
	}
	for _, mode := range []string{"live", "gate"} {
		var resurface bool
		if err := s.pool.QueryRow(ctx,
			`SELECT resurface FROM capture_decisions WHERE message_id=$1 AND mode=$2 ORDER BY id DESC LIMIT 1`,
			m, mode).Scan(&resurface); err != nil {
			t.Fatalf("read the %s row's resurface: %v", mode, err)
		}
		if resurface {
			t.Errorf("the %s row carries resurface=true. CC8: the gate and route writers stay resurface=false "+
				"(the recorded residual; a gate-path resurface is future work)", mode)
		}
	}
	var status string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, task).Scan(&status); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if status != "closed" {
		t.Errorf("the gate's task_log reopened the closed task (%q)", status)
	}
	if n := s.n(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='log'`, task); n != 1 {
		t.Errorf("log events on the task = %d, want 1 (the gate still logs)", n)
	}
}
