package google_test

// Floor 2 of three for bug gmail-reply-empty-subject-off-thread (Jira SWT-61):
// BuildOutboundMIME requires Subject the way it requires From, To and
// Message-ID.
//
// WHAT HAPPENED. send.go writes the Subject header only `if msg.Subject != ""`
// (send.go:71-73) — the ONE conditional header in a builder that hard-requires
// From/To/Message-ID (57-62). Subject is OPTIONAL in RFC 5322, so an empty one
// produced a syntactically valid message with NO Subject header at all, and
// nothing downstream complained: not SMTP submission, not the re-ingest, not
// invariant 5's own-message match (which happily matched an outbound message
// with subject length 0). It reached a client as a standalone, subject-less
// email (prod delivery #36).
//
// WHY A FLOOR AND NOT THE FIX. This function receives an OutboundMessage and has
// no database, so it cannot construct the right subject — the fix is the
// draft-time fill in tools.draftDelivery. This is the last line if a future
// caller builds a message another way (today only delivery.go:1196 does).
// Refusing turns a silent client-visible defect into a delivery marked `failed`
// with a reason (delivery.go:1220-1224), which is a safe non-send.
//
// EXPECTED RED: the empty- and blank-Subject cases get err == nil today. The
// From/To/Message-ID cases are GREEN controls that prove the table works; if
// they ever go red, the requirement this one joins has been deleted.
//
// ZERO network: BuildOutboundMIME is pure. sampleOutbound lives in send_test.go
// (same package, no build tag).

import (
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

func TestBuildOutboundMIME_RequiresSubject(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*google.OutboundMessage)
		wantErr string // a word the refusal must name, so a failed delivery says why
	}{
		// The SWT-61 cases. A blank-but-present subject is still no subject to
		// the person reading the mail.
		{"empty subject", func(m *google.OutboundMessage) { m.Subject = "" }, "subject"},
		{"blank subject", func(m *google.OutboundMessage) { m.Subject = "   " }, "subject"},

		// Controls, GREEN today: the three requirements Subject joins.
		{"empty from", func(m *google.OutboundMessage) { m.From = "" }, "from"},
		{"empty to", func(m *google.OutboundMessage) { m.To = "" }, "to"},
		{"empty message id", func(m *google.OutboundMessage) { m.MessageID = "" }, "message-id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := sampleOutbound("placeholder body")
			tc.mutate(&msg)

			raw, err := google.BuildOutboundMIME(msg)
			if err == nil {
				t.Fatalf("BuildOutboundMIME accepted a message with %s and produced:\n%s\n"+
					"A message missing this field cannot make sense to its recipient. An optional-by-RFC field "+
					"is not optional by product: `if x != \"\" { write x }` in a transport means \"silently emit a "+
					"message missing x\" (SWT-61 landmine)", tc.name, raw)
			}
			if !strings.Contains(strings.ToLower(err.Error()), tc.wantErr) {
				t.Errorf("refusal = %q, want it to name %q", err, tc.wantErr)
			}
		})
	}
}

// Control, GREEN today and after the fix: a real subject is written, exactly
// once, and the builder still produces a parseable message.
func TestBuildOutboundMIME_WritesTheSubjectHeaderOnce(t *testing.T) {
	raw, err := google.BuildOutboundMIME(sampleOutbound("placeholder body"))
	if err != nil {
		t.Fatalf("BuildOutboundMIME: %v", err)
	}
	if n := strings.Count(string(raw), "\r\nSubject: "); n != 1 {
		t.Errorf("assembled message has %d Subject header(s), want exactly 1:\n%s", n, raw)
	}
}
