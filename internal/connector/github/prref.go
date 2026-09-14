package github

// The ONE spelling of a GitHub pull request reference outside the webhook
// payloads (SWT-54, docs/tickets/treetop-pr-review-tasks_SPEC.md D2 point 3,
// D1 and D5). Pure: no I/O.
//
// A PR has one external_refs key under system='github', `{owner}/{repo}#{N}`,
// which is what PGTaskResolver.Resolve and orchestrator R9-R11 already use.
// Capture reaches PRs through GitHub notification mail, whose thread root is
// the PR-opened Message-ID `<{owner}/{repo}/pull/{N}@github.com>`, so its
// key_regex captures the PATH form `{owner}/{repo}/pull/{N}`. Both spellings
// parse here and collapse to one key. Two spellings of one PR would let one
// PR hold two tasks, silently: external_refs' UNIQUE (system, external_key)
// cannot see through spellings.

import (
	"net/mail"
	"regexp"
	"strconv"
	"strings"
)

// PRStateMerged, PRStateClosed and PRStateReopened are PRStateNotice's answers.
// The close reason reads "PR #N merged on GitHub" / "PR #N closed on GitHub".
// A reopened notice is a state notice too (owner decision 2026-09-14): it never
// closes a task and never reopens a dismissed one; it is logged, nothing more.
const (
	PRStateMerged   = "merged"
	PRStateClosed   = "closed"
	PRStateReopened = "reopened"
)

// PRStateEndsPR reports whether state is a notice that the PR is merged or
// closed (the two states that close a review task, D5).
func PRStateEndsPR(state string) bool {
	return state == PRStateMerged || state == PRStateClosed
}

// The X-GitHub-* notification headers D1 reads from stored raw mail.
const (
	HeaderReason    = "X-GitHub-Reason"
	HeaderSender    = "X-GitHub-Sender"
	HeaderRecipient = "X-GitHub-Recipient"
)

// A path segment of an owner or repo name: the seeded key_regex's class.
const prSegment = `[A-Za-z0-9._-]+`

var (
	prPathRe = regexp.MustCompile(`^(` + prSegment + `/` + prSegment + `)/pull/([0-9]+)$`)
	prKeyRe  = regexp.MustCompile(`^(` + prSegment + `/` + prSegment + `)#([0-9]+)$`)

	// GitHub's merge and close notices, as the FIRST non-empty body line.
	// Pre-check 0d observed "Closed #N."; "Merged #N into <branch>." is the
	// documented shape.
	prMergedRe   = regexp.MustCompile(`^Merged #([0-9]+) into \S+\.$`)
	prClosedRe   = regexp.MustCompile(`^Closed #([0-9]+)\.$`)
	prReopenedRe = regexp.MustCompile(`^Reopened #([0-9]+)\.$`)
)

// ParsePRRef accepts `{owner}/{repo}/pull/{N}` or `{owner}/{repo}#{N}` and
// nothing else: issues, actions, commits, a Message-ID with its brackets, and a
// number that is not a positive integer are all refused. HeadBranch is empty.
func ParsePRRef(s string) (PRRef, bool) {
	m := prPathRe.FindStringSubmatch(s)
	if m == nil {
		m = prKeyRe.FindStringSubmatch(s)
	}
	if m == nil {
		return PRRef{}, false
	}
	n, ok := prNumber(m[2])
	if !ok {
		return PRRef{}, false
	}
	return PRRef{Repo: m[1], PR: n}, true
}

// prNumber reads a PR number. GitHub numbers from 1, and a zero is what a
// failed Atoi leaves behind, so 0 (and a leading zero) is refused.
func prNumber(s string) (int, bool) {
	if s == "" || s[0] == '0' {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// PRKey is the canonical external_refs key: `{owner}/{repo}#{N}`.
func PRKey(ref PRRef) string {
	return ref.Repo + "#" + strconv.Itoa(ref.PR)
}

// PRURL is the PR's web URL.
func PRURL(ref PRRef) string {
	return "https://github.com/" + ref.Repo + "/pull/" + strconv.Itoa(ref.PR)
}

// PRRootMessageID is GitHub's PR-opened notification Message-ID, the root every
// later notification on the PR references. D1 finds the opening mail by exact
// equality on it.
func PRRootMessageID(ref PRRef) string {
	return "<" + ref.Repo + "/pull/" + strconv.Itoa(ref.PR) + "@github.com>"
}

// PRStateNotice reports whether body is GitHub's merge, close or reopen notice
// for PR n. It reads the first NON-EMPTY line only (what pre-check 0d measured):
// "merged in #3202" prose inside a PR description, a quoted notice, or a notice
// further down the body is not a notice. "Reopened #N." answers PRStateReopened,
// which is not a close (PRStateEndsPR).
func PRStateNotice(body string, n int) (string, bool) {
	line := firstNonEmptyLine(body)
	if line == "" {
		return "", false
	}
	for _, c := range []struct {
		re    *regexp.Regexp
		state string
	}{{prMergedRe, PRStateMerged}, {prClosedRe, PRStateClosed}, {prReopenedRe, PRStateReopened}} {
		m := c.re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if got, ok := prNumber(m[1]); ok && got == n {
			return c.state, true
		}
		return "", false
	}
	return "", false
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// Notification is the three X-GitHub-* facts of one notification mail. An
// absent header is "" (commit and push mail carries none), never an error.
type Notification struct {
	Reason    string
	Sender    string
	Recipient string
}

// NotificationFacts reads the headers through mail.Header.Get, which
// canonicalizes the name: net/mail keys the map "X-Github-Reason", so indexing
// it with the spelled "X-GitHub-Reason" would find nothing, silently.
func NotificationFacts(h mail.Header) Notification {
	return Notification{
		Reason:    strings.TrimSpace(h.Get(HeaderReason)),
		Sender:    strings.TrimSpace(h.Get(HeaderSender)),
		Recipient: strings.TrimSpace(h.Get(HeaderRecipient)),
	}
}
