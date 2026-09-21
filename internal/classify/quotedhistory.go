package classify

import (
	"regexp"
	"strings"
)

// SWT-70 (inquiry-reads-quoted-history). A reply carries the whole earlier
// conversation under its new text, and the inquiry lane handed all of it to the
// model as "the message to decide on". On a real client reply the new text was
// 6% of the body; the model judged the other 94% — the PREVIOUS message — and a
// delivered request came back "fyi, no reply needed". Replaying the stored
// prompt reproduced the miss 5 of 5 times; cutting the body at its first quote
// separator flipped it 5 of 5.
//
// So the quoted history is cut before the prompt is built. This is
// deterministic text handling, not a judgement: the thread's earlier messages
// reach the model anyway, once each, through the transcript block.

// minNewTextRunes is the floor under which a cut is NOT trusted. A reply written
// inline (answers interleaved with `>` quotes, or typed under the "On … wrote:"
// line) leaves almost nothing above the separator; cutting there would throw the
// reply away. Below the floor the body is kept whole — today's behaviour.
const minNewTextRunes = 20

var quoteSeparators = []*regexp.Regexp{
	// Outlook: "-----Original Message-----", and its Spanish form.
	regexp.MustCompile(`(?im)^\s*-{2,}\s*(original message|mensaje original)\s*-{2,}\s*$`),
	// Gmail / Apple Mail: "On Tue, Sep 15, 2026 at 3:16 PM Dana <d@x> wrote:",
	// which mail clients wrap onto a second line. Spanish: "El … escribió:".
	regexp.MustCompile(`(?im)^\s*(on|el)\b[^\n]{0,300}(\n[^\n]{0,200})?\b(wrote|escribió)\s*:\s*$`),
	// Outlook's header block without the dashed line: a rule of underscores, or
	// a bare "From:" line, followed within a few lines by "Sent:"/"Date:".
	regexp.MustCompile(`(?im)^\s*_{10,}\s*$`),
	regexp.MustCompile(`(?im)^\s*(from|de)\s*:[^\n]*\n(?:[^\n]*\n){0,3}?\s*(sent|date|enviado|fecha)\s*:`),
	// A block of "> quoted" lines: three or more in a row.
	regexp.MustCompile(`(?m)^(\s*>[^\n]*\n){3,}`),
}

// forwardMarkers name a FORWARD. What sits under a forward's header block is
// not history the recipient already has — it is the payload ("see below, can
// you handle this?"). A body carrying one is never cut.
var forwardMarkers = regexp.MustCompile(`(?im)^\s*(-{2,}\s*)?(begin )?(forwarded message|mensaje reenviado)\b`)

// forwardSubject is a forward's subject prefix: Fwd:, FW:, and Spanish RV:.
var forwardSubject = regexp.MustCompile(`(?i)^\s*(fwd?|rv|reenv)\s*:`)

// StripQuotedHistoryOf is StripQuotedHistory for a message whose subject is
// known: a forward (by subject) is kept whole.
func StripQuotedHistoryOf(subject, body string) string {
	if forwardSubject.MatchString(subject) {
		return body
	}
	return StripQuotedHistory(body)
}

// StripQuotedHistory returns the NEW text of a reply: the body up to its first
// quoted-history separator. The body comes back unchanged when it holds no
// separator, or when so little precedes the first one that the reply is
// probably written inline.
func StripQuotedHistory(body string) string {
	if forwardMarkers.MatchString(body) {
		return body
	}
	cut := -1
	for _, re := range quoteSeparators {
		if loc := re.FindStringIndex(body); loc != nil && (cut < 0 || loc[0] < cut) {
			cut = loc[0]
		}
	}
	if cut < 0 {
		return body
	}
	head := strings.TrimRight(body[:cut], " \t\r\n")
	if len([]rune(strings.Join(strings.Fields(head), ""))) < minNewTextRunes {
		return body
	}
	return head + "\n"
}
