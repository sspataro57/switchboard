//go:build integration

package tools_test

// task_requeue against a real database — activity-resurfaces (SWT-72,
// docs/tickets/activity-resurfaces_SPEC.md) D6, D8 and criteria 20, 21 and 24:
// one transaction under lockTask that refuses a closed task by name, always
// stamps reviewed_at, lifts holding -> ready (and NOTHING else), applies an
// optional priority through the shared applyPriority, and writes exactly one
// `reviewed` event — plus closeTransition's reviewed_at stamp on the CLOSE
// update only.
//
// Called through the executor with the REAL policy matrix as dashboard:… and
// opsctl:… (humanOnly passes both). Reuses revive_integration_test.go's
// rvSuite; amRequire0039 lives in activity_integration_test.go.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_swt72?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run 'Requeue|CloseStampsReviewed' ./internal/tools/
//
// RED TODAY: migration 0039 is not applied; after that, the tool is not registered.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - task_requeue accepts a closed task -> RefusesAClosedTaskByName.
//   - a missing priority defaults to 0 -> OmittedPriorityLeavesItUnchanged.
//   - it lifts a status other than holding -> LiftsHoldingToReadyAndNothingElse.
//   - it writes a second status_changed on a replay -> IsIdempotent.
//   - closeTransition stamps reviewed_at on the REOPEN update too -> CloseStampsReviewedButReopenDoesNot.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const (
	rqActor  = "dashboard:itest-revive" // rvDismisser: a human board click
	rqOpsctl = "opsctl:itest-revive"    // rvCloser: the other human shape
)

func requeueArgs(taskID int64, priority *int, note string) string {
	m := map[string]any{"task_id": taskID}
	if priority != nil {
		m["priority"] = *priority
	}
	if note != "" {
		m["note"] = note
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func intp(v int) *int { return &v }

// rqEvents returns this task's task_events rows of a type, newest last.
func rqEvents(t *testing.T, ctx context.Context, s *rvSuite, taskID int64, typ string) []map[string]any {
	t.Helper()
	rows, err := s.pool.Query(ctx,
		`SELECT payload FROM task_events WHERE task_id=$1 AND event_type=$2 ORDER BY id`, taskID, typ)
	if err != nil {
		t.Fatalf("read %s events of task %d: %v", typ, taskID, err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan event payload: %v", err)
		}
		m := map[string]any{}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &m)
		}
		out = append(out, m)
	}
	return out
}

// ---- criterion 20: the handler, in D6's order ----------------------------------

// D6 step 1: a closed task is refused BY NAME — task_reopen is the verb for
// that, and a silent success would tell the board a closed row was requeued.
func TestRequeue_RefusesAClosedTaskByName(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)
	amRequire0039(t, ctx, s.pool)

	task, _ := s.task(t, ctx, "rq-closed", "ready")
	s.close(t, ctx, task)
	closed := maTaskRow(t, ctx, s, task) // the close itself stamps reviewed_at (D8); the refusal must not move it
	time.Sleep(5 * time.Millisecond)
	out, err := s.run(ctx, "task_requeue", rqActor, task, requeueArgs(task, nil, ""))
	if err == nil {
		t.Fatalf("task_requeue on a closed task = %v, want an ERROR (D6 step 1)", out)
	}
	if !strings.Contains(err.Error(), "task_reopen") {
		t.Errorf("task_requeue on a closed task = %q, want a refusal NAMING task_reopen (D6 step 1: \"refuse a "+
			"closed task by name — task_reopen is the verb for that\")", err)
	}
	r := maTaskRow(t, ctx, s, task)
	if r.reviewedAt == nil || closed.reviewedAt == nil || !r.reviewedAt.Equal(*closed.reviewedAt) {
		t.Errorf("a refused task_requeue moved reviewed_at %v -> %v; the refusal is first, before anything is written",
			closed.reviewedAt, r.reviewedAt)
	}
	if r.status != "closed" {
		t.Errorf("a refused task_requeue left status=%q, want closed", r.status)
	}
}

// D6 steps 2 and 3, and criterion 21's first half. The `holding` lift is the
// ONLY transition the verb can make; every other open status is left alone.
func TestRequeue_LiftsHoldingToReadyAndNothingElse(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)
	amRequire0039(t, ctx, s.pool)

	// OQ-1 = B: holding -> ready, with its own status_changed event.
	held, _ := s.task(t, ctx, "rq-holding", "holding")
	out := s.call(t, ctx, "task_requeue", rqActor, held, requeueArgs(held, nil, "back on the queue"))
	if out["status"] != "ready" || out["reviewed"] != true {
		t.Errorf("task_requeue on a holding task = %v, want {status:\"ready\", reviewed:true} (D6, OQ-1 = B)", out)
	}
	if r := maTaskRow(t, ctx, s, held); r.status != "ready" {
		t.Errorf("task_requeue left a holding task in %q, want ready (D6 step 3)", r.status)
	} else if r.reviewedAt == nil {
		t.Errorf("task_requeue did not stamp reviewed_at (D6 step 2: unconditional)")
	}
	ev := rqEvents(t, ctx, s, held, "status_changed")
	if len(ev) != 1 {
		t.Fatalf("a holding requeue wrote %d status_changed events, want exactly 1 (D6 step 3)", len(ev))
	}
	if ev[0]["from"] != "holding" || ev[0]["to"] != "ready" || ev[0]["rule"] != "requeue" {
		t.Errorf("the status_changed payload = %v, want {from:holding, to:ready, rule:requeue, reason} — "+
			"internal/tools/dependency.go's spelling (D6 step 3)", ev[0])
	}

	// Every OTHER open status is untouched, and writes NO status_changed.
	for _, st := range []string{"ready", "blocked", "done_locally", "delivered", "claimed", "in_progress", "needs_feedback"} {
		st := st
		t.Run(st, func(t *testing.T) {
			task, _ := s.task(t, ctx, "rq-"+st, st)
			out := s.call(t, ctx, "task_requeue", rqActor, task, requeueArgs(task, nil, ""))
			if out["status"] != st {
				t.Errorf("task_requeue on a %s task reported status %v, want %q unchanged", st, out["status"], st)
			}
			if r := maTaskRow(t, ctx, s, task); r.status != st {
				t.Errorf("task_requeue moved a %s task to %q. D6 step 3: `holding -> ready` is the ONLY transition "+
					"this verb can make", st, r.status)
			} else if r.reviewedAt == nil {
				t.Errorf("task_requeue on a %s task did not stamp reviewed_at; step 2 is unconditional", st)
			}
			if ev := rqEvents(t, ctx, s, task, "status_changed"); len(ev) != 0 {
				t.Errorf("task_requeue on a %s task wrote %d status_changed events, want 0", st, len(ev))
			}
		})
	}
}

// D6 steps 4 and 5, and the audit row (invariant 3). Omitted priority =
// unchanged; a given one goes through applyPriority and its priority_changed
// event; the `reviewed` event carries D6's payload.
func TestRequeue_OmittedPriorityLeavesItUnchanged(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)
	amRequire0039(t, ctx, s.pool)

	task, _ := s.task(t, ctx, "rq-prio", "ready")
	s.exec(t, ctx, `UPDATE tasks SET priority=2 WHERE id=$1`, task)

	// Omitted: unchanged. A default of 0 would silently demote an elevated task.
	out := s.call(t, ctx, "task_requeue", rqActor, task, requeueArgs(task, nil, "not mine today"))
	if r := maTaskRow(t, ctx, s, task); r.priority != 2 {
		t.Errorf("task_requeue with no priority left priority=%d, want 2 unchanged. D6 step 4: a dropped argument "+
			"must never demote a task (task_set_priority's pointer rule); the board's select leads with "+
			"`priority: unchanged` for the same reason", r.priority)
	}
	if p, ok := out["priority"].(map[string]any); !ok || p["changed"] != false || p["from"] != p["to"] {
		t.Errorf("task_requeue result = %v, want priority:{from,to,changed:false} (the SPEC's result shape)", out)
	}
	if ev := rqEvents(t, ctx, s, task, "priority_changed"); len(ev) != 0 {
		t.Errorf("an omitted priority wrote %d priority_changed events, want 0", len(ev))
	}

	// Given and different: applied, with the EXISTING priority_changed event.
	s.call(t, ctx, "task_requeue", rqActor, task, requeueArgs(task, intp(0), "low priority, keep it queued"))
	if r := maTaskRow(t, ctx, s, task); r.priority != 0 {
		t.Errorf("task_requeue(priority=0) left priority=%d, want 0 — D6: \"low priority\" IS 0", r.priority)
	}
	pev := rqEvents(t, ctx, s, task, "priority_changed")
	if len(pev) != 1 || pev[0]["from"] != float64(2) || pev[0]["to"] != float64(0) {
		t.Errorf("priority_changed events = %v, want exactly one {from:2,to:0} through the SHARED applyPriority "+
			"(criterion 20)", pev)
	}

	// Given and the SAME: the idempotent no-op, no second event.
	s.call(t, ctx, "task_requeue", rqActor, task, requeueArgs(task, intp(0), ""))
	if pev := rqEvents(t, ctx, s, task, "priority_changed"); len(pev) != 1 {
		t.Errorf("re-applying the same priority wrote %d priority_changed events, want 1 — applyPriority's "+
			"same-value no-op (criterion 20)", len(pev))
	}

	// D6 step 5: one `reviewed` event per call, carrying the named payload.
	rev := rqEvents(t, ctx, s, task, "reviewed")
	if len(rev) != 3 {
		t.Fatalf("%d `reviewed` events after three calls, want 3 (D6 step 5: one per call)", len(rev))
	}
	first := rev[0]
	for _, k := range []string{"note", "from_status", "to_status", "priority_from", "priority_to",
		"priority_changed", "had_unreviewed_activity"} {
		if _, ok := first[k]; !ok {
			t.Errorf("the `reviewed` payload %v has no %q key (D6 step 5's payload)", first, k)
		}
	}
	if first["note"] != "not mine today" {
		t.Errorf("the `reviewed` payload's note = %v, want the note the caller passed", first["note"])
	}

	// Invariant 3: every call is an audit row, allowed for a human actor.
	if n := s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool='task_requeue'
	                      AND actor=$2 AND status='ok'`, task, rqActor); n != 3 {
		t.Errorf("task_requeue audit rows = %d, want 3 (invariant 3; and %s must PASS humanOnly)", n, rqActor)
	}
	// The other human shape passes too (criterion 22's handler-side control).
	s.call(t, ctx, "task_requeue", rqOpsctl, task, requeueArgs(task, nil, ""))
}

// Criterion 21: a second call changes nothing but reviewed_at and the second
// `reviewed` event — no second status_changed, and no priority_changed when
// priority is omitted. (IK: the owner works the board concurrently, so a
// double-tap must be a clean no-op.)
func TestRequeue_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)
	amRequire0039(t, ctx, s.pool)

	task, _ := s.task(t, ctx, "rq-idem", "holding")
	s.call(t, ctx, "task_requeue", rqActor, task, requeueArgs(task, nil, ""))
	afterFirst := maTaskRow(t, ctx, s, task)

	time.Sleep(5 * time.Millisecond)
	out := s.call(t, ctx, "task_requeue", rqActor, task, requeueArgs(task, nil, ""))
	afterSecond := maTaskRow(t, ctx, s, task)

	if out["status"] != "ready" || out["reviewed"] != true {
		t.Errorf("the second task_requeue = %v, want {status:ready, reviewed:true}", out)
	}
	if afterSecond.reviewedAt == nil || !afterSecond.reviewedAt.After(*afterFirst.reviewedAt) {
		t.Errorf("reviewed_at %v -> %v: the second call must move it (D2: monotone, so a later activity "+
			"re-surfaces and a double-tap clears it again)", afterFirst.reviewedAt, afterSecond.reviewedAt)
	}
	if afterSecond.priority != afterFirst.priority || afterSecond.status != afterFirst.status {
		t.Errorf("the second task_requeue changed status/priority %s/%d -> %s/%d",
			afterFirst.status, afterFirst.priority, afterSecond.status, afterSecond.priority)
	}
	if ev := rqEvents(t, ctx, s, task, "status_changed"); len(ev) != 1 {
		t.Errorf("%d status_changed events after two requeues, want 1: the task is already ready (criterion 21)", len(ev))
	}
	if ev := rqEvents(t, ctx, s, task, "priority_changed"); len(ev) != 0 {
		t.Errorf("%d priority_changed events with priority omitted, want 0 (criterion 21)", len(ev))
	}
	if ev := rqEvents(t, ctx, s, task, "reviewed"); len(ev) != 2 {
		t.Errorf("%d `reviewed` events after two requeues, want 2 (criterion 21)", len(ev))
	}
}

// ---- criterion 24 / D8: the close stamps the review -----------------------------

// A closed task leaves INCOMING through boardSectionOf's existing guard, so
// neither Dismiss nor Done needs a new argument. But a later SWT-36 reopen or
// SWT-45 revive would bring the row back with STALE unreviewed activity. One
// line prevents it: closeTransition's CLOSE update also sets reviewed_at; the
// REOPEN update leaves it alone.
func TestCloseStampsReviewedButReopenDoesNot(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)
	amRequire0039(t, ctx, s.pool)

	task, thread := s.task(t, ctx, "rq-close", "ready")
	now := s.dbNow(t, ctx)
	m := s.message(t, ctx, "rq-close", thread, "inbound", now, now)
	s.call(t, ctx, "task_mark_activity", maActor, task, activityArgs(task, m))
	if r := maTaskRow(t, ctx, s, task); r.activityAt == nil || r.reviewedAt != nil {
		t.Fatalf("CONTROL: before the close, activity_at=%v reviewed_at=%v, want set / NULL", r.activityAt, r.reviewedAt)
	}

	s.close(t, ctx, task)
	closed := maTaskRow(t, ctx, s, task)
	if closed.reviewedAt == nil {
		t.Fatalf("task_close left reviewed_at NULL. D8/criterion 24: closeTransition's CLOSE update stamps " +
			"reviewed_at = now(), so a later reopen does not bring the row back with STALE unreviewed activity")
	}
	if closed.activityAt == nil {
		t.Errorf("task_close cleared activity_at; D2 keeps the provenance — reviewed_at is what moves")
	}

	// The REOPEN update leaves reviewed_at alone: old activity stays reviewed.
	later := s.dbNow(t, ctx)
	m2 := s.message(t, ctx, "rq-close-2", thread, "inbound", later, later)
	s.revive(t, ctx, task, m2)
	reopened := maTaskRow(t, ctx, s, task)
	if reopened.status == "closed" {
		t.Fatalf("CONTROL: the revive did not reopen the task (status %q); the rest of this test is vacuous", reopened.status)
	}
	if reopened.reviewedAt == nil || !reopened.reviewedAt.Equal(*closed.reviewedAt) {
		t.Errorf("the reopen moved reviewed_at %v -> %v. D8: the REOPEN update leaves it alone, so old activity "+
			"stays reviewed and only NEW activity resurfaces (criterion 24)", closed.reviewedAt, reopened.reviewedAt)
	}
}
