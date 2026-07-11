package drafts_test

// Unit tests for the GPT draft worker (SPEC 08-draft-deliveries, criterion 9 +
// invariant 6). Everything runs against a fake provider.Client (records the
// Request, returns a canned {subject, body}), a fake Store, and a fake Executor
// that records the tool calls the worker makes — ZERO network, ZERO live LLM,
// ZERO Postgres. The deterministic halves (prompt assembly, schema shape,
// unresolvable-skip, non-fatal error handling) are exercised here; the SQL
// halves (Deliver-task queue, channel/thread resolution, advisory lock) belong
// to the integration suite.
//
// GREENFIELD NOTE: package internal/drafts does not exist yet; this file
// compile-FAILs under `go test ./...` until it is implemented. For greenfield
// code the SPEC's contract IS the signature. Imposed exported surface
// (drafts.go / store.go / prompt.go):
//
//   const PromptVersion = "drafts-v1"
//   var SystemPrompt string          // terse Salvador register, no sign-offs, no AI attribution
//   const SchemaName = "delivery_draft"
//   var DraftSchema json.RawMessage  // strict json_schema: exactly {subject, body}
//
//   type Config struct { Model string; MaxTokens, Limit int }
//
//   type ThreadMessage struct { Direction, Sender, Subject, BodyText string; SentAt time.Time }
//
//   // DeliverTask is one R3 Deliver task whose parent has no delivery row yet,
//   // with channel + thread resolved DETERMINISTICALLY by the store (never model
//   // output). Unresolvable => Channel=="" (or gmail with ThreadID==nil).
//   type DeliverTask struct {
//       DeliverTaskID int64   // the Deliver task (task_append_log target on skip)
//       ParentTaskID  int64   // the delivered work task (delivery attaches here)
//       ProjectSlug   string
//       Channel       string  // "gmail" | "upwork_chat" | "" (unresolvable)
//       ThreadID      *int64  // resolved thread; nil => unresolvable for gmail
//       TargetRef     string  // upwork thread_key
//       ParentTitle   string
//       ParentSummary string
//       ClientName    string
//       Thread        []ThreadMessage
//   }
//
//   type AIRun struct {
//       WorkerType, Provider, Model, Status string
//       Input, Output json.RawMessage
//       PromptTokens, CompletionTokens, LatencyMS int
//   }
//
//   type Store interface {
//       DeliverTasks(ctx context.Context, cfg Config) ([]DeliverTask, error)
//       RecordRun(ctx context.Context, run AIRun) (aiRunID int64, err error)
//   }
//
//   // Executor is the executor.Execute seam: the worker writes ONLY through it
//   // (invariant 3) — draft_delivery on success, task_append_log on skip.
//   type Executor interface {
//       Execute(ctx context.Context, call executor.Call) (executor.Result, error)
//   }
//
//   type Stats struct { Drafted, Skipped, Errors int }
//   func Run(ctx context.Context, store Store, client provider.Client, exec Executor, cfg Config) (Stats, error)

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/drafts"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/provider"
)

const draftsActor = "drafts:gpt"

// ---- fakes -------------------------------------------------------------------

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
	return f.scripts[i].resp, f.scripts[i].err
}

func okDraft(subject, body string) provider.Response {
	raw, _ := json.Marshal(map[string]string{"subject": subject, "body": body})
	return provider.Response{Raw: raw, Model: "gpt-5-mini-2025", PromptTokens: 90, CompletionTokens: 30, LatencyMS: 5}
}

type fakeStore struct {
	tasks []drafts.DeliverTask
	runs  []drafts.AIRun
	next  int64
}

func (s *fakeStore) DeliverTasks(_ context.Context, _ drafts.Config) ([]drafts.DeliverTask, error) {
	return s.tasks, nil
}

func (s *fakeStore) RecordRun(_ context.Context, run drafts.AIRun) (int64, error) {
	s.next++
	s.runs = append(s.runs, run)
	return s.next, nil
}

type fakeExec struct {
	calls []executor.Call
}

func (e *fakeExec) Execute(_ context.Context, call executor.Call) (executor.Result, error) {
	e.calls = append(e.calls, call)
	return executor.Result{Output: json.RawMessage(`{"delivery_id":1}`)}, nil
}

func (e *fakeExec) callsTo(tool string) []executor.Call {
	var out []executor.Call
	for _, c := range e.calls {
		if c.Tool == tool {
			out = append(out, c)
		}
	}
	return out
}

func i64(n int64) *int64 { return &n }

func gmailDeliverTask() drafts.DeliverTask {
	return drafts.DeliverTask{
		DeliverTaskID: 55,
		ParentTaskID:  9,
		ProjectSlug:   "acme",
		Channel:       "gmail",
		ThreadID:      i64(70),
		ParentTitle:   "Fix staging login",
		ParentSummary: "merged to main, deployed to staging",
		ClientName:    "Acme Corp",
		Thread: []drafts.ThreadMessage{
			{Direction: "inbound", Sender: "client@acme.example", Subject: "login broken", BodyText: "the login page is down on staging", SentAt: mustTime("2026-07-05T10:00:00Z")},
		},
	}
}

func defaultCfg() drafts.Config { return drafts.Config{Model: "gpt-5-mini", MaxTokens: 512} }

func mustTime(s string) time.Time {
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return tm
}

// ---- prompt + schema ---------------------------------------------------------

func TestDrafts_PromptCarriesContext_SchemaIsSubjectBodyOnly(t *testing.T) {
	store := &fakeStore{tasks: []drafts.DeliverTask{gmailDeliverTask()}}
	prov := &fakeProvider{scripts: []scriptedResp{{resp: okDraft("Re: login broken", "Pushed the fix to staging.")}}}
	exec := &fakeExec{}

	if _, err := drafts.Run(context.Background(), store, prov, exec, defaultCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(prov.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(prov.requests))
	}
	req := prov.requests[0]

	// User prompt carries the deterministic context: parent summary + title,
	// thread body, client name.
	for _, want := range []string{
		"merged to main, deployed to staging", // done_local summary
		"Fix staging login",                   // parent title
		"the login page is down on staging",   // thread context
		"Acme Corp",                           // client name
	} {
		if !strings.Contains(req.User, want) {
			t.Errorf("user prompt missing %q\n---\n%s", want, req.User)
		}
	}
	// System prompt demands Salvador's terse register and pins the
	// no-attribution / no-sign-off rule.
	sys := strings.ToLower(req.System)
	if !strings.Contains(sys, "terse") {
		t.Errorf("system prompt must instruct the terse (Salvador) register:\n%s", req.System)
	}
	if !strings.Contains(sys, "attribution") && !strings.Contains(sys, "sign-off") && !strings.Contains(sys, "signature") {
		t.Errorf("system prompt should pin the no-attribution / no-sign-off rule:\n%s", req.System)
	}

	// Strict schema forwarded, and it is EXACTLY {subject, body}: From/To/
	// recipient must NOT appear (invariant 6 — From is resolved server-side and
	// is not even in the model's schema).
	if req.SchemaName == "" || len(req.Schema) == 0 {
		t.Fatalf("request must carry a named strict schema; name=%q schema=%s", req.SchemaName, req.Schema)
	}
	props := schemaProps(t, req.Schema)
	if _, ok := props["subject"]; !ok {
		t.Errorf("schema missing property subject: %s", req.Schema)
	}
	if _, ok := props["body"]; !ok {
		t.Errorf("schema missing property body: %s", req.Schema)
	}
	for _, banned := range []string{"from", "from_account_id", "to", "recipient", "sender"} {
		if _, ok := props[banned]; ok {
			t.Errorf("schema must NOT expose %q — From/To are resolved server-side, never model-chosen: %s", banned, req.Schema)
		}
	}
	if req.Model != "gpt-5-mini" {
		t.Errorf("request model = %q, want cfg model gpt-5-mini", req.Model)
	}
}

func schemaProps(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var s struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("schema is not valid JSON: %v (%s)", err, raw)
	}
	if s.Properties == nil {
		t.Fatalf("schema has no properties object: %s", raw)
	}
	return s.Properties
}

// ---- draft creation through the executor -------------------------------------

func TestDrafts_CreatesDraftViaExecutor(t *testing.T) {
	store := &fakeStore{tasks: []drafts.DeliverTask{gmailDeliverTask()}}
	prov := &fakeProvider{scripts: []scriptedResp{{resp: okDraft("Re: login broken", "Pushed the fix to staging.")}}}
	exec := &fakeExec{}

	stats, err := drafts.Run(context.Background(), store, prov, exec, defaultCfg())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Drafted != 1 {
		t.Errorf("stats.Drafted = %d, want 1", stats.Drafted)
	}

	drafted := exec.callsTo("draft_delivery")
	if len(drafted) != 1 {
		t.Fatalf("draft_delivery executor calls = %d, want 1", len(drafted))
	}
	call := drafted[0]
	if call.Actor != draftsActor {
		t.Errorf("draft_delivery actor = %q, want %q", call.Actor, draftsActor)
	}
	var args map[string]any
	if err := json.Unmarshal(call.Args, &args); err != nil {
		t.Fatalf("draft_delivery args not JSON: %v (%s)", err, call.Args)
	}
	if got, _ := args["task_id"].(float64); int64(got) != 9 {
		t.Errorf("draft_delivery task_id = %v, want the parent work task 9", args["task_id"])
	}
	if args["channel"] != "gmail" {
		t.Errorf("draft_delivery channel = %v, want gmail", args["channel"])
	}
	if args["subject"] != "Re: login broken" {
		t.Errorf("draft_delivery subject = %v, want the model subject", args["subject"])
	}
	if args["body"] != "Pushed the fix to staging." {
		t.Errorf("draft_delivery body = %v, want the model body", args["body"])
	}
	if got, _ := args["thread_id"].(float64); int64(got) != 70 {
		t.Errorf("draft_delivery thread_id = %v, want the resolved thread 70", args["thread_id"])
	}
	// From is resolved server-side by the tool; the worker must not pass it.
	for _, banned := range []string{"from_account_id", "from", "to", "recipient"} {
		if _, ok := args[banned]; ok {
			t.Errorf("draft_delivery args must not carry %q (resolved server-side): %s", banned, call.Args)
		}
	}

	// ai_runs recorded by the worker (worker owns the bookkeeping).
	if len(store.runs) != 1 {
		t.Fatalf("ai_runs recorded = %d, want 1", len(store.runs))
	}
	r := store.runs[0]
	if r.WorkerType != "drafts" {
		t.Errorf("run.WorkerType = %q, want drafts", r.WorkerType)
	}
	if r.Provider != "openai" {
		t.Errorf("run.Provider = %q, want openai", r.Provider)
	}
	if r.Status != "ok" {
		t.Errorf("run.Status = %q, want ok", r.Status)
	}
	if !strings.Contains(string(r.Input), drafts.PromptVersion) {
		t.Errorf("run.Input missing prompt version %q: %s", drafts.PromptVersion, r.Input)
	}
	if len(r.Output) == 0 {
		t.Errorf("run.Output must carry the verbatim model JSON")
	}
}

// ---- unresolvable channel/thread: log + skip, no draft, no model call --------

func TestDrafts_UnresolvableAppendsLogAndSkips(t *testing.T) {
	cases := map[string]drafts.DeliverTask{
		"no channel resolved": func() drafts.DeliverTask {
			d := gmailDeliverTask()
			d.Channel = ""
			return d
		}(),
		"gmail with no thread": func() drafts.DeliverTask {
			d := gmailDeliverTask()
			d.ThreadID = nil
			return d
		}(),
	}
	for name, dt := range cases {
		dt := dt
		t.Run(name, func(t *testing.T) {
			store := &fakeStore{tasks: []drafts.DeliverTask{dt}}
			prov := &fakeProvider{scripts: []scriptedResp{{resp: okDraft("x", "y")}}}
			exec := &fakeExec{}

			stats, err := drafts.Run(context.Background(), store, prov, exec, defaultCfg())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if prov.calls != 0 {
				t.Errorf("provider calls = %d, want 0 (unresolvable: no model call)", prov.calls)
			}
			if n := len(exec.callsTo("draft_delivery")); n != 0 {
				t.Errorf("draft_delivery calls = %d, want 0 when unresolvable", n)
			}
			logs := exec.callsTo("task_append_log")
			if len(logs) != 1 {
				t.Fatalf("task_append_log calls = %d, want exactly 1 (draft-manually note)", len(logs))
			}
			var la map[string]any
			_ = json.Unmarshal(logs[0].Args, &la)
			if got, _ := la["task_id"].(float64); int64(got) != dt.DeliverTaskID {
				t.Errorf("task_append_log task_id = %v, want the Deliver task %d", la["task_id"], dt.DeliverTaskID)
			}
			if msg, _ := la["message"].(string); !strings.Contains(strings.ToLower(msg), "draft manually") {
				t.Errorf("task_append_log message = %q, want a 'draft manually' note", la["message"])
			}
			if stats.Skipped != 1 {
				t.Errorf("stats.Skipped = %d, want 1", stats.Skipped)
			}
		})
	}
}

// ---- provider error is non-fatal ---------------------------------------------

func TestDrafts_ProviderErrorNonFatal(t *testing.T) {
	good := gmailDeliverTask()
	bad := gmailDeliverTask()
	bad.DeliverTaskID = 56
	bad.ParentTaskID = 10
	store := &fakeStore{tasks: []drafts.DeliverTask{bad, good}}
	prov := &fakeProvider{scripts: []scriptedResp{
		{err: errors.New("provider down")},
		{resp: okDraft("Re: login broken", "Pushed the fix.")},
	}}
	exec := &fakeExec{}

	stats, err := drafts.Run(context.Background(), store, prov, exec, defaultCfg())
	if err == nil {
		t.Errorf("Run: want a non-nil error at the end when a task failed (exit non-zero)")
	}
	if stats.Errors != 1 {
		t.Errorf("stats.Errors = %d, want 1", stats.Errors)
	}
	if stats.Drafted != 1 {
		t.Errorf("stats.Drafted = %d, want 1 (the second task still drafts)", stats.Drafted)
	}
	if n := len(exec.callsTo("draft_delivery")); n != 1 {
		t.Errorf("draft_delivery calls = %d, want 1 (only the good task)", n)
	}
	// The failed task recorded an ai_runs row with status error; no draft for it.
	var errRuns int
	for _, r := range store.runs {
		if r.Status == "error" {
			errRuns++
		}
	}
	if errRuns != 1 {
		t.Errorf("error ai_runs = %d, want 1", errRuns)
	}
}
