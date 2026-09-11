//go:build integration

package tools_test

// SWT-37 (docs/tickets/mcp-task-verbs_SPEC.md) criteria 22 and 23 against a
// real database: task_dismiss, task_close and task_mark_delivered through
// executor.Execute, the only route to a handler (invariant 3), with the policy
// decision and the audit trail asserted as rows.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run MCPVerbs ./internal/tools/
//
// NEVER point DATABASE_URL at production (192.168.50.49): this suite writes and
// deletes fixtures, and verbsPool refuses to run there.
//
// IMPOSED SURFACE: none new here. It drives the SPEC's V1 rule
// (`mcp_human_only`) through the production policy wiring.
//
// TRAP 1 — THE EXECUTOR. Every call runs on queueMatrixExecutor
// (tasklist_integration_test.go): policy.NewMatrix(policy.NewPGSnapshotLoader(pool),
// policy.NewStatic(reg.Names()...)), the production shape. NOT newExecutor
// (lifecycle_integration_test.go), which is static-only and would ALLOW every
// worker call. Mutation M-e (swap in newExecutor) must turn the refusal half red;
// that is the proof this test exercises the matrix.
//
// TRAP 2 — THE AUDIT FK. audit_events.task_id REFERENCES tasks(id) with NO
// cascade (migrations/0001_initial.sql), and every call here passes
// executor.Call.TaskID, so each audit row pins its task. The actors are
// mcp:manual:salvo, mcp:itest-mcp-tools-acme(.main), orchestrator and
// ticketstatus:jira, none of which matches cleanupToolsData's
// `actor LIKE 'itest-mcp-tools-%'`. Without verbsCleanup's first two statements
// the NEXT run's `DELETE FROM tasks` fails on the FK. Order: policy_decisions →
// audit_events (by task_id of an itest-mcp-tools- task) → cleanupToolsData.
// task_dismissals goes with its task (0022, ON DELETE CASCADE). Cleanup runs at
// START and in t.Cleanup; run the suite twice in a row to prove it.
//
// EXPECTED RED today, for the right reason: task_dismiss is already humanOnly,
// so its refusal rows pass; but task_close and task_mark_delivered by
// mcp:itest-mcp-tools-acme are ALLOWED by the static fallback (no
// mcp_human_only yet), so the refusal half fails ("was ALLOWED") and the
// target rows move. The allowed half then fails on the already-moved targets,
// which is noise from the first failure, not a second defect.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
)

const (
	verbsSlug   = "itest-mcp-tools-verbs"
	verbsClient = "itest-mcp-tools-verbsclient"
	verbsHuman  = "mcp:manual:salvo" // this repo's .mcp.json identity and the user-scope install's
)

// The REAL worker-console shapes (opsworker sets OPS_WORKER_ID to the bare
// --client value), with a test-owned client name.
var verbsWorkerActors = []string{"mcp:itest-mcp-tools-acme", "mcp:itest-mcp-tools-acme.main"}

func verbsPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("DATABASE_URL points at production (192.168.50.49); this suite writes and deletes " +
			"fixtures — use the compose db on :5433")
	}
	pool := newToolsPool(t, ctx) // skips when DATABASE_URL is unset
	verbsCleanup(t, ctx, pool)
	t.Cleanup(func() {
		verbsCleanup(t, ctx, pool)
		pool.Close()
	})
	return pool
}

// verbsCleanup: TRAP 2 above. Audit rows are scoped by the TASK they pin, not
// by actor, because the actors here are real shapes.
func verbsCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const ours = `(SELECT id FROM audit_events WHERE task_id IN
	                 (SELECT id FROM tasks WHERE project_id IN
	                   (SELECT id FROM projects WHERE slug LIKE 'itest-mcp-tools-%')))`
	for _, q := range []string{
		`DELETE FROM policy_decisions WHERE audit_event_id IN ` + ours,
		`DELETE FROM audit_events WHERE id IN ` + ours,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("verbs cleanup %q: %v", q, err)
		}
	}
	cleanupToolsData(t, ctx, pool)
}

func seedVerbTask(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID int64, title, status string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status) VALUES ($1,$2,'claude',$3) RETURNING id`,
		projectID, title, status).Scan(&id); err != nil {
		t.Fatalf("seed task %q (%s): %v", title, status, err)
	}
	return id
}

type verbTaskState struct {
	status     string
	updatedAt  time.Time
	events     int
	dismissals int
}

func readVerbState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) verbTaskState {
	t.Helper()
	var s verbTaskState
	if err := pool.QueryRow(ctx,
		`SELECT t.status, t.updated_at,
		        (SELECT count(*) FROM task_events WHERE task_id = t.id),
		        (SELECT count(*) FROM task_dismissals WHERE task_id = t.id)
		   FROM tasks t WHERE t.id = $1`, id).Scan(&s.status, &s.updatedAt, &s.events, &s.dismissals); err != nil {
		t.Fatalf("read state of task %d: %v", id, err)
	}
	return s
}

type verbAudit struct {
	status, decision, rule string
}

// verbAudits returns the audit rows ONE (task, tool, actor) produced, with the
// policy decision recorded against each.
func verbAudits(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64, tool, actor string) []verbAudit {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT a.status, COALESCE(pd.decision,''), COALESCE(pd.rule,'')
		   FROM audit_events a LEFT JOIN policy_decisions pd ON pd.audit_event_id = a.id
		  WHERE a.task_id = $1 AND a.tool = $2 AND a.actor = $3
		  ORDER BY a.id`, taskID, tool, actor)
	if err != nil {
		t.Fatalf("read audit rows for task %d %s %s: %v", taskID, tool, actor, err)
	}
	defer rows.Close()
	var out []verbAudit
	for rows.Next() {
		var a verbAudit
		if err := rows.Scan(&a.status, &a.decision, &a.rule); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audit rows: %v", err)
	}
	return out
}

// callVerb runs one verb through the executor with Call.TaskID set (the SPEC
// requires it: the audit row must pin the task for TRAP 2's cleanup).
func callVerb(ctx context.Context, ex *executor.Executor, actor, tool string, taskID int64, args string) error {
	id := taskID
	_, err := ex.Execute(ctx, executor.Call{Tool: tool, Actor: actor, Args: json.RawMessage(args), TaskID: &id})
	return err
}

func assertOneAudit(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64, tool, actor, wantStatus, wantDecision, wantRule string) {
	t.Helper()
	rows := verbAudits(t, ctx, pool, taskID, tool, actor)
	if len(rows) != 1 {
		t.Errorf("%s by %q on task %d left %d audit rows (%+v), want exactly 1 (invariant 3)", tool, actor, taskID, len(rows), rows)
		return
	}
	a := rows[0]
	if a.status != wantStatus {
		t.Errorf("%s by %q on task %d: audit status = %q, want %q", tool, actor, taskID, a.status, wantStatus)
	}
	if a.decision != wantDecision {
		t.Errorf("%s by %q on task %d: policy_decisions.decision = %q, want %q", tool, actor, taskID, a.decision, wantDecision)
	}
	if wantRule != "" && a.rule != wantRule {
		t.Errorf("%s by %q on task %d: policy_decisions.rule = %q, want %q", tool, actor, taskID, a.rule, wantRule)
	}
}

// Criterion 22.
func TestMCPVerbs_Integration_WorkerRefusedHumanAllowed(t *testing.T) {
	ctx := context.Background()
	pool := verbsPool(t, ctx)
	projectID := seedProject(t, ctx, pool, verbsSlug, verbsClient)
	a := seedVerbTask(t, ctx, pool, projectID, "itest verbs A", "ready")
	b := seedVerbTask(t, ctx, pool, projectID, "itest verbs B", "done_locally")
	c := seedVerbTask(t, ctx, pool, projectID, "itest verbs C", "ready")
	d := seedVerbTask(t, ctx, pool, projectID, "itest verbs D", "in_progress")

	ex := queueMatrixExecutor(pool) // TRAP 1: the production matrix, NEVER newExecutor

	// Each verb aimed at a target it WOULD move for an allowed caller, so a
	// missing refusal is visible as a moved row, not only as a missing error.
	verbs := []struct {
		tool   string
		target int64
		args   string
		rule   string
	}{
		{"task_close", a, fmt.Sprintf(`{"task_id":%d,"reason":"itest"}`, a), "mcp_human_only"},
		{"task_mark_delivered", b, fmt.Sprintf(`{"task_id":%d}`, b), "mcp_human_only"},
		{"task_dismiss", c, fmt.Sprintf(`{"task_id":%d,"reason_code":"duplicate"}`, c), "human_only"},
	}

	// ---- refusal half: worker consoles ------------------------------------
	for _, actor := range verbsWorkerActors {
		for _, v := range verbs {
			before := readVerbState(t, ctx, pool, v.target)
			err := callVerb(ctx, ex, actor, v.tool, v.target, v.args)
			if err == nil {
				t.Errorf("%s by worker %q was ALLOWED; want a policy denial (%s)", v.tool, actor, v.rule)
			} else if !strings.Contains(err.Error(), "denied by policy ("+v.rule+")") {
				t.Errorf("%s by worker %q failed with %q, want a denial naming the rule %s", v.tool, actor, err, v.rule)
			}
			after := readVerbState(t, ctx, pool, v.target)
			if after.status != before.status || !after.updatedAt.Equal(before.updatedAt) {
				t.Errorf("%s by worker %q moved task %d: %s@%s → %s@%s; a denial changes nothing", v.tool, actor,
					v.target, before.status, before.updatedAt, after.status, after.updatedAt)
			}
			if after.events != before.events {
				t.Errorf("%s by worker %q added %d task_events row(s) to task %d", v.tool, actor, after.events-before.events, v.target)
			}
			if after.dismissals != 0 {
				t.Errorf("%s by worker %q left %d task_dismissals row(s) on task %d: a worker minted a label", v.tool,
					actor, after.dismissals, v.target)
			}
			assertOneAudit(t, ctx, pool, v.target, v.tool, actor, "denied", "deny", v.rule)
		}
	}

	// ---- allowed half: the human session ----------------------------------
	if err := callVerb(ctx, ex, verbsHuman, "task_close", a, fmt.Sprintf(`{"task_id":%d,"reason":"itest"}`, a)); err != nil {
		t.Errorf("task_close A as %s: %v", verbsHuman, err)
	}
	if s := taskStatus(t, ctx, pool, a); s != "closed" {
		t.Errorf("after task_close A status = %q, want closed", s)
	}
	if n := eventCount(t, ctx, pool, a, "status_changed"); n != 1 {
		t.Errorf("task_close A wrote %d status_changed events, want 1", n)
	}

	if err := callVerb(ctx, ex, verbsHuman, "task_mark_delivered", b, fmt.Sprintf(`{"task_id":%d}`, b)); err != nil {
		t.Errorf("task_mark_delivered B as %s: %v", verbsHuman, err)
	}
	if s := taskStatus(t, ctx, pool, b); s != "delivered" {
		t.Errorf("after task_mark_delivered B status = %q, want delivered", s)
	}

	if err := callVerb(ctx, ex, verbsHuman, "task_dismiss", c,
		fmt.Sprintf(`{"task_id":%d,"reason_code":"duplicate","note":"itest"}`, c)); err != nil {
		t.Errorf("task_dismiss C as %s: %v", verbsHuman, err)
	}
	if s := taskStatus(t, ctx, pool, c); s != "closed" {
		t.Errorf("after task_dismiss C status = %q, want closed", s)
	}
	var code, note, by string
	if err := pool.QueryRow(ctx,
		`SELECT reason_code, COALESCE(note,''), dismissed_by FROM task_dismissals WHERE task_id=$1`, c).
		Scan(&code, &note, &by); err != nil {
		t.Errorf("read task_dismissals for C: %v", err)
	} else {
		if code != "duplicate" || note != "itest" {
			t.Errorf("task_dismissals for C = (%q, %q), want (duplicate, itest)", code, note)
		}
		if by != verbsHuman {
			t.Errorf("task_dismissals.dismissed_by = %q, want %q EXACTLY: the actor is recorded unmodified, "+
				"so the mcp: tier of labelled data stays separable (V7)", by, verbsHuman)
		}
	}

	for _, x := range []struct {
		task int64
		tool string
	}{{a, "task_close"}, {b, "task_mark_delivered"}, {c, "task_dismiss"}} {
		assertOneAudit(t, ctx, pool, x.task, x.tool, verbsHuman, "ok", "allow", "")
	}

	// ---- the handler guards still bind on the new surface ------------------
	err := callVerb(ctx, ex, verbsHuman, "task_close", d, fmt.Sprintf(`{"task_id":%d,"reason":"itest"}`, d))
	if err == nil || !strings.Contains(err.Error(), "refusing to close active work") {
		t.Errorf("task_close D (in_progress) as %s = %v, want the handler's \"refusing to close active work\"", verbsHuman, err)
	}
	if s := taskStatus(t, ctx, pool, d); s != "in_progress" {
		t.Errorf("after the refused close D status = %q, want in_progress", s)
	}
	assertOneAudit(t, ctx, pool, d, "task_close", verbsHuman, "error", "allow", "")

	// A is closed now: marking it delivered is an idempotent no-op success.
	if err := callVerb(ctx, ex, verbsHuman, "task_mark_delivered", a, fmt.Sprintf(`{"task_id":%d}`, a)); err != nil {
		t.Errorf("task_mark_delivered on closed A = %v, want an ok no-op (idempotent)", err)
	}
	if s := taskStatus(t, ctx, pool, a); s != "closed" {
		t.Errorf("after mark_delivered on closed A status = %q, want closed", s)
	}
	assertOneAudit(t, ctx, pool, a, "task_mark_delivered", verbsHuman, "ok", "allow", "")

	// A fresh ready task: only done_locally moves.
	e := seedVerbTask(t, ctx, pool, projectID, "itest verbs E", "ready")
	if err := callVerb(ctx, ex, verbsHuman, "task_mark_delivered", e, fmt.Sprintf(`{"task_id":%d}`, e)); err == nil {
		t.Errorf("task_mark_delivered on a ready task succeeded; only done_locally transitions to delivered")
	}
	if s := taskStatus(t, ctx, pool, e); s != "ready" {
		t.Errorf("after the refused mark_delivered E status = %q, want ready", s)
	}
	assertOneAudit(t, ctx, pool, e, "task_mark_delivered", verbsHuman, "error", "allow", "")
}

// Criterion 23. The spine's own callers are untouched: the orchestrator (R2,
// R8) and the Jira reconciler close and deliver through the SAME matrix
// executor, and their policy_decisions.rule is exactly static-default — what
// it is on main today. Mutation M-c (drop the MCP-prefix condition) and M-d
// (task_close into humanOnly) turn this red.
func TestMCPVerbs_Integration_SpineCallersUnaffected(t *testing.T) {
	ctx := context.Background()
	pool := verbsPool(t, ctx)
	projectID := seedProject(t, ctx, pool, verbsSlug+"-spine", verbsClient+"-spine")
	ex := queueMatrixExecutor(pool)

	for _, actor := range []string{"orchestrator", "ticketstatus:jira"} {
		f := seedVerbTask(t, ctx, pool, projectID, "itest verbs spine "+actor, "done_locally")

		if err := callVerb(ctx, ex, actor, "task_mark_delivered", f, fmt.Sprintf(`{"task_id":%d}`, f)); err != nil {
			t.Errorf("task_mark_delivered as %q: %v — R8 calls this in-process", actor, err)
		}
		if s := taskStatus(t, ctx, pool, f); s != "delivered" {
			t.Errorf("after task_mark_delivered as %q status = %q, want delivered", actor, s)
		}
		if err := callVerb(ctx, ex, actor, "task_close", f, fmt.Sprintf(`{"task_id":%d,"reason":"itest spine"}`, f)); err != nil {
			t.Errorf("task_close as %q: %v — R2/R8 and the reconciler call this in-process", actor, err)
		}
		if s := taskStatus(t, ctx, pool, f); s != "closed" {
			t.Errorf("after task_close as %q status = %q, want closed", actor, s)
		}
		for _, tool := range []string{"task_mark_delivered", "task_close"} {
			assertOneAudit(t, ctx, pool, f, tool, actor, "ok", "allow", "static-default")
		}
	}
}
