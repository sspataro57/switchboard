package slackweb

// MentionsOwner (SWT-79, slack-channel-mentions D3) is the ONE spelling of "this
// Slack text @-mentions Salvador". Salvador, 2026-09-23: "channels is only when
// they mention me". Capture records the fact on a channel message's decision
// (the mention gate on the inquiry lane) and promote reads it to call a channel
// message addressed.
//
// The leaf stores mentions as DISPLAY NAMES in the text ("@Salvador",
// "@Salvador Spataro", "@SalvadorSpataro"), never as <@U…> ids (pre-check 0b:
// none in 30 days). The names are a Go constant, not configuration: a typo
// cannot widen the gate unreviewed (SWT-30 D2's reason).
//
// Plain Go, not a regular expression: the right boundary needs a lookahead
// ("." followed by a letter is a domain, not punctuation) that RE2 lacks.

import (
	"strings"
	"unicode"
)

// ownerMentionName and ownerMentionSurname spell the owner's display name. A
// mention is "@" + name, optionally followed directly by the surname (the joined
// "@SalvadorSpataro"); "@Salvador Spataro" is "@Salvador" + a space.
const (
	ownerMentionName    = "salvador"
	ownerMentionSurname = "spataro"
)

// MentionsOwner reports whether text @-mentions the owner. Case-insensitive.
//
// Left boundary: the "@" starts the text, or the rune before it is not a
// letter or digit and not one of the email local-part characters . _ % + -
// or a "/" (so "x@salvador.com" is an address and ".../@salvador/..." a URL
// path, not a mention). Right boundary: the name
// ends the text, or the next rune is not a letter, digit or "_", and is not a
// "." followed by a letter or digit (so "@salvador.com" is a domain).
// @here, @channel and @everyone are broadcasts, not a mention of him.
func MentionsOwner(text string) bool {
	rs := []rune(text)
	for i, r := range rs {
		if r != '@' || !mentionLeftBoundary(rs, i) {
			continue
		}
		end, ok := matchFold(rs, i+1, ownerMentionName)
		if !ok {
			continue
		}
		if joined, ok := matchFold(rs, end, ownerMentionSurname); ok {
			end = joined
		}
		if mentionRightBoundary(rs, end) {
			return true
		}
	}
	return false
}

// matchFold reports whether word (lower case) appears at rs[at:], ignoring
// case, and returns the index just past it.
func matchFold(rs []rune, at int, word string) (int, bool) {
	for _, w := range word {
		if at >= len(rs) || unicode.ToLower(rs[at]) != w {
			return 0, false
		}
		at++
	}
	return at, true
}

func mentionLeftBoundary(rs []rune, at int) bool {
	if at == 0 {
		return true
	}
	p := rs[at-1]
	if unicode.IsLetter(p) || unicode.IsDigit(p) {
		return false
	}
	// "/" too: "medium.com/@salvador/post" is a URL path, not a mention.
	return !strings.ContainsRune("._%+-/", p)
}

func mentionRightBoundary(rs []rune, at int) bool {
	if at >= len(rs) {
		return true
	}
	n := rs[at]
	if unicode.IsLetter(n) || unicode.IsDigit(n) || n == '_' {
		return false
	}
	if n == '.' && at+1 < len(rs) && (unicode.IsLetter(rs[at+1]) || unicode.IsDigit(rs[at+1])) {
		return false
	}
	return true
}
