package google_test

// Unit tests for the deterministic anchor extractor (SWT-25 link-preservation,
// acceptance criteria 1-9). ZERO network, ZERO Postgres, ZERO model — the
// extractor is a pure function of one HTML string, which is what makes
// `--normalize-only --all` a full rebuild of the corpus from raw.
//
// GREENFIELD NOTE: `internal/connector/google/links.go` does not exist yet, so
// this file compile-FAILS under `go test ./...` — the expected red state. For
// greenfield code the SPEC's contract IS the signature. Imposed surface, from
// the SPEC's "Internal Go surface added" block:
//
//	type Link struct{ Text, URL string }
//	func ExtractLinks(htmlBody string) []Link          // PURE
//
//	// NormalizedMessage gains:
//	//   Links []Link
//	// filled by BOTH mappers (criteria 8 and 9).
//
// THE DESIGN, so a reader knows what these tests are protecting: the model never
// authors a URL. The application extracts anchors here, deterministically, at
// normalize time; the prompt offers anchor TEXTS by number; the model answers
// with an INDEX; `classify.ResolveLink` turns the index back into a URL. Every
// filter below exists to make that numbered list short and honest — a list of
// eight unsubscribe links is a list in which the real call to action cannot be
// chosen.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

// lpRepoFile reads a path relative to the repo root (this package sits three
// levels down) and FAILS when it is missing — the control that stops the purity
// scan below passing vacuously.
func lpRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v (criterion 1 requires this file to exist; a scan with nothing to scan proves nothing)", rel, err)
	}
	return string(b)
}

// lpTexts / lpURLs project a candidate list for readable assertions.
func lpTexts(links []google.Link) []string {
	out := make([]string, 0, len(links))
	for _, l := range links {
		out = append(out, l.Text)
	}
	return out
}

func lpURLs(links []google.Link) []string {
	out := make([]string, 0, len(links))
	for _, l := range links {
		out = append(out, l.URL)
	}
	return out
}

func lpHasURL(links []google.Link, url string) bool {
	for _, l := range links {
		if l.URL == url {
			return true
		}
	}
	return false
}

// lpAnchor is the minimal HTML wrapper used by the table tests.
func lpAnchor(href, text string) string {
	return `<html><body><p><a href="` + href + `">` + text + `</a></p></body></html>`
}

// ---- criterion 1: links.go is PURE --------------------------------------------

// Same shape as internal/capture/rules_structure_test.go's TestRulesGo_IsPure,
// and for the same reason: purity is a property of the FILE, so it cannot rot
// into "pure except for one lookup". The control is that the file must exist —
// a scan with nothing to scan is the fixture-that-proves-nothing landmine
// wearing a lab coat.
//
// This also carries half of criterion 24: NOTHING FETCHES A LINK, EVER. The
// whole reason `img src` is excluded is that we do not follow beacons; an
// extractor that could make a request would make the refusal a matter of
// discipline instead of structure.
func TestLinksGo_IsPure(t *testing.T) {
	src := lpRepoFile(t, "internal/connector/google/links.go")
	if !strings.Contains(src, "func ExtractLinks(") {
		t.Fatalf("internal/connector/google/links.go does not declare ExtractLinks. Criterion 1 pins the " +
			"pure extractor to THIS file, because the file is what makes the purity checkable")
	}

	banned := []struct{ token, why string }{
		{`"net/http"`, "criterion 24: nothing fetches a link, anywhere, ever — no HEAD to expand a tracking redirect, no title fetch"},
		{`"net"`, "no network of any kind"},
		{`"os"`, "no environment, no files: same input, same output, forever"},
		{`"database/sql"`, "no database: extraction happens on the raw bytes the normalizer already holds"},
		{"pgx", "no database"},
		{`"context"`, "a context parameter is the first thing an I/O call needs; its absence is what keeps ExtractLinks offline"},
		{`"time"`, "no clock: a pure function of the HTML string"},
		{`"math/rand"`, "no randomness: document order is the index basis and it must be stable"},
		{"internal/provider", "no LLM: the extractor is the deterministic half of this ticket"},
		{"internal/textmatch", "criterion 5: the anchor-text comparison is spelled LOCALLY on purpose. " +
			"internal/textmatch is the ONE spelling of the delivery-identity comparison, and coupling a " +
			"content filter to it means a future change to how our own sends are recognised silently " +
			"changes which links a model is offered"},
	}
	for _, b := range banned {
		if strings.Contains(src, b.token) {
			t.Errorf("internal/connector/google/links.go mentions %q — criterion 1 forbids it: %s", b.token, b.why)
		}
	}
}

// ---- criterion 2: ANCHORS ONLY -------------------------------------------------

// The headline refusal. The only "link" in a Pines First Notice is an
// <img src="…/wf/open">, a SendGrid open-tracking pixel.
func TestExtractLinks_NeverFollowsImgSrc(t *testing.T) {
	got := google.ExtractLinks(lpHTMLTrackingPixelOnly)
	if len(got) != 0 {
		t.Errorf("ExtractLinks returned %d candidate(s) %v for a message whose only URL is an <img src>. "+
			"That img is a SendGrid OPEN-TRACKING PIXEL (…/wf/open): extracting it would put a tracking "+
			"beacon on a task, offer it to the model as the thing a person would click, and store a beacon "+
			"URL in normalized_messages.links. `<a href>` and nothing else — criterion 2. Zero candidates "+
			"is the CORRECT answer here and it is the common case: link_index null is ordinary output",
			len(got), got)
	}
}

func TestExtractLinks_IgnoresEveryNonAnchorURLCarrier(t *testing.T) {
	cases := []struct{ name, html string }{
		{"img src", `<img src="https://tracker.example.com/wf/open?u=1">`},
		{"link rel", `<link rel="stylesheet" href="https://cdn.example.com/mail.css">`},
		{"iframe src", `<iframe src="https://embed.example.com/video/9"></iframe>`},
		{"form action", `<form action="https://forms.example.com/submit"><input type="submit"></form>`},
		{"area href", `<map><area shape="rect" href="https://imagemap.example.com/go"></map>`},
		{"base href", `<base href="https://base.example.com/">`},
		{"script src", `<script src="https://cdn.example.com/track.js"></script>`},
		{"background attribute", `<td background="https://cdn.example.com/bg.png">cell</td>`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := google.ExtractLinks(`<html><body>` + tc.html + `</body></html>`)
			if len(got) != 0 {
				t.Errorf("ExtractLinks(%s) = %v, want no candidates. Criterion 2 is `<a href>` and NOTHING "+
					"else: every other carrier here is either an asset the mail client fetches or a "+
					"destination no human clicked, and one of them (img) is a beacon", tc.name, got)
			}
		})
	}
}

// ---- criterion 3: scheme allowlist + the length ceiling ------------------------

func TestExtractLinks_SchemeAllowlist(t *testing.T) {
	cases := []struct {
		name string
		href string
		keep bool
		why  string
	}{
		{"https", "https://portal.example.com/account", true, ""},
		{"http", "http://portal.example.com/account", true, ""},
		{"HTTPS uppercase", "HTTPS://PORTAL.EXAMPLE.COM/account", true, "compared case-insensitively"},
		{"leading whitespace", "  https://portal.example.com/account  ", true, "trimmed before comparison"},
		{"entity-encoded", "https://portal.example.com/pay?a=1&amp;b=2", true, "HTML entities decoded before comparison"},
		{"mailto", "mailto:billing@example.com", false, "an address is not a page to open"},
		{"tel", "tel:+15551234567", false, "not a page"},
		{"javascript", "javascript:void(0)", false, "never a destination, and storing it invites something to render it"},
		{"data", "data:text/html;base64,PGh0bWw+", false, "a payload, not a link"},
		{"bare fragment", "#main", false, "goes nowhere outside the message"},
		{"relative path", "/account/12345", false, "host-less: unresolvable without a base we deliberately do not read"},
		{"protocol-relative", "//cdn.example.com/x", false, "no scheme, and it resolves to whatever the renderer decides"},
		{"empty href", "", false, "nothing to store"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := google.ExtractLinks(lpAnchor(tc.href, "VIEW DETAILS"))
			if tc.keep && len(got) != 1 {
				t.Errorf("ExtractLinks(%q) = %v, want ONE candidate: http and https are the allowlist", tc.href, got)
			}
			if !tc.keep && len(got) != 0 {
				t.Errorf("ExtractLinks(%q) = %v, want none — %s. Criterion 3's allowlist is http/https only",
					tc.href, got, tc.why)
			}
		})
	}
}

func TestExtractLinks_DropsAURLLongerThan2048Bytes(t *testing.T) {
	long := "https://example.com/x?p=" + strings.Repeat("a", 2100)
	if got := google.ExtractLinks(lpAnchor(long, "VIEW DETAILS")); len(got) != 0 {
		t.Errorf("ExtractLinks kept a %d-byte URL (%d candidates). Criterion 3 caps at 2048: a data: payload "+
			"that sneaks past the scheme check would otherwise be STORED in a normalized_messages row and "+
			"RENDERED into a prompt", len(long), len(got))
	}
	ok := "https://example.com/x?p=" + strings.Repeat("a", 1000)
	if got := google.ExtractLinks(lpAnchor(ok, "VIEW DETAILS")); len(got) != 1 {
		t.Errorf("control: ExtractLinks dropped a %d-byte URL too (%v); the ceiling is 2048, not 'long'",
			len(ok), got)
	}
}

// ---- criterion 4: drop by URL --------------------------------------------------

// One survivor and one victim per drop-list entry. The victim proves the entry
// is in the list; the survivor proves the match is not so wide that it eats the
// call to action next to it.
func TestExtractLinks_DropsByURL(t *testing.T) {
	cases := []struct {
		entry    string
		victim   string
		survivor string
	}{
		{"unsubscribe", "https://lists.example.com/unsubscribe?u=9", "https://portal.example.com/account"},
		{"unsubscribe uppercase", "https://lists.example.com/UNSUBSCRIBE?u=9", "https://portal.example.com/pay"},
		{"opt-out", "https://mail.example.com/opt-out/abc", "https://portal.example.com/statements"},
		{"optout", "https://mail.example.com/optout?id=7", "https://portal.example.com/documents"},
		{"/wf/open", "https://u1234.ct.sendgrid.net/wf/open?upn=abc", "https://portal.example.com/notices"},
		{"privacy", "https://example.com/privacy-policy", "https://portal.example.com/violations"},
		{"terms", "https://example.com/terms-of-use", "https://portal.example.com/balance"},
		{"preferences", "https://example.com/email/preferences", "https://portal.example.com/invoice"},
		{"list-manage", "https://example.us1.list-manage.com/unsub?e=1", "https://portal.example.com/ticket"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.entry, func(t *testing.T) {
			html := `<html><body>` +
				`<p><a href="` + tc.survivor + `">VIEW DETAILS</a></p>` +
				`<p><a href="` + tc.victim + `">Manage your mail</a></p>` +
				`</body></html>`
			got := google.ExtractLinks(html)
			if lpHasURL(got, tc.victim) {
				t.Errorf("ExtractLinks kept %q. Criterion 4's URL drop list is a case-insensitive substring "+
					"match and %q is on it: these are footer plumbing, and every one of them that survives "+
					"takes an index away from the real call to action", tc.victim, tc.entry)
			}
			if !lpHasURL(got, tc.survivor) {
				t.Errorf("ExtractLinks dropped the SURVIVOR %q while filtering for %q (got %v). A drop rule "+
					"that also removes the portal link has made the message unactionable, which is the "+
					"failure this ticket exists to fix", tc.survivor, tc.entry, got)
			}
		})
	}
}

// The asset / CDN hosts. Named separately because they are a HOST rule, not a
// path rule: an image or stylesheet host is never something a person opens to
// act on a message, and mail templates link to them constantly.
func TestExtractLinks_DropsAssetAndCDNHosts(t *testing.T) {
	victims := []string{
		"https://cdn.example.com/logo.png",
		"https://assets.example.com/img/header.jpg",
		"https://static.example.com/style.css",
		"https://images.example.com/banner.gif",
	}
	for _, v := range victims {
		v := v
		t.Run(v, func(t *testing.T) {
			html := `<html><body>` +
				`<p><a href="https://portal.pinespropertymanagement.com/account">VIEW ACCOUNT</a></p>` +
				`<p><a href="` + v + `">Our logo</a></p>` +
				`</body></html>`
			got := google.ExtractLinks(html)
			if lpHasURL(got, v) {
				t.Errorf("ExtractLinks kept the asset host link %q (got %v). Criterion 4 drops asset/CDN "+
					"hosts: nobody opens a stylesheet to pay a bill", v, got)
			}
			if len(got) != 1 {
				t.Errorf("the portal link did not survive alongside %q: got %v", v, got)
			}
		})
	}
}

// ---- criterion 5: drop by ANCHOR TEXT ------------------------------------------

// Measured: the URL filter alone leaves "Unsubscribe", "Privacy", "Terms of
// Use", "View in Browser" and "here" standing, because plenty of senders host
// those behind URLs with none of the drop-list substrings in them.
func TestExtractLinks_DropsByAnchorText(t *testing.T) {
	// Each victim's URL is deliberately CLEAN — it carries no drop-list
	// substring — so only the TEXT rule can remove it. Without that the test
	// would pass on the URL filter and prove nothing.
	victims := []string{
		"Unsubscribe", "unsubscribe", "  UNSUBSCRIBE  ",
		"Privacy", "Privacy Policy", "Terms", "Terms of Use",
		"View in Browser", "View this email in your browser",
		"here", "Here", "Click here", "click here.",
		"Manage preferences", "Email preferences", "Opt out",
		// Spanish: 51 of the 1,609 personal messages are Spanish (Bank of
		// America duplicates its alerts). A filter that is English-only is a
		// filter that silently does half its job on them, and the result is
		// noise in the candidate list rather than an error anybody sees.
		"Cancelar suscripción", "Darse de baja", "Aviso de privacidad",
		"Términos", "Ver en el navegador",
	}
	for _, text := range victims {
		text := text
		t.Run(text, func(t *testing.T) {
			html := `<html><body>` +
				`<p><a href="https://portal.example.com/account">VIEW ACCOUNT</a></p>` +
				`<p><a href="https://n.example.com/a/b/c123">` + text + `</a></p>` +
				`</body></html>`
			got := google.ExtractLinks(html)
			if lpHasURL(got, "https://n.example.com/a/b/c123") {
				t.Errorf("the anchor text %q survived (got %v). Criterion 5: the comparison is lowercased, "+
					"whitespace-collapsed and stripped of surrounding punctuation before an EXACT match "+
					"against the drop list — its URL is clean, so only the text rule can remove it",
					text, got)
			}
			if len(got) != 1 {
				t.Errorf("VIEW ACCOUNT did not survive next to %q: got %v", text, got)
			}
		})
	}
}

// EXACT, not substring — and this is the case that says why.
func TestExtractLinks_AnchorTextMatchIsExactNotSubstring(t *testing.T) {
	survivors := []string{
		"Pay your bill here",
		"Click here to confirm your appointment",
		"Read the privacy notice we sent about your account",
		"View your terms of service change",
	}
	for _, text := range survivors {
		text := text
		t.Run(text, func(t *testing.T) {
			got := google.ExtractLinks(lpAnchor("https://portal.example.com/pay", text))
			if len(got) != 1 {
				t.Errorf("ExtractLinks dropped %q (got %v). Criterion 5 matches the drop list EXACTLY: a "+
					"bare \"here\" must go and \"pay your bill here\" must stay. A substring match here "+
					"silently deletes the one call to action in the message, and the reader sees no link "+
					"with no way to tell it was filtered", text, got)
			}
		})
	}
}

func TestExtractLinks_DropsEmptyAnchorText(t *testing.T) {
	cases := []struct{ name, html string }{
		{"empty", `<a href="https://portal.example.com/account"></a>`},
		{"whitespace only", `<a href="https://portal.example.com/account">   </a>`},
		{"image wrapper", `<a href="https://portal.example.com/account"><img src="https://cdn.example.com/btn.png"></a>`},
		{"nbsp only", `<a href="https://portal.example.com/account">&nbsp;</a>`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := google.ExtractLinks(`<html><body>` + tc.html + `</body></html>`); len(got) != 0 {
				t.Errorf("ExtractLinks kept an anchor with no text: %v. Criterion 5 always drops empty text "+
					"— these are IMAGE WRAPPERS, and an entry with no text is an entry the model cannot "+
					"choose meaningfully: the numbered list it reads is texts only", got)
			}
		})
	}
}

// ---- criterion 6: dedup, then cap at 8 ------------------------------------------

func TestExtractLinks_DedupsByURLKeepingTheFirstText(t *testing.T) {
	html := `<html><body>` +
		`<p><a href="https://portal.example.com/account"><img src="https://cdn.example.com/btn.png" alt=""></a></p>` +
		`<p><a href="https://portal.example.com/account">VIEW ACCOUNT</a></p>` +
		`<p><a href="https://portal.example.com/account">VIEW ACCOUNT</a></p>` +
		`</body></html>`
	got := google.ExtractLinks(html)
	if len(got) != 1 {
		t.Fatalf("ExtractLinks returned %d candidates %v for one URL appearing three times. Criterion 6 "+
			"dedups BY URL: the image wrapper and the text link under it are one link, and offering it "+
			"twice both wastes an index and makes the list read as two options", len(got), got)
	}
	// The image wrapper has empty text and is dropped by criterion 5 BEFORE
	// dedup, so the surviving entry is the first one that had text.
	if got[0].Text != "VIEW ACCOUNT" {
		t.Errorf("deduped entry text = %q, want %q — the first occurrence THAT SURVIVED the filters keeps "+
			"its text", got[0].Text, "VIEW ACCOUNT")
	}
}

func TestExtractLinks_CapsAtEightKeepingDocumentOrder(t *testing.T) {
	got := google.ExtractLinks(lpMarketing30())
	if len(got) != 8 {
		t.Fatalf("ExtractLinks returned %d candidates for a 30-anchor marketing mail, want 8. Criterion 6 "+
			"caps at 8 so marketing mail cannot flood the prompt: the median personal message has TWO "+
			"candidates, and a 30-entry numbered list is a list nobody — model or human — chooses from",
			len(got))
	}
	want := []string{
		"Deal number 1", "Deal number 2", "Deal number 3", "Deal number 4",
		"Deal number 5", "Deal number 6", "Deal number 7", "Deal number 8",
	}
	if !reflect.DeepEqual(lpTexts(got), want) {
		t.Errorf("capped list = %v, want %v — the cap keeps DOCUMENT ORDER (the first 8), it does not rank, "+
			"score or sort. The position a model is shown is the position in the message", lpTexts(got), want)
	}
}

// ---- criterion 7: document order is the index basis, and it is stable ----------

// Shape borrowed from TestNormalizeRFC822_Deterministic (rfc822_test.go:416).
// The index the model returns resolves by POSITION, so a run-to-run reorder
// would resolve a different URL from the same answer.
func TestExtractLinks_Deterministic(t *testing.T) {
	html := `<html><body>` +
		`<p><a href="https://portal.example.com/b">Second in the document</a></p>` +
		`<p><a href="https://portal.example.com/a">Third in the document</a></p>` +
		`<p><a href="https://portal.example.com/c">Fourth in the document</a></p>` +
		`</body></html>`
	first := google.ExtractLinks(html)
	second := google.ExtractLinks(html)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("ExtractLinks is not deterministic:\n%+v\n%+v", first, second)
	}
	if len(first) != 3 {
		t.Fatalf("ExtractLinks returned %d candidates, want 3", len(first))
	}
	wantURLs := []string{
		"https://portal.example.com/b",
		"https://portal.example.com/a",
		"https://portal.example.com/c",
	}
	if !reflect.DeepEqual(lpURLs(first), wantURLs) {
		t.Errorf("candidate order = %v, want document order %v. Nothing sorts, nothing ranks, nothing "+
			"scores (criterion 7) — the alphabetical order here is deliberately NOT the document order, so "+
			"a sort would be visible", lpURLs(first), wantURLs)
	}
}

// Real mail has `>` inside quoted attributes and attributes split across lines.
// A regexp misparse yields a WRONG URL, which is the class of defect you cannot
// see in a report — hence the tokenizer.
func TestExtractLinks_ParsesAwkwardRealWorldMarkup(t *testing.T) {
	html := "<html><body>\n" +
		`<p><a class="btn"` + "\n" + `   href="https://portal.example.com/account?q=a%3Eb"` + "\n" +
		`   target="_blank" title="a > b">VIEW ACCOUNT</a></p>` + "\n" +
		`<p><a href='https://portal.example.com/pay'>PAY NOW</a></p>` + "\n" +
		`</body></html>`
	got := google.ExtractLinks(html)
	if len(got) != 2 {
		t.Fatalf("ExtractLinks returned %d candidates %v, want 2 (an attribute split across lines with a "+
			"'>' inside a quoted value, and a single-quoted href)", len(got), got)
	}
	if got[0].URL != "https://portal.example.com/account?q=a%3Eb" {
		t.Errorf("first URL = %q, want the full href. A misparse here does not fail — it stores and offers "+
			"a WRONG URL, which is the one defect that is invisible in the report", got[0].URL)
	}
	if got[0].Text != "VIEW ACCOUNT" {
		t.Errorf("first text = %q, want %q", got[0].Text, "VIEW ACCOUNT")
	}
}

// ---- criterion 8: NormalizeRFC822 reads the part extractBodyText THROWS AWAY ----

// Premise 2, and the reason this ticket cannot be done by re-reading body_text:
// for a multipart/alternative message extractBodyText takes the plain part and
// DISCARDS the html one (rfc822.go:227-229), so every anchor in it is gone
// before stripHTML is ever reached. Without this the common case — a templated
// notice that ships plain text and HTML — silently yields no links.
func TestNormalizeRFC822_LinksComeFromTheHTMLPartEvenWhenPlainWinsTheBody(t *testing.T) {
	body := strings.Join([]string{
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
	msg := rfc822([]string{
		`Message-ID: <lp-alt@acme.example>`,
		`From: notices@pinespropertymanagement.example`,
		`MIME-Version: 1.0`,
		`Content-Type: multipart/alternative; boundary="lp42"`,
	}, body)

	nm, err := google.NormalizeRFC822(inboundRaw(msg), acctA, ownSet())
	if err != nil {
		t.Fatalf("NormalizeRFC822: %v", err)
	}

	// BodyText is still the PLAIN part — criterion 10, asserted here too because
	// the two halves of criterion 8 are "the links appear" AND "the body did not
	// move". The full byte-identical golden is bodytext_golden_test.go.
	if !strings.Contains(nm.BodyText, "Your account statement is ready.") ||
		strings.Contains(nm.BodyText, "VIEW ACCOUNT") {
		t.Errorf("BodyText = %q, want the text/plain part unchanged. Link extraction must READ the html "+
			"part, never promote it into the body", nm.BodyText)
	}

	want := []google.Link{
		{Text: "VIEW ACCOUNT", URL: "https://portal.pinespropertymanagement.com/account/12345"},
		{Text: "PAY NOW", URL: "https://portal.pinespropertymanagement.com/pay"},
	}
	if !reflect.DeepEqual(nm.Links, want) {
		t.Errorf("Links = %+v, want %+v.\nCriterion 8: NormalizeRFC822 fills Links from the message's "+
			"text/html part INDEPENDENTLY of which part won body_text. extractBodyText discards the html "+
			"part whenever plain is non-empty (rfc822.go:227-229), which is the shape of every templated "+
			"notice — so an extractor wired to the surviving TEXT finds nothing here, silently, on exactly "+
			"the messages this ticket exists for. (The Unsubscribe anchor is dropped by criterion 5.)",
			nm.Links, want)
	}
}

func TestNormalizeRFC822_LinksIsEmptyForAMessageWithNoAnchors(t *testing.T) {
	msg := rfc822([]string{
		`Message-ID: <lp-pixel@acme.example>`,
		`From: notices@pinespropertymanagement.example`,
		`Content-Type: text/html; charset="utf-8"`,
	}, lpHTMLTrackingPixelOnly)

	nm, err := google.NormalizeRFC822(inboundRaw(msg), acctA, ownSet())
	if err != nil {
		t.Fatalf("NormalizeRFC822: %v", err)
	}
	if len(nm.Links) != 0 {
		t.Errorf("Links = %+v for the Pines First Notice shape, want none. Its only URL is the SendGrid "+
			"/wf/open tracking pixel. NO CANDIDATES IS THE COMMON CASE and it is not an error anywhere "+
			"downstream: the prompt renders no list, the model answers null, and the extraction records "+
			"link_candidates:0", nm.Links)
	}
}

// ---- criterion 9: the Gmail-API mapper is wired too ----------------------------

// Not the production path (production is imap, CLAUDE.md build-order step 7),
// and wired anyway: the bridge and gmail_api raws are normalized by THIS mapper,
// so leaving Links empty here makes a future MAIL_SOURCE flip a silent,
// error-free loss of every link in the corpus.
func TestNormalizeGmailMessage_FillsLinksFromTheTextHTMLPart(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "gm-links-1",
		"threadId": "th-links-1",
		"internalDate": "1751364000000",
		"payload": {
			"mimeType": "multipart/alternative",
			"headers": [
				{"name": "Message-ID", "value": "<lp-gmail@acme.example>"},
				{"name": "From", "value": "notices@pinespropertymanagement.example"},
				{"name": "Subject", "value": "Your account statement is ready"}
			],
			"parts": [
				{"mimeType": "text/plain", "body": {"data": "` + b64url("Your account statement is ready.") + `"}},
				{"mimeType": "text/html", "body": {"data": "` + b64url(lpHTMLAlternative) + `"}}
			]
		}
	}`)

	nm, err := google.NormalizeGmailMessage(raw, acctA, map[string]bool{})
	if err != nil {
		t.Fatalf("NormalizeGmailMessage: %v", err)
	}
	if nm.BodyText != "Your account statement is ready." {
		t.Errorf("BodyText = %q, want the text/plain part (firstTextPlain is unchanged by this ticket)", nm.BodyText)
	}
	want := []google.Link{
		{Text: "VIEW ACCOUNT", URL: "https://portal.pinespropertymanagement.com/account/12345"},
		{Text: "PAY NOW", URL: "https://portal.pinespropertymanagement.com/pay"},
	}
	if !reflect.DeepEqual(nm.Links, want) {
		t.Errorf("Links = %+v, want %+v. Criterion 9: NormalizeGmailMessage fills Links the same way, via a "+
			"firstTextHTML sibling of firstTextPlain (normalize.go:133-145). Ten lines buys away a silent "+
			"whole-corpus loss the day MAIL_SOURCE changes", nm.Links, want)
	}
}
