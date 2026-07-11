package tools_test

// Unit tests for the SWT-4 lifecycle tools (SPEC 04-mcp-task-tools, acceptance
// criteria 2 & 3) EXTENDED with the five SWT-5 spine-facing orchestrator tools
// (SPEC 05-orchestrator-loop), the SWT-7 propose_slots availability tool
// (SPEC 07-google-oauth-pollers, criterion 12), and the SWT-8 delivery tools
// (SPEC 08-draft-deliveries). ZERO network, ZERO Postgres: tools are exercised
// through executor.Execute with a nil *pgxpool.Pool. Every assertion here stops
// BEFORE any handler runs — a validation failure returns from Execute after the
// Validate stage, and an unregistered name returns "unknown tool", neither of
// which dereferences the pool. That keeps the file offline while still proving
// the registration wiring and Validate contract through the real registry (per
// the task: prefer testing through executor.Execute).
//
// GREENFIELD NOTE (SWT-8): the seven delivery tools are not registered yet.
// tools.Register wires only the SWT-4/5/7 seventeen today, so:
//   - TestRegister_AllToolsRegistered fails (the seven missing from reg.Names()).
//   - TestValidate_RejectsMissingRequiredArgs/{delivery tools} fails: Execute
//     returns "unknown tool", which the assertion explicitly rejects as "not a
//     validation failure".
// After implementation the seven are wired via tools.Register with Validate
// funcs that reject empty args {} (each has a required field — see the
// toolsUnderTest note), and both tests pass.
//
// Surface exercised (SWT-4/5/7 shipped; the seven delivery tools are SWT-8):
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
// five SWT-5 spine-facing orchestrator tools, plus SWT-7's propose_slots (17),
// plus SWT-8's seven delivery tools = 24. All seven are wired by
// tools.Register(reg, pool) (SPEC 08 "wire names into ... Register"); only
// draft_delivery is MCP-listed (agent-facing), the other six are spine-facing
// (dashboard + opsctl call).
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
	// SWT-7 availability tool (registered, reachable via opsctl call):
	"propose_slots",
	// SWT-8 delivery tools:
	"draft_delivery",      // agent-facing (MCP-listed)
	"update_delivery",     // spine-facing
	"approve_delivery",    // spine-facing
	"send_delivery",       // spine-facing
	"mark_delivery_sent",  // spine-facing
	"task_mark_delivered", // spine-facing
	"set_sending_frozen",  // spine-facing
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
// tool: each has at least one required field per the SPEC's tool contract. For
// the SWT-8 delivery tools: draft_delivery needs task_id+channel+body;
// update_delivery/approve_delivery/send_delivery/mark_delivery_sent need
// delivery_id; task_mark_delivered needs task_id; set_sending_frozen requires
// an explicit `frozen` bool (freeze/unfreeze must be deliberate — an empty {}
// is rejected so the audited flag is never flipped by omission).
func TestValidate_RejectsMissingRequiredArgs(t *testing.T) {
	reg := executor.NewRegistry()
	tools.Register(reg, nil)
	ex := executor.New(reg, policy.NewStatic(reg.Names()...), audit.NewMemStore())
	ctx := context.Background()

	// create_task is already known to validate; the rest are the SWT-4/5/7/8 tools.
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
		// SWT-7:
		"propose_slots",
		// SWT-8:
		"draft_delivery",
		"update_delivery",
		"approve_delivery",
		"send_delivery",
		"mark_delivery_sent",
		"task_mark_delivered",
		"set_sending_frozen",
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
