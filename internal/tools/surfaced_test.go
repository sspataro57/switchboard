package tools_test

// task_mark_surfaced's no-database half — SWT-45
// (docs/tickets/jira-activity-revive_SPEC.md) J7 and criterion 15: the tool is
// registered on the executor, validates task_id / message_id / reason before
// any handler runs, and is ABSENT from both MCP profiles.
//
// Driven through executor.Execute with a NIL pool, capturerules_test.go's shape:
// every refusal stops at Validate, so nothing dereferences the pool, and an
// "unknown tool" error is explicitly rejected as NOT a validation failure.
//
// IMPOSED SURFACE (the SPEC's table):
//
//	task_mark_surfaced {task_id, message_id, reason} -> {task_id, surfaced, skipped?}
//	  registered in tools.Register; not humanOnly (capture calls it as
//	  capture:{connector}); no policy rule (static-default); never in
//	  internal/mcpserver/schemas.go or adapter.go's profile slices.
//
// RED TODAY: the tool is not registered. The two MCP-absence checks are GUARDS,
// green today and required to stay green (F7: an argument hidden from a schema
// is still reachable, so the tool's ABSENCE from every profile is the only
// boundary).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

const markSurfaced = "task_mark_surfaced"

func TestRegister_TaskMarkSurfacedIsRegistered(t *testing.T) {
	reg := executor.NewRegistry()
	tools.Register(reg, nil)
	for _, n := range reg.Names() {
		if n == markSurfaced {
			return
		}
	}
	t.Errorf("tool %q is not registered by tools.Register. J7: a creation by an overriding rule is surfaced "+
		"through the executor (validate -> policy -> audit), never by capture writing tasks.surfaced_* "+
		"itself (invariant 3)", markSurfaced)
}

// "validates task_id, message_id and reason." Each refusal names the field.
func TestValidate_TaskMarkSurfaced_RefusesIncompleteArgs(t *testing.T) {
	reg := executor.NewRegistry()
	tools.Register(reg, nil)
	ex := executor.New(reg, policy.NewStatic(reg.Names()...), audit.NewMemStore())
	ctx := context.Background()

	for _, tc := range []struct{ name, args, wantIn string }{
		{"empty object", `{}`, "task_id"},
		{"zero task_id", `{"task_id":0,"message_id":9,"reason":"created by an overriding rule"}`, "task_id"},
		{"missing message_id", `{"task_id":7,"reason":"created by an overriding rule"}`, "message_id"},
		{"zero message_id", `{"task_id":7,"message_id":0,"reason":"created by an overriding rule"}`, "message_id"},
		{"negative message_id", `{"task_id":7,"message_id":-2,"reason":"created by an overriding rule"}`, "message_id"},
		{"missing reason", `{"task_id":7,"message_id":9}`, "reason"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := ex.Execute(ctx, executor.Call{Tool: markSurfaced, Actor: "capture:google", Args: json.RawMessage(tc.args)})
			if err == nil {
				t.Fatalf("%s(%s) succeeded; want a validation failure", markSurfaced, tc.args)
			}
			if strings.Contains(err.Error(), "unknown tool") {
				t.Fatalf("%s is not registered (%v); that is not a validation failure", markSurfaced, err)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("%s(%s) = %q, want a refusal naming %q", markSurfaced, tc.args, err, tc.wantIn)
			}
		})
	}
}

// "absent from BOTH MCP profiles' schemas (structural, the
// TestTaskReopen_StaysOffTheMCPSchemas shape)." F7 is why this matters more than
// usual: the MCP adapter passes arguments through, so if a worker could REACH
// this tool it could mark any task surfaced with any inbound message and the
// reconciler would then hold that task open against a done ticket.
func TestTaskMarkSurfaced_StaysOffBothMCPProfiles(t *testing.T) {
	for _, rel := range []string{"internal/mcpserver/schemas.go", "internal/mcpserver/adapter.go"} {
		b, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		src := string(b)
		// Control: the scan must be looking at files that list tools at all.
		if !strings.Contains(src, `"task_list"`) {
			t.Fatalf("%s does not mention \"task_list\"; this scan is not looking at a tool list", rel)
		}
		if strings.Contains(src, `"`+markSurfaced+`"`) {
			t.Errorf("%s names %q. J7/F7: the tool is spine-facing (capture's creations only); a transport to it "+
				"lets a worker pin any task on the board against the reconciler", rel, markSurfaced)
		}
	}
}
