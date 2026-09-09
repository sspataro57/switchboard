package tools

// create_task's optional `status` argument (SWT-30,
// docs/tickets/classify-promotion_SPEC.md, "API / MCP tool changes"; the
// parameter docs/tickets/06-gpt-triage_SPEC.md reserved for the live slice).
// ZERO network, ZERO Postgres.
//
// WHY THIS FILE IS `package tools` AND THE OTHER TOOL TESTS ARE `package
// tools_test`. Every existing validation test drives Execute with a NIL pool and
// asserts a REFUSAL, which is safe precisely because a validation failure
// returns before any handler dereferences the pool. That shape cannot express
// the other half of this criterion — that `holding` is ACCEPTED — because an
// accepted call runs the handler and panics on the nil pool. So the accept cases
// call validateCreateTask directly, which is also the function the SPEC names
// ("validateCreateTask rejects any other value by name"). The reject cases below
// are equally reachable through Execute; they are kept next to their accept
// twins rather than split across two files for the sake of the package clause.
//
// The HANDLER half — that status='holding' really lands in tasks.status — needs
// a database and lives in internal/promote/store_integration_test.go
// (criterion 8), where the value is read back out of the column.
//
// GREENFIELD NOTE — EXPECTED RED. createTaskArgs has no Status field today, so
// this file compile-FAILS internal/tools. Once the field exists, the accept /
// reject / default assertions fail until validateCreateTask learns the enum.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Q1's answer, in one line: the review lane is a `holding` task in the same
// project, so create_task must accept exactly these two statuses and nothing
// else. `holding` is strictly LESS privileged than `ready` — the status agents
// can already produce — which is why widening the tool does not widen what an
// agent can do.
func TestValidateCreateTask_AcceptsReadyAndHolding(t *testing.T) {
	for _, status := range []string{"ready", "holding"} {
		t.Run(status, func(t *testing.T) {
			args := []byte(`{"project":"personal","title":"itest","status":"` + status + `"}`)
			if err := validateCreateTask(args); err != nil {
				t.Fatalf("validateCreateTask(status=%q) = %v, want nil. Criterion 8: `holding` is what "+
					"makes /tasks?project=personal&status=holding the review lane — a FILTER over the one "+
					"tasks table (invariant 2), not a second queue", status, err)
			}
		})
	}
}

// "validateCreateTask rejects any other value by name." By NAME matters: a
// status that silently fell back to `ready` would put a non-whitelisted flag on
// the live board, which is the exact failure criterion 8 exists to prevent.
func TestValidateCreateTask_RejectsAnyOtherStatusByName(t *testing.T) {
	// Every other value of the tasks.status enum (migrations/0001_initial.sql:
	// 123-126) plus the shapes a caller actually sends by accident: a
	// case-variant, a lane name from another system, and a bare typo.
	for _, status := range []string{
		"claimed", "in_progress", "needs_feedback", "pr_open", "awaiting_ci",
		"awaiting_merge", "done_locally", "delivered", "closed", "blocked",
		"READY", "Holding", "hold", "review", "todo",
	} {
		t.Run(status, func(t *testing.T) {
			args := []byte(`{"project":"personal","title":"itest","status":"` + status + `"}`)
			err := validateCreateTask(args)
			if err == nil {
				t.Fatalf("validateCreateTask(status=%q) = nil; create_task accepts exactly ready|holding. "+
					"Anything else here is either a lifecycle transition that belongs to the orchestrator "+
					"and its tools, or a typo that would silently create the wrong thing", status)
			}
			msg := err.Error()
			if !strings.Contains(msg, status) {
				t.Errorf("validateCreateTask(status=%q) failed with %q, which does not name the offending "+
					"value. assignee_type's refusal is the shape to copy — an error that does not quote "+
					"what it rejected sends the reader to the wrong field", status, msg)
			}
			for _, want := range []string{"ready", "holding"} {
				if !strings.Contains(msg, want) {
					t.Errorf("validateCreateTask(status=%q) failed with %q, which does not name %q as an "+
						"allowed value; the error is the only place the enum is written down for a caller",
						status, msg, want)
				}
			}
		})
	}
}

// Omitted status means `ready` — unchanged behaviour for every existing caller
// (the orchestrator, capture, plan import, the dashboard, every agent).
//
// IMPOSED: the default is applied in parseCreateTask, where AssigneeType's
// default already lives. That keeps "what did this call actually mean" a
// property of the parsed args rather than of one SQL literal in the handler,
// and it is what lets this assertion exist without a database.
func TestParseCreateTask_DefaultsStatusToReady(t *testing.T) {
	a, err := parseCreateTask([]byte(`{"project":"personal","title":"itest"}`))
	if err != nil {
		t.Fatalf("parseCreateTask: %v", err)
	}
	if a.Status != "ready" {
		t.Errorf("parseCreateTask with no status gave Status=%q, want %q. Every caller that predates "+
			"SWT-30 omits the field, and a task created by omission must stay a live one", a.Status, "ready")
	}
	if err := validateCreateTask([]byte(`{"project":"personal","title":"itest"}`)); err != nil {
		t.Errorf("validateCreateTask with no status = %v, want nil: the argument is OPTIONAL", err)
	}
}

// GREEN TODAY, AND MUST STAY GREEN. "The MCP schema in
// internal/mcpserver/schemas.go is left ALONE: agents keep the ready-only
// description, and `holding` is strictly less privileged than the status they
// can already produce, so no new agent capability appears."
//
// A guard rather than a discovery: the natural reflex when adding a tool
// argument is to add it to the schema too, and this ticket's argument for why
// the widening is safe rests on not doing that.
func TestMCPSchema_CreateTaskGainsNoStatusProperty(t *testing.T) {
	b, err := os.ReadFile("../mcpserver/schemas.go")
	if err != nil {
		t.Fatalf("read ../mcpserver/schemas.go: %v", err)
	}
	src := string(b)
	i := strings.Index(src, `Name:        "create_task"`)
	if i < 0 {
		t.Fatalf("create_task is not declared in internal/mcpserver/schemas.go; this guard has nothing to guard")
	}
	block := src[i:]
	// Bound at the NEXT tool's Name: declaration, not at "}," — the schema JSON
	// itself contains "}," after its first property, which cut the block before
	// "title" and failed the control against untouched code (found 2026-09-09).
	if j := strings.Index(block[1:], `Name:        "`); j > 0 {
		block = block[:j+1]
	}
	if strings.Contains(block, `"status"`) {
		t.Errorf("internal/mcpserver/schemas.go now advertises a `status` property on create_task. SWT-30 "+
			"leaves the MCP surface alone on purpose: the promoter is a spine service reaching the "+
			"executor directly, and an agent-visible lane parameter is a new capability nobody asked "+
			"for.\nblock: %s", block)
	}
	// The control: the block really is create_task's schema, or the scan above
	// passed over the wrong text.
	if !strings.Contains(block, `"title"`) {
		t.Errorf("the scanned block does not look like create_task's schema (no \"title\" property): %s", block)
	}
}

// Sanity: the argument is decoded from JSON at all. Without this, a
// `json:"status"` tag typo would make every reject case above pass for the wrong
// reason — the value would never reach the validator.
func TestParseCreateTask_ReadsTheStatusArgument(t *testing.T) {
	a, err := parseCreateTask([]byte(`{"project":"personal","title":"itest","status":"holding"}`))
	if err != nil {
		t.Fatalf("parseCreateTask: %v", err)
	}
	if a.Status != "holding" {
		t.Fatalf("parseCreateTask lost the status argument (got %q). The json tag must be `status` — a "+
			"typo there makes the validator's enum unreachable and every rejection above pass vacuously",
			a.Status)
	}
	// And it round-trips as the executor will send it: the promoter marshals a
	// map, not this struct.
	raw, _ := json.Marshal(map[string]any{"project": "personal", "title": "itest", "status": "holding"})
	if err := validateCreateTask(raw); err != nil {
		t.Fatalf("validateCreateTask on marshalled args = %v, want nil", err)
	}
}
