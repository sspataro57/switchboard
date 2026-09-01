package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"
)

func bridgeCalendarRaw(id, summary, start, end string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
		"id":%q,
		"status":"confirmed",
		"summary":%q,
		"transparency":"opaque",
		"start":{"dateTime":%q},
		"end":{"dateTime":%q},
		"attendees":[{"email":"client@example.net","responseStatus":"accepted"}]
	}`, id, summary, start, end))
}

func TestRunBridge_CalendarRunsAfterGmailAndPersistsExactRawBeforeCursor(t *testing.T) {
	var trace []string
	event1 := bridgeCalendarRaw("evt-1", "Review", "2026-07-27T14:00:00-04:00", "2026-07-27T14:30:00-04:00")
	event2 := bridgeCalendarRaw("evt-2", "Follow-up", "2026-07-28T09:00:00-04:00", "2026-07-28T09:30:00-04:00")
	source := oneBridgeAccountSource("work-main", "me@corp.example")
	source.trace = &trace
	source.calendarExports = map[string]BridgeCalendarExport{
		"work-main": {
			SchemaVersion: BridgeSchemaVersion,
			Account:       BridgeAccount{Alias: "work-main", Email: "me@corp.example"},
			Events:        []json.RawMessage{event1, event2},
			NextSyncToken: "calendar-fresh",
			Reset:         true,
		},
	}
	sink := newBridgeFixtureSink(&trace)
	sink.accountsByEmail["me@corp.example"] = Account{ID: 77, Email: "me@corp.example"}
	sink.cursors[77] = Cursor{GmailInternalDateMS: 1784995200000, CalendarSyncToken: "calendar-expired"}
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	stats, err := RunBridge(context.Background(), source, sink, Config{Now: now, Overlap: time.Hour})
	if err != nil {
		t.Fatalf("RunBridge: %v", err)
	}
	if stats.CalendarListed != 2 || stats.RawInserted != 2 || stats.GmailListed != 0 {
		t.Errorf("stats = %+v, want two Calendar observations and no Gmail messages", stats)
	}
	if len(source.calendarRequests) != 1 {
		t.Fatalf("calendar requests = %+v", source.calendarRequests)
	}
	request := source.calendarRequests[0]
	if request.Account != "work-main" || request.SyncToken != "calendar-expired" || request.MaxEvents <= 0 {
		t.Errorf("incremental calendar request = %+v", request)
	}
	if request.TimeMin != now.Add(-CalendarWindowPast).Format(time.RFC3339) || request.TimeMax != now.Add(CalendarWindowFuture).Format(time.RFC3339) {
		t.Errorf("fallback window = %q..%q, want %q..%q", request.TimeMin, request.TimeMax,
			now.Add(-CalendarWindowPast).Format(time.RFC3339), now.Add(CalendarWindowFuture).Format(time.RFC3339))
	}

	for _, want := range []struct {
		externalID string
		raw        json.RawMessage
	}{{"calendar:evt-1", event1}, {"calendar:evt-2", event2}} {
		key := bridgeRawKey(77, want.externalID)
		got, ok := sink.raw[key]
		if !ok {
			t.Errorf("missing exact raw observation %s", key)
			continue
		}
		if string(got) != string(want.raw) {
			t.Errorf("raw event %s changed:\n got %s\nwant %s", key, got, want.raw)
		}
	}
	if len(sink.savedCursors) != 1 {
		t.Fatalf("saved cursors = %+v, want exactly the Calendar commit", sink.savedCursors)
	}
	saved := sink.savedCursors[0]
	if saved.GmailInternalDateMS != 1784995200000 || saved.CalendarSyncToken != "calendar-fresh" {
		t.Errorf("saved cursor = %+v, want Gmail preserved and Calendar advanced", saved)
	}

	assertTraceBefore(t, trace, "source:export:work-main", "sink:start:77:calendar")
	assertTraceBefore(t, trace, "sink:start:77:calendar", "source:calendar-export:work-main")
	assertTraceBefore(t, trace, "sink:insert:"+bridgeRawKey(77, "calendar:evt-1"), "sink:save:77")
	assertTraceBefore(t, trace, "sink:insert:"+bridgeRawKey(77, "calendar:evt-2"), "sink:save:77")
	if !slices.Equal(sink.finishStatuses, []string{"ok", "ok"}) {
		t.Errorf("run statuses = %v, want Gmail ok then Calendar ok", sink.finishStatuses)
	}
}

func TestRunBridge_CalendarWindowAndTokenRequests(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		full      bool
		stored    string
		wantToken string
	}{
		{name: "initial window", stored: "", wantToken: ""},
		{name: "incremental token plus fallback window", stored: "sync-old", wantToken: "sync-old"},
		{name: "full forces window", full: true, stored: "sync-old", wantToken: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			source := oneBridgeAccountSource("personal", "me@example.com")
			sink := newBridgeFixtureSink(nil)
			sink.accountsByEmail["me@example.com"] = Account{ID: 77, Email: "me@example.com"}
			sink.cursors[77] = Cursor{GmailInternalDateMS: 1784995200000, CalendarSyncToken: tc.stored}
			if _, err := RunBridge(context.Background(), source, sink, Config{Now: now, Full: tc.full}); err != nil {
				t.Fatalf("RunBridge: %v", err)
			}
			if len(source.calendarRequests) != 1 {
				t.Fatalf("calendar requests = %+v", source.calendarRequests)
			}
			request := source.calendarRequests[0]
			if request.SyncToken != tc.wantToken {
				t.Errorf("sync_token = %q, want %q", request.SyncToken, tc.wantToken)
			}
			if request.TimeMin != now.Add(-CalendarWindowPast).Format(time.RFC3339) || request.TimeMax != now.Add(CalendarWindowFuture).Format(time.RFC3339) {
				t.Errorf("request lacks bounded fallback window: %+v", request)
			}
			if request.MaxEvents <= 0 {
				t.Errorf("request max_events = %d, want finite positive limit", request.MaxEvents)
			}
		})
	}
}

func TestRunBridge_CalendarRawFailureLeavesBothCursorFieldsUnchanged(t *testing.T) {
	event1 := bridgeCalendarRaw("evt-ok", "First", "2026-07-27T14:00:00Z", "2026-07-27T14:30:00Z")
	event2 := bridgeCalendarRaw("evt-fail", "Second", "2026-07-28T14:00:00Z", "2026-07-28T14:30:00Z")
	source := oneBridgeAccountSource("personal", "me@example.com")
	source.calendarExports = map[string]BridgeCalendarExport{
		"personal": {
			SchemaVersion: BridgeSchemaVersion,
			Account:       BridgeAccount{Alias: "personal", Email: "me@example.com"},
			Events:        []json.RawMessage{event1, event2}, NextSyncToken: "sync-new",
		},
	}
	sink := newBridgeFixtureSink(nil)
	sink.accountsByEmail["me@example.com"] = Account{ID: 77, Email: "me@example.com"}
	prior := Cursor{GmailInternalDateMS: 1784995200000, CalendarSyncToken: "sync-old"}
	sink.cursors[77] = prior
	sink.failInsertID = "calendar:evt-fail"

	_, err := RunBridge(context.Background(), source, sink, Config{Now: time.Now()})
	if err == nil {
		t.Fatal("RunBridge succeeded after Calendar raw persistence failed")
	}
	if len(sink.savedCursors) != 0 {
		t.Fatalf("cursor advanced after partial Calendar raw persistence: %+v", sink.savedCursors)
	}
	// reflect.DeepEqual rather than !=: Cursor is expected to gain a per-folder
	// map (SWT-11), which would make == a compile error here.
	if got := sink.cursors[77]; !reflect.DeepEqual(got, prior) {
		t.Errorf("cursor changed after Calendar failure: got %+v want %+v", got, prior)
	}
	if !slices.Equal(sink.finishStatuses, []string{"ok", "error"}) {
		t.Errorf("run statuses = %v, want Gmail ok then Calendar error", sink.finishStatuses)
	}
}

func TestRunBridge_CalendarBridgeFailureLeavesCursorUnchanged(t *testing.T) {
	source := oneBridgeAccountSource("personal", "me@example.com")
	source.calendarExportErrs = map[string]error{"personal": errors.New("calendar bridge unavailable")}
	sink := newBridgeFixtureSink(nil)
	sink.accountsByEmail["me@example.com"] = Account{ID: 77, Email: "me@example.com"}
	prior := Cursor{GmailInternalDateMS: 1784995200000, CalendarSyncToken: "sync-old"}
	sink.cursors[77] = prior

	_, err := RunBridge(context.Background(), source, sink, Config{Now: time.Now()})
	if err == nil {
		t.Fatal("RunBridge succeeded after Calendar bridge failure")
	}
	if len(sink.savedCursors) != 0 || !reflect.DeepEqual(sink.cursors[77], prior) {
		t.Errorf("cursor changed after Calendar bridge failure: saves=%+v cursor=%+v", sink.savedCursors, sink.cursors[77])
	}
	if !slices.Equal(sink.finishStatuses, []string{"ok", "error"}) {
		t.Errorf("run statuses = %v, want Gmail ok then Calendar error", sink.finishStatuses)
	}
}

func TestRunBridge_ReingestUnchangedCalendarEventsIsIdempotent(t *testing.T) {
	event := bridgeCalendarRaw("evt-same", "Stable", "2026-07-27T14:00:00Z", "2026-07-27T14:30:00Z")
	source := oneBridgeAccountSource("personal", "me@example.com")
	source.calendarExports = map[string]BridgeCalendarExport{
		"personal": {
			SchemaVersion: BridgeSchemaVersion,
			Account:       BridgeAccount{Alias: "personal", Email: "me@example.com"},
			Events:        []json.RawMessage{event}, NextSyncToken: "sync-stable",
		},
	}
	sink := newBridgeFixtureSink(nil)

	first, err := RunBridge(context.Background(), source, sink, Config{Now: time.Now()})
	if err != nil {
		t.Fatalf("first RunBridge: %v", err)
	}
	second, err := RunBridge(context.Background(), source, sink, Config{Now: time.Now()})
	if err != nil {
		t.Fatalf("second RunBridge: %v", err)
	}
	if first.RawInserted != 1 || second.RawUnchanged != 1 || second.RawInserted != 0 || second.RawUpdated != 0 {
		t.Errorf("Calendar idempotence stats first=%+v second=%+v", first, second)
	}
	if len(sink.inserts) != 1 || len(sink.updates) != 0 {
		t.Errorf("Calendar writes after unchanged re-ingest: inserts=%d updates=%d", len(sink.inserts), len(sink.updates))
	}
}

func TestBridgeCalendarRawUsesExistingCalendarNormalizer(t *testing.T) {
	raw := bridgeCalendarRaw("evt-existing", "Existing model", "2026-07-27T14:00:00-04:00", "2026-07-27T14:30:00-04:00")
	normalized, err := NormalizeCalendarEvent(raw)
	if err != nil {
		t.Fatalf("NormalizeCalendarEvent on bridge-captured raw: %v", err)
	}
	if normalized.Title != "Existing model" || normalized.Status != "confirmed" || normalized.Transparency != "opaque" {
		t.Errorf("existing canonical Calendar model not reused: %+v", normalized)
	}
	if len(normalized.Attendees) != 1 || normalized.Attendees[0].Email != "client@example.net" {
		t.Errorf("exact event attendees not preserved by existing normalizer: %+v", normalized.Attendees)
	}
}

func TestBridgeCalendarDeletionTombstoneUsesExistingCalendarNormalizer(t *testing.T) {
	raw := json.RawMessage(`{"id":"evt-deleted","status":"cancelled"}`)
	normalized, err := NormalizeCalendarEvent(raw)
	if err != nil {
		t.Fatalf("NormalizeCalendarEvent on bridge tombstone: %v", err)
	}
	if normalized.Status != "cancelled" || !normalized.StartsAt.IsZero() || !normalized.EndsAt.IsZero() {
		t.Fatalf("normalized tombstone = %+v", normalized)
	}
}
