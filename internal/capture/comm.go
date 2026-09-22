package capture

// comms-inbox (SWT-74 D2): does a task_log attach make its own COMM task —
// a row in INCOMING that says "someone said something, look" — or file
// silently, as it did before? Pure: the arming flag, the target's status and
// the three sender facts arrive as values from rules_store.go (the
// resurface.go / overrides precedent). No context, no database, no connector
// import, no environment, no clock, no pattern matching.
//
// Deliberately ABSENT, each because it would be an INERT predicate (the
// constant-discriminator landmine, five costumes and counting):
//   - a `dismissed` clause: taskForExternalRef joins task_dismissals under
//     `AND t.status = 'closed'`, so an OPEN task never carries one, and clause 2
//     already excluded every closed task — a dismiss clause would be false for
//     every row that reaches it;
//   - a `connectorCopy` clause: J3's structural dedup is enforced by NOT ARMING
//     the connector rules, so a Go clause would be false on every armed rule
//     that exists;
//   - `prNotice` / `prClose`: impossible under 0040's CHECK (NOT pr_review), a
//     data-level guarantee, so the clause could never fire;
//   - a `channel`: the comm path is channel-blind (criterion 45).

import "fmt"

// commInput is everything commTask reads.
type commInput struct {
	armed       bool   // the winning rule's arming column (rules_store.go reads it)
	status      string // the linked task's tasks.status, from taskForExternalRef
	blankSender bool   // blankSender(sender)                        — resurface.go's spelling
	notifier    bool   // notifierSender(sender, winner.notifiers)   — resurface.go's spelling
	ownJiraEdit bool   // anonymousJiraActor(sender)                 — ownaction.go's spelling
}

// commTask is true iff the rule is armed AND the task is not closed AND the
// sender is not blank AND not a notifier AND not Jira's Anonymous placeholder.
// The string is the reason fragment capture_decisions.reason records: FIRST
// CAUSE WINS, in that order, and every false outcome names its own cause so a
// smoke read of the decision row can tell them apart.
//
// The blank-sender clause FAILS CLOSED, as resurfaces' does: notifierSender is
// an equality and can never match "", so with no identity at all the message
// only logs.
func commTask(in commInput) (bool, string) {
	switch {
	case !in.armed:
		return false, "no comm task: the matched rule is not a comm rule"
	case in.status == "closed":
		return false, "no comm task: the task is closed, so SWT-45's revive and SWT-53's resurface own it"
	case in.blankSender:
		return false, "no comm task: the message has no sender identity (blank sender), so it only logs"
	case in.notifier:
		return false, "no comm task: the sender is on the project's notifier list"
	case in.ownJiraEdit:
		return false, "no comm task: the sender is Jira's anonymous placeholder, i.e. his own change"
	}
	return true, fmt.Sprintf("comm task: a person's message on an open task (%s)", in.status)
}
