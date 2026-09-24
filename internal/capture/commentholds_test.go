package capture

// SWT-82: commentHolds' truth table. ZERO I/O.

import (
	"strings"
	"testing"
)

func TestCommentHolds_TruthTable(t *testing.T) {
	const comment, issue = "jira:x.jira.com:comment:10042", "jira:x.jira.com:issue:API-1"
	person := commentHoldInput{activity: true, status: "ready", connectorCopy: true, externalID: comment}
	with := func(f func(*commentHoldInput)) commentHoldInput { in := person; f(&in); return in }

	for _, tc := range []struct {
		name string
		in   commentHoldInput
		want bool
		why  string // substring of the reason; "" = the reason must be empty
	}{
		{"a person's comment on a ready task", person, true, "SWT-82"},
		{"on a delivered task", with(func(i *commentHoldInput) { i.status = "delivered" }), true, "delivered"},
		{"on a done_locally task", with(func(i *commentHoldInput) { i.status = "done_locally" }), true, "done_locally"},
		{"not an activity rule", with(func(i *commentHoldInput) { i.activity = false }), false, ""},
		{"closed: J9's revive owns it", with(func(i *commentHoldInput) { i.status = "closed" }), false, ""},
		{"claimed", with(func(i *commentHoldInput) { i.status = "claimed" }), false, "active work"},
		{"in_progress", with(func(i *commentHoldInput) { i.status = "in_progress" }), false, "active work"},
		{"needs_feedback", with(func(i *commentHoldInput) { i.status = "needs_feedback" }), false, "active work"},
		{"a notification email (J10)", with(func(i *commentHoldInput) { i.connectorCopy = false; i.externalID = "<m@x>" }), false, ""},
		{"the description copy", with(func(i *commentHoldInput) { i.externalID = issue }), false, ""},
		{"blank sender", with(func(i *commentHoldInput) { i.blankSender = true }), false, "no sender"},
		{"notifier", with(func(i *commentHoldInput) { i.notifier = true }), false, "notifier"},
		{"closed outranks active-work wording", with(func(i *commentHoldInput) { i.status = "closed"; i.notifier = true }), false, ""},
	} {
		got, why := commentHolds(tc.in)
		if got != tc.want {
			t.Errorf("%s: commentHolds = %v, want %v (%q)", tc.name, got, tc.want, why)
		}
		if tc.why == "" && why != "" || tc.why != "" && !strings.Contains(why, tc.why) {
			t.Errorf("%s: reason %q, want it to contain %q", tc.name, why, tc.why)
		}
	}
}
