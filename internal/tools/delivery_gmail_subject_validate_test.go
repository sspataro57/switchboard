package tools_test

// SWT-61 boundary pins (bug gmail-reply-empty-subject-off-thread). ZERO network,
// ZERO Postgres: every assertion stops at the executor's VALIDATE stage, which
// runs before any handler dereferences the pool — the tools_unit_test.go /
// delivery_calendar_validate_test.go idiom with a nil *pgxpool.Pool
// (calValidateExecutor is defined there).
//
// Both are GREEN today and must STAY green: they are the two things the fix must
// NOT change, and neither is expressed anywhere else as a statement about
// SWT-61's scope.
//
//  1. UNTHREADED GMAIL DOES NOT EXIST, so the fix needs no exception for it. A
//     gmail draft without thread_id is already refused here ("From is resolved
//     from the thread"), and sendDelivery refuses a gmail row whose thread_id is
//     NULL. Every gmail delivery is a reply on an ingested thread, which is why
//     "Re: <the message we answer>" is a total rule. A future "new outbound
//     mail" capability would need an explicit subject of its own — do not
//     pre-carve an allowance for a case the code refuses.
//  2. CALENDAR'S SUBJECT RULE IS ALREADY CLOSED and stays where it is: the
//     validator requires it because it becomes the event summary. The gmail fill
//     is a handler-side resolution, not a new validator rule — moving calendar's
//     rule or copying gmail's into the validator would push the choice of
//     client-visible words back onto the caller, including the model.

import (
	"context"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
)

func TestRegression_SWT61_UnthreadedGmailIsStillRefused(t *testing.T) {
	ex := calValidateExecutor(t)
	_, err := ex.Execute(context.Background(), executor.Call{
		Tool: "draft_delivery", Actor: "dashboard:itest-swt61@example.com",
		Args: []byte(`{"task_id":1,"channel":"gmail","body":"placeholder reply"}`),
	})
	if err == nil {
		t.Fatal("draft_delivery accepted a gmail draft with no thread_id — SWT-61 leans on this: every gmail " +
			"delivery is a reply on an ingested thread, which is what makes \"Re: <the message we answer>\" total")
	}
	if !strings.Contains(err.Error(), "thread_id") {
		t.Errorf("refusal = %q, want it to name thread_id", err)
	}
}

func TestRegression_SWT61_CalendarStillRequiresItsOwnSubject(t *testing.T) {
	start, end := calBlock()
	ex := calValidateExecutor(t)
	_, err := ex.Execute(context.Background(), executor.Call{
		Tool: "draft_delivery", Actor: "dashboard:itest-swt61@example.com",
		Args: []byte(`{"task_id":1,"channel":"calendar","target_ref":"itest-swt61@example.com",` +
			`"body":"placeholder agenda","start":"` + start.Format("2006-01-02T15:04:05Z07:00") +
			`","end":"` + end.Format("2006-01-02T15:04:05Z07:00") + `"}`),
	})
	if err == nil {
		t.Fatal("draft_delivery accepted a calendar draft with no subject — it becomes the event summary " +
			"(delivery.go:151-153). SWT-61 must not move this rule")
	}
	if !strings.Contains(err.Error(), "subject") {
		t.Errorf("refusal = %q, want it to name subject", err)
	}
}
