package google_test

// Offline poller unit tests (SPEC 07-google-oauth-pollers, acceptance criteria
// 4, 5, 6, 13). Everything runs against the httptest fakeGoogle (fake_google_test.go)
// and an in-memory fake Sink — ZERO real network, ZERO Postgres. The raw-first
// upsert DECISION and the cursor/window/syncToken logic live in the Ingest
// functions (not the sink), so they are genuinely exercised here.
//
// GREENFIELD NOTE: package internal/connector/google does not exist yet; this
// file compile-FAILs under `go test ./...` until it is implemented — the
// expected failure mode. Imposed exported surface (the SPEC's ingest.go +
// gmail.go/calendar.go); for greenfield code the SPEC's contract IS the
// signature:
//
//   const (
//       DefaultOverlap  = time.Hour
//       DefaultBackfill = 90 * 24 * time.Hour
//   )
//
//   type Account struct {
//       ID                     int64
//       Email                  string
//       CalendarInAvailability bool
//   }
//
//   // Both cursors live side by side in source_accounts.sync_cursor.
//   type Cursor struct {
//       GmailInternalDateMS int64  `json:"gmail_internal_date_ms"`
//       CalendarSyncToken   string `json:"calendar_sync_token"`
//   }
//
//   type Stats struct {
//       GmailListed, GmailFetched, CalendarListed          int
//       RawInserted, RawUpdated, RawUnchanged              int
//       Normalized, DedupSkipped                           int
//   }
//
//   type Config struct {
//       Full, All bool
//       Overlap   time.Duration
//       Backfill  time.Duration
//       Now       time.Time // injectable clock; zero => time.Now()
//   }
//
//   // Sink is the ops-db side of the raw-first ingest phase. RawHash returns the
//   // stored content_hash for (account, external_id); Ingest compares and calls
//   // InsertRaw / UpdateRaw / neither. UpdateRaw resets normalized_at to NULL.
//   // sync_runs are per account × phase ("gmail" | "calendar").
//   type Sink interface {
//       Cursor(ctx context.Context, accountID int64) (Cursor, error)
//       SaveCursor(ctx context.Context, accountID int64, c Cursor) error
//       StartRun(ctx context.Context, accountID int64, phase string) (runID int64, err error)
//       FinishRun(ctx context.Context, runID int64, status string, stats Stats, errMsg string) error
//       RawHash(ctx context.Context, accountID int64, externalID string) (hash string, exists bool, err error)
//       InsertRaw(ctx context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) error
//       UpdateRaw(ctx context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) error
//   }
//
//   // Gmail identity is the userID (path segment); calendar identity rides on hc.
//   func NewGmailClient(hc *http.Client, baseURL, userID string) *GmailClient
//   func NewCalendarClient(hc *http.Client, baseURL string) *CalendarClient
//
//   func IngestGmail(ctx context.Context, gc *GmailClient, sink Sink, acct Account, cfg Config) (Stats, error)
//   func IngestCalendar(ctx context.Context, cc *CalendarClient, sink Sink, acct Account, cfg Config) (Stats, error)

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

// ---- fake Sink ------------------------------------------------------------

type rawWrite struct {
	externalID      string
	hash            string
	resetNormalized bool
}

type fakeSink struct {
	accountID int64
	cursor    google.Cursor
	stored    map[string]string // externalID -> content_hash

	inserts      []rawWrite
	updates      []rawWrite
	savedCursors []google.Cursor
	runs         []string // "start:phase" / "finish:status"
	finishStatus string
}

func newFakeSink() *fakeSink { return &fakeSink{accountID: 42, stored: map[string]string{}} }

func (s *fakeSink) Cursor(_ context.Context, _ int64) (google.Cursor, error) { return s.cursor, nil }

func (s *fakeSink) SaveCursor(_ context.Context, _ int64, c google.Cursor) error {
	s.savedCursors = append(s.savedCursors, c)
	s.cursor = c
	return nil
}

func (s *fakeSink) StartRun(_ context.Context, _ int64, phase string) (int64, error) {
	s.runs = append(s.runs, "start:"+phase)
	return int64(len(s.runs)), nil
}

func (s *fakeSink) FinishRun(_ context.Context, _ int64, status string, _ google.Stats, _ string) error {
	s.runs = append(s.runs, "finish:"+status)
	s.finishStatus = status
	return nil
}

func (s *fakeSink) RawHash(_ context.Context, _ int64, externalID string) (string, bool, error) {
	h, ok := s.stored[externalID]
	return h, ok, nil
}

func (s *fakeSink) InsertRaw(_ context.Context, _ int64, externalID string, _ json.RawMessage, hash string) error {
	s.inserts = append(s.inserts, rawWrite{externalID: externalID, hash: hash})
	s.stored[externalID] = hash
	return nil
}

func (s *fakeSink) UpdateRaw(_ context.Context, _ int64, externalID string, _ json.RawMessage, hash string) error {
	s.updates = append(s.updates, rawWrite{externalID: externalID, hash: hash, resetNormalized: true})
	s.stored[externalID] = hash
	return nil
}

func wroteRaw(recs []rawWrite, id string) *rawWrite {
	for i := range recs {
		if recs[i].externalID == id {
			return &recs[i]
		}
	}
	return nil
}

func acct() google.Account {
	return google.Account{ID: 42, Email: acctA, CalendarInAvailability: true}
}

// ---- Gmail cursor / window ------------------------------------------------

// Cursor read: messages.list q carries after:{floor((cursor-overlap)/1000)}
// (criterion 5) — the exact incremental seam.
func TestIngestGmail_CursorAfterMinusOverlap(t *testing.T) {
	ctx := context.Background()
	const cursorMS = int64(1751360000000)
	overlap := time.Hour

	fg := newFakeGoogle()
	defer fg.close()
	fg.addGmail(acctA, fakeGmailMsg{id: "m1", threadID: "t1",
		full: gmailFull("m1", "t1", "<m1@x>", "a@x.example", acctA, 1751362245000, "b1")})

	sink := newFakeSink()
	sink.cursor = google.Cursor{GmailInternalDateMS: cursorMS}

	gc := google.NewGmailClient(http.DefaultClient, fg.url(), acctA)
	if _, err := google.IngestGmail(ctx, gc, sink, acct(), google.Config{Overlap: overlap}); err != nil {
		t.Fatalf("IngestGmail: %v", err)
	}

	if len(fg.listReqs) == 0 {
		t.Fatalf("messages.list was never called")
	}
	wantAfter := (cursorMS - overlap.Milliseconds()) / 1000
	q := fg.listReqs[0].Q
	if !strings.Contains(q, fmt.Sprintf("after:%d", wantAfter)) {
		t.Errorf("messages.list q = %q, want it to contain after:%d (cursor minus overlap, seconds)", q, wantAfter)
	}
}

// Fresh cursor: first run backfills a configurable window (default 90d),
// expressed as after:{(now-backfill) in unix seconds} (criterion 5).
func TestIngestGmail_BackfillWindowOnFreshCursor(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	backfill := 90 * 24 * time.Hour

	fg := newFakeGoogle()
	defer fg.close()

	sink := newFakeSink() // zero cursor

	gc := google.NewGmailClient(http.DefaultClient, fg.url(), acctA)
	if _, err := google.IngestGmail(ctx, gc, sink, acct(),
		google.Config{Overlap: time.Hour, Backfill: backfill, Now: now}); err != nil {
		t.Fatalf("IngestGmail: %v", err)
	}
	if len(fg.listReqs) == 0 {
		t.Fatalf("messages.list was never called")
	}
	wantAfter := now.Add(-backfill).Unix()
	if q := fg.listReqs[0].Q; !strings.Contains(q, fmt.Sprintf("after:%d", wantAfter)) {
		t.Errorf("messages.list q = %q, want after:%d (now-backfill) on a fresh cursor", q, wantAfter)
	}
}

// Pagination: nextPageToken is followed; every listed message is fetched
// (criterion 5, list/fetch loop).
func TestIngestGmail_PaginationFollowsNextPageToken(t *testing.T) {
	ctx := context.Background()
	fg := newFakeGoogle()
	defer fg.close()
	fg.gmailPageSize = 1
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("m%d", i)
		fg.addGmail(acctA, fakeGmailMsg{id: id, threadID: "t" + id,
			full: gmailFull(id, "t"+id, "<"+id+"@x>", "s@x.example", acctA, int64(1751362245000+i), "body")})
	}

	sink := newFakeSink()
	gc := google.NewGmailClient(http.DefaultClient, fg.url(), acctA)
	stats, err := google.IngestGmail(ctx, gc, sink, acct(), google.Config{Overlap: time.Hour, Now: time.Now()})
	if err != nil {
		t.Fatalf("IngestGmail: %v", err)
	}

	if len(fg.listReqs) != 3 {
		t.Errorf("messages.list called %d times, want 3 (page followed twice)", len(fg.listReqs))
	}
	wantTokens := []string{"", "pg-1", "pg-2"}
	for i, want := range wantTokens {
		if i < len(fg.listReqs) && fg.listReqs[i].PageToken != want {
			t.Errorf("list[%d] pageToken = %q, want %q", i, fg.listReqs[i].PageToken, want)
		}
	}
	if stats.RawInserted != 3 {
		t.Errorf("RawInserted = %d, want 3 (all pages fetched)", stats.RawInserted)
	}
}

// Raw-first (criterion 4): each message lands in raw_source_items as
// gmail:{gmailMessageId} with a non-empty content_hash BEFORE any normalize;
// the cursor advances to the max internalDate on success.
func TestIngestGmail_RawFirstAndCursorAdvance(t *testing.T) {
	ctx := context.Background()
	fg := newFakeGoogle()
	defer fg.close()
	fg.addGmail(acctA, fakeGmailMsg{id: "m1", threadID: "t1",
		full: gmailFull("m1", "t1", "<m1@x>", "s@x.example", acctA, 1751362245000, "b1")})
	fg.addGmail(acctA, fakeGmailMsg{id: "m2", threadID: "t2",
		full: gmailFull("m2", "t2", "<m2@x>", "s@x.example", acctA, 1751362246000, "b2")})

	sink := newFakeSink()
	gc := google.NewGmailClient(http.DefaultClient, fg.url(), acctA)
	if _, err := google.IngestGmail(ctx, gc, sink, acct(), google.Config{Overlap: time.Hour, Now: time.Now()}); err != nil {
		t.Fatalf("IngestGmail: %v", err)
	}

	for _, id := range []string{"gmail:m1", "gmail:m2"} {
		w := wroteRaw(sink.inserts, id)
		if w == nil {
			t.Errorf("raw row %q not inserted; inserts=%+v", id, sink.inserts)
			continue
		}
		if w.hash == "" {
			t.Errorf("raw row %q inserted with empty content_hash (raw-first requires a hash)", id)
		}
	}
	if len(sink.savedCursors) == 0 {
		t.Fatalf("cursor never saved on success")
	}
	got := sink.savedCursors[len(sink.savedCursors)-1]
	if got.GmailInternalDateMS != 1751362246000 {
		t.Errorf("cursor GmailInternalDateMS = %d, want the max internalDate 1751362246000", got.GmailInternalDateMS)
	}
	if sink.finishStatus != "ok" {
		t.Errorf("run status = %q, want ok", sink.finishStatus)
	}
}

// Error handling (criterion 13): a messages.get failure aborts the run with a
// non-nil error, finishes sync_runs status=error, and does NOT advance the
// cursor.
func TestIngestGmail_ErrorLeavesCursorUnadvanced(t *testing.T) {
	ctx := context.Background()
	fg := newFakeGoogle()
	defer fg.close()
	fg.addGmail(acctA, fakeGmailMsg{id: "boom", threadID: "t1",
		full: gmailFull("boom", "t1", "<boom@x>", "s@x.example", acctA, 1751362245000, "b1")})
	fg.getStatus["boom"] = http.StatusInternalServerError

	sink := newFakeSink()
	sink.cursor = google.Cursor{GmailInternalDateMS: 1751000000000}

	gc := google.NewGmailClient(http.DefaultClient, fg.url(), acctA)
	_, err := google.IngestGmail(ctx, gc, sink, acct(), google.Config{Overlap: time.Hour, Now: time.Now()})
	if err == nil {
		t.Fatalf("IngestGmail: expected a non-nil error when messages.get returns 500")
	}
	if len(sink.savedCursors) != 0 {
		t.Errorf("cursor advanced on a failed run: %+v", sink.savedCursors)
	}
	if sink.finishStatus != "error" {
		t.Errorf("run status = %q, want error", sink.finishStatus)
	}
}

// ---- Calendar sync token / 410 -------------------------------------------

// Initial calendar sync (no token): singleEvents + timeMin + timeMax sent, no
// syncToken; the returned nextSyncToken is persisted (criterion 6).
func TestIngestCalendar_InitialSendsWindowPersistsToken(t *testing.T) {
	ctx := context.Background()
	fg := newFakeGoogle()
	defer fg.close()
	fg.calNextSyncToken = "SYNCTOK-INIT"
	fg.addCalendar(acctA, calFull("e1", "Standup", "2026-07-13T09:00:00+02:00", "2026-07-13T09:30:00+02:00"))

	sink := newFakeSink() // zero cursor -> no calendar_sync_token
	cc := google.NewCalendarClient(userHTTPClient(acctA), fg.url())
	if _, err := google.IngestCalendar(ctx, cc, sink, acct(),
		google.Config{Now: time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatalf("IngestCalendar: %v", err)
	}

	if len(fg.calListReqs) == 0 {
		t.Fatalf("events.list was never called")
	}
	first := fg.calListReqs[0]
	if first.SyncToken != "" {
		t.Errorf("initial events.list syncToken = %q, want empty (no token yet)", first.SyncToken)
	}
	if first.Single != "true" {
		t.Errorf("initial events.list singleEvents = %q, want true", first.Single)
	}
	if first.TimeMin == "" || first.TimeMax == "" {
		t.Errorf("initial events.list must send timeMin/timeMax window; got min=%q max=%q", first.TimeMin, first.TimeMax)
	}
	got := lastCursor(t, sink)
	if got.CalendarSyncToken != "SYNCTOK-INIT" {
		t.Errorf("persisted calendar_sync_token = %q, want SYNCTOK-INIT", got.CalendarSyncToken)
	}
}

// Incremental calendar sync (token present): syncToken sent, no time window;
// the new token replaces the old (criterion 6).
func TestIngestCalendar_IncrementalSendsSyncToken(t *testing.T) {
	ctx := context.Background()
	fg := newFakeGoogle()
	defer fg.close()
	fg.calNextSyncToken = "SYNCTOK-2"
	fg.addCalendar(acctA, calFull("e2", "Sync", "2026-07-14T10:00:00+02:00", "2026-07-14T10:30:00+02:00"))

	sink := newFakeSink()
	sink.cursor = google.Cursor{CalendarSyncToken: "SYNCTOK-1"}

	cc := google.NewCalendarClient(userHTTPClient(acctA), fg.url())
	if _, err := google.IngestCalendar(ctx, cc, sink, acct(), google.Config{Now: time.Now()}); err != nil {
		t.Fatalf("IngestCalendar: %v", err)
	}
	if len(fg.calListReqs) == 0 {
		t.Fatalf("events.list was never called")
	}
	first := fg.calListReqs[0]
	if first.SyncToken != "SYNCTOK-1" {
		t.Errorf("incremental events.list syncToken = %q, want the stored SYNCTOK-1", first.SyncToken)
	}
	if first.TimeMin != "" {
		t.Errorf("incremental events.list must NOT send timeMin (syncToken only); got %q", first.TimeMin)
	}
	if got := lastCursor(t, sink); got.CalendarSyncToken != "SYNCTOK-2" {
		t.Errorf("persisted calendar_sync_token = %q, want the advanced SYNCTOK-2", got.CalendarSyncToken)
	}
}

// HTTP 410 on a syncToken drops the token and re-windows (full resync recipe,
// criterion 6): a first request carries the stale token (gets 410), a second
// carries a time window and no token; the run still ends ok and persists a fresh
// token.
func TestIngestCalendar_410DropsTokenAndReWindows(t *testing.T) {
	ctx := context.Background()
	fg := newFakeGoogle()
	defer fg.close()
	fg.cal410OnSyncToken = true
	fg.calNextSyncToken = "SYNCTOK-RECOVER"
	fg.addCalendar(acctA, calFull("e3", "Recovered", "2026-07-15T09:00:00+02:00", "2026-07-15T09:30:00+02:00"))

	sink := newFakeSink()
	sink.cursor = google.Cursor{CalendarSyncToken: "STALE"}

	cc := google.NewCalendarClient(userHTTPClient(acctA), fg.url())
	if _, err := google.IngestCalendar(ctx, cc, sink, acct(),
		google.Config{Now: time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatalf("IngestCalendar (410 recovery): %v", err)
	}

	if len(fg.calListReqs) < 2 {
		t.Fatalf("events.list called %d times, want >= 2 (stale-token attempt then re-window)", len(fg.calListReqs))
	}
	if fg.calListReqs[0].SyncToken != "STALE" {
		t.Errorf("first calendar request syncToken = %q, want the STALE token (before 410)", fg.calListReqs[0].SyncToken)
	}
	last := fg.calListReqs[len(fg.calListReqs)-1]
	if last.SyncToken != "" {
		t.Errorf("re-window request must drop the syncToken; got %q", last.SyncToken)
	}
	if last.TimeMin == "" || last.TimeMax == "" {
		t.Errorf("re-window request must send a timeMin/timeMax window; got min=%q max=%q", last.TimeMin, last.TimeMax)
	}
	if sink.finishStatus != "ok" {
		t.Errorf("run status = %q, want ok (410 is a recoverable path)", sink.finishStatus)
	}
	if got := lastCursor(t, sink); got.CalendarSyncToken != "SYNCTOK-RECOVER" {
		t.Errorf("persisted calendar_sync_token = %q, want SYNCTOK-RECOVER (recovery captured a fresh token)", got.CalendarSyncToken)
	}
}

func lastCursor(t *testing.T, s *fakeSink) google.Cursor {
	t.Helper()
	if len(s.savedCursors) == 0 {
		t.Fatalf("cursor never saved")
	}
	return s.savedCursors[len(s.savedCursors)-1]
}
