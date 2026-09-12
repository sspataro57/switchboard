package ticketstatus_test

// SWT-40 Part D, criterion D2 (docs/tickets/inquiry-promote_SPEC.md, D-D3):
// ONE spelling of "is this ticket warranted", shared by the reconciler and the
// capture-time gate. Decide's predicate is extracted into a pure exported
//
//	func Warranted(obs Observation) (warranted bool, dropReason string, readable bool)
//
// and Decide calls it. decide_test.go is deliberately NOT touched (the criterion
// is that it passes UNMODIFIED); this file pins the extracted function on its
// own table and then proves, over a grid, that it and Decide can never disagree
// — which is the whole point of extracting it: the gate must never create a task
// the reconciler would close 15 minutes later.
//
// Imports: testing and the package under test only, the decide_test.go
// discipline (no pgx, no net, no jira client).
//
// GREENFIELD NOTE — EXPECTED RED: ticketstatus.Warranted does not exist, so
// this file compile-FAILs the package's unit build.

import (
	"testing"

	"github.com/sspataro57/switchboard/internal/ticketstatus"
)

const (
	wOwn   = "acc-warranted-own"
	wOther = "acc-warranted-other"
)

func wObs(category, name, assignee string, gate bool) ticketstatus.Observation {
	return ticketstatus.Observation{
		TicketKey:         "LHH-1",
		StatusCategory:    category,
		StatusName:        name,
		StatusKnown:       true,
		Assignee:          assignee,
		AssigneeKnown:     true,
		OwnAccountID:      wOwn,
		GateOn:            gate,
		TaskStatus:        "ready",
		DeliveredStatuses: []string{"Ready for QA"},
	}
}

func TestWarranted_Table(t *testing.T) {
	cases := []struct {
		name         string
		obs          ticketstatus.Observation
		wantWarrant  bool
		wantDrop     string
		wantReadable bool
	}{
		{"gated, open, assigned to own", wObs("indeterminate", "In Progress", wOwn, true), true, "", true},
		{"gated, open, assigned elsewhere", wObs("indeterminate", "In Progress", wOther, true), false, "not_assigned", true},
		{"gated, open, unassigned (null)", wObs("new", "To Do", "", true), false, "not_assigned", true},
		{"done category, assigned to own", wObs("done", "Closed", wOwn, true), false, "ticket_done", true},
		{"delivered status name, assigned to own", wObs("indeterminate", "Ready for QA", wOwn, true), false, "ticket_delivered", true},
		// E7's ordered list: ticket_done > ticket_delivered > not_assigned.
		{"done beats delivered and not_assigned", wObs("done", "Ready for QA", wOther, true), false, "ticket_done", true},
		{"delivered beats not_assigned", wObs("indeterminate", "Ready for QA", wOther, true), false, "ticket_delivered", true},
		{"gate off: assignee is never consulted", wObs("indeterminate", "In Progress", wOther, false), true, "", true},
	}
	for _, c := range cases {
		w, drop, readable := ticketstatus.Warranted(c.obs)
		if readable != c.wantReadable || w != c.wantWarrant || drop != c.wantDrop {
			t.Errorf("%s: Warranted = (warranted %v, drop %q, readable %v), want (%v, %q, %v)",
				c.name, w, drop, readable, c.wantWarrant, c.wantDrop, c.wantReadable)
		}
	}
}

// Criterion 32's evidence gaps are UNREADABLE, never a verdict — and D11: with
// the gate off the assignee is not evidence, so its absence is not a gap.
func TestWarranted_EvidenceGapsAreUnreadable(t *testing.T) {
	noStatus := wObs("indeterminate", "In Progress", wOwn, true)
	noStatus.StatusKnown = false
	noAssignee := wObs("indeterminate", "In Progress", "", true)
	noAssignee.AssigneeKnown = false
	noOwn := wObs("indeterminate", "In Progress", wOwn, true)
	noOwn.OwnAccountID = ""
	gateOffNoAssignee := wObs("indeterminate", "In Progress", "", false)
	gateOffNoAssignee.AssigneeKnown = false
	gateOffNoOwn := wObs("indeterminate", "In Progress", wOwn, false)
	gateOffNoOwn.OwnAccountID = ""

	for _, c := range []struct {
		name         string
		obs          ticketstatus.Observation
		wantReadable bool
	}{
		{"status unknown", noStatus, false},
		{"gated, assignee key absent", noAssignee, false},
		{"gated, own account id unknown", noOwn, false},
		{"gate off, assignee key absent", gateOffNoAssignee, true},
		{"gate off, own account id unknown", gateOffNoOwn, true},
	} {
		w, drop, readable := ticketstatus.Warranted(c.obs)
		if readable != c.wantReadable {
			t.Errorf("%s: readable = %v, want %v", c.name, readable, c.wantReadable)
		}
		if !readable && (w || drop != "") {
			t.Errorf("%s: an UNREADABLE observation returned a verdict (warranted %v, drop %q); criterion 32: "+
				"an evidence gap is never a verdict in either direction", c.name, w, drop)
		}
	}
}

// The one-spelling proof: over every combination of the facts Decide reads,
// Warranted and Decide agree. A copy of the predicate that drifted — one more
// clause in either — turns this red on the first differing row.
func TestWarranted_AgreesWithDecideEverywhere(t *testing.T) {
	rows := 0
	for _, category := range []string{"new", "indeterminate", "done"} {
		for _, name := range []string{"In Progress", "Ready for QA", "ready for qa"} {
			for _, statusKnown := range []bool{true, false} {
				for _, assignee := range []string{wOwn, wOther, ""} {
					for _, assigneeKnown := range []bool{true, false} {
						for _, own := range []string{wOwn, ""} {
							for _, gate := range []bool{true, false} {
								obs := ticketstatus.Observation{
									TicketKey: "LHH-9", StatusCategory: category, StatusName: name,
									StatusKnown: statusKnown, Assignee: assignee, AssigneeKnown: assigneeKnown,
									OwnAccountID: own, GateOn: gate, TaskStatus: "ready",
									DeliveredStatuses: []string{"Ready for QA"},
								}
								rows++
								w, drop, readable := ticketstatus.Warranted(obs)
								d := ticketstatus.Decide(obs, nil)
								if (d.Action == "unreadable") != !readable {
									t.Errorf("%+v: Decide action %q but Warranted readable=%v", obs, d.Action, readable)
									continue
								}
								if !readable {
									continue
								}
								if d.Warranted != w {
									t.Errorf("%+v: Decide.Warranted=%v, Warranted()=%v — two spellings of one predicate",
										obs, d.Warranted, w)
								}
								if !w && d.DropReason != drop {
									t.Errorf("%+v: Decide.DropReason=%q, Warranted() drop=%q", obs, d.DropReason, drop)
								}
								if w && drop != "" {
									t.Errorf("%+v: warranted but drop reason %q", obs, drop)
								}
							}
						}
					}
				}
			}
		}
	}
	if rows < 400 {
		t.Fatalf("grid covered only %d rows; the equivalence proves nothing at that size", rows)
	}
}
