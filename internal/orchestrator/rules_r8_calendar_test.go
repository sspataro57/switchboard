package orchestrator_test

// R8's calendar skip (SWT-28 / docs/tickets/calendar-booking_SPEC.md,
// acceptance criterion 29; Q2 = (b) in
// docs/tickets/calendar-booking_OPEN_QUESTIONS.md). Same voice and same shape
// as rules_r8_test.go: orchestrator.Evaluate over hand-built Events/Facts with
// ZERO I/O (invariant 7), reusing rules_test.go's helpers (executesOf, dump,
// exactlyOneExecute, argInt, argStr, fbPayload).
//
// THE CONTRACT. `ruleDeliveryLifecycle` returns nil — no task_mark_delivered,
// no task_close, and NO record_orchestration — when the event payload's
// `channel` is "calendar", read with the existing payloadStr helper.
//
// WHY THE MISSING record_orchestration IS THE SHARP HALF. R8 is channel-blind
// today. A booking against a task that is not done_locally makes
// task_mark_delivered FAIL; the engine logs the failure and carries on
// (engine.go:110-141), and the record_orchestration that follows is suppressed
// only after a failed create_task. So the `delivery_lifecycle` dedup key gets
// written anyway — and the task's LATER real delivery is then deduped and never
// marks it delivered. An implementation that skipped only the two mutations
// would look correct in a status assertion and still burn the key, silently,
// once. That is why the calendar case asserts ZERO actions rather than
// "no task_mark_delivered".
//
// GREENFIELD NOTE: rules.go:274 has no channel test, so the calendar case FAILS
// today with three actions where it wants none. The gmail and absent-channel
// controls pass today and must keep passing — they are what stops the fix from
// being "return nil" or "skip whenever channel is not gmail".

import (
	"testing"

	orch "github.com/sspataro57/switchboard/internal/orchestrator"
)

func TestEvaluate_R8_CalendarDeliverySentFiresNothing(t *testing.T) {
	const (
		eventID     = int64(2800)
		workTask    = int64(28)
		deliverTask = int64(280)
		deliveryID  = int64(2801)
	)
	// The facts are the FAVOURABLE ones: an R3 delivery_task record exists and
	// no delivery_lifecycle has been recorded, so every other channel would
	// fire all three actions here. Nothing but the channel may make the
	// difference.
	facts := orch.Facts{
		Task: orch.TaskFacts{ID: workTask, ProjectSlug: "acme", ProjectDelivery: "dashboard"},
		Orchestrations: []orch.Orchestration{
			{Rule: "delivery_task", TaskID: workTask, CreatedTaskID: deliverTask},
		},
	}

	t.Run("channel calendar -> zero actions, including no record_orchestration", func(t *testing.T) {
		ev := orch.Event{
			ID: eventID, TaskID: workTask, Type: "delivery_sent",
			Payload: fbPayload("delivery_id", float64(deliveryID), "channel", "calendar",
				"sent_external_id", "calendar:sb1zt2abc"),
		}
		actions := orch.Evaluate(ev, facts, orch.Config{})
		if len(actions) != 0 {
			t.Fatalf("a calendar delivery_sent produced %d actions, want 0: %s\n\n"+
				"Q2 answered (b): a booking never advances a task's lifecycle. A block is usually "+
				"INCIDENTAL to work still in progress (\"reserve two hours to finish this\"), and "+
				"task_mark_delivered refuses a task that is not done_locally — so firing here marks nothing "+
				"delivered and only burns the dedup key.", len(actions), dump(actions))
		}
		if n := len(executesOf(actions, "record_orchestration")); n != 0 {
			t.Errorf("record_orchestration fired %d times for a calendar booking. This is the sharp half: the "+
				"delivery_lifecycle record is R8's dedup key, so writing it here means the task's LATER real "+
				"delivery is deduped and NEVER marks the task delivered — a silent, permanent loss of the "+
				"lifecycle transition, one task at a time", n)
		}
	})

	t.Run("channel gmail is unchanged: mark + close + record", func(t *testing.T) {
		ev := orch.Event{
			ID: eventID, TaskID: workTask, Type: "delivery_sent",
			Payload: fbPayload("delivery_id", float64(deliveryID), "channel", "gmail"),
		}
		actions := orch.Evaluate(ev, facts, orch.Config{})

		mark := exactlyOneExecute(t, actions, "task_mark_delivered")
		if got := argInt(t, mark.Args, "task_id"); got != workTask {
			t.Errorf("task_mark_delivered task_id = %d, want the work task %d", got, workTask)
		}
		closeA := exactlyOneExecute(t, actions, "task_close")
		if got := argInt(t, closeA.Args, "task_id"); got != deliverTask {
			t.Errorf("task_close task_id = %d, want the Deliver task %d", got, deliverTask)
		}
		rec := exactlyOneExecute(t, actions, "record_orchestration")
		if got := argStr(t, rec.Args, "rule"); got != "delivery_lifecycle" {
			t.Errorf("record rule = %q, want delivery_lifecycle", got)
		}
	})

	t.Run("slack_reply is unchanged too: the skip is calendar-only, not a not-gmail test", func(t *testing.T) {
		ev := orch.Event{
			ID: eventID, TaskID: workTask, Type: "delivery_sent",
			Payload: fbPayload("delivery_id", float64(deliveryID), "channel", "slack_reply"),
		}
		if n := len(orch.Evaluate(ev, facts, orch.Config{})); n == 0 {
			t.Fatalf("a slack_reply delivery_sent produced zero actions. Criterion 29 skips channel == " +
				"\"calendar\" and nothing else; a guard written as \"only gmail proceeds\" would silently " +
				"stop marking slack and jira deliveries")
		}
	})

	t.Run("no channel key at all behaves exactly as today", func(t *testing.T) {
		// Every delivery_sent payload the send handlers emit carries `channel`
		// (delivery.go's gmail, jira and slack branches all set it), but the
		// RULE must not depend on that: payloadStr returns "" for an absent key,
		// and "" must take the normal path. An absent key that became a silent
		// skip would turn any future writer's omission into a task that is
		// never marked delivered, with no error anywhere — the failure mode
		// this repo has recorded four times.
		ev := orch.Event{
			ID: eventID, TaskID: workTask, Type: "delivery_sent",
			Payload: fbPayload("delivery_id", float64(deliveryID)),
		}
		actions := orch.Evaluate(ev, facts, orch.Config{})
		if len(executesOf(actions, "task_mark_delivered")) != 1 {
			t.Fatalf("a delivery_sent with NO channel key produced %s, want the unchanged R8 behaviour "+
				"(mark + close + record). Absent-because-unknown is not absent-because-calendar", dump(actions))
		}
		if len(executesOf(actions, "record_orchestration")) != 1 {
			t.Errorf("the absent-channel case must still record delivery_lifecycle: %s", dump(actions))
		}
	})

	t.Run("calendar skip does not depend on the R3 record or on dedup state", func(t *testing.T) {
		// Two shapes that reach different code inside the rule today: no
		// delivery_task record (nothing to close) and an already-recorded
		// lifecycle. Both must still be zero actions for calendar.
		ev := orch.Event{
			ID: eventID, TaskID: workTask, Type: "delivery_sent",
			Payload: fbPayload("delivery_id", float64(deliveryID), "channel", "calendar"),
		}
		bare := orch.Facts{Task: orch.TaskFacts{ID: workTask, ProjectSlug: "acme", ProjectDelivery: "dashboard"}}
		if actions := orch.Evaluate(ev, bare, orch.Config{}); len(actions) != 0 {
			t.Errorf("calendar booking with no R3 record produced %s, want none", dump(actions))
		}
		already := orch.Facts{
			Task: orch.TaskFacts{ID: workTask, ProjectSlug: "acme", ProjectDelivery: "dashboard"},
			Orchestrations: []orch.Orchestration{
				{Rule: "delivery_task", TaskID: workTask, CreatedTaskID: deliverTask},
				{Rule: "delivery_lifecycle", TaskID: workTask},
			},
		}
		if actions := orch.Evaluate(ev, already, orch.Config{}); len(actions) != 0 {
			t.Errorf("calendar booking on an already-recorded task produced %s, want none", dump(actions))
		}
	})
}
