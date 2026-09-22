//go:build integration

package capture_test

// SWT-72 follow-up (2026-09-22): a task a capture rule CREATES from a person's
// first message must land in INCOMING too — Salvador: "lyle's emails are not
// landing in incoming I see 2 today in mailspring". Both had been captured as
// tasks #487/#488 within minutes, then sat in QUEUE: only attaches marked
// activity. Now the create path marks the new task with the creating message,
// after provenance and before surfacing, through the same tool.
//
// Reuses crvSuite; craRequire0039 / craActivity live in
// rules_activity_integration_test.go.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_x?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run CaptureActivity_Integration_ACreatedTask ./internal/capture/
//
// MUTATION: drop the markRuleActivity call from the actionTask branch -> red.

import (
	"context"
	"testing"
)

func TestCaptureActivity_Integration_ACreatedTaskIsMarkedByItsMessage(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	craRequire0039(t, ctx, s.pool)

	// A first message about a NEW key on a body_regex rule: capture creates.
	s.mentionRule(t, ctx, false)
	now := s.dbNow(t, ctx)
	mail := s.mailMsg(t, ctx, "lyle@foundry.example", "inbound",
		"Fwd: All Acord forms", "CRV-8101 — the forms you asked for are attached", now, now)
	st := s.pass(t, ctx, "live")

	task, ok := s.taskOf(t, ctx, "CRV-8101")
	if !ok {
		t.Fatalf("CONTROL: the pass created no task for CRV-8101 (stats %+v)", st)
	}
	if st.TasksCreated != 1 {
		t.Errorf("RulesStats.TasksCreated = %d, want 1", st.TasksCreated)
	}
	at, by := craActivity(t, ctx, s, task)
	if at == nil || by == nil || *by != mail {
		t.Fatalf("a rule-CREATED task has activity_at=%v by=%v, want set / %d: the board puts it in INCOMING "+
			"only through these columns, and a first email from Lyle sat in QUEUE unseen", at, by, mail)
	}
	if st.Activity != 1 {
		t.Errorf("RulesStats.Activity = %d, want 1 (counted from marked:true on the create path too)", st.Activity)
	}
	if n := s.audits(t, ctx, task, "task_mark_activity", crvActor); n != 1 {
		t.Errorf("%d task_mark_activity audit rows as %s on the created task, want 1 (invariant 3)", n, crvActor)
	}

	// A replay of the pass (shadow, nothing live left to decide) marks nothing
	// twice: the same message is a no-op in the tool.
	st2 := s.pass(t, ctx, "live")
	at2, _ := craActivity(t, ctx, s, task)
	if st2.Activity != 0 || at2 == nil || !at2.Equal(*at) {
		t.Errorf("a second pass moved activity_at %v -> %v (Activity=%d); a replay never re-surfaces", at, at2, st2.Activity)
	}
}
