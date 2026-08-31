package classify_test

// SWT-23 criteria 18-20's harness half: the eval prints the strata apart, and
// says what each number is worth. Fake Store + fake provider.Client — ZERO
// network, ZERO Postgres, ZERO live model.
//
// WHY THE STRATA EXIST AT ALL, because the test is unreadable without it: the
// residue's actionable base rate is a couple of percent, so a uniform sample of
// 200 yields four or five positives and a recall computed on four positives is a
// coin flip printed to two decimal places. The labelled set is therefore drawn
// in three strata — `uniform` (the only one a base rate or an honest precision
// can come from), `enriched` (the recall denominator), `domain_gate` (criterion
// 6's per-domain samples) — and the stratum is recorded IN the file, because
// precision computed over an enriched sample WILL be quoted as if it were
// production precision unless the harness refuses to make that mistake for the
// reader.
//
// IMPOSED SURFACE:
//
//	type Label struct { …; Stratum string `json:"stratum,omitempty"` }
//	func Eval(ctx, store, router, cfg Config, labels []Label, w io.Writer) error
//
// GREENFIELD NOTE: Label.Stratum and Config.Lane do not exist and Eval takes
// five arguments today, so this file compile-FAILS. Expected red.

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/provider"
)

func stCfg(lane classify.Lane) classify.Config {
	return classify.Config{MaxTokens: 512, Lane: lane}
}

// stLine returns the first output line matching re, and FAILS when there is
// none — comparing against an empty string is the scan-with-nothing-to-scan
// landmine.
func stLine(t *testing.T, out, pattern, why string) string {
	t.Helper()
	re := regexp.MustCompile(pattern)
	for _, line := range strings.Split(out, "\n") {
		if re.MatchString(line) {
			return line
		}
	}
	t.Fatalf("the eval output has no line matching %s — %s\n\noutput:\n%s", pattern, why, out)
	return ""
}

// ---- criterion 20: three lines, and each one says what it is worth -----------

// The fixture is built so the two precisions DIFFER. If it were built so they
// agreed, a harness that ignored the strata entirely would pass — which is the
// "fixture shaped like the assertion" mistake this repo has paid for repeatedly.
//
//	stratum      labels  actionable  flagged by the model
//	uniform         5         1        3  (1 true, 2 false)
//	enriched        3         3        1
//	domain_gate     2         0        0
//
//	recall    (all labels)      2 of 4 actionable caught      = 0.50
//	precision (uniform only)    1 of 3 flagged were actionable = 0.33
//	precision (all labels)      2 of 4                         = 0.50  <- the WRONG number
//	base rate (uniform only)    1 of 5
func TestEval_PrintsTheStrataApart(t *testing.T) {
	msgs := []classify.PendingMessage{
		// uniform
		evMessage(31, "Your payment is due", "minimum payment $35 due 2026-09-03"),
		evMessage(32, "Ends tonight: 90% off everything", "final notice, act now, sale ends tonight"),
		evMessage(33, "Your statement is available", "your monthly statement is ready to view"),
		evMessage(34, "Payment received, thank you", "we received your payment, nothing to do"),
		evMessage(35, "Payment plan options for you", "payment plans available, learn more"),
		// enriched
		evMessage(41, "Verify the sign-in on your account", "confirm this was you or reset your password"),
		evMessage(42, "Your payment is overdue", "payment overdue, a late fee applies after 2026-09-10"),
		evMessage(43, "Appointment tomorrow at 9am", "confirm or reschedule your appointment"),
		// domain_gate
		evMessage(51, "12 new jobs for you", "recommended jobs based on your profile"),
		evMessage(52, "Someone viewed your profile", "see who is interested in you"),
	}
	labels := []classify.Label{
		{MessageID: 31, Label: "actionable", SubjectSHA256: evSubjectHash("Your payment is due"), Stratum: "uniform"},
		{MessageID: 32, Label: "not", SubjectSHA256: evSubjectHash("Ends tonight: 90% off everything"), Stratum: "uniform"},
		{MessageID: 33, Label: "not", SubjectSHA256: evSubjectHash("Your statement is available"), Stratum: "uniform"},
		{MessageID: 34, Label: "not", SubjectSHA256: evSubjectHash("Payment received, thank you"), Stratum: "uniform"},
		{MessageID: 35, Label: "not", SubjectSHA256: evSubjectHash("Payment plan options for you"), Stratum: "uniform"},
		{MessageID: 41, Label: "actionable", SubjectSHA256: evSubjectHash("Verify the sign-in on your account"), Stratum: "enriched"},
		{MessageID: 42, Label: "actionable", SubjectSHA256: evSubjectHash("Your payment is overdue"), Stratum: "enriched"},
		{MessageID: 43, Label: "actionable", SubjectSHA256: evSubjectHash("Appointment tomorrow at 9am"), Stratum: "enriched"},
		{MessageID: 51, Label: "not", SubjectSHA256: evSubjectHash("12 new jobs for you"), Stratum: "domain_gate"},
		{MessageID: 52, Label: "not", SubjectSHA256: evSubjectHash("Someone viewed your profile"), Stratum: "domain_gate"},
	}

	local := &evClient{flagIf: "payment"} // flags 31, 34, 35, 42
	var out bytes.Buffer
	if err := classify.Eval(context.Background(), &cfStore{pending: msgs},
		provider.NewRouter(nil, local, time.Minute), stCfg(classify.LaneResidue), labels, &out); err != nil {
		t.Fatalf("Eval: %v", err)
	}
	text := out.String()
	if local.calls != 10 {
		t.Fatalf("the model was called %d time(s) for 10 labels; nothing below can be read.\n%s",
			local.calls, text)
	}

	recall := stLine(t, text, `(?i)recall`, "criterion 20's first line: recall over ALL labels")
	if !strings.Contains(recall, "0.50") {
		t.Errorf("recall line = %q, want 0.50 (2 of 4 actionable caught, counting every stratum). Recall "+
			"uses ALL the labels because the enriched stratum IS the recall denominator — without it a "+
			"uniform 200 at a ~2%% base rate yields four positives and a recall that is noise", recall)
	}

	precision := stLine(t, text, `(?i)precision`, "criterion 20's second line: precision, UNIFORM STRATUM ONLY")
	if !regexp.MustCompile(`(?i)uniform`).MatchString(precision) {
		t.Errorf("precision line = %q — it does not say the number is computed on the uniform stratum "+
			"only. That label is the whole mechanism: a precision measured over a deliberately enriched "+
			"sample is not production precision, and it WILL be quoted as if it were", precision)
	}
	if !strings.Contains(precision, "0.33") {
		t.Errorf("precision line = %q, want 0.33 — 1 of the 3 flagged UNIFORM messages was actionable. "+
			"0.50 is the all-labels number and it is the wrong one to print: it flatters the classifier "+
			"with a denominator that was drawn to contain positives", precision)
	}
	if strings.Contains(precision, "0.50") {
		t.Errorf("precision line = %q and carries 0.50, the all-labels figure", precision)
	}

	base := stLine(t, text, `(?i)base rate`,
		"criterion 20's third line: the measured actionable base rate of the residue")
	if !regexp.MustCompile(`(?i)uniform`).MatchString(base) {
		t.Errorf("base-rate line = %q — it does not say the rate is from the uniform stratum. A base rate "+
			"computed over the enriched draw is not a base rate at all", base)
	}
	if !regexp.MustCompile(`\b1\b.{0,12}\b5\b`).MatchString(base) {
		t.Errorf("base-rate line = %q, want it to name 1 actionable of 5 uniform labels (k of n). THIS IS "+
			"THE NUMBER THAT DECIDES THE LANE'S FUTURE: if the residue is 0.5%% actionable after the rules, "+
			"the honest conclusion is that the lane runs daily on new mail and never on history", base)
	}

	// Each number says what it is worth — the point of printing three instead of
	// two. Without this the reader picks whichever one is highest.
	if !regexp.MustCompile(`(?i)over-?represent|enriched`).MatchString(text) {
		t.Errorf("the output never says that actionable-shaped mail is OVER-REPRESENTED in the labelled "+
			"set. Criterion 20 asks for the annotation, not just the arithmetic: three bare numbers with "+
			"no note beside them are three numbers a reader will quote interchangeably.\n%s", text)
	}

	// And the misses are still named by id — recall is the objective.
	i := regexp.MustCompile(`(?i)false negative`).FindStringIndex(text)
	if i == nil {
		t.Fatalf("no false-negative section:\n%s", text)
	}
	tail := text[i[0]:]
	for _, id := range []string{"41", "43"} {
		if !strings.Contains(tail, id) {
			t.Errorf("message %s (labelled actionable, classified not) is missing from the false-negative "+
				"section:\n%s", id, tail)
		}
	}
}

// ---- criterion 20: backwards compatibility, byte for byte ---------------------

// "A set carrying none (the personal file) produces today's output byte-for-byte
// — backwards compatibility is what keeps this from being a fork of the
// harness." The golden below was captured from the harness as it stands on
// `main` before this ticket, with this exact fixture. It is deliberately a
// literal and not a re-derivation: a golden computed by the code it is checking
// is not a golden.
//
// If this fails, read the diff before changing the constant. The personal lane's
// numbers (0.94 / 0.50, 2026-08-31) were measured through this output, and a
// harness that renders differently makes the two rows of the runbook's table
// incomparable — which is the same class of mistake as the 60-minute estimate
// quoted out of the context that produced it.
func TestEval_WithoutStrata_PrintsTodaysOutputByteForByte(t *testing.T) {
	const golden = "classify eval — model qwen3:8b — n=3 scored (3 labels in the file)\n" +
		"  recall    0.50   (1 of 2 actionable messages caught)\n" +
		"  precision 1.00   (1 of 1 flagged were actionable)\n" +
		"  median latency 380 ms\n" +
		"\n" +
		"false negatives (1) — labelled actionable, classified not:\n" +
		"  message 22  [#XN123456] Message from Pines Association - First Notice\n" +
		"\n" +
		"Recall is the objective: a missed payment or fine notice is a late fee, a false alarm\n" +
		"costs a second to dismiss. Tune against these labels, never against intuition — this\n" +
		"fixture has been wrong before and the models were right.\n"

	const paySubject = "Your payment is due"
	const hoaSubject = "[#XN123456] Message from Pines Association - First Notice"
	const stmtSubject = "Your statement is available"
	msgs := []classify.PendingMessage{
		evMessage(21, paySubject, "minimum payment $35 due 2026-09-03"),
		evMessage(22, hoaSubject, "please see attachment for additional detail"),
		evMessage(23, stmtSubject, "your monthly statement is ready to view"),
	}
	labels := []classify.Label{
		{MessageID: 21, Label: "actionable", SubjectSHA256: evSubjectHash(paySubject)},
		{MessageID: 22, Label: "actionable", SubjectSHA256: evSubjectHash(hoaSubject)},
		{MessageID: 23, Label: "not", SubjectSHA256: evSubjectHash(stmtSubject)},
	}

	local := &evClient{flagIf: "payment"}
	var out bytes.Buffer
	if err := classify.Eval(context.Background(), &cfStore{pending: msgs},
		provider.NewRouter(nil, local, time.Minute), stCfg(classify.LanePersonal), labels, &out); err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if out.String() != golden {
		t.Errorf("the strata-less eval output changed.\n--- want ---\n%s\n--- got ---\n%s\n"+
			"Criterion 20: a label set carrying NO strata prints today's output byte-for-byte. The "+
			"personal file has no `stratum` key (criterion 26 forbids one there), so this is the path "+
			"every SWT-22 number was measured through — a stratum breakdown printed for it would be three "+
			"lines computed over an empty uniform stratum, which is worse than no lines at all",
			golden, out.String())
	}
}

// A residue label set is scored on the residue PROMPT. Sharing classifyAll and
// renderUser is what makes an eval a real delta measurement rather than a
// different experiment (SWT-25 premise 6); sharing the SYSTEM PROMPT across
// lanes would make it the wrong experiment.
func TestEval_UsesTheLanesOwnSystemPrompt(t *testing.T) {
	msgs := []classify.PendingMessage{evMessage(61, "12 new jobs for you", "recommended jobs for you")}
	labels := []classify.Label{
		{MessageID: 61, Label: "not", SubjectSHA256: evSubjectHash("12 new jobs for you"), Stratum: "uniform"},
	}
	local := &evClient{}
	var out bytes.Buffer
	if err := classify.Eval(context.Background(), &cfStore{pending: msgs},
		provider.NewRouter(nil, local, time.Minute), stCfg(classify.LaneResidue), labels, &out); err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if len(local.requests) != 1 {
		t.Fatalf("the model saw %d request(s), want 1", len(local.requests))
	}
	if local.requests[0].System != classify.ResidueSystemPrompt {
		t.Errorf("Eval sent the personal system prompt while scoring the residue lane. The number it "+
			"prints would then describe a prompt nobody runs:\n%q", local.requests[0].System)
	}
}
