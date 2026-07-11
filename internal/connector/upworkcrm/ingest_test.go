package upworkcrm_test

// Unit tests for the ingest phase (SPEC 02-upwork-crm-connector, acceptance
// criteria 1, 2, 5, 6, 8, 11). Everything runs against a fake SourceReader and
// an in-memory fake Sink — ZERO network, ZERO Postgres. The hash-compare
// DECISION (insert vs update vs unchanged) lives in Ingest, not the sink, so it
// is genuinely exercised here rather than inside a test double.
//
// GREENFIELD NOTE: package internal/connector/upworkcrm does not exist yet;
// this file compile-FAILs under `go test ./...` until it is implemented — the
// expected failure mode. Expected exported surface (the SPEC's source.go +
// ingest.go):
//
//   // Source rows carry BOTH typed fields (for filter/cursor/external-id
//   // decisions) AND the verbatim to_jsonb(row) bytes (Raw) that get hashed and
//   // stored as raw_source_items.raw_json.
//   type SourceClient struct {
//       ID  string          // clients.id (uuid) -> external_id "clients:{id}"
//       Raw json.RawMessage // verbatim source row JSON
//   }
//   type SourceCommunication struct {
//       ID        string          // communications.id -> external_id "communications:{id}"
//       ClientID  string
//       CreatedAt time.Time       // cursor column (insert time)
//       IsDraft   bool            // is_draft=true is skipped at ingest
//       Raw       json.RawMessage // verbatim source row JSON
//   }
//   type SourceReader interface {
//       ListClients(ctx context.Context) ([]SourceClient, error)
//       ListCommunications(ctx context.Context, since time.Time) ([]SourceCommunication, error)
//   }
//
//   type Cursor struct { CommunicationsCreatedAt time.Time }
//
//   type Stats struct {
//       ClientsSeen, CommunicationsSeen               int
//       RawInserted, RawUpdated, RawUnchanged         int
//       Normalized, SuspectedMerges                   int
//   }
//
//   type Config struct { Full, All bool; Overlap time.Duration }
//   const DefaultOverlap = 24 * time.Hour
//
//   // Sink is the ops-db side of ingestion. RawHash returns the currently
//   // stored content_hash for (account, external_id); Ingest compares and calls
//   // InsertRaw / UpdateRaw / neither. UpdateRaw resets normalized_at to NULL.
//   type Sink interface {
//       EnsureAccount(ctx context.Context) (accountID int64, err error)
//       Cursor(ctx context.Context, accountID int64) (Cursor, error)
//       StartRun(ctx context.Context, accountID int64) (runID int64, err error)
//       RawHash(ctx context.Context, accountID int64, externalID string) (hash string, exists bool, err error)
//       InsertRaw(ctx context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) error
//       UpdateRaw(ctx context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) error
//       SaveCursor(ctx context.Context, accountID int64, c Cursor) error
//       FinishRun(ctx context.Context, runID int64, status string, stats Stats, errMsg string) error
//   }
//
//   func Ingest(ctx context.Context, src SourceReader, sink Sink, cfg Config) (Stats, error)

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/upworkcrm"
)

// ---- fakes ----------------------------------------------------------------

type fakeSource struct {
	clients      []upworkcrm.SourceClient
	comms        []upworkcrm.SourceCommunication
	listCommsErr error

	sinceSeen   time.Time // captured argument to ListCommunications
	commsCalled bool
}

func (f *fakeSource) ListClients(_ context.Context) ([]upworkcrm.SourceClient, error) {
	return f.clients, nil
}

func (f *fakeSource) ListCommunications(_ context.Context, since time.Time) ([]upworkcrm.SourceCommunication, error) {
	f.commsCalled = true
	f.sinceSeen = since
	if f.listCommsErr != nil {
		return nil, f.listCommsErr
	}
	return f.comms, nil
}

type writeRecord struct {
	externalID      string
	hash            string
	resetNormalized bool // true for UpdateRaw
}

type fakeSink struct {
	accountID int64
	cursor    upworkcrm.Cursor
	stored    map[string]string // externalID -> current content_hash

	rawHashQueried []string
	inserts        []writeRecord
	updates        []writeRecord
	savedCursors   []upworkcrm.Cursor

	runStarted   bool
	finishStatus string
	finishStats  upworkcrm.Stats
	finishErr    string
}

func newFakeSink() *fakeSink {
	return &fakeSink{accountID: 7, stored: map[string]string{}}
}

func (s *fakeSink) EnsureAccount(_ context.Context) (int64, error) { return s.accountID, nil }

func (s *fakeSink) Cursor(_ context.Context, _ int64) (upworkcrm.Cursor, error) {
	return s.cursor, nil
}

func (s *fakeSink) StartRun(_ context.Context, _ int64) (int64, error) {
	s.runStarted = true
	return 1, nil
}

func (s *fakeSink) RawHash(_ context.Context, _ int64, externalID string) (string, bool, error) {
	s.rawHashQueried = append(s.rawHashQueried, externalID)
	h, ok := s.stored[externalID]
	return h, ok, nil
}

func (s *fakeSink) InsertRaw(_ context.Context, _ int64, externalID string, _ json.RawMessage, hash string) error {
	s.inserts = append(s.inserts, writeRecord{externalID: externalID, hash: hash})
	s.stored[externalID] = hash
	return nil
}

func (s *fakeSink) UpdateRaw(_ context.Context, _ int64, externalID string, _ json.RawMessage, hash string) error {
	s.updates = append(s.updates, writeRecord{externalID: externalID, hash: hash, resetNormalized: true})
	s.stored[externalID] = hash
	return nil
}

func (s *fakeSink) SaveCursor(_ context.Context, _ int64, c upworkcrm.Cursor) error {
	s.savedCursors = append(s.savedCursors, c)
	s.cursor = c
	return nil
}

func (s *fakeSink) FinishRun(_ context.Context, _ int64, status string, stats upworkcrm.Stats, errMsg string) error {
	s.finishStatus = status
	s.finishStats = stats
	s.finishErr = errMsg
	return nil
}

func wroteExternalID(recs []writeRecord, id string) bool {
	for _, r := range recs {
		if r.externalID == id {
			return true
		}
	}
	return false
}

// ---- fixtures -------------------------------------------------------------

func commRaw(id, clientID, channel string, draft bool) json.RawMessage {
	m := map[string]any{
		"id":              id,
		"client_id":       clientID,
		"direction":       "inbound",
		"channel":         channel,
		"subject":         "s",
		"body":            "b-" + id,
		"communicated_at": "2026-07-01T10:00:00Z",
		"sender":          "x@y.z",
		"external_id":     "ext-" + id,
		"is_draft":        draft,
	}
	b, _ := json.Marshal(m)
	return b
}

func clientRaw(id string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"id": id, "name": "C-" + id, "email": nil})
	return b
}

// ---- tests ----------------------------------------------------------------

// Draft communications (is_draft=true) are NEVER ingested: no hash lookup, no
// insert, no update for them (criterion 8, unilateral decision "skip is_draft").
func TestIngest_SkipsDrafts(t *testing.T) {
	ctx := context.Background()
	src := &fakeSource{
		comms: []upworkcrm.SourceCommunication{
			{ID: "live", ClientID: clientUUID, CreatedAt: mustTime("2026-07-01T00:00:00Z"), IsDraft: false, Raw: commRaw("live", clientUUID, "upwork", false)},
			{ID: "draft", ClientID: clientUUID, CreatedAt: mustTime("2026-07-01T01:00:00Z"), IsDraft: true, Raw: commRaw("draft", clientUUID, "upwork", true)},
		},
	}
	sink := newFakeSink()

	if _, err := upworkcrm.Ingest(ctx, src, sink, upworkcrm.Config{Overlap: 24 * time.Hour}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	if wroteExternalID(sink.inserts, "communications:draft") || wroteExternalID(sink.updates, "communications:draft") {
		t.Errorf("draft communication was ingested; it must be skipped at query time")
	}
	for _, q := range sink.rawHashQueried {
		if q == "communications:draft" {
			t.Errorf("draft communication was even hash-checked; it must never enter ingestion")
		}
	}
	if !wroteExternalID(sink.inserts, "communications:live") {
		t.Errorf("non-draft communication was not ingested")
	}
}

// external_id convention: clients:{uuid} and communications:{uuid}.
func TestIngest_ExternalIDConvention(t *testing.T) {
	ctx := context.Background()
	src := &fakeSource{
		clients: []upworkcrm.SourceClient{{ID: clientUUID, Raw: clientRaw(clientUUID)}},
		comms:   []upworkcrm.SourceCommunication{{ID: commUUID, ClientID: clientUUID, CreatedAt: mustTime("2026-07-01T00:00:00Z"), Raw: commRaw(commUUID, clientUUID, "upwork", false)}},
	}
	sink := newFakeSink()

	if _, err := upworkcrm.Ingest(ctx, src, sink, upworkcrm.Config{Overlap: 24 * time.Hour}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !wroteExternalID(sink.inserts, "clients:"+clientUUID) {
		t.Errorf("client external_id not clients:{uuid}; inserts=%+v", sink.inserts)
	}
	if !wroteExternalID(sink.inserts, "communications:"+commUUID) {
		t.Errorf("communication external_id not communications:{uuid}; inserts=%+v", sink.inserts)
	}
}

// New rows are inserted and counted (criterion 2).
func TestIngest_NewRowsInserted(t *testing.T) {
	ctx := context.Background()
	src := &fakeSource{
		clients: []upworkcrm.SourceClient{{ID: clientUUID, Raw: clientRaw(clientUUID)}},
		comms: []upworkcrm.SourceCommunication{
			{ID: "m1", ClientID: clientUUID, CreatedAt: mustTime("2026-07-01T00:00:00Z"), Raw: commRaw("m1", clientUUID, "upwork", false)},
			{ID: "m2", ClientID: clientUUID, CreatedAt: mustTime("2026-07-01T01:00:00Z"), Raw: commRaw("m2", clientUUID, "email", false)},
		},
	}
	sink := newFakeSink()

	stats, err := upworkcrm.Ingest(ctx, src, sink, upworkcrm.Config{Overlap: 24 * time.Hour})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if stats.RawInserted != 3 { // 1 client + 2 comms
		t.Errorf("RawInserted = %d, want 3", stats.RawInserted)
	}
	if stats.RawUpdated != 0 || stats.RawUnchanged != 0 {
		t.Errorf("RawUpdated=%d RawUnchanged=%d, want 0/0 on a fresh sink", stats.RawUpdated, stats.RawUnchanged)
	}
}

// Unchanged rows (stored hash matches) are skipped: neither InsertRaw nor
// UpdateRaw is called, and RawUnchanged is counted (criterion 5 short-circuit).
func TestIngest_UnchangedRowsSkipped(t *testing.T) {
	ctx := context.Background()
	raw := commRaw("m1", clientUUID, "upwork", false)
	h, err := upworkcrm.ContentHash(raw)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	src := &fakeSource{comms: []upworkcrm.SourceCommunication{
		{ID: "m1", ClientID: clientUUID, CreatedAt: mustTime("2026-07-01T00:00:00Z"), Raw: raw},
	}}
	sink := newFakeSink()
	sink.stored["communications:m1"] = h // already ingested at this exact hash

	stats, err := upworkcrm.Ingest(ctx, src, sink, upworkcrm.Config{Overlap: 24 * time.Hour})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if wroteExternalID(sink.inserts, "communications:m1") || wroteExternalID(sink.updates, "communications:m1") {
		t.Errorf("unchanged row was written; content_hash match must short-circuit")
	}
	if stats.RawUnchanged != 1 {
		t.Errorf("RawUnchanged = %d, want 1", stats.RawUnchanged)
	}
	if stats.RawInserted != 0 || stats.RawUpdated != 0 {
		t.Errorf("RawInserted=%d RawUpdated=%d, want 0/0", stats.RawInserted, stats.RawUpdated)
	}
}

// Changed rows update raw in place AND reset normalized_at, counted as updated
// (criterion 6).
func TestIngest_ChangedRowsUpdatedResetNormalized(t *testing.T) {
	ctx := context.Background()
	raw := commRaw("m1", clientUUID, "upwork", false)
	src := &fakeSource{comms: []upworkcrm.SourceCommunication{
		{ID: "m1", ClientID: clientUUID, CreatedAt: mustTime("2026-07-01T00:00:00Z"), Raw: raw},
	}}
	sink := newFakeSink()
	sink.stored["communications:m1"] = "stale-hash-from-a-prior-body" // differs from current

	stats, err := upworkcrm.Ingest(ctx, src, sink, upworkcrm.Config{Full: true})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !wroteExternalID(sink.updates, "communications:m1") {
		t.Errorf("changed row was not updated; updates=%+v", sink.updates)
	}
	if wroteExternalID(sink.inserts, "communications:m1") {
		t.Errorf("changed row was inserted as new instead of updated in place")
	}
	for _, u := range sink.updates {
		if u.externalID == "communications:m1" && !u.resetNormalized {
			t.Errorf("UpdateRaw must reset normalized_at so re-normalization reprocesses the row")
		}
	}
	if stats.RawUpdated != 1 {
		t.Errorf("RawUpdated = %d, want 1", stats.RawUpdated)
	}
}

// Cursor read: ListCommunications receives sync_cursor.communications_created_at
// MINUS the 24h overlap window (unilateral decision).
func TestIngest_CursorOverlapWindow(t *testing.T) {
	ctx := context.Background()
	cursorAt := mustTime("2026-07-05T12:00:00Z")
	src := &fakeSource{}
	sink := newFakeSink()
	sink.cursor = upworkcrm.Cursor{CommunicationsCreatedAt: cursorAt}

	if _, err := upworkcrm.Ingest(ctx, src, sink, upworkcrm.Config{Overlap: 24 * time.Hour}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !src.commsCalled {
		t.Fatalf("ListCommunications was never called")
	}
	want := cursorAt.Add(-24 * time.Hour)
	if !src.sinceSeen.Equal(want) {
		t.Errorf("ListCommunications since = %v, want cursor-24h = %v", src.sinceSeen, want)
	}
}

// --full ignores the stored cursor and rescans from the zero time.
func TestIngest_FullRescansFromZero(t *testing.T) {
	ctx := context.Background()
	src := &fakeSource{}
	sink := newFakeSink()
	sink.cursor = upworkcrm.Cursor{CommunicationsCreatedAt: mustTime("2026-07-05T12:00:00Z")}

	if _, err := upworkcrm.Ingest(ctx, src, sink, upworkcrm.Config{Full: true, Overlap: 24 * time.Hour}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !src.sinceSeen.IsZero() {
		t.Errorf("--full since = %v, want zero time (full rescan)", src.sinceSeen)
	}
}

// On success the cursor advances to the MAX created_at seen; the run finishes ok.
func TestIngest_CursorAdvancesToMaxCreatedAtOnSuccess(t *testing.T) {
	ctx := context.Background()
	early := mustTime("2026-07-01T00:00:00Z")
	late := mustTime("2026-07-03T00:00:00Z")
	src := &fakeSource{comms: []upworkcrm.SourceCommunication{
		{ID: "m1", ClientID: clientUUID, CreatedAt: early, Raw: commRaw("m1", clientUUID, "upwork", false)},
		{ID: "m2", ClientID: clientUUID, CreatedAt: late, Raw: commRaw("m2", clientUUID, "upwork", false)},
	}}
	sink := newFakeSink()

	if _, err := upworkcrm.Ingest(ctx, src, sink, upworkcrm.Config{Overlap: 24 * time.Hour}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(sink.savedCursors) == 0 {
		t.Fatalf("cursor was never saved on a successful run")
	}
	got := sink.savedCursors[len(sink.savedCursors)-1]
	if !got.CommunicationsCreatedAt.Equal(late) {
		t.Errorf("advanced cursor = %v, want max created_at = %v", got.CommunicationsCreatedAt, late)
	}
	if sink.finishStatus != "ok" {
		t.Errorf("FinishRun status = %q, want %q", sink.finishStatus, "ok")
	}
}

// Failure bookkeeping (criterion 11): an error mid-run finishes sync_runs with
// status=error + message, does NOT advance the cursor, and returns a non-nil
// error (binary exits non-zero).
func TestIngest_ErrorMidRunDoesNotAdvanceCursor(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("source read failed")
	src := &fakeSource{listCommsErr: boom}
	sink := newFakeSink()
	sink.cursor = upworkcrm.Cursor{CommunicationsCreatedAt: mustTime("2026-07-05T12:00:00Z")}

	_, err := upworkcrm.Ingest(ctx, src, sink, upworkcrm.Config{Overlap: 24 * time.Hour})
	if err == nil {
		t.Fatalf("Ingest: expected a non-nil error when the source read fails")
	}
	if len(sink.savedCursors) != 0 {
		t.Errorf("cursor advanced on a failed run: %+v", sink.savedCursors)
	}
	if sink.finishStatus != "error" {
		t.Errorf("FinishRun status = %q, want %q", sink.finishStatus, "error")
	}
	if sink.finishErr == "" {
		t.Errorf("FinishRun error text is empty; the failure must be recorded")
	}
}

func mustTime(s string) time.Time {
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return tm
}
