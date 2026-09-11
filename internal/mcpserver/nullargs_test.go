package mcpserver_test

// SWT-38 follow-up (Codex pass 3, go-reviewer confirm): a client sending
// "arguments": null reached overwriteArgs as the bytes `null`, which decode into
// a nil map without error; the worker_id/pin assignment then panicked and took
// the stdio server down. null is now read as an empty object; arrays and
// scalars stay refused.
//
// MUTATION THAT MUST TURN THIS RED: drop the `if m == nil` guard in
// overwriteArgs → the null cases panic.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/mcpserver"
)

func TestCallTool_NullArgumentsDoNotPanic(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tool    string
		profile mcpserver.Profile
	}{
		{"user profile, pinned tool", "create_task", mcpserver.ProfileUser},
		{"user profile, unpinned tool", "task_list", mcpserver.ProfileUser},
		{"full profile", "task_get_next", mcpserver.ProfileFull},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			fx := &fakeExec{}
			srv := mcpserver.NewWithProfile(fx, "manual:salvo", tc.profile)
			if _, err := srv.CallTool(context.Background(), tc.tool, json.RawMessage(`null`)); err != nil {
				t.Fatalf("CallTool(%s, null) = %v, want forwarded as an empty object", tc.tool, err)
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(fx.lastCall.Args, &m); err != nil || m == nil {
				t.Fatalf("forwarded args %s are not a JSON object (%v)", fx.lastCall.Args, err)
			}
			if string(m["worker_id"]) != `"manual:salvo"` {
				t.Errorf("forwarded worker_id = %s, want the injected identity", m["worker_id"])
			}
		})
	}

	for _, raw := range []string{`[]`, `[1]`, `"x"`, `1`, `true`} {
		fx := &fakeExec{}
		srv := mcpserver.NewWithProfile(fx, "manual:salvo", mcpserver.ProfileUser)
		_, err := srv.CallTool(context.Background(), "create_task", json.RawMessage(raw))
		if err == nil || !strings.Contains(err.Error(), "not a JSON object") {
			t.Errorf("CallTool(create_task, %s) = %v, want a not-a-JSON-object refusal", raw, err)
		}
		if fx.called {
			t.Errorf("non-object args %s reached the executor", raw)
		}
	}
}
