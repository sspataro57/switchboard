package mcpserver_test

// SWT-38 (docs/tickets/mcp-task-capture_SPEC.md) criteria 12, 13, 14, 15 and
// 17: the task-capture surface on MCP — task_set_priority's schema, the hidden
// require_assignee_type pin, create_task's rewritten description, the user
// profile's pin injection, and "listing does not make it worker-callable".
// ZERO network: the adapter is driven with adapter_test.go's fakeExec;
// listedTool is mail_tools_test.go's, parseVerbSchema/assertVerbProps are
// task_verbs_test.go's, forwardedKeys/keyList/sortedCopy are
// queue_tools_test.go's. NOT capture_tools_test.go, which is SWT-17's
// capture-rules file.
//
// IMPOSED SURFACE (SPEC C4, C5, C7, "API / MCP tool changes"):
//
//	// internal/mcpserver/schemas.go — one new agentTools entry
//	task_set_priority {"task_id": integer,
//	                   "priority": integer, minimum tools.PriorityMin, maximum tools.PriorityMax,
//	                   "reason": string}, required [task_id, priority]
//	// create_task keeps its schema; its Description is rewritten (criterion 14).
//
//	// internal/mcpserver/adapter.go
//	type Server struct { …; pins map[string]map[string]string } // fixed by NewWithProfile, no env input
//	// ProfileUser: {"create_task":     {"require_assignee_type": "human"},
//	//               "task_append_log": {"require_assignee_type": "human"}}
//	// ProfileFull and ProfileRead: none.
//	// CallTool applies the pins AFTER injectWorkerID, by OVERWRITE — a
//	// model-supplied value is replaced, exactly as worker_id is.
//
//	// internal/tools/priority.go
//	const PriorityMin, PriorityMax; var PriorityLevels []string
//
// GREENFIELD NOTE — EXPECTED RED. tools.PriorityMin, tools.PriorityMax and
// tools.PriorityLevels do not exist, so this package's tests compile-FAIL until
// priority.go declares them. Once it compiles: listedTool fails with `tool
// "task_set_priority" is not MCP-listed`; the user profile refuses create_task
// and task_append_log at the MCP layer ("not available over MCP"); the full
// profile refuses task_set_priority; create_task's description lacks human /
// worker / claude. TestNoSchemaExposesRequireAssigneeType is GREEN today and
// must stay green.
//
// MUTATIONS THAT MUST TURN THIS FILE RED:
//   - M-a: drop the ProfileUser pins → TestUserProfile_PinsHumanAssignee (every
//     create_task / task_append_log row under the user profile).
//   - M-b: pins merge instead of overwrite (the model value wins) → the
//     "model-supplied pin is overwritten" rows.
//   - pins applied to every profile (not per profile) → the full-profile rows.
//   - a pin on any third user-profile tool → the "no pin" rows.
//   - M-d: remove task_set_priority from policy.humanOnly →
//     TestMCPListing_DoesNotMakeSetPriorityWorkerCallable.
//   - adding require_assignee_type to any schema → TestNoSchemaExposesRequireAssigneeType.

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/mcpserver"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

// ---- criterion 12: task_set_priority's schema ---------------------------------

type priorityBoundsProp struct {
	Type    string   `json:"type"`
	Minimum *float64 `json:"minimum"`
	Maximum *float64 `json:"maximum"`
}

func TestTaskSetPrioritySchema(t *testing.T) {
	tl := listedTool(t, "task_set_priority")
	s := parseVerbSchema(t, "task_set_priority", tl.InputSchema)
	assertVerbProps(t, "task_set_priority", s,
		map[string]string{"task_id": "integer", "priority": "integer", "reason": "string"},
		[]string{"task_id", "priority"})

	// The bounds a model reads are THE scale (C7): one spelling, in
	// internal/tools/priority.go.
	var b struct {
		Properties map[string]priorityBoundsProp `json:"properties"`
	}
	if err := json.Unmarshal(tl.InputSchema, &b); err != nil {
		t.Fatalf("task_set_priority InputSchema: %v", err)
	}
	p := b.Properties["priority"]
	if p.Minimum == nil || *p.Minimum != float64(tools.PriorityMin) {
		t.Errorf("task_set_priority priority minimum = %v, want tools.PriorityMin = %d", p.Minimum, tools.PriorityMin)
	}
	if p.Maximum == nil || *p.Maximum != float64(tools.PriorityMax) {
		t.Errorf("task_set_priority priority maximum = %v, want tools.PriorityMax = %d", p.Maximum, tools.PriorityMax)
	}

	d := strings.ToLower(tl.Description)
	if len(tools.PriorityLevels) != 4 {
		t.Errorf("POSITIVE CONTROL: tools.PriorityLevels = %v, want the four levels", tools.PriorityLevels)
	}
	for _, level := range tools.PriorityLevels {
		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(level) + `\b`).MatchString(d) {
			t.Errorf("task_set_priority description does not name the level %q — the model maps Salvador's words "+
				"to a number from here (C7). Description: %q", level, tl.Description)
		}
	}
	for _, want := range []struct{ re, why string }{
		{`higher[^.]{0,40}first`, "higher runs first (taskQueueOrder): 3 is urgent, not 0"},
		{`human sessions? only|human identities only|human.{0,40}only|worker.{0,80}(refused|denied)|(refused|denied).{0,80}worker`,
			"human sessions only; a worker console is refused by policy (C6)"},
		{`does not claim|doesn't claim|never claims|no claim`, "it reorders; it does not claim (C5 scope)"},
		{`nothing is sent|sends nothing|never sends|does not send|no message is sent`, "nothing is sent (C9)"},
	} {
		if !regexp.MustCompile(want.re).MatchString(d) {
			t.Errorf("task_set_priority description does not match /%s/ — %s. Description: %q", want.re, want.why, tl.Description)
		}
	}
	if strings.Contains(d, "worker_id") || strings.Contains(string(tl.InputSchema), "worker_id") {
		t.Errorf("task_set_priority mentions worker_id; identity is injected from OPS_WORKER_ID, never model-supplied")
	}
}

// ---- criterion 13: the pin is hidden ------------------------------------------

// GREEN TODAY, AND MUST STAY GREEN. require_assignee_type is absent from every
// schema (the SWT-37 criterion-28 expect_task_status precedent): it only
// narrows, so a model has no reason to see it, and a schema that advertised it
// would invite a model to "fix" a refusal by passing a different value — which
// the user profile overwrites anyway, but the full profile would not.
func TestNoSchemaExposesRequireAssigneeType(t *testing.T) {
	listed := 0
	for _, tl := range mcpserver.New(&fakeExec{}, testWorkerID).ListTools() {
		listed++
		if strings.Contains(string(tl.InputSchema), "require_assignee_type") {
			t.Errorf("%s's schema exposes require_assignee_type: the pin is hidden (C4) — the adapter sets it for "+
				"the user profile and a full-profile caller may only narrow itself with it. Schema: %s",
				tl.Name, tl.InputSchema)
		}
	}
	if listed < 20 {
		t.Fatalf("POSITIVE CONTROL FAILED: the full profile lists only %d tools", listed)
	}
}

// ---- criterion 14: create_task's description ----------------------------------

// Shared by both profiles (the user profile takes its entries FROM agentTools),
// so it must be true for both: a worker console reads it too.
func TestCreateTaskDescription_SaysWhoPicksItUp(t *testing.T) {
	tl := listedTool(t, "create_task")
	d := strings.ToLower(tl.Description)
	for _, want := range []struct{ re, why string }{
		{`human`, "the default assignee is human — Salvador's lane — which no worker console picks up (C1)"},
		{`worker`, "…the worker console / worker queue is what assignee_type routes to"},
		{`claude`, "claude puts the task in the worker queue for that project's client (C1: a routing field)"},
		{`refus`, "the user-scope install REFUSES claude (C4), so a model in another repo is not surprised by it"},
	} {
		if !regexp.MustCompile(want.re).MatchString(d) {
			t.Errorf("create_task description does not match /%s/ — %s. Description: %q", want.re, want.why, tl.Description)
		}
	}
	if strings.Contains(string(tl.InputSchema), `"status"`) || strings.Contains(string(tl.InputSchema), "parent_id") {
		t.Errorf("create_task schema gained status or parent_id; SWT-38 rewrites only the description. Schema: %s",
			tl.InputSchema)
	}
}

// ---- criterion 15: the user profile pins human --------------------------------

func forwardCall(t *testing.T, srv *mcpserver.Server, fx *fakeExec, tool, args string) map[string]json.RawMessage {
	t.Helper()
	if _, err := srv.CallTool(context.Background(), tool, json.RawMessage(args)); err != nil {
		t.Fatalf("CallTool(%s, %s): %v", tool, args, err)
	}
	if !fx.called || fx.lastCall.Tool != tool {
		t.Fatalf("forwarded %+v, want %s", fx.lastCall, tool)
	}
	return forwardedKeys(t, fx.lastCall.Args)
}

func TestUserProfile_PinsHumanAssignee(t *testing.T) {
	user := func() (*mcpserver.Server, *fakeExec) {
		fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{"task_id":900}`)}}
		return mcpserver.NewWithProfile(fx, "manual:salvo", mcpserver.ProfileUser), fx
	}

	t.Run("user/create_task gains the pin", func(t *testing.T) {
		srv, fx := user()
		args := forwardCall(t, srv, fx, "create_task", `{"project":"p","title":"t"}`)
		if fx.lastCall.Actor != "mcp:manual:salvo" {
			t.Errorf("forwarded Actor = %q, want mcp:manual:salvo", fx.lastCall.Actor)
		}
		if got := keyList(args); got != "project,require_assignee_type,title,worker_id" {
			t.Errorf("forwarded keys = %s, want project,require_assignee_type,title,worker_id — the user profile adds "+
				"worker_id and the pin, nothing else. Args: %s", got, fx.lastCall.Args)
		}
		if string(args["require_assignee_type"]) != `"human"` {
			t.Errorf("forwarded require_assignee_type = %s, want \"human\" (C4)", args["require_assignee_type"])
		}
		if string(args["worker_id"]) != `"manual:salvo"` {
			t.Errorf("forwarded worker_id = %s, want \"manual:salvo\"", args["worker_id"])
		}
	})

	// M-b. The pin OVERWRITES: an injected "require_assignee_type":"claude" must
	// not unlock claude work. assignee_type itself is left untouched — the
	// HANDLER refuses it (criterion 20(b)), which tells the model the truth.
	t.Run("user/create_task model-supplied pin is overwritten", func(t *testing.T) {
		srv, fx := user()
		args := forwardCall(t, srv, fx, "create_task",
			`{"project":"p","title":"t","assignee_type":"claude","require_assignee_type":"claude"}`)
		if string(args["require_assignee_type"]) != `"human"` {
			t.Errorf("forwarded require_assignee_type = %s, want \"human\": the pin must OVERWRITE a model value, "+
				"exactly as worker_id is overwritten — otherwise injected text in any repo unlocks the console queue",
				args["require_assignee_type"])
		}
		if string(args["assignee_type"]) != `"claude"` {
			t.Errorf("forwarded assignee_type = %s, want \"claude\" untouched: the adapter does not silently "+
				"rewrite it; the validator refuses it (C1)", args["assignee_type"])
		}
	})

	t.Run("user/task_append_log gains the pin", func(t *testing.T) {
		srv, fx := user()
		args := forwardCall(t, srv, fx, "task_append_log", `{"task_id":412,"message":"step 1"}`)
		if got := keyList(args); got != "message,require_assignee_type,task_id,worker_id" {
			t.Errorf("forwarded keys = %s, want message,require_assignee_type,task_id,worker_id. Args: %s", got, fx.lastCall.Args)
		}
		if string(args["require_assignee_type"]) != `"human"` {
			t.Errorf("forwarded require_assignee_type = %s, want \"human\" (C4: a log line on a claude task is "+
				"text inside a future worker prompt)", args["require_assignee_type"])
		}
	})

	t.Run("user/task_append_log model-supplied pin is overwritten", func(t *testing.T) {
		srv, fx := user()
		args := forwardCall(t, srv, fx, "task_append_log",
			`{"task_id":412,"message":"step 1","require_assignee_type":"claude"}`)
		if string(args["require_assignee_type"]) != `"human"` {
			t.Errorf("forwarded require_assignee_type = %s, want \"human\" (OVERWRITE)", args["require_assignee_type"])
		}
	})

	// The pins are EXACTLY two tools (C4's map). Every other user-profile tool
	// is forwarded without the key — a pin on task_set_priority would mean
	// nothing to its handler, and on a read it would be noise in the audit args.
	for _, tc := range []struct{ tool, args, keys string }{
		{"task_set_priority", `{"task_id":412,"priority":3}`, "priority,task_id,worker_id"},
		{"task_close", `{"task_id":412,"reason":"done"}`, "reason,task_id,worker_id"},
		{"task_dismiss", `{"task_id":412,"reason_code":"duplicate"}`, "reason_code,task_id,worker_id"},
		{"task_mark_delivered", `{"task_id":412}`, "task_id,worker_id"},
		{"task_list", `{"project":"p"}`, "project,worker_id"},
		{"task_get_next", `{"client":"acme"}`, "client,worker_id"},
		{"project_list", `{}`, "worker_id"},
	} {
		tc := tc
		t.Run("user/"+tc.tool+" carries no pin", func(t *testing.T) {
			srv, fx := user()
			args := forwardCall(t, srv, fx, tc.tool, tc.args)
			if _, ok := args["require_assignee_type"]; ok {
				t.Errorf("%s was forwarded WITH require_assignee_type; the user profile pins only create_task and "+
					"task_append_log (C4)", tc.tool)
			}
			if got := keyList(args); got != tc.keys {
				t.Errorf("%s forwarded keys = %s, want %s", tc.tool, got, tc.keys)
			}
		})
	}

	// The full profile has NO pins: worker consoles and this repo's session keep
	// creating claude tasks and logging on them (C1). A model-supplied value
	// passes through unchanged — it can only narrow its own call.
	full := func() (*mcpserver.Server, *fakeExec) {
		fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{}`)}}
		return mcpserver.New(fx, "acme"), fx
	}
	t.Run("full/create_task is not pinned", func(t *testing.T) {
		srv, fx := full()
		args := forwardCall(t, srv, fx, "create_task", `{"project":"p","title":"t","assignee_type":"claude"}`)
		if got := keyList(args); got != "assignee_type,project,title,worker_id" {
			t.Errorf("full-profile create_task forwarded keys = %s, want assignee_type,project,title,worker_id — "+
				"no pin outside the user profile (C4). Args: %s", got, fx.lastCall.Args)
		}
	})
	t.Run("full/task_append_log is not pinned", func(t *testing.T) {
		srv, fx := full()
		args := forwardCall(t, srv, fx, "task_append_log", `{"task_id":412,"message":"m"}`)
		if got := keyList(args); got != "message,task_id,worker_id" {
			t.Errorf("full-profile task_append_log forwarded keys = %s, want message,task_id,worker_id. Args: %s",
				got, fx.lastCall.Args)
		}
	})
	t.Run("full/a model-supplied value passes through", func(t *testing.T) {
		srv, fx := full()
		args := forwardCall(t, srv, fx, "create_task",
			`{"project":"p","title":"t","assignee_type":"claude","require_assignee_type":"claude"}`)
		if string(args["require_assignee_type"]) != `"claude"` {
			t.Errorf("full-profile create_task forwarded require_assignee_type = %s, want \"claude\" unchanged",
				args["require_assignee_type"])
		}
		srv, fx = full()
		args = forwardCall(t, srv, fx, "task_append_log", `{"task_id":412,"message":"m","require_assignee_type":"human"}`)
		if string(args["require_assignee_type"]) != `"human"` {
			t.Errorf("full-profile task_append_log forwarded require_assignee_type = %s, want \"human\" unchanged",
				args["require_assignee_type"])
		}
	})
}

// ---- criterion 17: listing does not widen who may call ------------------------

// The SWT-37 criterion 17 shape applied to task_set_priority. The worker ids
// are the REAL console shapes (opsworker sets OPS_WORKER_ID to the bare
// --client value). The full profile forwards the call (listing means
// forwarding); policy must then refuse it, human_only (C6).
func TestMCPListing_DoesNotMakeSetPriorityWorkerCallable(t *testing.T) {
	for _, workerID := range []string{"acme", "acme.main"} {
		workerID := workerID
		t.Run(workerID, func(t *testing.T) {
			fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{}`)}}
			srv := mcpserver.New(fx, workerID)
			if _, err := srv.CallTool(context.Background(), "task_set_priority",
				json.RawMessage(`{"task_id":412,"priority":3}`)); err != nil {
				t.Fatalf("CallTool(task_set_priority) as worker %q: %v — the full profile must LIST it (criterion "+
					"11); the refusal belongs to policy, not the adapter", workerID, err)
			}
			if want := "mcp:" + workerID; fx.lastCall.Actor != want {
				t.Fatalf("forwarded Actor = %q, want %q (identity is never model-chosen)", fx.lastCall.Actor, want)
			}
			d := policy.Decide(policy.Request{Tool: "task_set_priority", Actor: fx.lastCall.Actor}, policy.Snapshot{})
			if d.Decision != "deny" || d.Rule != "human_only" {
				t.Errorf("policy on the MCP-listed task_set_priority by %q = %s/%s, want deny/human_only — a "+
					"worker must never choose its own work (C6)", fx.lastCall.Actor, d.Decision, d.Rule)
			}
		})
	}

	// Positive control: the same call from the user-scope session is allowed.
	fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{}`)}}
	srv := mcpserver.NewWithProfile(fx, "manual:salvo", mcpserver.ProfileUser)
	if _, err := srv.CallTool(context.Background(), "task_set_priority",
		json.RawMessage(`{"task_id":412,"priority":3}`)); err != nil {
		t.Fatalf("user profile refused task_set_priority: %v", err)
	}
	if d := policy.Decide(policy.Request{Tool: "task_set_priority", Actor: fx.lastCall.Actor}, policy.Snapshot{}); d.Decision != "allow" {
		t.Errorf("task_set_priority by %q = %s/%s, want allow", fx.lastCall.Actor, d.Decision, d.Rule)
	}
}
