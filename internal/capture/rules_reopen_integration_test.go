//go:build integration

package capture_test

// SWT-36 (docs/tickets/dismiss-reopen-on-activity_SPEC.md) against a real
// database, the CAPTURE path: criteria 13, 14, 15, 18 (capture's pending set
// and ObserveOutbound), 23 (the open predicate in taskForExternalRef) and 26,
// plus D10's order (log first, then reopen) and D11 (shadow reopens nothing).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run CaptureReopen ./internal/capture/
//
// Build-tagged `integration` AND env-gated on DATABASE_URL, with a FATAL guard
// on 192.168.50.49. NO LLM, NO network.
//
// "TEST THE COLUMN, NOT THE FIXTURE": the task is CREATED by a first live pass
// (so external_refs is written by link_external_ref, not the harness), the
// thread carries both an inbound and an outbound message, and the dismissal goes
// through task_dismiss on the executor (criterion 26). The dismissed-ness is
// read from task_dismissals.reopened_at by taskForExternalRef and the reopen
// decision is the handler's. Mutations are named inline.
//
// ---- IMPOSED SURFACE ----------------------------------------------------------
//
//	type RulesStats struct { ...; Reopened int } // zero in shadow, always (criterion 14)
//
//	taskForExternalRef also returns the task's OPEN dismissal id (status='closed'
//	AND reopened_at IS NULL); ruleDecision carries it; the task_log reason gains
//	"task N was dismissed (reason_code); reopen requested against dismissal D".
//	Live: appendRuleLog, THEN task_reopen {task_id, dismissal_id, message_id,
//	reason} as cfg.Actor. A failed call fails the pass.
//
// GREENFIELD NOTE — EXPECTED RED: RulesStats.Reopened does not exist, so this
// file compile-FAILs the integration build first.
//
// CROSS-SUITE DISCIPLINE. EvaluateRules' pending set is GLOBAL, so a live pass
// here writes capture_decisions rows for any other suite's leftover inbound
// messages; those rows FK normalized_messages. This suite therefore deletes
// capture_decisions WHOLESALE at start and end — the rules_integration_test.go
// precedent, for the same reason, compose db only. It owns project
// itest-capreopen, account (jira, itest-capreopen-jira@example.test), threads
// jira:capreopen.jira.com:%, and audit rows by task plus its three actors.

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
	crrSlug    = "itest-capreopen"
	crrAccount = "itest-capreopen-jira@example.test"
	crrPrefix  = "jira:capreopen.jira.com:"
	crrKey     = "REO-1"
	crrThread  = crrPrefix + crrKey
	crrActor   = "capture:itest-capreopen"
	crrHuman   = "dashboard:itest-capreopen"
	crrCloser  = "opsctl:itest-capreopen"
)

type crrSuite struct {
	pool    *pgxpool.Pool
	ex      *executor.Executor
	project int64
	account int64
	thread  int64
}

func newCRRSuite(t *testing.T, ctx context.Context) *crrSuite {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db (cleanup deletes capture_decisions " +
			"wholesale); use the compose db on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	crrCleanup(t, ctx, pool)
	t.Cleanup(func() { crrCleanup(t, ctx, pool) })

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name='task_dismissals' AND column_name IN ('closed_from_status','reopened_at')`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("task_dismissals lacks migration 0026's columns (n=%d, err=%v); apply "+
			"migrations/0026_dismissal_reopen.sql to the compose db", n, err)
	}

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	s := &crrSuite{pool: pool, ex: executor.New(reg, checker, audit.NewPGStore(pool))}

	s.project = s.insID(t, ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,'itest-capreopen-client','manual','dashboard','/tmp/itest-capreopen','any') RETURNING id`, crrSlug)
	s.insID(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, url_template,
		                            priority, enabled, note)
		 VALUES ($1,'thread_key_prefix',$2,'jira','[A-Z]+-[0-9]+$','https://capreopen.jira.com/browse/{key}',
		         50,true,'itest-capreopen') RETURNING id`, s.project, crrPrefix)
	s.account = s.insID(t, ctx,
		`INSERT INTO source_accounts (provider, account_email, scopes, send_enabled, calendar_in_availability)
		 VALUES ('jira',$1,'{}',false,false) RETURNING id`, crrAccount)
	s.thread = s.insID(t, ctx,
		`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,$2,'[]') RETURNING id`,
		crrThread, crrKey)
	return s
}

func crrCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE provider='jira' AND account_email='` + crrAccount + `')`
	const projs = `(SELECT id FROM projects WHERE slug='` + crrSlug + `')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	const actors = `('` + crrActor + `','` + crrHuman + `','` + crrCloser + `')`
	for _, q := range []string{
		`DELETE FROM capture_decisions`, // wholesale: see the header
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
		`DELETE FROM projects WHERE slug='` + crrSlug + `'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN
		   (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE '` + crrPrefix + `%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider='jira' AND account_email='` + crrAccount + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func (s *crrSuite) insID(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", q, err)
	}
	return id
}

func (s *crrSuite) count(t *testing.T, ctx context.Context, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

func (s *crrSuite) dbNow(t *testing.T, ctx context.Context) time.Time {
	t.Helper()
	var now time.Time
	if err := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatalf("read db clock: %v", err)
	}
	return now
}

// message seeds one jira-channel message on REO-1's thread.
func (s *crrSuite) message(t *testing.T, ctx context.Context, label, direction string, createdAt, sentAt time.Time) int64 {
	t.Helper()
	raw := s.insID(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,'{}',$3, now()) RETURNING id`,
		s.account, "itest-capreopen-"+label, "itest-capreopen-h-"+label)
	sender := "Jira <jira@capreopen.jira.com>"
	if direction == "outbound" {
		sender = "Salvador"
	}
	return s.insID(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		    body_text, subject, sender, channel, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'jira',$9) RETURNING id`,
		raw, s.thread, direction, crrPrefix+"comment:"+label, sentAt,
		"a comment on "+crrKey+" ("+label+")", crrKey, sender, createdAt)
}

func (s *crrSuite) pass(t *testing.T, ctx context.Context, mode string) capture.RulesStats {
	t.Helper()
	stats, err := capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{Mode: mode, Actor: crrActor})
	if err != nil {
		t.Fatalf("EvaluateRules(%s): %v", mode, err)
	}
	return stats
}

func (s *crrSuite) execute(t *testing.T, ctx context.Context, tool, actor string, taskID int64, args string) {
	t.Helper()
	if _, err := s.ex.Execute(ctx, executor.Call{Tool: tool, Actor: actor, Args: []byte(args), TaskID: &taskID}); err != nil {
		t.Fatalf("%s(%s) as %s: %v", tool, args, actor, err)
	}
}

type crrDecision struct {
	action string
	taskID *int64
	reason string
}

func (s *crrSuite) decision(t *testing.T, ctx context.Context, msg int64, mode string) (crrDecision, bool) {
	t.Helper()
	var d crrDecision
	var reason *string
	err := s.pool.QueryRow(ctx,
		`SELECT action, task_id, reason FROM capture_decisions WHERE message_id=$1 AND mode=$2
		  ORDER BY id DESC LIMIT 1`, msg, mode).Scan(&d.action, &d.taskID, &reason)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return d, false
		}
		t.Fatalf("read %s decision for message %d: %v", mode, msg, err)
	}
	if reason != nil {
		d.reason = *reason
	}
	return d, true
}

func (s *crrSuite) status(t *testing.T, ctx context.Context, taskID int64) string {
	t.Helper()
	var st string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, taskID).Scan(&st); err != nil {
		t.Fatalf("read task %d: %v", taskID, err)
	}
	return st
}

func (s *crrSuite) events(t *testing.T, ctx context.Context, taskID int64, eventType string) int {
	t.Helper()
	return s.count(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type=$2`, taskID, eventType)
}

type crrFixture struct {
	task        int64
	ours        int64 // outbound on the thread (criterion 26)
	dismissal   int64
	dismissedAt time.Time
}

// createTask: the first inbound message on REO-1 creates the task through the
// real live pass; our own outbound reply sits on the same thread.
func (s *crrSuite) createTask(t *testing.T, ctx context.Context) crrFixture {
	t.Helper()
	var f crrFixture
	start := s.dbNow(t, ctx).Add(-30 * time.Minute)
	s.message(t, ctx, "m1", "inbound", start, start)
	f.ours = s.message(t, ctx, "ours", "outbound", start.Add(5*time.Minute), start.Add(5*time.Minute))
	s.pass(t, ctx, capture.RulesModeLive)
	if err := s.pool.QueryRow(ctx,
		`SELECT r.task_id FROM external_refs r JOIN tasks t ON t.id = r.task_id
		  WHERE r.system='jira' AND r.external_key=$1 AND t.project_id=$2`, crrKey, s.project).Scan(&f.task); err != nil {
		t.Fatalf("setup: the first live pass created no task for %s: %v", crrKey, err)
	}
	if st := s.status(t, ctx, f.task); st != "ready" {
		t.Fatalf("setup: the capture-created task is %q, want ready", st)
	}
	return f
}

func (s *crrSuite) dismiss(t *testing.T, ctx context.Context, f *crrFixture, code string) {
	t.Helper()
	s.execute(t, ctx, "task_dismiss", crrHuman, f.task, fmt.Sprintf(`{"task_id":%d,"reason_code":%q}`, f.task, code))
	if err := s.pool.QueryRow(ctx,
		`SELECT id, created_at FROM task_dismissals WHERE task_id=$1 ORDER BY id DESC LIMIT 1`, f.task).
		Scan(&f.dismissal, &f.dismissedAt); err != nil {
		t.Fatalf("setup: read the dismissal: %v", err)
	}
}

// ---- criteria 13, 14, 18 (pending), D10: the follow-up reopens ----------------

// MUTATIONS that turn this red:
//   - taskForExternalRef returns no dismissal id -> a log line only, task closed;
//   - no task_reopen after appendRuleLog -> same;
//   - task_reopen BEFORE appendRuleLog -> the event order assertion (D10).
func TestCaptureReopen_Integration_FollowUpOnADismissedTicketReopensIt(t *testing.T) {
	ctx := context.Background()
	s := newCRRSuite(t, ctx)

	f := s.createTask(t, ctx)
	s.dismiss(t, ctx, &f, "handled_elsewhere")
	logsBefore := s.events(t, ctx, f.task, "log")

	m2 := s.message(t, ctx, "m2", "inbound", f.dismissedAt.Add(time.Minute), f.dismissedAt.Add(time.Minute))
	stats := s.pass(t, ctx, capture.RulesModeLive)

	d, ok := s.decision(t, ctx, m2, capture.RulesModeLive)
	if !ok || d.action != "task_log" || d.taskID == nil || *d.taskID != f.task {
		t.Fatalf("live decision for the follow-up = %+v (found=%v), want task_log on task %d", d, ok, f.task)
	}
	for _, want := range []string{
		fmt.Sprintf("task %d was dismissed (handled_elsewhere)", f.task),
		fmt.Sprintf("reopen requested against dismissal %d", f.dismissal),
	} {
		if !strings.Contains(d.reason, want) {
			t.Errorf("decision reason %q does not contain %q (criterion 13)", d.reason, want)
		}
	}
	if got := s.status(t, ctx, f.task); got != "ready" {
		t.Errorf("the dismissed ticket's task is %q after a new inbound notification, want ready (its "+
			"pre-dismissal status). Today the follow-up is logged silently and the task stays closed — the "+
			"exact failure this ticket fixes", got)
	}
	if n := s.events(t, ctx, f.task, "log"); n != logsBefore+1 {
		t.Errorf("log events went %d -> %d, want exactly one appended line", logsBefore, n)
	}
	var by *string
	var byMsg *int64
	if err := s.pool.QueryRow(ctx,
		`SELECT reopened_by, reopened_by_message_id FROM task_dismissals WHERE id=$1`, f.dismissal).
		Scan(&by, &byMsg); err != nil {
		t.Fatalf("read the stamp: %v", err)
	}
	if by == nil || *by != crrActor || byMsg == nil || *byMsg != m2 {
		t.Errorf("dismissal stamp = (by %v, msg %v), want (%s, %d) — the configured capture:{connector} actor",
			by, byMsg, crrActor, m2)
	}
	if n := s.count(t, ctx,
		`SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool='task_reopen' AND actor=$2`,
		f.task, crrActor); n != 1 {
		t.Errorf("task_reopen audit rows = %d, want 1 (invariant 3)", n)
	}
	if stats.Reopened != 1 {
		t.Errorf("RulesStats.Reopened = %d, want 1 (criterion 14)", stats.Reopened)
	}

	// D10: the log line first, then the reopen — a crash between them leaves
	// today's behaviour, never a reopened task with no explanation.
	var lastLog, lastStatus int64
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE((SELECT max(id) FROM task_events WHERE task_id=$1 AND event_type='log'),0),
		        COALESCE((SELECT max(id) FROM task_events WHERE task_id=$1 AND event_type='status_changed'),0)`,
		f.task).Scan(&lastLog, &lastStatus); err != nil {
		t.Fatalf("read event order: %v", err)
	}
	if !(lastLog < lastStatus) {
		t.Errorf("event order: last log id %d, last status_changed id %d — D10 wants the log append FIRST", lastLog, lastStatus)
	}

	// Criterion 18, capture's layer: our own outbound message is not pending.
	if n := s.count(t, ctx, `SELECT count(*) FROM capture_decisions WHERE message_id=$1`, f.ours); n != 0 {
		t.Errorf("the OUTBOUND message on the dismissed ticket's thread has %d capture decision(s), want 0 "+
			"(D13 layer 1: pendingMessages is direction='inbound')", n)
	}
}

// ---- criterion 14 / D11: shadow reopens nothing ------------------------------

func TestCaptureReopen_Integration_ShadowReopensNothing(t *testing.T) {
	ctx := context.Background()
	s := newCRRSuite(t, ctx)

	f := s.createTask(t, ctx)
	s.dismiss(t, ctx, &f, "not_actionable")
	s.message(t, ctx, "m2", "inbound", f.dismissedAt.Add(time.Minute), f.dismissedAt.Add(time.Minute))

	stats := s.pass(t, ctx, capture.RulesModeShadow)
	if stats.Reopened != 0 {
		t.Errorf("RulesStats.Reopened in SHADOW = %d, want 0, always (criterion 14)", stats.Reopened)
	}
	if got := s.status(t, ctx, f.task); got != "closed" {
		t.Errorf("a SHADOW pass reopened the task (%q). D11: shadow calls no executor tool", got)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool='task_reopen'`, f.task); n != 0 {
		t.Errorf("a shadow pass called task_reopen %d time(s)", n)
	}
}

// ---- criterion 15: a closed task WITHOUT an open dismissal is unchanged --------

func TestCaptureReopen_Integration_APlainClosedTaskOnlyLogs(t *testing.T) {
	ctx := context.Background()
	s := newCRRSuite(t, ctx)

	f := s.createTask(t, ctx)
	s.execute(t, ctx, "task_close", crrCloser, f.task, fmt.Sprintf(`{"task_id":%d,"reason":"done"}`, f.task))
	logsBefore := s.events(t, ctx, f.task, "log")

	m2 := s.message(t, ctx, "m2", "inbound", s.dbNow(t, ctx), s.dbNow(t, ctx))
	stats := s.pass(t, ctx, capture.RulesModeLive)

	d, ok := s.decision(t, ctx, m2, capture.RulesModeLive)
	if !ok || d.action != "task_log" || d.taskID == nil || *d.taskID != f.task {
		t.Fatalf("decision = %+v (found=%v), want task_log on %d", d, ok, f.task)
	}
	if strings.Contains(d.reason, "was dismissed") {
		t.Errorf("decision reason %q claims a dismissal on a PLAIN-closed task (D3)", d.reason)
	}
	if got := s.status(t, ctx, f.task); got != "closed" {
		t.Errorf("a plain-closed task came back (%q). D3: only a dismissal reopens on activity", got)
	}
	if n := s.events(t, ctx, f.task, "log"); n != logsBefore+1 {
		t.Errorf("log events went %d -> %d, want +1", logsBefore, n)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool='task_reopen'`, f.task); n != 0 {
		t.Errorf("task_reopen was called %d time(s) for a task with no dismissal", n)
	}
	if stats.Reopened != 0 {
		t.Errorf("RulesStats.Reopened = %d, want 0", stats.Reopened)
	}
}

// ---- criterion 23 (capture): only an OPEN dismissal counts -------------------

// MUTATION: drop `reopened_at IS NULL` from taskForExternalRef -> the stamped
// dismissal id is carried, the reason claims a dismissal, and this goes red.
func TestCaptureReopen_Integration_AReclosedTaskIsPlainClosed(t *testing.T) {
	ctx := context.Background()
	s := newCRRSuite(t, ctx)

	f := s.createTask(t, ctx)
	s.dismiss(t, ctx, &f, "duplicate")
	s.message(t, ctx, "m2", "inbound", f.dismissedAt.Add(time.Minute), f.dismissedAt.Add(time.Minute))
	s.pass(t, ctx, capture.RulesModeLive)
	if got := s.status(t, ctx, f.task); got != "ready" {
		t.Fatalf("setup: the activity reopen did not happen (%q); the follow-up test is red first", got)
	}
	s.execute(t, ctx, "task_close", crrCloser, f.task, fmt.Sprintf(`{"task_id":%d,"reason":"done"}`, f.task))

	m3 := s.message(t, ctx, "m3", "inbound", s.dbNow(t, ctx), s.dbNow(t, ctx))
	stats := s.pass(t, ctx, capture.RulesModeLive)

	d, _ := s.decision(t, ctx, m3, capture.RulesModeLive)
	if strings.Contains(d.reason, "was dismissed") {
		t.Errorf("decision reason %q carries the OLD, already-reopened dismissal. taskForExternalRef must "+
			"return an open dismissal only (reopened_at IS NULL)", d.reason)
	}
	if got := s.status(t, ctx, f.task); got != "closed" {
		t.Errorf("the re-closed task came back (%q)", got)
	}
	if stats.Reopened != 0 {
		t.Errorf("RulesStats.Reopened = %d, want 0", stats.Reopened)
	}
}

// ---- criterion 18: ObserveOutbound writes outbound_observed only --------------

func TestCaptureReopen_Integration_ObserveOutboundNeverReopens(t *testing.T) {
	ctx := context.Background()
	s := newCRRSuite(t, ctx)

	f := s.createTask(t, ctx)
	s.dismiss(t, ctx, &f, "handled_elsewhere")
	// The only thread->task link ObserveOutbound knows is a deliveries row.
	s.insID(t, ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, approval_source)
		 VALUES ($1,'jira_comment',$2,'itest-capreopen draft','drafted','switchboard') RETURNING id`,
		f.task, crrThread)
	now := s.dbNow(t, ctx)
	s.message(t, ctx, "direct", "outbound", f.dismissedAt.Add(time.Minute), now.Add(-time.Minute))

	if _, err := capture.ObserveOutbound(ctx, s.pool, capture.Jira); err != nil {
		t.Fatalf("ObserveOutbound(Jira): %v", err)
	}
	if n := s.events(t, ctx, f.task, "outbound_observed"); n < 1 {
		t.Errorf("no outbound_observed event on the dismissed task — the fixture does not exercise the observer")
	}
	if got := s.status(t, ctx, f.task); got != "closed" {
		t.Errorf("ObserveOutbound reopened the dismissed task (%q). D13: the observer never changes status", got)
	}
	if n := s.events(t, ctx, f.task, "status_changed"); n != 1 {
		t.Errorf("status_changed events = %d, want 1 (the dismissal only)", n)
	}
	var reopenedAt *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT reopened_at FROM task_dismissals WHERE id=$1`, f.dismissal).Scan(&reopenedAt); err != nil {
		t.Fatalf("read the dismissal: %v", err)
	}
	if reopenedAt != nil {
		t.Errorf("an outbound observation stamped the dismissal")
	}
}
