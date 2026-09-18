package google_test

// Offline unit tests for the XOAUTH2 SASL mechanism (SPEC
// docs/tickets/microsoft-oauth-mail_SPEC.md, acceptance criteria 2 and 3;
// decision D5, invariant 7 "the XOAUTH2 initial response is a pure function").
//
// Zero network, zero Postgres, zero clock: the mechanism is a byte builder plus
// a one-frame state machine, which is the whole reason D5 implements it in-repo
// instead of bumping go-sasl (neither the pinned nor the current version has it).
//
// EXPECTED FAILURE MODE: **compile error**. internal/connector/google/xoauth2.go
// does not exist, so `go test ./internal/connector/google/` fails with
// "undefined: google.XOAuth2Client" until it does.
//
// IMPOSED SURFACE (the SPEC pins the constructor and the behaviour; the accessor
// name is this file's choice — rename here if the implementation spells it
// differently, but SOMETHING must expose the recorded challenge or criterion 3's
// "connect can include its status/scope in the returned error" is unreachable):
//
//	// XOAuth2Client returns the SASL mechanism Outlook requires. It performs no
//	// I/O: Start builds one byte string, Next records one frame.
//	func XOAuth2Client(username, accessToken string) *XOAuth2
//
//	type XOAuth2 struct{ ... }                       // implements sasl.Client
//	func (c *XOAuth2) Start() (mech string, ir []byte, err error)
//	func (c *XOAuth2) Next(challenge []byte) ([]byte, error)
//	// Challenge returns the server's failure frame as the mechanism received it
//	// (go-imap base64-DECODES the continuation before calling Next, so this is
//	// the JSON text, not the base64), or "" if the exchange never failed.
//	func (c *XOAuth2) Challenge() string

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/emersion/go-sasl"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

// The mechanism must satisfy the interface go-imap's (*client.Client).Authenticate
// takes; anything else means imap.go cannot use it at all.
var _ sasl.Client = google.XOAuth2Client("u@example.com", "tok")

const (
	// One fixed pair, pinned for all time. Changing either side of this test
	// changes what goes on the wire to Microsoft.
	msxUser  = "sspataro57@msn.com"
	msxToken = "ya29.FAKE-ACCESS-TOKEN"

	// base64 of: "user=" + msxUser + 0x01 + "auth=Bearer " + msxToken + 0x01 + 0x01
	// (criterion 2, verbatim). Computed independently of the implementation.
	msxWantIR = "dXNlcj1zc3BhdGFybzU3QG1zbi5jb20BYXV0aD1CZWFyZXIgeWEyOS5GQUtFLUFDQ0VTUy1UT0tFTgEB"
)

// wantIRBytes builds the expected initial response from its parts rather than
// pasting a literal with escape sequences: the Write tool decodes \u/\U (IK
// landmine), and a control byte typed into a source file is exactly the class of
// mistake that produces an invisible diff.
func wantIRBytes() []byte {
	const sepByte = byte(0x01)
	var b bytes.Buffer
	b.WriteString("user=" + msxUser)
	b.WriteByte(sepByte)
	b.WriteString("auth=Bearer " + msxToken)
	b.WriteByte(sepByte)
	b.WriteByte(sepByte)
	return b.Bytes()
}

// ---- criterion 2: the exact initial-response bytes ----------------------------

func TestXOAuth2Client_StartPinsTheInitialResponseBytes(t *testing.T) {
	mech, ir, err := google.XOAuth2Client(msxUser, msxToken).Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if mech != "XOAUTH2" {
		t.Errorf("mechanism = %q, want XOAUTH2 — it is the only mechanism "+
			"outlook.office365.com:993 advertises (the SPEC's measured CAPABILITY line)", mech)
	}

	want := wantIRBytes()
	if !bytes.Equal(ir, want) {
		t.Errorf("initial response bytes = %q, want %q\n"+
			"criterion 2: \"user=\" + username, one 0x01, \"auth=Bearer \" + token, TWO 0x01 — "+
			"one trailing 0x01 (the common mistake) makes Outlook answer NO with no useful reason", ir, want)
	}
	if got := base64.StdEncoding.EncodeToString(ir); got != msxWantIR {
		t.Errorf("base64(initial response) = %q, want %q (criterion 2 pins this string for the fixed pair %s / %s)",
			got, msxWantIR, msxUser, msxToken)
	}
	// Byte-level belt: the separator must be 0x01 and never a space, a colon or
	// a newline, and there must be exactly three of them in the whole string.
	if n := bytes.Count(ir, []byte{0x01}); n != 3 {
		t.Errorf("0x01 count = %d, want 3 (one separator plus the two terminators)", n)
	}
	if bytes.HasSuffix(ir, []byte("\r\n")) {
		t.Errorf("the initial response carries a line ending; go-imap writes the CRLF itself")
	}
}

// ---- criterion 3: an auth failure must NOT abort the exchange ------------------

func TestXOAuth2Client_NextReturnsAnEmptyResponseAndRecordsTheChallenge(t *testing.T) {
	// What Outlook sends on a bad/expired token: a continuation carrying base64
	// JSON. go-imap decodes it before handing it to Next.
	const challengeJSON = `{"status":"401","schemes":"Bearer","scope":"https://outlook.office.com/IMAP.AccessAsUser.All"}`

	c := google.XOAuth2Client(msxUser, msxToken)
	if _, _, err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	reply, err := c.Next([]byte(challengeJSON))
	if err != nil {
		t.Fatalf("Next returned an error (%v). Criterion 3: it must return an EMPTY response instead — "+
			"go-imap's responses.Authenticate cancels the exchange with \"*\" when Next errors, so the "+
			"server's tagged NO never arrives and the failure surfaces as a cancelled command with no cause", err)
	}
	if len(reply) != 0 {
		t.Errorf("Next returned %q, want an empty response (the XOAUTH2 failure frame is answered with an "+
			"empty line, which is what lets the server send its tagged NO)", reply)
	}

	if got := c.Challenge(); got != challengeJSON {
		t.Errorf("Challenge() = %q, want the frame as received %q. Without recording it, connect can only "+
			"report \"AUTHENTICATE failed\" — criterion 3 says that does not satisfy it, because it cannot "+
			"tell a revoked token (status 401) from a missing scope", got, challengeJSON)
	}
	for _, want := range []string{"401", "IMAP.AccessAsUser.All"} {
		if !strings.Contains(c.Challenge(), want) {
			t.Errorf("recorded challenge %q does not carry %q, which is what connect's error must name", c.Challenge(), want)
		}
	}

	// A mechanism that never saw a challenge must say so rather than inventing one.
	if got := google.XOAuth2Client(msxUser, msxToken).Challenge(); got != "" {
		t.Errorf("Challenge() before any exchange = %q, want empty", got)
	}
}

// A second Next (the server keeps talking) must stay quiet rather than erroring:
// the exchange is over and only the tagged reply matters.
func TestXOAuth2Client_NextIsIdempotentlyEmpty(t *testing.T) {
	c := google.XOAuth2Client(msxUser, msxToken)
	if _, _, err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := c.Next([]byte(`{"status":"401"}`)); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	reply, err := c.Next([]byte(`{"status":"401"}`))
	if err != nil {
		t.Fatalf("second Next: %v (an extra server frame must not turn into a client error)", err)
	}
	if len(reply) != 0 {
		t.Errorf("second Next returned %q, want empty", reply)
	}
}
