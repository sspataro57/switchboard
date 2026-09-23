package slackweb_test

// slack-channel-mentions (Jira SWT-79, docs/tickets/slack-channel-mentions_SPEC.md)
// criteria 1 and 2, decision D3: ONE spelling of "does this Slack message text
// @-mention Salvador". Salvador, 2026-09-23: "channels is only when they
// mention me" / "we only respond to mentions on those channels".
//
// Plain unit test: no build tag, no database, no browser.
//
// THE RULE (D3). Mentions arrive as DISPLAY NAMES in body_text ("@Salvador",
// "@Salvador Spataro", "@SalvadorSpataro"), never as <@U…> (pre-check 0b:
// raw_ids = 0 over 30 days, 89 name forms). Case-insensitive:
//
//   - the form: `@` + `salvador`, optionally followed DIRECTLY by `spataro`
//     (a bare "@Salvador" + space already covers "@Salvador Spataro");
//   - LEFT boundary: the `@` starts the text, or the rune before it is NOT a
//     Unicode letter or digit and NOT one of `. _ % + -` (the email local-part
//     characters), so x@salvador.com is an address, not a mention;
//   - RIGHT boundary: end of text, or a rune that is NOT a Unicode letter or
//     digit and NOT `_`; and NOT a `.` immediately followed by a letter or digit
//     (that is a domain: "@salvador.com");
//   - @here / @channel / @everyone are NOT a mention of him;
//   - <@U…> is NOT recognised (the leaf stores none; future work if it ever does).
//
// ---- IMPOSED SURFACE ---------------------------------------------------------
//
//	// internal/connector/slackweb/mention.go (new)
//	// MentionsOwner reports whether Slack message text @-mentions Salvador by
//	// display name. Pure; plain Go (RE2 has no lookahead, and the domain rule
//	// needs one); the owner's names are a Go constant in that file.
//	func MentionsOwner(text string) bool
//
// The name is chosen to stay OUTSIDE dmkey_test.go's DM-helper regex
// (`Is*Direct*|*DM*|*DirectMessage*`), so TestDirectMessageRuleHasOneExportedSpelling
// passes UNCHANGED (criterion 2).
//
// EXPECTED RED (greenfield): slackweb.MentionsOwner does not exist, so this file
// compile-FAILS internal/connector/slackweb's external test build with
// "undefined: slackweb.MentionsOwner". The compile error is confined to this file.
//
// MUTATIONS (V3-style): drop the left-boundary email set (x@salvador.com goes
// red); drop the domain lookahead (@salvador.com goes red); treat `_` as a
// boundary (@Salvador_bot goes red); compare case-sensitively (@salvador, goes
// red); stop at "@Salva" (@Salvadora goes red); accept @here/@channel (red).

import (
	"os"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
)

func TestMentionsOwner(t *testing.T) {
	const nbsp = " "
	cases := []struct {
		text string
		want bool
		why  string
	}{
		// ---- TRUE: criterion 1's list ----
		{"@Salvador can you check", true, "the plain form at the start of the text"},
		{"hey @Salvador Spataro", true, "the full display name: '@Salvador' followed by a space already matches"},
		{"@SalvadorSpataro", true, "the joined display name: after 'Salvador' comes a letter, so the joined form is explicit"},
		{"(@Salvador)", true, "'(' is a left boundary and ')' a right boundary"},
		{"cc @salvador,", true, "case-insensitive, and ',' ends the name"},
		{"thanks @Salvador.", true, "a trailing '.' at END of text is punctuation, not a domain"},
		{"@Salvador's PR", true, "an apostrophe ends the name (possessive)"},
		{"@Salvador" + nbsp + "Spataro", true, "a no-break space (U+00A0) is not a letter: the name ends there"},
		{"@Salvador", true, "the text is ONLY the mention"},
		{"Hi team\r\nplease look at the deploy\r\n@Salvador can you approve?", true,
			"a mention on a later line of a multi-line CRLF body"},

		// ---- TRUE: case, punctuation and Spanish (Avviato's #a-millon is Spanish) ----
		{"@SALVADOR please", true, "upper case"},
		{"@SALVADOR SPATARO please", true, "the full name in upper case"},
		{"@salvadorspataro", true, "the joined form in lower case"},
		{"@SalvadorSpataro's review", true, "the joined form, possessive"},
		{"hola @Salvador, ¿puedes revisar el PR?", true, "Spanish: ',' ends the name"},
		{"¿@Salvador?", true, "Spanish: '¿' is punctuation (not a letter), so it is a left boundary"},
		{"gracias @Salvador!", true, "'!' ends the name"},
		{"@Salvador: the build is red", true, "':' ends the name"},
		{"@Salvador... when you can", true, "a '.' followed by another '.' is not a domain"},
		{"thanks @Salvador.\nnext line", true, "a '.' followed by a newline is not a domain"},
		{"ping\t@Salvador", true, "a tab before the '@' is a left boundary"},
		{nbsp + "@Salvador", true, "a no-break space before the '@' is a left boundary"},
		{"@Salvador’s idea", true, "a typographic apostrophe (U+2019) ends the name"},
		{"@Salvador-", true, "'-' after the name is not a letter, digit or '_', so the name ends"},

		// ---- FALSE: criterion 1's list ----
		{"@Salvadora", false, "a longer name: 'a' after 'Salvador' is a letter (right boundary)"},
		{"@Salvador_bot", false, "'_' after the name continues an identifier (right boundary)"},
		{"@SalvadorSpataroX", false, "a letter after the joined form (right boundary)"},
		{"x@salvador.com", false, "an email address: a letter before the '@' (left boundary)"},
		{"a.b@Salvador.org", false, "an email address: a letter before the '@', and '.org' is a domain"},
		{"@salvador.com is down", false, "a domain at the START of the text: '.' followed by a letter"},
		{"Salvador can you", false, "no '@': a name in prose is not a mention"},
		{"@here", false, "@here is not a mention of him (\"only when they mention me\")"},
		{"@channel", false, "@channel is not a mention of him"},
		{"@everyone", false, "@everyone is not a mention of him"},
		{"<@U0182G5UH8V>", false, "a raw <@U…> id is NOT recognised (pre-check 0b found none; future work)"},
		{"", false, "the empty string"},

		// ---- FALSE: boundaries, spelled out ----
		{"@Salvador2", false, "a digit after the name (right boundary)"},
		{"@Salvadorí", false, "a Unicode letter after the name (right boundary is Unicode-aware)"},
		{"é@Salvador", false, "a Unicode letter before the '@' (left boundary is Unicode-aware)"},
		{"5@Salvador", false, "a digit before the '@' (left boundary)"},
		{"first_@Salvador", false, "'_' before the '@' is an email local-part character"},
		{"first-@Salvador", false, "'-' before the '@' is an email local-part character"},
		{"first+@Salvador", false, "'+' before the '@' is an email local-part character"},
		{"first%@Salvador", false, "'%' before the '@' is an email local-part character"},
		{"first.@Salvador", false, "'.' before the '@' is an email local-part character"},
		{"@Salvador.Spataro", false, "'.' followed by a letter reads as a domain"},
		{"@salvador.io", false, "a two-letter TLD is still a domain"},
		{"@Salvador.2", false, "'.' followed by a digit reads as a domain"},
		{"@ Salvador", false, "a space between '@' and the name"},
		{"@Salva", false, "a prefix of the name"},
		{"Salvador@", false, "the '@' after the name"},
		{"write to salvador@handsonconnect.org", false, "his own email address is not a mention"},
		{"@here @channel cc Salvador", false, "broadcasts plus a bare name"},
		{"@Salvadorian cuisine", false, "a word that starts with the name"},
		{"see https://medium.com/@salvador/post", false, "a URL path segment: '/' before the '@' (review, SWT-79)"},
		{"ping /@Salvador", false, "'/' before the '@' is a path, not a mention"},
	}
	for _, tc := range cases {
		if got := slackweb.MentionsOwner(tc.text); got != tc.want {
			t.Errorf("MentionsOwner(%q) = %v, want %v — %s", tc.text, got, tc.want, tc.why)
		}
	}
}

// A mention anywhere in the text counts, not only the first '@': an address
// earlier in the body must not end the search.
func TestMentionsOwner_ScansPastAnEarlierNonMention(t *testing.T) {
	for _, text := range []string{
		"mail x@salvador.com or ping @Salvador",
		"@Salvadora and @Salvador both here",
		"@here — actually @Salvador, you",
		"see salvador@example.com\r\n@salvador thoughts?",
	} {
		if !slackweb.MentionsOwner(text) {
			t.Errorf("MentionsOwner(%q) = false, want true: a real mention after a non-mention '@' still counts", text)
		}
	}
}

// D3's implementation notes, mechanised: the predicate lives in its own file,
// is plain Go (no regexp: RE2 has no lookahead, and a second spelling in SQL
// or regex is what SWT-70 banned), and reads no environment (the names are a
// reviewed Go constant, SWT-30 D2's reason: a typo cannot widen it unreviewed).
func TestMentionsOwner_LivesInMentionGoAsPlainGo(t *testing.T) {
	b, err := os.ReadFile("mention.go")
	if err != nil {
		t.Fatalf("read mention.go: %v — D3: the predicate lives in internal/connector/slackweb/mention.go", err)
	}
	src := string(b)
	if !strings.Contains(src, "func MentionsOwner(text string) bool") {
		t.Errorf("mention.go does not declare `func MentionsOwner(text string) bool` (D3's surface)")
	}
	for _, banned := range []struct{ frag, why string }{
		{`"regexp"`, "D3: plain Go, not a regex (RE2 has no lookahead and the domain rule needs one)"},
		{"os.Getenv", "D3: the owner's names are a Go constant, not an env var"},
		{"os.LookupEnv", "D3: the owner's names are a Go constant, not an env var"},
	} {
		if strings.Contains(src, banned.frag) {
			t.Errorf("mention.go contains %s — %s", banned.frag, banned.why)
		}
	}
}
