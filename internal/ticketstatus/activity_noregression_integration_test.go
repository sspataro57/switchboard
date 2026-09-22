//go:build integration

package ticketstatus_test

// activity-resurfaces (SWT-72, docs/tickets/activity-resurfaces_SPEC.md)
// criterion 26 — the J11 NON-REGRESSION, and the load-bearing test of D1.
//
// A jira-keyed OPEN task whose ticket is Done. Inbound activity lands on it and
// task_mark_activity stamps tasks.activity_at (exactly what capture's
// actionTaskLog branch does, criterion 8 — the same tool, the same executor,
// the same capture:{connector} actor). Then the reconciler runs: surfaced_at is
// still NULL, the decision is `closed` and NOT `resurfaced`, and the task IS
// closed.
//
// WHY THIS IS THE TEST THAT DECIDES D1. The brief recommended reusing
// surfaced_at. Jira mails on every close (IK SWT-45 J10); that mail is
// captured, matches the same rule and lands as a task_log on the still-open
// task. With reuse, every closed Treetop ticket would leave its task stuck on
// the board forever (decide.go:133-151: a new surfacing on a not-warranted,
// restorable ticket is a HOLD that ends only on a facts change or a hand close).
//
// MUTATION THAT MUST TURN THIS RED (SPEC "Mutations", row 1):
//   - task_mark_activity writes surfaced_at instead of activity_at -> the
//     reconciler HOLDS instead of closing, and both assertions below fail.
//
// DELIBERATE SCOPE NOTE: the SPEC's criterion 26 says "run a capture pass that
// attaches a comment". This suite is internal/ticketstatus's; importing the
// capture engine here would drag its GLOBAL pending set and its wholesale
// capture_decisions cleanup into the reconciler's fixtures (the IK's
// cross-suite landmine). The mark is therefore made the way capture makes it —
// one task_mark_activity through the real executor as capture:{connector} —
// and internal/capture's rules_activity_integration_test.go owns the proof
// that the capture pass is what calls it.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_swt72?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run ActivityDoesNotHold ./internal/ticketstatus/
//
// RED TODAY: migration 0039 is not applied, then task_mark_activity is not
// registered. Reuses store_integration_test.go's tsSuite and
// resurface_integration_test.go's addKey / surfacedAt / seen.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/ticketstatus"
)

const anCapture = "capture:jira" // the capture:{connector} shape D3 names

func anRequire0039(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name='tasks' AND column_name IN ('activity_at','activity_by_message_id','reviewed_at')`).Scan(&n); err != nil {
		t.Fatalf("probe 0039's columns: %v", err)
	}
	if n != 3 {
		t.Fatalf("found %d of the 3 columns migration 0039 adds. Criterion 1: apply "+
			"migrations/0039_task_activity_review.sql (`make migrate LOCAL_DB_URL=...`)", n)
	}
}

// anMessage seeds one inbound normalized message on this suite's polled
// account — the Jira notification mail shape (IK SWT-45's named-actor
// exemption: the From carries the actor).
func anMessage(t *testing.T, ctx context.Context, s *tsSuite, label string) int64 {
	t.Helper()
	raw := s.insID(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,'{}',$3, now()) RETURNING id`,
		s.accounts[tsPolledAcct], "itest-tstatus-act-"+label, "itest-tstatus-act-h-"+label)
	th := s.insID(t, ctx,
		`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,'itest-tstatus','[]') RETURNING id`,
		"jira:itest-tstatus-act:"+label)
	return s.insID(t, ctx,
		`INSERT INTO normalized_messages (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		                                  body_text, subject, sender, channel)
		 VALUES ($1,$2,'inbound',$3, now(),'itest-tstatus body','Katie Evans commented on ITS-ACT',
		         'Katie Evans (JIRA) <jira@example.test>','gmail') RETURNING id`,
		raw, th, "<itest-tstatus-act-"+label+"@mail.example>")
}

func TestTicketStatus_ActivityDoesNotHoldADoneTicketsTask(t *testing.T) {
	ctx := context.Background()
	s := newTSSuite(t, ctx)
	anRequire0039(t, ctx, s.pool)

	// A Jira-keyed OPEN task whose ticket is DONE and not assigned to us: the
	// reconciler's own candidate, the trigger's exact population.
	s.addKey(t, ctx, "ITS-ACT", "done", "ready")
	task := s.tasks["ITS-ACT"]
	msg := anMessage(t, ctx, s, "comment")

	// Exactly what capture's actionTaskLog branch does (criterion 8).
	args, _ := json.Marshal(map[string]any{"task_id": task, "message_id": msg,
		"reason": "capture: jira ITS-ACT — gmail message activity"})
	if _, err := s.ex.Execute(ctx, executor.Call{Tool: "task_mark_activity", Actor: anCapture,
		Args: args, TaskID: &task}); err != nil {
		t.Fatalf("task_mark_activity as %s: %v (criteria 2, 7)", anCapture, err)
	}

	var activityAt *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT activity_at FROM tasks WHERE id=$1`, task).Scan(&activityAt); err != nil {
		t.Fatalf("read activity_at: %v", err)
	}
	if activityAt == nil {
		t.Fatalf("CONTROL: task_mark_activity left activity_at NULL; the rest of this test is vacuous")
	}
	if at, by := s.surfacedAt(t, ctx, "ITS-ACT"); at != nil || by != nil {
		t.Fatalf("task_mark_activity wrote surfaced_at=%v by=%v. D1/criterion 26: activity is a BOARD fact; "+
			"writing the SWT-45 column makes the reconciler hold this task open against a done ticket, and "+
			"since Jira mails on every close, every closed ticket's task would stick on the board forever",
			at, by)
	}

	stats, _ := s.run(t, ctx, ticketstatus.Config{})

	if got := s.status(t, ctx, "ITS-ACT"); got != "closed" {
		t.Errorf("ITS-ACT (ticket done, activity_at set, surfaced_at NULL) is %q after Run, want closed. "+
			"Criterion 26 / D4: the reconciler never learns the new columns", got)
	}
	st := s.state(t, ctx, "ITS-ACT")
	if st.lastAction != "closed" {
		t.Errorf("ITS-ACT last_action = %q, want closed — NOT `resurfaced` (criterion 26). %+v", st.lastAction, st)
	}
	if stats.Resurfaced != 0 {
		t.Errorf("Stats.Resurfaced = %d, want 0: inbound activity on an open task is not an SWT-45 surfacing "+
			"(D1)", stats.Resurfaced)
	}
	// The control from SWT-45's own suite: a genuinely surfaced task still holds,
	// so this test cannot pass by the reconciler having lost its hold entirely.
	s.addKey(t, ctx, "ITS-ACT-CTL", "done", "ready")
	s.exec(t, ctx, `UPDATE tasks SET surfaced_at = now() WHERE id=$1`, s.tasks["ITS-ACT-CTL"])
	if _, _ = s.run(t, ctx, ticketstatus.Config{}); s.status(t, ctx, "ITS-ACT-CTL") != "ready" {
		t.Errorf("POSITIVE CONTROL FAILED: a task with surfaced_at set was closed too; the reconciler's SWT-45 " +
			"hold is gone, so the assertions above prove nothing about D1")
	}
}
