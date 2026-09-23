//go:build integration

package capture_test

// REGRESSION — SWT-80 / swb #553, bug `revived-task-not-in-incoming`
// (docs/bugs/revived-task-not-in-incoming_DIAGNOSIS.md, "Proposed fix scope" ->
// Tests 3 and 4).
//
// The fix moves capture's task_mark_activity from BEFORE the revive/reopen to
// AFTER it ("log -> reopen/revive -> mark"). task_mark_activity keeps skipping a
// closed task. These tests pin both halves of that:
//
//   - AuditOrder_*: on a revive and on a dismissal reopen, the audit trail for the
//     message reads task_append_log -> task_reopen -> task_mark_activity, all as
//     capture's actor, and the mark LANDS (column read back). RED today: the order
//     is append -> mark -> reopen and the mark is a skip (production #452/#381).
//
//   - ClosedNotReopened_*: five shapes where the message is logged onto a CLOSED
//     task and the task is NOT brought back. Each must leave activity_at /
//     activity_by_message_id unchanged, the task still closed, and exactly one
//     ok task_mark_activity audit row for the message (proof the call was MADE
//     and skipped — not left out). GREEN today and must stay green after the fix.
//     audit_events stores no tool result, so the "task_closed" answer is proven by
//     the triple: an ok audit row for this message + the task still closed after
//     the pass + the columns unchanged (the tool's only non-error, non-marking
//     answers are task_closed and already_marked_by_message, and the latter is
//     excluded because the columns never named this message).
//
// Reuses crvSuite (rules_revive_integration_test.go, the SWT-45/SWT-72 fixture)
// and crrSuite (rules_reopen_integration_test.go, the SWT-36 fixture). Both
// delete capture_decisions wholesale: run against the PRIVATE database.
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_revincoming?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run TestRegression_SWT80_ ./internal/capture/ -v

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/capture"
)

type s80Activity struct {
	at *time.Time
	by *int64
}

func s80ReadActivity(t *testing.T, ctx context.Context, pool *pgxpool.Pool, task int64) s80Activity {
	t.Helper()
	var a s80Activity
	if err := pool.QueryRow(ctx, `SELECT activity_at, activity_by_message_id FROM tasks WHERE id=$1`, task).
		Scan(&a.at, &a.by); err != nil {
		t.Fatalf("read activity of task %d: %v", task, err)
	}
	return a
}

func (a s80Activity) equal(b s80Activity) bool {
	sameTime := (a.at == nil && b.at == nil) || (a.at != nil && b.at != nil && a.at.Equal(*b.at))
	sameBy := (a.by == nil && b.by == nil) || (a.by != nil && b.by != nil && *a.by == *b.by)
	return sameTime && sameBy
}

// s80AssertSkippedMark is the negative's contract: the task stayed closed, its
// activity columns did not move, and the mark WAS attempted for this message
// (one ok audit row as the capture actor) and answered with a skip.
func s80AssertSkippedMark(t *testing.T, ctx context.Context, pool *pgxpool.Pool, task, msg int64, actor string,
	before s80Activity, st capture.RulesStats, label string) {
	t.Helper()
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, task).Scan(&status); err != nil {
		t.Fatalf("read task %d: %v", task, err)
	}
	if status != "closed" {
		t.Errorf("%s: task %d is %q, want still closed — this shape must NOT bring the task back", label, task, status)
	}
	after := s80ReadActivity(t, ctx, pool, task)
	if !after.equal(before) {
		t.Errorf("%s: activity moved (at %s -> %s, by %s -> %s); a closed task that was NOT reopened must be "+
			"skipped by task_mark_activity (D8: a later human reopen must not resurface stale activity)",
			label, s80Ptr(before.at), s80Ptr(after.at), s80Ptr(before.by), s80Ptr(after.by))
	}
	if after.by != nil && *after.by == msg {
		t.Errorf("%s: activity_by_message_id = the message %d on a closed, not-reopened task", label, msg)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events
		  WHERE task_id=$1 AND tool='task_mark_activity' AND actor=$2 AND status='ok'
		    AND (args->>'message_id')::bigint = $3`, task, actor, msg).Scan(&n); err != nil {
		t.Fatalf("count mark audits: %v", err)
	}
	if n != 1 {
		t.Errorf("%s: %d ok task_mark_activity audit row(s) for message %d as %s, want exactly 1 — the call is MADE "+
			"(one call site after the revive/reopen chain) and the tool answers task_closed", label, n, msg, actor)
	}
	if st.Activity != 0 {
		t.Errorf("%s: RulesStats.Activity = %d, want 0 (marked:false)", label, st.Activity)
	}
}

// s80AuditSeq is the capture actor's audit trail for ONE message on ONE task:
// the three tools the fix orders, oldest first.
func s80AuditSeq(t *testing.T, ctx context.Context, pool *pgxpool.Pool, task, msg int64, actor string) []string {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT tool FROM audit_events
		  WHERE task_id=$1 AND actor=$2 AND status='ok'
		    AND tool IN ('task_append_log','task_reopen','task_mark_activity')
		    AND (tool = 'task_append_log' OR (args->>'message_id')::bigint = $3)
		    AND id > COALESCE((SELECT max(id) FROM audit_events
		                        WHERE task_id=$1 AND tool IN ('task_close','task_dismiss')), 0)
		  ORDER BY id`, task, actor, msg)
	if err != nil {
		t.Fatalf("read audit order: %v", err)
	}
	defer rows.Close()
	var seq []string
	for rows.Next() {
		var tool string
		if err := rows.Scan(&tool); err != nil {
			t.Fatalf("scan audit: %v", err)
		}
		seq = append(seq, tool)
	}
	return seq
}

func s80SeqIs(seq []string, want ...string) bool {
	if len(seq) != len(want) {
		return false
	}
	for i := range seq {
		if seq[i] != want[i] {
			return false
		}
	}
	return true
}

// ---- Tests 4: the audit ORDER -------------------------------------------------

// A revive: task_append_log -> task_reopen (revive form) -> task_mark_activity.
// RED today: [task_append_log task_mark_activity task_reopen] and the mark skips.
func TestRegression_SWT80_AuditOrder_ReviveIsLogThenReopenThenMark(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	craRequire0039(t, ctx, s.pool)
	s.mentionRule(t, ctx, true)

	task, closedAt := s.closedTask(t, ctx, "CRV-8001")
	m := s.slackMsg(t, ctx, "CRV-8001 is broken again", closedAt.Add(time.Second), closedAt.Add(time.Second))
	st := s.pass(t, ctx, capture.RulesModeLive)
	if st.Revived != 1 {
		t.Fatalf("setup: Revived = %d, want 1 (the revive is what this test orders around)", st.Revived)
	}

	seq := s80AuditSeq(t, ctx, s.pool, task, m, crvActor)
	if !s80SeqIs(seq, "task_append_log", "task_reopen", "task_mark_activity") {
		t.Errorf("audit sequence for the reviving message = %v, want [task_append_log task_reopen "+
			"task_mark_activity]. SWT-80: a mark before the revive sees a closed task and skips (the `ok` audit "+
			"row hides it) — production #381's trail", seq)
	}
	if n := s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND actor=$2 AND tool='task_reopen'
	                        AND (args->>'revive')::boolean AND (args->>'message_id')::bigint=$3`, task, crvActor, m); n != 1 {
		t.Errorf("%d task_reopen audit rows in the REVIVE form (revive:true, message %d), want 1", n, m)
	}
	if a := s80ReadActivity(t, ctx, s.pool, task); a.by == nil || *a.by != m {
		t.Errorf("activity_by_message_id = %s after the revive, want %d — the mark after the revive must LAND",
			s80Ptr(a.by), m)
	}
}

// A dismissal reopen (SWT-36): the same order, the guarded (dismissal_id) form.
func TestRegression_SWT80_AuditOrder_DismissalReopenIsLogThenReopenThenMark(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	craRequire0039(t, ctx, s.pool)

	task := craSeedTask(t, ctx, s, "CRV-8002")
	craDismiss(t, ctx, s, task)
	now := s.dbNow(t, ctx)
	m := s.jiraMsg(t, ctx, "CRV-8002", now, now)
	st := s.pass(t, ctx, capture.RulesModeLive)
	if st.Reopened != 1 {
		t.Fatalf("setup: Reopened = %d, want 1", st.Reopened)
	}

	seq := s80AuditSeq(t, ctx, s.pool, task, m, crvActor)
	if !s80SeqIs(seq, "task_append_log", "task_reopen", "task_mark_activity") {
		t.Errorf("audit sequence for the reopening message = %v, want [task_append_log task_reopen "+
			"task_mark_activity] (SWT-80; production #452's trail was append -> mark -> reopen)", seq)
	}
	if a := s80ReadActivity(t, ctx, s.pool, task); a.by == nil || *a.by != m {
		t.Errorf("activity_by_message_id = %s after the dismissal reopen, want %d", s80Ptr(a.by), m)
	}
}

// ---- Tests 3: closed and NOT reopened — the mark is still skipped -----------

// (a) SWT-53 resurface: a NON-reviving rule's task_log onto a closed task with
// no open dismissal. The chat becomes its own inquiry item; the closed task is
// left alone.
func TestRegression_SWT80_ClosedNotReopened_ResurfaceLeavesActivity(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	craRequire0039(t, ctx, s.pool)
	s.mentionRule(t, ctx, false)

	task, closedAt := s.closedTask(t, ctx, "CRV-8101")
	before := s80ReadActivity(t, ctx, s.pool, task)
	m := s.slackMsg(t, ctx, "any news on CRV-8101?", closedAt.Add(time.Second), closedAt.Add(time.Second))
	st := s.pass(t, ctx, capture.RulesModeLive)

	var resurface bool
	if err := s.pool.QueryRow(ctx, `SELECT resurface FROM capture_decisions WHERE message_id=$1 AND mode='live'`, m).
		Scan(&resurface); err != nil {
		t.Fatalf("read the live decision: %v", err)
	}
	if !resurface {
		t.Fatalf("setup: capture_decisions.resurface = false; this fixture must be SWT-53's resurface shape")
	}
	if n := s.audits(t, ctx, task, "task_reopen", crvActor); n != 0 {
		t.Errorf("task_reopen called %d time(s); a resurface never reopens", n)
	}
	s80AssertSkippedMark(t, ctx, s.pool, task, m, crvActor, before, st, "SWT-53 resurface")
}

// (b) Rule 75's shape: a non-reviving body_regex rule takes a Jira NOTIFIER
// email copy (the sender is on projects.notifier_senders) about a ticket the
// RECONCILER closed. No revive, no resurface, no mark. Production: #381's
// 22:50 and 13:16 marks (audits 5705, 6201).
func TestRegression_SWT80_ClosedNotReopened_NotifierCopyOnReconcilerClosedTask(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	craRequire0039(t, ctx, s.pool)
	const key = "CRV-8102"

	s.snapshot(t, ctx, key, "indeterminate", "In Progress")
	start := s.dbNow(t, ctx).Add(-30 * time.Minute)
	s.jiraMsg(t, ctx, key, start, start)
	s.pass(t, ctx, capture.RulesModeLive)
	task := s.mustTask(t, ctx, key)
	s.snapshot(t, ctx, key, "done", "Done")
	s.reconcile(t, ctx)
	if got := s.status(t, ctx, task); got != "closed" {
		t.Fatalf("setup: the reconciler did not close %s (%q)", key, got)
	}
	if n := s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool='task_close' AND actor=$2 AND status='ok'`,
		task, crvTSActor); n != 1 {
		t.Fatalf("setup: %d task_close audits by %s, want 1 (a RECONCILER close)", n, crvTSActor)
	}

	s.exec(t, ctx, `UPDATE projects SET notifier_senders = ARRAY[$2::text] WHERE id=$1`, s.project, crvMailFrom)
	s.mentionRule(t, ctx, false) // rule 75: non-reviving body_regex on the key
	before := s80ReadActivity(t, ctx, s.pool, task)
	now := s.dbNow(t, ctx)
	m := s.mailMsg(t, ctx, crvMailFrom, "inbound", "[JIRA] ("+key+") Katie Evans commented",
		"Katie Evans commented on "+key, now, now)
	st := s.pass(t, ctx, capture.RulesModeLive)

	var resurface bool
	var action string
	if err := s.pool.QueryRow(ctx, `SELECT action, resurface FROM capture_decisions WHERE message_id=$1 AND mode='live'`, m).
		Scan(&action, &resurface); err != nil {
		t.Fatalf("read the live decision: %v", err)
	}
	if action != "task_log" || resurface {
		t.Fatalf("setup: decision action=%q resurface=%v, want task_log / false (a notifier copy logs silently)", action, resurface)
	}
	if n := s.audits(t, ctx, task, "task_reopen", crvActor); n != 0 || st.Revived != 0 {
		t.Errorf("task_reopen called %d time(s), Revived=%d; a non-reviving rule's notifier copy never revives", n, st.Revived)
	}
	s80AssertSkippedMark(t, ctx, s.pool, task, m, crvActor, before, st, "notifier email copy")
}

// (c) A revive the HANDLER refuses: the message was ingested before the close,
// so task_reopen answers message_predates_close. Capture asked; nothing reopened.
func TestRegression_SWT80_ClosedNotReopened_RefusedReviveLeavesActivity(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	craRequire0039(t, ctx, s.pool)
	s.mentionRule(t, ctx, true)

	task, closedAt := s.closedTask(t, ctx, "CRV-8103")
	before := s80ReadActivity(t, ctx, s.pool, task)
	m := s.slackMsg(t, ctx, "earlier: CRV-8103 is fine now", closedAt.Add(-10*time.Minute), closedAt.Add(-10*time.Minute))
	st := s.pass(t, ctx, capture.RulesModeLive)

	if n := s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool='task_reopen' AND actor=$2
	                        AND status='ok' AND (args->>'message_id')::bigint=$3`, task, crvActor, m); n != 1 {
		t.Fatalf("setup: %d task_reopen audits for the message, want 1 (capture asks; the handler refuses)", n)
	}
	if st.Revived != 0 {
		t.Fatalf("setup: Revived = %d, want 0 (message_predates_close)", st.Revived)
	}
	s80AssertSkippedMark(t, ctx, s.pool, task, m, crvActor, before, st, "refused revive (message_predates_close)")
}

// (d) The own-action guard (SWT-45 J17, ownActionSkip): Jira's email about HIS
// OWN comment on a closed task with no open dismissal. Logged, not revived, not
// marked.
func TestRegression_SWT80_ClosedNotReopened_HisOwnJiraCommentLeavesActivity(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	craRequire0039(t, ctx, s.pool)
	s.mailRule(t, ctx, true)
	s.ownActionPoller(t, ctx)

	task, closedAt := s.closedTask(t, ctx, "CRV-8104")
	emailSent := closedAt.Add(-10 * time.Minute)
	s.hisComment(t, ctx, "CRV-8104", emailSent.Add(-5*time.Minute))
	s.pollerRan(t, ctx, s.dbNow(t, ctx))
	before := s80ReadActivity(t, ctx, s.pool, task)
	m := s.anonMailMsg(t, ctx, "[JIRA] (CRV-8104) Fix the export", closedAt.Add(time.Second), emailSent)
	st := s.pass(t, ctx, capture.RulesModeLive)

	if reason, _ := s.decisionReason(t, ctx, m, capture.RulesModeLive); !containsAll(reason, "own_action", "revive skipped") {
		t.Fatalf("setup: live decision reason = %q, want the own-action skip", reason)
	}
	if n := s.audits(t, ctx, task, "task_reopen", crvActor); n != 0 {
		t.Errorf("task_reopen called %d time(s) for his own action", n)
	}
	s80AssertSkippedMark(t, ctx, s.pool, task, m, crvActor, before, st, "own-action guard")
}

// (e) A dismissal reopen the HANDLER refuses: the message was ingested before
// the dismissal, so task_reopen answers message_predates_dismissal.
func TestRegression_SWT80_ClosedNotReopened_RefusedDismissalReopenLeavesActivity(t *testing.T) {
	ctx := context.Background()
	s := newCRRSuite(t, ctx)

	f := s.createTask(t, ctx)
	s.dismiss(t, ctx, &f, "handled_elsewhere")
	before := s80ReadActivity(t, ctx, s.pool, f.task)
	prior := s.message(t, ctx, "swt80-prior", "inbound", f.dismissedAt.Add(-time.Minute), f.dismissedAt.Add(-time.Minute))
	st := s.pass(t, ctx, capture.RulesModeLive)

	if n := s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool='task_reopen' AND actor=$2
	                          AND status='ok' AND (args->>'message_id')::bigint=$3`, f.task, crrActor, prior); n != 1 {
		t.Fatalf("setup: %d task_reopen audits for the predating message, want 1 (capture asks; the handler refuses)", n)
	}
	if st.Reopened != 0 {
		t.Fatalf("setup: Reopened = %d, want 0 (message_predates_dismissal)", st.Reopened)
	}
	s80AssertSkippedMark(t, ctx, s.pool, f.task, prior, crrActor, before, st, "refused dismissal reopen")
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
