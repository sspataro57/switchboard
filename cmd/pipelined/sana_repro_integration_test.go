//go:build integration

package main

// REGRESSION — bug sana-email-not-captured (Jira SWT-58).
// docs/bugs/sana-email-not-captured.md (report), _REPRO.md, _DIAGNOSIS.md.
//
// CONVERTED FROM THE REPRODUCTION, DELIBERATELY. This file held the
// bug-reproducer's one test, TestSanaRepro_Integration_RoutedUnmatchedEmailBecomesHoldingTask.
// It seeded prod's situation around normalized_messages 291568 as found,
// route_after NULL on the receiving account (prod 1009) included, and asserted
// that the whole chain reached a Holding task. The diagnosis showed the chain is
// correct. The account was simply never armed: arming is a hand-run UPDATE by
// design (B-D7), and no Go code may do it. The CODE defect is that route_apply
// could not say so: routeInbox drops unarmed accounts in SQL, and its log line
// printed written=0 with every reason at 0.
//
// So the single test is split into three on the SAME fixture, varying only
// source_accounts.route_after (set by these tests; test code may arm):
//
//	(a) TestRegression_SanaEmailNotCaptured_UnarmedAccountIsCountedAndNothingRoutes
//	    route_after NULL, prod as found. THE regression for the silent log:
//	    route_apply's pass line reports an unarmed-waiting count >= 1, a line of
//	    that pass names the account, nothing routes, processed = 0 and no
//	    `routed` wake fires (hot-loop guard). Downstream stays empty.
//	(b) TestRegression_SanaEmailNotCaptured_ArmedBeforeVerdictBecomesHoldingTask
//	    The reproduction's ORIGINAL intent, kept. route_after 3h ago, before
//	    the verdict: a mode='route' row for collaboratory → a classify_inquiry
//	    verdict (fake model) → exactly one Holding task. The earlier outbound on
//	    the thread (74h before the ask) does not gate it `answered`. The log
//	    prints the unarmed counter at 0.
//	(c) TestRegression_SanaEmailNotCaptured_ArmedAfterVerdictWaitsThenReclassifies
//	    route_after = now(), after the shadow verdict: the prod path once the
//	    owner arms. The first route_apply pass counts verdict_before_arming = 1
//	    and writes no route row (unarmed 0). Then the route stage re-classifies
//	    (a fake route-shaped reply) and route_apply routes it by step `model`
//	    with the FRESH extraction. The chain then reaches one Holding task.
//
// WHAT FAILS BEFORE THE FIX: every case asserts the route_apply pass line's
// unarmed counter (any slog key containing "unarmed"; the name is the
// implementer's, DIAGNOSIS open question 4), and that key does not exist yet.
// (b)'s and (c)'s chain assertions already pass: the diagnosis verified the
// chain works once the account is armed, and they pin that it keeps working.
// The counter check reports with Errorf, so each case runs to the end.
//
// The inquiry and route stages' local model is a FAKE (sanaFakeOllama: httptest
// on 127.0.0.1; a canned schema-valid verdict per lane). No live LLM, no broker
// (a recording publisher). Run ONLY against the isolated database:
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_sanabug?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -v -run TestRegression_SanaEmailNotCaptured ./cmd/pipelined/
//
// Cleanup owns account itest-sanabug@handsonconnect.example.test, projects
// itest-sanabug-*, thread key sanaThreadKey, ai_runs models itest-sanabug-qwen3 /
// itest-fake, and audit rows of actor promote:%.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/pipeline"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	sanaAccount    = "itest-sanabug@handsonconnect.example.test"
	sanaCollab     = "itest-sanabug-collab"
	sanaReengine   = "itest-sanabug-re"
	sanaThreadKey  = "gmail:itest-sanabug@handsonconnect.example.test:<itest-sanabug-root@outlook.example.test>"
	sanaRouteModel = "itest-sanabug-qwen3"
	sanaSender     = `"Contact, Client" <client-contact@university.example.test>`
	sanaSubject    = "Question about a field in the classes payload"
)

func sanaCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE account_email='` + sanaAccount + `')`
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`
	const msgs = `(SELECT id FROM normalized_messages WHERE raw_source_item_id IN ` + raws + `)`
	const projs = `(SELECT id FROM projects WHERE slug IN ('` + sanaCollab + `','` + sanaReengine + `'))`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	const runs = `(SELECT id FROM ai_runs WHERE model IN ('` + sanaRouteModel + `','` + piqFake + `'))`
	for _, q := range []string{
		`DELETE FROM classify_promotions WHERE normalized_message_id IN ` + msgs + ` OR project_id IN ` + projs,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor LIKE 'promote:%')`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor LIKE 'promote:%'`,
		`DELETE FROM capture_decisions WHERE message_id IN ` + msgs + ` OR project_id IN ` + projs,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM source_account_projects WHERE source_account_id IN ` + accts + ` OR project_id IN ` + projs,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE thread_key='` + sanaThreadKey + `'`,
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN ` + raws + ` OR ai_run_id IN ` + runs,
		`DELETE FROM ai_runs WHERE id IN ` + runs,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE account_email='` + sanaAccount + `'`,
		`DELETE FROM projects WHERE slug IN ('` + sanaCollab + `','` + sanaReengine + `')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func sanaID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, q string, args ...any) int64 {
	t.Helper()
	var v int64
	if err := pool.QueryRow(ctx, q, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return v
}

// sanaFixture is prod's situation around 291568, route_after left NULL.
type sanaFixture struct {
	pool                    *pgxpool.Pool
	collab, reengine, acct  int64
	thread, first, msg, raw int64
	shadowExtraction        int64 // the seeded classify_route verdict (prod 11630)
	routeCalls, inqCalls    *atomic.Int32
}

// sanaFakeOllama is the local model's native API. A request whose `format`
// (the lane's JSON schema) names project_index is the ROUTE lane: it answers
// candidate 1 (collaboratory, the first source_account_projects row), quoting
// the new message's subject as evidence, which grounds it (classify.Grounded).
// Every other chat is the INQUIRY lane: fakeOllama's canned needs_reply verdict.
func sanaFakeOllama(t *testing.T, routeCalls, inqCalls *atomic.Int32) *httptest.Server {
	t.Helper()
	route, _ := json.Marshal(map[string]any{"project_index": 1, "evidence": sanaSubject, "reason": "asks about the classes payload"})
	inquiry, _ := json.Marshal(map[string]any{
		"needs_reply": true, "ask_kind": "question", "asker": "Sana",
		"ask": "the allowed values for classes[].levels", "reason": "asks the recipient directly",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{{"name": piqFake}}})
		case "/api/chat":
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Format json.RawMessage `json:"format"`
			}
			json.Unmarshal(body, &req)
			content := inquiry
			if bytes.Contains(req.Format, []byte("project_index")) {
				routeCalls.Add(1)
				content = route
			} else {
				inqCalls.Add(1)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"model": piqFake, "message": map[string]string{"role": "assistant", "content": string(content)},
				"done": true, "done_reason": "stop", "total_duration": 1000000,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sanaSetup(t *testing.T, ctx context.Context) *sanaFixture {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use an isolated compose db on :5433")
	}
	f := &sanaFixture{routeCalls: &atomic.Int32{}, inqCalls: &atomic.Int32{}}
	// Fake local model for the route and inquiry stages. localRouter reads these
	// at buildPass time, so they are set before any pass is built.
	srv := sanaFakeOllama(t, f.routeCalls, f.inqCalls)
	t.Setenv("OPS_LOCAL_PROVIDER_URL", srv.URL)
	t.Setenv("OPS_LOCAL_MODEL", piqFake)

	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	f.pool = pool
	sanaCleanup(t, ctx, pool)
	t.Cleanup(func() { sanaCleanup(t, context.Background(), pool) })

	// --- projects (prod: collaboratory id 4, with ai_inquiry true, ai_locality
	// any, ai_classify false, inquiry_promote_after 2026-09-13; reengine id 5).
	f.collab = sanaID(t, ctx, pool, `INSERT INTO projects (name, slug, client, execution, delivery, ai_locality, ai_classify,
	                                                        ai_inquiry, inquiry_promote_after)
	                                 VALUES ('Collaboratory itest',$1,'Collaboratory','manual','dashboard','any',false,true,
	                                         now() - interval '2 days') RETURNING id`, sanaCollab)
	f.reengine = sanaID(t, ctx, pool, `INSERT INTO projects (name, slug, client, execution, delivery)
	                                  VALUES ('ReEngine itest',$1,'ReEngine','manual','dashboard') RETURNING id`, sanaReengine)

	// --- receiving account (prod 1009: google, send_enabled true, route_after
	// NULL, left at its default; each test arms it or not) and its two route
	// candidates in prod's order (source_account_projects 1 and 2).
	f.acct = sanaID(t, ctx, pool, `INSERT INTO source_accounts (provider, account_email, send_enabled)
	                              VALUES ('google',$1,true) RETURNING id`, sanaAccount)
	if _, err := pool.Exec(ctx, `INSERT INTO source_account_projects (source_account_id, project_id, is_default, description)
	                             VALUES ($1,$2,true,'university partner integrations: activities, sync, request/response validation'),
	                                    ($1,$3,false,'the ReEngine platform and its LHH tickets')`, f.acct, f.collab, f.reengine); err != nil {
		t.Fatalf("seed candidates: %v", err)
	}

	// --- the thread (prod 159886): her inbound, Salvador's outbound reply, her
	// new inbound. Offsets mirror prod's spacing relative to "now".
	f.thread = sanaID(t, ctx, pool, `INSERT INTO normalized_threads (thread_key, subject, participants)
	                                VALUES ($1,'Activities Integration - Request and Response Validation','[]') RETURNING id`, sanaThreadKey)
	message := func(label, direction, sender, subject, body string, sentAgo time.Duration) (msg, raw int64) {
		t.Helper()
		raw = sanaID(t, ctx, pool, `INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		                            VALUES ($1,$2,'{}',$3, now()) RETURNING id`, f.acct, "imap:itest-sanabug:"+label, "itest-sanabug-h-"+label)
		msg = sanaID(t, ctx, pool, `INSERT INTO normalized_messages (raw_source_item_id, thread_id, direction, external_message_id,
		                                                            sent_at, body_text, subject, sender, channel)
		                            VALUES ($1,$2,$3,$4, now() - make_interval(secs => $5), $6, $7, $8, 'gmail') RETURNING id`,
			raw, f.thread, direction, "<itest-sanabug-"+label+"@outlook.example.test>", sentAgo.Seconds(), body, subject, sender)
		return msg, raw
	}
	f.first, _ = message("first", "inbound", sanaSender, "Activities Integration - Request and Response Validation",
		"Hi Salvador, the POST response does not match what we sent. Can you check?", 115*time.Hour)
	message("reply", "outbound", sanaAccount, "Re: Activities Integration - Request and Response Validation",
		"Hi Sana, I went through all three files. Everything you sent was saved.", 74*time.Hour)
	f.msg, f.raw = message("new", "inbound", sanaSender, sanaSubject,
		"Hi Salvador, thanks for the response. When we POST an activity with a 'classes' entry we get a 400: "+
			"Levels - at least one course level option must be enabled. Could you provide the allowed values for "+
			"classes[].levels, the JSON structure expected, and whether it is required? Thank you. Sana", 2*time.Hour)

	// --- capture: live `unmatched` on both inbound messages (prod 306096,
	// 390838); the outbound has no decision (capture filters inbound only).
	for _, m := range []int64{f.first, f.msg} {
		if _, err := pool.Exec(ctx, `INSERT INTO capture_decisions (message_id, mode, action, reason)
		                             VALUES ($1,'live','unmatched','no enabled rule matched')`, m); err != nil {
			t.Fatalf("seed unmatched decision: %v", err)
		}
	}

	// --- the shadow routing verdict (prod ai_runs 11734 / ai_extractions 11630),
	// recorded 110 minutes ago: the route lane picked candidate 1 =
	// collaboratory, grounded on the subject.
	routeRun := sanaID(t, ctx, pool, `INSERT INTO ai_runs (worker_type, provider, model, input, output, status, created_at)
	                                  VALUES ('classify_route','ollama',$1,
	                                          jsonb_build_object('prompt_version','route-v2','normalized_message_id',$2::bigint,
	                                                             'raw_source_item_id',$3::bigint,'thread_id',$4::bigint,'project_id',0,'project_slug',''),
	                                          jsonb_build_object('project_index',1,'evidence',$5::text,'reason','references the classes payload'),
	                                          'ok', now() - interval '110 minutes') RETURNING id`, sanaRouteModel, f.msg, f.raw, f.thread, sanaSubject)
	f.shadowExtraction = sanaID(t, ctx, pool, `INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields, created_at)
	                      VALUES ($1,$2, jsonb_build_object('reason','references the classes payload','sender',$3::text,
	                        'channel','gmail','subject',$4::text,'evidence',$4::text,'grounded',true,'candidates',2,
	                        'project_id',$5::bigint,'project_slug',$6::text,'project_index',1,
	                        'source_account_id',$7::bigint,'normalized_message_id',$8::bigint),
	                        now() - interval '110 minutes') RETURNING id`,
		routeRun, f.raw, sanaSender, sanaSubject, f.collab, sanaCollab, f.acct, f.msg)
	return f
}

// arm sets the receiving account's route_after to the SQL expression at.
func (f *sanaFixture) arm(t *testing.T, ctx context.Context, at string) {
	t.Helper()
	if _, err := f.pool.Exec(ctx, `UPDATE source_accounts SET route_after = `+at+` WHERE id = $1`, f.acct); err != nil {
		t.Fatalf("arm the fixture account: %v", err)
	}
}

// routeApply runs ONE registered route_apply pass with a recording publisher and
// its slog output captured. It returns processed, the pass line, every record
// of the pass and the publisher.
func (f *sanaFixture) routeApply(t *testing.T, ctx context.Context) (int, map[string]any, []map[string]any, *pdPub) {
	t.Helper()
	pub := &pdPub{}
	pass := registeredPass(t, pipeline.StageRouteApply, f.pool, pub)
	var n int
	var err error
	recs := captureSlog(t, func() { n, err = pass(ctx) })
	if err != nil {
		t.Fatalf("route_apply pass: %v", err)
	}
	line, ok := slogRecord(recs, "route_apply pass")
	if !ok {
		t.Fatalf("route_apply printed no \"route_apply pass\" line; records: %v", recs)
	}
	return n, line, recs, pub
}

func (f *sanaFixture) pass(t *testing.T, ctx context.Context, s pipeline.Stage) int {
	t.Helper()
	n, err := registeredPass(t, s, f.pool, nil)(ctx)
	if err != nil {
		t.Fatalf("%s pass: %v", s, err)
	}
	return n
}

// unarmedOnLine returns the pass line's unarmed-waiting counter(s), summed.
// ok=false, with a test error, if the line has none (the SWT-58 defect).
func unarmedOnLine(t *testing.T, line map[string]any) (float64, bool) {
	t.Helper()
	got := unarmedAttrs(line)
	if len(got) == 0 {
		t.Errorf("the route_apply pass line has no unarmed-waiting counter: %v. SWT-58: routeInbox drops "+
			"route_after-NULL accounts in SQL, so written=0 with every reason at 0 read the same as an empty inbox "+
			"while Sana's verdicted email waited on an account nobody armed. The line must print the count every pass, "+
			"zeros included", line)
		return 0, false
	}
	var sum float64
	for k, v := range got {
		if v < 0 {
			t.Errorf("route_apply pass %s is not a number: %v", k, line[k])
			return 0, false
		}
		sum += v
	}
	return sum, true
}

func (f *sanaFixture) routeRows(t *testing.T, ctx context.Context) (n int, project int64, step string, extraction int64) {
	t.Helper()
	if err := f.pool.QueryRow(ctx, `SELECT count(*), COALESCE(max(project_id),0), COALESCE(max(route_step),''),
	                                       COALESCE(max(ai_extraction_id),0)
	                                  FROM capture_decisions WHERE message_id=$1 AND mode='route'`, f.msg).
		Scan(&n, &project, &step, &extraction); err != nil {
		t.Fatalf("read route rows: %v", err)
	}
	return
}

func (f *sanaFixture) inquiryVerdicts(t *testing.T, ctx context.Context) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM ai_extractions e JOIN ai_runs r ON r.id=e.ai_run_id
	                                   AND r.worker_type='classify_inquiry' AND r.status='ok'
	                                 WHERE (e.fields->>'normalized_message_id')::bigint = $1`, f.msg).Scan(&n); err != nil {
		t.Fatalf("count inquiry verdicts: %v", err)
	}
	return n
}

// promotion returns the message's classify_promotions action and task, and the
// number of ready tasks in collaboratory (inquiry creates ready since 2026-09-18).
func (f *sanaFixture) promotion(t *testing.T, ctx context.Context) (action string, task *int64, ready int) {
	t.Helper()
	f.pool.QueryRow(ctx, `SELECT action, task_id FROM classify_promotions WHERE normalized_message_id=$1`, f.msg).Scan(&action, &task)
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE project_id=$1 AND status='ready'`, f.collab).Scan(&ready); err != nil {
		t.Fatalf("count ready tasks: %v", err)
	}
	return
}

// (a) route_after NULL: prod as found. THE regression for the silent log.
func TestRegression_SanaEmailNotCaptured_UnarmedAccountIsCountedAndNothingRoutes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	f := sanaSetup(t, ctx) // route_after stays NULL

	n, line, recs, pub := f.routeApply(t, ctx)

	// Hot-loop guard at the stage: the waiting message moved nothing.
	if n != 0 {
		t.Errorf("route_apply processed = %d on an unarmed account, want 0: the unarmed count must never be processed "+
			"(>= 1 publishes `routed`; a full pass re-runs at once)", n)
	}
	if len(pub.calls) != 0 {
		t.Errorf("route_apply published %d wake(s) after a pass that routed nothing: %+v", len(pub.calls), pub.calls)
	}
	if rows, _, _, _ := f.routeRows(t, ctx); rows != 0 {
		t.Errorf("%d mode='route' row(s) for a message on an unarmed account; B7: route_after NULL writes nothing", rows)
	}

	// The visibility: the count is on the line, and the pass names the account.
	if got, ok := unarmedOnLine(t, line); ok && got < 1 {
		t.Errorf("route_apply pass unarmed-waiting count = %v, want >= 1: Sana's live-unmatched email (and her earlier "+
			"one) sit on a candidate account with route_after NULL", got)
	}
	idRe := regexp.MustCompile(fmt.Sprintf(`\b%d\b`, f.acct))
	named := false
	for _, rec := range recs {
		for k, v := range rec {
			s := fmt.Sprint(v)
			if strings.Contains(s, sanaAccount) || (strings.Contains(strings.ToLower(k), "account") && idRe.MatchString(s)) {
				named = true
			}
		}
	}
	if !named {
		t.Errorf("no record of the route_apply pass names the waiting message's account (email %s, or id %d under an "+
			"*account* key); the operator has to know WHICH route_after to set. Records: %v", sanaAccount, f.acct, recs)
	}

	// Downstream stays empty: nothing reached inquiry or promote.
	f.pass(t, ctx, pipeline.StageInquiry)
	f.pass(t, ctx, pipeline.StageInquiryPromote)
	if v := f.inquiryVerdicts(t, ctx); v != 0 || f.inqCalls.Load() != 0 {
		t.Errorf("an unrouted, unarmed message got %d inquiry verdict(s) (%d model calls); want none", v, f.inqCalls.Load())
	}
	if _, task, ready := f.promotion(t, ctx); task != nil || ready != 0 {
		t.Errorf("an unrouted, unarmed message produced a task (%v, ready=%d); want none", task, ready)
	}
}

// (b) route_after 3h ago, before the 110-min-old verdict: the reproduction's
// original intent, route → inquiry → Holding.
func TestRegression_SanaEmailNotCaptured_ArmedBeforeVerdictBecomesHoldingTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	f := sanaSetup(t, ctx)
	f.arm(t, ctx, `now() - interval '3 hours'`)

	nRoute, line, _, _ := f.routeApply(t, ctx)
	if got, ok := unarmedOnLine(t, line); ok && got != 0 {
		t.Errorf("route_apply pass unarmed-waiting count = %v with the only candidate account armed, want 0 (printed)", got)
	}
	if nRoute != 1 {
		t.Errorf("route_apply processed = %d, want 1 (the new inbound; the earlier one has no verdict: pending_verdict)", nRoute)
	}
	if rows, project, step, ext := f.routeRows(t, ctx); rows != 1 || project != f.collab || step != "model" || ext != f.shadowExtraction {
		t.Errorf("STAGE route_apply: %d route row(s) (project %d, step %q, extraction %d); want 1 model row for "+
			"collaboratory (%d) naming the verdict (%d)", rows, project, step, ext, f.collab, f.shadowExtraction)
	}

	f.pass(t, ctx, pipeline.StageInquiry)
	if v := f.inquiryVerdicts(t, ctx); v != 1 {
		t.Errorf("STAGE inquiry: %d classify_inquiry verdict(s) (fake model called %d time(s)); want 1", v, f.inqCalls.Load())
	}

	f.pass(t, ctx, pipeline.StageInquiryPromote)
	action, task, ready := f.promotion(t, ctx)
	// The thread's only outbound (74h) is BEFORE the ask (2h): RepliedSinceCol is
	// strictly later, so it must not gate the ask `answered`. A task here is that proof.
	if task == nil || action != "task" || ready != 1 {
		t.Errorf("STAGE inquiry_promote: action=%q task_id=%v, ready tasks in collaboratory=%d; want task, a task id, "+
			"and exactly 1 'ready' task (an outbound BEFORE the ask must not gate it answered)", action, task, ready)
	}
}

// (c) route_after = now(), after the shadow verdict: the prod path once armed.
func TestRegression_SanaEmailNotCaptured_ArmedAfterVerdictWaitsThenReclassifies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	f := sanaSetup(t, ctx)
	f.arm(t, ctx, `now()`)

	// First route_apply pass: the shadow verdict predates arming (B-D7).
	n, line, _, pub := f.routeApply(t, ctx)
	if got, ok := unarmedOnLine(t, line); ok && got != 0 {
		t.Errorf("route_apply pass unarmed-waiting count = %v with the account armed, want 0 (printed)", got)
	}
	if v, _ := line["verdict_before_arming"].(float64); v != 1 {
		t.Errorf("route_apply pass verdict_before_arming = %v, want 1 (the shadow verdict is 110 min older than route_after)",
			line["verdict_before_arming"])
	}
	if n != 0 || len(pub.calls) != 0 {
		t.Errorf("first route_apply pass processed = %d with %d wake(s), want 0 and none", n, len(pub.calls))
	}
	if rows, _, _, _ := f.routeRows(t, ctx); rows != 0 {
		t.Errorf("a pre-arming verdict was applied: %d route row(s); B7 says never", rows)
	}

	// The route stage re-classifies it: after arming, only a verdict at or after
	// route_after is current (inboxWhereRoute), so the message is back in its inbox.
	f.pass(t, ctx, pipeline.StageRoute)
	if f.routeCalls.Load() == 0 {
		t.Fatalf("the route stage never called the (fake) local model; the armed message was not re-classified")
	}
	var fresh int64
	if err := f.pool.QueryRow(ctx, `SELECT COALESCE(max(e.id),0) FROM ai_extractions e
	                                 JOIN ai_runs r ON r.id=e.ai_run_id AND r.worker_type='classify_route' AND r.status='ok'
	                                WHERE e.raw_source_item_id=$1 AND e.id <> $2`, f.raw, f.shadowExtraction).Scan(&fresh); err != nil {
		t.Fatalf("read the fresh route verdict: %v", err)
	}
	if fresh == 0 {
		t.Fatalf("the route stage wrote no fresh classify_route verdict for the message")
	}

	f.routeApply(t, ctx)
	if rows, project, step, ext := f.routeRows(t, ctx); rows != 1 || project != f.collab || step != "model" || ext != fresh {
		t.Errorf("STAGE route_apply after re-classification: %d route row(s) (project %d, step %q, extraction %d); want "+
			"1 model row for collaboratory (%d) naming the FRESH verdict %d, never the shadow one %d",
			rows, project, step, ext, f.collab, fresh, f.shadowExtraction)
	}

	f.pass(t, ctx, pipeline.StageInquiry)
	if v := f.inquiryVerdicts(t, ctx); v != 1 {
		t.Errorf("STAGE inquiry: %d classify_inquiry verdict(s) (inquiry model called %d time(s)); want 1", v, f.inqCalls.Load())
	}
	f.pass(t, ctx, pipeline.StageInquiryPromote)
	if action, task, ready := f.promotion(t, ctx); task == nil || action != "task" || ready != 1 {
		t.Errorf("STAGE inquiry_promote: action=%q task_id=%v, ready tasks in collaboratory=%d; want task, a task id, "+
			"and exactly 1 'ready' task", action, task, ready)
	}
}
