//go:build integration

package tools_test

// SWT-52 (docs/tickets/board-status-lights_SPEC.md) D9 amendment, 2026-09-14:
//
//   - EVERY reopen clears the session marker (Codex review). A binary built
//     before 0033 closes a task without clearing working_state /
//     working_state_at; a later reopen must not resurrect that red or yellow.
//     The three reopen forms — plain, guarded (SWT-36), revive (SWT-45) — all
//     reach closeTransition's reopen UPDATE, and each is proven here from a
//     close written by raw SQL exactly as an old binary writes it (status,
//     updated_at, closed_at, closed_from_status — and NOT the marker).
//   - A CLAIM clears it (go-reviewer). The claim is the task's signal from then
//     on; a marker kept across claim → release re-emerged as a stale light when
//     the task went back to ready.
//
// MUTATION CHECKS (2026-09-14): drop `working_state = NULL, working_state_at =
// NULL` from closeTransition's reopen UPDATE → all three reopen subtests fail;
// drop it from task_claim's UPDATE → the claim test fails.
//
// Reuses verbsPool, queueMatrixExecutor, seedProject, the sg* helpers
// (signal_integration_test.go), newDroSuite (dismissal_reopen_integration_test.go)
// and newRVSuite (revive_integration_test.go), with their cleanup pacts.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
)

// executorCall scopes every call's audit rows to a test-owned task.
func executorCall(tool, actor string, task int64, args []byte) executor.Call {
	return executor.Call{Tool: tool, Actor: actor, Args: args, TaskID: &task}
}

// rcOldBinaryClose closes a task the way a pre-0033 closeTransition does: the
// close record is written, the marker is left in place.
func rcOldBinaryClose(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64, closedAgo string) {
	t.Helper()
	tag, err := pool.Exec(ctx, `UPDATE tasks SET status='closed', updated_at=now(),
		closed_at = now() - $2::interval, closed_from_status = status WHERE id=$1 AND status <> 'closed'`, id, closedAgo)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("old-binary close of %d: rows %d, err %v", id, tag.RowsAffected(), err)
	}
}

// rcMarked asserts the fixture: closed, with the old binary's marker still set.
func rcMarked(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64, state string) {
	t.Helper()
	if r := sgRead(t, ctx, pool, id); r.status != "closed" || r.state != state || r.stateAt == "" {
		t.Fatalf("CONTROL: fixture task %d = (status %q, marker %q at %q), want closed carrying %q",
			id, r.status, r.state, r.stateAt, state)
	}
}

func rcWantCleared(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64, form string, out map[string]any) {
	t.Helper()
	if out["reopened"] != true {
		t.Fatalf("%s reopen did not reopen task %d: %v", form, id, out)
	}
	r := sgRead(t, ctx, pool, id)
	if r.status == "closed" {
		t.Fatalf("%s reopen left task %d closed", form, id)
	}
	if r.state != "" || r.stateAt != "" {
		t.Errorf("%s reopen of task %d kept the old binary's marker (%q at %q); the D9 amendment: every reopen "+
			"clears it in the same UPDATE that moves the task out of closed", form, id, r.state, r.stateAt)
	}
	if k := csStatusKeys(t, ctx, pool, id); k != "from,reason,to" {
		t.Errorf("%s reopen's status_changed keys = %s, want exactly from,reason,to", form, k)
	}
}

func TestReopenSignal_Integration_EveryReopenClearsAnOldBinarysMarker(t *testing.T) {
	ctx := context.Background()

	t.Run("plain", func(t *testing.T) {
		pool := verbsPool(t, ctx)
		ex := queueMatrixExecutor(pool)
		proj := seedProject(t, ctx, pool, sgSlug, "itest-mcp-tools-signalclient")
		id := sgTask(t, ctx, pool, proj, "REOPENSIG plain", "human", "ready")
		if _, err := sgExec(ctx, ex, "mcp:manual:salvo", id, "needs_input"); err != nil {
			t.Fatalf("set needs_input: %v", err)
		}
		rcOldBinaryClose(t, ctx, pool, id, "1 hour")
		rcMarked(t, ctx, pool, id, "needs_input")

		raw, _ := json.Marshal(map[string]any{"task_id": id, "reason": "itest: reopen after an old binary's close"})
		res, err := ex.Execute(ctx, executorCall("task_reopen", "opsctl:salvo", id, raw))
		if err != nil {
			t.Fatalf("plain reopen: %v", err)
		}
		out := map[string]any{}
		_ = json.Unmarshal(res.Output, &out)
		rcWantCleared(t, ctx, pool, id, "plain", out)
	})

	t.Run("guarded", func(t *testing.T) {
		s := newDroSuite(t, ctx)
		thread := s.insID(t, ctx,
			`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,'itest-dreopen','[]') RETURNING id`,
			"itest-dreopen:signal-guarded")
		id := s.insID(t, ctx,
			`INSERT INTO tasks (project_id, title, assignee_type, status, source_thread_id)
			 VALUES ($1,'itest-dreopen signal guarded','human','ready',$2) RETURNING id`, s.project, thread)
		if _, err := sgExec(ctx, s.ex, "mcp:manual:salvo", id, "working"); err != nil {
			t.Fatalf("set working: %v", err)
		}
		// An old binary's DISMISS: the close record plus the typed dismissal row,
		// both an hour ago, and the marker left in place.
		rcOldBinaryClose(t, ctx, s.pool, id, "1 hour")
		dismissal := s.insID(t, ctx,
			`INSERT INTO task_dismissals (task_id, reason_code, dismissed_by, closed_from_status, created_at)
			 VALUES ($1,'duplicate',$2,'ready', now() - interval '1 hour') RETURNING id`, id, droHuman)
		rcMarked(t, ctx, s.pool, id, "working")

		now := s.dbNow(t, ctx)
		msg := s.message(t, ctx, "signal-guarded-in", thread, "inbound", now, now)
		rcWantCleared(t, ctx, s.pool, id, "guarded", s.guarded(t, ctx, id, dismissal, msg))
	})

	t.Run("revive", func(t *testing.T) {
		s := newRVSuite(t, ctx)
		id, thread := s.task(t, ctx, "signal-revive", "ready")
		s.exec(t, ctx, `UPDATE tasks SET assignee_type='human' WHERE id=$1`, id)
		if _, err := sgExec(ctx, s.ex, "mcp:manual:salvo", id, "needs_input"); err != nil {
			t.Fatalf("set needs_input: %v", err)
		}
		rcOldBinaryClose(t, ctx, s.pool, id, "1 hour")
		rcMarked(t, ctx, s.pool, id, "needs_input")

		now := s.dbNow(t, ctx)
		msg := s.message(t, ctx, "signal-revive-in", thread, "inbound", now, now)
		rcWantCleared(t, ctx, s.pool, id, "revive", s.call(t, ctx, "task_reopen", rvSpine, id, reviveArgs(id, msg)))
	})
}

// A claim clears the marker, so a claim → release cycle cannot bring it back.
func TestClaimSignal_Integration_AClaimClearsTheMarker(t *testing.T) {
	ctx := context.Background()
	pool := verbsPool(t, ctx)
	ex := queueMatrixExecutor(pool)
	proj := seedProject(t, ctx, pool, sgSlug, "itest-mcp-tools-signalclient")

	for _, state := range []string{"working", "needs_input"} {
		t.Run(state, func(t *testing.T) {
			id := sgTask(t, ctx, pool, proj, "CLAIMSIG "+state, "human", "ready")
			if _, err := sgExec(ctx, ex, "mcp:manual:salvo", id, state); err != nil {
				t.Fatalf("set %s: %v", state, err)
			}
			if r := sgRead(t, ctx, pool, id); r.state != state {
				t.Fatalf("CONTROL: marker = %q after setting %s", r.state, state)
			}

			claim, _ := json.Marshal(map[string]any{"task_id": id, "worker_id": "manual:salvo"})
			if _, err := ex.Execute(ctx, executorCall("task_claim", "opsctl:salvo", id, claim)); err != nil {
				t.Fatalf("claim: %v", err)
			}
			if r := sgRead(t, ctx, pool, id); r.status != "claimed" || r.state != "" || r.stateAt != "" {
				t.Errorf("after the claim: status %q, marker (%q, %q); want claimed with both NULL (D9 amendment: "+
					"the claim is the signal now)", r.status, r.state, r.stateAt)
			}

			release, _ := json.Marshal(map[string]any{"task_id": id, "worker_id": "manual:salvo", "reason": "itest: put back"})
			if _, err := ex.Execute(ctx, executorCall("task_release", "opsctl:salvo", id, release)); err != nil {
				t.Fatalf("release: %v", err)
			}
			if r := sgRead(t, ctx, pool, id); r.status != "ready" || r.state != "" || r.stateAt != "" {
				t.Errorf("after claim → release: status %q, marker (%q, %q); want ready with no marker — the "+
					"old %s must not re-emerge", r.status, r.state, r.stateAt, state)
			}
		})
	}
}
