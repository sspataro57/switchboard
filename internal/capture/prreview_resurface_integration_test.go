//go:build integration

package capture_test

// The SWT-53 (chat-on-closed-task) x SWT-54 (treetop-pr-review-tasks) seam,
// against a real database. Written at the merge of main (SWT-53, 0034) into
// SWT-54 (0035):
//
//   - SWT-54 D4's cross-ticket rule: a GitHub PR state notice (merged, closed or
//     reopened) logged onto a CLOSED review task NEVER resurfaces through the
//     inquiry lane. resurfaces() carries a prNotice disqualifier with its own
//     reason, set from decideMessage's d.prNotice. MUTATION: drop `case
//     in.prNotice` from resurfaces, or pass `prNotice: false` at the call site
//     → red.
//   - Ordinary PR mail (a colleague's comment) on a closed review task is a
//     task_log; what keeps it silent is the notifier list, because GitHub mail
//     comes from notifications@github.com (the collaboratory seed, owner decision
//     2026-09-14). With an EMPTY list it resurfaces, as any human message on a
//     closed task does (the control rows below).
//   - An UNTRUSTED state notice falls through (SWT-54 D1 amendment) and so
//     carries no notice flag: it closes nothing, and where it lands on a closed
//     bucket through rule 10 the GitHub notifier seed keeps it silent.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_isomerge?sslmode=disable TZ=UTC \
//	  go test -tags integration -p 1 -count=1 -run 'CapturePRReviewResurface' ./internal/capture/
//
// Uses the prrSuite harness (prreview_integration_test.go): same fixtures, same
// wholesale capture_decisions cleanup, so run it on a private database.

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// prrResurface reads the resurface fact capture WROTE on the message's live
// decision (the column, not a Go value).
func (s *prrSuite) prrResurface(t *testing.T, ctx context.Context, m prrMsg) (bool, string) {
	t.Helper()
	var resurface bool
	var reason string
	if err := s.pool.QueryRow(ctx,
		`SELECT resurface, COALESCE(reason,'') FROM capture_decisions
		  WHERE message_id=$1 AND mode='live' ORDER BY id DESC LIMIT 1`, m.id).Scan(&resurface, &reason); err != nil {
		t.Fatalf("read the live resurface of message %d (%s): %v", m.id, m.mid, err)
	}
	return resurface, reason
}

func (s *prrSuite) closeReviewTask(t *testing.T, ctx context.Context, task int64) {
	t.Helper()
	s.call(t, ctx, "task_close", task, fmt.Sprintf(`{"task_id":%d,"reason":"reviewed"}`, task))
	if got := s.status(t, ctx, task); got != "closed" {
		t.Fatalf("fixture: task %d status = %q after task_close, want closed", task, got)
	}
}

func (s *prrSuite) seedGitHubNotifier(t *testing.T, ctx context.Context) {
	t.Helper()
	s.exec(t, ctx, `UPDATE projects SET notifier_senders = ARRAY['notifications@github.com'] WHERE id=$1`, s.collab)
}

func TestCapturePRReviewResurface_Integration_AStateNoticeOnAClosedReviewTaskNeverResurfaces(t *testing.T) {
	ctx := context.Background()
	s := newPRRSuite(t, ctx, prrOpts{})

	s.colleague(t, ctx, prrWWW, 9901, "joseg-avviato", "Merge me", 30)
	s.colleague(t, ctx, prrWWW, 9902, "ananthsekar007", "Close me", 29)
	s.colleague(t, ctx, prrWWW, 9903, "joseg-avviato", "Reopen me", 28)
	s.colleague(t, ctx, prrWWW, 9904, "ananthsekar007", "Talk to me", 27)
	s.pass(t, ctx, "live")
	merged, closed := s.mustRef(t, ctx, prrKey(prrWWW, 9901)), s.mustRef(t, ctx, prrKey(prrWWW, 9902))
	reopened, control := s.mustRef(t, ctx, prrKey(prrWWW, 9903)), s.mustRef(t, ctx, prrKey(prrWWW, 9904))
	for _, task := range []int64{merged, closed, reopened, control} {
		s.closeReviewTask(t, ctx, task)
	}
	// The fixture pin: the notifier list is EMPTY, so no notice below is kept
	// silent by the list. Only the PR-notice disqualifier can keep it silent.
	if n := s.n(t, ctx, `SELECT cardinality(notifier_senders) FROM projects WHERE id=$1`, s.collab); n != 0 {
		t.Fatalf("fixture: collab's notifier_senders has %d entries, want 0 (the notice must not be masked by the list)", n)
	}
	logs := map[int64]int{}
	for _, task := range []int64{merged, closed, reopened, control} {
		logs[task] = s.events(t, ctx, task, "log")
	}

	const footer = "\n\n—\nReply to this email directly, view it on GitHub."
	notice := func(n int, title, body string) prrMsg {
		return s.mail(t, ctx, ghMail{repo: prrWWW, pr: n, reason: "state_change", sender: "joseg-avviato",
			recipient: prrLogin, subject: fmt.Sprintf("Re: [treetopllc/itest-prr-www] %s (PR #%d)", title, n),
			body: body + footer})
	}
	nMerged := notice(9901, "Merge me", "Merged #9901 into main.")
	nClosed := notice(9902, "Close me", "Closed #9902.")
	nReopened := notice(9903, "Reopen me", "Reopened #9903.")
	// The control: an ordinary colleague comment on a closed review task, same
	// fixture, same empty list. It DOES resurface, so the notices' false above
	// is the disqualifier's doing, not the fixture's.
	comment := s.mail(t, ctx, ghMail{repo: prrWWW, pr: 9904, reason: "comment", sender: "ananthsekar007",
		recipient: prrLogin, subject: "Re: [treetopllc/itest-prr-www] Talk to me (PR #9904)",
		body: "one more thing after your review"})

	st := s.pass(t, ctx, "live")

	for _, c := range []struct {
		name string
		task int64
		msg  prrMsg
	}{{"merged", merged, nMerged}, {"closed", closed, nClosed}, {"reopened", reopened, nReopened}} {
		if d := s.must(t, ctx, c.msg, "live"); d.action != "task_log" || d.taskID == nil || *d.taskID != c.task {
			t.Errorf("%s notice on a closed review task: decision (%s, %v), want task_log onto %d", c.name, d.action, d.taskID, c.task)
		}
		got, reason := s.prrResurface(t, ctx, c.msg)
		if got {
			t.Errorf("%s notice on CLOSED review task %d wrote resurface=true. SWT-54 D4 (cross-ticket rule): a PR "+
				"state notice is logged and nothing else changes; it must never resurface through SWT-53. reason: %s",
				c.name, c.task, reason)
		}
		if !strings.Contains(reason, "PR state notice") {
			t.Errorf("%s notice: the decision reason %q does not name the PR-state-notice disqualifier", c.name, reason)
		}
		if s.status(t, ctx, c.task) != "closed" {
			t.Errorf("%s notice moved closed review task %d off closed", c.name, c.task)
		}
		if got := s.events(t, ctx, c.task, "log") - logs[c.task]; got != 1 {
			t.Errorf("%s notice: logs +%d on task %d, want +1 (it lands in the history)", c.name, got, c.task)
		}
	}

	if d := s.must(t, ctx, comment, "live"); d.action != "task_log" || d.taskID == nil || *d.taskID != control {
		t.Errorf("control comment: decision (%s, %v), want task_log onto %d", d.action, d.taskID, control)
	}
	if got, reason := s.prrResurface(t, ctx, comment); !got {
		t.Errorf("CONTROL: an ordinary colleague comment on a closed review task with an EMPTY notifier list wrote "+
			"resurface=false (reason %q); it must resurface, or this test cannot tell the notice disqualifier from "+
			"the fixture", reason)
	}
	if st.Resurfaced != 1 {
		t.Errorf("Resurfaced = %d, want 1 (the control comment only)", st.Resurfaced)
	}
}

// Ordinary PR mail on a CLOSED review task under the owner's GitHub seed: the
// sender is "{login} <notifications@github.com>", senderAddress equals the seed
// entry, so the comment is logged onto the closed task silently (no resurface,
// no reopen). This is the owner decision "keep GitHub notification mail silent"
// meeting SWT-54: a comment after the review task was closed is in the task's
// history and nowhere else.
func TestCapturePRReviewResurface_Integration_OrdinaryPRMailOnAClosedReviewTaskLogsSilentlyUnderTheGitHubSeed(t *testing.T) {
	ctx := context.Background()
	s := newPRRSuite(t, ctx, prrOpts{})

	s.colleague(t, ctx, prrWWW, 9911, "joseg-avviato", "Search facets", 30)
	s.pass(t, ctx, "live")
	task := s.mustRef(t, ctx, prrKey(prrWWW, 9911))
	s.closeReviewTask(t, ctx, task)
	s.seedGitHubNotifier(t, ctx)
	before := s.events(t, ctx, task, "log")

	comment := s.mail(t, ctx, ghMail{repo: prrWWW, pr: 9911, reason: "comment", sender: "ananthsekar007",
		recipient: prrLogin, subject: "Re: [treetopllc/itest-prr-www] Search facets (PR #9911)",
		body: "found another edge case after the merge window"})
	st := s.pass(t, ctx, "live")

	if d := s.must(t, ctx, comment, "live"); d.action != "task_log" || d.taskID == nil || *d.taskID != task {
		t.Errorf("comment on a closed review task: decision (%s, %v), want task_log onto %d", d.action, d.taskID, task)
	}
	got, reason := s.prrResurface(t, ctx, comment)
	if got {
		t.Errorf("a colleague's comment from notifications@github.com resurfaced under the GitHub seed; the seed "+
			"keeps GitHub notification mail silent (SWT-53 owner decision). reason: %s", reason)
	}
	if !strings.Contains(reason, "notifier list") {
		t.Errorf("the decision reason %q does not name the notifier list", reason)
	}
	if s.status(t, ctx, task) != "closed" {
		t.Errorf("the comment reopened closed review task %d; a plain-closed task is never reopened by activity", task)
	}
	if got := s.events(t, ctx, task, "log") - before; got != 1 {
		t.Errorf("logs +%d on the closed review task, want +1", got)
	}
	if st.Resurfaced != 0 || st.Reopened != 0 {
		t.Errorf("Resurfaced = %d, Reopened = %d; want 0 and 0", st.Resurfaced, st.Reopened)
	}
}

// An UNTRUSTED state notice falls through before any pr_review action (SWT-54
// D1 amendment), so it carries no notice flag into SWT-53. Checked where it can
// land: rule 6 (attributed; resurface is task_log-only by CHECK) or rule 10's
// closed bucket (a task_log, kept silent by the GitHub seed). Neither closes
// nor reopens the review task, and neither resurfaces.
func TestCapturePRReviewResurface_Integration_AnUntrustedNoticeClosesNothingAndStaysSilent(t *testing.T) {
	ctx := context.Background()
	s := newPRRSuite(t, ctx, prrOpts{})

	bucket := s.seedBucket(t, ctx)
	s.closeReviewTask(t, ctx, bucket)
	s.colleague(t, ctx, prrWWW, 9921, "joseg-avviato", "Billing export", 30)
	s.pass(t, ctx, "live")
	review := s.mustRef(t, ctx, prrKey(prrWWW, 9921))
	s.closeReviewTask(t, ctx, review)
	s.seedGitHubNotifier(t, ctx)

	forged := func(body string) prrMsg {
		return s.mail(t, ctx, ghMail{repo: prrWWW, pr: 9921, reason: "state_change", sender: "joseg-avviato",
			recipient: prrLogin, subject: "Re: [treetopllc/itest-prr-www] Billing export (PR #9921)",
			body: body, auth: prrNoAuth})
	}
	toRule6 := forged("Merged #9921 into main.")
	toBucket := forged("Merged #9921 into main.\nsee PRW-7 for the staging part")
	st := s.pass(t, ctx, "live")

	d6 := s.must(t, ctx, toRule6, "live")
	if d6.action == "task_log" && d6.taskID != nil && *d6.taskID == review {
		t.Errorf("an untrusted notice was logged onto the review task %d; untrusted mail falls through before any "+
			"pr_review action", review)
	}
	if !strings.Contains(d6.reason, "untrusted GitHub mail") {
		t.Errorf("untrusted notice (rule 6 path): reason %q does not say it fell through as untrusted", d6.reason)
	}
	if got, reason := s.prrResurface(t, ctx, toRule6); got {
		t.Errorf("untrusted notice (rule 6 path) resurfaced; reason %q", reason)
	}

	db := s.must(t, ctx, toBucket, "live")
	if db.action != "task_log" || db.taskID == nil || *db.taskID != bucket {
		t.Errorf("untrusted notice naming PRW-7: decision (%s, %v), want task_log onto closed bucket %d (rule 10)",
			db.action, db.taskID, bucket)
	}
	got, reason := s.prrResurface(t, ctx, toBucket)
	if got {
		t.Errorf("untrusted notice on the closed bucket resurfaced under the GitHub seed; reason %q", reason)
	}
	if !strings.Contains(reason, "notifier list") {
		t.Errorf("untrusted notice on the bucket: reason %q does not name the notifier list", reason)
	}

	for _, task := range []int64{review, bucket} {
		if got := s.status(t, ctx, task); got != "closed" {
			t.Errorf("task %d status = %q after untrusted notices, want closed (nothing closes, nothing reopens)", task, got)
		}
	}
	if st.PRClosed != 0 || st.Resurfaced != 0 || st.Reopened != 0 {
		t.Errorf("PRClosed = %d, Resurfaced = %d, Reopened = %d; want all 0", st.PRClosed, st.Resurfaced, st.Reopened)
	}
}
