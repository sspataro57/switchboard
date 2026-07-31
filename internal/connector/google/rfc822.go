package google

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// NormalizeRFC822 turns one raw IMAP envelope into a NormalizedMessage.
//
// PURE: its only inputs are the raw row and the own-email set. That is what makes
// `--normalize-only --all` possible (criterion 5) — a full rebuild of every
// normalized row must never need an IMAP connection, because the mailbox may be
// unreachable, the UIDs may have been invalidated, and the message may have been
// deleted from the server since. Whatever we learned at fetch time has to be in
// raw_source_items or it is lost.

// maxBodyTextBytes caps BodyText. A mail body is context for triage and drafts,
// not an archive: the raw bytes remain in raw_source_items either way. The cap
// truncates rather than dropping, because the opening of a long mail is the part
// that carries the ask.
const maxBodyTextBytes = 256 * 1024

// imapRawEnvelope is the raw_json shape written by the ingest phase (criterion 5).
type imapRawEnvelope struct {
	Source       string   `json:"source"`
	Folder       string   `json:"folder"`
	UIDValidity  uint32   `json:"uidvalidity"`
	UID          uint32   `json:"uid"`
	InternalDate string   `json:"internaldate"`
	Flags        []string `json:"flags"`
	Size         int      `json:"size"`
	Truncated    bool     `json:"truncated"`
	RFC822B64    string   `json:"rfc822_b64"`
	// Parts lists the non-text parts a truncated capture left behind.
	Parts []MessagePart `json:"parts,omitempty"`
}

// imapExternalID is the raw_source_items key and the last-resort identifier.
// UIDVALIDITY is part of it on purpose: when a folder's generation changes the
// old UIDs mean nothing, so the new rows must not collide with the old ones.
func imapExternalID(folder string, uidValidity, uid uint32) string {
	return fmt.Sprintf("imap:%s:%d:%d", folder, uidValidity, uid)
}

// NormalizeRFC822 is the pure mapper for one IMAP-sourced message.
//
// ownEmails is the set of ALL provider='google' account emails, lowercased — the
// direction rule is reused verbatim from the Gmail-API path, which is what
// guarantees a Sent-folder copy of our own reply normalizes outbound and can
// therefore never be re-triaged into a new task (invariant 5).
func NormalizeRFC822(raw json.RawMessage, accountEmail string, ownEmails map[string]bool) (NormalizedMessage, error) {
	var env imapRawEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return NormalizedMessage{}, fmt.Errorf("parse imap envelope: %w", err)
	}
	if env.RFC822B64 == "" && !env.Truncated {
		return NormalizedMessage{}, fmt.Errorf("imap envelope %s carries no rfc822_b64", imapExternalID(env.Folder, env.UIDValidity, env.UID))
	}
	rfc822Bytes, err := base64.StdEncoding.DecodeString(env.RFC822B64)
	if err != nil {
		// Refuse rather than normalize a half-message: a body we cannot decode
		// would silently become an empty one, and an empty body reads to triage
		// and to a human as "they sent nothing".
		return NormalizedMessage{}, fmt.Errorf("decode rfc822_b64 for %s: %w",
			imapExternalID(env.Folder, env.UIDValidity, env.UID), err)
	}

	externalID := imapExternalID(env.Folder, env.UIDValidity, env.UID)

	msg, err := mail.ReadMessage(strings.NewReader(string(rfc822Bytes)))
	if err != nil {
		return NormalizedMessage{}, fmt.Errorf("parse rfc822 for %s: %w", externalID, err)
	}
	hdr := msg.Header

	// Message-ID is kept VERBATIM, angle brackets included: it is compared by
	// exact string equality against deliveries.sent_external_id for loop closure,
	// and BuildOutboundMIME writes the brackets when it reserves one.
	messageID := strings.TrimSpace(hdr.Get("Message-ID"))
	if messageID == "" {
		messageID = externalID
	}

	sentAt := parseMailDate(hdr.Get("Date"))
	if sentAt.IsZero() {
		// INTERNALDATE is the server's arrival stamp — always present, and the
		// only date available when a sender omits or mangles the Date header.
		if t, err := time.Parse(time.RFC3339, env.InternalDate); err == nil {
			sentAt = t
		}
	}

	sender := decodeWord(hdr.Get("From"))
	direction := "inbound"
	if isOwnAddress(hdr.Get("From"), ownEmails) {
		direction = "outbound"
	}

	// Extract unconditionally. A truncated capture is headers PLUS the text
	// parts — only attachments were left behind — so skipping extraction here
	// would throw away the body of every large message, which is exactly the ask
	// a client writes above a 3 MB PDF.
	body := extractBodyText(rfc822Bytes)
	if manifest := attachmentLine(env.Parts); manifest != "" {
		if body != "" {
			body += "\n\n"
		}
		body += manifest
	}

	// Every text field is forced to valid UTF-8 before it leaves this function.
	// normalized_messages columns are TEXT, and Postgres REJECTS a byte sequence
	// that is not valid UTF-8 — so "return the raw bytes on an unknown charset"
	// (which is the right call for a mail parser) becomes a failed INSERT that
	// aborts the whole normalize pass unless the bytes are repaired here. Found by
	// the first live ingest: a latin-1 (c) at 0xa9 killed the run.
	return NormalizedMessage{
		ThreadKey:         "gmail:" + accountEmail + ":" + threadRoot(hdr, messageID, externalID),
		ExternalMessageID: messageID,
		Direction:         direction,
		SentAt:            sentAt,
		Subject:           toValidUTF8(decodeWord(hdr.Get("Subject"))),
		Sender:            toValidUTF8(sender),
		BodyText:          toValidUTF8(body),
		Channel:           Channel,
		// No Gmail ids exist over IMAP. Leaving these empty is deliberate: the
		// gmail-msgid dedup index keys on external_message_id, not on these.
	}, nil
}

// threadRoot picks the identifier every message in a conversation shares.
//
// References[0] first because it is the ORIGINATING message of the chain —
// In-Reply-To only names the immediate parent, so threading on it would split one
// conversation into a chain of two-message threads. Falling back through
// In-Reply-To, then the message's own id (a thread root replying to nothing), and
// finally the external id so a message with no ids at all still gets a stable,
// non-empty third segment.
func threadRoot(hdr mail.Header, messageID, externalID string) string {
	if refs := strings.Fields(hdr.Get("References")); len(refs) > 0 {
		return refs[0]
	}
	if irt := strings.TrimSpace(hdr.Get("In-Reply-To")); irt != "" {
		// In-Reply-To may legally carry more than one id; the first is the parent.
		if f := strings.Fields(irt); len(f) > 0 {
			return f[0]
		}
	}
	if messageID != "" {
		return messageID
	}
	return externalID
}

// isOwnAddress reports whether a From header names one of our own mailboxes.
// Parsing before comparing matters: `"Salvador Spataro" <x@y>` must match x@y.
func isOwnAddress(from string, ownEmails map[string]bool) bool {
	if from == "" {
		return false
	}
	if addr, err := mail.ParseAddress(from); err == nil {
		return ownEmails[strings.ToLower(strings.TrimSpace(addr.Address))]
	}
	// Unparseable From: fall back to a bare-string comparison rather than
	// defaulting to inbound, which would let one of our own sends be re-triaged.
	return ownEmails[strings.ToLower(strings.TrimSpace(from))]
}

// wordDecoder decodes RFC 2047 encoded-words and tolerates charsets Go does not
// know, returning the raw bytes instead of failing — a subject line in an exotic
// encoding is worth showing imperfectly, never worth dropping the message for.
var wordDecoder = mime.WordDecoder{
	CharsetReader: func(_ string, input io.Reader) (io.Reader, error) { return input, nil },
}

func decodeWord(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	decoded, err := wordDecoder.DecodeHeader(v)
	if err != nil {
		return v
	}
	return decoded
}

func parseMailDate(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	if t, err := mail.ParseDate(v); err == nil {
		return t
	}
	// Senders emit non-conforming dates constantly; try the common shapes before
	// giving up and letting INTERNALDATE win.
	for _, layout := range []string{time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822} {
		if t, err := time.Parse(layout, v); err == nil {
			return t
		}
	}
	return time.Time{}
}

// extractBodyText walks the MIME tree for the first text/plain leaf, falling back
// to a tag-stripped text/html leaf. Best-effort throughout: a body that cannot be
// decoded degrades to whatever bytes are there rather than failing the message.
func extractBodyText(rfc822Bytes []byte) string {
	msg, err := mail.ReadMessage(strings.NewReader(string(rfc822Bytes)))
	if err != nil {
		return ""
	}
	plain, htmlBody := walkForText(msg.Header.Get("Content-Type"), msg.Header.Get("Content-Transfer-Encoding"), msg.Body, 0)
	body := plain
	if body == "" && htmlBody != "" {
		body = stripHTML(htmlBody)
	}
	return capBody(body)
}

// walkForText returns (firstPlain, firstHTML) found at or below this part.
// depth guards against a malformed message nesting multiparts without end.
func walkForText(contentType, encoding string, r io.Reader, depth int) (string, string) {
	if depth > 10 {
		return "", ""
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType == "" {
		// No Content-Type at all is a plain-text message by RFC 2045 default.
		mediaType = "text/plain"
		params = map[string]string{}
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return "", ""
		}
		var plain, htmlBody string
		mr := multipart.NewReader(r, boundary)
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			p, h := walkForText(part.Header.Get("Content-Type"),
				part.Header.Get("Content-Transfer-Encoding"), part, depth+1)
			if plain == "" {
				plain = p
			}
			if htmlBody == "" {
				htmlBody = h
			}
			part.Close()
			if plain != "" && htmlBody != "" {
				break
			}
		}
		return plain, htmlBody
	}

	switch {
	case mediaType == "text/plain":
		return decodeBody(r, encoding), ""
	case mediaType == "text/html":
		return "", decodeBody(r, encoding)
	}
	// Attachments and every other part type are deliberately ignored: this is a
	// text funnel, and raw_source_items already holds the bytes.
	return "", ""
}

// decodeBody applies the transfer encoding. Charset is left as received: Go's
// stdlib has no charset registry, and mangling unknown bytes would be worse than
// showing them (criterion 10 — unknown charset yields raw bytes, not an error).
func decodeBody(r io.Reader, encoding string) string {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "quoted-printable":
		r = quotedprintable.NewReader(r)
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, r)
	}
	b, err := io.ReadAll(io.LimitReader(r, maxBodyTextBytes+1))
	if err != nil && len(b) == 0 {
		return ""
	}
	return string(b)
}

// Go's regexp is RE2: no backreferences, so script and style need one pattern
// each rather than a single `<(script|style)>…</\1>`.
var (
	htmlScriptRE  = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</\s*script\s*>`)
	htmlStyleRE   = regexp.MustCompile(`(?is)<style\b[^>]*>.*?</\s*style\s*>`)
	htmlBreakRE   = regexp.MustCompile(`(?i)<br\s*/?>`)
	htmlBlockRE   = regexp.MustCompile(`(?i)</(p|div|tr|li|h[1-6])\s*>`)
	htmlTagRE     = regexp.MustCompile(`(?s)<[^>]*>`)
	htmlSpaceRE   = regexp.MustCompile(`[ \t]+`)
	htmlNewlineRE = regexp.MustCompile(`\n{3,}`)
)

// stripHTML renders an HTML-only body as readable text: script/style dropped
// wholesale (their contents are not prose), block boundaries turned into
// newlines, tags removed, entities unescaped.
func stripHTML(s string) string {
	// Script and style bodies are code, not prose — dropped wholesale rather
	// than tag-stripped, which would leave their contents in the text.
	s = htmlScriptRE.ReplaceAllString(s, "")
	s = htmlStyleRE.ReplaceAllString(s, "")
	s = htmlBreakRE.ReplaceAllString(s, "\n")
	s = htmlBlockRE.ReplaceAllString(s, "\n")
	s = htmlTagRE.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	// NBSP reads as a space to a human; leaving it makes downstream matching and
	// preview text behave unpredictably.
	s = strings.ReplaceAll(s, " ", " ")
	s = htmlSpaceRE.ReplaceAllString(s, " ")
	s = htmlNewlineRE.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// capBody truncates on a rune boundary so the stored text stays valid UTF-8.
func capBody(s string) string {
	if len(s) <= maxBodyTextBytes {
		return s
	}
	cut := s[:maxBodyTextBytes]
	// Drop a trailing partial sequence rather than storing half a rune.
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// attachmentLine renders the dropped parts as one readable trailing line.
//
// It goes into body_text rather than staying in raw_json because
// normalized_messages has no attachment column, and everything downstream —
// triage, the draft worker, mail_search — reads body_text. A manifest nothing
// can see is not context.
func attachmentLine(parts []MessagePart) string {
	if len(parts) == 0 {
		return ""
	}
	names := make([]string, 0, len(parts))
	for _, p := range parts {
		name := p.Filename
		if name == "" {
			name = p.ContentType
		}
		if p.Size > 0 {
			name = fmt.Sprintf("%s (%s)", name, humanBytes(p.Size))
		}
		names = append(names, name)
	}
	return "[Attachments not stored: " + strings.Join(names, ", ") + "]"
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// toValidUTF8 repairs text that is not valid UTF-8.
//
// Invalid bytes are almost always latin-1: a sender declared no charset, or
// declared one Go cannot decode, and the bytes are ISO-8859-1 or windows-1252.
// Interpreting each stray byte AS a latin-1 code point is lossless for that case
// (0xa9 becomes ©) and is strictly better than substituting U+FFFD, which would
// silently corrupt every accented word in the message.
//
// Valid UTF-8 is returned untouched, so correctly-encoded mail never goes through
// the transcode.
func toValidUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + len(s)/4)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			// A byte that is not part of a valid sequence: read it as latin-1.
			b.WriteRune(rune(s[i]))
			i++
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String()
}
