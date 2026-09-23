package promote_test

// REGRESSION — SWT-80 / swb #553, bug `revived-task-not-in-incoming`
// (docs/bugs/revived-task-not-in-incoming_DIAGNOSIS.md). Structural half, zero
// I/O: in act's "attached" branch the activity mark must come AFTER
// reopenDismissed. SWT-72 put it before; the inquiry lane's only attach is onto a
// DISMISSED (closed) task, the tool skips a closed task, so the reopened task
// never reached INCOMING (production #155, promote:inquiry). The integration
// half is TestRegression_SWT80_Promote* (swt80_reopen_activity_integration_test.go).

import (
	"strings"
	"testing"
)

func TestRegression_SWT80_PromoteMarksAfterReopenDismissed(t *testing.T) {
	body := oaFuncBody(t, "store.go", "act")
	if body == "" {
		t.Fatalf("internal/promote/store.go declares no act")
	}
	attached := strings.Index(body, `case "attached":`)
	if attached < 0 {
		t.Fatalf(`act has no case "attached"`)
	}
	branch := body[attached:]
	if end := strings.Index(branch, "default:"); end > 0 {
		branch = branch[:end]
	}
	mark := strings.Index(branch, "markVerdictActivity(")
	reopen := strings.Index(branch, "reopenDismissed(")
	if mark < 0 || reopen < 0 {
		t.Fatalf("the attached branch lacks markVerdictActivity (%d) or reopenDismissed (%d)", mark, reopen)
	}
	if strings.Count(branch, "markVerdictActivity(") != 1 {
		t.Errorf("the attached branch calls markVerdictActivity %d times, want ONE lane-agnostic call site",
			strings.Count(branch, "markVerdictActivity("))
	}
	if mark < reopen {
		t.Errorf("markVerdictActivity runs BEFORE reopenDismissed in act's attached branch. SWT-80: the tool skips " +
			"the still-dismissed task, the reopen then opens it, and it sits in QUEUE (production #155)")
	}
}
