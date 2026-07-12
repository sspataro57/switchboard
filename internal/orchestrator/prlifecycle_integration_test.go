//go:build integration

package orchestrator_test

// End-to-end PR lifecycle walk (SPEC 09-jira-github-connectors, criterion 16):
// one task driven through the FULL PR half of the status machine using only
// executor tools + engine drains — exactly what hooksd/the poller dispatch:
//
//	link_external_ref → pr_opened  → ready → pr_open        (R9)
//	ci_started                     → awaiting_ci             (R10)
//	ci_failed ×1                   → log only, stays put     (R11 streak 1)
//	ci_failed ×2                   → back to ready + logs    (R11 streak 2)
//	pr_opened (new PR, from ready) → pr_open                 (R9 — ready is a
//	                                                          legal source)
//	ci_started → ci_passed         → awaiting_ci → awaiting_merge
//	pr_merged                      → done_locally (done_local emitted)
//	next drain                     → R3 creates the Deliver child task
//
// Same task the whole way — red CI never creates a new one. Uses the
// integration_test.go harness (cleanupOrch owns the itest-orch-% corpus).

import (
	"context"
	"testing"

	orch "github.com/sspataro57/switchboard/internal/orchestrator"
)

func TestOrchestrator_Integration_PRLifecycleWalk(t *testing.T) {
	const (
		actor  = "itest-orch-prwalk"
		slug   = "itest-orch-prwalk"
		client = "itest-orch-prwalk-client"
	)
	ctx := context.Background()
	pool := newOrchPool(t, ctx)
	defer pool.Close()

	cleanupOrch(t, ctx, pool)
	defer cleanupOrch(t, ctx, pool)

	seedProjectDelivery(t, ctx, pool, slug, client, "dashboard")
	ex := newOrchExecutor(pool)
	engine := orch.NewEngine(pool, ex, &recordingPublisher{}, orch.Config{})
	setCursor(t, ctx, pool, maxEventID(t, ctx, pool))

	taskID := createReadyTask(t, ctx, ex, actor, slug, "prwalk work")
	if got := orchStatus(t, ctx, pool, taskID); got != "ready" {
		t.Fatalf("fresh task status = %s, want ready", got)
	}
	callOrch(t, ctx, ex, actor, "link_external_ref",
		`{"task_id":`+itoa(taskID)+`,"system":"github","external_key":"itest/prwalk#7"}`)

	step := func(tool, args, wantStatus, note string) {
		t.Helper()
		callOrch(t, ctx, ex, actor, tool, args)
		drain(t, ctx, engine)
		if got := orchStatus(t, ctx, pool, taskID); got != wantStatus {
			t.Fatalf("%s: status = %s, want %s", note, got, wantStatus)
		}
	}
	id := itoa(taskID)

	// PR #7 opened directly from ready (worker linked, PR arrives): R9 must
	// accept ready as a pr_open source.
	step("record_pr_event", `{"task_id":`+id+`,"action":"opened","pr":7,"url":"https://gh/itest/prwalk/7"}`,
		"pr_open", "pr_opened from ready")
	step("record_ci_event", `{"task_id":`+id+`,"phase":"started","run_id":101}`,
		"awaiting_ci", "ci_started")
	// First red: R11 logs and retries in place — the task does NOT move.
	step("record_ci_event", `{"task_id":`+id+`,"phase":"completed","conclusion":"failure","run_id":101,"run_url":"https://ci/101"}`,
		"awaiting_ci", "ci_failed streak 1")
	// Second consecutive red: SAME task back to ready with logs appended.
	step("record_ci_event", `{"task_id":`+id+`,"phase":"completed","conclusion":"failure","run_id":102,"run_url":"https://ci/102"}`,
		"ready", "ci_failed streak 2")

	// The retry ships as a new PR from ready; the streak reset on pr_opened.
	step("record_pr_event", `{"task_id":`+id+`,"action":"opened","pr":8,"url":"https://gh/itest/prwalk/8"}`,
		"pr_open", "retry PR opened from ready")
	step("record_ci_event", `{"task_id":`+id+`,"phase":"started","run_id":103}`,
		"awaiting_ci", "retry ci_started")
	step("record_ci_event", `{"task_id":`+id+`,"phase":"completed","conclusion":"success","run_id":103,"run_url":"https://ci/103"}`,
		"awaiting_merge", "ci_passed")
	step("record_pr_event", `{"task_id":`+id+`,"action":"merged","pr":8,"url":"https://gh/itest/prwalk/8"}`,
		"done_locally", "pr_merged")

	// The transition emitted done_local; the NEXT drain chains R3.
	drain(t, ctx, engine)
	var deliverTasks int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE parent_id=$1 AND title LIKE 'Deliver #%'`, taskID).Scan(&deliverTasks); err != nil {
		t.Fatalf("count deliver tasks: %v", err)
	}
	if deliverTasks != 1 {
		t.Fatalf("Deliver child tasks = %d, want exactly 1 (R3 chained off pr_merged)", deliverTasks)
	}

	// Replay safety: a poller sweep re-reports the merged PR — the record tool
	// dedups (no new event), the drain moves nothing, no second Deliver task.
	callOrch(t, ctx, ex, actor, "record_pr_event",
		`{"task_id":`+id+`,"action":"merged","pr":8,"url":"https://gh/itest/prwalk/8"}`)
	drain(t, ctx, engine)
	if got := orchStatus(t, ctx, pool, taskID); got != "done_locally" {
		t.Fatalf("after merged replay: status = %s, want done_locally", got)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE parent_id=$1 AND title LIKE 'Deliver #%'`, taskID).Scan(&deliverTasks); err != nil {
		t.Fatalf("recount deliver tasks: %v", err)
	}
	if deliverTasks != 1 {
		t.Fatalf("Deliver child tasks after replay = %d, want still 1", deliverTasks)
	}
}
