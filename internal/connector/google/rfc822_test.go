package google_test

// Unit tests for the pure RFC822 normalizer (SPEC imap-mail-connector,
// acceptance criteria 1, 7, 10; invariant 7 discipline transfer). Input is
// EXACTLY the raw_source_items.raw_json envelope written by the ingest phase
// (criterion 5) — nothing else. ZERO network, ZERO Postgres, no IMAP
// connection: criterion 5's `--normalize-only --all` rebuild is only possible
// if this function depends on the raw row and the own-email set alone.
//
// GREENFIELD NOTE: rfc822.go does not exist yet; this file compile-FAILs under
// `go test ./...` until it does — the expected failure mode. The imposed
// surface is documented in fake_imap_test.go; the contract asserted here is
// criterion 10 verbatim:
//
//   Channel            = "gmail" (existing e-mail vocabulary; decision 2)
//   ExternalMessageID  = RFC 5322 Message-ID verbatim, brackets kept;
//                        fallback imap:{folder}:{uidvalidity}:{uid}
//   SentAt             = Date header, falling back to IMAP INTERNALDATE
//   Direction          = outbound iff From ∈ own-email set (reused verbatim,
//                        so Sent-folder copies can never be re-triaged)
//   Subject / Sender   = headers, RFC 2047 encoded-words decoded
//   BodyText           = first text/plain leaf (QP/base64 decoded, charset
//                        best-effort, unknown charset => raw bytes not an
//                        error); HTML-only => tag-stripped fallback; capped
//                        at 256 KiB
//   ThreadKey          = "gmail:{account_email}:{root}" where root =
//                        References[0] | In-Reply-To | own Message-ID |
//                        external_id — the three-segment shape is MANDATORY
//                        (tools.splitGmailThreadKey, delivery.go:228, parses it
//                        and draft_delivery/send_delivery resolve From from
//                        segment 2).

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

// ownSet is the direction rule's input: ALL provider='google' account emails.
func ownSet() map[string]bool {
	return map[string]bool{strings.ToLower(acctA): true, strings.ToLower(acctB): true}
}

var imapInternalDate = time.Date(2026, 7, 11, 9, 59, 0, 0, time.UTC)

// inboundRaw builds an INBOX envelope (uidvalidity 7, uid 42) around a message.
func inboundRaw(msg []byte) json.RawMessage {
	return newIMAPEnvelope(imapINBOX, 7, 42, imapInternalDate, []string{"\\Answered"}, msg, false)
}

// ---- criterion 10: headers, direction, thread key ----------------------------

func TestNormalizeRFC822_InboundHeadersBodyAndThreadKey(t *testing.T) {
	msg := rfc822([]string{
		`Message-ID: <c1@acme.example>`,
		`Subject: =?utf-8?Q?Staging_caf=C3=A9?=`,
		`From: =?utf-8?Q?Jos=C3=A9_Client?= <jose@acme.example>`,
		`To: ` + acctA,
		`Date: Sat, 11 Jul 2026 10:00:00 +0000`,
		`MIME-Version: 1.0`,
		`Content-Type: text/plain; charset="utf-8"`,
	}, "Ping about the staging login.")

	nm, err := google.NormalizeRFC822(inboundRaw(msg), acctA, ownSet())
	if err != nil {
		t.Fatalf("NormalizeRFC822: %v", err)
	}

	if nm.Channel != "gmail" {
		t.Errorf("Channel = %q, want gmail (criterion 10: the existing e-mail vocabulary)", nm.Channel)
	}
	if nm.Channel != google.Channel {
		t.Errorf("Channel = %q, want the package constant google.Channel = %q", nm.Channel, google.Channel)
	}
	if nm.ExternalMessageID != "<c1@acme.example>" {
		t.Errorf("ExternalMessageID = %q, want <c1@acme.example> verbatim (brackets kept)", nm.ExternalMessageID)
	}
	if nm.Direction != "inbound" {
		t.Errorf("Direction = %q, want inbound (From is a stranger)", nm.Direction)
	}
	want := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	if !nm.SentAt.Equal(want) {
		t.Errorf("SentAt = %s, want the Date header %s", nm.SentAt, want)
	}
	if nm.Subject != "Staging café" {
		t.Errorf("Subject = %q, want %q (RFC 2047 encoded-word decoded)", nm.Subject, "Staging café")
	}
	if !strings.Contains(nm.Sender, "jose@acme.example") {
		t.Errorf("Sender = %q, want it to carry the From address", nm.Sender)
	}
	if !strings.Contains(nm.Sender, "José") {
		t.Errorf("Sender = %q, want the encoded-word display name decoded (José)", nm.Sender)
	}
	if got := strings.TrimSpace(nm.BodyText); got != "Ping about the staging login." {
		t.Errorf("BodyText = %q, want the text/plain leaf", got)
	}
	if want := "gmail:" + acctA + ":<c1@acme.example>"; nm.ThreadKey != want {
		t.Errorf("ThreadKey = %q, want %q (no References/In-Reply-To => own Message-ID is the root)", nm.ThreadKey, want)
	}
	// No Gmail ids exist on the IMAP path (decision 10).
	if nm.GmailThreadID != "" {
		t.Errorf("GmailThreadID = %q, want empty (IMAP has no Gmail thread id)", nm.GmailThreadID)
	}
}

// The three-segment gmail:{email}:{x} shape is load-bearing: splitGmailThreadKey
// (internal/tools/delivery.go) resolves the sending mailbox from segment 2.
func TestNormalizeRFC822_ThreadKeyKeepsThreeSegmentGmailShape(t *testing.T) {
	msg := rfc822([]string{
		`Message-ID: <root:with:colons@acme.example>`,
		`From: client@acme.example`,
		`Subject: colonised`,
	}, "body")

	nm, err := google.NormalizeRFC822(inboundRaw(msg), acctA, ownSet())
	if err != nil {
		t.Fatalf("NormalizeRFC822: %v", err)
	}
	parts := strings.SplitN(nm.ThreadKey, ":", 3)
	if len(parts) != 3 || parts[0] != "gmail" {
		t.Fatalf("ThreadKey %q does not split as gmail:{email}:{root} (splitGmailThreadKey would reject it)", nm.ThreadKey)
	}
	if parts[1] != acctA {
		t.Errorf("ThreadKey mailbox segment = %q, want %q (draft_delivery resolves From from it)", parts[1], acctA)
	}
	if parts[2] == "" {
		t.Errorf("ThreadKey root segment is empty: %q", nm.ThreadKey)
	}
}

func TestNormalizeRFC822_ThreadRootPrecedence(t *testing.T) {
	cases := []struct {
		name     string
		headers  []string
		wantRoot string
	}{
		{
			name: "References[0] wins over In-Reply-To",
			headers: []string{
				`Message-ID: <m3@acme.example>`,
				`In-Reply-To: <m2@acme.example>`,
				`References: <m1@acme.example> <m2@acme.example>`,
				`From: client@acme.example`,
			},
			wantRoot: "<m1@acme.example>",
		},
		{
			name: "In-Reply-To when there is no References chain",
			headers: []string{
				`Message-ID: <m2@acme.example>`,
				`In-Reply-To: <m1@acme.example>`,
				`From: client@acme.example`,
			},
			wantRoot: "<m1@acme.example>",
		},
		{
			name: "own Message-ID for a thread root",
			headers: []string{
				`Message-ID: <m1@acme.example>`,
				`From: client@acme.example`,
			},
			wantRoot: "<m1@acme.example>",
		},
		{
			name:     "external_id when the message carries no ids at all",
			headers:  []string{`From: client@acme.example`},
			wantRoot: "imap:INBOX:7:42",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			nm, err := google.NormalizeRFC822(inboundRaw(rfc822(tc.headers, "body")), acctA, ownSet())
			if err != nil {
				t.Fatalf("NormalizeRFC822: %v", err)
			}
			want := "gmail:" + acctA + ":" + tc.wantRoot
			if nm.ThreadKey != want {
				t.Errorf("ThreadKey = %q, want %q", nm.ThreadKey, want)
			}
		})
	}
}

// Invariant 5: the Sent-folder copy of our own mail is OUTBOUND, so triage's
// inbound-only filter can never re-triage it into a new task. The rule is the
// existing one — From ∈ the own-email set of ALL google accounts.
func TestNormalizeRFC822_DirectionOutboundForOwnFrom(t *testing.T) {
	for _, from := range []string{
		acctA,
		strings.ToUpper(acctA),
		`"Salvador Spataro" <` + acctA + `>`,
		acctB, // a different mailbox of ours is still ours
	} {
		from := from
		t.Run(from, func(t *testing.T) {
			msg := rfc822([]string{
				`Message-ID: <sb-42-1@example.com>`,
				`From: ` + from,
				`To: client@acme.example`,
				`Subject: Re: staging login`,
			}, "our reply")
			raw := newIMAPEnvelope(imapSent, 9, 4410, imapInternalDate, []string{"\\Seen"}, msg, false)

			nm, err := google.NormalizeRFC822(raw, acctA, ownSet())
			if err != nil {
				t.Fatalf("NormalizeRFC822: %v", err)
			}
			if nm.Direction != "outbound" {
				t.Errorf("Direction = %q for From %q, want outbound (own-email set)", nm.Direction, from)
			}
		})
	}
}

func TestNormalizeRFC822_SentAtFallsBackToInternalDate(t *testing.T) {
	msg := rfc822([]string{
		`Message-ID: <nodate@acme.example>`,
		`From: client@acme.example`,
	}, "body")

	nm, err := google.NormalizeRFC822(inboundRaw(msg), acctA, ownSet())
	if err != nil {
		t.Fatalf("NormalizeRFC822: %v", err)
	}
	if !nm.SentAt.Equal(imapInternalDate) {
		t.Errorf("SentAt = %s, want the IMAP INTERNALDATE %s (no Date header)", nm.SentAt, imapInternalDate)
	}
}

func TestNormalizeRFC822_ExternalMessageIDFallsBackToExternalID(t *testing.T) {
	msg := rfc822([]string{`From: client@acme.example`, `Subject: no message id`}, "body")

	nm, err := google.NormalizeRFC822(inboundRaw(msg), acctA, ownSet())
	if err != nil {
		t.Fatalf("NormalizeRFC822: %v", err)
	}
	if nm.ExternalMessageID != "imap:INBOX:7:42" {
		t.Errorf("ExternalMessageID = %q, want the imap:{folder}:{uidvalidity}:{uid} fallback", nm.ExternalMessageID)
	}
}

// ---- criterion 10: body extraction ------------------------------------------

func TestNormalizeRFC822_BodyDecoding(t *testing.T) {
	t.Run("quoted-printable text/plain", func(t *testing.T) {
		msg := rfc822([]string{
			`Message-ID: <qp@acme.example>`,
			`From: client@acme.example`,
			`Content-Type: text/plain; charset="utf-8"`,
			`Content-Transfer-Encoding: quoted-printable`,
		}, "Invoice total: 50=E2=82=AC due Friday")

		nm, err := google.NormalizeRFC822(inboundRaw(msg), acctA, ownSet())
		if err != nil {
			t.Fatalf("NormalizeRFC822: %v", err)
		}
		if !strings.Contains(nm.BodyText, "50€") {
			t.Errorf("BodyText = %q, want the quoted-printable decoded (50€)", nm.BodyText)
		}
	})

	t.Run("base64 text/plain", func(t *testing.T) {
		msg := rfc822([]string{
			`Message-ID: <b64@acme.example>`,
			`From: client@acme.example`,
			`Content-Type: text/plain; charset="utf-8"`,
			`Content-Transfer-Encoding: base64`,
		}, base64.StdEncoding.EncodeToString([]byte("Base64 body text.")))

		nm, err := google.NormalizeRFC822(inboundRaw(msg), acctA, ownSet())
		if err != nil {
			t.Fatalf("NormalizeRFC822: %v", err)
		}
		if got := strings.TrimSpace(nm.BodyText); got != "Base64 body text." {
			t.Errorf("BodyText = %q, want the base64-decoded text", got)
		}
	})

	t.Run("multipart/alternative walks to the text/plain leaf", func(t *testing.T) {
		body := strings.Join([]string{
			"--bnd42",
			`Content-Type: text/html; charset="utf-8"`,
			"",
			"<p>html loses</p>",
			"--bnd42",
			`Content-Type: text/plain; charset="utf-8"`,
			"",
			"plain wins",
			"--bnd42--",
			"",
		}, "\r\n")
		msg := rfc822([]string{
			`Message-ID: <mp@acme.example>`,
			`From: client@acme.example`,
			`MIME-Version: 1.0`,
			`Content-Type: multipart/alternative; boundary="bnd42"`,
		}, body)

		nm, err := google.NormalizeRFC822(inboundRaw(msg), acctA, ownSet())
		if err != nil {
			t.Fatalf("NormalizeRFC822: %v", err)
		}
		if got := strings.TrimSpace(nm.BodyText); got != "plain wins" {
			t.Errorf("BodyText = %q, want the first text/plain leaf (%q)", got, "plain wins")
		}
	})

	t.Run("html-only falls back to a tag-stripped body", func(t *testing.T) {
		msg := rfc822([]string{
			`Message-ID: <html@acme.example>`,
			`From: client@acme.example`,
			`Content-Type: text/html; charset="utf-8"`,
		}, "<html><body><p>Hello&nbsp;<b>world</b> &amp; friends</p></body></html>")

		nm, err := google.NormalizeRFC822(inboundRaw(msg), acctA, ownSet())
		if err != nil {
			t.Fatalf("NormalizeRFC822: %v", err)
		}
		if strings.Contains(nm.BodyText, "<p>") || strings.Contains(nm.BodyText, "<body") {
			t.Errorf("BodyText = %q, want HTML tags stripped", nm.BodyText)
		}
		for _, want := range []string{"Hello", "world", "& friends"} {
			if !strings.Contains(nm.BodyText, want) {
				t.Errorf("BodyText = %q, want it to contain %q (entities unescaped)", nm.BodyText, want)
			}
		}
	})

	t.Run("unknown charset yields raw bytes, never a hard error", func(t *testing.T) {
		msg := rfc822([]string{
			`Message-ID: <charset@acme.example>`,
			`From: client@acme.example`,
			`Content-Type: text/plain; charset="x-unknown-9000"`,
		}, "legacy \xff\xfe bytes")

		nm, err := google.NormalizeRFC822(inboundRaw(msg), acctA, ownSet())
		if err != nil {
			t.Fatalf("NormalizeRFC822 must not fail on an unknown charset: %v", err)
		}
		if !strings.Contains(nm.BodyText, "legacy") {
			t.Errorf("BodyText = %q, want the raw bytes preserved best-effort", nm.BodyText)
		}
	})

	t.Run("body is capped at 256 KiB", func(t *testing.T) {
		msg := rfc822([]string{
			`Message-ID: <huge@acme.example>`,
			`From: client@acme.example`,
			`Content-Type: text/plain; charset="utf-8"`,
		}, strings.Repeat("a", 300*1024))

		nm, err := google.NormalizeRFC822(inboundRaw(msg), acctA, ownSet())
		if err != nil {
			t.Fatalf("NormalizeRFC822: %v", err)
		}
		if len(nm.BodyText) > 256*1024 {
			t.Errorf("len(BodyText) = %d, want <= %d (criterion 10 cap)", len(nm.BodyText), 256*1024)
		}
		if len(nm.BodyText) == 0 {
			t.Errorf("BodyText is empty; the cap truncates, it does not drop the body")
		}
	})
}

// ---- criterion 7: the truncated (headers-only) capture still normalizes -------

func TestNormalizeRFC822_TruncatedHeadersOnlyStillNormalizes(t *testing.T) {
	headers := rfc822Headers([]string{
		`Message-ID: <big@acme.example>`,
		`In-Reply-To: <root@acme.example>`,
		`Subject: 12MB of holiday photos`,
		`From: client@acme.example`,
		`Date: Sat, 11 Jul 2026 10:00:00 +0000`,
	})
	raw := newIMAPEnvelope(imapINBOX, 7, 99, imapInternalDate, []string{}, headers, true)

	nm, err := google.NormalizeRFC822(raw, acctA, ownSet())
	if err != nil {
		t.Fatalf("NormalizeRFC822 (truncated): %v", err)
	}
	if nm.ExternalMessageID != "<big@acme.example>" {
		t.Errorf("ExternalMessageID = %q, want the header value (headers survive truncation)", nm.ExternalMessageID)
	}
	if nm.Subject != "12MB of holiday photos" {
		t.Errorf("Subject = %q, want it preserved on a truncated capture", nm.Subject)
	}
	if nm.Direction != "inbound" {
		t.Errorf("Direction = %q, want inbound", nm.Direction)
	}
	if want := "gmail:" + acctA + ":<root@acme.example>"; nm.ThreadKey != want {
		t.Errorf("ThreadKey = %q, want %q (threading survives truncation)", nm.ThreadKey, want)
	}
	if nm.BodyText != "" {
		t.Errorf("BodyText = %q, want empty for a headers-only capture", nm.BodyText)
	}
}

// ---- error + determinism ------------------------------------------------------

func TestNormalizeRFC822_RejectsUndecodableEnvelope(t *testing.T) {
	raw := json.RawMessage(`{"source":"imap","folder":"INBOX","uidvalidity":7,"uid":42,` +
		`"internaldate":"2026-07-11T09:59:00Z","flags":[],"size":3,"truncated":false,"rfc822_b64":"not base64!!"}`)

	if _, err := google.NormalizeRFC822(raw, acctA, ownSet()); err == nil {
		t.Fatal("NormalizeRFC822 accepted an undecodable rfc822_b64; want an error")
	}
}

func TestNormalizeRFC822_Deterministic(t *testing.T) {
	msg := rfc822([]string{
		`Message-ID: <det@acme.example>`,
		`References: <r1@acme.example> <r2@acme.example>`,
		`Subject: determinism`,
		`From: client@acme.example`,
		`Date: Sat, 11 Jul 2026 10:00:00 +0000`,
		`Content-Type: text/plain; charset="utf-8"`,
	}, "same in, same out")
	raw := inboundRaw(msg)

	first, err := google.NormalizeRFC822(raw, acctA, ownSet())
	if err != nil {
		t.Fatalf("NormalizeRFC822 (1): %v", err)
	}
	second, err := google.NormalizeRFC822(raw, acctA, ownSet())
	if err != nil {
		t.Fatalf("NormalizeRFC822 (2): %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("NormalizeRFC822 is not deterministic:\n%+v\n%+v", first, second)
	}
}

// Regression, found by the first live ingest against a real mailbox: a latin-1
// (c) at byte 0xa9 in a body with no usable charset aborted the whole normalize
// pass with `invalid byte sequence for encoding "UTF8"` when Postgres refused the
// INSERT. The unit tests all used a fake sink, so nothing had ever written one of
// these strings to a real TEXT column.
//
// Returning raw bytes on an unknown charset is still right for a mail parser —
// the repair belongs at the boundary, and it interprets stray bytes as latin-1
// rather than substituting U+FFFD, which would corrupt every accented word.
func TestNormalizeRFC822_RepairsInvalidUTF8ForPostgres(t *testing.T) {
	// 0xa9 is © in latin-1 and is not valid UTF-8 on its own.
	msg := rfc822([]string{
		`Message-ID: <latin1@acme.example>`,
		`From: client@acme.example`,
		`Subject: copyright \xa9 2026`,
		`Content-Type: text/plain; charset="x-unknown-9000"`,
	}, "Terms \xa9 2026 Acme, all rights reserved.")

	nm, err := google.NormalizeRFC822(inboundRaw(msg), acctA, ownSet())
	if err != nil {
		t.Fatalf("NormalizeRFC822: %v", err)
	}
	for name, field := range map[string]string{
		"BodyText": nm.BodyText, "Subject": nm.Subject, "Sender": nm.Sender,
	} {
		if !utf8.ValidString(field) {
			t.Errorf("%s is not valid UTF-8 (%q); Postgres rejects it and the whole "+
				"normalize pass aborts on the INSERT", name, field)
		}
	}
	if !strings.Contains(nm.BodyText, "©") {
		t.Errorf("BodyText = %q, want the 0xa9 byte read as latin-1 © rather than replaced", nm.BodyText)
	}
}
