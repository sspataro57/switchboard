package textmatch

// The ONE spelling of "the subject a reply carries" (bug
// gmail-reply-empty-subject-off-thread, Jira SWT-61).
//
// Two callers need the same rule: internal/tools/delivery.go builds the subject
// of a gmail reply at draft time (CLIENT-VISIBLE), and internal/capture/prreview.go
// strips the same prefixes to build a PR-review board title. Before SWT-61 only
// the second existed, as a private `^(?i:re:\s*)+` in capture. Two regexps that
// must agree is the SWT-13 canonicalization landmine: when they drift nothing
// errors — one surface doubles "Re: Re:" or eats a word, and only a human
// reading the mail notices. replysubject_callsites_test.go pins that this stays
// the only spelling.
//
// Measured on prod 2026-09-15 (see the diagnosis): 247 latest-inbound subjects
// and 190 stored thread subjects ALREADY start with a reply prefix, so a naive
// `"Re: " + subject` would double on hundreds of live threads.

import (
	"regexp"
	"strings"
)

// replyPrefix matches ONE leading English reply prefix: "re", optionally
// counted ("re[2]", the form Outlook and some clients emit), in any case, with
// any spacing around the colon.
//
// The COLON is what makes it a prefix, and that is the whole subtlety: "re"
// must be followed only by the optional count and then the colon, never by more
// letters. That is what keeps "Research: findings", "Reply: findings" and
// "Redacted: budget" intact — subjects whose first word merely starts with
// "re". Anchored, so "Question re: the program" keeps its interior words.
//
// Deliberately NOT here: localized prefixes (AW:, SV:, VS:, Antw:, Rif:, R:).
// The prod corpus contains zero of them, so handling them would be untested
// defensive code on a client-visible string; the unit tests pin that they stay
// in the words. "Fwd:" is likewise preserved — a reply to a forward is
// conventionally "Re: Fwd: …".
var replyPrefix = regexp.MustCompile(`^[[:space:]]*(?i:re)[[:space:]]*(?:\[[0-9]+\])?[[:space:]]*:[[:space:]]*`)

// StripReplyPrefix removes every leading English reply prefix from a subject —
// "Re:" repeated, in any case, with odd spacing, and the counted "Re[2]:" form
// — and trims. "Fwd:" is preserved. The remaining text is returned verbatim: it
// is client-visible, so nothing else about it is normalized. Idempotent.
func StripReplyPrefix(subject string) string {
	s := strings.TrimSpace(subject)
	// Repeated rather than a `+` quantifier so each pass re-anchors: it strips
	// mixed chains like "Re: Re[2]: Re: …" without the regexp having to
	// describe every interleaving.
	for {
		stripped := replyPrefix.ReplaceAllString(s, "")
		if stripped == s {
			break
		}
		s = stripped
	}
	return strings.TrimSpace(s)
}

// ReplySubject is the subject a reply to `subject` carries: exactly one "Re: "
// in front of StripReplyPrefix(subject), so replying to an already-prefixed
// subject never doubles it. Idempotent.
//
// Returns "" when nothing is left to reply about (empty, blank, or prefix-only
// input). The caller REFUSES the draft on "" rather than sending "Re: " alone —
// a subject that says nothing is what reached a client as delivery #36.
func ReplySubject(subject string) string {
	head := StripReplyPrefix(subject)
	if head == "" {
		return ""
	}
	return "Re: " + head
}
