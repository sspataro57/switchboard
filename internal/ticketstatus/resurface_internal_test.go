package ticketstatus

// SWT-45 (docs/tickets/jira-activity-revive_SPEC.md) criteria 35 and 36, the
// two unexported driver helpers the new action touches: count() routes an
// acting `resurfaced` to its own counter and a converged one to Converged, and
// decisionReason() composes the ONE log line the hold writes. Zero I/O. Reuses
// qdChanged from counters_internal_test.go (same package).
//
// IMPOSED SURFACE:
//
//	type Stats struct { ...; Resurfaced int }   // printed as `resurfaced`
//	decisionReason(obs, Decision{Action: "resurfaced", ...}) names:
//	  the ticket key, the drop fact (drop_reason), the surfacing — "message N"
//	  or, for a human's plain reopen (SurfacedByMessageID 0), "by hand" — and
//	  how the hold ends ("until" ... a status/assignee change or a hand close).
//
// RED TODAY: Observation has no SurfacedByMessageID (this file does not
// compile); with a stub that adds it, count() moves nothing for a resurfaced
// Act (no Resurfaced field, and the switch has no case) and decisionReason
// returns "" — both verified in the authoring session.

import (
	"strings"
	"testing"
	"time"
)

func TestCount_RoutesResurfaced(t *testing.T) {
	if moved := qdChanged(Decision{Action: "resurfaced", DropReason: "ticket_done", Act: true}); len(moved) != 1 || moved["Resurfaced"] != 1 {
		t.Errorf("count(resurfaced, Act) moved %v, want exactly {Resurfaced:1}. Criterion 35: count()'s switch routes "+
			"an acting resurfaced to Stats.Resurfaced — Verification step 7 reads `resurfaced=1` then `resurfaced=0`", moved)
	}
	if moved := qdChanged(Decision{Action: "resurfaced", DropReason: "ticket_done", Act: false}); len(moved) != 1 || moved["Converged"] != 1 {
		t.Errorf("count(resurfaced, !Act) moved %v, want exactly {Converged:1} — a held task the pass did nothing to "+
			"is converged, like every other no-op", moved)
	}
}

func TestDecisionReason_ResurfacedNamesTheTicketTheFactTheSurfacingAndTheWayOut(t *testing.T) {
	obs := Observation{
		TicketKey: "API-4103", StatusCategory: "done", StatusName: "TT-Closed", StatusKnown: true,
		Assignee: "acc-katie", AssigneeKnown: true, TaskStatus: "ready",
		SurfacedAt: time.Now(), SurfacedByMessageID: 4242,
	}
	d := Decision{Action: "resurfaced", DropReason: "ticket_done", Act: true}

	reason := decisionReason(obs, d)
	if reason == "" {
		t.Fatalf("decisionReason(resurfaced) = \"\". Criterion 36: the hold writes ONE log line on the task, and " +
			"this is its text — an empty line is a task held open with no explanation on its own page")
	}
	for _, want := range []struct{ frag, what string }{
		{"ticketstatus:", "the prefix every reconciler line carries"},
		{"API-4103", "the ticket"},
		{"ticket_done", "the drop fact being held off"},
		{"4242", "the surfacing message id"},
		{"until", "how the hold ends (a status/assignee change, or a hand close)"},
	} {
		if !strings.Contains(reason, want.frag) {
			t.Errorf("decisionReason(resurfaced) = %q does not contain %s (%q)", reason, want.what, want.frag)
		}
	}

	obs.SurfacedByMessageID = 0 // J8: a human's plain task_reopen surfaces with a NULL message
	byHand := decisionReason(obs, d)
	if !strings.Contains(byHand, "by hand") {
		t.Errorf("decisionReason(resurfaced by a human reopen) = %q, want it to say \"by hand\" — criterion 36: the "+
			"surfacing is the message id, or 'reopened by hand'", byHand)
	}
	if strings.Contains(byHand, "message 0") {
		t.Errorf("decisionReason(resurfaced by hand) = %q names a message 0; there is no message", byHand)
	}
}
