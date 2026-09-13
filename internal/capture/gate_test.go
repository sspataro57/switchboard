package capture

// SWT-40 Part D, criterion D3 (docs/tickets/inquiry-promote_SPEC.md, D-D3 and
// D-D5): capture.DecideGate is PURE — one table row per D-D3 outcome, including
// an expired unreadable hold, a done ticket and a delivered-status ticket. Zero
// I/O: no pool, no context, no clock (the hold's age arrives as a value).
//
// ---- IMPOSED surface (internal/capture/gate.go) --------------------------------
//
// The SPEC fixes the call `capture.DecideGate(obs, readable, existingRef)
// GateDecision`. Expiry needs the hold's age, which none of the three names
// carries, so the observation carries it:
//
//	const GateActor          = "capture:gate"
//	const GateMaxAge         = 72 * time.Hour
//	const GateMaxKeysPerPass = 50
//
//	type GateObservation struct {
//	    Ticket  ticketstatus.Observation // facts from the STORED snapshot; GateOn true
//	    HeldFor time.Duration            // now - the held MESSAGE's sent_at
//	}
//	type GateRef struct {
//	    TaskID      int64 // the task external_refs links the key to
//	    DismissalID int64 // its OPEN task_dismissals row (SWT-36), 0 = none
//	}
//	type GateDecision struct {
//	    Action      string // "held" | "task" | "task_log" | "attributed"
//	    Reason      string // "pending_lookup" (held); "not_assigned" | "ticket_done" |
//	                       // "ticket_delivered" | "gate_unverified_expired" (attributed)
//	    TaskID      int64  // task_log: the ref's task
//	    DismissalID int64  // task_log on a dismissed task: the guarded reopen's target
//	}
//	func DecideGate(obs GateObservation, readable bool, ref *GateRef) GateDecision
//
// `readable` is the DRIVER's half of "unreadable" (a snapshot exists, exactly one,
// and its key routed unambiguously); Warranted's own readable flag is the other
// half (status known, assignee known, own id known). Either false → unreadable.
//
// Expiry is strictly AFTER GateMaxAge ("≤ GateMaxAge old" is still in the inbox,
// D-D4). A READABLE hold past GateMaxAge decides on its facts: the expiry row is
// "STILL unreadable after GateMaxAge".
//
// GREENFIELD NOTE — EXPECTED RED: none of the above exists; this file
// compile-FAILs the capture unit build.

import (
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/ticketstatus"
)

const (
	gtOwn   = "acc-gate-own"
	gtOther = "acc-gate-other"
)

func gtObs(category, name, assignee string, age time.Duration) GateObservation {
	return GateObservation{
		Ticket: ticketstatus.Observation{
			TicketKey:         "LHH-4242",
			StatusCategory:    category,
			StatusName:        name,
			StatusKnown:       true,
			Assignee:          assignee,
			AssigneeKnown:     true,
			OwnAccountID:      gtOwn,
			GateOn:            true,
			DeliveredStatuses: []string{"Ready for QA"},
		},
		HeldFor: age,
	}
}

func TestGateConstants(t *testing.T) {
	if GateMaxAge != 72*time.Hour {
		t.Errorf("GateMaxAge = %v, want 72h (D-D5: after that a still-unreadable hold fails closed)", GateMaxAge)
	}
	if GateMaxKeysPerPass != 50 {
		t.Errorf("GateMaxKeysPerPass = %d, want 50 (D-D5)", GateMaxKeysPerPass)
	}
	if GateActor != "capture:gate" {
		t.Errorf("GateActor = %q, want capture:gate (D-D4: the executor actor, capture:{connector} shape)", GateActor)
	}
}

func TestDecideGate_Table(t *testing.T) {
	fresh := time.Hour
	expired := GateMaxAge + time.Minute
	justInside := GateMaxAge - time.Minute

	noStatus := gtObs("indeterminate", "In Progress", gtOwn, fresh)
	noStatus.Ticket.StatusKnown = false
	noOwn := gtObs("indeterminate", "In Progress", gtOwn, fresh)
	noOwn.Ticket.OwnAccountID = ""
	noAssigneeKey := gtObs("indeterminate", "In Progress", "", fresh)
	noAssigneeKey.Ticket.AssigneeKnown = false

	ref := &GateRef{TaskID: 77}
	dismissedRef := &GateRef{TaskID: 78, DismissalID: 901}

	cases := []struct {
		name          string
		obs           GateObservation
		readable      bool
		ref           *GateRef
		wantAction    string
		wantReason    string
		wantTask      int64
		wantDismissal int64
	}{
		// ---- unreadable → stay held, counted pending_lookup -------------------
		{"no snapshot / unrouted / ambiguous (driver says unreadable) — facts ignored",
			gtObs("indeterminate", "In Progress", gtOwn, fresh), false, nil, "held", "pending_lookup", 0, 0},
		{"snapshot without a status", noStatus, true, nil, "held", "pending_lookup", 0, 0},
		{"no own account id (D12)", noOwn, true, nil, "held", "pending_lookup", 0, 0},
		{"assignee key absent under the gate (D14)", noAssigneeKey, true, nil, "held", "pending_lookup", 0, 0},
		{"unreadable with a ref still holds (no log on evidence we lack)",
			gtObs("indeterminate", "In Progress", gtOwn, fresh), false, ref, "held", "pending_lookup", 0, 0},

		// ---- warranted ---------------------------------------------------------
		{"warranted, ref exists → log on the ref's task",
			gtObs("indeterminate", "In Progress", gtOwn, fresh), true, ref, "task_log", "", 77, 0},
		{"warranted, ref on a DISMISSED task → log + guarded reopen target",
			gtObs("indeterminate", "In Progress", gtOwn, fresh), true, dismissedRef, "task_log", "", 78, 901},
		{"warranted, no ref → create",
			gtObs("new", "To Do", gtOwn, fresh), true, nil, "task", "", 0, 0},

		// ---- not warranted → attributed, the reconciler's own drop reason -----
		{"assigned elsewhere", gtObs("indeterminate", "In Progress", gtOther, fresh), true, nil, "attributed", "not_assigned", 0, 0},
		{"unassigned (null)", gtObs("new", "To Do", "", fresh), true, nil, "attributed", "not_assigned", 0, 0},
		{"done ticket, assigned to own", gtObs("done", "Closed", gtOwn, fresh), true, nil, "attributed", "ticket_done", 0, 0},
		{"delivered-status ticket, assigned to own", gtObs("indeterminate", "Ready for QA", gtOwn, fresh), true, nil,
			"attributed", "ticket_delivered", 0, 0},
		{"done ticket with an existing ref → attributed, not a log",
			gtObs("done", "Closed", gtOwn, fresh), true, ref, "attributed", "ticket_done", 0, 0},
		{"not assigned with an existing ref → attributed, not a log",
			gtObs("indeterminate", "In Progress", gtOther, fresh), true, ref, "attributed", "not_assigned", 0, 0},

		// ---- GateMaxAge ----------------------------------------------------------
		{"still unreadable after GateMaxAge → fail closed",
			gtObs("indeterminate", "In Progress", gtOwn, expired), false, nil, "attributed", "gate_unverified_expired", 0, 0},
		{"still unreadable after GateMaxAge, ref exists → fail closed, no log",
			gtObs("indeterminate", "In Progress", gtOwn, expired), false, ref, "attributed", "gate_unverified_expired", 0, 0},
		{"unreadable just inside GateMaxAge → still held",
			gtObs("indeterminate", "In Progress", gtOwn, justInside), false, nil, "held", "pending_lookup", 0, 0},
		{"readable after GateMaxAge decides on its facts",
			gtObs("indeterminate", "In Progress", gtOwn, expired), true, nil, "task", "", 0, 0},
	}
	for _, c := range cases {
		got := DecideGate(c.obs, c.readable, c.ref)
		if got.Action != c.wantAction {
			t.Errorf("%s: action = %q, want %q (decision %+v)", c.name, got.Action, c.wantAction, got)
			continue
		}
		if c.wantReason != "" && got.Reason != c.wantReason {
			t.Errorf("%s: reason = %q, want %q", c.name, got.Reason, c.wantReason)
		}
		if got.TaskID != c.wantTask {
			t.Errorf("%s: task id = %d, want %d", c.name, got.TaskID, c.wantTask)
		}
		if got.DismissalID != c.wantDismissal {
			t.Errorf("%s: dismissal id = %d, want %d", c.name, got.DismissalID, c.wantDismissal)
		}
	}
}
