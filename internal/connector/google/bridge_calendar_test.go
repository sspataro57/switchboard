package google

// Greenfield Calendar protocol surface from
// docs/tickets/gmail-calendar-local-bridge_SPEC.md:
//
//   type BridgeCalendarExportRequest struct {
//       Account, SyncToken, TimeMin, TimeMax string
//       MaxEvents int
//   }
//   type BridgeCalendarExport struct {
//       SchemaVersion int
//       Account BridgeAccount
//       Events []json.RawMessage
//       NextSyncToken string
//       Reset bool
//   }
//   func (*CommandBridge) CalendarExport(context.Context, BridgeCalendarExportRequest) (BridgeCalendarExport, error)
//
// The subprocess request always includes a bounded fallback window. An
// incremental request additionally includes SyncToken; the local leaf uses the
// window only if Google rejects that token with HTTP 410 and reports Reset.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCommandBridge_CalendarExportMapsIncrementalRequestAndPreservesExactEvents(t *testing.T) {
	raw := json.RawMessage(`{"id":"evt-bridge-1","status":"confirmed","summary":"Client review","start":{"dateTime":"2026-07-27T14:00:00-04:00"},"end":{"dateTime":"2026-07-27T14:30:00-04:00"},"attendees":[{"email":"client@example.net","responseStatus":"accepted"}]}`)
	b, _ := newProtocolBridge(t, func(_ context.Context, _ string, operation string, stdin []byte, limits bridgeCommandLimits) ([]byte, []byte, error) {
		requireFiniteBridgeLimits(t, limits)
		if operation != "calendar-export" {
			t.Fatalf("operation = %q, want calendar-export", operation)
		}
		var request BridgeCalendarExportRequest
		if err := json.Unmarshal(stdin, &request); err != nil {
			t.Fatalf("decode calendar-export request: %v", err)
		}
		want := BridgeCalendarExportRequest{
			Account: "work-main", SyncToken: "sync-old",
			TimeMin: "2026-06-25T12:00:00Z", TimeMax: "2026-10-23T12:00:00Z",
			MaxEvents: 321,
		}
		if request != want {
			t.Errorf("calendar-export request = %+v, want %+v", request, want)
		}
		response := `{"schema_version":1,"account":{"alias":"work-main","email":"me@corp.example"},"events":[` + string(raw) + `],"next_sync_token":"sync-new","reset":false}`
		return []byte(response), nil, nil
	})

	got, err := b.CalendarExport(context.Background(), BridgeCalendarExportRequest{
		Account: "work-main", SyncToken: "sync-old",
		TimeMin: "2026-06-25T12:00:00Z", TimeMax: "2026-10-23T12:00:00Z",
		MaxEvents: 321,
	})
	if err != nil {
		t.Fatalf("CalendarExport: %v", err)
	}
	if got.SchemaVersion != BridgeSchemaVersion || got.Account.Alias != "work-main" || got.Account.Email != "me@corp.example" {
		t.Errorf("CalendarExport identity/version = %+v", got)
	}
	if got.NextSyncToken != "sync-new" || got.Reset {
		t.Errorf("CalendarExport cursor/reset = %+v", got)
	}
	if len(got.Events) != 1 || string(got.Events[0]) != string(raw) {
		t.Errorf("Calendar event changed across bridge:\n got %s\nwant %s", firstCalendarRaw(got.Events), raw)
	}
}

func TestCommandBridge_CalendarExportReportsSyncTokenReset(t *testing.T) {
	b, _ := newProtocolBridge(t, func(_ context.Context, _ string, operation string, _ []byte, _ bridgeCommandLimits) ([]byte, []byte, error) {
		if operation != "calendar-export" {
			t.Fatalf("operation = %q", operation)
		}
		return []byte(`{"schema_version":1,"account":{"alias":"personal","email":"me@example.com"},"events":[],"next_sync_token":"sync-recovered","reset":true}`), nil, nil
	})

	got, err := b.CalendarExport(context.Background(), BridgeCalendarExportRequest{
		Account: "personal", SyncToken: "sync-expired",
		TimeMin: "2026-06-25T12:00:00Z", TimeMax: "2026-10-23T12:00:00Z", MaxEvents: 100,
	})
	if err != nil {
		t.Fatalf("CalendarExport: %v", err)
	}
	if !got.Reset || got.NextSyncToken != "sync-recovered" {
		t.Errorf("reset result = %+v, want reset with recovered terminal token", got)
	}
}

func TestCommandBridge_CalendarExportValidatesRequestBeforeRunner(t *testing.T) {
	tests := []struct {
		name    string
		request BridgeCalendarExportRequest
	}{
		{name: "missing account", request: BridgeCalendarExportRequest{TimeMin: "2026-01-01T00:00:00Z", TimeMax: "2026-01-02T00:00:00Z", MaxEvents: 1}},
		{name: "unbounded events", request: BridgeCalendarExportRequest{Account: "personal", TimeMin: "2026-01-01T00:00:00Z", TimeMax: "2026-01-02T00:00:00Z"}},
		{name: "missing time min", request: BridgeCalendarExportRequest{Account: "personal", TimeMax: "2026-01-02T00:00:00Z", MaxEvents: 1}},
		{name: "missing time max", request: BridgeCalendarExportRequest{Account: "personal", TimeMin: "2026-01-01T00:00:00Z", MaxEvents: 1}},
		{name: "invalid time min", request: BridgeCalendarExportRequest{Account: "personal", TimeMin: "yesterday", TimeMax: "2026-01-02T00:00:00Z", MaxEvents: 1}},
		{name: "reversed window", request: BridgeCalendarExportRequest{Account: "personal", TimeMin: "2026-01-03T00:00:00Z", TimeMax: "2026-01-02T00:00:00Z", MaxEvents: 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			b, _ := newProtocolBridge(t, func(_ context.Context, _ string, _ string, _ []byte, _ bridgeCommandLimits) ([]byte, []byte, error) {
				called = true
				return nil, nil, nil
			})
			if _, err := b.CalendarExport(context.Background(), tc.request); err == nil {
				t.Fatalf("CalendarExport accepted invalid request: %+v", tc.request)
			}
			if called {
				t.Fatal("runner was called for an invalid calendar-export request")
			}
		})
	}
}

func TestCommandBridge_CalendarExportRejectsInvalidResponses(t *testing.T) {
	tests := []struct {
		name     string
		response string
		wantText string
	}{
		{name: "schema", response: `{"schema_version":2,"account":{"alias":"personal","email":"me@example.com"},"events":[],"next_sync_token":"next"}`, wantText: "schema_version"},
		{name: "wrong account", response: `{"schema_version":1,"account":{"alias":"work-main","email":"me@corp.example"},"events":[],"next_sync_token":"next"}`, wantText: "account"},
		{name: "missing terminal token", response: `{"schema_version":1,"account":{"alias":"personal","email":"me@example.com"},"events":[],"next_sync_token":""}`, wantText: "next_sync_token"},
		{name: "above event limit", response: `{"schema_version":1,"account":{"alias":"personal","email":"me@example.com"},"events":[{"id":"1"},{"id":"2"}],"next_sync_token":"next"}`, wantText: "maximum"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := newProtocolBridge(t, func(_ context.Context, _ string, _ string, _ []byte, _ bridgeCommandLimits) ([]byte, []byte, error) {
				return []byte(tc.response), nil, nil
			})
			_, err := b.CalendarExport(context.Background(), BridgeCalendarExportRequest{
				Account: "personal", TimeMin: "2026-01-01T00:00:00Z", TimeMax: "2026-01-02T00:00:00Z", MaxEvents: 1,
			})
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.wantText) {
				t.Fatalf("CalendarExport error = %v, want text %q", err, tc.wantText)
			}
		})
	}
}

func firstCalendarRaw(events []json.RawMessage) string {
	if len(events) == 0 {
		return "<none>"
	}
	return string(events[0])
}
