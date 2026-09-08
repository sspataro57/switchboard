package availability_test

// SWT-29 (docs/tickets/funnel-view_SPEC.md, "API / MCP tool changes" item 4):
// loadAccountStates becomes the EXPORTED CalendarSyncStates so the /funnel page
// can ask the same question propose_slots asks, with the same SQL.
//
// Plain unit test — no build tag, no database, no network. It is a signature
// pin plus a source scan, in the shape of callsites_test.go beside it.
//
// WHY THIS IS SAFE, and why it is NOT the door SWT-24 closed. The banned
// primitive is loadEvents: it reads normalized_events with no readiness check,
// so a caller holding it can be told "you are free all week" by an empty table.
// CalendarSyncStates reads sync_runs only, produces no free/busy answer, and
// carries no risk of that fail-open — the worst a misuse can do is print a
// freshness verdict. TestAvailability_ExportsNoUncheckedEventLoader bans exactly
// one spelling, `func LoadEvents(`, which this is not; it must stay green.
//
// GREENFIELD NOTE — EXPECTED RED. availability.CalendarSyncStates does not
// exist, so this file compile-FAILS the internal/availability test binary until
// the loader is exported.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/availability"
)

// The signature the dashboard is written against. A compile-time pin rather
// than a reflection test: what is being asserted is an exported spelling.
var _ func(context.Context, *pgxpool.Pool) ([]availability.AccountState, error) = availability.CalendarSyncStates

// One body, not two. "Keep loadAccountStates as a one-line internal alias OR
// update its call site" is the implementer's choice; what is NOT a choice is
// two copies of the SQL, because the whole reason /funnel calls this is that a
// second spelling of the readiness scope is how the page comes to say green
// while propose_slots refuses.
func TestCalendarSyncStates_IsTheOnlyCopyOfTheReadinessScopeSQL(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	joined := ""
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		joined += "\n" + string(b)
	}
	if len(joined) < 500 {
		t.Fatalf("no non-test sources found in internal/availability; the scan is vacuous")
	}

	if !strings.Contains(joined, "func CalendarSyncStates(") {
		t.Errorf("internal/availability does not export CalendarSyncStates. SPEC item 4: the account-states " +
			"loader is exported so the funnel page reads the SAME rows propose_slots reads")
	}
	// The two discriminators that define the availability scope, and the phase
	// predicate. Each must appear ONCE across the package's non-test sources.
	for _, needle := range []string{`r.stats->>'phase' = 'calendar'`, "calendar_in_availability"} {
		if n := strings.Count(joined, needle); n == 0 {
			t.Errorf("internal/availability no longer contains %q; the readiness scope has been re-spelled "+
				"and this scan has stopped matching", needle)
		}
	}
	if n := strings.Count(joined, `r.stats->>'phase' = 'calendar'`); n > 1 {
		t.Errorf("the calendar-phase predicate appears %d times in internal/availability, want 1. Exporting "+
			"the loader must MOVE the body, not copy it", n)
	}
	// Control: the one door is untouched.
	if !strings.Contains(joined, "func LoadBusy(") {
		t.Fatalf("POSITIVE CONTROL FAILED: internal/availability has no LoadBusy; this ticket does not " +
			"touch the free/busy entry point and the scan above is measuring the wrong package")
	}
	if strings.Contains(joined, "func LoadEvents(") {
		t.Fatalf("internal/availability exports LoadEvents again. SWT-24 criterion 1 stands: loadEvents " +
			"performs no readiness check and LoadBusy is the only door. Exporting the SYNC-STATE loader " +
			"is not licence to export the EVENT loader")
	}
}

// The doc comment on AccountState has claimed since SWT-24 that "the SQL that
// produces these lives in store.go (LoadAccountStates)" — naming a function
// that does not exist. This ticket makes it true, and a comment that names a
// real symbol is the difference between a boundary file's comment being trusted
// and being a defect (IK: "a comment can be a defect").
func TestAccountStateDocNamesTheRealLoader(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("availability.go"))
	if err != nil {
		t.Fatalf("read availability.go: %v", err)
	}
	src := string(b)
	i := strings.Index(src, "type AccountState struct")
	if i < 0 {
		t.Fatalf("POSITIVE CONTROL FAILED: availability.go has no AccountState struct")
	}
	doc := src[max(0, i-600):i]
	if strings.Contains(doc, "LoadAccountStates") {
		t.Errorf("AccountState's doc comment still points at LoadAccountStates, which is not a symbol in " +
			"this package. Point it at CalendarSyncStates")
	}
	if !strings.Contains(doc, "CalendarSyncStates") {
		t.Errorf("AccountState's doc comment does not name CalendarSyncStates. The doc is where the next " +
			"reader looks for the SQL that produces these rows")
	}
}
