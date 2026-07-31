// Package textmatch holds the one spelling of "does this text look like the text
// we sent" that every post-hoc delivery matcher shares.
//
// It exists because the rule was already written twice (slackweb's sink and
// capture's observer) and SWT-16's review found the jira matcher comparing raw
// bytes instead. Three spellings of a comparison that must agree exactly is the
// SWT-13 canonicalization landmine in a second costume: when two of them drift,
// nothing errors — a delivery silently stops being recognizable as our own, and
// capture then reports a message switchboard sent as sent by hand.
//
// Deliberately Go-side rather than SQL. A Postgres `regexp_replace(body,'\s+',...)`
// spelling looks equivalent and is not: POSIX \s does not cover the unicode spaces
// that Go's unicode.IsSpace (via strings.Fields) does, so NBSP alone would make the
// two disagree, again with no error anywhere.
package textmatch

import "strings"

// NormalizedPrefix collapses every run of whitespace to a single space, trims the
// ends, and truncates to limit RUNES (not bytes — a multi-byte character must not
// be cut in half, which would make two equal texts compare unequal).
//
// Whitespace is collapsed rather than compared because the text makes a round trip
// through a provider that is entitled to reformat it: Jira re-serializes comment
// bodies, and line endings, trailing spaces, and blank-line runs are all things a
// provider may legitimately change without changing the message.
func NormalizedPrefix(value string, limit int) string {
	normalized := strings.Join(strings.Fields(value), " ")
	if limit < 0 {
		limit = 0
	}
	runes := []rune(normalized)
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}
