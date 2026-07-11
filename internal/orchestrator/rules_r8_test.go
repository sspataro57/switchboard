package orchestrator_test

// R8 pure-rule tests (SPEC 08-draft-deliveries, acceptance criterion 10). Like
// the rest of rules_test.go this exercises orchestrator.Evaluate over hand-built
// Events/Facts with ZERO I/O (invariant 7) and reuses that file's assertion
// helpers (same _test package): executesOf, exactlyOneExecute, argInt, argStr,
// subMap, dump, fbPayload.
//
// GREENFIELD NOTE: R8 is not in Evaluate yet, so these cases fail (the delivery
// task is not closed, the work task is not marked delivered) until rules.go adds
// the `delivery_sent` branch. No new exported surface is imposed: R8 reads the
// existing Orchestration.CreatedTaskID (the R3 `delivery_task` record already
// carries the created Deliver task id) and emits the existing tool vocabulary
// plus task_mark_delivered (a SWT-8 tool).
//
// Contract (criterion 10): on task_event `delivery_sent`, where the event's task
// is the delivered WORK task N (SPEC criterion 5 fires delivery_sent on the
// delivery's task = the work task), Evaluate emits, in order:
//   - task_mark_delivered(task_id=N)                      (done_locally -> delivered)
//   - task_close(task_id=deliverTask)  — the R3 Deliver task, found via the
//     `delivery_task` orchestration record's CreatedTaskID
//   - record_orchestration(task_id=N, rule="delivery_lifecycle")  (dedup key)
// Dedup on task: a prior `delivery_lifecycle` record => no-op. A `delivery_confirmed`
// event fires no rule.

import (
	"testing"

	orch "github.com/sspataro57/switchboard/internal/orchestrator"
)

func TestEvaluate_R8_DeliverySent(t *testing.T) {
	const (
		eventID     = int64(800)
		workTask    = int64(9)  // the delivered work task (delivery.task_id)
		deliverTask = int64(55) // the R3 Deliver task created by delivery_task
		deliveryID  = int64(700)
	)
	mkEvent := func() orch.Event {
		return orch.Event{
			ID:      eventID,
			TaskID:  workTask,
			Type:    "delivery_sent",
			Payload: fbPayload("delivery_id", float64(deliveryID), "channel", "gmail"),
		}
	}
	// The R3 delivery_task record ties the work task N to its Deliver task via
	// CreatedTaskID (facts.go surfaces it; the struct already carries it).
	priorR3 := orch.Orchestration{Rule: "delivery_task", TaskID: workTask, CreatedTaskID: deliverTask}

	t.Run("marks work task delivered + closes the Deliver task + records", func(t *testing.T) {
		f := orch.Facts{
			Task:           orch.TaskFacts{ID: workTask, ProjectSlug: "acme", ProjectDelivery: "dashboard"},
			Orchestrations: []orch.Orchestration{priorR3},
		}
		actions := orch.Evaluate(mkEvent(), f, orch.Config{})

		markA := exactlyOneExecute(t, actions, "task_mark_delivered")
		if got := argInt(t, markA.Args, "task_id"); got != workTask {
			t.Errorf("task_mark_delivered task_id = %d, want the work task %d", got, workTask)
		}

		closeA := exactlyOneExecute(t, actions, "task_close")
		if got := argInt(t, closeA.Args, "task_id"); got != deliverTask {
			t.Errorf("task_close task_id = %d, want the Deliver task %d", got, deliverTask)
		}
		if r := argStr(t, closeA.Args, "reason"); r == "" {
			t.Error("task_close reason must be non-empty")
		}

		rec := exactlyOneExecute(t, actions, "record_orchestration")
		if got := argInt(t, rec.Args, "task_id"); got != workTask {
			t.Errorf("record_orchestration task_id = %d, want %d", got, workTask)
		}
		if got := argStr(t, rec.Args, "rule"); got != "delivery_lifecycle" {
			t.Errorf("record rule = %q, want delivery_lifecycle", got)
		}
		if got := argInt(t, rec.Args, "trigger_event_id"); got != eventID {
			t.Errorf("record trigger_event_id = %d, want %d", got, eventID)
		}
	})

	t.Run("no delivery_task record (manual dashboard draft): mark + record, no close", func(t *testing.T) {
		f := orch.Facts{
			Task:           orch.TaskFacts{ID: workTask, ProjectSlug: "acme", ProjectDelivery: "dashboard"},
			Orchestrations: nil, // delivery created outside R3 -> nothing to close
		}
		actions := orch.Evaluate(mkEvent(), f, orch.Config{})
		if n := len(executesOf(actions, "task_mark_delivered")); n != 1 {
			t.Fatalf("want the work task marked delivered even without an R3 record; got %d", n)
		}
		if n := len(executesOf(actions, "task_close")); n != 0 {
			t.Errorf("no Deliver task to close; task_close must not fire: %s", dump(actions))
		}
		if n := len(executesOf(actions, "record_orchestration")); n != 1 {
			t.Errorf("delivery_lifecycle must still be recorded; got %d", n)
		}
	})

	t.Run("dedup: delivery_lifecycle already recorded on N -> no actions", func(t *testing.T) {
		f := orch.Facts{
			Task: orch.TaskFacts{ID: workTask, ProjectSlug: "acme", ProjectDelivery: "dashboard"},
			Orchestrations: []orch.Orchestration{
				priorR3,
				{Rule: "delivery_lifecycle", TaskID: workTask},
			},
		}
		if actions := orch.Evaluate(mkEvent(), f, orch.Config{}); len(actions) != 0 {
			t.Fatalf("R8 must dedup on delivery_lifecycle for N; got %s", dump(actions))
		}
	})
}

// delivery_confirmed is the loop-closure event (connector-side). It is a
// bookkeeping stamp, not an orchestration trigger — no rule fires.
func TestEvaluate_R8_DeliveryConfirmedFiresNothing(t *testing.T) {
	const workTask = int64(9)
	ev := orch.Event{ID: 801, TaskID: workTask, Type: "delivery_confirmed", Payload: fbPayload("delivery_id", float64(700))}
	f := orch.Facts{
		Task:           orch.TaskFacts{ID: workTask, ProjectSlug: "acme", ProjectDelivery: "dashboard"},
		Orchestrations: []orch.Orchestration{{Rule: "delivery_task", TaskID: workTask, CreatedTaskID: 55}},
	}
	if actions := orch.Evaluate(ev, f, orch.Config{}); len(actions) != 0 {
		t.Fatalf("delivery_confirmed must fire no rule; got %s", dump(actions))
	}
}
