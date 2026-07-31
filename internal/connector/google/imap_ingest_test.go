package google_test

// Offline unit tests for the IMAP ingest phase (SPEC imap-mail-connector,
// acceptance criteria 1, 5, 6, 7, 8, 9, 19). Everything runs against the
// in-memory fakeIMAP MailSource (fake_imap_test.go) and a raw-retaining fake
// Sink — ZERO network, ZERO Postgres, no IMAP server. The raw-first upsert
// DECISION, the SEARCH/UID window arithmetic, the UIDVALIDITY resync and the
// cursor-advanced-last rule live in IngestIMAP (not the sink), so they are
// genuinely exercised here (same shape as poller_test.go for IngestGmail).
//
// GREENFIELD NOTE: imap.go / imap_ingest.go do not exist yet; this file
// compile-FAILs under `go test ./...` until they do — the expected failure
// mode. The imposed surface is documented at the top of fake_imap_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/chash"
	"github.com/sspataro57/switchboard/internal/connector/google"
)

var imapNow = time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

// imapMsg builds one FetchedMessage as the source would hand it back.
func imapMsg(uid uint32, at time.Time, flags []string, msg []byte) google.FetchedMessage {
	return google.FetchedMessage{
		UID: uid, InternalDate: at, Flags: flags, Size: len(msg), RFC822: msg,
	}
}

// inboxOnly is a LIST result with a single selectable folder.
func inboxOnly(uidValidity uint32) []google.Folder {
	return []google.Folder{{Name: imapINBOX, UIDValidity: uidValidity}}
}

func imapCfg() google.Config {
	return google.Config{
		Now:             imapNow,
		Backfill:        google.DefaultBackfill,
		MaxMessageBytes: google.DefaultMaxMessageBytes,
	}
}

// searchFor returns the recorded Search call for a folder (last one wins).
func searchFor(t *testing.T, src *fakeIMAP, folder string) fakeSearchCall {
	t.Helper()
	for i := len(src.searches) - 1; i >= 0; i-- {
		if src.searches[i].folder == folder {
			return src.searches[i]
		}
	}
	t.Fatalf("no SEARCH issued for folder %q; searches=%+v", folder, src.searches)
	return fakeSearchCall{}
}

// ---- criterion 5: raw-first, external id, base64 envelope --------------------

func TestIngestIMAP_RawFirstEnvelopeAndExternalID(t *testing.T) {
	ctx := context.Background()

	// An 8-bit-clean body: a JSON string could not carry these bytes, which is
	// exactly why the envelope is base64 (SPEC decision 5).
	body := "Ciao \xe8\xe9 — latin1 bytes \x00\x01 and a NUL"
	msg := rfc822([]string{
		`Message-ID: <raw1@acme.example>`,
		`From: client@acme.example`,
		`Subject: raw first`,
	}, body)

	src := newFakeIMAP()
	src.folders = inboxOnly(12)
	src.add(imapINBOX, imapMsg(88230, imapNow.Add(-2*time.Hour), []string{"\\Answered", "\\Flagged"}, msg))

	sink := newFakeIMAPSink()
	stats, err := google.IngestIMAP(ctx, src, sink, imapAccount(), imapCfg())
	if err != nil {
		t.Fatalf("IngestIMAP: %v", err)
	}

	const wantID = "imap:INBOX:12:88230"
	w := sink.write(wantID)
	if w == nil {
		t.Fatalf("no raw row for %q (external_id must be imap:{folder}:{uidvalidity}:{uid}); writes=%+v", wantID, sink.writes)
	}
	if w.hash == "" {
		t.Errorf("raw row %q written with an empty content_hash", wantID)
	}
	if wantHash, err := chash.ContentHash(w.raw); err != nil {
		t.Errorf("raw_json is not hashable by chash.ContentHash: %v", err)
	} else if w.hash != wantHash {
		t.Errorf("content_hash = %q, want chash.ContentHash(raw_json) = %q", w.hash, wantHash)
	}

	env := sink.envelope(t, wantID)
	if env.Source != "imap" {
		t.Errorf(`raw_json.source = %q, want "imap" (the normalize dispatch discriminator)`, env.Source)
	}
	if env.Folder != imapINBOX || env.UIDValidity != 12 || env.UID != 88230 {
		t.Errorf("envelope identity = %s/%d/%d, want INBOX/12/88230", env.Folder, env.UIDValidity, env.UID)
	}
	if _, err := time.Parse(time.RFC3339, env.InternalDate); err != nil {
		t.Errorf("envelope internaldate %q is not RFC3339: %v", env.InternalDate, err)
	}
	if env.Size != len(msg) {
		t.Errorf("envelope size = %d, want %d", env.Size, len(msg))
	}
	if env.Truncated {
		t.Errorf("envelope truncated = true for a small message")
	}
	if got := decodeB64(t, env.RFC822B64); string(got) != string(msg) {
		t.Errorf("rfc822_b64 does not round-trip the bytes as received:\n got %q\nwant %q", got, msg)
	}
	if !json.Valid(w.raw) {
		t.Errorf("raw_json is not valid JSON: %s", w.raw)
	}

	if stats.RawInserted != 1 {
		t.Errorf("RawInserted = %d, want 1", stats.RawInserted)
	}
	if stats.IMAPFetched != 1 {
		t.Errorf("IMAPFetched = %d, want 1", stats.IMAPFetched)
	}
}

// Criterion 6: the flags observed on the server are recorded verbatim; the
// connector never adds \Seen (it has no verb that could).
func TestIngestIMAP_RecordsObservedFlagsVerbatim(t *testing.T) {
	ctx := context.Background()
	msg := rfc822([]string{`Message-ID: <unread@acme.example>`, `From: client@acme.example`}, "still unread")

	src := newFakeIMAP()
	src.folders = inboxOnly(3)
	src.add(imapINBOX, imapMsg(7, imapNow.Add(-time.Hour), []string{"\\Answered"}, msg))

	sink := newFakeIMAPSink()
	if _, err := google.IngestIMAP(ctx, src, sink, imapAccount(), imapCfg()); err != nil {
		t.Fatalf("IngestIMAP: %v", err)
	}

	env := sink.envelope(t, "imap:INBOX:3:7")
	if !reflect.DeepEqual(env.Flags, []string{"\\Answered"}) {
		t.Errorf("envelope flags = %v, want [\\Answered] verbatim", env.Flags)
	}
	for _, f := range env.Flags {
		if strings.EqualFold(f, "\\Seen") {
			t.Errorf("ingest recorded \\Seen on a message that was unread: %v", env.Flags)
		}
	}
}

// Criterion 6, structural: the MailSource interface has NO write verb at all —
// no way to set a flag, move, delete, label or append. The only write this
// connector performs against the outside world is the SMTP submission of
// criterion 12.
func TestMailSource_HasNoMailboxWriteVerbs(t *testing.T) {
	typ := reflect.TypeOf((*google.MailSource)(nil)).Elem()
	forbidden := []string{"store", "setflag", "addflag", "removeflag", "seen", "move", "copy", "delete", "expunge", "append", "label", "markread"}
	for i := 0; i < typ.NumMethod(); i++ {
		name := strings.ToLower(typ.Method(i).Name)
		for _, bad := range forbidden {
			if strings.Contains(name, bad) {
				t.Errorf("MailSource declares a mailbox write verb %q (criterion 6: this connector never mutates the mailbox)", typ.Method(i).Name)
			}
		}
	}
	for _, want := range []string{"Folders", "Search", "Fetch"} {
		if _, ok := typ.MethodByName(want); !ok {
			t.Errorf("MailSource is missing %s", want)
		}
	}
}

// Criterion 6, the verification protocol's grep check in test form: the go-imap
// client fetches with BODY.PEEK and never mentions an expunge/seen/store/move
// verb. A live-mailbox assertion is the manual smoke (SPEC step 4).
func TestIMAPClientSource_UsesBodyPeekAndNoWriteVerbs(t *testing.T) {
	src, err := os.ReadFile("imap.go")
	if err != nil {
		t.Fatalf("read imap.go (criterion 6 lives there): %v", err)
	}
	lower := strings.ToLower(string(src))
	if !strings.Contains(lower, "peek") {
		t.Errorf("imap.go never mentions PEEK; every fetch must use BODY.PEEK[...] (criterion 6)")
	}
	for _, bad := range []string{"expunge", `\\seen`, ".store(", ".move(", ".append("} {
		if strings.Contains(lower, bad) {
			t.Errorf("imap.go contains %q — the connector must have no write verb against the mailbox", bad)
		}
	}
}

// ---- criterion 7: bounded initial ingest + size cap ---------------------------

func TestIngestIMAP_FirstPassSearchesSinceTheBackfillWindow(t *testing.T) {
	ctx := context.Background()
	src := newFakeIMAP()
	src.folders = inboxOnly(12)
	src.add(imapINBOX, imapMsg(1, imapNow.Add(-24*time.Hour), nil,
		rfc822([]string{`Message-ID: <recent@acme.example>`, `From: c@acme.example`}, "recent")))

	sink := newFakeIMAPSink() // zero cursor => first pass
	cfg := imapCfg()
	cfg.Backfill = 2160 * time.Hour // 90d, the --backfill default

	if _, err := google.IngestIMAP(ctx, src, sink, imapAccount(), cfg); err != nil {
		t.Fatalf("IngestIMAP: %v", err)
	}

	got := searchFor(t, src, imapINBOX)
	wantSince := imapNow.Add(-2160 * time.Hour)
	if got.crit.Since.IsZero() {
		t.Fatalf("first pass SEARCH criteria = %+v, want SINCE %s (a 106,930-message mailbox must never be fetched whole)", got.crit, wantSince)
	}
	if d := got.crit.Since.Sub(wantSince); d > 24*time.Hour || d < -24*time.Hour {
		t.Errorf("first pass SEARCH SINCE = %s, want ~%s (now - backfill)", got.crit.Since, wantSince)
	}
	if got.crit.FromUID != 0 {
		t.Errorf("first pass sent FromUID = %d, want 0 (no cursor yet)", got.crit.FromUID)
	}
}

func TestIngestIMAP_OversizeMessageIsCapturedHeadersOnly(t *testing.T) {
	ctx := context.Background()
	headers := []string{
		`Message-ID: <big@acme.example>`,
		`From: client@acme.example`,
		`Subject: 4MB of scans`,
	}
	huge := rfc822(headers, strings.Repeat("x", 4<<20))

	src := newFakeIMAP()
	src.folders = inboxOnly(12)
	src.add(imapINBOX, imapMsg(500, imapNow.Add(-time.Hour), nil, huge))

	sink := newFakeIMAPSink()
	cfg := imapCfg()
	cfg.MaxMessageBytes = 1 << 20

	stats, err := google.IngestIMAP(ctx, src, sink, imapAccount(), cfg)
	if err != nil {
		t.Fatalf("IngestIMAP: %v", err)
	}

	if len(src.fetches) == 0 {
		t.Fatalf("no FETCH issued")
	}
	if src.fetches[0].maxBytes != cfg.MaxMessageBytes {
		t.Errorf("FETCH maxBytes = %d, want MAIL_MAX_MESSAGE_BYTES %d", src.fetches[0].maxBytes, cfg.MaxMessageBytes)
	}
	env := sink.envelope(t, "imap:INBOX:12:500")
	if !env.Truncated {
		t.Errorf(`envelope truncated = false for a %d-byte message above the %d cap; the loss must be marked`, len(huge), cfg.MaxMessageBytes)
	}
	captured := decodeB64(t, env.RFC822B64)
	if len(captured) >= len(huge) {
		t.Errorf("captured %d bytes for an oversize message; want a headers-only capture", len(captured))
	}
	if !strings.Contains(string(captured), "<big@acme.example>") {
		t.Errorf("headers-only capture lost the Message-ID: %q", captured)
	}
	if stats.IMAPTruncated != 1 {
		t.Errorf("IMAPTruncated = %d, want 1", stats.IMAPTruncated)
	}
}

// ---- criterion 8: per-folder cursors -----------------------------------------

func TestIngestIMAP_IncrementalPassUsesTheStoredUIDNext(t *testing.T) {
	ctx := context.Background()
	src := newFakeIMAP()
	src.folders = inboxOnly(12)
	for _, uid := range []uint32{41, 42, 43, 44} {
		src.add(imapINBOX, imapMsg(uid, imapNow.Add(-time.Hour),
			nil, rfc822([]string{fmt.Sprintf("Message-ID: <m%d@acme.example>", uid), `From: c@acme.example`}, "b")))
	}

	sink := newFakeIMAPSink()
	sink.cursor = google.Cursor{IMAPFolders: map[string]google.FolderCursor{
		imapINBOX: {UIDValidity: 12, UIDNext: 43},
	}}

	stats, err := google.IngestIMAP(ctx, src, sink, imapAccount(), imapCfg())
	if err != nil {
		t.Fatalf("IngestIMAP: %v", err)
	}

	got := searchFor(t, src, imapINBOX)
	if got.crit.FromUID != 43 {
		t.Errorf("incremental SEARCH FromUID = %d, want the stored uid_next 43 (UID FETCH 43:*)", got.crit.FromUID)
	}
	if !got.crit.Since.IsZero() {
		t.Errorf("incremental pass also sent SINCE %s; the UID range is the incremental bound", got.crit.Since)
	}
	if stats.RawInserted != 2 {
		t.Errorf("RawInserted = %d, want 2 (uids 43,44 only)", stats.RawInserted)
	}
	if sink.write("imap:INBOX:12:42") != nil {
		t.Errorf("uid 42 was re-fetched below the cursor")
	}
	cur := lastIMAPCursor(t, sink)
	if cur.IMAPFolders[imapINBOX].UIDNext != 45 {
		t.Errorf("saved uid_next = %d, want 45 (max uid + 1)", cur.IMAPFolders[imapINBOX].UIDNext)
	}
	if cur.IMAPFolders[imapINBOX].UIDValidity != 12 {
		t.Errorf("saved uidvalidity = %d, want 12", cur.IMAPFolders[imapINBOX].UIDValidity)
	}
}

func TestIngestIMAP_UIDValidityChangeForcesFolderResync(t *testing.T) {
	ctx := context.Background()
	src := newFakeIMAP()
	src.folders = inboxOnly(13) // the server rotated UIDVALIDITY 12 -> 13
	for _, uid := range []uint32{1, 2} {
		src.add(imapINBOX, imapMsg(uid, imapNow.Add(-48*time.Hour),
			nil, rfc822([]string{fmt.Sprintf("Message-ID: <v%d@acme.example>", uid), `From: c@acme.example`}, "b")))
	}

	sink := newFakeIMAPSink()
	sink.cursor = google.Cursor{IMAPFolders: map[string]google.FolderCursor{
		imapINBOX: {UIDValidity: 12, UIDNext: 900},
	}}

	if _, err := google.IngestIMAP(ctx, src, sink, imapAccount(), imapCfg()); err != nil {
		t.Fatalf("IngestIMAP: %v", err)
	}

	got := searchFor(t, src, imapINBOX)
	if got.crit.FromUID != 0 {
		t.Errorf("SEARCH FromUID = %d after a UIDVALIDITY change, want 0 (the stored uid_next is discarded)", got.crit.FromUID)
	}
	if got.crit.Since.IsZero() {
		t.Errorf("SEARCH after a UIDVALIDITY change must re-run the SINCE window; criteria=%+v", got.crit)
	}
	// New external ids carry the NEW uidvalidity, so old raw rows survive as
	// history and normalize-time Message-ID dedup collapses the duplicates.
	for _, id := range []string{"imap:INBOX:13:1", "imap:INBOX:13:2"} {
		if sink.write(id) == nil {
			t.Errorf("no raw row for %q after resync; writes=%+v", id, sink.writes)
		}
	}
	cur := lastIMAPCursor(t, sink)
	if cur.IMAPFolders[imapINBOX].UIDValidity != 13 {
		t.Errorf("saved uidvalidity = %d, want the new 13", cur.IMAPFolders[imapINBOX].UIDValidity)
	}
}

// The cursor is written ONLY after every message in the pass is durably in
// raw_source_items: a mid-pass failure leaves it untouched, marks the sync_runs
// row error, and returns non-nil.
func TestIngestIMAP_ErrorLeavesCursorUnadvanced(t *testing.T) {
	ctx := context.Background()
	src := newFakeIMAP()
	src.folders = inboxOnly(12)
	src.add(imapINBOX, imapMsg(10, imapNow.Add(-time.Hour), nil,
		rfc822([]string{`Message-ID: <ok@acme.example>`, `From: c@acme.example`}, "ok")))
	src.add(imapINBOX, imapMsg(11, imapNow.Add(-time.Hour), nil,
		rfc822([]string{`Message-ID: <boom@acme.example>`, `From: c@acme.example`}, "boom")))
	src.fetchErrUID = 11

	sink := newFakeIMAPSink()
	sink.cursor = google.Cursor{IMAPFolders: map[string]google.FolderCursor{
		imapINBOX: {UIDValidity: 12, UIDNext: 10},
	}}

	if _, err := google.IngestIMAP(ctx, src, sink, imapAccount(), imapCfg()); err == nil {
		t.Fatal("IngestIMAP returned nil error though a FETCH failed")
	}
	if len(sink.savedCursors) != 0 {
		t.Errorf("cursor advanced on a failed pass: %+v", sink.savedCursors)
	}
	if sink.finishStatus != "error" {
		t.Errorf("sync_runs status = %q, want error", sink.finishStatus)
	}
}

// The gmail + calendar cursor keys ride along untouched on every save
// (the Cursor struct round-trips — SPEC "Runtime shapes").
func TestIngestIMAP_CursorSavePreservesGmailAndCalendarKeys(t *testing.T) {
	ctx := context.Background()
	src := newFakeIMAP()
	src.folders = inboxOnly(12)
	src.add(imapINBOX, imapMsg(5, imapNow.Add(-time.Hour), nil,
		rfc822([]string{`Message-ID: <keys@acme.example>`, `From: c@acme.example`}, "b")))

	sink := newFakeIMAPSink()
	sink.cursor = google.Cursor{GmailInternalDateMS: 1751364000000, CalendarSyncToken: "SYNCTOK-9"}

	if _, err := google.IngestIMAP(ctx, src, sink, imapAccount(), imapCfg()); err != nil {
		t.Fatalf("IngestIMAP: %v", err)
	}
	cur := lastIMAPCursor(t, sink)
	if cur.GmailInternalDateMS != 1751364000000 {
		t.Errorf("gmail_internal_date_ms = %d after an imap pass, want it preserved", cur.GmailInternalDateMS)
	}
	if cur.CalendarSyncToken != "SYNCTOK-9" {
		t.Errorf("calendar_sync_token = %q after an imap pass, want it preserved", cur.CalendarSyncToken)
	}
	if _, ok := cur.IMAPFolders[imapINBOX]; !ok {
		t.Errorf("imap_folders missing INBOX: %+v", cur.IMAPFolders)
	}
}

// The sync_cursor JSON shape is pinned by the SPEC's "Runtime shapes".
func TestCursor_IMAPFoldersJSONShape(t *testing.T) {
	in := google.Cursor{
		GmailInternalDateMS: 42,
		CalendarSyncToken:   "tok",
		IMAPFolders: map[string]google.FolderCursor{
			"INBOX":             {UIDValidity: 12, UIDNext: 88231},
			"[Gmail]/Sent Mail": {UIDValidity: 9, UIDNext: 4410},
		},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal cursor: %v", err)
	}
	for _, want := range []string{`"imap_folders"`, `"uidvalidity"`, `"uid_next"`, `"gmail_internal_date_ms"`, `"calendar_sync_token"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("cursor JSON %s is missing %s", raw, want)
		}
	}
	var back google.Cursor
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal cursor: %v", err)
	}
	if !reflect.DeepEqual(in, back) {
		t.Errorf("cursor does not round-trip:\n got %+v\nwant %+v", back, in)
	}
}

// An immediate second pass fetches nothing new and leaves the cursor stable.
func TestIngestIMAP_SecondPassIsANoOp(t *testing.T) {
	ctx := context.Background()
	src := newFakeIMAP()
	src.folders = inboxOnly(12)
	src.add(imapINBOX, imapMsg(20, imapNow.Add(-time.Hour), nil,
		rfc822([]string{`Message-ID: <once@acme.example>`, `From: c@acme.example`}, "b")))

	sink := newFakeIMAPSink()
	if _, err := google.IngestIMAP(ctx, src, sink, imapAccount(), imapCfg()); err != nil {
		t.Fatalf("first IngestIMAP: %v", err)
	}
	first := lastIMAPCursor(t, sink)

	stats, err := google.IngestIMAP(ctx, src, sink, imapAccount(), imapCfg())
	if err != nil {
		t.Fatalf("second IngestIMAP: %v", err)
	}
	if stats.RawInserted != 0 {
		t.Errorf("second pass RawInserted = %d, want 0", stats.RawInserted)
	}
	if got := sink.cursor; !reflect.DeepEqual(got.IMAPFolders, first.IMAPFolders) {
		t.Errorf("cursor drifted on a no-op pass: %+v -> %+v", first.IMAPFolders, got.IMAPFolders)
	}
}

// ---- criterion 9: INBOX + Sent only -------------------------------------------

func TestIngestIMAP_FolderSelection(t *testing.T) {
	noise := []google.Folder{
		{Name: "[Gmail]/Spam", UIDValidity: 1},
		{Name: "[Gmail]/Trash", UIDValidity: 1},
		{Name: "[Gmail]/All Mail", UIDValidity: 1},
		{Name: "[Gmail]/Drafts", UIDValidity: 1},
		{Name: "Receipts", UIDValidity: 1},
	}

	run := func(t *testing.T, folders []google.Folder, cfgFolders []string) []string {
		t.Helper()
		src := newFakeIMAP()
		src.folders = folders
		for _, f := range folders {
			src.add(f.Name, imapMsg(1, imapNow.Add(-time.Hour), nil,
				rfc822([]string{fmt.Sprintf("Message-ID: <%s@acme.example>", strings.ReplaceAll(f.Name, " ", "-")), `From: c@acme.example`}, "b")))
		}
		sink := newFakeIMAPSink()
		cfg := imapCfg()
		cfg.Folders = cfgFolders
		if _, err := google.IngestIMAP(context.Background(), src, sink, imapAccount(), cfg); err != nil {
			t.Fatalf("IngestIMAP: %v", err)
		}
		seen := map[string]bool{}
		var out []string
		for _, f := range src.fetches {
			if !seen[f.folder] {
				seen[f.folder] = true
				out = append(out, f.folder)
			}
		}
		return out
	}

	assertFolders := func(t *testing.T, got []string, want ...string) {
		t.Helper()
		gotSet := map[string]bool{}
		for _, g := range got {
			gotSet[g] = true
		}
		for _, w := range want {
			if !gotSet[w] {
				t.Errorf("folder %q was never fetched (fetched: %v)", w, got)
			}
			delete(gotSet, w)
		}
		for extra := range gotSet {
			t.Errorf("folder %q was fetched; only INBOX + Sent are in scope (criterion 9)", extra)
		}
	}

	t.Run("\\Sent SPECIAL-USE discovery", func(t *testing.T) {
		folders := append([]google.Folder{
			{Name: imapINBOX, UIDValidity: 12},
			{Name: "Sent", Sent: true, UIDValidity: 9},
		}, noise...)
		assertFolders(t, run(t, folders, nil), imapINBOX, "Sent")
	})

	t.Run("MAIL_FOLDERS override", func(t *testing.T) {
		folders := append([]google.Folder{
			{Name: imapINBOX, UIDValidity: 12},
			{Name: "Sent Items", UIDValidity: 9},
		}, noise...)
		assertFolders(t, run(t, folders, []string{imapINBOX, "Sent Items"}), imapINBOX, "Sent Items")
	})

	t.Run("[Gmail]/Sent Mail fallback", func(t *testing.T) {
		folders := append([]google.Folder{
			{Name: imapINBOX, UIDValidity: 12},
			{Name: imapSent, UIDValidity: 9},
		}, noise...)
		assertFolders(t, run(t, folders, nil), imapINBOX, imapSent)
	})
}

// ---- criterion 19: one sync_runs row per account per pass ---------------------

func TestIngestIMAP_OneSyncRunPerAccountPass(t *testing.T) {
	ctx := context.Background()
	src := newFakeIMAP()
	src.folders = []google.Folder{
		{Name: imapINBOX, UIDValidity: 12},
		{Name: imapSent, Sent: true, UIDValidity: 9},
	}
	src.add(imapINBOX, imapMsg(1, imapNow.Add(-time.Hour), nil,
		rfc822([]string{`Message-ID: <in@acme.example>`, `From: c@acme.example`}, "in")))
	src.add(imapSent, imapMsg(1, imapNow.Add(-time.Hour), nil,
		rfc822([]string{`Message-ID: <out@acme.example>`, `From: ` + acctA}, "out")))

	sink := newFakeIMAPSink()
	if _, err := google.IngestIMAP(ctx, src, sink, imapAccount(), imapCfg()); err != nil {
		t.Fatalf("IngestIMAP: %v", err)
	}
	want := []string{"start:imap", "finish:ok"}
	if !reflect.DeepEqual(sink.runs, want) {
		t.Errorf("sync_runs sequence = %v, want %v (one row per account per pass, phase 'imap')", sink.runs, want)
	}
}

func lastIMAPCursor(t *testing.T, s *fakeIMAPSink) google.Cursor {
	t.Helper()
	if len(s.savedCursors) == 0 {
		t.Fatalf("cursor never saved")
	}
	return s.savedCursors[len(s.savedCursors)-1]
}
