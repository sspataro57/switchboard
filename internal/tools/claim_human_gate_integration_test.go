//go:build integration

package tools_test

// SWT-38 (Codex review): task_get_next routes only claude tasks, but task_claim
// by id had no assignee check, so a worker console could take a HUMAN task —
// including one a user-scope session created from content read in another repo
// — and load its body into a worker prompt. Over MCP a non-human identity may
// now claim only claude tasks; human sessions keep claiming anything.
//
// MUTATION THAT MUST TURN THIS RED: drop the assignee check in claimTask → the
// worker's claim of the human task succeeds.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
)

func TestClaim_Integration_WorkerCannotClaimHumanTask(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	t.Cleanup(pool.Close)
	captureCleanup(t, ctx, pool)
	t.Cleanup(func() { captureCleanup(t, ctx, pool) })

	project := seedProject(t, ctx, pool, "itest-mcp-tools-claimgate", "itest-mcp-tools-claimgate")
	newTask := func(assignee string) int64 {
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO tasks (project_id, title, status, assignee_type) VALUES ($1,$2,'ready',$3) RETURNING id`,
			project, "itest claim gate "+assignee, assignee).Scan(&id); err != nil {
			t.Fatalf("seed %s task: %v", assignee, err)
		}
		return id
	}
	ex := queueMatrixExecutor(pool)
	claim := func(actor string, taskID int64) error {
		args, _ := json.Marshal(map[string]any{"task_id": taskID, "worker_id": strings.TrimPrefix(actor, "mcp:")})
		_, err := ex.Execute(ctx, executor.Call{Tool: "task_claim", Actor: actor, Args: args, TaskID: &taskID})
		return err
	}
	status := func(id int64) string {
		var s string
		if err := pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	const worker = "mcp:itest-mcp-tools-claimgate"
	humanTask := newTask("human")
	if err := claim(worker, humanTask); err == nil || !strings.Contains(err.Error(), "claims only claude tasks") {
		t.Errorf("a worker console claiming a HUMAN task = %v, want a refusal", err)
	}
	if got := status(humanTask); got != "ready" {
		t.Errorf("the refused human task is %q, want still ready", got)
	}

	// POSITIVE CONTROLS: the worker still claims claude work, and a human
	// session still claims human work.
	if err := claim(worker, newTask("claude")); err != nil {
		t.Errorf("a worker console claiming a claude task = %v, want ok", err)
	}
	if err := claim("mcp:manual:itest-mcp-tools-claimgate", humanTask); err != nil {
		t.Errorf("a human session claiming a human task = %v, want ok", err)
	}
}
