package ticketstatus

// The ONE place a configured "delivered" status name is compared to an observed
// one (SWT-34, docs/tickets/qa-delivered-drop_SPEC.md criteria 9-11).
//
// WHY THIS IS NOT A CONTRADICTION OF SWT-32's D2. D2 bans a status-NAME list in
// the binary, because names are one client's workflow configuration and Jira
// itself owns the structural fact of "finished" (statusCategory). This file
// answers a DIFFERENT question — "is the ball in my court" — which no function
// of statusCategory can answer: TT-In QA and TT-Work In Progress are both
// `indeterminate`. So the names live in a projects column, hand-armed, and this
// package's non-test sources still may not contain one (the structural ban in
// structure_test.go, which SWT-34 STRENGTHENED with the real Treetop strings).
//
// This file imports only `strings`, deliberately: the comparison is a pure fold
// over two values the driver already holds, and it must stay callable from the
// pure Decide.

import "strings"

// NormalizeStatusName folds a human-typed workflow label: lowercase, every run
// of UNICODE whitespace collapsed to a single space, ends trimmed.
//
// Both sides of the comparison are hand-typed — Jira serialises whatever an
// admin typed into the workflow column, and the configured entry is pasted into
// a psql UPDATE by hand — so a trailing space or an NBSP copied out of a browser
// is the realistic typo. Without the fold it would make an armed set match
// NOTHING, and an armed feature that matches nothing looks exactly like an
// unarmed one.
//
// strings.Fields splits on unicode.IsSpace, which is what makes the NBSP case
// work; internal/textmatch records the same reasoning, and it is why this must
// never be re-spelled in SQL (Postgres's POSIX \s does not cover those spaces,
// so the two would disagree silently).
func NormalizeStatusName(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// IsDeliveredStatus reports whether an observed status name is a member of a
// project's configured delivered set — EXACT on the normalized form, never
// substring and never prefix (E2).
//
// Substring matching was rejected explicitly: strings.Contains(name, "QA")
// would match TT-In QA, TT-QA Blocked and TT-Needs QA Rework, and two of those
// mean the ball IS in his court. The configured set is a set of whole names;
// adding one is one array element.
//
// An entry that normalizes to empty is IGNORED (E4), and an empty observed name
// is never a member (E5). Both guards exist for the same reason: an empty
// string matching everything would drop a client's entire board with no error —
// the "discriminating value is a constant in production" landmine, pre-empted on
// both sides.
func IsDeliveredStatus(name string, configured []string) bool {
	got := NormalizeStatusName(name)
	if got == "" {
		return false
	}
	for _, want := range configured {
		if w := NormalizeStatusName(want); w != "" && w == got {
			return true
		}
	}
	return false
}
