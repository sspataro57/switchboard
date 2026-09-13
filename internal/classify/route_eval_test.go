package classify_test

// The route eval (SWT-40 Part B, B10) — Part B review finding C (2026-09-13):
// the route lane's eval branched off before the generic eval's checkpoint, so
// `--checkpoint` was silently ignored, and one unparseable verdict aborted a
// multi-hour batch. The contract now:
//
//   - per-message CHECKPOINT and resume, as the generic eval: a rerun skips the
//     finished ids, says how many it resumed, scores the full set, and removes
//     the file on success; a checkpoint written by another evaluation (a
//     generic-lane line included) is refused before any request;
//   - an unparseable verdict is SCORED as a miss and COUNTED on the `misses:`
//     line, never an abort.
//
// Fake Store + fake local client: ZERO network, ZERO Postgres, ZERO model.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/provider"
)

// reClient answers per message (keyed by a substring of the rendered user
// prompt), and can die mid-batch: after failAfter answers it cancels the run's
// context and errors, so Eval's bounded retry is skipped (no real-time pause).
type reClient struct {
	calls     int
	answers   map[string]string
	failAfter int // < 0: never
	cancel    context.CancelFunc
}

func (c *reClient) Describe() provider.Descriptor {
	return provider.Descriptor{Name: "ollama", Endpoint: "http://127.0.0.1:11434"}
}

func (c *reClient) Probe(_ context.Context) error { return nil }

func (c *reClient) Complete(_ context.Context, req provider.Request) (provider.Response, error) {
	if c.failAfter >= 0 && c.calls >= c.failAfter {
		c.cancel()
		return provider.Response{}, fmt.Errorf("provider: unavailable: batch died mid-flight")
	}
	c.calls++
	for key, raw := range c.answers {
		if strings.Contains(req.User, key) {
			return provider.Response{Raw: []byte(raw), Model: "qwen3:8b", LatencyMS: 380}, nil
		}
	}
	return provider.Response{Raw: []byte(`{"project_index":null,"evidence":"","reason":"none"}`), Model: "qwen3:8b", LatencyMS: 380}, nil
}

// reFixture is three messages on the handsonconnect SHAPE (rtCandidates: alpha
// the default, beta the other), their rules-tier labels, and answers that
// agree with every label.
func reFixture() ([]classify.PendingMessage, []classify.Label, map[string]string) {
	type row struct {
		id            int64
		subject, body string
		label, answer string
	}
	rows := []row{
		{31, "re-31 Beta Engine feed timeline", "Please confirm the Beta Engine feed timeline.", "itest-beta",
			rtVerdict("2", "Beta Engine feed timeline")},
		{32, "re-32 Alpha activity sync", "The alpha activity sync failed overnight.", "itest-alpha",
			rtVerdict("null", "")}, // no choice → the default, alpha
		{33, "re-33 Beta data pipeline", "The beta data pipeline is late.", "itest-beta",
			rtVerdict("2", "beta data pipeline")},
	}
	var msgs []classify.PendingMessage
	var labels []classify.Label
	answers := map[string]string{}
	for _, r := range rows {
		m := rtMessages(1)[0]
		m.MessageID, m.RawSourceItemID, m.Subject, m.BodyText = r.id, 3000+r.id, r.subject, r.body
		msgs = append(msgs, m)
		labels = append(labels, classify.Label{MessageID: r.id, Label: r.label,
			SubjectSHA256: classify.SubjectHash(r.subject), Stratum: "rules"})
		answers[fmt.Sprintf("re-%d ", r.id)] = r.answer
	}
	return msgs, labels, answers
}

func reCfg(ckpt string) classify.Config {
	return classify.Config{MaxTokens: 512, Lane: classify.LaneRoute, EvalCheckpoint: ckpt}
}

// MUTATION: drop the `done[m.MessageID]` skip in evalRoute → the resumed run
// re-classifies all three (calls = 3).
func TestEval_RouteLane_CheckpointResumesPastACrash(t *testing.T) {
	msgs, labels, answers := reFixture()
	ckpt := filepath.Join(t.TempDir(), "route-eval.progress")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := &reClient{answers: answers, failAfter: 1, cancel: cancel}
	var out1 bytes.Buffer
	if err := classify.Eval(ctx, &cfStore{pending: msgs}, provider.NewRouter(nil, first, time.Minute),
		reCfg(ckpt), labels, &out1); err == nil {
		t.Fatalf("the route eval survived a provider crash; the checkpoint is for a run that DIES")
	}
	raw, err := os.ReadFile(ckpt)
	if err != nil {
		t.Fatalf("no checkpoint after a crash (%v): the route eval ignores --checkpoint (review finding C)", err)
	}
	if got := strings.Count(strings.TrimSpace(string(raw)), "\n") + 1; got != 1 {
		t.Fatalf("checkpoint holds %d line(s) after 1 verdict, want 1:\n%s", got, raw)
	}

	second := &reClient{answers: answers, failAfter: -1}
	var out2 bytes.Buffer
	if err := classify.Eval(context.Background(), &cfStore{pending: msgs}, provider.NewRouter(nil, second, time.Minute),
		reCfg(ckpt), labels, &out2); err != nil {
		t.Fatalf("resumed route eval: %v", err)
	}
	text := out2.String()
	if second.calls != 2 {
		t.Errorf("the resumed run classified %d message(s), want 2: the checkpointed verdict must be skipped, "+
			"not re-bought", second.calls)
	}
	for _, want := range []string{"resumed 1 verdicts from checkpoint", "n=3 scored", "agreement 3 of 3 scored"} {
		if !strings.Contains(text, want) {
			t.Errorf("the resumed route eval does not print %q (the checkpointed verdict must count in the "+
				"full-set score):\n%s", want, text)
		}
	}
	if _, serr := os.Stat(ckpt); !os.IsNotExist(serr) {
		t.Errorf("checkpoint still exists after a successful run (stat err %v); a deliberate rerun must "+
			"re-classify", serr)
	}
}

// MUTATION: make an unparseable verdict `return` an error again → Eval errors
// and message 33 is never classified.
func TestEval_RouteLane_AnUnparseableVerdictIsACountedMissNotAnAbort(t *testing.T) {
	msgs, labels, answers := reFixture()
	answers["re-32 "] = `{"project_index": 1, "evidence": "trunc` // cut off mid-string
	client := &reClient{answers: answers, failAfter: -1}
	var out bytes.Buffer
	if err := classify.Eval(context.Background(), &cfStore{pending: msgs}, provider.NewRouter(nil, client, time.Minute),
		reCfg(""), labels, &out); err != nil {
		t.Fatalf("one unparseable verdict aborted the route eval: %v (review finding C: score it as a miss)", err)
	}
	text := out.String()
	if client.calls != 3 {
		t.Errorf("calls = %d, want 3: the batch must go on past the unparseable verdict", client.calls)
	}
	for _, want := range []string{
		"n=3 scored",
		"misses: 0 no verdict, 1 unparseable",
		"agreement 2 of 3 scored",
		"message 32  label itest-alpha  routed (unparseable)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the route eval output lacks %q:\n%s", want, text)
		}
	}
}

// A checkpoint written by the GENERIC eval (five fields, another lane's key)
// is refused before any request, never merged into the route score.
func TestEval_RouteLane_CheckpointFromAnotherEvaluationIsRefused(t *testing.T) {
	msgs, labels, answers := reFixture()
	ckpt := filepath.Join(t.TempDir(), "route-eval.progress")
	if err := os.WriteFile(ckpt, []byte("31\ttrue\t380\tqwen3:8b\tclassify/personal-v1/abc/model=/think=false/max=512/ctx=0\n"), 0o644); err != nil {
		t.Fatalf("seed checkpoint: %v", err)
	}
	client := &reClient{answers: answers, failAfter: -1}
	var out bytes.Buffer
	err := classify.Eval(context.Background(), &cfStore{pending: msgs}, provider.NewRouter(nil, client, time.Minute),
		reCfg(ckpt), labels, &out)
	if err == nil || !strings.Contains(err.Error(), "different evaluation") {
		t.Errorf("a generic-lane checkpoint was accepted by the route eval (err %v); it must be refused", err)
	}
	if client.calls != 0 {
		t.Errorf("calls = %d before the refusal, want 0", client.calls)
	}
}
