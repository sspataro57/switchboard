package classify_test

// SWT-40 Part B structural guards for internal/classify
// (docs/tickets/inquiry-promote_SPEC.md, B-D5, B-D6, B-D9 and the data-model
// section). ZERO I/O beyond reading this repo's own source.
//
//   - The ledger learns 0032 (the SPEC's 0029_route_tier.sql, renumbered because
//     0029-0031 were taken). The ledger's predicate and ownership note live in
//     structure_test.go; this test keeps it from being edited back out.
//   - B-D9's carve-out is the amended TestClassifyPackage_FetchesNothingAndDecodesNoMIME
//     in structure_test.go.
//   - B-D5/B-D6: the route LANE only records verdicts. The applied decision is
//     a capture_decisions row that internal/capture/route.go writes; this
//     package never writes capture_decisions.

import (
	"regexp"
	"strings"
	"testing"
)

func TestMigrationLedger_Learns0032(t *testing.T) {
	const rel = "internal/classify/structure_test.go"
	src := csRepoFile(t, rel)
	i := strings.Index(src, "THIS LEDGER IS THE LIVING REGISTRY")
	if i < 0 {
		t.Fatalf("%s no longer carries the migration ledger; rewrite the guard, never delete it", rel)
	}
	start := i - 3500
	if start < 0 {
		start = 0
	}
	end := i + 1800
	if end > len(src) {
		end = len(src)
	}
	ledger := src[start:end]
	if !regexp.MustCompile(`n\s*!=\s*32\b`).MatchString(ledger) {
		t.Errorf("the ledger in %s does not account for migration 0032. SWT-40 Part B owns 0032_route_tier.sql "+
			"(the SPEC's 0029, renumbered: 0029 Part D, 0030 SWT-45, 0031 Part C)", rel)
	}
	// The ownership note sits ABOVE the marker, naming the owner and the rename.
	above := src[start:i]
	if !strings.Contains(above, "32 is SWT-40 Part B") || !strings.Contains(above, "0029_route_tier") {
		t.Errorf("the ledger's ownership note above the marker does not say 32 is SWT-40 Part B's, renumbered " +
			"from the SPEC's 0029_route_tier.sql. The registry's value is the OWNERSHIP, not the number")
	}
}

func TestClassifyPackage_NeverWritesCaptureDecisions(t *testing.T) {
	write := regexp.MustCompile(`(?is)(insert\s+into|update|delete\s+from)\s+capture_decisions\b`)
	for _, rel := range csSources(t, "internal/classify") {
		if m := write.FindString(csGoCode(t, rel)); m != "" {
			t.Errorf("%s contains %q. B-D5/B-D6: the route lane records a VERDICT (ai_runs + ai_extractions); "+
				"the applied decision is a mode='route' capture_decisions row written by internal/capture/route.go "+
				"under capture's lock, never by the classifier", rel, m)
		}
	}
}
