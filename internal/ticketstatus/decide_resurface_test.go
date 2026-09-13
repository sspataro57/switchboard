package ticketstatus_test

// THE purity proof for SWT-45's reconciler clause
// (docs/tickets/jira-activity-revive_SPEC.md J11, J12; criteria 29-32): a task
// that ACTIVITY (or a human) surfaced is held open against a ticket that no
// longer warrants it, until the ticket's facts change or a human closes it —
// and the whole thing is still a pure function of (observation, recorded state).
//
// Imports nothing but testing, time and the package under test, decide_test.go's
// discipline (structure_test.go's TestDecideTest_ImportsNothingThatCouldDoIO
// pins decide_test.go; this file holds itself to the same bar). Reuses obs(),
// tsOwn and tsOther from decide_test.go (same package).
//
// ---- IMPOSED SURFACE (criterion 29; the SPEC names every field) ---------------
//
//	type Observation struct { ...
//	    SurfacedAt          time.Time // tasks.surfaced_at, a VALUE (zero = NULL)
//	    SurfacedByMessageID int64     // tasks.surfaced_by_message_id (0 = NULL / a human)
//	}
//	type State struct { ...
//	    SurfacedSeen time.Time // ticket_status_syncs.surfaced_seen_at (zero = NULL)
//	}
//	type Decision struct { ...
//	    RecordSeen bool // the driver writes surfaced_seen_at := obs.SurfacedAt
//	}
//	Action gains "resurfaced" (migration 0030's sixth last_action).
//
//	newSurfacing = SurfacedAt set AND (state == nil OR !SurfacedAt.Equal(state.SurfacedSeen))
//	not warranted + restorable + newSurfacing                      -> resurfaced, Act
//	not warranted + restorable + last_action=resurfaced + same
//	    (status_category, status_name, assignee) as recorded        -> resurfaced, !Act
//	otherwise SWT-32/34 unchanged. RecordSeen: (warranted AND open) OR
//	(not warranted AND restorable). Active work records nothing.
//
// RED TODAY: none of the four names exist, so this file does not compile (and
// with it the package's test binary). Verified in the authoring session against
// a throwaway stub that added the fields and left Decide alone: every
// resurfaced row then failed on its merits (Action="closed" want "resurfaced"),
// the inert loop passed, and the simulator counted one Act per pass-with-a-
// surfaced-task instead of one in total.

import (
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/ticketstatus"
)

var (
	rsT0 = time.Date(2026, 9, 12, 14, 3, 7, 123456000, time.UTC) // a surfacing instant (µs, as Postgres stores it)
	rsT1 = rsT0.Add(90 * time.Minute)                            // a later surfacing
)

// rsSurfaced is S1's observation: the ticket is done, activity surfaced the
// task (it is open again), message 77 did it.
func rsSurfaced(taskStatus string) ticketstatus.Observation {
	o := obs("done", tsOwn, false)
	o.TaskStatus = taskStatus
	o.SurfacedAt = rsT0
	o.SurfacedByMessageID = 77
	return o
}

// rsBaseline is the state row a resurfaced Act records: the facts it held off,
// and the surfacing it consumed.
func rsBaseline(o ticketstatus.Observation, seen time.Time) *ticketstatus.State {
	return &ticketstatus.State{
		LastAction: "resurfaced", StatusCategory: o.StatusCategory, StatusName: o.StatusName,
		Assignee: o.Assignee, SurfacedSeen: seen,
	}
}

// ---- criterion 30: the decision-table rows ------------------------------------

func TestDecide_ANewSurfacingHoldsARestorableTaskOpen(t *testing.T) {
	for _, status := range []string{"holding", "ready", "blocked", "done_locally", "delivered"} {
		for _, st := range []struct {
			name  string
			state *ticketstatus.State
		}{
			{"never observed (S5: created by an overriding rule, same tick)", nil},
			{"this pass closed it, then activity revived it (S1)",
				&ticketstatus.State{LastAction: "closed", ClosedFromStatus: "ready", StatusCategory: "done", Assignee: tsOwn}},
			{"observed before, surfaced since", &ticketstatus.State{LastAction: "none", SurfacedSeen: rsT0.Add(-time.Hour)}},
			{"an earlier hold, then a NEWER surfacing", &ticketstatus.State{LastAction: "resurfaced", SurfacedSeen: rsT0.Add(-time.Hour),
				StatusCategory: "done", StatusName: "some workflow name", Assignee: tsOwn}},
		} {
			o := rsSurfaced(status)
			got := ticketstatus.Decide(o, st.state)
			if got.Action != "resurfaced" || !got.Act {
				t.Errorf("Decide(done ticket, task %s surfaced at %s, state=%s) = %+v, want Action=resurfaced Act=true. "+
					"J11: activity surfaced this task after the pass last saw it, so the pass logs ONE line and holds "+
					"it — closing it here is the bounce J12 proves absent (one flip per message, forever)",
					status, rsT0, st.name, got)
			}
			if got.DropReason != "ticket_done" {
				t.Errorf("Decide(... task %s, %s).DropReason = %q, want \"ticket_done\": a resurfaced row records the "+
					"drop fact it is HOLDING OFF (0030 supersedes 0023's 'NULL unless dropped')", status, st.name, got.DropReason)
			}
			if !got.RecordSeen {
				t.Errorf("Decide(... task %s, %s).RecordSeen = false: the surfacing must be CONSUMED, or the next "+
					"pass sees it as new again and logs a second line", status, st.name)
			}
		}
	}
}

func TestDecide_AHeldTaskWithUnchangedFactsConverges(t *testing.T) {
	o := rsSurfaced("ready")
	got := ticketstatus.Decide(o, rsBaseline(o, rsT0))
	if got.Action != "resurfaced" || got.Act {
		t.Errorf("Decide(held task, same surfacing, same facts) = %+v, want Action=resurfaced Act=false: the record "+
			"keeps saying resurfaced and NO executor call is made — 96 identical log lines a day otherwise", got)
	}
	if got.DropReason != "ticket_done" || !got.RecordSeen {
		t.Errorf("Decide(held, converged) = %+v, want DropReason=ticket_done RecordSeen=true", got)
	}

	// "Exact instant equality": the same instant in another Location is the same
	// surfacing. A struct == on time.Time compares the Location too and would
	// read every round trip through pgx (which hands back Local) as NEW.
	sameInstant := rsBaseline(o, rsT0.In(time.FixedZone("UTC+2", 2*3600)))
	if got := ticketstatus.Decide(o, sameInstant); got.Act {
		t.Errorf("Decide(held, SurfacedSeen = the same instant in another zone) = %+v, want Act=false. The "+
			"comparison is time.Equal, not ==", got)
	}
	// ...and one microsecond later is a new surfacing.
	later := o
	later.SurfacedAt = rsT0.Add(time.Microsecond)
	if got := ticketstatus.Decide(later, rsBaseline(o, rsT0)); got.Action != "resurfaced" || !got.Act {
		t.Errorf("Decide(held, SurfacedAt one µs after the recorded one) = %+v, want resurfaced Act=true — a new "+
			"revive is a new Jira event and earns one new log line", got)
	}
}

// "status_category, status_name or assignee changed -> closed, one row each."
// J11: a facts change is the ONLY thing that ends a hold from the reconciler's
// side, and each of the three must end it on its own.
func TestDecide_AFactsChangeEndsTheHold(t *testing.T) {
	t.Run("status_name (TT-Closed -> TT-Verified, category still done)", func(t *testing.T) {
		o := rsSurfaced("ready")
		state := rsBaseline(o, rsT0)
		state.StatusName = "a done-category name before"
		got := ticketstatus.Decide(o, state)
		if got.Action != "closed" || !got.Act || got.DropReason != "ticket_done" {
			t.Errorf("Decide(held, status NAME moved) = %+v, want closed Act=true ticket_done (S10)", got)
		}
	})
	t.Run("status_category (a delivered status -> done)", func(t *testing.T) {
		o := rsSurfaced("ready")
		o.StatusName = "QA-Handoff"
		o.DeliveredStatuses = []string{"QA-Handoff"}
		state := rsBaseline(o, rsT0)
		state.StatusCategory = "indeterminate" // held off as ticket_delivered; the ticket then moved to done
		got := ticketstatus.Decide(o, state)
		if got.Action != "closed" || !got.Act {
			t.Errorf("Decide(held, status CATEGORY moved) = %+v, want closed Act=true", got)
		}
	})
	t.Run("assignee (gate off; the ticket changed hands while done)", func(t *testing.T) {
		o := rsSurfaced("ready")
		o.Assignee = "acc-a-third-person"
		state := rsBaseline(o, rsT0)
		state.Assignee = tsOther
		got := ticketstatus.Decide(o, state)
		if got.Action != "closed" || !got.Act {
			t.Errorf("Decide(held, ASSIGNEE moved) = %+v, want closed Act=true", got)
		}
	})
}

func TestDecide_WarrantedAndOpenRecordsTheSurfacing(t *testing.T) {
	o := obs("indeterminate", tsOwn, true)
	o.TaskStatus = "ready"
	o.SurfacedAt = rsT0
	got := ticketstatus.Decide(o, nil)
	if got.Action != "none" || got.Act {
		t.Errorf("Decide(warranted, open, surfaced) = %+v, want Action=none Act=false — nothing to hold off", got)
	}
	if !got.RecordSeen {
		t.Errorf("Decide(warranted, open, surfaced).RecordSeen = false. J11: a surfacing observed while the ticket " +
			"was still warranted is CONSUMED, so a later fact change closes normally instead of being held by a " +
			"revive that happened while the ticket was live")
	}
}

// "a surfacing consumed while warranted, then not warranted -> closed."
func TestDecide_ASurfacingConsumedWhileWarrantedDoesNotHoldALaterDrop(t *testing.T) {
	o := obs("indeterminate", tsOwn, false)
	o.TaskStatus = "ready"
	o.SurfacedAt = rsT0
	d1 := ticketstatus.Decide(o, nil)
	if !d1.RecordSeen {
		t.Fatalf("setup: Decide(warranted, open, surfaced).RecordSeen = false; see the row above")
	}
	state := &ticketstatus.State{LastAction: d1.Action, StatusCategory: o.StatusCategory, StatusName: o.StatusName,
		Assignee: o.Assignee, SurfacedSeen: o.SurfacedAt}

	o.StatusCategory = "done"
	got := ticketstatus.Decide(o, state)
	if got.Action != "closed" || !got.Act {
		t.Errorf("Decide(ticket moved to done AFTER the surfacing was consumed) = %+v, want closed Act=true. The "+
			"hold is for activity the ticket's close has not answered; this activity predates the close", got)
	}
}

// "closed task + resurfaced state -> none, never a claimed close." A hand close
// (or a dismissal) of a held task STICKS: the pass never claims it and so never
// reopens it (SWT-32 criterion 23) — J10's "one click closes it for good".
func TestDecide_AHandClosedHeldTaskIsNeverClaimed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		surfaced  time.Time
		dismissed bool
	}{
		{"hand-closed, same surfacing", rsT0, false},
		{"hand-closed, and SurfacedAt moved (a closed task is never 'surfaced')", rsT1, false},
		{"dismissed while held", rsT0, true},
	} {
		o := rsSurfaced("closed")
		o.SurfacedAt = tc.surfaced
		o.Dismissed = tc.dismissed
		got := ticketstatus.Decide(o, rsBaseline(o, rsT0))
		if got.Action != "none" || got.Act {
			t.Errorf("Decide(done ticket, %s, state resurfaced) = %+v, want Action=none Act=false. Recording "+
				"'closed' here would claim a human's close and authorise a later REOPEN of it", tc.name, got)
		}
	}
}

func TestDecide_ActiveWorkIsUnchangedAndRecordsNothing(t *testing.T) {
	for _, status := range []string{"claimed", "in_progress", "needs_feedback"} {
		o := rsSurfaced(status)
		got := ticketstatus.Decide(o, nil)
		if got.Action != "refused_active" || !got.Act {
			t.Errorf("Decide(done ticket, surfaced, task %s) = %+v, want refused_active Act=true (SWT-32, unchanged)",
				status, got)
		}
		if got.RecordSeen {
			t.Errorf("Decide(done ticket, surfaced, task %s).RecordSeen = true. J11: active work records nothing — "+
				"surfaced_seen_at keeps its previous value (upsertState's COALESCE)", status)
		}
	}
}

func TestDecide_DismissalPathsAreUnchangedBySurfacing(t *testing.T) {
	o := obs("indeterminate", tsOwn, true)
	o.TaskStatus = "closed"
	o.Dismissed = true
	o.SurfacedAt = rsT0
	got := ticketstatus.Decide(o, &ticketstatus.State{LastAction: "closed", ClosedFromStatus: "ready"})
	if got.Action != "suppressed_dismissed" || !got.Act {
		t.Errorf("Decide(warranted, closed by this pass, dismissed, surfaced) = %+v, want suppressed_dismissed "+
			"Act=true — a dismissal still outranks the reconciler (SWT-32 D4)", got)
	}
}

// ---- criterion 31: inert by default -------------------------------------------

// legacyDecide is decide.go's Decide as it stood on main 26a6ae8 (SWT-32 +
// SWT-34), VERBATIM in logic. It is the oracle for criterion 31: "with
// SurfacedAt zero, every pre-existing decision is byte-identical". A copy is
// the only honest oracle — after the change there is no "old Decide" to call,
// and comparing Decide with itself proves nothing.
func legacyDecide(o ticketstatus.Observation, state *ticketstatus.State) ticketstatus.Decision {
	restorable := func(s string) bool {
		switch s {
		case "holding", "ready", "blocked", "done_locally", "delivered":
			return true
		}
		return false
	}
	active := func(s string) bool {
		switch s {
		case "claimed", "in_progress", "needs_feedback":
			return true
		}
		return false
	}
	if !o.StatusKnown || (o.GateOn && (!o.AssigneeKnown || o.OwnAccountID == "")) {
		return ticketstatus.Decision{Action: "unreadable"}
	}
	delivered := ticketstatus.IsDeliveredStatus(o.StatusName, o.DeliveredStatuses)
	warranted := o.StatusCategory != "done" && !delivered && (!o.GateOn || o.Assignee == o.OwnAccountID)
	drop := ""
	switch {
	case o.StatusCategory == "done":
		drop = "ticket_done"
	case delivered:
		drop = "ticket_delivered"
	case o.GateOn && o.Assignee != o.OwnAccountID:
		drop = "not_assigned"
	}
	if !warranted {
		switch {
		case restorable(o.TaskStatus):
			return ticketstatus.Decision{Warranted: false, Action: "closed", DropReason: drop, Act: true}
		case active(o.TaskStatus):
			act := state == nil || state.LastAction != "refused_active" ||
				state.StatusCategory != o.StatusCategory || state.Assignee != o.Assignee ||
				state.StatusName != o.StatusName
			return ticketstatus.Decision{Warranted: false, Action: "refused_active", DropReason: drop, Act: act}
		default:
			if state != nil && state.LastAction == "closed" {
				return ticketstatus.Decision{Warranted: false, Action: "closed", DropReason: drop, Act: false}
			}
			return ticketstatus.Decision{Warranted: false, Action: "none", DropReason: drop, Act: false}
		}
	}
	if o.TaskStatus != "closed" {
		return ticketstatus.Decision{Warranted: true, Action: "none"}
	}
	if state == nil || state.LastAction != "closed" {
		return ticketstatus.Decision{Warranted: true, Action: "none"}
	}
	if o.Dismissed {
		return ticketstatus.Decision{Warranted: true, Action: "suppressed_dismissed", Act: true}
	}
	restore := state.ClosedFromStatus
	if !restorable(restore) {
		restore = "ready"
	}
	return ticketstatus.Decision{Warranted: true, Action: "reopened", RestoreStatus: restore, Act: true}
}

// "with SurfacedAt zero, every pre-existing decide_test.go case yields a
// byte-identical Decision. This is asserted as a loop over the existing table,
// not left implicit." The pre-existing cases are functions, not one table, so
// the loop is over their whole INPUT SPACE instead: every fact combination the
// SWT-32 and SWT-34 tables vary, every task status in tasks' CHECK, every
// pre-0030 last_action (resurfaced cannot exist without a surfacing), with and
// without a stale SurfacedSeen. Five fields are compared — the SWT-32 Decision.
// RecordSeen is new and has no legacy value; the driver's COALESCE makes it
// inert when SurfacedAt is zero (it records zero only where the row already
// had nothing to preserve, and Decide never reads a zero SurfacedAt as new).
func TestDecide_InertByDefault_ByteIdenticalToTheLegacyTable(t *testing.T) {
	statuses := []string{"holding", "ready", "claimed", "in_progress", "needs_feedback", "pr_open",
		"awaiting_ci", "awaiting_merge", "done_locally", "delivered", "closed", "blocked"}
	type facts struct {
		category  string
		known     bool
		name      string
		delivered []string
	}
	factSet := []facts{
		{"done", true, "some workflow name", nil},
		{"indeterminate", true, "some workflow name", nil},
		{"new", true, "some workflow name", nil},
		{"indeterminate", true, "QA-Handoff", []string{"QA-Handoff"}},
		{"done", true, "QA-Handoff", []string{"QA-Handoff"}},
		{"indeterminate", true, "Elsewhere", []string{"QA-Handoff"}},
		{"", false, "", nil},
	}
	n := 0
	for _, f := range factSet {
		for _, assignee := range []string{tsOwn, tsOther, ""} {
			for _, assigneeKnown := range []bool{true, false} {
				for _, own := range []string{tsOwn, ""} {
					for _, gate := range []bool{false, true} {
						for _, status := range statuses {
							for _, dismissed := range []bool{false, true} {
								o := ticketstatus.Observation{
									TicketKey: "ITS-1", StatusCategory: f.category, StatusName: f.name, StatusKnown: f.known,
									Assignee: assignee, AssigneeKnown: assigneeKnown, OwnAccountID: own, GateOn: gate,
									TaskStatus: status, Dismissed: dismissed, DeliveredStatuses: f.delivered,
								}
								same := func(la string) *ticketstatus.State {
									return &ticketstatus.State{LastAction: la, StatusCategory: f.category,
										StatusName: f.name, Assignee: assignee}
								}
								states := []*ticketstatus.State{
									nil,
									{LastAction: "none"},
									{LastAction: "closed", ClosedFromStatus: "ready", StatusCategory: "done", Assignee: tsOwn},
									{LastAction: "closed", ClosedFromStatus: "delivered"},
									{LastAction: "closed", ClosedFromStatus: ""},
									{LastAction: "closed", ClosedFromStatus: "in_progress"},
									{LastAction: "reopened"},
									same("refused_active"),
									{LastAction: "refused_active", StatusCategory: f.category, StatusName: f.name, Assignee: "acc-moved"},
									{LastAction: "suppressed_dismissed"},
								}
								for i, st := range states {
									for _, staleSeen := range []bool{false, true} {
										var s *ticketstatus.State
										if st != nil {
											c := *st
											if staleSeen {
												c.SurfacedSeen = rsT0
											}
											s = &c
										} else if staleSeen {
											continue
										}
										n++
										want := legacyDecide(o, s)
										got := ticketstatus.Decide(o, s)
										if got.Warranted != want.Warranted || got.Action != want.Action ||
											got.DropReason != want.DropReason || got.RestoreStatus != want.RestoreStatus ||
											got.Act != want.Act {
											t.Fatalf("criterion 31 (inert by default): with SurfacedAt zero, Decide(%+v, state#%d %+v) = "+
												"{Warranted:%v Action:%s DropReason:%s RestoreStatus:%s Act:%v}, but SWT-32/34's Decide says "+
												"{Warranted:%v Action:%s DropReason:%s RestoreStatus:%s Act:%v}. Every project without a "+
												"reviving rule must behave byte-identically to today",
												o, i, s, got.Warranted, got.Action, got.DropReason, got.RestoreStatus, got.Act,
												want.Warranted, want.Action, want.DropReason, want.RestoreStatus, want.Act)
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	if n < 10000 {
		t.Fatalf("the inert loop compared only %d inputs; it is not covering the table", n)
	}
}

// ---- criterion 32: the no-loop property (J12) -------------------------------

// rsWorld is the part of the world the reconciler observes and acts on: the
// ticket's stored facts and the task row.
type rsWorld struct {
	category, name, assignee string
	taskStatus               string
	surfacedAt               time.Time
	surfacedBy               int64
}

// rsDriver applies Decide's writes exactly as store.go's driver does: the
// executor's effect on the task (close / reopen; a log changes nothing), and
// upsertState's rules for the state row — closed_from_status on an acting
// close, preserved on a convergent one; surfaced_seen_at := SurfacedAt iff
// RecordSeen, else preserved (the COALESCE); no row for unreadable.
type rsDriver struct {
	state *ticketstatus.State
	acts  []string
}

func (d *rsDriver) pass(w *rsWorld) {
	o := ticketstatus.Observation{
		TicketKey: "API-4103", StatusCategory: w.category, StatusName: w.name, StatusKnown: true,
		Assignee: w.assignee, AssigneeKnown: true, OwnAccountID: tsOwn, GateOn: false,
		TaskStatus: w.taskStatus, SurfacedAt: w.surfacedAt, SurfacedByMessageID: w.surfacedBy,
	}
	dec := ticketstatus.Decide(o, d.state)
	if dec.Action == "unreadable" {
		return
	}
	if dec.Act {
		d.acts = append(d.acts, dec.Action)
		switch dec.Action {
		case "closed":
			w.taskStatus = "closed"
		case "reopened":
			w.taskStatus = dec.RestoreStatus
		}
	}
	ns := &ticketstatus.State{LastAction: dec.Action, StatusCategory: o.StatusCategory, StatusName: o.StatusName,
		Assignee: o.Assignee}
	if dec.Action == "closed" {
		if dec.Act {
			ns.ClosedFromStatus = o.TaskStatus
		} else if d.state != nil {
			ns.ClosedFromStatus = d.state.ClosedFromStatus
		}
	}
	if dec.RecordSeen {
		ns.SurfacedSeen = o.SurfacedAt
	} else if d.state != nil {
		ns.SurfacedSeen = d.state.SurfacedSeen
	}
	d.state = ns
}

// rsS1 is the starting point of Salvador's own report (API-4103, task 85): the
// reconciler closed the task when the ticket went done, then an email about a
// comment arrived after the close and the revive put the task back, surfaced.
func rsS1() (*rsWorld, *rsDriver) {
	w := &rsWorld{category: "done", name: "a done-category name", assignee: tsOwn,
		taskStatus: "ready", surfacedAt: rsT0, surfacedBy: 1}
	d := &rsDriver{state: &ticketstatus.State{LastAction: "closed", ClosedFromStatus: "ready",
		StatusCategory: "done", StatusName: "a done-category name", Assignee: tsOwn}}
	return w, d
}

func TestNoLoop_FiftyPassesWithNoNewEventActOnce(t *testing.T) {
	w, d := rsS1()
	for pass := 1; pass <= 50; pass++ {
		d.pass(w)
	}
	if len(d.acts) != 1 || d.acts[0] != "resurfaced" {
		t.Errorf("50 passes over a fixed done ticket with a surfaced task and no new message acted %d times %v, "+
			"want exactly ONE (resurfaced). J12: with no new external event the system reaches a fixed point after "+
			"at most one pass — anything more is the ping-pong decision 1 told us to design out", len(d.acts), d.acts)
	}
	if w.taskStatus != "ready" {
		t.Errorf("after 50 passes the task is %q, want ready (held by activity)", w.taskStatus)
	}
}

func TestNoLoop_OneFactChangeAddsExactlyOneAct(t *testing.T) {
	w, d := rsS1()
	for pass := 1; pass <= 50; pass++ {
		if pass == 20 {
			w.name = "another done-category name" // S10: TT-Closed -> TT-Verified
		}
		d.pass(w)
	}
	want := []string{"resurfaced", "closed"}
	if len(d.acts) != 2 || d.acts[0] != want[0] || d.acts[1] != want[1] {
		t.Errorf("50 passes with ONE fact change at pass 20 acted %v, want exactly %v. J12: every re-close of a "+
			"surfaced task consumes a ticket fact change observed after the surfacing was recorded", d.acts, want)
	}
	if w.taskStatus != "closed" {
		t.Errorf("after the fact change the task is %q, want closed", w.taskStatus)
	}
}

func TestNoLoop_ARevivedAfterThatCloseAddsExactlyOneMore(t *testing.T) {
	w, d := rsS1()
	for pass := 1; pass <= 50; pass++ {
		if pass == 20 {
			w.name = "another done-category name"
		}
		if pass == 30 {
			// A new email ingested after that close: capture's revive restores the
			// task (closed_from_status ready) and stamps a NEW surfacing.
			w.taskStatus, w.surfacedAt, w.surfacedBy = "ready", rsT1, 2
		}
		d.pass(w)
	}
	want := []string{"resurfaced", "closed", "resurfaced"}
	if len(d.acts) != 3 || d.acts[0] != want[0] || d.acts[1] != want[1] || d.acts[2] != want[2] {
		t.Errorf("50 passes with a fact change at 20 and a revive at 30 acted %v, want exactly %v — one flip per "+
			"real Jira event, never per pass", d.acts, want)
	}
	if w.taskStatus != "ready" {
		t.Errorf("after the second revive the task is %q, want ready", w.taskStatus)
	}
}
