//go:build integration

package capture_test

// swb 650 / SWT-88 (Salvador, 2026-09-25: "there a lyle comm I don't see it as
// task" / "those are supposed to be on incoming"): Lyle Deitch's replies on
// closed foundry tasks 500 and 465 were logged and resurfaced to an inquiry
// lane foundry never armed, so nothing read them. On a project WITHOUT an
// armed inquiry lane, a person's message that would resurface onto a closed
// task reopens it into INCOMING instead. The armed case is
// TestCaptureResurface_Integration_AHumanOnAClosedTaskResurfaces.

import (
	"context"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/capture"
)

func (s *rsfSuite) unarm(t *testing.T, ctx context.Context) {
	t.Helper()
	s.exec(t, ctx, `UPDATE projects SET ai_inquiry = false, inquiry_promote_after = NULL WHERE id=$1`, s.project)
}

// lyleThread is foundry rule 68's shape: a gmail sender rule keyed per thread
// (never a catch-all bucket, which SWT-53 K3 says must not reopen). It creates
// the thread's task with a first message and closes it.
func (s *rsfSuite) lyleThread(t *testing.T, ctx context.Context, key string) int64 {
	t.Helper()
	s.insID(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, priority, enabled, note)
		 VALUES ($1,'sender','Lyle Deitch','gmail',95,true,'itest-capcc rule68 shape') RETURNING id`, s.project)
	s.message(t, ctx, key+"-1", key, "gmail", "Lyle Deitch <lyle@x.test>", "forms attached", s.treetop)
	s.pass(t, ctx, capture.RulesModeLive)
	var task int64
	if err := s.pool.QueryRow(ctx, `SELECT task_id FROM external_refs WHERE system='gmail' AND external_key=$1`, key).
		Scan(&task); err != nil {
		t.Fatalf("setup: no task for the gmail thread: %v", err)
	}
	s.closeTask(t, ctx, task)
	return task
}

// MUTATIONS: drop the swb 650 branch, or load inquiryArmed as a literal true
// -> the task stays closed and resurface=true, red.
func TestSWB650_Integration_AReplyOnAClosedTaskReopensIntoIncoming(t *testing.T) {
	ctx := context.Background()
	s := newRSFSuite(t, ctx)
	s.unarm(t, ctx)
	key := "gmail:itest-capcc:<lyle-reopen@x>"
	b := s.lyleThread(t, ctx, key)

	m := s.message(t, ctx, "lyle-reply", key, "gmail", "Lyle Deitch <lyle@x.test>", "here are the forms", s.treetop)
	st := s.pass(t, ctx, capture.RulesModeLive)

	d, ok := s.decision(t, ctx, m, capture.RulesModeLive)
	if !ok || d.resurface || !strings.Contains(d.reason, "swb 650") {
		t.Fatalf("decision %+v: want resurface=false and a reason naming the reopen (swb 650)", d)
	}
	if got := s.status(t, ctx, b); got != "ready" {
		t.Fatalf("closed task %d is %q after a person's reply on an unarmed project, want ready", b, got)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM tasks WHERE id=$1 AND activity_by_message_id=$2`, b, m); n != 1 {
		t.Errorf("the reopened task is not marked with the reply, so it is not in INCOMING")
	}
	if st.Revived != 1 || st.Resurfaced != 0 {
		t.Errorf("RulesStats revived=%d resurfaced=%d, want 1 / 0", st.Revived, st.Resurfaced)
	}
}

// Every other resurfaces() clause still holds on an unarmed project.
func TestSWB650_Integration_ANotifierStillOnlyLogs(t *testing.T) {
	ctx := context.Background()
	s := newRSFSuite(t, ctx)
	s.unarm(t, ctx)
	s.exec(t, ctx, `UPDATE projects SET notifier_senders = '{Jira}' WHERE id=$1`, s.project)
	b := s.bucket(t, ctx, "CCW")
	s.closeTask(t, ctx, b)

	s.human(t, ctx, "jirabot", "Jira", "Katie mentioned you on CCW-10355")
	if st := s.pass(t, ctx, capture.RulesModeLive); st.Revived != 0 {
		t.Errorf("a notifier's message reopened the task (Revived=%d)", st.Revived)
	}
	if got := s.status(t, ctx, b); got != "closed" {
		t.Errorf("task is %q after a notifier's message, want closed", got)
	}
}

func TestSWB650_Integration_ShadowSaysWouldReviveAndCallsNothing(t *testing.T) {
	ctx := context.Background()
	s := newRSFSuite(t, ctx)
	s.unarm(t, ctx)
	key := "gmail:itest-capcc:<lyle-shadow@x>"
	b := s.lyleThread(t, ctx, key)

	m := s.message(t, ctx, "lyle-shadow-2", key, "gmail", "Lyle Deitch <lyle@x.test>", "one more", s.treetop)
	s.pass(t, ctx, capture.RulesModeShadow)
	d, _ := s.decision(t, ctx, m, capture.RulesModeShadow)
	if !strings.Contains(d.reason, "would revive") {
		t.Errorf("shadow reason %q, want it to say would revive", d.reason)
	}
	if got := s.status(t, ctx, b); got != "closed" {
		t.Errorf("shadow reopened the task (%q)", got)
	}
}

// SWT-45 J18's reason: a gated project's task never comes back around the
// assignee check. The rule is gmail-keyed (foundry rule 68's shape), because a
// jira-keyed match on a gated project is held before this branch is reached.
// MUTATION: drop `&& !winner.gateOn` -> the task is reopened, red.
func TestSWB650_Integration_AGatedProjectIsNotReopened(t *testing.T) {
	ctx := context.Background()
	s := newRSFSuite(t, ctx)
	s.unarm(t, ctx)
	key := "gmail:itest-capcc:<lyle-thread@x>"
	task := s.lyleThread(t, ctx, key)
	s.exec(t, ctx, `UPDATE projects SET ticket_assignee_gate = true WHERE id=$1`, s.project)

	s.message(t, ctx, "lyle-2", key, "gmail", "Lyle Deitch <lyle@x.test>", "one more thing", s.treetop)
	s.pass(t, ctx, capture.RulesModeLive)
	if got := s.status(t, ctx, task); got != "closed" {
		t.Errorf("a gated project's closed task is %q, want closed (J18)", got)
	}
}
