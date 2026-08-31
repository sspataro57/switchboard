package google

// SWT-25 link-preservation: the PURE anchor extractor.
//
// The model never authors a URL. This file extracts anchors deterministically
// at normalize time; the prompt offers the anchor TEXTS by number; the model
// answers with an index; classify.ResolveLink turns the index back into one of
// THESE values. Every filter below exists to make that numbered list short and
// honest — a list of eight unsubscribe links is a list in which the real call
// to action cannot be chosen.
//
// PURE means: no context, no I/O, no clock, no randomness — same input, same
// output, forever. That is also what keeps `--normalize-only --all` a full
// rebuild of the corpus from raw. The tokenizer is golang.org/x/net/html, not a
// regexp: real mail has `>` inside quoted attributes and attributes split
// across lines, and a regexp misparse yields a WRONG URL — the one defect that
// is invisible in a report.
//
// ANCHORS ONLY — `<a href>` and nothing else. Never `img src`: the only "link"
// in a Pines First Notice is an <img> pointing at SendGrid's /wf/open
// open-tracking pixel, and extracting it would put a tracking beacon on a task.
// Zero candidates is the CORRECT answer there, and it is the common case.

import (
	"strings"

	xhtml "golang.org/x/net/html"
)

// Link is one candidate: the anchor's visible text and its destination.
// JSON tags are the normalized_messages.links element shape — {"text","url"} —
// and the ARRAY POSITION is the identity.
type Link struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

const (
	// maxLinkCandidates caps the list so a marketing mail cannot flood the
	// prompt. Measured over 400 personal messages: median 2 candidates after
	// filtering (down from a median of 4 and a mean of 12 unfiltered), and 288
	// of 400 land at 1-3. Document order, first 8 — no ranking, no scoring.
	maxLinkCandidates = 8
	// maxLinkURLBytes drops a URL a data: payload could smuggle past the scheme
	// check — it would otherwise be stored in a row and rendered into a prompt.
	maxLinkURLBytes = 2048
)

// urlDropList removes footer plumbing by case-insensitive substring match on
// the URL. Measured (the same 400-message sample as the cap): these entries are
// what turns a mean of 12 raw anchors into a median of 2 real candidates.
var urlDropList = []string{
	"unsubscribe",
	"opt-out",
	"optout",
	"/wf/open",
	"privacy",
	"terms",
	"preferences",
	"list-manage",
}

// assetHostLabels drops asset/CDN hosts by their FIRST host label. A stylesheet
// or image host is never something a person opens to act on a message, and mail
// templates link to them constantly.
var assetHostLabels = map[string]bool{
	"cdn":    true,
	"assets": true,
	"static": true,
	"images": true,
	"img":    true,
	"media":  true,
}

// anchorTextDropList removes by the anchor's TEXT, because the URL filter alone
// leaves "Unsubscribe", "Privacy", "Terms of Use", "View in Browser" and "here"
// standing — plenty of senders host those behind clean URLs.
//
// EXACT match (after lowercasing, whitespace-collapsing and trimming
// surrounding punctuation), never substring: "pay your bill here" must survive
// while a bare "here" must not — a substring match silently deletes the one
// call to action in the message.
//
// The Spanish spellings are load-bearing: 51 of the 1,609 personal messages are
// Spanish (Bank of America duplicates its alerts), and a filter that is
// English-only silently does half its job on them.
//
// The comparison is spelled LOCALLY and deliberately does not reuse the
// textmatch package: that package is the ONE spelling of the delivery-identity
// comparison, and coupling a content filter to it means a future change to how
// our own sends are recognised silently changes which links a model is offered.
var anchorTextDropList = map[string]bool{
	"unsubscribe":                     true,
	"privacy":                         true,
	"privacy policy":                  true,
	"terms":                           true,
	"terms of use":                    true,
	"view in browser":                 true,
	"view this email in your browser": true,
	"here":                            true,
	"click here":                      true,
	"manage preferences":              true,
	"email preferences":               true,
	"opt out":                         true,
	"cancelar suscripción":            true,
	"darse de baja":                   true,
	"aviso de privacidad":             true,
	"términos":                        true,
	"ver en el navegador":             true,
}

// ExtractLinks returns the filtered, deduped, capped candidate list for one
// HTML body, in DOCUMENT ORDER — the position a model is shown is the position
// in the message, and nothing sorts, ranks or scores.
func ExtractLinks(htmlBody string) []Link {
	if strings.TrimSpace(htmlBody) == "" {
		return nil
	}

	var out []Link
	seen := map[string]bool{}

	inAnchor := false
	href := ""
	var text strings.Builder

	flush := func() {
		if !inAnchor {
			return
		}
		inAnchor = false
		defer text.Reset()

		u := cleanLinkURL(href)
		if u == "" {
			return
		}
		display := collapseSpace(text.String())
		if display == "" {
			// Image wrappers. An entry with no text is an entry the model
			// cannot choose meaningfully: the numbered list it reads is texts
			// only.
			return
		}
		if anchorTextDropList[anchorDropKey(display)] {
			return
		}
		if seen[u] {
			// The image wrapper and the text link under it are one link;
			// offering it twice wastes an index and reads as two options.
			// First survivor keeps its text.
			return
		}
		seen[u] = true
		out = append(out, Link{Text: display, URL: u})
	}

	tok := xhtml.NewTokenizer(strings.NewReader(htmlBody))
	for {
		tt := tok.Next()
		if tt == xhtml.ErrorToken {
			break
		}
		switch tt {
		case xhtml.StartTagToken, xhtml.SelfClosingTagToken:
			name, hasAttr := tok.TagName()
			if string(name) != "a" {
				continue
			}
			flush() // a nested/unterminated anchor ends where the next begins
			inAnchor = true
			href = ""
			text.Reset()
			for hasAttr {
				k, v, more := tok.TagAttr()
				if string(k) == "href" {
					href = string(v)
				}
				hasAttr = more
			}
			if tt == xhtml.SelfClosingTagToken {
				flush()
			}
		case xhtml.EndTagToken:
			name, _ := tok.TagName()
			if string(name) == "a" {
				flush()
			}
		case xhtml.TextToken:
			if inAnchor {
				text.Write(tok.Text())
			}
		}
	}
	flush() // an anchor left open at EOF

	if len(out) > maxLinkCandidates {
		out = out[:maxLinkCandidates]
	}
	return out
}

// cleanLinkURL applies the URL-side filters: scheme allowlist (http/https only,
// case-insensitive, after trimming), the length ceiling, the substring drop
// list, and the asset-host rule. Returns "" to drop. Entities are ALREADY
// decoded: x/net/html's TagAttr unescapes attribute values, and decoding a
// second time would resolve a literal &amp;amp; one level too far.
func cleanLinkURL(href string) string {
	u := strings.TrimSpace(href)
	if u == "" || len(u) > maxLinkURLBytes {
		return ""
	}
	lower := strings.ToLower(u)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		// mailto:, tel:, javascript:, data:, bare fragments and host-less
		// relative URLs are not pages a person opens to act on a message —
		// and a relative URL is unresolvable without a base we deliberately
		// do not read.
		return ""
	}
	for _, frag := range urlDropList {
		if strings.Contains(lower, frag) {
			return ""
		}
	}
	if assetHostLabels[firstHostLabel(lower)] {
		return ""
	}
	return u
}

// firstHostLabel returns the first dotted label of the URL's host ("cdn" for
// https://cdn.example.com/x), lowercased by the caller.
func firstHostLabel(lowerURL string) string {
	rest := lowerURL[strings.Index(lowerURL, "://")+3:]
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.IndexByte(rest, '.'); i >= 0 {
		return rest[:i]
	}
	return rest
}

// collapseSpace normalizes runs of whitespace (NBSP included — unicode.IsSpace
// covers U+00A0) to single spaces and trims the ends, preserving case: this is
// the STORED text, and "VIEW ACCOUNT" should read as the sender wrote it.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// anchorDropKey is the drop-list comparison form: lowercased, collapsed, and
// stripped of surrounding punctuation so "Click here." matches "click here".
func anchorDropKey(display string) string {
	return strings.Trim(strings.ToLower(display), " .,:;!?()[]{}\"'«»¡¿-–—…")
}
