package google

// Internal tests for the refetch pass's unexported helpers (SWT-64).
//
// `package google`, not `google_test`, because refetchLikeEscape is unexported —
// and it has to be tested SOMEWHERE: review round 4 showed that replacing its
// body with `return s` leaves the entire unit and integration suite green. It is
// the one guard in this feature whose failure mode is unsafe rather than a
// refusal: `--from %` would then widen the selection to every sender in the
// mailbox, bounded only by --limit, on a pass that overwrites stored mail in
// place with no version history.
//
// Mirrors the shape of internal/tools/mailattach_test.go's likeEscape table, the
// sibling spelling this one must agree with.

import "testing"

func TestRefetchLikeEscape(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain text is untouched", "lyle@example.com", "lyle@example.com"},
		{"empty", "", ""},

		// The wildcards. Unescaped, each of these widens the selection.
		{"percent is escaped", "%", `\%`},
		{"percent inside a sender", "a%b", `a\%b`},
		{"underscore is escaped", "_", `\_`},
		{"underscore inside a sender", "a_b", `a\_b`},

		// The escape character itself, first — otherwise escaping the wildcards
		// would double-escape it.
		{"backslash is escaped", `\`, `\\`},
		{"backslash before a wildcard", `\%`, `\\\%`},

		// The shape that matters in practice.
		{"a domain is unaffected", "@foundryunderwriting.com", "@foundryunderwriting.com"},
		{"a wildcard domain is neutered", "@%.com", `@\%.com`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := refetchLikeEscape(tc.in); got != tc.want {
				t.Errorf("refetchLikeEscape(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The property the table is really asserting: after escaping, no LIKE
// metacharacter remains unescaped, so the pattern can only match itself.
func TestRefetchLikeEscape_LeavesNoBareWildcard(t *testing.T) {
	for _, in := range []string{"%", "_", `\`, "a%b_c", `%\_%`, "plain"} {
		got := refetchLikeEscape(in)
		for i := 0; i < len(got); i++ {
			if got[i] != '%' && got[i] != '_' {
				continue
			}
			if i == 0 || got[i-1] != '\\' {
				t.Errorf("refetchLikeEscape(%q) = %q leaves a bare %q at %d; it would widen the selection",
					in, got, string(got[i]), i)
			}
		}
	}
}
