package orchestrator_test

// comms-inbox (SWT-74, docs/tickets/comms-inbox_SPEC.md) D9 and criterion 40's
// structural half, invariant 7: no orchestrator rule learns anything about this
// ticket. The columns are capture's own log and the board's; the verbs are
// human ones; the spine's rules stay functions of (event, task, policy).
//
// ZERO I/O beyond reading this repo's own source. The behavioural half is the
// new `attached` row in rules_test.go's "must fire nothing" table.
//
// WHY THE NEW EVENT IS SAFE WITHOUT A RULE (D9): `attached` falls into
// Evaluate's nil default (rules.go fires only on status_changed with
// to ∈ {delivered, closed}), and task_attach's close goes through
// closeTransition, whose status_changed {to:"closed"} is the event R1/R2/R8
// already handle for every close in the system. So the lifecycle gains no new
// path at all.
//
// GREEN TODAY BY DESIGN, AND REQUIRED TO STAY GREEN — the mutation it exists to
// catch is a later session "helpfully" teaching a rule to route a comm or to
// read capture's arming flag, which would put a spine decision in front of the
// human judgement the whole ticket is about.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOrchestrator_LearnsNothingAboutComms(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/orchestrator: %v", err)
	}
	scanned := 0
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
		for _, banned := range []struct{ tok, why string }{
			{"comm_task", "D9: no orchestrator rule reads capture's per-rule arming flag — which rule is armed " +
				"for comms is routing DATA, and the spine never reads it"},
			{"comm_task_id", "D9: capture_decisions is capture's own log; the spine has never read it"},
			{"task_attach", "D7: Attach is humanOnly — the orchestrator is REFUSED by policy, so a call site " +
				"here would be a rule that dies on every tick"},
			{"task_match", "D6: proposing where a comm belongs is a question a human asks; a rule that asked it " +
				"would be an LLM-free spine decision standing in for his judgement"},
		} {
			if strings.Contains(src, banned.tok) {
				t.Errorf("internal/orchestrator/%s mentions %s — %s (criterion 40, invariant 7)",
					name, banned.tok, banned.why)
			}
		}
	}
	if scanned == 0 {
		t.Fatalf("POSITIVE CONTROL FAILED: no non-test .go file scanned in internal/orchestrator")
	}
}
