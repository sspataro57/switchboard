package dashboard

// SWT-40 Part B criterion B9's /funnel half (docs/tickets/inquiry-promote_SPEC.md):
// "/funnel reads the lane through the lane loop" — the route lane is one more
// entry in the classify shadow summary's lane list, read through
// classify.Summarize like the other three, never a second fold.
//
// GREENFIELD NOTE — EXPECTED RED: the loop lists three lanes.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestFunnel_LaneLoopIncludesTheRouteLane(t *testing.T) {
	b, err := os.ReadFile("funnel.go")
	if err != nil {
		t.Fatalf("read funnel.go: %v", err)
	}
	loops := regexp.MustCompile(`\[\]classify\.Lane\{([^}]*)\}`).FindAllStringSubmatch(string(b), -1)
	if len(loops) == 0 {
		t.Fatalf("POSITIVE CONTROL FAILED: funnel.go has no []classify.Lane{…} loop")
	}
	found := false
	for _, m := range loops {
		if strings.Contains(m[1], "classify.LanePersonal") && strings.Contains(m[1], "classify.LaneRoute") {
			found = true
		}
	}
	if !found {
		t.Errorf("funnel.go's lane loop does not include classify.LaneRoute (B9): %v", loops)
	}
}
