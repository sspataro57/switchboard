//go:build integration

package capture_test

// SWT-20 acceptance criterion 19: the capture engine RECORDS PROVENANCE. A
// live-mode `task` decision on an upwork message produces a task whose
// `source_thread_id` is that message's thread, written through the executor —
// and a failure to record it fails the pass LOUDLY rather than leaving a
// provenance-less task behind.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run CaptureProvenance ./internal/capture/
//
// WHY THIS IS THE LOAD-BEARING END OF THE TICKET: everything else in SWT-20
// consumes `tasks.source_thread_id`. If nothing writes it, the whole ticket is
// inert — `draft_delivery` refuses every upwork draft, `drafts` resolves no
// upwork target, and the two multi-room production clients stay exactly as
// undraftable as SWT-19's mitigation left them, with every test still green
// because every other suite sets the column by hand. That is this repo's
// recurring landmine (a guard whose column nothing in production writes), and
// this file is the only test that can see it.
//
// The linkRuleRef shape is the model (rules_store.go:749-766): a SECOND executor
// call after create_task, made by the driver, failing the pass rather than
// continuing. The reason is identical — without the ref the next notification
// creates a duplicate task; without the provenance the task can never be
// delivered, and both failures are silent unless the pass stops.
//
// GREENFIELD NOTE: red twice over today. `tasks.source_thread_id` does not exist
// until migration 0019 is applied to the compose db ("column source_thread_id
// does not exist" is the expected first failure), and `rules_store.go` neither
// selects `m.thread_id` in its pending query nor calls
// `task_set_source_thread`.
//
// Cross-suite discipline: this suite shares rules_integration_test.go's problem
// — EvaluateRules' pending filter is GLOBAL, so a run here writes a
// capture_decisions row for every other suite's leftover inbound message. It
// therefore joins the same pact and deletes capture_decisions wholesale (compose
// db only; the DSN guard in newCPSuite refuses the production address), and owns
// projects `itest-capprov-%`, the source account below, and the upwork thread
// keys built from cpClient.

import (
	"context"
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
	cpSlug    = "itest-capprov-client"
	cpAccount = "itest-capprov@upwork.example.test"
	cpClient  = "dddd4040-0000-0000-0000-0000000capp1"
	cpRoom    = "room_capp1a2b3c"
	cpExtMsg  = "story_itest_capprov_1"
)

// Assembled in Go: the key format has one spelling and no SQL may build or
// dissect it (upworkcrm/keyspelling_test.go).
func cpRoomedKey() string { return "upwork_crm:" + cpClient + ":room:" + cpRoom }

type cpSuite struct {
	pool     *pgxpool.Pool
	ex       *executor.Executor
	project  int64
	threadID int64
	msgID    int64
}

func newCPSuite(t *testing.T, ctx context.Context) *cpSuite {
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
	cpCleanup(t, ctx, pool)
	t.Cleanup(func() { cpCleanup(t, ctx, pool) })

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	s := &cpSuite{pool: pool, ex: executor.New(reg, checker, audit.NewPGStore(pool))}
	s.seed(t, ctx)
	return s
}

func cpCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const acct = `(SELECT id FROM source_accounts WHERE provider='upwork_crm' AND account_email='` + cpAccount + `')`
	const projs = `(SELECT id FROM projects WHERE slug LIKE 'itest-capprov-%')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	threads := []string{cpRoomedKey()}
	stmts := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM capture_decisions`, nil},
		{`DELETE FROM external_refs WHERE task_id IN ` + tasksOf, nil},
		{`DELETE FROM task_events WHERE task_id IN ` + tasksOf, nil},
		{`DELETE FROM deliveries WHERE task_id IN ` + tasksOf, nil},
		{`DELETE FROM policy_decisions WHERE audit_event_id IN
		    (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor LIKE 'capture:%')`, nil},
		{`DELETE FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor LIKE 'capture:%'`, nil},
		{`DELETE FROM tasks WHERE project_id IN ` + projs, nil},
		{`DELETE FROM capture_rules WHERE project_id IN ` + projs, nil},
		{`DELETE FROM projects WHERE slug LIKE 'itest-capprov-%'`, nil},
		{`DELETE FROM normalized_messages WHERE raw_source_item_id IN
		    (SELECT id FROM raw_source_items WHERE source_account_id IN ` + acct + `)`, nil},
		{`DELETE FROM normalized_threads WHERE thread_key = ANY($1)`, []any{threads}},
		{`DELETE FROM raw_source_items WHERE source_account_id IN ` + acct, nil},
		{`DELETE FROM sync_runs WHERE source_account_id IN ` + acct, nil},
		{`DELETE FROM source_accounts WHERE provider='upwork_crm' AND account_email=$1`, []any{cpAccount}},
	}
	for _, st := range stmts {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

// seed builds one upwork conversation and the rule that claims it.
//
// A `thread_key_prefix` rule with no `key_regex` uses the THREAD KEY verbatim as
// its external key, which is what a real upwork rule looks like: the
// conversation IS the ticket. `external_system='upwork_crm'` is what makes the
// decision a `task` rather than a bare attribution.
func (s *cpSuite) seed(t *testing.T, ctx context.Context) {
	t.Helper()
	ins := func(q string, args ...any) int64 {
		var id int64
		if err := s.pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
		return id
	}
	s.project = ins(`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
	                 VALUES ($1,$1,'itest-capprov client','manual','dashboard','/tmp/itest-capprov','any') RETURNING id`, cpSlug)
	ins(`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, priority, enabled, note)
	     VALUES ($1,'thread_key_prefix',$2,'upwork_crm',100,true,'itest-capprov upwork room') RETURNING id`,
		s.project, cpRoomedKey())

	acct := ins(`INSERT INTO source_accounts (provider, account_email, scopes, send_enabled, calendar_in_availability)
	             VALUES ('upwork_crm',$1,'{}',false,false)
	             ON CONFLICT (provider, account_email) DO UPDATE SET account_email=EXCLUDED.account_email
	             RETURNING id`, cpAccount)
	s.threadID = ins(`INSERT INTO normalized_threads (thread_key, subject, participants)
	                  VALUES ($1,'itest-capprov','[]') RETURNING id`, cpRoomedKey())
	raw := ins(`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
	            VALUES ($1,'communications:comm-itest-capprov','{}','itest-capprov-hash', now()) RETURNING id`, acct)
	s.msgID = ins(`INSERT INTO normalized_messages
	                 (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
	                  body_text, subject, sender, channel)
	               VALUES ($1,$2,'inbound',$3, now() - interval '20 minutes',
	                       'Can you take a look at the importer before Friday?','','client@itest-capprov','upwork')
	               RETURNING id`, raw, s.threadID, cpExtMsg)
}

// Criterion 19: a LIVE `task` decision records the message's thread on the task.
func TestCaptureProvenance_Integration_LiveTaskCarriesItsSourceThread(t *testing.T) {
	ctx := context.Background()
	s := newCPSuite(t, ctx)

	if _, err := capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{
		Mode: "live", Horizon: 24 * time.Hour,
	}); err != nil {
		t.Fatalf("EvaluateRules(live): %v", err)
	}

	var taskID int64
	if err := s.pool.QueryRow(ctx,
		`SELECT t.id FROM tasks t JOIN external_refs er ON er.task_id = t.id
		  WHERE er.system='upwork_crm' AND er.external_key=$1`, cpRoomedKey()).Scan(&taskID); err != nil {
		t.Fatalf("no task linked to the upwork conversation after a live run: %v", err)
	}

	var got *int64
	if err := s.pool.QueryRow(ctx, `SELECT source_thread_id FROM tasks WHERE id=$1`, taskID).Scan(&got); err != nil {
		t.Fatalf("read tasks.source_thread_id: %v", err)
	}
	if got == nil {
		t.Fatalf("task %d created by a live capture pass carries NO source_thread_id (the message's thread is "+
			"%d).\nWithout this write SWT-20 is inert: draft_delivery refuses every upwork draft for lack of "+
			"provenance, drafts resolves no target, and the two multi-room production clients stay exactly as "+
			"undraftable as SWT-19's mitigation left them — with every other test green, because every other "+
			"suite sets the column by hand", taskID, s.threadID)
	}
	if *got != s.threadID {
		t.Errorf("tasks.source_thread_id = %d, want %d — the thread of the MESSAGE that raised the task, not "+
			"any other thread of the client", *got, s.threadID)
	}

	// Invariant 3, and criterion 20's other half: the write went through the
	// executor, so it is audited and rules_structure_test.go's ban on touching
	// tasks directly from this package still holds.
	var audits int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE tool='task_set_source_thread' AND task_id=$1`, taskID).Scan(&audits); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if audits == 0 {
		t.Errorf("no audit_events row for task_set_source_thread on task %d. The capture engine must reach the "+
			"column the way it reaches create_task and link_external_ref — a direct UPDATE would skip "+
			"validate/policy/audit and break internal/capture/rules_structure_test.go", taskID)
	}
}

// Criterion 19's second clause, as the INVARIANT it protects: a live capture
// pass leaves NO task behind without provenance.
//
// The mechanism the SPEC asks for is `linkRuleRef`'s (rules_store.go:749-766) —
// a second executor call that FAILS THE PASS rather than continuing. That
// mechanism is not directly observable from outside (nothing here can make
// `task_set_source_thread` fail on demand without reaching into the tool), so
// what is asserted is the state it exists to prevent, which is the thing that
// actually hurts: a task created by capture that carries no conversation.
//
// Such a task is a dead end and a silent one. `draft_delivery` refuses it for
// lack of provenance, `drafts` resolves no target for it, and no later pass
// revisits it — the live decision is already recorded, so the create-task branch
// never runs for that key again. The `external_refs` row even guarantees it: the
// key is taken, forever (UNIQUE (system, external_key)).
//
// REVIEWER'S JOB, since the test cannot do it: check that the provenance call is
// made in the linkRuleRef SHAPE — after the task id is recorded on the decision,
// with the error returned rather than logged. A version that logs and continues
// satisfies this test on the happy path and produces exactly the dead-end task
// above the first time it fails.
func TestCaptureProvenance_Integration_NoTaskSurvivesWithoutProvenance(t *testing.T) {
	ctx := context.Background()
	s := newCPSuite(t, ctx)

	if _, err := capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{
		Mode: "live", Horizon: 24 * time.Hour,
	}); err != nil {
		t.Fatalf("EvaluateRules(live): %v", err)
	}

	var created, orphaned int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE source_thread_id IS NULL)
		   FROM tasks WHERE project_id=$1`, s.project).Scan(&created, &orphaned); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	// Guard the guard: if the pass created nothing, the assertion below is
	// vacuous and would stay green through any implementation.
	if created == 0 {
		t.Fatalf("the live pass created no task at all on project %d, so 'every created task carries "+
			"provenance' is vacuously true and this test proves nothing. The rule is a thread_key_prefix rule "+
			"with external_system='upwork_crm' over one inbound message on that thread", s.project)
	}
	if orphaned != 0 {
		t.Errorf("%d of %d tasks created by a live capture pass carry NO source_thread_id. Such a task is a "+
			"dead end: draft_delivery refuses it, drafts resolves no target for it, and no later pass revisits "+
			"it — the live decision is spent and external_refs has taken the key forever (UNIQUE (system, "+
			"external_key)). Fail the pass loudly instead, the way linkRuleRef does", orphaned, created)
	}
}
