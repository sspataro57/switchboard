package capture

// slack-channel-mentions (SWT-79, docs/tickets/slack-channel-mentions_SPEC.md
// D1-D2): is this `attributed` decision a Slack CHANNEL message that does not
// @-mention Salvador? Such a message gets no inquiry verdict — both inquiry
// inboxes skip a decision row carrying the fact. Salvador, 2026-09-23:
// "channels is only when they mention me" / "we only respond to mentions on
// those channels".
//
// Pure: the channel, the DM facts and the mention arrive as values from
// rules_store.go (the direct.go / resurface.go precedent). No context, no
// database, no connector import, no environment, no clock.

// channelMentionInput is everything channelUnmentioned reads.
type channelMentionInput struct {
	slack     bool // pm.channel == slackweb.Channel
	dm        bool // slackweb.IsDirectMessageKey(thread key): a 1:1 DM
	groupDM   bool // raw conversation.type == 'group_dm' (C… ids are ambiguous)
	mentioned bool // slackweb.MentionsOwner(body)
}

// channelUnmentionedSuffix is the reason suffix a flagged decision records.
const channelUnmentionedSuffix = "; a channel message that does not mention Salvador: no inquiry verdict (SWT-79)"

// channelUnmentioned is true iff the message is Slack AND not a DM AND not a
// group DM AND does not mention him. DMs and group DMs are SWT-78's (they are
// decided before reaching here, or keep today's path as a notifier/blank-sender
// DM); non-Slack channels are untouched. The string names the cause either way.
func channelUnmentioned(in channelMentionInput) (bool, string) {
	switch {
	case !in.slack:
		return false, "not Slack: the mention gate applies to Slack channels only"
	case in.dm:
		return false, "a 1:1 DM: never mention-gated"
	case in.groupDM:
		return false, "a group DM: never mention-gated"
	case in.mentioned:
		return false, "a channel message that mentions Salvador: eligible for an inquiry verdict"
	}
	return true, channelUnmentionedSuffix
}
