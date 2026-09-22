package orchestrator_test

// activity-resurfaces (SWT-72, docs/tickets/activity-resurfaces_SPEC.md) D9 and
// criterion 25's structural half, invariant 7: no orchestrator rule learns
// anything about this ticket. The columns are board-facing and review-facing;
// the spine's rules stay functions of (event, task, policy).
//
// ZERO I/O beyond reading this repo's own source. The behavioural half is the
// two new rows in rules_test.go's "must fire nothing" table.
//
// GREEN TODAY BY DESIGN, AND REQUIRED TO STAY GREEN — the mutation it exists to
// catch is a later session "helpfully" teaching a rule to requeue a task or to
// read the review flag, which would put an LLM-free spine decision in front of
// a human judgement the whole ticket is about.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOrchestrator_LearnsNothingAboutActivityOrReview(t *testing.T) {
	dir := "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read internal/orchestrator: %v", err)
	}
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src := string(b)
		scanned++
		for _, banned := range []struct{ tok, why string }{
			{"activity_at", "D9: no rule reads the activity columns — INCOMING is a render-time filter, not a lifecycle state"},
			{"activity_by_message_id", "D9: same"},
			{"reviewed_at", "D9: a review is a human judgement; nothing in the spine infers it"},
			{"task_requeue", "D6: Requeue is humanOnly — the orchestrator is REFUSED by policy, so a call site here " +
				"would be a rule that dies on every tick"},
			{"task_mark_activity", "D3: capture and promote call it; the orchestrator never does"},
		} {
			if strings.Contains(src, banned.tok) {
				t.Errorf("internal/orchestrator/%s mentions %s — %s (criterion 25, invariant 7)", name, banned.tok, banned.why)
			}
		}
	}
	if scanned == 0 {
		t.Fatalf("POSITIVE CONTROL FAILED: no non-test .go file scanned in internal/orchestrator")
	}
}
