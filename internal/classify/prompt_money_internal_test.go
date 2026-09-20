package classify

// receipts-become-tasks (SWT-68): classify-v2's money clauses. Under v1 every
// one of 19 receipts, autopay notices and refunds came back payment_due +
// actionable. Whether the MODEL now answers false is the eval's job
// (docs/evals/personal-actionability.jsonl carries the 19 as `not` and the Citi
// bill as `actionable`); this pins that the clauses it was measured with stay in
// the prompt, and that the version moved so v1 and v2 verdicts can be told apart.

import (
	"strings"
	"testing"
)

func TestSystemPrompt_MoneyClauses(t *testing.T) {
	if PromptVersion == "classify-v1" {
		t.Errorf("PromptVersion is still classify-v1; the money clauses changed the prompt, so the version moves")
	}
	flat := strings.Join(strings.Fields(SystemPrompt), " ")
	for _, want := range []string{
		"STILL HAS TO PAY",                                             // what payment_due means
		"minimum payment due",                                          // the first v2 draft lost three real card bills: keep this
		"money that has already moved",                                 // receipts
		"money that will move by itself",                               // autopay, installments, scheduled payments
		"nothing you need to do",                                       // the phrase one v1 verdict quoted while answering true
		"refunds and money arriving",                                   //
		"That tie-break does NOT cover money",                          // the recall tie-break's carve-out
		"A receipt, an automatic charge or a refund is informational.", // the kind's definition
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("SystemPrompt lost the clause %q (SWT-68: do not trim the money clauses; re-run the eval if they change)", want)
		}
	}
	// The measured near-miss clause and the recall objective are untouched.
	for _, kept := range []string{`"your statement is available"`, "RECALL IS THE OBJECTIVE", "When you are genuinely torn, answer true."} {
		if !strings.Contains(flat, kept) {
			t.Errorf("SystemPrompt lost %q; the money clauses narrow the tie-break, they do not replace it", kept)
		}
	}
}
