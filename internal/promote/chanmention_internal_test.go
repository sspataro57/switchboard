package promote

// slack-channel-mentions (Jira SWT-79, docs/tickets/slack-channel-mentions_SPEC.md)
// criterion 11, decision D4: a Slack channel message that @-mentions Salvador is
// ADDRESSED to him in promotion. Without it every top-level mention
// (conversation scope) and every mention in a thread he has not posted in is
// gated `not_addressed`, and the feature does nothing — in #a-millon above all.
//
// Plain unit test, zero I/O: addressed() and InquiryGate are pure; Mentioned is
// an INPUT (the inbox computes it with slackweb.MentionsOwner over nm.body_text).
//
// ---- IMPOSED SURFACE ---------------------------------------------------------
//
//	type InquiryCandidate struct { …; Mentioned bool }
//	// addressed gains, BEFORE the thread rule:
//	//   case c.Channel == slackweb.Channel && c.Mentioned: return true
//	// and KEEPS the `thread && prior post` clause.
//
// Mentioned is set by REFLECTION (by name), not by a composite-literal field,
// so this file compiles today and fails on BEHAVIOUR ("InquiryCandidate has no
// Mentioned field") without breaking the package's other tests.
//
// MUTATIONS: drop the new clause → the Mentioned channel rows go red; make it
// channel-agnostic (c.Mentioned alone) → the jira/upwork rows go red; drop the
// old thread clause → "thread + prior post, not Mentioned" goes red.

import (
	"reflect"
	"testing"
	"time"
)

var chmNow = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

func chmSetMentioned(t *testing.T, c *InquiryCandidate, v bool) {
	t.Helper()
	f := reflect.ValueOf(c).Elem().FieldByName("Mentioned")
	if !f.IsValid() {
		t.Fatalf("InquiryCandidate has no Mentioned field. SWT-79 D4/criterion 11: the inbox sets " +
			"Mentioned = slackweb.MentionsOwner(nm.body_text), and addressed() reads it")
	}
	if f.Kind() != reflect.Bool || !f.CanSet() {
		t.Fatalf("InquiryCandidate.Mentioned is not a settable bool (kind %s)", f.Kind())
	}
	f.SetBool(v)
}

func chmCandidate(channel, key, scope string) InquiryCandidate {
	return InquiryCandidate{
		AskKind: "question", Channel: channel, ThreadKey: key, ThreadScope: scope,
		StoredThreadID: 7, CurrentThreadID: 7, SentAt: chmNow.Add(-2 * time.Hour),
	}
}

func TestSWT79_Addressed_AChannelMentionIsAddressed(t *testing.T) {
	const (
		topLevel  = "slack:T0360B84U:C1C1TSLJH"
		thread    = "slack:T0360B84U:C1C1TSLJH:p1758550000000100"
		dm        = "slack:T0360B84U:D01EJRX6P45"
		groupDM   = "slack:T0HPR78RX:G01GROUPDM"
		jiraKey   = "jira:avviato.atlassian.net:LHH-1"
		upworkKey = "upwork:room:abc123"
		gmailKey  = "gmail:salvador@handsonconnect.org:thread-1"
	)
	cases := []struct {
		name                string
		channel, key, scope string
		mentioned, prior    bool
		wantAddressed       bool
		wantGate            string
	}{
		{"conversation-scope channel + Mentioned → addressed (D4: the point of the ticket)",
			"slack", topLevel, "conversation", true, false, true, ""},
		{"conversation-scope channel, not Mentioned → not_addressed (unchanged)",
			"slack", topLevel, "conversation", false, false, false, GateNotAddressed},
		{"conversation-scope channel, not Mentioned, even after he spoke there → not_addressed (unchanged)",
			"slack", topLevel, "conversation", false, true, false, GateNotAddressed},
		{"channel thread he never posted in + Mentioned → addressed",
			"slack", thread, "thread", true, false, true, ""},
		{"thread + prior post, not Mentioned → addressed (the old clause is KEPT)",
			"slack", thread, "thread", false, true, true, ""},
		{"channel thread, no prior post, not Mentioned → not_addressed (unchanged)",
			"slack", thread, "thread", false, false, false, GateNotAddressed},
		{"a group DM + Mentioned → addressed (a Slack non-DM candidate with a mention)",
			"slack", groupDM, "conversation", true, false, true, ""},
		{"a group DM, not Mentioned → not_addressed (unchanged, C-D4)",
			"slack", groupDM, "conversation", false, false, false, GateNotAddressed},
		{"a jira candidate with Mentioned → unchanged: not_addressed without a prior post (the clause is Slack-only)",
			"jira", jiraKey, "thread", true, false, false, GateNotAddressed},
		{"an upwork candidate with Mentioned → unchanged: not_addressed (the clause is Slack-only)",
			"upwork", upworkKey, "conversation", true, false, false, GateNotAddressed},
		{"a 1:1 DM, not Mentioned → addressed (unchanged)",
			"slack", dm, "conversation", false, false, true, ""},
		{"a 1:1 DM, Mentioned → addressed (unchanged)",
			"slack", dm, "conversation", true, false, true, ""},
		{"gmail, not Mentioned → addressed (unchanged)",
			"gmail", gmailKey, "thread", false, false, true, ""},
		{"gmail, Mentioned → addressed (unchanged)",
			"gmail", gmailKey, "thread", true, false, true, ""},
	}
	for _, tc := range cases {
		c := chmCandidate(tc.channel, tc.key, tc.scope)
		c.PriorPost = tc.prior
		chmSetMentioned(t, &c, tc.mentioned)
		if got := addressed(c); got != tc.wantAddressed {
			t.Errorf("%s: addressed(%+v) = %v, want %v", tc.name, c, got, tc.wantAddressed)
		}
		if got := InquiryGate(c, chmNow); got != tc.wantGate {
			t.Errorf("%s: InquiryGate = %q, want %q", tc.name, got, tc.wantGate)
		}
	}
}

// The mention does not jump the gates BEFORE not_addressed: an answered,
// stale or wrong-kind mention is still gated by its own reason (C3 order).
func TestSWT79_Addressed_AMentionDoesNotSkipEarlierGates(t *testing.T) {
	base := func() InquiryCandidate {
		c := chmCandidate("slack", "slack:T0360B84U:C1C1TSLJH", "conversation")
		chmSetMentioned(t, &c, true)
		return c
	}
	if got := InquiryGate(base(), chmNow); got != "" {
		t.Fatalf("CONTROL: a fresh, unanswered top-level channel question that mentions him is gated %q; "+
			"D4 makes it addressed", got)
	}
	for name, tc := range map[string]struct {
		mod  func(*InquiryCandidate)
		want string
	}{
		"answered":   {func(c *InquiryCandidate) { c.RepliedSince = true }, GateAnswered},
		"stale":      {func(c *InquiryCandidate) { c.SentAt = chmNow.Add(-73 * time.Hour) }, GateStale},
		"fyi kind":   {func(c *InquiryCandidate) { c.AskKind = "fyi" }, GateKind},
		"rethreaded": {func(c *InquiryCandidate) { c.CurrentThreadID = 8 }, GateRethreaded},
	} {
		c := base()
		tc.mod(&c)
		if got := InquiryGate(c, chmNow); got != tc.want {
			t.Errorf("%s mention: InquiryGate = %q, want %q", name, got, tc.want)
		}
	}
}
