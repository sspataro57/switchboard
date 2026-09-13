package slackweb_test

// SWT-40 Part C, criterion C5 and C-D4 (docs/tickets/inquiry-promote_SPEC.md):
// ONE spelling of "is this Slack thread key a 1:1 DIRECT MESSAGE", beside
// channelThreadKey (which builds the key) and IsRootedThreadKey (which reads
// the other half of its shape).
//
// Plain unit test: no build tag, no database, no browser.
//
// THE RULE (C-D4): the CONVERSATION segment — the third colon-separated field
// of slack:{ws}:{conv}[:{root}] — starts with an upper-case `D`. That is the
// leaf's own DM fallback (slackconnector src/slack/channels.ts:20) and the
// shape production stores (slack:T0360B84U:D01EJRX6P45). Group DMs (mpim,
// legacy `G…` ids) are EXCLUDED: C-D3's addressed-ness rule promotes a 1:1 DM
// because the only person it can be addressed to is Salvador; a group DM is a
// small channel and falls back to the thread rule.
//
// C-D4's go-live gate is §V4a, a PRODUCTION read: every slack thread's
// raw conversation.type='dm' must agree with this helper. That is the main
// thread's job; nothing here freezes a production count.
//
// ---- IMPOSED SURFACE ---------------------------------------------------------
//
//	// IsDirectMessageKey reports whether a slack thread key names a 1:1 DM
//	// conversation (rooted or not). Segment COUNT and position decide, as
//	// IsRootedThreadKey does; case-sensitive (the stored key keeps the
//	// exported case).
//	func IsDirectMessageKey(threadKey string) bool
//
// GREENFIELD NOTE — EXPECTED RED: slackweb.IsDirectMessageKey does not exist,
// so this file compile-FAILS internal/connector/slackweb's test build with
// "undefined: slackweb.IsDirectMessageKey".
//
// MUTATIONS (V3): read the WORKSPACE segment instead of the conversation one
// (the `slack:D…:C…` case goes red); accept `G` (the group-DM case goes red);
// lower-case the comparison (the `d…` case goes red).

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
)

func TestIsDirectMessageKey(t *testing.T) {
	cases := []struct {
		key string
		dm  bool
		why string
	}{
		{"slack:T0360B84U:D01EJRX6P45", true,
			"the production DM shape (What exists today): an unthreaded 1:1 DM — C-D3 addresses it to Salvador"},
		{"slack:T0HPR78RX:D07PRIVATE:p1757000000000100", true,
			"a threaded reply INSIDE a DM: the conversation segment is still `D…`, and the thread root does " +
				"not make the conversation any less 1:1"},
		{"slack:T0HPR78RX:C07ABCDEF", false,
			"a top-level channel message: C-D3's accepted recall cost — a channel ask promotes only through " +
				"the thread rule"},
		{"slack:T0HPR78RX:C07ABCDEF:p1757000000000100", false,
			"a channel thread: rooted, but not a DM (the thread rule decides it, from prior participation)"},
		{"slack:T0HPR78RX:G01GROUPDM", false,
			"a GROUP DM (legacy mpim id). C-D4 excludes group DMs by name: several people can be addressed"},
		{"slack:T0HPR78RX:G01GROUPDM:p1757000000000100", false, "a thread inside a group DM"},
		{"slack:D0WORKSPC:C07ABCDEF", false,
			"a workspace id that happens to start with D: the rule reads the CONVERSATION segment, never the " +
				"workspace one (mutation: read parts[1])"},
		{"slack:T0HPR78RX:d01lower", false,
			"lower-case `d`: the key keeps Slack's exported case, and the rule is case-SENSITIVE like every " +
				"thread_key_prefix comparison in capture"},
		{"slack:T0HPR78RX:", false, "an empty conversation segment is not a conversation"},
		{"slack::D01EJRX6P45", false, "an empty workspace segment is malformed, not a DM"},
		{"slack:T0HPR78RX:D01EJRX6P45:p1:extra", false,
			"five segments is not a shape the normalizer builds (no Slack id contains a colon)"},
		{"", false, "the empty key: a message with no thread is never a DM"},
		{"gmail:thread-D123", false, "a non-slack key is never a Slack DM"},
		{"jira:sspataro.atlassian.net:D-12", false, "a jira key whose third segment starts with D is not a DM"},
		{"upwork:room:D123", false, "an upwork key is not a Slack DM"},
	}
	for _, tc := range cases {
		if got := slackweb.IsDirectMessageKey(tc.key); got != tc.dm {
			t.Errorf("IsDirectMessageKey(%q) = %v, want %v — %s", tc.key, got, tc.dm, tc.why)
		}
	}
}

// The helper must agree with the key the REAL normalizer builds, for a DM and a
// channel, threaded and not (SWT-18's lesson: verify a data path against the
// data, not only against the strings a test made up).
func TestIsDirectMessageKey_AgreesWithTheKeysTheNormalizerBuilds(t *testing.T) {
	const tmpl = `{"kind":"message",
	  "workspace":{"id":"T0HPR78RX","own_user_id":"U0OWNSELF"},
	  "conversation":{"id":"%CONV%","name":"%NAME%","type":"%TYPE%","url":"https://app.slack.com/client/T0HPR78RX/%CONV%"},
	  "message":{"id":"p1757000000000900","timestamp":"2026-09-09T12:00:00Z","author":"Dana Ruiz",
	             "author_id":"U0DANARUIZ","text":"can you confirm the rotation date?"%ROOT%}}`
	build := func(conv, name, typ, root string) string {
		raw := strings.NewReplacer("%CONV%", conv, "%NAME%", name, "%TYPE%", typ, "%ROOT%", root).Replace(tmpl)
		_, msg, err := slackweb.NormalizeMessage([]byte(raw))
		if err != nil {
			t.Fatalf("NormalizeMessage(conv=%s root=%q): %v", conv, root, err)
		}
		return msg.ThreadKey
	}
	const root = `,"thread_root_id":"p1757000000000100"`

	for _, tc := range []struct {
		conv, name, typ, root string
		dm                    bool
	}{
		{"D07PRIVATE", "Dana Ruiz", "dm", "", true},
		{"D07PRIVATE", "Dana Ruiz", "dm", root, true},
		{"C07ABCDEF", "llamasite-eng", "channel", "", false},
		{"C07ABCDEF", "llamasite-eng", "channel", root, false},
		{"G07GROUPDM", "mpdm-dana--salvador--esteban-1", "mpim", "", false},
	} {
		key := build(tc.conv, tc.name, tc.typ, tc.root)
		if got := slackweb.IsDirectMessageKey(key); got != tc.dm {
			t.Errorf("the normalizer built %q for a %s conversation (rooted=%v) and IsDirectMessageKey says %v, "+
				"want %v. The builder and this reader are one fact read in two directions (C-D4); §V4a checks "+
				"the same agreement against every production slack thread", key, tc.typ, tc.root != "", got, tc.dm)
		}
	}
}

// ONE spelling (C5): exactly one exported DM helper lives in this package, in
// normalize.go beside channelThreadKey. A second function answering "is this a
// DM" is how the builder and a reader come to disagree about a format neither
// owns.
func TestDirectMessageRuleHasOneExportedSpelling(t *testing.T) {
	src, err := os.ReadFile("normalize.go")
	if err != nil {
		t.Fatalf("read normalize.go: %v", err)
	}
	if !strings.Contains(string(src), "func channelThreadKey(") {
		t.Fatalf("POSITIVE CONTROL FAILED: normalize.go no longer declares channelThreadKey; the DM helper is " +
			"defined as living beside it")
	}
	if !strings.Contains(string(src), "func IsDirectMessageKey(") {
		t.Errorf("normalize.go does not declare IsDirectMessageKey. C-D4: the DM rule lives beside " +
			"channelThreadKey (the builder) and IsRootedThreadKey (the other reader), not in promote or replyfold")
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	decl := regexp.MustCompile(`func\s+(Is\w*(?:Direct|DM|Dm)\w*|\w*DirectMessage\w*)\s*\(`)
	found := map[string]string{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		for _, m := range decl.FindAllStringSubmatch(string(b), -1) {
			found[m[1]] = n
		}
	}
	if len(found) != 1 {
		t.Errorf("internal/connector/slackweb declares %d DM-key helpers (%v), want exactly 1 "+
			"(IsDirectMessageKey, C5)", len(found), found)
	}
}
