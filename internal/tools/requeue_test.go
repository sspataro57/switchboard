package tools_test

// task_requeue's no-database half — activity-resurfaces (SWT-72,
// docs/tickets/activity-resurfaces_SPEC.md) D6 and criteria 19 and 20's
// structural half: the third review verb is registered on the executor,
// validates task_id and the priority RANGE before any handler runs, and writes
// the priority through the helper it now SHARES with task_set_priority.
//
// Driven through executor.Execute with a NIL pool (surfaced_test.go's shape):
// every refusal stops at Validate, so nothing dereferences the pool, and an
// "unknown tool" error is explicitly rejected as NOT a validation failure.
//
// IMPOSED SURFACE (the SPEC's "API / MCP tool changes" table — greenfield, so
// the SPEC's contract defines the signature):
//
//	task_requeue {task_id, priority?, note?}
//	  -> {task_id, status, reviewed, priority:{from,to,changed}}
//	  registered in tools.Register with validateRequeue / requeueTask,
//	  implemented in internal/tools/requeue.go; humanOnly in policy;
//	  listed in BOTH MCP profiles (agentTools + userProfileTools).
//	  priority is a POINTER: omitted = unchanged (task_set_priority's rule —
//	  a dropped argument must never demote a task), 0..3 when given.
//	// internal/tools/priority.go
//	func applyPriority(ctx, tx, taskID int64, to int, reason string) (from int, changed bool, err error)
//	  // factored out of setPriority; ONE spelling of the priority write
//	  // (priority + updated_at + the priority_changed {from,to,reason} event).
//
// GREENFIELD NOTE, EXPECTED RED: the tool is not registered and requeue.go /
// applyPriority do not exist, so every test here fails on its own assertion.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - task_requeue defaults a missing priority to 0 -> OmittedPriorityIsAccepted
//     (a non-pointer field cannot express "omitted"; the integration half proves
//     the effect).
//   - drop checkPriorityRange -> RefusesAPriorityOutsideTheScale.
//   - requeue.go spells the priority UPDATE itself -> SharesThePriorityWrite.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

const requeueTool = "task_requeue"

func requeueExecutor() *executor.Executor {
	reg := executor.NewRegistry()
	tools.Register(reg, nil)
	// The STATIC allow-list, not the matrix: this file tests validation, and
	// the humanOnly gate is internal/policy's matrix_requeue_test.go.
	return executor.New(reg, policy.NewStatic(reg.Names()...), audit.NewMemStore())
}

// Criterion 19: registered like every other tool.
func TestRegister_TaskRequeueIsRegistered(t *testing.T) {
	reg := executor.NewRegistry()
	tools.Register(reg, nil)
	for _, n := range reg.Names() {
		if n == requeueTool {
			return
		}
	}
	t.Errorf("tool %q is not registered by tools.Register. D6: Requeue is the third review verb beside Dismiss "+
		"and Done, and every board verb is one executor call (invariant 3)", requeueTool)
}

// Criterion 19: task_id <= 0 is refused by name.
func TestValidate_TaskRequeue_RefusesABadTaskID(t *testing.T) {
	ex := requeueExecutor()
	ctx := context.Background()
	for _, args := range []string{`{}`, `{"task_id":0}`, `{"task_id":-4,"priority":1}`} {
		_, err := ex.Execute(ctx, executor.Call{Tool: requeueTool, Actor: "dashboard:salvo", Args: json.RawMessage(args)})
		if err == nil {
			t.Fatalf("%s(%s) succeeded; want a validation failure naming task_id", requeueTool, args)
		}
		if strings.Contains(err.Error(), "unknown tool") {
			t.Fatalf("%s is not registered (%v); that is not a validation failure", requeueTool, err)
		}
		if !strings.Contains(err.Error(), "task_id") {
			t.Errorf("%s(%s) = %q, want a refusal naming task_id", requeueTool, args, err)
		}
	}
}

// Criterion 19: a priority outside 0..3 is refused by the EXISTING
// checkPriorityRange, whose message names the scale. D6: "Nothing is below 0,
// so 'low priority' IS 0 and the verb does not invent −1."
func TestValidate_TaskRequeue_RefusesAPriorityOutsideTheScale(t *testing.T) {
	ex := requeueExecutor()
	ctx := context.Background()
	for _, p := range []int{-1, -100, tools.PriorityMax + 1, 99} {
		args, _ := json.Marshal(map[string]any{"task_id": 41, "priority": p})
		_, err := ex.Execute(ctx, executor.Call{Tool: requeueTool, Actor: "dashboard:salvo", Args: args})
		if err == nil {
			t.Errorf("%s(priority=%d) succeeded; want a refusal: priority is %d..%d and there is no value below %d "+
				"(D6 — \"low priority\" is 0, never −1)", requeueTool, p, tools.PriorityMin, tools.PriorityMax, tools.PriorityMin)
			continue
		}
		if strings.Contains(err.Error(), "unknown tool") {
			t.Fatalf("%s is not registered (%v)", requeueTool, err)
		}
		if !strings.Contains(err.Error(), "priority") {
			t.Errorf("%s(priority=%d) = %q, want a refusal naming priority (the existing checkPriorityRange)",
				requeueTool, p, err)
		}
	}
}

// Criterion 19 / D6: priority is OPTIONAL and every in-range value passes
// validation. A missing priority must reach the HANDLER (where it means
// "unchanged"), never be refused and never be read as 0.
//
// The handler is reached with a nil pool, so it panics or errors on the
// connection; either answer proves the ARGUMENTS were accepted. Only a
// "validate task_requeue args" failure is a refusal.
func TestValidate_TaskRequeue_OmittedPriorityIsAccepted(t *testing.T) {
	ex := requeueExecutor()
	for _, args := range []string{
		`{"task_id":41}`,
		`{"task_id":41,"note":"not for me today"}`,
		`{"task_id":41,"priority":null}`,
		`{"task_id":41,"priority":0}`,
		`{"task_id":41,"priority":3,"note":"urgent after all"}`,
	} {
		args := args
		t.Run(args, func(t *testing.T) {
			var err error
			func() {
				defer func() { _ = recover() }() // a nil pool panics: that is past validation
				_, err = ex.Execute(context.Background(),
					executor.Call{Tool: requeueTool, Actor: "dashboard:salvo", Args: json.RawMessage(args)})
			}()
			if err == nil {
				return // panicked or somehow succeeded: either way, not a refusal
			}
			if strings.Contains(err.Error(), "unknown tool") {
				t.Fatalf("%s is not registered (%v)", requeueTool, err)
			}
			if strings.Contains(err.Error(), "validate "+requeueTool) {
				t.Errorf("%s(%s) was refused by the VALIDATOR: %v. D6: priority and note are optional, and an "+
					"omitted priority means unchanged (task_set_priority's pointer rule — a dropped argument "+
					"must never demote a task)", requeueTool, args, err)
			}
		})
	}
}

// Criterion 20: the priority write has ONE spelling. applyPriority is factored
// out of task_set_priority (whose behaviour and tests stay byte-unchanged) and
// task_requeue calls it — never a second UPDATE of tasks.priority.
func TestRequeue_SharesThePriorityWrite(t *testing.T) {
	prio, err := os.ReadFile("priority.go")
	if err != nil {
		t.Fatalf("read internal/tools/priority.go: %v", err)
	}
	req, err := os.ReadFile("requeue.go")
	if err != nil {
		t.Fatalf("read internal/tools/requeue.go: %v — criterion 20: task_requeue lives in its own file", err)
	}
	if !strings.Contains(string(prio), "func applyPriority(") {
		t.Errorf("priority.go declares no func applyPriority. Criterion 20: the priority write (priority, " +
			"updated_at and the priority_changed {from,to,reason} event) is factored out of setPriority and " +
			"SHARED, so the two verbs cannot drift")
	}
	if !strings.Contains(string(prio), "applyPriority(") || strings.Count(string(prio), "applyPriority(") < 2 {
		t.Errorf("priority.go does not CALL applyPriority; setPriority must go through the shared helper too " +
			"(criterion 20: one spelling, and task_set_priority's behaviour stays byte-unchanged)")
	}
	if !strings.Contains(string(req), "applyPriority(") {
		t.Errorf("requeue.go does not call applyPriority; a second UPDATE of tasks.priority is how the two verbs " +
			"drift (criterion 20)")
	}
	if strings.Contains(string(req), "priority_changed") {
		t.Errorf("requeue.go spells the priority_changed event itself; it belongs to applyPriority (criterion 20)")
	}
	if !strings.Contains(string(req), "reviewed") {
		t.Errorf("requeue.go never mentions `reviewed`; D6 step 5: one task_events row, type reviewed")
	}
}
