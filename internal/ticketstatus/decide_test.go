package ticketstatus_test

// THE purity proof for the ticket reconciler (SWT-32,
// docs/tickets/jira-status-sync_SPEC.md criteria 15 and 20-29): whether a ticket
// still warrants a task, and what to do about it, is a pure function of
// (observation, recorded state) with ZERO I/O.
//
// This file imports NOTHING but testing and the package under test — no pgx, no
// net, no internal/connector/jira — which is criterion 20's literal demand and
// the shape internal/promote/promote_test.go and internal/orchestrator/
// rules_test.go established. The ban is not left to review: structure_test.go
// PARSES this file's import block and decide.go's, so a future "just one lookup"
// cannot be added quietly.
//
// GREENFIELD NOTE — EXPECTED RED. internal/ticketstatus does not exist, so this
// directory holds only _test.go files and the package does not build: `go test
// ./...` reports "build constraints exclude all Go files in
// .../internal/ticketstatus" (and `go vet` reports "no non-test Go files").
// That IS the red state for a spec-first test. Verified in the authoring session
// against a throwaway stub declaring the surface below and returning zero
// values: every assertion here then fires on its own merits (Action="" want
// "closed", Warranted=false want true, and so on) rather than on the missing
// package.
//
// ---- IMPOSED surface (decide.go) ---------------------------------------------
//
// The SPEC fixes the CALL — `ticketstatus.Decide(obs Observation, state *State)
// Decision` (criterion 20) — and the vocabulary of the values; the field
// spellings below are this file's, chosen to be the smallest set that carries
// every input the SPEC's criteria name and nothing the driver could look up
// itself.
//
//	type Observation struct {
//	    TicketKey      string // diagnostic; the prose reasons name it
//	    StatusCategory string // jira.Facts — 'new' | 'indeterminate' | 'done'
//	    StatusName     string // was DIAGNOSTIC ONLY (D2); since SWT-34
//	                          // (2026-09-10) a per-project CONFIGURED set of
//	                          // names may branch on it — never code.
//	    StatusKnown    bool
//	    Assignee       string // fields.assignee.accountId ("" + known = unassigned)
//	    AssigneeKnown  bool
//	    OwnAccountID   string // the STORING account's sync_cursor->>'own_account_id'
//	    GateOn         bool   // projects.ticket_assignee_gate — from the COLUMN (criterion 33)
//	    TaskStatus     string // tasks.status as it stands right now
//	    Dismissed      bool   // a task_dismissals row exists for this task (D4)
//	}
//
//	// State is the ticket_status_syncs row for this external_ref, or nil when
//	// this pass has never observed the ref before.
//	type State struct {
//	    LastAction       string // the stored last_action
//	    ClosedFromStatus string // the status the task held when THIS pass closed it
//	    StatusCategory   string // the facts as last recorded — what makes
//	    Assignee         string // "the same unchanged observation" decidable (criterion 24)
//	}
//
//	type Decision struct {
//	    Warranted     bool
//	    Action        string // see below
//	    DropReason    string // 'ticket_done' | 'not_assigned'; "" unless dropped
//	    RestoreStatus string // the reopen target; "" otherwise
//	    Act           bool   // does the driver make an executor call?
//	}
//
// ACTION IS SPELLED AS THE DATABASE WILL STORE IT — `none | closed | reopened |
// refused_active | suppressed_dismissed`, migration 0023's CHECK on
// `last_action`, plus `unreadable` for criterion 32's evidence gaps (which is
// NOT a last_action: status_category is NOT NULL with a three-value CHECK, so a
// ref whose status could not be read cannot have a state row at all). The SPEC's
// prose says "close" and "reopen"; the past tense here is deliberate, for
// promote_test.go's recorded reason — the test asserts the values the database
// will see, and a constant that drifted from the CHECK would satisfy a test
// written against the constant.
//
// ACT IS ABOUT THE EXECUTOR, NOT ABOUT THE STATE ROW. Every decision except
// `unreadable` writes/updates the ticket_status_syncs row (invariant 7's "every
// no-op writes the state row, so 'why did nothing happen to this task' is
// answerable from the database"); Act=false means it does so without calling
// task_close / task_reopen / task_append_log. The pair is what makes criterion
// 24's "a second pass performs zero executor calls" and criterion 44's
// idempotence expressible as a PURE decision instead of a driver detail.
//
// The `closed`-with-Act=false case is the one worth reading twice: a ticket that
// is still done and a task this pass already closed must RE-RECORD 'closed', not
// fall back to 'none'. Writing 'none' there would erase the only fact that
// authorises a later reopen (D3), and the task would never come back — a bug
// that is invisible until the day someone reopens a ticket, weeks later.

import (
	"testing"

	"github.com/sspataro57/switchboard/internal/ticketstatus"
)

const (
	tsOwn   = "acc-own-5b1"        // the polling account's own accountId (D12)
	tsOther = "acc-someone-else-7" // a colleague's
)

// obs builds an observation with both facts KNOWN and a plain open task — the
// shape every case below varies one thing from.
func obs(category, assignee string, gateOn bool) ticketstatus.Observation {
	return ticketstatus.Observation{
		TicketKey:      "ITS-1",
		StatusCategory: category,
		StatusName:     "some workflow name",
		StatusKnown:    true,
		Assignee:       assignee,
		AssigneeKnown:  true,
		OwnAccountID:   tsOwn,
		GateOn:         gateOn,
		TaskStatus:     "ready",
	}
}

// ---- criterion 21: the warranted predicate, all 18 rows ----------------------

// "Unit table over the cross-product {done, indeterminate, new} × {mine, other,
// unassigned} × {gate on, gate off} — 18 rows, each naming the expected
// warranted and drop_reason."
//
// D15's predicate, in one place: warranted = statusCategory != 'done' AND (gate
// off OR assignee == own). Two facts, ONE predicate — two separate rules with
// two state machines would drift the moment a ticket is both closed and
// reassigned, which is the ordinary end of a finished ticket.
//
// The two properties the table exists to pin, beyond the arithmetic:
//   - with the gate OFF the assignee is irrelevant in all six rows (that is the
//     fail-closed default D11 chose: not gating leaves today's behaviour);
//   - with it ON, `done` yields drop_reason='ticket_done' even when the ticket
//     IS Salvador's — status precedence, so the report answers "why did this
//     leave the board" with the fact that came first.
func TestDecide_Warranted_EighteenRows(t *testing.T) {
	assignees := []struct{ name, id string }{
		{"mine", tsOwn},
		{"other", tsOther},
		{"unassigned", ""}, // D14: "assignee": null is a POSITIVE statement, and it counts as not-mine
	}
	for _, category := range []string{"done", "indeterminate", "new"} {
		for _, a := range assignees {
			for _, gateOn := range []bool{false, true} {
				category, a, gateOn := category, a, gateOn
				name := category + "/" + a.name + "/gate=" + map[bool]string{true: "on", false: "off"}[gateOn]
				t.Run(name, func(t *testing.T) {
					wantWarranted := category != "done" && (!gateOn || a.id == tsOwn)
					wantDrop := ""
					switch {
					case category == "done":
						wantDrop = "ticket_done"
					case gateOn && a.id != tsOwn:
						wantDrop = "not_assigned"
					}

					got := ticketstatus.Decide(obs(category, a.id, gateOn), nil)

					if got.Warranted != wantWarranted {
						t.Errorf("Decide(%s).Warranted = %v, want %v — D15: warranted = "+
							"statusCategory != 'done' AND (gate off OR assignee == own)",
							name, got.Warranted, wantWarranted)
					}
					if got.DropReason != wantDrop {
						t.Errorf("Decide(%s).DropReason = %q, want %q. drop_reason records WHICH fact "+
							"dropped it, for the report and for a later 'why did this leave the board' "+
							"query; migration 0023's CHECK allows only ticket_done and not_assigned, "+
							"and NULL unless the task was dropped", name, got.DropReason, wantDrop)
					}
				})
			}
		}
	}
}

// The name alone never drops a task in an UNARMED project — the behavioural
// half of criterion 8's scan. AMENDED 2026-09-10 (SWT-34): this test's
// observations carry no DeliveredStatuses, so the name is genuinely not
// consulted here; with a project armed, a configured name DOES decide, which
// is decide_delivered_test.go's table.
// A ticket NAMED "Closed" whose category is `indeterminate` is live work.
func TestDecide_IgnoresTheStatusName(t *testing.T) {
	o := obs("indeterminate", tsOwn, true)
	o.StatusName = "Closed"
	if got := ticketstatus.Decide(o, nil); !got.Warranted {
		t.Errorf("Decide(name=Closed, category=indeterminate) = %+v, want warranted. D2: the NAME is "+
			"per-project workflow configuration and nothing branches on it; a name list would pass "+
			"every fixture and then silently stop closing tasks the day a client renames a column", got)
	}
	o = obs("done", tsOwn, true)
	o.StatusName = "In Progress"
	if got := ticketstatus.Decide(o, nil); got.Warranted {
		t.Errorf("Decide(name=\"In Progress\", category=done) = %+v, want NOT warranted — the mirror "+
			"image, and the one where a name list keeps a finished ticket on the board forever", got)
	}
}

// ---- criterion 32: evidence gaps are `unreadable`, never a verdict -----------

// "unreadable — and therefore NO action in either direction — covers all
// evidence gaps: StatusKnown=false; gate ON with AssigneeKnown=false; gate ON
// with an empty own_account_id for the storing account (D12's fail-safe)."
//
// D12's fail-safe stated plainly: a task must not vanish because we could not
// identify ourselves. Every one of these could equally be read as "not assigned
// to me", and every such reading drops a real task off a real board with no
// explanation.
func TestDecide_EvidenceGapsAreUnreadable(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*ticketstatus.Observation)
	}{
		{"status unknown", func(o *ticketstatus.Observation) { o.StatusKnown = false; o.StatusCategory = "" }},
		{"gate on, assignee key absent", func(o *ticketstatus.Observation) { o.AssigneeKnown = false }},
		{"gate on, no own_account_id", func(o *ticketstatus.Observation) { o.OwnAccountID = "" }},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			o := obs("indeterminate", tsOther, true)
			tc.mut(&o)
			got := ticketstatus.Decide(o, nil)

			if got.Action != "unreadable" {
				t.Errorf("Decide(%s).Action = %q, want \"unreadable\" — criterion 32: an evidence gap "+
					"is counted and NOTHING happens to the task, in either direction. %q here means "+
					"a missing field was read as a fact", tc.name, got.Action, got.Action)
			}
			if got.Act {
				t.Errorf("Decide(%s).Act = true: an unreadable ref reaches the executor. D12's "+
					"fail-safe is that a task must not vanish because we could not identify "+
					"ourselves", tc.name)
			}
			if got.DropReason != "" {
				t.Errorf("Decide(%s).DropReason = %q, want empty — nothing dropped it; we could not "+
					"tell", tc.name, got.DropReason)
			}
		})
	}
}

// The gate-OFF mirror: with the gate off the assignee is not consulted, so a
// missing assignee key is NOT an evidence gap — there is no question to answer.
// Without this row the fail-safe above would quietly make every collaboratory
// ref unreadable the day an issue came back without an assignee key, and the
// status half would stop working for the reason the assignee half exists.
func TestDecide_GateOffMakesAMissingAssigneeIrrelevant(t *testing.T) {
	o := obs("done", "", false)
	o.AssigneeKnown = false
	got := ticketstatus.Decide(o, nil)
	if got.Action != "closed" || got.DropReason != "ticket_done" {
		t.Errorf("Decide(done, no assignee key, gate off) = %+v, want Action=closed "+
			"DropReason=ticket_done. With the gate off the assignee is never read, so its absence "+
			"cannot make the ticket unreadable — that is the whole meaning of the per-project "+
			"column (D11)", got)
	}
}

// ---- criteria 22-26: the close side ------------------------------------------

// "not warranted + task open (holding|ready|blocked|done_locally|delivered) ->
// close, recording closed_from_status = the task's current status and
// drop_reason."
func TestDecide_NotWarrantedClosesAnOpenTask(t *testing.T) {
	for _, status := range []string{"holding", "ready", "blocked", "done_locally", "delivered"} {
		status := status
		t.Run(status, func(t *testing.T) {
			o := obs("done", tsOwn, false)
			o.TaskStatus = status
			got := ticketstatus.Decide(o, nil)

			if got.Action != "closed" || !got.Act {
				t.Fatalf("Decide(done ticket, task %s) = %+v, want Action=closed Act=true — the five "+
					"statuses task_close accepts as a SOURCE are exactly the ones this pass may drop",
					status, got)
			}
			if got.DropReason != "ticket_done" {
				t.Errorf("Decide(done ticket, task %s).DropReason = %q, want \"ticket_done\"",
					status, got.DropReason)
			}
		})
	}
}

// "not warranted + task already closed + no state row -> none, and the state row
// is written with last_action='none'. The pass must never claim a close it did
// not make — this is what stops it reopening a human's dismissal later."
func TestDecide_NeverClaimsACloseItDidNotMake(t *testing.T) {
	o := obs("done", tsOwn, false)
	o.TaskStatus = "closed"
	got := ticketstatus.Decide(o, nil)

	if got.Action != "none" {
		t.Errorf("Decide(done ticket, task already closed, no state) = %+v, want Action=none. "+
			"Criterion 23: recording 'closed' for a close someone else made would authorise this "+
			"pass to REOPEN a human's dismissal (or an R8 delivery close) the day the ticket moves "+
			"out of Done", got)
	}
	if got.Act {
		t.Errorf("Decide(done ticket, task already closed, no state).Act = true — there is nothing " +
			"to do; task_close is idempotent but calling it writes an audit row every 15 minutes " +
			"forever (criterion 44)")
	}
}

// The converged case, and the trap inside it: a ticket that is STILL done and a
// task THIS pass closed must keep its 'closed' record. Falling back to 'none'
// here erases the only fact that authorises a later reopen (D3) — and the damage
// is invisible until someone reopens the ticket weeks later and the task never
// comes back.
func TestDecide_AStandingCloseStaysRecordedAsClosed(t *testing.T) {
	o := obs("done", tsOwn, false)
	o.TaskStatus = "closed"
	got := ticketstatus.Decide(o, &ticketstatus.State{
		LastAction: "closed", ClosedFromStatus: "ready", StatusCategory: "done", Assignee: tsOwn,
	})

	if got.Action != "closed" {
		t.Errorf("Decide(still-done ticket, task closed BY THIS PASS) = %+v, want Action=closed with "+
			"Act=false: the record stands and no executor call is made. Writing 'none' here would "+
			"make the reopen path unreachable after the first convergent pass", got)
	}
	if got.Act {
		t.Errorf("Decide(still-done ticket, task closed by this pass).Act = true — criterion 44: the "+
			"second run over an unchanged world performs ZERO executor calls, and the audit_events "+
			"count for ticketstatus:jira is unchanged. Got %+v", got)
	}
}

// "not warranted + task claimed|in_progress|needs_feedback -> refused_active:
// exactly ONE task_append_log naming the ticket, its status and its assignee, no
// status change, state recorded."
//
// Fact 11: task_close refuses these three ("never close work out from under a
// holder"), so the pass must handle the refusal rather than treat it as an
// error — a pass that let the refusal become an error would abort the whole run
// on one busy task.
func TestDecide_ActiveWorkIsRefusedNotClosed(t *testing.T) {
	for _, status := range []string{"claimed", "in_progress", "needs_feedback"} {
		status := status
		t.Run(status, func(t *testing.T) {
			o := obs("done", tsOwn, false)
			o.TaskStatus = status
			got := ticketstatus.Decide(o, nil)

			if got.Action != "refused_active" || !got.Act {
				t.Fatalf("Decide(done ticket, task %s) = %+v, want Action=refused_active Act=true: one "+
					"log line, no status change. Closing here would take the work away from a running "+
					"worker mid-turn", status, got)
			}
			if got.DropReason != "ticket_done" {
				t.Errorf("Decide(done ticket, task %s).DropReason = %q, want \"ticket_done\" — the "+
					"refusal still records WHY the ticket no longer warrants the task; that is what "+
					"the log line has to say", status, got.DropReason)
			}
		})
	}
}

// "A second pass over the same unchanged observation performs zero executor
// calls (no log spam every 15 minutes)."
//
// The dedup key is the recorded observation, not merely the recorded action: a
// ticket that moved from one colleague to another is NEW information and the log
// line names the assignee, so it says something a reader has not seen.
//
// AMENDED 2026-09-10 (SWT-34 E6): the key now also carries StatusName, because
// once a per-project CONFIGURED name can trigger the drop, a claimed task whose
// ticket moves between two delivered statuses of the same category and assignee
// is a changed observation that would otherwise get no second log line. These
// fixtures therefore name StatusName explicitly rather than leaning on the zero
// value — a State whose name is "" against an observation whose name is not is a
// CHANGED observation, which is correct behaviour and would make this test read
// as broken.
func TestDecide_RefusedActiveIsLoggedOncePerObservation(t *testing.T) {
	o := obs("indeterminate", tsOther, true) // not mine, gate armed
	o.TaskStatus = "in_progress"

	same := &ticketstatus.State{
		LastAction: "refused_active", StatusCategory: "indeterminate", Assignee: tsOther,
		StatusName: o.StatusName,
	}
	if got := ticketstatus.Decide(o, same); got.Act {
		t.Errorf("Decide(unchanged refusal, already recorded) = %+v, want Act=false. Criterion 24: a "+
			"second pass over the same unchanged observation performs ZERO executor calls — this pass "+
			"runs every 15 minutes and a task nobody has touched would otherwise collect 96 identical "+
			"log lines a day", got)
	}
	if got := ticketstatus.Decide(o, same); got.Action != "refused_active" {
		t.Errorf("Decide(unchanged refusal, already recorded).Action = %q, want \"refused_active\" — "+
			"the state row keeps saying what it said; only the executor call is suppressed",
			got.Action)
	}

	moved := &ticketstatus.State{
		LastAction: "refused_active", StatusCategory: "indeterminate", Assignee: "acc-a-third-person",
		StatusName: o.StatusName,
	}
	if got := ticketstatus.Decide(o, moved); !got.Act {
		t.Errorf("Decide(refusal whose ASSIGNEE changed since the record) = %+v, want Act=true. The "+
			"log line names the assignee, so a re-assignment is a fact the task's log has not yet "+
			"recorded — 'the same unchanged observation' is about the observation, not about the "+
			"verdict", got)
	}
}

// "warranted + task not closed -> none." The overwhelmingly common row: 29
// collaboratory tasks whose tickets are ordinary open work.
func TestDecide_WarrantedAndOpenIsANoOp(t *testing.T) {
	for _, status := range []string{"ready", "in_progress", "delivered", "blocked"} {
		status := status
		t.Run(status, func(t *testing.T) {
			o := obs("indeterminate", tsOwn, true)
			o.TaskStatus = status
			got := ticketstatus.Decide(o, nil)
			if got.Action != "none" || got.Act {
				t.Errorf("Decide(open ticket, task %s) = %+v, want Action=none Act=false. Every other "+
					"task is untouched — that sentence is the SPEC's 'usable alone' promise", status, got)
			}
		})
	}
}

// ---- criteria 26-28: the return path, and the human who outranks it ----------

// "warranted + task closed + state.last_action != 'closed' -> none. Integration
// case: a task closed by task_dismiss before this pass ever ran is never
// reopened."
//
// Both no-state and a state recording something else, because they are different
// failure modes: the first is "we have never looked", the second is "we looked
// and did not close it".
func TestDecide_ReopensOnlyWhatItRecordedClosing(t *testing.T) {
	o := obs("indeterminate", tsOwn, true)
	o.TaskStatus = "closed"

	for _, tc := range []struct {
		name  string
		state *ticketstatus.State
	}{
		{"never observed", nil},
		{"observed but never acted", &ticketstatus.State{LastAction: "none"}},
		{"we only refused it", &ticketstatus.State{LastAction: "refused_active"}},
		{"the dismissal suppression already fired", &ticketstatus.State{LastAction: "suppressed_dismissed"}},
		{"already reopened once, then closed by someone else", &ticketstatus.State{LastAction: "reopened"}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := ticketstatus.Decide(o, tc.state)
			if got.Action != "none" || got.Act {
				t.Errorf("Decide(warranted, task closed, state=%s) = %+v, want Action=none Act=false. "+
					"D3: coming back must be conditioned on 'THIS PASS closed it' — without that "+
					"precondition the pass resurrects every closed task whose ticket happens to be "+
					"open, including the ones a human dismissed and the ones R8 closed after "+
					"delivery", tc.name, got)
			}
		})
	}
}

// "warranted + task closed + state.last_action == 'closed' + no task_dismissals
// row -> reopen to closed_from_status, falling back to ready when it is absent
// or outside the allowed set."
//
// D6: restoring the status the task actually held is more honest than flattening
// a `delivered` task to `ready`, and the allowed set is exactly the set
// task_close accepts as a source — spelled ONCE in internal/tools/close.go and
// shared by both verbs (criterion 37).
func TestDecide_ReopenRestoresTheRecordedStatus(t *testing.T) {
	o := obs("indeterminate", tsOwn, true)
	o.TaskStatus = "closed"

	for _, tc := range []struct{ stored, want string }{
		{"ready", "ready"},
		{"holding", "holding"},
		{"blocked", "blocked"},
		{"done_locally", "done_locally"},
		{"delivered", "delivered"},
		{"", "ready"},            // never recorded
		{"in_progress", "ready"}, // outside the allowed set: a claim is not restorable
		{"nonsense", "ready"},    // a value no CHECK would accept
	} {
		tc := tc
		t.Run("from="+tc.stored, func(t *testing.T) {
			got := ticketstatus.Decide(o, &ticketstatus.State{
				LastAction: "closed", ClosedFromStatus: tc.stored, StatusCategory: "done", Assignee: tsOwn,
			})
			if got.Action != "reopened" || !got.Act {
				t.Fatalf("Decide(warranted, task closed by this pass, from=%q) = %+v, want "+
					"Action=reopened Act=true", tc.stored, got)
			}
			if got.RestoreStatus != tc.want {
				t.Errorf("Decide(... from=%q).RestoreStatus = %q, want %q. D6: restore what the task "+
					"held, and fall back to `ready` when the recorded value is absent or outside the "+
					"allowed set — never pass a status the tool will refuse, which would turn a "+
					"return into a run-time error", tc.stored, got.RestoreStatus, tc.want)
			}
		})
	}
}

// Both causes, one mechanism (the SPEC's "close/reopen is ONE mechanism over
// both facts and both sources"): a ticket that left `done`, and a ticket
// assigned back to Salvador.
func TestDecide_ReopenFiresForBothFacts(t *testing.T) {
	state := &ticketstatus.State{LastAction: "closed", ClosedFromStatus: "ready"}

	t.Run("ticket left done", func(t *testing.T) {
		o := obs("indeterminate", tsOther, false) // gate off: status alone decides
		o.TaskStatus = "closed"
		if got := ticketstatus.Decide(o, state); got.Action != "reopened" {
			t.Errorf("a ticket transitioned out of Done did not bring its task back: %+v", got)
		}
	})
	t.Run("ticket assigned back to me", func(t *testing.T) {
		o := obs("indeterminate", tsOwn, true) // gate on, now mine again
		o.TaskStatus = "closed"
		if got := ticketstatus.Decide(o, state); got.Action != "reopened" {
			t.Errorf("a ticket re-assigned to the polling account did not bring its task back: %+v. "+
				"'Assigned away from me' and 'moved to Done' are the same event as far as the board "+
				"is concerned, and so are their inverses", got)
		}
	})
}

// "warranted + task closed + state.last_action == 'closed' + a task_dismissals
// row -> suppressed_dismissed: ONE task_append_log on the closed task, no status
// change, recorded once and never repeated (D4)."
//
// D4's non-redundancy argument, which is the part worth remembering: this is NOT
// covered by the precondition above. The pass can close a task ITSELF and
// Salvador can THEN record a dismissal label on the already-closed row — SWT-31
// criterion 14 permits exactly that — so `last_action='closed'` and a dismissal
// row coexist, and only this clause stops the next reopen from undoing a human's
// judgement.
func TestDecide_ADismissedTaskNeverResurfaces(t *testing.T) {
	o := obs("indeterminate", tsOwn, true)
	o.TaskStatus = "closed"
	o.Dismissed = true

	got := ticketstatus.Decide(o, &ticketstatus.State{LastAction: "closed", ClosedFromStatus: "delivered"})

	if got.Action != "suppressed_dismissed" {
		t.Fatalf("Decide(warranted, closed by this pass, DISMISSED) = %+v, want "+
			"Action=suppressed_dismissed. D4: a human dismissal outranks a reconciler echo — the four "+
			"dismissal reasons are all statements about whether the task should exist, none of which "+
			"a status flip invalidates, and resurrecting the task would destroy the meaning of the "+
			"label as training data", got)
	}
	if got.RestoreStatus != "" {
		t.Errorf("Decide(... dismissed).RestoreStatus = %q, want empty: nothing is restored", got.RestoreStatus)
	}
	if !got.Act {
		t.Errorf("Decide(... dismissed).Act = false, want true on the FIRST observation: the " +
			"suppression appends ONE log line to the closed task, so 'why is this ticket open and " +
			"its task not' is answerable from the task page")
	}

	// ...and then never again: once the suppression is recorded, last_action is
	// no longer 'closed', so criterion 26's rule carries the rest — no second
	// log line, ever, for as long as the ticket stays open.
	after := ticketstatus.Decide(o, &ticketstatus.State{LastAction: "suppressed_dismissed"})
	if after.Act {
		t.Errorf("Decide(... dismissed, suppression already recorded) = %+v, want Act=false. "+
			"'Recorded once and never repeated' (D4) — every 15 minutes forever is not a record, it "+
			"is a leak", after)
	}
}

// D5, as a decision rather than a tool refusal: the dismissal check lives in the
// PASS, so `task_reopen` stays general and a human can undo a mis-click. The
// observable half here is that a dismissal does NOT suppress an ordinary close:
// a dismissed task whose ticket is done is already closed and simply converges.
func TestDecide_DismissalOnlyBlocksTheReturnPath(t *testing.T) {
	o := obs("done", tsOwn, true)
	o.TaskStatus = "closed"
	o.Dismissed = true
	got := ticketstatus.Decide(o, &ticketstatus.State{LastAction: "closed", ClosedFromStatus: "ready"})
	if got.Act {
		t.Errorf("Decide(done ticket, closed, dismissed) = %+v, want Act=false — nothing to do. The "+
			"dismissal clause is about RESURRECTION; a task that is already closed and whose ticket "+
			"is still done needs no log line to explain itself", got)
	}
}

// ---- criterion 29: flapping is symmetric in both dimensions -----------------

// "close -> reopen -> close over one state row, driven once by status and once
// by assignment, is a unit test."
//
// One state row, threaded through the way the driver threads it: each step's
// State is what the previous step's Decision would have written. That threading
// is the test — it is where a design that recorded the wrong thing (or nothing)
// on the convergent pass falls over.
func TestDecide_FlapsSymmetrically(t *testing.T) {
	for _, dim := range []struct {
		name       string
		gateOn     bool
		gone, back ticketstatus.Observation
	}{
		{
			name:   "by status",
			gateOn: false,
			gone:   obs("done", tsOwn, false),
			back:   obs("indeterminate", tsOwn, false),
		},
		{
			name:   "by assignment",
			gateOn: true,
			gone:   obs("indeterminate", tsOther, true),
			back:   obs("indeterminate", tsOwn, true),
		},
	} {
		dim := dim
		t.Run(dim.name, func(t *testing.T) {
			// 1. the ticket stops warranting the task: close, from `delivered`.
			step1 := dim.gone
			step1.TaskStatus = "delivered"
			d1 := ticketstatus.Decide(step1, nil)
			if d1.Action != "closed" || !d1.Act {
				t.Fatalf("step 1 (drop) = %+v, want Action=closed Act=true", d1)
			}
			state := &ticketstatus.State{
				LastAction: d1.Action, ClosedFromStatus: step1.TaskStatus,
				StatusCategory: step1.StatusCategory, Assignee: step1.Assignee,
			}

			// 2. it warrants again: reopen, to the status it held.
			step2 := dim.back
			step2.TaskStatus = "closed"
			d2 := ticketstatus.Decide(step2, state)
			if d2.Action != "reopened" || d2.RestoreStatus != "delivered" {
				t.Fatalf("step 2 (return) = %+v, want Action=reopened RestoreStatus=delivered — D6 "+
					"restores what the task held instead of flattening it to ready", d2)
			}
			state = &ticketstatus.State{
				LastAction: d2.Action, StatusCategory: step2.StatusCategory, Assignee: step2.Assignee,
			}

			// 3. and away again: the same close, from the restored status.
			step3 := dim.gone
			step3.TaskStatus = "delivered"
			d3 := ticketstatus.Decide(step3, state)
			if d3.Action != "closed" || !d3.Act {
				t.Fatalf("step 3 (drop again) = %+v, want Action=closed Act=true. A state machine that "+
					"could only fire once would leave a re-closed ticket's task on the board "+
					"permanently", d3)
			}
		})
	}
}
