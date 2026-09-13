//go:build integration

package main

// SWT-40 Part C wiring against the database and the compose broker
// (docs/tickets/inquiry-promote_SPEC.md, C-D1, C10, C13). Exercises the
// REGISTERED passes — stageImpls[inquiry|inquiry_promote] through buildPass —
// exactly as run() builds them.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_isoc?sslmode=disable \
//	MQTT_BROKER=tcp://localhost:1884 \
//	  go test -tags integration -p 1 -count=1 -run PipelinedInquiry ./cmd/pipelined/
//
//   - lock 0x5157_0022 held → the inquiry pass returns pipeline.ErrLockHeld;
//   - lock 0x5157_0021 held → the inquiry_promote pass returns pipeline.ErrLockHeld;
//   - inquiry_promote's processed counts verdicts ACTED ON, never gated ones
//     (a gated verdict stays in the inbox; re-counting it would make the loop
//     re-run at once, faster than the sweep);
//   - C13 end to end: a `captured` publish wakes the inquiry stage, which
//     writes a verdict (a FAKE local model: an httptest ollama returning a
//     canned schema-valid answer — never a live LLM) and publishes
//     `inquiry_classified`; inquiry_promote leaves it pending inside the grace;
//     once the fixture's sent_at moves past the grace, the SWEEP promotes it
//     into a holding task.
//
// No heartbeats are published (StageConfig.Status nil), so no retained topic
// is left behind; DialStage's retained LWT fires only on an unclean drop.
//
// GREENFIELD NOTE — EXPECTED RED: buildPass and the inquiry stageImpls entries
// do not exist (compile failure), then 0031 is missing (setup failure).
//
// Cleanup owns project itest-pipeinq, provider itest-pipeinq-src, threads with
// subject 'itest-pipeinq', ai_runs models itest-pipeinq-model / itest-fake.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/fleet"
	"github.com/sspataro57/switchboard/internal/pipeline"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	piqSlug     = "itest-pipeinq"
	piqProvider = "itest-pipeinq-src"
	piqModel    = "itest-pipeinq-model"
	piqFake     = "itest-fake"
	piqSubject  = "itest-pipeinq"
	piqGPULock  = int64(0x5157_0022)
	piqProLock  = int64(0x5157_0021)
)

type piqFixture struct {
	pool    *pgxpool.Pool
	project int64
	account int64
	thread  int64
}

func piqSetup(t *testing.T, ctx context.Context) *piqFixture {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use an isolated compose db on :5433")
	}
	t.Setenv("OPS_LOCAL_PROVIDER_URL", "")
	t.Setenv("OPS_LOCAL_MODEL", "")
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	piqCleanup(t, ctx, pool)
	t.Cleanup(func() { piqCleanup(t, context.Background(), pool) })
	f := &piqFixture{pool: pool}
	f.project = f.id(t, ctx, `INSERT INTO projects (name, slug, client, execution, delivery, ai_locality, ai_classify,
	                                                ai_inquiry, inquiry_promote_after)
	                          VALUES ($1,$1,'LlamaSite','manual','dashboard','any',false,true, now() - interval '1 hour')
	                          RETURNING id`, piqSlug)
	f.account = f.id(t, ctx, `INSERT INTO source_accounts (provider, account_email, send_enabled)
	                          VALUES ($1,'itest-pipeinq@pg-main',false) RETURNING id`, piqProvider)
	f.thread = f.id(t, ctx, `INSERT INTO normalized_threads (thread_key, subject, participants)
	                         VALUES ('gmail:itest-pipeinq:1',$1,'[]') RETURNING id`, piqSubject)
	return f
}

func piqCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE provider='` + piqProvider + `')`
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`
	const msgs = `(SELECT id FROM normalized_messages WHERE raw_source_item_id IN ` + raws + `)`
	const projs = `(SELECT id FROM projects WHERE slug='` + piqSlug + `')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	for _, q := range []string{
		`DELETE FROM classify_promotions WHERE normalized_message_id IN ` + msgs + ` OR project_id IN ` + projs,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor LIKE 'promote:%')`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor LIKE 'promote:%'`,
		`DELETE FROM capture_decisions WHERE message_id IN ` + msgs + ` OR project_id IN ` + projs,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE subject='` + piqSubject + `'`,
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM ai_extractions WHERE ai_run_id IN (SELECT id FROM ai_runs WHERE model IN ('` + piqModel + `','` + piqFake + `'))`,
		`DELETE FROM ai_runs WHERE model IN ('` + piqModel + `','` + piqFake + `')`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider='` + piqProvider + `'`,
		`DELETE FROM projects WHERE slug='` + piqSlug + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func (f *piqFixture) id(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var v int64
	if err := f.pool.QueryRow(ctx, q, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return v
}

// message seeds an inbound gmail question attributed (live) to the project.
func (f *piqFixture) message(t *testing.T, ctx context.Context, label string, sentAgo time.Duration) (msg, raw int64) {
	t.Helper()
	raw = f.id(t, ctx, `INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
	                    VALUES ($1,$2,'{}',$3, now()) RETURNING id`, f.account, "itest-pipeinq-"+label, "itest-pipeinq-h-"+label)
	msg = f.id(t, ctx, `INSERT INTO normalized_messages (raw_source_item_id, thread_id, direction, external_message_id,
	                                                     sent_at, body_text, subject, sender, channel)
	                    VALUES ($1,$2,'inbound',$3, now() - make_interval(secs => $4), 'can you confirm the date?',
	                            'itest-pipeinq','Dana <dana@pipeinq.example.test>','gmail') RETURNING id`,
		raw, f.thread, "<itest-pipeinq-"+label+"@x>", sentAgo.Seconds())
	f.id(t, ctx, `INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
	              VALUES ($1,'live','attributed',$2,'itest-pipeinq') RETURNING id`, msg, f.project)
	return msg, raw
}

// verdict writes a stored inquiry verdict by hand (the promote-stage tests).
func (f *piqFixture) verdict(t *testing.T, ctx context.Context, msg, raw int64) {
	t.Helper()
	run := f.id(t, ctx, `INSERT INTO ai_runs (worker_type, provider, model, input, output, status, created_at)
	                     VALUES ('classify_inquiry','itest',$1,'{}','{}','ok', now() - interval '5 minutes') RETURNING id`, piqModel)
	f.id(t, ctx, `INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields)
	              VALUES ($1,$2, jsonb_build_object('needs_reply',true,'ask_kind','question','asker','Dana',
	                'ask','can you confirm the date?','reason','asks directly','sender','Dana <dana@pipeinq.example.test>',
	                'subject','itest-pipeinq','channel','gmail','project_id',$3::bigint,'project_slug',$4::text,
	                'normalized_message_id',$5::bigint,'thread_id',$6::bigint,'thread_key','gmail:itest-pipeinq:1',
	                'thread_scope','thread','external_message_id','x','context_messages',0)) RETURNING id`,
		run, raw, f.project, piqSlug, msg, f.thread)
}

func registeredPass(t *testing.T, s pipeline.Stage, pool *pgxpool.Pool, pub pipeline.Publisher) pipeline.PassFunc {
	t.Helper()
	if _, ok := stageImpls[s]; !ok {
		t.Fatalf("stageImpls has no %q stage (C-D1)", s)
	}
	return buildPass(s, pool, pub)
}

func holdLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, key int64) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	var ok bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&ok); err != nil || !ok {
		conn.Release()
		t.Fatalf("take 0x%X: ok=%v err=%v", key, ok, err)
	}
	t.Cleanup(func() {
		conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
		conn.Release()
	})
}

func TestPipelinedInquiry_Integration_InquiryLockHeldIsErrLockHeld(t *testing.T) {
	ctx := context.Background()
	f := piqSetup(t, ctx)
	pass := registeredPass(t, pipeline.StageInquiry, f.pool, nil)
	holdLock(t, ctx, f.pool, piqGPULock)
	n, err := pass(ctx)
	if !errors.Is(err, pipeline.ErrLockHeld) || n != 0 {
		t.Errorf("inquiry pass with 0x%X held: (%d, %v), want (0, pipeline.ErrLockHeld) — the shared GPU lock: "+
			"retry in 30 s, then the sweep (E-D4)", piqGPULock, n, err)
	}
}

func TestPipelinedInquiry_Integration_PromoteLockHeldIsErrLockHeld(t *testing.T) {
	ctx := context.Background()
	f := piqSetup(t, ctx)
	pass := registeredPass(t, pipeline.StageInquiryPromote, f.pool, nil)
	holdLock(t, ctx, f.pool, piqProLock)
	n, err := pass(ctx)
	if !errors.Is(err, pipeline.ErrLockHeld) || n != 0 {
		t.Errorf("inquiry_promote pass with 0x%X held: (%d, %v), want (0, pipeline.ErrLockHeld) — the personal "+
			"CronJob's promote pass may hold it", piqProLock, n, err)
	}
}

func TestPipelinedInquiry_Integration_PromoteProcessedCountsActedVerdictsOnly(t *testing.T) {
	ctx := context.Background()
	f := piqSetup(t, ctx)
	m1, r1 := f.message(t, ctx, "acted", 2*time.Hour)
	f.verdict(t, ctx, m1, r1)
	m2, r2 := f.message(t, ctx, "pending", 10*time.Minute)
	f.verdict(t, ctx, m2, r2)
	n, err := registeredPass(t, pipeline.StageInquiryPromote, f.pool, nil)(ctx)
	if err != nil {
		t.Fatalf("inquiry_promote pass: %v", err)
	}
	if n != 1 {
		t.Errorf("processed = %d, want 1: the passing verdict counts; the pending one stays in the inbox and must "+
			"NOT count, or the loop re-runs at once while it sits inside its grace", n)
	}
}

// fakeOllama is the local model's native API with a canned, schema-valid
// inquiry verdict. Never a live LLM (test rules).
func fakeOllama(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	verdict, _ := json.Marshal(map[string]any{
		"needs_reply": true, "ask_kind": "question", "asker": "Dana",
		"ask": "can you confirm the date?", "reason": "asks the recipient directly",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{{"name": piqFake}}})
		case "/api/chat":
			calls.Add(1)
			json.NewEncoder(w).Encode(map[string]any{
				"model": piqFake, "message": map[string]string{"role": "assistant", "content": string(verdict)},
				"done": true, "done_reason": "stop", "total_duration": 1000000,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", within, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestPipelinedInquiry_Integration_CapturedWakeClassifiesThenSweepPromotesAfterGrace(t *testing.T) {
	broker := os.Getenv("MQTT_BROKER")
	if broker == "" {
		t.Skip("MQTT_BROKER not set; skipping the end-to-end pipeline test")
	}
	if strings.Contains(broker, "192.168.50.45") {
		t.Fatal("never the production broker; use tcp://localhost:1884")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	f := piqSetup(t, ctx)

	var chats atomic.Int32
	srv := fakeOllama(t, &chats)
	t.Setenv("OPS_LOCAL_PROVIDER_URL", srv.URL) // 127.0.0.1: a local endpoint by provider.LocalityOf
	t.Setenv("OPS_LOCAL_MODEL", piqFake)

	inqClient, err := pipeline.DialStage(ctx, broker, pipeline.StageInquiry)
	if err != nil {
		t.Fatalf("dial inquiry: %v", err)
	}
	defer inqClient.Disconnect()
	proClient, err := pipeline.DialStage(ctx, broker, pipeline.StageInquiryPromote)
	if err != nil {
		t.Fatalf("dial inquiry_promote: %v", err)
	}
	defer proClient.Disconnect()
	watch, err := fleet.NewSpineClient(ctx, broker, "itest-pipeinq-watch")
	if err != nil {
		t.Fatalf("dial watcher: %v", err)
	}
	defer watch.Disconnect()
	var classifiedWakes atomic.Int32
	if err := watch.Subscribe(pipeline.Topic(pipeline.EventInquiryClassified), func(string, []byte) { classifiedWakes.Add(1) }); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	inqPasses := make(chan int, 64)
	inner := registeredPass(t, pipeline.StageInquiry, f.pool, inqClient)
	inqLoop := pipeline.NewStageLoop(pipeline.StageConfig{
		Stage: pipeline.StageInquiry, Limit: stageImpls[pipeline.StageInquiry].limit, Sweep: time.Hour,
		Pass: func(ctx context.Context) (int, error) { n, err := inner(ctx); inqPasses <- n; return n, err },
	})
	proLoop := pipeline.NewStageLoop(pipeline.StageConfig{
		Stage: pipeline.StageInquiryPromote, Limit: stageImpls[pipeline.StageInquiryPromote].limit,
		Sweep: 300 * time.Millisecond,
		Pass:  registeredPass(t, pipeline.StageInquiryPromote, f.pool, proClient),
	})
	if err := pipeline.SubscribeWakes(inqClient, pipeline.StageInquiry, func(pipeline.Wake) { inqLoop.Notify() }); err != nil {
		t.Fatalf("subscribe inquiry: %v", err)
	}
	if err := pipeline.SubscribeWakes(proClient, pipeline.StageInquiryPromote, func(pipeline.Wake) { proLoop.Notify() }); err != nil {
		t.Fatalf("subscribe inquiry_promote: %v", err)
	}
	lctx, lcancel := context.WithCancel(ctx)
	done := make(chan struct{}, 2)
	go func() { inqLoop.Run(lctx); done <- struct{}{} }()
	go func() { proLoop.Run(lctx); done <- struct{}{} }()
	defer func() { lcancel(); <-done; <-done }()

	// The catch-up pass runs on an empty inbox; only then is the message seeded,
	// so the verdict below can only come from the `captured` wake (sweep 1h).
	select {
	case <-inqPasses:
	case <-time.After(20 * time.Second):
		t.Fatalf("the inquiry stage never ran its catch-up pass")
	}
	msg, _ := f.message(t, ctx, "e2e", 10*time.Minute)

	pub, err := fleet.NewSpineClient(ctx, broker, "itest-pipeinq-announce")
	if err != nil {
		t.Fatalf("dial announcer: %v", err)
	}
	defer pub.Disconnect()
	if err := pipeline.PublishWake(pub, pipeline.Wake{Event: pipeline.EventCaptured, Source: "itest-pipeinq", TS: time.Now().UTC()}); err != nil {
		t.Fatalf("publish captured: %v", err)
	}

	verdicts := func() int {
		var n int
		f.pool.QueryRow(ctx, `SELECT count(*) FROM ai_extractions e JOIN ai_runs r ON r.id=e.ai_run_id
		                       AND r.worker_type='classify_inquiry' AND r.status='ok'
		                       WHERE (e.fields->>'normalized_message_id')::bigint = $1`, msg).Scan(&n)
		return n
	}
	waitFor(t, 20*time.Second, "the captured wake to produce an inquiry verdict (C13)", func() bool { return verdicts() == 1 })
	if chats.Load() == 0 {
		t.Fatalf("a verdict appeared without a call to the (fake) local model")
	}
	waitFor(t, 10*time.Second, "the inquiry stage to publish inquiry_classified", func() bool { return classifiedWakes.Load() > 0 })

	promoted := func() (string, int64) {
		var action string
		var task *int64
		f.pool.QueryRow(ctx, `SELECT action, task_id FROM classify_promotions WHERE normalized_message_id=$1`, msg).Scan(&action, &task)
		if task == nil {
			return action, 0
		}
		return action, *task
	}
	time.Sleep(1500 * time.Millisecond) // several 300 ms sweeps inside the grace
	if a, _ := promoted(); a != "" {
		t.Fatalf("the verdict was promoted (%s) inside its 1h grace", a)
	}

	if _, err := f.pool.Exec(ctx, `UPDATE normalized_messages SET sent_at = now() - interval '2 hours' WHERE id=$1`, msg); err != nil {
		t.Fatalf("move the fixture clock: %v", err)
	}
	waitFor(t, 15*time.Second, "the sweep to promote the verdict after its grace", func() bool { _, task := promoted(); return task != 0 })
	action, task := promoted()
	var status string
	f.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, task).Scan(&status)
	if action != "review" || status != "holding" {
		t.Errorf("promoted as %s / task status %s, want review / holding (O7)", action, status)
	}
}
