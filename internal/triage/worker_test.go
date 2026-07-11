package triage_test

// Unit tests for the GPT triage worker (SPEC 06-gpt-triage, acceptance
// criteria 4, 5, 6, 7, 8). Everything runs against a fake provider.Client
// (records the Request, returns canned extraction JSON) and a fake Store
// (mirrors the connector's Sink pattern) — ZERO network, ZERO live LLM, ZERO
// Postgres. The deterministic halves of triage (prompt assembly, validation/
// clamping, verdict derivation, candidate clamp) are exercised here directly;
// the SQL halves (queue filter, context SQL, find_related_tasks, advisory
// lock, real writes) are covered in integration_test.go.
//
// GREENFIELD NOTE: package internal/triage does not exist yet; this file
// compile-FAILs under `go test ./...` until it is implemented — the expected
// failure mode. Imposed exported surface (the SPEC's triage.go/store.go/
// prompt.go). For greenfield code the SPEC's contract IS the signature:
//
//   const PromptVersion = "triage-v1"
//
//   type Config struct {
//       Model     string
//       MaxTokens int
//       Limit     int           // 0 = all pending
//       Since     time.Duration // 0 = no lower bound on sent_at
//   }
//
//   // Deterministic context the worker assembles per message (Store SQL).
//   type PendingMessage struct {
//       MessageID       int64
//       RawSourceItemID int64
//       ThreadID        int64
//       SentAt          time.Time
//       Sender, Subject, Channel, BodyText string
//       Direction       string // always "inbound" from the filter
//   }
//   type ThreadMessage struct {
//       Direction, Sender, Subject, BodyText string
//       SentAt time.Time
//   }
//   type Candidate struct {
//       ID         int64
//       Title      string
//       Status     string
//       Subproject string
//       UpdatedAt  time.Time
//   }
//   type MessageContext struct {
//       Message     PendingMessage
//       Thread      []ThreadMessage
//       PersonID    *int64
//       PersonName  string
//       ProjectID   *int64 // nil = UNMAPPED
//       ProjectSlug string
//       Candidates  []Candidate
//   }
//
//   // AIRun is one ai_runs row's worth of bookkeeping the worker hands the store.
//   type AIRun struct {
//       WorkerType, Provider, Model, Status string
//       Input            json.RawMessage // rendered user prompt + prompt version + candidate/message/raw ids
//       Output           json.RawMessage // verbatim model JSON (empty on error)
//       PromptTokens     int
//       CompletionTokens int
//       LatencyMS        int
//   }
//
//   // Store is the pg side. NOTE the shadow guarantee: it has NO task-write
//   // method — nothing here can insert into tasks/task_events/deliveries.
//   type Store interface {
//       PendingMessages(ctx context.Context, cfg Config) ([]PendingMessage, error)
//       AssembleContext(ctx context.Context, m PendingMessage) (MessageContext, error)
//       RecordRun(ctx context.Context, run AIRun) (aiRunID int64, err error)
//       RecordExtraction(ctx context.Context, aiRunID, rawSourceItemID int64, fields json.RawMessage) error
//   }
//
//   type Stats struct {
//       Processed, Errors, Actionable, Create, Attach, None int
//   }
//
//   func Run(ctx context.Context, store Store, client provider.Client, cfg Config) (Stats, error)

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/provider"
	"github.com/sspataro57/switchboard/internal/triage"
)

// ---- fake provider ---------------------------------------------------------

type scriptedResp struct {
	resp provider.Response
	err  error
}

type fakeProvider struct {
	scripts  []scriptedResp // consumed in order; last repeats if exhausted
	requests []provider.Request
	calls    int
}

func (f *fakeProvider) Complete(_ context.Context, req provider.Request) (provider.Response, error) {
	f.requests = append(f.requests, req)
	i := f.calls
	f.calls++
	if i >= len(f.scripts) {
		i = len(f.scripts) - 1
	}
	s := f.scripts[i]
	return s.resp, s.err
}

func okResp(content string) provider.Response {
	return provider.Response{
		Raw:              json.RawMessage(content),
		Model:            "gpt-5-mini-2025",
		PromptTokens:     100,
		CompletionTokens: 20,
		LatencyMS:        7,
	}
}

// ---- fake store ------------------------------------------------------------

type recordedRun struct {
	run     triage.AIRun
	aiRunID int64
}

type recordedExtraction struct {
	aiRunID         int64
	rawSourceItemID int64
	fields          json.RawMessage
}

type fakeStore struct {
	pending  []triage.PendingMessage
	contexts map[int64]triage.MessageContext // keyed by MessageID

	runs        []recordedRun
	extractions []recordedExtraction
	nextRunID   int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{contexts: map[int64]triage.MessageContext{}}
}

func (s *fakeStore) PendingMessages(_ context.Context, cfg triage.Config) ([]triage.PendingMessage, error) {
	if cfg.Limit > 0 && cfg.Limit < len(s.pending) {
		return s.pending[:cfg.Limit], nil
	}
	return s.pending, nil
}

func (s *fakeStore) AssembleContext(_ context.Context, m triage.PendingMessage) (triage.MessageContext, error) {
	if mc, ok := s.contexts[m.MessageID]; ok {
		return mc, nil
	}
	return triage.MessageContext{Message: m}, nil
}

func (s *fakeStore) RecordRun(_ context.Context, run triage.AIRun) (int64, error) {
	s.nextRunID++
	s.runs = append(s.runs, recordedRun{run: run, aiRunID: s.nextRunID})
	return s.nextRunID, nil
}

func (s *fakeStore) RecordExtraction(_ context.Context, aiRunID, rawSourceItemID int64, fields json.RawMessage) error {
	s.extractions = append(s.extractions, recordedExtraction{aiRunID: aiRunID, rawSourceItemID: rawSourceItemID, fields: fields})
	return nil
}

// ---- fixtures --------------------------------------------------------------

func i64(n int64) *int64 { return &n }

func mustTime(s string) time.Time {
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return tm
}

// mappedContext: a person mapped to a project, two open-task candidates, one
// prior thread message (outbound — legitimate history, must appear in context).
func mappedContext() triage.MessageContext {
	return triage.MessageContext{
		Message: triage.PendingMessage{
			MessageID:       501,
			RawSourceItemID: 9001,
			ThreadID:        70,
			SentAt:          mustTime("2026-07-05T10:00:00Z"),
			Sender:          "client@acme.example",
			Subject:         "login broken",
			Channel:         "upwork",
			BodyText:        "please fix the login bug on staging",
			Direction:       "inbound",
		},
		Thread: []triage.ThreadMessage{
			{Direction: "outbound", Sender: "me@sb.example", Subject: "login broken", BodyText: "earlier context reply from us", SentAt: mustTime("2026-07-04T10:00:00Z")},
		},
		PersonID:    i64(42),
		PersonName:  "Acme Corp",
		ProjectID:   i64(3),
		ProjectSlug: "acme-web",
		Candidates: []triage.Candidate{
			{ID: 10, Title: "Login flow revamp", Status: "ready", UpdatedAt: mustTime("2026-07-03T10:00:00Z")},
			{ID: 20, Title: "Billing page polish", Status: "in_progress", UpdatedAt: mustTime("2026-07-02T10:00:00Z")},
		},
	}
}

func defaultCfg() triage.Config {
	return triage.Config{Model: "gpt-5-mini", MaxTokens: 512}
}

// unmarshal the recorded fields JSON into a generic map for path assertions.
func fieldsMap(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("fields is not valid JSON: %v (%s)", err, raw)
	}
	return m
}

func fieldObj(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	o, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("fields[%q] is not a {value,confidence} object: %v", key, m[key])
	}
	return o
}

// ---- prompt assembly -------------------------------------------------------

// TestPromptAssembly_UserCarriesContext: the rendered user message fed to the
// provider carries the message body, thread context, person/client name, and
// the candidate task list (fed as DATA so the model picks attach_to_task_id).
func TestPromptAssembly_UserCarriesContext(t *testing.T) {
	store := newFakeStore()
	mc := mappedContext()
	store.pending = []triage.PendingMessage{mc.Message}
	store.contexts[mc.Message.MessageID] = mc
	prov := &fakeProvider{scripts: []scriptedResp{{resp: okResp(`{"actionable":{"value":true,"confidence":0.9},"kind":{"value":"action_request","confidence":0.9},"title":{"value":"Fix staging login","confidence":0.9},"body":{"value":"login broken","confidence":0.8},"priority":{"value":2,"confidence":0.8},"attach_to_task_id":{"value":10,"confidence":0.7},"summary":"clear bug report"}`)}}}

	if _, err := triage.Run(context.Background(), store, prov, defaultCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(prov.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(prov.requests))
	}
	user := prov.requests[0].User
	for _, want := range []string{
		"please fix the login bug on staging", // message body
		"earlier context reply from us",       // thread context (both directions)
		"Acme Corp",                           // person/client name
		"Login flow revamp",                   // candidate title
		"Billing page polish",                 // candidate title
	} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt missing %q\n---\n%s", want, user)
		}
	}
	// Candidate IDs must be present so the model can return a valid attach id.
	if !strings.Contains(user, "10") || !strings.Contains(user, "20") {
		t.Errorf("user prompt does not surface candidate ids 10/20:\n%s", user)
	}
}

// TestPromptAssembly_SystemCarriesRubric: the system prompt restates the ported
// rubric structure — the OUTPUT CONTRACT section and the schema enum vocabulary
// (SPEC pins these by substring).
func TestPromptAssembly_SystemCarriesRubric(t *testing.T) {
	store := newFakeStore()
	mc := mappedContext()
	store.pending = []triage.PendingMessage{mc.Message}
	store.contexts[mc.Message.MessageID] = mc
	prov := &fakeProvider{scripts: []scriptedResp{{resp: okResp(`{"actionable":{"value":true,"confidence":0.9},"kind":{"value":"question","confidence":0.9},"title":{"value":"t","confidence":0.9},"body":{"value":"b","confidence":0.9},"priority":{"value":0,"confidence":0.9},"attach_to_task_id":{"value":null,"confidence":0.9},"summary":"s"}`)}}}

	if _, err := triage.Run(context.Background(), store, prov, defaultCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	sys := strings.ToLower(prov.requests[0].System)
	for _, want := range []string{
		"actionable",      // signal family
		"attach",          // attach-vs-create rubric
		"create",          // attach-vs-create rubric
		"output contract", // explicit OUTPUT CONTRACT section (ported structure)
		"action_request",  // kind vocabulary
		"question",
		"scheduling",
		"status_update",
		"fyi",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing rubric marker %q", want)
		}
	}
	// The schema forwarded to the provider is the strict extraction schema.
	if prov.requests[0].SchemaName == "" || len(prov.requests[0].Schema) == 0 {
		t.Errorf("request must carry a named strict schema; got name=%q schema=%s", prov.requests[0].SchemaName, prov.requests[0].Schema)
	}
	if prov.requests[0].Model != "gpt-5-mini" {
		t.Errorf("request model = %q, want cfg model gpt-5-mini", prov.requests[0].Model)
	}
}

// TestRun_InputRecordsPromptVersionAndIDs: the ai_runs.input bookkeeping carries
// the prompt version + candidate/message/raw ids (criterion 4).
func TestRun_InputRecordsPromptVersionAndIDs(t *testing.T) {
	store := newFakeStore()
	mc := mappedContext()
	store.pending = []triage.PendingMessage{mc.Message}
	store.contexts[mc.Message.MessageID] = mc
	prov := &fakeProvider{scripts: []scriptedResp{{resp: okResp(`{"actionable":{"value":true,"confidence":0.9},"kind":{"value":"action_request","confidence":0.9},"title":{"value":"t","confidence":0.9},"body":{"value":"b","confidence":0.9},"priority":{"value":1,"confidence":0.9},"attach_to_task_id":{"value":10,"confidence":0.8},"summary":"s"}`)}}}

	if _, err := triage.Run(context.Background(), store, prov, defaultCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.runs) != 1 {
		t.Fatalf("ai_runs recorded = %d, want 1", len(store.runs))
	}
	r := store.runs[0].run
	if r.WorkerType != "triage" {
		t.Errorf("run.WorkerType = %q, want triage", r.WorkerType)
	}
	if r.Provider != "openai" {
		t.Errorf("run.Provider = %q, want openai", r.Provider)
	}
	if r.Model != "gpt-5-mini" {
		t.Errorf("run.Model = %q, want gpt-5-mini", r.Model)
	}
	if r.Status != "ok" {
		t.Errorf("run.Status = %q, want ok", r.Status)
	}
	input := string(r.Input)
	if !strings.Contains(input, triage.PromptVersion) {
		t.Errorf("run.Input missing prompt version %q: %s", triage.PromptVersion, input)
	}
	for _, want := range []string{"9001", "501", "10", "20"} { // raw id, message id, candidate ids
		if !strings.Contains(input, want) {
			t.Errorf("run.Input missing id %q: %s", want, input)
		}
	}
	// output is the verbatim model JSON.
	if len(r.Output) == 0 {
		t.Errorf("run.Output empty; must carry verbatim model JSON")
	}
}

// ---- response handling -----------------------------------------------------

// TestResponseHandling_PerFieldConfidencePreserved: each {value,confidence}
// pair lands verbatim in the extraction fields jsonb (criterion 5), plus the
// resolved context (message/thread/person/project ids, verdict).
func TestResponseHandling_PerFieldConfidencePreserved(t *testing.T) {
	store := newFakeStore()
	mc := mappedContext()
	store.pending = []triage.PendingMessage{mc.Message}
	store.contexts[mc.Message.MessageID] = mc
	prov := &fakeProvider{scripts: []scriptedResp{{resp: okResp(`{"actionable":{"value":true,"confidence":0.91},"kind":{"value":"action_request","confidence":0.83},"title":{"value":"Fix staging login","confidence":0.77},"body":{"value":"login broken on staging","confidence":0.66},"priority":{"value":2,"confidence":0.55},"attach_to_task_id":{"value":10,"confidence":0.72},"summary":"clear bug report"}`)}}}

	if _, err := triage.Run(context.Background(), store, prov, defaultCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.extractions) != 1 {
		t.Fatalf("ai_extractions recorded = %d, want 1", len(store.extractions))
	}
	e := store.extractions[0]
	if e.rawSourceItemID != 9001 {
		t.Errorf("extraction.raw_source_item_id = %d, want 9001 (raw linkage)", e.rawSourceItemID)
	}
	if e.aiRunID != store.runs[0].aiRunID {
		t.Errorf("extraction.ai_run_id = %d, want the recorded run id %d (FK ordering)", e.aiRunID, store.runs[0].aiRunID)
	}
	f := fieldsMap(t, e.fields)

	if got := fieldObj(t, f, "actionable")["confidence"]; got != 0.91 {
		t.Errorf("actionable.confidence = %v, want 0.91 (verbatim)", got)
	}
	if got := fieldObj(t, f, "title")["confidence"]; got != 0.77 {
		t.Errorf("title.confidence = %v, want 0.77 (verbatim)", got)
	}
	if got := fieldObj(t, f, "title")["value"]; got != "Fix staging login" {
		t.Errorf("title.value = %v, want verbatim", got)
	}
	if got := fieldObj(t, f, "priority")["confidence"]; got != 0.55 {
		t.Errorf("priority.confidence = %v, want 0.55 (verbatim)", got)
	}

	// Resolved context + verdict (criterion 4).
	if f["verdict"] != "attach" {
		t.Errorf("verdict = %v, want attach (attach_to_task_id set to a candidate)", f["verdict"])
	}
	if got := f["normalized_message_id"]; got != float64(501) {
		t.Errorf("normalized_message_id = %v, want 501", got)
	}
	if got := f["thread_id"]; got != float64(70) {
		t.Errorf("thread_id = %v, want 70", got)
	}
	if got := f["person_id"]; got != float64(42) {
		t.Errorf("person_id = %v, want 42", got)
	}
	if got := f["project_id"]; got != float64(3) {
		t.Errorf("project_id = %v, want 3", got)
	}
}

// TestResponseHandling_ClampsOutOfRangeConfidence: confidence > 1 is clamped to
// [0,1] and the clamp is recorded in fields.validation (criterion 5).
func TestResponseHandling_ClampsOutOfRangeConfidence(t *testing.T) {
	store := newFakeStore()
	mc := mappedContext()
	store.pending = []triage.PendingMessage{mc.Message}
	store.contexts[mc.Message.MessageID] = mc
	prov := &fakeProvider{scripts: []scriptedResp{{resp: okResp(`{"actionable":{"value":true,"confidence":1.5},"kind":{"value":"question","confidence":-0.2},"title":{"value":"t","confidence":0.9},"body":{"value":"b","confidence":0.9},"priority":{"value":0,"confidence":0.9},"attach_to_task_id":{"value":null,"confidence":0.9},"summary":"s"}`)}}}

	if _, err := triage.Run(context.Background(), store, prov, defaultCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	f := fieldsMap(t, store.extractions[0].fields)
	if got := fieldObj(t, f, "actionable")["confidence"]; got != 1.0 {
		t.Errorf("actionable.confidence = %v, want clamped to 1.0", got)
	}
	if got := fieldObj(t, f, "kind")["confidence"]; got != 0.0 {
		t.Errorf("kind.confidence = %v, want clamped to 0.0", got)
	}
	if v, ok := f["validation"]; !ok || v == nil {
		t.Errorf("fields.validation missing; clamp corrections must be recorded")
	}
}

// TestResponseHandling_NonCandidateAttachNulled: an attach_to_task_id NOT in the
// offered candidate set is nulled and the rejection recorded (criterion 6). The
// verdict then falls back to create (message is actionable).
func TestResponseHandling_NonCandidateAttachNulled(t *testing.T) {
	store := newFakeStore()
	mc := mappedContext() // candidates {10, 20}
	store.pending = []triage.PendingMessage{mc.Message}
	store.contexts[mc.Message.MessageID] = mc
	prov := &fakeProvider{scripts: []scriptedResp{{resp: okResp(`{"actionable":{"value":true,"confidence":0.9},"kind":{"value":"action_request","confidence":0.9},"title":{"value":"t","confidence":0.9},"body":{"value":"b","confidence":0.9},"priority":{"value":1,"confidence":0.9},"attach_to_task_id":{"value":999,"confidence":0.9},"summary":"s"}`)}}}

	if _, err := triage.Run(context.Background(), store, prov, defaultCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	f := fieldsMap(t, store.extractions[0].fields)
	if got := fieldObj(t, f, "attach_to_task_id")["value"]; got != nil {
		t.Errorf("attach_to_task_id.value = %v, want null (999 not in candidate set)", got)
	}
	if f["verdict"] != "create" {
		t.Errorf("verdict = %v, want create (attach rejected, still actionable)", f["verdict"])
	}
	if v, ok := f["validation"]; !ok || v == nil {
		t.Errorf("fields.validation missing; candidate rejection must be recorded")
	}
}

// TestResponseHandling_EmptyCandidatesForcesNullAttach: an unmapped person has
// no candidates, so attach_to_task_id must be null regardless of what the model
// says (criterion 6), project_id is null (extracted anyway — "extract
// everything"), verdict none for non-actionable.
func TestResponseHandling_UnmappedPersonExtractedWithNullProject(t *testing.T) {
	store := newFakeStore()
	m := triage.PendingMessage{MessageID: 601, RawSourceItemID: 9101, ThreadID: 71, SentAt: mustTime("2026-07-06T10:00:00Z"), Sender: "stranger@x.example", Subject: "thanks", Channel: "email", BodyText: "thanks, all good!", Direction: "inbound"}
	store.pending = []triage.PendingMessage{m}
	store.contexts[m.MessageID] = triage.MessageContext{
		Message: m, PersonID: i64(99), PersonName: "Unmapped Person",
		ProjectID: nil, ProjectSlug: "", Candidates: nil, // unmapped ⇒ empty candidate list
	}
	prov := &fakeProvider{scripts: []scriptedResp{{resp: okResp(`{"actionable":{"value":false,"confidence":0.95},"kind":{"value":"fyi","confidence":0.9},"title":{"value":"Thanks note","confidence":0.9},"body":{"value":"thanks","confidence":0.9},"priority":{"value":0,"confidence":0.9},"attach_to_task_id":{"value":10,"confidence":0.5},"summary":"acknowledgement"}`)}}}

	if _, err := triage.Run(context.Background(), store, prov, defaultCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.extractions) != 1 {
		t.Fatalf("non-actionable message must still get an extraction (extract everything); got %d", len(store.extractions))
	}
	f := fieldsMap(t, store.extractions[0].fields)
	if got := fieldObj(t, f, "attach_to_task_id")["value"]; got != nil {
		t.Errorf("attach_to_task_id.value = %v, want null (empty candidate list)", got)
	}
	if _, ok := f["project_id"]; !ok {
		t.Errorf("project_id key missing")
	} else if f["project_id"] != nil {
		t.Errorf("project_id = %v, want null (unmapped person)", f["project_id"])
	}
	if f["verdict"] != "none" {
		t.Errorf("verdict = %v, want none (not actionable)", f["verdict"])
	}
}

// TestResponseHandling_MalformedProviderJSONIsNonFatal: an unparseable model
// payload writes an ai_runs row status=error with NO extraction row, the worker
// continues with the next message, and Run returns a non-nil error at the end
// (criterion 8).
func TestResponseHandling_MalformedProviderJSONIsNonFatal(t *testing.T) {
	store := newFakeStore()
	bad := triage.PendingMessage{MessageID: 701, RawSourceItemID: 9201, ThreadID: 72, SentAt: mustTime("2026-07-07T09:00:00Z"), BodyText: "bad one", Direction: "inbound"}
	good := mappedContext().Message
	good.MessageID = 702
	good.RawSourceItemID = 9202
	store.pending = []triage.PendingMessage{bad, good}
	store.contexts[bad.MessageID] = triage.MessageContext{Message: bad}
	gc := mappedContext()
	gc.Message = good
	store.contexts[good.MessageID] = gc

	prov := &fakeProvider{scripts: []scriptedResp{
		{resp: okResp(`{ this is : not valid json`)},
		{resp: okResp(`{"actionable":{"value":true,"confidence":0.9},"kind":{"value":"action_request","confidence":0.9},"title":{"value":"t","confidence":0.9},"body":{"value":"b","confidence":0.9},"priority":{"value":1,"confidence":0.9},"attach_to_task_id":{"value":null,"confidence":0.9},"summary":"s"}`)},
	}}

	stats, err := triage.Run(context.Background(), store, prov, defaultCfg())
	if err == nil {
		t.Errorf("Run: expected non-nil error at end when a message failed (exit non-zero)")
	}
	// Both messages attempted; one error run + one ok run; exactly one extraction.
	if len(store.runs) != 2 {
		t.Errorf("ai_runs recorded = %d, want 2 (error run + ok run)", len(store.runs))
	}
	var errRuns, okRuns int
	for _, r := range store.runs {
		switch r.run.Status {
		case "error":
			errRuns++
		case "ok":
			okRuns++
		}
	}
	if errRuns != 1 || okRuns != 1 {
		t.Errorf("run statuses: error=%d ok=%d, want 1/1", errRuns, okRuns)
	}
	if len(store.extractions) != 1 {
		t.Errorf("ai_extractions recorded = %d, want 1 (no extraction for the failed message)", len(store.extractions))
	}
	if store.extractions[0].rawSourceItemID != 9202 {
		t.Errorf("the recorded extraction is for raw item %d, want the good message 9202", store.extractions[0].rawSourceItemID)
	}
	if stats.Errors != 1 || stats.Processed < 1 {
		t.Errorf("stats = %+v, want Errors=1 and at least one processed", stats)
	}
}

// TestRun_AbortsAfterConsecutiveProviderErrors: >5 consecutive provider errors
// abort the run (dead provider — stop burning the batch) (criterion 8). The
// exact boundary ("more than 5" ⇒ 6th trips it) is pinned; adjust if the impl
// chose a different off-by-one.
func TestRun_AbortsAfterConsecutiveProviderErrors(t *testing.T) {
	store := newFakeStore()
	for i := int64(0); i < 20; i++ {
		m := triage.PendingMessage{MessageID: 800 + i, RawSourceItemID: 9300 + i, ThreadID: 80, BodyText: "x", Direction: "inbound"}
		store.pending = append(store.pending, m)
		store.contexts[m.MessageID] = triage.MessageContext{Message: m}
	}
	// Every provider call is a hard error.
	prov := &fakeProvider{scripts: []scriptedResp{{err: errors.New("provider is down")}}}

	_, err := triage.Run(context.Background(), store, prov, defaultCfg())
	if err == nil {
		t.Fatalf("Run: expected a non-nil error when the provider is down")
	}
	if prov.calls != 6 {
		t.Errorf("provider calls = %d, want 6 (5 tolerated, the 6th consecutive error aborts)", prov.calls)
	}
	if len(store.extractions) != 0 {
		t.Errorf("extractions = %d, want 0 (nothing parsed)", len(store.extractions))
	}
}

// ---- idempotency (unit view of the filter contract) ------------------------

// TestRun_ProcessesOnlyWhatFilterReturns: Run drives exactly PendingMessages'
// output and makes no provider call when the filter is empty — the real
// NOT-EXISTS filter (already-extracted messages excluded) is exercised in
// integration_test.go; this pins that the worker adds no second processing path.
func TestRun_ProcessesOnlyWhatFilterReturns(t *testing.T) {
	// First run: two pending.
	store := newFakeStore()
	mc := mappedContext()
	store.pending = []triage.PendingMessage{mc.Message}
	store.contexts[mc.Message.MessageID] = mc
	prov := &fakeProvider{scripts: []scriptedResp{{resp: okResp(`{"actionable":{"value":true,"confidence":0.9},"kind":{"value":"question","confidence":0.9},"title":{"value":"t","confidence":0.9},"body":{"value":"b","confidence":0.9},"priority":{"value":0,"confidence":0.9},"attach_to_task_id":{"value":null,"confidence":0.9},"summary":"s"}`)}}}
	if _, err := triage.Run(context.Background(), store, prov, defaultCfg()); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	if prov.calls != 1 {
		t.Fatalf("first run provider calls = %d, want 1", prov.calls)
	}

	// Second run: filter now returns nothing (already extracted) ⇒ zero calls.
	empty := newFakeStore()
	prov2 := &fakeProvider{scripts: []scriptedResp{{resp: okResp(`{}`)}}}
	stats, err := triage.Run(context.Background(), empty, prov2, defaultCfg())
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if prov2.calls != 0 {
		t.Errorf("second run provider calls = %d, want 0 (idempotent — nothing pending)", prov2.calls)
	}
	if stats.Processed != 0 {
		t.Errorf("second run processed = %d, want 0", stats.Processed)
	}
}

// ---- shadow guarantee (structural) ----------------------------------------

// TestShadow_StoreHasNoTaskWriteMethod encodes the load-bearing shadow-mode
// invariant at the TYPE level (criterion 7 / invariant 2): the triage worker's
// Store surface has no method that could insert a task, task_event, or
// delivery. If a future edit adds such a method, this test fails — the worker
// package must contain no code path that writes tasks. The allowlist below is
// the entire legitimate surface; anything else is a shadow-mode violation.
func TestShadow_StoreHasNoTaskWriteMethod(t *testing.T) {
	allowed := map[string]bool{
		"PendingMessages":  true,
		"AssembleContext":  true,
		"RecordRun":        true,
		"RecordExtraction": true,
	}
	forbidden := []string{"task", "delivery", "deliveries", "event"}

	st := reflect.TypeOf((*triage.Store)(nil)).Elem()
	for i := 0; i < st.NumMethod(); i++ {
		name := st.Method(i).Name
		if !allowed[name] {
			t.Errorf("Store has unexpected method %q — shadow mode forbids new write surfaces; add to allowlist only if it cannot touch tasks/task_events/deliveries", name)
		}
		lname := strings.ToLower(name)
		for _, bad := range forbidden {
			if strings.Contains(lname, bad) {
				t.Errorf("Store method %q references %q — the worker must not read/write the task spine in shadow mode", name, bad)
			}
		}
	}
}
