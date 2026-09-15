//go:build integration

package tools_test

// SWT-52 (docs/tickets/board-status-lights_SPEC.md) criterion 20 / D9:
// closeTransition clears the session marker on a REAL close, through every
// close verb, and an idempotent re-close touches nothing. The light half (done
// / none dismissed (…)) and the board's Done route are in
// internal/dashboard/board_lights_integration_test.go.
//
// Reuses verbsPool, queueMatrixExecutor, seedProject and this package's
// signal_integration_test.go helpers.
//
// GREENFIELD NOTE — EXPECTED RED: task_signal is unregistered, so the marker
// cannot be set, and closeTransition does not clear the columns.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
)

func csCall(t *testing.T, ctx context.Context, ex *executor.Executor, actor, tool string, task int64, args map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(args)
	if _, err := ex.Execute(ctx, executor.Call{Tool: tool, Actor: actor, Args: raw, TaskID: &task}); err != nil {
		t.Fatalf("%s by %s on %d: %v", tool, actor, task, err)
	}
}

func csStatusKeys(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var keys []string
	if err := pool.QueryRow(ctx, `SELECT ARRAY(SELECT jsonb_object_keys(payload) ORDER BY 1) FROM task_events
		WHERE task_id=$1 AND event_type='status_changed' ORDER BY id DESC LIMIT 1`, id).Scan(&keys); err != nil {
		t.Fatalf("status_changed of %d: %v", id, err)
	}
	return strings.Join(keys, ",")
}

func TestCloseSignal_Integration_EveryCloseClearsTheMarker(t *testing.T) {
	ctx := context.Background()
	pool := verbsPool(t, ctx)
	ex := queueMatrixExecutor(pool)
	proj := seedProject(t, ctx, pool, sgSlug, "itest-mcp-tools-signalclient")

	for _, tc := range []struct {
		name, actor, tool, state string
		args                     func(int64) map[string]any
	}{
		{"task_close from needs_input", "mcp:manual:salvo", "task_close", "needs_input",
			func(id int64) map[string]any { return map[string]any{"task_id": id, "reason": "swb done"} }},
		{"task_dismiss from needs_input", "mcp:manual:salvo", "task_dismiss", "needs_input",
			func(id int64) map[string]any { return map[string]any{"task_id": id, "reason_code": "duplicate"} }},
		// The spine path: the orchestrator's close clears it too (D9: every close path).
		{"orchestrator task_close from working", "orchestrator", "task_close", "working",
			func(id int64) map[string]any { return map[string]any{"task_id": id, "reason": "R2"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := sgTask(t, ctx, pool, proj, "CLOSESIG "+tc.name, "human", "ready")
			if _, err := sgExec(ctx, ex, "mcp:manual:salvo", id, tc.state); err != nil {
				t.Fatalf("set %s: %v", tc.state, err)
			}
			if r := sgRead(t, ctx, pool, id); r.session != sgSession {
				t.Fatalf("CONTROL: session = %q after the real task_signal, want %q (SWT-56 criterion 10)", r.session, sgSession)
			}
			csCall(t, ctx, ex, tc.actor, tc.tool, id, tc.args(id))
			r := sgRead(t, ctx, pool, id)
			if r.status != "closed" || r.state != "" || r.stateAt != "" || r.session != "" {
				t.Errorf("after %s: status %q, marker (%q, %q, session %q); want closed with all three NULL (criterion 20 / D9; SWT-56 criterion 10)",
					tc.tool, r.status, r.state, r.stateAt, r.session)
			}
			if k := csStatusKeys(t, ctx, pool, id); k != "from,reason,to" {
				t.Errorf("status_changed keys = %s, want exactly from,reason,to (SWT-51 criterion 11)", k)
			}
		})
	}
}

// An idempotent re-close returns before the transitioning UPDATE: a closed task
// carrying an old binary's leftover marker keeps it, and nothing else moves.
func TestCloseSignal_Integration_IdempotentReCloseTouchesNothing(t *testing.T) {
	ctx := context.Background()
	pool := verbsPool(t, ctx)
	ex := queueMatrixExecutor(pool)
	proj := seedProject(t, ctx, pool, sgSlug, "itest-mcp-tools-signalclient")
	id := sgTask(t, ctx, pool, proj, "CLOSESIG already closed", "human", "closed")
	if _, err := pool.Exec(ctx, `UPDATE tasks SET working_state='needs_input', working_state_at=now() - interval '1 hour', working_session='kube-c7',
		closed_at = now() - interval '2 hours' WHERE id=$1`, id); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before := sgRead(t, ctx, pool, id)
	events := sgCount(t, ctx, pool, `SELECT count(*) FROM task_events WHERE task_id=$1`, id)

	csCall(t, ctx, ex, "mcp:manual:salvo", "task_close", id, map[string]any{"task_id": id, "reason": "again"})

	if after := sgRead(t, ctx, pool, id); after != before {
		t.Errorf("an idempotent re-close changed the row: before %+v after %+v (criterion 20)", before, after)
	}
	if n := sgCount(t, ctx, pool, `SELECT count(*) FROM task_events WHERE task_id=$1`, id); n != events {
		t.Errorf("task_events %d → %d on an idempotent re-close", events, n)
	}
}
