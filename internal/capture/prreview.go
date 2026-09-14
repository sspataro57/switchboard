package capture

// The PURE half of the PR-review rule (SWT-54,
// docs/tickets/treetop-pr-review-tasks_SPEC.md D1 and D2 point 4). No database,
// no clock, no environment: prReviewFacts (prreview_store.go) reads the stored
// headers, decidePRAuthor decides, and the driver acts. The ownActionFacts →
// decideOwnAction split, again.
//
// His GitHub login is DATA, never a literal here: row 2 reads it per mail from
// X-GitHub-Recipient, and extra logins live in capture_rules.exclude_pr_authors.

import (
	"fmt"
	"mime"
	"net/mail"
	"net/textproto"
	"regexp"
	"strings"

	"github.com/sspataro57/switchboard/internal/connector/github"
	"github.com/sspataro57/switchboard/internal/textmatch"
)

// ---- origin check (SWT-54 SPEC D1 amendment 2026-09-14, threat model) ----------
//
// Anyone can mail him a message whose Message-ID / References are GitHub-shaped
// (they become the thread key the pr_review rule matches) and whose X-GitHub-*
// headers say whatever the sender likes. Before a pr_review rule acts on a mail,
// or counts one as authorship evidence, the mail must prove it came from GitHub.
// The proof is the Authentication-Results header the RECEIVING MX (Gmail)
// prepends. Prod, 2026-09-14, read-only: 121 of 121 Treetop PR-thread mails in 90
// days carried one from mx.google.com with dkim=pass for github.com, on both
// receiving accounts.

// prTrustedAuthServID is the authserv-id of the receiving MX whose
// Authentication-Results header is trusted: Gmail's. It is DATA — change it here,
// never at a call site — if the receiving accounts ever move off Gmail.
const prTrustedAuthServID = "mx.google.com"

// prTrustedDKIMDomain is the signing domain a GitHub notification's passing DKIM
// result must name (header.d=github.com, or header.i=…@github.com).
const prTrustedDKIMDomain = "github.com"

// trustedGitHubNotification reports whether h is a GitHub notification the
// receiving MX authenticated. It reads ONLY the FIRST (topmost)
// Authentication-Results header: the receiving MX prepends its own, so any
// header below it came with the message and may be forged. That header must be
// written by prTrustedAuthServID exactly and carry a dkim=pass result whose
// header.d is prTrustedDKIMDomain or whose header.i ends in "@"+prTrustedDKIMDomain.
// Anything else — no header, another authserv-id, dkim=fail, a pass for another
// domain, header.d=github.com.evil.example — is untrusted. Pure.
//
// It also requires every header the rule acts on to appear exactly once
// (duplicatedGitHubHeader): a relayed genuine notification still passes DKIM
// when unsigned copies are PREPENDED, and mail.Header.Get would read the
// attacker's copy.
func trustedGitHubNotification(h mail.Header) bool {
	if duplicatedGitHubHeader(h) != "" {
		return false
	}
	values := h["Authentication-Results"] // textproto keeps header order: [0] is the topmost
	if len(values) == 0 {
		return false
	}
	parts := authResultsParts(values[0])
	if len(parts) < 2 {
		return false
	}
	// authserv-id [CFWS authres-version]: the first token of the first part.
	id := strings.Fields(parts[0])
	if len(id) == 0 || id[0] != prTrustedAuthServID {
		return false
	}
	// A quote anywhere in a dkim result makes the whole mail untrusted: a quoted
	// property value can smuggle a second "header.d=github.com" token past
	// whitespace tokenization (header.i="x header.d=github.com "@evil.example).
	// The one exception is Gmail's own quoting of header.b when the signature
	// prefix holds a '/' or '+' (prod, 2026-09-14: 4 of the 150 PR mails a 30-day dry run matched, e.g.
	// header.b="FVP32/f4"). A quoted string of base64 characters only has no
	// whitespace, ';', '(' or '\', so it can neither hide a token boundary, split
	// a part, open a comment nor shift the quote pairing.
	for _, resinfo := range parts[1:] {
		if authResultIsDKIM(resinfo) && strings.Contains(authResultsQuotedSigRe.ReplaceAllString(resinfo, "header.b=q"), `"`) {
			return false
		}
	}
	for _, resinfo := range parts[1:] {
		if authResultIsGitHubDKIMPass(resinfo) {
			return true
		}
	}
	return false
}

// authResultIsDKIM reports whether a resinfo's method is dkim.
func authResultIsDKIM(resinfo string) bool {
	tokens := strings.Fields(authResultsEqRe.ReplaceAllString(resinfo, "="))
	if len(tokens) == 0 {
		return false
	}
	method, _, _ := strings.Cut(tokens[0], "=")
	return strings.EqualFold(method, "dkim")
}

// prSingleInstanceHeaders are the headers whose values drive a pr_review
// action: the thread headers (the thread key, hence the PR the mail acts on)
// and every X-GitHub-* header D1 reads. Genuine GitHub mail carries each at
// most once. Threat (Codex re-review, 2026-09-14): an attacker relays a genuine
// GitHub-signed notification and prepends unsigned copies; DKIM verifies the
// signed instance, Gmail stamps dkim=pass for github.com, and Header.Get returns
// the attacker's first instance. With a single instance enforced, the value read
// is the one the signature covered.
var prSingleInstanceHeaders = []string{
	"Message-ID", "References", "In-Reply-To",
	github.HeaderReason, github.HeaderSender, github.HeaderRecipient,
	// The two SIGNED headers the PR binding reads (prMailBindingFailure).
	"List-ID", "Subject",
}

// prUntrustedReason is the whole origin check for one mail about PR ref: ""
// when trusted, else the evidence for the decision reason. Pure. Order:
// duplicated headers, the receiving MX's DKIM verdict, then the binding of the
// PR identity to SIGNED headers.
func prUntrustedReason(h mail.Header, ref github.PRRef) string {
	if dup := duplicatedGitHubHeader(h); dup != "" {
		return fmt.Sprintf("header %s appears more than once; only the signed instance could be trusted", dup)
	}
	if !trustedGitHubNotification(h) {
		return fmt.Sprintf("the topmost Authentication-Results is not %s's with dkim=pass for %s",
			prTrustedAuthServID, prTrustedDKIMDomain)
	}
	return prMailBindingFailure(h, ref)
}

// prMailBindingFailure binds the PR a mail acts on to headers GitHub's DKIM
// signature COVERS (SPEC D1 amendment, 2026-09-14). Prod h= lists, 90 days:
// openings sign date:from:reply-to:to:cc:subject:list-id:…, follow-ups add
// in-reply-to:references. Message-ID and every X-GitHub-* header are NOT
// signed, so a relayed genuine opening of the attacker's OWN PR with its
// Message-ID replaced by a treetopllc root would otherwise pass. It returns ""
// when bound, else which binding failed:
//
//   - List-ID: its angle part must be "{repo}.{owner}.github.com" for ref
//     (prod: 123 of 123 are exactly "owner/repo <repo.owner.github.com>");
//   - Subject: decoded (RFC 2047; prod has encoded subjects), it must END with
//     "(PR #N)" for ref's N (prod: 123 of 123 after decoding).
//
// Pure.
func prMailBindingFailure(h mail.Header, ref github.PRRef) string {
	owner, name, _ := strings.Cut(ref.Repo, "/")
	listID := strings.TrimSpace(h.Get("List-ID"))
	want := name + "." + owner + ".github.com"
	angle := ""
	if i := strings.LastIndexByte(listID, '<'); i >= 0 {
		if j := strings.IndexByte(listID[i:], '>'); j > 0 {
			angle = listID[i+1 : i+j]
		}
	}
	if !strings.EqualFold(angle, want) {
		if listID == "" {
			return fmt.Sprintf("List-ID binding failed: no List-ID header; want <%s>", want)
		}
		return fmt.Sprintf("List-ID binding failed: signed List-ID %q does not name <%s>", listID, want)
	}
	subject := h.Get("Subject")
	if dec, err := new(mime.WordDecoder).DecodeHeader(subject); err == nil {
		subject = dec
	}
	if !strings.HasSuffix(strings.TrimSpace(subject), fmt.Sprintf("(PR #%d)", ref.PR)) {
		return fmt.Sprintf("Subject binding failed: signed Subject %q does not end with (PR #%d)", subject, ref.PR)
	}
	return ""
}

// duplicatedGitHubHeader names the first prSingleInstanceHeaders entry that
// appears more than once in h ("" = none). Names are compared canonically, so
// "Message-ID" and "message-id" are the same header. Pure.
func duplicatedGitHubHeader(h mail.Header) string {
	for _, name := range prSingleInstanceHeaders {
		if len(h[textproto.CanonicalMIMEHeaderKey(name)]) > 1 {
			return name
		}
	}
	return ""
}

// authResultIsGitHubDKIMPass reads one resinfo ("dkim=pass header.i=@github.com
// header.s=… header.b=…") with comments already removed.
func authResultIsGitHubDKIMPass(resinfo string) bool {
	tokens := strings.Fields(authResultsEqRe.ReplaceAllString(resinfo, "="))
	if len(tokens) == 0 {
		return false
	}
	method, result, ok := strings.Cut(tokens[0], "=")
	if !ok || !strings.EqualFold(method, "dkim") || !strings.EqualFold(authResultsUnquote(result), "pass") {
		return false
	}
	for _, tok := range tokens[1:] {
		prop, val, ok := strings.Cut(tok, "=")
		if !ok {
			continue
		}
		val = strings.ToLower(authResultsUnquote(val))
		switch strings.ToLower(prop) {
		case "header.d":
			if val == prTrustedDKIMDomain {
				return true
			}
		case "header.i":
			if strings.HasSuffix(val, "@"+prTrustedDKIMDomain) {
				return true
			}
		}
	}
	return false
}

// authResultsEqRe folds "method = result" (CFWS is legal around '=') to "method=result".
var authResultsEqRe = regexp.MustCompile(`\s*=\s*`)

// authResultsQuotedSigRe matches "header.b=" followed by a quoted string of
// base64 characters only — the one quoted value Gmail writes in a dkim result.
// It also matches such a span inside another value (after '.', '=' or '@'),
// which is harmless: with the span's quotes removed, the resinfo tokenizes the
// same, so nothing the pre-fix rule rejected can pass. See
// trustedGitHubNotification.
var authResultsQuotedSigRe = regexp.MustCompile(`(?i)\bheader\.b\s*=\s*"[A-Za-z0-9+/=]*"`)

// authResultsParts removes RFC 5322 comments (nested, with quoted-pairs) from an
// Authentication-Results value and splits it on the ';' that are outside quoted
// strings. A comment is replaced by a space, so it can neither hide nor forge a
// token: "(dkim=pass header.d=github.com)" is a comment, not a result.
func authResultsParts(v string) []string {
	var parts []string
	var b strings.Builder
	depth, inQuote := 0, false
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == '\\' && (inQuote || depth > 0):
			if inQuote {
				b.WriteByte(c)
				if i+1 < len(v) {
					b.WriteByte(v[i+1])
				}
			}
			i++
		case inQuote:
			b.WriteByte(c)
			if c == '"' {
				inQuote = false
			}
		case c == '(':
			depth++
		case c == ')' && depth > 0:
			depth--
			if depth == 0 {
				b.WriteByte(' ')
			}
		case depth > 0:
			// inside a comment: dropped
		case c == '"':
			inQuote = true
			b.WriteByte(c)
		case c == ';':
			parts = append(parts, strings.TrimSpace(b.String()))
			b.Reset()
		default:
			b.WriteByte(c)
		}
	}
	return append(parts, strings.TrimSpace(b.String()))
}

func authResultsUnquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `\`, "")
	}
	return s
}

// The four authorship verdicts. The dry run and the decision reasons print
// these words.
const (
	prAuthorOwn          = "own"
	prAuthorExcluded     = "excluded"
	prAuthorOther        = "other"
	prAuthorUndetermined = "undetermined"
)

// GitHub's two reasons that carry authorship information by themselves.
const (
	ghReasonAuthor          = "author"           // the recipient authored the thread
	ghReasonReviewRequested = "review_requested" // you cannot request your own review
)

// prAuthorFacts is everything D1 knows about one PR, read from stored raw mail.
type prAuthorFacts struct {
	// reasons is the X-GitHub-Reason of every stored inbound mail read for the
	// PR ("" = header absent, as on commit and push mail).
	reasons []string
	// openingFound: the inbound message whose external_message_id is exactly
	// <{owner}/{repo}/pull/{N}@github.com> was found and its headers read.
	openingFound bool
	// openingSender is its X-GitHub-Sender: the PR author's login.
	openingSender string
	// openingRecipient is its X-GitHub-Recipient: his login as GitHub
	// addressed that mail.
	openingRecipient string
}

// prAuthorVerdict is decidePRAuthor's answer. author is the PR author's login
// when known; evidence is the human-readable why, carried into the decision
// reason (invariant 7: every decision says why).
type prAuthorVerdict struct {
	verdict  string
	author   string
	evidence string
	// entry is the exclude_pr_authors entry that matched (verdict excluded only).
	entry string
}

// decidePRAuthor is D1's table, first hit wins:
//
//	1  any mail has reason author                         own
//	2  opening found, sender == its recipient (folded)    own
//	3  opening found, sender matches exclude_pr_authors   excluded
//	4  opening found, sender not empty                    other (named)
//	5  any mail has reason review_requested               other (not named)
//	6  none of the above                                  undetermined (fail-open: create)
//
// Two EMPTY logins never read as his (row 2 needs a sender): two absent values
// are not a match.
func decidePRAuthor(f prAuthorFacts, exclude []string) prAuthorVerdict {
	if prHasReason(f.reasons, ghReasonAuthor) {
		return prAuthorVerdict{verdict: prAuthorOwn,
			evidence: "a stored mail on the PR carries X-GitHub-Reason: author"}
	}
	if f.openingFound && f.openingSender != "" {
		switch {
		case strings.EqualFold(f.openingSender, f.openingRecipient):
			return prAuthorVerdict{verdict: prAuthorOwn, author: f.openingSender,
				evidence: fmt.Sprintf("the opening mail's X-GitHub-Sender %s is its X-GitHub-Recipient %s",
					f.openingSender, f.openingRecipient)}
		}
		if entry, ok := matchExcludedAuthor(f.openingSender, exclude); ok {
			return prAuthorVerdict{verdict: prAuthorExcluded, author: f.openingSender, entry: entry,
				evidence: fmt.Sprintf("the opening mail's X-GitHub-Sender %s matches exclude_pr_authors entry %s",
					f.openingSender, entry)}
		}
		return prAuthorVerdict{verdict: prAuthorOther, author: f.openingSender,
			evidence: fmt.Sprintf("the opening mail's X-GitHub-Sender is %s", f.openingSender)}
	}
	if prHasReason(f.reasons, ghReasonReviewRequested) {
		return prAuthorVerdict{verdict: prAuthorOther,
			evidence: "a stored mail on the PR carries X-GitHub-Reason: review_requested (author not named)"}
	}
	return prAuthorVerdict{verdict: prAuthorUndetermined,
		evidence: "no reason author or review_requested, and no opening mail naming a sender"}
}

func prHasReason(reasons []string, want string) bool {
	for _, r := range reasons {
		if strings.EqualFold(strings.TrimSpace(r), want) {
			return true
		}
	}
	return false
}

// matchExcludedAuthor reports the exclude_pr_authors entry login matches. An
// entry either equals the login case-insensitively, or starts with '*' and is a
// case-insensitive SUFFIX match ("*[bot]" covers every GitHub App bot). A bare
// "*" (an empty suffix) matches nothing: the tool refuses it, and a stored one
// must not exclude every PR. An empty login matches nothing.
func matchExcludedAuthor(login string, exclude []string) (string, bool) {
	if login == "" {
		return "", false
	}
	lower := strings.ToLower(login)
	for _, entry := range exclude {
		if suffix, star := strings.CutPrefix(entry, "*"); star {
			if suffix != "" && strings.HasSuffix(lower, strings.ToLower(suffix)) {
				return entry, true
			}
			continue
		}
		if entry != "" && strings.EqualFold(login, entry) {
			return entry, true
		}
	}
	return "", false
}

var (
	// "Re: ", repeated and in any case, at the start of a subject.
	prSubjectReplyRe = regexp.MustCompile(`^(?i:re:\s*)+`)
	// GitHub's "[owner/repo] " subject prefix.
	prSubjectRepoRe = regexp.MustCompile(`^\[[^\]\s/]+/[^\]\s]+\]\s*`)
)

// prReviewTitle is D2 point 4's board title for a pr_review task:
// "Review PR #N — {repo}: {head}", where head is the subject with leading
// "Re: " (repeated, any case), a leading "[owner/repo] ", and a trailing
// " (PR #N)" for THIS N stripped — a different N is left in place. An empty head
// falls back to the body's first line, then to nothing (no dangling
// separator). Truncated with the ONE spelling, textmatch.NormalizedPrefix.
func prReviewTitle(ref github.PRRef, subject, body string) string {
	head := strings.TrimSpace(subject)
	head = strings.TrimSpace(prSubjectReplyRe.ReplaceAllString(head, ""))
	head = strings.TrimSpace(prSubjectRepoRe.ReplaceAllString(head, ""))
	head = strings.TrimSpace(strings.TrimSuffix(head, fmt.Sprintf("(PR #%d)", ref.PR)))
	if head == "" {
		head = ruleFirstLine(body)
	}
	name := ref.Repo
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	title := fmt.Sprintf("Review PR #%d — %s", ref.PR, name)
	if head != "" {
		title += ": " + head
	}
	return textmatch.NormalizedPrefix(title, rulesTitleLen)
}
