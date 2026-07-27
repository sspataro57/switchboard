package slackweb_test

import (
	"testing"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
)

func TestParseTargetURL(t *testing.T) {
	for _, tc := range []struct {
		name      string
		value     string
		messageID string
		valid     bool
	}{
		{"channel", "https://app.slack.com/client/T123/C456", "", true},
		{"thread", "https://app.slack.com/client/T123/C456/p1710000000123456", "p1710000000123456", true},
		{"dm", "https://app.slack.com/client/T123/D456", "", true},
		{"wrong host", "https://evil.example/client/T123/C456", "", false},
		{"credentials", "https://user@app.slack.com/client/T123/C456", "", false},
		{"query", "https://app.slack.com/client/T123/C456?token=no", "", false},
		{"extra path", "https://app.slack.com/client/T123/C456/p1/extra", "", false},
		{"bad ids", "https://app.slack.com/client/team/channel", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, err := slackweb.ParseTargetURL(tc.value)
			if tc.valid && err != nil {
				t.Fatalf("ParseTargetURL: %v", err)
			}
			if !tc.valid && err == nil {
				t.Fatalf("ParseTargetURL(%q) unexpectedly succeeded", tc.value)
			}
			if tc.valid && target.MessageID != tc.messageID {
				t.Errorf("message id = %q, want %q", target.MessageID, tc.messageID)
			}
		})
	}
}

func TestTargetCanonicalURL(t *testing.T) {
	// ParseTargetURL accepts spellings the loop-closure matcher never will, so
	// draft_delivery stores CanonicalURL rather than the caller's string. A
	// regression here yields deliveries that can never be confirmed, silently.
	cases := []struct {
		name, input, want string
	}{
		{"channel", "https://app.slack.com/client/T0360B84U/C123",
			"https://app.slack.com/client/T0360B84U/C123"},
		{"trailing slash collapses", "https://app.slack.com/client/T0360B84U/C123/",
			"https://app.slack.com/client/T0360B84U/C123"},
		{"thread keeps message id", "https://app.slack.com/client/T0360B84U/C123/p1700000000000100",
			"https://app.slack.com/client/T0360B84U/C123/p1700000000000100"},
		{"thread trailing slash", "https://app.slack.com/client/T0360B84U/C123/p1700000000000100/",
			"https://app.slack.com/client/T0360B84U/C123/p1700000000000100"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, err := slackweb.ParseTargetURL(tc.input)
			if err != nil {
				t.Fatalf("slackweb.ParseTargetURL(%q) = %v", tc.input, err)
			}
			if got := target.CanonicalURL(); got != tc.want {
				t.Fatalf("CanonicalURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTargetCanonicalURLRoundTrips(t *testing.T) {
	// Canonical output must itself parse back to the same target, or storing it
	// would break the prefill path that re-parses target_ref.
	for _, raw := range []string{
		"https://app.slack.com/client/T1/C1/",
		"https://app.slack.com/client/T1/D1/p1700000000000100",
	} {
		first, err := slackweb.ParseTargetURL(raw)
		if err != nil {
			t.Fatalf("slackweb.ParseTargetURL(%q) = %v", raw, err)
		}
		second, err := slackweb.ParseTargetURL(first.CanonicalURL())
		if err != nil {
			t.Fatalf("slackweb.ParseTargetURL(canonical of %q) = %v", raw, err)
		}
		if first != second {
			t.Fatalf("round trip changed target: %+v -> %+v", first, second)
		}
	}
}
