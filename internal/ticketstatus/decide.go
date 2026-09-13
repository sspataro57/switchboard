// Package ticketstatus reconciles switchboard tasks with the Jira tickets they
// reference (SWT-32, docs/tickets/jira-status-sync_SPEC.md): a ticket that
// stops warranting its task — moved to Done, or assigned away under a gated
// project — drops the task from the board, and a ticket that warrants it again
// brings it back. One predicate, THREE facts since SWT-34 (status category, a per-project
// configured delivered status, the assignee gate), both directions (D15).
//
// The DECISION in this file is pure: a function of (observation, recorded
// state) with zero I/O, in the internal/orchestrator/rules.go discipline
// (invariant 7). The driver (store.go) supplies the observation from stored
// rows — never from an HTTP response in memory (D19) — and makes every task
// write through the executor as ticketstatus:jira (invariant 3).
package ticketstatus

// Observation is everything Decide may know about one candidate ref: the two
// ticket facts as read from the STORED raw snapshot, the identity to compare
// against, the project's gate, the task as it stands, and whether a human
// dismissed it (D4).
type Observation struct {
	TicketKey      string // diagnostic; the prose reasons name it
	StatusCategory string // jira.Facts — 'new' | 'indeterminate' | 'done'
	StatusName     string // DIAGNOSTIC ONLY (D2). Nothing branches on it.
	StatusKnown    bool
	Assignee       string // fields.assignee.accountId ("" + known = unassigned, D14)
	AssigneeKnown  bool
	OwnAccountID   string // the STORING account's sync_cursor->>'own_account_id' (D12)
	GateOn         bool   // projects.ticket_assignee_gate — from the COLUMN (criterion 33)
	TaskStatus     string // tasks.status as it stands right now
	// Dismissed: an OPEN task_dismissals row (reopened_at IS NULL) exists for
	// this task (D4; SWT-36 D9). A dismissal overtaken by inbound activity, or
	// undone by a human, no longer suppresses — the task is ordinary again,
	// and a re-dismissal re-arms D4.
	Dismissed bool
	// DeliveredStatuses is the per-project set of status NAMES meaning "I
	// delivered; the ball is in someone else's court" (SWT-34). A VALUE the
	// driver read from the projects column — if Decide could look it up
	// itself, the decision table would stop proving anything (invariant 7),
	// which is also why the column is named in the driver's query and nowhere
	// else.
	DeliveredStatuses []string
}

// State is the ticket_status_syncs row for this external_ref, or nil when this
// pass has never observed the ref before.
type State struct {
	LastAction       string // the stored last_action
	ClosedFromStatus string // the status the task held when THIS pass closed it
	StatusCategory   string // the facts as last recorded — what makes "the same
	Assignee         string // unchanged observation" decidable (criterion 24)
	// StatusName joins that key for SWT-34's E6: once a NAME can trigger the
	// drop, a claimed task whose ticket moves between two delivered statuses of
	// the same category and assignee would otherwise get no second log line —
	// and its existing log would name a status the ticket has left.
	StatusName string
}

// Decision is what one observation becomes. Action is spelled as
// ticket_status_syncs.last_action stores it — none | closed | reopened |
// refused_active | suppressed_dismissed — plus `unreadable` for evidence gaps,
// which is NOT a last_action (an unreadable ref gets no state row at all:
// status_category is NOT NULL with a three-value CHECK). Act is about the
// EXECUTOR: every decision except unreadable writes/updates the state row;
// Act=false means it does so without calling task_close / task_reopen /
// task_append_log — the pair that makes criterion 44's idempotence a pure fact.
type Decision struct {
	Warranted     bool
	Action        string
	DropReason    string // 'ticket_done' | 'not_assigned'; "" unless dropped
	RestoreStatus string // the reopen target; "" otherwise
	Act           bool
}

// restorable is D6's set as Decide consults it: the statuses a reopen may
// restore. The authoritative single spelling for the two VERBS lives in
// internal/tools/close.go (openStatuses, criterion 37); this copy exists only
// so Decide can stay pure — it must match, and the integration suite drives
// both through real rows.
func restorable(status string) bool {
	switch status {
	case "holding", "ready", "blocked", "done_locally", "delivered":
		return true
	}
	return false
}

// activeWork mirrors task_close's refusal set: never close work out from under
// a holder.
func activeWork(status string) bool {
	switch status {
	case "claimed", "in_progress", "needs_feedback":
		return true
	}
	return false
}

// Decide is the whole rule set (criteria 21-29, 32). Pure — zero I/O.
func Decide(obs Observation, state *State) Decision {
	warranted, drop, readable := Warranted(obs)
	if !readable {
		return Decision{Action: "unreadable"}
	}

	if !warranted {
		switch {
		case restorable(obs.TaskStatus):
			return Decision{Warranted: false, Action: "closed", DropReason: drop, Act: true}
		case activeWork(obs.TaskStatus):
			// One log line per OBSERVATION, not per pass: a re-assignment is a
			// fact the task's log has not recorded yet; an unchanged one is 96
			// identical lines a day (criterion 24).
			act := state == nil || state.LastAction != "refused_active" ||
				state.StatusCategory != obs.StatusCategory || state.Assignee != obs.Assignee ||
				// E6: the NAME is part of the observation once it can trigger
				// the drop — a move between two delivered statuses of one
				// category is new information the task's log has not recorded.
				state.StatusName != obs.StatusName
			return Decision{Warranted: false, Action: "refused_active", DropReason: drop, Act: act}
		default: // closed
			if state != nil && state.LastAction == "closed" {
				// Converged — and the record must KEEP saying 'closed' (D3):
				// writing 'none' here would erase the only fact that authorises
				// a later reopen, invisibly, until the day the ticket reopens
				// and the task never comes back.
				return Decision{Warranted: false, Action: "closed", DropReason: drop, Act: false}
			}
			// The pass must never claim a close it did not make (criterion 23)
			// — that claim is what would authorise reopening a human's
			// dismissal or an R8 delivery close later.
			return Decision{Warranted: false, Action: "none", DropReason: drop, Act: false}
		}
	}

	// Warranted.
	if obs.TaskStatus != "closed" {
		return Decision{Warranted: true, Action: "none"}
	}
	if state == nil || state.LastAction != "closed" {
		// Closed by someone else — a human, R8, a hand-run task_close. Not ours
		// to undo (criterion 26).
		return Decision{Warranted: true, Action: "none"}
	}
	if obs.Dismissed {
		// D4: a human dismissal outranks a reconciler echo — and this is NOT
		// redundant with the clause above: the pass can close a task itself and
		// a human can THEN label the closed row (SWT-31 criterion 14), so
		// last_action='closed' and a dismissal row coexist. One log line, once.
		return Decision{Warranted: true, Action: "suppressed_dismissed", Act: true}
	}
	restore := state.ClosedFromStatus
	if !restorable(restore) {
		restore = "ready"
	}
	return Decision{Warranted: true, Action: "reopened", RestoreStatus: restore, Act: true}
}

// Warranted is the ONE spelling of "does this ticket warrant a task" (SWT-40
// D-D3), shared by Decide and the capture-time gate (capture.DecideGate), so the
// gate never creates a task this reconciler would close 15 minutes later. Pure —
// zero I/O. readable=false is an evidence gap, and then warranted and dropReason
// carry no verdict in either direction (criterion 32). dropReason names the fact
// that dropped the ticket ('ticket_done' | 'ticket_delivered' | 'not_assigned')
// and is "" when warranted.
func Warranted(obs Observation) (warranted bool, dropReason string, readable bool) {
	// Criterion 32: evidence gaps are unreadable, never a verdict in either
	// direction. D12's fail-safe: a task must not vanish because we could not
	// identify ourselves. With the gate OFF the assignee is never consulted, so
	// its absence is not a gap (D11's whole meaning).
	if !obs.StatusKnown || (obs.GateOn && (!obs.AssigneeKnown || obs.OwnAccountID == "")) {
		return false, "", false
	}

	// D15 as SWT-34 extends it: one predicate, now THREE facts. drop_reason
	// records WHICH fact dropped it, chosen by ONE ordered list (E7):
	// ticket_done > ticket_delivered > not_assigned.
	//
	// The order's argument: `done` keeps the top slot because the strongest
	// statement about a ticket is that it is finished (an admin may give a
	// done-category status a QA-sounding name, and it is still done). `delivered` sits
	// directly beneath it because it is the same KIND of fact — the ticket's own
	// lifecycle — and a weaker version of it; a QA ticket also assigned to a QA
	// engineer was dropped because he delivered it, and recording that as
	// `not_assigned` would make the counter he reads to judge a capture rule
	// wrong. Inserting it between the two leaves SWT-32's done > not_assigned
	// relation byte-identical.
	delivered := IsDeliveredStatus(obs.StatusName, obs.DeliveredStatuses)
	warranted = obs.StatusCategory != "done" && !delivered &&
		(!obs.GateOn || obs.Assignee == obs.OwnAccountID)
	drop := ""
	switch {
	case obs.StatusCategory == "done":
		drop = "ticket_done"
	case delivered:
		drop = "ticket_delivered"
	case obs.GateOn && obs.Assignee != obs.OwnAccountID:
		drop = "not_assigned"
	}
	return warranted, drop, true
}
