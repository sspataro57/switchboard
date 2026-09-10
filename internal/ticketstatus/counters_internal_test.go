package ticketstatus

// The counter routing (SWT-34, docs/tickets/qa-delivered-drop_SPEC.md criterion
// 26): `count()` must route a close by its drop_reason with a SWITCH, so that a
// fourth reason is a visible gap rather than a silently miscounted
// `ticket_done`.
//
// An INTERNAL test (package ticketstatus, statusset_test.go's shape) because
// count is unexported and the routing is exactly the kind of one-line if/else
// that reads correct and counts wrong. Zero I/O.
//
// GREENFIELD NOTE — EXPECTED RED. Stats has no ClosedTicketDelivered field, so
// this file does not compile: `go vet ./internal/ticketstatus/` reports
// "unknown field ClosedTicketDelivered". Verified in the authoring session
// against a throwaway stub that added the field and left count()'s if/else
// alone: the delivered case then failed on its merits —
// `count(closed/ticket_delivered) incremented ClosedTicketDone` — which is the
// exact defect criterion 26 names ("today anything that is not not_assigned
// counts as ticket_done").
//
// ---- IMPOSED surface (store.go) ---------------------------------------------
//
//	type Stats struct {
//	    Considered, ClosedTicketDone, ClosedTicketDelivered, ClosedNotAssigned, Reopened int
//	    ...  // otherwise SWT-32's, unchanged
//	}
//
// The field name is fixed by criterion 25: it is printed as
// `closed_ticket_delivered` in BOTH counter lines, and
// delivered_structure_test.go derives the printed name FROM the field name, so
// the two cannot drift.

import (
	"reflect"
	"testing"
)

// qdChanged applies count() to a zero Stats and returns the names of the fields
// that moved, with their deltas. Reflection rather than a hand-written list of
// comparisons: the assertion "exactly ONE counter moved" is the whole point, and
// a hand-written list silently stops covering a field the day one is added.
func qdChanged(d Decision) map[string]int {
	var stats Stats
	count(&stats, d)

	moved := map[string]int{}
	v := reflect.ValueOf(stats)
	for i := 0; i < v.NumField(); i++ {
		if n := int(v.Field(i).Int()); n != 0 {
			moved[v.Type().Field(i).Name] = n
		}
	}
	return moved
}

func TestCount_RoutesEachDropReasonToItsOwnCounter(t *testing.T) {
	for _, tc := range []struct {
		reason string
		want   string
		why    string
	}{
		{
			reason: "ticket_done", want: "ClosedTicketDone",
			why: "SWT-32's, and the control: without it the two rows below would be scanning a " +
				"function that counts nothing at all",
		},
		{
			reason: "not_assigned", want: "ClosedNotAssigned",
			why: "SWT-32's other reason, which this ticket must not disturb",
		},
		{
			reason: "ticket_delivered", want: "ClosedTicketDelivered",
			why: "criterion 26: the existing `else` must no longer swallow it. A QA drop counted as " +
				"ticket_done makes the smoke's central check — closed_ticket_delivered equals the " +
				"TT-In QA open-task count, no more and no fewer — unfalsifiable",
		},
	} {
		tc := tc
		t.Run(tc.reason, func(t *testing.T) {
			moved := qdChanged(Decision{Action: "closed", DropReason: tc.reason, Act: true})
			if len(moved) != 1 || moved[tc.want] != 1 {
				t.Errorf("count(closed/%s) moved %v, want exactly {%s:1}. drop_reason and its counter "+
					"are one vocabulary: the reason recorded in ticket_status_syncs and the number "+
					"printed in the CronJob line have to be the same fact, or 'why did this leave the "+
					"board' has two answers — %s", tc.reason, moved, tc.want, tc.why)
			}
		})
	}
}

// The reason criterion 26 asks for a SWITCH and not a second `else if`: a value
// nobody has taught count() about must fall through to NOTHING, loudly zero,
// rather than be absorbed by whichever branch happens to be last.
//
// qa-question-resurface (E9) is the concrete fourth value already on the way —
// it needs its own last_action and, when it lands, its own counter. If the
// if/else stands, its closes are reported as ticket_done and the discrepancy is
// invisible in every log line.
func TestCount_AnUnknownDropReasonIsNotSwallowedByAnyCounter(t *testing.T) {
	moved := qdChanged(Decision{Action: "closed", DropReason: "a_reason_no_migration_allows", Act: true})
	if n := moved["ClosedTicketDone"]; n != 0 {
		t.Errorf("count(closed/an unknown reason) incremented ClosedTicketDone. Criterion 26: a "+
			"switch on the reason makes a fourth value a gap the next reader can SEE; an if/else "+
			"makes it a wrong number nobody can distinguish from a right one (moved=%v)", moved)
	}
	if len(moved) != 0 {
		t.Errorf("count(closed/an unknown reason) moved %v, want nothing. The CHECK in migration 0025 "+
			"is what stops such a row existing at all — this counter's job is to make the day it "+
			"happens visible, not to guess", moved)
	}
}

// The counters SWT-32 owns, unchanged — the control that proves the switch
// replaced only the close branch. A refusal is not a close, and neither is a
// convergent no-op.
func TestCount_TheNonCloseActionsAreUnchanged(t *testing.T) {
	for _, tc := range []struct {
		d    Decision
		want string
	}{
		{Decision{Action: "reopened", Act: true}, "Reopened"},
		{Decision{Action: "refused_active", DropReason: "ticket_delivered", Act: true}, "RefusedActive"},
		{Decision{Action: "suppressed_dismissed", Act: true}, "SuppressedDismissed"},
		{Decision{Action: "closed", DropReason: "ticket_delivered", Act: false}, "Converged"},
		{Decision{Action: "unreadable"}, "Unreadable"},
	} {
		tc := tc
		t.Run(tc.d.Action+"/act="+map[bool]string{true: "yes", false: "no"}[tc.d.Act], func(t *testing.T) {
			moved := qdChanged(tc.d)
			if len(moved) != 1 || moved[tc.want] != 1 {
				t.Errorf("count(%+v) moved %v, want exactly {%s:1}. A refusal whose CAUSE is the new "+
					"delivered fact is still a refusal, and a close this pass did not have to make is "+
					"still `converged` — the drop_reason names the cause, never the action",
					tc.d, moved, tc.want)
			}
		})
	}
}
