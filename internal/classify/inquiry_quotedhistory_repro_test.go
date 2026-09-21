package classify

// REPRODUCTION — SWT-70 / inquiry-reads-quoted-history (docs/bugs/inquiry-reads-quoted-history.md).
//
// Bug: an inbound client reply whose NEW text (above its "-----Original Message-----"
// separator) delivers requested material was given needs_reply=false / ask_kind="fyi"
// by the inquiry lane. The stored reason paraphrases the PREVIOUS message, which is
// present only inside the quoted chain below the separator.
//
// This test is model-free. It drives the pure function that builds the inquiry lane's
// user prompt — renderInquiryUser, internal/classify/inquiry.go:175, the function whose
// output is stored verbatim in ai_runs.input->>'user_prompt' (runInput, classify.go) —
// and asserts the property the report calls correct: the quoted reply history must not
// be part of what the model is asked to judge, i.e. it is absent, or fenced and
// subordinate to the new text.
//
// It FAILS TODAY: the target body is rendered verbatim (renderMessage caps at 4000 chars
// and does nothing else), so the quoted chain is handed to the model as plain message
// text, and the flattened thread-context line carries a second copy of it.
//
// The fixture is SYNTHETIC — same shape as production run 12721 (message 385428), none
// of the client's words: short new paragraph, separator, two-level quoted chain, trailing
// disclaimer; a prior outbound context message and a prior inbound one that itself quotes.
//
// Run: go test ./internal/classify/ -run TestRenderInquiryUser_QuotedReplyHistory -v

import (
	"strings"
	"testing"
	"time"
)

// quotedOnlyClaim appears ONLY inside quoted history — never in any message's new
// text. If it reaches the model, the model was shown a promise that the sender has
// since kept, as though it were the message.
const quotedOnlyClaim = "I'll send the field list along tomorrow."

// newTextAsk is the whole of the target message's new text: the material the
// previous message promised, now delivered.
const newTextAsk = `Morning,

Sorry I didn't send this Friday. The fields we need are:

Course
- offering_unit (VARCHAR(100))
- department (VARCHAR(100))

Section
- seat_count (INTEGER)
- labels (TEXT)

Thanks
`

// quotedChain is everything from the first separator down: the sender's own prior
// message, then ours quoted under it, then a boilerplate disclaimer.
const quotedChain = `-----Original Message-----
From: Dana Reyes
Sent: Thursday, September 17, 2026 3:16 PM
To: 'ops@example.org' <ops@example.org>
Subject: RE: [EXT] Re: Questions About Portal Integration

Great, thanks

` + quotedOnlyClaim + `

-----Original Message-----
From: ops@example.org <ops@example.org>
Sent: Thursday, September 17, 2026 10:43 AM
To: Dana Reyes <dreyes@example.edu>
Subject: RE: [EXT] Re: Questions About Portal Integration

Hi Dana,

Both answers, and one of them is done now.

1. PATCH for sections - available today

You were right that it was missing. The identifier goes in the body, not the
path, the same shape as the courses PATCH. Send only the fields you want
changed. I tested it this morning against one of the sections your integration
loaded on staging.

One rough edge worth knowing: an identifier that does not exist currently
returns a 500 rather than a 404. It is ticketed for an upcoming release.

2. Custom fields - the API cannot create them, by design

The inbound API publishes the fields a portal has configured and stores values
against them, but it never defines one. That is deliberate: definitions are
configuration and stay with portal administrators.

So send me the details and I'll create them for you. For each field: the name
and label, the type, and whether it should be visible to coordinators. Once
they are in I'll confirm they are readable and writable from your end before
you build against them.

Thanks,
Ops

--

LEGAL DISCLAIMER: The information contained in this message is intended solely
for the individual or entity to which it is addressed and may contain
confidential material. Any unauthorized dissemination, distribution, copying or
taking action in reliance upon this information by persons or entities other
than the intended recipient is expressly prohibited.
`

func quotedHistoryFixture() PendingMessage {
	sent := time.Date(2026, 9, 21, 13, 57, 2, 0, time.UTC)
	priorOut := `Hi Dana, Both answers, and one of them is done now. 1. PATCH for sections - available today You were right that it was missing. The identifier goes in the body, not the path, the same shape as the courses PATCH. Send only the fields you want changed. I tested it this morning against one of the sections your integration loaded on staging. One rough edge worth knowing: an identifier that does not exist currently returns a 500 rather than a 404. It is ticketed for an upcoming release. 2. Custom fields - the API cannot create them, by design The inbound API publishes the fields a portal has configured and stores values against them, but it never defines one.`
	priorIn := "Great, thanks\n\n" + quotedOnlyClaim + "\n\n" + quotedChain[strings.Index(quotedChain, "-----Original Message-----\nFrom: ops@example.org"):]

	return PendingMessage{
		MessageID:       385428,
		RawSourceItemID: 96917,
		ThreadID:        327208,
		ThreadKey:       "gmail:ops@example.org:<root@example.org>",
		SentAt:          sent,
		Sender:          `"Dana Reyes" <dreyes@example.edu>`,
		Subject:         "RE: [EXT] Re: Questions About Portal Integration",
		Channel:         "gmail",
		Direction:       "inbound",
		BodyText:        newTextAsk + "\n" + quotedChain,
		ProjectID:       4,
		ProjectSlug:     "collaboratory",
		ThreadContext: []ThreadMessage{
			{MessageID: 385001, SentAt: sent.Add(-96 * time.Hour), Direction: "outbound", BodyText: priorOut},
			{MessageID: 385100, SentAt: sent.Add(-91 * time.Hour), Direction: "inbound", BodyText: priorIn},
		},
	}
}

func TestRenderInquiryUser_QuotedReplyHistoryIsNotJudged(t *testing.T) {
	m := quotedHistoryFixture()
	prompt := renderInquiryUser(m)

	const decideMarker = "The message to decide on:"
	i := strings.Index(prompt, decideMarker)
	if i < 0 {
		t.Fatalf("prompt has no %q section; fixture or renderer changed shape:\n%s", decideMarker, prompt)
	}
	decide := prompt[i:]

	// Measurement, for the record: how much of what the model is asked to judge is
	// quoted history rather than the message.
	if sep := strings.Index(decide, "-----Original Message-----"); sep >= 0 {
		t.Logf("message-to-decide-on section: %d chars, of which %d (%.0f%%) sit below the first quote separator",
			len(decide), len(decide)-sep, 100*float64(len(decide)-sep)/float64(len(decide)))
	}

	t.Run("target body", func(t *testing.T) {
		// The previous message's promise lives only under the separator. Reaching the
		// model at all means the verdict can be written about it — which is what run
		// 12721 did.
		if strings.Contains(decide, quotedOnlyClaim) {
			t.Errorf("quoted reply history is inside the message the model is asked to judge:\n"+
				"the sentence %q appears only below %q, yet it is in the decide-on section.\n"+
				"Expected: the quoted chain absent, or fenced and marked as prior history subordinate to the new text.",
				quotedOnlyClaim, "-----Original Message-----")
		}
	})

	t.Run("thread context line", func(t *testing.T) {
		// The prior inbound message is flattened onto one "them:" line. It carries its
		// OWN quoted copy of our earlier message, so the same history is shown twice.
		var themLine string
		for _, line := range strings.Split(prompt[:i], "\n") {
			if strings.HasPrefix(line, "them: ") {
				themLine = line
			}
		}
		if themLine == "" {
			t.Fatalf("no \"them:\" context line in the prompt:\n%s", prompt[:i])
		}
		if strings.Contains(themLine, "-----Original Message-----") {
			t.Errorf("a thread-context line carries its own quoted reply history:\n"+
				"the %q separator and the headers under it are inside the flattened context line (%d chars).\n"+
				"Expected: context lines show each prior message's new text only.",
				"-----Original Message-----", len(themLine))
		}
	})
}
