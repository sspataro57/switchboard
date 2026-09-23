//go:build integration

package capture_test

// REPRODUCTION — SWT-80 / swb #553, bug `revived-task-not-in-incoming`
// (docs/bugs/revived-task-not-in-incoming.md, docs/bugs/revived-task-not-in-incoming_REPRO.md).
//
// Report (Salvador, 2026-09-23): "so there is comment in jira from katie and the
// task didn't reopen". Production: task #381 (WEB-10362, human, collaboratory)
// was closed by the ticketstatus reconciler; Katie's Jira comment (connector
// copy, message 459664, rule 71 = thread_key_contains on the jira thread,
// revive=true) revived it closed -> ready, but tasks.activity_at /
// activity_by_message_id stayed on the day-old message 403477. A revived task
// whose activity stamp predates its revive is not "needs review" on the board
// (activity_at > reviewed_at), so it sits in QUEUE instead of INCOMING.
//
// Each test asserts BOTH the part that works today (the status comes back) and
// the part the report is about (the task's activity stamp is the new comment,
// newer than reviewed_at — i.e. it lands in INCOMING). The second half is RED
// today; it goes green once fixed.
//
// Run against a PRIVATE database (the compose Postgres is shared; this suite
// deletes capture_decisions wholesale, the precedent in rules_revive_*):
//
//	psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c "CREATE DATABASE ops_revincoming"
//	make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_revincoming?sslmode=disable'
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_revincoming?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run TestRegression_SWT80_ ./internal/capture/ -v
//
// "Test the column, not the fixture": the rules are capture_rules rows, the
// task is created by a live capture pass (external_refs written by
// link_external_ref), closed by ticketstatus.Run from a stored done snapshot
// (the reconciler's own task_close) or by task_dismiss on the executor, and
// every assertion reads tasks' columns.

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
	"github.com/sspataro57/switchboard/internal/connector/jira"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/ticketstatus"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	s80Slug      = "itest-swt80"
	s80JiraAcct  = "itest-swt80-jira@example.test"
	s80Site      = "jira:swt80.jira.com:"
	s80RevPfx    = s80Site + "RVA-" // rule 71's shape: thread_key_contains, revive=true
	s80PlainPfx  = s80Site + "PLN-" // a NON-reviving jira rule (the pure SWT-36 path)
	s80Actor     = "capture:itest-swt80"
	s80Dismisser = "dashboard:itest-swt80"
	s80Own       = "acc-itest-swt80-own"
	s80TSActor   = "ticketstatus:jira"
)

type s80Suite struct {
	pool    *pgxpool.Pool
	ex      *executor.Executor
	project int64
	acct    int64
	seq     int
}

func newS80Suite(t *testing.T, ctx context.Context) *s80Suite {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use a private db on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	s80Cleanup(t, ctx, pool)
	t.Cleanup(func() { s80Cleanup(t, ctx, pool) })

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	s := &s80Suite{pool: pool, ex: executor.New(reg, checker, audit.NewPGStore(pool))}

	// collaboratory's shape: NOT gated.
	s.project = s.id(t, ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality, ticket_assignee_gate)
		 VALUES ($1,$1,'itest-swt80-client','manual','dashboard','/tmp/itest-swt80','any',false) RETURNING id`, s80Slug)
	s.acct = s.id(t, ctx,
		`INSERT INTO source_accounts (provider, account_email, scopes, send_enabled, calendar_in_availability, sync_cursor)
		 VALUES ('jira',$1,'{}',false,false,$2::jsonb) RETURNING id`, s80JiraAcct, `{"own_account_id":"`+s80Own+`"}`)

	rule := func(kind, pattern string, priority int, revive, enabled bool) {
		s.id(t, ctx,
			`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, url_template,
			                            priority, enabled, note, revive, addressed)
			 VALUES ($1,$5,$2,'jira','[A-Z]+-[0-9]+$','https://swt80.jira.com/browse/{key}',$3,$6,
			         'itest-swt80',$4,false) RETURNING id`, s.project, pattern, priority, revive, kind, enabled)
	}
	rule("thread_key_contains", s80RevPfx, 92, true, true)    // production rule 71
	rule("thread_key_prefix", s80RevPfx, 50, false, false)    // production rule 3 (created #381; disabled today)
	rule("thread_key_contains", s80PlainPfx, 92, false, true) // non-reviving
	return s
}

func s80Cleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE account_email = '` + s80JiraAcct + `')`
	const projs = `(SELECT id FROM projects WHERE slug = '` + s80Slug + `')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	const actors = `('` + s80Actor + `','` + s80Dismisser + `')`
	for _, q := range []string{
		`DELETE FROM capture_decisions`, // wholesale: EvaluateRules' pending set is global
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
		`DELETE FROM projects WHERE slug = '` + s80Slug + `'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE '` + s80Site + `%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE account_email = '` + s80JiraAcct + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func (s *s80Suite) id(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", q, err)
	}
	return id
}

// setRules: reviving=true is production today (rule 71 on, rule 3 off);
// false is the state when #381 was created.
func (s *s80Suite) setRules(t *testing.T, ctx context.Context, reviving bool) {
	t.Helper()
	s.exec(t, ctx, `UPDATE capture_rules SET enabled = (revive = $3) WHERE project_id=$1 AND pattern=$2`,
		s.project, s80RevPfx, reviving)
}

func (s *s80Suite) exec(t *testing.T, ctx context.Context, q string, args ...any) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func (s *s80Suite) dbNow(t *testing.T, ctx context.Context) time.Time {
	t.Helper()
	var now time.Time
	if err := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatalf("read db clock: %v", err)
	}
	return now
}

// comment stores an inbound jira-channel comment (the connector copy) on
// thread jira:{site}:{KEY}, ingested at `created`.
func (s *s80Suite) comment(t *testing.T, ctx context.Context, threadKey, sender string, created time.Time) int64 {
	t.Helper()
	s.seq++
	tag := fmt.Sprintf("%d-%d", time.Now().UnixNano(), s.seq)
	var thread int64
	if err := s.pool.QueryRow(ctx, `SELECT id FROM normalized_threads WHERE thread_key=$1`, threadKey).Scan(&thread); err != nil {
		thread = s.id(t, ctx, `INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,$1,'[]') RETURNING id`, threadKey)
	}
	raw := s.id(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,'{}',$3, now()) RETURNING id`, s.acct, "itest-swt80-"+tag, "itest-swt80-h-"+tag)
	return s.id(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel, created_at)
		 VALUES ($1,$2,'inbound',$3,$4,$5,'',$6,'jira',$4) RETURNING id`,
		raw, thread, "<itest-swt80-"+tag+">", created, "a comment on "+threadKey+" ("+tag+")", sender)
}

func (s *s80Suite) pass(t *testing.T, ctx context.Context) capture.RulesStats {
	t.Helper()
	stats, err := capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{Mode: capture.RulesModeLive, Actor: s80Actor})
	if err != nil {
		t.Fatalf("EvaluateRules(live): %v", err)
	}
	return stats
}

// snapshot stores KEY's issue row under the polled jira account (what the
// reconciler decides from).
func (s *s80Suite) snapshot(t *testing.T, ctx context.Context, key, category, name string) {
	t.Helper()
	doc := `{"id":"1","key":"` + key + `","fields":{"summary":"itest ` + key + `","status":{"name":"` + name +
		`","statusCategory":{"key":"` + category + `"}},"assignee":{"accountId":"` + s80Own + `"}}}`
	s.exec(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, ingested_at)
		 VALUES ($1,$2,$3::jsonb,$4, now())
		 ON CONFLICT (source_account_id, external_id) DO UPDATE
		   SET raw_json = EXCLUDED.raw_json, content_hash = EXCLUDED.content_hash, ingested_at = EXCLUDED.ingested_at`,
		s.acct, jira.IssueRawID(key), doc, "itest-swt80-"+key+"-"+category+"-"+name)
}

// createTask: KEY's first comment, ingested 30 minutes ago, creates the task
// through a live pass. Returns the task and that first message.
func (s *s80Suite) createTask(t *testing.T, ctx context.Context, pfx, key string) (int64, int64) {
	t.Helper()
	start := s.dbNow(t, ctx).Add(-30 * time.Minute)
	first := s.comment(t, ctx, pfx+key[4:], "Katie Evans", start)
	s.pass(t, ctx)
	var task int64
	if err := s.pool.QueryRow(ctx,
		`SELECT r.task_id FROM external_refs r JOIN tasks t ON t.id=r.task_id
		  WHERE r.system='jira' AND r.external_key=$1 AND t.project_id=$2`, key, s.project).Scan(&task); err != nil {
		t.Fatalf("setup: the first live pass created no task for %s: %v", key, err)
	}
	// Production's #381 is a human task; make that explicit (column, not assumed).
	s.exec(t, ctx, `UPDATE tasks SET assignee_type='human', worker_type=NULL WHERE id=$1`, task)
	return task, first
}

type s80Row struct {
	status       string
	activityAt   *time.Time
	activityBy   *int64
	reviewedAt   *time.Time
	surfacedBy   *int64
	closedAt     *time.Time
	closedFromSt *string
}

func (s *s80Suite) row(t *testing.T, ctx context.Context, task int64) s80Row {
	t.Helper()
	var r s80Row
	if err := s.pool.QueryRow(ctx,
		`SELECT status, activity_at, activity_by_message_id, reviewed_at, surfaced_by_message_id, closed_at, closed_from_status
		   FROM tasks WHERE id=$1`, task).
		Scan(&r.status, &r.activityAt, &r.activityBy, &r.reviewedAt, &r.surfacedBy, &r.closedAt, &r.closedFromSt); err != nil {
		t.Fatalf("read task %d: %v", task, err)
	}
	return r
}

func s80Ptr[T any](p *T) string {
	if p == nil {
		return "NULL"
	}
	return fmt.Sprint(*p)
}

// assertIncoming is the report's expectation: the new comment is the task's
// activity stamp, and the stamp is newer than reviewed_at ("needs review" =
// activity_at > reviewed_at, SWT-72) — the task is in INCOMING.
func (s *s80Suite) assertIncoming(t *testing.T, ctx context.Context, task, msg int64, label string) {
	t.Helper()
	r := s.row(t, ctx, task)
	if r.activityBy == nil || *r.activityBy != msg {
		t.Errorf("%s: task %d activity_by_message_id = %s, want %d (the new comment). activity_at=%s reviewed_at=%s — "+
			"the task came back but its activity stamp is older than the comment, so it is not in INCOMING",
			label, task, s80Ptr(r.activityBy), msg, s80Ptr(r.activityAt), s80Ptr(r.reviewedAt))
	}
	var needsReview bool
	if err := s.pool.QueryRow(ctx,
		`SELECT status <> 'closed' AND activity_at IS NOT NULL AND (reviewed_at IS NULL OR activity_at > reviewed_at)
		   FROM tasks WHERE id=$1`, task).Scan(&needsReview); err != nil {
		t.Fatalf("read needs-review of task %d: %v", task, err)
	}
	if !needsReview {
		t.Errorf("%s: task %d is not 'needs review' (activity_at=%s <= reviewed_at=%s): it sits in QUEUE, not INCOMING",
			label, task, s80Ptr(r.activityAt), s80Ptr(r.reviewedAt))
	}
}

// ---- Variant A: production's #381 exactly ------------------------------------
// Reconciler close (ticketstatus.Run -> task_close as ticketstatus:jira), then a
// jira-channel comment matched by a revive=true thread_key_contains rule.
func TestRegression_SWT80_ReconcilerClosedTaskRevivedByJiraCommentLandsInIncoming(t *testing.T) {
	ctx := context.Background()
	s := newS80Suite(t, ctx)
	const key = "RVA-10362"

	// #381 predates its reviving rule: rule 3 (non-reviving thread_key_prefix)
	// created it unsurfaced. A creation BY a reviving rule surfaces the task, and
	// the reconciler then holds it open (SWT-45 J11). So: rule-3 shape on and
	// rule-71 shape off while the task is created and closed, then the swap that
	// is production's state today (capture_rules.enabled).
	s.setRules(t, ctx, false)
	s.snapshot(t, ctx, key, "indeterminate", "In Progress")
	task, _ := s.createTask(t, ctx, s80RevPfx, key)

	s.snapshot(t, ctx, key, "done", "Done")
	if _, err := ticketstatus.Run(ctx, s.pool, s.ex, ticketstatus.Config{}); err != nil {
		t.Fatalf("ticketstatus.Run: %v", err)
	}
	before := s.row(t, ctx, task)
	if before.status != "closed" {
		t.Fatalf("setup: the reconciler did not close %s (status %q)", key, before.status)
	}
	var closes int
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool='task_close' AND actor=$2 AND status='ok'`,
		task, s80TSActor).Scan(&closes)
	if closes != 1 {
		t.Fatalf("setup: want exactly one task_close by %s, got %d", s80TSActor, closes)
	}
	t.Logf("after reconciler close: activity_by=%s activity_at=%s reviewed_at=%s closed_at=%s",
		s80Ptr(before.activityBy), s80Ptr(before.activityAt), s80Ptr(before.reviewedAt), s80Ptr(before.closedAt))

	s.setRules(t, ctx, true)
	c := s.comment(t, ctx, s80RevPfx+key[4:], "Katie Evans", s.dbNow(t, ctx))
	stats := s.pass(t, ctx)

	after := s.row(t, ctx, task)
	t.Logf("after comment %d: status=%s revived=%d activity_by=%s activity_at=%s reviewed_at=%s surfaced_by=%s",
		c, after.status, stats.Revived, s80Ptr(after.activityBy), s80Ptr(after.activityAt), s80Ptr(after.reviewedAt),
		s80Ptr(after.surfacedBy))

	// Works today: the revive.
	if after.status != "ready" || stats.Revived != 1 {
		t.Fatalf("the comment did not revive the task: status=%q Revived=%d (want ready / 1)", after.status, stats.Revived)
	}
	if after.surfacedBy == nil || *after.surfacedBy != c {
		t.Errorf("surfaced_by_message_id = %s, want %d", s80Ptr(after.surfacedBy), c)
	}
	// The bug: the revived task is not in INCOMING.
	s.assertIncoming(t, ctx, task, c, "reconciler close + activity revive")
}

// ---- Variant B: SWT-36 — dismissed, then a comment under the REVIVING rule ----
func TestRegression_SWT80_DismissedTaskReopenedByCommentOnRevivingRuleLandsInIncoming(t *testing.T) {
	ctx := context.Background()
	s := newS80Suite(t, ctx)
	const key = "RVA-20001"
	s80DismissThenComment(t, ctx, s, s80RevPfx, key, "reviving rule")
}

// ---- Variant C: SWT-36 — dismissed, then a comment under a NON-reviving rule --
func TestRegression_SWT80_DismissedTaskReopenedByCommentOnPlainRuleLandsInIncoming(t *testing.T) {
	ctx := context.Background()
	s := newS80Suite(t, ctx)
	const key = "PLN-20002"
	s80DismissThenComment(t, ctx, s, s80PlainPfx, key, "non-reviving rule")
}

func s80DismissThenComment(t *testing.T, ctx context.Context, s *s80Suite, pfx, key, label string) {
	t.Helper()
	task, _ := s.createTask(t, ctx, pfx, key)
	if _, err := s.ex.Execute(ctx, executor.Call{Tool: "task_dismiss", Actor: s80Dismisser, TaskID: &task,
		Args: []byte(fmt.Sprintf(`{"task_id":%d,"reason_code":"not_actionable"}`, task))}); err != nil {
		t.Fatalf("setup: task_dismiss: %v", err)
	}
	before := s.row(t, ctx, task)
	if before.status != "closed" {
		t.Fatalf("setup: task_dismiss left %s's task %q, want closed", key, before.status)
	}
	t.Logf("after dismissal: activity_by=%s activity_at=%s reviewed_at=%s",
		s80Ptr(before.activityBy), s80Ptr(before.activityAt), s80Ptr(before.reviewedAt))

	c := s.comment(t, ctx, pfx+key[4:], "Katie Evans", s.dbNow(t, ctx))
	s.pass(t, ctx)

	after := s.row(t, ctx, task)
	var reopenedBy *int64
	_ = s.pool.QueryRow(ctx, `SELECT reopened_by_message_id FROM task_dismissals WHERE task_id=$1 ORDER BY id DESC LIMIT 1`,
		task).Scan(&reopenedBy)
	t.Logf("after comment %d: status=%s dismissal.reopened_by_message_id=%s activity_by=%s activity_at=%s reviewed_at=%s",
		c, after.status, s80Ptr(reopenedBy), s80Ptr(after.activityBy), s80Ptr(after.activityAt), s80Ptr(after.reviewedAt))

	// Works today (SWT-36): the reopen.
	if after.status == "closed" {
		t.Fatalf("%s: the comment did not reopen the dismissed task (still closed)", label)
	}
	if reopenedBy == nil || *reopenedBy != c {
		t.Errorf("%s: dismissal reopened_by_message_id = %s, want %d", label, s80Ptr(reopenedBy), c)
	}
	s.assertIncoming(t, ctx, task, c, "dismissal reopen, "+label)
}

// ---- Control: the same comment on an OPEN task does land in INCOMING ---------
func TestRegression_SWT80_Control_CommentOnOpenTaskLandsInIncoming(t *testing.T) {
	ctx := context.Background()
	s := newS80Suite(t, ctx)
	const key = "RVA-30003"
	task, _ := s.createTask(t, ctx, s80RevPfx, key)
	// A human review of the creation (reviewed_at stamped via task_requeue would be
	// the dashboard path; a direct stamp is enough for a control).
	s.exec(t, ctx, `UPDATE tasks SET reviewed_at = clock_timestamp() WHERE id=$1`, task)
	c := s.comment(t, ctx, s80RevPfx+key[4:], "Katie Evans", s.dbNow(t, ctx))
	s.pass(t, ctx)
	if st := s.row(t, ctx, task).status; st != "ready" {
		t.Fatalf("control: task status %q, want ready", st)
	}
	s.assertIncoming(t, ctx, task, c, "control (open task)")
}
