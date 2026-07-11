package tools_test

// Unit tests for the SWT-4 lifecycle tools (SPEC 04-mcp-task-tools, acceptance
// criteria 2 & 3) EXTENDED with the five SWT-5 spine-facing orchestrator tools
// (SPEC 05-orchestrator-loop). ZERO network, ZERO Postgres: tools are exercised
// through executor.Execute with a nil *pgxpool.Pool. Every assertion here stops
// BEFORE any handler runs — a validation failure returns from Execute after the
// Validate stage, and an unregistered name returns "unknown tool", neither of
// which dereferences the pool. That keeps the file offline while still proving
// the registration wiring and Validate contract through the real registry
// (per the task: prefer testing through executor.Execute).
//
// GREENFIELD NOTE: the five new spine tools (task_add_dependency, task_block,
// task_unblock, task_close, record_orchestration) are not registered yet.
// tools.Register wires only the SWT-4 eleven today, so:
//   - TestRegister_AllToolsRegistered fails (missing names in reg.Names()).
//   - TestValidate_RejectsMissingRequiredArgs fails: Execute returns
//     "unknown tool" for the not-yet-registered names, and the assertion
//     explicitly rejects that error text as "not a validation failure".
// After implementation, each tool is registered with a Validate that rejects
// missing required args (every one has at least one required field), and both
// tests pass.
//
// Surface exercised (all already shipped in step 1):
//   func tools.Register(*executor.Registry, *pgxpool.Pool)
//   executor.NewRegistry / (*Registry).Names / (*Registry).Register
//   executor.New / (*Executor).Execute / executor.Call
//   policy.NewStatic / audit.NewMemStore

import (
	"context"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

// allToolNames is the full registry the SPEC pins: create_task (shipped) plus
// the eight agent-facing and two spine-facing SWT-4 lifecycle tools, plus the
// five SWT-5 spine-facing orchestrator tools = 16. The five orchestrator tools
// are registered on the executor but NOT MCP-listed (SPEC 05 §"API / MCP tool
// changes": absent from mcpserver.agentTools — agents must not gate their own
// dependencies or close tasks).
var allToolNames = []string{
	"create_task",
	"task_get_next",
	"task_claim",
	"task_context",
	"task_append_log",
	"request_feedback",
	"mark_done_local",
	"create_child_task",
	"record_decision",
	"task_release",    // spine-facing (registered, not MCP-listed)
	"answer_feedback", // spine-facing
	// SWT-5 spine-facing orchestrator tools (registered, not MCP-listed):
	"task_add_dependency",
	"task_block",
	"task_unblock",
	"task_close",
	"record_orchestration",
}

func TestRegister_AllToolsRegistered(t *testing.T) {
	reg := executor.NewRegistry()
	tools.Register(reg, nil) // nil pool: Register only builds closures, never derefs.

	got := map[string]bool{}
	for _, n := range reg.Names() {
		got[n] = true
	}
	for _, want := range allToolNames {
		if !got[want] {
			t.Errorf("tool %q not registered by tools.Register (registry: %v)", want, reg.Names())
		}
	}
}

// TestValidate_RejectsMissingRequiredArgs drives each tool through Execute with
// empty args {}. The executor runs Validate before the handler, so a missing
// required field must surface as a validation error — NOT "unknown tool"
// (unregistered) and NOT a policy denial. Empty args are illegal for every
// tool: each has at least one required field per the SPEC's tool contract
// (SWT-5's five: task_add_dependency needs task_id+depends_on_task_id;
// task_block/task_unblock need task_id; task_close needs task_id+reason;
// record_orchestration needs task_id+rule).
func TestValidate_RejectsMissingRequiredArgs(t *testing.T) {
	reg := executor.NewRegistry()
	tools.Register(reg, nil)
	ex := executor.New(reg, policy.NewStatic(reg.Names()...), audit.NewMemStore())
	ctx := context.Background()

	// create_task is already known to validate; the rest are the SWT-4 + SWT-5 tools.
	toolsUnderTest := []string{
		"create_task",
		"task_get_next",
		"task_claim",
		"task_context",
		"task_append_log",
		"request_feedback",
		"mark_done_local",
		"create_child_task",
		"record_decision",
		"task_release",
		"answer_feedback",
		// SWT-5:
		"task_add_dependency",
		"task_block",
		"task_unblock",
		"task_close",
		"record_orchestration",
	}

	for _, name := range toolsUnderTest {
		name := name
		t.Run(name, func(t *testing.T) {
			_, err := ex.Execute(ctx, executor.Call{
				Tool:  name,
				Actor: "unit",
				Args:  []byte(`{}`),
			})
			if err == nil {
				t.Fatalf("Execute(%s, {}) = nil error, want a validation failure", name)
			}
			msg := err.Error()
			if strings.Contains(msg, "unknown tool") {
				t.Fatalf("Execute(%s, {}) returned %q: tool is not registered (Validate never ran)", name, msg)
			}
			if strings.Contains(msg, "denied by policy") {
				t.Fatalf("Execute(%s, {}) returned a policy denial %q, want a validation failure "+
					"(policy allows every registered internal tool)", name, msg)
			}
			// A real validation failure is wrapped as "validate <tool> args: ...".
			if !strings.Contains(msg, "validate") {
				t.Errorf("Execute(%s, {}) error = %q, want a validate-stage error", name, msg)
			}
		})
	}
}
