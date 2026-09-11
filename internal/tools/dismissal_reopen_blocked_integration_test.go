//go:build integration

package tools_test

// SWT-36, Codex re-review: a task dismissed while BLOCKED keeps
// closed_from_status='blocked'. If its dependencies were satisfied while it was
// closed, their completion events could not unblock a closed task (R5 only
// unblocks BLOCKED dependents), and closed → blocked fires no R5 — so a
// verbatim restore would strand it. The guarded reopen therefore restores
// blocked only while a dependency is still unmet (depUnsatisfiedPredicate),
// else ready.
//
// The same holds the other way (Codex pass 3): a task dismissed from READY may
// gain an unmet dependency while closed, and R4 only blocks READY tasks, so a
// verbatim restore would hand a worker gated work. For ready|blocked targets
// the reopen re-derives the gate from the dependencies.
//
// MUTATIONS THAT MUST TURN THIS RED: drop the dependency check in reopenTask →
// "blocked-met" comes back blocked and "ready-unmet" comes back ready; narrow it
// back to `target == "blocked"` → "ready-unmet" comes back ready.

import (
	"context"
	"testing"
)

func TestDismissalReopen_BlockedRestoreRechecksDependencies(t *testing.T) {
	ctx := context.Background()
	s := newDroSuite(t, ctx)

	for _, tc := range []struct {
		label, dismissedFrom, depStatus, want string
	}{
		{"blocked-met", "blocked", "done_locally", "ready"}, // dependency finished while dismissed
		{"blocked-unmet", "blocked", "ready", "blocked"},    // still waiting: blocked is right
		// Codex pass 3: dismissed from READY, then an unmet dependency was added
		// while closed (R4 only blocks READY tasks, so it never fired). A verbatim
		// restore to ready would let a worker claim it early.
		{"ready-unmet", "ready", "ready", "blocked"},
		{"ready-met", "ready", "done_locally", "ready"},
	} {
		tc := tc
		t.Run(tc.label, func(t *testing.T) {
			dep := s.insID(t, ctx,
				`INSERT INTO tasks (project_id, title, status) VALUES ($1,$2,'ready') RETURNING id`,
				s.project, "itest-dreopen dep "+tc.label)
			c := s.seedDismissed(t, ctx, tc.label, tc.dismissedFrom, "handled_elsewhere")
			if _, err := s.pool.Exec(ctx,
				`INSERT INTO task_dependencies (task_id, depends_on_task_id) VALUES ($1,$2)`, c.task, dep); err != nil {
				t.Fatalf("seed dependency: %v", err)
			}
			t.Cleanup(func() {
				_, _ = s.pool.Exec(ctx, `DELETE FROM task_dependencies WHERE task_id=$1`, c.task)
			})
			if _, err := s.pool.Exec(ctx, `UPDATE tasks SET status=$1 WHERE id=$2`, tc.depStatus, dep); err != nil {
				t.Fatalf("set dependency status: %v", err)
			}

			now := s.dbNow(t, ctx)
			m := s.message(t, ctx, tc.label+"-in1", c.thread, "inbound", now, now)
			out := s.guarded(t, ctx, c.task, c.dismissal, m)
			if out["reopened"] != true {
				t.Fatalf("guarded reopen = %v, want reopened:true", out)
			}
			if got := s.status(t, ctx, c.task); got != tc.want {
				t.Errorf("a task dismissed while blocked, dependency %s, reopened as %q, want %q",
					tc.depStatus, got, tc.want)
			}
		})
	}
}
