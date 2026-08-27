package store

import "testing"

// The marker had three independent definitions before SWT-19's fourth pass, with
// no test tying them together. The failure mode was silent by construction: the
// reconcilers skip any row whose error contains the marker (their fire-once
// guard) and mark_delivery_sent strips it to re-arm. If the two spellings ever
// disagreed, the strip would miss, the guard would keep seeing a marker, and
// that delivery could never be flagged again — no error, no log, just an alarm
// that stopped existing for the one row it had already caught.
func TestStripUnconfirmedNote(t *testing.T) {
	note := UnconfirmedNoteMarker + " 6 sync passes with no matching Upwork message. Check the thread"
	sendErr := "smtp: 550 mailbox unavailable"

	cases := []struct {
		name, in, want, why string
	}{
		{
			"marker alone", note, "",
			"nothing else to keep, so the column becomes empty and the caller stores NULL",
		},
		{
			"marker appended to a real error", sendErr + " | " + note, sendErr,
			"the sender failure is a genuine diagnostic and the re-arm must not destroy it — an earlier " +
				"cut set error=NULL and justified it with a claim about the audit trail that was false",
		},
		{
			"marker first, error second", note + " | " + sendErr, sendErr,
			"the leading separator must not survive as a note that begins with a pipe",
		},
		{
			"unrelated error only", sendErr, sendErr,
			"a row that was never flagged must come through untouched",
		},
		{"empty", "", "", "no error, no change"},
		{
			"prose that merely contains the words",
			"delivery was unconfirmed after review by hand",
			"delivery was unconfirmed after review by hand",
			"the pattern is ANCHORED on the string start or a ' | ' separator, so the marker only counts " +
				"where a reconciler could actually have written it. Prose that happens to contain the same " +
				"words mid-sentence is left alone — which is why the anchor is not decoration",
		},
		{
			"a note that STARTS with the marker words but is not one",
			UnconfirmedNoteMarker + " review, per the client", "",
			"the honest limit, recorded rather than hidden: anchored-and-prefix means anything opening with " +
				"the marker text is treated as a marker. Safe today because the reconcilers are the only " +
				"writers of notes in this column, but a future writer must not open a note this way",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripUnconfirmedNote(tc.in); got != tc.want {
				t.Errorf("StripUnconfirmedNote(%q) = %q, want %q — %s", tc.in, got, tc.want, tc.why)
			}
		})
	}
}

// Strip and Has must agree: the guard and the re-arm are two halves of one
// contract, and a disagreement is the silent failure above.
func TestUnconfirmedNoteGuardAndStripAgree(t *testing.T) {
	note := UnconfirmedNoteMarker + " 6 sync passes"
	for _, in := range []string{note, "smtp: 550 | " + note, note + " | smtp: 550"} {
		if !HasUnconfirmedNote(in) {
			t.Errorf("HasUnconfirmedNote(%q) = false, but this is a flagged row; the reconciler would "+
				"re-flag it and emit a duplicate event", in)
		}
		if stripped := StripUnconfirmedNote(in); HasUnconfirmedNote(stripped) {
			t.Errorf("after stripping %q the guard still sees a marker in %q — the re-arm did not re-arm, "+
				"so this delivery can never be flagged again", in, stripped)
		}
	}
}
