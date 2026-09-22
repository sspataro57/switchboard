package ticketstatus_test

// activity-resurfaces (SWT-72, docs/tickets/activity-resurfaces_SPEC.md) D4 and
// criterion 39's first half: the Jira reconciler is UNTOUCHED. Its
// Observation.SurfacedAt still comes from tasks.surfaced_at alone, and no file
// in internal/ticketstatus learns the word activity_at or reviewed_at.
//
// D1's whole argument rests on this. The brief recommended reusing surfaced_at;
// decide.go says that breaks the reconciler on exactly this population — Jira
// mails on every close (IK SWT-45 J10), that mail is captured, matches the same
// rule and lands as a task_log on the still-open task, so reuse would leave
// EVERY closed Treetop ticket's task stuck on the board. The behavioural proof
// is criterion 26 (activity_noregression_integration_test.go); this is the
// structural one.
//
// ZERO I/O beyond reading this repo. revive_structure_test.go's neighbour;
// reuses tsRepoFile from structure_test.go.
//
// GREEN TODAY BY DESIGN, AND REQUIRED TO STAY GREEN.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTicketStatus_LearnsNeitherActivityNorReview(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/ticketstatus: %v", err)
	}
	scanned, sawSurfaced := 0, false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src := string(b)
		scanned++
		if strings.Contains(src, "surfaced_at") {
			sawSurfaced = true
		}
		for _, banned := range []struct{ tok, why string }{
			{"activity_at", "D4: the reconciler never learns the board-facing column. Reading it would hold every " +
				"done ticket's task open forever — the exact failure D1 refused"},
			{"reviewed_at", "D4: a review is the board's, not the reconciler's"},
			{"task_requeue", "D4: the reconciler closes and reopens; it never requeues"},
			{"task_mark_activity", "D3: capture and promote call it"},
		} {
			if strings.Contains(src, banned.tok) {
				t.Errorf("internal/ticketstatus/%s mentions %s — %s (criteria 39, D4)", name, banned.tok, banned.why)
			}
		}
	}
	if scanned == 0 {
		t.Fatalf("POSITIVE CONTROL FAILED: no non-test .go file scanned in internal/ticketstatus")
	}
	// CONTROL: the package DOES still read surfaced_at — otherwise the scan
	// above would pass on a package that had lost SWT-45's hold entirely.
	if !sawSurfaced {
		t.Fatalf("POSITIVE CONTROL FAILED: no file in internal/ticketstatus mentions surfaced_at; SWT-45's " +
			"resurfaced hold is gone, and this scan proves nothing about D4")
	}
	// The one place the hold is decided still reads the SWT-45 column and only it.
	decide := tsRepoFile(t, "internal/ticketstatus/decide.go")
	if !strings.Contains(decide, "SurfacedAt") {
		t.Errorf("decide.go no longer mentions Observation.SurfacedAt; D4: newSurfacing still comes from " +
			"surfaced_at alone")
	}
}
