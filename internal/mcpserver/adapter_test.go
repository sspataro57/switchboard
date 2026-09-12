package mcpserver_test

// Unit tests for the ops-mcp adapter (SPEC 04-mcp-task-tools, acceptance
// criterion 3). ZERO network: the adapter is driven with a fake executor and a
// hardcoded worker id. These pin the three properties the SPEC makes normative
// for the MCP boundary:
//
//   1. tools/list returns EXACTLY the agent-facing allowlist (create_task plus
//      the eight agent tools), each carrying a JSON input schema. The two
//      spine-facing tools (task_release, answer_feedback) are ABSENT.
//   2. tools/call maps a tool name + args to executor.Call{Tool, Actor, Args}
//      with Actor = "mcp:" + OPS_WORKER_ID.
//   3. A model-supplied worker_id in the args is force-overwritten from the
//      server's OPS_WORKER_ID before the executor sees it (identity is never
//      model-chosen), and a spine-facing name is rejected at the MCP layer
//      without ever reaching the executor.
//
// GREENFIELD NOTE: package internal/mcpserver does not exist yet, so this file
// compile-FAILs. It imposes the following exported surface (documented here so
// the implementer can match it; the SPEC leaves the internal shape open, this
// is the minimal testable adapter):
//
//   type Executor interface {
//       Execute(context.Context, executor.Call) (executor.Result, error)
//   }
//   type Tool struct {
//       Name        string
//       Description string
//       InputSchema json.RawMessage // a JSON Schema object
//   }
//   func New(ex Executor, workerID string) *Server
//   func (*Server) ListTools() []Tool
//   func (*Server) CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error)
//
// The literal stdio initialize/tools-list/tools-call round-trip over the
// official go-sdk is exercised by smoke_integration_test.go (in-process against
// a real db) rather than by hand-rolling the wire framing here.
//
// SWT-12 (slack-send-promotion) Q1: mark_delivery_sent joins the agent surface —
// and NOTHING else does. NARROWED by criterion 24 after adversarial review: the
// handler additionally permits only ONE transition over this transport (resolving
// a slack_reply row already in 'sending'), because delivery_sent drives
// orchestrator R8 and an injected call could otherwise fabricate a delivery that
// never happened. That handler-level restriction is covered by
// TestSlackReview_Integration_MCPMayOnlyResolveASendingRow in internal/tools;
// what this file pins is the listing and the worker-denial.
//
// SWT-37 (mcp-task-verbs) criteria 7 and 17: task_dismiss, task_close and
// task_mark_delivered join wantAgentTools; task_dismiss leaves spineTools;
// TestMCPListing_DoesNotMakeTaskVerbsWorkerCallable pins that listing them does
// not make them worker-callable. GREENFIELD — EXPECTED RED until schemas.go
// gains the three entries and internal/policy gains mcp_human_only.
//
// SWT-38 (mcp-task-capture) criterion 11: task_set_priority joins
// wantAgentTools, so the full profile lists 23 tools. EXPECTED RED until
// schemas.go gains the entry. Its worker refusal is pinned in
// task_capture_test.go (TestMCPListing_DoesNotMakeSetPriorityWorkerCallable).
//
// SWT-42 (mail-attachments) criterion 21: mail_list_attachments and
// mail_read_attachment join wantAgentTools, so the full profile lists 25
// tools. EXPECTED RED until schemas.go gains both entries.

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/mcpserver"
	"github.com/sspataro57/switchboard/internal/policy"
)

const testWorkerID = "manual:test"

// fakeExec records the last executor.Call the adapter forwarded.
type fakeExec struct {
	called   bool
	lastCall executor.Call
	result   executor.Result
	err      error
}

func (f *fakeExec) Execute(_ context.Context, call executor.Call) (executor.Result, error) {
	f.called = true
	f.lastCall = call
	return f.result, f.err
}

// agent-facing allowlist the SPEC pins for tools/list.
var wantAgentTools = []string{
	"create_task",
	"task_get_next",
	"task_claim",
	"task_context",
	"task_append_log",
	"request_feedback",
	"mark_done_local",
	"create_child_task",
	"record_decision",
	"draft_delivery",     // agent-facing since SWT-8: THE route for client-visible words
	"link_external_ref",  // agent-facing since SWT-9: workers link their PRs/issues
	"mark_delivery_sent", // agent-facing since SWT-12 (Q1): resolve a 'sending' Slack row
	// SWT-11 (criterion 16): read-only, served from normalized_messages rather
	// than a live mailbox, so no provider connection sits behind an agent call.
	"mail_search",
	"mail_read_thread",
	// SWT-11 (criterion 17): MCP-listed so an interactive session can finish what
	// it drafted. Still policy.humanOnly — the actor gate, not the tool list, is
	// what stands between an autonomous worker and a send.
	"approve_delivery",
	"send_delivery",
	// SWT-28 criterion 25: the calendar auto tier's verb, agent-facing where
	// send_delivery is not — a worker books its own focus time with no human
	// in the loop. The matrix (channel_mismatch, kill switch, rate limit) and
	// the handler's LoadBusy refusal are the gates, not the actor.
	"book_calendar_block",
	// SWT-35 (task-list-mcp) criterion 18: read-only queue reads, the
	// mail_search precedent — not humanOnly, not snapshotGated, and nothing in
	// either handler branches on who is calling (L12). Listing task_list does
	// not widen claiming: a worker still takes work only via task_get_next
	// (L14, a prompt rule).
	"task_list",
	"project_list",
	// SWT-37 (mcp-task-verbs) criterion 7. V0, Salvador's decision of
	// 2026-09-10: dismiss, close and mark delivered from ANY Claude Code
	// session ("Every repo's session", taken with the stated prompt-injection
	// risk). Listing them here removes the transport allowlist as a refusal for
	// worker consoles, which share this full profile, so the POLICY gates are
	// what keep workers out now: task_dismiss by policy.humanOnly (V2, rule
	// human_only) and task_close / task_mark_delivered by the transport rule
	// mcp_human_only (V1). Neither verb can be humanOnly: the orchestrator (R2,
	// R8) and the Jira reconciler call them in-process.
	// TestMCPListing_DoesNotMakeTaskVerbsWorkerCallable pins the refusal for
	// the real worker shapes.
	"task_dismiss",
	"task_close",
	"task_mark_delivered",
	// SWT-38 (mcp-task-capture) criterion 11, C5/C6: reorder any task's
	// priority (0..3). Listing it removes the transport allowlist as a refusal
	// for worker consoles, which share this full profile, so policy.humanOnly
	// (rule human_only) is what keeps a worker from choosing its own work. It
	// is humanOnly, not mcp_human_only: no spine caller sets priority, so the
	// orchestrator is refused too (C6). Pinned for the real worker shapes by
	// TestMCPListing_DoesNotMakeSetPriorityWorkerCallable (task_capture_test.go).
	"task_set_priority",
	// SWT-42 (mail-attachments) criterion 21: read-only, the mail_search shape —
	// not humanOnly, not snapshotGated; they write nothing but their audit row.
	// Served from the stored IMAP bytes in raw_source_items, never a live
	// mailbox. The SWT-21 locality gate is in the HANDLER and applies to every
	// caller and every profile (criterion 16, D2), so listing them here gives a
	// worker console nothing the gate would refuse anyone else.
	"mail_list_attachments",
	"mail_read_attachment",
}

// spine-facing tools must never appear in tools/list nor be callable via MCP.
//
// SWT-12 Q1 grounds for the three delivery verbs here: this session ingests
// Slack and email content, so the verbs that can put words in front of a client
// must not be one prompt injection away. Recording an already-sent message is
// the ONE delivery verb where an injected call can do no external damage —
// approving, sending, and un-sending are not.
// Spine-facing: registered on the executor but deliberately NOT MCP-listed, so
// the adapter rejects them by name before the executor is reached.
//
// approve_delivery and send_delivery were on this list until SWT-11 (criterion
// 17). They are now MCP-listed on purpose: the surface was draft-only and
// therefore unusable in practice — Salvador could ask an interactive session to
// draft a reply but had to leave the session to approve and send it. They are
// NOT less protected as a result. They stay in policy.humanOnly, and
// policy.humanActor() strips one leading "mcp:" transport prefix so
// mcp:manual:salvo passes while mcp:worker:X is denied with rule human_only.
// The audit row keeps the full unmodified actor, so an MCP-triggered send is
// still distinguishable from an opsctl one.
var spineTools = []string{
	"task_release", "answer_feedback", "prefill_delivery",
	"mark_delivery_failed",
	// SWT-20 criterion 2, asserted deliberately rather than by omission.
	// task_set_source_thread writes tasks.source_thread_id, which is the fact
	// draft_delivery binds an upwork target to (SPEC §4). An agent that could
	// write it could name any conversation and then aim a delivery there — the
	// exposure the pass-four upwork_chat closure was written for, and the reason
	// external_refs was rejected as the provenance store (D1: link_external_ref
	// IS agent-facing, with a free-text external_key). Same shape as the capture
	// rule tools: the transport, not an actor prefix, is the boundary.
	"task_set_source_thread",
	// task_dismiss WAS here (SWT-31 criterion 10) and MOVED to wantAgentTools
	// in SWT-37 (mcp-task-verbs, criterion 7; V0, the owner decision of
	// 2026-09-10). SWT-31 gave a worker two independent refusals: absence from
	// this transport allowlist, plus policy.humanOnly. The move was safe because
	// humanOnly still refuses EVERY worker shape — mcp:{client},
	// mcp:{client}.{sub}, mcp:worker:*, and every non-MCP automated caller —
	// pinned by internal/policy TestDecide_TaskDismiss_FullActorCorpus
	// (criterion 3). What was given up (V2's honest delta): for a worker
	// console, humanOnly is now the ONLY gate, and an actor prefix is a
	// transport label, not a trust boundary. The SWT-11
	// approve_delivery/send_delivery precedent: listed, human-gated.
	//
	// SWT-32 criterion 39, asserted deliberately rather than by omission (the
	// SWT-20 / SWT-31 precedent). task_reopen moves a task OUT of `closed`, the
	// one status nothing else can leave; an agent that could call it could
	// resurrect work it had just closed, and could undo a human's dismissal. The
	// gate is this transport allowlist ALONE — unlike task_dismiss, the verb is
	// deliberately not in policy.humanOnly (D7: the reconciler calls it as
	// ticketstatus:jira), which is precisely why its absence from the MCP surface
	// is asserted rather than assumed.
	"task_reopen",
}

func TestDraftDeliverySchema_IncludesSlackReply(t *testing.T) {
	srv := mcpserver.New(&fakeExec{}, testWorkerID)
	for _, tool := range srv.ListTools() {
		if tool.Name == "draft_delivery" {
			if !strings.Contains(string(tool.InputSchema), `"slack_reply"`) {
				t.Fatalf("draft_delivery schema lacks slack_reply: %s", tool.InputSchema)
			}
			return
		}
	}
	t.Fatal("draft_delivery not listed")
}

func TestListTools_ExactlyAgentAllowlist(t *testing.T) {
	srv := mcpserver.New(&fakeExec{}, testWorkerID)

	list := srv.ListTools()
	got := make([]string, 0, len(list))
	for _, tl := range list {
		got = append(got, tl.Name)
		if len(tl.InputSchema) == 0 {
			t.Errorf("tool %q has empty InputSchema; tools/list must carry a JSON schema", tl.Name)
			continue
		}
		if !json.Valid(tl.InputSchema) {
			t.Errorf("tool %q InputSchema is not valid JSON: %s", tl.Name, tl.InputSchema)
		}
	}

	sort.Strings(got)
	want := append([]string(nil), wantAgentTools...)
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("tools/list names = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tools/list names = %v, want exactly %v", got, want)
		}
	}

	// Spine-facing tools must be absent.
	present := map[string]bool{}
	for _, n := range got {
		present[n] = true
	}
	for _, n := range spineTools {
		if present[n] {
			t.Errorf("spine-facing tool %q leaked into tools/list", n)
		}
	}
}

// SWT-12 criterion 20 / Q1: mark_delivery_sent is listed WITH a schema and is
// callable, so an interactive session can resolve a stuck Slack send without
// shelling out to `opsctl call`. Recording a leaf-gated draft is deliberately NOT
// reachable this way (criterion 24) — the handler refuses it.
func TestMarkDeliverySent_IsMCPListedAndCallable(t *testing.T) {
	fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{"delivery_id":7,"status":"sent"}`)}}
	srv := mcpserver.New(fx, testWorkerID)

	var schema string
	for _, tool := range srv.ListTools() {
		if tool.Name == "mark_delivery_sent" {
			schema = string(tool.InputSchema)
		}
	}
	if schema == "" {
		t.Fatalf("mark_delivery_sent is not in tools/list (SWT-12 criterion 20)")
	}
	if !strings.Contains(schema, `"delivery_id"`) {
		t.Errorf("mark_delivery_sent schema does not declare delivery_id: %s", schema)
	}

	out, err := srv.CallTool(context.Background(), "mark_delivery_sent", json.RawMessage(`{"delivery_id":7}`))
	if err != nil {
		t.Fatalf("CallTool(mark_delivery_sent): %v", err)
	}
	if !fx.called {
		t.Fatal("CallTool(mark_delivery_sent) did not forward to the executor")
	}
	if fx.lastCall.Tool != "mark_delivery_sent" {
		t.Errorf("forwarded Tool = %q, want mark_delivery_sent", fx.lastCall.Tool)
	}
	if want := "mcp:" + testWorkerID; fx.lastCall.Actor != want {
		t.Errorf("forwarded Actor = %q, want %q", fx.lastCall.Actor, want)
	}
	if !strings.Contains(string(out), `"status":"sent"`) {
		t.Errorf("CallTool output = %s, want the executor result verbatim", out)
	}
}

// The security property Q1 turns on: LISTING mark_delivery_sent does not widen
// WHO may call it. The MCP adapter is a transport, not an authority — the
// human-only gate in the policy matrix is what refuses an autonomous worker
// identity, and it still does with the tool on the agent surface.
func TestMCPListing_DoesNotMakeMarkDeliverySentWorkerCallable(t *testing.T) {
	const workerID = "worker:avviato"

	// The adapter WILL forward it (that is what listing means) with the worker's
	// own MCP identity...
	fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{}`)}}
	srv := mcpserver.New(fx, workerID)
	if _, err := srv.CallTool(context.Background(), "mark_delivery_sent", json.RawMessage(`{"delivery_id":7}`)); err != nil {
		t.Fatalf("CallTool(mark_delivery_sent) as a worker: %v", err)
	}
	if fx.lastCall.Actor != "mcp:"+workerID {
		t.Fatalf("forwarded Actor = %q, want %q (identity is never model-chosen)", fx.lastCall.Actor, "mcp:"+workerID)
	}

	// ...and the policy matrix denies it, human_only, before any handler runs.
	// slack_reply / under limit / not frozen: the ONLY thing standing between an
	// autonomous console and this verb is the actor gate.
	d := policy.Decide(
		policy.Request{Tool: "mark_delivery_sent", Actor: fx.lastCall.Actor},
		policy.Snapshot{SentLastHour: map[string]int{"slack_reply": 0}, Channel: "slack_reply", HourlyLimit: 10},
	)
	if d.Decision != "deny" || d.Rule != "human_only" {
		t.Fatalf("policy on the MCP-listed mark_delivery_sent by %q = %q/%q, want deny/human_only — "+
			"MCP-listing must not make a spine verb worker-callable (SWT-12 Q1)", fx.lastCall.Actor, d.Decision, d.Rule)
	}
}

// SWT-37 criterion 17, the mark_delivery_sent shape above applied to the three
// task verbs. The worker ids are the REAL console shapes: opsworker sets
// OPS_WORKER_ID to the bare --client value, so a console arrives as mcp:acme or
// mcp:acme.main (mcp:worker:* exists only in tests). The adapter forwards each
// verb with that identity, because listing means forwarding. Policy must then
// refuse it: human_only for task_dismiss (V2, unchanged) and mcp_human_only for
// task_close and task_mark_delivered (V1). If the rule is folded into humanOnly
// the rule string changes and this fails, and so does the spine
// (internal/policy criterion 21).
func TestMCPListing_DoesNotMakeTaskVerbsWorkerCallable(t *testing.T) {
	verbs := []struct{ tool, args, rule string }{
		{"task_dismiss", `{"task_id":412,"reason_code":"duplicate"}`, "human_only"},
		{"task_close", `{"task_id":412,"reason":"it's done"}`, "mcp_human_only"},
		{"task_mark_delivered", `{"task_id":412}`, "mcp_human_only"},
	}
	for _, workerID := range []string{"acme", "acme.main"} {
		for _, v := range verbs {
			workerID, v := workerID, v
			t.Run(workerID+"/"+v.tool, func(t *testing.T) {
				fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{}`)}}
				srv := mcpserver.New(fx, workerID)
				if _, err := srv.CallTool(context.Background(), v.tool, json.RawMessage(v.args)); err != nil {
					t.Fatalf("CallTool(%s) as worker %q: %v — the full profile must LIST the verb (criterion 7); "+
						"the refusal belongs to policy, not to the adapter", v.tool, workerID, err)
				}
				if !fx.called || fx.lastCall.Tool != v.tool {
					t.Fatalf("forwarded %+v, want %s", fx.lastCall, v.tool)
				}
				if want := "mcp:" + workerID; fx.lastCall.Actor != want {
					t.Fatalf("forwarded Actor = %q, want %q (identity is never model-chosen)", fx.lastCall.Actor, want)
				}
				d := policy.Decide(policy.Request{Tool: v.tool, Actor: fx.lastCall.Actor}, policy.Snapshot{})
				if d.Decision != "deny" || d.Rule != v.rule {
					t.Errorf("policy on the MCP-listed %s by %q = %s/%s, want deny/%s — MCP-listing must not "+
						"make a task verb worker-callable (SWT-37 V1/V2)", v.tool, fx.lastCall.Actor, d.Decision, d.Rule, v.rule)
				}
			})
		}
	}
}

func TestCallTool_MapsToExecutorCallWithMCPActor(t *testing.T) {
	fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{"task":null}`)}}
	srv := mcpserver.New(fx, testWorkerID)

	out, err := srv.CallTool(context.Background(), "task_get_next", json.RawMessage(`{"client":"acme"}`))
	if err != nil {
		t.Fatalf("CallTool(task_get_next): %v", err)
	}
	if !fx.called {
		t.Fatal("CallTool did not forward to the executor")
	}
	if fx.lastCall.Tool != "task_get_next" {
		t.Errorf("forwarded Tool = %q, want task_get_next", fx.lastCall.Tool)
	}
	if want := "mcp:" + testWorkerID; fx.lastCall.Actor != want {
		t.Errorf("forwarded Actor = %q, want %q", fx.lastCall.Actor, want)
	}
	if string(out) != `{"task":null}` {
		t.Errorf("CallTool output = %s, want the executor result verbatim", out)
	}
}

// The model may put any worker_id in the args; the adapter overwrites it from
// OPS_WORKER_ID before the executor is called. A model cannot act as another
// worker.
func TestCallTool_OverwritesModelSuppliedWorkerID(t *testing.T) {
	fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{}`)}}
	srv := mcpserver.New(fx, testWorkerID)

	// Model tries to impersonate "victim".
	_, err := srv.CallTool(context.Background(), "task_claim",
		json.RawMessage(`{"task_id":42,"worker_id":"victim"}`))
	if err != nil {
		t.Fatalf("CallTool(task_claim): %v", err)
	}

	var forwarded struct {
		TaskID   int64  `json:"task_id"`
		WorkerID string `json:"worker_id"`
	}
	if err := json.Unmarshal(fx.lastCall.Args, &forwarded); err != nil {
		t.Fatalf("unmarshal forwarded args %s: %v", fx.lastCall.Args, err)
	}
	if forwarded.WorkerID != testWorkerID {
		t.Errorf("forwarded worker_id = %q, want %q (must be overwritten from OPS_WORKER_ID)",
			forwarded.WorkerID, testWorkerID)
	}
	if forwarded.TaskID != 42 {
		t.Errorf("forwarded task_id = %d, want 42 (other args must pass through)", forwarded.TaskID)
	}
}

// Spine-facing tools are rejected by name at the MCP layer and never reach the
// executor — the allowlist is the gate.
func TestCallTool_RejectsSpineFacingTools(t *testing.T) {
	for _, name := range spineTools {
		name := name
		t.Run(name, func(t *testing.T) {
			fx := &fakeExec{}
			srv := mcpserver.New(fx, testWorkerID)

			_, err := srv.CallTool(context.Background(), name, json.RawMessage(`{}`))
			if err == nil {
				t.Fatalf("CallTool(%s) = nil error, want rejection (spine-facing, not MCP-listed)", name)
			}
			if fx.called {
				t.Errorf("CallTool(%s) forwarded to the executor; it must be rejected at the MCP layer", name)
			}
		})
	}
}

// An entirely unknown tool name is rejected at the adapter before the executor.
func TestCallTool_RejectsUnknownTool(t *testing.T) {
	fx := &fakeExec{}
	srv := mcpserver.New(fx, testWorkerID)

	if _, err := srv.CallTool(context.Background(), "raw_sql", json.RawMessage(`{}`)); err == nil {
		t.Fatal("CallTool(raw_sql) = nil error, want rejection")
	}
	if fx.called {
		t.Error("CallTool(raw_sql) forwarded to the executor; unknown names must be rejected")
	}
}
