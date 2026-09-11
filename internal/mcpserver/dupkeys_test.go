package mcpserver_test

// SWT-38 (Codex re-review): the reservation guards decode the caller's original
// key order (last duplicate wins) while the handler decodes the re-marshalled,
// key-SORTED args. Two keys that fold to the same field can therefore pass a
// guard with one value and reach the handler with the other:
// {"parent_id":123,"PARENT_ID":null} passed rejectParentID and created a task
// under a worker's task. CallTool now refuses any args object with two
// fold-equivalent keys before any guard runs. Keys are JSON \u escapes in Go raw
// strings, so this file is plain ASCII.
//
// MUTATION THAT MUST TURN THIS RED: drop the rejectFoldDuplicateKeys call in
// CallTool → the calls are forwarded (fx.called) instead of refused.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/mcpserver"
)

func TestCallTool_RefusesFoldDuplicateKeys(t *testing.T) {
	for _, tc := range []struct {
		name, tool, args string
		profile          mcpserver.Profile
	}{
		{"parent_id case pair", "create_task",
			`{"project":"p","title":"t","parent_id":123,"PARENT_ID":null}`, mcpserver.ProfileUser},
		{"parent_id case pair, full profile", "create_task",
			`{"project":"p","title":"t","PARENT_ID":null,"parent_id":123}`, mcpserver.ProfileFull},
		{"kind Kelvin pair", "task_append_log",
			`{"task_id":1,"message":"m","kind":"log","\u212aind":"session"}`, mcpserver.ProfileFull},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			fx := &fakeExec{}
			srv := mcpserver.NewWithProfile(fx, "manual:salvo", tc.profile)
			_, err := srv.CallTool(context.Background(), tc.tool, json.RawMessage(tc.args))
			if err == nil || !strings.Contains(err.Error(), "ambiguous arguments") {
				t.Errorf("CallTool(%s, %s) = %v, want an ambiguous-arguments refusal", tc.tool, tc.args, err)
			}
			if fx.called {
				t.Errorf("the ambiguous call reached the executor: %s", fx.lastCall.Args)
			}
		})
	}

	// POSITIVE CONTROL: distinct fields still pass.
	fx := &fakeExec{}
	srv := mcpserver.New(fx, "manual:salvo")
	if _, err := srv.CallTool(context.Background(), "task_append_log",
		json.RawMessage(`{"task_id":1,"message":"m","kind":"log"}`)); err != nil {
		t.Errorf("an ordinary call = %v, want forwarded", err)
	}
}
