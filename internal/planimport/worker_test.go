package planimport_test

// Unit tests for the propose flow's raw-first + provider + bookkeeping half
// (SPEC 10-plan-import, criteria 2 & 3) — everything BEFORE the executor
// propose_plan_import call. Exercised against a fake provider.Client (records
// the Request, returns canned plan_tree JSON) and a fake Store (in-memory,
// triage worker_test idiom) — ZERO network, ZERO live LLM, ZERO Postgres. The
// real SQL Store + the executor propose/approve/apply path are covered in
// internal/tools/planimport_integration_test.go.
//
// GREENFIELD NOTE: package internal/planimport does not exist yet; compile-FAIL
// under `go test ./...` is the expected failure mode. Imposed exported surface
// (planimport.go / prompt.go / store.go), followed by the implementer:
//
//   const PromptVersion = "plan-import-v1"
//   const SchemaName    = "plan_tree"
//   const SystemPrompt  = "..."               // forbids inventing work; terse register; no AI refs
//   var   PlanTreeSchema json.RawMessage       // strict json_schema forwarded to the provider
//
//   type Config struct { Model string; MaxTokens int }
//
//   // AIRun is one ai_runs row's worth of bookkeeping the flow hands the store
//   // (triage.AIRun shape).
//   type AIRun struct {
//       WorkerType, Provider, Model, Status string
//       Input, Output json.RawMessage
//       PromptTokens, CompletionTokens, LatencyMS int
//   }
//
//   // Store is the planimport pg side. SHADOW-LIKE GUARANTEE (invariant 2): it
//   // has NO task-write method — the propose flow never touches tasks/
//   // task_events; the approved tree materializes only through the apply
//   // executor tool.
//   type Store interface {
//       EnsurePlanAccount(ctx context.Context) (accountID int64, err error)
//       UpsertRaw(ctx context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) (rawItemID int64, err error)
//       RecordRun(ctx context.Context, run AIRun) (aiRunID int64, err error)
//       RecordExtraction(ctx context.Context, aiRunID, rawSourceItemID int64, fields json.RawMessage) (aiExtractionID int64, err error)
//   }
//
//   // Proposal is what Propose returns; cmd/planimport feeds these ids to the
//   // executor propose_plan_import call.
//   type Proposal struct {
//       ProjectSlug, SourcePath, ContentHash string
//       RawSourceItemID, AIRunID, AIExtractionID int64
//   }
//
//   // Propose runs raw-first (EnsurePlanAccount + UpsertRaw with the FULL file
//   // content) → provider.Complete(plan_tree) → Validate → RecordRun (+
//   // RecordExtraction on success). A provider error OR a hard-invalid tree
//   // records an error ai_run and returns a non-nil error with NO extraction
//   // (criteria 2-4). content is the plan file bytes.
//   func Propose(ctx context.Context, store Store, client provider.Client, cfg Config,
//       projectSlug, sourcePath string, content []byte) (Proposal, error)

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/planimport"
	"github.com/sspataro57/switchboard/internal/provider"
)

// ---- fake provider ---------------------------------------------------------

type scriptedResp struct {
	resp provider.Response
	err  error
}

type fakeProvider struct {
	scripts  []scriptedResp
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
		PromptTokens:     200,
		CompletionTokens: 60,
		LatencyMS:        9,
	}
}

// ---- fake store ------------------------------------------------------------

type recordedRun struct {
	run     planimport.AIRun
	aiRunID int64
}

type recordedRaw struct {
	accountID  int64
	externalID string
	raw        json.RawMessage
	hash       string
}

type recordedExtraction struct {
	aiRunID         int64
	rawSourceItemID int64
	fields          json.RawMessage
}

type fakeStore struct {
	ensureCalls int
	raws        []recordedRaw
	runs        []recordedRun
	extractions []recordedExtraction
	nextRawID   int64
	nextRunID   int64
	nextExtrID  int64
}

func (s *fakeStore) EnsurePlanAccount(_ context.Context) (int64, error) {
	s.ensureCalls++
	return 500, nil
}

func (s *fakeStore) UpsertRaw(_ context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) (int64, error) {
	s.nextRawID++
	s.raws = append(s.raws, recordedRaw{accountID: accountID, externalID: externalID, raw: raw, hash: hash})
	return s.nextRawID, nil
}

func (s *fakeStore) RecordRun(_ context.Context, run planimport.AIRun) (int64, error) {
	s.nextRunID++
	s.runs = append(s.runs, recordedRun{run: run, aiRunID: s.nextRunID})
	return s.nextRunID, nil
}

func (s *fakeStore) RecordExtraction(_ context.Context, aiRunID, rawSourceItemID int64, fields json.RawMessage) (int64, error) {
	s.nextExtrID++
	s.extractions = append(s.extractions, recordedExtraction{aiRunID: aiRunID, rawSourceItemID: rawSourceItemID, fields: fields})
	return s.nextExtrID, nil
}

// ---- fixtures --------------------------------------------------------------

const planContent = "# Followups\n\n- Fix staging login\n- Ship CSV export (after login)\n"

// a well-formed two-node plan_tree the model would emit (plan_order absent —
// array position is authoritative).
const goodTreeJSON = `{"summary":"two followups","tasks":[
  {"ref":"login","parent_ref":null,"title":"Fix staging login","body":"login broken","assignee_type":"claude","subproject":null,"worker_type":null,"priority":2,"depends_on_refs":[],"confidence":0.9,"notes":""},
  {"ref":"export","parent_ref":null,"title":"Ship CSV export","body":"add export","assignee_type":"claude","subproject":null,"worker_type":null,"priority":0,"depends_on_refs":["login"],"confidence":0.8,"notes":"after login"}
]}`

// a tree with a parent cycle — Go validation must reject it (criterion 4).
const cyclicTreeJSON = `{"summary":"bad","tasks":[
  {"ref":"a","parent_ref":"b","title":"A","body":"","assignee_type":"claude","subproject":null,"worker_type":null,"priority":0,"depends_on_refs":[],"confidence":0.9,"notes":""},
  {"ref":"b","parent_ref":"a","title":"B","body":"","assignee_type":"claude","subproject":null,"worker_type":null,"priority":0,"depends_on_refs":[],"confidence":0.9,"notes":""}
]}`

func cfg() planimport.Config { return planimport.Config{Model: "gpt-5-mini", MaxTokens: 2048} }

// ---- prompt / schema wiring ------------------------------------------------

// TestPropose_ProviderCallCarriesSchemaAndContent: the provider Request uses the
// strict plan_tree schema, the cfg model, and a user prompt carrying the file
// content; the system prompt forbids inventing work (criterion 3, invariant 6).
func TestPropose_ProviderCallCarriesSchemaAndContent(t *testing.T) {
	store := &fakeStore{}
	prov := &fakeProvider{scripts: []scriptedResp{{resp: okResp(goodTreeJSON)}}}

	if _, err := planimport.Propose(context.Background(), store, prov, cfg(),
		"switchboard", "/home/salvo/plans/followups.md", []byte(planContent)); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if len(prov.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(prov.requests))
	}
	req := prov.requests[0]
	if req.SchemaName != planimport.SchemaName || planimport.SchemaName != "plan_tree" {
		t.Errorf("request SchemaName = %q, want plan_tree", req.SchemaName)
	}
	if len(req.Schema) == 0 {
		t.Errorf("request carries no strict schema")
	}
	if req.Model != "gpt-5-mini" {
		t.Errorf("request Model = %q, want cfg model gpt-5-mini", req.Model)
	}
	if !strings.Contains(req.User, "Fix staging login") {
		t.Errorf("user prompt missing the file content:\n%s", req.User)
	}
	if !strings.Contains(strings.ToLower(req.System), "invent") {
		t.Errorf("system prompt must forbid inventing work not in the file:\n%s", req.System)
	}
}

// ---- raw-first (invariant 1 / criterion 2) ---------------------------------

// TestPropose_RawFirstWithFullContent: EnsurePlanAccount + UpsertRaw run BEFORE
// the provider call, the raw external_id is plan:{slug}:{hash}, and raw_json
// carries the FULL file text + path (reparse-forever, criterion 2).
func TestPropose_RawFirstWithFullContent(t *testing.T) {
	store := &fakeStore{}
	prov := &fakeProvider{scripts: []scriptedResp{{resp: okResp(goodTreeJSON)}}}

	prop, err := planimport.Propose(context.Background(), store, prov, cfg(),
		"switchboard", "/home/salvo/plans/followups.md", []byte(planContent))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if store.ensureCalls != 1 {
		t.Errorf("EnsurePlanAccount calls = %d, want 1", store.ensureCalls)
	}
	if len(store.raws) != 1 {
		t.Fatalf("UpsertRaw calls = %d, want 1", len(store.raws))
	}
	r := store.raws[0]
	wantHash := planimport.ContentHash([]byte(planContent))
	if r.hash != wantHash {
		t.Errorf("raw hash = %q, want ContentHash(content) %q", r.hash, wantHash)
	}
	if want := "plan:switchboard:" + wantHash; r.externalID != want {
		t.Errorf("raw external_id = %q, want %q", r.externalID, want)
	}
	if prop.ContentHash != wantHash {
		t.Errorf("Proposal.ContentHash = %q, want %q", prop.ContentHash, wantHash)
	}
	// raw_json carries the full file content + path.
	var rawDoc map[string]any
	if err := json.Unmarshal(r.raw, &rawDoc); err != nil {
		t.Fatalf("raw_json is not valid JSON: %v", err)
	}
	if got, _ := rawDoc["content"].(string); got != planContent {
		t.Errorf("raw_json.content = %q, want the full file text", got)
	}
	if got, _ := rawDoc["path"].(string); got != "/home/salvo/plans/followups.md" {
		t.Errorf("raw_json.path = %q, want the source path", got)
	}
}

// TestPropose_ProviderErrorRecordsErrorRunNoExtraction: a provider failure
// records an error ai_run (worker_type plan_import) and NO extraction, still
// leaving the raw row behind (criterion 2/3); Propose returns non-nil.
func TestPropose_ProviderErrorRecordsErrorRunNoExtraction(t *testing.T) {
	store := &fakeStore{}
	prov := &fakeProvider{scripts: []scriptedResp{{err: errors.New("provider is down")}}}

	_, err := planimport.Propose(context.Background(), store, prov, cfg(),
		"switchboard", "/home/salvo/plans/followups.md", []byte(planContent))
	if err == nil {
		t.Fatalf("Propose: want a non-nil error on provider failure")
	}
	if len(store.raws) != 1 {
		t.Errorf("raw rows = %d, want 1 (raw-first: written BEFORE the provider call)", len(store.raws))
	}
	if len(store.runs) != 1 {
		t.Fatalf("ai_runs = %d, want 1 (an error run)", len(store.runs))
	}
	r := store.runs[0].run
	if r.WorkerType != "plan_import" {
		t.Errorf("run.WorkerType = %q, want plan_import", r.WorkerType)
	}
	if r.Status != "error" {
		t.Errorf("run.Status = %q, want error", r.Status)
	}
	if len(store.extractions) != 0 {
		t.Errorf("ai_extractions = %d, want 0 on provider error", len(store.extractions))
	}
}

// TestPropose_HardInvalidTreeRecordsErrorRunNoExtraction: a model tree that
// fails Go validation (parent cycle) records an error ai_run and NO extraction,
// and Propose returns non-nil — NO proposal is made (criterion 4).
func TestPropose_HardInvalidTreeRecordsErrorRunNoExtraction(t *testing.T) {
	store := &fakeStore{}
	prov := &fakeProvider{scripts: []scriptedResp{{resp: okResp(cyclicTreeJSON)}}}

	_, err := planimport.Propose(context.Background(), store, prov, cfg(),
		"switchboard", "/home/salvo/plans/followups.md", []byte(planContent))
	if err == nil {
		t.Fatalf("Propose: want a non-nil error on a hard-invalid tree")
	}
	if len(store.runs) != 1 || store.runs[0].run.Status != "error" {
		t.Errorf("want exactly one ai_run with status error; got %+v", store.runs)
	}
	if len(store.extractions) != 0 {
		t.Errorf("ai_extractions = %d, want 0 for an invalid tree (no proposal)", len(store.extractions))
	}
}

// TestPropose_SuccessRecordsRunAndExtraction: a valid tree records an ok ai_run
// (input carries prompt version + raw id + rendered prompt) and ONE extraction
// on the plan's raw item, whose fields carry the validated tree with Go-assigned
// plan_order (criterion 3/4). The Proposal returns the three ids.
func TestPropose_SuccessRecordsRunAndExtraction(t *testing.T) {
	store := &fakeStore{}
	prov := &fakeProvider{scripts: []scriptedResp{{resp: okResp(goodTreeJSON)}}}

	prop, err := planimport.Propose(context.Background(), store, prov, cfg(),
		"switchboard", "/home/salvo/plans/followups.md", []byte(planContent))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if len(store.runs) != 1 {
		t.Fatalf("ai_runs = %d, want 1", len(store.runs))
	}
	run := store.runs[0]
	if run.run.WorkerType != "plan_import" {
		t.Errorf("run.WorkerType = %q, want plan_import", run.run.WorkerType)
	}
	if run.run.Status != "ok" {
		t.Errorf("run.Status = %q, want ok", run.run.Status)
	}
	if !strings.Contains(string(run.run.Input), planimport.PromptVersion) {
		t.Errorf("run.Input missing prompt version %q: %s", planimport.PromptVersion, run.run.Input)
	}
	if len(run.run.Output) == 0 {
		t.Errorf("run.Output empty; must carry the verbatim model JSON")
	}

	if len(store.extractions) != 1 {
		t.Fatalf("ai_extractions = %d, want 1", len(store.extractions))
	}
	e := store.extractions[0]
	if e.aiRunID != run.aiRunID {
		t.Errorf("extraction.ai_run_id = %d, want the recorded run id %d", e.aiRunID, run.aiRunID)
	}
	// The extraction links to the plan's raw item (the first — and only —
	// UpsertRaw, id 1 in the fake).
	if e.rawSourceItemID != 1 {
		t.Errorf("extraction.raw_source_item_id = %d, want the plan's raw item (1)", e.rawSourceItemID)
	}
	// fields carry the validated tree with plan_order.
	var fields map[string]any
	if err := json.Unmarshal(e.fields, &fields); err != nil {
		t.Fatalf("fields is not valid JSON: %v", err)
	}
	tasks, ok := fields["tasks"].([]any)
	if !ok || len(tasks) != 2 {
		t.Fatalf("fields.tasks = %v, want 2 validated task nodes", fields["tasks"])
	}
	first, _ := tasks[0].(map[string]any)
	if first == nil || first["plan_order"] == nil {
		t.Errorf("fields.tasks[0] missing Go-assigned plan_order: %v", tasks[0])
	}

	// Proposal ids are populated for the executor propose_plan_import call.
	if prop.RawSourceItemID == 0 || prop.AIRunID == 0 || prop.AIExtractionID == 0 {
		t.Errorf("Proposal ids incomplete: %+v", prop)
	}
	if prop.ProjectSlug != "switchboard" || prop.SourcePath != "/home/salvo/plans/followups.md" {
		t.Errorf("Proposal project/path not carried through: %+v", prop)
	}
}
