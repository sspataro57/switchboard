package classify_test

// The eval harness (SWT-22 criteria 22 and 23). Fake Store + fake
// provider.Client — ZERO network, ZERO Postgres, ZERO live model.
//
// GREENFIELD NOTE: internal/classify does not exist, so this compile-FAILS —
// expected. Imposed surface beyond worker_test.go's:
//
//	type Label struct {
//	    MessageID     int64  `json:"message_id"`
//	    Label         string `json:"label"`          // "actionable" | "not"
//	    SubjectSHA256 string `json:"subject_sha256"` // 16 hex of sha256(NormalizedPrefix(subject,120))
//	    Note          string `json:"note,omitempty"`
//	}
//	func Eval(ctx context.Context, store Store, router *provider.Router, labels []Label, w io.Writer) error
//
// Why the two tests here are the ones worth having: the labelled set is the ONLY
// thing anyone is allowed to tune against (the SPEC's goal 4), and this fixture
// has already been wrong once — the spike's first eval scored every model
// 0.10-0.27 recall because the LABELS were wrong, not the models. So the harness
// has to refuse to score on the wrong lane, and it has to notice when a label
// stops pointing at the message it was written for.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/provider"
	"github.com/sspataro57/switchboard/internal/textmatch"
)

// evClient answers per message rather than per call index, because the harness's
// iteration order is not part of the contract and a test that depended on it
// would be pinning an implementation detail.
type evClient struct {
	calls    int
	requests []provider.Request
	// flagIf: the classifier answers actionable=true when the rendered prompt
	// contains this substring.
	flagIf string
}

func (c *evClient) Describe() provider.Descriptor {
	return provider.Descriptor{Name: "ollama", Endpoint: "http://127.0.0.1:11434"}
}

func (c *evClient) Probe(_ context.Context) error { return nil }

func (c *evClient) Complete(_ context.Context, req provider.Request) (provider.Response, error) {
	c.calls++
	c.requests = append(c.requests, req)
	v := cfNotActionable
	if c.flagIf != "" && strings.Contains(req.User, c.flagIf) {
		v = cfActionable
	}
	return provider.Response{Raw: []byte(v), Model: "qwen3:8b", LatencyMS: 380}, nil
}

// evSubjectHash is the ONE spelling of the fixture's hash: sha256 over
// textmatch.NormalizedPrefix(subject, 120), first 16 hex characters
// (criteria 21 and 23). internal/textmatch is the repo's single spelling of
// prefix normalisation; re-spelling it here or in SQL is how two hashes of the
// "same" subject stop agreeing with no error anywhere.
func evSubjectHash(subject string) string {
	sum := sha256.Sum256([]byte(textmatch.NormalizedPrefix(subject, 120)))
	return hex.EncodeToString(sum[:])[:16]
}

func evMessage(id int64, subject, body string) classify.PendingMessage {
	return classify.PendingMessage{
		MessageID:        id,
		RawSourceItemID:  1000 + id,
		SentAt:           time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		Sender:           "alerts@bank.example.test",
		Subject:          subject,
		Channel:          "gmail",
		BodyText:         body,
		Direction:        "inbound",
		ProjectID:        7,
		ProjectSlug:      "personal",
		ProjectLocalOnly: true,
		Attribution:      provider.AttrProject,
	}
}

// ---- criterion 22: it refuses to score on the hosted lane ---------------------

func TestEval_RefusesWhenRouteDoesNotReturnTheLocalLane(t *testing.T) {
	msgs := []classify.PendingMessage{
		evMessage(1, "Your payment is due", "minimum payment $35 due 2026-09-03"),
	}
	labels := []classify.Label{
		{MessageID: 1, Label: "actionable", SubjectSHA256: evSubjectHash("Your payment is due")},
	}

	t.Run("control: with a local lane it scores", func(t *testing.T) {
		local := &evClient{flagIf: "payment"}
		var out bytes.Buffer
		if err := classify.Eval(context.Background(), &cfStore{pending: msgs},
			provider.NewRouter(cfHosted(), local, time.Minute), labels, &out); err != nil {
			t.Fatalf("Eval on the local lane returned %v. Without a control the refusal below is satisfied "+
				"by an Eval that never works at all", err)
		}
		if local.calls != 1 {
			t.Fatalf("local client calls = %d, want 1", local.calls)
		}
		if !regexp.MustCompile(`(?i)recall`).MatchString(out.String()) {
			t.Errorf("the eval output never prints recall:\n%s", out.String())
		}
	})

	t.Run("no local lane: refuse, send nothing", func(t *testing.T) {
		general := cfHosted()
		var out bytes.Buffer
		err := classify.Eval(context.Background(), &cfStore{pending: msgs},
			provider.NewRouter(general, nil, time.Minute), labels, &out)
		if err == nil {
			t.Errorf("Eval succeeded with no local lane. Criterion 22: it must REFUSE (non-zero exit, " +
				"nothing sent) — an eval on the hosted lane is both a leak of the whole labelled corpus and " +
				"a meaningless number, since the model being scored is not the model that would run")
		}
		if general.calls != 0 {
			t.Errorf("the hosted client recorded %d call(s) during a refused eval; 'nothing sent' is half "+
				"the criterion", general.calls)
		}
	})
}

// ---- criterion 23: label drift is reported and EXCLUDED -----------------------

// The labels ARE the fixture. This fixture has already been wrong once, and a
// silently re-pointed id would move the score with no visible cause — which is
// indistinguishable from the prompt getting better or worse.
//
// The load-bearing assertion is the CALL COUNT, not the wording: a drifted id
// must be excluded BEFORE it is classified, so it cannot contribute to recall by
// any path. That assertion survives any rewording of the report.
func TestEval_ReportsAndExcludesLabelDrift(t *testing.T) {
	const goodSubject = "Your payment is due"
	const driftedSubject = "Now: your statement is available"

	msgs := []classify.PendingMessage{
		evMessage(1, goodSubject, "minimum payment $35 due 2026-09-03"),
		// Message 2 no longer says what its label was written against — the id was
		// re-pointed by a re-normalisation, or the label was typed from the wrong row.
		evMessage(2, driftedSubject, "your monthly statement is ready to view"),
	}
	labels := []classify.Label{
		{MessageID: 1, Label: "actionable", SubjectSHA256: evSubjectHash(goodSubject)},
		{MessageID: 2, Label: "actionable", SubjectSHA256: evSubjectHash("HOA violation notice — first notice")},
		// A label whose message is not in the db at all: also excluded, and for the
		// same reason — a score computed over a set that is missing rows silently
		// is a score nobody can reproduce.
		{MessageID: 999, Label: "not", SubjectSHA256: evSubjectHash("deleted message")},
	}

	local := &evClient{flagIf: "payment"}
	var out bytes.Buffer
	if err := classify.Eval(context.Background(), &cfStore{pending: msgs},
		provider.NewRouter(nil, local, time.Minute), labels, &out); err != nil {
		t.Fatalf("Eval: %v", err)
	}

	if local.calls != 1 {
		t.Errorf("the classifier ran %d time(s), want 1. A label whose subject hash disagrees must be "+
			"EXCLUDED, not merely annotated: if message 2 is still classified, it still lands in the "+
			"numerator or the denominator of a recall figure that nobody can reproduce", local.calls)
	}
	text := out.String()
	if !strings.Contains(text, "2") || !regexp.MustCompile(`(?i)drift|exclud|mismatch`).MatchString(text) {
		t.Errorf("the eval output does not name the drifted id and say it was excluded:\n%s\n"+
			"Criterion 23: it PRINTS and EXCLUDES — an exclusion nobody sees is a silently shrinking "+
			"fixture", text)
	}
	if !strings.Contains(text, "999") {
		t.Errorf("the eval output does not name the missing id 999:\n%s", text)
	}
	// n must reflect what was actually scored, not what was in the file.
	if regexp.MustCompile(`(?i)\bn\b\D{0,12}3\b`).MatchString(text) {
		t.Errorf("the eval output reports n=3 while only one label survived drift checking:\n%s", text)
	}
}

// ---- criterion 22: recall, precision, n, latency, and the MISSES by id --------

// Recall is the objective (0.90 vs 0.80 is why this model was chosen), so the
// false negatives are the output that matters — a score without them tells an
// operator that something is wrong and nothing about what.
func TestEval_PrintsRecallPrecisionAndEveryFalseNegativeByID(t *testing.T) {
	const paySubject = "Your payment is due"
	const hoaSubject = "[#XN123456] Message from Pines Association - First Notice"
	const stmtSubject = "Your statement is available"

	msgs := []classify.PendingMessage{
		evMessage(11, paySubject, "minimum payment $35 due 2026-09-03"),
		evMessage(12, hoaSubject, "please see attachment for additional detail"),
		evMessage(13, stmtSubject, "your monthly statement is ready to view"),
	}
	labels := []classify.Label{
		{MessageID: 11, Label: "actionable", SubjectSHA256: evSubjectHash(paySubject)},
		{MessageID: 12, Label: "actionable", SubjectSHA256: evSubjectHash(hoaSubject), Note: "fine + cure-by date live in the attachment"},
		{MessageID: 13, Label: "not", SubjectSHA256: evSubjectHash(stmtSubject)},
	}

	// The classifier flags only the payment notice: 1 of 2 actionable caught, no
	// false positives. Recall 0.5, precision 1.0, one false negative — message 12,
	// which is exactly the class hosted models missed 3 of 5 times.
	local := &evClient{flagIf: "payment"}
	var out bytes.Buffer
	if err := classify.Eval(context.Background(), &cfStore{pending: msgs},
		provider.NewRouter(nil, local, time.Minute), labels, &out); err != nil {
		t.Fatalf("Eval: %v", err)
	}
	text := out.String()

	for _, want := range []string{"recall", "precision", "latency"} {
		if !regexp.MustCompile(`(?i)` + want).MatchString(text) {
			t.Errorf("the eval output never mentions %q (criterion 22 lists recall, precision, n, median "+
				"latency and the false negatives):\n%s", want, text)
		}
	}
	if !regexp.MustCompile(`(?i)false negative`).MatchString(text) {
		t.Errorf("the eval output has no false-negative section:\n%s\nRecall is the objective, so the "+
			"MISSES are the output that matters — a missed payment notice is a late fee", text)
	}
	// The misses must be named by ID, in the false-negative section — not merely
	// counted. Everything from the section heading onward is searched, so the
	// exact layout is free to change.
	i := regexp.MustCompile(`(?i)false negative`).FindStringIndex(text)
	if i == nil {
		t.Fatalf("no false-negative section to search:\n%s", text)
	}
	tail := text[i[0]:]
	if !strings.Contains(tail, "12") {
		t.Errorf("message 12 — labelled actionable, classified not-actionable — is not named in the "+
			"false-negative section:\n%s\nThat is the HOA violation class hosted models missed 3 of 5 "+
			"times; a recall number without the ids tells an operator that something is wrong and nothing "+
			"about what", tail)
	}
	if strings.Contains(tail, "11") {
		t.Errorf("message 11 was labelled actionable AND classified actionable, yet appears in the "+
			"false-negative section:\n%s", tail)
	}
	if local.calls != 3 {
		t.Errorf("the classifier ran %d time(s), want 3 — every label whose hash agrees is scored", local.calls)
	}
}
