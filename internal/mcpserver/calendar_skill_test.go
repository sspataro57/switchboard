package mcpserver

import (
	"os"
	"strings"
	"testing"
)

// SWT-109 criterion 28: the calendar skill lives in the repo, uses only the
// MCP verbs, retires the .ics script with no fallback, and make install-skill
// installs it.
func TestCalendarSkill(t *testing.T) {
	b, err := os.ReadFile("../../skills/calendar/SKILL.md")
	if err != nil {
		t.Fatalf("skills/calendar/SKILL.md: %v", err)
	}
	s := string(b)
	for _, want := range []string{"calendar_list_events", "propose_slots", "book_time", "cancel_reservation",
		"release_offer_holds", "offered_windows", "no fallback", "saved, not yet on Google"} {
		if !strings.Contains(s, want) {
			t.Errorf("the skill does not mention %q", want)
		}
	}
	if strings.Contains(s, "python3 ~/.claude/calendar-agenda.py") {
		t.Errorf("the skill still tells sessions to run the .ics script")
	}
	mk, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mk), "skills/calendar/SKILL.md ~/.claude/skills/calendar/SKILL.md") {
		t.Errorf("make install-skill does not install the calendar skill")
	}
}
