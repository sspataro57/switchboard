//go:build integration

package mcpserver_test

// MCP server protocol smoke (SPEC 04-mcp-task-tools, acceptance criterion 7),
// run IN-PROCESS against real Postgres rather than by hand-rolling the stdio
// JSON-RPC wire framing: the exact go-sdk wire format is pinned only at
// implementation time, and a wire-level test would fail on a correct server if
// the SDK framing differs from a pre-implementation guess. In-process is the
// option the task explicitly allows ("in-process (or via stdio pipes)"), and it
// still exercises the whole gate: mcpserver.Server -> executor.Execute -> real
// db, with an audit_events row as proof (criterion 2/7). The literal stdio
// initialize handshake is covered by the SPEC's manual smoke (verification
// protocol step 3).
//
// Build-tagged `integration`, env-gated on DATABASE_URL.
//
// GREENFIELD NOTE: package internal/mcpserver does not exist yet, so this
// compile-FAILs under `-tags integration`. Surface used is the same imposed in
// adapter_test.go (mcpserver.New / ListTools / CallTool) plus the shipped
// executor/policy/audit/tools/store packages.

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/mcpserver"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	smokeWorkerID = "itest-mcp-mcp-w1"
	smokeActor    = "mcp:itest-mcp-mcp-w1"
	smokeSlug     = "itest-mcp-mcp-smoke"
	smokeClient   = "itest-mcp-mcp-smokeclient"
)

func TestMCP_Integration_ListAndCallRoundTrip(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	defer pool.Close()
	cleanupSmoke(t, ctx, pool)

	// Seed a project + one ready claude task for task_get_next to return.
	var projectID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery)
		 VALUES ($1,$1,$2,'manual','dashboard') RETURNING id`,
		smokeSlug, smokeClient).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status, priority)
		 VALUES ($1,'smoke task','claude','ready',1)`, projectID); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	ex := executor.New(reg, policy.NewStatic(reg.Names()...), audit.NewPGStore(pool))
	srv := mcpserver.New(ex, smokeWorkerID)

	// tools/list: agent-facing allowlist present, spine tools absent.
	names := map[string]bool{}
	for _, tl := range srv.ListTools() {
		names[tl.Name] = true
	}
	for _, want := range []string{
		"create_task", "task_get_next", "task_claim", "task_context",
		"task_append_log", "request_feedback", "mark_done_local",
		"create_child_task", "record_decision",
	} {
		if !names[want] {
			t.Errorf("tools/list missing agent tool %q", want)
		}
	}
	for _, absent := range []string{"task_release", "answer_feedback"} {
		if names[absent] {
			t.Errorf("tools/list leaked spine-facing tool %q", absent)
		}
	}

	// tools/call task_get_next: round-trips to the executor and returns our task.
	out, err := srv.CallTool(ctx, "task_get_next", json.RawMessage(`{"client":"`+smokeClient+`"}`))
	if err != nil {
		t.Fatalf("CallTool(task_get_next): %v", err)
	}
	var next struct {
		Task *struct {
			ID    int64  `json:"id"`
			Title string `json:"title"`
		} `json:"task"`
	}
	if err := json.Unmarshal(out, &next); err != nil {
		t.Fatalf("unmarshal task_get_next output %s: %v", out, err)
	}
	if next.Task == nil || next.Task.Title != "smoke task" {
		t.Fatalf("task_get_next returned %+v, want the seeded smoke task", next.Task)
	}

	// The call produced an audit_events row under the MCP actor (invariant 3).
	var auditRows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE actor=$1 AND tool='task_get_next' AND status='ok'`,
		smokeActor).Scan(&auditRows); err != nil {
		t.Fatalf("count audit_events: %v", err)
	}
	if auditRows < 1 {
		t.Errorf("no ok audit_events row for task_get_next under actor %q", smokeActor)
	}
}

func cleanupSmoke(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	stmts := []string{
		`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE actor=$1)`,
		`DELETE FROM audit_events WHERE actor=$1`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s, smokeActor); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
	slugStmts := []string{
		`DELETE FROM task_events WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`,
		`DELETE FROM task_claims WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`,
		`DELETE FROM projects WHERE slug=$1`,
	}
	for _, s := range slugStmts {
		if _, err := pool.Exec(ctx, s, smokeSlug); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
}
