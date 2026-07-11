//go:build integration

package tools_test

// Integration tests for the SWT-4 lifecycle tools (SPEC 04-mcp-task-tools,
// acceptance criteria 4 & 5). Build-tagged `integration` AND env-gated on
// DATABASE_URL: excluded from the offline `go test ./...`, skipped cleanly
// without the db. Run with:
//
//   DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//     go test -tags integration ./internal/tools/
//
// Every mutation goes through executor.Execute against real Postgres — the only
// route to a handler (invariant 3). worker_id is passed directly in the args
// here (the MCP-boundary injection is covered by internal/mcpserver's tests);
// this exercises the tools as the wrapper's in-process executor client does.
//
// GREENFIELD NOTE: the lifecycle tools are not registered yet, so under
// `-tags integration` these Execute calls return "unknown tool" and the test
// FAILs at the first mutation. After implementation the full walk passes.
//
// Return shapes asserted (normative in the SPEC's tool contract):
//   create_task        -> {"task_id": N}
//   task_get_next      -> {"task": {"id":N,"project":..,"subproject":..,"title":..,"priority":..} | null}
//   task_claim         -> {"claim_id":N,"task_id":N,"expires_at":"..."}
//   request_feedback   -> {"feedback_request_id":N}
//   record_decision    -> {"decision_id":N}
//   create_child_task  -> {"task_id":N}
//   mark_done_local    -> {"task_id":N,"status":"done_locally"}
// task_context returns a JSON document; assertions here are substring checks on
// its bytes (title/body/project-slug/decision-title present) to avoid pinning
// exact JSON keys the SPEC leaves to the implementer.

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

// ---- shared test scaffolding (used by the ordering test too) ----

const (
	toolsSlugLike  = "itest-mcp-tools-%"
	toolsActorLike = "itest-mcp-tools-%"
)

// newToolsPool opens the pool or skips when DATABASE_URL is unset.
func newToolsPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	return pool
}

// cleanupToolsData removes leftovers from prior runs in FK order so the tests
// are rerunnable against a persistent db. Scoped by test-owned prefixes.
func cleanupToolsData(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM policy_decisions WHERE audit_event_id IN
			(SELECT id FROM audit_events WHERE actor LIKE $1)`, []any{toolsActorLike}},
		{`DELETE FROM audit_events WHERE actor LIKE $1`, []any{toolsActorLike}},
		{`DELETE FROM task_events WHERE task_id IN
			(SELECT id FROM tasks WHERE project_id IN
				(SELECT id FROM projects WHERE slug LIKE $1))`, []any{toolsSlugLike}},
		{`DELETE FROM task_claims WHERE task_id IN
			(SELECT id FROM tasks WHERE project_id IN
				(SELECT id FROM projects WHERE slug LIKE $1))`, []any{toolsSlugLike}},
		{`DELETE FROM feedback_requests WHERE task_id IN
			(SELECT id FROM tasks WHERE project_id IN
				(SELECT id FROM projects WHERE slug LIKE $1))`, []any{toolsSlugLike}},
		// children before parents (tasks.parent_id self-FK)
		{`DELETE FROM tasks WHERE parent_id IS NOT NULL AND project_id IN
			(SELECT id FROM projects WHERE slug LIKE $1)`, []any{toolsSlugLike}},
		{`DELETE FROM tasks WHERE project_id IN
			(SELECT id FROM projects WHERE slug LIKE $1)`, []any{toolsSlugLike}},
		{`DELETE FROM decisions WHERE project_id IN
			(SELECT id FROM projects WHERE slug LIKE $1)`, []any{toolsSlugLike}},
		{`DELETE FROM projects WHERE slug LIKE $1`, []any{toolsSlugLike}},
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("cleanup %q: %v", s.sql, err)
		}
	}
}

func seedProject(t *testing.T, ctx context.Context, pool *pgxpool.Pool, slug, client string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path)
		 VALUES ($1, $2, $3, 'manual', 'dashboard', '/tmp/itest') RETURNING id`,
		slug, slug, client).Scan(&id)
	if err != nil {
		t.Fatalf("seed project %q: %v", slug, err)
	}
	return id
}

func newExecutor(pool *pgxpool.Pool) *executor.Executor {
	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	return executor.New(reg, policy.NewStatic(reg.Names()...), audit.NewPGStore(pool))
}

// callOK runs a tool through the executor and fails the test on error.
func callOK(t *testing.T, ctx context.Context, ex *executor.Executor, actor, tool, args string) json.RawMessage {
	t.Helper()
	res, err := ex.Execute(ctx, executor.Call{Tool: tool, Actor: actor, Args: []byte(args)})
	if err != nil {
		t.Fatalf("Execute(%s): %v", tool, err)
	}
	return res.Output
}

func taskStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatalf("read status of task %d: %v", id, err)
	}
	return s
}

func eventCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64, eventType string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type=$2`,
		taskID, eventType).Scan(&n); err != nil {
		t.Fatalf("count %s events for task %d: %v", eventType, taskID, err)
	}
	return n
}

// ---- the full lifecycle ----

func TestLifecycle_Integration_FullWalk(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()
	cleanupToolsData(t, ctx, pool)

	const (
		actor   = "itest-mcp-tools-lifecycle"
		slug    = "itest-mcp-tools-life"
		client  = "itest-mcp-tools-lifeclient"
		workerA = "itest-mcp-tools-workerA"
	)
	seedProject(t, ctx, pool, slug, client)
	ex := newExecutor(pool)

	// 1. create_task -> ready
	var created struct {
		TaskID int64 `json:"task_id"`
	}
	out := callOK(t, ctx, ex, actor, "create_task",
		`{"project":"`+slug+`","title":"lifecycle task","body":"do the thing","assignee_type":"claude","priority":5}`)
	mustUnmarshal(t, out, &created)
	if created.TaskID == 0 {
		t.Fatal("create_task returned task_id 0")
	}
	taskID := created.TaskID
	if s := taskStatus(t, ctx, pool, taskID); s != "ready" {
		t.Fatalf("after create_task status = %q, want ready", s)
	}

	// 2. task_get_next -> returns our task, does NOT claim it (still ready)
	var next getNextOut
	out = callOK(t, ctx, ex, actor, "task_get_next", `{"client":"`+client+`"}`)
	mustUnmarshal(t, out, &next)
	if next.Task == nil || next.Task.ID != taskID {
		t.Fatalf("task_get_next returned %+v, want task id %d", next.Task, taskID)
	}
	if s := taskStatus(t, ctx, pool, taskID); s != "ready" {
		t.Fatalf("task_get_next mutated status to %q; peek must not claim", s)
	}

	// 3. task_claim(workerA) -> claimed + task_claims row + 'claimed' event
	var claim struct {
		ClaimID   int64  `json:"claim_id"`
		TaskID    int64  `json:"task_id"`
		ExpiresAt string `json:"expires_at"`
	}
	out = callOK(t, ctx, ex, actor, "task_claim",
		`{"task_id":`+itoa(taskID)+`,"worker_id":"`+workerA+`"}`)
	mustUnmarshal(t, out, &claim)
	if claim.ClaimID == 0 {
		t.Fatal("task_claim returned claim_id 0")
	}
	if claim.ExpiresAt == "" {
		t.Error("task_claim did not stamp expires_at")
	}
	if s := taskStatus(t, ctx, pool, taskID); s != "claimed" {
		t.Fatalf("after task_claim status = %q, want claimed", s)
	}
	assertClaimRow(t, ctx, pool, taskID, workerA, false /*wantReleased*/)
	if eventCount(t, ctx, pool, taskID, "claimed") != 1 {
		t.Errorf("expected exactly one 'claimed' task_event")
	}

	// 5. task_context as claim holder -> in_progress + document + status_changed
	out = callOK(t, ctx, ex, actor, "task_context",
		`{"task_id":`+itoa(taskID)+`,"worker_id":"`+workerA+`"}`)
	doc := string(out)
	for _, want := range []string{"lifecycle task", "do the thing", slug} {
		if !contains(doc, want) {
			t.Errorf("task_context document missing %q; got: %s", want, doc)
		}
	}
	if s := taskStatus(t, ctx, pool, taskID); s != "in_progress" {
		t.Fatalf("task_context (holder) did not flip claimed->in_progress; status = %q", s)
	}
	if eventCount(t, ctx, pool, taskID, "status_changed") < 1 {
		t.Errorf("expected a 'status_changed' task_event on claimed->in_progress")
	}

	// 6. task_append_log -> 'log' event, no status change
	callOK(t, ctx, ex, actor, "task_append_log",
		`{"task_id":`+itoa(taskID)+`,"message":"progress note","worker_id":"`+workerA+`"}`)
	if eventCount(t, ctx, pool, taskID, "log") != 1 {
		t.Errorf("expected exactly one 'log' task_event")
	}
	if s := taskStatus(t, ctx, pool, taskID); s != "in_progress" {
		t.Errorf("task_append_log changed status to %q; it must not", s)
	}

	// 7. request_feedback -> needs_feedback + open row + 'feedback_requested'
	var fb struct {
		FeedbackRequestID int64 `json:"feedback_request_id"`
	}
	out = callOK(t, ctx, ex, actor, "request_feedback",
		`{"task_id":`+itoa(taskID)+`,"worker_id":"`+workerA+`","question":"which approach?"}`)
	mustUnmarshal(t, out, &fb)
	if fb.FeedbackRequestID == 0 {
		t.Fatal("request_feedback returned feedback_request_id 0")
	}
	if s := taskStatus(t, ctx, pool, taskID); s != "needs_feedback" {
		t.Fatalf("after request_feedback status = %q, want needs_feedback", s)
	}
	assertFeedbackStatus(t, ctx, pool, fb.FeedbackRequestID, "open")
	if eventCount(t, ctx, pool, taskID, "feedback_requested") != 1 {
		t.Errorf("expected exactly one 'feedback_requested' task_event")
	}

	// 8. answer_feedback (spine) -> row answered; task STAYS needs_feedback.
	// SPEC pins: the flip to in_progress happens at resume time (next
	// task_context by the holder), NOT in answer_feedback.
	callOK(t, ctx, ex, actor, "answer_feedback",
		`{"feedback_request_id":`+itoa(fb.FeedbackRequestID)+`,"answer":"approach B"}`)
	assertFeedbackStatus(t, ctx, pool, fb.FeedbackRequestID, "answered")
	if s := taskStatus(t, ctx, pool, taskID); s != "needs_feedback" {
		t.Fatalf("after answer_feedback status = %q, want needs_feedback (unchanged per SPEC)", s)
	}
	if eventCount(t, ctx, pool, taskID, "feedback_answered") != 1 {
		t.Errorf("expected exactly one 'feedback_answered' task_event")
	}
	var answeredAt *string
	if err := pool.QueryRow(ctx,
		`SELECT answered_at::text FROM feedback_requests WHERE id=$1`, fb.FeedbackRequestID).Scan(&answeredAt); err != nil {
		t.Fatalf("read answered_at: %v", err)
	}
	if answeredAt == nil {
		t.Error("answer_feedback did not set answered_at")
	}

	// 9. task_context as holder -> flips needs_feedback -> in_progress
	callOK(t, ctx, ex, actor, "task_context",
		`{"task_id":`+itoa(taskID)+`,"worker_id":"`+workerA+`"}`)
	if s := taskStatus(t, ctx, pool, taskID); s != "in_progress" {
		t.Fatalf("task_context (holder) did not flip needs_feedback->in_progress; status = %q", s)
	}

	// 10. record_decision -> decisions row; injected into subsequent context
	var dec struct {
		DecisionID int64 `json:"decision_id"`
	}
	out = callOK(t, ctx, ex, actor, "record_decision",
		`{"project":"`+slug+`","title":"use postgres queue","body":"skip locked","worker_id":"`+workerA+`"}`)
	mustUnmarshal(t, out, &dec)
	if dec.DecisionID == 0 {
		t.Fatal("record_decision returned decision_id 0")
	}
	var createdBy string
	if err := pool.QueryRow(ctx,
		`SELECT created_by FROM decisions WHERE id=$1`, dec.DecisionID).Scan(&createdBy); err != nil {
		t.Fatalf("read decision created_by: %v", err)
	}
	if !contains(createdBy, workerA) {
		t.Errorf("decision created_by = %q, want to contain worker id %q", createdBy, workerA)
	}
	out = callOK(t, ctx, ex, actor, "task_context",
		`{"task_id":`+itoa(taskID)+`,"worker_id":"`+workerA+`"}`)
	if !contains(string(out), "use postgres queue") {
		t.Errorf("task_context did not inject the recorded decision; got: %s", out)
	}

	// 11. create_child_task -> child with parent_id; 'child_created' on parent
	var child struct {
		TaskID int64 `json:"task_id"`
	}
	out = callOK(t, ctx, ex, actor, "create_child_task",
		`{"parent_task_id":`+itoa(taskID)+`,"title":"child task"}`)
	mustUnmarshal(t, out, &child)
	if child.TaskID == 0 {
		t.Fatal("create_child_task returned task_id 0")
	}
	var parentID *int64
	var childStatus string
	if err := pool.QueryRow(ctx,
		`SELECT parent_id, status FROM tasks WHERE id=$1`, child.TaskID).Scan(&parentID, &childStatus); err != nil {
		t.Fatalf("read child task: %v", err)
	}
	if parentID == nil || *parentID != taskID {
		t.Errorf("child parent_id = %v, want %d", parentID, taskID)
	}
	if childStatus != "ready" {
		t.Errorf("child status = %q, want ready", childStatus)
	}
	if eventCount(t, ctx, pool, taskID, "child_created") != 1 {
		t.Errorf("expected exactly one 'child_created' task_event on the parent")
	}

	// 12. mark_done_local -> done_locally + claim released_at + 'done_local'
	var done struct {
		TaskID int64  `json:"task_id"`
		Status string `json:"status"`
	}
	out = callOK(t, ctx, ex, actor, "mark_done_local",
		`{"task_id":`+itoa(taskID)+`,"worker_id":"`+workerA+`","summary":"finished"}`)
	mustUnmarshal(t, out, &done)
	if done.Status != "done_locally" {
		t.Errorf("mark_done_local returned status %q, want done_locally", done.Status)
	}
	if s := taskStatus(t, ctx, pool, taskID); s != "done_locally" {
		t.Fatalf("after mark_done_local status = %q, want done_locally", s)
	}
	assertClaimRow(t, ctx, pool, taskID, workerA, true /*wantReleased*/)
	if eventCount(t, ctx, pool, taskID, "done_local") != 1 {
		t.Errorf("expected exactly one 'done_local' task_event")
	}

	// 13. task_release (spine) on a fresh claimed task -> back to ready
	var t3 struct {
		TaskID int64 `json:"task_id"`
	}
	out = callOK(t, ctx, ex, actor, "create_task",
		`{"project":"`+slug+`","title":"releasable","assignee_type":"claude"}`)
	mustUnmarshal(t, out, &t3)
	callOK(t, ctx, ex, actor, "task_claim",
		`{"task_id":`+itoa(t3.TaskID)+`,"worker_id":"`+workerA+`"}`)
	callOK(t, ctx, ex, actor, "task_release",
		`{"task_id":`+itoa(t3.TaskID)+`,"worker_id":"`+workerA+`","reason":"stub failure"}`)
	if s := taskStatus(t, ctx, pool, t3.TaskID); s != "ready" {
		t.Fatalf("after task_release status = %q, want ready", s)
	}
	assertClaimRow(t, ctx, pool, t3.TaskID, workerA, true /*wantReleased*/)
	if eventCount(t, ctx, pool, t3.TaskID, "released") != 1 {
		t.Errorf("expected exactly one 'released' task_event")
	}

	// audit + policy coverage: every tool call produced an ok audit row and an
	// allow policy_decisions row (invariant 3, criterion 2).
	assertAuditTrail(t, ctx, pool, actor, []string{
		"create_task", "task_get_next", "task_claim", "task_context",
		"task_append_log", "request_feedback", "answer_feedback",
		"record_decision", "create_child_task", "mark_done_local", "task_release",
	})
}

// ---- claim contention (criterion 5) ----

// Two goroutines double-claim the SAME task id: exactly one wins, the other
// gets a clean already-claimed error (SKIP LOCKED fail-fast, never blocks).
func TestClaim_Integration_DoubleClaimSameID(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()
	cleanupToolsData(t, ctx, pool)

	const (
		actor  = "itest-mcp-tools-contend"
		slug   = "itest-mcp-tools-contend"
		client = "itest-mcp-tools-contendclient"
	)
	seedProject(t, ctx, pool, slug, client)
	ex := newExecutor(pool)

	var created struct {
		TaskID int64 `json:"task_id"`
	}
	out := callOK(t, ctx, ex, actor, "create_task",
		`{"project":"`+slug+`","title":"contended","assignee_type":"claude"}`)
	mustUnmarshal(t, out, &created)
	taskID := created.TaskID

	const workers = 2
	var wg sync.WaitGroup
	errs := make([]error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := ex.Execute(ctx, executor.Call{
				Tool:  "task_claim",
				Actor: actor,
				Args:  []byte(`{"task_id":` + itoa(taskID) + `,"worker_id":"itest-mcp-tools-w` + itoa(int64(i)) + `"}`),
			})
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("double-claim winners = %d, want exactly 1 (errs: %v)", wins, errs)
	}

	var claimRows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_claims WHERE task_id=$1 AND released_at IS NULL`, taskID).Scan(&claimRows); err != nil {
		t.Fatalf("count claim rows: %v", err)
	}
	if claimRows != 1 {
		t.Errorf("unreleased task_claims rows = %d, want 1", claimRows)
	}
	if s := taskStatus(t, ctx, pool, taskID); s != "claimed" {
		t.Errorf("contended task status = %q, want claimed", s)
	}
}

// N>=8 goroutines race get_next+claim over two tasks: each task claimed exactly
// once, losers get clean errors, nobody blocks.
func TestClaim_Integration_ManyRacersTwoTasks(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()
	cleanupToolsData(t, ctx, pool)

	const (
		actor  = "itest-mcp-tools-race"
		slug   = "itest-mcp-tools-race"
		client = "itest-mcp-tools-raceclient"
	)
	seedProject(t, ctx, pool, slug, client)
	ex := newExecutor(pool)

	for i := 0; i < 2; i++ {
		callOK(t, ctx, ex, actor, "create_task",
			`{"project":"`+slug+`","title":"race`+itoa(int64(i))+`","assignee_type":"claude"}`)
	}

	const racers = 8
	var wg sync.WaitGroup
	wins := make([]int, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			workerID := "itest-mcp-tools-r" + itoa(int64(i))
			for attempt := 0; attempt < 6; attempt++ {
				res, err := ex.Execute(ctx, executor.Call{
					Tool:  "task_get_next",
					Actor: actor,
					Args:  []byte(`{"client":"` + client + `"}`),
				})
				if err != nil {
					return
				}
				var next getNextOut
				if err := json.Unmarshal(res.Output, &next); err != nil || next.Task == nil {
					return // nothing left to claim
				}
				_, cerr := ex.Execute(ctx, executor.Call{
					Tool:  "task_claim",
					Actor: actor,
					Args:  []byte(`{"task_id":` + itoa(next.Task.ID) + `,"worker_id":"` + workerID + `"}`),
				})
				if cerr == nil {
					wins[i]++
					return
				}
				// lost the race for this id: re-peek.
			}
		}(i)
	}
	close(start)
	wg.Wait()

	total := 0
	for _, w := range wins {
		total += w
	}
	if total != 2 {
		t.Fatalf("total successful claims = %d, want 2 (one per task)", total)
	}

	var unreleased int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_claims WHERE released_at IS NULL AND task_id IN
			(SELECT id FROM tasks WHERE project_id IN
				(SELECT id FROM projects WHERE slug=$1))`, slug).Scan(&unreleased); err != nil {
		t.Fatalf("count claims: %v", err)
	}
	if unreleased != 2 {
		t.Errorf("unreleased claim rows = %d, want 2 (each task claimed exactly once)", unreleased)
	}
}

// ---- shared assertion + tiny helpers ----

type getNextOut struct {
	Task *struct {
		ID         int64  `json:"id"`
		Project    string `json:"project"`
		Subproject string `json:"subproject"`
		Title      string `json:"title"`
		Priority   int    `json:"priority"`
	} `json:"task"`
}

func assertClaimRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64, workerID string, wantReleased bool) {
	t.Helper()
	var expiresAt, releasedAt *string
	err := pool.QueryRow(ctx,
		`SELECT expires_at::text, released_at::text FROM task_claims
		 WHERE task_id=$1 AND worker_id=$2 ORDER BY id DESC LIMIT 1`,
		taskID, workerID).Scan(&expiresAt, &releasedAt)
	if err != nil {
		t.Fatalf("read claim row for task %d worker %s: %v", taskID, workerID, err)
	}
	if expiresAt == nil {
		t.Errorf("claim row expires_at is NULL; ClaimTTL must be stamped")
	}
	if wantReleased && releasedAt == nil {
		t.Errorf("claim row released_at is NULL, want set")
	}
	if !wantReleased && releasedAt != nil {
		t.Errorf("claim row released_at = %v, want NULL (still active)", *releasedAt)
	}
}

func assertFeedbackStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64, want string) {
	t.Helper()
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM feedback_requests WHERE id=$1`, id).Scan(&status); err != nil {
		t.Fatalf("read feedback_requests %d: %v", id, err)
	}
	if status != want {
		t.Errorf("feedback_requests %d status = %q, want %q", id, status, want)
	}
}

func assertAuditTrail(t *testing.T, ctx context.Context, pool *pgxpool.Pool, actor string, toolNames []string) {
	t.Helper()
	for _, tool := range toolNames {
		var okRows int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM audit_events WHERE actor=$1 AND tool=$2 AND status='ok'`,
			actor, tool).Scan(&okRows); err != nil {
			t.Fatalf("count audit_events for %s: %v", tool, err)
		}
		if okRows < 1 {
			t.Errorf("no ok audit_events row for tool %q (invariant 3)", tool)
		}
		var allowRows int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM policy_decisions p JOIN audit_events a ON a.id = p.audit_event_id
			 WHERE a.actor=$1 AND p.tool=$2 AND p.decision='allow'`,
			actor, tool).Scan(&allowRows); err != nil {
			t.Fatalf("count policy_decisions for %s: %v", tool, err)
		}
		if allowRows < 1 {
			t.Errorf("no allow policy_decisions row for tool %q", tool)
		}
	}
}

func mustUnmarshal(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
