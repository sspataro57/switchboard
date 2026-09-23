package capture_test

// REGRESSION — SWT-80 / swb #553, bug `revived-task-not-in-incoming`
// (docs/bugs/revived-task-not-in-incoming_DIAGNOSIS.md). Structural half of the
// order pin, zero I/O: in EvaluateRules' actionTaskLog branch the TARGET's
// markRuleActivity call must come AFTER reviveRuleTask and reopenRuleTask.
// SWT-72 put it before them; task_mark_activity skips a closed task, so the
// mark was a silent no-op for exactly the tasks the revive/reopen brings back
// (production #381, #452). The integration half is
// TestRegression_SWT80_AuditOrder_* (swt80_closed_not_reopened_integration_test.go).

import (
	"strings"
	"testing"
)

func TestRegression_SWT80_TheTargetMarkFollowsTheReviveAndTheReopen(t *testing.T) {
	body := craFuncBody(t, "rules_store.go", "EvaluateRules")
	if body == "" {
		t.Fatalf("internal/capture/rules_store.go declares no EvaluateRules")
	}
	logBranch := strings.Index(body, "case actionTaskLog:")
	if logBranch < 0 {
		t.Fatalf("EvaluateRules has no actionTaskLog branch")
	}
	branch := body[logBranch:]
	mark := craTargetMark(branch)
	if mark < 0 {
		t.Fatalf("the actionTaskLog branch has no markRuleActivity call on *decision.taskID (the attach target)")
	}
	for _, call := range []string{"reviveRuleTask(", "reopenRuleTask("} {
		at := strings.Index(branch, call)
		if at < 0 {
			t.Errorf("the actionTaskLog branch no longer calls %s", call)
			continue
		}
		if mark < at {
			t.Errorf("the target's markRuleActivity runs BEFORE %s. SWT-80: the tool skips a closed task, so a "+
				"mark before the revive/reopen never lands on the task it brings back, and the close already "+
				"stamped reviewed_at — the task sits in QUEUE. Order: log -> revive/reopen -> mark", call)
		}
	}
	if app := strings.Index(branch, "appendRuleLog("); app < 0 || app > mark {
		t.Errorf("appendRuleLog must still come first in the branch (SWT-72 D3 / SWT-45 J9: log first)")
	}
}
