package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/availability"
)

func nyLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	return loc
}

// criterion 1: default window, and the three refusals.
func TestAgendaWindow(t *testing.T) {
	loc := nyLoc(t)
	now := time.Date(2026, 10, 21, 15, 4, 0, 0, time.UTC) // 11:04 in New York
	s, e, err := agendaWindow(calendarListEventsArgs{}, now, loc)
	if err != nil {
		t.Fatalf("default window: %v", err)
	}
	if got := s.In(loc).Format(time.RFC3339); got != "2026-10-21T00:00:00-04:00" {
		t.Errorf("default start = %s, want today 00:00 New York", got)
	}
	if e.Sub(s) != 7*24*time.Hour {
		t.Errorf("default length = %s, want 7 days", e.Sub(s))
	}

	s, _, err = agendaWindow(calendarListEventsArgs{WindowStart: "2026-10-27", WindowEnd: "2026-10-28"}, now, loc)
	if err != nil {
		t.Fatalf("date window: %v", err)
	}
	if got := s.Format(time.RFC3339); got != "2026-10-27T00:00:00-04:00" {
		t.Errorf("YYYY-MM-DD start = %s, want local midnight", got)
	}

	for name, a := range map[string]calendarListEventsArgs{
		"empty":    {WindowStart: "2026-10-27T10:00:00Z", WindowEnd: "2026-10-27T10:00:00Z"},
		"inverted": {WindowStart: "2026-10-28T10:00:00Z", WindowEnd: "2026-10-27T10:00:00Z"},
		"too long": {WindowStart: "2026-10-01", WindowEnd: "2026-11-02"},
	} {
		if _, _, err := agendaWindow(a, now, loc); err == nil {
			t.Errorf("%s window was accepted; criterion 1 refuses it", name)
		}
	}
	if err := validateCalendarListEvents([]byte(`{"window_start":"next tuesday"}`)); err == nil {
		t.Errorf("a non-date window_start validated")
	}
}

// criteria 4 and 5: every timestamp in AVAIL_TZ, all-day as plain dates,
// transparent events not busy, a pending reservation shown with its reason,
// and a confirmed reservation shown ONCE as the Google event.
func TestRenderAgenda(t *testing.T) {
	loc := nyLoc(t)
	now := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	standup := time.Date(2026, 10, 27, 19, 0, 0, 0, time.UTC) // 15:00 New York
	exp := now.Add(72 * time.Hour)
	ag := availability.Agenda{
		Accounts: []availability.AgendaAccount{{
			AccountState: availability.AccountState{AccountID: 1, Email: "salvador@handsonconnect.org", LastCalendarSync: now.Add(-4 * time.Hour)},
		}},
		Events: []availability.AgendaEvent{
			{Title: "SE/Collab Weekly Standup", Start: standup, End: standup.Add(30 * time.Minute),
				Account: "salvador@handsonconnect.org", Transparency: "opaque", ExternalID: "calendar:abc"},
			{Title: "Home", AllDay: true, Start: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC),
				End: time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC), Account: "sspataro@gmail.com",
				Transparency: "transparent", ExternalID: "calendar:home"},
			{Title: "Busy", Start: standup.Add(2 * time.Hour), End: standup.Add(3 * time.Hour),
				Account: "sspataro@gmail.com", Transparency: "opaque", ExternalID: "calendar:sb1t2"},
		},
		Reservations: []availability.Reservation{{
			DeliveryID: 9, TaskID: 5, Start: standup.Add(-2 * time.Hour), End: standup.Add(-time.Hour),
			Account: "salvador@handsonconnect.org", Status: "approved", Label: "Prep for Grady",
			PendingReason: "account_not_write_enabled", Verified: true, HoldFor: 236, HoldExpiresAt: &exp,
		}},
	}
	confirmed := map[string]agendaReservationInfo{"calendar:sb1t2": {
		Reservation: availability.Reservation{DeliveryID: 7, TaskID: 3, Label: "Focus block", ExternalID: "calendar:sb1t2", Verified: true},
		Confirmed:   true,
	}}
	out := renderAgenda(ag, confirmed, loc, now, now.Add(7*24*time.Hour), now, 24*time.Hour, false)

	if out["timezone"] != "America/New_York" {
		t.Errorf("timezone = %v", out["timezone"])
	}
	events := out["events"].([]map[string]any)
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4 (3 Google + 1 pending reservation; the confirmed one once)", len(events))
	}
	// Ordered by start: the pending reservation (13:00), standup (15:00), busy block (17:00), then Wed all-day.
	if events[0]["reservation"] == nil || events[0]["on_google"] != false {
		t.Fatalf("first row should be the pending reservation: %v", events[0])
	}
	res := events[0]["reservation"].(map[string]any)
	if res["state"] != "pending" || res["pending_reason"] != "account_not_write_enabled" || res["kind"] != "hold" {
		t.Errorf("pending reservation object = %v", res)
	}
	if events[1]["start"] != "2026-10-27T15:00:00-04:00" {
		t.Errorf("standup start = %v, want 15:00 New York (the delivery 236 incident)", events[1]["start"])
	}
	if events[2]["reservation"] == nil || events[2]["reservation"].(map[string]any)["state"] != "confirmed" {
		t.Errorf("the observed reservation must ride on its Google event: %v", events[2])
	}
	if events[3]["start"] != "2026-10-28" || events[3]["end"] != "2026-10-29" || events[3]["busy"] != false {
		t.Errorf("all-day transparent event rendered as %v", events[3])
	}
	for _, e := range events {
		if s, ok := e["start"].(string); ok && strings.HasSuffix(s, "Z") {
			t.Errorf("timestamp %s is in UTC, not AVAIL_TZ", s)
		}
	}

	frozenOut := renderAgenda(availability.Agenda{Reservations: []availability.Reservation{{
		DeliveryID: 1, Start: now, End: now.Add(time.Hour), Status: "approved"}}},
		nil, loc, now, now.Add(time.Hour), now, time.Hour, true)
	r := frozenOut["events"].([]map[string]any)[0]["reservation"].(map[string]any)
	if r["pending_reason"] != "sending_frozen" {
		t.Errorf("a pending reservation under the kill switch should read sending_frozen, got %v", r["pending_reason"])
	}
}

// criterion 7: a bad AVAIL_TZ is an error, never a silent UTC.
func TestAvailabilityConfigRefusesBadTZ(t *testing.T) {
	t.Setenv("AVAIL_TZ", "America/Miam")
	if _, _, err := availabilityConfig(); err == nil {
		t.Fatal("a misspelled AVAIL_TZ was accepted")
	}
	t.Setenv("AVAIL_TZ", "")
	cfg, _, err := availabilityConfig()
	if err != nil || cfg.Location.String() != "America/New_York" {
		t.Errorf("default tz = %v, %v; want America/New_York", cfg.Location, err)
	}
}
