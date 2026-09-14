package github_test

// SWT-54 (docs/tickets/treetop-pr-review-tasks_SPEC.md): the pure half of the
// GitHub PR vocabulary capture now leans on. ZERO I/O.
//
//   - D2 point 3 / criterion 6: ONE spelling of a PR key, `{owner}/{repo}#{N}`,
//     reached from either the thread-root path form `{owner}/{repo}/pull/{N}`
//     (what the seeded key_regex captures) or the key itself. Two spellings of
//     one PR under system='github' would let one PR hold two tasks, silently —
//     external_refs' UNIQUE (system, external_key) cannot see through spellings
//     (IK: SWT-13/18/19, the landmine's five instances).
//   - PRURL / PRRootMessageID: the ref's URL (url_template is refused for github
//     because the canonical key contains '#') and the Message-ID D1's opening
//     lookup compares by exact equality.
//   - D5 / criterion 14: PRStateNotice, pinned with Step 0d's real first lines
//     ("Closed #3145.", "Closed #3186.", "Closed #3854.") and GitHub's documented
//     "Merged #N into <branch>.". First line only: 0d found merged prose such as
//     "merged in #3202" inside PR descriptions, and that must NOT close anything.
//   - D1's header half: the X-GitHub-* header names and NotificationFacts.
//
// GREENFIELD NOTE — EXPECTED RED: internal/connector/github/prref.go does not
// exist, so this file compile-FAILS the package's tests.
//
// IMPOSED SURFACE (the SPEC names the functions; the Go shapes are chosen here):
//
//	func ParsePRRef(s string) (PRRef, bool)               // "{owner}/{repo}/pull/{N}" | "{owner}/{repo}#{N}"; HeadBranch ""
//	func PRKey(ref PRRef) string                          // "{owner}/{repo}#{N}"
//	func PRURL(ref PRRef) string                          // "https://github.com/{owner}/{repo}/pull/{N}"
//	func PRRootMessageID(ref PRRef) string                // "<{owner}/{repo}/pull/{N}@github.com>"
//	func PRStateNotice(body string, n int) (string, bool) // PRStateMerged | PRStateClosed
//	const PRStateMerged = "merged"; const PRStateClosed = "closed"
//	const HeaderReason = "X-GitHub-Reason"; HeaderSender = "X-GitHub-Sender"; HeaderRecipient = "X-GitHub-Recipient"
//	type Notification struct { Reason, Sender, Recipient string }
//	func NotificationFacts(h mail.Header) Notification
//
// Two readings chosen here, flagged so they are decided rather than discovered:
//  1. PRStateNotice reads the first NON-EMPTY line of the body, because that is
//     what pre-check 0d measured ("the first non-empty body_text line").
//  2. PR number 0 is refused: GitHub numbers from 1, and a zero is what a
//     failed Atoi leaves behind.

import (
	"net/mail"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/connector/github"
)

// ---- D2 point 3: parse both spellings -------------------------------------------

func TestParsePRRef_AcceptsThePathAndTheKeySpelling(t *testing.T) {
	cases := []struct {
		in   string
		repo string
		n    int
	}{
		// The path form is exactly what the seeded key_regex's first group captures
		// out of `gmail:{acct}:<treetopllc/collaboratory-www/pull/3179@github.com>`.
		{"treetopllc/collaboratory-www/pull/3179", "treetopllc/collaboratory-www", 3179},
		{"treetopllc/collaboratory-www#3179", "treetopllc/collaboratory-www", 3179},
		{"treetopllc/gonoble/pull/12", "treetopllc/gonoble", 12},
		// Step 0b's undetermined roots: another treetopllc repo.
		{"treetopllc/noble-go-sdk/pull/486", "treetopllc/noble-go-sdk", 486},
		{"treetopllc/noble-go-sdk#486", "treetopllc/noble-go-sdk", 486},
		// The key_regex's repo class is [A-Za-z0-9._-]+.
		{"treetopllc/web.app_v2/pull/7", "treetopllc/web.app_v2", 7},
	}
	for _, c := range cases {
		ref, ok := github.ParsePRRef(c.in)
		if !ok {
			t.Errorf("ParsePRRef(%q) refused a PR reference; D2: both the thread-root path form and the connector's "+
				"key form must parse, or a github-keyed decision has no canonical key", c.in)
			continue
		}
		if ref.Repo != c.repo || ref.PR != c.n || ref.HeadBranch != "" {
			t.Errorf("ParsePRRef(%q) = %+v, want {Repo:%q PR:%d HeadBranch:\"\"}", c.in, ref, c.repo, c.n)
		}
	}
}

// Issues, actions, commits and anything not exactly a PR derive NO ref: "a
// github key that does not parse gives attribution only, never a ref" (D2).
func TestParsePRRef_RefusesEverythingThatIsNotAPR(t *testing.T) {
	for _, in := range []string{
		"",
		"treetopllc/collaboratory-www/issues/214",
		"treetopllc/collaboratory-www/actions/runs/9912",
		"treetopllc/collaboratory-www/commit/0a1b2c3",
		"treetopllc/collaboratory-www/pull/0",
		"treetopllc/collaboratory-www#0",
		"treetopllc/collaboratory-www/pull/-3",
		"treetopllc/collaboratory-www/pull/",
		"treetopllc/collaboratory-www#",
		"treetopllc/collaboratory-www#abc",
		"treetopllc/collaboratory-www/pull/3179/files",
		"collaboratory-www#3179",
		"/collaboratory-www#3179",
		"treetopllc/#3179",
		"a/b/c#1",
		// The Message-ID itself is not a key: the key_regex strips the brackets and
		// @github.com, and a parser that also accepted them would be a second
		// spelling of the same input.
		"<treetopllc/collaboratory-www/pull/3179@github.com>",
		"treetopllc/collaboratory-www/pull/3179@github.com",
	} {
		if ref, ok := github.ParsePRRef(in); ok {
			t.Errorf("ParsePRRef(%q) = %+v, accepted; want refused (not a PR reference in either spelling)", in, ref)
		}
	}
}

// ---- criterion 6: ONE spelling, and the round trips that keep it one ------------

func TestPRKey_IsTheConnectorSpellingAndEverySpellingRoundTripsToIt(t *testing.T) {
	ref := github.PRRef{Repo: "treetopllc/collaboratory-www", PR: 3179}
	const wantKey = "treetopllc/collaboratory-www#3179"

	if got := github.PRKey(ref); got != wantKey {
		t.Fatalf("PRKey(%+v) = %q, want %q — the spelling PGTaskResolver.Resolve and orchestrator R9-R11 already "+
			"use (store.go's fmt.Sprintf(\"%%s#%%d\", ...))", ref, got, wantKey)
	}
	if got := github.PRURL(ref); got != "https://github.com/treetopllc/collaboratory-www/pull/3179" {
		t.Errorf("PRURL = %q, want https://github.com/treetopllc/collaboratory-www/pull/3179", got)
	}
	if got := github.PRRootMessageID(ref); got != "<treetopllc/collaboratory-www/pull/3179@github.com>" {
		t.Errorf("PRRootMessageID = %q, want <treetopllc/collaboratory-www/pull/3179@github.com> — GitHub's "+
			"PR-opened Message-ID, which D1's opening lookup compares by exact equality", got)
	}

	// Both input spellings collapse to one key.
	for _, in := range []string{"treetopllc/collaboratory-www/pull/3179", wantKey} {
		r, ok := github.ParsePRRef(in)
		if !ok || github.PRKey(r) != wantKey {
			t.Errorf("PRKey(ParsePRRef(%q)) = %q (ok=%v), want %q: two spellings of one PR must be one key",
				in, github.PRKey(r), ok, wantKey)
		}
	}

	// The Message-ID round trip: what the key_regex captures out of the root id
	// (brackets and @github.com stripped) parses back to the SAME ref. If
	// PRRootMessageID and ParsePRRef ever disagree, the opening lookup and the key
	// name different PRs.
	root := github.PRRootMessageID(ref)
	path := strings.TrimSuffix(strings.TrimPrefix(root, "<"), "@github.com>")
	back, ok := github.ParsePRRef(path)
	if !ok || back.Repo != ref.Repo || back.PR != ref.PR {
		t.Errorf("ParsePRRef(%q) (from PRRootMessageID) = %+v ok=%v, want %+v", path, back, ok, ref)
	}
}

// ---- D5 / criterion 14: the merge/close notice ----------------------------------

func TestPRStateNotice_FirstLineShapes(t *testing.T) {
	const footer = "\n\n—\nReply to this email directly, view it on GitHub:\n" +
		"https://github.com/treetopllc/collaboratory-www/pull/3186#event-11223344\n" +
		"You are receiving this because you are subscribed to this thread."
	cases := []struct {
		name  string
		body  string
		n     int
		state string
		ok    bool
	}{
		// Step 0d, observed on prod (the only three notice lines in 90 days).
		{"0d observed: Closed #3145.", "Closed #3145.", 3145, github.PRStateClosed, true},
		{"0d observed: Closed #3186. with GitHub's footer", "Closed #3186." + footer, 3186, github.PRStateClosed, true},
		{"0d observed: Closed #3854. CRLF", "Closed #3854.\r\n", 3854, github.PRStateClosed, true},
		// GitHub's documented shape; none arrived in the 0d window.
		{"documented: Merged #N into main.", "Merged #3179 into main.", 3179, github.PRStateMerged, true},
		{"documented: a branch with a slash", "Merged #3179 into release/2026-09." + footer, 3179, github.PRStateMerged, true},
		{"0d measured the first NON-EMPTY line", "\n\nClosed #3145.\n", 3145, github.PRStateClosed, true},

		{"a different N does not close", "Closed #3145.", 3146, "", false},
		{"a longer number sharing the prefix", "Closed #31450.", 3145, "", false},
		{"Merged, different N", "Merged #3180 into main.", 3179, "", false},
		{"Reopened is SWT-53's, never a close", "Reopened #3145.", 3145, "", false},
		// 0d: merged prose inside a PR description must NOT match.
		{"0d: 'merged in #3202' prose", "This builds on the fix merged in #3202 last week.", 3202, "", false},
		{"prose at line start is not the notice", "Merged in #3202 was the old fix; this replaces it.", 3202, "", false},
		{"a notice mid-body is not a notice", "Thanks for the review!\nMerged #3179 into main.", 3179, "", false},
		{"Closed mid-body", "LGTM\n\nClosed #3145.", 3145, "", false},
		{"a quoted notice", "> Closed #3145.", 3145, "", false},
		{"a closing keyword in a description", "Closes #3145.", 3145, "", false},
		{"Fixes", "Fixes #3145.", 3145, "", false},
		{"empty body", "", 3145, "", false},
	}
	for _, c := range cases {
		state, ok := github.PRStateNotice(c.body, c.n)
		if ok != c.ok || state != c.state {
			t.Errorf("%s: PRStateNotice(%q, %d) = (%q, %v), want (%q, %v)", c.name, c.body, c.n, state, ok, c.state, c.ok)
		}
	}
	if github.PRStateMerged != "merged" || github.PRStateClosed != "closed" {
		t.Errorf("PRStateMerged/PRStateClosed = %q/%q, want merged/closed — the close reason reads "+
			"\"PR #N merged on GitHub\" / \"PR #N closed on GitHub\" (OQ-2 = a)", github.PRStateMerged, github.PRStateClosed)
	}
}

// ---- D1: the header names, and reading them from a parsed header -----------------

func TestNotificationFacts_ReadsTheThreeGitHubHeaders(t *testing.T) {
	if github.HeaderReason != "X-GitHub-Reason" || github.HeaderSender != "X-GitHub-Sender" ||
		github.HeaderRecipient != "X-GitHub-Recipient" {
		t.Errorf("header consts = %q/%q/%q, want X-GitHub-Reason/X-GitHub-Sender/X-GitHub-Recipient",
			github.HeaderReason, github.HeaderSender, github.HeaderRecipient)
	}

	// A header parsed by net/mail is keyed CANONICALLY ("X-Github-Reason"), so a
	// reader that indexes the map with the spelled "X-GitHub-Reason" finds
	// nothing, silently, and every PR reads as undetermined. Parse a real header
	// block rather than building the map by hand, or the test cannot see that.
	raw := "From: joseg-avviato <notifications@github.com>\r\n" +
		"To: treetopllc/collaboratory-www <collaboratory-www@noreply.github.com>\r\n" +
		"Subject: Re: [treetopllc/collaboratory-www] Ranking widget (PR #3179)\r\n" +
		"Message-ID: <treetopllc/collaboratory-www/pull/3179/review/1@github.com>\r\n" +
		"X-GitHub-Reason: review_requested\r\n" +
		"X-GitHub-Sender: joseg-avviato\r\n" +
		"X-GitHub-Recipient:   sspataro57  \r\n" +
		"\r\n" +
		"body\r\n"
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("fixture: parse header block: %v", err)
	}
	got := github.NotificationFacts(msg.Header)
	want := github.Notification{Reason: "review_requested", Sender: "joseg-avviato", Recipient: "sspataro57"}
	if got != want {
		t.Errorf("NotificationFacts = %+v, want %+v (Step 0b: his login is sspataro57, the only recipient seen; "+
			"values trimmed)", got, want)
	}

	// Step 0a: commit and push mail carries none of the three. Absent is empty,
	// never an error — that mail contributes no evidence.
	bare, err := mail.ReadMessage(strings.NewReader(
		"From: Salvador Spataro <noreply@github.com>\r\nSubject: [treetopllc/collaboratory-www] 3 new commits\r\n\r\nx\r\n"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if got := github.NotificationFacts(bare.Header); got != (github.Notification{}) {
		t.Errorf("NotificationFacts(commit mail) = %+v, want the zero Notification", got)
	}
}
