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
	// AMENDED by SWT-56 (signal-session-name) criterion 20: a third property, session.
	if len(s.Properties) != 3 || s.Properties["task_id"].Type != "integer" {
		t.Errorf("task_signal properties = %s, want exactly task_id (integer), state and session (SWT-56 criterion 20)", tl.InputSchema)
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

	// SWT-56 criterion 20: the session property, its cap pinned to the handler's
	// const (the TestTaskSetPrioritySchema way), and the words a model reads.
	var ss struct {
		Properties map[string]struct {
			Type        string `json:"type"`
			MaxLength   *int   `json:"maxLength"`
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tl.InputSchema, &ss); err != nil {
		t.Fatalf("task_signal InputSchema: %v", err)
	}
	if sp, ok := ss.Properties["session"]; !ok {
		t.Errorf("task_signal schema has no session property (criterion 20)")
	} else {
		if sp.Type != "string" {
			t.Errorf("task_signal session type = %q, want string", sp.Type)
		}
		if sp.MaxLength == nil || *sp.MaxLength != tools.SessionNameMax {
			t.Errorf("task_signal session maxLength = %v, want tools.SessionNameMax = %d", sp.MaxLength, tools.SessionNameMax)
		}
		for _, want := range []string{"tmux window name", "Your swb session name is <name>", "required for working and needs_input",
			"the name only"} { // swb 431: the hook's name replaces SWT-56's ListAgents name
			if !strings.Contains(sp.Description, want) {
				t.Errorf("task_signal session description does not say %q (criterion 20): %q", want, sp.Description)
			}
		}
	}
	const always = "Always pass session (your swb session name: the tmux window name, else the last folder of the working " +
		"directory, exactly as the SessionStart hook states it) with working and needs_input: the board shows it so " +
		"Salvador knows which session to reply in."
	if !strings.Contains(tl.Description, always) {
		t.Errorf("task_signal description lacks %q (criterion 20)", always)
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
	if len(full) != 31 {
		t.Errorf("the full profile lists %d tools, want 31 (26 → 27 criterion 23; 27 → 28 with SWT-72's task_requeue; 28 → 30 with SWT-74's task_match + task_attach; 30 → 31 with SWT-77's send_slack_reply)", len(full))
	}
	// SWT-56 criterion 27: task_context makes the user profile fifteen.
	if len(user) != 19 {
		t.Errorf("the user profile lists %d tools, want 19 (14 → 15 SWT-56 criterion 27; 15 → 16 with SWT-72's task_requeue; 16 → 18 with SWT-74's task_match + task_attach; 18 → 19 with SWT-77's send_slack_reply)", len(user))
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

	// AMENDED by SWT-56 (signal-session-name) criterion 21: a model-supplied session
	// (emoji ones included) reaches the executor byte-identical — it is data, not a
	// pin — while worker_id is still overwritten.
	for _, sess := range []string{"kube-c7", "Fix the board " + string(rune(0x1F6A6)),
		string([]rune{0x1F468, 0x200D, 0x1F469, 0x200D, 0x1F467})} {
		fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{}`)}}
		srv := mcpserver.NewWithProfile(fx, "manual:salvo", mcpserver.ProfileUser)
		in, _ := json.Marshal(map[string]any{"task_id": 412, "state": "needs_input", "session": sess, "worker_id": "victim"})
		if _, err := srv.CallTool(context.Background(), "task_signal", in); err != nil {
			t.Fatalf("user profile refused task_signal with session %q: %v", sess, err)
		}
		args := forwardedKeys(t, fx.lastCall.Args)
		var got string
		_ = json.Unmarshal(args["session"], &got)
		if got != sess {
			t.Errorf("forwarded session = %q, want %q byte-identical (S1: never rewritten)", got, sess)
		}
		if string(args["worker_id"]) != `"manual:salvo"` {
			t.Errorf("forwarded worker_id = %s, want \"manual:salvo\" (overwritten)", args["worker_id"])
		}
		if k := keyList(args); k != "session,state,task_id,worker_id" {
			t.Errorf("forwarded keys = %s, want session,state,task_id,worker_id — task_signal still has no pin", k)
		}
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
