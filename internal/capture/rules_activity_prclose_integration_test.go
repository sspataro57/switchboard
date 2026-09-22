//go:build integration

package capture_test

// activity-resurfaces (SWT-72, docs/tickets/activity-resurfaces_SPEC.md)
// criterion 12 and D3's ONE deliberate exclusion: a PR merge/close notice does
// NOT surface its review task. `activity_at` stays NULL and the task is closed
// exactly as SWT-54 closes it today.
//
// The argument, in D3's words: "The next call closes the task; surfacing a row
// in order to close it one statement later is noise, and when the close is
// refused the task is active work someone holds."
//
// Reuses prreview_integration_test.go's prrSuite wholesale — its GitHub-mail
// fixtures carry the real IMAP envelope (X-GitHub-Reason / -Sender /
// -Recipient) that the prClose decision is read from, so nothing here hands
// capture a header through a Go struct.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_swt72?sslmode=disable TZ=UTC \
//	  go test -tags integration -p 1 -count=1 -run CaptureActivityPRClose ./internal/capture/
//
// RED TODAY: migration 0039 is not applied; then the mark is not made anywhere,
// so the ACTIVE-work half (the control) fails.
//
// MUTATION THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - remove the prClose exclusion in capture -> the two closed rows gain an
//     activity_at.

import (
	"context"
	"fmt"
	"testing"
)

func TestCaptureActivityPRClose_Integration_AMergeNoticeDoesNotSurface(t *testing.T) {
	ctx := context.Background()
	s := newPRRSuite(t, ctx, prrOpts{})
	craRequire0039(t, ctx, s.pool)

	for i, n := range []int{9601, 9602} {
		s.colleague(t, ctx, prrWWW, n, "joseg-avviato", fmt.Sprintf("Change %d", n), 60-i)
	}
	s.pass(t, ctx, "live")
	merged := s.mustRef(t, ctx, prrKey(prrWWW, 9601))
	held := s.mustRef(t, ctx, prrKey(prrWWW, 9602))
	// 9602 is ACTIVE work: a session holds it, so the close is REFUSED — D3's
	// second sentence ("when the close is refused the task is active work
	// someone holds"), which must still not surface it.
	s.exec(t, ctx, `UPDATE tasks SET status='in_progress' WHERE id=$1`, held)
	s.exec(t, ctx, `UPDATE tasks SET activity_at = NULL, activity_by_message_id = NULL WHERE id IN ($1,$2)`, merged, held)

	const footer = "\n\n—\nReply to this email directly, view it on GitHub."
	notice := func(n int, body string, mins int) prrMsg {
		return s.mail(t, ctx, ghMail{repo: prrWWW, pr: n, reason: "state_change", sender: "joseg-avviato",
			recipient: prrLogin, subject: fmt.Sprintf("Re: [treetopllc/itest-prr-www] Change %d (PR #%d)", n, n),
			body: body, minsAgo: mins})
	}
	notice(9601, "Merged #9601 into main."+footer, 9)
	notice(9602, "Closed #9602."+footer, 8)
	st := s.pass(t, ctx, "live")

	if st.PRClosed != 1 {
		t.Errorf("RulesStats.PRClosed = %d, want 1 — SWT-54's close is unchanged (criterion 12)", st.PRClosed)
	}
	if got := s.status(t, ctx, merged); got != "closed" {
		t.Errorf("the merged PR's review task is %q, want closed (criterion 12: the task is closed as today)", got)
	}
	if got := s.status(t, ctx, held); got != "in_progress" {
		t.Errorf("the active review task is %q, want in_progress (close refuses active work: a skip)", got)
	}

	for _, c := range []struct {
		task int64
		what string
	}{{merged, "a merged PR whose review task was closed"}, {held, "a closed PR whose review task is active work"}} {
		var at *string
		if err := s.pool.QueryRow(ctx, `SELECT activity_at::text FROM tasks WHERE id=$1`, c.task).Scan(&at); err != nil {
			t.Fatalf("read activity_at of task %d: %v", c.task, err)
		}
		if at != nil {
			t.Errorf("%s has activity_at=%s. Criterion 12 / D3's one exclusion: capture skips the mark when "+
				"decision.prClose is set — surfacing a row in order to close it one statement later is noise, "+
				"and a refused close means someone is holding the work", c.what, *at)
		}
	}
	if n := s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE tool='task_mark_activity' AND task_id IN ($1,$2)`,
		merged, held); n != 0 {
		t.Errorf("%d task_mark_activity audit rows for the PR-close notices, want 0 (criterion 12)", n)
	}
	if st.Activity != 0 {
		t.Errorf("RulesStats.Activity = %d on a prClose-only pass, want 0 (criterion 12)", st.Activity)
	}

	// CONTROL: an ORDINARY comment on a live review task DOES surface it, so the
	// exclusion above is the prClose flag's doing and not a hook that never runs.
	live := s.mustRef(t, ctx, prrKey(prrWWW, 9602))
	s.exec(t, ctx, `UPDATE tasks SET status='ready' WHERE id=$1`, live)
	s.mail(t, ctx, ghMail{repo: prrWWW, pr: 9602, reason: "comment", sender: "joseg-avviato", recipient: prrLogin,
		subject: "Re: [treetopllc/itest-prr-www] Change 9602 (PR #9602)", body: "one more thought" + footer, minsAgo: 2})
	stc := s.pass(t, ctx, "live")
	var at *string
	if err := s.pool.QueryRow(ctx, `SELECT activity_at::text FROM tasks WHERE id=$1`, live).Scan(&at); err != nil {
		t.Fatalf("read activity_at of task %d: %v", live, err)
	}
	if at == nil || stc.Activity != 1 {
		t.Errorf("POSITIVE CONTROL FAILED: an ordinary comment on an open review task left activity_at=%v and "+
			"Activity=%d; the exclusion above proves nothing if the hook never fires on this path", at, stc.Activity)
	}
}
