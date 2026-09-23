package mcpserver_test

// SWT-56 (docs/tickets/signal-session-name_SPEC.md) criteria 27, 28 and 33:
// task_context joins the USER profile, pinned READ-ONLY (S12 layer 1). ZERO
// network: adapter_test.go's fakeExec, task_capture_test.go's forwardCall,
// queue_tools_test.go's forwardedKeys/keyList.
//
// IMPOSED SURFACE (S12, criterion 27):
//
//	userProfileTools gains "task_context" (fifteen tools); readProfileTools unchanged
//	userProfilePins["task_context"] = {"worker_id": "", "require_read_only": "true"}
//
// Here the PIN is tested alone: whatever worker_id the model sends (none, the
// holder's, a Kelvin-sign fold key), the executor receives worker_id "" and
// require_read_only "true". The handler flag alone is internal/tools
// taskcontext_readonly_integration_test.go; both together through a real
// executor is user_context_integration_test.go.
//
// GREENFIELD NOTE — EXPECTED RED: the user profile does not list task_context,
// so every user-profile call is refused at the MCP layer, and the description
// still says "As the claim holder this marks work started".
//
// MUTATION: delete the task_context entry from userProfilePins → the pin rows.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/mcpserver"
)

func TestUserProfile_TaskContextIsPinnedReadOnly(t *testing.T) {
	// The Kelvin sign U+212A folds to k under encoding/json's case-insensitive
	// field match; built from its value, never pasted (criterion 28).
	kelvin := `{"task_id":1,"wor` + string(rune(0x212A)) + `er_id":"manual:salvo"}`
	for _, tc := range []struct{ name, args string }{
		{"task_id only", `{"task_id":1}`},
		{"the holder's worker_id", `{"task_id":1,"worker_id":"manual:salvo"}`},
		{"a Kelvin-sign worker key", kelvin},
		{"require_read_only false", `{"task_id":1,"require_read_only":"false"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{"task":{}}`)}}
			srv := mcpserver.NewWithProfile(fx, "manual:salvo", mcpserver.ProfileUser)
			args := forwardCall(t, srv, fx, "task_context", tc.args)
			if fx.lastCall.Actor != "mcp:manual:salvo" {
				t.Errorf("forwarded Actor = %q, want mcp:manual:salvo", fx.lastCall.Actor)
			}
			if got := keyList(args); got != "require_read_only,task_id,worker_id" {
				t.Errorf("forwarded keys = %s, want require_read_only,task_id,worker_id — the pins OVERWRITE, and a "+
					"fold-equivalent worker key is dropped. Args: %s", got, fx.lastCall.Args)
			}
			if string(args["worker_id"]) != `""` {
				t.Errorf("forwarded worker_id = %s, want \"\" (S12 layer 1: a user-profile read is never the holder's fetch)",
					args["worker_id"])
			}
			if string(args["require_read_only"]) != `"true"` {
				t.Errorf("forwarded require_read_only = %s, want \"true\" (S12 layer 2's marker, pinned)", args["require_read_only"])
			}
		})
	}

	t.Run("fold-duplicate worker keys are refused", func(t *testing.T) {
		fx := &fakeExec{}
		srv := mcpserver.NewWithProfile(fx, "manual:salvo", mcpserver.ProfileUser)
		if _, err := srv.CallTool(context.Background(), "task_context",
			json.RawMessage(`{"task_id":1,"worker_id":"a","WORKER_ID":"b"}`)); err == nil {
			t.Errorf("the user profile accepted {worker_id, WORKER_ID}; rejectFoldDuplicateKeys refuses the pair")
		}
		if fx.called {
			t.Errorf("an ambiguous args object reached the executor")
		}
	})

	// Control: the full profile has no pin, so worker consoles keep the holder
	// transition (internal/worker/loop.go fetchContext).
	t.Run("full profile control", func(t *testing.T) {
		fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{}`)}}
		args := forwardCall(t, mcpserver.New(fx, "manual:salvo"), fx, "task_context", `{"task_id":1}`)
		if got := keyList(args); got != "task_id,worker_id" {
			t.Errorf("full-profile task_context forwarded keys = %s, want task_id,worker_id — NO require_read_only", got)
		}
		if string(args["worker_id"]) != `"manual:salvo"` {
			t.Errorf("full-profile worker_id = %s, want \"manual:salvo\" (the holder transition stays)", args["worker_id"])
		}
	})

	t.Run("the read profile never lists it", func(t *testing.T) {
		fx := &fakeExec{}
		srv := mcpserver.NewWithProfile(fx, "manual:salvo", mcpserver.ProfileRead)
		if _, err := srv.CallTool(context.Background(), "task_context", json.RawMessage(`{"task_id":1}`)); err == nil || fx.called {
			t.Errorf("ProfileRead served task_context; it has no pins and is the fail-closed floor (S12)")
		}
	})
}

// Criteria 27 and 33: listed in both profiles as ONE entry, the schema
// byte-unchanged, and S14's description.
func TestTaskContext_OneEntryBothProfiles(t *testing.T) {
	find := func(ts []mcpserver.Tool) *mcpserver.Tool {
		for i := range ts {
			if ts[i].Name == "task_context" {
				return &ts[i]
			}
		}
		return nil
	}
	full := mcpserver.New(&fakeExec{}, testWorkerID).ListTools()
	user := mcpserver.NewWithProfile(&fakeExec{}, "manual:salvo", mcpserver.ProfileUser).ListTools()
	if len(full) != 31 {
		t.Errorf("the full profile lists %d tools, want 31 (27 → 28 with SWT-72's task_requeue; 28 → 30 with SWT-74's task_match + task_attach; 30 → 31 with SWT-77's send_slack_reply)", len(full))
	}
	if len(user) != 19 {
		t.Errorf("the user profile lists %d tools, want 19 (14 → 15 criterion 27; 15 → 16 with SWT-72's task_requeue; 16 → 18 with SWT-74's task_match + task_attach; 18 → 19 with SWT-77's send_slack_reply)", len(user))
	}
	f, u := find(full), find(user)
	if f == nil || u == nil {
		t.Fatalf("task_context listed: full %v, user %v — want both (criterion 27)", f != nil, u != nil)
	}
	if f.Description != u.Description || string(f.InputSchema) != string(u.InputSchema) {
		t.Errorf("task_context's user-profile entry differs from the full profile's: one spelling (criterion 33)")
	}

	var got, want map[string]any
	if err := json.Unmarshal(f.InputSchema, &got); err != nil {
		t.Fatalf("task_context schema: %v", err)
	}
	_ = json.Unmarshal([]byte(`{"type":"object","properties":{"task_id":{"type":"integer"}},"required":["task_id"]}`), &want)
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Errorf("task_context schema = %s, want it unchanged %s — {task_id} only (criterion 33)", g, w)
	}
	if s := string(f.InputSchema); strings.Contains(s, "worker_id") || strings.Contains(s, "require_") {
		t.Errorf("task_context's schema exposes worker_id or a pin (criterion 33)")
	}

	d := f.Description
	for _, tok := range []string{"read-only", "task_id", "last 50 events", "log", "body"} {
		if !strings.Contains(d, tok) {
			t.Errorf("task_context description does not say %q (S14). Description: %q", tok, d)
		}
	}
	if strings.Contains(d, "As the claim holder this marks work started") {
		t.Errorf("task_context description still says `As the claim holder this marks work started`: false for the user "+
			"profile (S14). Description: %q", d)
	}
}
