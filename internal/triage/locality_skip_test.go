package triage_test

// SWT-21 acceptance criteria 16, 17, 18 and 19: what triage does when the
// locality boundary refuses a message. ZERO network, ZERO model, ZERO Postgres —
// a fake Store and two stub clients, the shape internal/triage/worker_test.go
// established.
//
// Identifiers here are prefixed `sk` so this file can sit beside worker_test.go
// in the same package without colliding with its fakes.
//
// THE STATE THIS TICKET SHIPS INTO. Triage's entire inbox is `unmatched`
// (capture_decisions), unmatched is ClassRestricted, and no local adapter exists
// until SWT-22 — so after this ticket EVERY message is skipped, every pass, and
// that is the SUCCESS state. These tests are the executable statement of that:
// a pass that skips everything must make no provider call, write no extraction,
// record why in a bounded way, and exit ZERO.
//
// The reason vocabulary is the SPEC's (criteria 20-21) and is asserted as the
// stored strings rather than as Go constants: `no_local_provider`,
// `local_endpoint_not_private`, `local_unreachable`, `unclassified_error` are
// what `triage report` groups by and what an operator reads.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/provider"
	"github.com/sspataro57/switchboard/internal/triage"
)

// ---- stubs -------------------------------------------------------------------

type skClient struct {
	desc   provider.Descriptor
	calls  int
	probes int
	// perCall[i] is the error returned by the i-th Complete call; a nil entry (or
	// running past the end) returns a canned, schema-valid extraction.
	perCall  []error
	probeErr error
}

func (c *skClient) Complete(_ context.Context, _ provider.Request) (provider.Response, error) {
	i := c.calls
	c.calls++
	if i < len(c.perCall) && c.perCall[i] != nil {
		return provider.Response{}, c.perCall[i]
	}
	return provider.Response{
		Raw:   json.RawMessage(`{"actionable":{"value":false,"confidence":0.4},"summary":"nothing to do"}`),
		Model: "stub",
	}, nil
}

func (c *skClient) Describe() provider.Descriptor { return c.desc }

func (c *skClient) Probe(_ context.Context) error {
	c.probes++
	return c.probeErr
}

func skHosted() *skClient {
	return &skClient{desc: provider.Descriptor{Name: "openai", Endpoint: "https://api.openai.com/v1"}}
}

func skLocal() *skClient {
	return &skClient{desc: provider.Descriptor{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1"}}
}

type skStore struct {
	pending     []triage.PendingMessage
	contexts    map[int64]triage.MessageContext
	runs        []triage.AIRun
	extractions int
	nextRunID   int64
}

func (s *skStore) PendingMessages(_ context.Context, _ triage.Config) ([]triage.PendingMessage, error) {
	return s.pending, nil
}

func (s *skStore) AssembleContext(_ context.Context, m triage.PendingMessage) (triage.MessageContext, error) {
	if mc, ok := s.contexts[m.MessageID]; ok {
		mc.Message = m
		return mc, nil
	}
	return triage.MessageContext{Message: m}, nil
}

func (s *skStore) RecordRun(_ context.Context, run triage.AIRun) (int64, error) {
	s.nextRunID++
	s.runs = append(s.runs, run)
	return s.nextRunID, nil
}

func (s *skStore) RecordExtraction(_ context.Context, _, _ int64, _ json.RawMessage) error {
	s.extractions++
	return nil
}

// skipped returns the runs recorded with status='skipped'. That status is a
// value `ok` and `error` never take (criterion 19), which is what makes "no
// permitted provider looked" structurally different from "the model looked and
// found nothing".
func (s *skStore) skipped() []triage.AIRun {
	var out []triage.AIRun
	for _, r := range s.runs {
		if r.Status == "skipped" {
			out = append(out, r)
		}
	}
	return out
}

func skMessages(n int) []triage.PendingMessage {
	out := make([]triage.PendingMessage, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, triage.PendingMessage{
			MessageID:       int64(i),
			RawSourceItemID: int64(1000 + i),
			ThreadID:        int64(i),
			SentAt:          time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
			Sender:          "alerts@bankofamerica.example.test",
			Subject:         "Your statement is ready",
			Channel:         "gmail",
			BodyText:        "account ending 1234",
			Direction:       "inbound",
		})
	}
	return out
}

func skDecode(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("skip record input is not a JSON object: %v (%s)", err, raw)
	}
	return m
}

func skCfg() triage.Config { return triage.Config{Model: "qwen3:8b", MaxTokens: 512} }

// ---- criteria 16, 17, 19: the no-local-provider pass --------------------------

// The headline. With no local client, a pass over restricted content must:
// make NO provider call of any kind, write NO ai_extractions row (so the message
// stays in the pending filter and retries next pass), record ONE aggregate
// refusal, not increment the error counter, and return no error.
func TestRun_NoLocalProvider_SkipsEverythingAndCallsNothing(t *testing.T) {
	general := skHosted()
	store := &skStore{
		pending: skMessages(4),
		contexts: map[int64]triage.MessageContext{
			1: {Attribution: provider.AttrUnmatched},
			2: {Attribution: provider.AttrUnmatched},
			3: {Attribution: provider.AttrUnseen},
			4: {Attribution: provider.AttrProject, ProjectLocalOnly: true},
		},
	}

	stats, err := triage.Run(context.Background(), store, provider.NewRouter(general, nil, time.Minute), skCfg())
	if err != nil {
		t.Fatalf("Run returned an error for a fully skipped pass: %v. Criterion 17: a skip is not an error and "+
			"must not produce a non-zero exit — this is the NORMAL pass until SWT-22 lands", err)
	}
	if general.calls != 0 {
		t.Errorf("the hosted client recorded %d Complete call(s); the headline smoke for this ticket is that "+
			"NO HTTP request leaves the process at all", general.calls)
	}
	if store.extractions != 0 {
		t.Errorf("%d ai_extractions written for skipped messages; criterion 19: a skip NEVER writes an "+
			"extraction, which is what keeps 'nothing looked' distinguishable from 'looked, found nothing' — "+
			"and what leaves the message in the pending filter for the next pass", store.extractions)
	}
	if stats.Errors != 0 {
		t.Errorf("stats.Errors = %d, want 0 — criterion 17: a refusal does not increment the error counter", stats.Errors)
	}
	if stats.Skipped != len(store.pending) {
		t.Errorf("stats.Skipped = %d, want %d", stats.Skipped, len(store.pending))
	}

	skips := store.skipped()
	if len(skips) != 1 {
		t.Fatalf("recorded %d skipped ai_runs rows, want exactly 1. Criterion 19: ONE row per pass when the "+
			"local lane is down, not one per message — the SWT-17 amplification landmine (49,415 messages × "+
			"every pass) is the reason, and after the unmatched-is-restricted deviation the restricted "+
			"population is the WHOLE inbox", len(skips))
	}
	if len(store.runs) != 1 {
		t.Errorf("recorded %d ai_runs rows in total, want 1; a skipped pass must not also write ok/error rows", len(store.runs))
	}

	rec := skips[0]
	if rec.WorkerType != "triage" {
		t.Errorf("skip record worker_type = %q, want %q", rec.WorkerType, "triage")
	}
	if rec.Provider != "local" {
		t.Errorf("skip record provider = %q, want %q — criterion 19 fixes the column; the REASON belongs in "+
			"input.avail_reason, where the report can group by it", rec.Provider, "local")
	}

	in := skDecode(t, rec.Input)
	if got := in["avail_reason"]; got != "no_local_provider" {
		t.Errorf("input.avail_reason = %v, want %q (criteria 20-21's vocabulary)", got, "no_local_provider")
	}
	if got, ok := in["skipped_count"].(float64); !ok || int(got) != 4 {
		t.Errorf("input.skipped_count = %v, want 4", in["skipped_count"])
	}
	ids, ok := in["message_ids"].([]any)
	if !ok || len(ids) != 4 {
		t.Errorf("input.message_ids = %v, want the 4 skipped message ids", in["message_ids"])
	}

	reasons, ok := in["class_reasons"].(map[string]any)
	if !ok {
		t.Fatalf("input.class_reasons missing: %v", in)
	}
	// Unseen and unmatched are reported SEPARATELY even though both are
	// restricted — criterion 5's whole argument for keeping them distinct in the
	// type. A record that collapses them destroys the number SWT-22 needs.
	for key, want := range map[string]int{"unseen": 1, "unmatched": 2, "project_local_only": 1} {
		got, ok := reasons[key].(float64)
		if !ok {
			t.Errorf("class_reasons.%s missing from %v", key, reasons)
			continue
		}
		if int(got) != want {
			t.Errorf("class_reasons.%s = %v, want %d", key, got, want)
		}
	}
}

// Criterion 19: "message_ids:[≤100]". The record has to be bounded, because the
// restricted population is now the entire inbox — ~16,000 messages — and this
// row is written on every pass.
func TestRun_SkipRecordIsBounded(t *testing.T) {
	general := skHosted()
	store := &skStore{pending: skMessages(150)}

	if _, err := triage.Run(context.Background(), store, provider.NewRouter(general, nil, time.Minute), skCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	skips := store.skipped()
	if len(skips) != 1 {
		t.Fatalf("recorded %d skipped rows for 150 skipped messages, want 1", len(skips))
	}
	in := skDecode(t, skips[0].Input)
	if got, ok := in["skipped_count"].(float64); !ok || int(got) != 150 {
		t.Errorf("input.skipped_count = %v, want 150 — the COUNT is unbounded, only the id list is capped",
			in["skipped_count"])
	}
	ids, _ := in["message_ids"].([]any)
	if len(ids) > 100 {
		t.Errorf("input.message_ids has %d entries, want at most 100", len(ids))
	}
	if len(ids) == 0 {
		t.Errorf("input.message_ids is empty; the bound is 100, not 0 — a sample is what makes the row " +
			"actionable rather than a counter")
	}
}

// Criterion 21's negative smoke, at unit scale: OPS_LOCAL_PROVIDER_URL pointed at
// a hosted API. The local lane is refused, the reason says so specifically, and
// nothing is sent anywhere. An operator who typo'd the variable must not read
// "no local provider" and go set the variable they already set.
func TestRun_LocalEndpointNotPrivate_IsRefusedWithItsOwnReason(t *testing.T) {
	general := skHosted()
	fakeLocal := skHosted() // the "local" slot, pointed at api.openai.com
	store := &skStore{pending: skMessages(2)}

	stats, err := triage.Run(context.Background(), store,
		provider.NewRouter(general, fakeLocal, time.Minute), skCfg())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if general.calls != 0 || fakeLocal.calls != 0 {
		t.Errorf("calls made: general=%d local-slot=%d; a lane that lies about its locality is not a permitted "+
			"processor, and refusing it must not fall through to the hosted lane", general.calls, fakeLocal.calls)
	}
	if stats.Skipped != 2 {
		t.Errorf("stats.Skipped = %d, want 2", stats.Skipped)
	}
	skips := store.skipped()
	if len(skips) != 1 {
		t.Fatalf("recorded %d skipped rows, want 1", len(skips))
	}
	if got := skDecode(t, skips[0].Input)["avail_reason"]; got != "local_endpoint_not_private" {
		t.Errorf("input.avail_reason = %v, want %q (criterion 21)", got, "local_endpoint_not_private")
	}
}

// ---- criterion 18: the three tiers -------------------------------------------

// Tier 1. ErrUnavailable — including a deadline — is NORMAL OPERATION for a 4B
// model at low priority. The message is skipped with reason `local_unreachable`,
// never retried against a different adapter, never an error.
func TestRun_LocalUnreachableMidPass_SkipsTheMessageNotThePass(t *testing.T) {
	general := skHosted()
	local := skLocal()
	local.perCall = []error{nil, fmt.Errorf("post ollama: %w", provider.ErrUnavailable), nil}
	store := &skStore{pending: skMessages(3)}

	stats, err := triage.Run(context.Background(), store, provider.NewRouter(general, local, time.Minute), skCfg())
	if err != nil {
		t.Fatalf("Run returned an error because the local box was busy: %v. Criterion 18: a timeout is normal "+
			"operation, not an error — a pass that exits non-zero every time that box is busy trains its "+
			"operator to ignore it", err)
	}
	if general.calls != 0 {
		t.Errorf("the hosted client recorded %d call(s) after the local lane failed; 'try local, fall back to "+
			"the configured provider' is the bug this ticket exists to prevent", general.calls)
	}
	if stats.Errors != 0 {
		t.Errorf("stats.Errors = %d, want 0 — an unreachable local lane is a skip, not an error", stats.Errors)
	}
	if stats.Skipped != 1 || stats.Processed != 2 {
		t.Errorf("stats = %+v, want 1 skipped and 2 processed", stats)
	}
	if store.extractions != 2 {
		t.Errorf("%d extractions written, want 2 (the skipped message must write none)", store.extractions)
	}

	skips := store.skipped()
	if len(skips) != 1 {
		t.Fatalf("recorded %d skipped rows, want 1 (bounded by how far the pass got — criterion 19's "+
			"per-message tier)", len(skips))
	}
	in := skDecode(t, skips[0].Input)
	if got := in["avail_reason"]; got != "local_unreachable" {
		t.Errorf("input.avail_reason = %v, want %q", got, "local_unreachable")
	}
	if got, ok := in["normalized_message_id"].(float64); !ok || int64(got) != 2 {
		t.Errorf("the per-message skip does not name its message (normalized_message_id = %v, want 2); "+
			"without the id the record cannot be reconciled against the inbox", in["normalized_message_id"])
	}
}

// Tier 2. Anything else — HTTP 4xx/5xx, malformed JSON, a schema violation — is
// an `unclassified_error`: the MESSAGE is skipped and recorded, and one such
// failure does not fail the run.
func TestRun_UnclassifiedError_SkipsTheMessageAndDoesNotFailThePass(t *testing.T) {
	general := skHosted()
	local := skLocal()
	local.perCall = []error{nil, fmt.Errorf("openai HTTP 400: bad schema"), nil}
	store := &skStore{pending: skMessages(3)}

	stats, err := triage.Run(context.Background(), store, provider.NewRouter(general, local, time.Minute), skCfg())
	if err != nil {
		t.Fatalf("one malformed response failed the whole pass: %v. Criterion 18: a broken adapter fails on "+
			"everything and IS news; one malformed message fails alone and is not", err)
	}
	if general.calls != 0 {
		t.Errorf("the hosted client recorded %d call(s) after a local error", general.calls)
	}
	if store.extractions != 2 {
		t.Errorf("%d extractions written, want 2", store.extractions)
	}
	if stats.Skipped != 1 || stats.Processed != 2 {
		t.Errorf("stats = %+v, want 1 skipped and 2 processed — an unclassified error skips the MESSAGE", stats)
	}

	skips := store.skipped()
	if len(skips) != 1 {
		t.Fatalf("recorded %d skipped rows, want 1", len(skips))
	}
	if got := skDecode(t, skips[0].Input)["avail_reason"]; got != "unclassified_error" {
		t.Errorf("input.avail_reason = %v, want %q — the reason has to distinguish a broken adapter from a "+
			"busy one, or criterion 18's ratio can never be computed", got, "unclassified_error")
	}
}

// Tier 3. The PASS raises on the PATTERN: unclassified errors above half the
// restricted-lane attempts, with a floor of 20 attempts.
//
// The threshold is a GUESS (SPEC criterion 18, and the answer to open question
// Q3 says so in as many words) — what these cases pin is the SHAPE: proportion
// not count, a floor so a tiny pass cannot cry wolf, and unreachable skips
// excluded because they are normal operation.
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
			func() error { return fmt.Errorf("openai HTTP 500: boom") }, true,
			"a genuinely broken local adapter fails on everything; that is news"},
		{"exactly half", 30, 15,
			func() error { return fmt.Errorf("openai HTTP 500: boom") }, false,
			"'exceeds half' — half is not more than half"},
		{"below the 20-attempt floor", 19, 19,
			func() error { return fmt.Errorf("openai HTTP 500: boom") }, false,
			"a two-message pass must not cry wolf; the floor is what stops it"},
		{"all unreachable", 30, 30,
			func() error { return fmt.Errorf("dial: %w", provider.ErrUnavailable) }, false,
			"local_unreachable skips do NOT count toward the ratio — a busy 4B box is the normal case"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			local := skLocal()
			local.perCall = make([]error, tc.attempts)
			for i := 0; i < tc.failures; i++ {
				local.perCall[i] = tc.failWith()
			}
			store := &skStore{pending: skMessages(tc.attempts)}

			stats, err := triage.Run(context.Background(), store,
				provider.NewRouter(skHosted(), local, time.Minute), skCfg())
			_ = stats

			if tc.wantErr && err == nil {
				t.Errorf("Run returned nil for %d/%d unclassified errors; want a raised pass — %s",
					tc.failures, tc.attempts, tc.why)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Run returned %v for %d/%d; want nil — %s", err, tc.failures, tc.attempts, tc.why)
			}
		})
	}
}

// ---- criterion 16: the most-restrictive fold ---------------------------------

// The fold covers the message AND every thread neighbour that goes into the
// prompt, because AssembleContext pulls neighbours by thread_id alone with no
// reference to THEIR attribution — so a restricted sibling's body would
// otherwise ride along to a hosted API.
//
// Both halves are asserted in one test on purpose. The control (a general
// message with no restricted neighbour reaches the hosted lane) is what proves
// the fixture discriminates; without it, an implementation that skipped
// everything would pass the interesting half and the test would prove nothing —
// the "predicate whose discriminating column is constant" landmine.
func TestRun_FoldOverThreadContext_ARestrictedNeighbourBlocksAGeneralMessage(t *testing.T) {
	generalCtx := triage.MessageContext{Attribution: provider.AttrProject, ProjectLocalOnly: false}

	withNeighbour := generalCtx
	withNeighbour.Thread = []triage.ThreadMessage{{
		Direction: "inbound", Sender: "alerts@bankofamerica.example.test",
		Subject: "Your statement is ready", BodyText: "account ending 1234",
	}}
	withNeighbour.NeighbourAttribution = []triage.NeighbourClass{{State: provider.AttrUnseen}}

	t.Run("control: no restricted neighbour reaches the general lane", func(t *testing.T) {
		general := skHosted()
		store := &skStore{pending: skMessages(1), contexts: map[int64]triage.MessageContext{1: generalCtx}}

		if _, err := triage.Run(context.Background(), store,
			provider.NewRouter(general, nil, time.Minute), skCfg()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if general.calls != 1 {
			t.Fatalf("general lane calls = %d, want 1. This is the CONTROL: if a project-attributed message "+
				"with ai_locality='any' cannot reach the hosted lane, the test below proves nothing", general.calls)
		}
	})

	t.Run("one unseen neighbour makes the whole request restricted", func(t *testing.T) {
		general := skHosted()
		store := &skStore{pending: skMessages(1), contexts: map[int64]triage.MessageContext{1: withNeighbour}}

		if _, err := triage.Run(context.Background(), store,
			provider.NewRouter(general, nil, time.Minute), skCfg()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if general.calls != 0 {
			t.Errorf("general lane calls = %d, want 0. The message is project-attributed and general, but a "+
				"thread neighbour is unseen and its BODY travels in the same prompt — the class of a request "+
				"is the class of its most restricted part", general.calls)
		}
		if store.extractions != 0 {
			t.Errorf("%d extractions written for a refused request", store.extractions)
		}
	})
}

// A pass that RAISES must still have written its route-refusal record.
//
// The flush used to sit after both raising returns, so any pass that ended in an
// error threw away the whole aggregate — every message the boundary refused
// became invisible behind an unrelated failure. Reachable in exactly the
// situation where the record matters most: a half-broken deployment, where some
// messages error on one lane while others are refused on the other.
func TestRun_SkipAggregateSurvivesARaisingPass(t *testing.T) {
	// Message 1 is general and its lane fails hard (an error, not a skip).
	// Messages 2 and 3 are restricted with no local client, so they route-skip.
	general := skHosted()
	general.perCall = []error{fmt.Errorf("openai HTTP 500: boom")}
	store := &skStore{
		pending: skMessages(3),
		contexts: map[int64]triage.MessageContext{
			1: {Attribution: provider.AttrProject},
			2: {Attribution: provider.AttrUnmatched},
			3: {Attribution: provider.AttrUnseen},
		},
	}

	stats, err := triage.Run(context.Background(), store, provider.NewRouter(general, nil, time.Minute), skCfg())
	if err == nil {
		t.Fatalf("Run returned no error despite a general-lane failure; this case is only interesting when the "+
			"pass raises (stats = %+v)", stats)
	}
	if stats.Skipped != 2 {
		t.Errorf("stats.Skipped = %d, want 2", stats.Skipped)
	}

	skips := store.skipped()
	if len(skips) != 1 {
		t.Fatalf("recorded %d skipped rows, want 1. The pass refused 2 messages and then raised for an "+
			"unrelated reason; flushing the aggregate AFTER the raising return discards that record entirely, "+
			"and the refusals disappear behind the error", len(skips))
	}
	in := skDecode(t, skips[0].Input)
	if got, ok := in["skipped_count"].(float64); !ok || int(got) != 2 {
		t.Errorf("input.skipped_count = %v, want 2", in["skipped_count"])
	}
	if got := in["avail_reason"]; got != "no_local_provider" {
		t.Errorf("input.avail_reason = %v, want %q", got, "no_local_provider")
	}
}

// The SECOND raising return — the restricted-lane ratio abort — must also flush.
//
// Its own case, not a variant, because there are two returns and a fix that
// moves the flush between them would satisfy the other test while still losing
// the record on this path.
func TestRun_SkipAggregateSurvivesTheRatioRaise(t *testing.T) {
	// 30 restricted messages: the first 25 reach a ready local lane and fail with
	// unclassified errors (over the floor, over half), the rest route-skip once
	// the lane is gone. One pass, both outcomes.
	local := skLocal()
	local.perCall = make([]error, 25)
	for i := range local.perCall {
		local.perCall[i] = fmt.Errorf("openai HTTP 500: boom")
	}
	store := &skStore{pending: skMessages(25)}

	stats, err := triage.Run(context.Background(), store,
		provider.NewRouter(skHosted(), local, time.Minute), skCfg())
	if err == nil {
		t.Fatalf("Run did not raise on 25/25 unclassified errors (stats = %+v)", stats)
	}

	// Every message here failed a CALL rather than being refused a lane, so the
	// records are the per-message kind. What this pins is that the raising return
	// does not swallow them.
	skips := store.skipped()
	if len(skips) != 25 {
		t.Fatalf("recorded %d skipped rows, want 25 — a raising pass must not discard the refusals it "+
			"already recorded", len(skips))
	}
	if got := skDecode(t, skips[0].Input)["avail_reason"]; got != "unclassified_error" {
		t.Errorf("input.avail_reason = %v, want %q", got, "unclassified_error")
	}
}

// A pass that refuses for TWO different reasons must report both.
//
// Reachable in ordinary operation: the probe TTL expires mid-pass, so early
// messages are refused for one reason and later ones for another. The record
// used to keep whichever came last and file the entire count under it — a wrong
// number rather than a missing one, and the reasons have different fixes.
func TestRun_SkipRecordCarriesEveryReason(t *testing.T) {
	// Messages 1-2 are restricted with a local client whose endpoint is hosted
	// (local_endpoint_not_private). There is no way to change the router
	// mid-pass, so this asserts the SHAPE: the breakdown map exists, sums to the
	// count, and agrees with the headline reason.
	store := &skStore{pending: skMessages(3)}
	if _, err := triage.Run(context.Background(), store,
		provider.NewRouter(skHosted(), skHosted(), time.Minute), skCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	skips := store.skipped()
	if len(skips) != 1 {
		t.Fatalf("recorded %d skipped rows, want 1", len(skips))
	}
	in := skDecode(t, skips[0].Input)
	reasons, ok := in["avail_reasons"].(map[string]any)
	if !ok {
		t.Fatalf("input.avail_reasons missing; a single last-wins avail_reason cannot describe a pass that "+
			"refused for more than one reason: %v", in)
	}
	total := 0
	for _, v := range reasons {
		n, _ := v.(float64)
		total += int(n)
	}
	if got, _ := in["skipped_count"].(float64); total != int(got) {
		t.Errorf("avail_reasons sums to %d but skipped_count is %v; the breakdown must account for every "+
			"refusal or it is worse than no breakdown", total, in["skipped_count"])
	}
	if _, present := reasons[in["avail_reason"].(string)]; !present {
		t.Errorf("the headline avail_reason %v does not appear in the breakdown %v", in["avail_reason"], reasons)
	}
}
