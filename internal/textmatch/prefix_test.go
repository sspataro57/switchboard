package textmatch

import "testing"

func TestNormalizedPrefix(t *testing.T) {
	cases := []struct {
		name  string
		value string
		limit int
		want  string
	}{
		{"plain text is unchanged", "hello there", 120, "hello there"},
		{"runs of spaces collapse", "hello    there", 120, "hello there"},
		{"newlines collapse to a space", "hello\nthere", 120, "hello there"},
		{"crlf collapses like lf", "hello\r\nthere", 120, "hello there"},
		{"tabs collapse", "hello\t\tthere", 120, "hello there"},
		{"blank line runs collapse", "hello\n\n\n\nthere", 120, "hello there"},
		{"ends are trimmed", "  hello there \n\n", 120, "hello there"},
		// NBSP is why this lives in Go: Postgres's POSIX \s would not collapse it.
		{"non-breaking space is whitespace", "hello\u00a0there", 120, "hello there"},
		{"truncates to the limit", "abcdef", 3, "abc"},
		{"limit longer than input is fine", "abc", 100, "abc"},
		{"zero limit yields empty", "abc", 0, ""},
		{"negative limit yields empty", "abc", -1, ""},
		{"empty stays empty", "", 120, ""},
		{"whitespace only becomes empty", " \n\t ", 120, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizedPrefix(tc.value, tc.limit); got != tc.want {
				t.Fatalf("NormalizedPrefix(%q, %d) = %q, want %q", tc.value, tc.limit, got, tc.want)
			}
		})
	}
}

// A multi-byte character must never be cut in half: truncating by byte would
// produce an invalid rune and make two equal texts compare unequal.
func TestNormalizedPrefixTruncatesByRuneNotByte(t *testing.T) {
	got := NormalizedPrefix("héllo wörld", 5)
	if got != "héllo" {
		t.Fatalf("got %q, want %q", got, "héllo")
	}
	for i, r := range got {
		if r == '�' {
			t.Fatalf("truncation split a rune at byte %d: %q", i, got)
		}
	}
}

// The whole point of the package: the same text formatted two ways by a provider
// round trip must compare equal.
func TestNormalizedPrefixSurvivesAProviderRoundTrip(t *testing.T) {
	sent := "Deployed the fix to staging.\n\nWill confirm once CI is green."
	returned := "Deployed the fix to staging.\r\n\r\nWill confirm once CI is green.  "
	if NormalizedPrefix(sent, 120) != NormalizedPrefix(returned, 120) {
		t.Fatalf("round-tripped text did not match:\n sent:     %q\n returned: %q",
			NormalizedPrefix(sent, 120), NormalizedPrefix(returned, 120))
	}
}
