package google_test

// Structural guard for the transport swap's blast radius (pipedream-calendar /
// docs/tickets/pipedream-calendar_SPEC.md, acceptance criteria 20 and the
// "nothing may branch on either" half of 17).
//
// Plain unit test, no build tag, no database — the shape of
// internal/availability/callsites_test.go, which this file deliberately does
// NOT live beside: adding a file under internal/availability would itself
// violate criterion 20 ("no file under internal/availability changes";
// git diff --stat shows none). So the guard lives with the code that could
// break it and scans upward.
//
// THIS IS A PIN, NOT A RED. Like TestNormalizedEventsHasExactlyOneReader it
// passes from day one and its job is to keep passing: the readiness contract is
// INHERITED, not re-implemented. Its teeth are the mutation — add "pipedream"
// or "calendar_source" to internal/availability/store.go and it goes red.
//
// WHY IT MATTERS. LoadBusy is fail-closed on (status='ok', stats->>'phase' =
// 'calendar', freshness) and nothing else. The moment the readiness half learns
// which transport produced a run, "the poller is healthy" stops being one
// question with one answer, and a Pipedream-shaped special case in the refusal
// path is how an outage starts answering propose_slots instead of refusing it.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// availabilityDir is internal/availability, relative to this package.
var availabilityDir = filepath.Join("..", "..", "availability")

// bannedInAvailability are the spellings that would mean the readiness contract
// had learned about this ticket.
var bannedInAvailability = []string{
	"pipedream", "Pipedream", "PIPEDREAM",
	"CAL_SOURCE", "calendar_source", "CalendarSource",
	"calendar_empty_snapshot", "CalendarEmptySnapshot",
}

func TestAvailabilityKnowsNothingAboutTheCalendarTransport(t *testing.T) {
	entries, err := os.ReadDir(availabilityDir)
	if err != nil {
		t.Fatalf("read %s: %v", availabilityDir, err)
	}

	scanned := map[string]bool{}
	for _, e := range entries {
		// Non-test sources only: they are the contract. The test files are
		// covered by the git assertion below, which sees any edit at all.
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(availabilityDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		scanned[e.Name()] = true
		for _, banned := range bannedInAvailability {
			if strings.Contains(string(body), banned) {
				t.Errorf("internal/availability/%s names %q. Criterion 20: no file under "+
					"internal/availability changes, and criterion 17: readiness keys on sync_runs.status "+
					"and stats->>'phase' ALONE. A transport-aware refusal path is how a Pipedream outage "+
					"stops refusing and starts answering propose_slots from data nobody fetched",
					e.Name(), banned)
			}
		}
	}

	// Positive control: without it the scan silently passes if the package
	// moves or the walk root stops matching — the failure mode of every
	// source-scanning test.
	for _, want := range []string{"store.go", "availability.go"} {
		if !scanned[want] {
			t.Errorf("expected to scan internal/availability/%s and did not; the scan has stopped matching "+
				"(check availabilityDir)", want)
		}
	}
}

// The same criterion at the level it is actually written in: git sees no change
// under internal/availability on this branch — with ONE dated exception.
// Two-dot against main deliberately, so an UNCOMMITTED edit counts too.
// Skipped rather than failed where git or the base ref is unavailable, because
// a source tarball is a legitimate place to run go test — the scan above is
// the version that always runs.
//
// REWRITTEN FOR SWT-28 (2026-09-07), the migration-guard convention: the
// original rule was "no file changes at all" (SWT-27 criterion 20 — a
// transport swap must not touch the fail-closed reader). SWT-28's codex
// amendment adds loadReservations to store.go: a SECOND BUSY INPUT (an
// unconfirmed booked delivery reserves its slot), not a readiness change —
// the refusal path, NotReady, the horizon and the one-normalized_events-reader
// rule are untouched, and the transport-agnosticism scan above still holds.
// So the seal narrows: store.go may differ; every OTHER file under
// internal/availability still may not.
func TestGitShowsNoChangeUnderInternalAvailability(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := filepath.Join("..", "..", "..")
	if err := exec.Command("git", "-C", repo, "rev-parse", "--verify", "main").Run(); err != nil {
		t.Skip("no main ref to diff against")
	}
	out, err := exec.Command("git", "-C", repo, "diff", "--name-only", "main", "--", "internal/availability").Output()
	if err != nil {
		t.Skipf("git diff failed: %v", err)
	}
	for _, changed := range strings.Fields(strings.TrimSpace(string(out))) {
		if strings.HasSuffix(changed, "_test.go") {
			// Tests under internal/availability GUARD the readiness contract
			// rather than being it (callsites_test.go is the enforcement, and
			// SWT-29 added calendarsyncstates_test.go). The seal protects the
			// non-test sources; a new assertion file is not a contract change.
			continue
		}
		if changed == "internal/availability/store.go" {
			// SWT-28's loadReservations amendment, and SWT-29's export of the
			// account-states loader as CalendarSyncStates (same body, new
			// name) — both recorded in their SPECs; the readiness rule itself
			// is untouched.
			continue
		}
		if changed == "internal/availability/availability.go" {
			// SWT-29: AccountState's doc comment now names the loader that
			// actually exists (CalendarSyncStates). A comment-only change,
			// demanded by calendarsyncstates_test.go's doc assertion.
			continue
		}
		t.Errorf("this branch changes %s under internal/availability. Only store.go and availability.go "+
			"carry recorded amendments (SWT-28 reservations; SWT-29 export + doc), and _test.go files are "+
			"assertions rather than contract; an edit anywhere else is a change to the fail-closed reader "+
			"nobody has argued for", changed)
	}
}
