//go:build integration

package orchestrator_test

// Integration tests for the orchestrator engine + the five SWT-5 spine tools +
// migration 0003 (SPEC 05-orchestrator-loop, acceptance criteria 2, 3, 5, 6, 7,
// 8). Build-tagged `integration` AND env-gated on DATABASE_URL (the full-loop
// resume test additionally gates on MQTT_BROKER): excluded from the offline
// `go test ./...`, skipped cleanly without the services. Run with:
//
//   DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//   MQTT_BROKER=tcp://localhost:1884 \
//     go test -tags integration ./internal/orchestrator/
//
// Every task-state mutation goes through executor.Execute (invariant 3); the
// engine's own SQL is read-only facts + orchestrator_cursor bookkeeping. Tests
// drive the engine one drain/tick at a time (the `--once` shape) rather than
// running the LISTEN loop, so they are deterministic.
//
// GREENFIELD NOTE: internal/orchestrator, migration 0003, the five spine tools,
// and fleet.NewSpineClient do not exist yet — under `-tags integration` this
// file compile-FAILs (missing package/symbols) and, once it compiles, FAILs at
// the first assertion (no trigger, unknown tool). That is the expected
// spec-first failure mode.
//
// ---- Imposed exported surface (engine.go / apply.go) --------------------------
//
//   // Publisher is the fleet-command surface the applier needs (apply.go depends
//   // on this interface, not the concrete client, so tests inject a fake).
//   // *fleet.Client (via fleet.NewSpineClient) satisfies it directly.
//   type Publisher interface { PublishCommand(workerID string, cmd fleet.Cmd) error }
//
//   func NewEngine(pool *pgxpool.Pool, ex *executor.Executor, pub Publisher, cfg Config) *Engine
//   func (e *Engine) DrainOnce(ctx context.Context) (processed int, err error) // events id>cursor, apply, advance
//   func (e *Engine) TickOnce(ctx context.Context, now time.Time) error         // R6 expiry + R7 brief
//
//   // fleet.NewSpineClient(ctx, brokerURL, clientID) (*fleet.Client, error) — no
//   // will, command publisher; MUST use a distinct client id (not fleetd's).

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/fleet"
	orch "github.com/sspataro57/switchboard/internal/orchestrator"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

const orchSlugLike = "itest-orch-%"

// recordingPublisher is a no-broker Publisher for the engine tests that don't
// assert on the resume publish (they still exercise the applier's publish path
// without a live broker).
type recordingPublisher struct {
	mu   sync.Mutex
	msgs []struct {
		worker string
		cmd    fleet.Cmd
	}
}

func (p *recordingPublisher) PublishCommand(workerID string, cmd fleet.Cmd) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs = append(p.msgs, struct {
		worker string
		cmd    fleet.Cmd
	}{workerID, cmd})
	return nil
}

// ---- scaffolding --------------------------------------------------------------

func newOrchPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping orchestrator integration test")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	return pool
}

func cleanupOrch(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	inOurTasks := `task_id IN (SELECT id FROM tasks WHERE project_id IN
		(SELECT id FROM projects WHERE slug LIKE $1))`
	stmts := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM policy_decisions WHERE audit_event_id IN
			(SELECT id FROM audit_events WHERE actor LIKE $1 OR ` + inOurTasks + `)`, []any{orchSlugLike}},
		{`DELETE FROM audit_events WHERE actor LIKE $1 OR ` + inOurTasks, []any{orchSlugLike}},
		{`DELETE FROM task_events WHERE ` + inOurTasks, []any{orchSlugLike}},
		{`DELETE FROM task_claims WHERE ` + inOurTasks, []any{orchSlugLike}},
		{`DELETE FROM task_dependencies WHERE ` + inOurTasks + ` OR depends_on_task_id IN
			(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug LIKE $1))`, []any{orchSlugLike}},
		{`DELETE FROM feedback_requests WHERE ` + inOurTasks, []any{orchSlugLike}},
		{`DELETE FROM external_refs WHERE ` + inOurTasks, []any{orchSlugLike}},
		// children before parents (tasks.parent_id self-FK)
		{`DELETE FROM tasks WHERE parent_id IS NOT NULL AND project_id IN
			(SELECT id FROM projects WHERE slug LIKE $1)`, []any{orchSlugLike}},
		{`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug LIKE $1)`, []any{orchSlugLike}},
		{`DELETE FROM decisions WHERE project_id IN (SELECT id FROM projects WHERE slug LIKE $1)`, []any{orchSlugLike}},
		{`DELETE FROM projects WHERE slug LIKE $1`, []any{orchSlugLike}},
		{`DELETE FROM orchestrator_cursor WHERE name LIKE $1`, []any{orchSlugLike}},
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("cleanup %q: %v", s.sql, err)
		}
	}
}

func seedProjectDelivery(t *testing.T, ctx context.Context, pool *pgxpool.Pool, slug, client, delivery string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path)
		 VALUES ($1,$2,$3,'manual',$4,'/tmp/itest')`,
		slug, slug, client, delivery); err != nil {
		t.Fatalf("seed project %q: %v", slug, err)
	}
}

func newOrchExecutor(pool *pgxpool.Pool) *executor.Executor {
	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	return executor.New(reg, policy.NewStatic(reg.Names()...), audit.NewPGStore(pool))
}

func callOrch(t *testing.T, ctx context.Context, ex *executor.Executor, actor, tool, args string) []byte {
	t.Helper()
	res, err := ex.Execute(ctx, executor.Call{Tool: tool, Actor: actor, Args: []byte(args)})
	if err != nil {
		t.Fatalf("Execute(%s, %s): %v", tool, args, err)
	}
	return res.Output
}

func createReadyTask(t *testing.T, ctx context.Context, ex *executor.Executor, actor, slug, title string) int64 {
	t.Helper()
	out := callOrch(t, ctx, ex, actor, "create_task",
		`{"project":"`+slug+`","title":"`+title+`","assignee_type":"claude"}`)
	var r struct {
		TaskID int64 `json:"task_id"`
	}
	mustJSON(t, out, &r)
	if r.TaskID == 0 {
		t.Fatal("create_task returned task_id 0")
	}
	return r.TaskID
}

func orchStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatalf("status of task %d: %v", id, err)
	}
	return s
}

func maxEventID(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(max(id),0) FROM task_events`).Scan(&n); err != nil {
		t.Fatalf("max event id: %v", err)
	}
	return n
}

func setCursor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int64) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`UPDATE orchestrator_cursor SET last_event_id=$1, updated_at=now() WHERE name='orchestrator'`, n); err != nil {
		t.Fatalf("set cursor: %v", err)
	}
}

func cursorValue(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx, `SELECT last_event_id FROM orchestrator_cursor WHERE name='orchestrator'`).Scan(&n); err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	return n
}

func claimAndProgress(t *testing.T, ctx context.Context, ex *executor.Executor, actor string, taskID int64, worker string) {
	t.Helper()
	callOrch(t, ctx, ex, actor, "task_claim",
		`{"task_id":`+itoa(taskID)+`,"worker_id":"`+worker+`"}`)
	callOrch(t, ctx, ex, actor, "task_context",
		`{"task_id":`+itoa(taskID)+`,"worker_id":"`+worker+`"}`)
}

func markDone(t *testing.T, ctx context.Context, ex *executor.Executor, actor string, taskID int64, worker string) {
	t.Helper()
	callOrch(t, ctx, ex, actor, "mark_done_local",
		`{"task_id":`+itoa(taskID)+`,"worker_id":"`+worker+`","summary":"done"}`)
}

func drain(t *testing.T, ctx context.Context, e *orch.Engine) int {
	t.Helper()
	n, err := e.DrainOnce(ctx)
	if err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	return n
}

func mustJSON(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// ---- criterion 2: migration 0003 artifacts + cursor seed formula -------------

func TestOrchestrator_Integration_Migration0003(t *testing.T) {
	ctx := context.Background()
	pool := newOrchPool(t, ctx)
	defer pool.Close()
	cleanupOrch(t, ctx, pool)

	// Trigger present on task_events.
	var trig int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_trigger WHERE tgname='task_events_notify' AND NOT tgisinternal`).Scan(&trig); err != nil {
		t.Fatalf("query pg_trigger: %v", err)
	}
	if trig != 1 {
		t.Errorf("task_events_notify trigger count = %d, want 1 (migration 0003)", trig)
	}

	// Cursor bookkeeping row seeded by the migration.
	var name string
	if err := pool.QueryRow(ctx,
		`SELECT name FROM orchestrator_cursor WHERE name='orchestrator'`).Scan(&name); err != nil {
		t.Fatalf("orchestrator_cursor 'orchestrator' row missing (migration 0003 seed): %v", err)
	}

	// Seed formula: the migration seeds at COALESCE(max(task_events.id),0). Prove
	// the exact expression yields current max — so a first deploy never replays
	// pre-migration history. Uses a test-owned cursor name (cleaned up above/next run).
	if _, err := pool.Exec(ctx,
		`INSERT INTO orchestrator_cursor (name, last_event_id)
		 VALUES ('itest-orch-seed', COALESCE((SELECT max(id) FROM task_events), 0))`); err != nil {
		t.Fatalf("exercise seed formula: %v", err)
	}
	var seeded, want int64
	if err := pool.QueryRow(ctx, `SELECT last_event_id FROM orchestrator_cursor WHERE name='itest-orch-seed'`).Scan(&seeded); err != nil {
		t.Fatalf("read seeded cursor: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COALESCE(max(id),0) FROM task_events`).Scan(&want); err != nil {
		t.Fatalf("read max event id: %v", err)
	}
	if seeded != want {
		t.Errorf("seed formula produced %d, want max(task_events.id)=%d", seeded, want)
	}
}

// ---- criterion 3: NOTIFY trigger fires with the event id as payload ----------

func TestOrchestrator_Integration_Notify(t *testing.T) {
	ctx := context.Background()
	pool := newOrchPool(t, ctx)
	defer pool.Close()
	cleanupOrch(t, ctx, pool)

	const (
		actor  = "itest-orch-notify"
		slug   = "itest-orch-notify"
		client = "itest-orch-notify-client"
		worker = "itest-orch-notify-w"
	)
	seedProjectDelivery(t, ctx, pool, slug, client, "console")
	ex := newOrchExecutor(pool)
	taskID := createReadyTask(t, ctx, ex, actor, slug, "notify task")

	// Dedicated LISTEN connection established BEFORE the event insert.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire listen conn: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN task_events"); err != nil {
		t.Fatalf("LISTEN task_events: %v", err)
	}

	// task_claim inserts a 'claimed' task_event -> the trigger must pg_notify its id.
	callOrch(t, ctx, ex, actor, "task_claim", `{"task_id":`+itoa(taskID)+`,"worker_id":"`+worker+`"}`)

	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	n, err := conn.Conn().WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("WaitForNotification: %v (expected the trigger to fire on the 'claimed' insert)", err)
	}
	if n.Channel != "task_events" {
		t.Errorf("notification channel = %q, want task_events", n.Channel)
	}
	// Payload is the event id ONLY (SPEC: keeps us under the notify byte limit).
	gotID, perr := strconv.ParseInt(strings.TrimSpace(n.Payload), 10, 64)
	if perr != nil {
		t.Fatalf("notification payload %q is not an integer event id: %v", n.Payload, perr)
	}
	var claimedID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM task_events WHERE task_id=$1 AND event_type='claimed' ORDER BY id DESC LIMIT 1`,
		taskID).Scan(&claimedID); err != nil {
		t.Fatalf("read claimed event id: %v", err)
	}
	if gotID != claimedID {
		t.Errorf("notify payload id = %d, want the 'claimed' event id %d", gotID, claimedID)
	}
}

// ---- criterion 5 (db half): feedback drain, R1 exactly once, dedup on re-drain

func TestOrchestrator_Integration_FeedbackDrain(t *testing.T) {
	ctx := context.Background()
	pool := newOrchPool(t, ctx)
	defer pool.Close()
	cleanupOrch(t, ctx, pool)

	const (
		actor  = "itest-orch-fbdrain"
		slug   = "itest-orch-fbdrain"
		client = "itest-orch-fbdrain-client"
		worker = "itest-orch-fbdrain-w"
	)
	seedProjectDelivery(t, ctx, pool, slug, client, "console")
	ex := newOrchExecutor(pool)
	engine := orch.NewEngine(pool, ex, &recordingPublisher{}, orch.Config{})

	// Cursor at current max so the drain sees only the events we are about to make.
	setCursor(t, ctx, pool, maxEventID(t, ctx, pool))

	taskID := createReadyTask(t, ctx, ex, actor, slug, "parked task")
	claimAndProgress(t, ctx, ex, actor, taskID, worker)
	fbOut := callOrch(t, ctx, ex, actor, "request_feedback",
		`{"task_id":`+itoa(taskID)+`,"worker_id":"`+worker+`","question":"which approach, A or B?"}`)
	var fb struct {
		FeedbackRequestID int64 `json:"feedback_request_id"`
	}
	mustJSON(t, fbOut, &fb)

	beforeCursor := cursorValue(t, ctx, pool)

	// First drain: R1 creates the answer task exactly once + an orchestrated row.
	drain(t, ctx, engine)

	answerID, title := answerTask(t, ctx, pool, taskID)
	if answerID == 0 {
		t.Fatal("R1 did not create the human answer task")
	}
	if !strings.Contains(title, "Answer feedback") || !strings.Contains(title, itoa(fb.FeedbackRequestID)) {
		t.Errorf("answer task title = %q, want it to name the feedback request", title)
	}
	assertHumanChild(t, ctx, pool, answerID, taskID, "which approach")

	// orchestrated decision row on the source task, with the applier-injected created_task_id.
	assertOrchestrated(t, ctx, pool, taskID, "feedback_task", answerID)

	// audit trail for the orchestrator's own create_task (invariant 3, actor orchestrator).
	assertOrchestratorAudit(t, ctx, pool, "create_task")

	if cursorValue(t, ctx, pool) <= beforeCursor {
		t.Errorf("cursor did not advance after drain (before=%d after=%d)", beforeCursor, cursorValue(t, ctx, pool))
	}

	// Re-drain from BEFORE the feedback_requested event: dedup (the orchestrated
	// fact) must prevent a second answer task — idempotency, not just cursor luck.
	setCursor(t, ctx, pool, beforeCursor)
	drain(t, ctx, engine)
	if n := answerTaskCount(t, ctx, pool, taskID); n != 1 {
		t.Fatalf("re-drain created a duplicate answer task; count = %d, want 1", n)
	}
}

// ---- criterion 6: delivery rule (dashboard yields a Deliver task, console none)

func TestOrchestrator_Integration_DeliveryRule(t *testing.T) {
	ctx := context.Background()
	pool := newOrchPool(t, ctx)
	defer pool.Close()
	cleanupOrch(t, ctx, pool)

	const actor = "itest-orch-deliver"
	seedProjectDelivery(t, ctx, pool, "itest-orch-deliver-dash", "itest-orch-deliver-dc", "dashboard")
	seedProjectDelivery(t, ctx, pool, "itest-orch-deliver-cons", "itest-orch-deliver-cc", "console")
	ex := newOrchExecutor(pool)
	engine := orch.NewEngine(pool, ex, &recordingPublisher{}, orch.Config{})

	setCursor(t, ctx, pool, maxEventID(t, ctx, pool))

	dashTask := createReadyTask(t, ctx, ex, actor, "itest-orch-deliver-dash", "dash work")
	consTask := createReadyTask(t, ctx, ex, actor, "itest-orch-deliver-cons", "cons work")
	for _, id := range []int64{dashTask, consTask} {
		claimAndProgress(t, ctx, ex, actor, id, "itest-orch-deliver-w")
		markDone(t, ctx, ex, actor, id, "itest-orch-deliver-w")
	}
	beforeCursor := cursorValue(t, ctx, pool)
	drain(t, ctx, engine)

	if n := deliverTaskCount(t, ctx, pool, dashTask); n != 1 {
		t.Errorf("dashboard project: Deliver task count = %d, want 1", n)
	}
	if n := deliverTaskCount(t, ctx, pool, consTask); n != 0 {
		t.Errorf("console project: Deliver task count = %d, want 0 (console skips R3)", n)
	}
	// Source task stays done_locally (transition to delivered is step 8's).
	if s := orchStatus(t, ctx, pool, dashTask); s != "done_locally" {
		t.Errorf("dashboard source task status = %q, want done_locally (unchanged)", s)
	}

	// Replay: still exactly one delivery task (task-scoped dedup).
	setCursor(t, ctx, pool, beforeCursor)
	drain(t, ctx, engine)
	if n := deliverTaskCount(t, ctx, pool, dashTask); n != 1 {
		t.Errorf("re-drain duplicated the Deliver task; count = %d, want 1", n)
	}
}

// ---- criterion 8: claim expiry sweep (releases; exempts needs_feedback) ------

func TestOrchestrator_Integration_ClaimExpiry(t *testing.T) {
	ctx := context.Background()
	pool := newOrchPool(t, ctx)
	defer pool.Close()
	cleanupOrch(t, ctx, pool)

	const (
		actor   = "itest-orch-expiry"
		slug    = "itest-orch-expiry"
		client  = "itest-orch-expiry-client"
		workerA = "itest-orch-expiry-wa"
		workerB = "itest-orch-expiry-wb"
	)
	seedProjectDelivery(t, ctx, pool, slug, client, "console")
	ex := newOrchExecutor(pool)
	engine := orch.NewEngine(pool, ex, &recordingPublisher{}, orch.Config{})

	// Task 1: in_progress with an expired claim -> must be released.
	live := createReadyTask(t, ctx, ex, actor, slug, "crashed worker task")
	claimAndProgress(t, ctx, ex, actor, live, workerA)
	backdateClaim(t, ctx, pool, live)

	// Task 2: needs_feedback with an expired claim -> must be EXEMPT.
	parked := createReadyTask(t, ctx, ex, actor, slug, "parked task")
	callOrch(t, ctx, ex, actor, "task_claim", `{"task_id":`+itoa(parked)+`,"worker_id":"`+workerB+`"}`)
	callOrch(t, ctx, ex, actor, "request_feedback",
		`{"task_id":`+itoa(parked)+`,"worker_id":"`+workerB+`","question":"stuck"}`)
	backdateClaim(t, ctx, pool, parked)

	if err := engine.TickOnce(ctx, time.Now()); err != nil {
		t.Fatalf("TickOnce: %v", err)
	}

	if s := orchStatus(t, ctx, pool, live); s != "ready" {
		t.Errorf("expired in_progress task status = %q, want ready (swept back)", s)
	}
	if !claimReleased(t, ctx, pool, live) {
		t.Error("swept claim released_at is NULL, want set")
	}
	if r := releasedReason(t, ctx, pool, live); !strings.Contains(r, "sweep") {
		t.Errorf("'released' event reason = %q, want it to name the sweep", r)
	}

	if s := orchStatus(t, ctx, pool, parked); s != "needs_feedback" {
		t.Errorf("parked task status = %q, want needs_feedback (exempt from sweep)", s)
	}
	if claimReleased(t, ctx, pool, parked) {
		t.Error("needs_feedback claim was released; it must be exempt")
	}
}

// ---- criterion 7: dependency gating (block on add, unblock on done / close) --

func TestOrchestrator_Integration_Dependencies(t *testing.T) {
	ctx := context.Background()
	pool := newOrchPool(t, ctx)
	defer pool.Close()
	cleanupOrch(t, ctx, pool)

	const (
		actor  = "itest-orch-deps"
		slug   = "itest-orch-deps"
		client = "itest-orch-deps-client"
		worker = "itest-orch-deps-w"
	)
	seedProjectDelivery(t, ctx, pool, slug, client, "console")
	ex := newOrchExecutor(pool)
	engine := orch.NewEngine(pool, ex, &recordingPublisher{}, orch.Config{})
	setCursor(t, ctx, pool, maxEventID(t, ctx, pool))

	// -- single dep: add on a ready task -> blocked; complete dep -> ready -------
	depA := createReadyTask(t, ctx, ex, actor, slug, "dep A")
	blockedB := createReadyTask(t, ctx, ex, actor, slug, "dependent B")
	addDep(t, ctx, ex, actor, blockedB, depA)
	drain(t, ctx, engine) // R4 on dependency_added
	if s := orchStatus(t, ctx, pool, blockedB); s != "blocked" {
		t.Fatalf("B status = %q after add_dependency+drain, want blocked", s)
	}
	claimAndProgress(t, ctx, ex, actor, depA, worker)
	markDone(t, ctx, ex, actor, depA, worker)
	drain(t, ctx, engine) // R5 on done_local
	if s := orchStatus(t, ctx, pool, blockedB); s != "ready" {
		t.Fatalf("B status = %q after dep A done+drain, want ready", s)
	}

	// -- two deps: flip only after the SECOND completes -------------------------
	depC := createReadyTask(t, ctx, ex, actor, slug, "dep C")
	depD := createReadyTask(t, ctx, ex, actor, slug, "dep D")
	multiB := createReadyTask(t, ctx, ex, actor, slug, "dependent multi")
	addDep(t, ctx, ex, actor, multiB, depC)
	addDep(t, ctx, ex, actor, multiB, depD)
	drain(t, ctx, engine)
	if s := orchStatus(t, ctx, pool, multiB); s != "blocked" {
		t.Fatalf("multiB status = %q, want blocked (two unmet deps)", s)
	}
	claimAndProgress(t, ctx, ex, actor, depC, worker)
	markDone(t, ctx, ex, actor, depC, worker)
	drain(t, ctx, engine)
	if s := orchStatus(t, ctx, pool, multiB); s != "blocked" {
		t.Fatalf("multiB status = %q after only C done, want still blocked", s)
	}
	claimAndProgress(t, ctx, ex, actor, depD, worker)
	markDone(t, ctx, ex, actor, depD, worker)
	drain(t, ctx, engine)
	if s := orchStatus(t, ctx, pool, multiB); s != "ready" {
		t.Fatalf("multiB status = %q after both deps done, want ready", s)
	}

	// -- task_close of a dep also unblocks (status_changed-to-closed path) -------
	depE := createReadyTask(t, ctx, ex, actor, slug, "dep E")
	closeB := createReadyTask(t, ctx, ex, actor, slug, "dependent close")
	addDep(t, ctx, ex, actor, closeB, depE)
	drain(t, ctx, engine)
	if s := orchStatus(t, ctx, pool, closeB); s != "blocked" {
		t.Fatalf("closeB status = %q, want blocked", s)
	}
	callOrch(t, ctx, ex, actor, "task_close", `{"task_id":`+itoa(depE)+`,"reason":"obsolete"}`)
	drain(t, ctx, engine)
	if s := orchStatus(t, ctx, pool, closeB); s != "ready" {
		t.Fatalf("closeB status = %q after dep E closed, want ready", s)
	}
}

// ---- criterion 5 (full loop, needs MQTT): resume observed on the cmd topic ----

func TestOrchestrator_Integration_FullLoopResume(t *testing.T) {
	ctx := context.Background()
	broker := os.Getenv("MQTT_BROKER")
	if broker == "" || os.Getenv("DATABASE_URL") == "" {
		t.Skip("MQTT_BROKER and/or DATABASE_URL not set; skipping full-loop test")
	}
	pool := newOrchPool(t, ctx)
	defer pool.Close()
	cleanupOrch(t, ctx, pool)

	const (
		actor  = "itest-orch-full"
		slug   = "itest-orch-full"
		client = "itest-orch-full-client"
		worker = "itest-orch-full-w" // must be a valid worker id (no dots/slashes)
	)
	seedProjectDelivery(t, ctx, pool, slug, client, "console")
	ex := newOrchExecutor(pool)

	// Real spine client as the engine's Publisher (distinct client id from fleetd).
	spine, err := fleet.NewSpineClient(ctx, broker, "switchboard-orchestratord-itest")
	if err != nil {
		t.Fatalf("fleet.NewSpineClient: %v", err)
	}
	defer spine.Disconnect()
	engine := orch.NewEngine(pool, ex, spine, orch.Config{})
	setCursor(t, ctx, pool, maxEventID(t, ctx, pool))

	// Raw subscriber on the holder's cmd topic to observe the resume publish.
	resumeCh := make(chan fleet.Cmd, 1)
	sub := rawSubscribe(t, broker, "itest-orch-full-sub", fleet.CmdTopic(worker), func(payload []byte) {
		if c, perr := fleet.ParseCmd(payload); perr == nil {
			select {
			case resumeCh <- c:
			default:
			}
		}
	})
	defer sub.Disconnect(250)

	// Park a worker on feedback.
	taskID := createReadyTask(t, ctx, ex, actor, slug, "park then resume")
	claimAndProgress(t, ctx, ex, actor, taskID, worker)
	fbOut := callOrch(t, ctx, ex, actor, "request_feedback",
		`{"task_id":`+itoa(taskID)+`,"worker_id":"`+worker+`","question":"go ahead?"}`)
	var fb struct {
		FeedbackRequestID int64 `json:"feedback_request_id"`
	}
	mustJSON(t, fbOut, &fb)

	// Drain: R1 creates the answer task.
	drain(t, ctx, engine)
	answerID, _ := answerTask(t, ctx, pool, taskID)
	if answerID == 0 {
		t.Fatal("R1 did not create the answer task")
	}

	// Human answers (WITHOUT --resume: the orchestrator owns the publish now).
	callOrch(t, ctx, ex, actor, "answer_feedback",
		`{"feedback_request_id":`+itoa(fb.FeedbackRequestID)+`,"answer":"yes, proceed"}`)

	// Drain: R2 publishes resume to the holder + closes the answer task.
	drain(t, ctx, engine)

	select {
	case cmd := <-resumeCh:
		if cmd.Action != fleet.ActionResume {
			t.Errorf("cmd action = %q, want resume", cmd.Action)
		}
		var args struct {
			TaskID            int64 `json:"task_id"`
			FeedbackRequestID int64 `json:"feedback_request_id"`
		}
		mustJSON(t, cmd.Args, &args)
		if args.TaskID != taskID || args.FeedbackRequestID != fb.FeedbackRequestID {
			t.Errorf("resume args = {task_id:%d, feedback_request_id:%d}, want {%d, %d}",
				args.TaskID, args.FeedbackRequestID, taskID, fb.FeedbackRequestID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("did not observe a resume command on the holder's cmd topic within 10s")
	}

	if s := orchStatus(t, ctx, pool, answerID); s != "closed" {
		t.Errorf("answer task status = %q, want closed (R2 retires it)", s)
	}
}

// ---- small DB helpers ---------------------------------------------------------

func backdateClaim(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64) {
	t.Helper()
	tag, err := pool.Exec(ctx,
		`UPDATE task_claims SET expires_at = now() - interval '1 hour'
		 WHERE task_id=$1 AND released_at IS NULL`, taskID)
	if err != nil {
		t.Fatalf("backdate claim for task %d: %v", taskID, err)
	}
	if tag.RowsAffected() == 0 {
		t.Fatalf("no active claim to backdate for task %d", taskID)
	}
}

func claimReleased(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64) bool {
	t.Helper()
	var releasedAt *string
	if err := pool.QueryRow(ctx,
		`SELECT released_at::text FROM task_claims WHERE task_id=$1 ORDER BY id DESC LIMIT 1`, taskID).Scan(&releasedAt); err != nil {
		t.Fatalf("read claim for task %d: %v", taskID, err)
	}
	return releasedAt != nil
}

func releasedReason(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64) string {
	t.Helper()
	var reason string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(payload->>'reason','') FROM task_events
		 WHERE task_id=$1 AND event_type='released' ORDER BY id DESC LIMIT 1`, taskID).Scan(&reason); err != nil {
		t.Fatalf("read released reason for task %d: %v", taskID, err)
	}
	return reason
}

func addDep(t *testing.T, ctx context.Context, ex *executor.Executor, actor string, taskID, dependsOn int64) {
	t.Helper()
	callOrch(t, ctx, ex, actor, "task_add_dependency",
		`{"task_id":`+itoa(taskID)+`,"depends_on_task_id":`+itoa(dependsOn)+`}`)
}

// answerTask returns the (id, title) of the single human answer child of taskID, or (0,"").
func answerTask(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64) (int64, string) {
	t.Helper()
	var id int64
	var title string
	err := pool.QueryRow(ctx,
		`SELECT id, title FROM tasks WHERE parent_id=$1 AND title LIKE 'Answer feedback%' ORDER BY id DESC LIMIT 1`,
		taskID).Scan(&id, &title)
	if err != nil {
		return 0, ""
	}
	return id, title
}

func answerTaskCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE parent_id=$1 AND title LIKE 'Answer feedback%'`, taskID).Scan(&n); err != nil {
		t.Fatalf("count answer tasks: %v", err)
	}
	return n
}

func deliverTaskCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE parent_id=$1 AND title LIKE 'Deliver #%'`, taskID).Scan(&n); err != nil {
		t.Fatalf("count deliver tasks: %v", err)
	}
	return n
}

func assertHumanChild(t *testing.T, ctx context.Context, pool *pgxpool.Pool, childID, parentID int64, bodyContains string) {
	t.Helper()
	var parent *int64
	var assignee, body string
	if err := pool.QueryRow(ctx,
		`SELECT parent_id, assignee_type, COALESCE(body,'') FROM tasks WHERE id=$1`, childID).Scan(&parent, &assignee, &body); err != nil {
		t.Fatalf("read child task %d: %v", childID, err)
	}
	if parent == nil || *parent != parentID {
		t.Errorf("child parent_id = %v, want %d", parent, parentID)
	}
	if assignee != "human" {
		t.Errorf("child assignee_type = %q, want human", assignee)
	}
	if !strings.Contains(body, bodyContains) {
		t.Errorf("child body missing %q; body=%q", bodyContains, body)
	}
}

func assertOrchestrated(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64, rule string, createdTaskID int64) {
	t.Helper()
	var created int64
	err := pool.QueryRow(ctx,
		`SELECT COALESCE((payload->>'created_task_id')::bigint, 0) FROM task_events
		 WHERE task_id=$1 AND event_type='orchestrated' AND payload->>'rule'=$2 ORDER BY id DESC LIMIT 1`,
		taskID, rule).Scan(&created)
	if err != nil {
		t.Fatalf("no 'orchestrated' event rule=%q on task %d: %v", rule, taskID, err)
	}
	if created != createdTaskID {
		t.Errorf("orchestrated payload created_task_id = %d, want %d (applier must inject the runtime id)", created, createdTaskID)
	}
}

func assertOrchestratorAudit(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tool string) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE actor='orchestrator' AND tool=$1 AND status='ok'`, tool).Scan(&n); err != nil {
		t.Fatalf("count orchestrator audit for %s: %v", tool, err)
	}
	if n < 1 {
		t.Errorf("no ok audit_events row for actor=orchestrator tool=%q (invariant 3)", tool)
	}
}

// rawSubscribe connects a plain paho client subscribed to topic, invoking cb per message.
func rawSubscribe(t *testing.T, broker, clientID, topic string, cb func([]byte)) mqtt.Client {
	t.Helper()
	opts := mqtt.NewClientOptions().
		AddBroker(broker).
		SetClientID(clientID).
		SetConnectTimeout(5 * time.Second).
		SetAutoReconnect(false)
	c := mqtt.NewClient(opts)
	tok := c.Connect()
	if !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
		t.Fatalf("paho connect %s: %v", clientID, tok.Error())
	}
	stok := c.Subscribe(topic, 1, func(_ mqtt.Client, m mqtt.Message) { cb(m.Payload()) })
	if !stok.WaitTimeout(5*time.Second) || stok.Error() != nil {
		t.Fatalf("subscribe %s: %v", topic, stok.Error())
	}
	return c
}
