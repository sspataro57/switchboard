//go:build integration

package tools_test

// task_mark_surfaced against a real database — SWT-45
// (docs/tickets/jira-activity-revive_SPEC.md) J7 and criterion 15's handler
// half: under the tasks row lock it refuses a non-inbound message with an ERROR,
// skips a closed task, is a no-op when surfaced_by_message_id already equals the
// message, and otherwise writes surfaced_at = now() and the message id. Called
// through the executor as capture:google with the REAL policy matrix, so a
// humanOnly slip would show here as a denial. Reuses revive_integration_test.go's
// suite (its cleanup pact, its require-0030 sentence).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_iso45?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run MarkSurfaced ./internal/tools/
//
// RED TODAY: 0030 is not applied; after that, the tool is not registered.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const msActor = "capture:google"

func markArgs(taskID, messageID int64) string {
	b, _ := json.Marshal(map[string]any{"task_id": taskID, "message_id": messageID,
		"reason": "capture: created by an overriding rule"})
	return string(b)
}

func TestMarkSurfaced_StampsAnOpenTaskOncePerMessage(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)

	task, thread := s.task(t, ctx, "ms", "ready")
	now := s.dbNow(t, ctx)
	m1 := s.message(t, ctx, "ms1", thread, "inbound", now, now)

	out := s.call(t, ctx, "task_mark_surfaced", msActor, task, markArgs(task, m1))
	if out["surfaced"] != true {
		t.Errorf("task_mark_surfaced on an open task = %v, want surfaced:true", out)
	}
	r := s.row(t, ctx, task)
	if r.surfacedAt == nil || r.surfacedBy == nil || *r.surfacedBy != m1 {
		t.Fatalf("after task_mark_surfaced surfaced_at=%v by=%v, want set / %d", r.surfacedAt, r.surfacedBy, m1)
	}
	if n := s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool='task_mark_surfaced'
	                      AND actor=$2 AND status='ok'`, task, msActor); n != 1 {
		t.Errorf("task_mark_surfaced audit rows = %d, want 1 — invariant 3, and the policy decision for "+
			"capture:google must be allow (not humanOnly)", n)
	}

	// The same message twice: a no-op; surfaced_at does NOT move.
	time.Sleep(5 * time.Millisecond)
	s.call(t, ctx, "task_mark_surfaced", msActor, task, markArgs(task, m1))
	if r2 := s.row(t, ctx, task); r2.surfacedAt == nil || !r2.surfacedAt.Equal(*r.surfacedAt) {
		t.Errorf("a replayed task_mark_surfaced moved surfaced_at %v -> %v. J7: no-op if surfaced_by_message_id "+
			"already equals this message — a moved instant reads as a NEW surfacing to the reconciler and earns a "+
			"second log line", r.surfacedAt, r2.surfacedAt)
	}

	// A different, later message re-stamps.
	later := s.dbNow(t, ctx)
	m2 := s.message(t, ctx, "ms2", thread, "inbound", later, later)
	s.call(t, ctx, "task_mark_surfaced", msActor, task, markArgs(task, m2))
	if r3 := s.row(t, ctx, task); r3.surfacedBy == nil || *r3.surfacedBy != m2 || !r3.surfacedAt.After(*r.surfacedAt) {
		t.Errorf("a different message left surfaced_by=%v surfaced_at=%v, want %d and later than %v", r3.surfacedBy,
			r3.surfacedAt, m2, r.surfacedAt)
	}
}

func TestMarkSurfaced_ANonInboundMessageIsAnError(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)

	task, thread := s.task(t, ctx, "ms-out", "ready")
	ours := s.message(t, ctx, "ms-out", thread, "outbound", s.dbNow(t, ctx), s.dbNow(t, ctx))
	for _, msg := range []int64{ours, 987654321} {
		if out, err := s.run(ctx, "task_mark_surfaced", msActor, task, markArgs(task, msg)); err == nil || strings.Contains(err.Error(), "unknown tool") || strings.Contains(err.Error(), "validate ") {
			t.Errorf("task_mark_surfaced with message %d (outbound or missing) = %v, want an ERROR (invariant 5)", msg, out)
		}
	}
	if r := s.row(t, ctx, task); r.surfacedAt != nil {
		t.Errorf("a refused task_mark_surfaced wrote surfaced_at=%v", r.surfacedAt)
	}
}

func TestMarkSurfaced_AClosedTaskIsSkipped(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)

	task, thread := s.task(t, ctx, "ms-closed", "ready")
	s.close(t, ctx, task)
	m := s.message(t, ctx, "ms-closed", thread, "inbound", s.dbNow(t, ctx), s.dbNow(t, ctx))
	out := s.call(t, ctx, "task_mark_surfaced", msActor, task, markArgs(task, m))
	if out["surfaced"] != false || out["skipped"] == nil || out["skipped"] == "" {
		t.Errorf("task_mark_surfaced on a closed task = %v, want {surfaced:false, skipped:<reason>}. J7: a closed "+
			"task is skipped — the crash window degrades to today's behaviour and the next overriding email revives it", out)
	}
	if r := s.row(t, ctx, task); r.surfacedAt != nil || r.status != "closed" {
		t.Errorf("a skipped task_mark_surfaced left status=%s surfaced_at=%v, want closed / NULL", r.status, r.surfacedAt)
	}
}
