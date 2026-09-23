package capture_test

// activity-resurfaces (SWT-72, docs/tickets/activity-resurfaces_SPEC.md) D7 and
// criteria 8 and 10's structural halves: the capture hook is CHANNEL-BLIND,
// RULE-BLIND and ASSIGNEE-BLIND, and it excludes exactly one case — SWT-54's
// merged/closed PR notice.
//
// "Neither new function contains a channel or rule-kind literal" (criterion
// 10). Shape 2 is why: José writes directly from jose.g@avviato.com with no
// Jira in the path, and a body_regex rule on a WEB-NNNNN key files it onto the
// ticket's task. A hook that keyed on `jira` would leave exactly his messages
// in the black hole.
//
// ZERO I/O beyond reading this repo's own source.
//
// GREENFIELD NOTE, EXPECTED RED: rules_store.go declares no markRuleActivity,
// so both tests fail by name.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - key the capture hook on the channel or the rule kind -> ChannelAndRuleBlind.
//   - remove the prClose exclusion -> ThePRCloseExclusionIsSpelledOnce.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

func craFuncBody(t *testing.T, rel, name string) string {
	t.Helper()
	b, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, b, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != name || fd.Body == nil {
			continue
		}
		return string(b[fset.Position(fd.Pos()).Offset:fset.Position(fd.End()).Offset])
	}
	return ""
}

func TestCaptureActivity_ChannelAndRuleBlind(t *testing.T) {
	body := craFuncBody(t, "rules_store.go", "markRuleActivity")
	if body == "" {
		t.Fatalf("internal/capture/rules_store.go declares no markRuleActivity. Criterion 8: in EvaluateRules' " +
			"actionTaskLog branch, live mode only, after appendRuleLog succeeds and before the " +
			"prClose/revive/reopen branch, it calls task_mark_activity {task_id, message_id: pm.msg.ID, " +
			"reason:\"capture: …\"} as cfg.Actor")
	}
	if !strings.Contains(body, `"task_mark_activity"`) {
		t.Errorf("markRuleActivity does not call the task_mark_activity tool (criterion 8, invariant 3: no " +
			"direct write of tasks.activity_*)")
	}
	for _, banned := range []struct{ tok, why string }{
		{`"jira"`, "criterion 10: shape 2 is José's DIRECT mail — no Jira in the path"},
		{`"gmail"`, "criterion 10: shape 1 is a Jira comment and shape 4 is Slack"},
		{`"slack"`, "criterion 10: channel-blind"},
		{`"upwork"`, "criterion 10: channel-blind"},
		{`"github"`, "criterion 10: channel-blind"},
		{"pm.channel", "criterion 10: the hook never reads the channel at all"},
		{`"body_regex"`, "criterion 10: rule-blind — a sender rule, a body_regex rule and a thread_key_prefix " +
			"rule all converge on the same task_log branch"},
		{`"sender"`, "criterion 10: rule-blind"},
		{`"thread_key_prefix"`, "criterion 10: rule-blind"},
		{"criteriaType", "criterion 10: rule-blind"},
		{`"human"`, "D7 / OQ-2 = A: assignee-blind — a comment on a claude task in flight surfaces it too"},
		{"assignee", "D7 / OQ-2 = A: assignee-blind"},
	} {
		if strings.Contains(body, banned.tok) {
			t.Errorf("markRuleActivity contains %s — %s", banned.tok, banned.why)
		}
	}
}

// D3's ONE deliberate exclusion: capture skips the mark when decision.prClose
// is set (SWT-54's merged/closed PR notice). The next call closes the task, and
// surfacing a row in order to close it one statement later is noise.
func TestCaptureActivity_ThePRCloseExclusionIsSpelledOnce(t *testing.T) {
	body := craFuncBody(t, "rules_store.go", "EvaluateRules")
	if body == "" {
		t.Fatalf("internal/capture/rules_store.go declares no EvaluateRules")
	}
	// AMENDED 2026-09-22 (SWT-72 follow-up, swb #491): the create path
	// (actionTask) marks the new task too, so the call this test is about is
	// the one INSIDE the actionTaskLog branch — look there, not at the first
	// occurrence in the function.
	logBranch := strings.Index(body, "case actionTaskLog:")
	if logBranch < 0 {
		t.Fatalf("EvaluateRules has no actionTaskLog branch")
	}
	// AMENDED by SWT-80 (revived-task-not-in-incoming): the TARGET's mark is the
	// call on *decision.taskID — the comm task's mark (SWT-74, on commID) sits in
	// the same branch, and once the target's mark moves below the revive/reopen
	// chain the FIRST call in the branch is no longer the target's.
	rel := craTargetMark(body[logBranch:])
	if rel < 0 {
		t.Fatalf("EvaluateRules' actionTaskLog branch never calls markRuleActivity (criterion 8)")
	}
	i := logBranch + rel
	// The call must be GUARDED by the prClose flag, and must come AFTER
	// appendRuleLog (D3's ordering: a crash between them degrades to today).
	logAt := strings.Index(body, "appendRuleLog(")
	if logAt < 0 || logAt > i {
		t.Errorf("markRuleActivity is called before appendRuleLog (or appendRuleLog is gone). Criterion 8: log " +
			"first, THEN the mark — a crash between the two leaves exactly today's behaviour")
	}
	// Look at the statement window around the call for the exclusion.
	start := i - 400
	if start < 0 {
		start = 0
	}
	window := body[start : i+200]
	if !strings.Contains(window, "prClose") {
		t.Errorf("the markRuleActivity call is not guarded by decision.prClose. Criterion 12 / D3: \"capture "+
			"skips the mark when decision.prClose is set (SWT-54's merged/closed PR notice). The next call "+
			"closes the task; surfacing a row in order to close it one statement later is noise\":\n%s", window)
	}
}

// craTargetMark finds the markRuleActivity call whose task argument is
// *decision.taskID (the attach TARGET), or -1.
func craTargetMark(s string) int {
	from := 0
	for {
		i := strings.Index(s[from:], "markRuleActivity(")
		if i < 0 {
			return -1
		}
		at := from + i
		end := strings.Index(s[at:], ")")
		if end > 0 && strings.Contains(s[at:at+end], "*decision.taskID") {
			return at
		}
		from = at + len("markRuleActivity(")
	}
}
