package mcpserver_test

// SWT-38 (go-reviewer F1): encoding/json matches struct fields
// case-insensitively with Unicode folding. A model-supplied key that FOLDS to a
// pinned or injected one decodes as the real field and, sorting after the exact
// key when the adapter re-marshals the args, wins:
//   - "require_a\u017fsignee_type" (U+017F LATIN SMALL LETTER LONG S folds to s)
//   - "wor\u212aer_id" (U+212A KELVIN SIGN folds to k)
// These tests send ONE such variant (two or more are refused outright as
// ambiguous, see dupkeys_test.go): the adapter must strip it when it sets the
// pinned or injected value, so the decoder sees only the server's value. Keys
// are JSON \u escapes in Go raw strings, so this file is plain ASCII.
//
// MUTATION THAT MUST TURN THIS RED: drop the EqualFold delete loop in
// overwriteArgs → both tests fail (the pin reads "claude", worker_id reads
// "spoof"), because both variants sort after the exact key.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/mcpserver"
)

func TestPins_FoldEquivalentKeyCannotOverrideThePin(t *testing.T) {
	fx := &fakeExec{}
	srv := mcpserver.NewWithProfile(fx, "manual:salvo", mcpserver.ProfileUser)
	args := `{"project":"p","title":"t","assignee_type":"claude","require_a\u017fsignee_type":"claude"}`
	if _, err := srv.CallTool(context.Background(), "create_task", json.RawMessage(args)); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	// Decode exactly as the handler does: into a struct with the real tag.
	var got struct {
		Require string `json:"require_assignee_type"`
	}
	if err := json.Unmarshal(fx.lastCall.Args, &got); err != nil {
		t.Fatalf("forwarded args: %v", err)
	}
	if got.Require != "human" {
		t.Errorf("the handler would read require_assignee_type=%q, want the pinned \"human\" — a fold-equivalent "+
			"key overrode the pin. Forwarded: %s", got.Require, fx.lastCall.Args)
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(fx.lastCall.Args, &m)
	for k := range m {
		if k != "require_assignee_type" && strings.EqualFold(k, "require_assignee_type") {
			t.Errorf("forwarded args still carry the fold-equivalent key %q", k)
		}
	}
}

func TestWorkerID_FoldEquivalentKeyCannotSpoofIdentity(t *testing.T) {
	fx := &fakeExec{}
	srv := mcpserver.New(fx, "acme")
	if _, err := srv.CallTool(context.Background(), "task_get_next",
		json.RawMessage(`{"client":"acme","wor\u212aer_id":"spoof"}`)); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	var got struct {
		WorkerID string `json:"worker_id"`
	}
	if err := json.Unmarshal(fx.lastCall.Args, &got); err != nil {
		t.Fatalf("forwarded args: %v", err)
	}
	if got.WorkerID != "acme" {
		t.Errorf("a handler would read worker_id=%q, want the injected \"acme\" (identity is never model-chosen). "+
			"Forwarded: %s", got.WorkerID, fx.lastCall.Args)
	}
}
