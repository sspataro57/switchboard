//go:build integration

package mcpserver_test

// mcp-task-reopen (docs/tickets/mcp-task-reopen_SPEC.md, 2026-11-10) criteria
// 16, 17 and 18 against a real database: task_reopen through the MCP adapter
// over the PRODUCTION executor wiring (udExecutor: policy.NewMatrix in front of
// the real registry's static allow-list — never a static-only executor, which
// allows every worker call; IK SWT-37 LANDMINE).
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_<branch>?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run TaskReopen_Integration ./internal/mcpserver/
//
// Criterion 16 runs as the worker id the SPEC names, manual:salvo, because
// task_dismissals.reopened_by = 'mcp:manual:salvo' is the provenance being
// pinned. MCP audit rows carry a NULL task_id (IK), and that actor is shared
// with every real session, so cleanup scopes audit rows by args->>'task_id'
// over THIS test's tasks (plus the test-owned dismisser actor), never by actor
// alone — the ucCleanup shape with the FK order it needs (policy_decisions →
// audit_events → task_events → tasks → projects).
//
// GREENFIELD NOTE — EXPECTED RED: until the profiles list task_reopen the
// adapter answers "not available over MCP" (criteria 16, 17 fail on that); until
// mcpHumanOnly gains it, acme's call is ALLOWED and reopens the task (17); until
// the adapter guard exists, a message_id call reaches the executor and leaves an
// audit row (18).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/mcpserver"
)

const (
	rrSlug      = "itest-mcp-reopen-proj"
	rrDismisser = "opsctl:itest-mcp-reopen" // human: task_dismiss is humanOnly
	rrSession   = "manual:salvo"            // criterion 16's worker id, verbatim
	rrWorker    = "acme"                    // criterion 17's worker console
)

func rrPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	pool := ucPool(t, ctx) // skips without DATABASE_URL; refuses the real ops db
	rrCleanup(t, ctx, pool)
	t.Cleanup(func() { rrCleanup(t, ctx, pool) })
	return pool
}

func rrCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const projs = `(SELECT id FROM projects WHERE slug = '` + rrSlug + `')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	const audits = `(SELECT id FROM audit_events WHERE actor LIKE '%itest-mcp-reopen%' OR task_id IN ` + tasksOf +
		` OR (args->>'task_id') IN (SELECT id::text FROM tasks WHERE project_id IN ` + projs + `))`
	for _, q := range []string{
		`DELETE FROM policy_decisions WHERE audit_event_id IN ` + audits,
		`DELETE FROM audit_events WHERE id IN ` + audits,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM projects WHERE slug = '` + rrSlug + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

// rrClosedHumanTask seeds a ready HUMAN task and dismisses it through the real
// executor (not by UPDATE), so closed_at, closed_from_status and the open
// task_dismissals row are exactly what production writes.
func rrClosedHumanTask(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ex *executor.Executor, title string) int64 {
	t.Helper()
	var proj int64
	if err := pool.QueryRow(ctx, `SELECT id FROM projects WHERE slug=$1`, rrSlug).Scan(&proj); err != nil {
		if err := pool.QueryRow(ctx, `INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
			VALUES ($1,$1,'itest-mcp-reopen-client','manual','dashboard','/tmp/itest','any') RETURNING id`, rrSlug).Scan(&proj); err != nil {
			t.Fatalf("seed project: %v", err)
		}
	}
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO tasks (project_id, title, assignee_type, status)
		VALUES ($1,$2,'human','ready') RETURNING id`, proj, title).Scan(&id); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if _, err := ex.Execute(ctx, executor.Call{
		Tool: "task_dismiss", Actor: rrDismisser, TaskID: &id,
		Args: []byte(fmt.Sprintf(`{"task_id":%d,"reason_code":"duplicate","note":"itest-mcp-reopen"}`, id)),
	}); err != nil {
		t.Fatalf("CONTROL: dismissing task %d through the executor: %v", id, err)
	}
	if s := rrStatus(t, ctx, pool, id); s != "closed" {
		t.Fatalf("CONTROL: after the dismiss task %d is %s, want closed", id, s)
	}
	if n := rrOpenDismissals(t, ctx, pool, id); n != 1 {
		t.Fatalf("CONTROL: task %d has %d open dismissals after the dismiss, want 1", id, n)
	}
	return id
}

func rrStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatalf("status of %d: %v", id, err)
	}
	return s
}

func rrOpenDismissals(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_dismissals WHERE task_id=$1 AND reopened_at IS NULL`, id).Scan(&n); err != nil {
		t.Fatalf("open dismissals of %d: %v", id, err)
	}
	return n
}

func rrStatusEvents(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='status_changed'`, id).Scan(&n); err != nil {
		t.Fatalf("status events of %d: %v", id, err)
	}
	return n
}

// rrReopenAudits counts task_reopen audit rows for this task (by args, since the
// adapter sets no TaskID), optionally of one status.
func rrReopenAudits(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64, status string) int {
	t.Helper()
	q := `SELECT count(*) FROM audit_events WHERE tool='task_reopen' AND args->>'task_id' = $1`
	args := []any{fmt.Sprint(id)}
	if status != "" {
		q += ` AND status = $2`
		args = append(args, status)
	}
	var n int
	if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("task_reopen audit rows for %d: %v", id, err)
	}
	return n
}

type rrResult struct {
	TaskID   int64  `json:"task_id"`
	Status   string `json:"status"`
	Reopened bool   `json:"reopened"`
}

func rrDecode(t *testing.T, out json.RawMessage) rrResult {
	t.Helper()
	var r rrResult
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("task_reopen output %s: %v", out, err)
	}
	return r
}

// Criterion 16: a closed HUMAN task with an open dismissal, reopened through
// ProfileUser as manual:salvo — the full end state, the idempotent second call,
// and status:"blocked" landing blocked.
func TestTaskReopen_Integration_UserProfilePlainReopen(t *testing.T) {
	ctx := context.Background()
	pool := rrPool(t, ctx)
	ex := udExecutor(pool)
	user := mcpserver.NewWithProfile(ex, rrSession, mcpserver.ProfileUser)

	id := rrClosedHumanTask(t, ctx, pool, ex, "itest reopen plain")
	eventsBefore := rrStatusEvents(t, ctx, pool, id)

	out, err := user.CallTool(ctx, reopenName, json.RawMessage(fmt.Sprintf(`{"task_id":%d,"reason":"more to add"}`, id)))
	if err != nil {
		t.Fatalf("user-profile task_reopen as %s: %v (criterion 16)", rrSession, err)
	}
	if r := rrDecode(t, out); r.TaskID != id || r.Status != "ready" || !r.Reopened {
		t.Errorf("task_reopen result = %s, want {task_id:%d, status:ready, reopened:true}", out, id)
	}

	var status string
	var closedAtNull, closedFromNull, surfacedSet, surfacedMsgNull bool
	if err := pool.QueryRow(ctx, `SELECT status, closed_at IS NULL, closed_from_status IS NULL,
		surfaced_at IS NOT NULL, surfaced_by_message_id IS NULL FROM tasks WHERE id=$1`, id).
		Scan(&status, &closedAtNull, &closedFromNull, &surfacedSet, &surfacedMsgNull); err != nil {
		t.Fatalf("read task %d: %v", id, err)
	}
	if status != "ready" || !closedAtNull || !closedFromNull {
		t.Errorf("task %d = (status %s, closed_at NULL %v, closed_from_status NULL %v), want (ready, true, true)",
			id, status, closedAtNull, closedFromNull)
	}
	if !surfacedSet || !surfacedMsgNull {
		t.Errorf("task %d surfaced_at set = %v, surfaced_by_message_id NULL = %v; want true, true (F3/F4: "+
			"mcp:manual:salvo is human, so the plain reopen surfaces it and the Jira status sync holds it)",
			id, surfacedSet, surfacedMsgNull)
	}

	var reopenedSet, msgNull bool
	var reopenedBy string
	if err := pool.QueryRow(ctx, `SELECT reopened_at IS NOT NULL, COALESCE(reopened_by,''), reopened_by_message_id IS NULL
		FROM task_dismissals WHERE task_id=$1 ORDER BY id DESC LIMIT 1`, id).Scan(&reopenedSet, &reopenedBy, &msgNull); err != nil {
		t.Fatalf("read dismissal of %d: %v", id, err)
	}
	if !reopenedSet || reopenedBy != "mcp:manual:salvo" || !msgNull {
		t.Errorf("dismissal of %d = (reopened_at set %v, reopened_by %q, reopened_by_message_id NULL %v), want "+
			"(true, \"mcp:manual:salvo\", true) — the provenance of a session's reopen", id, reopenedSet, reopenedBy, msgNull)
	}

	if got := rrStatusEvents(t, ctx, pool, id); got != eventsBefore+1 {
		t.Errorf("status_changed events on %d went %d → %d, want exactly one new", id, eventsBefore, got)
	}
	var from, to, reason string
	if err := pool.QueryRow(ctx, `SELECT payload->>'from', payload->>'to', payload->>'reason' FROM task_events
		WHERE task_id=$1 AND event_type='status_changed' ORDER BY id DESC LIMIT 1`, id).Scan(&from, &to, &reason); err != nil {
		t.Fatalf("read status event of %d: %v", id, err)
	}
	if from != "closed" || to != "ready" || reason != "more to add" {
		t.Errorf("status_changed = {from:%s, to:%s, reason:%q}, want {from:closed, to:ready, reason:\"more to add\"}", from, to, reason)
	}
	if n := rrReopenAudits(t, ctx, pool, id, "ok"); n != 1 {
		t.Errorf("task_reopen audit rows with status ok for %d = %d, want 1 (invariant 3)", id, n)
	}
	var actor string
	if err := pool.QueryRow(ctx, `SELECT actor FROM audit_events WHERE tool='task_reopen' AND args->>'task_id'=$1
		ORDER BY id DESC LIMIT 1`, fmt.Sprint(id)).Scan(&actor); err != nil {
		t.Fatalf("read task_reopen audit actor: %v", err)
	}
	if actor != "mcp:manual:salvo" {
		t.Errorf("task_reopen audit actor = %q, want mcp:manual:salvo", actor)
	}

	// The second call: reopened:false, no event.
	eventsAfter := rrStatusEvents(t, ctx, pool, id)
	out, err = user.CallTool(ctx, reopenName, json.RawMessage(fmt.Sprintf(`{"task_id":%d,"reason":"again"}`, id)))
	if err != nil {
		t.Fatalf("second task_reopen: %v (an open task is an idempotent success, F3)", err)
	}
	if r := rrDecode(t, out); r.Reopened || r.Status != "ready" {
		t.Errorf("second task_reopen = %s, want {status:ready, reopened:false}", out)
	}
	if got := rrStatusEvents(t, ctx, pool, id); got != eventsAfter {
		t.Errorf("the second (no-op) reopen wrote a status event: %d → %d", eventsAfter, got)
	}

	// status:"blocked" lands blocked.
	b := rrClosedHumanTask(t, ctx, pool, ex, "itest reopen blocked")
	out, err = user.CallTool(ctx, reopenName, json.RawMessage(fmt.Sprintf(`{"task_id":%d,"reason":"waiting on X","status":"blocked"}`, b)))
	if err != nil {
		t.Fatalf("task_reopen status=blocked: %v", err)
	}
	if r := rrDecode(t, out); r.Status != "blocked" || !r.Reopened {
		t.Errorf("task_reopen status=blocked result = %s, want {status:blocked, reopened:true}", out)
	}
	if s := rrStatus(t, ctx, pool, b); s != "blocked" {
		t.Errorf("task %d after reopen status=blocked is %s, want blocked", b, s)
	}
}

// Criterion 17: the same kind of closed task through ProfileFull as worker
// acme: an MCP error, one denied audit row whose policy_decisions row is
// deny/mcp_human_only, and nothing changed.
func TestTaskReopen_Integration_WorkerConsoleDenied(t *testing.T) {
	ctx := context.Background()
	pool := rrPool(t, ctx)
	ex := udExecutor(pool)
	id := rrClosedHumanTask(t, ctx, pool, ex, "itest reopen worker")

	full := mcpserver.NewWithProfile(ex, rrWorker, mcpserver.ProfileFull)
	_, err := full.CallTool(ctx, reopenName, json.RawMessage(fmt.Sprintf(`{"task_id":%d,"reason":"smoke"}`, id)))
	if err == nil {
		t.Fatalf("task_reopen as worker %q succeeded; want a policy denial (criterion 17, mcp_human_only)", rrWorker)
	}
	if !strings.Contains(err.Error(), "mcp_human_only") {
		t.Errorf("worker task_reopen error = %q, want it to name mcp_human_only (criterion 17)", err)
	}

	var auditID int64
	var auditStatus, actor string
	if err := pool.QueryRow(ctx, `SELECT id, status, actor FROM audit_events WHERE tool='task_reopen' AND args->>'task_id'=$1
		ORDER BY id DESC LIMIT 1`, fmt.Sprint(id)).Scan(&auditID, &auditStatus, &actor); err != nil {
		t.Fatalf("no task_reopen audit row for task %d: %v (invariant 3: a denial is audited)", id, err)
	}
	if n := rrReopenAudits(t, ctx, pool, id, ""); n != 1 {
		t.Errorf("task_reopen audit rows for %d = %d, want exactly 1", id, n)
	}
	if auditStatus != "denied" || actor != "mcp:"+rrWorker {
		t.Errorf("audit row = (status %s, actor %s), want (denied, mcp:%s)", auditStatus, actor, rrWorker)
	}
	var decision, rule string
	if err := pool.QueryRow(ctx, `SELECT decision, COALESCE(rule,'') FROM policy_decisions WHERE audit_event_id=$1`, auditID).
		Scan(&decision, &rule); err != nil {
		t.Fatalf("policy_decisions row for audit %d: %v", auditID, err)
	}
	if decision != "deny" || rule != "mcp_human_only" {
		t.Errorf("policy_decisions = %s/%s, want deny/mcp_human_only (criterion 17)", decision, rule)
	}
	if s := rrStatus(t, ctx, pool, id); s != "closed" {
		t.Errorf("task %d is %s after the denied reopen, want still closed", id, s)
	}
	if n := rrOpenDismissals(t, ctx, pool, id); n != 1 {
		t.Errorf("task %d has %d open dismissals after the denied reopen, want 1 (still open)", id, n)
	}
}

// Criterion 18: a call that carries message_id, on either profile, is refused
// before the executor runs — no audit_events row, no change to the task. The
// args are the GUARDED form with a real dismissal id, i.e. a call the handler
// would otherwise validate and attempt.
func TestTaskReopen_Integration_MessageIDRefusedBeforeTheExecutor(t *testing.T) {
	ctx := context.Background()
	pool := rrPool(t, ctx)
	ex := udExecutor(pool)
	for _, p := range []struct {
		name    string
		profile mcpserver.Profile
	}{{"full", mcpserver.ProfileFull}, {"user", mcpserver.ProfileUser}} {
		id := rrClosedHumanTask(t, ctx, pool, ex, "itest reopen guarded "+p.name)
		var dismissal int64
		if err := pool.QueryRow(ctx, `SELECT id FROM task_dismissals WHERE task_id=$1 AND reopened_at IS NULL`, id).Scan(&dismissal); err != nil {
			t.Fatalf("open dismissal of %d: %v", id, err)
		}
		eventsBefore := rrStatusEvents(t, ctx, pool, id)
		srv := mcpserver.NewWithProfile(ex, rrSession, p.profile)
		args := fmt.Sprintf(`{"task_id":%d,"reason":"x","dismissal_id":%d,"message_id":1}`, id, dismissal)
		if _, err := srv.CallTool(ctx, reopenName, json.RawMessage(args)); err == nil {
			t.Errorf("%s profile: task_reopen with message_id succeeded; want the adapter's refusal (criterion 18)", p.name)
		}
		if n := rrReopenAudits(t, ctx, pool, id, ""); n != 0 {
			t.Errorf("%s profile: %d task_reopen audit rows for %d, want 0 — the guard refuses BEFORE the executor "+
				"(criterion 18)", p.name, n, id)
		}
		if s := rrStatus(t, ctx, pool, id); s != "closed" {
			t.Errorf("%s profile: task %d is %s, want still closed", p.name, id, s)
		}
		if n := rrOpenDismissals(t, ctx, pool, id); n != 1 {
			t.Errorf("%s profile: task %d has %d open dismissals, want 1", p.name, id, n)
		}
		if got := rrStatusEvents(t, ctx, pool, id); got != eventsBefore {
			t.Errorf("%s profile: status events on %d went %d → %d, want unchanged", p.name, id, eventsBefore, got)
		}
	}
}
