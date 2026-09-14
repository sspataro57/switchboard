package promote_test

// chat-on-closed-task (SWT-53, docs/tickets/chat-on-closed-task_SPEC.md)
// criterion 17's promote half: docs/runbooks/local-classifier.md "Inquiry
// promotion", "What promotes", gains the task_log + resurface clause, and the
// section documents the body line. ZERO I/O beyond reading this repo. Reuses
// prRepoFile (structure_test.go).
//
// RED TODAY on its assertions (the runbook carries no resurface text) — once
// the package's test build compiles (resurface_internal_test.go needs
// Verdict.LoggedOnTaskID first).

import (
	"strings"
	"testing"
)

func TestRunbook_LocalClassifierWhatPromotesGainsTheResurfaceClause(t *testing.T) {
	const rel = "docs/runbooks/local-classifier.md"
	doc := prRepoFile(t, rel)
	i := strings.Index(doc, "## Inquiry promotion (SWT-40 Part C)")
	if i < 0 {
		t.Fatalf("%s has no \"## Inquiry promotion (SWT-40 Part C)\" section", rel)
	}
	section := doc[i:]
	if j := strings.Index(section[1:], "\n## "); j > 0 {
		section = section[:j+1]
	}
	j := strings.Index(section, "**What promotes.**")
	if j < 0 {
		t.Fatalf("%s's Inquiry promotion section has no \"**What promotes.**\" paragraph", rel)
	}
	para := section[j:]
	if k := strings.Index(para, "\n\n"); k > 0 {
		para = para[:k]
	}
	lp := strings.ToLower(para)
	for _, want := range []struct{ frag, why string }{
		{"task_log", "the widened admission: a latest decision of task_log …"},
		{"resurface", "… that capture recorded with resurface, onto a task that is still closed (CC5)"},
	} {
		if !strings.Contains(lp, want.frag) {
			t.Errorf("%s \"What promotes\" never mentions %q — %s\n%s", rel, want.frag, want.why, para)
		}
	}
	if !strings.Contains(strings.ToLower(section), "logged_on_closed_task") {
		t.Errorf("%s's Inquiry promotion section never documents the body line `logged_on_closed_task: N` (CC6)", rel)
	}
}
