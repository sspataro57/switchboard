package capture

import (
	"fmt"
	"strings"
)

// SWT-45 (docs/tickets/jira-activity-revive_SPEC.md) J1: which capture rules'
// matches count as Jira activity that revives a closed task (or creates one) and
// surfaces it past the reconciler. Pure — the flags arrive as values read from
// the capture_rules and projects columns.

// overrides reports whether a match by a rule with these flags acts as activity
// on a project whose assignee gate is gateOn:
//
//	overrides = revive AND (NOT gateOn OR addressed)
//
// On a gate-off project `revive` alone overrides (owner decision 1: any
// activity). On a gated project only `addressed` does (decision 3: only mail
// addressed to him bypasses the assignee check); a revive-only rule there still
// goes through the SWT-40 Part D gate, so an operator cannot bypass the gate by
// forgetting which flag means what. `addressed` without `revive` is refused by
// migration 0030's CHECK and overrides nothing here either.
func overrides(revive, addressed, gateOn bool) bool {
	return revive && (!gateOn || addressed)
}

// commentHoldInput is what commentHolds may know about an activity match on a
// task that is NOT closed (SWT-82). Values only.
type commentHoldInput struct {
	activity      bool   // overrides(...) for the winning rule, on a jira-keyed match
	status        string // tasks.status of the linked task
	connectorCopy bool   // the message is the Jira connector's own copy (channel jira)
	externalID    string // normalized_messages.external_message_id
	blankSender   bool
	notifier      bool
}

// commentHolds reports whether an activity match on an OPEN task surfaces it
// (SWT-82), so the ticket-status reconciler holds the task instead of closing
// it for a delivered or done ticket in the same tick.
//
// Only a PERSON's COMMENT as the Jira connector copied it qualifies: the
// connector stores it as jira:{site}:comment:{id} and marks his own comments
// outbound, which capture never decides. Everything else keeps J10 (activity
// on an open task only logs): Jira's notification EMAILS, above all the one
// every close sends, and the ticket-description copy (…:issue:{KEY}), which
// the connector re-emits on any update. Without this, a comment that lands
// while the task is still open is logged and then closed seconds later by the
// reconciler, while the same comment a minute after the close revives the task
// (J9) — Jahnvi's QA comments on API-4323/4324, 2026-09-23.
func commentHolds(in commentHoldInput) (bool, string) {
	switch {
	case !in.activity:
		return false, ""
	case in.status == "closed":
		return false, "" // J9's revive owns a closed task
	case in.status == "claimed" || in.status == "in_progress" || in.status == "needs_feedback":
		// Active work: the holder already reads the comment in the log, and the
		// reconciler never consumes a surfacing while work is active, so it
		// would linger and hold the task through a LATER delivered status.
		return false, fmt.Sprintf("not held: the task is %s (active work)", in.status)
	case !in.connectorCopy || !strings.Contains(in.externalID, ":comment:"):
		return false, ""
	case in.blankSender:
		return false, "not held: the comment has no sender identity"
	case in.notifier:
		return false, "not held: the commenter is on the project's notifier list"
	}
	return true, fmt.Sprintf("a person's Jira comment on a %s task holds it against the ticket-status reconciler (SWT-82)", in.status)
}
