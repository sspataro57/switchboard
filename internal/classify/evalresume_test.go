package classify_test

// The eval CHECKPOINT (added after the 874-label residue eval died twice,
// hours in — first to GPU-contention timeouts, then to evalCmd's own 2-hour
// context ceiling wearing a provider costume). The contract:
//
//   - every verdict is appended to Config.EvalCheckpoint as it lands;
//   - a rerun loads the file, SKIPS the finished ids (they are not
//     re-classified — the request would be byte-identical), and says how many
//     it resumed;
//   - the file is REMOVED on success, so a deliberate rerun re-classifies
//     rather than reusing stale verdicts;
//   - an empty path disables the whole mechanism (every other test's shape).
//
// The load-bearing assertions are the CALL COUNTS: the second run must send
// exactly the messages the first run never finished, and the final numbers
// must be computed over the full set.

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

// crashClient answers like evClient until failAfter verdicts have landed, then
// cancels the run's context and errors — the shape of a batch dying mid-flight.
// Cancelling first matters: Eval's bounded retry deliberately skips a dead
// context, so the test exercises the crash path without the retry's real-time
// pause.
type crashClient struct {
	evClient
	failAfter int
	cancel    context.CancelFunc
}

func (c *crashClient) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	if c.evClient.calls >= c.failAfter {
		c.cancel()
		return provider.Response{}, fmt.Errorf("provider: unavailable: batch died mid-flight")
	}
	return c.evClient.Complete(ctx, req)
}

func TestEval_CheckpointResumesPastACrash(t *testing.T) {
	const s1 = "Your payment is due"
	const s2 = "Your statement is available"
	const s3 = "Second payment reminder"

	msgs := []classify.PendingMessage{
		evMessage(21, s1, "minimum payment $35 due 2026-09-03"),
		evMessage(22, s2, "your monthly statement is ready to view"),
		evMessage(23, s3, "final payment reminder before the late fee"),
	}
	labels := []classify.Label{
		{MessageID: 21, Label: "actionable", SubjectSHA256: evSubjectHash(s1)},
		{MessageID: 22, Label: "not", SubjectSHA256: evSubjectHash(s2)},
		{MessageID: 23, Label: "actionable", SubjectSHA256: evSubjectHash(s3)},
	}
	ckpt := filepath.Join(t.TempDir(), "eval.progress")
	cfg := stCfg(classify.LanePersonal)
	cfg.EvalCheckpoint = ckpt

	// First run: one verdict lands, then the batch dies.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := &crashClient{evClient: evClient{flagIf: "payment"}, failAfter: 1, cancel: cancel}
	var out1 bytes.Buffer
	err := classify.Eval(ctx, &cfStore{pending: msgs},
		provider.NewRouter(nil, first, time.Minute), cfg, labels, &out1)
	if err == nil {
		t.Fatalf("Eval survived a provider crash; the checkpoint is for a run that DIES")
	}
	raw, rerr := os.ReadFile(ckpt)
	if rerr != nil {
		t.Fatalf("no checkpoint after a crash: %v — the file must hold every verdict that landed, or the "+
			"next run pays for all of them again", rerr)
	}
	if got := strings.Count(strings.TrimSpace(string(raw)), "\n") + 1; got != 1 {
		t.Fatalf("checkpoint holds %d line(s) after 1 successful verdict, want 1:\n%s", got, raw)
	}

	// Second run: resumes, classifies ONLY the remainder, scores the full set,
	// and removes the checkpoint.
	second := &evClient{flagIf: "payment"}
	var out2 bytes.Buffer
	if err := classify.Eval(context.Background(), &cfStore{pending: msgs},
		provider.NewRouter(nil, second, time.Minute), cfg, labels, &out2); err != nil {
		t.Fatalf("resumed Eval: %v", err)
	}
	if second.calls != 2 {
		t.Errorf("the resumed run classified %d message(s), want 2 — the checkpointed verdict must be "+
			"SKIPPED, not re-bought; re-classifying it silently is the whole cost this mechanism removes",
			second.calls)
	}
	text := out2.String()
	if !strings.Contains(text, "resumed 1 verdicts from checkpoint") {
		t.Errorf("the resumed run does not SAY it resumed:\n%s\nAn invisible resume is a score whose "+
			"provenance nobody can state", text)
	}
	if !strings.Contains(text, "n=3 scored") {
		t.Errorf("the resumed run does not score the full set (want n=3 scored):\n%s", text)
	}
	// Both actionable messages carry "payment", so a full-set score reads
	// recall 1.00; a run that dropped the checkpointed verdict reads 0.50.
	if !strings.Contains(text, "recall    1.00") {
		t.Errorf("recall is not 1.00 over the merged set:\n%s", text)
	}
	if _, serr := os.Stat(ckpt); !os.IsNotExist(serr) {
		t.Errorf("checkpoint still exists after a successful run (stat err %v); a deliberate rerun must "+
			"re-classify, never reuse stale verdicts", serr)
	}
}

// otherModelClient answers like evClient but reports a different model — the
// CLASSIFY_MODEL-changed-between-runs shape.
type otherModelClient struct{ evClient }

func (c *otherModelClient) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	resp, err := c.evClient.Complete(ctx, req)
	resp.Model = "other-model:1b"
	return resp, err
}

func TestEval_CheckpointRefusesAModelChange(t *testing.T) {
	const s1 = "Your payment is due"
	const s2 = "Second payment reminder"
	msgs := []classify.PendingMessage{
		evMessage(31, s1, "minimum payment $35 due 2026-09-03"),
		evMessage(32, s2, "final payment reminder before the late fee"),
	}
	labels := []classify.Label{
		{MessageID: 31, Label: "actionable", SubjectSHA256: evSubjectHash(s1)},
		{MessageID: 32, Label: "actionable", SubjectSHA256: evSubjectHash(s2)},
	}
	ckpt := filepath.Join(t.TempDir(), "eval.progress")
	cfg := stCfg(classify.LanePersonal)
	cfg.EvalCheckpoint = ckpt

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := &crashClient{evClient: evClient{flagIf: "payment"}, failAfter: 1, cancel: cancel}
	var out1 bytes.Buffer
	if err := classify.Eval(ctx, &cfStore{pending: msgs},
		provider.NewRouter(nil, first, time.Minute), cfg, labels, &out1); err == nil {
		t.Fatalf("Eval survived the crash the fixture arranged")
	}

	second := &otherModelClient{evClient{flagIf: "payment"}}
	var out2 bytes.Buffer
	err := classify.Eval(context.Background(), &cfStore{pending: msgs},
		provider.NewRouter(nil, second, time.Minute), cfg, labels, &out2)
	if err == nil {
		t.Fatalf("Eval merged a checkpoint from qwen3:8b with verdicts from other-model:1b — a resume "+
			"after a model change must REFUSE, or the header lies about what the number was measured on\n%s",
			out2.String())
	}
	for _, want := range []string{"qwen3:8b", "other-model:1b"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if _, serr := os.Stat(ckpt); serr != nil {
		t.Errorf("the checkpoint was removed by a REFUSED run (%v); the verdicts it holds are still the "+
			"only record of the first model's work", serr)
	}
}
