//go:build integration

package orchestrator_test

// SWT-41 review (Codex, high x2): the daemon's liveness and lock checks reach
// INTO a drain through DrainHooks.
//   - Progress runs after every processed event, so a long catch-up keeps the
//     /healthz tick fresh instead of being restarted mid-drain.
//   - Guard runs before every batch; an error stops the drain before it applies
//     anything, so a process whose lock is gone cannot keep mutating.
//
// MUTATIONS THAT MUST TURN THIS RED: drop the Progress call in DrainOnce
// (progress stays 0); drop the Guard check (the failing-guard drain processes
// the events and moves the cursor).

import (
	"context"
	"errors"
	"testing"

	orch "github.com/sspataro57/switchboard/internal/orchestrator"
)

func TestEngine_Integration_DrainHooksReportProgressAndStopOnGuard(t *testing.T) {
	ctx := context.Background()
	pool := newOrchPool(t, ctx)
	t.Cleanup(pool.Close)
	healthGuard(t)
	cleanupOrch(t, ctx, pool)
	t.Cleanup(func() { cleanupOrch(t, context.Background(), pool) })

	const (
		actor = "itest-orch-hooks"
		slug  = "itest-orch-hooks"
	)
	seedProjectDelivery(t, ctx, pool, slug, "itest-orch-hooks-client", "dashboard")
	ex := newOrchExecutor(pool)
	task := createReadyTask(t, ctx, ex, actor, slug, "hooks fixture")
	setCursor(t, ctx, pool, maxEventID(t, ctx, pool))

	logEvents := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			if _, err := pool.Exec(ctx,
				`INSERT INTO task_events (task_id, event_type, payload) VALUES ($1,'log','{}')`, task); err != nil {
				t.Fatalf("insert log event: %v", err)
			}
		}
	}

	// Progress: once per processed event.
	logEvents(3)
	engine := orch.NewEngine(pool, ex, &recordingPublisher{}, orch.Config{})
	progress := 0
	engine.SetDrainHooks(orch.DrainHooks{Progress: func() { progress++ }})
	n := drain(t, ctx, engine)
	if n != 3 || progress != n {
		t.Errorf("drained %d events with %d progress calls, want 3 and 3: a catch-up must keep the liveness "+
			"tick fresh per event, or /healthz restarts it mid-drain", n, progress)
	}

	// Guard: an error stops the drain before anything is applied.
	logEvents(2)
	before := cursorValue(t, ctx, pool)
	engine.SetDrainHooks(orch.DrainHooks{Guard: func(context.Context) error { return errors.New("lock lost") }})
	processed, err := engine.DrainOnce(ctx)
	if err == nil {
		t.Error("DrainOnce with a failing guard returned no error")
	}
	if processed != 0 {
		t.Errorf("DrainOnce with a failing guard processed %d events, want 0", processed)
	}
	if after := cursorValue(t, ctx, pool); after != before {
		t.Errorf("cursor moved %d -> %d under a failing guard: a process whose lock is gone kept draining", before, after)
	}
}
