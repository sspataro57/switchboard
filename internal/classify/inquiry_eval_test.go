package classify_test

// SWT-33 criteria 23, 24 and 31 (+ D7) — the eval harness on the inquiry lane,
// and the sub-threshold REFUSAL that makes shipping a 40-line labelled set safe.
// Fake Store + fake provider.Client: ZERO network, ZERO Postgres, ZERO live
// model.
//
// WHY THIS GUARD EXISTS, in the SPEC's own words: "this repo has a recorded
// history of a number quoted out of the context that produced it — the 0.25 s
// warm benchmark that turned 29.5 GPU-hours into '60 minutes' — and a guard
// beats a convention every time." Q3's answer was to ship at 40 labels WITH the
// condition enforced in code, not by convention.
//
// ---- IMPOSED SURFACE ---------------------------------------------------------
//
//	// criterion 31: ONE exported threshold and ONE exported marker, each with
//	// ONE spelling.
//	const EvalResultThreshold = 120
//	const EvalIndicativeMarker = "INDICATIVE ONLY — this is not a measurement"
//
// The marker is a bare sentence rather than a format string on purpose: the same
// bytes are quoted in the runbook's score-table row (criterion 31c) and in any
// Jira comment, and a `%d`-carrying constant cannot be. The n and the threshold
// are printed BESIDE it, from EvalResultThreshold, so there is still exactly one
// spelling of 120 in Go.
//
// GREENFIELD NOTE — EXPECTED RED. classify.EvalResultThreshold,
// classify.EvalIndicativeMarker and classify.LaneInquiry do not exist, so this
// file compile-FAILS with "undefined: classify.EvalResultThreshold". Verified in
// the authoring session against a throwaway stub: the sub-threshold test then
// fires on its merits (the output carries `recall 0.83` and no marker), and the
// at-threshold characterization is GREEN under today's Eval — it is a guard, not
// a discovery, and its whole job is to STAY green.

import (
	"bytes"
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/provider"
)

// ratioShaped is criterion 31's own pattern, verbatim: `\d\.\d\d`. Anything of
// this shape below the threshold is a number that will be quoted as a result.
var ratioShaped = regexp.MustCompile(`\d\.\d\d`)

// iqLabelSet builds n labelled inquiry messages with a fixed, hand-computable
// outcome: every 4th message asks a question (the fake flags it), every 10th
// message is labelled needs_reply but does NOT ask one (a false negative). The
// numbers are deterministic so the counts the eval prints can be asserted.
func iqLabelSet(n int) ([]classify.PendingMessage, []classify.Label) {
	var msgs []classify.PendingMessage
	var labels []classify.Label
	for i := 1; i <= n; i++ {
		body, lbl := "deploy finished, no action needed", "not"
		switch {
		case i%4 == 0:
			body, lbl = "can you confirm the rotation date?", "needs_reply"
		case i%10 == 0:
			lbl = "needs_reply"
		}
		m := iqMessages(1)[0]
		m.MessageID = int64(i)
		m.RawSourceItemID = int64(3000 + i)
		m.Subject = "llamasite-eng"
		m.BodyText = body
		msgs = append(msgs, m)
		labels = append(labels, classify.Label{
			MessageID: int64(i), Label: lbl, SubjectSHA256: classify.SubjectHash("llamasite-eng")})
	}
	return msgs, labels
}

// iqEvalClient answers needs_reply when the rendered prompt contains a question,
// through the INQUIRY contract. A fake returning the actionability shape would
// decode to a zero verdict and score every message as a miss.
type iqEvalClient struct{ calls, probes int }

func (c *iqEvalClient) Describe() provider.Descriptor {
	return provider.Descriptor{Name: "ollama", Endpoint: "http://127.0.0.1:11434"}
}
func (c *iqEvalClient) Probe(_ context.Context) error { c.probes++; return nil }
func (c *iqEvalClient) Complete(_ context.Context, req provider.Request) (provider.Response, error) {
	c.calls++
	v := iqNoReply
	if strings.Contains(req.User, "can you confirm") {
		v = iqNeedsReply
	}
	return provider.Response{Raw: []byte(v), Model: "qwen3:8b", LatencyMS: 380}, nil
}

func iqEvalCfg() classify.Config {
	// NO Since. Criterion 23's carve-over from SWT-23: an eval is bounded by its
	// LABEL FILE, so the run path's --since refusal (D6) deliberately does not
	// apply — refusing here would refuse the one command the ticket's numbers
	// come from.
	return classify.Config{MaxTokens: 512, Lane: classify.LaneInquiry}
}

// ---- criterion 31(a): below the threshold, counts and a marker, NO ratio ------

func TestEval_BelowThreshold_PrintsCountsAndTheMarkerAndNoRatio(t *testing.T) {
	if classify.EvalResultThreshold != 120 {
		t.Fatalf("classify.EvalResultThreshold = %d, want 120. Q3's answer ships a 40-line starter set on "+
			"exactly ONE condition — that below 120 labels no precision or recall number may be presented "+
			"as a result — and 120 is the size of the stratified set the runbook commits to (uniform >= 80 "+
			"+ enriched >= 40)", classify.EvalResultThreshold)
	}
	const n = 40
	msgs, labels := iqLabelSet(n)
	local := &iqEvalClient{}

	var out bytes.Buffer
	if err := classify.Eval(context.Background(), &cfStore{pending: msgs},
		provider.NewRouter(nil, local, time.Minute), iqEvalCfg(), labels, &out); err != nil {
		t.Fatalf("Eval(inquiry, n=%d): %v", n, err)
	}
	got := out.String()

	// POSITIVE CONTROL first: the eval actually ran and scored. Without it,
	// "no ratio in the output" is satisfied by an eval that printed nothing.
	if local.calls != n {
		t.Fatalf("POSITIVE CONTROL FAILED: the local client saw %d call(s), want %d. Every assertion below "+
			"is about what a REAL scoring pass prints; a refusal that scored nothing would satisfy them "+
			"all", local.calls, n)
	}
	if !strings.Contains(got, "n="+strconv.Itoa(n)) {
		t.Errorf("the sub-threshold output never names n:\n%s", got)
	}

	// THE REFUSAL. Criterion 31: no 0.78-shaped number ANYWHERE in the output.
	if m := ratioShaped.FindAllString(got, -1); len(m) > 0 {
		t.Errorf("the eval printed ratio-shaped number(s) %v at n=%d, below EvalResultThreshold=%d.\n"+
			"--- output ---\n%s\n"+
			"Criterion 31: with fewer than %d scored labels Eval prints COUNTS and a marker, and no "+
			"\\d\\.\\d\\d anywhere. This repo shipped a 25-29x cost error by quoting the 0.25 s warm "+
			"benchmark out of the context that produced it; a convention would not have stopped it, and "+
			"this is the guard that does.", m, n, classify.EvalResultThreshold, got, classify.EvalResultThreshold)
	}

	// The MARKER, verbatim, one spelling.
	if !strings.Contains(classify.EvalIndicativeMarker, "INDICATIVE ONLY") {
		t.Errorf("classify.EvalIndicativeMarker = %q. It must be worded so it cannot be quoted out of "+
			"context — the SPEC's own example is `INDICATIVE ONLY — n=40 < 120, this is not a "+
			"measurement`", classify.EvalIndicativeMarker)
	}
	if !regexp.MustCompile(`(?i)not a measurement`).MatchString(classify.EvalIndicativeMarker) {
		t.Errorf("classify.EvalIndicativeMarker = %q; it does not say the number is NOT A MEASUREMENT, "+
			"which is the only part of it that survives being pasted into a Jira comment",
			classify.EvalIndicativeMarker)
	}
	if !strings.Contains(got, classify.EvalIndicativeMarker) {
		t.Errorf("the sub-threshold output does not carry classify.EvalIndicativeMarker verbatim:\n%s\n"+
			"One constant, one spelling — the same bytes go in the runbook's score-table row (criterion "+
			"31c), and a marker composed at the print site drifts from the one the runbook quotes", got)
	}
	if !strings.Contains(got, strconv.Itoa(classify.EvalResultThreshold)) {
		t.Errorf("the sub-threshold output never names the threshold (%d):\n%s\nA marker that says 'this "+
			"is indicative' without saying indicative OF WHAT leaves the reader to guess the bar",
			classify.EvalResultThreshold, got)
	}

	// The COUNTS are still printed — the refusal replaces the ratio, it does not
	// delete the result. 40 labels: 10 ask a question (all caught), 2 more
	// (messages 10 and 30) are labelled needs_reply and ask nothing (missed) =>
	// caught 10 of 12, and 10 of 10 flagged were labelled needs_reply.
	for _, want := range []string{"10 of 12", "10 of 10"} {
		if !strings.Contains(got, want) {
			t.Errorf("the sub-threshold output does not carry the count %q:\n%s\nCriterion 31 prints "+
				"COUNTS (`caught 7 of 9 labelled needs_reply; 7 of 12 flagged were labelled needs_reply`) "+
				"— a refusal that printed nothing would make the 40-label set worthless rather than "+
				"honest", want, got)
		}
	}
	// And in the lane's own vocabulary, not the actionability one.
	if !strings.Contains(got, classify.LaneInquiry.Contract.PositiveLabel) {
		t.Errorf("the sub-threshold output never names %q. Criterion 23: the eval scores the lane's "+
			"DECISION field, and a count line phrased in another lane's vocabulary describes a different "+
			"question:\n%s", classify.LaneInquiry.Contract.PositiveLabel, got)
	}
	if strings.Contains(got, "actionable") {
		t.Errorf("the inquiry eval's output uses the word \"actionable\":\n%s\nD1: `actionable` scored "+
			"against inquiry labels would make two different questions' recall/precision falsely "+
			"comparable, and the output is where that confusion starts", got)
	}
}

// ---- criterion 31(b): at/above the threshold, TODAY'S output, byte for byte ---

// A CHARACTERIZATION test, and the reason criterion 31 cannot regress the two
// MEASURED lanes. The personal file carries 280 labels and the residue 874, both
// comfortably above the threshold, so this path is the one every published
// number came through: 0.94 / 0.50 on 2026-08-31 and the four dated rows in the
// runbook's table. If the guard changed a byte here, those rows would stop
// describing anything reproducible.
//
// The whole string is the assertion — column padding, the two-space gutters, the
// blank lines and the closing three-line note included.
func TestEval_AtThreshold_PersonalLaneOutputIsByteIdenticalToToday(t *testing.T) {
	msgs, labels := iqPersonalGoldenSet(classify.EvalResultThreshold)
	local := &evClient{flagIf: "payment"}

	var out bytes.Buffer
	if err := classify.Eval(context.Background(), &cfStore{pending: msgs},
		provider.NewRouter(cfHosted(), local, time.Minute),
		stCfg(classify.LanePersonal), labels, &out); err != nil {
		t.Fatalf("Eval(personal, n=%d): %v", classify.EvalResultThreshold, err)
	}
	if out.String() != iqPersonalGolden {
		t.Errorf("the personal lane's eval output changed at n=%d labels (>= EvalResultThreshold).\n\n"+
			"--- got ---\n%q\n\n--- want ---\n%q\n\n"+
			"Criterion 31(b): at or above the threshold the existing ratio output is BYTE-IDENTICAL to "+
			"today, so the sub-threshold guard cannot regress the two measured lanes. The runbook's dated "+
			"score rows (0.83/0.58, 0.94/0.50, 0.59/0.28, 0.57/0.67) were all printed by this path.",
			classify.EvalResultThreshold, out.String(), iqPersonalGolden)
	}
	// And the ratio really is there — otherwise a golden captured from a broken
	// build would pin the refusal as the normal output.
	if !ratioShaped.MatchString(out.String()) {
		t.Fatalf("POSITIVE CONTROL FAILED: the at-threshold output carries no \\d\\.\\d\\d ratio at all:\n%s\n"+
			"The sub-threshold assertion above is only meaningful if the ABOVE-threshold path prints one",
			out.String())
	}
	if strings.Contains(out.String(), classify.EvalIndicativeMarker) {
		t.Errorf("the at-threshold output carries the INDICATIVE-ONLY marker:\n%s\nThe marker is for "+
			"n < %d only; printed on a 280-label run it would teach the reader to ignore it",
			out.String(), classify.EvalResultThreshold)
	}
}

// The 120-label personal fixture and the output today's Eval produces for it,
// captured from the current implementation in the authoring session (2026-09-10)
// by running Eval over exactly this fixture. Every 4th message is a payment
// notice the fake flags; every 10th of the rest is labelled actionable and is
// not flagged, which is what makes the recall a real 0.83 rather than a 1.00
// that would hide a formatting change in the denominators.
func iqPersonalGoldenSet(n int) ([]classify.PendingMessage, []classify.Label) {
	var msgs []classify.PendingMessage
	var labels []classify.Label
	for i := 1; i <= n; i++ {
		subj, lbl := "This week in widgets", "not"
		switch {
		case i%4 == 0:
			subj, lbl = "Your payment is due", "actionable"
		case i%10 == 0:
			lbl = "actionable"
		}
		msgs = append(msgs, evMessage(int64(i), subj, "body"))
		labels = append(labels, classify.Label{
			MessageID: int64(i), Label: lbl, SubjectSHA256: evSubjectHash(subj)})
	}
	return msgs, labels
}

const iqPersonalGolden = "classify eval — model qwen3:8b — n=120 scored (120 labels in the file)\n" +
	"  recall    0.83   (30 of 36 actionable messages caught)\n" +
	"  precision 1.00   (30 of 30 flagged were actionable)\n" +
	"  median latency 380 ms\n" +
	"\n" +
	"false negatives (6) — labelled actionable, classified not:\n" +
	"  message 10  This week in widgets\n" +
	"  message 30  This week in widgets\n" +
	"  message 50  This week in widgets\n" +
	"  message 70  This week in widgets\n" +
	"  message 90  This week in widgets\n" +
	"  message 110  This week in widgets\n" +
	"\n" +
	"Recall is the objective: a missed payment or fine notice is a late fee, a false alarm\n" +
	"costs a second to dismiss. Tune against these labels, never against intuition — this\n" +
	"fixture has been wrong before and the models were right.\n"

// ---- Codex round 6: duplicate labels cannot inflate n past the threshold -----

// The refusal gates on the SCORED count. A set of 61 labels repeated to 122
// rows would cross EvalResultThreshold and print ratios with no new judgement
// behind them — so Eval refuses a duplicated message id before scoring,
// whoever calls it.
func TestEval_DuplicateLabelsAreRefusedBeforeScoring(t *testing.T) {
	msgs, labels := iqLabelSet(61)
	doubled := append(append([]classify.Label(nil), labels...), labels...)
	local := &iqEvalClient{}
	var out bytes.Buffer
	err := classify.Eval(context.Background(), &cfStore{pending: msgs},
		provider.NewRouter(nil, local, time.Minute), iqEvalCfg(), doubled, &out)
	if err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("a 61-label set repeated to %d rows returned %v; want a refusal naming the duplicate:\n%s",
			len(doubled), err, out.String())
	}
	if local.calls != 0 || ratioShaped.MatchString(out.String()) {
		t.Errorf("the refused eval made %d call(s) and printed %q; it must refuse before scoring anything",
			local.calls, out.String())
	}
	// Before ANY I/O — including the router's availability probe of the local
	// endpoint (Codex round 7: the first cut checked after Route).
	if local.probes != 0 {
		t.Errorf("the refused eval probed the local endpoint %d time(s); the duplicate check must run before "+
			"the router is asked anything", local.probes)
	}
	// Control: the same set, once, scores normally — and DOES probe, or the
	// zero-probe assertion above would hold for a router that never probes.
	control := &iqEvalClient{}
	if err := classify.Eval(context.Background(), &cfStore{pending: msgs},
		provider.NewRouter(nil, control, time.Minute), iqEvalCfg(), labels, &bytes.Buffer{}); err != nil {
		t.Fatalf("CONTROL FAILED: the un-duplicated set was refused: %v", err)
	}
	if control.probes == 0 {
		t.Fatalf("POSITIVE CONTROL FAILED: a normal eval never probed the local endpoint, so 'zero probes' on " +
			"the refused run proves nothing")
	}
}

// ---- Codex round 5: owner-blanket labels are reported, not silently merged ---

// The owner labelled some rows by blanket instruction ("everything older was
// already dealt with"), which can mark a real ask-at-the-time `not`. Those rows
// stay in the score — they are his judgement — but Eval must SAY how many were
// scored and how many the model flagged, or the false-positive count silently
// includes them.
func TestEval_OwnerBlanketLabelsAreReportedOnTheirOwnLine(t *testing.T) {
	msgs, labels := iqLabelSet(40)
	// Messages 4 and 8 ask a question (the fake flags them); the owner labelled
	// them `not` in bulk. Message 1 is plain chatter, also blanket-labelled.
	for i := range labels {
		switch labels[i].MessageID {
		case 4, 8:
			labels[i].Label = "not"
			labels[i].Note = classify.OwnerBlanketNote
		case 1:
			labels[i].Note = classify.OwnerBlanketNote
		case 12:
			// Flagged AND labelled needs_reply: a true positive. It is scored as
			// a blanket row but must NOT count as "labelled `not` and flagged"
			// (go-reviewer L5 — pins the `!want` gate).
			labels[i].Note = classify.OwnerBlanketNote
		}
	}
	var out bytes.Buffer
	if err := classify.Eval(context.Background(), &cfStore{pending: msgs},
		provider.NewRouter(nil, &iqEvalClient{}, time.Minute), iqEvalCfg(), labels, &out); err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if !strings.Contains(out.String(), "owner-blanket labels: 4 scored, 2 labelled `not` and flagged") {
		t.Errorf("the eval does not report the owner-blanket rows (want 4 scored, 2 labelled not and flagged — "+
			"the flagged needs_reply row is a true positive, not a possible bulk-labelled ask):\n%s", out.String())
	}

	// STRATIFIED (go-reviewer M1): the precision line is uniform-only, so the
	// blanket line must say how many of its flags are in the uniform stratum —
	// the all-strata count cannot be subtracted from a uniform-only line.
	msgsS, labelsS := iqLabelSet(40)
	for i := range labelsS {
		labelsS[i].Stratum = "uniform"
		switch labelsS[i].MessageID {
		case 4: // flagged, blanket `not`, uniform
			labelsS[i].Label, labelsS[i].Note = "not", classify.OwnerBlanketNote
		case 8: // flagged, blanket `not`, ENRICHED
			labelsS[i].Label, labelsS[i].Note = "not", classify.OwnerBlanketNote
			labelsS[i].Stratum = "enriched"
		case 1: // not flagged, blanket, uniform
			labelsS[i].Note = classify.OwnerBlanketNote
		}
	}
	var strat bytes.Buffer
	if err := classify.Eval(context.Background(), &cfStore{pending: msgsS},
		provider.NewRouter(nil, &iqEvalClient{}, time.Minute), iqEvalCfg(), labelsS, &strat); err != nil {
		t.Fatalf("Eval (stratified): %v", err)
	}
	if !strings.Contains(strat.String(), "owner-blanket labels: 3 scored, 2 labelled `not` and flagged (1 in the uniform stratum") {
		t.Errorf("the stratified eval does not split the owner-blanket flags by the uniform stratum "+
			"(want 3 scored, 2 flagged, 1 uniform):\n%s", strat.String())
	}

	// Control: a set with no such note prints no such line — the personal
	// golden at n=120 pins the same for the measured lanes.
	msgs2, labels2 := iqLabelSet(40)
	var plain bytes.Buffer
	if err := classify.Eval(context.Background(), &cfStore{pending: msgs2},
		provider.NewRouter(nil, &iqEvalClient{}, time.Minute), iqEvalCfg(), labels2, &plain); err != nil {
		t.Fatalf("Eval (control): %v", err)
	}
	if strings.Contains(plain.String(), "owner-blanket") {
		t.Errorf("a set with no owner-blanket notes printed the owner-blanket line:\n%s", plain.String())
	}
}

// ---- Codex re-review: the refusal is Eval's own, on EVERY lane ----------------

// The first cut scoped criterion 31 to the inquiry lane plus a caller-set flag,
// which any caller of this exported function could leave unset and get the
// exact bare ratio the guard exists to prevent. The refusal now keys on the
// SCORED n inside Eval for every lane, so no caller can obtain a sub-threshold
// ratio. The measured lanes' own files (280, 874) sit above the threshold, so
// what the runbook published is unchanged — the at-threshold golden above.
func TestEval_BelowThreshold_RefusesARatioOnEveryLane(t *testing.T) {
	msgs, labels := iqPersonalGoldenSet(12)
	var out bytes.Buffer
	if err := classify.Eval(context.Background(), &cfStore{pending: msgs},
		provider.NewRouter(nil, &evClient{flagIf: "payment"}, time.Minute), stCfg(classify.LanePersonal),
		labels, &out); err != nil {
		t.Fatalf("Eval: %v", err)
	}
	got := out.String()
	if m := ratioShaped.FindAllString(got, -1); len(m) > 0 {
		t.Errorf("a 12-label personal eval printed ratio(s) %v — no caller may get a sub-threshold ratio:\n%s",
			m, got)
	}
	if !strings.Contains(got, classify.EvalIndicativeMarker) {
		t.Errorf("a 12-label personal eval does not carry the marker:\n%s", got)
	}
	// Still the personal lane: its own vocabulary and objective.
	if !strings.Contains(got, "labelled actionable") || !strings.Contains(got, "Recall is the objective") {
		t.Errorf("the refused personal eval lost its own vocabulary or objective:\n%s", got)
	}
}

// ---- the trap: 31's threshold and 22's fixture minimum are ONE fact ----------

// Criterion 31 says "below 120 no ratio". Criterion 22 says the labelled set's
// minimum is 40 today, "with a comment carrying the dated commitment to 120
// stratified", and that "strata are OPTIONAL on this file at n<120 and REQUIRED
// once the minimum is raised".
//
// Those are TWO numbers describing ONE fact. Spelled twice, they drift the first
// time the minimum is raised — the raise happens in the structure test, the
// refusal keeps firing from eval.go, and the runbook says one thing while the
// binary does another. So the labelled-set guard must express its 120 through
// classify.EvalResultThreshold, and this test is what says so.
func TestEvalResultThreshold_IsTheOneSpellingOfOneHundredTwenty(t *testing.T) {
	const rel = "internal/classify/structure_test.go"
	src := csRepoFile(t, rel)

	i := strings.Index(src, "docs/evals/inquiry-needs-reply.jsonl")
	if i < 0 {
		t.Fatalf("%s does not name docs/evals/inquiry-needs-reply.jsonl. Criterion 22: the labelled-set "+
			"table gains the third file as a ROW — a separate copy of the guard is how the three files' "+
			"rules drift", rel)
	}
	end := i + 2000
	if end > len(src) {
		end = len(src)
	}
	row := src[i:end]

	if !strings.Contains(row, "classify.EvalResultThreshold") {
		t.Errorf("%s's inquiry row does not express its 120-label commitment through "+
			"classify.EvalResultThreshold:\n%s\n"+
			"Criterion 31's threshold and criterion 22's dated commitment are ONE fact. Spelled twice, "+
			"they drift the first time the minimum is raised: the table says 120, eval.go says 120, and "+
			"the day one of them becomes 200 the other keeps refusing (or stops refusing) with nothing "+
			"to notice it by.", rel, row[:min(len(row), 600)])
	}
	if regexp.MustCompile(`\b120\b`).MatchString(row) {
		t.Errorf("%s's inquiry row carries a bare 120 literal:\n%s\nUse classify.EvalResultThreshold. "+
			"A literal here is the second spelling this test exists to prevent", rel, row[:min(len(row), 600)])
	}
}

// ---- criterion 24: eval STILL writes no ai_runs / ai_extractions -------------

// Re-asserted for the new lane, and the reason is stated where it can be read:
// this is the ONLY thing stopping an eval over a labelled set from injecting
// fresh-timestamped verdicts into a promoter's inbox. internal/promote's inbox
// keys on `worker_type='classify'` and `r.created_at >= p.classify_promote_after`
// — an eval that recorded runs would manufacture exactly that shape.
func TestEval_InquiryLane_WritesNoRunsAndNoExtractions(t *testing.T) {
	msgs, labels := iqLabelSet(12)
	store := &cfStore{pending: msgs}
	local := &iqEvalClient{}

	var out bytes.Buffer
	if err := classify.Eval(context.Background(), store,
		provider.NewRouter(nil, local, time.Minute), iqEvalCfg(), labels, &out); err != nil {
		t.Fatalf("Eval: %v", err)
	}
	// Control: it really scored.
	if local.calls != 12 {
		t.Fatalf("POSITIVE CONTROL FAILED: the local client saw %d call(s), want 12; an eval that scored "+
			"nothing writes nothing for uninteresting reasons", local.calls)
	}
	if len(store.runs) != 0 {
		t.Errorf("Eval recorded %d ai_runs row(s). Criterion 24: it records NONE, and any future change to "+
			"eval persistence must say why that is safe — the promoter's inbox is a query over ai_runs "+
			"created_at, so scored verdicts with fresh timestamps ARE promotable rows", len(store.runs))
	}
	if len(store.extractions) != 0 {
		t.Errorf("Eval recorded %d ai_extractions row(s); criterion 24 requires none", len(store.extractions))
	}
}

// ---- criterion 23: the eval scores needs_reply, not actionable ---------------

func TestEval_InquiryLane_ScoresTheLanesDecisionField(t *testing.T) {
	// Every message asks a question and every label says needs_reply, so a
	// scorer reading the right key sees a perfect run and one reading
	// `actionable` (absent from an inquiry verdict, decoding to false) sees a
	// total miss. The two outcomes are maximally far apart on purpose.
	msgs, labels := iqLabelSet(4)
	for i := range msgs {
		msgs[i].BodyText = "can you confirm the rotation date?"
	}
	for i := range labels {
		labels[i].Label = "needs_reply"
	}

	var out bytes.Buffer
	if err := classify.Eval(context.Background(), &cfStore{pending: msgs},
		provider.NewRouter(nil, &iqEvalClient{}, time.Minute), iqEvalCfg(), labels, &out); err != nil {
		t.Fatalf("Eval: %v", err)
	}
	got := out.String()

	if !strings.Contains(got, "4 of 4") {
		t.Errorf("the inquiry eval scored something other than 4 of 4 on a set where every verdict is "+
			"needs_reply:true and every label is needs_reply:\n%s\n"+
			"Criterion 23: the eval scores the LANE's decision field. Reading `actionable` off an inquiry "+
			"verdict decodes to false on every row — a 0-of-4 that looks like a terrible prompt rather "+
			"than a scorer pointed at the wrong key", got)
	}
	if !strings.Contains(got, "0)") && !regexp.MustCompile(`(?i)false negatives \(0\)|misses \(0\)|none`).MatchString(got) {
		t.Errorf("the inquiry eval reports misses on a perfect run:\n%s", got)
	}
}
