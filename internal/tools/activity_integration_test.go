//go:build integration

package tools_test

// task_mark_activity against a real database — activity-resurfaces (SWT-72,
// docs/tickets/activity-resurfaces_SPEC.md) D3 and criterion 4: under the
// tasks row lock it refuses a non-inbound or missing message with an ERROR
// (invariant 5), skips a closed task, is a no-op when activity_by_message_id
// already equals the message, and otherwise writes activity_at = now() and the
// message id — and NOTHING else moves.
//
// Called through the executor as capture:itest-revive with the REAL policy
// matrix, so a humanOnly slip would show here as a denial (criterion 7).
// Reuses revive_integration_test.go's rvSuite wholesale (its cleanup pact, its
// fixtures).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_swt72?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run MarkActivity ./internal/tools/
//
// RED TODAY: migration 0039 is not applied (amRequire0039 fails every test with
// one sentence); after that, the tool is not registered.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - drop the closed-task skip -> AClosedTaskIsSkipped (and criterion 11 in internal/capture).
//   - drop the non-inbound error -> ANonInboundMessageIsAnError.
//   - write surfaced_at instead of activity_at -> StampsAnOpenTaskOncePerMessage
//     (and criterion 26 in internal/ticketstatus).
//   - stamp updated_at too -> StampsAnOpenTaskOncePerMessage's untouched-columns half.
//   - write a task_events row -> StampsAnOpenTaskOncePerMessage's event count.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const maActor = "capture:itest-revive" // rvSpine: capture's shape, not a human

// amRequire0039 turns "column tasks.activity_at does not exist" — which
// otherwise surfaces from inside a scan and reads like a broken fixture — into
// the one sentence that is true before this ticket lands.
func amRequire0039(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name='tasks'
		    AND column_name IN ('activity_at','activity_by_message_id','reviewed_at')`).Scan(&n); err != nil {
		t.Fatalf("probe 0039's columns: %v", err)
	}
	if n != 3 {
		t.Fatalf("found %d of the 3 columns migration 0039 adds (tasks.activity_at, activity_by_message_id, "+
			"reviewed_at). Criterion 1: migrations/0039_task_activity_review.sql adds them; "+
			"`make migrate LOCAL_DB_URL=...` applies it. Merging a migration is not applying it", n)
	}
}

func activityArgs(taskID, messageID int64) string {
	b, _ := json.Marshal(map[string]any{"task_id": taskID, "message_id": messageID,
		"reason": "capture: jira COMMENT-1 — gmail message activity"})
	return string(b)
}

type maRow struct {
	activityAt *time.Time
	activityBy *int64
	reviewedAt *time.Time
	surfacedAt *time.Time
	status     string
	priority   int
	updatedAt  time.Time
}

func maTaskRow(t *testing.T, ctx context.Context, s *rvSuite, taskID int64) maRow {
	t.Helper()
	var r maRow
	if err := s.pool.QueryRow(ctx,
		`SELECT activity_at, activity_by_message_id, reviewed_at, surfaced_at, status, priority, updated_at
		   FROM tasks WHERE id=$1`, taskID).
		Scan(&r.activityAt, &r.activityBy, &r.reviewedAt, &r.surfacedAt, &r.status, &r.priority, &r.updatedAt); err != nil {
		t.Fatalf("read task %d: %v", taskID, err)
	}
	return r
}

// Criterion 4's happy path plus D3's "it touches nothing else".
func TestMarkActivity_StampsAnOpenTaskOncePerMessage(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)
	amRequire0039(t, ctx, s.pool)

	task, thread := s.task(t, ctx, "ma", "ready")
	before := maTaskRow(t, ctx, s, task)
	if before.activityAt != nil || before.reviewedAt != nil {
		t.Fatalf("CONTROL: a freshly inserted task has activity_at=%v reviewed_at=%v, want both NULL "+
			"(D1: every existing row is NULL at rollout, no backfill)", before.activityAt, before.reviewedAt)
	}
	eventsBefore := s.n(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1`, task)

	now := s.dbNow(t, ctx)
	m1 := s.message(t, ctx, "ma1", thread, "inbound", now, now)

	out := s.call(t, ctx, "task_mark_activity", maActor, task, activityArgs(task, m1))
	if out["marked"] != true {
		t.Errorf("task_mark_activity on an open task = %v, want marked:true (criterion 4)", out)
	}
	if out["task_id"] == nil {
		t.Errorf("task_mark_activity result %v carries no task_id; the SPEC's table says {task_id, marked, skipped?}", out)
	}
	r := maTaskRow(t, ctx, s, task)
	if r.activityAt == nil || r.activityBy == nil || *r.activityBy != m1 {
		t.Fatalf("after task_mark_activity activity_at=%v by=%v, want set / %d (criterion 4)", r.activityAt, r.activityBy, m1)
	}

	// D3: it touches NOTHING else.
	if r.surfacedAt != nil {
		t.Errorf("task_mark_activity set surfaced_at=%v. D1: surfaced_at keeps its SWT-45 meaning — reusing it "+
			"would hold every done Jira ticket's task open forever (criterion 26)", r.surfacedAt)
	}
	if r.reviewedAt != nil {
		t.Errorf("task_mark_activity set reviewed_at=%v; only a review stamps it (D2)", r.reviewedAt)
	}
	if !r.updatedAt.Equal(before.updatedAt) {
		t.Errorf("task_mark_activity moved updated_at %v -> %v. D3: updated_at feeds the board's `updated` cell "+
			"and the SWT-45 revive guard's fallback", before.updatedAt, r.updatedAt)
	}
	if r.status != before.status || r.priority != before.priority {
		t.Errorf("task_mark_activity changed status/priority to %s/%d, want %s/%d unchanged",
			r.status, r.priority, before.status, before.priority)
	}
	if n := s.n(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1`, task); n != eventsBefore {
		t.Errorf("task_mark_activity wrote %d task_events rows, want 0. D3/D9: the caller appended the log line "+
			"one statement earlier, and a second event is noise on the orchestrator's feed", n-eventsBefore)
	}

	// Criterion 4's audit half (invariant 3), and criterion 7's policy half:
	// capture:{connector} must be ALLOWED — a humanOnly slip dies here.
	if n := s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool='task_mark_activity'
	                      AND actor=$2 AND status='ok'`, task, maActor); n != 1 {
		t.Errorf("task_mark_activity audit rows = %d, want 1 — invariant 3, and the policy decision for %s must "+
			"be allow (D3: not humanOnly; capture and promote call it)", n, maActor)
	}

	// The same message twice: a no-op; activity_at does NOT move.
	time.Sleep(5 * time.Millisecond)
	out2 := s.call(t, ctx, "task_mark_activity", maActor, task, activityArgs(task, m1))
	if out2["marked"] != false || out2["skipped"] != "already_marked_by_message" {
		t.Errorf("a replayed task_mark_activity = %v, want {marked:false, skipped:\"already_marked_by_message\"} "+
			"(criterion 4)", out2)
	}
	if r2 := maTaskRow(t, ctx, s, task); r2.activityAt == nil || !r2.activityAt.Equal(*r.activityAt) {
		t.Errorf("a replayed task_mark_activity moved activity_at %v -> %v. D3: a replay must not re-surface a "+
			"row the human already reviewed", r.activityAt, r2.activityAt)
	}

	// A different, later message re-stamps — that is how a second comment
	// re-surfaces a task whose first comment was already reviewed (D2).
	later := s.dbNow(t, ctx)
	m2 := s.message(t, ctx, "ma2", thread, "inbound", later, later)
	s.call(t, ctx, "task_mark_activity", maActor, task, activityArgs(task, m2))
	if r3 := maTaskRow(t, ctx, s, task); r3.activityBy == nil || *r3.activityBy != m2 || !r3.activityAt.After(*r.activityAt) {
		t.Errorf("a different message left activity_by=%v activity_at=%v, want %d and later than %v",
			r3.activityBy, r3.activityAt, m2, r.activityAt)
	}
}

// Criterion 4 / invariant 5: one of OUR sends re-entering through ingestion can
// never surface a task.
func TestMarkActivity_ANonInboundMessageIsAnError(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)
	amRequire0039(t, ctx, s.pool)

	task, thread := s.task(t, ctx, "ma-out", "ready")
	ours := s.message(t, ctx, "ma-out", thread, "outbound", s.dbNow(t, ctx), s.dbNow(t, ctx))
	for _, msg := range []int64{ours, 987654321} {
		out, err := s.run(ctx, "task_mark_activity", maActor, task, activityArgs(task, msg))
		if err == nil {
			t.Errorf("task_mark_activity with message %d (outbound or missing) = %v, want an ERROR naming "+
				"invariant 5 (criterion 4)", msg, out)
			continue
		}
		if strings.Contains(err.Error(), "unknown tool") || strings.Contains(err.Error(), "validate ") {
			t.Errorf("task_mark_activity with message %d failed before the handler (%v); the refusal is the "+
				"HANDLER's, under the row lock", msg, err)
		}
	}
	if r := maTaskRow(t, ctx, s, task); r.activityAt != nil {
		t.Errorf("a refused task_mark_activity wrote activity_at=%v", r.activityAt)
	}
}

// Criterion 4 / D3: a closed task is a SKIP, which is what keeps SWT-45's
// revive and SWT-36's reopen out of this ticket for free — both branches run
// AFTER the mark, so the task is still closed when the mark is attempted.
func TestMarkActivity_AClosedTaskIsSkipped(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)
	amRequire0039(t, ctx, s.pool)

	task, thread := s.task(t, ctx, "ma-closed", "ready")
	s.close(t, ctx, task)
	m := s.message(t, ctx, "ma-closed", thread, "inbound", s.dbNow(t, ctx), s.dbNow(t, ctx))
	out := s.call(t, ctx, "task_mark_activity", maActor, task, activityArgs(task, m))
	if out["marked"] != false || out["skipped"] != "task_closed" {
		t.Errorf("task_mark_activity on a closed task = %v, want {marked:false, skipped:\"task_closed\"} "+
			"(criterion 4: closed tasks are SWT-45's and SWT-53's, out of scope here by construction)", out)
	}
	if r := maTaskRow(t, ctx, s, task); r.activityAt != nil || r.status != "closed" {
		t.Errorf("a skipped task_mark_activity left status=%s activity_at=%v, want closed / NULL", r.status, r.activityAt)
	}
}
