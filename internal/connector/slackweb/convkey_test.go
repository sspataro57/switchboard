package slackweb_test

import (
	"testing"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
)

// SWT-78 decision 4: a rooted thread folds into its conversation, so the
// conversation key is the unrooted form for both shapes, and nothing else parses.
func TestConversationThreadKey(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"slack:T0360B84U:DSAV4HJ2F", "slack:T0360B84U:DSAV4HJ2F", true},
		{"slack:T0360B84U:DSAV4HJ2F:p1758550000000100", "slack:T0360B84U:DSAV4HJ2F", true},
		{"slack:T0HPR78RX:C0BPR9FUCLE", "slack:T0HPR78RX:C0BPR9FUCLE", true},
		{"slack:T0360B84U", "", false},
		{"slack:T0360B84U:D1:p1:extra", "", false},
		{"slack::DSAV4HJ2F", "", false},
		{"slack:T0360B84U:DSAV4HJ2F:", "", false},
		{"gmail:thread-1:x", "", false},
		{"", "", false},
	} {
		got, ok := slackweb.ConversationThreadKey(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ConversationThreadKey(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
