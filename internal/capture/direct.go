package capture

// slack-messages-not-becoming-tasks (SWT-78, docs/bugs/slack-messages-not-becoming-tasks_DIAGNOSIS.md
// items A-B): does a message at capture's attribution-only exit take the DM
// CONVERSATION-TASK path instead of the inquiry lane? Salvador, 2026-09-23:
// "basically all DMs to me are actionable just messages on the general forum
// need decision" / "fix it so DMs skip qwen" / "one task per conversation".
// On 2026-09-22 the inquiry lane (qwen) dropped 16 of 22 DMs to him as
// needs_reply=false, a verdict nothing reads.
//
// Pure: the channel, the DM facts and the two sender facts arrive as values
// from rules_store.go (the resurface.go / comm.go precedent). No context, no
// database, no connector import, no environment, no clock.
//
// Deliberately ABSENT: a direction. Capture never decides an outbound message
// (pendingMessages reads direction='inbound' only, invariant 5), so his own
// messages can never reach this predicate, and a field here would invite a
// caller to believe the predicate is what keeps them out.

import "fmt"

// directInput is everything directConversationTask reads.
type directInput struct {
	slack       bool // pm.channel == slackweb.Channel, computed by the caller
	dm          bool // slackweb.IsDirectMessageKey(thread key): a 1:1 DM, rooted or not
	groupDM     bool // raw conversation.type == 'group_dm', read from the COLUMN (C… ids are ambiguous)
	blankSender bool // blankSender(sender)                      — resurface.go's spelling
	notifier    bool // notifierSender(sender, winner.notifiers) — resurface.go's spelling
}

// directConversationTask is true iff the message is Slack AND a DM or group DM
// AND the sender is not blank AND not on the project's notifier list (the Jira
// app's author id is an ordinary U… id; the list is how a bot is told apart).
// The string is the reason fragment the decision row records: FIRST CAUSE
// WINS, and every false outcome names its own cause.
//
// The blank-sender clause FAILS CLOSED, as resurfaces' and commTask's do: with
// no identity there is nobody to tell a bot from a person, so the message keeps
// today's attribution-only decision.
func directConversationTask(in directInput) (bool, string) {
	switch {
	case !in.slack:
		return false, "not a DM task: the message is not Slack"
	case !in.dm && !in.groupDM:
		return false, "not a DM task: the conversation is a channel, so the inquiry lane decides"
	case in.blankSender:
		return false, "not a DM task: the message has no sender identity (blank sender)"
	case in.notifier:
		return false, "not a DM task: the sender is on the project's notifier list"
	}
	kind := "a DM"
	if !in.dm {
		kind = "a group DM"
	}
	return true, fmt.Sprintf("DM task: a person's message in %s is always actionable (SWT-78); no classifier", kind)
}
