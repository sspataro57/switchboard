package ticketstatus_test

// The THIRD `warranted` clause, as a pure decision (SWT-34,
// docs/tickets/qa-delivered-drop_SPEC.md criteria 13-21): a ticket sitting in a
// status this project has CONFIGURED as "delivered" no longer warrants its
// task, and a ticket that LEAVES that set warrants it again.
//
// This file imports nothing but testing and the package under test, exactly as
// decide_test.go does — structure_test.go's TestDecideTest_ImportsNothingThat
// CouldDoIO scans that file, and TestDecideGo_IsPure scans decide.go; the new
// clause must not cost either of them their green (criterion 13).
//
// GREENFIELD NOTE — EXPECTED RED. Observation has no DeliveredStatuses field,
// State has no StatusName field, and ticketstatus.IsDeliveredStatus does not
// exist, so this package's test binary does not compile: `go vet
// ./internal/ticketstatus/` reports "unknown field DeliveredStatuses in struct
// literal of type ticketstatus.Observation" (and the same for StatusName). That
// IS the red state for a spec-first test. Verified in the authoring session
// against a throwaway stub that added both fields and left Decide's body
// untouched: every assertion below then fired on its own merits — 36-row table
// rows reporting `Warranted = true, want false` and `DropReason = "", want
// "ticket_delivered"`, the flap failing at step 1, E6's second-log case
// reporting `Act = false, want true` — rather than on the missing surface.
//
// ---- IMPOSED surface (decide.go), beyond decide_test.go's -------------------
//
// Two fields and one clause. Nothing else about Decide's signature moves: this
// is a CLAUSE in the existing predicate, not a second pass (E8), and everything
// on the return path — reopen, dismissal suppression, the active-work refusal,
// the state row, idempotence — is inherited unchanged.
//
//	type Observation struct {
//	    ...                        // unchanged, SWT-32's
//	    StatusName        string   // no longer diagnostic-only: a per-project
//	                               // CONFIGURED set may branch on it (criterion 31)
//	    DeliveredStatuses []string // projects.ticket_delivered_statuses, a VALUE
//	                               // supplied by the driver from the COLUMN
//	}
//
//	type State struct {
//	    ...                   // unchanged, SWT-32's
//	    StatusName string     // E6: the drop-triggering NAME joins the
//	                          // "unchanged observation" key, from the
//	                          // ticket_status_syncs.status_name column that
//	                          // already exists
//	}
//
//	// Decision.DropReason gains a third value: 'ticket_done' |
//	// 'ticket_delivered' | 'not_assigned', chosen by ONE ordered list (E7).
//
// WHY A VALUE AND NOT A LOOKUP. The set arrives as a []string the driver read
// from `projects` (criterion 22). If Decide could reach a pool for it, the
// 36-row table below would stop proving anything — that is invariant 7's whole
// point, and the reason the fold lives in a file importing only `strings`.

import (
	"testing"

	"github.com/sspataro57/switchboard/internal/ticketstatus"
)

// The three real Treetop names. They are the CONFIGURED strings the runbook
// seeds, and criterion 30 bans them from this package's non-test sources — a
// test file is the only place in the repo they may legitimately appear.
const (
	qdInQA     = "TT-In QA"            // seeded (E11 seeds this one ONLY)
	qdInReview = "TT-In Review"        // a SECOND delivered status, used for E6's move
	qdWIP      = "TT-Work In Progress" // indeterminate like TT-In QA, and the ball IS in his court
	qdReopened = "TT-Reopened"         // category `new`, the flap's return leg
	qdVerified = "TT-Verified"         // category `done` — the precedence pair
)

// qdArmed is the collaboratory shape after the runbook's arming UPDATE.
func qdArmed() []string { return []string{qdInQA} }

// qdObs is decide_test.go's obs() with the two new inputs named explicitly.
// Nothing here relies on a zero value: a fixture that leans on a default is a
// fixture nobody can read, and this ticket's whole risk is a set that is empty
// when the reader thinks it is armed.
func qdObs(category, statusName, assignee string, gateOn bool, delivered []string) ticketstatus.Observation {
	return ticketstatus.Observation{
		TicketKey:         "ITS-1",
		StatusCategory:    category,
		StatusName:        statusName,
		StatusKnown:       true,
		Assignee:          assignee,
		AssigneeKnown:     true,
		OwnAccountID:      tsOwn,
		GateOn:            gateOn,
		TaskStatus:        "ready",
		DeliveredStatuses: delivered,
	}
}

// ---- criteria 14, 15, 16: the predicate and the ordered reason --------------

// The decision table grows from 18 rows to 36: {done, indeterminate, new} ×
// {mine, other, unassigned} × {gate on, gate off} × {name in set, name not in
// set}.
//
// The predicate, in ONE place (criterion 14):
//
//	warranted = statusCategory != 'done' AND NOT deliveredStatus AND (gate off OR assignee == own)
//
// and the RECORDED reason by ONE ordered list (E7):
//
//	ticket_done > ticket_delivered > not_assigned
//
// PRECEDENCE IS PINNED BY THIS TABLE, NOT BY PRODUCTION. No project today has
// both a gate armed (reengine) and a delivered set (collaboratory), so the two
// NEW pairs — done+delivered, and delivered+not-assigned — cannot be exercised
// in the field at all. The SPEC says to say so rather than imply they are
// observed: this table and migration 0025's comment are the only places the
// order is stated, which is exactly why it is stated ONCE and asserted here for
// every row rather than spot-checked.
//
// The relation SWT-32 already records is unchanged by construction:
// ticket_delivered is inserted BETWEEN done and not_assigned, so no
// currently-recorded reason changes meaning.
func TestDecide_DeliveredStatus_ThirtySixRows(t *testing.T) {
	assignees := []struct{ name, id string }{
		{"mine", tsOwn},
		{"other", tsOther},
		{"unassigned", ""}, // D14: `"assignee": null` is a POSITIVE statement and counts as not-mine
	}
	names := []struct {
		label string
		value string
		inSet bool
	}{
		{"in-set", qdInQA, true},
		{"not-in-set", qdWIP, false},
	}

	rows := 0
	for _, category := range []string{"done", "indeterminate", "new"} {
		for _, a := range assignees {
			for _, gateOn := range []bool{false, true} {
				for _, n := range names {
					category, a, gateOn, n := category, a, gateOn, n
					label := category + "/" + a.name + "/gate=" +
						map[bool]string{true: "on", false: "off"}[gateOn] + "/" + n.label
					rows++
					t.Run(label, func(t *testing.T) {
						wantWarranted := category != "done" && !n.inSet &&
							(!gateOn || a.id == tsOwn)
						wantDrop := ""
						switch { // the ordered list, spelled once, in the order E7 fixes
						case category == "done":
							wantDrop = "ticket_done"
						case n.inSet:
							wantDrop = "ticket_delivered"
						case gateOn && a.id != tsOwn:
							wantDrop = "not_assigned"
						}

						got := ticketstatus.Decide(qdObs(category, n.value, a.id, gateOn, qdArmed()), nil)

						if got.Warranted != wantWarranted {
							t.Errorf("Decide(%s).Warranted = %v, want %v — criterion 14: warranted = "+
								"statusCategory != 'done' AND NOT deliveredStatus AND (gate off OR "+
								"assignee == own). The delivered clause answers a DIFFERENT question "+
								"from statusCategory ('is the ball in my court', not 'is this "+
								"finished'), which is why it cannot be a function of the category",
								label, got.Warranted, wantWarranted)
						}
						if got.DropReason != wantDrop {
							t.Errorf("Decide(%s).DropReason = %q, want %q — E7's ONE ordered list: "+
								"ticket_done > ticket_delivered > not_assigned. Two independent if "+
								"chains is how the recorded reason and the counter Salvador reads "+
								"drift apart", label, got.DropReason, wantDrop)
						}
					})
				}
			}
		}
	}
	if rows != 36 {
		t.Errorf("the decision table drove %d rows, want 36 (criterion 16: the cross-product of three "+
			"categories, three assignee shapes, two gate states and two membership states). A table "+
			"that shrank is a table that stopped covering a dimension", rows)
	}
}

// The other half of criterion 16, and the SPEC insists it be ASSERTED rather
// than left implicit: with an EMPTY delivered set every one of SWT-32's 18 rows
// is byte-identical to today.
//
// That is the "inert by default" proof — reengine, saka, foundry, town-ai,
// homelab and personal all have '{}' and must behave exactly as they did before
// this ticket. The expectation here is SWT-32's formula written out again, NOT
// the new one with a term dropped: a shared helper would pass even if both were
// wrong in the same way.
func TestDecide_AnEmptyDeliveredSetIsByteIdenticalToSWT32(t *testing.T) {
	assignees := []struct{ name, id string }{
		{"mine", tsOwn}, {"other", tsOther}, {"unassigned", ""},
	}
	for _, empty := range []struct {
		label string
		set   []string
	}{
		{"nil", nil},
		{"empty slice", []string{}},
		{"one empty entry", []string{""}},    // E4: an entry that folds to empty is IGNORED...
		{"one blank entry", []string{"   "}}, // ...and so must a whitespace-only one be
	} {
		empty := empty
		for _, category := range []string{"done", "indeterminate", "new"} {
			for _, a := range assignees {
				for _, gateOn := range []bool{false, true} {
					category, a, gateOn := category, a, gateOn
					label := empty.label + "/" + category + "/" + a.name + "/gate=" +
						map[bool]string{true: "on", false: "off"}[gateOn]
					t.Run(label, func(t *testing.T) {
						// SWT-32's predicate and precedence, restated verbatim.
						wantWarranted := category != "done" && (!gateOn || a.id == tsOwn)
						wantDrop := ""
						switch {
						case category == "done":
							wantDrop = "ticket_done"
						case gateOn && a.id != tsOwn:
							wantDrop = "not_assigned"
						}

						// The status NAME is a real one, and it is in NOBODY's
						// set: an unarmed project's tickets carry names too, and
						// the clause must be inert for them, not merely unused.
						got := ticketstatus.Decide(qdObs(category, qdInQA, a.id, gateOn, empty.set), nil)

						if got.Warranted != wantWarranted || got.DropReason != wantDrop {
							t.Errorf("Decide(%s) = {Warranted:%v DropReason:%q}, want {%v %q}. E1: '{}' "+
								"is TODAY'S BEHAVIOUR EXACTLY — the clause is inert until an operator "+
								"arms one project by hand. A delivered set that matched anything by "+
								"default would drop tasks on six projects nobody asked about, as a "+
								"deploy side effect", label, got.Warranted, got.DropReason,
								wantWarranted, wantDrop)
						}
					})
				}
			}
		}
	}
}

// Criterion 15's two NEW precedence pairs, driven explicitly rather than only as
// rows in the table above — because these two are the ones a reader will want to
// find by name when they ask "why does this say ticket_done".
//
// Pinned by decision, not by production: no project has both a gate and a
// delivered set today (E7).
func TestDecide_DropReasonPrecedenceIsOneOrderedList(t *testing.T) {
	t.Run("done beats delivered", func(t *testing.T) {
		// An admin may map a status NAMED like a QA column into the `done`
		// category. Jira's own structure wins: the strongest statement about a
		// ticket is that it is finished (D15 already made status precede
		// assignment, and this keeps that shape).
		o := qdObs("done", qdInQA, tsOwn, false, qdArmed())
		got := ticketstatus.Decide(o, nil)
		if got.Warranted {
			t.Fatalf("Decide(done AND in the delivered set) = %+v, want not warranted", got)
		}
		if got.DropReason != "ticket_done" {
			t.Errorf("Decide(done AND in the delivered set).DropReason = %q, want \"ticket_done\" — E7: "+
				"ticket_done keeps the top slot. A status mapped into `done` while NAMED like a QA "+
				"column is finished, and the report must say so", got.DropReason)
		}
	})

	t.Run("delivered beats not_assigned", func(t *testing.T) {
		// A QA ticket handed to a QA engineer is dropped BECAUSE he delivered
		// it; 'not_assigned' there would be an artifact of the handoff, and the
		// counter Salvador reads to judge whether a capture rule is too broad
		// would be wrong.
		o := qdObs("indeterminate", qdInQA, tsOther, true, qdArmed())
		got := ticketstatus.Decide(o, nil)
		if got.Warranted {
			t.Fatalf("Decide(delivered AND assigned away under an armed gate) = %+v, want not warranted", got)
		}
		if got.DropReason != "ticket_delivered" {
			t.Errorf("Decide(delivered AND assigned away under an armed gate).DropReason = %q, want "+
				"\"ticket_delivered\" — E7 inserts it directly under ticket_done and ABOVE "+
				"not_assigned: it is a statement about the ticket's own lifecycle, the same kind of "+
				"fact as the one above it, while assignment is orthogonal", got.DropReason)
		}
	})

	t.Run("SWT-32's done > not_assigned relation is unchanged", func(t *testing.T) {
		// The control that makes the two above meaningful: inserting a value
		// BETWEEN two existing ones must leave their relation byte-identical, or
		// a currently-recorded reason silently changes meaning.
		o := qdObs("done", qdWIP, tsOther, true, qdArmed())
		if got := ticketstatus.Decide(o, nil); got.DropReason != "ticket_done" {
			t.Errorf("Decide(done, assigned away, armed gate, name NOT in the set).DropReason = %q, "+
				"want \"ticket_done\" — this pair is SWT-32's and this ticket must not move it",
				got.DropReason)
		}
		o = qdObs("indeterminate", qdWIP, tsOther, true, qdArmed())
		if got := ticketstatus.Decide(o, nil); got.DropReason != "not_assigned" {
			t.Errorf("Decide(open, assigned away, armed gate, name NOT in the set).DropReason = %q, "+
				"want \"not_assigned\" — the third slot still exists; the new value did not swallow it",
				got.DropReason)
		}
	})
}

// The clause consults the fold, not a raw comparison — E2/E3 at the DECIDE
// level, which is where the SQL alternative would have been invisible.
func TestDecide_TheDeliveredClauseUsesTheNormalizedComparison(t *testing.T) {
	// Configured by hand with a trailing space and the wrong case; observed
	// exactly as Jira serialises it.
	o := qdObs("indeterminate", qdInQA, tsOwn, false, []string{"  tt-in qa "})
	got := ticketstatus.Decide(o, nil)
	if got.Warranted || got.DropReason != "ticket_delivered" {
		t.Errorf("Decide(name=%q, configured=[\"  tt-in qa \"]) = %+v, want not warranted with "+
			"DropReason=ticket_delivered. E2: the fold is applied to BOTH sides — a trailing space "+
			"pasted into a psql UPDATE must not silently disarm the project", qdInQA, got)
	}

	// And the negative control that stops the fold from becoming a substring
	// match: the thing E2 refused by name.
	o = qdObs("indeterminate", "TT-QA Blocked", tsOwn, false, qdArmed())
	if got := ticketstatus.Decide(o, nil); !got.Warranted {
		t.Errorf("Decide(name=\"TT-QA Blocked\", configured=[%q]) = %+v, want WARRANTED. E2: exact, "+
			"never substring — TT-QA Blocked and TT-Needs QA Rework mean the ball IS in his court, and "+
			"dropping them is the one failure this ticket must not create", qdInQA, got)
	}
}

// ---- criterion 21 / E5: an empty NAME is not an evidence gap ----------------

// Reaching Decide at all means StatusKnown is true, so the CATEGORY was
// readable; only the name is missing. Counting that `unreadable` would suppress
// the REOPEN direction too, stranding a task the pass had already closed — so
// "not a member" is the fail-safe reading, the same direction D12 chose.
func TestDecide_AnEmptyStatusNameIsNotAnEvidenceGap(t *testing.T) {
	o := qdObs("indeterminate", "", tsOwn, false, qdArmed())
	got := ticketstatus.Decide(o, nil)

	if got.Action == "unreadable" {
		t.Fatalf("Decide(readable category, EMPTY name, armed set) = %+v, want a normal verdict. E5: "+
			"unreadable would suppress the reopen direction as well, stranding a task this pass had "+
			"closed — and in practice Jira always sends a name alongside a category, so this is the "+
			"defensive reading, not an expected path", got)
	}
	if !got.Warranted || got.DropReason != "" {
		t.Errorf("Decide(readable category, EMPTY name, armed set) = %+v, want warranted with no drop "+
			"reason: an unreadable NAME is 'not a member', which keeps the task on the board", got)
	}

	// The reopen half of the same row, which is the reason E5 chose this
	// direction: a task this pass closed as delivered must still come back when
	// the name arrives empty.
	o.TaskStatus = "closed"
	back := ticketstatus.Decide(o, &ticketstatus.State{
		LastAction: "closed", ClosedFromStatus: "ready", StatusCategory: "indeterminate",
		StatusName: qdInQA, Assignee: tsOwn,
	})
	if back.Action != "reopened" {
		t.Errorf("Decide(closed-as-delivered task, name now EMPTY) = %+v, want Action=reopened. "+
			"Treating the empty name as a gap would leave the task closed forever with nothing in the "+
			"database explaining why", back)
	}
}

// ---- criterion 17: every SWT-32 behaviour, for the NEW drop cause ------------

// E8's claim, made checkable: the delivered clause is a TERM in the existing
// predicate, so close, refusal, the never-claim-a-close rule, reopen and the
// dismissal suppression are all inherited. Each is one sub-test, driven with the
// delivered fact as the cause instead of the done fact.
func TestDecide_TheDeliveredDropInheritsEverySWT32Behaviour(t *testing.T) {
	delivered := func(taskStatus string) ticketstatus.Observation {
		o := qdObs("indeterminate", qdInQA, tsOwn, false, qdArmed())
		o.TaskStatus = taskStatus
		return o
	}

	t.Run("open task closes, recording ticket_delivered", func(t *testing.T) {
		for _, status := range []string{"holding", "ready", "blocked", "done_locally", "delivered"} {
			got := ticketstatus.Decide(delivered(status), nil)
			if got.Action != "closed" || !got.Act || got.DropReason != "ticket_delivered" {
				t.Errorf("Decide(delivered ticket, task %s) = %+v, want Action=closed Act=true "+
					"DropReason=ticket_delivered — the five statuses task_close accepts as a SOURCE "+
					"are exactly the ones this pass may drop, whichever fact dropped it", status, got)
			}
		}
	})

	t.Run("active work is refused, not closed", func(t *testing.T) {
		for _, status := range []string{"claimed", "in_progress", "needs_feedback"} {
			got := ticketstatus.Decide(delivered(status), nil)
			if got.Action != "refused_active" || !got.Act || got.DropReason != "ticket_delivered" {
				t.Errorf("Decide(delivered ticket, task %s) = %+v, want Action=refused_active Act=true "+
					"DropReason=ticket_delivered: one log line, no status change. Closing here would "+
					"take the work away from a running worker mid-turn", status, got)
			}
		}
	})

	t.Run("never claims a close it did not make", func(t *testing.T) {
		got := ticketstatus.Decide(delivered("closed"), nil)
		if got.Action != "none" || got.Act {
			t.Errorf("Decide(delivered ticket, task already closed, no state row) = %+v, want "+
				"Action=none Act=false. Recording 'closed' for a close someone else made would "+
				"authorise this pass to REOPEN a human's dismissal the day the ticket leaves QA", got)
		}
	})

	t.Run("reopen when the ticket LEAVES the set", func(t *testing.T) {
		// Salvador's "unless reopened", and it comes free: the ticket moved to
		// TT-Reopened, warranted is true again, and Decide's existing branch
		// restores the status the task held (fact 4).
		o := qdObs("new", qdReopened, tsOwn, false, qdArmed())
		o.TaskStatus = "closed"
		got := ticketstatus.Decide(o, &ticketstatus.State{
			LastAction: "closed", ClosedFromStatus: "delivered", StatusCategory: "indeterminate",
			StatusName: qdInQA, Assignee: tsOwn,
		})
		if got.Action != "reopened" || !got.Act {
			t.Fatalf("Decide(ticket left the delivered set, task closed by this pass) = %+v, want "+
				"Action=reopened Act=true. E8: the return path is inherited — the reopen branch keys "+
				"on state.LastAction == \"closed\" alone and never asks WHICH fact closed it", got)
		}
		if got.RestoreStatus != "delivered" {
			t.Errorf("Decide(... reopen).RestoreStatus = %q, want \"delivered\" — D6 restores what the "+
				"task held rather than flattening it to ready", got.RestoreStatus)
		}
	})

	t.Run("a dismissed task never resurfaces", func(t *testing.T) {
		o := qdObs("indeterminate", qdWIP, tsOwn, false, qdArmed()) // out of the set again
		o.TaskStatus = "closed"
		o.Dismissed = true
		state := &ticketstatus.State{
			LastAction: "closed", ClosedFromStatus: "ready", StatusCategory: "indeterminate",
			StatusName: qdInQA, Assignee: tsOwn,
		}
		got := ticketstatus.Decide(o, state)
		if got.Action != "suppressed_dismissed" || !got.Act {
			t.Fatalf("Decide(left the delivered set, closed by this pass, DISMISSED) = %+v, want "+
				"Action=suppressed_dismissed Act=true. D4 outranks the reconciler whichever fact "+
				"closed the task", got)
		}
		after := ticketstatus.Decide(o, &ticketstatus.State{LastAction: "suppressed_dismissed"})
		if after.Act {
			t.Errorf("Decide(... suppression already recorded) = %+v, want Act=false — recorded once, "+
				"never repeated", after)
		}
	})
}

// ---- criterion 18: flapping through the NEW fact is symmetric ---------------

// TT-Work In Progress -> TT-In QA -> TT-Reopened -> TT-In QA over ONE state row,
// threaded the way the driver threads it: each step's State is what the previous
// step's Decision would have written. That threading IS the test — it is where a
// design that recorded the wrong thing on the convergent pass falls over.
//
// Every name here has the same statusCategory as its neighbour or a different
// one by accident, never by design: the point is that the NAME is doing the
// work.
func TestDecide_FlapsThroughTheDeliveredSet(t *testing.T) {
	set := qdArmed()

	// 1. mid-build: the task is live and nothing happens.
	step1 := qdObs("indeterminate", qdWIP, tsOwn, false, set)
	step1.TaskStatus = "delivered"
	if d := ticketstatus.Decide(step1, nil); d.Action != "none" || d.Act {
		t.Fatalf("step 1 (TT-Work In Progress) = %+v, want Action=none Act=false: an armed project's "+
			"OTHER tickets must be untouched, or arming collaboratory empties its whole board", d)
	}

	// 2. handed back: it drops, from `delivered`.
	step2 := qdObs("indeterminate", qdInQA, tsOwn, false, set)
	step2.TaskStatus = "delivered"
	d2 := ticketstatus.Decide(step2, nil)
	if d2.Action != "closed" || !d2.Act || d2.DropReason != "ticket_delivered" {
		t.Fatalf("step 2 (TT-In QA) = %+v, want Action=closed Act=true DropReason=ticket_delivered", d2)
	}
	state := &ticketstatus.State{
		LastAction: d2.Action, ClosedFromStatus: step2.TaskStatus,
		StatusCategory: step2.StatusCategory, StatusName: step2.StatusName, Assignee: step2.Assignee,
	}

	// 3. reopened by the client: it comes back, to the status it held.
	step3 := qdObs("new", qdReopened, tsOwn, false, set)
	step3.TaskStatus = "closed"
	d3 := ticketstatus.Decide(step3, state)
	if d3.Action != "reopened" || d3.RestoreStatus != "delivered" {
		t.Fatalf("step 3 (TT-Reopened) = %+v, want Action=reopened RestoreStatus=delivered — "+
			"Salvador's 'unless reopened', which E8 gets for free by making this a term in the same "+
			"predicate rather than a second pass", d3)
	}
	state = &ticketstatus.State{
		LastAction: d3.Action, StatusCategory: step3.StatusCategory,
		StatusName: step3.StatusName, Assignee: step3.Assignee,
	}

	// 4. and handed back again: the same drop, from the restored status.
	step4 := qdObs("indeterminate", qdInQA, tsOwn, false, set)
	step4.TaskStatus = "delivered"
	d4 := ticketstatus.Decide(step4, state)
	if d4.Action != "closed" || !d4.Act || d4.DropReason != "ticket_delivered" {
		t.Fatalf("step 4 (TT-In QA again) = %+v, want Action=closed Act=true "+
			"DropReason=ticket_delivered. A state machine that could only fire once would leave a "+
			"re-delivered ticket's task on the board permanently", d4)
	}
}

// ---- criterion 19: a convergent re-observation that only changes the reason --

// A task already closed with drop_reason='ticket_delivered' whose ticket then
// moves to TT-Verified (category `done`) must RE-RECORD 'closed' with the new
// reason and make ZERO executor calls. Two things are being pinned at once:
//   - the record keeps saying 'closed' (D3) — writing 'none' here erases the only
//     fact that authorises a later reopen, invisibly;
//   - the reason moves ticket_delivered -> ticket_done, because precedence is
//     recomputed every pass from the observation and never read back from the
//     state row.
//
// The state-row half (closed_from_status preserved, converged counted) is the
// driver's and is asserted in the integration suite.
func TestDecide_AConvergentReasonChangeMakesNoExecutorCall(t *testing.T) {
	o := qdObs("done", qdVerified, tsOwn, false, qdArmed())
	o.TaskStatus = "closed"
	got := ticketstatus.Decide(o, &ticketstatus.State{
		LastAction: "closed", ClosedFromStatus: "ready", StatusCategory: "indeterminate",
		StatusName: qdInQA, Assignee: tsOwn,
	})

	if got.Action != "closed" {
		t.Fatalf("Decide(task closed as delivered, ticket now TT-Verified/done) = %+v, want "+
			"Action=closed with Act=false: the record stands and no executor call is made", got)
	}
	if got.Act {
		t.Errorf("Decide(task closed as delivered, ticket now done).Act = true — criterion 19: zero " +
			"executor calls. This pass is a hitchhiker on a */15 CronJob; re-closing an already-closed " +
			"task every tick writes an audit row 96 times a day forever")
	}
	if got.DropReason != "ticket_done" {
		t.Errorf("Decide(task closed as delivered, ticket now done).DropReason = %q, want "+
			"\"ticket_done\" — the reason is recomputed from the observation on every pass, so the "+
			"state row converges on the strongest true statement about the ticket rather than "+
			"preserving the first one recorded", got.DropReason)
	}
}

// ---- criterion 20 / E6: the drop-triggering NAME joins the dedup key --------

// The defect this SPEC found while reading SWT-32: `loadCandidates` puts only
// status_category and assignee into `State`, and Decide compares those two to
// decide whether an observation is "unchanged". Once the drop-triggering fact is
// the NAME, a claimed task whose ticket moves between two configured delivered
// statuses — same category, same assignee — gets no second log line, and its
// existing log names a status the ticket has LEFT.
//
// Both directions, because the widened key must not become "log every pass":
// with an unchanged name there is still exactly nothing to say.
func TestDecide_RefusedActiveKeyIncludesTheStatusName(t *testing.T) {
	// A claimed task in an armed project whose set holds BOTH names.
	both := []string{qdInQA, qdInReview}
	o := qdObs("indeterminate", qdInQA, tsOwn, false, both)
	o.TaskStatus = "in_progress"

	moved := &ticketstatus.State{
		LastAction: "refused_active", StatusCategory: "indeterminate",
		StatusName: qdInReview, Assignee: tsOwn,
	}
	if got := ticketstatus.Decide(o, moved); !got.Act {
		t.Errorf("Decide(refusal whose STATUS NAME changed since the record: %q -> %q, same category, "+
			"same assignee) = %+v, want Act=true. E6: without the name in the key the task gets no "+
			"second log line and the line it HAS names a status the ticket has left — the log then "+
			"says the work was handed back for review when it is sitting in QA",
			qdInReview, qdInQA, got)
	}

	same := &ticketstatus.State{
		LastAction: "refused_active", StatusCategory: "indeterminate",
		StatusName: qdInQA, Assignee: tsOwn,
	}
	if got := ticketstatus.Decide(o, same); got.Act {
		t.Errorf("Decide(unchanged refusal, already recorded) = %+v, want Act=false. Widening the key "+
			"must not turn it into 'log every pass': this runs every 15 minutes and a task nobody has "+
			"touched would collect 96 identical lines a day", got)
	}

	// The latent case E6 also closes: a plain rename inside one category, which
	// was harmless before this ticket and is not after it.
	renamed := &ticketstatus.State{
		LastAction: "refused_active", StatusCategory: "indeterminate",
		StatusName: qdWIP, Assignee: tsOwn,
	}
	if got := ticketstatus.Decide(o, renamed); !got.Act {
		t.Errorf("Decide(refusal whose ticket moved %q -> %q) = %+v, want Act=true: the transition INTO "+
			"the delivered set is precisely the fact the holder needs to see, and it changes neither "+
			"the category nor the assignee", qdWIP, qdInQA, got)
	}

	// SWT-32's two key components still work — the control that proves the
	// widened key is a WIDENING and not a replacement.
	movedAssignee := &ticketstatus.State{
		LastAction: "refused_active", StatusCategory: "indeterminate",
		StatusName: qdInQA, Assignee: tsOther,
	}
	if got := ticketstatus.Decide(o, movedAssignee); !got.Act {
		t.Errorf("Decide(refusal whose ASSIGNEE changed) = %+v, want Act=true — SWT-32's criterion 24, "+
			"which this ticket must not cost", got)
	}
	movedCategory := &ticketstatus.State{
		LastAction: "refused_active", StatusCategory: "new",
		StatusName: qdInQA, Assignee: tsOwn,
	}
	if got := ticketstatus.Decide(o, movedCategory); !got.Act {
		t.Errorf("Decide(refusal whose CATEGORY changed) = %+v, want Act=true — SWT-32's other key "+
			"component, unchanged", got)
	}
}
