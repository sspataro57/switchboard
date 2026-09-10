//go:build integration

package classify_test

// SWT-33 criteria 9, 12, 15, 17, 18, 26 and 30, against a real database.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run ClassifyInquiry ./internal/classify/
//
// WHY THESE ARE HERE AND NOT IN inquiry_test.go. Every one of them turns on a
// value Postgres produces: `p.ai_inquiry`, the latest `capture_decisions.action`,
// `nm.direction = 'outbound'`, `nm.sent_at`, the stored `channel` in
// ai_extractions.fields. A fake store would supply the very values the query is
// meant to compute — SWT-21's sixth landmine, whose standing rule is that a
// predicate fed by a COLUMN gets its regression test here and must fail when the
// column is dropped. The mutations that must turn each assertion red are named
// inline.
//
// CROSS-POLLUTION PACT (IK, "integration suites cross-pollute"; `make
// integration` runs -p 1 for this reason). ai_runs and ai_extractions are shared
// with the links, residue, store and summary suites, and criterion 9's filter is
// GLOBAL — an inquiry pass here sees any ai_inquiry-attributed inbound message
// another suite left behind. So:
//   - every lane-level number is asserted as a DELTA (summarize before, seed,
//     summarize after); an absolute "flagged == 1" would be a flake, not a check;
//   - every row this suite writes is marked: projects `itest-swt33-%`, source
//     provider `itest-swt33-src`, ai_runs `model='itest-swt33-model'`, thread
//     keys `slack:T0ITEST33:%` / `jira:itest-swt33%`;
//   - cleanup runs at START and at END, in FK order. ai_extractions.raw_source_item_id
//     has NO cascade, so a leftover row of ours would make ANOTHER suite's
//     `DELETE FROM raw_source_items` fail inside its cleanup, which reads exactly
//     like the pact breaking. capture_decisions has TWO parent FKs and only
//     message_id cascades; both sides are deleted below.
//
// NO PRODUCTION COUNT IS FROZEN HERE (criterion 12, SWT-19's recorded rule).
// The 2026-09-10 per-workspace measurement (T0HPR78RX: 2,155 inbound / 2,393
// outbound) is what says the fold's discriminating column is not a constant; it
// belongs in the RUNBOOK with its date. This suite seeds its own outbound row
// and proves the fold READS it by MUTATING it — the measurement says the column
// has values, the mutation says the query looks at them.
//
// ANTI-DATE-ROT: every fixture row is seeded relative to now().
//
// GREENFIELD NOTE — EXPECTED RED. migration 0024 has not been applied to the
// compose db, so seeding fails on `column "ai_inquiry" of relation "projects"
// does not exist`; and classify.LaneInquiry / the new Summary counters do not
// exist, so the file compile-FAILS the integration build first. Both are the
// expected red state.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/promote"
	"github.com/sspataro57/switchboard/internal/provider"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	iqProvider      = "itest-swt33-src"
	iqArmedProject  = "itest-swt33-armed"
	iqUnarmedProj   = "itest-swt33-unarmed"
	iqModel         = "itest-swt33-model"
	iqMarker        = "swt33-inquiry"
	iqSlackKeyPfx   = "slack:T0ITEST33:"
	iqJiraKeyPfx    = "jira:itest-swt33"
	iqSubjectPrefix = "itest-swt33"
)

type iqCorpus struct {
	pool          *pgxpool.Pool
	armedProject  int64
	unarmedProjID int64

	// criterion 9's populations
	slackThreadedID   int64 // OURS: armed, inbound, attributed, rooted slack key
	slackChannelID    int64 // OURS: armed, inbound, attributed, unrooted slack key
	jiraID            int64 // OURS: armed, inbound, attributed, jira thread
	noThreadID        int64 // OURS: armed, inbound, attributed, thread_id NULL
	unarmedID         int64 // ai_inquiry=false — the control that dies if the clause is dropped
	taskDecisionID    int64 // latest decision 'task' on the armed project
	taskLogDecisionID int64 // latest decision 'task_log' on the armed project
	outboundID        int64 // our own send, re-entered through ingestion

	slackThreadedThread int64
	slackChannelThread  int64
	jiraThread          int64

	slackThreadedRaw int64
	slackChannelRaw  int64
	jiraRaw          int64
}

func iqCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN
	                (SELECT id FROM source_accounts WHERE provider='` + iqProvider + `'))`
	const projects = `(SELECT id FROM projects WHERE slug LIKE 'itest-swt33-%')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projects + `)`
	stmts := []string{
		// Our ai rows first, wherever they landed — including on OTHER suites'
		// raw items, which a GLOBAL inquiry pass will have classified.
		`DELETE FROM ai_extractions WHERE ai_run_id IN (SELECT id FROM ai_runs WHERE model='` + iqModel + `')`,
		`DELETE FROM ai_extractions WHERE ai_run_id IN
		   (SELECT id FROM ai_runs WHERE input->>'itest' = '` + iqMarker + `')`,
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM ai_runs WHERE model='` + iqModel + `'`,
		`DELETE FROM ai_runs WHERE input->>'itest' = '` + iqMarker + `'`,
		`DELETE FROM classify_promotions WHERE normalized_message_id IN
		   (SELECT id FROM normalized_messages WHERE raw_source_item_id IN ` + raws + `)`,
		`DELETE FROM capture_decisions WHERE message_id IN
		   (SELECT id FROM normalized_messages WHERE raw_source_item_id IN ` + raws + `)`,
		`DELETE FROM capture_decisions WHERE project_id IN ` + projects,
		`DELETE FROM capture_decisions WHERE task_id IN ` + tasksOf,
		`DELETE FROM external_refs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM tasks WHERE project_id IN ` + projects,
		`DELETE FROM capture_rules WHERE project_id IN ` + projects,
		`DELETE FROM projects WHERE slug LIKE 'itest-swt33-%'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		// Thread cleanup by KEY PREFIX. Legitimate and test-only: it must match
		// BOTH slack shapes (rooted and conversation-level) and this is the only
		// spelling that does — the file is exempted BY PATH in
		// internal/connector/slackweb/threadscope_test.go, never by a pattern.
		`DELETE FROM normalized_threads WHERE thread_key LIKE '` + iqSlackKeyPfx + `%'
		    OR thread_key LIKE '` + iqJiraKeyPfx + `%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN
		   (SELECT id FROM source_accounts WHERE provider='` + iqProvider + `')`,
		`DELETE FROM sync_runs WHERE source_account_id IN
		   (SELECT id FROM source_accounts WHERE provider='` + iqProvider + `')`,
		`DELETE FROM source_accounts WHERE provider='` + iqProvider + `'`,
	}
	for _, q := range stmts {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func newIQSuite(t *testing.T, ctx context.Context) *iqCorpus {
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
	iqCleanup(t, ctx, pool)
	t.Cleanup(func() { iqCleanup(t, ctx, pool) })
	return iqSeed(t, ctx, pool)
}

func iqSeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *iqCorpus {
	t.Helper()
	c := &iqCorpus{pool: pool}

	ins := func(q string, args ...any) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("insert %q: %v", q, err)
		}
		return id
	}

	account := ins(`INSERT INTO source_accounts (provider, account_email, send_enabled)
	                VALUES ($1,'itest-swt33@pg-main',false) RETURNING id`, iqProvider)

	// THE ARMED PROJECT, in `collaboratory`'s real shape and not a convenient
	// one: ai_locality='any' (criterion 10 — this lane has NO locality clause,
	// and a fixture that quietly used local_only would make the filter look
	// right while production returned nothing) and a NON-NULL client, which is
	// the out-of-scope-1 fact that keeps promotion a separate ticket.
	//
	// Every flag is named EXPLICITLY. 0016 defaulted ai_locality and 23 fixtures
	// silently started skipping; 0018 repeated it with ai_classify; 0024
	// defaults ai_inquiry to false, so a fixture that omitted it would exercise
	// nothing and PASS.
	c.armedProject = ins(`INSERT INTO projects (name, slug, client, execution, delivery,
	                                            ai_locality, ai_classify, ai_inquiry)
	                      VALUES ($1,$1,'LlamaSite','manual','dashboard','any',false,true) RETURNING id`,
		iqArmedProject)
	// The CONTROL project: identical in every respect except ai_inquiry.
	// Dropping `AND p.ai_inquiry` from the filter must make this one's message
	// appear — that is criterion 9's named mutation.
	c.unarmedProjID = ins(`INSERT INTO projects (name, slug, client, execution, delivery,
	                                             ai_locality, ai_classify, ai_inquiry)
	                       VALUES ($1,$1,'LlamaSite','manual','dashboard','any',false,false) RETURNING id`,
		iqUnarmedProj)

	msg := func(label, threadKey, channel, direction, body string, minsAgo int) (msgID, rawID, threadID int64) {
		rawID = ins(`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		             VALUES ($1,$2,'{}',$3, now()) RETURNING id`,
			account, "itest-swt33-"+label, "itest-swt33-h-"+label)
		if threadKey != "" {
			threadID = ins(`INSERT INTO normalized_threads (thread_key, subject, participants)
			                VALUES ($1,$2,'[]') RETURNING id`, threadKey, iqSubjectPrefix+" "+label)
			msgID = ins(`INSERT INTO normalized_messages
			               (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
			                body_text, subject, sender, channel)
			             VALUES ($1,$2,$3,$4, now() - make_interval(mins => $5), $6,$7,$8,$9) RETURNING id`,
				rawID, threadID, direction, "itest-swt33-ext-"+label, minsAgo, body,
				iqSubjectPrefix+"-"+label, "Dana Ruiz", channel)
			return msgID, rawID, threadID
		}
		msgID = ins(`INSERT INTO normalized_messages
		               (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		                body_text, subject, sender, channel)
		             VALUES ($1,NULL,$2,$3, now() - make_interval(mins => $4), $5,$6,$7,$8) RETURNING id`,
			rawID, direction, "itest-swt33-ext-"+label, minsAgo, body,
			iqSubjectPrefix+"-"+label, "Dana Ruiz", channel)
		return msgID, rawID, 0
	}

	// A ROOTED slack key — thread-EXACT. slackweb appends ThreadRootID to the
	// key (normalize.go:71-76), which is what makes `answered in thread` a
	// claim the data can support.
	c.slackThreadedID, c.slackThreadedRaw, c.slackThreadedThread = msg("threaded",
		iqSlackKeyPfx+"C0ITESTAA:p1757000000000100", "slack", "inbound",
		"can you confirm the rotation date for the staging key?", 90)
	// An UNROOTED slack key — the whole conversation. A later outbound here
	// only means Salvador has spoken in the channel since.
	c.slackChannelID, c.slackChannelRaw, c.slackChannelThread = msg("channel",
		iqSlackKeyPfx+"C0ITESTBB", "slack", "inbound",
		"who owns the billing export now?", 80)
	c.jiraID, c.jiraRaw, c.jiraThread = msg("jira",
		iqJiraKeyPfx+".atlassian.net:WEB-9901", "jira", "inbound",
		"should we ship this behind a flag or wait for the review?", 70)
	// No thread at all: thread_scope='none'.
	c.noThreadID, _, _ = msg("nothread", "", "gmail", "inbound",
		"quick question about the invoice schedule", 60)
	c.unarmedID, _, _ = msg("unarmed", iqSlackKeyPfx+"C0ITESTCC", "slack", "inbound",
		"a question in a project nobody armed", 50)
	c.taskDecisionID, _, _ = msg("taskdec", iqSlackKeyPfx+"C0ITESTDD", "slack", "inbound",
		"a message a rule already turned into a task", 40)
	c.taskLogDecisionID, _, _ = msg("tasklog", iqSlackKeyPfx+"C0ITESTEE", "slack", "inbound",
		"a follow-up on a thread that already produced a task", 35)
	// Our own send, re-entered through ingestion (invariant 5).
	c.outboundID, _, _ = msg("outbound", iqSlackKeyPfx+"C0ITESTFF", "slack", "outbound",
		"we are on it", 30)

	dec := func(msgID int64, action string, project *int64, extra string) {
		t.Helper()
		if project == nil {
			ins(`INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
			     VALUES ($1,'shadow','unmatched',NULL,'itest-swt33') RETURNING id`, msgID)
			return
		}
		if extra != "" {
			ins(`INSERT INTO capture_decisions (message_id, mode, action, project_id,
			                                    external_system, external_key, reason)
			     VALUES ($1,'shadow',$2,$3,'slack',$4,'itest-swt33') RETURNING id`,
				msgID, action, *project, extra)
			return
		}
		ins(`INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
		     VALUES ($1,'shadow',$2,$3,'itest-swt33') RETURNING id`, msgID, action, *project)
	}

	for _, id := range []int64{c.slackThreadedID, c.slackChannelID, c.jiraID, c.noThreadID} {
		dec(id, "attributed", &c.armedProject, "")
	}
	dec(c.unarmedID, "attributed", &c.unarmedProjID, "")
	dec(c.taskDecisionID, "task", &c.armedProject, "itest-swt33-task")
	dec(c.taskLogDecisionID, "task_log", &c.armedProject, "itest-swt33-task")
	// The outbound message gets NO decision, and it never can: the capture
	// engine reads direction='inbound' (that line IS invariant 5). See the note
	// in the first test about what this fixture can and cannot prove.

	return c
}

func iqPending(t *testing.T, ctx context.Context, c *iqCorpus) map[int64]classify.PendingMessage {
	t.Helper()
	rows, err := classify.NewStore(c.pool).PendingMessages(ctx,
		classify.Config{Lane: classify.LaneInquiry, Since: 24 * time.Hour})
	if err != nil {
		t.Fatalf("PendingMessages(inquiry): %v", err)
	}
	out := map[int64]classify.PendingMessage{}
	for _, m := range rows {
		out[m.MessageID] = m
	}
	return out
}

// ---- criterion 9: the inquiry inbox, produced by Postgres --------------------

func TestClassifyInquiry_Integration_InboxIsAttributedToAnArmedProject(t *testing.T) {
	ctx := context.Background()
	c := newIQSuite(t, ctx)
	got := iqPending(t, ctx, c)

	// OURS, and the headline of criterion 9: an ai_locality='any' ARMED
	// project's messages ARE returned. This is the assertion that dies if
	// someone copies the personal lane's `p.ai_locality = 'local_only'` clause
	// across — and the lane would then classify nothing while every other test
	// in this file still passed.
	for _, id := range []int64{c.slackThreadedID, c.slackChannelID, c.jiraID, c.noThreadID} {
		if _, ok := got[id]; !ok {
			t.Errorf("message %d is inbound, latest-decision 'attributed' to an ai_inquiry project, and "+
				"is NOT in the inquiry inbox. The armed project is ai_locality='any' ON PURPOSE "+
				"(criterion 10): this lane's filter carries NO locality clause, because its containment "+
				"is cmd/classify's nil general lane plus criterion 11's pinned class. Add an "+
				"ai_locality='local_only' clause and the lane returns zero rows on the only project it "+
				"is armed for", id)
		}
	}

	// THE MUTATION criterion 9 names: drop `AND p.ai_inquiry` and this appears.
	if _, ok := got[c.unarmedID]; ok {
		t.Errorf("message %d is attributed to a project with ai_inquiry=false and was returned as "+
			"pending. Only the ai_inquiry clause excludes it: the project is otherwise IDENTICAL to the "+
			"armed one, right down to ai_locality and the client name. If you are reading this after "+
			"mutating the query, good — that is the mutation criterion 9 exists to catch. If you are "+
			"reading it otherwise, the filter is not keyed on ai_inquiry at all", c.unarmedID)
	}

	// 'task' and 'task_log': they NAME a project, so the project join does not
	// exclude them — only `latest.action = 'attributed'` does. Without these
	// fixtures the clause is untested, and widening it to
	// IN ('attributed','task','task_log') leaves everything else green.
	for _, id := range []int64{c.taskDecisionID, c.taskLogDecisionID} {
		if _, ok := got[id]; ok {
			t.Errorf("message %d has a latest decision of 'task'/'task_log' and was returned in the "+
				"inquiry inbox. Those messages already produced a task; classifying them again "+
				"double-counts them. capture_decisions.action has exactly four values and no 'ignore' "+
				"verb, so a filter spelled `action <> 'unmatched'` catches these two by accident — spell "+
				"it as the positive", id)
		}
	}

	// Outbound. Stated as honestly as its siblings in store_integration_test.go:
	// this assertion CANNOT discriminate the `direction='inbound'` clause and is
	// not pretending to. The capture engine reads direction='inbound', so an
	// outbound message can never carry an 'attributed' decision and the decision
	// join already excludes it. The clause is written anyway so a reader need
	// not know that to trust the query, and it must NEVER be described as the
	// thing that keeps our own sends out.
	if _, ok := got[c.outboundID]; ok {
		t.Errorf("message %d is outbound and was returned in the inquiry inbox", c.outboundID)
	}

	// ---- the row itself: the thread identity comes from COLUMNS -------------
	m := got[c.slackThreadedID]
	if m.ThreadID != c.slackThreadedThread {
		t.Errorf("row ThreadID = %d, want %d — the normalized_threads row id, which is what a later "+
			"ticket hands to task_set_source_thread", m.ThreadID, c.slackThreadedThread)
	}
	if m.ThreadKey != iqSlackKeyPfx+"C0ITESTAA:p1757000000000100" {
		t.Errorf("row ThreadKey = %q, want the stored key VERBATIM. inboxSelect must project "+
			"normalized_threads.thread_key; re-deriving it later is exactly what this ticket exists to "+
			"avoid", m.ThreadKey)
	}
	if m.ExternalMessageID != "itest-swt33-ext-threaded" {
		t.Errorf("row ExternalMessageID = %q, want the stored normalized_messages.external_message_id. "+
			"For a conversation-scoped verdict it is the ONLY thing a later ticket can root a new thread "+
			"at", m.ExternalMessageID)
	}
	if m.Channel != "slack" {
		t.Errorf("row Channel = %q, want \"slack\" — criterion 30 groups every count by this value",
			m.Channel)
	}
	if m.Attribution != provider.AttrProject {
		t.Errorf("row Attribution = %v, want AttrProject: the filter requires an 'attributed' decision. "+
			"Note this is exactly WHY criterion 11's pin is needed — AttrProject + ProjectLocalOnly=false "+
			"is ClassGeneral, and cmd/classify has no general client", m.Attribution)
	}
	if m.ProjectLocalOnly {
		t.Errorf("row ProjectLocalOnly = true for an ai_locality='any' project; the projection is " +
			"COALESCE(p.ai_locality = 'local_only', false) and it must be FALSE here — that is the whole " +
			"premise of criterion 11")
	}
}

// A second pass must not re-classify what the first one recorded (the NOT EXISTS
// keyed on worker_type='classify_inquiry'), and a PERSONAL-lane extraction must
// not retire a message from THIS lane.
func TestClassifyInquiry_Integration_ExtractionDedupIsKeyedOnItsOwnWorkerType(t *testing.T) {
	ctx := context.Background()
	c := newIQSuite(t, ctx)

	// A verdict from ANOTHER lane on one of our messages: the message must
	// still be pending here.
	var alienRun int64
	if err := c.pool.QueryRow(ctx,
		`INSERT INTO ai_runs (worker_type, provider, model, input, status)
		 VALUES ('classify','fake',$1,'{"itest":"`+iqMarker+`"}'::jsonb,'ok') RETURNING id`,
		iqModel).Scan(&alienRun); err != nil {
		t.Fatalf("insert alien run: %v", err)
	}
	if _, err := c.pool.Exec(ctx,
		`INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields)
		 VALUES ($1,$2,'{"actionable":false}'::jsonb)`, alienRun, c.slackThreadedRaw); err != nil {
		t.Fatalf("insert alien extraction: %v", err)
	}
	if _, ok := iqPending(t, ctx, c)[c.slackThreadedID]; !ok {
		t.Errorf("message %d disappeared from the inquiry inbox after a PERSONAL-lane extraction was "+
			"written for its raw item. The NOT EXISTS keys on worker_type='classify_inquiry'; sharing a "+
			"value across lanes makes a message classified by one permanently invisible to the others",
			c.slackThreadedID)
	}

	// Now the lane's own pass, and the message must leave.
	local := &iqFakeClient{}
	if _, err := classify.Run(ctx, classify.NewStore(c.pool),
		provider.NewRouter(nil, local, time.Minute),
		classify.Config{Model: iqModel, MaxTokens: 512, Lane: classify.LaneInquiry,
			Since: 24 * time.Hour}); err != nil {
		t.Fatalf("classify.Run(inquiry): %v", err)
	}
	iqRequireOurFixtureWasClassified(t, ctx, c, local.calls)
	if _, ok := iqPending(t, ctx, c)[c.slackThreadedID]; ok {
		t.Errorf("message %d is STILL pending after the inquiry lane recorded a verdict for it; the "+
			"NOT EXISTS is not seeing this lane's own extractions and every pass will re-spend the GPU "+
			"on the same corpus", c.slackThreadedID)
	}
}

// ---- criterion 15: thread_scope, from the key Postgres stored ----------------

func TestClassifyInquiry_Integration_ThreadScopeIsRecordedFromTheStoredKey(t *testing.T) {
	ctx := context.Background()
	c := newIQSuite(t, ctx)

	if _, err := classify.Run(ctx, classify.NewStore(c.pool),
		provider.NewRouter(nil, &iqFakeClient{}, time.Minute),
		classify.Config{Model: iqModel, MaxTokens: 512, Lane: classify.LaneInquiry,
			Since: 24 * time.Hour}); err != nil {
		t.Fatalf("classify.Run(inquiry): %v", err)
	}

	scopes := map[int64]string{}
	keys := map[int64]string{}
	rows, err := c.pool.Query(ctx,
		`SELECT (e.fields->>'normalized_message_id')::bigint,
		        COALESCE(e.fields->>'thread_scope',''),
		        COALESCE(e.fields->>'thread_key','')
		   FROM ai_extractions e JOIN ai_runs r ON r.id = e.ai_run_id
		  WHERE r.worker_type = $1 AND r.model = $2 AND r.status='ok'
		    AND (e.fields->>'normalized_message_id') IS NOT NULL`,
		classify.LaneInquiry.WorkerType, iqModel)
	if err != nil {
		t.Fatalf("select verdicts: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var scope, key string
		if err := rows.Scan(&id, &scope, &key); err != nil {
			t.Fatalf("scan verdict: %v", err)
		}
		scopes[id], keys[id] = scope, key
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate verdicts: %v", err)
	}
	if len(scopes) == 0 {
		t.Fatalf("no inquiry verdicts were recorded at all; the assertions below would pass vacuously")
	}

	for _, tc := range []struct {
		id    int64
		scope string
		why   string
	}{
		{c.slackThreadedID, "thread",
			"a ROOTED slack key (slack:{ws}:{conv}:{root}) is thread-EXACT, so a later outbound on it is " +
				"genuinely a reply in THIS thread"},
		{c.slackChannelID, "conversation",
			"an unrooted slack key is the whole channel or DM. Measured 2026-09-10: 113 thread-exact keys " +
				"vs 83 conversation-level, and ONE conversation-level key holds 9,704 messages. Rendering " +
				"this as 'answered' would hide real open inquiries behind a channel Salvador happened to " +
				"speak in"},
		{c.jiraID, "thread",
			"non-slack channels are 'thread' (criterion 14): a jira issue key IS the thread"},
		{c.noThreadID, "none",
			"a message with thread_id NULL has no thread to reply into at all, and 'none' is that " +
				"statement as data rather than as a missing field"},
	} {
		got, ok := scopes[tc.id]
		if !ok {
			t.Errorf("message %d has no inquiry verdict; expected thread_scope=%q", tc.id, tc.scope)
			continue
		}
		if got != tc.scope {
			t.Errorf("message %d recorded thread_scope=%q, want %q (key %q) — %s",
				tc.id, got, tc.scope, keys[tc.id], tc.why)
		}
	}
	// The key is stored VERBATIM, not re-derived.
	if want := iqSlackKeyPfx + "C0ITESTAA:p1757000000000100"; keys[c.slackThreadedID] != want {
		t.Errorf("message %d recorded thread_key %q, want %q verbatim as stored at classify time",
			c.slackThreadedID, keys[c.slackThreadedID], want)
	}
}

// ---- criteria 12 + 17: the replied-since fold, and the MUTATION that proves it

// The fold's discriminating column is `normalized_messages.direction`, and Slack
// direction FAILS CLOSED per workspace on SLACK_CONNECTOR_OWN_USER_IDS. If the
// column were empty for Slack this whole fold would be a predicate whose
// discriminator is a constant in production — this repo's most repeated defect.
// Q1 MEASURED it on 2026-09-10 (T0HPR78RX: 2,155 inbound / 2,393 outbound,
// latest outbound 2026-09-09) and the counts live in the RUNBOOK, with their
// date, never as a literal here: that corpus is live and a frozen literal cries
// wolf every day a message arrives.
//
// The measurement says the column HAS VALUES. This test says the QUERY READS
// THEM, by mutating the row three ways and requiring each to move the verdict
// back to Open.
func TestClassifyInquiry_Integration_RepliedSinceFoldBitesAndIsMutable(t *testing.T) {
	ctx := context.Background()
	c := newIQSuite(t, ctx)

	before := iqSummary(t, ctx, c.pool)

	// Verdicts for the three threaded shapes, all needs_reply.
	iqSeedVerdict(t, ctx, c, c.slackThreadedID, c.slackThreadedRaw, "slack", "thread",
		c.slackThreadedThread)
	iqSeedVerdict(t, ctx, c, c.slackChannelID, c.slackChannelRaw, "slack", "conversation",
		c.slackChannelThread)
	iqSeedVerdict(t, ctx, c, c.jiraID, c.jiraRaw, "jira", "thread", c.jiraThread)

	// Before any outbound exists, all three are OPEN. This is the baseline the
	// mutations below must return to.
	got := iqSummary(t, ctx, c.pool)
	if d := got.Open - before.Open; d != 3 {
		t.Fatalf("Open moved by %d, want 3 before any outbound row exists (Answered+%d, Spoke+%d). "+
			"Without this baseline the mutation assertions cannot tell 'the fold released the verdict' "+
			"from 'the fold never held it'", d,
			got.AnsweredInThread-before.AnsweredInThread,
			got.SpokeInConversationSince-before.SpokeInConversationSince)
	}
	if d := got.Flagged - before.Flagged; d != 3 {
		t.Fatalf("Flagged moved by %d, want 3", d)
	}

	// Now the OUTBOUND rows: later than each classified message, on the same
	// thread. THE SEEDED ROW IS THE FOLD'S INPUT.
	outThreaded := iqSeedOutbound(t, ctx, c, "reply-threaded", c.slackThreadedThread, "slack", 10)
	outChannel := iqSeedOutbound(t, ctx, c, "reply-channel", c.slackChannelThread, "slack", 10)
	outJira := iqSeedOutbound(t, ctx, c, "reply-jira", c.jiraThread, "jira", 10)

	got = iqSummary(t, ctx, c.pool)
	if d := got.Open - before.Open; d != 0 {
		t.Errorf("Open moved by %d, want 0 once every thread carries a later outbound row", d)
	}
	if d := got.AnsweredInThread - before.AnsweredInThread; d != 2 {
		t.Errorf("AnsweredInThread moved by %d, want 2 (the rooted slack thread and the jira issue). "+
			"A later outbound on a THREAD-EXACT key is genuinely a reply in that thread", d)
	}
	if d := got.SpokeInConversationSince - before.SpokeInConversationSince; d != 1 {
		t.Errorf("SpokeInConversationSince moved by %d, want 1 (the unrooted slack channel). Criterion "+
			"17: the three states are NEVER COLLAPSED. On a conversation-level key a later outbound only "+
			"means Salvador has SPOKEN in the channel since — in a DM that is nearly a reply, in a busy "+
			"channel it is weak, and folding it into 'answered' hides real open inquiries", d)
	}
	if got.Flagged-before.Flagged != (got.Open-before.Open)+
		(got.AnsweredInThread-before.AnsweredInThread)+(got.SpokeInConversationSince-before.SpokeInConversationSince) {
		t.Errorf("Flagged (+%d) != Open (+%d) + AnsweredInThread (+%d) + SpokeInConversationSince (+%d). "+
			"Criterion 17 states the identity: every flagged verdict is in exactly one of the three "+
			"states, and a fourth (or a double count) makes the header line lie",
			got.Flagged-before.Flagged, got.Open-before.Open,
			got.AnsweredInThread-before.AnsweredInThread,
			got.SpokeInConversationSince-before.SpokeInConversationSince)
	}

	// ---- MUTATION 1: flip the outbound row's DIRECTION ----------------------
	//
	// The named mutation of criterion 12. If the fold does not actually read
	// `direction`, this changes nothing and the counters stay put — which is the
	// exact shape of a predicate whose discriminating column is inert.
	iqExec(t, ctx, c.pool, `UPDATE normalized_messages SET direction='inbound' WHERE id=$1`, outThreaded)
	got = iqSummary(t, ctx, c.pool)
	if d := got.Open - before.Open; d != 1 {
		t.Errorf("after flipping outbound row %d to direction='inbound', Open moved by %d, want 1.\n"+
			"THE FOLD IS NOT READING `direction`. Invariant 5's marker is the whole evidence for "+
			"'answered': an inbound reply from THEM is not an answer from US, and a fold that cannot "+
			"tell them apart reports every busy thread as handled", outThreaded, d)
	}
	iqExec(t, ctx, c.pool, `UPDATE normalized_messages SET direction='outbound' WHERE id=$1`, outThreaded)

	// ---- MUTATION 2: move the outbound row BEFORE the classified message ----
	iqExec(t, ctx, c.pool,
		`UPDATE normalized_messages SET sent_at = (SELECT sent_at FROM normalized_messages WHERE id=$2)
		   - interval '5 minutes' WHERE id=$1`, outChannel, c.slackChannelID)
	got = iqSummary(t, ctx, c.pool)
	if d := got.SpokeInConversationSince - before.SpokeInConversationSince; d != 0 {
		t.Errorf("after moving outbound row %d BEFORE the classified message, "+
			"SpokeInConversationSince moved by %d, want 0.\nTHE FOLD IS NOT COMPARING sent_at. A reply "+
			"that predates the question answers nothing, and a fold keyed on 'an outbound exists "+
			"anywhere on this thread' marks every long-running conversation answered forever", outChannel, d)
	}
	if d := got.Open - before.Open; d != 1 {
		t.Errorf("after moving outbound row %d before the classified message, Open moved by %d, want 1 — "+
			"the verdict returns to Open, it does not vanish from the counters", outChannel, d)
	}
	iqExec(t, ctx, c.pool,
		`UPDATE normalized_messages SET sent_at = now() - interval '10 minutes' WHERE id=$1`, outChannel)

	// ---- MUTATION 3: delete the outbound row entirely -----------------------
	iqExec(t, ctx, c.pool, `DELETE FROM normalized_messages WHERE id=$1`, outJira)
	got = iqSummary(t, ctx, c.pool)
	if d := got.AnsweredInThread - before.AnsweredInThread; d != 1 {
		t.Errorf("after deleting outbound row %d, AnsweredInThread moved by %d, want 1 — the jira "+
			"verdict must return to Open", outJira, d)
	}
	if d := got.Open - before.Open; d != 1 {
		t.Errorf("after deleting outbound row %d, Open moved by %d, want 1", outJira, d)
	}
}

// ---- criterion 18: the fold is READ-ONLY -------------------------------------

func TestClassifyInquiry_Integration_TheFoldWritesNothing(t *testing.T) {
	ctx := context.Background()
	c := newIQSuite(t, ctx)

	iqSeedVerdict(t, ctx, c, c.slackThreadedID, c.slackThreadedRaw, "slack", "thread", c.slackThreadedThread)
	iqSeedOutbound(t, ctx, c, "readonly-reply", c.slackThreadedThread, "slack", 10)

	snapshot := func() (int, string) {
		t.Helper()
		var n int
		var fields string
		if err := c.pool.QueryRow(ctx,
			`SELECT count(*), COALESCE(max(e.fields::text),'')
			   FROM ai_extractions e JOIN ai_runs r ON r.id = e.ai_run_id
			  WHERE r.worker_type=$1 AND r.model=$2`,
			classify.LaneInquiry.WorkerType, iqModel).Scan(&n, &fields); err != nil {
			t.Fatalf("snapshot verdicts: %v", err)
		}
		return n, fields
	}

	n0, f0 := snapshot()
	if n0 == 0 {
		t.Fatalf("POSITIVE CONTROL FAILED: no inquiry verdict rows exist; a read-only claim over an " +
			"empty table is not a claim")
	}

	first := iqSummary(t, ctx, c.pool)
	n1, f1 := snapshot()
	second := iqSummary(t, ctx, c.pool)
	n2, f2 := snapshot()

	if n1 != n0 || n2 != n0 {
		t.Errorf("the verdict row count moved across two Summarize calls (%d -> %d -> %d). Criterion 18: "+
			"the fold NEVER deletes or edits a verdict row. That is what makes it tunable without "+
			"re-running the GPU and what makes a mis-tuned rule lose no data — SWT-31's 'dismissals are "+
			"labelled data' instinct applied one lane earlier", n0, n1, n2)
	}
	if f1 != f0 || f2 != f0 {
		t.Errorf("a verdict's stored fields changed across a Summarize call:\nbefore: %s\nafter:  %s\n"+
			"The fold is a READ, and a fold that writes back its own conclusion cannot be re-tuned", f0, f2)
	}
	if first.Open != second.Open || first.AnsweredInThread != second.AnsweredInThread ||
		first.SpokeInConversationSince != second.SpokeInConversationSince ||
		first.Flagged != second.Flagged || first.Classified != second.Classified {
		t.Errorf("a second report over the same window printed different counts:\nfirst:  %+v\nsecond: %+v\n"+
			"Criterion 18: there is NO STATE. A report that changes what it says on a second read has "+
			"consumed something", first, second)
	}
}

// ---- criterion 30: the by-channel breakdown ----------------------------------

// Q2's stated cost — "a bad number will not say which message shape broke" — is
// BOUGHT BACK here rather than accepted. The breakdown is what replaces the
// rejected --channel flag, and it lives in Summarize so the CLI report and
// /funnel cannot disagree.
func TestClassifyInquiry_Integration_CountsAreBrokenDownByChannel(t *testing.T) {
	ctx := context.Background()
	c := newIQSuite(t, ctx)
	before := iqSummary(t, ctx, c.pool)

	// slack (one answered in thread), slack (one open, conversation), jira (one
	// open), gmail (one open, no thread).
	iqSeedVerdict(t, ctx, c, c.slackThreadedID, c.slackThreadedRaw, "slack", "thread", c.slackThreadedThread)
	iqSeedVerdict(t, ctx, c, c.slackChannelID, c.slackChannelRaw, "slack", "conversation", c.slackChannelThread)
	iqSeedVerdict(t, ctx, c, c.jiraID, c.jiraRaw, "jira", "thread", c.jiraThread)
	iqSeedVerdict(t, ctx, c, c.noThreadID, 0, "gmail", "none", 0)
	iqSeedOutbound(t, ctx, c, "bychannel-reply", c.slackThreadedThread, "slack", 10)

	got := iqSummary(t, ctx, c.pool)

	if got.ByChannel == nil {
		t.Fatalf("Summary.ByChannel is nil. Criterion 30: EVERY count the inquiry lane prints is broken " +
			"down BY CHANNEL — classified, flagged, open, answered-in-thread, " +
			"spoke-in-conversation-since and skipped — in classify.Summarize, so the CLI report and the " +
			"/funnel block cannot disagree")
	}
	for _, ch := range []string{"slack", "jira", "gmail"} {
		if _, ok := got.ByChannel[ch]; !ok {
			t.Errorf("Summary.ByChannel has no %q row: %+v. The armed project carries all three channels "+
				"(D5: a Jira comment asking a question and a client email asking one are the same "+
				"question as a Slack DM asking one)", ch, got.ByChannel)
		}
	}

	delta := func(ch string, pick func(classify.ChannelCounts) int) int {
		return pick(got.ByChannel[ch]) - pick(before.ByChannel[ch])
	}
	if d := delta("slack", func(c classify.ChannelCounts) int { return c.Flagged }); d != 2 {
		t.Errorf("ByChannel[slack].Flagged moved by %d, want 2", d)
	}
	if d := delta("slack", func(c classify.ChannelCounts) int { return c.AnsweredInThread }); d != 1 {
		t.Errorf("ByChannel[slack].AnsweredInThread moved by %d, want 1", d)
	}
	if d := delta("slack", func(c classify.ChannelCounts) int { return c.Open }); d != 1 {
		t.Errorf("ByChannel[slack].Open moved by %d, want 1", d)
	}
	if d := delta("jira", func(c classify.ChannelCounts) int { return c.Open }); d != 1 {
		t.Errorf("ByChannel[jira].Open moved by %d, want 1", d)
	}
	if d := delta("gmail", func(c classify.ChannelCounts) int { return c.Open }); d != 1 {
		t.Errorf("ByChannel[gmail].Open moved by %d, want 1", d)
	}

	// THE IDENTITY: the channel rows sum to the lane totals. A breakdown that
	// does not add up is worse than none — it is a second set of numbers to
	// argue with.
	sum := func(pick func(classify.ChannelCounts) int) int {
		n := 0
		for _, cc := range got.ByChannel {
			n += pick(cc)
		}
		return n
	}
	for _, tc := range []struct {
		name  string
		total int
		pick  func(classify.ChannelCounts) int
	}{
		{"Classified", got.Classified, func(c classify.ChannelCounts) int { return c.Classified }},
		{"Flagged", got.Flagged, func(c classify.ChannelCounts) int { return c.Flagged }},
		{"Open", got.Open, func(c classify.ChannelCounts) int { return c.Open }},
		{"AnsweredInThread", got.AnsweredInThread, func(c classify.ChannelCounts) int { return c.AnsweredInThread }},
		{"SpokeInConversationSince", got.SpokeInConversationSince,
			func(c classify.ChannelCounts) int { return c.SpokeInConversationSince }},
	} {
		if s := sum(tc.pick); s != tc.total {
			t.Errorf("the by-channel rows sum to %d for %s but the lane total is %d. Criterion 30: the "+
				"channel rows SUM to the lane totals — a breakdown that does not add up is a second set "+
				"of numbers nobody can reconcile, and the whole point was that a bad number should say "+
				"which message shape broke", s, tc.name, tc.total)
		}
	}

	// The channel comes from the STORED fields (Q2, recorded so it is not
	// re-litigated), never a re-join to normalized_messages for a second copy of
	// what was classified. The gmail verdict is seeded with raw_source_item_id
	// NULL for exactly this reason: a join back could only produce blanks.
	if got.ByChannel["gmail"].Classified == before.ByChannel["gmail"].Classified {
		t.Errorf("the gmail verdict (seeded with raw_source_item_id NULL) is not counted. Its `channel` " +
			"comes from ai_extractions.fields — the single deliberate exception is the replied-since " +
			"fold, which is a statement about the world NOW and must join")
	}
}

// ---- criterion 26: shadow is structural -------------------------------------

func TestClassifyInquiry_Integration_CreatesNoTasksAndStaysOutOfThePromoterInbox(t *testing.T) {
	ctx := context.Background()
	c := newIQSuite(t, ctx)

	// Arm the promoter on the SAME project, so the zero below is the
	// worker_type discrimination and not a missing cutover.
	iqExec(t, ctx, c.pool,
		`UPDATE projects SET ai_classify = true, classify_promote_after = now() - interval '1 day'
		  WHERE id = $1`, c.armedProject)

	tasksBefore := iqCount(t, ctx, c.pool, `SELECT count(*) FROM tasks`)
	promoBefore, err := promote.Run(ctx, c.pool, nil, promote.Config{DryRun: true})
	if err != nil {
		t.Fatalf("promote.Run(dry-run, baseline): %v", err)
	}

	local := &iqFakeClient{}
	if _, err := classify.Run(ctx, classify.NewStore(c.pool),
		provider.NewRouter(nil, local, time.Minute),
		classify.Config{Model: iqModel, MaxTokens: 512, Lane: classify.LaneInquiry,
			Since: 24 * time.Hour}); err != nil {
		t.Fatalf("classify.Run(inquiry): %v", err)
	}
	iqRequireOurFixtureWasClassified(t, ctx, c, local.calls)

	if got := iqCount(t, ctx, c.pool, `SELECT count(*) FROM tasks`); got != tasksBefore {
		t.Errorf("tasks moved from %d to %d across an inquiry pass. Shadow is STRUCTURAL (criterion 26): "+
			"classify.Store has NO task-write method, and going live ADDS an executor create_task call in "+
			"internal/promote rather than removing a guard here", tasksBefore, got)
	}

	promoAfter, err := promote.Run(ctx, c.pool, nil, promote.Config{DryRun: true})
	if err != nil {
		t.Fatalf("promote.Run(dry-run, after): %v", err)
	}
	if d := promoAfter.Considered - promoBefore.Considered; d != 0 {
		t.Errorf("the promoter's inbox grew by %d rows after an inquiry pass. internal/promote is "+
			"hard-scoped to worker_type='classify' BY NAME, and criterion 26's assertion is that this "+
			"remains true — not that new code enforces it. `collaboratory` has a non-NULL client, unlike "+
			"`personal`, so a promoted task WOULD appear in a worker queue (task_get_next filters "+
			"p.client = $1) and could be claimed by a Claude console. That is out of scope 1, a lifecycle "+
			"decision, not a flag", d)
	}

	// CONTROL: a PERSONAL-lane verdict on the same message IS promotable, so
	// the zero above is discrimination rather than an inert inbox.
	var personalRun int64
	if err := c.pool.QueryRow(ctx,
		`INSERT INTO ai_runs (worker_type, provider, model, input, status)
		 VALUES ('classify','fake',$1,'{"itest":"`+iqMarker+`"}'::jsonb,'ok') RETURNING id`,
		iqModel).Scan(&personalRun); err != nil {
		t.Fatalf("insert control run: %v", err)
	}
	if _, err := c.pool.Exec(ctx,
		`INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields)
		 VALUES ($1,$2,'{"actionable":true,"kind":"payment_due","title":"itest-swt33 control"}'::jsonb)`,
		personalRun, c.slackThreadedRaw); err != nil {
		t.Fatalf("insert control extraction: %v", err)
	}
	promoControl, err := promote.Run(ctx, c.pool, nil, promote.Config{DryRun: true})
	if err != nil {
		t.Fatalf("promote.Run(dry-run, control): %v", err)
	}
	if promoControl.Considered <= promoAfter.Considered {
		t.Fatalf("POSITIVE CONTROL FAILED: a worker_type='classify' actionable verdict on the SAME "+
			"message and the SAME armed project did not enter the promoter's inbox either (%d -> %d). "+
			"Without it, the zero above proves only that nothing is promotable in this fixture",
			promoAfter.Considered, promoControl.Considered)
	}
}

// ---- helpers ------------------------------------------------------------------

// iqFakeClient is the local lane, answering the INQUIRY contract. No live model,
// no network — the seam every classify suite uses.
type iqFakeClient struct{ calls int }

func (c *iqFakeClient) Describe() provider.Descriptor {
	return provider.Descriptor{Name: "ollama", Endpoint: "http://127.0.0.1:11434"}
}
func (c *iqFakeClient) Probe(_ context.Context) error { return nil }
func (c *iqFakeClient) Complete(_ context.Context, _ provider.Request) (provider.Response, error) {
	c.calls++
	return provider.Response{Raw: []byte(iqNeedsReply), Model: iqModel, LatencyMS: 10}, nil
}

func iqSummary(t *testing.T, ctx context.Context, pool *pgxpool.Pool) classify.Summary {
	t.Helper()
	// since = 0: the whole history. The window is not what this suite is about,
	// and a window bound would make the deltas depend on wall-clock timing.
	got, err := classify.Summarize(ctx, pool, 0, classify.LaneInquiry.WorkerType)
	if err != nil {
		t.Fatalf("classify.Summarize(inquiry): %v", err)
	}
	if got.ByChannel == nil {
		got.ByChannel = map[string]classify.ChannelCounts{}
	}
	return got
}

// iqSeedVerdict writes one flagged inquiry verdict directly, in the shape the
// worker records (criterion 13). Written by hand rather than by running the
// worker so the fold can be exercised on a chosen thread_scope without also
// exercising the scope decision — which criterion 15 pins separately.
func iqSeedVerdict(t *testing.T, ctx context.Context, c *iqCorpus,
	messageID, rawItemID int64, channel, scope string, threadID int64) {
	t.Helper()
	var runID int64
	if err := c.pool.QueryRow(ctx,
		`INSERT INTO ai_runs (worker_type, provider, model, input, status)
		 VALUES ($1,'ollama',$2,'{"itest":"`+iqMarker+`"}'::jsonb,'ok') RETURNING id`,
		classify.LaneInquiry.WorkerType, iqModel).Scan(&runID); err != nil {
		t.Fatalf("insert inquiry run: %v", err)
	}
	fields := fmt.Sprintf(`{"needs_reply":true,"ask_kind":"question","asker":"Dana Ruiz",
	   "ask":"itest-swt33 ask for message %d","reason":"itest","sender":"Dana Ruiz",
	   "subject":"itest-swt33","channel":%q,"project_id":%d,"project_slug":%q,
	   "normalized_message_id":%d,"thread_id":%d,"thread_key":"itest-swt33-key",
	   "thread_scope":%q,"external_message_id":"itest-swt33-ext","context_messages":2}`,
		messageID, channel, c.armedProject, iqArmedProject, messageID, threadID, scope)

	var raw any
	if rawItemID != 0 {
		raw = rawItemID
	}
	if _, err := c.pool.Exec(ctx,
		`INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields) VALUES ($1,$2,$3::jsonb)`,
		runID, raw, fields); err != nil {
		t.Fatalf("insert inquiry extraction: %v", err)
	}
}

// iqSeedOutbound writes ONE outbound message on a thread, minsAgo minutes ago —
// the fold's input, and the row the mutations above move.
func iqSeedOutbound(t *testing.T, ctx context.Context, c *iqCorpus,
	label string, threadID int64, channel string, minsAgo int) int64 {
	t.Helper()
	return iqSeedOnThread(t, ctx, c, label, threadID, channel, "outbound", minsAgo)
}

// iqSeedOnThread writes ONE message of either direction on a thread, with NO
// capture decision — a thread neighbour, never an inbox row of its own.
func iqSeedOnThread(t *testing.T, ctx context.Context, c *iqCorpus,
	label string, threadID int64, channel, direction string, minsAgo int) int64 {
	t.Helper()
	var rawID int64
	if err := c.pool.QueryRow(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ((SELECT id FROM source_accounts WHERE provider=$1),$2,'{}',$3, now()) RETURNING id`,
		iqProvider, "itest-swt33-"+label, "itest-swt33-h-"+label).Scan(&rawID); err != nil {
		t.Fatalf("insert outbound raw item: %v", err)
	}
	var id int64
	if err := c.pool.QueryRow(ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		    body_text, subject, sender, channel)
		 VALUES ($1,$2,$7,$3, now() - make_interval(mins => $4),
		         'itest-swt33 ' || $5, $5, 'Salvador', $6) RETURNING id`,
		rawID, threadID, "itest-swt33-ext-"+label, minsAgo, iqSubjectPrefix+"-"+label, channel,
		direction).Scan(&id); err != nil {
		t.Fatalf("insert %s message: %v", direction, err)
	}
	return id
}

// ---- criterion 16 against Postgres: the transcript loader's SQL --------------

// Added by the SWT-33 re-review. The unit tests hand ThreadContext in directly,
// so they cannot see the loader's SQL — an added `direction = 'inbound'` (the
// neighbours() filter, copied across) would leave every one of them green and
// strip our own replies from every transcript. IK landmine (6): a predicate fed
// by a column gets its regression test where Postgres produces the values.
func TestClassifyInquiry_Integration_TranscriptIsNewestSixPriorBothDirections(t *testing.T) {
	ctx := context.Background()
	c := newIQSuite(t, ctx)

	// The target (slackThreadedID) was sent 90 minutes ago. Eight earlier
	// messages alternate direction; one arrives after it.
	var prior []int64
	for i, mins := range []int{180, 170, 160, 150, 140, 130, 120, 110} {
		dir := "inbound"
		if i%2 == 1 {
			dir = "outbound"
		}
		prior = append(prior, iqSeedOnThread(t, ctx, c, fmt.Sprintf("prior-%d", i),
			c.slackThreadedThread, "slack", dir, mins))
	}
	later := iqSeedOnThread(t, ctx, c, "later", c.slackThreadedThread, "slack", "inbound", 5)

	m, ok := iqPending(t, ctx, c)[c.slackThreadedID]
	if !ok {
		t.Fatalf("message %d is not in the inquiry inbox; nothing below can be read", c.slackThreadedID)
	}
	var got []int64
	dirs := map[string]int{}
	for _, cm := range m.ThreadContext {
		got = append(got, cm.MessageID)
		dirs[cm.Direction]++
		if cm.MessageID == later {
			t.Errorf("the transcript carries message %d, which arrived AFTER the target. Criterion 16 / D2: "+
				"prior only, or a verdict stops being a stable property of its message", later)
		}
	}
	want := prior[2:] // the newest six, oldest first
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("transcript ids = %v, want %v — the six prior messages NEAREST the target, oldest first", got, want)
	}
	if dirs["outbound"] == 0 || dirs["inbound"] == 0 {
		t.Errorf("transcript directions = %v; BOTH must travel. A loader filtered to inbound (neighbours()' "+
			"invariant-5 rule) produces a transcript in which nothing was ever answered", dirs)
	}
}

// ---- criterion 17: the fold reads the LATEST outbound --------------------------

// Added by the SWT-33 re-review: every other fixture puts exactly one outbound
// row on a thread, so a regression from max() to min() — or to "any outbound,
// whenever" — stayed green. Here the thread carries an outbound BEFORE the
// question and one after it.
func TestClassifyInquiry_Integration_FoldReadsTheLatestOutbound(t *testing.T) {
	ctx := context.Background()
	c := newIQSuite(t, ctx)
	before := iqSummary(t, ctx, c.pool)
	// The /funnel path passes a WINDOW (since > 0), which appends a WHERE after
	// the replied-since joins; every other call here uses since = 0.
	windowed := func() classify.Summary {
		t.Helper()
		s, err := classify.Summarize(ctx, c.pool, 24*time.Hour, classify.LaneInquiry.WorkerType)
		if err != nil {
			t.Fatalf("classify.Summarize(inquiry, 24h): %v", err)
		}
		return s
	}
	beforeW := windowed()

	iqSeedVerdict(t, ctx, c, c.slackThreadedID, c.slackThreadedRaw, "slack", "thread", c.slackThreadedThread)
	// Our reply from two hours ago answers nothing asked 90 minutes ago.
	iqSeedOutbound(t, ctx, c, "early-reply", c.slackThreadedThread, "slack", 120)
	got := iqSummary(t, ctx, c.pool)
	if d := got.Open - before.Open; d != 1 {
		t.Errorf("with only an EARLIER outbound on the thread, Open moved by %d, want 1 — a reply that predates "+
			"the question answers nothing", d)
	}

	iqSeedOutbound(t, ctx, c, "late-reply", c.slackThreadedThread, "slack", 10)
	got = iqSummary(t, ctx, c.pool)
	if d := got.AnsweredInThread - before.AnsweredInThread; d != 1 {
		t.Errorf("with an earlier AND a later outbound, AnsweredInThread moved by %d, want 1. A fold keyed on "+
			"the EARLIEST outbound (min) still reads this as open", d)
	}
	if d := got.Open - before.Open; d != 0 {
		t.Errorf("with a later outbound on the thread, Open moved by %d, want 0", d)
	}
	w := windowed()
	if d := w.AnsweredInThread - beforeW.AnsweredInThread; d != 1 {
		t.Errorf("through the windowed (since=24h, the /funnel path) Summarize, AnsweredInThread moved by %d, "+
			"want 1 — the window must not detach the replied-since join", d)
	}
}

// Codex adversarial review, two rounds. Round one asked for a (sent_at, id)
// tie-break; round two showed the id is a BIGSERIAL insertion key, so a
// backfilled OLDER message can carry a HIGHER id. On a timestamp tie the fold
// therefore FAILS CLOSED: a same-instant outbound — even one with a higher id —
// does not answer the question, which stays open. Showing one extra open line
// is the safe error; hiding a real one is not.
func TestClassifyInquiry_Integration_SameInstantReplyFailsClosed(t *testing.T) {
	ctx := context.Background()
	c := newIQSuite(t, ctx)
	before := iqSummary(t, ctx, c.pool)

	iqSeedVerdict(t, ctx, c, c.slackThreadedID, c.slackThreadedRaw, "slack", "thread", c.slackThreadedThread)
	reply := iqSeedOutbound(t, ctx, c, "same-instant-reply", c.slackThreadedThread, "slack", 10)
	if reply <= c.slackThreadedID {
		t.Fatalf("POSITIVE CONTROL FAILED: the reply's id %d is not above the question's %d; this test is "+
			"about a HIGHER id not counting", reply, c.slackThreadedID)
	}
	iqExec(t, ctx, c.pool,
		`UPDATE normalized_messages SET sent_at = (SELECT sent_at FROM normalized_messages WHERE id=$2) WHERE id=$1`,
		reply, c.slackThreadedID)

	got := iqSummary(t, ctx, c.pool)
	if d := got.Open - before.Open; d != 1 {
		t.Errorf("a same-instant outbound with a HIGHER id moved Open by %d, want 1. Ties fail closed: the id "+
			"is an insertion key, not event order, and a tie-break on it can hide a real open inquiry", d)
	}
	if d := got.AnsweredInThread - before.AnsweredInThread; d != 0 {
		t.Errorf("a same-instant outbound moved AnsweredInThread by %d, want 0", d)
	}
}

// Final re-review (L2) plus the fail-closed tie rule: two outbound rows at the
// question's own instant (ids either side of it) answer nothing; a NULL-stamped
// outbound can never be "the latest" and never hides a real reply; a strictly
// later outbound answers.
func TestClassifyInquiry_Integration_FoldTiesAndNullStamps(t *testing.T) {
	ctx := context.Background()
	c := newIQSuite(t, ctx)
	before := iqSummary(t, ctx, c.pool)
	th := c.slackThreadedThread

	a := iqSeedOutbound(t, ctx, c, "tie-a", th, "slack", 50)
	q := iqSeedOnThread(t, ctx, c, "tie-q", th, "slack", "inbound", 50)
	b := iqSeedOutbound(t, ctx, c, "tie-b", th, "slack", 50)
	if !(a < q && q < b) {
		t.Fatalf("POSITIVE CONTROL FAILED: ids a=%d q=%d b=%d are not ordered a < q < b", a, q, b)
	}
	iqExec(t, ctx, c.pool,
		`UPDATE normalized_messages SET sent_at = (SELECT sent_at FROM normalized_messages WHERE id=$1)
		  WHERE id IN ($2, $3)`, q, a, b)
	iqSeedVerdict(t, ctx, c, q, 0, "slack", "thread", th)

	got := iqSummary(t, ctx, c.pool)
	if d := got.Open - before.Open; d != 1 {
		t.Errorf("two outbound rows at the question's own instant moved Open by %d, want 1 — ties fail "+
			"closed, whichever side of the question's id they fall", d)
	}

	// A NULL-stamped outbound joins: it is not "later" than anything.
	n := iqSeedOutbound(t, ctx, c, "tie-null", th, "slack", 1)
	iqExec(t, ctx, c.pool, `UPDATE normalized_messages SET sent_at = NULL WHERE id = $1`, n)
	got = iqSummary(t, ctx, c.pool)
	if d := got.Open - before.Open; d != 1 {
		t.Errorf("a NULL-stamped outbound moved Open by %d, want 1 — a row with no instant answers nothing", d)
	}

	// A strictly later reply answers — and the NULL-stamped row must not hide it.
	iqSeedOutbound(t, ctx, c, "tie-later", th, "slack", 1)
	got = iqSummary(t, ctx, c.pool)
	if d := got.AnsweredInThread - before.AnsweredInThread; d != 1 {
		t.Errorf("with a strictly later outbound (and a NULL-stamped one) on the thread, AnsweredInThread "+
			"moved by %d, want 1 — the latest instant must ignore NULL stamps", d)
	}
}

// iqRequireOurFixtureWasClassified is the positive control every "the pass did
// not do X" assertion needs, and it is scoped to THIS SUITE'S raw items on
// purpose. `local.calls > 0` alone is not enough under the cross-pollution pact:
// criterion 9's filter is global, so a pass can classify another suite's
// leftovers and report a non-zero call count while every message of the armed
// fixture was skipped — which is precisely the criterion 11 no-op the assertion
// is supposed to detect.
func iqRequireOurFixtureWasClassified(t *testing.T, ctx context.Context, c *iqCorpus, calls int) {
	t.Helper()
	ours := iqCount(t, ctx, c.pool,
		`SELECT count(*) FROM ai_extractions e
		   JOIN ai_runs r ON r.id = e.ai_run_id AND r.worker_type=$1 AND r.status='ok'
		  WHERE e.raw_source_item_id = ANY($2)`,
		classify.LaneInquiry.WorkerType,
		[]int64{c.slackThreadedRaw, c.slackChannelRaw, c.jiraRaw})
	if ours == 0 {
		t.Fatalf("POSITIVE CONTROL FAILED: the inquiry pass recorded NO verdict for any of this suite's "+
			"armed-project messages (the local client was called %d time(s), which under the "+
			"cross-pollution pact may be another suite's rows entirely; %d of ours are still pending).\n"+
			"Criterion 11: with general=nil and an ai_locality='any' project, an unpinned class fold "+
			"returns (nil, DecideSkip, no_general_provider) for EVERY message — the pass exits 0 and the "+
			"report is indistinguishable from an empty inbox. Every 'the lane did not do X' assertion "+
			"below is worthless until this passes.", calls, len(iqPending(t, ctx, c)))
	}
}

func iqExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func iqCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}
