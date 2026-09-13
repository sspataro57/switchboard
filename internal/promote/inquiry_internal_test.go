package promote

// O7 (docs/tickets/inquiry-promote_SPEC.md, "Owner decisions", answered
// 2026-09-11: "A is ok") — promoted inquiry tasks start in HOLDING.
//
// THIS PIN IS DELIBERATE. inquiryCreateStatus is the inquiry lane's autonomy
// argument, and it is a Go constant rather than a column or an env var for
// SWT-30 D2's reason: a typo cannot widen it unreviewed.
//
// THE FLIP. After about two weeks, when `classify promote --outcomes` satisfies
// Salvador, new inquiry tasks go straight to `ready`. That flip is a
// DELIBERATE ONE-LINE CHANGE — `inquiryCreateStatus = "ready"` — and it MUST
// edit the assertion below in the SAME diff (plus inquiry_test.go's
// TestDecide_InquiryCreatesAHoldingReviewTask). A diff that changes the
// constant without this test is exactly what this test exists to stop.
//
// GREENFIELD NOTE — EXPECTED RED: inquiryCreateStatus, Verdict.Lane and
// LaneInquiry do not exist, so the package's internal test build compile-FAILS.

import "testing"

func TestInquiryCreateStatus_IsHoldingUnderO7(t *testing.T) {
	if inquiryCreateStatus != "holding" {
		t.Fatalf("inquiryCreateStatus = %q, want \"holding\" (O7: Holding first). Flipping it to \"ready\" is a "+
			"deliberate one-line change that edits THIS assertion in the same diff", inquiryCreateStatus)
	}
	d := Decide(Verdict{Lane: LaneInquiry, Kind: "question"}, nil)
	if d.Status != inquiryCreateStatus {
		t.Errorf("Decide(inquiry create).Status = %q, want inquiryCreateStatus (%q): the constant must be the ONE "+
			"place the create status is spelled", d.Status, inquiryCreateStatus)
	}
	// holding is the review lane (classify_promotions.action='review'); ready is
	// a live task ('task'). The action follows the status, never the other way.
	wantAction := map[string]string{"holding": "review", "ready": "task"}[inquiryCreateStatus]
	if d.Action != wantAction {
		t.Errorf("Decide(inquiry create).Action = %q, want %q for status %q", d.Action, wantAction, inquiryCreateStatus)
	}
}
