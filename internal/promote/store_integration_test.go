//go:build integration

package promote_test

// SWT-30 against a real database: the promoter's inbox, its cutover, its double
// residue exclusion, its two lanes, its attach rule, its refusals and its lock
// (docs/tickets/classify-promotion_SPEC.md criteria 2, 3, 4, 5, 7, 8, 9, 10, 11,
// 12, 14, 16, 17).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run Promote ./internal/promote/
//
// Build-tagged `integration` AND env-gated on DATABASE_URL, with a FATAL guard
// on 192.168.50.49 — this suite deletes rows.
//
// WHY EVERY ONE OF THESE IS HERE AND NOT IN promote_test.go. Decide is pure and
// tested there. Everything in this file turns on a value POSTGRES produces: the
// worker_type on ai_runs, the LATEST capture_decisions row and its project, the
// `(action='unmatched') = (project_id IS NULL)` CHECK from 0015, the
// classify_promote_after comparison, the unique index, tasks.source_thread_id,
// tasks.status. A fake store would supply the very values the filters are
// supposed to compute — SWT-21's 6th landmine, whose standing rule is: for any
// predicate whose input comes from a COLUMN, the regression test belongs in the
// integration suite and must fail when the column is dropped from the SELECT.
// The mutation that must turn each assertion red is named inline.
//
// NO LLM, NO NETWORK. The promoter reads stored ai_extractions rows; this suite
// writes those rows by hand, exactly as `classify run` would have. A pass here
// must cost zero GPU seconds.
//
// CROSS-POLLUTION PACT (IK, "integration suites cross-pollute"; `make
// integration` runs -p 1 for this reason). The promoter's inbox is GLOBAL, like
// capture's and classify's. What keeps this suite from acting on other suites'
// leftovers is the cutover itself: promotion requires
// projects.classify_promote_after IS NOT NULL and NO other suite sets it (0021
// adds the column with no default and no backfill). That is criterion 5's
// fail-closed design doing double duty, and it is the reason the global
// assertions below are honest. Conversely this suite owns, and clears at start
// AND at end:
//   - projects  itest-promote-%
//   - source_accounts provider itest-promote-src
//   - normalized_threads itest-promote:%
//   - ai_runs  model itest-promote-model  (worker_type is the REAL lane name —
//     criterion 3 is about that name, so it cannot be faked with a private one)
//   - classify_promotions rows referencing any of the above
//   - audit_events / policy_decisions with actor promote:%
//
// ANTI-DATE-ROT: every timestamp is now() or now() - interval '...'. There is no
// literal date anywhere in this file.
//
// GREENFIELD NOTE — EXPECTED RED. internal/promote does not exist and migration
// 0021 has not been applied to the compose db, so this file compile-FAILS the
// integration build and then fails at seed time on the missing
// classify_promote_after column.
//
// ---- IMPOSED surface (store.go / promote.go), beyond promote_test.go's -------
//
//	// Run is the driver: advisory lock, inbox query, per-verdict claim, executor
//	// calls. Shaped on capture.EvaluateRules, which the SPEC names as the sibling
//	// to copy.
//	func Run(ctx context.Context, pool *pgxpool.Pool, ex *executor.Executor, cfg Config) (Stats, error)
//	type Config struct { DryRun bool; Limit int }
//	type Stats struct { ... }
//
// Stats' FIELDS are deliberately not asserted: the SPEC names a printed plan,
// not Go field names. Every assertion below is against the database, which is
// the contract. Criterion 5's "exits 0 with a line saying no project has a
// cutover set" is a CLI concern (cmd/classify) and is checked by the smoke step
// in the SPEC's verification protocol, not here — asserting a log line through
// this seam would pin prose.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/promote"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	pmProvider = "itest-promote-src"
	pmAccount  = "itest-promote@pg-main"
	pmModel    = "itest-promote-model"

	// The promoting project: local_only + ai_classify + a cutover. The `personal`
	// shape, which is the only shape that promotes.
	pmProject = "itest-promote-personal"
	// local_only + ai_classify=FALSE, cutover SET. The control for criterion 2's
	// ai_classify clause: the ONLY thing that excludes it is that clause, so
	// dropping it from the inbox turns this suite red instead of quietly widening
	// promotion to the bulk lane.
	pmNoAIProject = "itest-promote-noai"

	// SPEC criterion 17: "pg_try_advisory_lock on key 0x5157_0021 (free —
	// verified against every key in the repo: 0005 orchestrator, 0006 triage,
	// 0015 capture, 0022 classify, 0028 calendar booking)".
	pmLockKey = int64(0x5157_0021)

	// The actor every executor call must carry (criterion 13), in the
	// capture:{connector} shape.
	pmActor = "promote:classify"
)

// ---- harness ------------------------------------------------------------------

type pmSuite struct {
	pool *pgxpool.Pool
	ex   *executor.Executor

	project     int64
	noAIProject int64

	msg    map[string]int64 // label -> normalized_messages.id
	raw    map[string]int64 // label -> raw_source_items.id
	thread map[string]int64 // label -> normalized_threads.id
	extr   map[string]int64 // label -> ai_extractions.id

	// closedTask is the finished task seeded on the `reraise` thread (Q3's
	// second branch).
	closedTask int64
}

func newPMSuite(t *testing.T, ctx context.Context) *pmSuite {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db (this suite deletes fixtures and " +
			"sets classify_promote_after); use the compose db on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	pmCleanup(t, ctx, pool)
	t.Cleanup(func() { pmCleanup(t, ctx, pool) })

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))

	s := &pmSuite{
		pool: pool, ex: executor.New(reg, checker, audit.NewPGStore(pool)),
		msg: map[string]int64{}, raw: map[string]int64{}, thread: map[string]int64{}, extr: map[string]int64{},
	}
	s.seed(t, ctx)
	return s
}

// pmCleanup deletes this suite's fixtures in the FK order the SPEC's
// verification protocol lists:
//
//	classify_promotions -> task_events -> tasks -> capture_decisions ->
//	normalized_messages -> normalized_threads -> ai_extractions -> ai_runs ->
//	raw_source_items -> source_accounts -> projects
//
// with two departures, both forced by FKs the SPEC's list does not mention and
// both learned from internal/capture/rules_integration_test.go:
//
//   - audit_events.task_id REFERENCES tasks, and policy_decisions.audit_event_id
//     REFERENCES audit_events. The executor writes both for every create_task /
//     task_append_log / task_set_source_thread call, so `DELETE FROM tasks`
//     fails without clearing them first. They are swept by ACTOR as well as by
//     task, because the audit row for a create_task call is written BEFORE the
//     task exists and therefore carries no task_id to scope by.
//   - classify_promotions.ai_extraction_id REFERENCES ai_extractions, so the
//     promotion rows must go before the extractions (they already do).
func pmCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE provider='` + pmProvider + `')`
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`
	const msgs = `(SELECT id FROM normalized_messages WHERE raw_source_item_id IN ` + raws + `)`
	const projs = `(SELECT id FROM projects WHERE slug LIKE 'itest-promote-%')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`

	stmts := []string{
		`DELETE FROM classify_promotions WHERE normalized_message_id IN ` + msgs +
			` OR project_id IN ` + projs + ` OR task_id IN ` + tasksOf,
		`DELETE FROM external_refs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM deliveries WHERE task_id IN ` + tasksOf,
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor LIKE 'promote:%')`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor LIKE 'promote:%'`,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM capture_decisions WHERE message_id IN ` + msgs + ` OR project_id IN ` + projs,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'itest-promote:%'`,
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM ai_extractions WHERE ai_run_id IN (SELECT id FROM ai_runs WHERE model='` + pmModel + `')`,
		`DELETE FROM ai_runs WHERE model='` + pmModel + `'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider='` + pmProvider + `'`,
		`DELETE FROM projects WHERE slug LIKE 'itest-promote-%'`,
	}
	for _, q := range stmts {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func (s *pmSuite) insID(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", q, err)
	}
	return id
}

func (s *pmSuite) count(t *testing.T, ctx context.Context, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n
}

// exec runs a statement and fails the test on error.
func (s *pmSuite) exec(t *testing.T, ctx context.Context, q string, args ...any) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// run drives one live pass.
func (s *pmSuite) run(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := promote.Run(ctx, s.pool, s.ex, promote.Config{}); err != nil {
		t.Fatalf("promote.Run: %v", err)
	}
}

// ---- fixture ------------------------------------------------------------------

// msgSpec is one seeded message.
type pmMsg struct {
	label     string
	thread    string // thread label; messages sharing one share a normalized_threads row
	sender    string
	subject   string
	body      string
	direction string
	minsAgo   int
}

// verdictSpec is one stored classify verdict: an ai_runs row plus its
// ai_extractions row, written exactly as internal/classify/classify.go writes
// them (fields carry actionable/kind/title/reason/sender/subject/project_id).
type pmVerdict struct {
	message    string // message label
	workerType string // 'classify' | 'classify_residue' — criterion 3(a) turns on THIS
	status     string // ai_runs.status; 'ok' is the only eligible value (criterion 2)
	actionable bool
	kind       string
	title      string
	// runAgeMins positions ai_runs.created_at relative to now(); the cutover sits
	// at now() - 1 hour, so 30 is AFTER it and 120 is BEFORE it (criterion 4).
	runAgeMins int
	// storedProject overrides fields->>'project_id' (criterion 16). Zero means
	// "the project the message is currently attributed to".
	storedProject int64
}

func (s *pmSuite) seed(t *testing.T, ctx context.Context) {
	t.Helper()

	// ai_locality and ai_classify are named EXPLICITLY on both projects. 0016
	// defaults ai_locality to 'local_only' and 0018 defaults ai_classify to
	// false, so a fixture that omits them gets a project the lane SKIPS — the
	// suite would then pass while exercising nothing (23 fixtures hit exactly
	// this when 0016 landed). client is NULL: D6 depends on it
	// (getnext.go filters p.client = $1, so a personal task can never be handed
	// to a worker queue).
	s.project = s.insID(t, ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, ai_locality, ai_classify,
		                       classify_promote_after)
		 VALUES ($1,$1,NULL,'manual','dashboard','local_only',true, now() - interval '1 hour')
		 RETURNING id`, pmProject)
	s.noAIProject = s.insID(t, ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, ai_locality, ai_classify,
		                       classify_promote_after)
		 VALUES ($1,$1,NULL,'manual','dashboard','local_only',false, now() - interval '1 hour')
		 RETURNING id`, pmNoAIProject)

	account := s.insID(t, ctx,
		`INSERT INTO source_accounts (provider, account_email, send_enabled)
		 VALUES ($1,$2,false) RETURNING id`, pmProvider, pmAccount)

	thread := func(label, subject string) int64 {
		if id, ok := s.thread[label]; ok {
			return id
		}
		id := s.insID(t, ctx,
			`INSERT INTO normalized_threads (thread_key, subject, participants)
			 VALUES ($1,$2,'[]') RETURNING id`, "itest-promote:"+label, subject)
		s.thread[label] = id
		return id
	}

	message := func(m pmMsg) {
		rawID := s.insID(t, ctx,
			`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
			 VALUES ($1,$2,'{}',$3, now()) RETURNING id`,
			account, "itest-promote-"+m.label, "itest-promote-h-"+m.label)
		threadID := thread(m.thread, m.subject)
		id := s.insID(t, ctx,
			`INSERT INTO normalized_messages
			   (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
			    body_text, subject, sender, channel)
			 VALUES ($1,$2,$3,$4, now() - make_interval(mins => $5), $6,$7,$8,'gmail') RETURNING id`,
			rawID, threadID, m.direction, "itest-promote-"+m.label, m.minsAgo, m.body, m.subject, m.sender)
		s.msg[m.label] = id
		s.raw[m.label] = rawID
	}

	// The corpus. Every message is inbound: the classify inbox filters
	// direction='inbound' and the capture engine only ever decides inbound
	// messages, so invariant 5 holds upstream of this package (SPEC, invariant 5).
	for _, m := range []pmMsg{
		{label: "due", thread: "bank", sender: "Alerts <alerts@bank.example>",
			subject: "Your payment is due", body: "minimum payment $35 due on account ending 1234",
			direction: "inbound", minsAgo: 50},
		{label: "followup", thread: "bank", sender: "Alerts <alerts@bank.example>",
			subject: "Re: Your payment is due", body: "second notice: minimum payment $35",
			direction: "inbound", minsAgo: 20},
		{label: "info", thread: "news", sender: "HOA <hoa@example.test>",
			subject: "Community newsletter", body: "the pool is open until October",
			direction: "inbound", minsAgo: 45},
		{label: "old", thread: "oldbill", sender: "Alerts <alerts@bank.example>",
			subject: "Your payment is due", body: "classified before the cutover",
			direction: "inbound", minsAgo: 200},
		{label: "notact", thread: "quiet", sender: "Alerts <alerts@bank.example>",
			subject: "Statement available", body: "nothing to do",
			direction: "inbound", minsAgo: 40},
		{label: "errrun", thread: "broken", sender: "Alerts <alerts@bank.example>",
			subject: "Your payment is due", body: "the model call failed",
			direction: "inbound", minsAgo: 39},
		{label: "noai", thread: "bulk", sender: "Deals <deals@shop.example>",
			subject: "Ends tonight", body: "90% off everything",
			direction: "inbound", minsAgo: 38},
		{label: "residue_unmatched", thread: "resunm", sender: "Stranger <s@example.test>",
			subject: "hello", body: "no rule covers this",
			direction: "inbound", minsAgo: 37},
		{label: "residue_attributed", thread: "resattr", sender: "Alerts <alerts@bank.example>",
			subject: "Your payment is due", body: "attributed, but classified by the residue lane",
			direction: "inbound", minsAgo: 36},
		{label: "personal_unmatched", thread: "persunm", sender: "Alerts <alerts@bank.example>",
			subject: "Your payment is due", body: "personal-lane verdict over an unmatched message",
			direction: "inbound", minsAgo: 35},
		{label: "reraise", thread: "settled", sender: "Alerts <alerts@bank.example>",
			subject: "Final notice", body: "the first task for this thread was closed last month",
			direction: "inbound", minsAgo: 34},
		{label: "stale", thread: "moved", sender: "Alerts <alerts@bank.example>",
			subject: "Your payment is due", body: "re-attributed after it was classified",
			direction: "inbound", minsAgo: 33},
		{label: "claimed", thread: "crashed", sender: "Alerts <alerts@bank.example>",
			subject: "Your payment is due", body: "a promotion row already claims this message",
			direction: "inbound", minsAgo: 32},
	} {
		message(m)
	}

	// The capture decisions. 0015's CHECK makes (action='unmatched') =
	// (project_id IS NULL) a SCHEMA fact, which is the second, structural reason
	// the residue can never be promoted (criterion 3b).
	attributed := func(label string, project int64) {
		s.exec(t, ctx,
			`INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
			 VALUES ($1,'shadow','attributed',$2,'itest-promote: personal rule')`, s.msg[label], project)
	}
	unmatched := func(label string) {
		s.exec(t, ctx,
			`INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
			 VALUES ($1,'shadow','unmatched',NULL,'itest-promote: no rule')`, s.msg[label])
	}
	for _, l := range []string{"due", "followup", "info", "old", "notact", "errrun",
		"residue_attributed", "reraise", "stale", "claimed"} {
		attributed(l, s.project)
	}
	attributed("noai", s.noAIProject)
	unmatched("residue_unmatched")
	unmatched("personal_unmatched")

	// Criterion 16: `stale` was attributed to the no-ai project when it was
	// classified, then RE-attributed to the promoting project — the ordinary way
	// a rule set is tuned. The LATEST decision is what counts, and the verdict's
	// stored project_id below still names the old one.
	s.exec(t, ctx,
		`INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
		 VALUES ($1,'shadow','attributed',$2,'itest-promote: re-attributed')`,
		s.msg["stale"], s.project)

	// The verdicts.
	for _, v := range []pmVerdict{
		{message: "due", workerType: "classify", status: "ok", actionable: true,
			kind: "payment_due", title: "Pay the $35 minimum on the bank card", runAgeMins: 45},
		{message: "followup", workerType: "classify", status: "ok", actionable: true,
			kind: "payment_due", title: "Second notice: pay the $35 minimum", runAgeMins: 15},
		{message: "info", workerType: "classify", status: "ok", actionable: true,
			kind: "informational", title: "HOA newsletter mentions a pool closure date", runAgeMins: 44},
		// BEFORE the cutover (criterion 4). Everything else about it is eligible.
		{message: "old", workerType: "classify", status: "ok", actionable: true,
			kind: "payment_due", title: "An old bill classified before the cutover", runAgeMins: 120},
		{message: "notact", workerType: "classify", status: "ok", actionable: false,
			kind: "informational", title: "Statement available", runAgeMins: 43},
		{message: "errrun", workerType: "classify", status: "error", actionable: true,
			kind: "payment_due", title: "A verdict from a failed run", runAgeMins: 42},
		{message: "noai", workerType: "classify", status: "ok", actionable: true,
			kind: "payment_due", title: "A bulk-lane flag", runAgeMins: 41},
		// Criterion 3, all three shapes:
		//  - the real residue message: unmatched AND classify_residue (both bars)
		//  - residue lane over an ATTRIBUTED message: only worker_type excludes it
		//  - personal lane over an UNMATCHED message: only the project join does
		{message: "residue_unmatched", workerType: "classify_residue", status: "ok", actionable: true,
			kind: "payment_due", title: "Residue flag over an unmatched message", runAgeMins: 40},
		{message: "residue_attributed", workerType: "classify_residue", status: "ok", actionable: true,
			kind: "payment_due", title: "Residue flag over an attributed message", runAgeMins: 39},
		{message: "personal_unmatched", workerType: "classify", status: "ok", actionable: true,
			kind: "payment_due", title: "Personal flag over an unmatched message", runAgeMins: 38},
		{message: "reraise", workerType: "classify", status: "ok", actionable: true,
			kind: "deadline", title: "Final notice: respond by the 15th", runAgeMins: 30},
		{message: "stale", workerType: "classify", status: "ok", actionable: true,
			kind: "payment_due", title: "A bill on a re-attributed thread", runAgeMins: 29,
			storedProject: s.noAIProject},
		{message: "claimed", workerType: "classify", status: "ok", actionable: true,
			kind: "payment_due", title: "A bill whose promotion row already exists", runAgeMins: 28},
	} {
		s.verdict(t, ctx, v)
	}

	// Q3's second branch: the `settled` thread already carries a task, and it is
	// CLOSED. Seeded directly rather than through the executor — a fixture is not
	// a production write, and invariant 3 is asserted against the promoter's own
	// calls (audit_events with actor promote:classify), not against the harness.
	s.closedTask = s.insID(t, ctx,
		`INSERT INTO tasks (project_id, title, body, assignee_type, status, priority, source_thread_id)
		 VALUES ($1,'itest-promote: the first notice, settled','', 'human','closed',0,$2) RETURNING id`,
		s.project, s.thread["settled"])

	// Criterion 12's crash artifact: a promotion row that claims `claimed` with
	// task_id NULL, exactly what a crash between the claim and the executor call
	// leaves behind.
	s.exec(t, ctx,
		`INSERT INTO classify_promotions
		   (normalized_message_id, raw_source_item_id, ai_extraction_id, project_id, kind, action, task_id, reason)
		 VALUES ($1,$2,$3,$4,'payment_due','task',NULL,'itest-promote: crashed between the claim and create_task')`,
		s.msg["claimed"], s.raw["claimed"], s.extr["claimed"], s.project)
}

// verdict writes one ai_runs + ai_extractions pair. fields is the shape
// internal/classify/classify.go:347-360 stores, including sender/subject/
// project_id, because criteria 15 and 16 read them from THERE and never join
// back to normalized_messages for a second copy of what the model was shown.
func (s *pmSuite) verdict(t *testing.T, ctx context.Context, v pmVerdict) {
	t.Helper()
	runID := s.insID(t, ctx,
		`INSERT INTO ai_runs (worker_type, provider, model, input, output, status, created_at)
		 VALUES ($1,'itest-promote',$2, jsonb_build_object('itest','itest-promote'), '{}', $3,
		         now() - make_interval(mins => $4))
		 RETURNING id`, v.workerType, pmModel, v.status, v.runAgeMins)

	project := v.storedProject
	if project == 0 {
		project = s.project
	}
	var sender, subject string
	if err := s.pool.QueryRow(ctx,
		`SELECT sender, subject FROM normalized_messages WHERE id=$1`, s.msg[v.message]).
		Scan(&sender, &subject); err != nil {
		t.Fatalf("read message %s: %v", v.message, err)
	}
	fields := fmt.Sprintf(`{"actionable":%t,"kind":%q,"title":%q,"reason":"itest-promote fixture verdict",`+
		`"sender":%q,"subject":%q,"project_id":%d,"normalized_message_id":%d,"link_candidates":0}`,
		v.actionable, v.kind, v.title, sender, subject, project, s.msg[v.message])

	s.extr[v.message] = s.insID(t, ctx,
		`INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields)
		 VALUES ($1,$2,$3::jsonb) RETURNING id`, runID, s.raw[v.message], fields)
}

// ---- read helpers -------------------------------------------------------------

type pmPromotion struct {
	action   string
	kind     string
	project  int64
	taskID   *int64
	reason   string
	extrID   int64
	rawID    *int64
	rowCount int
}

// promotion reads the classify_promotions row for a labelled message, or reports
// its absence. Absence is a first-class outcome here: criterion 4 says a
// pre-cutover verdict leaves NO row ("absence, not a 'skipped' row").
func (s *pmSuite) promotion(t *testing.T, ctx context.Context, label string) (pmPromotion, bool) {
	t.Helper()
	var p pmPromotion
	p.rowCount = s.count(t, ctx,
		`SELECT count(*) FROM classify_promotions WHERE normalized_message_id=$1`, s.msg[label])
	if p.rowCount == 0 {
		return p, false
	}
	var reason *string
	if err := s.pool.QueryRow(ctx,
		`SELECT action, kind, project_id, task_id, reason, ai_extraction_id, raw_source_item_id
		   FROM classify_promotions WHERE normalized_message_id=$1`, s.msg[label]).
		Scan(&p.action, &p.kind, &p.project, &p.taskID, &reason, &p.extrID, &p.rawID); err != nil {
		t.Fatalf("read promotion for %s: %v", label, err)
	}
	if reason != nil {
		p.reason = *reason
	}
	return p, true
}

// taskOf returns the task a labelled message's promotion row points at.
func (s *pmSuite) taskOf(t *testing.T, ctx context.Context, label string) int64 {
	t.Helper()
	p, ok := s.promotion(t, ctx, label)
	if !ok {
		t.Fatalf("no classify_promotions row for %s", label)
	}
	if p.taskID == nil {
		t.Fatalf("classify_promotions row for %s has task_id NULL", label)
	}
	return *p.taskID
}

func (s *pmSuite) taskStatus(t *testing.T, ctx context.Context, taskID int64) (status, title string, project int64, thread *int64) {
	t.Helper()
	if err := s.pool.QueryRow(ctx,
		`SELECT status, title, project_id, source_thread_id FROM tasks WHERE id=$1`, taskID).
		Scan(&status, &title, &project, &thread); err != nil {
		t.Fatalf("read task %d: %v", taskID, err)
	}
	return
}

// ourTasks counts tasks in this suite's projects.
func (s *pmSuite) ourTasks(t *testing.T, ctx context.Context) int {
	t.Helper()
	return s.count(t, ctx,
		`SELECT count(*) FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug LIKE 'itest-promote-%')`)
}

// ourPromotions counts promotion rows over this suite's messages.
func (s *pmSuite) ourPromotions(t *testing.T, ctx context.Context) int {
	t.Helper()
	return s.count(t, ctx,
		`SELECT count(*) FROM classify_promotions p
		  WHERE p.normalized_message_id IN
		        (SELECT id FROM normalized_messages WHERE raw_source_item_id IN
		           (SELECT id FROM raw_source_items WHERE source_account_id IN
		              (SELECT id FROM source_accounts WHERE provider='`+pmProvider+`')))`)
}

// requirePromotedControl asserts that the pass DID promote the one plainly
// eligible verdict in the corpus.
//
// Every exclusion test below is an ABSENCE assertion, and an absence is only
// evidence if something was present: a promoter that does nothing at all
// satisfies "the residue was not promoted", "the pre-cutover verdict was not
// promoted" and "the crash artifact was not completed" simultaneously and
// forever. This is the repo's "fixture that proves nothing" rule applied to a
// negative — so every such test calls this first.
func (s *pmSuite) requirePromotedControl(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, ok := s.promotion(t, ctx, "due"); !ok {
		t.Fatalf("control failed: the plainly eligible payment_due verdict was not promoted, so every " +
			"absence asserted below is vacuous — a promoter that does nothing passes all of them")
	}
}

// ---- criterion 2: the inbox ----------------------------------------------------

// "ai_extractions joined to ai_runs with worker_type='classify' AND status='ok',
// fields->>'actionable'='true', joined through normalized_messages to the
// message's LATEST capture_decisions row and its project, where that project has
// ai_classify AND classify_promote_after IS NOT NULL AND the verdict's
// ai_runs.created_at >= classify_promote_after, and no classify_promotions row
// exists for the message."
//
// Each exclusion below has exactly ONE clause standing between it and the board,
// and the mutation that turns this red is named with it.
func TestPromote_Integration_InboxExcludesEveryIneligibleVerdict(t *testing.T) {
	ctx := context.Background()
	s := newPMSuite(t, ctx)
	s.run(t, ctx)
	s.requirePromotedControl(t, ctx)

	for _, tc := range []struct{ label, why, mutation string }{
		{"notact", "the verdict is actionable=false — the classifier looked and found nothing to do",
			"drop fields->>'actionable'='true'"},
		{"errrun", "the ai_run FAILED; its fields are whatever a broken call left behind",
			"drop AND status='ok'"},
		{"noai", "the project is local_only but ai_classify=false — the `bulk` shape, ~5,700 claimed " +
			"marketing messages",
			"drop AND p.ai_classify"},
	} {
		if _, ok := s.promotion(t, ctx, tc.label); ok {
			t.Errorf("message %q was promoted: %s. THE MUTATION for this case is `%s` — if you are reading "+
				"this after making it, that is the point", tc.label, tc.why, tc.mutation)
		}
	}
}

// ---- criterion 3: the residue lane cannot be promoted, twice over -------------

// "(a) worker_type='classify' excludes classify_residue rows by name. (b) The
// project join excludes them structurally: a residue message's latest decision
// is 'unmatched', and 0015's CHECK makes (action='unmatched') = (project_id IS
// NULL) a schema fact, so an inner join to projects returns zero residue rows
// without erroring (the SWT-23 lesson, restated)."
//
// Three fixtures, because two of them ISOLATE the two bars. Without them the
// pair is untestable: the real residue shape satisfies neither clause, so
// dropping EITHER one leaves every assertion green and the ticket ships with one
// bar that was never load-bearing — this repo's constant-discriminator landmine.
func TestPromote_Integration_ResidueIsExcludedByNameAndByTheProjectJoin(t *testing.T) {
	ctx := context.Background()
	s := newPMSuite(t, ctx)
	s.run(t, ctx)
	s.requirePromotedControl(t, ctx)

	// The real shape: unmatched AND classify_residue. Both bars.
	if _, ok := s.promotion(t, ctx, "residue_unmatched"); ok {
		t.Errorf("the classify_residue verdict over an UNMATCHED message produced a promotion row. The " +
			"residue lane stays shadow forever until a separate ticket argues for it (SPEC, out of " +
			"scope); it is excluded twice over and neither exclusion held")
	}
	if n := s.count(t, ctx,
		`SELECT count(*) FROM classify_promotions p
		   JOIN ai_extractions e ON e.id = p.ai_extraction_id
		   JOIN ai_runs r ON r.id = e.ai_run_id
		  WHERE r.worker_type='classify_residue'`); n != 0 {
		t.Errorf("%d promotion row(s) trace back to worker_type='classify_residue'. This is the SPEC's own "+
			"smoke query (verification protocol 4f) and its answer is 0", n)
	}

	// (a) alone: attributed to the promoting project, so the project join ADMITS
	// it. Only the worker_type name keeps it out.
	// THE MUTATION: widen to worker_type LIKE 'classify%' and this goes red.
	if _, ok := s.promotion(t, ctx, "residue_attributed"); ok {
		t.Errorf("a classify_residue verdict over an ATTRIBUTED message was promoted. Its project passes " +
			"every project-side clause, so the ONLY thing that can exclude it is worker_type='classify' " +
			"spelled exactly — `LIKE 'classify%%'` promotes the residue lane")
	}

	// (b) alone: the personal lane's own worker_type, over an UNMATCHED message.
	// Only the project join keeps it out.
	// THE MUTATION: LEFT JOIN projects instead of an inner join and this goes red.
	if _, ok := s.promotion(t, ctx, "personal_unmatched"); ok {
		t.Errorf("a worker_type='classify' verdict over an UNMATCHED message was promoted. An unmatched " +
			"decision has project_id NULL by CHECK constraint (0015), so an INNER join to projects drops " +
			"it — a LEFT join would promote it into a NULL project and the promoter would have to invent " +
			"a target")
	}
}

// ---- criterion 4: forward-only ------------------------------------------------

// "a verdict whose ai_runs.created_at precedes the project's
// classify_promote_after is never promoted, and no promotion row is written for
// it (absence, not a 'skipped' row). Test: two verdicts, one either side of the
// cutover; exactly one promotes."
//
// Q2's answer, which is the VERDICT clock and only the verdict clock: `old` is
// the older MESSAGE too, but what excludes it is when its verdict was recorded.
func TestPromote_Integration_CutoverIsTheVerdictClock(t *testing.T) {
	ctx := context.Background()
	s := newPMSuite(t, ctx)
	s.run(t, ctx)

	if p, ok := s.promotion(t, ctx, "old"); ok {
		t.Errorf("the verdict recorded BEFORE classify_promote_after was promoted (action=%q). Forward-only "+
			"is the whole shape of this ticket: no backfill of historical flags", p.action)
	}
	if _, ok := s.promotion(t, ctx, "due"); !ok {
		t.Errorf("the verdict recorded AFTER classify_promote_after was NOT promoted. Half of criterion 4 " +
			"is that exactly ONE of the pair promotes — a cutover that excludes everything is a stall " +
			"that looks like correctness")
	}
	// And the absence is an absence, not a row saying 'skipped'.
	if n := s.count(t, ctx,
		`SELECT count(*) FROM classify_promotions WHERE normalized_message_id=$1`, s.msg["old"]); n != 0 {
		t.Errorf("%d classify_promotions row(s) exist for the pre-cutover verdict. The SPEC says ABSENCE, "+
			"not a 'skipped' row: the log records decisions taken, and UNIQUE (normalized_message_id) "+
			"means a skip row would permanently block the message from ever promoting", n)
	}
}

// ---- criterion 5: promotion is OFF until a human sets the cutover -------------

// "With classify_promote_after IS NULL on every project, a full run creates
// nothing, writes no promotion rows and exits 0."
//
// The global assertion is honest here for the reason the header gives: no other
// suite sets a cutover, so with ours cleared there is no eligible project in the
// database at all.
func TestPromote_Integration_NullCutoverEverywherePromotesNothing(t *testing.T) {
	ctx := context.Background()
	s := newPMSuite(t, ctx)
	s.exec(t, ctx, `UPDATE projects SET classify_promote_after = NULL WHERE slug LIKE 'itest-promote-%'`)

	before := s.ourTasks(t, ctx)
	if _, err := promote.Run(ctx, s.pool, s.ex, promote.Config{}); err != nil {
		t.Fatalf("promote.Run with no cutover anywhere returned an error: %v — criterion 5 says it exits 0 "+
			"with a line saying no project has a cutover set. Not promoting is a stall, and a stall is "+
			"not an error", err)
	}
	// 1, not 0: seed() always plants criterion 12's crash artifact (a promotion
	// row for `claimed` with task_id NULL), and the log is append-only — the
	// promoter must not delete it. The dry-run test accounts for the same row;
	// the first cut of this assertion did not (fixed 2026-09-09).
	if n := s.ourPromotions(t, ctx); n != 1 {
		t.Errorf("%d promotion row(s) in this suite after a run with classify_promote_after NULL "+
			"everywhere, want 1 (the seeded crash artifact alone). NULL is the fail-closed side of D1: "+
			"promoting by accident fills a board with rows nobody chose", n)
	}
	if n := s.ourTasks(t, ctx); n != before {
		t.Errorf("task count in this suite's projects went %d -> %d with no cutover set; want unchanged",
			before, n)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM classify_promotions`); n != 1 {
		t.Errorf("%d classify_promotions row(s) exist globally after a run with no cutover anywhere, want "+
			"1 (this suite's seeded crash artifact). No other suite sets classify_promote_after (0021 "+
			"adds it with no default and no backfill), so more than that means the cutover is not the "+
			"gate", n)
	}
}

// ---- criterion 14 (the half that is a write ban) ------------------------------

// "--dry-run and the real path read the same rows and take the same decisions;
// dry-run performs no writes of any kind (no promotion rows either)."
func TestPromote_Integration_DryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	s := newPMSuite(t, ctx)

	before := s.ourTasks(t, ctx)
	if _, err := promote.Run(ctx, s.pool, s.ex, promote.Config{DryRun: true}); err != nil {
		t.Fatalf("promote.Run(dry-run): %v", err)
	}
	if n := s.ourPromotions(t, ctx); n != 1 {
		// 1 = the seeded crash artifact for `claimed`, which the dry run must not
		// touch either.
		t.Errorf("after a DRY RUN this suite has %d promotion row(s), want 1 (the seeded crash artifact). "+
			"Dry-run writes nothing of any kind — the verification protocol tells an operator to confirm "+
			"`SELECT count(*) FROM classify_promotions` = 0 before going live, and a dry run that "+
			"claimed messages would make that check lie", n)
	}
	if n := s.ourTasks(t, ctx); n != before {
		t.Errorf("dry run changed the task count %d -> %d", before, n)
	}

	// The control, for the same reason requirePromotedControl exists: a promoter
	// that does nothing writes nothing in dry-run too. The LIVE pass over the
	// identical inbox must produce rows — "the same rows, taken by the same
	// decisions, without the writes" is what criterion 14 actually claims.
	s.run(t, ctx)
	if n := s.ourPromotions(t, ctx); n <= 1 {
		t.Errorf("the LIVE pass over the same inbox produced %d promotion row(s) (1 = the seeded crash "+
			"artifact alone). Dry-run's write ban is only meaningful against a path that would have "+
			"written", n)
	}
}

// ---- criteria 7 + 8: the two lanes --------------------------------------------

// Whitelisted kinds become a LIVE task; every other flagged kind lands in the
// review lane as `holding` and never as `ready`.
func TestPromote_Integration_WhitelistCreatesReadyAndTheRestHold(t *testing.T) {
	ctx := context.Background()
	s := newPMSuite(t, ctx)
	s.run(t, ctx)

	// criterion 7: payment_due -> ready, in the CURRENT project, provenance set.
	p, ok := s.promotion(t, ctx, "due")
	if !ok {
		t.Fatalf("no promotion row for the payment_due verdict")
	}
	if p.action != "task" {
		t.Errorf("promotion action for a payment_due verdict = %q, want %q", p.action, "task")
	}
	if p.kind != "payment_due" {
		t.Errorf("promotion kind = %q, want payment_due (copied from the stored verdict, never re-derived)", p.kind)
	}
	if p.rawID == nil {
		t.Errorf("promotion row has raw_source_item_id NULL. Invariant 1: the promoter copies it from the " +
			"verdict so every promoted task is traceable back to the provider JSON that produced it")
	}
	taskID := s.taskOf(t, ctx, "due")
	status, _, project, thread := s.taskStatus(t, ctx, taskID)
	if status != "ready" {
		t.Errorf("task %d created from a payment_due verdict has status %q, want ready — criterion 7 says "+
			"a LIVE task on the personal board", taskID, status)
	}
	if project != s.project {
		t.Errorf("task %d landed in project %d, want %d (the message's current attribution)",
			taskID, project, s.project)
	}
	if thread == nil || *thread != s.thread["bank"] {
		t.Errorf("task %d has source_thread_id %v, want %d. task_set_source_thread is called on every task "+
			"the promoter creates — without it criterion 9's attach lookup is INERT on production data "+
			"and every follow-up creates a duplicate", taskID, thread, s.thread["bank"])
	}

	// criterion 8: informational -> holding, and never ready.
	pi, ok := s.promotion(t, ctx, "info")
	if !ok {
		t.Fatalf("no promotion row for the informational verdict; a non-whitelisted flag is still a " +
			"decision and it is still recorded")
	}
	if pi.action != "review" {
		t.Errorf("promotion action for an informational verdict = %q, want %q", pi.action, "review")
	}
	reviewTask := s.taskOf(t, ctx, "info")
	rStatus, _, rProject, _ := s.taskStatus(t, ctx, reviewTask)
	if rStatus != "holding" {
		t.Errorf("the review-lane task %d has status %q, want holding. Q1's answer: the review lane is "+
			"/tasks?project=personal&status=holding — a FILTER over the one tasks table (invariant 2), "+
			"and board.go already renders holding as the first column", reviewTask, rStatus)
	}
	if rProject != s.project {
		t.Errorf("review task %d landed in project %d, want %d — the review lane is the SAME project",
			reviewTask, rProject, s.project)
	}

	// criterion 13: both went through the executor, as promote:classify.
	if n := s.count(t, ctx,
		`SELECT count(*) FROM audit_events WHERE actor=$1 AND tool='create_task' AND status='ok'`,
		pmActor); n < 2 {
		t.Errorf("only %d ok create_task audit_events with actor %q; both the live task and the review "+
			"task go through executor.Execute (invariant 3), so 'who created this and when' is "+
			"answerable from audit_events", n, pmActor)
	}
}

// ---- criterion 9: attach to an OPEN task, create past a finished one ----------

// Both branches, on real rows. The open branch runs inside ONE pass: `due` and
// `followup` share a thread and both are eligible, so the pass must create one
// task for the oldest verdict and ATTACH the newer one to it — which is only
// possible if the inbox is ordered oldest-verdict-first and
// task_set_source_thread ran before the second decision.
func TestPromote_Integration_AttachesToTheOpenTaskOnTheThread(t *testing.T) {
	ctx := context.Background()
	s := newPMSuite(t, ctx)
	s.run(t, ctx)

	created := s.taskOf(t, ctx, "due")
	p, ok := s.promotion(t, ctx, "followup")
	if !ok {
		t.Fatalf("no promotion row for the follow-up message; an attach is a decision and it is recorded")
	}
	if p.action != "attached" {
		t.Fatalf("follow-up promotion action = %q, want %q — attach-before-create is what stops a "+
			"re-classified or follow-up message from creating a duplicate task", p.action, "attached")
	}
	if p.taskID == nil || *p.taskID != created {
		t.Errorf("follow-up promotion points at task %v, want the task the first verdict created (%d)",
			p.taskID, created)
	}
	if n := s.count(t, ctx,
		`SELECT count(*) FROM tasks WHERE source_thread_id=$1`, s.thread["bank"]); n != 1 {
		t.Errorf("%d tasks carry source_thread_id = the bank thread, want exactly 1. Criterion 9: no "+
			"second task, no status change", n)
	}
	if n := s.count(t, ctx,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='log'`, created); n != 1 {
		t.Errorf("task %d has %d 'log' events, want exactly ONE task_append_log for the attached "+
			"follow-up. task_append_log has no dedup of its own — capture's recorded reason for refusing "+
			"--all in live mode — so 'exactly one' is the only safe number", created, n)
	}
	if n := s.count(t, ctx,
		`SELECT count(*) FROM audit_events WHERE actor=$1 AND tool='task_append_log' AND status='ok'`,
		pmActor); n < 1 {
		t.Errorf("no ok task_append_log audit_events with actor %q; the attach append is an executor call "+
			"like every other write (invariant 3)", pmActor)
	}
}

// Q3's answer (b), the branch that flipped: a thread whose only task is CLOSED
// gets a NEW task, and the promotion row names the closed one.
func TestPromote_Integration_ClosedTaskOnTheThreadYieldsANewTask(t *testing.T) {
	ctx := context.Background()
	s := newPMSuite(t, ctx)
	s.run(t, ctx)

	p, ok := s.promotion(t, ctx, "reraise")
	if !ok {
		t.Fatalf("no promotion row for the message on the settled thread")
	}
	if p.action != "task" {
		t.Fatalf("promotion action = %q, want %q. A thread yields at most one OPEN task: attaching to a "+
			"closed task buries a real new obligation as a log line nobody opens (Q3 answer (b), "+
			"2026-09-09)", p.action, "task")
	}
	newTask := s.taskOf(t, ctx, "reraise")
	if newTask == s.closedTask {
		t.Fatalf("the promoter reused the CLOSED task %d instead of creating a new one", s.closedTask)
	}
	status, _, _, thread := s.taskStatus(t, ctx, newTask)
	if status != "ready" {
		t.Errorf("the re-raised deadline task %d has status %q, want ready — it fell through to the "+
			"whitelist rules (criteria 7-8), which is the whole point of the fall-through", newTask, status)
	}
	if thread == nil || *thread != s.thread["settled"] {
		t.Errorf("the new task %d has source_thread_id %v, want %d: the NEXT message on this thread must "+
			"attach to this task, not create a third", newTask, thread, s.thread["settled"])
	}
	if !strings.Contains(p.reason, strconv.FormatInt(s.closedTask, 10)) {
		t.Errorf("promotion reason %q does not record the closed task's id (%d). Criterion 9 asks for it "+
			"by name: without it, 'why is there a second task on this thread' has no answer in the row "+
			"that made the decision", p.reason, s.closedTask)
	}
	if st, _, _, _ := s.taskStatus(t, ctx, s.closedTask); st != "closed" {
		t.Errorf("the closed task %d is now %q; the promoter must not touch it — it has no verb that "+
			"reopens a task and inventing one here is a different ticket", s.closedTask, st)
	}
}

// ---- criterion 10: re-classification is a no-op --------------------------------

// "a second ai_extractions verdict for a message that already has a
// classify_promotions row produces nothing at all — no task, no log append, no
// second promotion row."
//
// Distinct from criterion 9: 9 is a DIFFERENT message on the same thread, 10 is
// the SAME message classified twice. The distinction is load-bearing because
// task_append_log has no dedup of its own, so this must be a REFUSAL, not an
// append.
func TestPromote_Integration_ReclassificationOfTheSameMessageDoesNothing(t *testing.T) {
	ctx := context.Background()
	s := newPMSuite(t, ctx)
	s.run(t, ctx)

	taskID := s.taskOf(t, ctx, "due")
	tasksBefore := s.ourTasks(t, ctx)
	promosBefore := s.ourPromotions(t, ctx)
	eventsBefore := s.count(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1`, taskID)

	// The same message, classified again — a fresh verdict, recorded now, well
	// after the cutover. Everything about it is eligible EXCEPT that the message
	// already has a promotion row.
	s.verdict(t, ctx, pmVerdict{
		message: "due", workerType: "classify", status: "ok", actionable: true,
		kind: "payment_due", title: "Pay the $35 minimum on the bank card (re-classified)", runAgeMins: 1,
	})
	s.run(t, ctx)

	if n := s.ourTasks(t, ctx); n != tasksBefore {
		t.Errorf("re-classifying an already-promoted message changed the task count %d -> %d", tasksBefore, n)
	}
	if n := s.ourPromotions(t, ctx); n != promosBefore {
		t.Errorf("re-classifying an already-promoted message wrote a second promotion row (%d -> %d). The "+
			"inbox's `no classify_promotions row exists for the message` clause is what refuses it, and "+
			"UNIQUE (normalized_message_id) is what makes the refusal structural", promosBefore, n)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1`, taskID); n != eventsBefore {
		t.Errorf("task %d gained a task_event on re-classification (%d -> %d). This must be a refusal, not "+
			"an append: task_append_log has no dedup, so 'attach the newer verdict' would add one log "+
			"line per re-classification forever", taskID, eventsBefore, n)
	}
}

// ---- criterion 11: idempotency is structural ----------------------------------

// "Running the promoter twice over the same inbox creates exactly the same rows
// as running it once", plus the index that makes it true.
func TestPromote_Integration_RunTwiceEqualsRunOnce(t *testing.T) {
	ctx := context.Background()
	s := newPMSuite(t, ctx)

	s.run(t, ctx)
	tasks1 := s.ourTasks(t, ctx)
	promos1 := s.ourPromotions(t, ctx)
	events1 := s.count(t, ctx,
		`SELECT count(*) FROM task_events WHERE task_id IN
		   (SELECT id FROM tasks WHERE project_id IN
		      (SELECT id FROM projects WHERE slug LIKE 'itest-promote-%'))`)

	s.run(t, ctx)
	if n := s.ourTasks(t, ctx); n != tasks1 {
		t.Errorf("second run changed the task count %d -> %d; the SPEC's 'usable alone' says a second run "+
			"creates nothing", tasks1, n)
	}
	if n := s.ourPromotions(t, ctx); n != promos1 {
		t.Errorf("second run changed the promotion count %d -> %d", promos1, n)
	}
	if n := s.count(t, ctx,
		`SELECT count(*) FROM task_events WHERE task_id IN
		   (SELECT id FROM tasks WHERE project_id IN
		      (SELECT id FROM projects WHERE slug LIKE 'itest-promote-%'))`); n != events1 {
		t.Errorf("second run appended task_events (%d -> %d) — a second pass must not re-append to the "+
			"tasks the first one made", events1, n)
	}

	// The structural half: the unique index, asserted by trying to break it.
	// "UNIQUE (normalized_message_id) is the whole idempotency story and it is
	// structural, not advisory" (D4).
	//
	// The control first. Inserting a "duplicate" of a row that does not exist
	// would succeed for the wrong reason and read as a missing index.
	s.requirePromotedControl(t, ctx)
	_, err := s.pool.Exec(ctx,
		`INSERT INTO classify_promotions
		   (normalized_message_id, raw_source_item_id, ai_extraction_id, project_id, kind, action, reason)
		 VALUES ($1,$2,$3,$4,'payment_due','task','itest-promote: duplicate by hand')`,
		s.msg["due"], s.raw["due"], s.extr["due"], s.project)
	if err == nil {
		t.Errorf("a SECOND classify_promotions row for the same normalized_message_id was accepted. " +
			"Criterion 11: the dedup key is a UNIQUE INDEX, not a WHERE clause someone remembers — " +
			"`ON CONFLICT (normalized_message_id) DO NOTHING RETURNING id` needs it to exist")
	} else if !strings.Contains(err.Error(), "23505") && !strings.Contains(strings.ToLower(err.Error()), "duplicate") {
		t.Errorf("the duplicate insert failed with %v, which is not a unique violation; the constraint that "+
			"rejected it is not the one criterion 11 names", err)
	}
}

// ---- criterion 12: claim-before-act -------------------------------------------

// "the classify_promotions row is inserted BEFORE the executor calls it
// describes, then updated with the resulting task_id. A crash between the two
// leaves a promotion row with task_id IS NULL (visible, diagnosable) — never a
// task nothing remembers, which a second run would duplicate."
//
// The crash is SEEDED rather than simulated: what the contract actually promises
// is that the artifact is inert and diagnosable, and that is exactly what the
// next run must do with it — nothing.
func TestPromote_Integration_ACrashArtifactIsNeverDuplicated(t *testing.T) {
	ctx := context.Background()
	s := newPMSuite(t, ctx)
	s.run(t, ctx)
	s.requirePromotedControl(t, ctx)

	p, ok := s.promotion(t, ctx, "claimed")
	if !ok {
		t.Fatalf("the seeded crash artifact for `claimed` disappeared; the promoter must not delete " +
			"promotion rows — the log is append-only (D4)")
	}
	if p.rowCount != 1 {
		t.Errorf("%d promotion rows for the crashed message, want 1. The claim is one row per message "+
			"FOREVER: whoever won it owns the action, so a later pass must not act on that message",
			p.rowCount)
	}
	if p.taskID != nil {
		t.Errorf("the crash artifact now points at task %d. A second run must NOT complete a claim it did "+
			"not make: the row is the diagnosable record that something was decided and not carried out, "+
			"and 'action=task with no task_id' is how an operator finds these", *p.taskID)
	}
	if n := s.count(t, ctx,
		`SELECT count(*) FROM tasks WHERE source_thread_id=$1`, s.thread["crashed"]); n != 0 {
		t.Errorf("%d task(s) exist on the crashed message's thread. The claim is what stops a second run "+
			"from creating the task a crashed run may or may not have created — and the ordering "+
			"(row first, executor second) is what makes the ambiguity land on the visible side", n)
	}
}

// ---- criterion 16: current attribution wins ------------------------------------

// "The target project is the message's CURRENT attribution (the latest
// capture_decisions project), not the stored fields->>'project_id'. When the two
// differ ... the promotion row's reason records both ids."
func TestPromote_Integration_CurrentAttributionBeatsTheStoredProjectID(t *testing.T) {
	ctx := context.Background()
	s := newPMSuite(t, ctx)
	s.run(t, ctx)

	// Fixture control: the two really do differ, or this test proves nothing.
	var stored int64
	if err := s.pool.QueryRow(ctx,
		`SELECT (fields->>'project_id')::bigint FROM ai_extractions WHERE id=$1`, s.extr["stale"]).
		Scan(&stored); err != nil {
		t.Fatalf("read stored project_id: %v", err)
	}
	if stored == s.project {
		t.Fatalf("fixture invalid: the stored project_id (%d) equals the current attribution (%d), so this "+
			"test would pass whichever one the promoter read", stored, s.project)
	}

	p, ok := s.promotion(t, ctx, "stale")
	if !ok {
		t.Fatalf("no promotion row for the re-attributed message")
	}
	if p.project != s.project {
		t.Errorf("promotion row project_id = %d, want %d (the LATEST capture_decisions project). Reading "+
			"the stored fields->>'project_id' puts the task in a project the rules no longer assign — "+
			"and here that project is ai_classify=false, which would never have produced the verdict "+
			"at all", p.project, s.project)
	}
	taskID := s.taskOf(t, ctx, "stale")
	if _, _, taskProject, _ := s.taskStatus(t, ctx, taskID); taskProject != s.project {
		t.Errorf("task %d landed in project %d, want %d", taskID, taskProject, s.project)
	}
	for _, want := range []int64{stored, s.project} {
		if !strings.Contains(p.reason, strconv.FormatInt(want, 10)) {
			t.Errorf("promotion reason %q does not record project id %d. Criterion 16 asks for BOTH ids: "+
				"the disagreement is the interesting fact — it says the rules moved the message after it "+
				"was classified, and a row that records only the winner hides that", p.reason, want)
		}
	}
}

// ---- criterion 17: single-instance ---------------------------------------------

// "the pass takes pg_try_advisory_lock on key 0x5157_0021 ... Losing the lock is
// an error and a non-zero exit, as `classify run` does — this is a solo CronJob,
// not a hitchhiker on a connector pass."
//
// Note this is the OPPOSITE of capture.EvaluateRules, which logs and returns
// nil. Copying the sibling's lock SHAPE (dedicated connection, explicit unlock)
// while keeping classify's lock POLICY (losing it is an error) is a deliberate
// split, so it gets its own assertion.
func TestPromote_Integration_RefusesToRunWhileTheLockIsHeld(t *testing.T) {
	ctx := context.Background()
	s := newPMSuite(t, ctx)

	// A dedicated connection holds the lock for the duration, exactly as a
	// concurrent pass would. Session-level: releasing the connection is NOT
	// enough to release it, which is the leak internal/classify/store.go's
	// TryLock comment describes.
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	var held bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, pmLockKey).Scan(&held); err != nil {
		conn.Release()
		t.Fatalf("pg_try_advisory_lock: %v", err)
	}
	if !held {
		conn.Release()
		t.Fatalf("could not take advisory lock 0x%X in the test itself; something else holds it", pmLockKey)
	}
	defer func() {
		if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, pmLockKey); err != nil {
			t.Errorf("release advisory lock: %v", err)
		}
		conn.Release()
	}()

	tasksBefore := s.ourTasks(t, ctx)
	promosBefore := s.ourPromotions(t, ctx)

	if _, err := promote.Run(ctx, s.pool, s.ex, promote.Config{}); err == nil {
		t.Errorf("promote.Run returned nil while advisory lock 0x%X was held. Criterion 17: losing the "+
			"lock is an ERROR and a non-zero exit — a solo CronJob that silently no-ops looks exactly "+
			"like an empty inbox, and 'the promoter has not run for a week' is then invisible", pmLockKey)
	}
	if n := s.ourPromotions(t, ctx); n != promosBefore {
		t.Errorf("a run that lost the lock still wrote promotion rows (%d -> %d)", promosBefore, n)
	}
	if n := s.ourTasks(t, ctx); n != tasksBefore {
		t.Errorf("a run that lost the lock still created tasks (%d -> %d)", tasksBefore, n)
	}

	// The KEY is part of the contract too, and it is asserted by this test's own
	// setup rather than by a source scan: a constant that is never TAKEN proves
	// nothing. If this test starts failing at "could not take advisory lock in
	// the test itself", something else in the repo claimed 0x5157_0021 and the
	// collision criterion 17 checked for is real.
}
