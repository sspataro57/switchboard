//go:build integration

package google_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	bridgePGEmail = "itest-google-bridge-existing@example.com"
	bridgePGNew   = "itest-google-bridge-new@example.com"
	bridgePGKey   = "itest-google-bridge-key"
	bridgePGToken = "itest-google-bridge-refresh-token"
)

type pgBridgeSource struct {
	account          google.BridgeAccount
	raw              json.RawMessage
	calendarRaw      json.RawMessage
	requests         []google.BridgeExportRequest
	calendarRequests []google.BridgeCalendarExportRequest
}

func (s *pgBridgeSource) Accounts(context.Context) (google.BridgeAccounts, error) {
	return google.BridgeAccounts{SchemaVersion: google.BridgeSchemaVersion, Accounts: []google.BridgeAccount{s.account}}, nil
}

func (s *pgBridgeSource) Export(_ context.Context, request google.BridgeExportRequest) (google.BridgeExport, error) {
	s.requests = append(s.requests, request)
	return google.BridgeExport{
		SchemaVersion:     google.BridgeSchemaVersion,
		Account:           s.account,
		Messages:          []json.RawMessage{s.raw},
		MaxInternalDateMS: 1784995200123,
	}, nil
}

func (s *pgBridgeSource) CalendarExport(_ context.Context, request google.BridgeCalendarExportRequest) (google.BridgeCalendarExport, error) {
	s.calendarRequests = append(s.calendarRequests, request)
	return google.BridgeCalendarExport{
		SchemaVersion: google.BridgeSchemaVersion,
		Account:       s.account,
		Events:        []json.RawMessage{s.calendarRaw},
		NextSyncToken: "bridge-calendar-next",
	}, nil
}

func cleanupGoogleBridge(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	statements := []string{
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-google-bridge-%'))`,
		`DELETE FROM normalized_events WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-google-bridge-%'))`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'gmail:itest-google-bridge-%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-google-bridge-%')`,
		`DELETE FROM sync_runs WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-google-bridge-%')`,
		`DELETE FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-google-bridge-%'`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("bridge cleanup %q: %v", statement, err)
		}
	}
}

func TestGmailBridge_PostgresAccountPreservationRawCursorAndIdempotence(t *testing.T) {
	requireCompose(t)
	ctx := context.Background()
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	defer pool.Close()
	cleanupGoogleBridge(t, ctx, pool)
	defer cleanupGoogleBridge(t, ctx, pool)

	var existingID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts
		   (provider, account_email, refresh_token_encrypted, scopes, domain_default,
		    send_enabled, calendar_in_availability, sync_cursor)
		 VALUES ('google', $1, pgp_sym_encrypt($2,$3), $4, 'client.example', true, false,
		         '{"gmail_internal_date_ms":1784990000000,"calendar_sync_token":"keep-calendar"}')
		 RETURNING id`,
		bridgePGEmail, bridgePGToken, bridgePGKey, google.Scopes).Scan(&existingID); err != nil {
		t.Fatalf("insert existing bridge account: %v", err)
	}

	sink := google.NewPGSink(pool)
	preserved, err := sink.EnsureBridgeAccount(ctx, google.BridgeAccount{Alias: "work-main", Email: "ITEST-GOOGLE-BRIDGE-EXISTING@EXAMPLE.COM"})
	if err != nil {
		t.Fatalf("EnsureBridgeAccount existing: %v", err)
	}
	if preserved.ID != existingID {
		t.Fatalf("existing account id = %d, want %d", preserved.ID, existingID)
	}
	decrypted, err := google.DecryptRefreshToken(ctx, pool, existingID, bridgePGKey)
	if err != nil || decrypted != bridgePGToken {
		t.Fatalf("existing refresh token changed: token=%q err=%v", decrypted, err)
	}
	var scopes []string
	var domain string
	var sendEnabled, calendarAvailability bool
	if err := pool.QueryRow(ctx,
		`SELECT scopes, domain_default, send_enabled, calendar_in_availability
		 FROM source_accounts WHERE id=$1`, existingID).
		Scan(&scopes, &domain, &sendEnabled, &calendarAvailability); err != nil {
		t.Fatalf("read preserved account policy: %v", err)
	}
	if !equalStringSet(scopes, google.Scopes) || domain != "client.example" || !sendEnabled || calendarAvailability {
		t.Fatalf("existing account policy changed: scopes=%v domain=%q send=%v calendar=%v", scopes, domain, sendEnabled, calendarAvailability)
	}

	created, err := sink.EnsureBridgeAccount(ctx, google.BridgeAccount{Alias: "work-new", Email: bridgePGNew})
	if err != nil {
		t.Fatalf("EnsureBridgeAccount new: %v", err)
	}
	var tokenIsNull bool
	var newSend bool
	if err := pool.QueryRow(ctx,
		`SELECT refresh_token_encrypted IS NULL, send_enabled FROM source_accounts WHERE id=$1`, created.ID).
		Scan(&tokenIsNull, &newSend); err != nil {
		t.Fatalf("read new bridge account: %v", err)
	}
	if !tokenIsNull || newSend {
		t.Fatalf("new bridge account token_null=%v send_enabled=%v, want true/false", tokenIsNull, newSend)
	}

	raw := json.RawMessage(`{
		"id":"bridge-pg-1","threadId":"bridge-thread-1","internalDate":"1784995200123",
		"payload":{"mimeType":"text/plain","headers":[
			{"name":"Message-ID","value":"<bridge-pg-1@example.net>"},
			{"name":"From","value":"client@example.net"},
			{"name":"To","value":"itest-google-bridge-existing@example.com"}
		],"body":{"data":"YnJpZGdlIHBnIGJvZHk"}}
	}`)
	source := &pgBridgeSource{
		account: google.BridgeAccount{Alias: "work-main", Email: bridgePGEmail}, raw: raw,
		calendarRaw: json.RawMessage(`{
			"id":"bridge-cal-pg-1","status":"confirmed","summary":"Bridge calendar integration",
			"transparency":"opaque",
			"start":{"dateTime":"2026-07-27T14:00:00-04:00"},
			"end":{"dateTime":"2026-07-27T14:30:00-04:00"},
			"attendees":[{"email":"client@example.net","responseStatus":"accepted"}]
		}`),
	}
	cfg := google.Config{Now: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC), Overlap: time.Hour, Backfill: 24 * time.Hour}
	first, err := google.RunBridge(ctx, source, sink, cfg)
	if err != nil {
		t.Fatalf("first RunBridge: %v", err)
	}
	if first.RawInserted != 2 || first.CalendarListed != 1 {
		t.Fatalf("first bridge stats = %+v, want Gmail + Calendar raw inserts", first)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items WHERE source_account_id=$1 AND normalized_at IS NULL`, existingID); got != 2 {
		t.Fatalf("pending bridge raw before Normalize = %d, want Gmail + Calendar", got)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_events WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id=$1)`, existingID); got != 0 {
		t.Fatalf("Calendar normalized before raw-first boundary: %d events", got)
	}
	if _, err := google.Normalize(ctx, sink, google.Config{}); err != nil {
		t.Fatalf("Normalize bridge raw: %v", err)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_events WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id=$1)`, existingID); got != 1 {
		t.Fatalf("normalized bridge Calendar events = %d, want 1", got)
	}
	second, err := google.RunBridge(ctx, source, sink, cfg)
	if err != nil {
		t.Fatalf("second RunBridge: %v", err)
	}
	if second.RawUnchanged != 2 || second.RawInserted != 0 {
		t.Fatalf("bridge idempotence stats first=%+v second=%+v", first, second)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items WHERE source_account_id=$1 AND external_id='gmail:bridge-pg-1'`, existingID); got != 1 {
		t.Fatalf("bridge raw rows = %d, want 1", got)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items WHERE source_account_id=$1 AND external_id='calendar:bridge-cal-pg-1'`, existingID); got != 1 {
		t.Fatalf("bridge Calendar raw rows = %d, want 1", got)
	}
	var cursor google.Cursor
	var cursorRaw []byte
	if err := pool.QueryRow(ctx, `SELECT sync_cursor FROM source_accounts WHERE id=$1`, existingID).Scan(&cursorRaw); err != nil {
		t.Fatalf("read bridge cursor: %v", err)
	}
	if err := json.Unmarshal(cursorRaw, &cursor); err != nil {
		t.Fatalf("parse bridge cursor: %v", err)
	}
	if cursor.GmailInternalDateMS != 1784995200123 || cursor.CalendarSyncToken != "bridge-calendar-next" {
		t.Fatalf("bridge cursor = %+v", cursor)
	}
	if len(source.requests) != 2 || source.requests[0].AfterUnix != (int64(1784990000000)-(time.Hour).Milliseconds())/1000 {
		t.Fatalf("bridge export requests = %+v", source.requests)
	}
	if len(source.calendarRequests) != 2 || source.calendarRequests[0].SyncToken != "keep-calendar" || source.calendarRequests[1].SyncToken != "bridge-calendar-next" {
		t.Fatalf("bridge Calendar requests = %+v", source.calendarRequests)
	}
	for _, request := range source.calendarRequests {
		if request.TimeMin == "" || request.TimeMax == "" || request.MaxEvents <= 0 {
			t.Fatalf("bridge Calendar request is not bounded/recoverable: %+v", request)
		}
	}

	// Incremental Calendar sync can replace a full event with a minimal
	// deletion tombstone. It must update the existing normalized row to
	// cancelled/null times rather than leaving the prior busy event stale.
	source.calendarRaw = json.RawMessage(`{"id":"bridge-cal-pg-1","status":"cancelled"}`)
	tombstoneStats, err := google.RunBridge(ctx, source, sink, cfg)
	if err != nil {
		t.Fatalf("tombstone RunBridge: %v", err)
	}
	if tombstoneStats.RawUpdated != 1 || tombstoneStats.RawUnchanged != 1 {
		t.Fatalf("tombstone ingest stats = %+v, want Calendar update + Gmail unchanged", tombstoneStats)
	}
	if _, err := google.Normalize(ctx, sink, google.Config{}); err != nil {
		t.Fatalf("Normalize Calendar tombstone: %v", err)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_events
		 WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id=$1)
		   AND status='cancelled' AND starts_at IS NULL AND ends_at IS NULL`, existingID); got != 1 {
		t.Fatalf("normalized Calendar tombstones = %d, want one cancelled row with null interval", got)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items WHERE source_account_id=$1 AND normalized_at IS NULL`, existingID); got != 0 {
		t.Fatalf("pending raw after tombstone Normalize = %d, want 0", got)
	}
}

// TestGmailBridge_PostgresSupersedeAndRevive exercises the SQL a fake sink
// cannot: the windowed reset replacement, the refusal of an empty snapshot,
// superseded rows going invisible to RawHash, and revive-on-re-observation. Every one of
// these is a silent-data-loss failure mode if the SQL is subtly wrong.
func TestGmailBridge_PostgresSupersedeAndRevive(t *testing.T) {
	requireCompose(t)
	ctx := context.Background()
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	defer pool.Close()
	cleanupGoogleBridge(t, ctx, pool)
	defer cleanupGoogleBridge(t, ctx, pool)

	sink := google.NewPGSink(pool)
	account, err := sink.EnsureBridgeAccount(ctx, google.BridgeAccount{
		Alias: "itest-supersede", Email: bridgePGNew,
	})
	if err != nil {
		t.Fatalf("EnsureBridgeAccount: %v", err)
	}

	kept := json.RawMessage(`{"id":"kept","status":"confirmed","start":{"dateTime":"2026-08-01T10:00:00Z"},"end":{"dateTime":"2026-08-01T11:00:00Z"}}`)
	gone := json.RawMessage(`{"id":"gone","status":"confirmed","start":{"dateTime":"2026-08-02T10:00:00Z"},"end":{"dateTime":"2026-08-02T11:00:00Z"}}`)
	if err := sink.InsertRaw(ctx, account.ID, "calendar:kept", kept, "hash-kept"); err != nil {
		t.Fatalf("InsertRaw kept: %v", err)
	}
	if err := sink.InsertRaw(ctx, account.ID, "calendar:gone", gone, "hash-gone"); err != nil {
		t.Fatalf("InsertRaw gone: %v", err)
	}

	window := func() (time.Time, time.Time) {
		return time.Now().Add(-30 * 24 * time.Hour), time.Now().Add(90 * 24 * time.Hour)
	}
	from, to := window()
	superseded, err := sink.SupersedeAbsentCalendar(ctx, account.ID, []string{"calendar:kept"}, from, to)
	if err != nil {
		t.Fatalf("SupersedeAbsentCalendar: %v", err)
	}
	if superseded != 1 {
		t.Fatalf("superseded = %d, want 1 (only calendar:gone)", superseded)
	}

	// Idempotent: a second reset with the same snapshot supersedes nothing new.
	again, err := sink.SupersedeAbsentCalendar(ctx, account.ID, []string{"calendar:kept"}, from, to)
	if err != nil {
		t.Fatalf("second SupersedeAbsentCalendar: %v", err)
	}
	if again != 0 {
		t.Fatalf("second reset superseded %d rows, want 0 — the sweep is not idempotent", again)
	}

	var liveHash string
	var found bool
	liveHash, found, err = sink.RawHash(ctx, account.ID, "calendar:gone")
	if err != nil {
		t.Fatalf("RawHash superseded: %v", err)
	}
	if found {
		t.Fatalf("RawHash reported a superseded row as live (hash %q); a re-created event would be skipped as unchanged", liveHash)
	}
	if _, found, err = sink.RawHash(ctx, account.ID, "calendar:kept"); err != nil || !found {
		t.Fatalf("RawHash kept = (found %v, err %v), want the live row", found, err)
	}

	// The event comes back with the SAME id — the upsert must revive it.
	revived := json.RawMessage(`{"id":"gone","status":"confirmed","start":{"dateTime":"2026-09-09T10:00:00Z"},"end":{"dateTime":"2026-09-09T11:00:00Z"}}`)
	if err := sink.InsertRaw(ctx, account.ID, "calendar:gone", revived, "hash-revived"); err != nil {
		t.Fatalf("InsertRaw revived (upsert over a superseded row): %v", err)
	}
	var supersededAt *time.Time
	var normalizedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT superseded_at, normalized_at FROM raw_source_items
		  WHERE source_account_id=$1 AND external_id='calendar:gone'`,
		account.ID).Scan(&supersededAt, &normalizedAt); err != nil {
		t.Fatalf("read revived row: %v", err)
	}
	if supersededAt != nil {
		t.Fatal("re-observed row is still superseded; the revived event will never normalize")
	}
	if normalizedAt != nil {
		t.Fatal("revived row kept normalized_at; the canonical row will not be rebuilt")
	}
	if _, found, err = sink.RawHash(ctx, account.ID, "calendar:gone"); err != nil || !found {
		t.Fatalf("RawHash after revive = (found %v, err %v), want the live row", found, err)
	}

	// An empty replacement is refused outright: it cannot be told apart from a
	// broken leaf, and acting on it would cancel the whole window.
	if _, err := sink.SupersedeAbsentCalendar(ctx, account.ID, nil,
		time.Now().Add(-30*24*time.Hour), time.Now().Add(90*24*time.Hour)); err == nil {
		t.Fatal("an empty reset snapshot was accepted; it would cancel every event in the window")
	}

	// Out-of-window history must survive a reset: it was never queried, so its
	// absence proves nothing.
	old := json.RawMessage(`{"id":"ancient","status":"confirmed","start":{"dateTime":"2019-01-01T10:00:00Z"},"end":{"dateTime":"2019-01-01T11:00:00Z"}}`)
	if err := sink.InsertRaw(ctx, account.ID, "calendar:ancient", old, "hash-ancient"); err != nil {
		t.Fatalf("InsertRaw ancient: %v", err)
	}
	if _, err := sink.SupersedeAbsentCalendar(ctx, account.ID, []string{"calendar:kept"},
		time.Now().Add(-30*24*time.Hour), time.Now().Add(90*24*time.Hour)); err != nil {
		t.Fatalf("windowed supersede: %v", err)
	}
	if _, found, err = sink.RawHash(ctx, account.ID, "calendar:ancient"); err != nil || !found {
		t.Fatal("a 2019 event outside the reset window was superseded; resets would rewrite history")
	}
}
