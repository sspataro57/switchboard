package textmatch

// Unit tests for the ONE spelling of "the subject a reply carries"
// (bug gmail-reply-empty-subject-off-thread, Jira SWT-61).
//
// WHY THESE LIVE HERE. Two callers need the same rule and today neither has it:
// internal/tools/delivery.go must build the subject of a gmail reply at DRAFT
// time, and internal/capture/prreview.go already strips reply prefixes to build
// a board title (prSubjectReplyRe, its own regexp). Per the repo's one-spelling
// rule the strip belongs in one place; textmatch is where the shared pure text
// rules already live (NormalizedPrefix). replysubject_callsites_test.go pins
// that neither file re-spells it.
//
// WHY IT MATTERS. Measured on prod 2026-09-15 (diagnosis, "Which subject the
// reply should carry"): 247 latest-inbound subjects and 190 stored thread
// subjects ALREADY start with a reply prefix. A naive `"Re: " + subject` would
// double the prefix on hundreds of live threads, on a client-visible surface.
//
// GREENFIELD NOTE — EXPECTED RED: internal/textmatch declares neither
// StripReplyPrefix nor ReplySubject, so this file does not compile and the
// whole textmatch test binary fails ("undefined: StripReplyPrefix"). That is the
// expected failure mode until the helper is written. Imposed surface (the
// owner's decisions, 2026-09-16):
//
//	// StripReplyPrefix removes every leading English reply prefix from a
//	// subject — "Re:" repeated, in any case, with odd spacing, and the "Re[2]:"
//	// counted form — and trims. "Fwd:" is PRESERVED (a reply to a forward is
//	// conventionally "Re: Fwd: ..."). The remaining text is returned verbatim:
//	// it is client-visible.
//	func StripReplyPrefix(subject string) string
//
//	// ReplySubject is the subject a reply to `subject` carries: exactly one
//	// "Re: " in front of StripReplyPrefix(subject). Idempotent. Returns "" when
//	// nothing is left to reply about (empty, blank, or prefix-only input) — the
//	// caller refuses the draft rather than sending "Re: " alone.
//	func ReplySubject(subject string) string
//
// OUT OF SCOPE, deliberately: localized reply prefixes (AW:, SV:, VS:, Antw:,
// Rif:, R:). The prod corpus contains ZERO of them (diagnosis), so handling
// them would be untested defensive code; the cases below pin that they are left
// in the words rather than silently eaten. Revisit only with a real message.

import "testing"

// ---- StripReplyPrefix: what comes off, and what must NOT ----------------------

func TestStripReplyPrefix(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		// Nothing to strip.
		{"empty", "", ""},
		{"blank", "   ", ""},
		{"plain subject untouched", "Question about the program", "Question about the program"},
		{"trims only", "  Question about the program  ", "Question about the program"},

		// The common shapes.
		{"one prefix", "Re: Question about the program", "Question about the program"},
		{"repeated", "Re: Re: Question", "Question"},
		{"repeated three deep", "Re: Re: Re: Question", "Question"},
		{"upper", "RE: Question", "Question"},
		{"lower", "re: Question", "Question"},
		{"mixed case", "rE: Question", "Question"},
		{"no space after colon", "Re:Question", "Question"},
		{"extra spaces after colon", "Re:    Question", "Question"},
		{"space before colon", "Re : Question", "Question"},
		{"leading whitespace then prefix", "   Re: Question", "Question"},
		{"mixed case repeated", "RE: re: Question", "Question"},

		// The counted form Outlook and some clients emit.
		{"counted", "Re[2]: Question", "Question"},
		{"counted two digits", "RE[10]: Question", "Question"},
		{"counted mixed with plain", "Re: Re[2]: Re: Question", "Question"},

		// Forwards are preserved: a reply to a forward is "Re: Fwd: ...".
		{"forward preserved", "Fwd: Question", "Fwd: Question"},
		{"reply to a forward keeps the forward", "Re: Fwd: Question", "Fwd: Question"},

		// Localized prefixes are NOT stripped (out of scope — see the header).
		{"german left alone", "AW: Question", "AW: Question"},
		{"swedish left alone", "SV: Question", "SV: Question"},

		// Nothing left to reply about.
		{"prefix only", "Re:", ""},
		{"prefix only repeated and spaced", "Re: Re:   ", ""},

		// It must not eat a word that merely STARTS with "re" — the colon
		// belongs to the prefix, not to the first word of the subject.
		{"word starting with re", "Research: findings", "Research: findings"},
		{"reply is not a prefix", "Reply: findings", "Reply: findings"},
		{"redacted is not a prefix", "Redacted: budget", "Redacted: budget"},
		// ...nor a colon that belongs to the subject's own words.
		{"interior re: survives", "Question re: the program", "Question re: the program"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripReplyPrefix(tc.in); got != tc.want {
				t.Errorf("StripReplyPrefix(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestStripReplyPrefix_IsIdempotent(t *testing.T) {
	for _, in := range []string{
		"", "Question", "Re: Question", "Re: Re: Question", "Re[2]: Question",
		"Fwd: Question", "Re: Fwd: Question", "AW: Question", "Re:",
	} {
		once := StripReplyPrefix(in)
		if twice := StripReplyPrefix(once); twice != once {
			t.Errorf("StripReplyPrefix is not idempotent on %q: %q then %q", in, once, twice)
		}
	}
}

// ---- ReplySubject: exactly one prefix, never doubled --------------------------

func TestReplySubject(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"plain gains one prefix", "Question about the program", "Re: Question about the program"},
		{"already prefixed is unchanged", "Re: Question", "Re: Question"},
		{"doubled collapses to one", "Re: Re: Question", "Re: Question"},
		{"counted collapses to one", "Re[2]: Question", "Re: Question"},
		{"case is canonicalized on the prefix only", "re: question", "Re: question"},
		{"upper prefix canonicalized, words verbatim", "RE: QUESTION", "Re: QUESTION"},
		{"odd spacing", "Re :   Question", "Re: Question"},
		{"forward is replied to, not stripped", "Fwd: Question", "Re: Fwd: Question"},
		{"localized prefix stays in the words", "AW: Question", "Re: AW: Question"},
		{"trims the words", "   Question   ", "Re: Question"},

		// Nothing to reply about: the caller must refuse, never send "Re: ".
		{"empty", "", ""},
		{"blank", "   ", ""},
		{"prefix only", "Re:", ""},
		{"prefix only repeated", "Re: Re: ", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReplySubject(tc.in); got != tc.want {
				t.Errorf("ReplySubject(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The property that protects the 247 already-prefixed prod subjects: applying
// the rule to its own output never adds a second prefix.
func TestReplySubject_IsIdempotent(t *testing.T) {
	for _, in := range []string{
		"", "Question", "Re: Question", "Re: Re: Question", "Re[2]: Question",
		"RE: Question", "Fwd: Question", "AW: Question", "   ", "Re:",
	} {
		once := ReplySubject(in)
		if twice := ReplySubject(once); twice != once {
			t.Errorf("ReplySubject is not idempotent on %q: %q then %q — a re-draft or a second pass "+
				"would double the prefix on a client-visible subject", in, once, twice)
		}
	}
}
