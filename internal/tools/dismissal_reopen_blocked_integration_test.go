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
// MUTATION THAT MUST TURN THIS RED: drop the `if target == "blocked"` dependency
// check in reopenTask → the "deps met" case comes back blocked.

import (
	"context"
	"testing"
)

func TestDismissalReopen_BlockedRestoreRechecksDependencies(t *testing.T) {
	ctx := context.Background()
	s := newDroSuite(t, ctx)

	for _, tc := range []struct {
		label, depStatus, want string
	}{
		{"blocked-met", "done_locally", "ready"}, // the dependency finished while the task was dismissed
		{"blocked-unmet", "ready", "blocked"},    // still waiting: blocked is right
	} {
		tc := tc
		t.Run(tc.label, func(t *testing.T) {
			dep := s.insID(t, ctx,
				`INSERT INTO tasks (project_id, title, status) VALUES ($1,$2,'ready') RETURNING id`,
				s.project, "itest-dreopen dep "+tc.label)
			c := s.seedDismissed(t, ctx, tc.label, "blocked", "handled_elsewhere")
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
