package store

import "regexp"

// UnconfirmedNoteMarker is THE prefix both delivery reconcilers write into
// deliveries.error, and the thing every re-arm path must strip.
//
// It lives here — in the one package the tool layer and both connector packages
// already import — because it had three independent definitions and no contract
// test, which is a silent-failure generator of exactly the kind this repo keeps
// paying for. The failure mode is specific and invisible: the reconcilers use
// this string as their FIRE-ONCE guard, skipping any row whose error already
// contains it, and `mark_delivery_sent` strips it to RE-ARM the alarm for a new
// attempt. If a connector's spelling drifts from the tool layer's, the strip
// silently misses, the guard sees a marker that is still there, and that
// delivery can never be flagged again. Nothing errors. The alarm simply stops
// existing for the one row it had already caught once.
//
// SWT-19 shipped that drift risk twice — once as a duplicated constant, then as
// a private copy in the tool layer — before this consolidation.
const UnconfirmedNoteMarker = "unconfirmed after"

// unconfirmedNotePattern matches one whole marker note, including the " | "
// separator that joins it to an earlier diagnostic when one is present.
//
// Anchored on the separator or the string start so it cannot eat into the
// middle of an unrelated error: the note runs to the next separator or the end,
// and everything outside it survives. Preserving the rest is the point — an
// earlier sender failure is a real diagnostic, and an earlier cut of the re-arm
// destroyed it by clearing the whole column.
var unconfirmedNotePattern = regexp.MustCompile(`(?s)(^|\s\|\s)` + regexp.QuoteMeta(UnconfirmedNoteMarker) + `[^|]*`)

// StripUnconfirmedNote removes the reconciler's marker note from a
// deliveries.error value, leaving any unrelated diagnostic intact. The result is
// empty when nothing else remains, which callers store as NULL.
func StripUnconfirmedNote(errText string) string {
	if errText == "" {
		return ""
	}
	out := unconfirmedNotePattern.ReplaceAllString(errText, "")
	// A leading separator can survive when the marker was the FIRST of several
	// notes; trim it rather than leaving a note that starts with " | ".
	for len(out) > 0 && (out[0] == ' ' || out[0] == '|') {
		out = out[1:]
	}
	for len(out) > 0 && (out[len(out)-1] == ' ' || out[len(out)-1] == '|') {
		out = out[:len(out)-1]
	}
	return out
}

// HasUnconfirmedNote reports whether a note is already present — the
// reconcilers' fire-once guard, in Go, so a caller that has the row in hand
// does not have to re-spell the check in SQL.
func HasUnconfirmedNote(errText string) bool {
	return unconfirmedNotePattern.MatchString(errText)
}
