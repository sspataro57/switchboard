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

// Describe is required by provider.Client since SWT-21. The fake sits in the
// GENERAL lane and describes a hosted endpoint, which is where this work
// actually belongs: these fixtures draft client-facing replies for client projects, which is
// general content by construction.
func (f *fakeProvider) Describe() provider.Descriptor {
	return provider.Descriptor{Name: "fake", Endpoint: "https://api.example.test/v1"}
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

	if _, err := drafts.Run(context.Background(), store, provider.NewRouter(prov, nil, 0), exec, defaultCfg()); err != nil {
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

	stats, err := drafts.Run(context.Background(), store, provider.NewRouter(prov, nil, 0), exec, defaultCfg())
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
	// SWT-37 (Q1 = b): the worker read the parent as done_locally before its
	// model call; this arg makes draft_delivery re-check it under the task row
	// lock, so a hand close or "delivered" in between gets no draft.
	if args["expect_task_status"] != "done_locally" {
		t.Errorf("draft_delivery expect_task_status = %v, want done_locally (the status DeliverTasks read)",
			args["expect_task_status"])
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
	// Since SWT-21 this column names the LANE THAT SERVED, from the client's
	// Describe(), not the constant "openai". Two lanes now exist, and a hardcoded
	// value would label every locally-processed row as hosted.
	if r.Provider != "fake" {
		t.Errorf("run.Provider = %q, want the serving lane's descriptor name %q", r.Provider, "fake")
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

			stats, err := drafts.Run(context.Background(), store, provider.NewRouter(prov, nil, 0), exec, defaultCfg())
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

	stats, err := drafts.Run(context.Background(), store, provider.NewRouter(prov, nil, 0), exec, defaultCfg())
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

// ---- SWT-43 (delivery-deny) criteria 22-24: the Redo prompt -------------------
//
// IMPOSED SURFACE (SPEC criteria 20 and 22):
//
//	type DeliverTask struct {
//	    ...
//	    RedraftOf     int64  // newest redraft-requested rejected row on the parent; 0 = none
//	    RejectedBody  string // that row's body
//	    RejectionNote string // that row's rejection_note ("" when none)
//	}
//	const PromptVersion = "drafts-v2"
//	// ai_runs.input gains "redraft_of_delivery_id" (0 when absent)
//
// The SPEC does not spell the section header. These tests require the section
// to say "rejected" (D1's vocabulary — the same word the status carries) and
// "His reason:" (the SPEC's words), and require NEITHER when RedraftOf is 0.
//
// GREENFIELD NOTE — EXPECTED RED: the three fields do not exist, so package
// drafts_test compile-FAILS until drafts.go declares them.

const draftsClosingLine = "Draft the message telling the client this work is done."

func redraftDeliverTask() drafts.DeliverTask {
	d := gmailDeliverTask()
	d.RedraftOf = 4242
	d.RejectedBody = "Hi team, apologies for the delay on this, we sincerely regret the inconvenience caused."
	d.RejectionNote = "shorter, no apology"
	return d
}

// runOneDraft drives drafts.Run over exactly one task and returns what the
// provider was asked, plus the fakes.
func runOneDraft(t *testing.T, dt drafts.DeliverTask) (provider.Request, *fakeStore, *fakeExec) {
	t.Helper()
	store := &fakeStore{tasks: []drafts.DeliverTask{dt}}
	prov := &fakeProvider{scripts: []scriptedResp{{resp: okDraft("Re: login broken", "Fix is live on staging.")}}}
	exec := &fakeExec{}
	if _, err := drafts.Run(context.Background(), store, provider.NewRouter(prov, nil, 0), exec, defaultCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(prov.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(prov.requests))
	}
	return prov.requests[0], store, exec
}

// Criterion 23, first half.
func TestDrafts_Redraft_PromptCarriesTheRejectedDraftAndTheReason(t *testing.T) {
	dt := redraftDeliverTask()
	req, _, _ := runOneDraft(t, dt)
	user := req.User

	for _, want := range []string{dt.RejectedBody, "His reason:", dt.RejectionNote} {
		if !strings.Contains(user, want) {
			t.Errorf("redraft prompt is missing %q (criterion 22)\n---\n%s", want, user)
		}
	}
	lower := strings.ToLower(user)
	if !strings.Contains(lower, "rejected") {
		t.Errorf("the redraft section never says the earlier draft was REJECTED; the model has to know the " +
			"text it is shown is what not to send")
	}
	if !strings.Contains(lower, "new message") || !strings.Contains(lower, "repeat") {
		t.Errorf("the redraft section must instruct the model to write a NEW message that addresses the reason "+
			"and not to REPEAT the rejected draft (criterion 22)\n---\n%s", user)
	}
	// The original context is still there: a redraft is the same job, re-done.
	for _, want := range []string{"merged to main, deployed to staging", "Fix staging login", "the login page is down on staging"} {
		if !strings.Contains(user, want) {
			t.Errorf("redraft prompt lost the task context %q", want)
		}
	}
	if !strings.HasSuffix(user, draftsClosingLine) {
		t.Errorf("the closing %q line must stay LAST (criterion 22); prompt ends %q",
			draftsClosingLine, user[max(0, len(user)-120):])
	}
	// The rejected text sits BEFORE the closing instruction, not after it.
	if i, j := strings.Index(user, dt.RejectedBody), strings.Index(user, draftsClosingLine); i < 0 || j < i {
		t.Errorf("rejected draft at %d, closing line at %d; the section is appended before the closing line", i, j)
	}
}

// Criterion 23, second half: no redraft, no section.
func TestDrafts_Redraft_NoSectionWithoutARedraft(t *testing.T) {
	req, _, _ := runOneDraft(t, gmailDeliverTask())
	user := req.User
	for _, banned := range []string{"His reason", "(no reason given)"} {
		if strings.Contains(user, banned) {
			t.Errorf("a first draft's prompt contains %q; the section is appended only when RedraftOf != 0", banned)
		}
	}
	if strings.Contains(strings.ToLower(user), "rejected") {
		t.Errorf("a first draft's prompt mentions a rejected draft:\n%s", user)
	}
	if !strings.HasSuffix(user, draftsClosingLine) {
		t.Errorf("the closing line is no longer last for a first draft")
	}
}

func TestDrafts_Redraft_NoNoteSaysNoReasonGiven(t *testing.T) {
	dt := redraftDeliverTask()
	dt.RejectionNote = ""
	req, _, _ := runOneDraft(t, dt)
	if !strings.Contains(req.User, "His reason:") || !strings.Contains(req.User, "(no reason given)") {
		t.Errorf("a redraft with no note must say \"His reason:\" followed by \"(no reason given)\" (criterion 22)\n---\n%s", req.User)
	}
}

// The rejected draft is truncated to 600 bytes with the existing truncate: it
// is context, not a template, and a long rejected email would crowd out the
// thread.
func TestDrafts_Redraft_RejectedDraftIsTruncatedTo600Bytes(t *testing.T) {
	dt := redraftDeliverTask()
	head := strings.Repeat("x", 600)
	dt.RejectedBody = head + "TAIL-PAST-THE-600-BYTE-CUT"
	req, _, _ := runOneDraft(t, dt)
	if !strings.Contains(req.User, head) {
		t.Errorf("the first 600 bytes of the rejected draft are not in the prompt")
	}
	if strings.Contains(req.User, "TAIL-PAST") {
		t.Errorf("the rejected draft was not truncated at 600 bytes (criterion 22)")
	}
}

// Criterion 22: PromptVersion drafts-v2; ai_runs.input names the rejected row
// (the ONLY link between a redraft and the row it replaces, criterion 24).
func TestDrafts_Redraft_PromptVersionAndRunInput(t *testing.T) {
	if drafts.PromptVersion != "drafts-v2" {
		t.Errorf("PromptVersion = %q, want drafts-v2: the user prompt gained a section, so old and new runs "+
			"must be told apart in ai_runs", drafts.PromptVersion)
	}
	for _, tc := range []struct {
		name string
		dt   drafts.DeliverTask
		want float64
	}{
		{"redraft", redraftDeliverTask(), 4242},
		{"first draft", gmailDeliverTask(), 0},
	} {
		_, store, _ := runOneDraft(t, tc.dt)
		if len(store.runs) != 1 {
			t.Fatalf("%s: ai_runs recorded = %d, want 1", tc.name, len(store.runs))
		}
		var input map[string]any
		if err := json.Unmarshal(store.runs[0].Input, &input); err != nil {
			t.Fatalf("%s: run input is not JSON: %v", tc.name, err)
		}
		got, ok := input["redraft_of_delivery_id"].(float64)
		if !ok || got != tc.want {
			t.Errorf("%s: ai_runs.input redraft_of_delivery_id = %v (present=%v), want %v (0 when absent)",
				tc.name, input["redraft_of_delivery_id"], ok, tc.want)
		}
		if input["prompt_version"] != "drafts-v2" {
			t.Errorf("%s: ai_runs.input prompt_version = %v, want drafts-v2", tc.name, input["prompt_version"])
		}
	}
}

// Criterion 24: the new row is created EXACTLY as today — same tool, same
// actor, same expect_task_status, and no argument linking it to the rejected
// row (the link is ai_runs.input only).
func TestDrafts_Redraft_DraftsThroughTheSameCall(t *testing.T) {
	_, _, first := runOneDraft(t, gmailDeliverTask())
	_, _, redo := runOneDraft(t, redraftDeliverTask())
	a, b := first.callsTo("draft_delivery"), redo.callsTo("draft_delivery")
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("draft_delivery calls = %d / %d, want 1 / 1", len(a), len(b))
	}
	if b[0].Actor != draftsActor {
		t.Errorf("redraft draft_delivery actor = %q, want %q", b[0].Actor, draftsActor)
	}
	keys := func(raw json.RawMessage) map[string]any {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("draft_delivery args: %v", err)
		}
		return m
	}
	ka, kb := keys(a[0].Args), keys(b[0].Args)
	if kb["expect_task_status"] != "done_locally" {
		t.Errorf("redraft expect_task_status = %v, want done_locally (D7: the only status a draft can land on)", kb["expect_task_status"])
	}
	for k := range kb {
		if _, ok := ka[k]; !ok {
			t.Errorf("the redraft's draft_delivery carries %q, which a first draft does not. Criterion 24: the new "+
				"row carries no link back to the rejected row", k)
		}
	}
	if len(ka) != len(kb) {
		t.Errorf("draft_delivery arg keys differ: first draft %v, redraft %v", ka, kb)
	}
}

// Criterion 23: SystemPrompt and DraftSchema are byte-unchanged. The redraft
// instruction belongs in the USER prompt; the system prompt carries the
// no-attribution rules (invariant 6) and must not drift while a section is
// added beside it.
func TestDrafts_Redraft_SystemPromptAndSchemaAreByteUnchanged(t *testing.T) {
	const wantSystem = `You draft outbound client messages for a solo contract engineer (Salvador).
Write in his terse register: short sentences, plain words, no fluff, no
corporate padding, no exclamation marks. One idea per paragraph. Sign off
with "Salvador" or nothing. NEVER mention AI, assistants, automation, or add
any attribution line (no "Generated with", no Co-Authored-By). The message
should tell the client what was done and what happens next, grounded ONLY in
the task summary and thread context provided.

OUTPUT CONTRACT:
- subject: short reply subject (reuse the thread's subject with Re: when
  replying; empty for chat channels).
- body: the message text, self-contained, ready to send verbatim.`
	const wantSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["subject", "body"],
  "properties": {
    "subject": {"type": "string"},
    "body": {"type": "string"}
  }
}`
	if drafts.SystemPrompt != wantSystem {
		t.Errorf("SystemPrompt changed; criterion 23 keeps it byte-unchanged (the redraft section is USER prompt)")
	}
	if string(drafts.DraftSchema) != wantSchema {
		t.Errorf("DraftSchema changed; criterion 23 keeps the model contract exactly {subject, body}")
	}
}
