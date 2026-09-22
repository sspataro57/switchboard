package dashboard

// Task #513: an HTML-table email (a bank alert) converts to body_text with
// dozens of whitespace-only lines between its cells, and the source-message
// <pre> (pre-wrap) rendered every one of them — screens of blank space. These
// pin the DISPLAY tidy; body_text itself is never touched (invariant 1).

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// alertShapedBody mirrors normalized_messages 404509's shape with placeholder
// text: CRLF line ends, lines of a lone space, a tab, a no-break space, an
// ideographic space, and runs of them far longer than one blank line.
func alertShapedBody() string {
	return strings.Join([]string{
		"",
		" \r",
		"An online transfer occurred over the limit you set\r",
		"",
		"\r",
		" \r",
		" \r",
		" ",
		" \r",
		"\t \r",
		"　",
		" \r",
		" A recent transfer went over the limit you set.\r",
		" \r",
		"",
		" Account\r",
		" \r",
		" \r",
		" ending in 0000\r",
		strings.Repeat(" \r\n", 40) + " Amount   \t",
		" \r",
		" \r",
		"",
	}, "\n")
}

func TestTidySourceBody_CollapsesWhitespaceOnlyRuns(t *testing.T) {
	raw := alertShapedBody()
	if n := strings.Count(raw, "\n"); n < 50 {
		t.Fatalf("POSITIVE CONTROL FAILED: the fixture has %d lines, not the dozens a table email converts to", n)
	}
	want := strings.Join([]string{
		"An online transfer occurred over the limit you set",
		"",
		" A recent transfer went over the limit you set.",
		"",
		" Account",
		"",
		" ending in 0000",
		"",
		" Amount",
	}, "\n")
	if got := tidySourceBody(raw); got != want {
		t.Errorf("tidySourceBody(alert-shaped body) =\n%q\nwant\n%q\n— whitespace-only lines (incl. CR, tab, "+
			"U+00A0, U+3000) are blank, a blank run is ONE blank line, the ends carry none, and content lines keep "+
			"their indentation", got, want)
	}
}

func TestTidySourceBody_KeepsContentAndSingleBlankLines(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"empty", "", ""},
		{"only whitespace", " \r\n \n\t\n", ""},
		{"no blank lines", "one\ntwo", "one\ntwo"},
		{"one blank line kept", "one\n\ntwo", "one\n\ntwo"},
		{"inner spacing kept", "a   b\t c", "a   b\t c"},
		{"quoted chain kept", "> quoted\n>\n> more", "> quoted\n>\n> more"},
		{"indentation kept", "def f():\n    return 1\n\n  > nested\r", "def f():\n    return 1\n\n  > nested"},
	} {
		if got := tidySourceBody(tc.in); got != tc.want {
			t.Errorf("%s: tidySourceBody(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// The cap marker is about what the CAP dropped, never about what the tidy did.
// A short body that collapses from 60 lines to 9 must carry no marker, and a
// body cut by left(body_text, cap) must report the cut in STORED characters.
func TestDisplayBody_CapNoteIgnoresTheTidy(t *testing.T) {
	raw := alertShapedBody()
	rawChars := utf8.RuneCountInString(raw)

	body, cutTo := displayBody(raw, rawChars)
	if cutTo != 0 {
		t.Errorf("an uncut body reports cutTo=%d, want 0: collapsing blank lines shortened the TEXT, but nothing "+
			"was cut, and a marker here would claim the mailbox holds more than the page shows", cutTo)
	}
	if utf8.RuneCountInString(body) >= rawChars {
		t.Fatalf("POSITIVE CONTROL FAILED: the tidy did not shorten the fixture (%d -> %d characters), so this "+
			"test cannot tell a before-tidy count from an after-tidy one", rawChars, utf8.RuneCountInString(body))
	}
	if body != tidySourceBody(raw) {
		t.Errorf("displayBody returned %q, want the tidied body %q", body, tidySourceBody(raw))
	}

	_, cutTo = displayBody(raw, rawChars+500)
	if cutTo != rawChars {
		t.Errorf("a cut body reports cutTo=%d, want %d — the characters of STORED text the cap kept, counted "+
			"before the tidy", cutTo, rawChars)
	}
}
