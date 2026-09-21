package classify

import (
	"strings"
	"testing"
)

// SWT-70: StripQuotedHistory keeps a reply's NEW text and cuts the conversation
// quoted under it. Fixtures are synthetic.
func TestStripQuotedHistory(t *testing.T) {
	const newText = "Morning,\n\nSorry for the delay. The fields we need are offering_unit and department.\n\nThanks"
	for _, tc := range []struct {
		name, body string
		wantCut    bool
	}{
		{"outlook dashed separator", newText + "\n\n-----Original Message-----\nFrom: Dana Reyes\nSent: Thursday\n\nI'll send the list tomorrow.", true},
		{"outlook, spanish", newText + "\n\n-----Mensaje original-----\nDe: Dana\nEnviado: jueves\n\nMañana lo envío.", true},
		{"gmail on-wrote, one line", newText + "\n\nOn Tue, Sep 15, 2026 at 3:16 PM Dana Reyes <dana@example.edu> wrote:\n> I'll send the list tomorrow.\n", true},
		{"gmail on-wrote, wrapped", newText + "\n\nOn Tue, Sep 15, 2026 at 3:16 PM Dana Reyes\n<dana@example.edu> wrote:\n> I'll send the list tomorrow.\n", true},
		{"gmail, spanish", newText + "\n\nEl mar, 15 sept 2026 a las 15:16, Dana (<dana@example.edu>) escribió:\n> Mañana lo envío.\n", true},
		{"outlook header block without dashes", newText + "\n\nFrom: Dana Reyes <dana@example.edu>\nSent: Thursday, September 17, 2026 3:16 PM\nTo: ops@example.org\nSubject: RE: fields\n\nI'll send the list tomorrow.", true},
		{"underscore rule", newText + "\n\n________________________________\nFrom: Dana\nSent: Thursday\n\nI'll send the list tomorrow.", true},
		{"a block of quoted lines", newText + "\n\n> I'll send the list tomorrow.\n> It is nearly done.\n> Thanks for waiting.\n", true},
		{"no quote at all", newText, false},
		{"one quoted line is not a block", newText + "\n> as you said\nand more new text here.", false},
		// An inline reply: almost nothing above the separator, the answers are
		// below it. Cutting would throw the reply away, so the body is kept whole.
		{"inline reply under on-wrote", "Hi,\n\nOn Tue, Sep 15, 2026 Dana <d@example.edu> wrote:\n> Which fields?\noffering_unit and department, please add them this week.\n", false},
		{"a sentence that merely says 'wrote'", "On Tuesday I wrote to the registrar and she has not answered yet, can you chase her for me please?", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := StripQuotedHistory(tc.body)
			if !tc.wantCut {
				if got != tc.body {
					t.Errorf("the body was cut and must not be:\n%q", got)
				}
				return
			}
			if strings.Contains(got, "tomorrow") || strings.Contains(got, "Mañana") || strings.Contains(got, "wrote:") || strings.Contains(got, "Original Message") {
				t.Errorf("quoted history survived the cut:\n%q", got)
			}
			if !strings.Contains(got, "offering_unit and department") {
				t.Errorf("the new text was lost:\n%q", got)
			}
		})
	}
}

// A forward's header block looks exactly like a reply's, but what follows it is
// the payload, not history. It is never cut.
func TestStripQuotedHistory_KeepsAForwardWhole(t *testing.T) {
	gmail := "Can you take a look at this and tell me what we owe?\n\n---------- Forwarded message ---------\nFrom: Billing <billing@example.com>\nDate: Tue, Sep 15, 2026\nSubject: Invoice 1182\n\nYour invoice of $400 is due Friday."
	if got := StripQuotedHistory(gmail); got != gmail {
		t.Errorf("a Gmail forward was cut:\n%q", got)
	}
	outlook := "Can you take a look at this and tell me what we owe?\n\nFrom: Billing <billing@example.com>\nSent: Tuesday, September 15, 2026\nTo: Dana\nSubject: Invoice 1182\n\nYour invoice of $400 is due Friday."
	if got := StripQuotedHistoryOf("FW: Invoice 1182", outlook); got != outlook {
		t.Errorf("an Outlook forward (FW: subject) was cut:\n%q", got)
	}
	if got := StripQuotedHistoryOf("RE: Invoice 1182", outlook); got == outlook {
		t.Errorf("the same header block under a RE: subject is reply history and must be cut")
	}
}

// Production mail is CRLF: 696 of 757 collaboratory bodies carry \r. The
// line-anchored separators match there only because \s* before $ absorbs the
// \r; tightening a pattern to `-{2,}$` would stop matching every Outlook mail
// with the LF-only fixtures above still green. These fixtures are CRLF.
func TestStripQuotedHistory_CRLFBodies(t *testing.T) {
	const newText = "Morning,\r\n\r\nThe fields we need are offering_unit and department.\r\n\r\nThanks"
	for _, tc := range []struct{ name, tail string }{
		{"dashed separator", "\r\n\r\n-----Original Message-----\r\nFrom: Dana Reyes\r\nSent: Thursday\r\n\r\nI'll send the list tomorrow.\r\n"},
		{"underscore rule", "\r\n\r\n________________________________\r\nFrom: Dana\r\nSent: Thursday\r\n\r\nI'll send the list tomorrow.\r\n"},
		{"on-wrote", "\r\n\r\nOn Tue, Sep 15, 2026 at 3:16 PM Dana Reyes <dana@example.edu> wrote:\r\n> I'll send the list tomorrow.\r\n"},
		{"on-wrote, wrapped", "\r\n\r\nOn Tue, Sep 15, 2026 at 3:16 PM Dana Reyes\r\n<dana@example.edu> wrote:\r\n> I'll send the list tomorrow.\r\n"},
		{"header block", "\r\n\r\nFrom: Dana Reyes <dana@example.edu>\r\nSent: Thursday, September 17, 2026\r\nTo: ops@example.org\r\n\r\nI'll send the list tomorrow.\r\n"},
		{"quoted block", "\r\n\r\n> I'll send the list tomorrow.\r\n> It is nearly done.\r\n> Thanks for waiting.\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := StripQuotedHistory(newText + tc.tail)
			// The separator LINE must go too: with the dashed or underscore pattern
			// broken on CRLF, the From:/Sent: block under it still cuts the history,
			// so "no tomorrow" alone would pass with the separator left dangling.
			for _, leak := range []string{"tomorrow", "Original Message", "_____", "wrote:", "From:"} {
				if strings.Contains(got, leak) {
					t.Errorf("%q survived the cut on a CRLF body:\n%q", leak, got)
				}
			}
			if !strings.Contains(got, "offering_unit and department") {
				t.Errorf("the new text was lost:\n%q", got)
			}
		})
	}
}

// The TARGET goes through the subject-aware stripper: a forward recognised only
// by its FW: subject (Outlook writes no "Forwarded message" line) is rendered
// whole, payload included. Swapping the call for the body-only variant in
// renderInquiryUser must turn this red.
func TestRenderInquiryUser_AForwardBySubjectIsRenderedWhole(t *testing.T) {
	body := "Can you take a look at this and tell me what we owe?\n\nFrom: Billing <billing@example.com>\nSent: Tuesday, September 15, 2026\nTo: Dana\nSubject: Invoice 1182\n\nYour invoice of $400 is due Friday."
	prompt := renderInquiryUser(PendingMessage{MessageID: 1, Channel: "gmail", Sender: "dana@example.edu", Subject: "FW: Invoice 1182", BodyText: body})
	if !strings.Contains(prompt, "due Friday") {
		t.Errorf("the forwarded payload was cut from a FW: target:\n%s", prompt)
	}
	reply := renderInquiryUser(PendingMessage{MessageID: 1, Channel: "gmail", Sender: "dana@example.edu", Subject: "RE: Invoice 1182", BodyText: body})
	if strings.Contains(reply, "due Friday") {
		t.Errorf("the same header block under a RE: subject is reply history and must be cut:\n%s", reply)
	}
}
