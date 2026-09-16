package google_test

// Offline unit tests for the targeted IMAP re-fetch (SPEC mail-refetch-targeted
// / SWT-64, test plan U1-U6; acceptance criteria 5, 7, 8, 9, 10, 11, 12, 13,
// 17, 18). Everything runs against the in-memory fakeIMAP MailSource and the
// raw-retaining fakeIMAPSink (fake_imap_test.go) — ZERO network, ZERO Postgres,
// no IMAP server, no live mailbox.
//
// GREENFIELD NOTE: internal/connector/google/refetch.go does not exist yet, so
// this file compile-FAILs under `go test ./...` with "undefined: google.Refetch*"
// until it does — the expected failure mode. For greenfield code the SPEC's
// contract IS the signature; the surface below is the SPEC's "Files likely to
// touch" proposal, with the three additions the test plan forces (marked ADDED,
// and called out in the report for confirmation at implementation):
//
//	const RefetchMaxMessageBytes = 100 << 20 // 100 MiB (criterion 11)
//
//	type RefetchQuery struct {
//	    AccountEmail, From string
//	    Since, Until       time.Time
//	    RawIDs             []int64
//	    Limit              int
//	}
//
//	type RefetchTarget struct {
//	    RawID, AccountID          int64
//	    AccountEmail, ExternalID  string
//	    Folder                    string
//	    UIDValidity, UID          uint32
//	    StoredSize                int
//	    Truncated                 bool
//	    Sender, Subject           string
//	    SentAt                    time.Time
//	}
//
//	func SelectRefetchTargets(ctx context.Context, pool *pgxpool.Pool, q RefetchQuery) ([]RefetchTarget, error)
//	func RefetchMessages(ctx context.Context, src MailSource, sink Sink, acct Account,
//	    targets []RefetchTarget, cfg RefetchConfig) (RefetchStats, error)
//
//	// ADDED 1 — RefetchConfig, which the SPEC names but does not spell.
//	// DryRun lives HERE, not only in cmd/opsctl, so that criterion 5's "writes
//	// NOTHING" is exercised against the REAL PGSink by I6 rather than against a
//	// cmd-level branch no test can reach. Out follows the capture.GateDryRunConfig
//	// precedent (`Out: os.Stdout`, gate.go:67) so cmd/opsctl still owns where the
//	// plan is printed.
//	type RefetchConfig struct {
//	    MaxBytes int       // 0 => RefetchMaxMessageBytes
//	    DryRun   bool
//	    Out      io.Writer
//	}
//
//	// ADDED 2 — the stats/refusal shape. The three counter names come from the
//	// SPEC verbatim: uidvalidity_changed (criterion 7), folder_not_selectable
//	// (criterion 8), gone (criterion 9); imap_fetched / raw_updated /
//	// raw_unchanged are criterion 18's carried stats.
//	type RefetchStats struct {
//	    Planned, IMAPFetched                         int
//	    RawInserted, RawUpdated, RawUnchanged        int
//	    UIDValidityChanged, FolderNotSelectable      int
//	    EnvelopeMismatch, Gone                       int
//	    Refusals                                     []RefetchRefusal
//	}
//
//	// ADDED 3 — criterion 10: "every refusal is a per-target outcome printed with
//	// its raw id and reason", so the raw id must survive in the returned value.
//	type RefetchRefusal struct {
//	    RawID  int64
//	    Reason string // uidvalidity_changed | folder_not_selectable | gone | envelope...
//	}
//
// D6 (SPEC "Decisions made unilaterally"): a refusal is an OUTCOME, not an
// error — every test here asserts err == nil on a refused target.

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

var refetchNow = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

// refetchExternalID is the `imap:{folder}:{uidvalidity}:{uid}` spelling
// (rfc822.go:52, unexported). Spelled here ONLY to build fixtures and to read a
// write back; the production spelling stays the single source of truth.
func refetchExternalID(folder string, uidValidity, uid uint32) string {
	return fmt.Sprintf("imap:%s:%d:%d", folder, uidValidity, uid)
}

// refetchTarget is one already-ingested, over-cap row as SelectRefetchTargets
// would hand it back. Content is PLACEHOLDER throughout — no client data.
func refetchTarget(rawID int64, folder string, uidValidity, uid uint32) google.RefetchTarget {
	return google.RefetchTarget{
		RawID:        rawID,
		AccountID:    imapAccount().ID,
		AccountEmail: imapAccount().Email,
		ExternalID:   refetchExternalID(folder, uidValidity, uid),
		Folder:       folder,
		UIDValidity:  uidValidity,
		UID:          uid,
		StoredSize:   3_500_000,
		Truncated:    true,
		Sender:       "placeholder-sender@placeholder.example",
		Subject:      "PLACEHOLDER subject",
		SentAt:       refetchNow.Add(-8 * 24 * time.Hour),
	}
}

// refetchFullMessage is the message as it sits on the server: headers, a text
// part and the attachment bytes the 1 MiB fetch-time cap dropped.
func refetchFullMessage(messageID string) []byte {
	return rfc822([]string{
		`Message-ID: ` + messageID,
		`From: Placeholder Sender <placeholder-sender@placeholder.example>`,
		`To: placeholder-recipient@placeholder.example`,
		`Subject: PLACEHOLDER subject`,
		`Content-Type: multipart/mixed; boundary="ph-boundary"`,
	}, strings.Join([]string{
		`--ph-boundary`,
		`Content-Type: text/plain; charset=utf-8`,
		``,
		`PLACEHOLDER body text.`,
		`--ph-boundary`,
		`Content-Type: application/octet-stream; name="PLACEHOLDER-attachment.bin"`,
		`Content-Transfer-Encoding: base64`,
		``,
		`UExBQ0VIT0xERVItQVRUQUNITUVOVC1CWVRFUw==`,
		`--ph-boundary--`,
		``,
	}, "\r\n"))
}

// storedHash seeds the sink so the target's row already EXISTS: the refetch is
// an upsert over it (criterion 12), never an insert of a second row.
const storedHash = "stored-hash-of-the-truncated-capture"

// refusalFor returns the recorded refusal for a raw id, or nil.
func refusalFor(stats google.RefetchStats, rawID int64) *google.RefetchRefusal {
	for i := range stats.Refusals {
		if stats.Refusals[i].RawID == rawID {
			return &stats.Refusals[i]
		}
	}
	return nil
}

// ---- U1: the dangerous failure — a stale UIDVALIDITY (criterion 7, M1) ------

// TestRefetchMessages_StaleUIDValidityIsRefusedAndNeverFetched pins the central
// criterion. The folder's generation has rolled (12 -> 99), so UID 7 now names a
// DIFFERENT message. Fetching it and upserting it over raw row 73094 would
// destroy that row's stored bytes with no way back — raw_json is overwritten in
// place and raw_source_items keeps no version history (SPEC "Rollback"). So the
// assertion is not "it noticed": it is that no FETCH was issued at all.
//
// Mutation M1 (drop the stored.UIDValidity == live.UIDValidity comparison) must
// turn this red.
func TestRefetchMessages_StaleUIDValidityIsRefusedAndNeverFetched(t *testing.T) {
	ctx := context.Background()

	src := newFakeIMAP()
	src.folders = []google.Folder{{Name: imapINBOX, UIDValidity: 99}} // live generation
	// The message that now sits at UID 7. It is NOT the stored one, and it must
	// never be transferred, hashed or written.
	src.add(imapINBOX, imapMsg(7, refetchNow.Add(-time.Hour), nil, rfc822([]string{
		`Message-ID: <wrong-message@placeholder.example>`,
		`From: someone-else@placeholder.example`,
		`Subject: PLACEHOLDER a different message entirely`,
	}, "PLACEHOLDER body of the message that now occupies UID 7")))

	sink := newFakeIMAPSink()
	target := refetchTarget(73094, imapINBOX, 12, 7) // stored generation 12
	sink.stored[target.ExternalID] = storedHash

	stats, err := google.RefetchMessages(ctx, src, sink, imapAccount(),
		[]google.RefetchTarget{target}, google.RefetchConfig{})
	if err != nil {
		t.Fatalf("RefetchMessages: %v (D6: a refusal is an outcome, not an error)", err)
	}

	if len(src.fetches) != 0 {
		t.Errorf("FETCH issued for a stale UIDVALIDITY: %+v\n"+
			"criterion 7: the pass must not fetch whatever now sits at that UID, must not resync, must not search", src.fetches)
	}
	if len(sink.writes) != 0 {
		t.Errorf("wrote %d raw row(s) for a stale UIDVALIDITY: %+v\n"+
			"this is the unrecoverable write: raw_json is replaced wholesale and there is no version history", len(sink.writes), sink.writes)
	}
	// Named explicitly: the row that must not be overwritten, and the row that
	// must not be created under the LIVE generation either.
	for _, id := range []string{target.ExternalID, refetchExternalID(imapINBOX, 99, 7)} {
		if w := sink.write(id); w != nil {
			t.Errorf("raw row %q written (update=%v) — a stale UID must produce no write of any shape", id, w.update)
		}
	}
	if sink.stored[target.ExternalID] != storedHash {
		t.Errorf("stored content_hash for %q changed to %q; the truncated capture must survive untouched",
			target.ExternalID, sink.stored[target.ExternalID])
	}
	if stats.UIDValidityChanged != 1 {
		t.Errorf("stats.UIDValidityChanged = %d, want 1 (criterion 7 counts the refusal)", stats.UIDValidityChanged)
	}
	if stats.IMAPFetched != 0 || stats.RawUpdated != 0 || stats.RawInserted != 0 {
		t.Errorf("stats = %+v, want zero fetched/updated/inserted", stats)
	}
	r := refusalFor(stats, target.RawID)
	if r == nil {
		t.Fatalf("no refusal recorded for raw id %d; criterion 10: every refusal is a per-target outcome carrying its raw id and reason (refusals=%+v)",
			target.RawID, stats.Refusals)
	}
	if !strings.Contains(strings.ToLower(r.Reason), "uidvalidity") {
		t.Errorf("refusal reason for raw %d = %q, want it to name uidvalidity (the counter is uidvalidity_changed)", target.RawID, r.Reason)
	}
}

// ---- U2: happy path, and the whole point of the ticket (criteria 11, 12, 18) -

func TestRefetchMessages_FetchesAtTheRaisedCapAndUpsertsInPlace(t *testing.T) {
	ctx := context.Background()

	const msgID = "<placeholder-refetch-1@placeholder.example>"
	full := refetchFullMessage(msgID)

	src := newFakeIMAP()
	src.folders = []google.Folder{{Name: imapINBOX, UIDValidity: 12}}
	src.add(imapINBOX, imapMsg(7, refetchNow.Add(-8*24*time.Hour), []string{"\\Seen"}, full))

	sink := newFakeIMAPSink()
	target := refetchTarget(73094, imapINBOX, 12, 7)
	sink.stored[target.ExternalID] = storedHash // the truncated capture already on disk

	stats, err := google.RefetchMessages(ctx, src, sink, imapAccount(),
		[]google.RefetchTarget{target}, google.RefetchConfig{})
	if err != nil {
		t.Fatalf("RefetchMessages: %v", err)
	}

	// Mutation M7 (pass google.MaxMessageBytes() instead) must turn this red:
	// at the 1 MiB connector cap the attachment bytes are never put on the wire,
	// which is the defect this ticket exists to repair.
	if len(src.fetches) != 1 {
		t.Fatalf("Fetch calls = %d, want exactly 1: %+v", len(src.fetches), src.fetches)
	}
	if got := src.fetches[0].maxBytes; got != google.RefetchMaxMessageBytes {
		t.Errorf("Fetch maxBytes = %d, want RefetchMaxMessageBytes = %d (100 MiB, criterion 11)",
			got, google.RefetchMaxMessageBytes)
	}
	if google.RefetchMaxMessageBytes != 100<<20 {
		t.Errorf("RefetchMaxMessageBytes = %d, want %d (100 MiB — the owner's instruction)",
			google.RefetchMaxMessageBytes, 100<<20)
	}
	if google.RefetchMaxMessageBytes == google.DefaultMaxMessageBytes {
		t.Errorf("RefetchMaxMessageBytes == DefaultMaxMessageBytes; the two cap numbers are different by design (SPEC 'The two cap numbers')")
	}
	if src.fetches[0].folder != imapINBOX || len(src.fetches[0].uids) != 1 || src.fetches[0].uids[0] != 7 {
		t.Errorf("Fetch = folder %q uids %v, want INBOX [7] — only the named UID is fetched (criterion 2, bounded)",
			src.fetches[0].folder, src.fetches[0].uids)
	}

	// Criterion 12: the SAME external id, upserted over the existing row.
	w := sink.write(target.ExternalID)
	if w == nil {
		t.Fatalf("no raw row written for %q; writes=%+v", target.ExternalID, sink.writes)
	}
	if len(sink.writes) != 1 {
		t.Errorf("writes = %d, want 1: a refetch upserts the target row and creates nothing else (%+v)", len(sink.writes), sink.writes)
	}
	if !w.update {
		t.Errorf("raw row %q written via InsertRaw; the row already exists, so the hash-compare must route it to UpdateRaw", target.ExternalID)
	}
	if w.hash == storedHash {
		t.Errorf("content_hash unchanged (%q) after refetching the full message; the bytes differ, so the hash must too", w.hash)
	}
	if stats.RawUpdated != 1 {
		t.Errorf("stats.RawUpdated = %d, want 1 (%+v)", stats.RawUpdated, stats)
	}
	if stats.IMAPFetched != 1 {
		t.Errorf("stats.IMAPFetched = %d, want 1", stats.IMAPFetched)
	}
	if stats.UIDValidityChanged != 0 || stats.Gone != 0 || len(stats.Refusals) != 0 {
		t.Errorf("stats = %+v, want no refusals on the happy path", stats)
	}

	env := sink.envelope(t, target.ExternalID)
	if env.Source != "imap" {
		t.Errorf(`envelope source = %q, want "imap" (the normalize dispatch discriminator)`, env.Source)
	}
	if env.Folder != imapINBOX || env.UIDValidity != 12 || env.UID != 7 {
		t.Errorf("envelope identity = %s/%d/%d, want INBOX/12/7", env.Folder, env.UIDValidity, env.UID)
	}
	if env.Truncated {
		t.Errorf("envelope truncated = true after a 100 MiB fetch; the whole message fits, so the recovered row must not be marked truncated")
	}
	if env.RFC822B64 == "" {
		t.Fatalf("envelope rfc822_b64 is empty; the recovered bytes are the deliverable")
	}
	if got := decodeB64(t, env.RFC822B64); !bytes.Equal(got, full) {
		t.Errorf("rfc822_b64 does not round-trip the message as received (%d bytes stored, %d fetched)", len(got), len(full))
	} else if !bytes.Contains(got, []byte("PLACEHOLDER-attachment.bin")) {
		t.Errorf("the stored bytes do not carry the attachment part; recovering it is the point of the pass")
	}

	// Criterion 18: its OWN sync_runs phase, so availability's calendar readiness
	// and the funnel's per-phase grouping are unaffected.
	if len(sink.runs) != 2 || sink.runs[0] != "start:imap_refetch" || sink.runs[1] != "finish:ok" {
		t.Errorf("sync_runs calls = %v, want [start:imap_refetch finish:ok] (criterion 18)", sink.runs)
	}
}

func TestRefetchMessages_MaxBytesOverrideIsHonoured(t *testing.T) {
	ctx := context.Background()
	src := newFakeIMAP()
	src.folders = []google.Folder{{Name: imapINBOX, UIDValidity: 12}}
	src.add(imapINBOX, imapMsg(7, refetchNow, nil, refetchFullMessage("<placeholder-refetch-2@placeholder.example>")))

	sink := newFakeIMAPSink()
	target := refetchTarget(73094, imapINBOX, 12, 7)
	sink.stored[target.ExternalID] = storedHash

	const override = 5 << 20
	if _, err := google.RefetchMessages(ctx, src, sink, imapAccount(),
		[]google.RefetchTarget{target}, google.RefetchConfig{MaxBytes: override}); err != nil {
		t.Fatalf("RefetchMessages: %v", err)
	}
	if len(src.fetches) != 1 || src.fetches[0].maxBytes != override {
		t.Errorf("Fetch maxBytes = %+v, want %d (--max-bytes override, criterion 11)", src.fetches, override)
	}
}

// ---- U3: the cursor never moves (criterion 13, M5) ---------------------------

// TestRefetchMessages_NeverMovesTheCursor. The pass reads UIDs from
// raw_source_items, so it has no business writing a folder position — and
// writing one would rewind or advance the always-on connector's incremental
// window over a 106,930-message mailbox.
func TestRefetchMessages_NeverMovesTheCursor(t *testing.T) {
	ctx := context.Background()
	src := newFakeIMAP()
	src.folders = []google.Folder{{Name: imapINBOX, UIDValidity: 12}}
	src.add(imapINBOX, imapMsg(7, refetchNow, nil, refetchFullMessage("<placeholder-refetch-3@placeholder.example>")))

	sink := newFakeIMAPSink()
	// A cursor that is already well past this UID: moving it at all is a defect,
	// in either direction.
	sink.cursor = google.Cursor{IMAPFolders: map[string]google.FolderCursor{
		imapINBOX: {UIDValidity: 12, UIDNext: 88231},
	}}
	before := sink.cursor
	target := refetchTarget(73094, imapINBOX, 12, 7)
	sink.stored[target.ExternalID] = storedHash

	if _, err := google.RefetchMessages(ctx, src, sink, imapAccount(),
		[]google.RefetchTarget{target}, google.RefetchConfig{}); err != nil {
		t.Fatalf("RefetchMessages: %v", err)
	}

	if len(sink.savedCursors) != 0 {
		t.Errorf("SaveCursor/SaveCursorField called %d time(s) during a refetch: %+v\n"+
			"criterion 13: the pass never calls either and never writes a FolderCursor", len(sink.savedCursors), sink.savedCursors)
	}
	if got := sink.cursor.IMAPFolders[imapINBOX]; got != before.IMAPFolders[imapINBOX] {
		t.Errorf("cursor for INBOX = %+v, want %+v unchanged", got, before.IMAPFolders[imapINBOX])
	}
}

// ---- U4: fetched messages are matched by UID, never by position (M6) --------

// shuffledIMAP returns Fetch results in REVERSE order. A server may answer in
// any order, and pairing result[i] with target[i] would file one message's bytes
// under another message's row — the same unrecoverable overwrite as U1, reached
// by a different mistake.
type shuffledIMAP struct{ *fakeIMAP }

func (s shuffledIMAP) Fetch(ctx context.Context, folder string, uids []uint32, maxBytes int) ([]google.FetchedMessage, error) {
	out, err := s.fakeIMAP.Fetch(ctx, folder, uids, maxBytes)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func TestRefetchMessages_MatchesFetchedMessagesByUIDNotPosition(t *testing.T) {
	ctx := context.Background()

	inner := newFakeIMAP()
	inner.folders = []google.Folder{{Name: imapINBOX, UIDValidity: 12}}
	inner.add(imapINBOX, imapMsg(7, refetchNow.Add(-72*time.Hour), nil,
		refetchFullMessage("<placeholder-uid-7@placeholder.example>")))
	inner.add(imapINBOX, imapMsg(9, refetchNow.Add(-48*time.Hour), nil,
		refetchFullMessage("<placeholder-uid-9@placeholder.example>")))
	// UID 11 is NOT in the folder: the message was expunged (criterion 9).
	src := shuffledIMAP{inner}

	sink := newFakeIMAPSink()
	var targets []google.RefetchTarget
	for i, uid := range []uint32{7, 9, 11} {
		tg := refetchTarget(int64(73090+i), imapINBOX, 12, uid)
		sink.stored[tg.ExternalID] = storedHash
		targets = append(targets, tg)
	}

	stats, err := google.RefetchMessages(ctx, src, sink, imapAccount(), targets, google.RefetchConfig{})
	if err != nil {
		t.Fatalf("RefetchMessages: %v", err)
	}

	// Each written envelope must carry the UID of the row it was written under.
	for _, uid := range []uint32{7, 9} {
		id := refetchExternalID(imapINBOX, 12, uid)
		if sink.write(id) == nil {
			t.Fatalf("no raw row written for %q; writes=%+v", id, sink.writes)
		}
		env := sink.envelope(t, id)
		if env.UID != uid {
			t.Errorf("raw row %q carries envelope uid %d; a fetched message was paired with the wrong target "+
				"(match by FetchedMessage.UID, never by position in the result slice — criterion 9)", id, env.UID)
		}
		want := "<placeholder-uid-" + fmt.Sprint(uid) + "@placeholder.example>"
		if got := decodeB64(t, env.RFC822B64); !bytes.Contains(got, []byte(want)) {
			t.Errorf("raw row %q does not carry Message-ID %s; its bytes belong to another message", id, want)
		}
	}

	// The expunged one: counted gone, and nothing written.
	goneID := refetchExternalID(imapINBOX, 12, 11)
	if w := sink.write(goneID); w != nil {
		t.Errorf("raw row %q written for a UID the server returned no message for", goneID)
	}
	if stats.Gone != 1 {
		t.Errorf("stats.Gone = %d, want 1 (criterion 9: the UID was expunged)", stats.Gone)
	}
	if stats.RawUpdated != 2 {
		t.Errorf("stats.RawUpdated = %d, want 2", stats.RawUpdated)
	}
	if r := refusalFor(stats, targets[2].RawID); r == nil {
		t.Errorf("no per-target outcome recorded for the expunged raw id %d (refusals=%+v)", targets[2].RawID, stats.Refusals)
	} else if !strings.Contains(strings.ToLower(r.Reason), "gone") {
		t.Errorf("reason for the expunged raw id = %q, want it to name gone", r.Reason)
	}
}

// ---- U5: a folder outside the selectable set is refused (criterion 8) -------

func TestRefetchMessages_RefusesFolderOutsideTheSelectableSet(t *testing.T) {
	ctx := context.Background()

	src := newFakeIMAP()
	src.folders = []google.Folder{
		{Name: imapINBOX, UIDValidity: 12},
		{Name: imapSent, UIDValidity: 34, Sent: true},
	}
	// The same UID exists in All Mail. It must not be fetched from there, and it
	// must not be fetched from some other folder instead.
	src.add("[Gmail]/All Mail", imapMsg(7, refetchNow, nil,
		refetchFullMessage("<placeholder-all-mail@placeholder.example>")))
	src.add(imapINBOX, imapMsg(7, refetchNow, nil,
		refetchFullMessage("<placeholder-inbox@placeholder.example>")))

	sink := newFakeIMAPSink()
	target := refetchTarget(73094, "[Gmail]/All Mail", 12, 7)
	sink.stored[target.ExternalID] = storedHash

	stats, err := google.RefetchMessages(ctx, src, sink, imapAccount(),
		[]google.RefetchTarget{target}, google.RefetchConfig{})
	if err != nil {
		t.Fatalf("RefetchMessages: %v (D6: a refusal is an outcome, not an error)", err)
	}
	if len(src.fetches) != 0 {
		t.Errorf("FETCH issued for a non-selectable folder: %+v (criterion 8)", src.fetches)
	}
	if len(sink.writes) != 0 {
		t.Errorf("wrote %d raw row(s) for a non-selectable folder: %+v", len(sink.writes), sink.writes)
	}
	if stats.FolderNotSelectable != 1 {
		t.Errorf("stats.FolderNotSelectable = %d, want 1", stats.FolderNotSelectable)
	}
	if r := refusalFor(stats, target.RawID); r == nil {
		t.Errorf("no refusal recorded for raw id %d (refusals=%+v)", target.RawID, stats.Refusals)
	} else if !strings.Contains(strings.ToLower(r.Reason), "folder") {
		t.Errorf("refusal reason = %q, want it to name the folder (folder_not_selectable)", r.Reason)
	}
}

// ---- U6: the envelope must agree with external_id (criterion 6) -------------

// A row whose raw_json coordinates disagree with its external_id was not written
// by this connector. It is refused rather than reconciled: picking either side
// is a guess about which message the row holds, and a wrong guess fetches and
// overwrites the wrong message.
func TestRefetchMessages_RefusesEnvelopeDisagreeingWithExternalID(t *testing.T) {
	ctx := context.Background()

	src := newFakeIMAP()
	src.folders = []google.Folder{{Name: imapINBOX, UIDValidity: 12}}
	src.add(imapINBOX, imapMsg(7, refetchNow, nil, refetchFullMessage("<placeholder-uid-7@placeholder.example>")))
	src.add(imapINBOX, imapMsg(8, refetchNow, nil, refetchFullMessage("<placeholder-uid-8@placeholder.example>")))

	sink := newFakeIMAPSink()
	target := refetchTarget(73094, imapINBOX, 12, 8)
	target.ExternalID = refetchExternalID(imapINBOX, 12, 7) // says 7; the envelope says 8
	sink.stored[target.ExternalID] = storedHash

	stats, err := google.RefetchMessages(ctx, src, sink, imapAccount(),
		[]google.RefetchTarget{target}, google.RefetchConfig{})
	if err != nil {
		t.Fatalf("RefetchMessages: %v (D6: a refusal is an outcome, not an error)", err)
	}
	if len(src.fetches) != 0 {
		t.Errorf("FETCH issued for a row whose envelope disagrees with its external_id: %+v", src.fetches)
	}
	if len(sink.writes) != 0 {
		t.Errorf("wrote %d raw row(s) for a disagreeing row: %+v", len(sink.writes), sink.writes)
	}
	if stats.EnvelopeMismatch != 1 {
		t.Errorf("stats.EnvelopeMismatch = %d, want 1 (criterion 6)", stats.EnvelopeMismatch)
	}
	if r := refusalFor(stats, target.RawID); r == nil {
		t.Errorf("no refusal recorded for raw id %d (refusals=%+v)", target.RawID, stats.Refusals)
	} else if !strings.Contains(strings.ToLower(r.Reason), "envelope") {
		t.Errorf("refusal reason = %q, want it to name the envelope disagreement", r.Reason)
	}
}

// ---- dry run, offline half (criteria 5, 17, 18; the sibling of I6) ----------

// The DB half is I6. This one pins the three things a fake sink can see that
// Postgres cannot show as cheaply: no FETCH, no sync_runs, no cursor write.
func TestRefetchMessages_DryRunContactsIMAPButFetchesAndWritesNothing(t *testing.T) {
	ctx := context.Background()

	src := newFakeIMAP()
	src.folders = []google.Folder{{Name: imapINBOX, UIDValidity: 99}} // live generation differs
	src.add(imapINBOX, imapMsg(7, refetchNow, nil, refetchFullMessage("<placeholder-dry@placeholder.example>")))

	sink := newFakeIMAPSink()
	target := refetchTarget(73094, imapINBOX, 12, 7)
	sink.stored[target.ExternalID] = storedHash

	var out bytes.Buffer
	if _, err := google.RefetchMessages(ctx, src, sink, imapAccount(),
		[]google.RefetchTarget{target}, google.RefetchConfig{DryRun: true, Out: &out}); err != nil {
		t.Fatalf("RefetchMessages(dry run): %v", err)
	}

	if len(src.fetches) != 0 {
		t.Errorf("dry run issued a FETCH: %+v (D5: it contacts IMAP for the live UIDVALIDITY only)", src.fetches)
	}
	if len(sink.writes) != 0 {
		t.Errorf("dry run wrote %d raw row(s): %+v (criterion 5: it writes NOTHING)", len(sink.writes), sink.writes)
	}
	if len(sink.runs) != 0 {
		t.Errorf("dry run touched sync_runs: %v (criterion 5: no sync_runs row)", sink.runs)
	}
	if len(sink.savedCursors) != 0 {
		t.Errorf("dry run wrote the cursor: %+v", sink.savedCursors)
	}

	// Criterion 5: one plan line per target, carrying the facts worth knowing
	// BEFORE a live run — above all that this target would be refused.
	line := out.String()
	for _, want := range []string{
		fmt.Sprint(target.RawID), target.AccountEmail, target.Folder,
		target.Sender, target.Subject, "truncated",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("dry-run plan line does not mention %q; got:\n%s", want, line)
		}
	}
	if !strings.Contains(line, "12") || !strings.Contains(line, "99") {
		t.Errorf("dry-run plan line must show the stored uidvalidity (12) AND the live one (99); got:\n%s", line)
	}
}

// ---- the guards review added (2026-09-16) ------------------------------------

// A target belonging to ANOTHER account must be refused before anything else.
//
// The write keys on the PASS's account, not the row's. ListAppPasswordAccounts
// matches lower(account_email) while source_accounts is unique only on
// (provider, account_email), so case-distinct rows are legal and the CLI can
// resolve the wrong one. Without this guard RawHash finds no stored hash for
// (wrong account, external id), the insert branch runs, and the recovered bytes
// land on a NEW row under the wrong account while the real row stays truncated —
// with every counter reading as success.
func TestRefetchMessages_RefusesATargetBelongingToAnotherAccount(t *testing.T) {
	ctx := context.Background()

	src := newFakeIMAP()
	src.folders = []google.Folder{{Name: imapINBOX, UIDValidity: 12}}
	src.add(imapINBOX, imapMsg(7, refetchNow, nil, refetchFullMessage("<placeholder-other-acct@placeholder.example>")))

	sink := newFakeIMAPSink()
	target := refetchTarget(73094, imapINBOX, 12, 7)
	target.AccountID = imapAccount().ID + 1 // a different mailbox's row
	sink.stored[target.ExternalID] = storedHash

	stats, err := google.RefetchMessages(ctx, src, sink, imapAccount(),
		[]google.RefetchTarget{target}, google.RefetchConfig{})
	if err != nil {
		t.Fatalf("RefetchMessages: %v (a refusal is an outcome, not an error)", err)
	}
	if len(src.fetches) != 0 {
		t.Errorf("FETCH issued for another account's target: %+v", src.fetches)
	}
	if len(sink.writes) != 0 {
		t.Errorf("wrote %d raw row(s) for another account's target: %+v", len(sink.writes), sink.writes)
	}
	if stats.WrongAccount != 1 {
		t.Errorf("stats.WrongAccount = %d, want 1", stats.WrongAccount)
	}
	if r := refusalFor(stats, target.RawID); r == nil {
		t.Errorf("no refusal recorded for raw id %d (refusals=%+v)", target.RawID, stats.Refusals)
	} else if !strings.Contains(strings.ToLower(r.Reason), "account") {
		t.Errorf("refusal reason = %q, want it to name the account", r.Reason)
	}
}

// A row that no longer exists is refused, never inserted.
//
// For a refetch the row must already exist — the target came from it. Reaching
// the insert branch means the row moved or vanished between selection and write,
// so the pass no longer knows what it is repairing; inserting would create a
// second row rather than fix the first.
func TestRefetchMessages_RefusesWhenTheRowVanishedBeforeTheWrite(t *testing.T) {
	ctx := context.Background()

	src := newFakeIMAP()
	src.folders = []google.Folder{{Name: imapINBOX, UIDValidity: 12}}
	src.add(imapINBOX, imapMsg(7, refetchNow, nil, refetchFullMessage("<placeholder-vanished@placeholder.example>")))

	sink := newFakeIMAPSink() // deliberately NOT seeded: the row is gone
	target := refetchTarget(73094, imapINBOX, 12, 7)

	// A SMALL cap on purpose, so the fetch comes back truncated. Without it
	// m.Truncated is false and the StillTruncated assertion below is vacuous —
	// it would read 0 whether or not the counter is incremented in the wrong
	// place, which is exactly how the first version of this pin passed while the
	// bug was present.
	stats, err := google.RefetchMessages(ctx, src, sink, imapAccount(),
		[]google.RefetchTarget{target}, google.RefetchConfig{MaxBytes: 64})
	if err != nil {
		t.Fatalf("RefetchMessages: %v", err)
	}
	if len(sink.writes) != 0 {
		t.Errorf("wrote %d raw row(s) for a vanished row: %+v — a refetch repairs a row, it does not create one",
			len(sink.writes), sink.writes)
	}
	if stats.RawInserted != 0 {
		t.Errorf("stats.RawInserted = %d, want 0", stats.RawInserted)
	}
	if stats.RowVanished != 1 {
		t.Errorf("stats.RowVanished = %d, want 1", stats.RowVanished)
	}
	// Review round 4 reproduced this exact lie: the increment sat past the
	// downgrade/shrink guards but BEFORE the vanished-row check, so a refused
	// row still counted as "still truncated" — in the counter, in the CLI's
	// "re-run with a larger --max-bytes" advice, and in the durable sync_runs
	// row. It is now incremented only after UpdateRaw succeeds.
	if stats.StillTruncated != 0 {
		t.Errorf("stats.StillTruncated = %d, want 0: nothing was written, so no truncated capture was stored. "+
			"The counter must be incremented after the write succeeds, not before the guards", stats.StillTruncated)
	}
	if r := refusalFor(stats, target.RawID); r == nil {
		t.Errorf("no refusal recorded for raw id %d (refusals=%+v)", target.RawID, stats.Refusals)
	}
}

// A repair that recovered nothing must SAY so.
//
// If the message is over this pass's cap too, the row is overwritten with a
// fresh truncated capture and every other counter reads as success
// (raw_updated=1). StillTruncated is the only number that distinguishes
// "recovered the attachments" from "did the same thing again".
func TestRefetchMessages_CountsARepairThatCameBackStillTruncated(t *testing.T) {
	ctx := context.Background()

	src := newFakeIMAP()
	src.folders = []google.Folder{{Name: imapINBOX, UIDValidity: 12}}
	src.add(imapINBOX, imapMsg(7, refetchNow, nil, refetchFullMessage("<placeholder-still-trunc@placeholder.example>")))

	sink := newFakeIMAPSink()
	target := refetchTarget(73094, imapINBOX, 12, 7)
	sink.stored[target.ExternalID] = storedHash

	// A cap far below the message: the fetch truncates again.
	stats, err := google.RefetchMessages(ctx, src, sink, imapAccount(),
		[]google.RefetchTarget{target}, google.RefetchConfig{MaxBytes: 64})
	if err != nil {
		t.Fatalf("RefetchMessages: %v", err)
	}
	if stats.StillTruncated != 1 {
		t.Errorf("stats.StillTruncated = %d, want 1: the repair recovered nothing and must not read as success (%+v)",
			stats.StillTruncated, stats)
	}
	if stats.IMAPFetched != 1 {
		t.Errorf("stats.IMAPFetched = %d, want 1", stats.IMAPFetched)
	}
	env := sink.envelope(t, target.ExternalID)
	if !env.Truncated {
		t.Errorf("envelope truncated = false, but the fetch was capped below the message size")
	}
}

// A refetch must never DOWNGRADE a complete stored capture to a truncated one.
//
// The second instance of this ticket's core hazard, found in review: the
// selection does not filter on `truncated` (a sender window legitimately spans
// complete and truncated rows), so a --max-bytes below a stored COMPLETE
// message makes the fetch truncate, the hash differ, and UpdateRaw replace a
// full capture with headers-plus-one-text-part. raw_json is overwritten in
// place with no version history, so those bytes are gone.
func TestRefetchMessages_RefusesToOverwriteACompleteRowWithATruncatedFetch(t *testing.T) {
	ctx := context.Background()

	src := newFakeIMAP()
	src.folders = []google.Folder{{Name: imapINBOX, UIDValidity: 12}}
	src.add(imapINBOX, imapMsg(7, refetchNow, nil, refetchFullMessage("<placeholder-downgrade@placeholder.example>")))

	sink := newFakeIMAPSink()
	target := refetchTarget(73094, imapINBOX, 12, 7)
	target.Truncated = false // the stored row is COMPLETE
	sink.stored[target.ExternalID] = storedHash

	// A cap far below the message: this fetch comes back truncated.
	stats, err := google.RefetchMessages(ctx, src, sink, imapAccount(),
		[]google.RefetchTarget{target}, google.RefetchConfig{MaxBytes: 64})
	if err != nil {
		t.Fatalf("RefetchMessages: %v (a refusal is an outcome, not an error)", err)
	}
	if len(sink.writes) != 0 {
		t.Errorf("wrote %d raw row(s), replacing a COMPLETE capture with a truncated one: %+v\n"+
			"raw_json is overwritten in place and there is no version history — those bytes are unrecoverable",
			len(sink.writes), sink.writes)
	}
	if sink.stored[target.ExternalID] != storedHash {
		t.Errorf("stored content_hash changed to %q; the complete capture must survive untouched", sink.stored[target.ExternalID])
	}
	if stats.WouldDowngrade != 1 {
		t.Errorf("stats.WouldDowngrade = %d, want 1 (%+v)", stats.WouldDowngrade, stats)
	}
	if stats.RawUpdated != 0 {
		t.Errorf("stats.RawUpdated = %d, want 0", stats.RawUpdated)
	}
	if r := refusalFor(stats, target.RawID); r == nil {
		t.Errorf("no refusal recorded for raw id %d (refusals=%+v)", target.RawID, stats.Refusals)
	} else if !strings.Contains(strings.ToLower(r.Reason), "complete") {
		t.Errorf("refusal reason = %q, want it to say the stored row is complete", r.Reason)
	}
}

// Control: when the stored row was ALREADY truncated, a truncated refetch still
// writes. That is the honest "did the same thing again" case, and StillTruncated
// is what says so — refusing it would block the legitimate retry at a higher cap.
func TestRefetchMessages_StillWritesWhenTheStoredRowWasAlreadyTruncated(t *testing.T) {
	ctx := context.Background()

	src := newFakeIMAP()
	src.folders = []google.Folder{{Name: imapINBOX, UIDValidity: 12}}
	src.add(imapINBOX, imapMsg(7, refetchNow, nil, refetchFullMessage("<placeholder-retrunc@placeholder.example>")))

	sink := newFakeIMAPSink()
	target := refetchTarget(73094, imapINBOX, 12, 7) // refetchTarget sets Truncated: true
	if !target.Truncated {
		t.Fatalf("fixture drift: this control needs a stored-truncated target")
	}
	sink.stored[target.ExternalID] = storedHash

	stats, err := google.RefetchMessages(ctx, src, sink, imapAccount(),
		[]google.RefetchTarget{target}, google.RefetchConfig{MaxBytes: 64})
	if err != nil {
		t.Fatalf("RefetchMessages: %v", err)
	}
	if stats.WouldDowngrade != 0 {
		t.Errorf("stats.WouldDowngrade = %d, want 0: the stored row was already truncated", stats.WouldDowngrade)
	}
	if stats.RawUpdated != 1 {
		t.Errorf("stats.RawUpdated = %d, want 1 (%+v)", stats.RawUpdated, stats)
	}
	if stats.StillTruncated != 1 {
		t.Errorf("stats.StillTruncated = %d, want 1", stats.StillTruncated)
	}
}

// The BYTE FLOOR: a repair may not shrink a capture, even when both sides are
// "truncated" (review round 3).
//
// `truncated` is a flag, and two truncated captures are not equal. A row stored
// under the 1 MiB cap holds headers plus one text part; refetching it with a
// smaller --max-bytes yields headers plus a shorter text part, or headers alone.
// The flag matches on both sides, the hash differs, and the update would shrink
// the row — unrecoverably, since raw_json has no version history.
func TestRefetchMessages_RefusesAReplacementSmallerThanTheStoredRow(t *testing.T) {
	ctx := context.Background()

	src := newFakeIMAP()
	src.folders = []google.Folder{{Name: imapINBOX, UIDValidity: 12}}
	src.add(imapINBOX, imapMsg(7, refetchNow, nil, refetchFullMessage("<placeholder-shrink@placeholder.example>")))

	sink := newFakeIMAPSink()
	target := refetchTarget(73094, imapINBOX, 12, 7)
	// Already truncated, so the flag guard does NOT catch this one...
	target.Truncated = true
	// ...but the stored row holds far more bytes than a 64-byte fetch can return.
	target.StoredB64Len = 5_000_000
	sink.stored[target.ExternalID] = storedHash

	stats, err := google.RefetchMessages(ctx, src, sink, imapAccount(),
		[]google.RefetchTarget{target}, google.RefetchConfig{MaxBytes: 64})
	if err != nil {
		t.Fatalf("RefetchMessages: %v (a refusal is an outcome, not an error)", err)
	}
	if len(sink.writes) != 0 {
		t.Errorf("wrote %d raw row(s), shrinking a stored capture: %+v", len(sink.writes), sink.writes)
	}
	if sink.stored[target.ExternalID] != storedHash {
		t.Errorf("stored content_hash changed to %q; the larger capture must survive", sink.stored[target.ExternalID])
	}
	if stats.WouldShrink != 1 {
		t.Errorf("stats.WouldShrink = %d, want 1 (%+v)", stats.WouldShrink, stats)
	}
	if stats.WouldDowngrade != 0 {
		t.Errorf("stats.WouldDowngrade = %d, want 0: both sides are truncated, so only the BYTE floor applies",
			stats.WouldDowngrade)
	}
	if stats.StillTruncated != 0 {
		t.Errorf("stats.StillTruncated = %d, want 0: nothing was written, so nothing came back still truncated",
			stats.StillTruncated)
	}
}
