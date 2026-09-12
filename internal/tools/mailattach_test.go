package tools

// SWT-42 (docs/tickets/mail-attachments_SPEC.md) criteria 6-10 and 20 — the
// no-database half of mail_list_attachments / mail_read_attachment. ZERO
// network, ZERO Postgres: the text rule, the inline cap, the file writer and
// the validators are pure (or local-filesystem) helpers in mailattach.go, so
// they are exercised directly; the Postgres-backed half (class, finder,
// handlers, audit) is mailattach_integration_test.go.
//
// In-package (package tools), the priority_test.go precedent: criteria 6-10
// are properties of unexported helpers.
//
// IMPOSED SURFACE (SPEC "Files likely to touch", mailattach.go; the SPEC fixes
// the constants' names, this test fixes the helpers' — reported to the
// implementer):
//
//	const mailAttachmentTextCap = 100 << 10            // criterion 7
//	const mailAttachmentFileTTL = 7 * 24 * time.Hour   // criterion 10
//	const mailAttachFinderDefaultLimit = 10            // criterion 3
//	const mailAttachFinderMaxLimit = 25                // criterion 3
//	const mailboxCleanMinFiled = 20                    // criterion 13, O2
//
//	// criterion 6: (text, true) when the part is text by CONTENT; the returned
//	// text is the bytes as a string, latin-1-repaired for a declared text/*
//	// part whose charset Go cannot read (toValidUTF8's rule).
//	func attachmentText(contentType, charset string, data []byte) (string, bool)
//
//	// criterion 7: one inline page from byte offset. Fields read here:
//	// Text, Offset, ReturnedBytes, TotalBytes, Truncated, NextOffset.
//	func pageAttachmentText(text string, offset int) (<page struct>, error)
//
//	// criteria 8-10: write under <os.UserCacheDir()>/switchboard/attachments/
//	// <rawID>/<index>-<sanitized name> through an os.Root, after the 7-day sweep;
//	// returns the absolute path.
//	func writeAttachmentFile(rawID int64, index int, filename string, data []byte) (string, error)
//
//	func validateMailListAttachments(args []byte) error   // criterion 20
//	func validateMailReadAttachment(args []byte) error    // criterion 20
//
// GREENFIELD NOTE — EXPECTED RED: mailattach.go does not exist, so this file
// compile-FAILs the internal/tools test binary (every test in the package,
// in-package and tools_test alike) with undefined: mailAttachmentTextCap,
// attachmentText, pageAttachmentText, writeAttachmentFile, ... — the
// priority_test.go precedent. Once it compiles, the refusal table fails with
// `unknown tool "mail_list_attachments"` until createtask.go registers both.

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"image"
	"image/png"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
)

// ---- the constants the SPEC names --------------------------------------------------

func TestMailAttach_ConstantsAreTheSPECs(t *testing.T) {
	if mailAttachmentTextCap != 100*1024 {
		t.Errorf("mailAttachmentTextCap = %d, want 100 KiB (criterion 7, D7: covers the 84,274-byte Response.json "+
			"in one call)", mailAttachmentTextCap)
	}
	if mailAttachmentFileTTL != 7*24*time.Hour {
		t.Errorf("mailAttachmentFileTTL = %v, want 7 days (criterion 10, D9)", mailAttachmentFileTTL)
	}
	if mailAttachFinderDefaultLimit != 10 || mailAttachFinderMaxLimit != 25 {
		t.Errorf("finder limits = %d/%d, want default 10, cap 25 (criterion 3)",
			mailAttachFinderDefaultLimit, mailAttachFinderMaxLimit)
	}
	if mailboxCleanMinFiled != 20 {
		t.Errorf("mailboxCleanMinFiled = %d, want 20 (criterion 13, owner decision O2)", mailboxCleanMinFiled)
	}
}

// ---- criterion 6: text vs file, decided from the content --------------------------

func maPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatalf("png: %v", err)
	}
	return buf.Bytes()
}

func maZip(t *testing.T, entry string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(entry)
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	_, _ = w.Write([]byte("<x/>"))
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestAttachmentText_IsDecidedFromTheContent(t *testing.T) {
	json := []byte(`{"request":"activities","n":[1,2,3]}`)
	pad := bytes.Repeat([]byte("a"), 600) // past net/http.DetectContentType's 512-byte window
	for _, tc := range []struct {
		name, contentType, charset string
		data                       []byte
		wantText                   bool
		want                       string // checked when non-empty
	}{
		// The pinned cases (criterion 6).
		{"octet-stream JSON is text", "application/octet-stream", "", json, true, string(json)},
		{"text/plain containing a NUL is a file", "text/plain", "utf-8", []byte("before\x00after"), false, ""},
		{"%PDF- is a file", "application/pdf", "", []byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\n%%EOF\n"), false, ""},
		{"a docx (zip) is a file", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "",
			maZip(t, "word/document.xml"), false, ""},
		{"an xlsx (zip) declared octet-stream is a file", "application/octet-stream", "",
			maZip(t, "xl/workbook.xml"), false, ""},
		{"PNG is a file", "image/png", "", maPNG(t), false, ""},
		{"an iso-8859-1 CSV is repaired as latin-1", "text/csv", "iso-8859-1",
			[]byte("name;city\nJos\xe9;Roma\n"), true, "name;city\nJosé;Roma\n"},
		// The three-part rule: each clause must bite on its own.
		{"a BOM-prefixed JSON is text (UTF-8 is judged after stripping the BOM)", "application/json", "",
			append([]byte("\xef\xbb\xbf"), json...), true, ""},
		{"a NUL within the first 8 KiB but past the sniff window is a file", "text/plain", "",
			append(append([]byte(nil), pad...), 0, 'z'), false, ""},
		{"invalid UTF-8 past the sniff window, no text/* declaration, is a file", "application/octet-stream", "",
			append(append([]byte(nil), pad...), 0xff, 0xfe, 'z'), false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, isText := attachmentText(tc.contentType, tc.charset, tc.data)
			if isText != tc.wantText {
				t.Fatalf("attachmentText(%q, %q, %d bytes) text=%v, want %v (criterion 6: text means no NUL in the "+
					"first 8 KiB, valid UTF-8 after a BOM, and DetectContentType text/*)",
					tc.contentType, tc.charset, len(tc.data), isText, tc.wantText)
			}
			if tc.want != "" && got != tc.want {
				t.Errorf("attachmentText text = %q, want %q", got, tc.want)
			}
			if isText && !utf8.ValidString(got) {
				t.Errorf("attachmentText returned invalid UTF-8 as text: %q", got)
			}
		})
	}
}

// ---- criterion 7: the 100 KiB inline cap -----------------------------------------

func TestPageAttachmentText_CapsOnARuneBoundaryAndPagesTheRest(t *testing.T) {
	total := 150 * 1024
	// A 2-byte rune straddles the cap: bytes cap-1 and cap. The capBody rule backs
	// off to cap-1 rather than returning half a rune.
	text := strings.Repeat("a", mailAttachmentTextCap-1) + "é" + strings.Repeat("b", total-mailAttachmentTextCap-1)
	if len(text) != total {
		t.Fatalf("fixture is %d bytes, want %d", len(text), total)
	}
	n := mailAttachmentTextCap - 1

	first, err := pageAttachmentText(text, 0)
	if err != nil {
		t.Fatalf("pageAttachmentText(offset 0): %v", err)
	}
	if !first.Truncated || first.ReturnedBytes != n || first.TotalBytes != total || first.NextOffset != n || first.Offset != 0 {
		t.Errorf("first page = truncated %v, returned %d, total %d, next %d, offset %d; want true, %d, %d, %d, 0 — "+
			"cut on a rune boundary, never mid-rune (criterion 7)", first.Truncated, first.ReturnedBytes,
			first.TotalBytes, first.NextOffset, first.Offset, n, total, n)
	}
	line := fmt.Sprintf("[attachment truncated: showing bytes 0–%d of %d; call mail_read_attachment again with "+
		"offset=%d, or to_file=true]", n, total, n)
	if !strings.HasPrefix(first.Text, text[:n]) {
		t.Errorf("first page does not start with the first %d bytes of the part", n)
	}
	if !strings.HasSuffix(first.Text, line) {
		t.Errorf("first page does not END with the line %q; tail: %q", line, first.Text[max(0, len(first.Text)-200):])
	} else if mid := first.Text[n : len(first.Text)-len(line)]; strings.Trim(mid, "\n") != "" {
		t.Errorf("between the content and the truncation line: %q, want only a line break", mid)
	}
	if !utf8.ValidString(first.Text) {
		t.Errorf("first page is not valid UTF-8: it cut a rune")
	}

	rest, err := pageAttachmentText(text, n)
	if err != nil {
		t.Fatalf("pageAttachmentText(offset %d): %v", n, err)
	}
	if rest.Text != text[n:] || rest.Truncated || rest.ReturnedBytes != total-n || rest.Offset != n || rest.TotalBytes != total {
		t.Errorf("second page = %d bytes (truncated %v, returned %d, offset %d, total %d); want exactly the "+
			"remaining %d bytes, not truncated", len(rest.Text), rest.Truncated, rest.ReturnedBytes, rest.Offset,
			rest.TotalBytes, total-n)
	}

	if _, err := pageAttachmentText(text, total+1); err == nil {
		t.Errorf("an offset past the end (%d of %d) was accepted; it must be an error", total+1, total)
	}
	if _, err := pageAttachmentText(text, n+1); err == nil {
		t.Errorf("offset %d is the second byte of \"é\" and was accepted; an offset not on a rune boundary is an error", n+1)
	}
}

func TestPageAttachmentText_TheWorkedExamplesLargestPartFitsInOneCall(t *testing.T) {
	resp := `{"pad":"` + strings.Repeat("x", 84274-10) + `"}`
	page, err := pageAttachmentText(resp, 0)
	if err != nil {
		t.Fatalf("pageAttachmentText: %v", err)
	}
	if page.Truncated || page.Text != resp || page.ReturnedBytes != 84274 || page.TotalBytes != 84274 {
		t.Errorf("the 84,274-byte Response.json came back truncated=%v with %d bytes; D7: the cap covers it in one call",
			page.Truncated, len(page.Text))
	}
}

// ---- criterion 8: the file writer ------------------------------------------------

func maMode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	st, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return st.Mode().Perm()
}

func TestWriteAttachmentFile_PathModesAndDigest(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	data := []byte(`[{"id":1,"state":"open"},{"id":2,"state":"closed"}]`)

	path, err := writeAttachmentFile(77761, 3, "GetAll-Response.json", data)
	if err != nil {
		t.Fatalf("writeAttachmentFile: %v", err)
	}
	want := filepath.Join(cache, "switchboard", "attachments", "77761", "3-GetAll-Response.json")
	if path != want {
		t.Errorf("path = %q, want %q (<os.UserCacheDir()>/switchboard/attachments/<raw id>/<index>-<name>)", path, want)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("path %q is not absolute", path)
	}
	if ucd, err := os.UserCacheDir(); err != nil || !strings.HasPrefix(path, ucd+string(filepath.Separator)) {
		t.Errorf("path %q is not under os.UserCacheDir() (%q, %v)", path, ucd, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if sha256.Sum256(got) != sha256.Sum256(data) {
		t.Errorf("the file's sha256 differs from the decoded part's")
	}
	if m := maMode(t, path); m != 0o600 {
		t.Errorf("file mode = %o, want 0600", m)
	}
	for _, dir := range []string{filepath.Join(cache, "switchboard", "attachments"), filepath.Dir(path)} {
		if m := maMode(t, dir); m != 0o700 {
			t.Errorf("directory %s mode = %o, want 0700", dir, m)
		}
	}

	// The same part saved again (a second to_file read) lands on the same path.
	again, err := writeAttachmentFile(77761, 3, "GetAll-Response.json", data)
	if err != nil || again != path {
		t.Errorf("second write = %q, %v; want the same path, no error", again, err)
	}
}

// ---- criterion 9: no path escapes -----------------------------------------------

func TestWriteAttachmentFile_EveryHostileNameLandsInsideTheBase(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	base := filepath.Join(cache, "switchboard", "attachments")
	safe := regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

	for i, tc := range []struct {
		name, want, suffix string // want: the exact sanitized name, "" = properties only
	}{
		{"../../.bashrc", "bashrc", ""},
		{"/etc/passwd", "passwd", ""},
		{`..\..\x`, "", ""},
		{".hidden", "hidden", ""},
		{"a\x00b\r\nc.txt", "a_b__c.txt", ""},
		{strings.Repeat("n", 296) + ".pdf", strings.Repeat("n", 76) + ".pdf", ""}, // a 300-byte name
		{"été report.pdf", "", ".pdf"},                                            // an RFC 2047 encoded-word name, as ListAttachments decodes it
		{"résumé final.pdf", "", ".pdf"},                                          // an RFC 2231 filename* name, decoded
		{"", "part", ""},
		{"..", "part", ""},
	} {
		idx := i + 1
		t.Run(fmt.Sprintf("%d %q", idx, tc.name), func(t *testing.T) {
			path, err := writeAttachmentFile(4242, idx, tc.name, []byte("payload"))
			if err != nil {
				t.Fatalf("writeAttachmentFile(%q): %v", tc.name, err)
			}
			if rel, err := filepath.Rel(base, path); err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
				t.Fatalf("%q landed at %q, OUTSIDE %s (criterion 9)", tc.name, path, base)
			}
			if filepath.Dir(path) != filepath.Join(base, "4242") {
				t.Errorf("%q landed in %s, want directly in %s", tc.name, filepath.Dir(path), filepath.Join(base, "4242"))
			}
			file := filepath.Base(path)
			prefix := fmt.Sprintf("%d-", idx)
			if !strings.HasPrefix(file, prefix) {
				t.Fatalf("file %q does not start with %q (<index>-<sanitized>)", file, prefix)
			}
			s := strings.TrimPrefix(file, prefix)
			if !safe.MatchString(s) || strings.HasPrefix(s, ".") || len(s) > 80 {
				t.Errorf("sanitized %q -> %q: want only [A-Za-z0-9._-], no leading dot, at most 80 bytes", tc.name, s)
			}
			if tc.want != "" && s != tc.want {
				t.Errorf("sanitized %q -> %q, want %q", tc.name, s, tc.want)
			}
			if tc.suffix != "" && !strings.HasSuffix(s, tc.suffix) {
				t.Errorf("sanitized %q -> %q lost its extension %s", tc.name, s, tc.suffix)
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != "payload" {
				t.Errorf("read back %s = %q, %v", path, got, err)
			}
		})
	}

	// Nothing was written anywhere else under the cache.
	_ = filepath.WalkDir(cache, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if rel, _ := filepath.Rel(base, p); strings.HasPrefix(rel, "..") {
				t.Errorf("a file landed outside the base: %s", p)
			}
		}
		return nil
	})
}

// "All writes go through an os.Root opened on the base directory, so a symlink
// planted inside it pointing outside is refused, not followed (test)."
func TestWriteAttachmentFile_RefusesASymlinkPlantedInsideTheBase(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	base := filepath.Join(cache, "switchboard", "attachments")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}

	t.Run("a raw-id directory that is a symlink out of the base", func(t *testing.T) {
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(base, "5151")); err != nil {
			t.Fatal(err)
		}
		if _, err := writeAttachmentFile(5151, 1, "evil.txt", []byte("pwned")); err == nil {
			t.Errorf("writeAttachmentFile through base/5151 -> %s succeeded; the symlink must be REFUSED, not followed", outside)
		}
		if entries, _ := os.ReadDir(outside); len(entries) != 0 {
			t.Errorf("the write escaped the base through a symlink: %s now holds %d entries", outside, len(entries))
		}
	})

	t.Run("a file name that is a symlink out of the base", func(t *testing.T) {
		outside := t.TempDir()
		victim := filepath.Join(outside, "victim.txt")
		if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(base, "5252"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, filepath.Join(base, "5252", "1-victim.txt")); err != nil {
			t.Fatal(err)
		}
		_, _ = writeAttachmentFile(5252, 1, "victim.txt", []byte("pwned"))
		if got, _ := os.ReadFile(victim); string(got) != "original" {
			t.Errorf("the write followed base/5252/1-victim.txt out of the base and overwrote %s with %q", victim, got)
		}
	})
}

// ---- criterion 10: the 7-day sweep -----------------------------------------------

func maBackdate(t *testing.T, age time.Duration, paths ...string) {
	t.Helper()
	when := time.Now().Add(-age)
	for _, p := range paths {
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatalf("chtimes %s: %v", p, err)
		}
	}
}

func maPlant(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWriteAttachmentFile_SweepsEntriesOlderThanSevenDays(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	base := filepath.Join(cache, "switchboard", "attachments")

	old := filepath.Join(base, "1111", "1-old.txt")
	young := filepath.Join(base, "2222", "1-young.txt")
	stale := filepath.Join(cache, "other-app", "stale.txt") // not under the base: never touched
	locked := filepath.Join(base, "3333", "1-locked.txt")   // the sweep cannot read this directory
	for _, p := range []string{old, young, stale, locked} {
		maPlant(t, p)
	}
	// Files first, then their directories (creating a file bumps its directory's
	// mtime), so a sweep keyed on either one sees the intended age.
	maBackdate(t, 8*24*time.Hour, old, filepath.Dir(old), stale, filepath.Dir(stale), locked, filepath.Dir(locked))
	maBackdate(t, 6*24*time.Hour, young, filepath.Dir(young))
	if err := os.Chmod(filepath.Dir(locked), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(locked), 0o700) })

	path, err := writeAttachmentFile(9999, 1, "new.txt", []byte("new"))
	if err != nil {
		t.Fatalf("writeAttachmentFile: %v — the sweep is best-effort and its errors (an unreadable directory "+
			"under the base) must never fail the write (criterion 10)", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the new file is missing: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("%s (8 days old) survived the write; entries older than mailAttachmentFileTTL are removed first", old)
	}
	if _, err := os.Stat(young); err != nil {
		t.Errorf("%s (6 days old) was removed: %v; only entries older than 7 days go", young, err)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("%s is OUTSIDE the base and was removed: %v; the sweep is confined to the attachments directory", stale, err)
	}
}

// ---- criterion 20: validation ----------------------------------------------------

func maValidateExecutor() *executor.Executor {
	reg := executor.NewRegistry()
	Register(reg, nil) // nil pool: Register only builds closures
	return executor.New(reg, policy.NewStatic(reg.Names()...), audit.NewMemStore())
}

// maExecute runs one call and turns a handler panic (validation passed, so the
// handler ran against the nil pool) into an error the table can report.
func maExecute(ex *executor.Executor, tool, args string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("PANIC: validation ACCEPTED these args and the handler ran on a nil pool: %v", r)
		}
	}()
	_, err = ex.Execute(context.Background(), executor.Call{Tool: tool, Actor: "mcp:manual:test", Args: []byte(args)})
	return err
}

func TestValidate_MailAttachmentTools_Refuse(t *testing.T) {
	ex := maValidateExecutor()
	for _, tc := range []struct{ tool, args, why string }{
		// mail_list_attachments
		{"mail_list_attachments", `{}`, "no identifier"},
		{"mail_list_attachments", `{"worker_id":"manual:test"}`, "no identifier (worker_id is injected, not an identifier)"},
		{"mail_list_attachments", `{"since":"2026-09-01T00:00:00Z","limit":5}`, "the finder needs from or subject (criterion 3)"},
		{"mail_list_attachments", `{"from":"   "}`, "a blank from is no selector"},
		{"mail_list_attachments", `{"raw_source_item_id":1,"from":"sana"}`, "families mixed: an id with finder fields"},
		{"mail_list_attachments", `{"message_id":"<a@b>","subject":"x"}`, "families mixed: an id with finder fields"},
		{"mail_list_attachments", `{"thread_key":"gmail:a@b:<r>","since":"2026-09-01T00:00:00Z"}`, "families mixed: an id with a finder field"},
		{"mail_list_attachments", `{"raw_source_item_id":1,"message_id":"<a@b>"}`, "two id kinds"},
		{"mail_list_attachments", `{"thread_id":3,"thread_key":"gmail:a@b:<r>"}`, "two id kinds"},
		{"mail_list_attachments", `{"from":"sana","limit":-1}`, "a negative limit"},
		// mail_read_attachment
		{"mail_read_attachment", `{"index":1}`, "no identifier"},
		{"mail_read_attachment", `{"thread_id":3,"index":1}`, "a thread is not a read identifier (raw_source_item_id | message_id)"},
		{"mail_read_attachment", `{"raw_source_item_id":1}`, "no part selector"},
		{"mail_read_attachment", `{"raw_source_item_id":1,"message_id":"<a@b>","index":1}`, "two id kinds"},
		{"mail_read_attachment", `{"raw_source_item_id":1,"index":1,"filename":"Request.json"}`, "two part selectors (index + filename)"},
		{"mail_read_attachment", `{"raw_source_item_id":1,"filename":"a.json","part_id":"2"}`, "two part selectors (filename + part_id)"},
		{"mail_read_attachment", `{"raw_source_item_id":1,"index":0}`, "index out of range (1-based)"},
		{"mail_read_attachment", `{"raw_source_item_id":1,"index":-2}`, "index out of range"},
		{"mail_read_attachment", `{"raw_source_item_id":1,"index":1,"offset":-5}`, "a negative offset"},
	} {
		t.Run(tc.tool+" "+tc.args, func(t *testing.T) {
			err := maExecute(ex, tc.tool, tc.args)
			if err == nil {
				t.Fatalf("%s accepted %s — criterion 20 refuses it: %s", tc.tool, tc.args, tc.why)
			}
			if !strings.Contains(err.Error(), "validate "+tc.tool+" args") {
				t.Errorf("%s %s failed with %q, want a VALIDATION refusal (\"validate %s args: …\") — %s",
					tc.tool, tc.args, err, tc.tool, tc.why)
			}
		})
	}
}

// The positive half, on the validators directly (the executor would run the
// handler on a nil pool): every legal family, with and without the injected
// worker_id, passes. A validator that refused everything would pass the table
// above; this is what stops that.
func TestValidate_MailAttachmentTools_AcceptEveryFamily(t *testing.T) {
	for _, args := range []string{
		`{"raw_source_item_id":77761,"worker_id":"manual:salvo"}`,
		`{"message_id":"<a@b>"}`,
		`{"thread_id":3}`,
		`{"thread_key":"gmail:a@b:<r>"}`,
		`{"from":"sana"}`,
		`{"subject":"Activities","since":"2026-09-01T00:00:00Z","until":"2026-09-30T00:00:00Z","limit":25}`,
		`{"from":"sana","limit":30,"worker_id":"manual:salvo"}`, // capped at 25, not refused
	} {
		if err := validateMailListAttachments([]byte(args)); err != nil {
			t.Errorf("validateMailListAttachments(%s) = %v, want nil", args, err)
		}
	}
	for _, args := range []string{
		`{"raw_source_item_id":77761,"index":1,"worker_id":"manual:salvo"}`,
		`{"message_id":"<a@b>","filename":"Request.json"}`,
		`{"raw_source_item_id":77761,"part_id":"1.3","offset":102399}`,
		`{"raw_source_item_id":77761,"index":2,"to_file":true}`,
		`{"raw_source_item_id":77761,"index":1,"offset":0}`,
	} {
		if err := validateMailReadAttachment([]byte(args)); err != nil {
			t.Errorf("validateMailReadAttachment(%s) = %v, want nil", args, err)
		}
	}
}

// Codex review: the finder's from/subject are literal substrings. An unescaped
// "%" or "_" would match every sender and make one call MIME-walk the mailbox.
func TestLikeEscape_WildcardsAreLiteral(t *testing.T) {
	for in, want := range map[string]string{
		"sana":        "sana",
		"%":           `\%`,
		"_":           `\_`,
		`a\b`:         `a\\b`,
		`100%_done\x`: `100\%\_done\\x`,
	} {
		if got := likeEscape(in); got != want {
			t.Errorf("likeEscape(%q) = %q, want %q", in, got, want)
		}
	}
	if mailAttachFinderByteBudget <= 0 || mailAttachFinderByteBudget > 64<<20 {
		t.Errorf("mailAttachFinderByteBudget = %d, want a positive budget of at most 64 MiB", mailAttachFinderByteBudget)
	}
}
