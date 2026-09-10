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
	// 2 of 2; a run that dropped the checkpointed verdict reads 1 of 2. (Counts,
	// since SWT-33: n=3 is below classify.EvalResultThreshold.)
	if !strings.Contains(text, "2 of 2 labelled actionable caught") {
		t.Errorf("recall is not 2 of 2 over the merged set:\n%s", text)
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

// Codex adversarial review, round 3 (SWT-33): a checkpoint is bound to the
// evaluation that wrote it — lane worker_type, prompt version, thinking knob.
// Resumed under ANOTHER lane, its verdicts answer a different question; merged
// silently, they would be scored as this lane's measurement, and with every id
// checkpointed not a single request (and so no model check) would run.
func TestEval_CheckpointFromAnotherLaneIsRefused(t *testing.T) {
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

	// A PERSONAL-lane run lands one verdict, then dies.
	personal := stCfg(classify.LanePersonal)
	personal.EvalCheckpoint = ckpt
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := &crashClient{evClient: evClient{flagIf: "payment"}, failAfter: 1, cancel: cancel}
	_ = classify.Eval(ctx, &cfStore{pending: msgs}, provider.NewRouter(nil, first, time.Minute),
		personal, labels, &bytes.Buffer{})
	if _, err := os.Stat(ckpt); err != nil {
		t.Fatalf("POSITIVE CONTROL FAILED: no checkpoint after the crash (%v)", err)
	}

	// The same file resumed as the RESIDUE lane is refused, before any request.
	residue := stCfg(classify.LaneResidue)
	residue.EvalCheckpoint = ckpt
	other := &evClient{flagIf: "payment"}
	err := classify.Eval(context.Background(), &cfStore{pending: msgs},
		provider.NewRouter(nil, other, time.Minute), residue, labels, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "different evaluation") {
		t.Fatalf("a personal-lane checkpoint resumed under the residue lane returned %v; want a refusal naming "+
			"a different evaluation", err)
	}
	if other.calls != 0 {
		t.Errorf("the refused resume sent %d request(s); it must refuse before classifying anything", other.calls)
	}

	// Control: the SAME lane resumes it normally.
	again := &evClient{flagIf: "payment"}
	var out bytes.Buffer
	if err := classify.Eval(context.Background(), &cfStore{pending: msgs},
		provider.NewRouter(nil, again, time.Minute), personal, labels, &out); err != nil {
		t.Fatalf("CONTROL FAILED: the personal lane could not resume its own checkpoint: %v", err)
	}
	if again.calls != 1 || !strings.Contains(out.String(), "resumed 1 verdicts") {
		t.Errorf("the same-lane resume sent %d request(s), want 1, and must say it resumed:\n%s", again.calls, out.String())
	}
}

// A checkpoint written before lines carried their evaluation key (four
// tab-separated fields) cannot prove which evaluation produced it, so it is
// refused rather than trusted — failing closed costs one rescore. Pins that
// backward-compatibility decision (go-reviewer, final pass).
func TestEval_UnkeyedLegacyCheckpointIsRefused(t *testing.T) {
	const s1 = "Your payment is due"
	msgs := []classify.PendingMessage{evMessage(51, s1, "minimum payment $35 due 2026-09-03")}
	labels := []classify.Label{{MessageID: 51, Label: "actionable", SubjectSHA256: evSubjectHash(s1)}}
	ckpt := filepath.Join(t.TempDir(), "eval.progress")
	if err := os.WriteFile(ckpt, []byte("51\ttrue\t380\tqwen3:8b\n"), 0o600); err != nil {
		t.Fatalf("write legacy checkpoint: %v", err)
	}
	cfg := stCfg(classify.LanePersonal)
	cfg.EvalCheckpoint = ckpt
	client := &evClient{flagIf: "payment"}
	err := classify.Eval(context.Background(), &cfStore{pending: msgs},
		provider.NewRouter(nil, client, time.Minute), cfg, labels, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "unbound") {
		t.Fatalf("an unkeyed (4-field) checkpoint returned %v; want a refusal saying it is unbound", err)
	}
	if client.calls != 0 {
		t.Errorf("the refused resume sent %d request(s); it must refuse before classifying anything", client.calls)
	}
}

// Codex adversarial review, round 4: the key also binds the CONFIGURED model
// and the output-shaping budget. The server-reported-model check only runs
// after a fresh request, so a fully checkpointed file resumed under a changed
// model would otherwise publish the old model's verdicts as a current score.
// The key is checked at load, before any request — which is what covers the
// fully checkpointed case — so a partial checkpoint proves the same path.
func TestEval_CheckpointFromAnotherModelOrBudgetIsRefused(t *testing.T) {
	const s1 = "Your payment is due"
	const s2 = "Second payment reminder"
	msgs := []classify.PendingMessage{
		evMessage(41, s1, "minimum payment $35 due 2026-09-03"),
		evMessage(42, s2, "final payment reminder before the late fee"),
	}
	labels := []classify.Label{
		{MessageID: 41, Label: "actionable", SubjectSHA256: evSubjectHash(s1)},
		{MessageID: 42, Label: "actionable", SubjectSHA256: evSubjectHash(s2)},
	}
	for _, tc := range []struct {
		name   string
		change func(*classify.Config)
	}{
		{"model", func(c *classify.Config) { c.Model = "other-model:1b" }},
		{"max tokens", func(c *classify.Config) { c.MaxTokens = 2048 }},
		{"num ctx", func(c *classify.Config) { c.NumCtx = 8192 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ckpt := filepath.Join(t.TempDir(), "eval.progress")
			cfg := stCfg(classify.LanePersonal)
			cfg.Model = "qwen3:8b"
			cfg.EvalCheckpoint = ckpt
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			first := &crashClient{evClient: evClient{flagIf: "payment"}, failAfter: 1, cancel: cancel}
			_ = classify.Eval(ctx, &cfStore{pending: msgs}, provider.NewRouter(nil, first, time.Minute),
				cfg, labels, &bytes.Buffer{})
			if _, err := os.Stat(ckpt); err != nil {
				t.Fatalf("POSITIVE CONTROL FAILED: no checkpoint after the crash (%v)", err)
			}

			changed := cfg
			tc.change(&changed)
			second := &evClient{flagIf: "payment"}
			err := classify.Eval(context.Background(), &cfStore{pending: msgs},
				provider.NewRouter(nil, second, time.Minute), changed, labels, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), "different evaluation") {
				t.Fatalf("a checkpoint resumed after changing the %s returned %v; want a refusal", tc.name, err)
			}
			if second.calls != 0 {
				t.Errorf("the refused resume sent %d request(s); it must refuse before classifying anything",
					second.calls)
			}
		})
	}
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
