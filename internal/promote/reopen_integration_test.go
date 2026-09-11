//go:build integration

package promote_test

// SWT-36 (docs/tickets/dismiss-reopen-on-activity_SPEC.md) against a real
// database, the PROMOTE path: criteria 10, 11, 12, 18 (promote's inbox), 23
// (the open predicate in threadTask) and 26, plus D7 and D11.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run PromoteReopen ./internal/promote/
//
// Build-tagged `integration` AND env-gated on DATABASE_URL, with a FATAL guard
// on 192.168.50.49. NO LLM: verdicts are stored rows written by hand, exactly
// as `classify run` writes them.
//
// "TEST THE COLUMN, NOT THE FIXTURE": the dismissed task is found by
// threadTask from tasks.source_thread_id + task_dismissals.reopened_at, and the
// reopen decision is the handler's, from normalized_messages.created_at vs
// task_dismissals.created_at. Every fixture is production-shaped (criterion 26):
// the task is CREATED by a first promote pass (so source_thread_id is set by
// task_set_source_thread, not by the harness), the thread carries both an
// inbound and an outbound message, and the dismissal goes through task_dismiss
// on the executor. Mutations are named inline.
//
// ---- IMPOSED SURFACE, beyond promote_test.go's --------------------------------
//
//	type Stats struct { ...; Reopened int } // counted from task_reopen's reopened:true (criterion 11)
//
//	Run, on a Decision with ReopenDismissalID != 0:
//	  claim (classify_promotions action='attached', task_id = the dismissed task,
//	         reason "thread's task N was dismissed (reason_code); attached, reopen
//	         requested against dismissal D")
//	  -> task_append_log (existing text) -> task_reopen {task_id, dismissal_id,
//	     message_id, reason} as promote:classify -> record task_id.
//	  Dry-run prints the request and writes nothing.
//
// GREENFIELD NOTE — EXPECTED RED: Stats.Reopened does not exist, so this file
// compile-FAILs the integration build; after it exists, 0026's columns are
// missing (prrRequire0026), and after that, threadTask still falls through Q3
// and a duplicate task is created — each assertion below catches it on its own.
//
// CLEANUP PACT: owns projects itest-promreopen-%, source_accounts provider
// itest-promreopen-src, threads itest-promreopen:%, ai_runs model
// itest-promreopen-model, and audit rows by task plus actors promote:% (the
// SWT-30 suite's sweep, same reason) and this file's two human actors.
// Promotion is gated on classify_promote_after, which no other suite sets, so
// the global inbox acts only on this suite's rows (criterion 5 doing double duty).

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/promote"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	prrProvider = "itest-promreopen-src"
	prrAccount  = "itest-promreopen@pg-main"
	prrModel    = "itest-promreopen-model"
	prrProject  = "itest-promreopen-personal"
	prrHuman    = "dashboard:itest-promreopen"
	prrCloser   = "opsctl:itest-promreopen"
	prrSender   = "Alerts <alerts@bank.example>"
)

type prrSuite struct {
	pool    *pgxpool.Pool
	ex      *executor.Executor
	project int64
	account int64
}

func newPRRSuite(t *testing.T, ctx context.Context) *prrSuite {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use the compose db on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	prrCleanup(t, ctx, pool)
	t.Cleanup(func() { prrCleanup(t, ctx, pool) })
	prrRequire0026(t, ctx, pool)

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	s := &prrSuite{pool: pool, ex: executor.New(reg, checker, audit.NewPGStore(pool))}

	// The `personal` shape — the only shape that promotes (see the SWT-30 suite).
	s.project = s.insID(t, ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, ai_locality, ai_classify,
		                       classify_promote_after)
		 VALUES ($1,$1,NULL,'manual','dashboard','local_only',true, now() - interval '1 hour')
		 RETURNING id`, prrProject)
	s.account = s.insID(t, ctx,
		`INSERT INTO source_accounts (provider, account_email, send_enabled) VALUES ($1,$2,false) RETURNING id`,
		prrProvider, prrAccount)
	return s
}

func prrRequire0026(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name='task_dismissals' AND column_name IN ('closed_from_status','reopened_at')`).Scan(&n); err != nil {
		t.Fatalf("probe task_dismissals columns: %v", err)
	}
	if n != 2 {
		t.Fatalf("task_dismissals lacks migration 0026's columns; apply migrations/0026_dismissal_reopen.sql " +
			"to the compose db (`make migrate`)")
	}
}

func prrCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE provider='` + prrProvider + `')`
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`
	const msgs = `(SELECT id FROM normalized_messages WHERE raw_source_item_id IN ` + raws + `)`
	const projs = `(SELECT id FROM projects WHERE slug LIKE 'itest-promreopen-%')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	const actors = `(actor LIKE 'promote:%' OR actor IN ('` + prrHuman + `','` + prrCloser + `'))`
	for _, q := range []string{
		`DELETE FROM classify_promotions WHERE normalized_message_id IN ` + msgs +
			` OR project_id IN ` + projs + ` OR task_id IN ` + tasksOf,
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM external_refs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM deliveries WHERE task_id IN ` + tasksOf,
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + ` OR ` + actors + `)`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf + ` OR ` + actors,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM capture_decisions WHERE message_id IN ` + msgs + ` OR project_id IN ` + projs,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'itest-promreopen:%'`,
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM ai_extractions WHERE ai_run_id IN (SELECT id FROM ai_runs WHERE model='` + prrModel + `')`,
		`DELETE FROM ai_runs WHERE model='` + prrModel + `'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider='` + prrProvider + `'`,
		`DELETE FROM projects WHERE slug LIKE 'itest-promreopen-%'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func (s *prrSuite) insID(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", q, err)
	}
	return id
}

func (s *prrSuite) count(t *testing.T, ctx context.Context, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

func (s *prrSuite) dbNow(t *testing.T, ctx context.Context) time.Time {
	t.Helper()
	var now time.Time
	if err := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatalf("read db clock: %v", err)
	}
	return now
}

func (s *prrSuite) thread(t *testing.T, ctx context.Context, label string) int64 {
	t.Helper()
	return s.insID(t, ctx,
		`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,'itest-promreopen','[]') RETURNING id`,
		"itest-promreopen:"+label)
}

// message seeds a message and its raw row; created_at is explicit because D2's
// clock is the ingest time.
func (s *prrSuite) message(t *testing.T, ctx context.Context, label string, thread int64,
	direction string, createdAt time.Time) (msg, raw int64) {
	t.Helper()
	raw = s.insID(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,'{}',$3, now()) RETURNING id`,
		s.account, "itest-promreopen-"+label, "itest-promreopen-h-"+label)
	sender := prrSender
	if direction == "outbound" {
		sender = "Salvador <salvador@example.test>"
	}
	msg = s.insID(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		    body_text, subject, sender, channel, created_at)
		 VALUES ($1,$2,$3,$4,$5,'itest-promreopen body','Your payment is due',$6,'gmail',$5) RETURNING id`,
		raw, thread, direction, "<itest-promreopen-"+label+"@mail.example>", createdAt, sender)
	return msg, raw
}

// verdict writes the ai_runs + ai_extractions pair classify writes, recorded
// NOW (after the cutover).
func (s *prrSuite) verdict(t *testing.T, ctx context.Context, msg, raw int64, kind, title string) {
	t.Helper()
	run := s.insID(t, ctx,
		`INSERT INTO ai_runs (worker_type, provider, model, input, output, status)
		 VALUES ('classify','itest-promreopen',$1, jsonb_build_object('itest','itest-promreopen'), '{}', 'ok')
		 RETURNING id`, prrModel)
	fields := fmt.Sprintf(`{"actionable":true,"kind":%q,"title":%q,"reason":"itest-promreopen verdict",`+
		`"sender":%q,"subject":"Your payment is due","project_id":%d,"normalized_message_id":%d,"link_candidates":0}`,
		kind, title, prrSender, s.project, msg)
	s.insID(t, ctx, `INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields) VALUES ($1,$2,$3::jsonb) RETURNING id`,
		run, raw, fields)
}

// inbound is a promotable message: stored verdict + the latest capture decision
// attributing it to the promoting project.
func (s *prrSuite) inbound(t *testing.T, ctx context.Context, label string, thread int64,
	createdAt time.Time, kind string) int64 {
	t.Helper()
	msg, raw := s.message(t, ctx, label, thread, "inbound", createdAt)
	s.verdict(t, ctx, msg, raw, kind, "itest-promreopen "+label)
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
		 VALUES ($1,'shadow','attributed',$2,'itest-promreopen')`, msg, s.project); err != nil {
		t.Fatalf("attribute message %s: %v", label, err)
	}
	return msg
}

// outbound is D13's layer 2 as a fixture: our own send, classified, with NO
// capture_decisions row — capture's pending set is inbound-only, so an outbound
// message can never have one, and promote's inbox inner-joins it.
func (s *prrSuite) outbound(t *testing.T, ctx context.Context, label string, thread int64, createdAt time.Time) int64 {
	t.Helper()
	msg, raw := s.message(t, ctx, label, thread, "outbound", createdAt)
	s.verdict(t, ctx, msg, raw, "payment_due", "itest-promreopen our own reply "+label)
	return msg
}

func (s *prrSuite) run(t *testing.T, ctx context.Context, cfg promote.Config) promote.Stats {
	t.Helper()
	stats, err := promote.Run(ctx, s.pool, s.ex, cfg)
	if err != nil {
		t.Fatalf("promote.Run: %v", err)
	}
	return stats
}

type prrPromotion struct {
	action string
	taskID *int64
	reason string
}

func (s *prrSuite) promotion(t *testing.T, ctx context.Context, msg int64) (prrPromotion, bool) {
	t.Helper()
	var p prrPromotion
	var reason *string
	err := s.pool.QueryRow(ctx,
		`SELECT action, task_id, reason FROM classify_promotions WHERE normalized_message_id=$1`, msg).
		Scan(&p.action, &p.taskID, &reason)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return p, false
		}
		t.Fatalf("read promotion for message %d: %v", msg, err)
	}
	if reason != nil {
		p.reason = *reason
	}
	return p, true
}

func (s *prrSuite) status(t *testing.T, ctx context.Context, taskID int64) string {
	t.Helper()
	var st string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, taskID).Scan(&st); err != nil {
		t.Fatalf("read task %d: %v", taskID, err)
	}
	return st
}

func (s *prrSuite) execute(t *testing.T, ctx context.Context, tool, actor string, taskID int64, args string) {
	t.Helper()
	if _, err := s.ex.Execute(ctx, executor.Call{Tool: tool, Actor: actor, Args: []byte(args), TaskID: &taskID}); err != nil {
		t.Fatalf("%s(%s) as %s: %v", tool, args, actor, err)
	}
}

type prrThread struct {
	thread      int64
	task        int64
	ours        int64 // the outbound message on the thread (criterion 26)
	dismissal   int64
	dismissedAt time.Time
}

// seedPromotedThenDismissed: a first verdict (informational -> the review lane,
// `holding`) is promoted by the REAL pass, which records the task's source
// thread; then Salvador dismisses it on the board.
func (s *prrSuite) seedPromotedThenDismissed(t *testing.T, ctx context.Context, label, code string) prrThread {
	t.Helper()
	var th prrThread
	th.thread = s.thread(t, ctx, label)
	start := s.dbNow(t, ctx).Add(-30 * time.Minute)
	first := s.inbound(t, ctx, label+"-first", th.thread, start, "informational")
	th.ours = s.outbound(t, ctx, label+"-ours", th.thread, start.Add(time.Minute))

	s.run(t, ctx, promote.Config{})
	p, ok := s.promotion(t, ctx, first)
	if !ok || p.taskID == nil || p.action != "review" {
		t.Fatalf("setup: the first verdict was not promoted into a review task (%+v, found=%v)", p, ok)
	}
	th.task = *p.taskID
	if st := s.status(t, ctx, th.task); st != "holding" {
		t.Fatalf("setup: the promoted task is %q, want holding", st)
	}
	s.execute(t, ctx, "task_dismiss", prrHuman, th.task,
		`{"task_id":`+strconv.FormatInt(th.task, 10)+`,"reason_code":"`+code+`"}`)
	if err := s.pool.QueryRow(ctx,
		`SELECT id, created_at FROM task_dismissals WHERE task_id=$1 ORDER BY id DESC LIMIT 1`, th.task).
		Scan(&th.dismissal, &th.dismissedAt); err != nil {
		t.Fatalf("setup: read the dismissal: %v", err)
	}
	return th
}

func (s *prrSuite) projectTasks(t *testing.T, ctx context.Context) int {
	t.Helper()
	return s.count(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, s.project)
}

// ---- criteria 10, 11, D5, D7, 18: a new message brings the task back ----------

// MUTATIONS that turn this red:
//   - drop threadTask's middle (dismissed) lookup -> Q3 creates a NEW ready task
//     for the payment_due verdict (project task count 2, action 'task');
//   - never call task_reopen after the log -> the task stays closed;
//   - restore `ready` instead of closed_from_status -> the review-lane task goes
//     live on an email (D5).
func TestPromoteReopen_NewInboundBringsTheDismissedTaskBack(t *testing.T) {
	ctx := context.Background()
	s := newPRRSuite(t, ctx)

	th := s.seedPromotedThenDismissed(t, ctx, "back", "not_actionable")
	// A WHITELISTED kind, deliberately: under today's Q3 this is the case that
	// creates a live `ready` duplicate.
	next := s.inbound(t, ctx, "back-next", th.thread, th.dismissedAt.Add(time.Minute), "payment_due")
	logsBefore := s.count(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='log'`, th.task)

	stats := s.run(t, ctx, promote.Config{})

	p, ok := s.promotion(t, ctx, next)
	if !ok {
		t.Fatalf("no classify_promotions row for the new message")
	}
	if p.action != "attached" || p.taskID == nil || *p.taskID != th.task {
		t.Errorf("promotion = {action %q, task %v}, want {attached, %d}. D7: the action vocabulary is "+
			"unchanged (no 'reopened' action); the typed outcome lives in task_dismissals", p.action, p.taskID, th.task)
	}
	for _, want := range []string{
		"was dismissed (not_actionable)",
		"reopen requested against dismissal " + strconv.FormatInt(th.dismissal, 10),
	} {
		if !strings.Contains(p.reason, want) {
			t.Errorf("promotion reason %q does not contain %q (D7)", p.reason, want)
		}
	}
	if got := s.status(t, ctx, th.task); got != "holding" {
		t.Errorf("the dismissed task is %q after a new inbound message, want holding (its pre-dismissal "+
			"status; D5 — a review-lane task must not go live by email)", got)
	}
	if n := s.projectTasks(t, ctx); n != 1 {
		t.Errorf("project has %d tasks, want 1. D8/invariant 2: a message on a dismissed thread returns to the "+
			"SAME task; a second one is the duplicate this ticket removes", n)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='log'`, th.task); n != logsBefore+1 {
		t.Errorf("log events on the task went %d -> %d, want exactly one appended line", logsBefore, n)
	}
	var by *string
	var byMsg *int64
	if err := s.pool.QueryRow(ctx,
		`SELECT reopened_by, reopened_by_message_id FROM task_dismissals WHERE id=$1`, th.dismissal).
		Scan(&by, &byMsg); err != nil {
		t.Fatalf("read the dismissal stamp: %v", err)
	}
	if by == nil || *by != promote.Actor || byMsg == nil || *byMsg != next {
		t.Errorf("dismissal stamp = (by %v, msg %v), want (%s, %d)", by, byMsg, promote.Actor, next)
	}
	if n := s.count(t, ctx,
		`SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool='task_reopen' AND actor=$2`,
		th.task, promote.Actor); n != 1 {
		t.Errorf("task_reopen audit rows by %s = %d, want 1 (invariant 3: the reopen goes through the executor)",
			promote.Actor, n)
	}
	if stats.Reopened != 1 {
		t.Errorf("Stats.Reopened = %d, want 1 (criterion 11: counted from reopened:true)", stats.Reopened)
	}
	// Criterion 18, promote's layer: our own outbound reply is not in the inbox.
	if _, ok := s.promotion(t, ctx, th.ours); ok {
		t.Errorf("the OUTBOUND message on the dismissed thread has a promotion row. D13 layer 2: the inbox " +
			"inner-joins the latest capture_decisions row, which an outbound message can never have")
	}
}

// ---- criterion 12 (D8): a message that predates the dismissal ----------------

// "A message that predates the dismissal yields action='attached' on the
// dismissed task, one log line, the task still closed, and NO new task."
func TestPromoteReopen_AMessageThatPredatesTheDismissalOnlyLogs(t *testing.T) {
	ctx := context.Background()
	s := newPRRSuite(t, ctx)

	th := s.seedPromotedThenDismissed(t, ctx, "prior", "handled_elsewhere")
	// Ingested before the dismissal, classified after it — the verdict lag.
	prior := s.inbound(t, ctx, "prior-next", th.thread, th.dismissedAt.Add(-time.Minute), "deadline")

	stats := s.run(t, ctx, promote.Config{})

	p, ok := s.promotion(t, ctx, prior)
	if !ok || p.action != "attached" || p.taskID == nil || *p.taskID != th.task {
		t.Errorf("promotion for a predating message = %+v (found=%v), want attached to %d. D8: today's Q3 "+
			"fall-through creates a duplicate of the task he just dismissed", p, ok, th.task)
	}
	if got := s.status(t, ctx, th.task); got != "closed" {
		t.Errorf("a message ingested BEFORE the dismissal reopened the task (%q). Salvador saw it when he "+
			"dismissed; the handler answers message_predates_dismissal", got)
	}
	if n := s.projectTasks(t, ctx); n != 1 {
		t.Errorf("project has %d tasks, want 1 — NO new task (criterion 12)", n)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='log'`, th.task); n != 1 {
		t.Errorf("log events on the dismissed task = %d, want 1", n)
	}
	var reopenedAt *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT reopened_at FROM task_dismissals WHERE id=$1`, th.dismissal).
		Scan(&reopenedAt); err != nil {
		t.Fatalf("read the dismissal: %v", err)
	}
	if reopenedAt != nil {
		t.Errorf("the dismissal was stamped reopened by a message that predates it")
	}
	if stats.Reopened != 0 {
		t.Errorf("Stats.Reopened = %d, want 0", stats.Reopened)
	}
}

// ---- criterion 23 (promote): only an OPEN dismissal counts --------------------

// "A task dismissed, activity-reopened, then task_close'd is plain-closed. The
// next verdict creates a new task (Q3)."
//
// MUTATION: drop `reopened_at IS NULL` from threadTask's middle lookup -> the
// stamped dismissal is found, the verdict attaches to the closed task instead of
// creating one, and this goes red.
func TestPromoteReopen_APlainClosedTaskFallsThroughQ3(t *testing.T) {
	ctx := context.Background()
	s := newPRRSuite(t, ctx)

	th := s.seedPromotedThenDismissed(t, ctx, "reclosed", "duplicate")
	first := s.inbound(t, ctx, "reclosed-next1", th.thread, th.dismissedAt.Add(time.Minute), "informational")
	s.run(t, ctx, promote.Config{})
	if got := s.status(t, ctx, th.task); got != "holding" {
		t.Fatalf("setup: the activity reopen did not happen (task %q); criterion 11 is red first", got)
	}
	s.execute(t, ctx, "task_close", prrCloser, th.task,
		`{"task_id":`+strconv.FormatInt(th.task, 10)+`,"reason":"handled; plain close"}`)

	second := s.inbound(t, ctx, "reclosed-next2", th.thread, s.dbNow(t, ctx), "payment_due")
	s.run(t, ctx, promote.Config{})

	p, ok := s.promotion(t, ctx, second)
	if !ok || p.action != "task" || p.taskID == nil || *p.taskID == th.task {
		t.Errorf("promotion after a PLAIN close = %+v (found=%v), want action=task on a NEW task. D3: a plain "+
			"task_close is lifecycle, not a dismissal — Q3's fall-through applies", p, ok)
	}
	if got := s.status(t, ctx, th.task); got != "closed" {
		t.Errorf("the plain-closed task came back (%q)", got)
	}
	if _, ok := s.promotion(t, ctx, first); !ok {
		t.Errorf("setup message lost its promotion row")
	}
}

// ---- D11 / criterion 11: dry-run writes nothing ------------------------------

func TestPromoteReopen_DryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	s := newPRRSuite(t, ctx)

	th := s.seedPromotedThenDismissed(t, ctx, "dry", "wrong_kind")
	next := s.inbound(t, ctx, "dry-next", th.thread, th.dismissedAt.Add(time.Minute), "payment_due")
	s.run(t, ctx, promote.Config{DryRun: true})

	if _, ok := s.promotion(t, ctx, next); ok {
		t.Errorf("a dry run wrote a classify_promotions row")
	}
	if got := s.status(t, ctx, th.task); got != "closed" {
		t.Errorf("a dry run reopened the task (%q)", got)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool='task_reopen'`, th.task); n != 0 {
		t.Errorf("a dry run called task_reopen %d time(s); D11: it prints the request and writes nothing", n)
	}
}
