package tools_test

// Structural pin for the horizon wiring (SWT-24 /
// docs/tickets/calendar-availability_SPEC.md, acceptance criterion 7). Plain
// unit test — no build tag, no database, no env.
//
// THE RULE: propose_slots refuses a window not fully inside
// [now - google.CalendarWindowPast, now + google.CalendarWindowFuture], and it
// references THOSE constants. A re-spelled `90 * 24 * time.Hour` in this package
// is a second source of truth for a number that lives in the connector's sync
// recipe: change the fetched window there and the refusal silently keeps
// certifying a span nothing fetches any more, which is the SAME fail-open the
// ticket removes (an empty busy set that means "we never looked"). The repo has
// paid for a duplicated spelling four times — the whitespace normalization, the
// upwork thread key, the capture-rules horizon, the external_refs system list.
//
// GREENFIELD NOTE: this fails today because internal/tools/proposeslots.go
// neither imports the connector nor names the constants — it has no horizon at
// all, which is exactly criterion 7's bug.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestProposeSlots_UsesTheConnectorsWindowConstants(t *testing.T) {
	src, err := os.ReadFile("proposeslots.go")
	if err != nil {
		t.Fatalf("read proposeslots.go: %v", err)
	}
	body := string(src)

	for _, want := range []string{"google.CalendarWindowPast", "google.CalendarWindowFuture"} {
		if !strings.Contains(body, want) {
			t.Errorf("proposeslots.go does not reference %s. The synced horizon is the connector's constant "+
				"(ingest.go's calendar window); the availability wiring passes it in rather than re-spelling "+
				"it, so the refusal can never certify a span the poller stopped fetching (criterion 7)", want)
		}
	}

	// The other half of the rule: no literal 30-day/90-day durations open-coded
	// here. Matching the shape `30 * 24 * time.Hour` (and its unspaced forms)
	// rather than any number, so unrelated arithmetic is not flagged.
	respelled := regexp.MustCompile(`(30|90)\s*\*\s*24\s*\*\s*time\.Hour`)
	if m := respelled.FindAllString(body, -1); len(m) > 0 {
		t.Errorf("proposeslots.go re-spells the calendar window as %v; use google.CalendarWindowPast / "+
			"google.CalendarWindowFuture so there is one definition of what was actually synced", m)
	}
}
