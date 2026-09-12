package google

// SWT-42 (docs/tickets/mail-attachments_SPEC.md): the PURE half of the
// attachment tools — criteria 1, 4, 6 (the pinned cases' decoded bytes), 9 (the
// declared names), 11, 12 and 26. ZERO network, ZERO Postgres: the only input is
// the raw_source_items.raw_json envelope, built here the way
// internal/classify/links_integration_test.go builds it — real MIME bytes,
// base64-encoded into an {"source":"imap",…} envelope.
//
// In-package (package google) because criterion 26 compares against the
// unexported planOversizeFetch / pathString numbering.
//
// IMPOSED SURFACE (SPEC "Files likely to touch", attachments.go; the exact field
// names are this test's choice and are reported to the implementer):
//
//	type Attachment struct {
//	    Index             int    // 1-based, depth-first
//	    PartID            string // IMAP numbering, pathString's
//	    Filename          string // AS DECLARED: RFC 2047 / RFC 2231 decoded, never sanitized
//	    ContentType       string // declared media type, lowercased, no parameters
//	    Charset           string // declared charset parameter ("" when none)
//	    Disposition       string // "attachment" | "inline" | ""
//	    SizeBytes         int    // decoded size; the manifest's encoded size when truncated
//	    SizeIsEncoded     bool
//	    Available         bool
//	    UnavailableReason string
//	}
//	type SourceInfo struct {
//	    Truncated         bool   // the envelope's truncated flag
//	    UnavailableReason string // message-level: set for a gmail:-shaped (non-imap) row
//	}
//	type AttachmentSelector struct { Index int; Filename string; PartID string }
//	func ListAttachments(raw json.RawMessage) ([]Attachment, SourceInfo, error)
//	func ReadAttachment(raw json.RawMessage, sel AttachmentSelector) (Attachment, []byte, error)
//
// ReadAttachment returns the part's TRANSFER-decoded bytes (base64 /
// quoted-printable / 7bit alike); charset handling and the text-vs-file
// decision are internal/tools' (criterion 6's rule lives in mailattach.go).
//
// GREENFIELD NOTE — EXPECTED RED: attachments.go does not exist, so this file
// compile-FAILs the google test binary with undefined: ListAttachments,
// ReadAttachment, Attachment, AttachmentSelector.

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"mime/quotedprintable"
	"regexp"
	"strings"
	"testing"

	"github.com/emersion/go-imap"
)

// ---- MIME builders ---------------------------------------------------------------

// attLeaf is one MIME entity: its header lines, a blank line, its body.
func attLeaf(headers []string, body string) string {
	return strings.Join(headers, "\r\n") + "\r\n\r\n" + body
}

// attMulti is a multipart entity whose parts are already-built entities.
func attMulti(mediaType, boundary string, parts ...string) string {
	var b strings.Builder
	b.WriteString("Content-Type: " + mediaType + `; boundary="` + boundary + `"` + "\r\n\r\n")
	for _, p := range parts {
		b.WriteString("--" + boundary + "\r\n" + p + "\r\n")
	}
	b.WriteString("--" + boundary + "--\r\n")
	return b.String()
}

// attMessage prefixes an entity with top-level headers. The entity's own header
// lines continue the same header block.
func attMessage(subject, msgID string, entity string) string {
	return strings.Join([]string{
		"From: Sana Maryam <sana@collab.example>",
		"To: salvador@handsonconnect.example",
		"Subject: " + subject,
		"Message-ID: " + msgID,
		"Date: Thu, 10 Sep 2026 22:06:00 +0000",
		"MIME-Version: 1.0",
	}, "\r\n") + "\r\n" + entity
}

// attB64 wraps base64 at 76 columns, the way mail clients write it.
func attB64(b []byte) string {
	s := base64.StdEncoding.EncodeToString(b)
	var out strings.Builder
	for len(s) > 76 {
		out.WriteString(s[:76] + "\r\n")
		s = s[76:]
	}
	out.WriteString(s)
	return out.String()
}

// attQP is binary-safe quoted-printable: CR/LF are encoded, so the decoded bytes
// are byte-identical to the input.
func attQP(t *testing.T, b []byte) string {
	t.Helper()
	var buf bytes.Buffer
	w := quotedprintable.NewWriter(&buf)
	w.Binary = true
	if _, err := w.Write(b); err != nil {
		t.Fatalf("qp write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("qp close: %v", err)
	}
	return buf.String()
}

func attB64Part(mediaType, filename string, data []byte) string {
	return attLeaf([]string{
		"Content-Type: " + mediaType + `; name="` + filename + `"`,
		`Content-Disposition: attachment; filename="` + filename + `"`,
		"Content-Transfer-Encoding: base64",
	}, attB64(data))
}

// attEnvelope is the imapRawEnvelope shape buildIMAPEnvelope writes.
func attEnvelope(t *testing.T, msg string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"source": "imap", "folder": "INBOX", "uidvalidity": 73, "uid": 77761,
		"internaldate": "2026-09-10T22:06:00Z", "flags": []string{}, "size": len(msg),
		"truncated": false, "rfc822_b64": base64.StdEncoding.EncodeToString([]byte(msg)),
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return raw
}

// ---- the main fixture: 77761's shape ---------------------------------------------

// attRequestJSON carries a marker that appears nowhere else (the integration
// suite's criterion 18 scans audit rows for its twin).
var attRequestJSON = []byte("{\"request\":\"itest-attach-marker-5b1e\",\n \"activities\":[{\"id\":1},{\"id\":2}]}\n")

// attResponseJSON is exactly 84,274 bytes — the worked example's Response.json.
func attResponseJSON() []byte {
	b := []byte(`{"pad":"` + strings.Repeat("x", 84274-10) + `"}`)
	if len(b) != 84274 {
		panic("attResponseJSON is not 84,274 bytes")
	}
	return b
}

var attGetAllJSON = []byte(`[{"id":1,"state":"open"},{"id":2,"state":"closed"}]`)

// attMainFixture: multipart/mixed holding multipart/alternative (plain + html),
// then Request.json (application/octet-stream, base64), Response.json
// (application/json, quoted-printable) and GetAll-Response.json
// (application/octet-stream, 7bit). Three encodings on purpose: size_bytes and
// the bytes returned must not depend on the one the sender picked.
func attMainFixture(t *testing.T) json.RawMessage {
	t.Helper()
	body := attMulti("multipart/mixed", "mix",
		attMulti("multipart/alternative", "alt",
			attLeaf([]string{`Content-Type: text/plain; charset="utf-8"`}, "Hi Salvador, request and responses attached."),
			attLeaf([]string{`Content-Type: text/html; charset="utf-8"`}, "<p>Hi Salvador, request and responses attached.</p>"),
		),
		attB64Part("application/octet-stream", "Request.json", attRequestJSON),
		attLeaf([]string{
			`Content-Type: application/json; name="Response.json"`,
			`Content-Disposition: attachment; filename="Response.json"`,
			"Content-Transfer-Encoding: quoted-printable",
		}, attQP(t, attResponseJSON())),
		attLeaf([]string{
			`Content-Type: application/octet-stream; name="GetAll-Response.json"`,
			`Content-Disposition: attachment; filename="GetAll-Response.json"`,
			"Content-Transfer-Encoding: 7bit",
		}, string(attGetAllJSON)),
	)
	return attEnvelope(t, attMessage("Activities Integration – Request and Response Validation",
		"<itest-attach-main@collab.example>", body))
}

// ---- criterion 1 ---------------------------------------------------------------

func TestListAttachments_MainFixtureListsExactlyTheThreeAttachments(t *testing.T) {
	atts, info, err := ListAttachments(attMainFixture(t))
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if info.Truncated {
		t.Errorf("SourceInfo.Truncated = true for a fully stored message")
	}
	want := []struct {
		partID, filename, contentType string
		size                          int
	}{
		{"2", "Request.json", "application/octet-stream", len(attRequestJSON)},
		{"3", "Response.json", "application/json", 84274},
		{"4", "GetAll-Response.json", "application/octet-stream", len(attGetAllJSON)},
	}
	if len(atts) != len(want) {
		t.Fatalf("ListAttachments returned %d entries (%+v), want exactly the 3 attachments — the text/plain and "+
			"text/html body parts (1.1, 1.2) are left out (criterion 1)", len(atts), atts)
	}
	for i, w := range want {
		a := atts[i]
		if a.Index != i+1 {
			t.Errorf("entry %d: index = %d, want %d (1-based, depth-first)", i, a.Index, i+1)
		}
		if a.PartID != w.partID {
			t.Errorf("entry %d: part_id = %q, want %q (IMAP numbering, pathString's)", i, a.PartID, w.partID)
		}
		if a.Filename != w.filename {
			t.Errorf("entry %d: filename = %q, want %q", i, a.Filename, w.filename)
		}
		if a.ContentType != w.contentType {
			t.Errorf("entry %d: content_type = %q, want %q (declared media type)", i, a.ContentType, w.contentType)
		}
		if a.Disposition != "attachment" {
			t.Errorf("entry %d: disposition = %q, want attachment", i, a.Disposition)
		}
		if a.SizeBytes != w.size {
			t.Errorf("entry %d (%s): size_bytes = %d, want the DECODED size %d — the transfer encoding (base64 / "+
				"quoted-printable / 7bit) must not change it", i, w.filename, a.SizeBytes, w.size)
		}
		if a.SizeIsEncoded {
			t.Errorf("entry %d: size_is_encoded = true on a stored part", i)
		}
		if !a.Available || a.UnavailableReason != "" {
			t.Errorf("entry %d: available=%v reason=%q, want available with no reason", i, a.Available, a.UnavailableReason)
		}
	}
}

// Criterion 5's pure half: every selector finds the same part, and the bytes are
// the part's decoded bytes whatever the transfer encoding.
func TestReadAttachment_EverySelectorReturnsTheDecodedBytes(t *testing.T) {
	raw := attMainFixture(t)
	for _, tc := range []struct {
		name string
		sel  AttachmentSelector
		want []byte
	}{
		{"index 1 (base64)", AttachmentSelector{Index: 1}, attRequestJSON},
		{"filename Request.json", AttachmentSelector{Filename: "Request.json"}, attRequestJSON},
		{"part_id 2", AttachmentSelector{PartID: "2"}, attRequestJSON},
		{"index 2 (quoted-printable)", AttachmentSelector{Index: 2}, attResponseJSON()},
		{"index 3 (7bit)", AttachmentSelector{Index: 3}, attGetAllJSON},
	} {
		t.Run(tc.name, func(t *testing.T) {
			att, data, err := ReadAttachment(raw, tc.sel)
			if err != nil {
				t.Fatalf("ReadAttachment(%+v): %v", tc.sel, err)
			}
			if !bytes.Equal(data, tc.want) {
				t.Errorf("ReadAttachment(%+v) returned %d bytes, want the %d decoded bytes byte-identical",
					tc.sel, len(data), len(tc.want))
			}
			if att.SizeBytes != len(tc.want) {
				t.Errorf("attachment size_bytes = %d, want %d", att.SizeBytes, len(tc.want))
			}
		})
	}
}

// Criterion 20's pure half: a part that is not there, and an ambiguous filename.
func TestReadAttachment_RefusesAMissingOrAmbiguousPart(t *testing.T) {
	if _, _, err := ReadAttachment(attMainFixture(t), AttachmentSelector{Index: 4}); err == nil {
		t.Error("ReadAttachment(index 4) of a 3-attachment message succeeded; index out of range must be an error")
	}
	if _, _, err := ReadAttachment(attMainFixture(t), AttachmentSelector{Filename: "nope.json"}); err == nil {
		t.Error("ReadAttachment(filename nope.json) succeeded; no part has that name")
	}

	dup := attMulti("multipart/mixed", "dup",
		attLeaf([]string{`Content-Type: text/plain; charset="utf-8"`}, "two reports attached"),
		attB64Part("text/csv", "report.csv", []byte("a,b\n1,2\n")),
		attB64Part("application/pdf", "summary.pdf", []byte("%PDF-1.4\n%%EOF\n")),
		attB64Part("text/csv", "report.csv", []byte("a,b\n3,4\n")),
	)
	_, _, err := ReadAttachment(attEnvelope(t, attMessage("dup names", "<itest-attach-dup@x>", dup)),
		AttachmentSelector{Filename: "report.csv"})
	if err == nil {
		t.Fatal("ReadAttachment(filename report.csv) picked one of two parts named report.csv; a filename matching " +
			"two parts must be refused (criterion 20)")
	}
	if !regexp.MustCompile(`\b1\b.*\b3\b`).MatchString(err.Error()) {
		t.Errorf("ambiguous-filename error = %q, want it to list the matching indexes 1 and 3", err)
	}
}

// ---- criterion 4 ---------------------------------------------------------------

func TestListAttachments_TextLeavesWithANameAndForwardedMail(t *testing.T) {
	inner := strings.Join([]string{
		"From: someone@else.example",
		"Subject: the original",
		"MIME-Version: 1.0",
	}, "\r\n") + "\r\n" + attMulti("multipart/mixed", "inner",
		attLeaf([]string{`Content-Type: text/plain`}, "forwarded body"),
		attB64Part("application/pdf", "inner-contract.pdf", []byte("%PDF-1.7 inner")),
	)
	body := attMulti("multipart/mixed", "outer",
		attLeaf([]string{`Content-Type: text/plain; charset="utf-8"`}, "See the notes and the forwarded mail."),
		attLeaf([]string{
			`Content-Type: text/plain; charset="utf-8"; name="notes.txt"`,
			`Content-Disposition: attachment; filename="notes.txt"`,
		}, "a text/plain leaf WITH a filename is an attachment"),
		attLeaf([]string{
			`Content-Type: text/html; charset="utf-8"`,
			`Content-Disposition: attachment`,
		}, "<p>an html leaf with Content-Disposition: attachment and no name</p>"),
		attLeaf([]string{
			`Content-Type: message/rfc822; name="fwd.eml"`,
			`Content-Disposition: attachment; filename="fwd.eml"`,
		}, inner),
	)
	atts, _, err := ListAttachments(attEnvelope(t, attMessage("criterion 4", "<itest-attach-c4@x>", body)))
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	var got []string
	for _, a := range atts {
		got = append(got, a.PartID+"|"+a.ContentType+"|"+a.Filename)
	}
	want := []string{"2|text/plain|notes.txt", "3|text/html|", "4|message/rfc822|fwd.eml"}
	if strings.Join(got, " ; ") != strings.Join(want, " ; ") {
		t.Errorf("ListAttachments = %v, want %v — a text leaf WITH a filename or attachment disposition is listed; "+
			"message/rfc822 is ONE leaf and inner-contract.pdf inside it is not walked into (criterion 4)", got, want)
	}
}

// ---- criterion 6: the pinned cases' bytes come back exactly ---------------------

func attPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatalf("png: %v", err)
	}
	return buf.Bytes()
}

func attZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	_, _ = w.Write([]byte("<w:document/>"))
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// The text-vs-file RULE is internal/tools' (mailattach_test.go pins it). What the
// connector owes it is the exact decoded bytes of every pinned case, plus the
// declared charset of the latin-1 CSV, so that rule can be applied at all.
func TestReadAttachment_PinnedCasesDecodeExactly(t *testing.T) {
	latin1 := []byte("name;city\nJos\xe9;Roma\n")
	cases := []struct {
		filename, mediaType string
		data                []byte
	}{
		{"payload.json", "application/octet-stream", []byte(`{"ok":true,"n":[1,2,3]}`)},
		{"has-nul.txt", "text/plain", []byte("before\x00after")},
		{"doc.pdf", "application/pdf", []byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\n%%EOF\n")},
		{"doc.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", attZip(t)},
		{"pixel.png", "image/png", attPNG(t)},
	}
	parts := []string{attLeaf([]string{`Content-Type: text/plain`}, "pinned cases")}
	for _, c := range cases {
		parts = append(parts, attB64Part(c.mediaType, c.filename, c.data))
	}
	parts = append(parts, attLeaf([]string{
		`Content-Type: text/csv; charset=iso-8859-1; name="latin1.csv"`,
		`Content-Disposition: attachment; filename="latin1.csv"`,
		"Content-Transfer-Encoding: base64",
	}, attB64(latin1)))
	raw := attEnvelope(t, attMessage("pinned", "<itest-attach-pinned@x>", attMulti("multipart/mixed", "pin", parts...)))

	for i, c := range cases {
		att, data, err := ReadAttachment(raw, AttachmentSelector{Index: i + 1})
		if err != nil {
			t.Errorf("ReadAttachment(%s): %v", c.filename, err)
			continue
		}
		if att.Filename != c.filename || !bytes.Equal(data, c.data) {
			t.Errorf("ReadAttachment(index %d) = %q with %d bytes, want %q byte-identical (%d bytes)",
				i+1, att.Filename, len(data), c.filename, len(c.data))
		}
	}
	att, data, err := ReadAttachment(raw, AttachmentSelector{Filename: "latin1.csv"})
	if err != nil {
		t.Fatalf("ReadAttachment(latin1.csv): %v", err)
	}
	if !bytes.Equal(data, latin1) {
		t.Errorf("latin1.csv bytes = %q, want the raw ISO-8859-1 bytes %q: the connector transfer-decodes only; "+
			"the latin-1 repair is internal/tools' (toValidUTF8's rule)", data, latin1)
	}
	if strings.ToLower(att.Charset) != "iso-8859-1" || att.ContentType != "text/csv" {
		t.Errorf("latin1.csv content_type/charset = %q/%q, want text/csv/iso-8859-1 — without the declared charset "+
			"the caller cannot apply criterion 6's repair", att.ContentType, att.Charset)
	}
}

// ---- criterion 9: names arrive AS DECLARED (decoded, not sanitized) -------------

func TestListAttachments_FilenamesAreDecodedButNotSanitized(t *testing.T) {
	encodedWord := "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte("été report.pdf")) + "?="
	body := attMulti("multipart/mixed", "names",
		attLeaf([]string{`Content-Type: text/plain`}, "names"),
		attB64Part("text/plain", "../../.bashrc", []byte("x")),
		attLeaf([]string{
			`Content-Type: application/pdf; name="` + encodedWord + `"`,
			`Content-Disposition: attachment; filename="` + encodedWord + `"`,
			"Content-Transfer-Encoding: base64",
		}, attB64([]byte("%PDF-1.4 rfc2047"))),
		attLeaf([]string{
			`Content-Type: application/pdf`,
			`Content-Disposition: attachment; filename*=UTF-8''r%C3%A9sum%C3%A9%20final.pdf`,
			"Content-Transfer-Encoding: base64",
		}, attB64([]byte("%PDF-1.4 rfc2231"))),
		attLeaf([]string{
			`Content-Type: text/plain`,
			`Content-Disposition: attachment; filename*=UTF-8''a%00b%0D%0Ac.txt`,
		}, "control characters in the name"),
		attLeaf([]string{
			`Content-Type: application/pdf; name="only-ct-name.pdf"`,
			"Content-Transfer-Encoding: base64",
		}, attB64([]byte("%PDF-1.4 ct name"))),
	)
	atts, _, err := ListAttachments(attEnvelope(t, attMessage("names", "<itest-attach-names@x>", body)))
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	want := []string{"../../.bashrc", "été report.pdf", "résumé final.pdf", "a\x00b\r\nc.txt", "only-ct-name.pdf"}
	if len(atts) != len(want) {
		t.Fatalf("ListAttachments returned %d entries, want %d: %+v", len(atts), len(want), atts)
	}
	for i, w := range want {
		if atts[i].Filename != w {
			t.Errorf("entry %d filename = %q, want %q — RFC 2047 and RFC 2231 names are DECODED, a "+
				"Content-Type name= is the fallback (partFilename's order), and nothing is sanitized here: "+
				"sanitizing is the file writer's job in internal/tools (criterion 9)", i+1, atts[i].Filename, w)
		}
	}
}

// ---- criterion 11: a truncated capture ------------------------------------------

func attTruncatedFixture(t *testing.T) json.RawMessage {
	t.Helper()
	msg := attMessage("contract attached (big)", "<itest-attach-trunc@x>",
		attLeaf([]string{`Content-Type: text/plain; charset="utf-8"`}, "The signed contract is attached."))
	raw, err := json.Marshal(map[string]any{
		"source": "imap", "folder": "INBOX", "uidvalidity": 73, "uid": 9001,
		"internaldate": "2026-09-10T22:06:00Z", "flags": []string{}, "size": 3_000_000,
		"truncated": true, "rfc822_b64": base64.StdEncoding.EncodeToString([]byte(msg)),
		"parts": []map[string]any{
			{"part_id": "2", "filename": "contract.pdf", "content_type": "application/pdf", "size": 2_900_000},
			{"part_id": "3", "content_type": "image/png", "size": 40_000},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

const attTruncReason = "not stored: message was over the 1 MiB capture cap (MAIL_MAX_MESSAGE_BYTES)"

func TestListAttachments_TruncatedCaptureListsTheManifestAsUnavailable(t *testing.T) {
	t.Setenv("MAIL_MAX_MESSAGE_BYTES", "") // the default cap, 1 MiB
	atts, info, err := ListAttachments(attTruncatedFixture(t))
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if !info.Truncated {
		t.Errorf("SourceInfo.Truncated = false for a truncated capture")
	}
	if len(atts) != 2 {
		t.Fatalf("ListAttachments = %+v, want the 2 manifest entries", atts)
	}
	for i, w := range []struct {
		partID, filename, ct string
		size                 int
	}{{"2", "contract.pdf", "application/pdf", 2_900_000}, {"3", "", "image/png", 40_000}} {
		a := atts[i]
		if a.Index != i+1 || a.PartID != w.partID || a.Filename != w.filename || a.ContentType != w.ct {
			t.Errorf("entry %d = %+v, want index %d part %s %q %s", i, a, i+1, w.partID, w.filename, w.ct)
		}
		if a.SizeBytes != w.size || !a.SizeIsEncoded {
			t.Errorf("entry %d size = %d (encoded=%v), want the server-reported %d with size_is_encoded=true",
				i, a.SizeBytes, a.SizeIsEncoded, w.size)
		}
		if a.Available {
			t.Errorf("entry %d is available; its bytes were never stored", i)
		}
		if a.UnavailableReason != attTruncReason {
			t.Errorf("entry %d unavailable_reason = %q, want exactly %q", i, a.UnavailableReason, attTruncReason)
		}
	}
}

func TestReadAttachment_TruncatedPartIsAnErrorSayingNotStored(t *testing.T) {
	t.Setenv("MAIL_MAX_MESSAGE_BYTES", "")
	_, _, err := ReadAttachment(attTruncatedFixture(t), AttachmentSelector{Index: 1})
	if err == nil {
		t.Fatal("ReadAttachment on a truncated capture's manifest part succeeded; its bytes were never stored")
	}
	if !strings.Contains(err.Error(), "not stored") || !strings.Contains(err.Error(), "1 MiB") {
		t.Errorf("error = %q, want it to contain \"not stored\" and \"1 MiB\" (criterion 11)", err)
	}
}

// ---- criterion 12: a gmail:-shaped raw row --------------------------------------

const attGmailReason = "this message came through the Gmail API/bridge path, which stores no attachment bytes"

// The Gmail API's format=full resource (also what the bridge writes): an
// attachmentId and a size, never the bytes. No "source":"imap".
var attGmailRaw = json.RawMessage(`{"id":"18c0ffee","threadId":"18c0ffee","internalDate":"1789077960000",
 "payload":{"mimeType":"multipart/mixed","headers":[{"name":"Subject","value":"x"}],
  "parts":[{"partId":"0","mimeType":"text/plain","filename":"","body":{"size":5,"data":"aGVsbG8"}},
           {"partId":"1","mimeType":"application/pdf","filename":"a.pdf","body":{"attachmentId":"ANGjdJ8","size":1234}}]}}`)

func TestListAttachments_GmailShapedRowSaysWhyThereAreNoBytes(t *testing.T) {
	atts, info, err := ListAttachments(attGmailRaw)
	if err != nil {
		t.Fatalf("ListAttachments(gmail-shaped raw) = error %v; listing must RETURN the message with a reason, "+
			"not fail (criterion 12)", err)
	}
	if len(atts) != 0 {
		t.Errorf("ListAttachments(gmail-shaped raw) listed %+v; that path stores no attachment bytes", atts)
	}
	if info.UnavailableReason != attGmailReason {
		t.Errorf("SourceInfo.UnavailableReason = %q, want exactly %q", info.UnavailableReason, attGmailReason)
	}

	_, _, err = ReadAttachment(attGmailRaw, AttachmentSelector{Index: 1})
	if err == nil || !strings.Contains(err.Error(), attGmailReason) {
		t.Errorf("ReadAttachment(gmail-shaped raw) error = %v, want one containing %q", err, attGmailReason)
	}
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "does not exist") {
		t.Errorf("error %q says \"does not exist\": say why, never that (criterion 11/12 heading)", err)
	}
}

// ---- criterion 26: one part numbering -------------------------------------------

func TestListAttachments_PartIDsEqualTheOversizeManifest(t *testing.T) {
	// multipart/mixed
	//   1   multipart/related
	//   1.1   multipart/alternative
	//   1.1.1   text/plain
	//   1.1.2   text/html
	//   1.2   image/png  (inline logo.png)
	//   2   application/pdf (contract.pdf)
	//   3   multipart/mixed
	//   3.1   application/zip (bundle.zip)
	//   3.2   message/rfc822 (fwd.eml)
	fwd := "From: a@b.example\r\nSubject: fwd\r\n\r\nforwarded"
	body := attMulti("multipart/mixed", "m0",
		attMulti("multipart/related", "r1",
			attMulti("multipart/alternative", "a11",
				attLeaf([]string{`Content-Type: text/plain`}, "plain"),
				attLeaf([]string{`Content-Type: text/html`}, "<p>html</p>"),
			),
			attLeaf([]string{
				`Content-Type: image/png; name="logo.png"`,
				`Content-Disposition: inline; filename="logo.png"`,
				`Content-ID: <logo@x>`,
				"Content-Transfer-Encoding: base64",
			}, attB64(attPNG(t))),
		),
		attB64Part("application/pdf", "contract.pdf", []byte("%PDF-1.4 c")),
		attMulti("multipart/mixed", "m3",
			attB64Part("application/zip", "bundle.zip", attZip(t)),
			attLeaf([]string{`Content-Type: message/rfc822; name="fwd.eml"`, `Content-Disposition: attachment; filename="fwd.eml"`}, fwd),
		),
	)
	atts, _, err := ListAttachments(attEnvelope(t, attMessage("numbering", "<itest-attach-26@x>", body)))
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}

	leaf := func(typ, sub string, disp map[string]string) *imap.BodyStructure {
		return &imap.BodyStructure{MIMEType: typ, MIMESubType: sub, DispositionParams: disp, Size: 100}
	}
	bs := &imap.BodyStructure{MIMEType: "multipart", MIMESubType: "mixed", Parts: []*imap.BodyStructure{
		{MIMEType: "multipart", MIMESubType: "related", Parts: []*imap.BodyStructure{
			{MIMEType: "multipart", MIMESubType: "alternative", Parts: []*imap.BodyStructure{
				leaf("text", "plain", nil), leaf("text", "html", nil),
			}},
			leaf("image", "png", map[string]string{"filename": "logo.png"}),
		}},
		leaf("application", "pdf", map[string]string{"filename": "contract.pdf"}),
		{MIMEType: "multipart", MIMESubType: "mixed", Parts: []*imap.BodyStructure{
			leaf("application", "zip", map[string]string{"filename": "bundle.zip"}),
			leaf("message", "rfc822", map[string]string{"filename": "fwd.eml"}),
		}},
	}}
	_, _, _, manifest := planOversizeFetch(bs)

	var fromList, fromManifest []string
	for _, a := range atts {
		fromList = append(fromList, a.PartID)
	}
	for _, p := range manifest {
		fromManifest = append(fromManifest, p.PartID)
	}
	if strings.Join(fromList, ",") != strings.Join(fromManifest, ",") {
		t.Errorf("ListAttachments part_ids = %v, planOversizeFetch manifest part_ids = %v — ONE numbering "+
			"(pathString), or a truncated row's part_id and a stored row's part_id name different parts (criterion 26)",
			fromList, fromManifest)
	}
	if want := "1.2,2,3.1,3.2"; strings.Join(fromManifest, ",") != want {
		t.Fatalf("POSITIVE CONTROL: the manifest itself is %v, want %s — the fixture no longer exercises nesting",
			fromManifest, want)
	}
}
