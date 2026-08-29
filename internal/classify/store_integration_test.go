//go:build integration

package classify_test

// SWT-22 criteria 11, 12, 15 and 17, against a real database.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run ClassifyStore ./internal/classify/
//
// WHY THIS FILE CARRIES THE LOAD, stated plainly because it is the whole lesson
// of criteria 12 and 13.
//
// The predicate that actually protects this worker is criterion 11's filter:
// inbound, LATEST capture decision 'attributed', project ai_locality
// 'local_only'. `ai_locality` has exactly two values in production — 'any' on
// ~20 projects and 'local_only' on `personal` — and it is the ONLY column here
// that discriminates. The class fold does not: criterion 11 selects only
// local_only projects, so every message this worker sees is ClassRestricted by
// construction, and a unit test that supplies the class would be supplying the
// value it then asserts on.
//
// SWT-21 shipped that exact bug twice in one ticket. `drafts.DeliverTasks` never
// selected `p.ai_locality`, so the guard was inert in production while its unit
// test passed from the day it was written — because the unit test set the field
// itself. The rule since: for any predicate whose input comes from a column, the
// regression test belongs in the INTEGRATION suite and it must fail when the
// column is dropped from the SELECT. Mutate criterion 11's query to drop the
// `ai_locality` join and TestClassifyStore_Integration_InboxIsAttributedToALocalOnlyProject
// must go red on the `any` case. If it stays green, you tested your fixture.
//
// GREENFIELD NOTE: internal/classify does not exist, so this compile-FAILS under
// `-tags integration` — the expected red state.
//
// CROSS-SUITE DISCIPLINE (the mutual-cleanup pact; `make integration` runs -p 1
// for this reason). Criterion 11's filter is GLOBAL, so a classify pass here
// sees any local_only-attributed inbound message another suite left behind — and
// conversely every ai_runs/ai_extractions row this suite writes carries
// model='itest-classify-model', so its cleanup removes them wherever they landed.
// That matters because ai_extractions.raw_source_item_id has no cascade: a
// leftover row of ours would make ANOTHER suite's `DELETE FROM raw_source_items`
// fail inside cleanup, which reads exactly like the pact breaking. Cleanup runs
// at start AND end, in FK order, scoped by 'itest-classify%'.
//
// Note capture_decisions has TWO parent FKs and only ONE cascade:
// message_id is ON DELETE CASCADE, project_id and task_id are NOT. SWT-21's
// cleanup missed the project side once; the deletes below cover both.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/provider"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	ciProvider     = "itest-classify-src"
	ciLocalProject = "itest-classify-local"
	ciAnyProject   = "itest-classify-any"
	ciModel        = "itest-classify-model"
)

type ciCorpus struct {
	pool         *pgxpool.Pool
	localProject int64
	anyProject   int64

	// criterion 12's four shapes, plus two more the SPEC's wording implies
	unseenID     int64 // no capture_decisions row at all
	unmatchedID  int64 // latest decision action='unmatched' — triage's inbox
	anyAttrID    int64 // latest 'attributed' to an ai_locality='any' project
	localAttrID  int64 // latest 'attributed' to ai_locality='local_only'  <- OURS
	supersededID int64 // attributed to local_only, then re-evaluated unmatched
	taskAttrID   int64 // latest decision 'task' on local_only — a rule that CREATES tasks
	outboundID   int64 // our own send, re-entered through ingestion

	localRaw int64
}

func ciCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN
	                (SELECT id FROM source_accounts WHERE provider='` + ciProvider + `'))`
	const projects = `(SELECT id FROM projects WHERE slug LIKE 'itest-classify-%')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projects + `)`
	stmts := []string{
		// Our ai rows first, wherever they landed — including on OTHER suites'
		// raw items, which a global pass will have classified.
		`DELETE FROM ai_extractions WHERE ai_run_id IN (SELECT id FROM ai_runs WHERE model='` + ciModel + `')`,
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM ai_runs WHERE model='` + ciModel + `'`,
		// capture_decisions: BOTH parent sides. message_id cascades, project_id
		// and task_id do not.
		`DELETE FROM capture_decisions WHERE message_id IN
		   (SELECT id FROM normalized_messages WHERE raw_source_item_id IN ` + raws + `)`,
		`DELETE FROM capture_decisions WHERE project_id IN ` + projects,
		`DELETE FROM capture_decisions WHERE task_id IN ` + tasksOf,
		`DELETE FROM external_refs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM tasks WHERE project_id IN ` + projects,
		`DELETE FROM capture_rules WHERE project_id IN ` + projects,
		`DELETE FROM projects WHERE slug LIKE 'itest-classify-%'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'itest-classify:%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN
		   (SELECT id FROM source_accounts WHERE provider='` + ciProvider + `')`,
		`DELETE FROM sync_runs WHERE source_account_id IN
		   (SELECT id FROM source_accounts WHERE provider='` + ciProvider + `')`,
		`DELETE FROM source_accounts WHERE provider='` + ciProvider + `'`,
	}
	for _, q := range stmts {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func ciSeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *ciCorpus {
	t.Helper()
	c := &ciCorpus{pool: pool}

	ins := func(q string, args ...any) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("insert %q: %v", q, err)
		}
		return id
	}

	account := ins(`INSERT INTO source_accounts (provider, account_email, send_enabled)
	                VALUES ($1,'itest-classify@pg-main',false) RETURNING id`, ciProvider)

	// The two projects. ai_locality is named EXPLICITLY on both — migration 0016
	// defaults the column to 'local_only', so a fixture that omits it makes its
	// suite skip rather than fail (internal/provider/structure_test.go scans for
	// this). Here it is more than hygiene: the 'any' project IS the control.
	c.localProject = ins(`INSERT INTO projects (name, slug, client, execution, delivery, ai_locality)
	                      VALUES ($1,$1,NULL,'manual','dashboard','local_only') RETURNING id`, ciLocalProject)
	c.anyProject = ins(`INSERT INTO projects (name, slug, client, execution, delivery, ai_locality)
	                    VALUES ($1,$1,'Acme','manual','dashboard','any') RETURNING id`, ciAnyProject)

	// One thread per message: the neighbour fold is not what this suite is about,
	// and a shared thread would let a neighbour's class explain an outcome the
	// filter is supposed to explain.
	msg := func(label, sender, subject, body, direction string, minsAgo int) (msgID, rawID int64) {
		rawID = ins(`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		             VALUES ($1,$2,'{}',$3, now()) RETURNING id`,
			account, "itest-classify-"+label, "itest-classify-h-"+label)
		threadID := ins(`INSERT INTO normalized_threads (thread_key, subject, participants)
		                 VALUES ($1,$2,'[]') RETURNING id`, "itest-classify:"+label, subject)
		msgID = ins(`INSERT INTO normalized_messages
		               (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		                body_text, subject, sender, channel)
		             VALUES ($1,$2,$3,$4, now() - make_interval(mins => $5), $6,$7,$8,'gmail') RETURNING id`,
			rawID, threadID, direction, "itest-classify-"+label, minsAgo, body, subject, sender)
		return msgID, rawID
	}

	c.unseenID, _ = msg("unseen", "alerts@bank.example", "Your payment is due",
		"the capture pass has not looked at this one yet", "inbound", 60)
	c.unmatchedID, _ = msg("unmatched", "news@brand.example", "This week in widgets",
		"no rule covers this chatter", "inbound", 50)
	c.anyAttrID, _ = msg("anyattr", "client@acme.example", "Login broken on staging",
		"please fix the login bug", "inbound", 40)
	c.localAttrID, c.localRaw = msg("localattr", "alerts@bank.example", "Your payment is due",
		"minimum payment $35 due 2026-09-03 on account ending 1234", "inbound", 30)
	c.supersededID, _ = msg("superseded", "alerts@bank.example", "Your statement is available",
		"attributed once, then the rule was narrowed", "inbound", 20)
	c.taskAttrID, _ = msg("taskattr", "alerts@bank.example", "Your payment is due",
		"a personal rule that was given an external_system, so the engine created a task", "inbound", 25)
	c.outboundID, _ = msg("outbound", "me@sb.example", "re: Login broken on staging",
		"we are on it", "outbound", 10)

	// The decisions. Note the constraint the schema enforces:
	// (action='unmatched') = (project_id IS NULL), so 'attributed' always names a
	// project — which is what criterion 11's join relies on.
	ins(`INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
	     VALUES ($1,'shadow','unmatched',NULL,'itest-classify: no rule') RETURNING id`, c.unmatchedID)
	ins(`INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
	     VALUES ($1,'shadow','attributed',$2,'itest-classify: client rule') RETURNING id`,
		c.anyAttrID, c.anyProject)
	ins(`INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
	     VALUES ($1,'shadow','attributed',$2,'itest-classify: personal rule') RETURNING id`,
		c.localAttrID, c.localProject)
	// Superseded: attributed FIRST, then re-evaluated as unmatched because the
	// rule was disabled or narrowed. The ordinary way a rule set is tuned.
	ins(`INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
	     VALUES ($1,'shadow','attributed',$2,'itest-classify: personal rule') RETURNING id`,
		c.supersededID, c.localProject)
	ins(`INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
	     VALUES ($1,'shadow','unmatched',NULL,'itest-classify: rule narrowed') RETURNING id`,
		c.supersededID)
	// action='task': the shape a personal rule takes once it is given an
	// external_system. It NAMES a project, so the project join does not exclude
	// it — only `latest.action = 'attributed'` does. Without this fixture that
	// clause is untested: widening it to IN ('attributed','task','task_log')
	// leaves every other assertion in this file green.
	ins(`INSERT INTO capture_decisions (message_id, mode, action, project_id, external_system, external_key, reason)
	     VALUES ($1,'shadow','task',$2,'gmail','itest-classify-task','itest-classify: rule with a system') RETURNING id`,
		c.taskAttrID, c.localProject)

	// The outbound message gets NO decision, and it never can: the capture engine
	// reads direction='inbound' (that line IS invariant 5). See the note in the
	// first test about what this fixture can and cannot prove.

	return c
}

func newCISuite(t *testing.T, ctx context.Context) *ciCorpus {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db (cleanup deletes corpus rows); " +
			"use the compose db on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	ciCleanup(t, ctx, pool)
	t.Cleanup(func() { ciCleanup(t, ctx, pool) })
	return ciSeed(t, ctx, pool)
}

func ciPending(t *testing.T, ctx context.Context, c *ciCorpus) map[int64]classify.PendingMessage {
	t.Helper()
	rows, err := classify.NewStore(c.pool).PendingMessages(ctx, classify.Config{})
	if err != nil {
		t.Fatalf("PendingMessages: %v", err)
	}
	out := map[int64]classify.PendingMessage{}
	for _, m := range rows {
		out[m.MessageID] = m
	}
	return out
}

func ciCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

// ---- criteria 11 and 12: the population shapes, produced by Postgres ----------

func TestClassifyStore_Integration_InboxIsAttributedToALocalOnlyProject(t *testing.T) {
	ctx := context.Background()
	c := newCISuite(t, ctx)
	got := ciPending(t, ctx, c)

	// State 4 — attributed to a local_only project. OURS, and the only one.
	m, ok := got[c.localAttrID]
	if !ok {
		t.Fatalf("message %d (latest decision 'attributed' to an ai_locality='local_only' project) is NOT in "+
			"the inbox. This population is the ~1,609 messages nothing reads today: attribution took them "+
			"out of triage's inbox (which takes 'unmatched') and gave them no other consumer. If they are "+
			"not here, this worker classifies nothing", c.localAttrID)
	}

	// State 3 — attributed to an 'any' project. THE DISCRIMINATOR. This is the
	// assertion that goes red when the ai_locality join is dropped from the
	// SELECT, and it is the only one in this file that can.
	if _, ok := got[c.anyAttrID]; ok {
		t.Errorf("message %d is attributed to a project with ai_locality='any' and was returned as pending. "+
			"That is client work: already routed, possibly already carrying a task, and NOT this worker's. "+
			"If you are reading this after mutating the query, good — that is the mutation criterion 12 "+
			"exists to catch. If you are reading it otherwise, the filter is not keyed on ai_locality at all",
			c.anyAttrID)
	}

	// State 2 — unmatched. Triage's inbox, never ours.
	if _, ok := got[c.unmatchedID]; ok {
		t.Errorf("message %d has a latest decision of 'unmatched' and was returned as pending. That is "+
			"TRIAGE's inbox — the ~14,500-message residue this ticket deliberately does not classify "+
			"(SWT-23 owns it, and it needs a different prompt: triage's asks client-work questions)",
			c.unmatchedID)
	}

	// State 1 — unseen. The engine has not looked.
	if _, ok := got[c.unseenID]; ok {
		t.Errorf("message %d has NO capture_decisions row and was returned as pending. Unseen is not "+
			"attributed: this worker's inbox is an EXISTS on an 'attributed' LATEST decision, never a "+
			"NOT EXISTS over decisions generally", c.unseenID)
	}

	// LATEST means latest. A rule narrowed after an attribution takes the message
	// back out — the ordinary way a rule set is tuned.
	if _, ok := got[c.supersededID]; ok {
		t.Errorf("message %d was attributed and then re-evaluated as 'unmatched', and is still in this "+
			"worker's inbox. The filter must read the LATEST decision (ORDER BY id DESC), not any decision "+
			"that happens to name a project — otherwise a disabled rule leaves messages classified against "+
			"a project the rules no longer assign them", c.supersededID)
	}

	// action='task' on the SAME local_only project. This is the assertion that
	// makes `latest.action = 'attributed'` load-bearing rather than decorative:
	// a 'task' decision names a project, so the project join keeps it and only
	// the action clause drops it. Verified by mutation — widening the clause to
	// IN ('attributed','task','task_log') leaves every other assertion here
	// green, which is why this case exists.
	//
	// The behaviour it pins is a real operational trap: give a personal capture
	// rule an external_system and its messages start arriving as 'task', which
	// silently removes them from this worker's inbox. Excluding them is correct —
	// a message that already produced a task is not un-triaged work — but it must
	// be a decision the query states, not an accident of a join.
	if _, ok := got[c.taskAttrID]; ok {
		t.Errorf("message %d has a latest decision of 'task' on a local_only project and was returned as "+
			"pending. It already produced a task; classifying it again would double-count it. The clause "+
			"that excludes it is `latest.action = 'attributed'` — the project join does NOT, because a "+
			"'task' decision names a project", c.taskAttrID)
	}

	// The outbound message is excluded. STATED HONESTLY: this assertion cannot
	// discriminate the `direction='inbound'` clause, and it is not pretending to.
	// The capture engine only ever decides inbound messages (invariant 5), so an
	// outbound message can never carry an 'attributed' decision and is already
	// excluded by the decision join alone. Removing the direction clause would
	// leave this green. It is asserted anyway because a reader must not have to
	// know that to trust the query — but the clause must never be DESCRIBED as
	// the thing that keeps our own sends out.
	if _, ok := got[c.outboundID]; ok {
		t.Errorf("message %d is outbound and was returned as pending", c.outboundID)
	}
	if n := ciCount(t, ctx, c.pool,
		`SELECT count(*) FROM capture_decisions WHERE message_id=$1`, c.outboundID); n != 0 {
		t.Errorf("the outbound fixture carries %d capture decision(s); it is meant to represent the "+
			"structurally-undecidable case (invariant 5), and with a decision present it represents nothing "+
			"that occurs in production", n)
	}

	// The row itself carries what the worker needs, from the SAME read as the
	// body (drafts' one-query pattern — the class and the project must not be
	// able to disagree).
	if m.ProjectSlug != ciLocalProject || m.ProjectID != c.localProject {
		t.Errorf("pending row project = %d/%q, want %d/%q", m.ProjectID, m.ProjectSlug, c.localProject, ciLocalProject)
	}
	if m.Attribution != provider.AttrProject {
		t.Errorf("pending row Attribution = %v, want AttrProject", m.Attribution)
	}
	if m.BodyText == "" || m.Subject == "" || m.Sender == "" {
		t.Errorf("pending row is missing prompt inputs: sender=%q subject=%q body=%q. The spike measured "+
			"that subject+sender suffices for most senders and that the BODY is required for the templated "+
			"ones — Pines truncates its topic out of the subject entirely", m.Sender, m.Subject, m.BodyText)
	}
	if m.RawSourceItemID != c.localRaw {
		t.Errorf("pending row raw_source_item_id = %d, want %d (raw-first linkage for the extraction)",
			m.RawSourceItemID, c.localRaw)
	}
	// ProjectLocalOnly is TRUE here and it is true for every row this filter can
	// ever return — a constant in production, by construction (criterion 13).
	// Asserted so the struct is populated at all, NOT as evidence that anything
	// discriminates on it.
	if !m.ProjectLocalOnly {
		t.Errorf("pending row ProjectLocalOnly = false for a local_only project; the field is fed by the " +
			"same read as the project and must not be left at its zero value")
	}
}

// ---- criterion 11: worker_type is what keeps the three workers apart ----------

func TestClassifyStore_Integration_ATriageExtractionDoesNotHideAMessage(t *testing.T) {
	ctx := context.Background()
	c := newCISuite(t, ctx)

	if _, ok := ciPending(t, ctx, c)[c.localAttrID]; !ok {
		t.Fatalf("control failed: message %d is not pending before any extraction exists", c.localAttrID)
	}

	var triageRun int64
	if err := c.pool.QueryRow(ctx,
		`INSERT INTO ai_runs (worker_type, provider, model, status) VALUES ('triage','fake',$1,'ok') RETURNING id`,
		ciModel).Scan(&triageRun); err != nil {
		t.Fatalf("insert triage run: %v", err)
	}
	if _, err := c.pool.Exec(ctx,
		`INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields) VALUES ($1,$2,'{}')`,
		triageRun, c.localRaw); err != nil {
		t.Fatalf("insert triage extraction: %v", err)
	}

	if _, ok := ciPending(t, ctx, c)[c.localAttrID]; !ok {
		t.Errorf("message %d left this worker's inbox because TRIAGE extracted it. The NOT EXISTS must key "+
			"on ai_runs.worker_type='classify': that column is the only thing keeping the three workers' "+
			"rows from seeing each other (triage's own filter keys on 'triage', plan_import's on its own), "+
			"and without it a triage pass over the same message silently retires it here", c.localAttrID)
	}

	var classifyRun int64
	if err := c.pool.QueryRow(ctx,
		`INSERT INTO ai_runs (worker_type, provider, model, status) VALUES ('classify','fake',$1,'ok') RETURNING id`,
		ciModel).Scan(&classifyRun); err != nil {
		t.Fatalf("insert classify run: %v", err)
	}
	if _, err := c.pool.Exec(ctx,
		`INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields) VALUES ($1,$2,'{}')`,
		classifyRun, c.localRaw); err != nil {
		t.Fatalf("insert classify extraction: %v", err)
	}
	if _, ok := ciPending(t, ctx, c)[c.localAttrID]; ok {
		t.Errorf("message %d is still pending after a classify extraction was recorded for its raw item; "+
			"the pass would reclassify the same 1,609 messages forever", c.localAttrID)
	}
}

// ---- criterion 17: "did not run" and "found nothing" are different rows -------

// The operational difference, end to end and with Postgres producing every
// value:
//
//	                              | ai_runs   | ai_extractions | next pass
//	the classifier found nothing  | 'ok'      | one row        | GONE from the inbox
//	no permitted provider looked  | 'skipped' | none           | STILL in the inbox
//
// An alarm that cannot tell "did not run" from "found nothing" is the failure
// class this whole ticket is built against, and the re-appearance is the half a
// row count cannot show.
func TestClassify_Integration_SkippedStaysPending_ClassifiedLeaves(t *testing.T) {
	ctx := context.Background()
	c := newCISuite(t, ctx)

	// Invariant 2 / criterion 15: shadow is structural. Snapshot the spine.
	before := map[string]int{}
	for _, tbl := range []string{"tasks", "task_events", "deliveries", "external_refs"} {
		before[tbl] = ciCount(t, ctx, c.pool, fmt.Sprintf(`SELECT count(*) FROM %s`, tbl))
	}

	cfg := classify.Config{Model: ciModel, MaxTokens: 512}
	st := classify.NewStore(c.pool)

	// ---- lane 1: no permitted provider looked --------------------------------
	hosted := cfHosted()
	if _, err := classify.Run(ctx, st, provider.NewRouter(hosted, nil, time.Minute), cfg); err != nil {
		t.Fatalf("Run with no local lane returned an error: %v. A refusal is the boundary working: exit "+
			"zero, retry next pass", err)
	}
	if hosted.calls != 0 {
		t.Fatalf("the hosted client was called %d time(s) with restricted content. Every message in this "+
			"worker's inbox belongs to a local_only project — bank, HOA and health mail", hosted.calls)
	}
	if n := ciCount(t, ctx, c.pool,
		`SELECT count(*) FROM ai_runs WHERE worker_type='classify' AND status='skipped' AND model=$1`,
		ciModel); n == 0 {
		t.Errorf("a refused pass recorded no status='skipped' ai_runs row. Without it, 'the box was off' " +
			"renders as processed:0 and is indistinguishable from an empty inbox or a dead poller")
	}
	if n := ciCount(t, ctx, c.pool,
		`SELECT count(*) FROM ai_extractions e JOIN ai_runs r ON r.id=e.ai_run_id
		  WHERE r.worker_type='classify' AND e.raw_source_item_id=$1`, c.localRaw); n != 0 {
		t.Errorf("a skipped message wrote %d ai_extractions row(s). No skip of any kind writes one — that "+
			"is exactly what leaves the message in the inbox for the next pass", n)
	}
	if _, ok := ciPending(t, ctx, c)[c.localAttrID]; !ok {
		t.Errorf("message %d left the inbox after a pass that never looked at it. 'Nothing looked' must "+
			"leave the queue untouched, or an outage silently retires the mail it was supposed to read",
			c.localAttrID)
	}

	// ---- lane 2: the classifier looked and found nothing ---------------------
	local := cfLocal() // canned verdict: actionable=false
	if _, err := classify.Run(ctx, st, provider.NewRouter(nil, local, time.Minute), cfg); err != nil {
		t.Fatalf("Run on the local lane: %v", err)
	}
	if local.calls == 0 {
		t.Fatalf("the local client was never called; the control for this test is that the same fixture CAN " +
			"be classified — without it the 'still pending' assertion above is satisfied by a worker that " +
			"does nothing at all")
	}
	if n := ciCount(t, ctx, c.pool,
		`SELECT count(*) FROM ai_runs WHERE worker_type='classify' AND status='ok' AND model=$1`,
		ciModel); n == 0 {
		t.Errorf("no status='ok' ai_runs row after a successful pass")
	}
	if n := ciCount(t, ctx, c.pool,
		`SELECT count(*) FROM ai_extractions e JOIN ai_runs r ON r.id=e.ai_run_id
		  WHERE r.worker_type='classify' AND e.raw_source_item_id=$1`, c.localRaw); n != 1 {
		t.Errorf("the classified message has %d classify extraction(s), want exactly 1. A verdict of "+
			"actionable=false is still a VERDICT: it is recorded, and it is what removes the message from "+
			"the inbox", n)
	}
	if _, ok := ciPending(t, ctx, c)[c.localAttrID]; ok {
		t.Errorf("message %d is still pending after the classifier looked at it and answered. 'Looked and "+
			"found nothing' must retire the message, or every pass reclassifies the whole corpus",
			c.localAttrID)
	}

	// Criterion 15: zero rows added to the spine, in shadow, by either lane.
	for tbl, n := range before {
		if got := ciCount(t, ctx, c.pool, fmt.Sprintf(`SELECT count(*) FROM %s`, tbl)); got != n {
			t.Errorf("%s went from %d to %d rows. The classifier creates NOTHING: when a flagged message "+
				"becomes a task it must go through the executor's create_task with a classify: actor "+
				"(invariant 3), and nothing in this ticket may pre-empt that with a direct write", tbl, n, got)
		}
	}
}
