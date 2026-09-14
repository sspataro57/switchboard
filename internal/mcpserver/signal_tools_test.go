package mcpserver_test

// SWT-52 (docs/tickets/board-status-lights_SPEC.md) criteria 23 and 24:
// task_signal on the MCP surface. ZERO network: adapter_test.go's fakeExec,
// mail_tools_test.go's listedTool, queue_tools_test.go's forwardedKeys/keyList.
//
// IMPOSED SURFACE: an agentTools entry
//
//	task_signal {"task_id": integer, "state": {"type":"string","enum":["working","needs_input","clear"]}},
//	            required [task_id, state]
//
// listed in both profiles, with no user-profile pin. The enum equals
// tools.SignalStates() (imposed in internal/tools/signal_test.go).
//
// GREENFIELD NOTE — EXPECTED RED: tools.SignalStates does not exist, so this
// package's tests compile-FAIL; once it compiles, listedTool fails with
// `tool "task_signal" is not MCP-listed`.

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

func TestTaskSignalSchema(t *testing.T) {
	tl := listedTool(t, "task_signal")
	var s struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Type string   `json:"type"`
			Enum []string `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(tl.InputSchema, &s); err != nil {
		t.Fatalf("task_signal InputSchema: %v (%s)", err, tl.InputSchema)
	}
	if s.Type != "object" {
		t.Errorf("task_signal schema type = %q, want object", s.Type)
	}
	if len(s.Properties) != 2 || s.Properties["task_id"].Type != "integer" {
		t.Errorf("task_signal properties = %s, want exactly task_id (integer) and state (criterion 23)", tl.InputSchema)
	}
	st, ok := s.Properties["state"]
	if !ok {
		t.Fatalf("task_signal schema has no state property")
	}
	if st.Type != "" && st.Type != "string" {
		t.Errorf("task_signal state type = %q, want string", st.Type)
	}
	if strings.Join(st.Enum, ",") != strings.Join(tools.SignalStates(), ",") {
		t.Errorf("task_signal state enum = %v, want tools.SignalStates() = %v — the enum a model reads is the "+
			"handler's set (the TestTaskSetPrioritySchema shape)", st.Enum, tools.SignalStates())
	}
	if strings.Join(sortedCopy(s.Required), ",") != "state,task_id" {
		t.Errorf("task_signal required = %v, want [task_id state]", s.Required)
	}
	if strings.Contains(string(tl.InputSchema), "worker_id") || strings.Contains(string(tl.InputSchema), "require_") {
		t.Errorf("task_signal's schema exposes worker_id or a pin; identity is injected and task_signal has no pins (D13)")
	}

	d := strings.ToLower(tl.Description)
	for _, want := range []struct{ re, why string }{
		{`human tasks? only`, "human tasks only (D7)"},
		{`(changes|change) no status|never changes (its |the )?status|does not change (its |the )?status`, "it changes no status (D4)"},
		{`nothing but the state`, "it records nothing but the state (owner: switchboard is a status board)"},
		{`answer.{0,80}console`, "Salvador answers in the console…"},
		{`never (through|in|via) switchboard`, "…never through switchboard"},
		{`finish.{0,80}task_close`, "finishing is task_close"},
	} {
		if !regexp.MustCompile(want.re).MatchString(d) {
			t.Errorf("task_signal description does not match /%s/ — %s. Description: %q", want.re, want.why, tl.Description)
		}
	}
}

// Criteria 23/24: the counts, both profiles, the same entry.
func TestTaskSignal_ListedInBothProfiles(t *testing.T) {
	full := mcpserver.New(&fakeExec{}, testWorkerID).ListTools()
	user := mcpserver.NewWithProfile(&fakeExec{}, "manual:salvo", mcpserver.ProfileUser).ListTools()
	if len(full) != 27 {
		t.Errorf("the full profile lists %d tools, want 27 (26 → 27, criterion 23)", len(full))
	}
	if len(user) != 14 {
		t.Errorf("the user profile lists %d tools, want 14 (13 → 14, criterion 23)", len(user))
	}
	find := func(ts []mcpserver.Tool) *mcpserver.Tool {
		for i := range ts {
			if ts[i].Name == "task_signal" {
				return &ts[i]
			}
		}
		return nil
	}
	f, u := find(full), find(user)
	if f == nil || u == nil {
		t.Fatalf("task_signal listed: full %v, user %v — want both (D7)", f != nil, u != nil)
	}
	if f.Description != u.Description || string(f.InputSchema) != string(u.InputSchema) {
		t.Errorf("task_signal's user-profile entry differs from the full profile's: one spelling (V3)")
	}
}

// userProfilePins unchanged: the user profile forwards task_signal with the
// injected worker_id and nothing else.
func TestUserProfile_ForwardsTaskSignalWithoutPin(t *testing.T) {
	fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{"task_id":412,"state":"working","changed":true,"state_at":"x"}`)}}
	srv := mcpserver.NewWithProfile(fx, "manual:salvo", mcpserver.ProfileUser)
	if _, err := srv.CallTool(context.Background(), "task_signal", json.RawMessage(`{"task_id":412,"state":"working"}`)); err != nil {
		t.Fatalf("user profile refused task_signal: %v", err)
	}
	if fx.lastCall.Tool != "task_signal" || fx.lastCall.Actor != "mcp:manual:salvo" {
		t.Errorf("forwarded %q as %q, want task_signal as mcp:manual:salvo", fx.lastCall.Tool, fx.lastCall.Actor)
	}
	if got := keyList(forwardedKeys(t, fx.lastCall.Args)); got != "state,task_id,worker_id" {
		t.Errorf("forwarded keys = %s, want state,task_id,worker_id — task_signal has no pin (D7, D13)", got)
	}
}

// Criterion 24: listing does not make it worker-callable (the
// TestMCPListing_DoesNotMakeSetPriorityWorkerCallable shape).
func TestMCPListing_DoesNotMakeTaskSignalWorkerCallable(t *testing.T) {
	for _, workerID := range []string{"acme", "acme.main"} {
		fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{}`)}}
		srv := mcpserver.New(fx, workerID)
		if _, err := srv.CallTool(context.Background(), "task_signal",
			json.RawMessage(`{"task_id":412,"state":"working"}`)); err != nil {
			t.Fatalf("CallTool(task_signal) as worker %q: %v — the full profile must LIST it; the refusal is policy's", workerID, err)
		}
		d := policy.Decide(policy.Request{Tool: "task_signal", Actor: fx.lastCall.Actor}, policy.Snapshot{})
		if d.Decision != "deny" || d.Rule != "human_only" {
			t.Errorf("policy on task_signal by %q = %s/%s, want deny/human_only (D7)", fx.lastCall.Actor, d.Decision, d.Rule)
		}
	}
	fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{}`)}}
	srv := mcpserver.NewWithProfile(fx, "manual:salvo", mcpserver.ProfileUser)
	if _, err := srv.CallTool(context.Background(), "task_signal", json.RawMessage(`{"task_id":412,"state":"clear"}`)); err != nil {
		t.Fatalf("user profile refused task_signal: %v", err)
	}
	if d := policy.Decide(policy.Request{Tool: "task_signal", Actor: fx.lastCall.Actor}, policy.Snapshot{}); d.Decision != "allow" {
		t.Errorf("task_signal by %q = %s/%s, want allow", fx.lastCall.Actor, d.Decision, d.Rule)
	}
}
