//go:build integration

package tools_test

// SWT-52 (docs/tickets/board-status-lights_SPEC.md) criterion 18 / D7 / D10:
// task_signal through executor.Execute on the PRODUCTION wiring
// (queueMatrixExecutor), with events, result keys and audit rows asserted.
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_isolights?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run Signal ./internal/tools/
//
// Reuses verbsPool (mcp_verbs_integration_test.go): cleanup of audit rows by
// task_id, then cleanupToolsData, at start and in t.Cleanup. Every call passes
// Call.TaskID so its audit rows are scoped to a test-owned task.
//
// GREENFIELD NOTE — EXPECTED RED: task_signal is not registered ("unknown
// tool"), and until 0033 is applied the working_state columns do not exist.

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
)

const sgSlug = "itest-mcp-tools-signal"

func sgTask(t *testing.T, ctx context.Context, pool *pgxpool.Pool, proj int64, title, assignee, status string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO tasks (project_id, title, assignee_type, status) VALUES ($1,$2,$3,$4) RETURNING id`,
		proj, title, assignee, status).Scan(&id); err != nil {
		t.Fatalf("seed task %q: %v", title, err)
	}
	return id
}

func sgExec(ctx context.Context, ex *executor.Executor, actor string, task int64, state string) (map[string]json.RawMessage, error) {
	args, _ := json.Marshal(map[string]any{"task_id": task, "state": state, "worker_id": strings.TrimPrefix(actor, "mcp:")})
	res, err := ex.Execute(ctx, executor.Call{Tool: "task_signal", Actor: actor, Args: args, TaskID: &task})
	if err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		return nil, err
	}
	return out, nil
}

type sgRow struct {
	state, stateAt, status, updatedAt, closedAt, closedFrom, surfacedAt string
	priority                                                            int
}

func sgRead(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) sgRow {
	t.Helper()
	var r sgRow
	if err := pool.QueryRow(ctx, `SELECT COALESCE(working_state,''), COALESCE(working_state_at::text,''), status,
		updated_at::text, COALESCE(closed_at::text,''), COALESCE(closed_from_status,''), COALESCE(surfaced_at::text,''), priority
		FROM tasks WHERE id=$1`, id).Scan(&r.state, &r.stateAt, &r.status, &r.updatedAt, &r.closedAt, &r.closedFrom,
		&r.surfacedAt, &r.priority); err != nil {
		t.Fatalf("read task %d: %v", id, err)
	}
	return r
}

func sgCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

func sgEvents(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) int {
	return sgCount(t, ctx, pool, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='working_state_changed'`, id)
}

func sgLastEvent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) (keys []string, from, to, worker string) {
	t.Helper()
	if err := pool.QueryRow(ctx, `SELECT ARRAY(SELECT jsonb_object_keys(payload) ORDER BY 1), COALESCE(payload->>'from','?'),
		COALESCE(payload->>'to','?'), COALESCE(payload->>'worker_id','')
		FROM task_events WHERE task_id=$1 AND event_type='working_state_changed' ORDER BY id DESC LIMIT 1`, id).
		Scan(&keys, &from, &to, &worker); err != nil {
		t.Fatalf("last working_state_changed of %d: %v", id, err)
	}
	return
}

func sgStr(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

// Set, refresh, change, clear, clear again — D10 (b), (c), (e).
func TestSignal_Integration_SetRefreshChangeClear(t *testing.T) {
	ctx := context.Background()
	pool := verbsPool(t, ctx)
	ex := queueMatrixExecutor(pool)
	proj := seedProject(t, ctx, pool, sgSlug, "itest-mcp-tools-signalclient")
	h := sgTask(t, ctx, pool, proj, "SIGNAL human task", "human", "ready")
	before := sgRead(t, ctx, pool, h)

	// set
	out, err := sgExec(ctx, ex, "mcp:manual:salvo", h, "working")
	if err != nil {
		t.Fatalf("task_signal working: %v", err)
	}
	var keys []string
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "changed,state,state_at,task_id" {
		t.Errorf("result keys = %v, want exactly [changed state state_at task_id] (D10 e)", keys)
	}
	if sgStr(out["state"]) != "working" || string(out["changed"]) != "true" || sgStr(out["state_at"]) == "" {
		t.Errorf("set result = %v, want state working, changed true, a state_at", out)
	}
	after := sgRead(t, ctx, pool, h)
	if after.state != "working" || after.stateAt == "" {
		t.Errorf("after set: (%q, %q), want (working, set)", after.state, after.stateAt)
	}
	if after.status != before.status || after.updatedAt != before.updatedAt || after.closedAt != "" ||
		after.surfacedAt != before.surfacedAt || after.priority != before.priority {
		t.Errorf("task_signal touched status/updated_at/closed/surfaced/priority: before %+v after %+v (D10 d)", before, after)
	}
	if n := sgEvents(t, ctx, pool, h); n != 1 {
		t.Errorf("working_state_changed events after set = %d, want 1", n)
	}
	if k, from, to, w := sgLastEvent(t, ctx, pool, h); strings.Join(k, ",") != "from,to,worker_id" || from != "" || to != "working" || w != "manual:salvo" {
		t.Errorf("set event = keys %v from %q to %q worker %q, want [from to worker_id] \"\" working manual:salvo (D10 b)", k, from, to, w)
	}

	// refresh: same state, timestamp moves, no event.
	if _, err := pool.Exec(ctx, `UPDATE tasks SET working_state_at = now() - interval '1 hour' WHERE id=$1`, h); err != nil {
		t.Fatalf("age: %v", err)
	}
	out, err = sgExec(ctx, ex, "mcp:manual:salvo", h, "working")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if string(out["changed"]) != "false" {
		t.Errorf("refresh changed = %s, want false (a refresh is not news)", out["changed"])
	}
	if n := sgCount(t, ctx, pool, `SELECT count(*) FROM tasks WHERE id=$1 AND working_state_at > now() - interval '1 minute'`, h); n != 1 {
		t.Errorf("refresh did not move working_state_at to now() (D6: set on every call with a state)")
	}
	if n := sgEvents(t, ctx, pool, h); n != 1 {
		t.Errorf("events after refresh = %d, want still 1", n)
	}

	// change
	out, err = sgExec(ctx, ex, "mcp:manual:salvo", h, "needs_input")
	if err != nil {
		t.Fatalf("change: %v", err)
	}
	if string(out["changed"]) != "true" || sgStr(out["state"]) != "needs_input" {
		t.Errorf("change result = %v", out)
	}
	if _, from, to, _ := sgLastEvent(t, ctx, pool, h); from != "working" || to != "needs_input" || sgEvents(t, ctx, pool, h) != 2 {
		t.Errorf("change event from %q to %q (events %d), want working → needs_input, 2 events", from, to, sgEvents(t, ctx, pool, h))
	}

	// clear
	out, err = sgExec(ctx, ex, "mcp:manual:salvo", h, "clear")
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if string(out["changed"]) != "true" || sgStr(out["state"]) != "" {
		t.Errorf("clear result = %v, want state \"\" changed true", out)
	}
	if r := sgRead(t, ctx, pool, h); r.state != "" || r.stateAt != "" {
		t.Errorf("after clear: (%q, %q), want both NULL (D10 c)", r.state, r.stateAt)
	}
	if _, from, to, _ := sgLastEvent(t, ctx, pool, h); from != "needs_input" || to != "" || sgEvents(t, ctx, pool, h) != 3 {
		t.Errorf("clear event from %q to %q, want needs_input → \"\" (3 events)", from, to)
	}

	// clear again: no-op success, no event.
	out, err = sgExec(ctx, ex, "mcp:manual:salvo", h, "clear")
	if err != nil {
		t.Fatalf("second clear: %v", err)
	}
	if string(out["changed"]) != "false" || sgEvents(t, ctx, pool, h) != 3 {
		t.Errorf("second clear changed %s, events %d; want false, 3", out["changed"], sgEvents(t, ctx, pool, h))
	}

	if n := sgCount(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE tool='task_signal' AND task_id=$1 AND status='ok'`, h); n != 5 {
		t.Errorf("ok audit rows = %d, want 5 (invariant 3: every call audited)", n)
	}
	for _, tbl := range []string{"task_claims", "feedback_requests", "deliveries"} {
		if n := sgCount(t, ctx, pool, `SELECT count(*) FROM `+tbl+` WHERE task_id=$1`, h); n != 0 {
			t.Errorf("%s rows for the signalled task = %d, want 0 (D10 d)", tbl, n)
		}
	}
	if r := sgRead(t, ctx, pool, h); r.status != "ready" || r.updatedAt != before.updatedAt {
		t.Errorf("after five signals status %q updated_at %q, want ready and unchanged %q", r.status, r.updatedAt, before.updatedAt)
	}
}

// D10 (a): holding and blocked accept; every other status refuses BY NAME;
// clear is accepted on any status.
func TestSignal_Integration_StatusRules(t *testing.T) {
	ctx := context.Background()
	pool := verbsPool(t, ctx)
	ex := queueMatrixExecutor(pool)
	proj := seedProject(t, ctx, pool, sgSlug, "itest-mcp-tools-signalclient")

	for _, st := range []string{"holding", "blocked"} {
		id := sgTask(t, ctx, pool, proj, "SIGNAL "+st, "human", st)
		if _, err := sgExec(ctx, ex, "mcp:manual:salvo", id, "working"); err != nil {
			t.Errorf("working on a %s human task: %v, want accepted (D7)", st, err)
		}
	}
	for _, tc := range []struct{ status, phrase string }{
		{"closed", "reopen it first"},
		{"claimed", "held by a claim; its status is its signal"},
		{"in_progress", "held by a claim; its status is its signal"},
		{"needs_feedback", "held by a claim; its status is its signal"},
		{"pr_open", "held by a claim; its status is its signal"},
		{"awaiting_ci", "held by a claim; its status is its signal"},
		{"awaiting_merge", "held by a claim; its status is its signal"},
		{"done_locally", "already done"},
		{"delivered", "already done"},
	} {
		id := sgTask(t, ctx, pool, proj, "SIGNAL refused "+tc.status, "human", tc.status)
		for _, state := range []string{"working", "needs_input"} {
			_, err := sgExec(ctx, ex, "mcp:manual:salvo", id, state)
			if err == nil {
				t.Errorf("%s on a %s task was accepted; D10 a refuses it", state, tc.status)
				continue
			}
			if !strings.Contains(err.Error(), tc.status) || !strings.Contains(err.Error(), tc.phrase) {
				t.Errorf("%s on %s refused with %q, want the status and %q by name", state, tc.status, err, tc.phrase)
			}
		}
		if r := sgRead(t, ctx, pool, id); r.state != "" || sgEvents(t, ctx, pool, id) != 0 {
			t.Errorf("refused %s task was marked (%q) or got events", tc.status, r.state)
		}
		if n := sgCount(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE tool='task_signal' AND task_id=$1 AND status='error'`, id); n != 2 {
			t.Errorf("error audit rows for the refused %s task = %d, want 2", tc.status, n)
		}
		out, err := sgExec(ctx, ex, "mcp:manual:salvo", id, "clear")
		if err != nil || string(out["changed"]) != "false" {
			t.Errorf("clear on an unmarked %s task = (%v, %v), want a no-op success (clear is accepted on any status)", tc.status, out, err)
		}
	}

	// Residue from an old binary: a closed task still marked. clear removes it.
	res := sgTask(t, ctx, pool, proj, "SIGNAL closed residue", "human", "closed")
	if _, err := pool.Exec(ctx, `UPDATE tasks SET working_state='needs_input', working_state_at=now() WHERE id=$1`, res); err != nil {
		t.Fatalf("seed residue: %v", err)
	}
	out, err := sgExec(ctx, ex, "opsctl:salvo", res, "clear")
	if err != nil || string(out["changed"]) != "true" {
		t.Errorf("clear on a closed task with a leftover marker = (%v, %v), want changed true", out, err)
	}
	if r := sgRead(t, ctx, pool, res); r.state != "" || r.stateAt != "" || r.status != "closed" {
		t.Errorf("after clear: %+v, want marker NULL and still closed", r)
	}
}

// D7: a claude task is refused for EVERY caller, humans included.
func TestSignal_Integration_ClaudeTaskRefusedForEveryCaller(t *testing.T) {
	ctx := context.Background()
	pool := verbsPool(t, ctx)
	ex := queueMatrixExecutor(pool)
	proj := seedProject(t, ctx, pool, sgSlug, "itest-mcp-tools-signalclient")
	c := sgTask(t, ctx, pool, proj, "SIGNAL claude task", "claude", "ready")
	for _, actor := range []string{"opsctl:salvo", "mcp:manual:salvo"} {
		for _, state := range []string{"working", "needs_input", "clear"} {
			_, err := sgExec(ctx, ex, actor, c, state)
			if err == nil {
				t.Errorf("%s %s on a claude task was accepted; D7: its in-progress signal is its claim", actor, state)
			} else if !strings.Contains(err.Error(), "human") {
				t.Errorf("%s %s on a claude task refused with %q, want it to say human tasks only", actor, state, err)
			}
		}
	}
	if r := sgRead(t, ctx, pool, c); r.state != "" || sgEvents(t, ctx, pool, c) != 0 {
		t.Errorf("claude task was marked or got events")
	}
}

// Criterion 18 / 21 through the real matrix: automated callers are refused
// human_only, audited as denied, and nothing moves.
func TestSignal_Integration_AutomatedCallersDenied(t *testing.T) {
	ctx := context.Background()
	pool := verbsPool(t, ctx)
	ex := queueMatrixExecutor(pool)
	proj := seedProject(t, ctx, pool, sgSlug, "itest-mcp-tools-signalclient")
	h := sgTask(t, ctx, pool, proj, "SIGNAL denied", "human", "ready")
	for _, actor := range []string{"mcp:acme", "mcp:acme.web", "orchestrator"} {
		_, err := sgExec(ctx, ex, actor, h, "working")
		if err == nil || !strings.Contains(err.Error(), "denied by policy (human_only)") {
			t.Errorf("task_signal by %s = %v, want denied by policy (human_only)", actor, err)
		}
	}
	if n := sgCount(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE tool='task_signal' AND task_id=$1 AND status='denied'`, h); n != 3 {
		t.Errorf("denied audit rows = %d, want 3", n)
	}
	if r := sgRead(t, ctx, pool, h); r.state != "" {
		t.Errorf("a denied call marked the task %q", r.state)
	}
}
