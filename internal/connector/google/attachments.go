package google

// SWT-42: list and read the attachments of an ingested message, straight from the
// stored raw envelope (raw_source_items.raw_json). PURE: no I/O. The IMAP
// connector stores each whole RFC822 message base64 in rfc822_b64 up to the
// capture cap; a truncated capture keeps headers + one text part and lists what
// it left behind in `parts`. The text funnel (NormalizeRFC822) deliberately
// ignores attachments — this is the only reader of them.
//
// Part numbering is pathString's, the same one planOversizeFetch writes into a
// truncated manifest, so a part_id names the same part whichever way the row was
// captured (criterion 26).

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
)

// Attachment is one listed part of a message.
type Attachment struct {
	Index       int    `json:"index"`   // 1-based, depth-first
	PartID      string `json:"part_id"` // IMAP numbering (pathString)
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"` // declared media type, lowercased, no parameters
	// Charset is the declared charset parameter; the caller needs it for the
	// latin-1 repair and it is not part of the tool output.
	Charset           string `json:"-"`
	Disposition       string `json:"disposition"`
	SizeBytes         int    `json:"size_bytes"` // decoded size; the manifest's encoded size when truncated
	SizeIsEncoded     bool   `json:"size_is_encoded"`
	Available         bool   `json:"available"`
	UnavailableReason string `json:"unavailable_reason"`
}

// SourceInfo describes the raw row as a whole.
type SourceInfo struct {
	Truncated         bool
	UnavailableReason string // set for a row whose path stores no attachment bytes
}

// AttachmentSelector picks one part: exactly one field is set.
type AttachmentSelector struct {
	Index    int
	Filename string
	PartID   string
}

// GmailPathReason is what a gmail:-shaped (Gmail API / bridge) row says: those
// paths carry an attachment id and a size, never the bytes (criterion 12).
const GmailPathReason = "this message came through the Gmail API/bridge path, which stores no attachment bytes"

// maxAttachmentDepth bounds the multipart walk, like walkForText.
const maxAttachmentDepth = 10

type attachmentEnvelope struct {
	Source    string        `json:"source"`
	Size      int           `json:"size"`
	Truncated bool          `json:"truncated"`
	RFC822B64 string        `json:"rfc822_b64"`
	Parts     []MessagePart `json:"parts,omitempty"`
}

// truncatedReason names the capture cap in force now, in MiB when it is a whole
// number of them. It says why the bytes are absent — never that the part does
// not exist.
func truncatedReason() string {
	cap := MaxMessageBytes()
	size := fmt.Sprintf("%d-byte", cap)
	if cap%(1<<20) == 0 {
		size = fmt.Sprintf("%d MiB", cap>>20)
	}
	return "not stored: message was over the " + size + " capture cap (MAIL_MAX_MESSAGE_BYTES)"
}

// leaf is one walked MIME leaf with its still-transfer-encoded body.
type leaf struct {
	att      Attachment
	encoding string
	body     []byte
}

// ListAttachments lists the attachment parts of one raw envelope.
func ListAttachments(raw json.RawMessage) ([]Attachment, SourceInfo, error) {
	leaves, info, err := walkEnvelope(raw)
	if err != nil || info.UnavailableReason != "" {
		return nil, info, err
	}
	out := make([]Attachment, 0, len(leaves))
	for _, l := range leaves {
		out = append(out, l.att)
	}
	return out, info, nil
}

// ReadAttachment returns one part's transfer-decoded bytes. Charset handling and
// the text-vs-file decision belong to the caller.
func ReadAttachment(raw json.RawMessage, sel AttachmentSelector) (Attachment, []byte, error) {
	leaves, info, err := walkEnvelope(raw)
	if err != nil {
		return Attachment{}, nil, err
	}
	if info.UnavailableReason != "" {
		return Attachment{}, nil, errors.New(info.UnavailableReason)
	}
	var match []int
	for i, l := range leaves {
		switch {
		case sel.Index != 0 && l.att.Index == sel.Index,
			sel.Filename != "" && l.att.Filename == sel.Filename,
			sel.PartID != "" && l.att.PartID == sel.PartID:
			match = append(match, i)
		}
	}
	switch {
	case len(match) == 0 && sel.Index != 0:
		return Attachment{}, nil, fmt.Errorf("index %d is out of range: the message has %d attachment(s)", sel.Index, len(leaves))
	case len(match) == 0 && sel.Filename != "":
		return Attachment{}, nil, fmt.Errorf("no attachment is named %q; list them with mail_list_attachments", sel.Filename)
	case len(match) == 0:
		return Attachment{}, nil, fmt.Errorf("no attachment has part_id %q; list them with mail_list_attachments", sel.PartID)
	case len(match) > 1:
		idx := make([]string, 0, len(match))
		for _, i := range match {
			idx = append(idx, fmt.Sprint(leaves[i].att.Index))
		}
		return Attachment{}, nil, fmt.Errorf("filename %q matches attachments %s; choose one by index", sel.Filename, strings.Join(idx, ", "))
	}
	l := leaves[match[0]]
	if !l.att.Available {
		return l.att, nil, errors.New(l.att.UnavailableReason)
	}
	data, err := decodeTransfer(l.body, l.encoding)
	if err != nil {
		return l.att, nil, fmt.Errorf("decode attachment %d: %w", l.att.Index, err)
	}
	return l.att, data, nil
}

func walkEnvelope(raw json.RawMessage) ([]leaf, SourceInfo, error) {
	var env attachmentEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, SourceInfo{}, fmt.Errorf("parse raw envelope: %w", err)
	}
	if env.Source != "imap" {
		return nil, SourceInfo{UnavailableReason: GmailPathReason}, nil
	}
	info := SourceInfo{Truncated: env.Truncated}
	if env.Truncated {
		reason := truncatedReason()
		leaves := make([]leaf, 0, len(env.Parts))
		for i, p := range env.Parts {
			leaves = append(leaves, leaf{att: Attachment{
				Index: i + 1, PartID: p.PartID, Filename: decodeWord(p.Filename),
				ContentType: strings.ToLower(p.ContentType), SizeBytes: p.Size,
				SizeIsEncoded: true, UnavailableReason: reason,
			}})
		}
		return leaves, info, nil
	}
	rfc822, err := base64.StdEncoding.DecodeString(env.RFC822B64)
	if err != nil {
		return nil, info, fmt.Errorf("decode rfc822_b64: %w", err)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(rfc822))
	if err != nil {
		return nil, info, fmt.Errorf("parse rfc822: %w", err)
	}
	var leaves []leaf
	walkParts(textproto.MIMEHeader(msg.Header), msg.Body, nil, 0, &leaves)
	return leaves, info, nil
}

// walkParts numbers multipart children 1..n per level (pathString), treats
// message/rfc822 as one leaf (a forwarded mail is not walked into), and keeps a
// leaf unless it is a body part: text/plain or text/html with no filename and no
// attachment disposition.
func walkParts(h textproto.MIMEHeader, r io.Reader, path []int, depth int, out *[]leaf) {
	if depth > maxAttachmentDepth {
		return
	}
	mediaType, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil || mediaType == "" {
		mediaType, params = "text/plain", map[string]string{}
	}
	mediaType = strings.ToLower(mediaType)
	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return
		}
		mr := multipart.NewReader(r, boundary)
		for i := 1; ; i++ {
			// NextRawPart: NextPart would silently decode quoted-printable and drop
			// the header, making the decode path depend on the sender's choice.
			part, err := mr.NextRawPart()
			if err != nil {
				return
			}
			walkParts(part.Header, part, append(append([]int(nil), path...), i), depth+1, out)
			part.Close()
		}
	}

	disposition, dparams, err := mime.ParseMediaType(h.Get("Content-Disposition"))
	if err != nil {
		disposition, dparams = strings.ToLower(strings.TrimSpace(strings.SplitN(h.Get("Content-Disposition"), ";", 2)[0])), nil
	}
	disposition = strings.ToLower(disposition)
	filename := decodeWord(dparams["filename"])
	if filename == "" {
		filename = decodeWord(params["name"])
	}
	if (mediaType == "text/plain" || mediaType == "text/html") && filename == "" && disposition != "attachment" {
		return // a body part: the text funnel's, not an attachment
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return
	}
	encoding := strings.ToLower(strings.TrimSpace(h.Get("Content-Transfer-Encoding")))
	size := len(body)
	if decoded, err := decodeTransfer(body, encoding); err == nil {
		size = len(decoded)
	}
	*out = append(*out, leaf{
		att: Attachment{
			Index: len(*out) + 1, PartID: pathString(path), Filename: filename,
			ContentType: mediaType, Charset: params["charset"], Disposition: disposition,
			SizeBytes: size, Available: true,
		},
		encoding: encoding,
		body:     body,
	})
}

func decodeTransfer(body []byte, encoding string) ([]byte, error) {
	switch encoding {
	case "base64":
		return io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(body)))
	case "quoted-printable":
		return io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body)))
	default: // 7bit, 8bit, binary, or none
		return body, nil
	}
}
