package capture

// chat-on-closed-task (SWT-53, docs/tickets/chat-on-closed-task_SPEC.md) CC3 and
// CC4: whether a task_log onto a task RESURFACES the message through the inquiry
// lane, and whether its sender is a notifier. Pure: the task's status, the
// activity flag, the dismissal, the connector-copy fact and the notifier list
// all arrive as values from rules_store.go (the revive.go / overrides
// precedent).

import (
	"fmt"
	"strings"
)

// resurfaceInput is everything resurfaces reads. The action is not an input:
// resurfaces is called only on decideMessage's `found` branch, where the action
// is already task_log (migration 0034's CHECK pins the pair in the data).
type resurfaceInput struct {
	status        string // the linked task's tasks.status, from taskForExternalRef
	activity      bool   // decideMessage's `activity` (SWT-45 J1)
	dismissed     bool   // the task has an OPEN dismissal (SWT-36)
	connectorCopy bool   // pm.channel == jira.Channel (SWT-45 J3), computed by the caller
	notifier      bool   // notifierSender(sender, the winner project's notifier list) (CC4)
	blankSender   bool   // blankSender(sender): the message carries no sender identity (CC4b)
	// prNotice: the message is GitHub's PR state notice (merged, closed or
	// reopened) for a pr_review rule's PR, i.e. decideMessage's d.prNotice != ""
	// (treetop-pr-review-tasks, SWT-54 D4). Cross-ticket rule: a state notice is
	// logged and NOTHING else changes, so it never resurfaces the review task it
	// closed (or any closed review task) through the inquiry lane, whatever the
	// notifier list says. Computed by the caller, so this file stays pure.
	prNotice bool
}

// resurfaces is true iff the task is closed AND the match is not activity AND
// the task has no open dismissal AND the message is not the Jira connector's own
// copy AND the message is not a GitHub PR state notice (SWT-54 D4) AND the sender
// is not a notifier AND the sender is not blank. The string
// is the reason fragment the decision row records; every false outcome names its
// own cause, so a smoke read of capture_decisions.reason can tell them apart.
//
// The blank-sender disqualifier (CC4b) FAILS CLOSED: notifierSender is an
// equality match, so no list entry can ever equal an empty sender, and without
// this case a message with no identity at all would resurface into the inquiry
// model by default. With no sender there is nobody to tell a bot from a human,
// so the message only logs.
func resurfaces(in resurfaceInput) (bool, string) {
	switch {
	case in.status != "closed":
		return false, fmt.Sprintf("not resurfaced: the task is %q, not closed, so its log line is on the board", in.status)
	case in.activity:
		return false, "not resurfaced: the rule is activity, so SWT-45 owns this closed task"
	case in.dismissed:
		return false, "not resurfaced: the task has an open dismissal, so SWT-36's guarded reopen owns it"
	case in.connectorCopy:
		return false, "not resurfaced: the Jira connector's own copy never resurfaces (SWT-45 J3)"
	case in.prNotice:
		return false, "not resurfaced: a GitHub PR state notice (merged, closed or reopened) is logged only and never resurfaces a review task (SWT-54 D4)"
	case in.notifier:
		return false, "not resurfaced: the sender is on the project's notifier list"
	case in.blankSender:
		return false, "not resurfaced: the message has no sender identity (blank sender), so it is logged silently"
	}
	return true, "logged onto a closed task; resurfaced for the inquiry lane (chat-on-closed-task)"
}

// blankSender reports whether the stored sender is empty or whitespace only:
// the message carries no identity, so resurfaces fails closed on it (CC4b).
func blankSender(sender string) bool {
	return strings.TrimSpace(sender) == ""
}

// notifierSender reports whether an entry of list, trimmed and case-folded,
// EQUALS the whole stored sender (a Slack display name) or the address parsed
// from it (a mail From). Equality, never a substring: the `sender` capture
// criterion is a substring match, and reusing it would let an entry `Jira`
// swallow a human whose name merely contains it. Empty and whitespace entries
// are skipped, so they can never equal an address-less sender's "".
func notifierSender(sender string, list []string) bool {
	whole := strings.ToLower(strings.TrimSpace(sender))
	addr := strings.ToLower(strings.TrimSpace(senderAddress(sender)))
	for _, e := range list {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if e == whole || (addr != "" && e == addr) {
			return true
		}
	}
	return false
}
