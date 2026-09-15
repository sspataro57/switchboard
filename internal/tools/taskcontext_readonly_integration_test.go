//go:build integration

package tools_test

// SWT-56 (docs/tickets/signal-session-name_SPEC.md) criterion 29, the
// integration half: with require_read_only:"true" task_context leaves a claimed
// (or needs_feedback) task exactly as it is, EVEN WITH the holder's worker_id.
// Each of S12's two layers is exercised ALONE here at the handler:
//   - the flag alone: worker_id is the matching holder, only the flag stops the flip;
//   - an empty worker_id alone: no flag, and nothing flips (today's condition,
//     pinned so a refactor that falls back to the actor goes red here);
//   - the control: neither layer, and the holder's fetch flips to in_progress as today.
//
// The adapter half (the profile pin forces worker_id "" and the flag) is
// internal/mcpserver user_context_test.go (unit) and
// user_context_integration_test.go (through the profile, real executor).
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_isosess?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run TaskContextReadOnly ./internal/tools/
//
// GREENFIELD NOTE — EXPECTED RED: taskContext has no require_read_only branch,
// so the flag-alone steps flip the task to in_progress.
//
// MUTATION: remove the handler's require_read_only branch → the flag-alone steps.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
)

const ctxWorker = "manual:salvo"

func ctxExec(ctx context.Context, ex *executor.Executor, tool string, task int64, args map[string]any) error {
	raw, _ := json.Marshal(args)
	_, err := ex.Execute(ctx, executor.Call{Tool: tool, Actor: "mcp:" + ctxWorker, Args: raw, TaskID: &task})
	return err
}

// ctxClaims is the task's claim rows, verbatim, so "unchanged" means unchanged.
func ctxClaims(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT id::text || '|' || worker_id || '|' || COALESCE(expires_at::text,'') || '|' ||
		COALESCE(released_at::text,'') FROM task_claims WHERE task_id=$1 ORDER BY id`, id)
	if err != nil {
		t.Fatalf("claims: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan claim: %v", err)
		}
		out = append(out, s)
	}
	return strings.Join(out, ";")
}

func ctxStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) (status string, changes int) {
	t.Helper()
	if err := pool.QueryRow(ctx, `SELECT status, (SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='status_changed')
		FROM tasks WHERE id=$1`, id).Scan(&status, &changes); err != nil {
		t.Fatalf("status of %d: %v", id, err)
	}
	return
}

func TestTaskContextReadOnly_Integration_EachGuardAlone(t *testing.T) {
	ctx := context.Background()
	pool := verbsPool(t, ctx)
	ex := queueMatrixExecutor(pool)
	proj := seedProject(t, ctx, pool, "itest-mcp-tools-ctxro", "itest-mcp-tools-ctxroclient")
	id := sgTask(t, ctx, pool, proj, "CTXRO human task", "human", "ready")

	if err := ctxExec(ctx, ex, "task_claim", id, map[string]any{"task_id": id, "worker_id": ctxWorker}); err != nil {
		t.Fatalf("task_claim as %s: %v", ctxWorker, err)
	}
	if s, _ := ctxStatus(t, ctx, pool, id); s != "claimed" {
		t.Fatalf("CONTROL: after the claim status = %q, want claimed", s)
	}

	readOnly := func(step, want string) {
		t.Helper()
		claims := ctxClaims(t, ctx, pool, id)
		_, changes := ctxStatus(t, ctx, pool, id)
		// The flag ALONE: the holder's own worker_id rides along.
		if err := ctxExec(ctx, ex, "task_context", id,
			map[string]any{"task_id": id, "worker_id": ctxWorker, "require_read_only": "true"}); err != nil {
			t.Fatalf("%s: task_context with require_read_only: %v", step, err)
		}
		s, n := ctxStatus(t, ctx, pool, id)
		if s != want {
			t.Errorf("%s: require_read_only:\"true\" with the holder's worker_id moved the task %s → %s. Criterion 29: "+
				"the flag skips the transition block whatever worker_id says", step, want, s)
		}
		if n != changes {
			t.Errorf("%s: a read-only task_context wrote a status_changed event (%d → %d)", step, changes, n)
		}
		if got := ctxClaims(t, ctx, pool, id); got != claims {
			t.Errorf("%s: a read-only task_context changed the claim rows: %s → %s", step, claims, got)
		}
		// An empty worker_id ALONE (S12 layer 1 at the handler): no flag, no flip.
		if err := ctxExec(ctx, ex, "task_context", id, map[string]any{"task_id": id, "worker_id": ""}); err != nil {
			t.Fatalf("%s: task_context with worker_id \"\": %v", step, err)
		}
		if s, n := ctxStatus(t, ctx, pool, id); s != want || n != changes {
			t.Errorf("%s: task_context with an empty worker_id moved the task %s → %s (events %d → %d)", step, want, s, changes, n)
		}
	}
	control := func(step, from string) {
		t.Helper()
		_, changes := ctxStatus(t, ctx, pool, id)
		if err := ctxExec(ctx, ex, "task_context", id, map[string]any{"task_id": id, "worker_id": ctxWorker}); err != nil {
			t.Fatalf("%s: holder task_context: %v", step, err)
		}
		if s, n := ctxStatus(t, ctx, pool, id); s != "in_progress" || n != changes+1 {
			t.Errorf("CONTROL %s: the holder's fetch without the flag left %s → %s (events %d → %d); it must still start "+
				"work as today (lifecycle steps 5 and 9)", step, from, s, changes, n)
		}
	}

	readOnly("claimed", "claimed")
	control("claimed", "claimed")

	if err := ctxExec(ctx, ex, "request_feedback", id,
		map[string]any{"task_id": id, "worker_id": ctxWorker, "question": "itest: which approach?"}); err != nil {
		t.Fatalf("request_feedback: %v", err)
	}
	if s, _ := ctxStatus(t, ctx, pool, id); s != "needs_feedback" {
		t.Fatalf("CONTROL: after request_feedback status = %q, want needs_feedback", s)
	}
	readOnly("needs_feedback", "needs_feedback")
	control("needs_feedback", "needs_feedback")

	// A value other than "" / "true" is refused by name, before the handler.
	err := ctxExec(ctx, ex, "task_context", id, map[string]any{"task_id": id, "require_read_only": "false"})
	if err == nil || !strings.Contains(err.Error(), "require_read_only") {
		t.Errorf("task_context with require_read_only \"false\" = %v, want a refusal naming require_read_only", err)
	}
}

// SWT-56 smoke finding (2026-09-15): projects.client is nullable, and personal,
// bulk and homelab have none on prod. task_context scanned p.client into a plain
// string, so every task in a client-less project failed to read ("cannot scan
// NULL into *string") — worker consoles never met it (they work client
// projects), but the user profile now reads any task (Q1 "Show everything").
// MUTATION: drop the COALESCE on p.client → this test.
func TestTaskContext_Integration_ClientlessProjectReads(t *testing.T) {
	ctx := context.Background()
	pool := verbsPool(t, ctx)
	ex := queueMatrixExecutor(pool)
	proj := seedProject(t, ctx, pool, "itest-mcp-tools-ctxnoclient", "itest-mcp-tools-ctxnoclientclient")
	if _, err := pool.Exec(ctx, `UPDATE projects SET client = NULL WHERE id = $1`, proj); err != nil {
		t.Fatalf("null the client: %v", err)
	}
	id := sgTask(t, ctx, pool, proj, "CTXNOCLIENT human task", "human", "ready")

	raw, _ := json.Marshal(map[string]any{"task_id": id, "require_read_only": "true"})
	res, err := ex.Execute(ctx, executor.Call{Tool: "task_context", Actor: "mcp:" + ctxWorker, Args: raw, TaskID: &id})
	if err != nil {
		t.Fatalf("task_context on a task in a project with no client: %v (a user-scope session must be able to read "+
			"personal, bulk and homelab tasks)", err)
	}
	var doc struct {
		Task    map[string]any `json:"task"`
		Project map[string]any `json:"project"`
	}
	if err := json.Unmarshal(res.Output, &doc); err != nil {
		t.Fatalf("decode the document: %v", err)
	}
	if doc.Task["title"] != "CTXNOCLIENT human task" {
		t.Errorf("task.title = %v, want the seeded title", doc.Task["title"])
	}
	if c, ok := doc.Project["client"]; !ok || c != "" {
		t.Errorf("project.client = %#v (present %v), want \"\" for a project with no client", c, ok)
	}
}
