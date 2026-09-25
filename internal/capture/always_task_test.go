package capture

import (
	"strings"
	"testing"
)

// swb 703 (SWT-93): an always_task rule's message takes the direct path on any
// channel, unless the sender is blank or a notifier; a Slack DM keeps its
// SWT-78 reason. MUTATIONS: drop the alwaysTask clause -> the mail row goes
// red; let it bypass the sender checks -> the blank/notifier rows go red.
func TestAlwaysTask_DirectConversationTask(t *testing.T) {
	cases := []struct {
		name      string
		in        directInput
		want      bool
		reasonHas string
	}{
		{"mail on an always_task rule", directInput{alwaysTask: true}, true, "always-task"},
		{"mail on an ordinary keyless rule", directInput{}, false, "neither slack nor upwork"},
		{"always_task, blank sender", directInput{alwaysTask: true, blankSender: true}, false, "sender"},
		{"always_task, notifier sender", directInput{alwaysTask: true, notifier: true}, false, "notifier"},
		{"always_task never reaches a Slack channel: SWT-78/79 keep Slack's policy",
			directInput{alwaysTask: true, slack: true}, false, "channel"},
		{"a Slack DM on an always_task rule keeps SWT-78's reason", directInput{alwaysTask: true, slack: true, dm: true},
			true, "swt-78"},
	}
	for _, tc := range cases {
		got, why := directConversationTask(tc.in)
		if got != tc.want || !strings.Contains(strings.ToLower(why), tc.reasonHas) {
			t.Errorf("%s: directConversationTask = %v (%q), want %v with a reason naming %q", tc.name, got, why,
				tc.want, tc.reasonHas)
		}
	}
}

func TestAlwaysTask_MarkerAndConversationKeyForMail(t *testing.T) {
	mail := pendingMessage{channel: "gmail"}
	mail.msg.ThreadKey = "gmail:me@example.test:18c2a:b"
	if got := directMarker(mail); got != alwaysTaskBodyMarker {
		t.Errorf("mail marker = %q, want the always-task marker", got)
	}
	if conv, ok := directConversationKey(mail); !ok || conv != mail.msg.ThreadKey {
		t.Errorf("mail conversation key = %q (%v), want the thread key used whole", conv, ok)
	}
	dm := pendingMessage{channel: "slack"}
	dm.msg.ThreadKey = "slack:T0360B84U:DSAV4HJ2F"
	if got := directMarker(dm); got != directTaskBodyMarker {
		t.Errorf("DM marker = %q, want SWT-78's", got)
	}
}
