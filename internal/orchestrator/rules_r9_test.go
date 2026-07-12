package orchestrator_test

// R9/R10/R11 pure-rule tests (SPEC 09-jira-github-connectors, criterion 14).
// Like the rest of rules_test.go this exercises orchestrator.Evaluate over
// hand-built Events/Facts with ZERO I/O (invariant 7) and reuses that file's
// assertion helpers (same _test package): executesOf, exactlyOneExecute,
// argInt, argStr, dump, fbPayload.
//
// GREENFIELD NOTE: R9-R11 are not in Evaluate yet AND this file references two
// imposed struct fields that rules.go does not define yet:
//   - orch.Facts.CIFailureStreak  int   // consecutive ci_failed since the last
//                                         // ci_passed/pr_opened (the SPEC's CI
//                                         // failure streak Fact, computed in SQL
//                                         // by facts.go — criterion 14 / inv. 7)
//   - orch.Orchestration.TriggerEventID int64 // the trigger_event_id the
//                                         // record_orchestration wrote; facts.go
//                                         // surfaces it so multi-event PR/CI
//                                         // rules dedup per event, not per task.
// Until those fields + the R9-R11 branches exist, the whole orchestrator_test
// package does NOT compile — the expected greenfield failure. The existing
// R1-R8 tests resume compiling and passing once the fields land.
//
// Contract (criterion 14). New task_events feed these rules (record_pr_event /
// record_ci_event write them): pr_opened|pr_merged|pr_closed, ci_started|
// ci_passed|ci_failed. All three rules dedup via a record_orchestration whose
// trigger_event_id matches ev.ID (idempotent replay; tool-level idempotency on
// pr/run id is the other half). The PR/CI status change is always effected via
// task_pr_transition (same-status => tool no-op); done_locally's transition
// handler emits a done_local task_event so R3 (Deliver task) chains unchanged.

import (
	"strings"
	"testing"

	orch "github.com/sspataro57/switchboard/internal/orchestrator"
)

// dedupRec builds a prior orchestration record that matches THIS event id.
func dedupRec(rule string, task, evID int64) orch.Orchestration {
	return orch.Orchestration{Rule: rule, TaskID: task, TriggerEventID: evID}
}

// ---- R9: pr lifecycle ---------------------------------------------------------

func TestEvaluate_R9_PRLifecycle(t *testing.T) {
	const (
		task  = int64(9)
		evID  = int64(900)
		prNum = float64(12)
		url   = "https://github.com/sspataro57/switchboard/pull/12"
	)

	t.Run("pr_opened -> transition to pr_open + record", func(t *testing.T) {
		ev := orch.Event{ID: evID, TaskID: task, Type: "pr_opened", Payload: fbPayload("pr", prNum, "url", url)}
		f := orch.Facts{Task: orch.TaskFacts{ID: task, Status: "in_progress"}}
		actions := orch.Evaluate(ev, f, orch.Config{})

		tr := exactlyOneExecute(t, actions, "task_pr_transition")
		if got := argInt(t, tr.Args, "task_id"); got != task {
			t.Errorf("transition task_id = %d, want %d", got, task)
		}
		if got := argStr(t, tr.Args, "to"); got != "pr_open" {
			t.Errorf("transition to = %q, want pr_open", got)
		}
		rec := exactlyOneExecute(t, actions, "record_orchestration")
		if got := argStr(t, rec.Args, "rule"); got != "pr_lifecycle" {
			t.Errorf("record rule = %q, want pr_lifecycle", got)
		}
		if got := argInt(t, rec.Args, "trigger_event_id"); got != evID {
			t.Errorf("record trigger_event_id = %d, want %d", got, evID)
		}
	})

	t.Run("pr_merged -> transition to done_locally with the PR url in the summary", func(t *testing.T) {
		ev := orch.Event{ID: evID, TaskID: task, Type: "pr_merged", Payload: fbPayload("pr", prNum, "url", url)}
		f := orch.Facts{Task: orch.TaskFacts{ID: task, Status: "awaiting_merge"}}
		tr := exactlyOneExecute(t, orch.Evaluate(ev, f, orch.Config{}), "task_pr_transition")
		if got := argStr(t, tr.Args, "to"); got != "done_locally" {
			t.Errorf("merge transition to = %q, want done_locally (handler emits done_local so R3 chains)", got)
		}
		// The transition carries a summary naming the merged PR (R3's Deliver body
		// surfaces it).
		summary := argStr(t, tr.Args, "summary")
		if !strings.Contains(summary, url) {
			t.Errorf("merge transition summary = %q, want it to reference the PR url %q", summary, url)
		}
	})

	t.Run("pr_closed (unmerged) -> back to ready + append_log", func(t *testing.T) {
		ev := orch.Event{ID: evID, TaskID: task, Type: "pr_closed", Payload: fbPayload("pr", prNum, "url", url)}
		f := orch.Facts{Task: orch.TaskFacts{ID: task, Status: "pr_open"}}
		actions := orch.Evaluate(ev, f, orch.Config{})
		tr := exactlyOneExecute(t, actions, "task_pr_transition")
		if got := argStr(t, tr.Args, "to"); got != "ready" {
			t.Errorf("closed-unmerged transition to = %q, want ready", got)
		}
		if n := len(executesOf(actions, "task_append_log")); n != 1 {
			t.Errorf("pr_closed must append a log explaining the re-queue; task_append_log count = %d", n)
		}
	})

	t.Run("dedup: pr_lifecycle already recorded for THIS event -> no actions", func(t *testing.T) {
		ev := orch.Event{ID: evID, TaskID: task, Type: "pr_opened", Payload: fbPayload("pr", prNum, "url", url)}
		f := orch.Facts{
			Task:           orch.TaskFacts{ID: task, Status: "in_progress"},
			Orchestrations: []orch.Orchestration{dedupRec("pr_lifecycle", task, evID)},
		}
		if actions := orch.Evaluate(ev, f, orch.Config{}); len(actions) != 0 {
			t.Fatalf("R9 must dedup on trigger_event_id; got %s", dump(actions))
		}
	})
}

// ---- R10: ci lifecycle --------------------------------------------------------

func TestEvaluate_R10_CILifecycle(t *testing.T) {
	const (
		task = int64(9)
		evID = int64(1000)
	)
	payload := fbPayload("run_id", float64(4242), "run_url", "https://github.com/o/r/actions/runs/4242")

	t.Run("ci_started (from pr_open) -> awaiting_ci", func(t *testing.T) {
		ev := orch.Event{ID: evID, TaskID: task, Type: "ci_started", Payload: payload}
		f := orch.Facts{Task: orch.TaskFacts{ID: task, Status: "pr_open"}}
		actions := orch.Evaluate(ev, f, orch.Config{})
		tr := exactlyOneExecute(t, actions, "task_pr_transition")
		if got := argStr(t, tr.Args, "to"); got != "awaiting_ci" {
			t.Errorf("ci_started transition to = %q, want awaiting_ci", got)
		}
		rec := exactlyOneExecute(t, actions, "record_orchestration")
		if got := argStr(t, rec.Args, "rule"); got != "ci_lifecycle" {
			t.Errorf("record rule = %q, want ci_lifecycle", got)
		}
	})

	t.Run("ci_passed (from awaiting_ci) -> awaiting_merge", func(t *testing.T) {
		ev := orch.Event{ID: evID, TaskID: task, Type: "ci_passed", Payload: payload}
		f := orch.Facts{Task: orch.TaskFacts{ID: task, Status: "awaiting_ci"}}
		tr := exactlyOneExecute(t, orch.Evaluate(ev, f, orch.Config{}), "task_pr_transition")
		if got := argStr(t, tr.Args, "to"); got != "awaiting_merge" {
			t.Errorf("ci_passed transition to = %q, want awaiting_merge", got)
		}
	})

	t.Run("dedup: ci_lifecycle already recorded for THIS event -> no actions", func(t *testing.T) {
		ev := orch.Event{ID: evID, TaskID: task, Type: "ci_started", Payload: payload}
		f := orch.Facts{
			Task:           orch.TaskFacts{ID: task, Status: "pr_open"},
			Orchestrations: []orch.Orchestration{dedupRec("ci_lifecycle", task, evID)},
		}
		if actions := orch.Evaluate(ev, f, orch.Config{}); len(actions) != 0 {
			t.Fatalf("R10 must dedup on trigger_event_id; got %s", dump(actions))
		}
	})
}

// ---- R11: ci failure streak ---------------------------------------------------

func TestEvaluate_R11_CIFailureStreak(t *testing.T) {
	const (
		task   = int64(9)
		evID   = int64(1100)
		runURL = "https://github.com/o/r/actions/runs/77"
	)
	mkEvent := func() orch.Event {
		return orch.Event{ID: evID, TaskID: task, Type: "ci_failed",
			Payload: fbPayload("run_id", float64(77), "run_url", runURL)}
	}

	t.Run("streak 1 -> append_log only (retry in place, same task)", func(t *testing.T) {
		f := orch.Facts{Task: orch.TaskFacts{ID: task, Status: "awaiting_ci"}, CIFailureStreak: 1}
		actions := orch.Evaluate(mkEvent(), f, orch.Config{})

		logA := exactlyOneExecute(t, actions, "task_append_log")
		if !anyArgContains(logA.Args, runURL) {
			t.Errorf("streak-1 log must carry the run URL %q; args=%v", runURL, logA.Args)
		}
		if n := len(executesOf(actions, "task_pr_transition")); n != 0 {
			t.Errorf("streak 1 must NOT transition back to ready; task_pr_transition count = %d", n)
		}
		rec := exactlyOneExecute(t, actions, "record_orchestration")
		if got := argStr(t, rec.Args, "rule"); got != "ci_failure" {
			t.Errorf("record rule = %q, want ci_failure", got)
		}
	})

	t.Run("streak 2 -> transition to ready (same task, never a new one) + log", func(t *testing.T) {
		f := orch.Facts{Task: orch.TaskFacts{ID: task, Status: "awaiting_ci"}, CIFailureStreak: 2}
		actions := orch.Evaluate(mkEvent(), f, orch.Config{})

		tr := exactlyOneExecute(t, actions, "task_pr_transition")
		if got := argInt(t, tr.Args, "task_id"); got != task {
			t.Errorf("streak-2 transition task_id = %d, want the SAME task %d (CLAUDE.md status machine)", got, task)
		}
		if got := argStr(t, tr.Args, "to"); got != "ready" {
			t.Errorf("streak-2 transition to = %q, want ready", got)
		}
		if n := len(executesOf(actions, "create_task")); n != 0 {
			t.Errorf("red CI ×2 must reuse the same task, never create one; create_task count = %d", n)
		}
		if n := len(executesOf(actions, "task_append_log")); n != 1 {
			t.Errorf("streak-2 must also append a log with the failure; task_append_log count = %d", n)
		}
	})

	t.Run("dedup: ci_failure already recorded for THIS event -> no actions", func(t *testing.T) {
		f := orch.Facts{
			Task:            orch.TaskFacts{ID: task, Status: "awaiting_ci"},
			CIFailureStreak: 2,
			Orchestrations:  []orch.Orchestration{dedupRec("ci_failure", task, evID)},
		}
		if actions := orch.Evaluate(mkEvent(), f, orch.Config{}); len(actions) != 0 {
			t.Fatalf("R11 must dedup on trigger_event_id; got %s", dump(actions))
		}
	})
}

// anyArgContains reports whether any string-valued arg contains sub (the log
// message arg name is left to the impl — message/log/summary all acceptable).
func anyArgContains(args map[string]any, sub string) bool {
	for _, v := range args {
		if s, ok := v.(string); ok && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
