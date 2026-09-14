package google

// RawMailHeaders (SWT-54, docs/tickets/treetop-pr-review-tasks_SPEC.md D1 and
// criterion 19): the header block of one STORED raw envelope. Capture reads
// GitHub's X-GitHub-* authorship headers here, from raw_source_items.raw_json,
// because normalized_messages carries none of them, and NormalizeRFC822's
// output must not change to carry one (IK "STANDING RULE").

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/mail"
	"net/textproto"
)

// RawMailHeaders returns the RFC822 header of an IMAP raw envelope.
//
//   - ok=false, err=nil: the row is not an IMAP envelope (a Gmail API or bridge
//     row, or '{}'), or it stores no RFC822 bytes. It contributes nothing and
//     must never fail a capture pass.
//   - err != nil: an IMAP envelope whose bytes cannot be decoded.
//
// A TRUNCATED capture still has its headers: the IMAP connector keeps headers
// plus text parts over MAIL_MAX_MESSAGE_BYTES, so walkEnvelope's "truncated
// means nothing to read" shortcut does not apply here.
func RawMailHeaders(raw json.RawMessage) (mail.Header, bool, error) {
	var env attachmentEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, false, fmt.Errorf("parse raw envelope: %w", err)
	}
	if env.Source != "imap" || env.RFC822B64 == "" {
		return nil, false, nil
	}
	rfc822, err := base64.StdEncoding.DecodeString(env.RFC822B64)
	if err != nil {
		return nil, false, fmt.Errorf("decode rfc822_b64: %w", err)
	}
	// The header block only: ReadMIMEHeader stops at the blank line, so the body
	// (possibly large) is never parsed. A header block with no blank line after
	// it (a headers-only capture) ends at EOF, which is still a header.
	tp := textproto.NewReader(bufio.NewReader(bytes.NewReader(rfc822)))
	hdr, err := tp.ReadMIMEHeader()
	if err != nil && (err != io.EOF || len(hdr) == 0) {
		return nil, false, fmt.Errorf("parse rfc822 header: %w", err)
	}
	return mail.Header(hdr), true, nil
}
