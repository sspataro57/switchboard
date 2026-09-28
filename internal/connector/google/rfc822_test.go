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

// swb 760: Lyle's Outlook mail arrived as Subject
// =?Windows-1252?Q?Re:_M5-M6_Package_3_=97_delivered?= with a Windows-1252
// quoted-printable body, and was stored with U+0097 (a C1 control) where the
// em dash was. Grady's arrived as =?big5?B?...?= with a big5 body, and "—"
// (big5 A1 58) became "¡X". Both are decoded by their declared charset now.
func TestNormalizeRFC822_DecodesTheDeclaredCharset(t *testing.T) {
	lyle := []byte("Message-ID: <lyle@foundry.example>\r\n" +
		"From: Lyle <lyle@foundry.example>\r\n" +
		"Subject: =?Windows-1252?Q?Re:_M5-M6_Package_3_=97_delivered?=\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=\"Windows-1252\"\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"Package 3 is accepted =96 thanks. =93Qualified=94 items=85\r\n")
	nm, err := google.NormalizeRFC822(inboundRaw(lyle), acctA, ownSet())
	if err != nil {
		t.Fatalf("NormalizeRFC822: %v", err)
	}
	if nm.Subject != "Re: M5-M6 Package 3 — delivered" {
		t.Errorf("Subject = %q, want the em dash", nm.Subject)
	}
	if !strings.Contains(nm.BodyText, "accepted – thanks. “Qualified” items…") {
		t.Errorf("BodyText = %q, want windows-1252 punctuation decoded", nm.BodyText)
	}

	// Big5, base64 encoded-word and body: "UX preview — full" / "a — b".
	big5Subject := "=?big5?B?" + base64.StdEncoding.EncodeToString([]byte("UX preview \xa1\x58 full")) + "?="
	grady := []byte("Message-ID: <grady@foundry.example>\r\n" +
		"From: Grady <grady@foundry.example>\r\n" +
		"Subject: " + big5Subject + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=\"big5\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString([]byte("a \xa1\x58 b")) + "\r\n")
	nm, err = google.NormalizeRFC822(inboundRaw(grady), acctA, ownSet())
	if err != nil {
		t.Fatalf("NormalizeRFC822 big5: %v", err)
	}
	if nm.Subject != "UX preview — full" {
		t.Errorf("big5 Subject = %q, want \"UX preview — full\"", nm.Subject)
	}
	if !strings.Contains(nm.BodyText, "a — b") {
		t.Errorf("big5 BodyText = %q, want \"a — b\"", nm.BodyText)
	}
}

// A UTF-8 body is never transcoded, not even to repair it: our own sends are
// UTF-8 and confirmDeliveryByBodyPrefix compares their ingested copies byte for
// byte (IK standing rule on body_text).
func TestNormalizeRFC822_UTF8BodyIsByteIdentical(t *testing.T) {
	body := "Paid today — thanks “a” … ok"
	msg := rfc822([]string{
		`Message-ID: <utf8@acme.example>`,
		`From: client@acme.example`,
		`Subject: =?UTF-8?Q?Paid_=E2=80=94_today?=`,
		`Content-Type: text/plain; charset="UTF-8"`,
	}, body)
	nm, err := google.NormalizeRFC822(inboundRaw(msg), acctA, ownSet())
	if err != nil {
		t.Fatalf("NormalizeRFC822: %v", err)
	}
	if !strings.HasPrefix(nm.BodyText, body) || nm.Subject != "Paid — today" {
		t.Errorf("UTF-8 changed: subject %q body %q", nm.Subject, nm.BodyText)
	}
}

// An undeclared or unknown charset's stray 0x80-0x9F bytes read as
// windows-1252 punctuation, never as C1 controls.
func TestNormalizeRFC822_StrayHighBytesReadAsWindows1252(t *testing.T) {
	msg := rfc822([]string{
		`Message-ID: <stray@acme.example>`,
		`From: client@acme.example`,
		"Subject: done \x97 shipped",
		`Content-Type: text/plain; charset="x-unknown-9000"`,
	}, "it\x92s done")
	nm, err := google.NormalizeRFC822(inboundRaw(msg), acctA, ownSet())
	if err != nil {
		t.Fatalf("NormalizeRFC822: %v", err)
	}
	if nm.Subject != "done — shipped" || !strings.Contains(nm.BodyText, "it’s done") {
		t.Errorf("subject %q body %q, want windows-1252 punctuation", nm.Subject, nm.BodyText)
	}
	for _, r := range nm.Subject + nm.BodyText {
		if r >= 0x80 && r <= 0x9f {
			t.Errorf("C1 control %U survived", r)
		}
	}
}

// A sender that declares iso-8859-1 but sends UTF-8 must not be decoded twice:
// "café — ok" would become "cafÃ© â€” ok" (swb 760 prod dry run).
func TestNormalizeRFC822_MislabelledUTF8StaysUTF8(t *testing.T) {
	msg := rfc822([]string{
		`Message-ID: <mislabel@acme.example>`,
		`From: client@acme.example`,
		`Subject: =?windows-1252?Q?caf=C3=A9_=E2=80=94_ok?=`, // iso-8859-1 words are decoded by mime itself, never by our CharsetReader
		`Content-Type: text/plain; charset="iso-8859-1"`,
	}, "café — ok")
	nm, err := google.NormalizeRFC822(inboundRaw(msg), acctA, ownSet())
	if err != nil {
		t.Fatalf("NormalizeRFC822: %v", err)
	}
	if nm.Subject != "café — ok" || !strings.HasPrefix(nm.BodyText, "café — ok") {
		t.Errorf("subject %q body %q, want the UTF-8 kept", nm.Subject, nm.BodyText)
	}
}

// Review round 1, swb 760: mime decodes iso-8859-1 words itself, bypassing the
// CharsetReader, so an Outlook dash in one came back as U+0097.
func TestNormalizeRFC822_Iso88591WordC1ReadsAsWindows1252(t *testing.T) {
	msg := rfc822([]string{
		`Message-ID: <iso@acme.example>`,
		`From: client@acme.example`,
		`Subject: =?ISO-8859-1?Q?Package_3_=97_delivered?=`,
	}, "x")
	nm, err := google.NormalizeRFC822(inboundRaw(msg), acctA, ownSet())
	if err != nil {
		t.Fatalf("NormalizeRFC822: %v", err)
	}
	if nm.Subject != "Package 3 — delivered" {
		t.Errorf("Subject = %q, want the em dash", nm.Subject)
	}
}

// A mislabelled UTF-8 body over the cap, cut mid-rune, stays UTF-8: one broken
// tail must not send 256 KiB through windows-1252.
func TestNormalizeRFC822_CappedMislabelledUTF8StaysUTF8(t *testing.T) {
	unit := "é" // 2 bytes
	// decodeBody reads maxBodyTextBytes+1 bytes: the last one is é's first byte.
	body := unit + strings.Repeat("a", 256*1024-2) + unit + "tail" // an early é shows any transcode
	msg := rfc822([]string{
		`Message-ID: <cap@acme.example>`,
		`From: client@acme.example`,
		`Subject: cap`,
		`Content-Type: text/plain; charset="windows-1252"`,
	}, body)
	nm, err := google.NormalizeRFC822(inboundRaw(msg), acctA, ownSet())
	if err != nil {
		t.Fatalf("NormalizeRFC822: %v", err)
	}
	if !strings.HasPrefix(nm.BodyText, "éaaa") || !utf8.ValidString(nm.BodyText) {
		t.Errorf("capped body was transcoded: head %q", nm.BodyText[:8])
	}
}

// Replacement-class labels would collapse an 8-bit body to one U+FFFD, and
// x-user-defined maps to private-use runes: both keep the raw-bytes repair.
func TestNormalizeRFC822_OddLabelsKeepTheRawRepair(t *testing.T) {
	for _, label := range []string{"iso-2022-kr", "x-user-defined"} {
		msg := rfc822([]string{
			`Message-ID: <odd@acme.example>`,
			`From: client@acme.example`,
			`Subject: odd`,
			`Content-Type: text/plain; charset="` + label + `"`,
		}, "caf\xe9 \x97 ok")
		nm, err := google.NormalizeRFC822(inboundRaw(msg), acctA, ownSet())
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if !strings.HasPrefix(nm.BodyText, "café — ok") {
			t.Errorf("%s: body %q, want the windows-1252 repair of the raw bytes", label, nm.BodyText)
		}
	}
}
