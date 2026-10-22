package policy

import "testing"

// SWT-109 criterion 18: the calendar_push stage's two verbs are sendShaped and
// freezeGated with their channel pinned to calendar by name. The kill switch
// stops them; booking (book_time) is not send-shaped and keeps working.
func TestCalendarPushVerbsArePinnedSendShapedAndFrozen(t *testing.T) {
	for _, tool := range []string{"push_calendar_reservations", "apply_hold_releases"} {
		if toolChannel[tool] != "calendar" {
			t.Errorf("%s channel pin = %q, want calendar", tool, toolChannel[tool])
		}
		if !sendShaped[tool] || !freezeGated[tool] {
			t.Errorf("%s must be sendShaped and freezeGated", tool)
		}
		req := Request{Tool: tool, Actor: "pipeline:calendar_push"}
		if d := Decide(req, Snapshot{Channel: "calendar", SendingFrozen: true}); d.Decision != "deny" || d.Rule != "kill_switch" {
			t.Errorf("%s under the kill switch: %+v, want deny kill_switch", tool, d)
		}
		if d := Decide(req, Snapshot{Channel: "calendar"}); d.Decision != "allow" {
			t.Errorf("%s with the switch off: %+v, want allow", tool, d)
		}
		if d := Decide(req, Snapshot{Channel: "calendar", SentLastHour: map[string]int{"calendar": 10}, HourlyLimit: 10}); d.Decision != "deny" || d.Rule != "rate_limit" {
			t.Errorf("%s over the hourly limit: %+v, want deny rate_limit", tool, d)
		}
	}
	for _, tool := range []string{"book_time", "cancel_reservation", "release_offer_holds", "calendar_list_events"} {
		if sendShaped[tool] || freezeGated[tool] || humanOnly[tool] {
			t.Errorf("%s is DB-only and agent-callable: not sendShaped, freezeGated or humanOnly", tool)
		}
		if d := Decide(Request{Tool: tool, Actor: "mcp:manual:salvo"}, Snapshot{SendingFrozen: true}); d.Decision != "allow" {
			t.Errorf("%s under the kill switch: %+v, want allow (the switch stops pushes, not booking)", tool, d)
		}
	}
}
