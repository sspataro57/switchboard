package google_test

// SWT-25 (link-preservation) criterion 10: `body_text` comes out BYTE-IDENTICAL.
//
// This is the one file in this ticket that is GREEN BEFORE the implementation
// and must STAY green after it. It is a golden lock, not a red-first test: every
// `want` below was produced by running the CURRENT normalizer (before any link
// extraction existed) and pasted verbatim. Its whole purpose is to fail if the
// refactor that hands the HTML part to the extractor changes one byte of what
// `extractBodyText` returns.
//
// WHY THAT MATTERS, and why the failure message repeats it (premise 5):
// `PGSink.confirmDeliveryByBodyPrefix` (sink.go:542-543) identifies OUR OWN
// sends post hoc by `textmatch.NormalizedPrefix(nm.BodyText, 120)`, compared
// against the body stored on the delivery row. **google has no reconciler** —
// institutional knowledge, "the four matchers": a refusal there is SILENT. So a
// body_text that shifts by one space does not raise, does not log, and does not
// retry; it leaves the delivery permanently unconfirmable with `sent_external_id`
// NULL, and (since SWT-16) makes capture log `outbound_observed` — a false claim
// that switchboard's own mail was sent by hand.
//
// Corollary, in scope terms: `stripHTML` is NOT rewritten in this ticket. Not
// "improved", not moved onto the tokenizer. If it is ever worth doing it is its
// own ticket with its own before/after over the 49k-row corpus.
//
// The fixtures are chosen to cover every branch `extractBodyText` can take:
// text/plain direct, quoted-printable, base64, multipart/alternative (where the
// HTML part is DISCARDED and the extractor must read it anyway — criterion 8),
// html-only (the stripHTML fallback), the tracking-pixel-only shape, a 30-anchor
// marketing mail, and a latin-1 repair. Fixture-level coverage is not production
// coverage: the production check is the md5 digest over `normalized_messages`
// before and after the backfill (verification protocol step 4), and this file
// does not claim to replace it.

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

// lpGolden is one fixture: the raw message bytes and the EXACT BodyText the
// pre-SWT-25 normalizer produced for it.
type lpGolden struct {
	name string
	msg  []byte
	want string
}

// lpHTMLTrackingPixelOnly is the Pines First Notice shape: a single img pointing
// at SendGrid's /wf/open beacon, and NO anchors at all. Shared with links_test.go
// (criterion 2) so the two tests speak about the same message.
const lpHTMLTrackingPixelOnly = `<html><body>` +
	`<p>Dear Homeowner,</p>` +
	`<p>Please see the attached First Notice regarding your property.</p>` +
	`<img src="https://u1234.ct.sendgrid.net/wf/open?upn=abc123" alt="" width="1" height="1" border="0" />` +
	`</body></html>`

// lpHTMLAlternative is a templated notice that ships BOTH a text/plain body and
// an HTML alternative carrying two anchors — the common case premise 2 names,
// where walkForText's html return value is thrown away.
const lpHTMLAlternative = `<html><body>` +
	`<p>Your account statement is ready.</p>` +
	`<p><a href="https://portal.pinespropertymanagement.com/account/12345">VIEW ACCOUNT</a></p>` +
	`<p><a href="https://portal.pinespropertymanagement.com/pay">PAY NOW</a></p>` +
	`<p><a href="https://lists.example.com/unsubscribe?u=9">Unsubscribe</a></p>` +
	`</body></html>`

// lpMarketing30 builds a marketing mail with 30 distinct anchors (criterion 6's
// cap fixture) — as a body it also exercises stripHTML over a large document.
func lpMarketing30() string {
	var b strings.Builder
	b.WriteString(`<html><body><h1>September deals</h1>`)
	for i := 1; i <= 30; i++ {
		b.WriteString(`<p><a href="https://shop.example.com/product/`)
		b.WriteString(string(rune('a' + (i-1)%26)))
		b.WriteString(itoaSmall(i))
		b.WriteString(`">Deal number `)
		b.WriteString(itoaSmall(i))
		b.WriteString(`</a></p>`)
	}
	b.WriteString(`</body></html>`)
	return b.String()
}

// itoaSmall avoids pulling strconv into a fixture builder for two digits.
func itoaSmall(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

func lpGoldens() []lpGolden {
	multipartBody := strings.Join([]string{
		"--lp42",
		`Content-Type: text/plain; charset="utf-8"`,
		"",
		"Your account statement is ready.",
		"View it in the portal.",
		"--lp42",
		`Content-Type: text/html; charset="utf-8"`,
		"",
		lpHTMLAlternative,
		"--lp42--",
		"",
	}, "\r\n")

	return []lpGolden{
		{
			name: "text/plain direct",
			msg: rfc822([]string{
				`Message-ID: <lp-plain@acme.example>`,
				`From: client@acme.example`,
				`Content-Type: text/plain; charset="utf-8"`,
			}, "Ping about the staging login.\n\nThanks,\nJosé"),
			want: "Ping about the staging login.\n\nThanks,\nJosé",
		},
		{
			name: "quoted-printable",
			msg: rfc822([]string{
				`Message-ID: <lp-qp@acme.example>`,
				`From: client@acme.example`,
				`Content-Type: text/plain; charset="utf-8"`,
				`Content-Transfer-Encoding: quoted-printable`,
			}, "Invoice total: 50=E2=82=AC due Friday"),
			want: "Invoice total: 50€ due Friday",
		},
		{
			name: "base64",
			msg: rfc822([]string{
				`Message-ID: <lp-b64@acme.example>`,
				`From: client@acme.example`,
				`Content-Type: text/plain; charset="utf-8"`,
				`Content-Transfer-Encoding: base64`,
			}, base64.StdEncoding.EncodeToString([]byte("Base64 body text."))),
			want: "Base64 body text.",
		},
		{
			// THE case criterion 8 changes: today the HTML part is discarded
			// because plain is non-empty. After this ticket the extractor reads
			// it — and this golden says body_text must not notice.
			name: "multipart/alternative, plain wins and the HTML part is discarded",
			msg: rfc822([]string{
				`Message-ID: <lp-alt@acme.example>`,
				`From: notices@pinespropertymanagement.example`,
				`MIME-Version: 1.0`,
				`Content-Type: multipart/alternative; boundary="lp42"`,
			}, multipartBody),
			want: "Your account statement is ready.\r\nView it in the portal.",
		},
		{
			name: "html only, stripHTML fallback with anchors",
			msg: rfc822([]string{
				`Message-ID: <lp-html@acme.example>`,
				`From: notices@pinespropertymanagement.example`,
				`Content-Type: text/html; charset="utf-8"`,
			}, lpHTMLAlternative),
			want: "Your account statement is ready.\nVIEW ACCOUNT\nPAY NOW\nUnsubscribe",
		},
		{
			name: "html only, tracking pixel and no anchors (Pines First Notice)",
			msg: rfc822([]string{
				`Message-ID: <lp-pixel@acme.example>`,
				`From: notices@pinespropertymanagement.example`,
				`Content-Type: text/html; charset="utf-8"`,
			}, lpHTMLTrackingPixelOnly),
			want: "Dear Homeowner,\nPlease see the attached First Notice regarding your property.",
		},
		{
			name: "html only, 30-anchor marketing mail",
			msg: rfc822([]string{
				`Message-ID: <lp-mkt@acme.example>`,
				`From: deals@shop.example`,
				`Content-Type: text/html; charset="utf-8"`,
			}, lpMarketing30()),
			want: "September deals\n" +
				"Deal number 1\nDeal number 2\nDeal number 3\nDeal number 4\nDeal number 5\nDeal number 6\n" +
				"Deal number 7\nDeal number 8\nDeal number 9\nDeal number 10\nDeal number 11\nDeal number 12\n" +
				"Deal number 13\nDeal number 14\nDeal number 15\nDeal number 16\nDeal number 17\nDeal number 18\n" +
				"Deal number 19\nDeal number 20\nDeal number 21\nDeal number 22\nDeal number 23\nDeal number 24\n" +
				"Deal number 25\nDeal number 26\nDeal number 27\nDeal number 28\nDeal number 29\nDeal number 30",
		},
		{
			name: "latin-1 repair (the 0xa9 regression)",
			msg: rfc822([]string{
				`Message-ID: <lp-latin1@acme.example>`,
				`From: client@acme.example`,
				`Content-Type: text/plain; charset="x-unknown-9000"`,
			}, "Terms \xa9 2026 Acme, all rights reserved."),
			want: "Terms © 2026 Acme, all rights reserved.",
		},
	}
}

// TestNormalizeRFC822_BodyTextIsByteIdenticalAcrossTheLinkRefactor is criterion
// 10. Every `want` is a literal recorded from the pre-SWT-25 code; the assertion
// is byte equality, not "contains".
func TestNormalizeRFC822_BodyTextIsByteIdenticalAcrossTheLinkRefactor(t *testing.T) {
	for _, g := range lpGoldens() {
		g := g
		t.Run(g.name, func(t *testing.T) {
			nm, err := google.NormalizeRFC822(inboundRaw(g.msg), acctA, ownSet())
			if err != nil {
				t.Fatalf("NormalizeRFC822: %v", err)
			}
			if nm.BodyText != g.want {
				t.Errorf("BodyText CHANGED.\n got: %q\nwant: %q\n\n"+
					"SWT-25 criterion 10: body_text must come out byte-identical. It is the input to "+
					"confirmDeliveryByBodyPrefix (google/sink.go:542), which recognises OUR OWN sends by "+
					"their whitespace-normalised 120-character prefix — and google has NO RECONCILER, so a "+
					"mismatch is SILENT: the delivery sits unconfirmed forever with sent_external_id NULL, "+
					"nothing retries an exact comparison, and capture logs outbound_observed claiming a "+
					"human sent it. If you are here because you rewrote stripHTML: that is explicitly out "+
					"of scope for this ticket.", nm.BodyText, g.want)
			}
		})
	}
}
