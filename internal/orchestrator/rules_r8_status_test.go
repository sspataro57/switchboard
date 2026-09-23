package orchestrator_test

// R8's task-status gate (slack-auto-tier / SWT-77,
// docs/tickets/slack-auto-tier_SPEC.md, decision D8, acceptance criterion 21).
// Same voice and shape as rules_r8_test.go and rules_r8_calendar_test.go:
// orchestrator.Evaluate over hand-built Events/Facts, ZERO I/O (invariant 7),
// reusing rules_test.go's helpers (executesOf, dump, exactlyOneExecute, argInt,
// argStr, fbPayload).
//
// THE CONTRACT. ruleDeliveryLifecycle returns ZERO actions — no
// task_mark_delivered, no task_close and, crucially, no record_orchestration —
// when f.Task.Status is not one of done_locally, delivered, closed: the exact
// set task_mark_delivered accepts (tools/close.go; schemas.go "Only done_locally
// moves; delivered or closed is a no-op success; anything else is refused").
// The calendar skip stays where it is and is checked FIRST.
//
// WHY THIS TICKET HAS TO CLOSE IT. send_slack_reply lets a session post to
// Slack on ORDINARY tasks — in_progress, claimed, ready — and every send emits
// delivery_sent (at the click, or later from slackweb's export promotion).
// Today R8 is status-blind: task_mark_delivered is refused for those tasks, the
// engine logs the failure and continues, and the record_orchestration that
// follows lands anyway. That record is R8's dedup key, so the task's LATER real
// delivery is deduped into silence: it never moves to delivered and its Deliver
// task is never closed. SWT-28 found this for calendar and skipped the channel;
// D8 generalises the reasoning to the task's status.
//
// GREENFIELD NOTE — EXPECTED RED. rules.go:274-309 reads no status, so every
// non-accepted status case below fails today with three (or two) actions where
// it wants none. The done_locally / delivered / closed controls pass today and
// must keep passing: they are what stops the fix from being "return nil".
//
// MUTATIONS THAT MUST TURN THIS RED: drop the status gate (every zero case);
// gate on `!= "done_locally"` only (the delivered/closed controls); move the
// gate above the calendar skip AND make it return actions for calendar
// (calendar case); skip only task_mark_delivered but still record (the
// record_orchestration assertion).

import (
	"testing"

	orch "github.com/sspataro57/switchboard/internal/orchestrator"
)

const (
	r8sEventID     = int64(7700)
	r8sWorkTask    = int64(77)
	r8sDeliverTask = int64(770)
	r8sDeliveryID  = int64(7701)
)

// r8sFacts are the FAVOURABLE facts for R8: an R3 delivery_task record exists
// and no delivery_lifecycle has been recorded, so the only thing that can make
// the difference between three actions and none is the task's status.
func r8sFacts(status string) orch.Facts {
	return orch.Facts{
		Task: orch.TaskFacts{ID: r8sWorkTask, ProjectSlug: "acme", ProjectDelivery: "dashboard", Status: status},
		Orchestrations: []orch.Orchestration{
			{Rule: "delivery_task", TaskID: r8sWorkTask, CreatedTaskID: r8sDeliverTask},
		},
	}
}

func r8sEvent(channel string) orch.Event {
	payload := fbPayload("delivery_id", float64(r8sDeliveryID))
	if channel != "" {
		payload = fbPayload("delivery_id", float64(r8sDeliveryID), "channel", channel)
	}
	return orch.Event{ID: r8sEventID, TaskID: r8sWorkTask, Type: "delivery_sent", Payload: payload}
}

// Criterion 21, the headline case: a Slack reply sent while the task is still
// being worked on. This is the shape send_slack_reply makes ROUTINE.
func TestEvaluate_R8_InProgressTaskFiresNothing(t *testing.T) {
	actions := orch.Evaluate(r8sEvent("slack_reply"), r8sFacts("in_progress"), orch.Config{})
	if len(actions) != 0 {
		t.Fatalf("delivery_sent on an in_progress task produced %d actions, want 0: %s\n\n"+
			"D8: task_mark_delivered refuses a task that is not done_locally, the engine logs that and carries "+
			"on, and the record_orchestration below it lands anyway — burning the delivery_lifecycle dedup key, "+
			"so the task's LATER real delivery never marks it delivered", len(actions), dump(actions))
	}
	if n := len(executesOf(actions, "record_orchestration")); n != 0 {
		t.Errorf("record_orchestration fired %d times for a task R8 cannot mark delivered. This is the sharp "+
			"half: skipping only task_mark_delivered and still recording is the bug, not the fix", n)
	}
}

// Criterion 21 over the whole status set: every status task_mark_delivered
// would refuse. Enumerated, not sampled — a gate written as `== "in_progress"`
// would pass the headline case and leave claimed/ready/needs_feedback open.
// "" is included: facts.go always loads the status, so an empty one means
// "unknown", and the gate is an allow-list (the exact set the verb accepts).
func TestEvaluate_R8_EveryUnmarkableStatusFiresNothing(t *testing.T) {
	for _, status := range []string{
		"holding", "ready", "claimed", "in_progress", "needs_feedback",
		"pr_open", "awaiting_ci", "awaiting_merge", "blocked", "",
	} {
		status := status
		name := status
		if name == "" {
			name = "(empty status)"
		}
		for _, channel := range []string{"slack_reply", "gmail", "jira_comment", ""} {
			channel := channel
			t.Run(name+"/channel="+channel, func(t *testing.T) {
				actions := orch.Evaluate(r8sEvent(channel), r8sFacts(status), orch.Config{})
				if len(actions) != 0 {
					t.Errorf("delivery_sent (channel %q) on a %q task produced %s, want zero actions. D8 is a "+
						"status gate, not a channel gate: task_mark_delivered accepts only done_locally, "+
						"delivered and closed, and a lifecycle record for a refused action mutes the task's real "+
						"delivery later", channel, status, dump(actions))
				}
			})
		}
	}
}

// Criterion 21's control: a done_locally task yields TODAY's three actions,
// unchanged — mark the work task, close the R3 Deliver task, record the key.
func TestEvaluate_R8_DoneLocallyTaskKeepsTodaysThreeActions(t *testing.T) {
	for _, channel := range []string{"slack_reply", "gmail", ""} {
		channel := channel
		t.Run("channel="+channel, func(t *testing.T) {
			actions := orch.Evaluate(r8sEvent(channel), r8sFacts("done_locally"), orch.Config{})
			if len(actions) != 3 {
				t.Fatalf("done_locally delivery_sent (channel %q) produced %s, want exactly three actions "+
					"(mark + close + record)", channel, dump(actions))
			}
			if actions[0].Tool != "task_mark_delivered" || actions[1].Tool != "task_close" ||
				actions[2].Tool != "record_orchestration" {
				t.Errorf("action order = %s, want task_mark_delivered, task_close, record_orchestration "+
					"(today's order, unchanged)", dump(actions))
			}
			mark := exactlyOneExecute(t, actions, "task_mark_delivered")
			if got := argInt(t, mark.Args, "task_id"); got != r8sWorkTask {
				t.Errorf("task_mark_delivered task_id = %d, want the work task %d", got, r8sWorkTask)
			}
			closeA := exactlyOneExecute(t, actions, "task_close")
			if got := argInt(t, closeA.Args, "task_id"); got != r8sDeliverTask {
				t.Errorf("task_close task_id = %d, want the Deliver task %d", got, r8sDeliverTask)
			}
			rec := exactlyOneExecute(t, actions, "record_orchestration")
			if got := argStr(t, rec.Args, "rule"); got != "delivery_lifecycle" {
				t.Errorf("record rule = %q, want delivery_lifecycle", got)
			}
		})
	}
}

// delivered and closed are in the accepted set too (task_mark_delivered is a
// no-op success for both), so D8 leaves them on today's path. Guards against a
// fix written as `Status != "done_locally" -> nil`.
func TestEvaluate_R8_DeliveredAndClosedKeepTodaysPath(t *testing.T) {
	for _, status := range []string{"delivered", "closed"} {
		status := status
		t.Run(status, func(t *testing.T) {
			actions := orch.Evaluate(r8sEvent("slack_reply"), r8sFacts(status), orch.Config{})
			if len(executesOf(actions, "task_mark_delivered")) != 1 {
				t.Errorf("delivery_sent on a %s task produced %s, want today's task_mark_delivered (a no-op "+
					"success for %s — D8 names the exact set the verb accepts)", status, dump(actions), status)
			}
			if len(executesOf(actions, "record_orchestration")) != 1 {
				t.Errorf("delivery_sent on a %s task must still record delivery_lifecycle: %s", status, dump(actions))
			}
		})
	}
}

// The calendar skip is still checked FIRST and is still unconditional: zero
// actions for a calendar booking whatever the task's status — including
// done_locally, the one status where every other channel fires.
func TestEvaluate_R8_CalendarSkipStillFirstAndUnconditional(t *testing.T) {
	for _, status := range []string{"done_locally", "delivered", "closed", "in_progress", ""} {
		status := status
		t.Run("status="+status, func(t *testing.T) {
			if actions := orch.Evaluate(r8sEvent("calendar"), r8sFacts(status), orch.Config{}); len(actions) != 0 {
				t.Errorf("a calendar delivery_sent on a %q task produced %s, want none. SWT-28 criterion 29 "+
					"is unchanged by D8: a booking never advances a task's lifecycle", status, dump(actions))
			}
		})
	}
}

// Dedup still applies on the accepted path: a done_locally task already
// carrying delivery_lifecycle yields nothing.
func TestEvaluate_R8_StatusGateDoesNotDisturbDedup(t *testing.T) {
	f := r8sFacts("done_locally")
	f.Orchestrations = append(f.Orchestrations, orch.Orchestration{Rule: "delivery_lifecycle", TaskID: r8sWorkTask})
	if actions := orch.Evaluate(r8sEvent("slack_reply"), f, orch.Config{}); len(actions) != 0 {
		t.Errorf("done_locally task with delivery_lifecycle already recorded produced %s, want none", dump(actions))
	}
}
