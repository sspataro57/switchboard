package google_test

// SWT-54 (docs/tickets/treetop-pr-review-tasks_SPEC.md) D1 and criterion 19:
// the header reader over a STORED raw envelope. Authorship evidence is read
// from raw_source_items.raw_json.rfc822_b64 headers, never from
// normalized_messages — no X-GitHub-* header reaches that table, and
// NormalizeRFC822's output must not change to carry one (IK "STANDING RULE";
// the byte-identity half stays pinned by bodytext_golden_test.go). ZERO I/O.
//
// GREENFIELD NOTE — EXPECTED RED: google.RawMailHeaders does not exist, so this
// file compile-FAILS the package's external tests.
//
// IMPOSED SURFACE (the SPEC's own spelling):
//
//	func RawMailHeaders(raw json.RawMessage) (mail.Header, bool, error)
//
//	ok=false, err=nil  — a row that is not an IMAP envelope (Gmail API / bridge
//	                     shaped, or the '{}' raw_json every other integration
//	                     suite writes): it contributes nothing, and it must never
//	                     fail a capture pass.
//	err != nil         — an IMAP envelope whose bytes cannot be decoded.
//
// A TRUNCATED capture still has its headers: the IMAP connector keeps headers
// plus text parts over MAIL_MAX_MESSAGE_BYTES (IK "Mail attachments over MCP"),
// so walkEnvelope's "truncated means nothing to read" shortcut must NOT apply
// here — the headers are exactly what survived.

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

const rhGitHubMail = "From: joseg-avviato <notifications@github.com>\r\n" +
	"To: treetopllc/collaboratory-www <collaboratory-www@noreply.github.com>\r\n" +
	"Subject: [treetopllc/collaboratory-www] Ranking widget (PR #3179)\r\n" +
	"Message-ID: <treetopllc/collaboratory-www/pull/3179@github.com>\r\n" +
	"Date: Mon, 14 Sep 2026 12:00:00 +0000\r\n" +
	"X-GitHub-Reason: review_requested\r\n" +
	"X-GitHub-Sender: joseg-avviato\r\n" +
	"X-GitHub-Recipient: sspataro57\r\n" +
	"Content-Type: text/plain; charset=UTF-8\r\n" +
	"\r\n" +
	"joseg-avviato requested your review on: treetopllc/collaboratory-www#3179 Ranking widget.\r\n"

// rhEnvelope is the imapRawEnvelope shape buildIMAPEnvelope writes (the
// attachments_test.go attEnvelope precedent).
func rhEnvelope(t *testing.T, msg string, truncated bool) json.RawMessage {
	t.Helper()
	env := map[string]any{
		"source": "imap", "folder": "INBOX", "uidvalidity": 73, "uid": 90001,
		"internaldate": "2026-09-14T12:00:01Z", "flags": []string{}, "size": len(msg),
		"truncated": truncated, "rfc822_b64": base64.StdEncoding.EncodeToString([]byte(msg)),
	}
	if truncated {
		env["size"] = 9 << 20
		env["parts"] = []map[string]any{{"part_id": "2", "content_type": "application/pdf", "filename": "big.pdf", "size": 9 << 20}}
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return raw
}

func TestRawMailHeaders_IMAPEnvelopeYieldsTheGitHubHeaders(t *testing.T) {
	h, ok, err := google.RawMailHeaders(rhEnvelope(t, rhGitHubMail, false))
	if err != nil || !ok {
		t.Fatalf("RawMailHeaders(imap envelope) = ok %v, err %v; want ok=true, err=nil", ok, err)
	}
	for name, want := range map[string]string{
		"X-GitHub-Reason":    "review_requested",
		"X-GitHub-Sender":    "joseg-avviato",
		"X-GitHub-Recipient": "sspataro57",
		"Message-ID":         "<treetopllc/collaboratory-www/pull/3179@github.com>",
	} {
		if got := h.Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
}

func TestRawMailHeaders_TruncatedCaptureStillHasItsHeaders(t *testing.T) {
	h, ok, err := google.RawMailHeaders(rhEnvelope(t, rhGitHubMail, true))
	if err != nil || !ok {
		t.Fatalf("RawMailHeaders(truncated imap envelope) = ok %v, err %v; want ok=true — a truncated row keeps "+
			"headers plus text parts, only attachments were left behind", ok, err)
	}
	if got := h.Get("X-GitHub-Sender"); got != "joseg-avviato" {
		t.Errorf("X-GitHub-Sender on a truncated capture = %q, want joseg-avviato", got)
	}
}

// A row with no rfc822_b64 contributes nothing (D1 "Scope of the read"), and
// the '{}' raw_json is what every other integration suite stores, so a pass
// over a shared corpus must see ok=false rather than an error.
func TestRawMailHeaders_NonIMAPRowIsNotOKAndNotAnError(t *testing.T) {
	for name, raw := range map[string]string{
		"gmail API shape (headers present, but not an RFC822)": `{"id":"18f0a1","threadId":"18f0a1","payload":` +
			`{"headers":[{"name":"X-GitHub-Reason","value":"author"}]}}`,
		"bridge shape": `{"source":"bridge","id":"18f0a2","snippet":"hi"}`,
		"empty object": `{}`,
	} {
		h, ok, err := google.RawMailHeaders(json.RawMessage(raw))
		if err != nil {
			t.Errorf("%s: RawMailHeaders err = %v, want nil — a gmail:-shaped row must never fail the pass", name, err)
		}
		if ok {
			t.Errorf("%s: RawMailHeaders ok = true (headers %v), want false: only an IMAP envelope carries the "+
				"RFC822 the X-GitHub-* headers are read from", name, h)
		}
	}
}

func TestRawMailHeaders_UndecodableIsAnError(t *testing.T) {
	for name, raw := range map[string]string{
		"not JSON":       `not json at all`,
		"bad base64":     `{"source":"imap","truncated":false,"rfc822_b64":"%%%not-base64%%%"}`,
		"truncated JSON": `{"source":"imap","rfc822_b64":"`,
	} {
		if _, ok, err := google.RawMailHeaders(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: RawMailHeaders err = nil (ok=%v), want an error", name, ok)
		}
	}
}
