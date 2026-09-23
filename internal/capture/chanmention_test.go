package capture

// slack-channel-mentions (Jira SWT-79, docs/tickets/slack-channel-mentions_SPEC.md)
// criteria 3 and 4, decisions D1/D2: capture records, on every `attributed`
// decision it writes, whether the message is a Slack CHANNEL message that does
// not @-mention Salvador. Such a message gets no inquiry verdict (qwen). DMs and
// group DMs are never flagged (SWT-78 owns them), and non-Slack is never flagged.
// Salvador, 2026-09-23: "channels is only when they mention me".
//
// ZERO I/O. Every case below reaches decideMessage through a path that never
// touches the pool (the attribution-only exits, the keyless exit, the non-PR
// github exit, and a pr_review fall-through whose origin check refuses a
// message with no stored raw row before any query), so the pool is nil.
//
// ---- IMPOSED SURFACE (the SPEC's "API / tool changes" → Capture) --------------
//
//	type ruleDecision struct { …; channelUnmentioned bool } // WRITTEN, like resurface
//
//	decideMessage = the inner decision (which prFallThrough keeps calling)
//	                + ONE post-decision step that sets channelUnmentioned from
//	                  pm.channel == slackweb.Channel, slackweb.IsDirectMessageKey,
//	                  pm.rawConvType == "group_dm" and slackweb.MentionsOwner(body),
//	                  and on true appends chmSuffix to the reason.
//
// The field is read by REFLECTION (by name), not by selector, so this file
// compiles today and fails on BEHAVIOUR: "ruleDecision has no channelUnmentioned
// field". A test that named the new pure predicate directly would compile-fail
// the whole internal/capture test binary (unit AND integration) until it exists,
// hiding every other red; the pure table therefore goes through decideMessage.
//
// MUTATIONS: apply the fact inside the inner decision (the path prFallThrough
// recurses into) → the suffix appears twice → FallThroughAppliesTheFactOnce red;
// drop the groupDM clause → NotifierGroupDMIsNeverFlagged red; drop the DM
// clause → NotifierDMIsNeverFlagged red; flag every exit but the keyless/github
// ones → EveryAttributedExit red.

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// chmSuffix is criterion 4's exact reason suffix.
const chmSuffix = "; a channel message that does not mention Salvador: no inquiry verdict (SWT-79)"

const (
	chmChannelKey = "slack:TCHMUNIT:C0CHMGENERAL"
	chmDMKey      = "slack:TCHMUNIT:D0CHMDM"
	chmGroupKey   = "slack:TCHMUNIT:C0CHMMPDM" // a C… group DM: only the raw type tells
)

// chmFact reads ruleDecision.channelUnmentioned by name.
func chmFact(t *testing.T, d ruleDecision) bool {
	t.Helper()
	f := reflect.ValueOf(d).FieldByName("channelUnmentioned")
	if !f.IsValid() {
		t.Fatalf("ruleDecision has no channelUnmentioned field. SWT-79 D2/criterion 5: the fact is a WRITTEN " +
			"field of the decision (like resurface), recorded on capture_decisions.channel_unmentioned")
	}
	if f.Kind() != reflect.Bool {
		t.Fatalf("ruleDecision.channelUnmentioned is %s, want bool", f.Kind())
	}
	return f.Bool()
}

// chmRules is the rule set every case decides against, in storedRule shape:
//
//	id 1: a workspace-wide catch-all on the Slack prefix, NO external_system
//	      (rules 8/9's shape: attribution only), priority 1;
//	id 2: a gmail sender rule, attribution only;
//	id 3: a jira body_regex rule whose key_regex never matches (the keyless exit);
//	id 4: a github body_regex rule whose key is an ISSUE, not a PR (the non-PR exit);
//	id 5: a pr_review github rule (the fall-through exit).
func chmRules() ([]Rule, map[int64]storedRule) {
	jira, github := "jira", "github"
	never := `NEVERMATCHES-([0-9]+)`
	stored := []storedRule{
		{rule: Rule{ID: 1, Project: "collab", Kind: KindThreadKeyPrefix, Pattern: "slack:TCHMUNIT:", Priority: 1, Enabled: true},
			projectID: 100, notifiers: []string{"Jira"}},
		{rule: Rule{ID: 2, Project: "personal", Kind: KindSender, Pattern: "dana@example.test", Priority: 5, Enabled: true},
			projectID: 200},
		{rule: Rule{ID: 3, Project: "collab", Kind: KindBodyRegex, Pattern: `KEYLESS`, Source: &jira,
			ExternalKeyRegex: &never, Priority: 50, Enabled: true},
			projectID: 100, extSystem: "jira", notifiers: []string{"Jira"}},
		{rule: Rule{ID: 4, Project: "collab", Kind: KindBodyRegex, Pattern: `github\.com/([\w.-]+/[\w.-]+/issues/[0-9]+)`,
			Source: &github, Priority: 60, Enabled: true},
			projectID: 100, extSystem: "github", notifiers: []string{"Jira"}},
		{rule: Rule{ID: 5, Project: "collab", Kind: KindBodyRegex, Pattern: `github\.com/([\w.-]+/[\w.-]+/pull/[0-9]+)`,
			Source: &github, Priority: 70, Enabled: true},
			projectID: 100, extSystem: "github", prReview: true, notifiers: []string{"Jira"}},
	}
	return rulesForEvaluate(stored)
}

type chmCase struct {
	name                        string
	channel, key, rawType, from string
	body                        string
	wantAction                  string
	wantFact                    bool
}

func chmPM(c chmCase) pendingMessage {
	// rawItemID stays nil: prMailTrusted refuses "no stored raw row" before any
	// query, so the pr_review case falls through with no I/O.
	return pendingMessage{
		msg:         Message{ID: 1, ThreadKey: c.key, Sender: c.from, BodyText: c.body, Subject: "#general"},
		channel:     c.channel,
		rawConvType: c.rawType,
	}
}

func chmDecide(t *testing.T, c chmCase) ruleDecision {
	t.Helper()
	rules, byID := chmRules()
	d, _, err := decideMessage(context.Background(), nil, RulesModeLive, chmPM(c), rules, byID, nil)
	if err != nil {
		t.Fatalf("%s: decideMessage: %v", c.name, err)
	}
	return d
}

// Criterion 3's table, through the decision itself: true exactly for an
// attributed Slack channel message (not DM, not group DM) with no mention.
func TestSWT79_ChannelUnmentioned_WhoIsFlagged(t *testing.T) {
	cases := []chmCase{
		{name: "an attributed Slack channel message with no mention → flagged",
			channel: "slack", key: chmChannelKey, rawType: "public_channel", from: "Dana Ruiz",
			body: "morning all, the deploy is done", wantAction: "attributed", wantFact: true},
		{name: "a channel THREAD reply with no mention → flagged (D5: his literal rule)",
			channel: "slack", key: chmChannelKey + ":p1758550000000100", rawType: "public_channel", from: "Dana Ruiz",
			body: "agreed", wantAction: "attributed", wantFact: true},
		{name: "a private channel with no mention → flagged",
			channel: "slack", key: chmChannelKey, rawType: "private_channel", from: "Dana Ruiz",
			body: "ok", wantAction: "attributed", wantFact: true},
		{name: "an email ADDRESS is not a mention → flagged",
			channel: "slack", key: chmChannelKey, rawType: "public_channel", from: "Dana Ruiz",
			body: "write to x@salvador.com", wantAction: "attributed", wantFact: true},
		{name: "@here is not a mention of him → flagged",
			channel: "slack", key: chmChannelKey, rawType: "public_channel", from: "Dana Ruiz",
			body: "@here standup in 5", wantAction: "attributed", wantFact: true},

		{name: "mentioned: @Salvador → not flagged",
			channel: "slack", key: chmChannelKey, rawType: "public_channel", from: "Dana Ruiz",
			body: "@Salvador can you confirm the date?", wantAction: "attributed", wantFact: false},
		{name: "mentioned: @Salvador Spataro, in Spanish → not flagged",
			channel: "slack", key: chmChannelKey, rawType: "public_channel", from: "Dana Ruiz",
			body: "hola @Salvador Spataro, ¿puedes revisar?", wantAction: "attributed", wantFact: false},
		{name: "mentioned: @SalvadorSpataro → not flagged",
			channel: "slack", key: chmChannelKey, rawType: "public_channel", from: "Dana Ruiz",
			body: "cc @SalvadorSpataro", wantAction: "attributed", wantFact: false},

		{name: "a 1:1 DM from a notifier (attributed, not the SWT-78 DM path) → never flagged",
			channel: "slack", key: chmDMKey, rawType: "dm", from: "Jira",
			body: "Your daily digest is ready", wantAction: "attributed", wantFact: false},
		{name: "a C… group DM (raw group_dm) from a notifier → never flagged",
			channel: "slack", key: chmGroupKey, rawType: "group_dm", from: "Jira",
			body: "digest", wantAction: "attributed", wantFact: false},
		{name: "a blank-sender 1:1 DM (attributed: SWT-78 fails closed) → never flagged",
			channel: "slack", key: chmDMKey, rawType: "dm", from: "  ",
			body: "system message", wantAction: "attributed", wantFact: false},

		{name: "gmail attributed by a sender rule → never flagged (not Slack)",
			channel: "gmail", key: "gmail:acct:thread-1", rawType: "", from: "Dana <dana@example.test>",
			body: "hello, no mention here", wantAction: "attributed", wantFact: false},
		{name: "an unmatched Slack channel message → not flagged (not attributed; the CHECK forbids it)",
			channel: "slack", key: "slack:TOTHER:C0NOTHING", rawType: "public_channel", from: "Dana Ruiz",
			body: "no rule matches this", wantAction: "unmatched", wantFact: false},
	}
	for _, c := range cases {
		d := chmDecide(t, c)
		if d.action != c.wantAction {
			t.Errorf("%s: action = %q (reason %q), want %q", c.name, d.action, d.reason, c.wantAction)
			continue
		}
		if got := chmFact(t, d); got != c.wantFact {
			t.Errorf("%s: channelUnmentioned = %v, want %v (reason %q)", c.name, got, c.wantFact, d.reason)
		}
		n := strings.Count(d.reason, chmSuffix)
		switch {
		case c.wantFact && n != 1:
			t.Errorf("%s: reason %q carries the SWT-79 suffix %d time(s), want exactly 1 (criterion 4)", c.name, d.reason, n)
		case !c.wantFact && n != 0:
			t.Errorf("%s: reason %q carries the SWT-79 suffix but the fact is false; otherwise the reason is "+
				"UNCHANGED (criterion 4)", c.name, d.reason)
		}
	}
}

// Criterion 3's first-cause-wins, in the form the decision row can show: the
// same channel message flips from flagged to not-flagged on each disqualifier
// alone (mention, DM, group DM, not Slack).
func TestSWT79_ChannelUnmentioned_EachDisqualifierAlone(t *testing.T) {
	base := chmCase{name: "base", channel: "slack", key: chmChannelKey, rawType: "public_channel", from: "Jira",
		body: "build 42 finished", wantAction: "attributed"}
	if !chmFact(t, chmDecide(t, base)) {
		t.Fatalf("CONTROL: an attributed, unmentioned Slack channel message is not flagged; the cases below " +
			"would pass for the wrong reason")
	}
	offs := map[string]func(*chmCase){
		"mentioned":                 func(c *chmCase) { c.body = "build 42 finished @Salvador" },
		"1:1 DM (the key)":          func(c *chmCase) { c.key, c.rawType = chmDMKey, "dm" },
		"group DM (the raw type)":   func(c *chmCase) { c.rawType = "group_dm" },
		"not Slack (channel field)": func(c *chmCase) { c.channel = "upwork" },
	}
	for name, mod := range offs {
		c := base
		mod(&c)
		d := chmDecide(t, c)
		if d.action != "attributed" {
			t.Fatalf("%s: fixture bug, action %q (reason %q)", name, d.action, d.reason)
		}
		if chmFact(t, d) {
			t.Errorf("%s alone: still flagged (reason %q); each disqualifier alone must clear the fact", name, d.reason)
		}
	}
}

// Criterion 4 / the SWT-78 codex lesson: EVERY exit that leaves a message
// `attributed` records the fact — the catch-all's, a keyed rule that derived
// no key, and a github key that is not a PR.
func TestSWT79_ChannelUnmentioned_EveryAttributedExit(t *testing.T) {
	for _, c := range []chmCase{
		{name: "keyless jira rule on a channel message", channel: "slack", key: chmChannelKey,
			rawType: "public_channel", from: "Dana Ruiz", body: "KEYLESS status please", wantAction: "attributed", wantFact: true},
		{name: "github ISSUE key on a channel message (not a PR)", channel: "slack", key: chmChannelKey,
			rawType: "public_channel", from: "Dana Ruiz", body: "see https://github.com/acme/web/issues/7",
			wantAction: "attributed", wantFact: true},
		{name: "keyless jira rule, but mentioned", channel: "slack", key: chmChannelKey,
			rawType: "public_channel", from: "Dana Ruiz", body: "KEYLESS @Salvador?", wantAction: "attributed", wantFact: false},
	} {
		d := chmDecide(t, c)
		if d.action != c.wantAction {
			t.Errorf("%s: action = %q (reason %q), want %q", c.name, d.action, d.reason, c.wantAction)
			continue
		}
		if got := chmFact(t, d); got != c.wantFact {
			t.Errorf("%s: channelUnmentioned = %v, want %v (reason %q)", c.name, got, c.wantFact, d.reason)
		}
		if c.wantFact && strings.Count(d.reason, chmSuffix) != 1 {
			t.Errorf("%s: reason %q, want the SWT-79 suffix exactly once", c.name, d.reason)
		}
	}
}

// Criterion 4: prFallThrough re-decides by calling the decision recursively.
// The fact is applied ONCE, by the outer decideMessage, so the suffix appears
// exactly once and at the END of the reason (after the fall-through prefix and
// the fallen-to reason). MUTATION: apply it inside the recursion → 2 suffixes.
func TestSWT79_FallThroughAppliesTheFactOnce(t *testing.T) {
	c := chmCase{name: "untrusted PR link in a channel", channel: "slack", key: chmChannelKey,
		rawType: "public_channel", from: "Dana Ruiz", body: "please review https://github.com/acme/web/pull/12",
		wantAction: "attributed", wantFact: true}
	d := chmDecide(t, c)
	if !strings.Contains(d.reason, "untrusted GitHub mail") {
		t.Fatalf("CONTROL: the message did not fall through the pr_review rule (reason %q); this test is about the "+
			"recursive path", d.reason)
	}
	if d.action != "attributed" {
		t.Fatalf("fell-to action = %q (reason %q), want attributed by the catch-all", d.action, d.reason)
	}
	if !chmFact(t, d) {
		t.Errorf("the fallen-through channel message is not flagged (reason %q)", d.reason)
	}
	if n := strings.Count(d.reason, chmSuffix); n != 1 {
		t.Errorf("reason %q carries the SWT-79 suffix %d time(s), want exactly 1: the fact is applied once, after "+
			"the rule decision, never inside the path prFallThrough recurses into (criterion 4)", d.reason, n)
	}
	if !strings.HasSuffix(d.reason, chmSuffix) {
		t.Errorf("reason %q does not END with the SWT-79 suffix; the fact is applied after the whole decision", d.reason)
	}

	c.body = "@Salvador please review https://github.com/acme/web/pull/12"
	d = chmDecide(t, c)
	if chmFact(t, d) || strings.Contains(d.reason, chmSuffix) {
		t.Errorf("a MENTIONED fallen-through channel message is flagged (reason %q)", d.reason)
	}
}
