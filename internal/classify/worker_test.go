package classify_test

// Unit tests for the `classify` worker (SWT-22, acceptance criteria 14, 15, 16,
// 17's unit half, and 18's PromptVersion bookkeeping). Fake Store + fake
// provider.Client — ZERO network, ZERO Postgres, ZERO live model. The SQL half
// (criterion 11's inbox filter, criterion 12's four population shapes,
// criterion 17's two lanes) is in store_integration_test.go, deliberately: any
// assertion about a value that comes from a database COLUMN belongs in a test
// that makes Postgres produce it (SWT-21's 6th landmine instance).
//
// GREENFIELD NOTE: package internal/classify does not exist yet, so this file
// compile-FAILS under `go test ./...` — the expected red state. For greenfield
// code the SPEC's contract IS the signature. Imposed surface, from the SPEC's
// "Internal Go surface added" block plus the sibling shapes it names
// (internal/triage/triage.go for the worker, internal/drafts/store.go for the
// one-query fold):
//
//	const PromptVersion = "classify-v1"   // value not pinned here, only its presence
//	const SchemaName    = "classify_verdict"
//	const AdvisoryLockKey = 0x5157_0022
//	var  SystemPrompt   string
//	var  VerdictSchema  json.RawMessage
//
//	type Config struct {
//	    Model     string
//	    MaxTokens int
//	    Limit     int           // 0 = all pending
//	    Since     time.Duration // 0 = no lower bound on sent_at
//	}
//
//	type NeighbourClass struct {
//	    State     provider.AttributionState
//	    LocalOnly bool
//	}
//
//	// One row of criterion 11's inbox, with its class inputs folded in the SAME
//	// query as its body (drafts' pattern, which SWT-21's Deviation 9 names as
//	// the better one — class and project cannot disagree if they are one read).
//	type PendingMessage struct {
//	    MessageID       int64
//	    RawSourceItemID int64
//	    ThreadID        int64
//	    SentAt          time.Time
//	    Sender, Subject, Channel, BodyText, Direction string
//	    ProjectID        int64
//	    ProjectSlug      string
//	    ProjectLocalOnly bool
//	    Attribution      provider.AttributionState
//	    Neighbours       []NeighbourClass  // INBOUND thread neighbours only
//	}
//
//	type AIRun struct {
//	    WorkerType, Provider, Model, Status string
//	    Input, Output json.RawMessage
//	    PromptTokens, CompletionTokens, LatencyMS int
//	}
//
//	// SHADOW GUARANTEE: no task-write method (criterion 15).
//	type Store interface {
//	    PendingMessages(ctx context.Context, cfg Config) ([]PendingMessage, error)
//	    MessagesByID(ctx context.Context, cfg Config, ids []int64) ([]PendingMessage, error)  // cfg: SWT-23
//	    RecordRun(ctx context.Context, run AIRun) (aiRunID int64, err error)
//	    RecordExtraction(ctx context.Context, aiRunID, rawSourceItemID int64, fields json.RawMessage) error
//	}
//
//	type Stats struct{ Processed, Flagged, Skipped, Errors int }
//	func Run(ctx context.Context, store Store, router *provider.Router, cfg Config) (Stats, error)
//	func NewStore(pool *pgxpool.Pool) *PGStore
//
// THE HONESTY LABEL (criterion 13), because it governs what is NOT in this file.
// Criterion 11 selects only projects with ai_locality='local_only', so EVERY
// message this worker sees is provider.ClassRestricted BY CONSTRUCTION. There is
// therefore no unit test here that "proves the class fold works" — such a test
// would supply the very value it then asserts on, which is this repo's
// seventh-instance landmine (a predicate whose discriminating column is a
// constant in production). The fixtures below are shaped like PRODUCTION —
// AttrProject + LocalOnly — and the two things that actually protect this worker
// are pinned elsewhere: the inbox filter, in store_integration_test.go, and the
// Router's refusal, in TestRun_NoLocalProvider_* below, which asserts ZERO
// hosted calls and has a control proving the same fixture CAN be classified.

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/provider"
)

// ---- fakes -------------------------------------------------------------------

type cfClient struct {
	desc     provider.Descriptor
	calls    int
	probes   int
	requests []provider.Request
	// perCall[i] is the error returned by the i-th Complete; nil (or past the
	// end) returns a canned, schema-valid verdict.
	perCall  []error
	verdict  string
	probeErr error
}

func (c *cfClient) Describe() provider.Descriptor { return c.desc }

func (c *cfClient) Probe(_ context.Context) error {
	c.probes++
	return c.probeErr
}

func (c *cfClient) Complete(_ context.Context, req provider.Request) (provider.Response, error) {
	i := c.calls
	c.calls++
	c.requests = append(c.requests, req)
	if i < len(c.perCall) && c.perCall[i] != nil {
		return provider.Response{}, c.perCall[i]
	}
	v := c.verdict
	if v == "" {
		v = cfNotActionable
	}
	return provider.Response{
		Raw:              json.RawMessage(v),
		Model:            "qwen3:8b",
		PromptTokens:     40,
		CompletionTokens: 25,
		LatencyMS:        380,
	}, nil
}

const (
	cfActionable    = `{"actionable":true,"kind":"payment_due","title":"Card payment due 2026-09-03","reason":"names an amount and a due date"}`
	cfNotActionable = `{"actionable":false,"kind":"informational","title":"Statement available","reason":"informational, nothing to do"}`
)

// cfLocal is the lane this worker is BUILT for: an endpoint that classifies
// local, and a Probe that answers. A local client that cannot demonstrate
// reachability is UNREACHABLE, not ready (router.probe) — a fake without Probe
// would make this whole file skip and pass while exercising nothing.
func cfLocal() *cfClient {
	return &cfClient{desc: provider.Descriptor{Name: "ollama", Endpoint: "http://127.0.0.1:11434"}}
}

func cfHosted() *cfClient {
	return &cfClient{desc: provider.Descriptor{Name: "openai", Endpoint: "https://api.openai.com/v1"}}
}

type cfStore struct {
	pending     []classify.PendingMessage
	runs        []classify.AIRun
	extractions []cfExtraction
	nextRunID   int64
}

type cfExtraction struct {
	aiRunID         int64
	rawSourceItemID int64
	fields          json.RawMessage
}

func (s *cfStore) PendingMessages(_ context.Context, cfg classify.Config) ([]classify.PendingMessage, error) {
	if cfg.Limit > 0 && cfg.Limit < len(s.pending) {
		return s.pending[:cfg.Limit], nil
	}
	return s.pending, nil
}

// MessagesByID takes the Config since SWT-23: the residue lane loads by id with
// NO action and NO project predicate (criterion 17), while the personal lane
// keeps its local_only join, so the loader has to know which lane is asking.
func (s *cfStore) MessagesByID(_ context.Context, _ classify.Config, ids []int64) ([]classify.PendingMessage, error) {
	byID := map[int64]classify.PendingMessage{}
	for _, m := range s.pending {
		byID[m.MessageID] = m
	}
	var out []classify.PendingMessage
	for _, id := range ids {
		if m, ok := byID[id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *cfStore) RecordRun(_ context.Context, run classify.AIRun) (int64, error) {
	s.nextRunID++
	s.runs = append(s.runs, run)
	return s.nextRunID, nil
}

func (s *cfStore) RecordExtraction(_ context.Context, aiRunID, rawSourceItemID int64, fields json.RawMessage) error {
	s.extractions = append(s.extractions, cfExtraction{aiRunID, rawSourceItemID, fields})
	return nil
}

func (s *cfStore) withStatus(status string) []classify.AIRun {
	var out []classify.AIRun
	for _, r := range s.runs {
		if r.Status == status {
			out = append(out, r)
		}
	}
	return out
}

// cfMessages builds n messages in the shape criterion 11's filter ACTUALLY
// yields: inbound, positively attributed, to a project whose ai_locality is
// 'local_only'. Not a convenience — a fixture that varied the attribution here
// would be inventing a population this worker's inbox cannot contain, and every
// assertion made on it would be about that invention (criterion 13).
func cfMessages(n int) []classify.PendingMessage {
	out := make([]classify.PendingMessage, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, classify.PendingMessage{
			MessageID:        int64(i),
			RawSourceItemID:  int64(1000 + i),
			ThreadID:         int64(i),
			SentAt:           time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
			Sender:           "alerts@bankofamerica.example.test",
			Subject:          "Your payment is due",
			Channel:          "gmail",
			BodyText:         "Minimum payment $35 due 2026-09-03 on account ending 1234",
			Direction:        "inbound",
			ProjectID:        7,
			ProjectSlug:      "personal",
			ProjectLocalOnly: true,
			Attribution:      provider.AttrProject,
		})
	}
	return out
}

// cfCfg is the PERSONAL lane's config. The lane is named explicitly because
// SWT-23 criterion 10 refuses a zero-value Config.Lane rather than defaulting
// it — a helper that left it unset would make every test in this file assert
// against whatever the default happened to be.
func cfCfg() classify.Config {
	return classify.Config{Model: "qwen3:8b", MaxTokens: 512, Lane: classify.LanePersonal}
}

func cfDecode(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("ai_runs.input is not a JSON object: %v (%s)", err, raw)
	}
	return m
}

// ---- criterion 14: the Router's refusal, with its control ---------------------

// The headline for this worker: restricted content NEVER reaches the general
// client. The two subtests together are the assertion — the control proves the
// fixture CAN be classified, so "zero hosted calls" means "the boundary refused"
// rather than "nothing happened". SWT-21 shipped an end-to-end test that passed
// with its guard fully inert because its fixture skipped for an unrelated
// reason; the control is what stops that repeating.
func TestRun_NoLocalProvider_MakesZeroHostedCalls(t *testing.T) {
	t.Run("control: with a local lane the same fixture IS classified", func(t *testing.T) {
		local := cfLocal()
		store := &cfStore{pending: cfMessages(3)}

		stats, err := classify.Run(context.Background(), store,
			provider.NewRouter(cfHosted(), local, time.Minute), cfCfg())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if local.calls != 3 || stats.Processed != 3 {
			t.Fatalf("local calls = %d, stats = %+v; want 3 classified. If this fixture cannot be processed "+
				"at all, the zero-hosted-calls assertion below proves nothing — it would be satisfied by a "+
				"worker that does nothing", local.calls, stats)
		}
		if len(store.extractions) != 3 {
			t.Errorf("%d extractions recorded, want 3", len(store.extractions))
		}
	})

	t.Run("no local lane: nothing is sent anywhere", func(t *testing.T) {
		general := cfHosted()
		store := &cfStore{pending: cfMessages(3)}

		stats, err := classify.Run(context.Background(), store,
			provider.NewRouter(general, nil, time.Minute), cfCfg())
		if err != nil {
			t.Fatalf("Run returned an error for a fully skipped pass: %v. A refusal is the boundary working, "+
				"not a fault: exit zero, retry next pass", err)
		}
		if general.calls != 0 {
			t.Errorf("the hosted client recorded %d Complete call(s). This is the leak the whole boundary "+
				"exists to prevent: bank, HOA and health mail bodies posted to a hosted API. There is no "+
				"code path in this worker from restricted content to the general client", general.calls)
		}
		if len(store.extractions) != 0 {
			t.Errorf("%d ai_extractions written for skipped messages; criterion 16: no skip of any kind "+
				"writes an extraction — that is what leaves the message in the inbox for the next pass",
				len(store.extractions))
		}
		if stats.Skipped != 3 || stats.Processed != 0 || stats.Errors != 0 {
			t.Errorf("stats = %+v, want 3 skipped / 0 processed / 0 errors", stats)
		}
	})
}

// ---- criterion 16: skip semantics, reused from triage rather than reinvented --

func TestRun_RouteRefusal_WritesOneAggregateRowPerPass(t *testing.T) {
	store := &cfStore{pending: cfMessages(4)}

	if _, err := classify.Run(context.Background(), store,
		provider.NewRouter(cfHosted(), nil, time.Minute), cfCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	skips := store.withStatus("skipped")
	if len(skips) != 1 {
		t.Fatalf("recorded %d skipped ai_runs rows, want exactly 1. Criterion 16 reuses triage's shape: ONE "+
			"aggregate row per pass when the lane is refused, because a refused lane refuses the whole inbox "+
			"identically — a row per message is SWT-17's amplification landmine (~1,609 rows per pass, "+
			"forever, saying nothing the aggregate does not)", len(skips))
	}
	if len(store.runs) != 1 {
		t.Errorf("recorded %d ai_runs rows in total, want 1; a fully skipped pass must not also write "+
			"ok/error rows", len(store.runs))
	}

	rec := skips[0]
	if rec.WorkerType != "classify" {
		t.Errorf("worker_type = %q, want %q. It is worker_type that keeps this worker's rows and triage's "+
			"from seeing each other — triage's NOT EXISTS keys on 'triage' and criterion 11's on 'classify'",
			rec.WorkerType, "classify")
	}
	in := cfDecode(t, rec.Input)
	if got := in["avail_reason"]; got != "no_local_provider" {
		t.Errorf("input.avail_reason = %v, want %q — the SPEC's vocabulary, which `classify report` groups "+
			"by and an operator reads", got, "no_local_provider")
	}
	if _, ok := in["avail_reasons"]; !ok {
		t.Errorf("input.avail_reasons is missing. A pass can refuse for more than one reason (the probe TTL " +
			"expiring mid-pass makes no_local_provider and local_unreachable coexist), and filing the whole " +
			"count under the dominant one prints a wrong number where an operator looks")
	}
	if got, ok := in["skipped_count"].(float64); !ok || int(got) != 4 {
		t.Errorf("input.skipped_count = %v, want 4", in["skipped_count"])
	}
	ids, ok := in["message_ids"].([]any)
	if !ok || len(ids) != 4 {
		t.Errorf("input.message_ids = %v, want the 4 skipped ids", in["message_ids"])
	}
	if _, ok := in["sampled"]; !ok {
		t.Errorf("input.sampled is missing; without it a truncated id list is indistinguishable from a complete one")
	}
	// class_reasons is required by criterion 16 and it is CONSTANT here — every
	// message in this worker's inbox is restricted for the same reason
	// (project_local_only), by construction. Asserted for presence and shape
	// only; it cannot discriminate anything and is not pretending to.
	if _, ok := in["class_reasons"].(map[string]any); !ok {
		t.Errorf("input.class_reasons missing from %v", in)
	}
}

func TestRun_SkipRecordIsBounded(t *testing.T) {
	store := &cfStore{pending: cfMessages(150)}

	if _, err := classify.Run(context.Background(), store,
		provider.NewRouter(cfHosted(), nil, time.Minute), cfCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	skips := store.withStatus("skipped")
	if len(skips) != 1 {
		t.Fatalf("recorded %d skipped rows for 150 skipped messages, want 1", len(skips))
	}
	in := cfDecode(t, skips[0].Input)
	if got, ok := in["skipped_count"].(float64); !ok || int(got) != 150 {
		t.Errorf("input.skipped_count = %v, want 150 — the COUNT is unbounded, only the id list is capped",
			in["skipped_count"])
	}
	ids, _ := in["message_ids"].([]any)
	if len(ids) > 100 || len(ids) == 0 {
		t.Errorf("input.message_ids has %d entries, want 1..100 (criterion 16). A sample is what makes the "+
			"row actionable; an unbounded list reintroduces by the back door the volume the aggregate exists "+
			"to avoid", len(ids))
	}
}

// Tier 1: ErrUnavailable is NORMAL OPERATION for a 5.9 GB model sharing a card
// with a desktop. Per-message skip, its own row, and it does NOT count toward
// the ratio.
func TestRun_LocalUnreachable_SkipsTheMessageNotThePass(t *testing.T) {
	local := cfLocal()
	local.perCall = []error{nil, fmt.Errorf("post ollama: %w", provider.ErrUnavailable), nil}
	general := cfHosted()
	store := &cfStore{pending: cfMessages(3)}

	stats, err := classify.Run(context.Background(), store,
		provider.NewRouter(general, local, time.Minute), cfCfg())
	if err != nil {
		t.Fatalf("Run returned an error because the local box was busy: %v. A cold load is 4.04s and the "+
			"card is shared with a desktop — a pass that raises whenever that happens trains its operator "+
			"to ignore it", err)
	}
	if general.calls != 0 {
		t.Errorf("the hosted client recorded %d call(s) after the local lane failed. 'Try local, fall back "+
			"to the configured provider' is the one change this boundary exists to prevent", general.calls)
	}
	if stats.Skipped != 1 || stats.Processed != 2 || stats.Errors != 0 {
		t.Errorf("stats = %+v, want 1 skipped / 2 processed / 0 errors", stats)
	}
	if len(store.extractions) != 2 {
		t.Errorf("%d extractions written, want 2 (the skipped message writes none, so it stays in the inbox)",
			len(store.extractions))
	}
	skips := store.withStatus("skipped")
	if len(skips) != 1 {
		t.Fatalf("recorded %d skipped rows, want 1 (the per-message tier)", len(skips))
	}
	in := cfDecode(t, skips[0].Input)
	if got := in["avail_reason"]; got != "local_unreachable" {
		t.Errorf("input.avail_reason = %v, want %q", got, "local_unreachable")
	}
	if got, ok := in["normalized_message_id"].(float64); !ok || int64(got) != 2 {
		t.Errorf("the per-message skip does not name its message (normalized_message_id = %v, want 2); "+
			"without the id the record cannot be reconciled against the inbox", in["normalized_message_id"])
	}
}

// Tier 2: anything else is an `unclassified_error` — the message skips, and one
// such failure does not fail the pass.
func TestRun_UnclassifiedError_SkipsTheMessageAndDoesNotFailThePass(t *testing.T) {
	local := cfLocal()
	local.perCall = []error{nil, fmt.Errorf("ollama HTTP 404: 404 page not found"), nil}
	general := cfHosted()
	store := &cfStore{pending: cfMessages(3)}

	stats, err := classify.Run(context.Background(), store,
		provider.NewRouter(general, local, time.Minute), cfCfg())
	if err != nil {
		t.Fatalf("one malformed response failed the whole pass: %v. A broken adapter fails on EVERYTHING "+
			"and is news; one bad message is not", err)
	}
	if general.calls != 0 {
		t.Errorf("the hosted client recorded %d call(s) after a local error", general.calls)
	}
	if stats.Skipped != 1 || stats.Processed != 2 {
		t.Errorf("stats = %+v, want 1 skipped / 2 processed", stats)
	}
	if len(store.extractions) != 2 {
		t.Errorf("%d extractions written, want 2", len(store.extractions))
	}
	skips := store.withStatus("skipped")
	if len(skips) != 1 {
		t.Fatalf("recorded %d skipped rows, want 1", len(skips))
	}
	if got := cfDecode(t, skips[0].Input)["avail_reason"]; got != "unclassified_error" {
		t.Errorf("input.avail_reason = %v, want %q — merge this with local_unreachable and criterion 16's "+
			"ratio can never be computed", got, "unclassified_error")
	}
}

// Tier 3: the PASS raises on the PATTERN — unclassified errors above HALF the
// restricted-lane attempts, with a floor of 20. The thresholds are triage's and
// they are guesses; what these cases pin is the SHAPE: a proportion not a count,
// a floor so a tiny pass cannot cry wolf, and unreachable skips excluded because
// they are normal operation.
func TestRun_UnclassifiedErrorRatio_RaisesOnlyOnThePattern(t *testing.T) {
	cases := []struct {
		name     string
		attempts int
		failures int
		failWith func() error
		wantErr  bool
		why      string
	}{
		{"majority unclassified over the floor", 30, 16,
			func() error { return fmt.Errorf("ollama HTTP 500: boom") }, true,
			"a genuinely broken local adapter fails on everything; that is news"},
		{"exactly half", 30, 15,
			func() error { return fmt.Errorf("ollama HTTP 500: boom") }, false,
			"'exceeds half' — half is not more than half"},
		{"below the 20-attempt floor", 19, 19,
			func() error { return fmt.Errorf("ollama HTTP 500: boom") }, false,
			"a tiny pass where everything failed must not cry wolf"},
		{"all unreachable, well over half", 30, 30,
			func() error { return fmt.Errorf("post ollama: %w", provider.ErrUnavailable) }, false,
			"ErrUnavailable is a busy box, and a busy box must never raise no matter how many messages it " +
				"refuses — that is the whole tier-1/tier-2 distinction"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			local := cfLocal()
			local.perCall = make([]error, tc.attempts)
			for i := 0; i < tc.failures; i++ {
				local.perCall[i] = tc.failWith()
			}
			store := &cfStore{pending: cfMessages(tc.attempts)}

			_, err := classify.Run(context.Background(), store,
				provider.NewRouter(cfHosted(), local, time.Minute), cfCfg())
			if tc.wantErr && err == nil {
				t.Errorf("Run returned no error with %d/%d unclassified failures — %s",
					tc.failures, tc.attempts, tc.why)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Run returned %v with %d/%d failures — %s", err, tc.failures, tc.attempts, tc.why)
			}
			if len(store.extractions) != tc.attempts-tc.failures {
				t.Errorf("%d extractions, want %d — a skip of any kind writes none",
					len(store.extractions), tc.attempts-tc.failures)
			}
		})
	}
}

// ---- criterion 17's unit half + 18's bookkeeping ------------------------------

// "The classifier looked and found nothing" is status='ok' PLUS an extraction
// row — and it is that extraction that removes the message from the inbox. The
// integration test asserts the disappearance; this one asserts the two rows and
// the request that produced them.
func TestRun_ClassifiedMessage_RecordsAnOkRunAndAnExtraction(t *testing.T) {
	local := cfLocal()
	local.verdict = cfActionable
	store := &cfStore{pending: cfMessages(1)}

	stats, err := classify.Run(context.Background(), store,
		provider.NewRouter(cfHosted(), local, time.Minute), cfCfg())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Processed != 1 || stats.Flagged != 1 {
		t.Errorf("stats = %+v, want 1 processed and 1 flagged (the canned verdict is actionable)", stats)
	}

	oks := store.withStatus("ok")
	if len(oks) != 1 {
		t.Fatalf("recorded %d status='ok' rows, want 1", len(oks))
	}
	run := oks[0]
	if run.WorkerType != "classify" {
		t.Errorf("worker_type = %q, want %q", run.WorkerType, "classify")
	}
	// The provider column names the LANE THAT SERVED, from Describe().Name — not
	// a constant. Hardcoding it would make the audit trail answer the one
	// question it exists to answer, wrongly.
	if run.Provider != "ollama" {
		t.Errorf("ai_runs.provider = %q, want %q (the serving client's Describe().Name)", run.Provider, "ollama")
	}
	if run.Model != "qwen3:8b" {
		t.Errorf("ai_runs.model = %q, want the configured local model", run.Model)
	}
	if run.PromptTokens != 40 || run.CompletionTokens != 25 || run.LatencyMS != 380 {
		t.Errorf("usage not carried through: tokens=%d/%d latency=%d",
			run.PromptTokens, run.CompletionTokens, run.LatencyMS)
	}

	in := cfDecode(t, run.Input)
	if got, _ := in["prompt_version"].(string); got != classify.PromptVersion {
		t.Errorf("input.prompt_version = %v, want %q — triage's convention, and the only thing that makes a "+
			"prompt change re-sweepable later", in["prompt_version"], classify.PromptVersion)
	}
	if got, ok := in["normalized_message_id"].(float64); !ok || int64(got) != 1 {
		t.Errorf("input.normalized_message_id = %v, want 1", in["normalized_message_id"])
	}

	if len(store.extractions) != 1 {
		t.Fatalf("recorded %d extractions, want 1", len(store.extractions))
	}
	ext := store.extractions[0]
	if ext.rawSourceItemID != 1001 {
		t.Errorf("extraction is linked to raw_source_item %d, want 1001 — raw-first linkage is what makes "+
			"re-extraction possible", ext.rawSourceItemID)
	}
	fields := cfDecode(t, ext.fields)
	for _, k := range []string{"actionable", "kind", "title", "reason"} {
		if _, ok := fields[k]; !ok {
			t.Errorf("ai_extractions.fields has no %q key: %s", k, ext.fields)
		}
	}
	if _, ok := fields["confidence"]; ok {
		t.Errorf("ai_extractions.fields carries a `confidence` key: %s. qwen3:8b returns exactly 0.95 on "+
			"EVERYTHING it flags — 27 true positives and 17 false positives, identical. Storing it makes a "+
			"constant look like a dial (criterion 18)", ext.fields)
	}

	// The request itself: the classify prompt and schema, not triage's.
	if len(local.requests) != 1 {
		t.Fatalf("the local client saw %d requests, want 1", len(local.requests))
	}
	req := local.requests[0]
	if req.System != classify.SystemPrompt {
		t.Errorf("Request.System is not classify.SystemPrompt. Triage's prompt opens 'ONE inbound message "+
			"from an EXISTING client thread ... and a list of currently-open tasks' and carries "+
			"attach_to_task_id — asking that of an HOA violation notice is asking the wrong question:\n%q",
			req.System)
	}
	if req.SchemaName != classify.SchemaName || string(req.Schema) != string(classify.VerdictSchema) {
		t.Errorf("Request schema = %q/%s, want classify's", req.SchemaName, req.Schema)
	}
	if req.Model != "qwen3:8b" || req.MaxTokens != 512 {
		t.Errorf("Request model/MaxTokens = %q/%d, want the configured values", req.Model, req.MaxTokens)
	}
	if !strings.Contains(req.User, "alerts@bankofamerica.example.test") ||
		!strings.Contains(req.User, "Your payment is due") {
		t.Errorf("the rendered user prompt carries neither sender nor subject:\n%s\nThe spike measured that "+
			"subject+sender is enough for most senders and that the BODY is required for the templated ones "+
			"(Pines truncates its topic out of the subject)", req.User)
	}
	if !strings.Contains(req.User, "Minimum payment $35") {
		t.Errorf("the rendered user prompt does not carry the body:\n%s", req.User)
	}
}

// ---- criterion 15: shadow is STRUCTURAL --------------------------------------

// Same shape as internal/triage/worker_test.go's TestShadow_StoreHasNoTaskWriteMethod
// (criterion 15 says so by name). Going live ADDS an executor create_task call
// through the executor (invariant 3); it does not remove a guard here.
func TestShadow_StoreHasNoTaskWriteMethod(t *testing.T) {
	allowed := map[string]bool{
		"PendingMessages":  true,
		"MessagesByID":     true,
		"RecordRun":        true,
		"RecordExtraction": true,
	}
	forbidden := []string{"task", "delivery", "deliveries", "event", "decision"}

	st := reflect.TypeOf((*classify.Store)(nil)).Elem()
	if st.NumMethod() == 0 {
		t.Fatalf("classify.Store has no methods at all; a scan with nothing to scan proves nothing")
	}
	for i := 0; i < st.NumMethod(); i++ {
		name := st.Method(i).Name
		if !allowed[name] {
			t.Errorf("Store has unexpected method %q — shadow mode forbids new write surfaces. Add to the "+
				"allowlist only if it cannot touch tasks/task_events/deliveries/external_refs", name)
		}
		lname := strings.ToLower(name)
		for _, bad := range forbidden {
			if strings.Contains(lname, bad) {
				t.Errorf("Store method %q references %q — the classifier reads capture decisions and writes "+
					"ai_runs/ai_extractions, nothing else", name, bad)
			}
		}
	}
}

// The advisory-lock key, pinned at the type level. Criterion 10 names it and
// requires no collision; structure_test.go does the repo-wide collision scan.
func TestAdvisoryLockKey(t *testing.T) {
	if classify.AdvisoryLockKey != 0x5157_0022 {
		t.Errorf("classify.AdvisoryLockKey = %#x, want 0x51570022 (criterion 10). 0x51570005 orchestrator, "+
			"0x51570006 triage, 0x51570007 google/drafts and 0x51570015 capture are taken — a collision "+
			"makes two unrelated workers silently exclude each other",
			classify.AdvisoryLockKey)
	}
}
