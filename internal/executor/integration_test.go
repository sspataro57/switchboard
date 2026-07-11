//go:build integration

package executor_test

// Integration test (SPEC 01-schema-executor, acceptance criterion 5): one real
// Execute call against the dockerized local Postgres writes real audit_events
// and policy_decisions rows through the pg-backed stores.
//
// Build-tagged `integration` AND env-gated on DATABASE_URL: excluded from the
// default `go test ./...` (which must stay zero-network), and skips cleanly
// when the DB env is not set. Run with:
//
//   DATABASE_URL=postgres://…/ops go test -tags integration ./internal/executor/
//
// GREENFIELD NOTE: the referenced packages/constructors do not exist yet, so
// under `-tags integration` this compile-FAILs. Expected surface:
//
//   func store.NewPool(ctx context.Context) (*pgxpool.Pool, error)  // reads DATABASE_URL
//   func audit.NewPGStore(pool *pgxpool.Pool) audit.Store
//   func policy.NewStatic(registered ...string) policy.Checker
//   func tools.Register(reg *executor.Registry, pool *pgxpool.Pool)  // wires create_task
//
// pgx types are used only via inference (no direct pgx import here).

import (
	"context"
	"os"
	"testing"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

func TestExecutor_Integration_WritesAuditAndPolicyRows(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()

	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	defer pool.Close()

	// Clean up leftovers from previous runs so the test is rerunnable against a
	// persistent db — FK order: policy_decisions -> audit_events -> tasks -> projects.
	const slug = "integ-test-executor"
	const actor = "integration-test"
	if _, err := pool.Exec(ctx,
		`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE actor=$1)`, actor); err != nil {
		t.Fatalf("cleanup policy_decisions: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM audit_events WHERE actor=$1`, actor); err != nil {
		t.Fatalf("cleanup audit_events: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, slug); err != nil {
		t.Fatalf("cleanup tasks: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM projects WHERE slug=$1`, slug); err != nil {
		t.Fatalf("cleanup projects: %v", err)
	}

	// Seed a project to satisfy create_task's slug -> id resolution.
	var projectID int64
	err = pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery)
		 VALUES ($1, $2, $3, 'manual', 'dashboard') RETURNING id`,
		"Integ Test Executor", slug, "IntegClient").Scan(&projectID)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	ex := executor.New(reg, policy.NewStatic(reg.Names()...), audit.NewPGStore(pool))

	args := []byte(`{"project":"` + slug + `","title":"integration hello"}`)
	if _, err := ex.Execute(ctx, executor.Call{Tool: "create_task", Actor: actor, Args: args}); err != nil {
		t.Fatalf("Execute create_task: %v", err)
	}

	// A tasks row was created through the executor.
	var taskCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE project_id=$1 AND status='ready' AND title='integration hello'`,
		projectID).Scan(&taskCount); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if taskCount != 1 {
		t.Errorf("tasks rows = %d, want 1", taskCount)
	}

	// An audit_events row: status ok for this actor/tool.
	var auditCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE actor=$1 AND tool='create_task' AND status='ok' AND started_at IS NOT NULL`,
		actor).Scan(&auditCount); err != nil {
		t.Fatalf("count audit_events: %v", err)
	}
	if auditCount != 1 {
		t.Errorf("audit_events rows = %d, want 1", auditCount)
	}

	// A policy_decisions row: allow for create_task.
	var policyCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM policy_decisions WHERE tool='create_task' AND decision='allow'`).Scan(&policyCount); err != nil {
		t.Fatalf("count policy_decisions: %v", err)
	}
	if policyCount < 1 {
		t.Errorf("policy_decisions allow rows = %d, want >= 1", policyCount)
	}
}
