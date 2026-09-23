package capture

// TestRegression_SlackMessagesNotBecomingTasks — bug slack-messages-not-becoming-tasks
// (Jira SWT-78, swb #521; docs/bugs/slack-messages-not-becoming-tasks_DIAGNOSIS.md,
// "Proposed fix scope" items A, B and G.1). The PURE half: whether a message at
// capture's attribution-only exit is a DM from a person, and therefore takes the
// DM conversation-task path instead of the inquiry lane (qwen). ZERO I/O.
//
// Salvador, 2026-09-23: "basically all DMs to me are actionable just messages
// on the general forum need decision" / "fix it so DMs skip qwen" / "one task
// per conversation". Decision 3: group DMs are included, read from the raw
// conversation.type='group_dm' (NOT from the id prefix: 40 of 53 production
// group DMs have C… ids). Decision 7: the per-project notifier list is the bot
// exclusion (a Jira app's author id is an ordinary U… id).
//
// ---- IMPOSED SURFACE (the comm.go / resurface.go shape) -----------------------
//
//	internal/capture/direct.go (new, PURE: no context, no pgx, no connector
//	import, no environment, no clock):
//
//	  type directInput struct {
//	      slack       bool // pm.channel == slackweb.Channel, computed by the caller
//	      dm          bool // slackweb.IsDirectMessageKey(thread key), computed by the caller
//	      groupDM     bool // raw conversation.type == 'group_dm', read from the COLUMN
//	                       // (pendingMessageCols) by the caller
//	      blankSender bool // blankSender(sender)                      — resurface.go's spelling
//	      notifier    bool // notifierSender(sender, winner.notifiers) — resurface.go's spelling
//	  }
//
//	  // directConversationTask is true iff slack AND (dm OR groupDM) AND NOT
//	  // blankSender AND NOT notifier. The string is the reason fragment the
//	  // decision row records; every false outcome names its own cause.
//	  func directConversationTask(in directInput) (bool, string)
//
// There is deliberately NO direction/outbound field: capture never decides an
// outbound message (pendingMessages reads direction='inbound' only, invariant
// 5), so "outbound is never an input" is enforced by the struct's shape here
// and by the integration suite's outbound fixture.
//
// EXPECTED RED (greenfield): direct.go, directInput and directConversationTask
// do not exist, so this file does not compile — and, the resurface_test.go /
// comm_test.go precedent, neither does internal/capture's test binary until
// direct.go exists. The compile errors are confined to this file.

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
)

// dtIn builds the input the way rules_store.go will: from a thread key, the raw
// conversation type, the channel, the sender and the project's notifier list,
// through the ONE spelling of each fact. The test reads real keys, so the G…
// and C… group-DM rows prove IsDirectMessageKey does NOT carry them — the raw
// type does.
func dtIn(channel, threadKey, rawType, sender string, notifiers []string) directInput {
	return directInput{
		slack:       channel == slackweb.Channel,
		dm:          slackweb.IsDirectMessageKey(threadKey),
		groupDM:     rawType == "group_dm",
		blankSender: blankSender(sender),
		notifier:    notifierSender(sender, notifiers),
	}
}

func TestRegression_SWT78_DirectConversationTask_WhoTakesTheDMPath(t *testing.T) {
	jira := []string{"Jira"}
	cases := []struct {
		name                          string
		channel, key, rawType, sender string
		notifiers                     []string
		want                          bool
		reasonHas                     string // a word the false reason must carry ("" = any non-empty reason)
	}{
		{"a person's 1:1 DM (José, DSAV4HJ2F shape)", "slack", "slack:T0360B84U:DSAV4HJ2F", "dm", "Jose Garcia", jira, true, ""},
		{"a person's message in a ROOTED thread inside a 1:1 DM (decision 4 folds it)", "slack",
			"slack:T0HPR78RX:D04F7LXRB8B:p1758550000000100", "dm", "Katie", jira, true, ""},
		{"a legacy G… group DM, typed group_dm in raw", "slack", "slack:T0HPR78RX:G01GROUPDM", "group_dm", "Dana Ruiz", jira, true, ""},
		{"a C… group DM typed group_dm in raw (40 of 53 prod mpdms; decision 3)", "slack",
			"slack:T0HPR78RX:C0BPR9FUCLE", "group_dm", "Dana Ruiz", jira, true, ""},
		{"a public channel with a C… id", "slack", "slack:T0HPR78RX:C03J2KTN1PD", "public_channel", "Dana Ruiz", jira, false, ""},
		{"a thread inside a public channel", "slack", "slack:T0HPR78RX:C03J2KTN1PD:p1758550000000100", "public_channel",
			"Dana Ruiz", jira, false, ""},
		{"the Jira app's DM (D01EJRX6P45; author id is an ordinary U…, the notifier list tells)", "slack",
			"slack:T0HPR78RX:D01EJRX6P45", "dm", "Jira", jira, false, "notifier"},
		{"the Jira app, case-folded equality", "slack", "slack:T0HPR78RX:D01EJRX6P45", "dm", "  jira ", jira, false, "notifier"},
		{"a blank sender fails CLOSED (no identity: nobody to tell a bot from a person)", "slack",
			"slack:T0360B84U:DSAV4HJ2F", "dm", "   ", jira, false, "sender"},
		{"a group DM from the Jira app", "slack", "slack:T0HPR78RX:C0BPR9FUCLE", "group_dm", "Jira", jira, false, "notifier"},
		{"a human whose name merely CONTAINS a notifier entry is still a person (equality, not substring)", "slack",
			"slack:T0360B84U:DSAV4HJ2F", "dm", "Jiraiya Smith", jira, true, ""},
		{"not slack: a gmail thread is never a Slack DM", "gmail", "gmail:thread-D123", "", "Dana <d@x.test>", jira, false, ""},
		{"not slack even with a group_dm-looking raw type", "upwork", "upwork:room:D123", "group_dm", "Dana", jira, false, ""},
	}
	for _, tc := range cases {
		got, why := directConversationTask(dtIn(tc.channel, tc.key, tc.rawType, tc.sender, tc.notifiers))
		if got != tc.want {
			t.Errorf("%s: directConversationTask = %v (%q), want %v", tc.name, got, why, tc.want)
			continue
		}
		if strings.TrimSpace(why) == "" {
			t.Errorf("%s: empty reason; every outcome names its cause on the decision row", tc.name)
		}
		if !tc.want && !strings.Contains(strings.ToLower(why), tc.reasonHas) {
			t.Errorf("%s: reason %q does not name its own cause (%q)", tc.name, why, tc.reasonHas)
		}
	}
}

// Every false outcome names a DIFFERENT cause, so a smoke read of
// capture_decisions.reason tells them apart (the resurfaces/commTask rule).
func TestRegression_SWT78_DirectConversationTask_DistinctReasons(t *testing.T) {
	base := directInput{slack: true, dm: true}
	if ok, _ := directConversationTask(base); !ok {
		t.Fatalf("CONTROL: a slack 1:1 DM from a named non-notifier sender does not apply")
	}
	offs := map[string]directInput{
		"not slack":    {slack: false, dm: true},
		"not a DM":     {slack: true},
		"blank sender": {slack: true, dm: true, blankSender: true},
		"notifier":     {slack: true, dm: true, notifier: true},
	}
	seen := map[string]string{}
	for name, in := range offs {
		ok, why := directConversationTask(in)
		if ok {
			t.Errorf("%s: applies; want not", name)
		}
		if prev, dup := seen[why]; dup {
			t.Errorf("%s and %s share the reason %q; each disqualifier names its own cause", name, prev, why)
		}
		seen[why] = name
	}
}

// The full truth table: true in exactly the rows slack AND (dm OR groupDM) AND
// NOT blankSender AND NOT notifier. MUTATIONS: drop the notifier clause → the
// Jira rows go red; drop groupDM → the group-DM rows go red; accept channel
// (drop the dm/groupDM requirement) → the channel rows go red.
func TestRegression_SWT78_DirectConversationTask_TruthTable(t *testing.T) {
	bools := []bool{false, true}
	for _, slack := range bools {
		for _, dm := range bools {
			for _, group := range bools {
				for _, blank := range bools {
					for _, notifier := range bools {
						in := directInput{slack: slack, dm: dm, groupDM: group, blankSender: blank, notifier: notifier}
						want := slack && (dm || group) && !blank && !notifier
						if got, why := directConversationTask(in); got != want {
							t.Errorf("directConversationTask(%+v) = %v (%q), want %v", in, got, why, want)
						}
					}
				}
			}
		}
	}
}

// "Outbound is never an input": the predicate cannot be handed a direction.
// Capture's pending query reads inbound only (invariant 5), and a field here
// would invite a caller to think the predicate is what keeps his own messages
// out. The integration suite's outbound fixture proves the behaviour.
func TestRegression_SWT78_DirectInputHasNoDirection(t *testing.T) {
	rt := reflect.TypeOf(directInput{})
	for i := 0; i < rt.NumField(); i++ {
		n := strings.ToLower(rt.Field(i).Name)
		if strings.Contains(n, "direction") || strings.Contains(n, "outbound") || strings.Contains(n, "inbound") {
			t.Errorf("directInput has field %q; direction is never an input (capture decides inbound only)", rt.Field(i).Name)
		}
	}
}
