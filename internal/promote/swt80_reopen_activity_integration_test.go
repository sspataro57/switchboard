//go:build integration

package promote_test

// REGRESSION — SWT-80 / swb #553, bug `revived-task-not-in-incoming`
// (docs/bugs/revived-task-not-in-incoming_DIAGNOSIS.md, "Proposed fix scope" ->
// Tests 5). The promote half of the bug, production #155 (promote:inquiry,
// 2026-09-22 18:03:53Z, message 405014, audits 5215-5217): a new inquiry ask on
// the thread of a DISMISSED task attaches to it and reopenDismissed reopens it —
// but act's "attached" branch called task_mark_activity BEFORE the reopen, the
// tool skipped the still-closed task, and the reopened task sat in QUEUE with
// activity_at NULL.
//
// Reuses the inquiry suite (inquiry_integration_test.go: iqpSuite and the
// ReopensADismissedTask fixture shape). NO LLM: verdicts are hand-written
// ai_runs/ai_extractions rows.
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_revincoming?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run TestRegression_SWT80_ ./internal/promote/ -v
//
// RED TODAY: ReopenedDismissedTaskLandsInIncoming (activity not the message,
// audit order append -> mark -> reopen). GREEN TODAY and after the fix:
// RefusedReopenLeavesActivity (message_predates_dismissal: not reopened, not marked).

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/promote"
)

func s80pFmt[T any](p *T) string {
	if p == nil {
		return "NULL"
	}
	return fmt.Sprint(*p)
}

type s80pActivity struct {
	at *time.Time
	by *int64
}

func (s *iqpSuite) s80Activity(t *testing.T, ctx context.Context, task int64) s80pActivity {
	t.Helper()
	var a s80pActivity
	if err := s.pool.QueryRow(ctx, `SELECT activity_at, activity_by_message_id FROM tasks WHERE id=$1`, task).
		Scan(&a.at, &a.by); err != nil {
		t.Fatalf("read activity of task %d: %v", task, err)
	}
	return a
}

// s80DismissedThreadTask: an ask creates the thread's task through a real
// inquiry pass, then a human dismisses it (task_dismiss on the executor).
func (s *iqpSuite) s80DismissedThreadTask(t *testing.T, ctx context.Context, label string) (key string, task, dismissal int64, dismissedAt time.Time) {
	t.Helper()
	key = gmailKey(label)
	a, ar := s.message(t, ctx, iqpMsg{label: label + "-a", key: key, sentAt: s.ago(3 * time.Hour)})
	s.decision(t, ctx, a, "live", "attributed", s.armed)
	s.verdict(t, ctx, a, ar, iqpV{scope: "thread"})
	s.run(t, ctx, promote.Config{})
	task = s.taskOf(t, ctx, a)
	s.execute(t, ctx, "task_dismiss", iqpHuman, task, fmt.Sprintf(`{"task_id":%d,"reason_code":"not_actionable"}`, task))
	if err := s.pool.QueryRow(ctx, `SELECT id, created_at FROM task_dismissals WHERE task_id=$1 ORDER BY id DESC LIMIT 1`, task).
		Scan(&dismissal, &dismissedAt); err != nil {
		t.Fatalf("setup: read the dismissal: %v", err)
	}
	if st := s.status(t, ctx, task); st != "closed" {
		t.Fatalf("setup: the dismissed task is %q, want closed", st)
	}
	return key, task, dismissal, dismissedAt
}

// #155's shape: rule 2 of Decide (dismissed thread task -> attach + reopen).
// After the fix the reopened task is marked with the ask and lands in INCOMING.
func TestRegression_SWT80_PromoteReopenedDismissedTaskLandsInIncoming(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	key, task, dismissal, _ := s.s80DismissedThreadTask(t, ctx, "swt80-reopen")

	// Ingested AFTER the dismissal (created_at = now()), sent 90 minutes ago (past grace).
	b, br := s.message(t, ctx, iqpMsg{label: "swt80-reopen-b", key: key, sentAt: s.ago(90 * time.Minute)})
	s.decision(t, ctx, b, "live", "attributed", s.armed)
	s.verdict(t, ctx, b, br, iqpV{scope: "thread", ask: "still waiting on this"})
	st := s.run(t, ctx, promote.Config{})

	// Works today (SWT-36 D8): the attach and the reopen.
	p, ok := s.promotion(t, ctx, b)
	if !ok || p.action != "attached" || p.taskID == nil || *p.taskID != task {
		t.Fatalf("setup: new ask on a dismissed task's thread: %+v (found=%v), want attached to %d", p, ok, task)
	}
	if st.Reopened != 1 || s.status(t, ctx, task) == "closed" {
		t.Fatalf("setup: Reopened=%d status=%q, want 1 / open (dismissal %d)", st.Reopened, s.status(t, ctx, task), dismissal)
	}

	// The bug: the reopened task must carry the ask as its activity and be
	// needs-review (activity_at > reviewed_at — the dismissal stamped reviewed_at).
	a := s.s80Activity(t, ctx, task)
	if a.by == nil || *a.by != b || a.at == nil {
		t.Errorf("reopened task %d has activity_by=%s activity_at=%s, want %d / set. SWT-80 (production #155): "+
			"act marked BEFORE reopenDismissed, the tool skipped the still-dismissed task, and the reopened task "+
			"sat in QUEUE", task, s80pFmt(a.by), s80pFmt(a.at), b)
	}
	var needsReview bool
	if err := s.pool.QueryRow(ctx,
		`SELECT status <> 'closed' AND activity_at IS NOT NULL AND (reviewed_at IS NULL OR activity_at > reviewed_at)
		   FROM tasks WHERE id=$1`, task).Scan(&needsReview); err != nil {
		t.Fatalf("read needs-review: %v", err)
	}
	if !needsReview {
		t.Errorf("reopened task %d is not needs-review: it is in QUEUE, not INCOMING (SWT-80)", task)
	}
	if st.Activity != 1 {
		t.Errorf("Stats.Activity = %d, want 1 (the mark after the reopen answers marked:true)", st.Activity)
	}

	// The audit ORDER for the ask, as promote:inquiry: append -> reopen -> mark.
	rows, err := s.pool.Query(ctx,
		`SELECT tool FROM audit_events
		  WHERE task_id=$1 AND actor='promote:inquiry' AND status='ok'
		    AND tool IN ('task_append_log','task_reopen','task_mark_activity')
		    AND id > (SELECT max(id) FROM audit_events WHERE task_id=$1 AND tool='task_dismiss')
		  ORDER BY id`, task)
	if err != nil {
		t.Fatalf("read audit order: %v", err)
	}
	defer rows.Close()
	var seq []string
	for rows.Next() {
		var tool string
		if err := rows.Scan(&tool); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seq = append(seq, tool)
	}
	if len(seq) != 3 || seq[0] != "task_append_log" || seq[1] != "task_reopen" || seq[2] != "task_mark_activity" {
		t.Errorf("promote:inquiry audit sequence on the reopened task = %v, want [task_append_log task_reopen "+
			"task_mark_activity] (SWT-80; #155's trail was append -> mark -> reopen, audits 5215-5217)", seq)
	}
}

// The negative: a reopen the HANDLER refuses (the ask was ingested before the
// dismissal: message_predates_dismissal). The attach happens, the task stays
// closed, and its activity columns do not move — the mark is attempted and
// skipped (task_closed).
func TestRegression_SWT80_PromoteRefusedReopenLeavesActivity(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	key, task, _, dismissedAt := s.s80DismissedThreadTask(t, ctx, "swt80-refused")

	b, br := s.message(t, ctx, iqpMsg{label: "swt80-refused-b", key: key, sentAt: s.ago(90 * time.Minute)})
	// Ingested BEFORE the dismissal (normalized_messages.created_at is the clock
	// task_reopen's guard reads, D2); classified after it — the verdict lag.
	s.exec(t, ctx, `UPDATE normalized_messages SET created_at = $2 WHERE id=$1`, b, dismissedAt.Add(-time.Minute))
	s.decision(t, ctx, b, "live", "attributed", s.armed)
	s.verdict(t, ctx, b, br, iqpV{scope: "thread", ask: "one more thing"})
	before := s.s80Activity(t, ctx, task)
	st := s.run(t, ctx, promote.Config{})

	p, ok := s.promotion(t, ctx, b)
	if !ok || p.action != "attached" || p.taskID == nil || *p.taskID != task {
		t.Fatalf("setup: ask on a dismissed task's thread: %+v (found=%v), want attached to %d", p, ok, task)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND actor='promote:inquiry'
	                          AND tool='task_reopen' AND status='ok' AND (args->>'message_id')::bigint=$2`, task, b); n != 1 {
		t.Fatalf("setup: %d task_reopen audits for the ask, want 1 (promote asks; the handler refuses)", n)
	}
	if got := s.status(t, ctx, task); got != "closed" || st.Reopened != 0 {
		t.Fatalf("setup: status=%q Reopened=%d, want closed / 0 (message_predates_dismissal)", got, st.Reopened)
	}

	after := s.s80Activity(t, ctx, task)
	sameAt := (before.at == nil && after.at == nil) || (before.at != nil && after.at != nil && before.at.Equal(*after.at))
	sameBy := (before.by == nil && after.by == nil) || (before.by != nil && after.by != nil && *before.by == *after.by)
	if !sameAt || !sameBy {
		t.Errorf("a refused reopen moved activity (at %s -> %s, by %s -> %s); a task that stays closed must not be "+
			"marked (D8)", s80pFmt(before.at), s80pFmt(after.at), s80pFmt(before.by), s80pFmt(after.by))
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND actor='promote:inquiry'
	                          AND tool='task_mark_activity' AND status='ok' AND (args->>'message_id')::bigint=$2`, task, b); n != 1 {
		t.Errorf("%d ok task_mark_activity audits for the ask, want 1 — the lane-agnostic call is made and the tool "+
			"answers task_closed", n)
	}
	if st.Activity != 0 {
		t.Errorf("Stats.Activity = %d, want 0", st.Activity)
	}
}
