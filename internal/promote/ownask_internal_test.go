package promote

// activity-resurfaces (SWT-72, docs/tickets/activity-resurfaces_SPEC.md) D11
// and criteria 14 and 15, the halves that need no database: the body's
// `related_task` line and the promotion reason's new part. ZERO I/O.
// resurface_internal_test.go (SWT-53 CC6) is the template and the neighbour.
//
// ---- IMPOSED SURFACE ---------------------------------------------------------
//
//	type Decision struct { ...; RelatedTaskID int64 }
//	func bodyFor(v Verdict, relatedTaskID int64) string   // ONE new argument
//	func inquiryBody(v Verdict, relatedTaskID int64) string
//	  // the C-D9 fixed list gains ONE line at its END, after `verdict`:
//	  //   "related_task: N\n", or "related_task: (none)\n" — the contract's
//	  // own convention for an empty value. CC6's conditional
//	  // `logged_on_closed_task` line stays the LAST line of the whole body when
//	  // set (it is an appendix to the fixed list, not a member of it).
//	  // The id reaches it from Decision.RelatedTaskID, which `act` already holds
//	  // when it calls createVerdictTask -> bodyFor. The personal lane passes 0
//	  // and taskBody is byte-unchanged.
//	decisionReason(v, d, existing, finished): when d.RelatedTaskID != 0, one
//	  more part:
//	  "thread's open task N; created its own task (owner decision 2026-09-22:
//	   an ask is always its own task)"
//
// SIGNATURE NOTE (greenfield): the SPEC names Decision.RelatedTaskID and the
// body line but not the plumbing between them. The smallest honest spelling is
// ONE argument on bodyFor/inquiryBody, because `act` already holds the Decision
// when it calls createVerdictTask. Chosen here so the contract has one shape;
// the SPEC does not forbid another.
//
// RED TODAY: Decision has no RelatedTaskID, so the package's internal test
// build does not compile.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - drop related_task from the body -> BodyCarriesTheRelatedTaskKey.
//   - print the related id only when set -> the `(none)` case.
//   - decisionReason drops the part -> ReasonNamesTheThreadsOpenTask.

import (
	"strings"
	"testing"
)

// Criterion 15: EVERY inquiry body carries the key, `(none)` when there is no
// related task. The line is the last of the C-D9 fixed list.
func TestInquiryBody_CarriesTheRelatedTaskKey(t *testing.T) {
	v := rsVerdict()

	none := bodyFor(v, 0)
	if !strings.Contains(none, "\nrelated_task: (none)\n") {
		t.Errorf("an inquiry body with no related task does not carry `related_task: (none)`. Criterion 15: the "+
			"C-D9 contract is amended by exactly one line, and \"(none)\" is the contract's own convention for "+
			"an empty value:\n%s", none)
	}
	if !strings.HasSuffix(none, "related_task: (none)\n") {
		t.Errorf("`related_task` is not the LAST line of the fixed list (D11: one more line, LAST, after "+
			"`verdict`):\n%s", none)
	}
	if i, j := strings.Index(none, "verdict: "), strings.Index(none, "related_task: "); i < 0 || j < i {
		t.Errorf("`related_task` does not follow `verdict`:\n%s", none)
	}

	// With a related task, the SAME body with the id in place of (none): nothing
	// else about the contract moves.
	withID := bodyForRelated(t, v, 452)
	if want := strings.Replace(none, "related_task: (none)\n", "related_task: 452\n", 1); withID != want {
		t.Errorf("the body with related task 452 =\n%s\nwant\n%s(criterion 15: exactly one line differs)", withID, want)
	}

	// CC6's appendix stays the last line of the WHOLE body when set: SWT-53's
	// line is an appendix to the fixed list, not a member of it.
	v.LoggedOnTaskID = 57
	both := bodyForRelated(t, v, 452)
	if !strings.HasSuffix(both, "logged_on_closed_task: 57\n") {
		t.Errorf("with LoggedOnTaskID set the body no longer ends with SWT-53's line; CC6 says it is LAST and "+
			"only when set:\n%s", both)
	}
	if !strings.Contains(both, "related_task: 452\n") {
		t.Errorf("the related_task line vanished when LoggedOnTaskID was set:\n%s", both)
	}
}

// Criterion 14 / D11: classify_promotions.reason records the decision in
// Salvador's own words, so the readout says why a thread now holds two tasks.
func TestDecisionReason_NamesTheThreadsOpenTaskAndTheOwnerDecision(t *testing.T) {
	const part = "thread's open task 452; created its own task (owner decision 2026-09-22: an ask is always its own task)"
	v := rsVerdict()
	d := Decide(v, &ExistingTask{ID: 452, Status: "ready", AssigneeType: "human"})
	if d.RelatedTaskID != 452 {
		t.Fatalf("CONTROL: Decide gave %+v, want RelatedTaskID 452; the reason below cannot be produced", d)
	}
	got := decisionReason(v, d, &ExistingTask{ID: 452, Status: "ready", AssigneeType: "human"}, nil)
	if !strings.Contains(got, part) {
		t.Errorf("decisionReason(inquiry create with RelatedTaskID 452) = %q,\nwant a part %q (D11)", got, part)
	}
	// A plain create (no thread task) says nothing about a related task.
	plain := decisionReason(v, Decide(v, nil), nil, nil)
	if strings.Contains(plain, "created its own task (owner decision") {
		t.Errorf("decisionReason(plain inquiry create) = %q; the part belongs to the RelatedTaskID case only", plain)
	}
	// A PERSONAL attach keeps its own reason, byte-unchanged.
	pv := Verdict{Kind: "payment_due"}
	pd := Decide(pv, &ExistingTask{ID: 452, Status: "ready", AssigneeType: "human"})
	if r := decisionReason(pv, pd, &ExistingTask{ID: 452, Status: "ready"}, nil); strings.Contains(r, "owner decision") {
		t.Errorf("decisionReason(personal attach) = %q; D11 touches the inquiry lane only", r)
	}
}

// bodyForRelated renders the inquiry body for a verdict whose Decision carries
// RelatedTaskID.
func bodyForRelated(t *testing.T, v Verdict, related int64) string {
	t.Helper()
	return bodyFor(v, related)
}
