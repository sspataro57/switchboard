//go:build integration

package capture_test

// SWT-45 (docs/tickets/jira-activity-revive_SPEC.md) against a real database,
// the CAPTURE path end to end: criteria 2 (the two CHECKs), 18 (the flags and
// the gate are COLUMN-fed, both directions), 19 (taskForExternalRef returns the
// status), 21 (log THEN revive; a failed revive keeps the log), 22 (create,
// link, provenance, THEN task_mark_surfaced), 23 (activity on an open task only
// logs), 25 (shadow calls nothing and says "would revive"), 26 (S4: connector +
// email copies revive ONCE, both orders), 27 (an outbound Jira-mail-shaped
// message never revives), 37 (S2/S3 with the reconciler, no ping-pong) and 38
// (S5: an overriding creation survives the same-tick reconcile) — plus the
// owner's 2026-09-12 answer: a Slack mention of a key IS activity (revives or
// creates), and only NEW messages act (no backfill).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_iso45?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run CaptureRevive ./internal/capture/
//
// Build-tagged `integration` AND env-gated on DATABASE_URL, with a FATAL guard
// on 192.168.50.49. NO LLM, NO network (ticketstatus.Run is called with a nil
// lookup factory; every snapshot is already stored).
//
// "TEST THE COLUMN, NOT THE FIXTURE": every rule is seeded through the
// capture_rules COLUMNS (revive, addressed) and the gate through
// projects.ticket_assignee_gate; the tasks are created by a first live pass (so
// external_refs is written by link_external_ref) and closed through task_close;
// the revive's decision is the handler's. MUTATIONS named inline.
//
// ---- IMPOSED SURFACE ----------------------------------------------------------
//
//	type RulesStats struct { ...; Revived, SurfacedCreated int } // zero in shadow
//	loadRules selects r.revive, r.addressed, p.ticket_assignee_gate;
//	overrides(W) = revive AND (NOT gate OR addressed)  (J1)
//	live, ref + task closed + overrides -> task_append_log THEN
//	  task_reopen {task_id, message_id, revive:true, reason}; the decision reason
//	  says "revive requested"; Revived counts reopened:true answers.
//	live, no ref + overrides -> create_task, link_external_ref,
//	  task_set_source_thread, task_mark_surfaced (in that order); SurfacedCreated.
//	shadow: no executor call; the reason says "would revive".
//
// RED TODAY: 0030 is not applied (crvRequire0030), and RulesStats has neither
// counter, so this file does not compile.
//
// PART D (SWT-40, unmerged): TestCaptureRevive_Integration_TheGateIsColumnFed's
// addressed half is ALSO the column-fed test for Part D's `&& !winner.addressed`
// clause — once Part D's `held` lands, a missing clause holds the addressed
// message and that half goes red.
//
// CROSS-SUITE DISCIPLINE: EvaluateRules' pending set is GLOBAL, so this suite
// deletes capture_decisions WHOLESALE at start and end (the precedent in
// rules_integration_test.go / rules_reopen_integration_test.go). Run it against
// a private database (ops_iso45) when other worktrees share the compose db.

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
	crvSlug       = "itest-caprev"
	crvGatedSlug  = "itest-caprev-gated"
	crvJiraAcct   = "itest-caprev-jira@example.test"
	crvMailAcct   = "itest-caprev-mail@example.test"
	crvSlackAcct  = "titestcaprev@slack-web.local"
	crvJiraPrefix = "jira:caprev.jira.com:"
	crvGatedPfx   = "jira:caprevgated.jira.com:"
	crvMailPfx    = "gmail:itest-caprev-mail@example.test:"
	crvSlackPfx   = "slack:TITESTCAPREV:"
	crvMailFrom   = "jira@caprev.jira.com"
	crvGatedFrom  = "jira@caprevgated.jira.com"
	crvActor      = "capture:itest-caprev"
	crvHuman      = "opsctl:itest-caprev"
	crvDismisser  = "dashboard:itest-caprev"
	crvOwn        = "acc-itest-caprev-own"
	crvTSActor    = "ticketstatus:jira"
)

type crvSuite struct {
	pool                    *pgxpool.Pool
	ex                      *executor.Executor
	project, gated          int64
	jiraAcct, mail, slackAc int64
	seq                     int
}

func crvRequire0030(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE (table_name='capture_rules' AND column_name IN ('revive','addressed'))
		     OR (table_name='tasks' AND column_name IN ('closed_at','surfaced_at','surfaced_by_message_id'))
		     OR (table_name='ticket_status_syncs' AND column_name='surfaced_seen_at')`).Scan(&n); err != nil {
		t.Fatalf("probe 0030's columns: %v", err)
	}
	if n != 6 {
		t.Fatalf("found %d of the 6 capture_rules/tasks/ticket_status_syncs columns migration 0030 adds; apply "+
			"migrations/0030_jira_activity_revive.sql to the compose db (`make migrate LOCAL_DB_URL=...`)", n)
	}
}

func newCRVSuite(t *testing.T, ctx context.Context) *crvSuite {
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
	crvRequire0030(t, ctx, pool)
	crvCleanup(t, ctx, pool)
	t.Cleanup(func() { crvCleanup(t, ctx, pool) })

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	s := &crvSuite{pool: pool, ex: executor.New(reg, checker, audit.NewPGStore(pool))}

	proj := func(slug string, gate bool) int64 {
		return s.id(t, ctx,
			`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality, ticket_assignee_gate)
			 VALUES ($1,$1,'itest-caprev-client','manual','dashboard','/tmp/itest-caprev','any',$2) RETURNING id`, slug, gate)
	}
	// ticket_assignee_gate named EXPLICITLY on both: 0023 defaults it false, and a
	// fixture that leaned on the default would exercise the gate half not at all.
	s.project = proj(crvSlug, false)
	s.gated = proj(crvGatedSlug, true)

	acct := func(provider, email, cursor string) int64 {
		return s.id(t, ctx,
			`INSERT INTO source_accounts (provider, account_email, scopes, send_enabled, calendar_in_availability, sync_cursor)
			 VALUES ($1,$2,'{}',false,false,$3::jsonb) RETURNING id`, provider, email, cursor)
	}
	s.jiraAcct = acct("jira", crvJiraAcct, `{"own_account_id":"`+crvOwn+`"}`)
	s.mail = acct("google", crvMailAcct, `{}`)
	s.slackAc = acct("slack_web", crvSlackAcct, `{}`)

	// The connector-side rule (rules 3-5's shape): NON-reviving, per J3.
	s.rule(t, ctx, s.project, "thread_key_prefix", crvJiraPrefix, `[A-Z]+-[0-9]+$`, 95, false, false)
	s.rule(t, ctx, s.gated, "thread_key_prefix", crvGatedPfx, `[A-Z]+-[0-9]+$`, 95, false, false)
	return s
}

func crvCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE account_email IN ('` + crvJiraAcct + `','` + crvMailAcct + `','` + crvSlackAcct + `'))`
	const projs = `(SELECT id FROM projects WHERE slug IN ('` + crvSlug + `','` + crvGatedSlug + `'))`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	const actors = `('` + crvActor + `','` + crvHuman + `','` + crvDismisser + `')`
	for _, q := range []string{
		`DROP TRIGGER IF EXISTS itest_caprev_block_reopen ON tasks`,
		`DROP FUNCTION IF EXISTS itest_caprev_block_reopen()`,
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
		`DELETE FROM projects WHERE slug IN ('` + crvSlug + `','` + crvGatedSlug + `')`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE '` + crvJiraPrefix + `%' OR thread_key LIKE '` + crvGatedPfx +
			`%' OR thread_key LIKE '` + crvMailPfx + `%' OR thread_key LIKE '` + crvSlackPfx + `%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE account_email IN ('` + crvJiraAcct + `','` + crvMailAcct + `','` + crvSlackAcct + `')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func (s *crvSuite) id(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", q, err)
	}
	return id
}

func (s *crvSuite) n(t *testing.T, ctx context.Context, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

func (s *crvSuite) exec(t *testing.T, ctx context.Context, q string, args ...any) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func (s *crvSuite) dbNow(t *testing.T, ctx context.Context) time.Time {
	t.Helper()
	var now time.Time
	if err := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatalf("read db clock: %v", err)
	}
	return now
}

// rule seeds one capture_rules row THROUGH THE COLUMNS criterion 18 is about.
func (s *crvSuite) rule(t *testing.T, ctx context.Context, project int64, kind, pattern, keyRegex string,
	priority int, revive, addressed bool) int64 {
	t.Helper()
	return s.id(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, url_template,
		                            priority, enabled, note, revive, addressed)
		 VALUES ($1,$2,$3,'jira',$4,'https://caprev.jira.com/browse/{key}',$5,true,'itest-caprev',$6,$7) RETURNING id`,
		project, kind, pattern, keyRegex, priority, revive, addressed)
}

// mailRule is J2's shape: the notification sender, a SUBJECT-anchored key.
func (s *crvSuite) mailRule(t *testing.T, ctx context.Context, revive bool) {
	s.rule(t, ctx, s.project, "sender", crvMailFrom, `^[^\n]*?\b(CRV-[0-9]+)\b`, 92, revive, false)
}

// mentionRule is the rule-10 successor the owner's answer arms: any message
// naming a key, a WHOLE-KEY key_regex (J1's precondition for --revive).
func (s *crvSuite) mentionRule(t *testing.T, ctx context.Context, revive bool) {
	s.rule(t, ctx, s.project, "body_regex", `\bCRV-[0-9]+\b`, `\b(CRV-[0-9]+)\b`, 90, revive, false)
}

func (s *crvSuite) thread(t *testing.T, ctx context.Context, key string) int64 {
	t.Helper()
	var id int64
	err := s.pool.QueryRow(ctx, `SELECT id FROM normalized_threads WHERE thread_key=$1`, key).Scan(&id)
	if err == nil {
		return id
	}
	return s.id(t, ctx, `INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,$1,'[]') RETURNING id`, key)
}

func (s *crvSuite) msg(t *testing.T, ctx context.Context, acct int64, threadKey, channel, direction, sender, subject, body string,
	created, sent time.Time) int64 {
	t.Helper()
	s.seq++
	tag := fmt.Sprintf("%d-%d", time.Now().UnixNano(), s.seq)
	raw := s.id(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,'{}',$3, now()) RETURNING id`, acct, "itest-caprev-"+tag, "itest-caprev-h-"+tag)
	return s.id(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		raw, s.thread(t, ctx, threadKey), direction, "<itest-caprev-"+tag+">", sent, body, subject, sender, channel, created)
}

// jiraMsg is the connector copy of a comment: thread jira:{site}:{KEY}.
func (s *crvSuite) jiraMsg(t *testing.T, ctx context.Context, key string, created, sent time.Time) int64 {
	pfx := crvJiraPrefix
	if strings.HasPrefix(key, "CRG-") {
		pfx = crvGatedPfx
	}
	return s.msg(t, ctx, s.jiraAcct, pfx+key, "jira", "inbound", "Katie Evans", "", "a comment on "+key, created, sent)
}

// mailMsg is the Jira notification EMAIL copy: key in the subject only (F-facts).
func (s *crvSuite) mailMsg(t *testing.T, ctx context.Context, from, direction, subject, body string, created, sent time.Time) int64 {
	s.seq++
	return s.msg(t, ctx, s.mail, fmt.Sprintf("%s<n%d@caprev>", crvMailPfx, s.seq), "gmail", direction,
		"Katie Evans (JIRA) <"+from+">", subject, body, created, sent)
}

func (s *crvSuite) slackMsg(t *testing.T, ctx context.Context, body string, created, sent time.Time) int64 {
	return s.msg(t, ctx, s.slackAc, crvSlackPfx+"C1", "slack", "inbound", "Katie Evans", "", body, created, sent)
}

func (s *crvSuite) pass(t *testing.T, ctx context.Context, mode string) capture.RulesStats {
	t.Helper()
	stats, err := capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{Mode: mode, Actor: crvActor})
	if err != nil {
		t.Fatalf("EvaluateRules(%s): %v", mode, err)
	}
	return stats
}

func (s *crvSuite) reconcile(t *testing.T, ctx context.Context) ticketstatus.Stats {
	t.Helper()
	st, err := ticketstatus.Run(ctx, s.pool, s.ex, ticketstatus.Config{})
	if err != nil {
		t.Fatalf("ticketstatus.Run: %v", err)
	}
	return st
}

// snapshot stores the ticket's issue row under the polled jira account — what
// the reconciler decides from (D19).
func (s *crvSuite) snapshot(t *testing.T, ctx context.Context, key, category, name string) {
	t.Helper()
	doc := `{"id":"1","key":"` + key + `","fields":{"summary":"itest ` + key + `","status":{"name":"` + name +
		`","statusCategory":{"key":"` + category + `"}},"assignee":{"accountId":"` + crvOwn + `"}}}`
	s.exec(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, ingested_at)
		 VALUES ($1,$2,$3::jsonb,$4, now())
		 ON CONFLICT (source_account_id, external_id) DO UPDATE
		   SET raw_json = EXCLUDED.raw_json, content_hash = EXCLUDED.content_hash, ingested_at = EXCLUDED.ingested_at`,
		s.jiraAcct, jira.IssueRawID(key), doc, "itest-caprev-"+key+"-"+category+"-"+name)
}

func (s *crvSuite) taskOf(t *testing.T, ctx context.Context, key string) (int64, bool) {
	t.Helper()
	var id int64
	err := s.pool.QueryRow(ctx,
		`SELECT r.task_id FROM external_refs r JOIN tasks t ON t.id = r.task_id
		  WHERE r.system='jira' AND r.external_key=$1 AND t.project_id IN ($2,$3)`, key, s.project, s.gated).Scan(&id)
	if err != nil {
		return 0, false
	}
	return id, true
}

func (s *crvSuite) mustTask(t *testing.T, ctx context.Context, key string) int64 {
	t.Helper()
	id, ok := s.taskOf(t, ctx, key)
	if !ok {
		t.Fatalf("no task linked to %s", key)
	}
	return id
}

func (s *crvSuite) status(t *testing.T, ctx context.Context, task int64) string {
	t.Helper()
	var st string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, task).Scan(&st); err != nil {
		t.Fatalf("read task %d: %v", task, err)
	}
	return st
}

func (s *crvSuite) surfaced(t *testing.T, ctx context.Context, task int64) (*time.Time, *int64) {
	t.Helper()
	var at *time.Time
	var by *int64
	if err := s.pool.QueryRow(ctx, `SELECT surfaced_at, surfaced_by_message_id FROM tasks WHERE id=$1`, task).Scan(&at, &by); err != nil {
		t.Fatalf("read surfacing of task %d: %v", task, err)
	}
	return at, by
}

func (s *crvSuite) events(t *testing.T, ctx context.Context, task int64, typ string) int {
	t.Helper()
	return s.n(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type=$2`, task, typ)
}

func (s *crvSuite) audits(t *testing.T, ctx context.Context, task int64, tool, actor string) int {
	t.Helper()
	return s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool=$2 AND actor=$3`, task, tool, actor)
}

func (s *crvSuite) decisionReason(t *testing.T, ctx context.Context, msg int64, mode string) (string, bool) {
	t.Helper()
	var reason *string
	err := s.pool.QueryRow(ctx, `SELECT reason FROM capture_decisions WHERE message_id=$1 AND mode=$2 ORDER BY id DESC LIMIT 1`,
		msg, mode).Scan(&reason)
	if err != nil {
		return "", false
	}
	if reason == nil {
		return "", true
	}
	return *reason, true
}

func (s *crvSuite) humanClose(t *testing.T, ctx context.Context, task int64) {
	t.Helper()
	if _, err := s.ex.Execute(ctx, executor.Call{Tool: "task_close", Actor: crvHuman, TaskID: &task,
		Args: []byte(fmt.Sprintf(`{"task_id":%d,"reason":"itest-caprev: done"}`, task))}); err != nil {
		t.Fatalf("task_close: %v", err)
	}
}

// closedTask creates KEY's task through a live pass over its connector copy
// (non-reviving rule), then a human closes it. Returns the task and the db
// clock right after the close.
func (s *crvSuite) closedTask(t *testing.T, ctx context.Context, key string) (int64, time.Time) {
	t.Helper()
	start := s.dbNow(t, ctx).Add(-30 * time.Minute)
	s.jiraMsg(t, ctx, key, start, start)
	s.pass(t, ctx, capture.RulesModeLive)
	task := s.mustTask(t, ctx, key)
	s.humanClose(t, ctx, task)
	return task, s.dbNow(t, ctx)
}

// ---- criterion 2: the CHECKs --------------------------------------------------

// MUTATION: drop either CHECK from 0030 -> its violation inserts and this goes red.
func TestCaptureRevive_Integration_TheActivityChecksRefuseIllegalRules(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	ins := func(ext, key any, revive, addressed bool, pattern string) error {
		_, err := s.pool.Exec(ctx,
			`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, priority, revive, addressed)
			 VALUES ($1,'body_regex',$2,$3,$4,1,$5,$6)`, s.project, pattern, ext, key, revive, addressed)
		return err
	}
	for _, tc := range []struct {
		name       string
		ext, key   any
		rev, addr  bool
		constraint string
	}{
		{"revive with no key_regex (the rule-10 shape)", "jira", nil, true, false, "capture_rules_revive_needs_key"},
		{"revive with no external_system", nil, `(X-[0-9]+)`, true, false, "capture_rules_revive_needs_key"},
		{"addressed without revive", "jira", `(X-[0-9]+)`, false, true, "capture_rules_addressed_implies_revive"},
	} {
		err := ins(tc.ext, tc.key, tc.rev, tc.addr, "itest-caprev-check-"+tc.constraint+fmt.Sprint(tc.rev, tc.addr, tc.ext == nil))
		if err == nil || !strings.Contains(err.Error(), tc.constraint) {
			t.Errorf("INSERT %s = %v, want a violation of %s (criterion 2)", tc.name, err, tc.constraint)
		}
	}
	if err := ins("jira", `(X-[0-9]+)`, true, true, "itest-caprev-check-legal"); err != nil {
		t.Errorf("INSERT of a legal reviving, addressed rule failed: %v — the control for the three refusals", err)
	}
}

// ---- criteria 18, 19, 21: Jira mail revives a closed ticket's task ------------

// MUTATIONS that turn this red:
//   - loadRules selects a literal false for r.revive (criterion 18);
//   - taskForExternalRef returns a literal 'ready' status (criterion 19);
//   - task_reopen BEFORE task_append_log (criterion 21's order).
func TestCaptureRevive_Integration_JiraMailRevivesAClosedTicketTask(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	s.mailRule(t, ctx, true)

	task, closedAt := s.closedTask(t, ctx, "CRV-1")
	logs := s.events(t, ctx, task, "log")
	m := s.mailMsg(t, ctx, crvMailFrom, "inbound", "Katie Evans mentioned you on CRV-1",
		"Katie Evans mentioned you on a comment. See also CRV-99.", closedAt.Add(time.Second), closedAt.Add(time.Second))

	stats := s.pass(t, ctx, capture.RulesModeLive)

	if got := s.status(t, ctx, task); got != "ready" {
		t.Fatalf("CRV-1's closed task is %q after a Jira notification email ingested after the close, want ready. "+
			"Decision 1: ANY Jira activity revives the ticket's closed task — today the email is logged silently (F3)", got)
	}
	if at, by := s.surfaced(t, ctx, task); at == nil || by == nil || *by != m {
		t.Errorf("the revived task has surfaced_at=%v surfaced_by=%v, want set / %d (J6 h)", at, by, m)
	}
	if n := s.events(t, ctx, task, "log"); n != logs+1 {
		t.Errorf("log events went %d -> %d, want exactly +1", logs, n)
	}
	var lastLog, lastStatus int64
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE((SELECT max(id) FROM task_events WHERE task_id=$1 AND event_type='log'),0),
		        COALESCE((SELECT max(id) FROM task_events WHERE task_id=$1 AND event_type='status_changed'),0)`,
		task).Scan(&lastLog, &lastStatus); err != nil {
		t.Fatalf("read event order: %v", err)
	}
	if !(lastLog < lastStatus) {
		t.Errorf("event order: last log %d, last status_changed %d — criterion 21: log FIRST, then revive (a crash "+
			"between them leaves today's state)", lastLog, lastStatus)
	}
	if reason, ok := s.decisionReason(t, ctx, m, capture.RulesModeLive); !ok || !strings.Contains(reason, "revive requested") {
		t.Errorf("live decision reason = %q (found=%v), want it to contain \"revive requested\" (criterion 21)", reason, ok)
	}
	if n := s.audits(t, ctx, task, "task_reopen", crvActor); n != 1 {
		t.Errorf("task_reopen audit rows by %s = %d, want 1 (invariant 3)", crvActor, n)
	}
	if stats.Revived != 1 || stats.Appended != 1 {
		t.Errorf("RulesStats revived=%d appended=%d, want 1 / 1 (criterion 21: Revived counts reopened:true answers)",
			stats.Revived, stats.Appended)
	}
	if _, found := s.taskOf(t, ctx, "CRV-99"); found {
		t.Errorf("a task was created for CRV-99, named only in the BODY. J2's key regex reads the SUBJECT line only (F2)")
	}
}

// Criterion 18's other direction. MUTATION: a literal true for r.revive in
// loadRules -> this task comes back and the test goes red.
func TestCaptureRevive_Integration_ANonRevivingRuleOnlyLogs(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	s.mailRule(t, ctx, false)

	task, closedAt := s.closedTask(t, ctx, "CRV-2")
	logs := s.events(t, ctx, task, "log")
	s.mailMsg(t, ctx, crvMailFrom, "inbound", "(CRV-2) Fix the export", "status changed",
		closedAt.Add(time.Second), closedAt.Add(time.Second))
	stats := s.pass(t, ctx, capture.RulesModeLive)

	if got := s.status(t, ctx, task); got != "closed" {
		t.Errorf("a NON-reviving rule reopened a plain-closed task (%q). Every project without a reviving rule "+
			"behaves byte-identically to today", got)
	}
	if n := s.events(t, ctx, task, "log"); n != logs+1 {
		t.Errorf("log events went %d -> %d, want +1 (today's append)", logs, n)
	}
	if n := s.audits(t, ctx, task, "task_reopen", crvActor); n != 0 || stats.Revived != 0 {
		t.Errorf("task_reopen called %d time(s), Revived=%d — want 0 / 0", n, stats.Revived)
	}
	if at, _ := s.surfaced(t, ctx, task); at != nil {
		t.Errorf("a non-reviving rule surfaced the task (%v)", at)
	}
}

// gatedClosedTask is closedTask for a key on the GATED project. Since SWT-40
// Part D merged, the connector copy of a gated jira key is `held` (no task), so
// the fixture task is created with the gate OFF — the column is read every pass
// (IK: "Gate turned off with holds pending") — and the gate is re-armed before
// the test acts.
func (s *crvSuite) gatedClosedTask(t *testing.T, ctx context.Context, key string) (int64, time.Time) {
	t.Helper()
	s.exec(t, ctx, `UPDATE projects SET ticket_assignee_gate = false WHERE id = $1`, s.gated)
	task, at := s.closedTask(t, ctx, key)
	s.exec(t, ctx, `UPDATE projects SET ticket_assignee_gate = true WHERE id = $1`, s.gated)
	return task, at
}

// decisionAction is the latest decision's action for a message in a mode.
func (s *crvSuite) decisionAction(t *testing.T, ctx context.Context, msg int64, mode string) (string, bool) {
	t.Helper()
	var action string
	err := s.pool.QueryRow(ctx, `SELECT action FROM capture_decisions WHERE message_id=$1 AND mode=$2 ORDER BY id DESC LIMIT 1`,
		msg, mode).Scan(&action)
	if err != nil {
		return "", false
	}
	return action, true
}

// Criterion 18's gate half and criterion 20 through real rows. It is ALSO the
// column-fed test for Part D's held clause (SWT-40 merged first, so this branch
// owns the interaction): an addressed match on a gated project is not held; a
// revive-only one still is. MUTATIONS:
//   - a literal false for p.ticket_assignee_gate in loadRules -> the revive-only
//     rule revives CRG-1 and this goes red;
//   - drop `&& !activity` from decideMessage's held condition -> CRG-2's
//     addressed mention is held and CRG-2 stays closed.
func TestCaptureRevive_Integration_TheGateIsColumnFed(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	// A revive-only status-mail rule and J4's addressed rule, on the GATED project.
	s.rule(t, ctx, s.gated, "sender", crvGatedFrom, `^[^\n]*?\b(CRG-[0-9]+)\b`, 92, true, false)
	s.rule(t, ctx, s.gated, "body_regex", `\A[^\n]*(?:\bmentioned you on CRG-[0-9]+|\bassigned CRG-[0-9]+ to you)`,
		`\A[^\n]*?\b(CRG-[0-9]+)\b`, 93, true, true)

	status1, at1 := s.gatedClosedTask(t, ctx, "CRG-1")
	status2, at2 := s.gatedClosedTask(t, ctx, "CRG-2")
	m1 := s.mailMsg(t, ctx, crvGatedFrom, "inbound", "(CRG-1) moved to Done", "", at1.Add(time.Second), at1.Add(time.Second))
	m2 := s.mailMsg(t, ctx, crvGatedFrom, "inbound", "Ana Rossi mentioned you on CRG-2", "", at2.Add(time.Second), at2.Add(time.Second))
	s.pass(t, ctx, capture.RulesModeLive)

	if got := s.status(t, ctx, status1); got != "closed" {
		t.Errorf("a REVIVE-ONLY rule on a gated project reopened CRG-1 (%q). Decision 3 / J1: on a gated project only "+
			"`addressed` overrides — non-addressed activity follows the Part D gate", got)
	}
	if a, ok := s.decisionAction(t, ctx, m1, capture.RulesModeLive); !ok || a != "held" {
		t.Errorf("CRG-1's revive-only status mail decided %q (found=%v), want held — Part D's gate, unchanged (S9)", a, ok)
	}
	if a, ok := s.decisionAction(t, ctx, m2, capture.RulesModeLive); !ok || a != "task_log" {
		t.Errorf("CRG-2's ADDRESSED mention decided %q (found=%v), want task_log — decision 3: an addressed match "+
			"overrides the gate and is never held", a, ok)
	}
	if got := s.status(t, ctx, status2); got != "ready" {
		t.Errorf("an ADDRESSED rule on a gated project left CRG-2 %q, want ready. Decision 3: 'X mentioned you on K' "+
			"overrides the assignee check (S8). (Once Part D merges, this is also the test for `&& !winner.addressed`.)", got)
	}
	if _, by := s.surfaced(t, ctx, status2); by == nil || *by != m2 {
		t.Errorf("CRG-2's revive did not surface with the addressed message (by=%v)", by)
	}
}

// ---- the Part D interaction, end to end (SWT-40 D-D1 x SWT-45 decision 3) -----

// Beyond the column-fed test above, through real rows on the GATED project:
//
//	(a) an ADDRESSED match for a key with NO task creates it at capture time —
//	    no `held` row, no Jira lookup — and surfaces it (S8; Part D's D-D6 path
//	    changes for the mail the addressed rule claims);
//	(b) a revive-only match for another new key is `held` and creates nothing;
//	    the gate stage then resolves it, and a gate-path creation never surfaces;
//	(c) a revive-only match on a CLOSED task is `held`, and the gate stage's
//	    resolution LOGS on the closed task and never revives or surfaces it —
//	    the SPEC's Part D section: "gate-path task_logs do not revive (decision
//	    3: non-addressed activity follows the gate)"; a gate-path revive is
//	    Future work, deliberately not built.
//
// The gate resolves from STORED snapshots (Lookup nil, no Jira): each snapshot
// is written after its message was first seen, so it is fresh enough to decide.
// MUTATION: drop `&& !activity` from decideMessage's held condition -> (a) is
// held and CRG-10 gets no task.
func TestCaptureRevive_Integration_AddressedOverridesTheGateAndTheGateNeverRevives(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	s.rule(t, ctx, s.gated, "sender", crvGatedFrom, `^[^\n]*?\b(CRG-[0-9]+)\b`, 92, true, false)
	s.rule(t, ctx, s.gated, "body_regex", `\A[^\n]*(?:\bmentioned you on CRG-[0-9]+|\bassigned CRG-[0-9]+ to you)`,
		`\A[^\n]*?\b(CRG-[0-9]+)\b`, 93, true, true)

	closed, _ := s.gatedClosedTask(t, ctx, "CRG-12")

	now := s.dbNow(t, ctx)
	addressed := s.mailMsg(t, ctx, crvGatedFrom, "inbound", "Ana Rossi assigned CRG-10 to you", "", now, now)
	fresh := s.mailMsg(t, ctx, crvGatedFrom, "inbound", "(CRG-11) moved to In Progress", "", now, now)
	onClosed := s.mailMsg(t, ctx, crvGatedFrom, "inbound", "(CRG-12) Ana commented", "", now, now)
	stats := s.pass(t, ctx, capture.RulesModeLive)

	// (a)
	if a, ok := s.decisionAction(t, ctx, addressed, capture.RulesModeLive); !ok || a != "task" {
		t.Errorf("the addressed 'assigned CRG-10 to you' mail decided %q (found=%v), want task — not held", a, ok)
	}
	created, ok := s.taskOf(t, ctx, "CRG-10")
	if !ok {
		t.Fatalf("no task for CRG-10: an addressed match on a gated project must create at capture time (S8)")
	}
	if _, by := s.surfaced(t, ctx, created); by == nil || *by != addressed {
		t.Errorf("CRG-10's task was not surfaced by the addressed mail (by=%v)", by)
	}
	if stats.TasksCreated != 1 || stats.SurfacedCreated != 1 || stats.Revived != 0 {
		t.Errorf("live pass stats tasks_created=%d surfaced_created=%d revived=%d, want 1 / 1 / 0",
			stats.TasksCreated, stats.SurfacedCreated, stats.Revived)
	}
	// (b) and (c): both held, nothing created or revived at capture time.
	for _, m := range []struct {
		id   int64
		what string
	}{{fresh, "CRG-11's status mail (new key)"}, {onClosed, "CRG-12's comment mail (closed task)"}} {
		if a, ok := s.decisionAction(t, ctx, m.id, capture.RulesModeLive); !ok || a != "held" {
			t.Errorf("%s decided %q (found=%v), want held — non-addressed activity follows the gate", m.what, a, ok)
		}
	}
	if _, found := s.taskOf(t, ctx, "CRG-11"); found {
		t.Errorf("capture created a task for the held CRG-11")
	}

	// The gate stage resolves both holds: both tickets are open and his.
	s.snapshot(t, ctx, "CRG-11", "indeterminate", "In Progress")
	s.snapshot(t, ctx, "CRG-12", "indeterminate", "In Progress")
	gs, err := capture.RunGate(ctx, s.pool, s.ex, capture.GateConfig{})
	if err != nil {
		t.Fatalf("RunGate: %v", err)
	}
	if gs.TasksCreated != 1 || gs.Appended != 1 {
		t.Fatalf("gate stats tasks_created=%d appended=%d (pending=%d), want 1 / 1 — setup: the stored snapshots did "+
			"not decide the holds", gs.TasksCreated, gs.Appended, gs.PendingLookup)
	}
	// (b)
	gateCreated := s.mustTask(t, ctx, "CRG-11")
	if at, _ := s.surfaced(t, ctx, gateCreated); at != nil {
		t.Errorf("the gate-created CRG-11 task was surfaced (%v); gate-path creations never surface", at)
	}
	// (c)
	if got := s.status(t, ctx, closed); got != "closed" {
		t.Errorf("the gate's task_log REVIVED CRG-12's closed task (%q). Part D's gate stage must never revive on its "+
			"own: non-addressed activity follows the gate, and a gate-path revive is Future work", got)
	}
	if at, _ := s.surfaced(t, ctx, closed); at != nil {
		t.Errorf("the gate surfaced CRG-12's closed task (%v)", at)
	}
	if n := s.audits(t, ctx, closed, "task_reopen", capture.GateActor); n != 0 {
		t.Errorf("the gate called task_reopen %d time(s) on CRG-12's task (no open dismissal), want 0", n)
	}
}

// ---- criterion 22: creation by an overriding rule surfaces --------------------

func TestCaptureRevive_Integration_CreationByAnOverridingRuleSurfaces(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	s.mailRule(t, ctx, true)

	var baseline int64
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(max(id),0) FROM audit_events`).Scan(&baseline); err != nil {
		t.Fatalf("audit baseline: %v", err)
	}
	now := s.dbNow(t, ctx)
	m := s.mailMsg(t, ctx, crvMailFrom, "inbound", "Katie Evans assigned CRV-5 to you", "", now, now)
	stats := s.pass(t, ctx, capture.RulesModeLive)

	task := s.mustTask(t, ctx, "CRV-5")
	rows, err := s.pool.Query(ctx, `SELECT tool FROM audit_events WHERE actor=$1 AND id > $2 AND status='ok' ORDER BY id`,
		crvActor, baseline)
	if err != nil {
		t.Fatalf("read audit order: %v", err)
	}
	var got []string
	for rows.Next() {
		var tool string
		if err := rows.Scan(&tool); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, tool)
	}
	rows.Close()
	want := "create_task,link_external_ref,task_set_source_thread,task_mark_surfaced"
	if strings.Join(got, ",") != want {
		t.Errorf("capture's executor calls for a first overriding email = %v, want [%s] in that order, all as %s "+
			"(criterion 22). Surfacing LAST: a crash before it degrades to today (J7's crash window)", got, want, crvActor)
	}
	if at, by := s.surfaced(t, ctx, task); at == nil || by == nil || *by != m {
		t.Errorf("the created task has surfaced_at=%v by=%v, want set / %d — without it the same-tick reconcile closes "+
			"a done ticket's brand-new task (S5)", at, by, m)
	}
	if stats.SurfacedCreated != 1 || stats.TasksCreated != 1 {
		t.Errorf("RulesStats surfaced_created=%d tasks_created=%d, want 1 / 1", stats.SurfacedCreated, stats.TasksCreated)
	}
}

func TestCaptureRevive_Integration_ANonOverridingCreationDoesNotSurface(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	s.mailRule(t, ctx, false)
	now := s.dbNow(t, ctx)
	s.mailMsg(t, ctx, crvMailFrom, "inbound", "Katie Evans assigned CRV-6 to you", "", now, now)
	stats := s.pass(t, ctx, capture.RulesModeLive)

	task := s.mustTask(t, ctx, "CRV-6")
	if at, _ := s.surfaced(t, ctx, task); at != nil {
		t.Errorf("a non-overriding creation surfaced the task (%v); it must create exactly as today", at)
	}
	if n := s.audits(t, ctx, task, "task_mark_surfaced", crvActor); n != 0 || stats.SurfacedCreated != 0 {
		t.Errorf("task_mark_surfaced called %d time(s), SurfacedCreated=%d, want 0 / 0", n, stats.SurfacedCreated)
	}
}

// ---- criterion 23 / J10: activity on an OPEN task only logs --------------------

func TestCaptureRevive_Integration_ActivityOnAnOpenTaskOnlyLogs(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	s.mailRule(t, ctx, true)

	for i, status := range []string{"ready", "delivered", "in_progress"} {
		key := fmt.Sprintf("CRV-1%d", i)
		start := s.dbNow(t, ctx).Add(-30 * time.Minute)
		s.jiraMsg(t, ctx, key, start, start)
		s.pass(t, ctx, capture.RulesModeLive)
		task := s.mustTask(t, ctx, key)
		s.exec(t, ctx, `UPDATE tasks SET status=$2 WHERE id=$1`, task, status)
		logs := s.events(t, ctx, task, "log")

		now := s.dbNow(t, ctx)
		s.mailMsg(t, ctx, crvMailFrom, "inbound", "("+key+") moved to TT-Closed", "", now, now)
		stats := s.pass(t, ctx, capture.RulesModeLive)

		if got := s.status(t, ctx, task); got != status {
			t.Errorf("activity on a %s task changed it to %q", status, got)
		}
		if at, _ := s.surfaced(t, ctx, task); at != nil {
			t.Errorf("activity on an OPEN (%s) task surfaced it (%v). J10: if activity on an open task surfaced it, "+
				"every Jira close email would land before the jira tick and the reconciler would never close a "+
				"Treetop task for a done ticket again", status, at)
		}
		if n := s.events(t, ctx, task, "log"); n != logs+1 {
			t.Errorf("%s task: log events %d -> %d, want +1", status, logs, n)
		}
		if s.audits(t, ctx, task, "task_reopen", crvActor)+s.audits(t, ctx, task, "task_mark_surfaced", crvActor) != 0 ||
			stats.Revived != 0 {
			t.Errorf("%s task: capture called task_reopen/task_mark_surfaced (Revived=%d); want log only", status, stats.Revived)
		}
	}
}

// ---- criterion 25: shadow says "would revive" and calls nothing ---------------

func TestCaptureRevive_Integration_ShadowWouldReviveAndCallsNothing(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	s.mailRule(t, ctx, true)

	task, closedAt := s.closedTask(t, ctx, "CRV-3")
	var baseline int64
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(max(id),0) FROM audit_events`).Scan(&baseline); err != nil {
		t.Fatalf("audit baseline: %v", err)
	}
	m := s.mailMsg(t, ctx, crvMailFrom, "inbound", "Katie Evans mentioned you on CRV-3", "",
		closedAt.Add(time.Second), closedAt.Add(time.Second))
	stats := s.pass(t, ctx, capture.RulesModeShadow)

	if reason, ok := s.decisionReason(t, ctx, m, capture.RulesModeShadow); !ok || !strings.Contains(reason, "would revive") {
		t.Errorf("shadow decision reason = %q (found=%v), want it to contain \"would revive\" (criterion 25)", reason, ok)
	}
	if n := s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE actor=$1 AND id > $2`, crvActor, baseline); n != 0 {
		t.Errorf("a SHADOW pass made %d executor call(s); shadow calls nothing", n)
	}
	if got := s.status(t, ctx, task); got != "closed" {
		t.Errorf("a SHADOW pass revived the task (%q)", got)
	}
	if stats.Revived != 0 || stats.SurfacedCreated != 0 {
		t.Errorf("shadow counters revived=%d surfaced_created=%d, want 0 / 0, always", stats.Revived, stats.SurfacedCreated)
	}
}

// ---- criterion 26 / S4 / J3: two copies of one event revive ONCE --------------

func TestCaptureRevive_Integration_ConnectorAndEmailCopiesReviveOnce(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	s.mailRule(t, ctx, true)

	for _, tc := range []struct {
		name, key  string
		emailFirst bool
	}{
		{"connector copy first", "CRV-7", false},
		{"email copy first", "CRV-8", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task, closedAt := s.closedTask(t, ctx, tc.key)
			logs := s.events(t, ctx, task, "log")
			changes := s.events(t, ctx, task, "status_changed")
			ingested := closedAt.Add(time.Second)
			early, late := closedAt.Add(2*time.Second), closedAt.Add(3*time.Second)
			jiraSent, mailSent := early, late
			if tc.emailFirst {
				jiraSent, mailSent = late, early
			}
			s.jiraMsg(t, ctx, tc.key, ingested, jiraSent)
			s.mailMsg(t, ctx, crvMailFrom, "inbound", "Katie Evans mentioned you on "+tc.key, "", ingested, mailSent)
			stats := s.pass(t, ctx, capture.RulesModeLive)

			if n := s.events(t, ctx, task, "status_changed"); n != changes+1 {
				t.Errorf("status_changed events %d -> %d, want exactly ONE closed -> open (J12: one event, one revive)", changes, n)
			}
			if n := s.events(t, ctx, task, "log"); n != logs+2 {
				t.Errorf("log events %d -> %d, want +2 (both copies log)", logs, n)
			}
			if n := s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool='task_reopen' AND status='ok'`, task); n != 1 {
				t.Errorf("task_reopen audit rows = %d, want exactly 1 — only the EMAIL rule revives (J3), and even if both "+
					"could, the second finds the task open", n)
			}
			if got := s.status(t, ctx, task); got != "ready" || stats.Revived != 1 {
				t.Errorf("task %s, Revived=%d; want ready / 1", got, stats.Revived)
			}
		})
	}
}

// ---- criterion 27: an outbound Jira-mail-shaped message never revives ---------

func TestCaptureRevive_Integration_AnOutboundJiraMailNeverRevives(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	s.mailRule(t, ctx, true)

	task, closedAt := s.closedTask(t, ctx, "CRV-50")
	m := s.mailMsg(t, ctx, crvMailFrom, "outbound", "Salvador Spataro mentioned you on CRV-50", "",
		closedAt.Add(time.Second), closedAt.Add(time.Second))
	s.pass(t, ctx, capture.RulesModeLive)

	if n := s.n(t, ctx, `SELECT count(*) FROM capture_decisions WHERE message_id=$1`, m); n != 0 {
		t.Errorf("the outbound message has %d capture decision(s), want 0 (invariant 5: the pending filter)", n)
	}
	if got := s.status(t, ctx, task); got != "closed" {
		t.Errorf("an OUTBOUND Jira-mail-shaped message revived the task (%q)", got)
	}
}

// ---- criterion 21: a failed revive keeps the log line -------------------------

// The failure is injected with a trigger scoped to ONE task id (dropped in
// cleanup): the revive's UPDATE raises, its transaction rolls back, and the
// log line — a separate, earlier executor call — must still be there.
func TestCaptureRevive_Integration_ALogIsKeptWhenTheReviveFails(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	s.mailRule(t, ctx, true)

	task, closedAt := s.closedTask(t, ctx, "CRV-60")
	s.exec(t, ctx, `CREATE OR REPLACE FUNCTION itest_caprev_block_reopen() RETURNS trigger LANGUAGE plpgsql AS $$
	                BEGIN RAISE EXCEPTION 'itest-caprev: reopen blocked'; END $$`)
	s.exec(t, ctx, fmt.Sprintf(`CREATE TRIGGER itest_caprev_block_reopen BEFORE UPDATE ON tasks FOR EACH ROW
	                WHEN (OLD.id = %d AND OLD.status = 'closed' AND NEW.status <> 'closed')
	                EXECUTE FUNCTION itest_caprev_block_reopen()`, task))
	logs := s.events(t, ctx, task, "log")

	m := s.mailMsg(t, ctx, crvMailFrom, "inbound", "Katie Evans mentioned you on CRV-60", "",
		closedAt.Add(time.Second), closedAt.Add(time.Second))
	_, _ = capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{Mode: capture.RulesModeLive, Actor: crvActor})
	if n := s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool='task_reopen' AND actor=$2`, task, crvActor); n == 0 {
		t.Errorf("capture never ATTEMPTED the revive (no task_reopen audit row for the task) — the failure this test " +
			"injects was never reached, so 'the log survives a failed revive' is unproven")
	}

	if n := s.events(t, ctx, task, "log"); n != logs+1 {
		t.Errorf("after a failed revive the log events went %d -> %d, want +1: the log is appended FIRST and "+
			"survives (criterion 21)", logs, n)
	}
	if got := s.status(t, ctx, task); got != "closed" {
		t.Errorf("the task is %q, want closed — the blocked revive must not half-apply", got)
	}
	if _, ok := s.decisionReason(t, ctx, m, capture.RulesModeLive); !ok {
		t.Errorf("no live decision recorded for the message whose revive failed")
	}
}

// ---- criterion 37: S2 and S3 with the reconciler — no ping-pong ----------------

func TestCaptureRevive_Integration_TheCloseEmailNeverPingPongs(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	s.mailRule(t, ctx, true)

	quiet := func(t *testing.T, task int64) {
		t.Helper()
		before := s.audits(t, ctx, task, "task_close", crvTSActor) + s.audits(t, ctx, task, "task_reopen", crvTSActor) +
			s.audits(t, ctx, task, "task_append_log", crvTSActor)
		for i := 0; i < 5; i++ {
			s.reconcile(t, ctx)
		}
		after := s.audits(t, ctx, task, "task_close", crvTSActor) + s.audits(t, ctx, task, "task_reopen", crvTSActor) +
			s.audits(t, ctx, task, "task_append_log", crvTSActor)
		if after != before {
			t.Errorf("5 further Runs made %d executor call(s) on the task, want 0 (J12: a fixed point after at most "+
				"one pass)", after-before)
		}
	}

	t.Run("S2: the close email is ingested BEFORE the jira tick", func(t *testing.T) {
		s.snapshot(t, ctx, "CRV-20", "indeterminate", "In Progress")
		start := s.dbNow(t, ctx).Add(-30 * time.Minute)
		s.jiraMsg(t, ctx, "CRV-20", start, start)
		s.pass(t, ctx, capture.RulesModeLive)
		task := s.mustTask(t, ctx, "CRV-20")

		s.snapshot(t, ctx, "CRV-20", "done", "Closed-ish")
		now := s.dbNow(t, ctx)
		s.mailMsg(t, ctx, crvMailFrom, "inbound", "(CRV-20) Fix the export", "status changed to done", now, now)
		if st := s.pass(t, ctx, capture.RulesModeLive); st.Revived != 0 {
			t.Errorf("the close email on an OPEN task revived something (Revived=%d)", st.Revived)
		}
		s.reconcile(t, ctx)
		if got := s.status(t, ctx, task); got != "closed" {
			t.Errorf("S2: task is %q after the reconciler saw the done ticket, want closed (it stays closed)", got)
		}
		quiet(t, task)
		if got := s.status(t, ctx, task); got != "closed" {
			t.Errorf("S2: task came back (%q)", got)
		}
	})

	t.Run("S3: the close email is ingested AFTER the reconciler's close", func(t *testing.T) {
		s.snapshot(t, ctx, "CRV-21", "indeterminate", "In Progress")
		start := s.dbNow(t, ctx).Add(-30 * time.Minute)
		s.jiraMsg(t, ctx, "CRV-21", start, start)
		s.pass(t, ctx, capture.RulesModeLive)
		task := s.mustTask(t, ctx, "CRV-21")

		s.snapshot(t, ctx, "CRV-21", "done", "Closed-ish")
		s.reconcile(t, ctx)
		if got := s.status(t, ctx, task); got != "closed" {
			t.Fatalf("setup: the reconciler did not close CRV-21 (%q)", got)
		}
		now := s.dbNow(t, ctx)
		s.mailMsg(t, ctx, crvMailFrom, "inbound", "(CRV-21) Fix the export", "status changed to done", now, now)
		if st := s.pass(t, ctx, capture.RulesModeLive); st.Revived != 1 {
			t.Errorf("S3: the close email ingested after the close did not revive (Revived=%d) — decision 1", st.Revived)
		}
		logsBefore := s.audits(t, ctx, task, "task_append_log", crvTSActor)
		s.reconcile(t, ctx)
		if got := s.status(t, ctx, task); got != "ready" {
			t.Errorf("S3: the reconciler re-closed the revived task (%q) — the ping-pong decision 1 forbids", got)
		}
		if n := s.audits(t, ctx, task, "task_append_log", crvTSActor); n != logsBefore+1 {
			t.Errorf("S3: the hold wrote %d reconciler log line(s), want exactly 1", n-logsBefore)
		}
		quiet(t, task)
		if got := s.status(t, ctx, task); got != "ready" {
			t.Errorf("S3: the held task closed during the quiet passes (%q)", got)
		}
	})
}

// ---- criterion 38 / S5: an overriding creation survives the same-tick Run -----

func TestCaptureRevive_Integration_CreationOnADoneTicketSurvivesTheSameTick(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	s.mailRule(t, ctx, true)

	s.snapshot(t, ctx, "CRV-30", "done", "Closed-ish")
	now := s.dbNow(t, ctx)
	s.mailMsg(t, ctx, crvMailFrom, "inbound", "Katie Evans mentioned you on CRV-30", "", now, now)
	s.pass(t, ctx, capture.RulesModeLive)
	task := s.mustTask(t, ctx, "CRV-30")

	st := s.reconcile(t, ctx)
	if got := s.status(t, ctx, task); got != "ready" {
		t.Errorf("S5: the task an overriding email created for a DONE ticket is %q after the same-tick Run, want ready. "+
			"J11 reverses SWT-32 D8's same-tick close for overriding rules", got)
	}
	if st.Resurfaced < 1 {
		t.Errorf("ticketstatus Stats.Resurfaced = %d, want >= 1", st.Resurfaced)
	}
}

// ---- the owner's answer (2026-09-12): a Slack mention IS activity ------------

func TestCaptureRevive_Integration_ASlackMentionRevivesOrCreates(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	s.mentionRule(t, ctx, true)

	task, closedAt := s.closedTask(t, ctx, "CRV-40")
	m := s.slackMsg(t, ctx, "can you look at CRV-40?", closedAt.Add(time.Second), closedAt.Add(time.Second))
	now := s.dbNow(t, ctx)
	fresh := s.slackMsg(t, ctx, "also CRV-41 needs a look", now, now)
	stats := s.pass(t, ctx, capture.RulesModeLive)

	if got := s.status(t, ctx, task); got != "ready" {
		t.Errorf("a Slack message naming CRV-40 left its closed task %q, want ready. Owner, 2026-09-12: 'a Slack or "+
			"GitHub message that names a ticket key counts as Jira activity: it revives the ticket's closed task'", got)
	}
	if _, by := s.surfaced(t, ctx, task); by == nil || *by != m {
		t.Errorf("the Slack revive did not surface with the Slack message (by=%v)", by)
	}
	created, ok := s.taskOf(t, ctx, "CRV-41")
	if !ok {
		t.Fatalf("a Slack message naming CRV-41 (no task) created none — '...or creates one'")
	}
	if _, by := s.surfaced(t, ctx, created); by == nil || *by != fresh {
		t.Errorf("the Slack-created task was not surfaced by its message (by=%v)", by)
	}
	if stats.Revived != 1 || stats.SurfacedCreated != 1 {
		t.Errorf("RulesStats revived=%d surfaced_created=%d, want 1 / 1", stats.Revived, stats.SurfacedCreated)
	}
}

// "Only new messages act (no historical backfill)." Two ways a message can be
// old: ingested before the close (the handler's guard), or already decided
// before the reviving rule existed (capture_decisions_live_uniq: the live claim
// is spent, and --all is refused in live).
func TestCaptureRevive_Integration_OnlyNewMessagesAct(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)

	t.Run("a mention ingested before the close does not revive", func(t *testing.T) {
		s.mentionRule(t, ctx, true)
		task, closedAt := s.closedTask(t, ctx, "CRV-42")
		logs := s.events(t, ctx, task, "log")
		s.slackMsg(t, ctx, "earlier: CRV-42 is fine now", closedAt.Add(-10*time.Minute), closedAt.Add(-10*time.Minute))
		stats := s.pass(t, ctx, capture.RulesModeLive)
		if got := s.status(t, ctx, task); got != "closed" || stats.Revived != 0 {
			t.Errorf("a mention ingested BEFORE the close left the task %q (Revived=%d), want closed / 0 — "+
				"message_predates_close", got, stats.Revived)
		}
		if n := s.events(t, ctx, task, "log"); n != logs+1 {
			t.Errorf("log events %d -> %d, want +1 (it is still logged)", logs, n)
		}
		if n := s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool='task_reopen' AND actor=$2 AND status='ok'`,
			task, crvActor); n != 1 {
			t.Errorf("task_reopen audit rows = %d, want exactly 1: capture must ASK (the rule overrides, the task is "+
				"closed) and the HANDLER must answer message_predates_close — the guard lives under the row lock, never "+
				"in capture", n)
		}
	})
}

func TestCaptureRevive_Integration_ARuleAddedLaterDoesNotBackfill(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)

	task, closedAt := s.closedTask(t, ctx, "CRV-43")
	m := s.slackMsg(t, ctx, "CRV-43 again?", closedAt.Add(time.Second), closedAt.Add(time.Second))
	s.pass(t, ctx, capture.RulesModeLive) // no mention rule yet: the live claim is spent on `unmatched`
	s.mentionRule(t, ctx, true)
	s.pass(t, ctx, capture.RulesModeLive)

	if n := s.n(t, ctx, `SELECT count(*) FROM capture_decisions WHERE message_id=$1 AND mode='live'`, m); n != 1 {
		t.Errorf("the message has %d live decisions, want 1 — one live action per message, forever", n)
	}
	if got := s.status(t, ctx, task); got != "closed" {
		t.Errorf("adding a reviving rule later revived a task from an already-decided message (%q). Owner: only NEW "+
			"messages act; the ~654 historically mentioned keys must not become tasks retroactively", got)
	}
}

// ---- criterion 11 through capture: the pre-0030 close (closed_at NULL) -------

// A task closed before 0030 has closed_at NULL, and the revive guard falls back
// to updated_at. Through a real capture pass: a message ingested BEFORE that
// instant is logged and not revived; one ingested after it revives. The first
// pass's task_append_log must not move updated_at — capture logs FIRST and
// revives second, so a log that stamped updated_at would make every pre-0030
// task unrevivable.
//
// MUTATION: COALESCE(t.closed_at, t.updated_at) -> t.closed_at in reviveGuarded
// -> the guard is NULL, the handler errors, and the second pass goes red.
func TestCaptureRevive_Integration_APre0030CloseFallsBackToUpdatedAt(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	s.mailRule(t, ctx, true)

	task, _ := s.closedTask(t, ctx, "CRV-80")
	closedAt := s.dbNow(t, ctx).Add(-10 * time.Minute)
	// Fixture: what a pre-0030 close looks like — status closed, no close record,
	// updated_at = the close instant (the old closeTransition stamped only that).
	s.exec(t, ctx, `UPDATE tasks SET closed_at = NULL, closed_from_status = NULL, updated_at = $2 WHERE id = $1`,
		task, closedAt)

	before := s.mailMsg(t, ctx, crvMailFrom, "inbound", "(CRV-80) Old news", "",
		closedAt.Add(-time.Minute), closedAt.Add(-time.Minute))
	first := s.pass(t, ctx, capture.RulesModeLive)
	if got := s.status(t, ctx, task); got != "closed" || first.Revived != 0 {
		t.Fatalf("a message ingested BEFORE the pre-0030 close's updated_at revived the task (%q, revived=%d); "+
			"want closed / 0 (message_predates_close)", got, first.Revived)
	}
	if n := s.audits(t, ctx, task, "task_reopen", crvActor); n != 1 {
		t.Errorf("task_reopen audit rows = %d, want 1: capture asked, and the HANDLER refused on the fallback", n)
	}
	var updated time.Time
	if err := s.pool.QueryRow(ctx, `SELECT updated_at FROM tasks WHERE id = $1`, task).Scan(&updated); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}
	if !updated.Equal(closedAt) {
		t.Fatalf("updated_at moved %s -> %s across a log-only pass: the fallback instant is no longer the close "+
			"(the log for message %d stamped it)", closedAt, updated, before)
	}

	after := s.mailMsg(t, ctx, crvMailFrom, "inbound", "Katie Evans mentioned you on CRV-80", "",
		closedAt.Add(time.Minute), closedAt.Add(time.Minute))
	second := s.pass(t, ctx, capture.RulesModeLive)
	if got := s.status(t, ctx, task); got != "ready" || second.Revived != 1 {
		t.Errorf("a message ingested AFTER the pre-0030 close's updated_at left the task %q (revived=%d); want ready / 1 "+
			"— closed_at NULL must fall back to updated_at, not refuse", got, second.Revived)
	}
	if at, by := s.surfaced(t, ctx, task); at == nil || by == nil || *by != after {
		t.Errorf("surfaced_at=%v by=%v, want set / %d", at, by, after)
	}
}

// ---- J18 through capture: a non-jira reviving rule is inert -------------------

// Criterion 45's capture half. capture_rule_add refuses `revive` without
// external_system='jira', but 0030's CHECK asks only for SOME system, so a
// github reviving+addressed rule can still be stored by direct SQL. On a GATED
// project it must neither revive nor surface: Part D's hold keys on
// system == "jira", so without decideMessage's `system == "jira" &&` such a rule
// would revive and surface past the assignee check.
//
// MUTATION: drop `system == "jira" &&` from decideMessage's `activity` -> red
// (CRG-41's closed task revives; CRG-42's new task is surfaced).
func TestCaptureRevive_Integration_ANonJiraRevivingRuleIsInert(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	rule := s.id(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, url_template,
		                            priority, enabled, note, revive, addressed)
		 VALUES ($1,'sender',$2,'github',$3,'https://github.test/{key}',92,true,'itest-caprev',false,false) RETURNING id`,
		s.gated, crvGatedFrom, `^[^\n]*?\b(CRG-[0-9]+)\b`)
	githubTask := func(key string) (int64, bool) {
		var id int64
		err := s.pool.QueryRow(ctx, `SELECT r.task_id FROM external_refs r JOIN tasks t ON t.id = r.task_id
		                              WHERE r.system='github' AND r.external_key=$1 AND t.project_id=$2`, key, s.gated).Scan(&id)
		return id, err == nil
	}

	// A github-linked task for CRG-41, created while the rule is plain, then closed.
	start := s.dbNow(t, ctx).Add(-30 * time.Minute)
	s.mailMsg(t, ctx, crvGatedFrom, "inbound", "Katie Evans mentioned you on CRG-41", "", start, start)
	s.pass(t, ctx, capture.RulesModeLive)
	task, ok := githubTask("CRG-41")
	if !ok {
		t.Fatalf("fixture: no github-linked task for CRG-41 after the plain rule's pass")
	}
	s.humanClose(t, ctx, task)
	closedAt := s.dbNow(t, ctx)

	// Stored around capture_rule_add: revive + addressed on a github rule.
	s.exec(t, ctx, `UPDATE capture_rules SET revive = true, addressed = true WHERE id = $1`, rule)
	m := s.mailMsg(t, ctx, crvGatedFrom, "inbound", "Katie Evans mentioned you on CRG-41", "", closedAt.Add(time.Second), closedAt)
	s.mailMsg(t, ctx, crvGatedFrom, "inbound", "Katie Evans mentioned you on CRG-42", "", closedAt.Add(time.Second), closedAt)

	stats := s.pass(t, ctx, capture.RulesModeLive)

	if got := s.status(t, ctx, task); got != "closed" {
		t.Errorf("CRG-41's github-linked task is %q, want closed: a reviving rule on a non-jira system is inert (J18)", got)
	}
	if reason, _ := s.decisionReason(t, ctx, m, capture.RulesModeLive); strings.Contains(reason, "revive") {
		t.Errorf("CRG-41's reason = %q, want no revive wording", reason)
	}
	created, found := githubTask("CRG-42")
	if !found {
		t.Fatalf("no github-linked task for CRG-42: the rule still creates, it just is not activity")
	}
	if at, _ := s.surfaced(t, ctx, created); at != nil {
		t.Errorf("CRG-42's task was surfaced (%v) by a non-jira rule on a gated project (J18)", at)
	}
	if stats.Revived != 0 || stats.SurfacedCreated != 0 {
		t.Errorf("RulesStats revived=%d surfaced_created=%d, want 0 / 0", stats.Revived, stats.SurfacedCreated)
	}
}
