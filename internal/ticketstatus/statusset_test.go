package ticketstatus

// The open-status set is spelled in internal/tools/close.go (openStatuses, the
// authority both verbs share) and mirrored here by restorable/activeWork so
// Decide can stay pure. This pin is what binds the copies (go-reviewer F5,
// 2026-09-09): it parses the authority's literal out of close.go and drives
// every tasks.status value through both local predicates, so a drift emits a
// restore status validateReopen would refuse — caught here instead of as an
// aborted pass in production.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestRestorableMatchesTheToolsOpenStatusList(t *testing.T) {
	b, err := os.ReadFile("../tools/close.go")
	if err != nil {
		t.Fatalf("read internal/tools/close.go: %v", err)
	}
	m := regexp.MustCompile(`var openStatuses = \[\]string\{([^}]*)\}`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatalf("internal/tools/close.go no longer declares openStatuses; the authority this pin " +
			"binds against has moved")
	}
	authority := map[string]bool{}
	for _, q := range regexp.MustCompile(`"([a-z_]+)"`).FindAllStringSubmatch(m[1], -1) {
		authority[q[1]] = true
	}
	if len(authority) == 0 {
		t.Fatalf("parsed no statuses out of openStatuses (%q)", strings.TrimSpace(m[1]))
	}

	// Every tasks.status value (0001's CHECK), driven through both predicates.
	all := []string{"holding", "ready", "claimed", "in_progress", "needs_feedback", "pr_open",
		"awaiting_ci", "awaiting_merge", "done_locally", "blocked", "delivered", "closed"}
	for _, st := range all {
		if restorable(st) != authority[st] {
			t.Errorf("restorable(%q) = %v but tools.openStatuses says %v — Decide would emit a restore "+
				"status validateReopen refuses, and the reopen becomes a run-time error mid-pass",
				st, restorable(st), authority[st])
		}
	}
	for _, st := range []string{"claimed", "in_progress", "needs_feedback"} {
		if !activeWork(st) {
			t.Errorf("activeWork(%q) = false; the mirror of task_close's refusal set has drifted", st)
		}
	}
}
