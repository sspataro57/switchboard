package mcpserver

import (
	"strings"
	"testing"
)

// SWT-109 criteria 8, 11 and 27: the calendar tools on both profiles, the
// book_time pin on the user profile only, and the session instructions.
func TestCalendarTools_ProfilesPinsAndInstructions(t *testing.T) {
	calendar := []string{"calendar_list_events", "propose_slots", "book_time", "cancel_reservation", "release_offer_holds"}
	full := NewWithProfile(nil, "w", ProfileFull)
	user := NewWithProfile(nil, "manual:salvo", ProfileUser)
	for _, n := range calendar {
		if !full.allowed[n] || !user.allowed[n] {
			t.Errorf("%s must be listed on both profiles (full=%v user=%v)", n, full.allowed[n], user.allowed[n])
		}
	}
	for _, n := range []string{"push_calendar_reservations", "apply_hold_releases"} {
		if agentToolNames[n] {
			t.Errorf("%s is the push stage's verb; it must be off every MCP profile", n)
		}
	}
	if got := userProfilePins["book_time"]["require_assignee_type"]; got != "human" {
		t.Errorf("user-profile book_time pin = %q, want human (criterion 11)", got)
	}
	if full.pins != nil {
		t.Errorf("the full profile carries pins: %v", full.pins)
	}
	for _, tl := range agentTools {
		if tl.Name == "book_time" && strings.Contains(string(tl.InputSchema), "account") {
			t.Errorf("book_time's schema mentions an account; the calendar comes from the task's project only")
		}
		if tl.Name == "book_time" && strings.Contains(string(tl.InputSchema), "require_assignee_type") {
			t.Errorf("the pin must not appear in a schema")
		}
	}
	for _, want := range []string{"calendar_list_events", "book_time", "cancel_reservation", "propose_slots",
		"offered_windows", "release_offer_holds", "keep_start", "saved, not yet on Google"} {
		if !strings.Contains(Instructions, want) {
			t.Errorf("Instructions do not mention %q (criterion 27)", want)
		}
	}
}
