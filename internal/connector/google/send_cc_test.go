package google_test

// gmail-delivery-cc (SWT-69) criteria 13 and 14, and D13 — the transport half,
// offline. BuildOutboundMIME is pure; the envelope test speaks to the
// in-process fake SMTP listener from smtp_test.go. NEVER a live send.
//
//	go test -run 'Cc' ./internal/connector/google/
//
// GREENFIELD — EXPECTED RED: google.OutboundMessage has no Cc field, so this
// package's tests compile-FAIL until send.go gains it:
//
//	type OutboundMessage struct {
//	    From, To, Subject, Body, MessageID, InReplyTo string
//	    Cc         []string   // criterion 13; there is NO Bcc field, ever
//	    References []string
//	    Date       time.Time
//	}
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations" 9 and 10):
//   - write the Cc header BEFORE To, or with a separator other than ", " →
//     TestBuildOutboundMIME_CcHeaderFollowsTo (byte comparison).
//   - never fold → TestBuildOutboundMIME_FoldsALongCcList.
//   - write the header when the list is empty → the empty test.
//   - drop the control-character floor → the refusal test.
//   - add a Bcc field or header → TestOutboundMessage_HasNoBcc.
//   - "optimize" smtp.go to track To separately instead of parsing the built
//     MIME → TestSubmitSMTP_EnvelopeIncludesTheCc.

import (
	"context"
	"net/mail"
	"reflect"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

const (
	ccKatie   = "kevans@cecollaboratory.com"
	ccBilling = "billing@acme.example"
)

// headerLines returns the physical lines of one header field, including its
// continuation (folded) lines, without the trailing CRLFs.
func headerLines(t *testing.T, raw []byte, field string) []string {
	t.Helper()
	var out []string
	inField := false
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, field+":"):
			inField = true
			out = append(out, line)
		case inField && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")):
			out = append(out, line)
		case inField:
			return out
		}
	}
	return out
}

// Criterion 13: the Cc header is written AFTER To, addresses joined with ", ".
// Byte comparison, not a parse: "somewhere in the message" would pass on a Cc
// written into the body.
func TestBuildOutboundMIME_CcHeaderFollowsTo(t *testing.T) {
	msg := sampleOutbound("Thanks — pushed the fix to staging.")
	msg.Cc = []string{ccKatie, ccBilling}

	raw, err := google.BuildOutboundMIME(msg)
	if err != nil {
		t.Fatalf("BuildOutboundMIME with a Cc: %v", err)
	}
	want := "To: " + sendTo + "\r\nCc: " + ccKatie + ", " + ccBilling + "\r\n"
	if !strings.Contains(string(raw), want) {
		t.Errorf("built message does not carry %q immediately after To (D13: the Cc header comes after To, "+
			"addresses joined with \", \"):\n%s", want, raw)
	}

	// ...and it is a real header: the recipient's client must parse it.
	parsed, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("assembled bytes are not a parseable RFC 2822 message: %v\n%s", err, raw)
	}
	addrs, err := mail.ParseAddressList(parsed.Header.Get("Cc"))
	if err != nil {
		t.Fatalf("Cc header %q does not parse: %v", parsed.Header.Get("Cc"), err)
	}
	if len(addrs) != 2 || addrs[0].Address != ccKatie || addrs[1].Address != ccBilling {
		t.Errorf("Cc parses to %v, want [%s %s] in order", addrs, ccKatie, ccBilling)
	}
	// An address-only Cc needs no RFC 2047 encoding and no quoting (D5): the
	// header must carry no display name at all.
	for _, a := range addrs {
		if a.Name != "" {
			t.Errorf("Cc carries the display name %q; D5 stores the address only — a display name on a "+
				"client-visible header would be model-chosen (invariant 6)", a.Name)
		}
	}
}

// An empty (or absent) Cc writes NO header: an empty `Cc:` line is a header
// some clients show as a blank recipient, and it changes the envelope.
func TestBuildOutboundMIME_NoCcHeaderWhenEmpty(t *testing.T) {
	for _, tc := range []struct {
		name string
		cc   []string
	}{
		{"nil", nil},
		{"empty", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := sampleOutbound("no carbon copies here")
			msg.Cc = tc.cc
			raw, err := google.BuildOutboundMIME(msg)
			if err != nil {
				t.Fatalf("BuildOutboundMIME: %v", err)
			}
			if strings.Contains(string(raw), "Cc:") {
				t.Errorf("a delivery with no Cc still wrote a Cc header:\n%s", raw)
			}
		})
	}
}

// D13's fold: RFC 5322 §2.2.3 FWS — break at a comma, continue with CRLF + one
// space, and keep every line well under the 998-octet limit. Ten addresses is
// the maximum a row can carry (MaxCcAddresses), so this is the worst case that
// can actually reach the wire.
func TestBuildOutboundMIME_FoldsALongCcList(t *testing.T) {
	var cc []string
	for i := 0; i < 10; i++ {
		cc = append(cc, strings.Repeat("x", 20)+string(rune('a'+i))+"@cecollaboratory.com")
	}
	msg := sampleOutbound("ten people need to see this")
	msg.Cc = cc

	raw, err := google.BuildOutboundMIME(msg)
	if err != nil {
		t.Fatalf("BuildOutboundMIME with %d addresses: %v", len(cc), err)
	}
	lines := headerLines(t, raw, "Cc")
	if len(lines) == 0 {
		t.Fatal("no Cc header at all")
	}
	if len(lines) == 1 {
		t.Errorf("the Cc header is ONE line of %d characters; D13 folds at 78:\n%s", len(lines[0]), lines[0])
	}
	for i, line := range lines {
		if len(line) > 78 {
			t.Errorf("Cc header line %d is %d characters:\n%s", i, len(line), line)
		}
		if i > 0 && !strings.HasPrefix(line, " ") {
			t.Errorf("Cc continuation line %d does not begin with one space (RFC 5322 FWS): %q", i, line)
		}
		if i > 0 && strings.HasPrefix(line, "  ") {
			t.Errorf("Cc continuation line %d begins with more than one space: %q", i, line)
		}
	}

	// The fold is invisible to the reader: all ten addresses, in order.
	parsed, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("folded message does not parse: %v\n%s", err, raw)
	}
	addrs, err := mail.ParseAddressList(parsed.Header.Get("Cc"))
	if err != nil {
		t.Fatalf("folded Cc header does not parse: %v", err)
	}
	if len(addrs) != len(cc) {
		t.Fatalf("folded Cc parses to %d addresses, want %d", len(addrs), len(cc))
	}
	for i := range cc {
		if addrs[i].Address != cc[i] {
			t.Errorf("folded Cc[%d] = %q, want %q", i, addrs[i].Address, cc[i])
		}
	}
}

// The transport-level FLOOR, the shape BuildOutboundMIME already has for
// Subject (send.go:63-75): an address carrying any byte outside 0x21-0x7E is an
// ERROR, not a written header. This is the last line of defence against header
// injection if anything ever writes the column directly.
func TestBuildOutboundMIME_RefusesANonPrintableCc(t *testing.T) {
	for _, tc := range []struct {
		name string
		addr string
	}{
		{"CRLF injection", "a@x.io\r\nBcc: victim@x.io"},
		{"bare LF", "a@x.io\nX-Evil: 1"},
		{"bare CR", "a@x.io\r"},
		{"NUL", "a@x.io\x00"},
		{"tab", "a@\tx.io"},
		{"space", "a b@x.io"},
		{"non-ASCII", "josé@example.com"},
		{"empty address", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := sampleOutbound("body")
			msg.Cc = []string{ccKatie, tc.addr}
			raw, err := google.BuildOutboundMIME(msg)
			if err == nil {
				t.Fatalf("BuildOutboundMIME accepted the Cc %q and wrote:\n%s", tc.addr, raw)
			}
			if raw != nil {
				t.Errorf("a refused build returned %d bytes; it must write nothing", len(raw))
			}
		})
	}
}

// OUT OF SCOPE, permanently: a blind copy on an approve-first channel is a
// recipient the review surface cannot show honestly. No field, no header.
func TestOutboundMessage_HasNoBcc(t *testing.T) {
	typ := reflect.TypeOf(google.OutboundMessage{})
	for i := 0; i < typ.NumField(); i++ {
		if strings.Contains(strings.ToLower(typ.Field(i).Name), "bcc") {
			t.Errorf("OutboundMessage has a %s field: Bcc is out of scope and stays that way", typ.Field(i).Name)
		}
	}
	msg := sampleOutbound("body")
	msg.Cc = []string{ccKatie}
	raw, err := google.BuildOutboundMIME(msg)
	if err != nil {
		t.Fatalf("BuildOutboundMIME: %v", err)
	}
	if strings.Contains(string(raw), "Bcc:") {
		t.Errorf("the built message carries a Bcc header:\n%s", raw)
	}
}

// Criterion 14: the SMTP envelope includes the Cc addresses with NO change to
// smtp.go — recipientsFromMIME already parses To/Cc/Bcc out of the message we
// built, so the envelope can never disagree with the headers the recipient
// sees. The addresses are deduped.
func TestSubmitSMTP_EnvelopeIncludesTheCc(t *testing.T) {
	srv := newFakeSMTP(t)
	msg := sampleOutbound("Thanks — pushed the fix to staging.")
	msg.Cc = []string{ccKatie, ccBilling}
	raw, err := google.BuildOutboundMIME(msg)
	if err != nil {
		t.Fatalf("BuildOutboundMIME: %v", err)
	}

	if err := google.SubmitSMTP(context.Background(), smtpConfigFor(t, srv), sendFrom, raw); err != nil {
		t.Fatalf("SubmitSMTP: %v", err)
	}
	_, rcpt, _, _ := srv.received()
	want := []string{sendTo, ccKatie, ccBilling}
	if len(rcpt) != len(want) {
		t.Fatalf("RCPT TO = %v, want %v (To + Cc: a Cc that is not an envelope recipient never arrives)", rcpt, want)
	}
	for i := range want {
		if rcpt[i] != want[i] {
			t.Errorf("RCPT TO[%d] = %q, want %q", i, rcpt[i], want[i])
		}
	}

	// A Cc that repeats the To is ONE envelope recipient, not two (the same
	// dedupe send_delivery's D7 drop relies on at the row level).
	srv2 := newFakeSMTP(t)
	msg2 := sampleOutbound("body")
	msg2.Cc = []string{sendTo}
	raw2, err := google.BuildOutboundMIME(msg2)
	if err != nil {
		t.Fatalf("BuildOutboundMIME: %v", err)
	}
	if err := google.SubmitSMTP(context.Background(), smtpConfigFor(t, srv2), sendFrom, raw2); err != nil {
		t.Fatalf("SubmitSMTP: %v", err)
	}
	if _, rcpt2, _, _ := srv2.received(); len(rcpt2) != 1 || rcpt2[0] != sendTo {
		t.Errorf("RCPT TO = %v, want [%s] once: a Cc equal to the To must not double the envelope", rcpt2, sendTo)
	}
}
