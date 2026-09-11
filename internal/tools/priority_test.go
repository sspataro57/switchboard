package tools

// SWT-38 (docs/tickets/mcp-task-capture_SPEC.md) criteria 1, 2 and 3: the
// create_task profile pin and priority range, task_set_priority's validator,
// and the ONE spelling of the 0..3 priority scale. ZERO network, ZERO Postgres.
//
// WHY `package tools`: createtask_status_test.go's reason, unchanged. Driving
// Execute with a nil pool can assert only REFUSALS; half of every criterion
// here is an ACCEPT, which would run the handler and deref the nil pool. So the
// validators are called directly — they are also the functions the SPEC names.
// The registration half (task_set_priority reaches Validate rather than
// returning "unknown tool") lives in tools_unit_test.go's allToolNames and
// toolsUnderTest.
//
// IMPOSED SURFACE (SPEC C4, C5, C7, "API / MCP tool changes"):
//
//	// internal/tools/priority.go (new)
//	const PriorityMin = 0
//	const PriorityMax = 3
//	var PriorityLevels = []string{"normal", "elevated", "high", "urgent"} // index = value
//	type setPriorityArgs struct {
//	    TaskID   int64  `json:"task_id"`
//	    Priority *int   `json:"priority"` // a pointer: a missing priority is NOT 0
//	    Reason   string `json:"reason,omitempty"`
//	}
//	func validateSetPriority(args []byte) error
//	// Register gains {"task_set_priority", validateSetPriority, setPriority}.
//
//	// internal/tools/createtask.go
//	// createTaskArgs gains RequireAssigneeType string `json:"require_assignee_type,omitempty"`.
//	// validateCreateTask: if it is set and differs from the parsed AssigneeType
//	// (default human), return
//	//   fmt.Errorf("assignee_type %q is refused here: this session's tasks are assigned to human (Salvador's lane); worker tasks are created from the switchboard repo", a.AssigneeType)
//	// and a priority outside [PriorityMin, PriorityMax] is refused.
//
//	// internal/tools/appendlog.go
//	// appendLogArgs gains the same RequireAssigneeType field. That one is
//	// HANDLER-enforced (it needs the task's row), so it is covered by
//	// mcp_capture_integration_test.go criterion 20(f), not here.
//
// GREENFIELD NOTE — EXPECTED RED. validateSetPriority, PriorityMin,
// PriorityMax and PriorityLevels do not exist, so this file compile-FAILS
// internal/tools (and with it every test in the package's test binary) until
// priority.go declares them. Once it compiles, the create_task pin and range
// rows fail until validateCreateTask learns them.
//
// MUTATIONS THAT MUST TURN THIS FILE RED:
//   - M-i: validateSetPriority reads a missing priority as 0 (a plain int, or a
//     nil pointer defaulted) → the "missing priority" and "null priority" rows
//     of TestValidateSetPriority_RejectsIncompleteArgs.
//   - validateCreateTask ignores require_assignee_type → the "claude under a
//     human pin" row of TestValidateCreateTask_RequireAssigneeTypePin.
//   - validateCreateTask refuses claude with NO pin (the check applied
//     unconditionally) → the "full profile unchanged" accept row.
//   - an off-by-one in either range (e.g. [0,4] or [1,3]) → the boundary rows of
//     TestValidateCreateTask_PriorityRange / TestValidateSetPriority_RangeIsTheScale.

import (
	"fmt"
	"strings"
	"testing"
)

// ---- criterion 1: create_task's pin -------------------------------------------

// The pin ONLY NARROWS (C4). The user profile always injects
// require_assignee_type:"human"; a full-profile caller that passes it restricts
// itself and nothing more; with no pin, claude stays legal — worker consoles and
// this repo's session keep creating claude tasks (C1, "Full profile unchanged").
func TestValidateCreateTask_RequireAssigneeTypePin(t *testing.T) {
	for _, tc := range []struct{ name, args string }{
		{"pin human, assignee unset (defaults to human)",
			`{"project":"p","title":"t","require_assignee_type":"human"}`},
		{"pin human, assignee human",
			`{"project":"p","title":"t","assignee_type":"human","require_assignee_type":"human"}`},
		// The shape the user-scope adapter actually forwards: worker_id injected
		// into every call, the pin overwritten after it.
		{"pin human with the injected worker_id",
			`{"project":"p","title":"t","require_assignee_type":"human","worker_id":"manual:salvo"}`},
		{"full profile unchanged: no pin, assignee claude",
			`{"project":"p","title":"t","assignee_type":"claude"}`},
		{"no pin, assignee unset", `{"project":"p","title":"t"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateCreateTask([]byte(tc.args)); err != nil {
				t.Errorf("validateCreateTask(%s) = %v, want nil", tc.args, err)
			}
		})
	}

	t.Run("claude under a human pin is refused", func(t *testing.T) {
		args := `{"project":"p","title":"t","assignee_type":"claude","require_assignee_type":"human"}`
		err := validateCreateTask([]byte(args))
		if err == nil {
			t.Fatalf("validateCreateTask(%s) = nil. C1/C4: from the user profile a claude task is REFUSED, not "+
				"silently rewritten — a claude task lands in the console queue for the project's client, where a "+
				"running console would claim it and do the same work in parallel", args)
		}
		msg := err.Error()
		for _, want := range []string{`"claude"`, "human", "switchboard repo"} {
			if !strings.Contains(msg, want) {
				t.Errorf("refusal %q does not contain %q. C4 spells it: assignee_type %%q is refused here: this "+
					"session's tasks are assigned to human (Salvador's lane); worker tasks are created from the "+
					"switchboard repo — the model reads this and must learn where claude tasks come from", msg, want)
			}
		}
	})

	// "If it is set and differs from the parsed AssigneeType (default human)":
	// the default counts. A caller pinning claude without asking for claude
	// narrowed itself to nothing, and is told so.
	t.Run("a pin that differs from the defaulted assignee is refused", func(t *testing.T) {
		args := `{"project":"p","title":"t","require_assignee_type":"claude"}`
		if err := validateCreateTask([]byte(args)); err == nil {
			t.Errorf("validateCreateTask(%s) = nil; the pin is compared against the PARSED assignee "+
				"(default human), so claude ≠ human must refuse", args)
		}
	})
}

// ---- criterion 1: create_task's priority range --------------------------------

// C7: validateCreateTask gains the scale's range check. Only calls that pass
// priority are touched, and every in-process caller passes 0 or nothing (SPEC
// fact 2), so an absent priority must keep passing.
func TestValidateCreateTask_PriorityRange(t *testing.T) {
	for _, p := range []int{-1, 4} {
		p := p
		t.Run(fmt.Sprintf("refuse %d", p), func(t *testing.T) {
			args := fmt.Sprintf(`{"project":"p","title":"t","priority":%d}`, p)
			err := validateCreateTask([]byte(args))
			if err == nil {
				t.Fatalf("validateCreateTask(priority=%d) = nil; the scale is 0..3 (normal, elevated, high, urgent)", p)
			}
			if !strings.Contains(err.Error(), "priority") {
				t.Errorf("validateCreateTask(priority=%d) failed with %q, which does not name the field", p, err)
			}
		})
	}
	for _, p := range []int{0, 1, 2, 3} {
		p := p
		t.Run(fmt.Sprintf("accept %d", p), func(t *testing.T) {
			args := fmt.Sprintf(`{"project":"p","title":"t","priority":%d}`, p)
			if err := validateCreateTask([]byte(args)); err != nil {
				t.Errorf("validateCreateTask(priority=%d) = %v, want nil", p, err)
			}
			// The same through the user profile's shape (criterion 20(c): "swb add"
			// with priority 3 creates an urgent HUMAN task).
			pinned := fmt.Sprintf(`{"project":"p","title":"t","priority":%d,"require_assignee_type":"human"}`, p)
			if err := validateCreateTask([]byte(pinned)); err != nil {
				t.Errorf("validateCreateTask(priority=%d, pinned human) = %v, want nil", p, err)
			}
		})
	}
	t.Run("absent", func(t *testing.T) {
		if err := validateCreateTask([]byte(`{"project":"p","title":"t"}`)); err != nil {
			t.Errorf("validateCreateTask with no priority = %v, want nil: the argument is OPTIONAL", err)
		}
	})
}

// ---- criterion 2: validateSetPriority -----------------------------------------

func TestValidateSetPriority_RejectsIncompleteArgs(t *testing.T) {
	for _, tc := range []struct{ name, args, wantIn string }{
		{"empty object", `{}`, "task_id"},
		{"missing task_id", `{"priority":2}`, "task_id"},
		{"zero task_id", `{"task_id":0,"priority":2}`, "task_id"},
		// M-i. A missing priority must NOT be read as 0: "swb prioritize 412"
		// with a dropped argument would otherwise silently DEMOTE 412 to normal —
		// the opposite of what was asked (no level means urgent, C7).
		{"missing priority", `{"task_id":412}`, "priority"},
		{"null priority", `{"task_id":412,"priority":null}`, "priority"},
		{"missing priority with a reason", `{"task_id":412,"reason":"do this first"}`, "priority"},
		{"not an object", `[]`, ""},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := validateSetPriority([]byte(tc.args))
			if err == nil {
				t.Fatalf("validateSetPriority(%s) = nil, want a refusal (C5: task_id and priority are REQUIRED)", tc.args)
			}
			if tc.wantIn != "" && !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("validateSetPriority(%s) failed with %q, which does not name %q", tc.args, err, tc.wantIn)
			}
		})
	}
}

func TestValidateSetPriority_RangeIsTheScale(t *testing.T) {
	for _, p := range []int{-1, 4, 100} {
		p := p
		t.Run(fmt.Sprintf("refuse %d", p), func(t *testing.T) {
			err := validateSetPriority([]byte(fmt.Sprintf(`{"task_id":412,"priority":%d}`, p)))
			if err == nil {
				t.Fatalf("validateSetPriority(priority=%d) = nil; the scale is [PriorityMin, PriorityMax] = [0, 3]", p)
			}
			if !strings.Contains(err.Error(), "priority") {
				t.Errorf("validateSetPriority(priority=%d) failed with %q, which does not name the field", p, err)
			}
		})
	}
	for _, p := range []int{0, 1, 2, 3} {
		p := p
		t.Run(fmt.Sprintf("accept %d", p), func(t *testing.T) {
			for _, args := range []string{
				fmt.Sprintf(`{"task_id":412,"priority":%d}`, p), // reason is OPTIONAL (C5)
				fmt.Sprintf(`{"task_id":412,"priority":%d,"reason":"Salvador: do this first"}`, p),
				// What the adapter forwards: worker_id injected into every call.
				fmt.Sprintf(`{"task_id":412,"priority":%d,"worker_id":"manual:salvo"}`, p),
			} {
				if err := validateSetPriority([]byte(args)); err != nil {
					t.Errorf("validateSetPriority(%s) = %v, want nil", args, err)
				}
			}
		})
	}
	// The consts, not only the literals: the validator's bounds are THE scale.
	if err := validateSetPriority([]byte(fmt.Sprintf(`{"task_id":1,"priority":%d}`, PriorityMax+1))); err == nil {
		t.Errorf("validateSetPriority accepts PriorityMax+1 = %d", PriorityMax+1)
	}
	if err := validateSetPriority([]byte(fmt.Sprintf(`{"task_id":1,"priority":%d}`, PriorityMin-1))); err == nil {
		t.Errorf("validateSetPriority accepts PriorityMin-1 = %d", PriorityMin-1)
	}
}

// ---- criterion 3: the one spelling of the scale -------------------------------

// C7: "0 normal, 1 elevated, 2 high, 3 urgent" — triage's words
// (internal/triage/prompt.go), the only scale the repo already speaks. Higher
// runs first (taskQueueOrder). The MCP schema's minimum/maximum and its
// description's level names are asserted equal to these in
// internal/mcpserver/task_capture_test.go (criterion 12).
func TestPriorityLevels_OneSpellingOfTheScale(t *testing.T) {
	if PriorityMin != 0 || PriorityMax != 3 {
		t.Errorf("[PriorityMin, PriorityMax] = [%d, %d], want [0, 3] (C7)", PriorityMin, PriorityMax)
	}
	if len(PriorityLevels) != PriorityMax-PriorityMin+1 {
		t.Fatalf("PriorityLevels has %d entries, want PriorityMax-PriorityMin+1 = %d (index = value)",
			len(PriorityLevels), PriorityMax-PriorityMin+1)
	}
	want := []string{"normal", "elevated", "high", "urgent"}
	for i, w := range want {
		if PriorityLevels[i] != w {
			t.Errorf("PriorityLevels[%d] = %q, want %q", i, PriorityLevels[i], w)
		}
	}
}
