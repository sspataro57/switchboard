package promote_test

// receipts-become-tasks (SWT-68). 19 receipts, autopay notices and refunds
// became ready tasks because the personal lane's verdict called them
// payment_due + actionable. The fix is in the verdict (classify-v2's money
// clauses, pinned by internal/classify's prompt test and measured by the eval
// set, which now carries the 19 as `not`): an actionable=false verdict never
// enters the promoter's inbox.
//
// What Decide must KEEP doing is the other half: a real bill still becomes a
// ready task. The control is production's own — task 397, Citi, "Your payment
// due date is approaching" (extraction 12290), same kind as the 19.

import (
	"testing"

	"github.com/sspataro57/switchboard/internal/promote"
)

func TestDecide_ARealBillStillBecomesAReadyTask(t *testing.T) {
	got := promote.Decide(promote.Verdict{
		Kind: "payment_due", MessageID: 347452, ExtractionID: 12290,
		ProjectID: 6, ProjectSlug: "personal", StoredProjectID: 6,
		Title: "Payment due", Subject: "Your payment due date is approaching",
	}, nil)
	if got.Action != "task" || got.Status != "ready" {
		t.Fatalf("a payment_due verdict -> action=%q status=%q, want a ready task: tightening what counts as "+
			"payment_due must not stop a real bill (SWT-68's positive control)", got.Action, got.Status)
	}
}
