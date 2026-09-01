package tools_test

// SWT-20 acceptance criteria 2 and 3 (the half that needs no database):
// `task_set_source_thread` is a SPINE tool — registered on the executor, absent
// from the MCP schema list — and it refuses malformed arguments at VALIDATE.
//
// ZERO network, ZERO Postgres: every assertion drives executor.Execute with a
// NIL *pgxpool.Pool and stops before any handler dereferences it — the
// tools_unit_test.go / capturerules_test.go idiom.
//
// WHY THE TOOL IS NOT MCP-LISTED, stated here because a reviewer will otherwise
// reach for the IK entry on actor prefixes and conclude the opposite: this tool
// writes the fact that DECIDES where a delivery may be aimed. If an agent could
// call it, the whole SWT-20 binding collapses to "the caller names its own
// target" — which is precisely the exposure the pass-four closure
// (delivery.go:279-281) was written for, and precisely why `external_refs` was
// rejected as the provenance store (SPEC D1: `link_external_ref` IS agent-facing
// and its external_key is free text). The protection is structural — no agent
// has a TRANSPORT to the tool — not an actor-prefix check, which the IK entry
// correctly calls a transport label rather than a trust boundary. Same shape as
// `capture_rule_add` / `capture_rule_set_enabled` (SPEC "Sibling patterns").
//
// GREENFIELD NOTE: red today. `tools.Register` wires no `task_set_source_thread`,
// so Execute returns "unknown tool" — which every assertion below explicitly
// rejects as NOT a validation failure — and the structural scan finds the name
// in neither file.
//
// IMPOSED CONTRACT (SPEC "API / MCP tool changes"):
//
//	request  {"task_id": N, "thread_id": M}
//	response {"task_id": N, "thread_id": M}
//
// Both fields required; both must be non-zero. Policy: static fallthrough — NOT
// humanOnly, because the capture engine (`capture:{connector}`) is its main
// caller and a human-only gate would make the funnel's own observer illegal.

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

const setSourceThreadTool = "task_set_source_thread"

func provenanceExecutor(t *testing.T) *executor.Executor {
	t.Helper()
	reg := executor.NewRegistry()
	tools.Register(reg, nil) // nil pool: Register only builds closures.
	return executor.New(reg, policy.NewStatic(reg.Names()...), audit.NewMemStore())
}

// Criterion 2, first half: registered on the executor, so the write runs the
// full path — registry lookup → validate → policy → audit start → handler →
// audit complete (invariant 3). `tasks.source_thread_id` has exactly one writer
// and "who recorded this task's conversation, and when" is answerable from
// audit_events.
func TestRegister_TaskSetSourceThreadRegistered(t *testing.T) {
	reg := executor.NewRegistry()
	tools.Register(reg, nil)
	for _, n := range reg.Names() {
		if n == setSourceThreadTool {
			return
		}
	}
	t.Errorf("tool %q is not registered by tools.Register (registry: %v). Provenance is written through the "+
		"executor or not at all: a direct UPDATE from the capture engine would also break "+
		"internal/capture/rules_structure_test.go's ban on touching tasks (criterion 20)",
		setSourceThreadTool, reg.Names())
}

// Criterion 3's argument contract, at validate: neither field may be omitted,
// and neither may be zero.
//
// Zero is spelled out rather than left to the FK because `{"task_id":0}` is what
// a caller that forgot to unmarshal a result sends, and `tasks.id` starts at 1 —
// so the FK would reject it anyway, but only after a policy check and two audit
// rows, with an error about a foreign key rather than about the argument.
func TestValidate_TaskSetSourceThread_RejectsIncompleteArgs(t *testing.T) {
	ex := provenanceExecutor(t)

	cases := []struct{ name, args, want string }{
		{"empty object", `{}`, "task_id"},
		{"no thread_id", `{"task_id":42}`, "thread_id"},
		{"no task_id", `{"thread_id":91}`, "task_id"},
		{"zero task_id", `{"task_id":0,"thread_id":91}`, "task_id"},
		{"zero thread_id", `{"task_id":42,"thread_id":0}`, "thread_id"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := ex.Execute(context.Background(), executor.Call{
				Tool: setSourceThreadTool, Actor: "capture:itest", Args: json.RawMessage(tc.args),
			})
			if err == nil {
				t.Fatalf("Execute(%s, %s) = nil error, want a validation failure", setSourceThreadTool, tc.args)
			}
			msg := err.Error()
			if strings.Contains(msg, "unknown tool") {
				t.Fatalf("%s is not registered: %q", setSourceThreadTool, msg)
			}
			if strings.Contains(msg, "denied by policy") {
				t.Fatalf("got a policy denial %q; a missing argument is not a permissions question, and the "+
					"tool is deliberately NOT humanOnly — the capture engine is its main caller", msg)
			}
			if !strings.Contains(msg, "validate") {
				t.Errorf("error = %q, want a validate-stage failure (nothing may reach the handler with a "+
					"half-specified provenance write)", msg)
			}
			if !strings.Contains(msg, tc.want) {
				t.Errorf("error = %q, want it to name the offending field %q", msg, tc.want)
			}
		})
	}
}

// Criterion 2, second half, mechanically: the name is in the executor's
// registration table and NOT in the MCP schema list.
//
// A source scan rather than a call, because the absence is the point and an
// absence is only provable against the file that would have to contain it. The
// positive controls below are what stop this from passing because the scan
// broke: an agent-facing tool must be found in schemas.go, and a known spine
// tool must be found in createtask.go.
func TestTaskSetSourceThread_IsRegisteredButNotMCPListed(t *testing.T) {
	registry := mustReadToolsFile(t, "createtask.go")
	schemas := mustReadToolsFile(t, filepath.Join("..", "mcpserver", "schemas.go"))

	// Positive controls first. Without these the two assertions below pass on an
	// empty string, which is how every source-scanning test dies quietly.
	if !strings.Contains(schemas, `"draft_delivery"`) {
		t.Fatalf("internal/mcpserver/schemas.go does not mention draft_delivery; this scan has stopped " +
			"looking at the agent-facing tool list and can no longer prove an absence")
	}
	if !strings.Contains(registry, `"capture_rule_add"`) {
		t.Fatalf("internal/tools/createtask.go does not mention capture_rule_add; this scan has stopped " +
			"looking at the executor's registration table")
	}

	if !strings.Contains(registry, `"`+setSourceThreadTool+`"`) {
		t.Errorf("internal/tools/createtask.go does not register %q. SPEC criterion 2 pins the registration "+
			"to that table so the tool runs the executor path", setSourceThreadTool)
	}
	if strings.Contains(schemas, setSourceThreadTool) {
		t.Errorf("internal/mcpserver/schemas.go LISTS %q. It must not: an agent that can write a task's source "+
			"thread can aim that task's future deliveries at a conversation of its choosing, which is exactly "+
			"the exposure the upwork_chat closure was written for. The protection is the schema list, not an "+
			"actor-prefix check (IK: an actor prefix is a transport label, not a trust boundary)",
			setSourceThreadTool)
	}
}

func mustReadToolsFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}
