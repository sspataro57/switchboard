package promote

// chat-on-closed-task (SWT-53, docs/tickets/chat-on-closed-task_SPEC.md) CC6:
// promote changes only what it COPIES. The body gains one line, LAST and only
// when set; the promotion row's reason gains one part. InquiryGate, Decide,
// threadTask, inquiryCreateStatus and the actor are untouched (their pins
// stay in inquiry_internal_test.go and inquiry_test.go). ZERO I/O.
//
// ---- IMPOSED SURFACE ---------------------------------------------------------
//
//	type Verdict struct { ...; LoggedOnTaskID int64 }
//	  // latest.task_id when the message's latest capture decision is task_log
//	  // (a resurfaced message), 0 otherwise.
//	inquiryBody(v): when v.LoggedOnTaskID != 0, appends
//	  "logged_on_closed_task: N\n" as the LAST line; otherwise byte-identical.
//	decisionReason(v, ...): when v.LoggedOnTaskID != 0, one more part,
//	  "capture logged the message onto closed task N; resurfaced (chat-on-closed-task)".
//
// RED TODAY: Verdict has no LoggedOnTaskID, so the package's internal test
// build does not compile.

import (
	"strings"
	"testing"
	"time"
)

func rsVerdict() Verdict {
	sent := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	return Verdict{
		Lane: LaneInquiry, Kind: "question", MessageID: 901, ExtractionID: 77, ProjectID: 3,
		Asker: "asunda45", Sender: "asunda45", Channel: "slack", SentAt: &sent,
		StoredThreadID: 55, ThreadKey: "slack:T0:D0ASUNDA", ThreadScope: "conversation",
		Reason: "asks Salvador directly",
	}
}

func TestInquiryBody_LoggedOnClosedTaskIsTheLastLineAndOnlyWhenSet(t *testing.T) {
	v := rsVerdict()
	base := inquiryBody(v)
	if strings.Contains(base, "logged_on_closed_task") {
		t.Errorf("a verdict with LoggedOnTaskID 0 carries the logged_on_closed_task line; every existing body must "+
			"stay byte-identical (C8's exact-text tests):\n%s", base)
	}
	if !strings.HasSuffix(base, "verdict: asks Salvador directly\n") {
		t.Fatalf("CONTROL: the body's last line is no longer `verdict: …`:\n%s", base)
	}

	v.LoggedOnTaskID = 57
	got := inquiryBody(v)
	if want := base + "logged_on_closed_task: 57\n"; got != want {
		t.Errorf("inquiryBody with LoggedOnTaskID 57 =\n%s\nwant\n%s(CC6: the existing body, then ONE line, LAST)", got, want)
	}
	if bodyFor(v) != got {
		t.Errorf("bodyFor(inquiry verdict) does not return inquiryBody's text")
	}
}

func TestDecisionReason_NamesTheClosedTaskCaptureLoggedOnto(t *testing.T) {
	const part = "capture logged the message onto closed task 57; resurfaced (chat-on-closed-task)"
	create := Decision{Action: "review", Status: inquiryCreateStatus}

	v := rsVerdict()
	v.LoggedOnTaskID = 57
	if got := decisionReason(v, create, nil, nil); got != part {
		t.Errorf("decisionReason(resurfaced create) = %q, want %q (CC6's one part, alone)", got, part)
	}

	// Composed with Q3's fall-through past the thread's finished task.
	finished := &ExistingTask{ID: 88, Status: "closed", AssigneeType: "human"}
	got := decisionReason(v, create, nil, finished)
	if !strings.Contains(got, part) || !strings.Contains(got, "thread's task 88 is closed") {
		t.Errorf("decisionReason(resurfaced, Q3 fall-through) = %q; want both the resurface part and the Q3 part", got)
	}

	// Composed with an attach onto the Holding task a first resurfaced ask made (T5).
	open := &ExistingTask{ID: 91, Status: "holding", AssigneeType: "human"}
	if got := decisionReason(v, Decision{Action: "attached", TaskID: 91}, open, nil); !strings.Contains(got, part) {
		t.Errorf("decisionReason(resurfaced attach) = %q; the reason names the closed task on an attach too", got)
	}

	// Unset: unchanged.
	v.LoggedOnTaskID = 0
	if got := decisionReason(v, create, nil, nil); got != "" {
		t.Errorf("decisionReason(attributed create) = %q, want \"\" (unchanged)", got)
	}
}
