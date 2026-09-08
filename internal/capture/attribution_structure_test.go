package capture

// SWT-29 (docs/tickets/funnel-view_SPEC.md, criteria 14, 15 and 16): the
// capture-attribution section of /funnel reads through capture's OWN spelling of
// "the latest decision for this message", exported as AttributionTrend.
//
// Structural half — ZERO I/O beyond reading this package's source. The numbers
// are pinned in attribution_integration_test.go, where Postgres produces them.
// This file references no symbol that does not exist yet, so it fails on
// ASSERTIONS rather than on a compile error.
//
// GREENFIELD NOTE — EXPECTED RED: internal/capture/attribution.go does not
// exist.

import (
	"path/filepath"
	"strings"
	"testing"
)

// The three facts the query shape encodes, each with the reason it is not
// negotiable. All three have bitten this repo in some costume.
func TestAttributionTrend_KeepsTheThreeStatesAndTheInboundFilter(t *testing.T) {
	src := mustReadRepoFile(t, filepath.Join("internal", "capture", "attribution.go"))
	if len(src) < 200 {
		t.Fatalf("internal/capture/attribution.go is %d bytes; the scans below would pass vacuously", len(src))
	}

	for _, want := range []string{"func AttributionTrend(", "type DayAttribution struct"} {
		if !strings.Contains(src, want) {
			t.Errorf("internal/capture/attribution.go has no %q. Criterion 16: the latest-decision predicate "+
				"is capture's spelling, exported from this package; the dashboard adds no SQL that picks a "+
				"message's newest capture_decisions row", want)
		}
	}

	// 1. The LEFT join is what makes "not yet evaluated" a THIRD state rather
	//    than a silent merge. No decision row = UNSEEN (the engine has not
	//    looked); latest decision 'unmatched' = the inbox; latest decision
	//    routed = never re-triage. Conflating the first two is the named
	//    capture trap (IK, capture rules contract).
	if !strings.Contains(src, "LEFT JOIN LATERAL") {
		t.Errorf("attribution.go has no LEFT JOIN LATERAL. Criterion 14: THREE states, never two. An inner " +
			"join drops every message with no decision row, and the 'not yet evaluated' column — the one " +
			"the section exists to expose — silently becomes zero")
	}

	// 2. The lateral is ordered by id DESC, the same "latest" latestDecisions
	//    and triage's project lookup use. The two must not drift.
	if !strings.Contains(src, "cd.id DESC") {
		t.Errorf("attribution.go's lateral does not order by cd.id DESC. Shadow is re-runnable by design and " +
			"writes a decision per pass; anything other than the newest row makes the numbers depend on how " +
			"many times someone ran the pass")
	}

	// 3. direction='inbound' IS invariant 5 here. capture/rules_store.go filters
	//    it, so an OUTBOUND message can never carry a decision, on any pass, in
	//    any mode — 21,194 outbound messages, ZERO decisions, measured. Its
	//    absence is absent-because-impossible, and counting it as "not yet
	//    evaluated" would be a lie about 21k rows.
	if !strings.Contains(src, "direction = 'inbound'") && !strings.Contains(src, "direction='inbound'") {
		t.Errorf("attribution.go does not filter direction='inbound' (criterion 15). Outbound rows are our " +
			"own sends re-entering through ingestion; capture only ever decides inbound, so an outbound " +
			"row's missing decision means 'not applicable', never 'pending'")
	}

	// The action fold is POSITIVELY spelled. 0015's CHECK makes
	// (action='unmatched') = (project_id IS NULL) a schema fact, so keying on
	// project_id IS NULL would be a second spelling of the same constraint — and
	// the one that silently stops meaning what it says if the CHECK ever moves.
	if strings.Contains(src, "project_id IS NULL") {
		t.Errorf("attribution.go keys the fold on project_id IS NULL. Key on `action`, positively spelled: " +
			"0015's CHECK ((action='unmatched') = (project_id IS NULL)) is why the two agree today, and a " +
			"predicate that reads a column to infer another column's value is the constant-discriminator " +
			"landmine waiting for the CHECK to change")
	}
}
