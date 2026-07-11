package orchestrator_test

// THE invariant-7 proof (SPEC 05-orchestrator-loop, acceptance criterion 1):
// the orchestrator's rules are pure functions of (event, facts, config) with
// ZERO I/O. This file imports no pgx, no paho, no net, no provider adapter — it
// exercises orchestrator.Evaluate over hand-built Events/Facts/Config and asserts
// the exact typed Actions each rule (R1–R7) emits, plus every no-op and
// skip-branch the SPEC pins. It must run under `go test -count=1` in <1s.
//
// GREENFIELD NOTE: internal/orchestrator does not exist yet, so this file
// compile-FAILs until rules.go is written. That is the expected failure mode
// for a spec-first test. The exported surface imposed here IS the contract the
// SPEC's "internal/orchestrator/ rules.go (pure rules + Event/Facts/Config/
// Action types)" line leaves to the implementer; match these names.
//
// ---- Imposed exported surface (rules.go) --------------------------------------
//
//   // Evaluate is the pure rule core: given one event, the read-only facts a
//   // loader gathered for it, and static config, it returns the ordered actions
//   // to apply. No I/O, no clock reads except ev.Now. "orchestrated" and unknown
//   // event types return nil (no rule fires; no loops).
//   func Evaluate(ev Event, f Facts, cfg Config) []Action
//
//   const EventTick = "tick" // synthetic event for the cron ticker (R6, R7)
//
//   const (
//       ActionExecute = "execute" // a registered tool call via Executor.Execute
//       ActionPublish = "publish" // a fleet command via PublishCommand
//   )
//
//   type Event struct {
//       ID      int64          // task_events.id (trigger_event_id); 0 for a tick
//       TaskID  int64          // task_events.task_id (the triggering task N)
//       Type    string         // event_type, or EventTick
//       Payload map[string]any // parsed task_events.payload (JSON: numbers are float64)
//       Now     time.Time      // evaluation clock (tick rules only; process-local TZ)
//   }
//
//   type Action struct {
//       Kind        string         // ActionExecute | ActionPublish
//       Tool        string         // executor tool name          (ActionExecute)
//       Args        map[string]any // tool args (Go-native values) (ActionExecute)
//       WorkerID    string         // target worker                (ActionPublish)
//       PublishVerb string         // fleet cmd verb, e.g. "resume"(ActionPublish)
//       PublishArgs map[string]any // fleet cmd args               (ActionPublish)
//   }
//
//   type Config struct { BriefProject string; BriefHour int } // BriefProject=="" disables R7
//
//   type Facts struct {
//       Task                TaskFacts        // the triggering task (R1, R3, R4)
//       Orchestrations      []Orchestration  // prior 'orchestrated' rows (dedup: R1/R2/R3)
//       ActiveClaimWorkerID string           // unreleased-claim holder on Task ("" = none) (R2)
//       Dependents          []DependentTask  // tasks depending on Task (R5)
//       ExpiredClaims       []ExpiredClaim    // claims to reap this tick (R6)
//       BriefExists         bool             // a dated brief already exists (R7 dedup)
//       BriefCounts         []ProjectCounts  // per-project snapshot for the brief body (R7)
//   }
//   type TaskFacts    struct { ID int64; ProjectSlug, ProjectDelivery, Title, Status string; HasUnmetDep bool }
//   type Orchestration struct { Rule string; FeedbackRequestID, CreatedTaskID, TaskID int64 }
//   type DependentTask struct { ID int64; Status string; AllDepsSatisfied bool }
//   type ExpiredClaim  struct { TaskID int64; WorkerID, Status string }
//   type ProjectCounts struct { ProjectSlug string; Ready, Blocked, NeedsFeedback, DoneLocally, OpenFeedback int }
//
// NOTE on created_task_id: R1/R3 emit create_task followed by record_orchestration.
// The created task's id is a RUNTIME output the pure layer cannot know, so it is
// injected by the applier (verified in the integration test's orchestrated-row
// assertion), NOT asserted here. The pure test asserts everything deterministic.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	orch "github.com/sspataro57/switchboard/internal/orchestrator"
)

// ---- assertion helpers --------------------------------------------------------

func executesOf(actions []orch.Action, tool string) []orch.Action {
	var out []orch.Action
	for _, a := range actions {
		if a.Kind == orch.ActionExecute && a.Tool == tool {
			out = append(out, a)
		}
	}
	return out
}

func publishesOf(actions []orch.Action) []orch.Action {
	var out []orch.Action
	for _, a := range actions {
		if a.Kind == orch.ActionPublish {
			out = append(out, a)
		}
	}
	return out
}

func exactlyOneExecute(t *testing.T, actions []orch.Action, tool string) orch.Action {
	t.Helper()
	got := executesOf(actions, tool)
	if len(got) != 1 {
		t.Fatalf("want exactly one %q execute action, got %d (all: %s)", tool, len(got), dump(actions))
	}
	return got[0]
}

func dump(actions []orch.Action) string {
	var b strings.Builder
	for i, a := range actions {
		if a.Kind == orch.ActionPublish {
			fmt.Fprintf(&b, "\n  [%d] publish -> %s verb=%s args=%v", i, a.WorkerID, a.PublishVerb, a.PublishArgs)
		} else {
			fmt.Fprintf(&b, "\n  [%d] execute %s args=%v", i, a.Tool, a.Args)
		}
	}
	return b.String()
}

func argInt(t *testing.T, args map[string]any, key string) int64 {
	t.Helper()
	v, ok := args[key]
	if !ok {
		t.Fatalf("args missing %q (args=%v)", key, args)
	}
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		t.Fatalf("arg %q is %T, want an integer", key, v)
		return 0
	}
}

func argStr(t *testing.T, args map[string]any, key string) string {
	t.Helper()
	v, ok := args[key]
	if !ok {
		t.Fatalf("args missing %q (args=%v)", key, args)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("arg %q is %T, want string", key, v)
	}
	return s
}

func subMap(t *testing.T, args map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := args[key]
	if !ok {
		t.Fatalf("args missing %q (args=%v)", key, args)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("arg %q is %T, want map[string]any", key, v)
	}
	return m
}

// fbPayload builds a JSON-style payload (numbers as float64, mimicking a
// json-unmarshalled task_events.payload).
func fbPayload(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

// ---- R1: feedback task on feedback_requested ---------------------------------

func TestEvaluate_R1_FeedbackTask(t *testing.T) {
	const (
		eventID  = int64(100)
		taskN    = int64(7)
		fbM      = int64(42)
		slug     = "acme"
		question = "which database engine?"
	)
	ev := orch.Event{
		ID:      eventID,
		TaskID:  taskN,
		Type:    "feedback_requested",
		Payload: fbPayload("feedback_request_id", float64(fbM), "question", question),
	}
	f := orch.Facts{Task: orch.TaskFacts{ID: taskN, ProjectSlug: slug}}

	t.Run("fires", func(t *testing.T) {
		actions := orch.Evaluate(ev, f, orch.Config{})

		create := exactlyOneExecute(t, actions, "create_task")
		if got := argStr(t, create.Args, "project"); got != slug {
			t.Errorf("answer task project = %q, want %q", got, slug)
		}
		if got := argInt(t, create.Args, "parent_id"); got != taskN {
			t.Errorf("answer task parent_id = %d, want %d (SPEC: parent = the parked task)", got, taskN)
		}
		if got := argStr(t, create.Args, "assignee_type"); got != "human" {
			t.Errorf("answer task assignee_type = %q, want human", got)
		}
		wantTitle := fmt.Sprintf("Answer feedback #%d on task #%d", fbM, taskN)
		if got := argStr(t, create.Args, "title"); got != wantTitle {
			t.Errorf("answer task title = %q, want %q", got, wantTitle)
		}
		body := argStr(t, create.Args, "body")
		for _, want := range []string{question, "answer-feedback", "--id", fmt.Sprintf("%d", fbM)} {
			if !strings.Contains(body, want) {
				t.Errorf("answer task body missing %q; body=%q", want, body)
			}
		}

		rec := exactlyOneExecute(t, actions, "record_orchestration")
		if got := argInt(t, rec.Args, "task_id"); got != taskN {
			t.Errorf("record_orchestration task_id = %d, want %d", got, taskN)
		}
		if got := argStr(t, rec.Args, "rule"); got != "feedback_task" {
			t.Errorf("record rule = %q, want feedback_task", got)
		}
		if got := argInt(t, rec.Args, "trigger_event_id"); got != eventID {
			t.Errorf("record trigger_event_id = %d, want %d", got, eventID)
		}
		if got := argInt(t, subMap(t, rec.Args, "payload"), "feedback_request_id"); got != fbM {
			t.Errorf("record payload feedback_request_id = %d, want %d", got, fbM)
		}
	})

	t.Run("dedup skip when feedback_task already orchestrated for M", func(t *testing.T) {
		dd := f
		dd.Orchestrations = []orch.Orchestration{{Rule: "feedback_task", FeedbackRequestID: fbM, TaskID: taskN, CreatedTaskID: 999}}
		if actions := orch.Evaluate(ev, dd, orch.Config{}); len(actions) != 0 {
			t.Fatalf("R1 must be a no-op when already orchestrated for M; got %s", dump(actions))
		}
	})

	t.Run("dedup does not match a different M", func(t *testing.T) {
		dd := f
		dd.Orchestrations = []orch.Orchestration{{Rule: "feedback_task", FeedbackRequestID: fbM + 1, TaskID: taskN}}
		if actions := orch.Evaluate(ev, dd, orch.Config{}); len(executesOf(actions, "create_task")) != 1 {
			t.Fatalf("R1 must still fire for a different M; got %s", dump(actions))
		}
	})
}

// ---- R2: resume on answer + close answer task --------------------------------

func TestEvaluate_R2_ResumeOnAnswer(t *testing.T) {
	const (
		eventID    = int64(200)
		taskN      = int64(7)
		fbM        = int64(42)
		holder     = "switchboard"
		answerTask = int64(55)
	)
	ev := orch.Event{
		ID:      eventID,
		TaskID:  taskN,
		Type:    "feedback_answered",
		Payload: fbPayload("feedback_request_id", float64(fbM)),
	}
	// R1's decision is loaded so R2 can find the answer task to close.
	priorR1 := orch.Orchestration{Rule: "feedback_task", FeedbackRequestID: fbM, TaskID: taskN, CreatedTaskID: answerTask}

	t.Run("active claim: publish resume + close answer + record", func(t *testing.T) {
		f := orch.Facts{
			Task:                orch.TaskFacts{ID: taskN},
			ActiveClaimWorkerID: holder,
			Orchestrations:      []orch.Orchestration{priorR1},
		}
		actions := orch.Evaluate(ev, f, orch.Config{})

		pubs := publishesOf(actions)
		if len(pubs) != 1 {
			t.Fatalf("want exactly one publish (resume); got %d: %s", len(pubs), dump(actions))
		}
		if pubs[0].WorkerID != holder {
			t.Errorf("resume worker = %q, want %q (the active-claim holder)", pubs[0].WorkerID, holder)
		}
		if pubs[0].PublishVerb != "resume" {
			t.Errorf("publish verb = %q, want resume", pubs[0].PublishVerb)
		}
		if got := argInt(t, pubs[0].PublishArgs, "task_id"); got != taskN {
			t.Errorf("resume args task_id = %d, want %d", got, taskN)
		}
		if got := argInt(t, pubs[0].PublishArgs, "feedback_request_id"); got != fbM {
			t.Errorf("resume args feedback_request_id = %d, want %d", got, fbM)
		}

		closeA := exactlyOneExecute(t, actions, "task_close")
		if got := argInt(t, closeA.Args, "task_id"); got != answerTask {
			t.Errorf("task_close task_id = %d, want the answer task %d", got, answerTask)
		}
		if r := argStr(t, closeA.Args, "reason"); r == "" {
			t.Error("task_close reason must be non-empty")
		}

		rec := exactlyOneExecute(t, actions, "record_orchestration")
		if got := argStr(t, rec.Args, "rule"); got != "feedback_resume" {
			t.Errorf("record rule = %q, want feedback_resume", got)
		}
		if _, skipped := subMap(t, rec.Args, "payload")["skipped"]; skipped {
			t.Error("record payload must NOT carry a skipped reason when a claim was active")
		}
	})

	t.Run("no active claim: skip publish, still close answer, record skipped", func(t *testing.T) {
		f := orch.Facts{
			Task:                orch.TaskFacts{ID: taskN},
			ActiveClaimWorkerID: "", // worker died while parked; LWT fired
			Orchestrations:      []orch.Orchestration{priorR1},
		}
		actions := orch.Evaluate(ev, f, orch.Config{})

		if pubs := publishesOf(actions); len(pubs) != 0 {
			t.Fatalf("no active claim must skip the resume publish; got %s", dump(actions))
		}
		closeA := exactlyOneExecute(t, actions, "task_close")
		if got := argInt(t, closeA.Args, "task_id"); got != answerTask {
			t.Errorf("task_close task_id = %d, want %d (answer task still retired)", got, answerTask)
		}
		rec := exactlyOneExecute(t, actions, "record_orchestration")
		if got := subMap(t, rec.Args, "payload")["skipped"]; got != "no_active_claim" {
			t.Errorf("record payload skipped = %v, want \"no_active_claim\"", got)
		}
	})

	t.Run("no answer task found: publish only", func(t *testing.T) {
		f := orch.Facts{
			Task:                orch.TaskFacts{ID: taskN},
			ActiveClaimWorkerID: holder,
			Orchestrations:      nil, // R1 never ran / pre-orchestrator
		}
		actions := orch.Evaluate(ev, f, orch.Config{})
		if len(publishesOf(actions)) != 1 {
			t.Fatalf("want the resume publish; got %s", dump(actions))
		}
		if len(executesOf(actions, "task_close")) != 0 {
			t.Errorf("no answer task to close; task_close must not fire: %s", dump(actions))
		}
	})

	t.Run("dedup: feedback_resume already recorded for M -> no actions", func(t *testing.T) {
		f := orch.Facts{
			Task:                orch.TaskFacts{ID: taskN},
			ActiveClaimWorkerID: holder,
			Orchestrations: []orch.Orchestration{
				priorR1,
				{Rule: "feedback_resume", FeedbackRequestID: fbM, TaskID: taskN},
			},
		}
		if actions := orch.Evaluate(ev, f, orch.Config{}); len(actions) != 0 {
			t.Fatalf("R2 must be a no-op once feedback_resume is recorded for M; got %s", dump(actions))
		}
	})
}

// ---- R3: delivery task on done_local -----------------------------------------

func TestEvaluate_R3_DeliveryTask(t *testing.T) {
	const (
		eventID = int64(300)
		taskN   = int64(9)
		slug    = "acme"
		title   = "ship the widget"
		summary = "merged to main, tag v1.2"
	)
	mkEvent := func() orch.Event {
		return orch.Event{
			ID:      eventID,
			TaskID:  taskN,
			Type:    "done_local",
			Payload: fbPayload("summary", summary, "worker_id", "w1"),
		}
	}

	t.Run("dashboard delivery: one Deliver task + record", func(t *testing.T) {
		f := orch.Facts{Task: orch.TaskFacts{ID: taskN, ProjectSlug: slug, ProjectDelivery: "dashboard", Title: title}}
		actions := orch.Evaluate(mkEvent(), f, orch.Config{})

		create := exactlyOneExecute(t, actions, "create_task")
		if got := argStr(t, create.Args, "project"); got != slug {
			t.Errorf("delivery task project = %q, want %q", got, slug)
		}
		if got := argInt(t, create.Args, "parent_id"); got != taskN {
			t.Errorf("delivery task parent_id = %d, want %d", got, taskN)
		}
		if got := argStr(t, create.Args, "assignee_type"); got != "human" {
			t.Errorf("delivery task assignee_type = %q, want human", got)
		}
		wantTitle := fmt.Sprintf("Deliver #%d: %s", taskN, title)
		if got := argStr(t, create.Args, "title"); got != wantTitle {
			t.Errorf("delivery task title = %q, want %q", got, wantTitle)
		}
		body := argStr(t, create.Args, "body")
		for _, want := range []string{summary, "dashboard"} {
			if !strings.Contains(body, want) {
				t.Errorf("delivery task body missing %q; body=%q", want, body)
			}
		}
		rec := exactlyOneExecute(t, actions, "record_orchestration")
		if got := argStr(t, rec.Args, "rule"); got != "delivery_task" {
			t.Errorf("record rule = %q, want delivery_task", got)
		}
		if got := argInt(t, rec.Args, "task_id"); got != taskN {
			t.Errorf("record task_id = %d, want %d", got, taskN)
		}
	})

	t.Run("console delivery: no task (operator delivers as part of the work)", func(t *testing.T) {
		f := orch.Facts{Task: orch.TaskFacts{ID: taskN, ProjectSlug: slug, ProjectDelivery: "console", Title: title}}
		if actions := executesOf(orch.Evaluate(mkEvent(), f, orch.Config{}), "create_task"); len(actions) != 0 {
			t.Fatalf("console delivery must skip R3; got %s", dump(actions))
		}
	})

	t.Run("auto delivery still produces a human task (until step 8)", func(t *testing.T) {
		f := orch.Facts{Task: orch.TaskFacts{ID: taskN, ProjectSlug: slug, ProjectDelivery: "auto", Title: title}}
		if n := len(executesOf(orch.Evaluate(mkEvent(), f, orch.Config{}), "create_task")); n != 1 {
			t.Fatalf("auto delivery must still create one delivery task; got %d", n)
		}
	})

	t.Run("dedup: delivery_task already recorded on N -> no actions", func(t *testing.T) {
		f := orch.Facts{
			Task:           orch.TaskFacts{ID: taskN, ProjectSlug: slug, ProjectDelivery: "dashboard", Title: title},
			Orchestrations: []orch.Orchestration{{Rule: "delivery_task", TaskID: taskN}},
		}
		if actions := orch.Evaluate(mkEvent(), f, orch.Config{}); len(executesOf(actions, "create_task")) != 0 {
			t.Fatalf("R3 must dedup on task-scoped delivery_task; got %s", dump(actions))
		}
	})
}

// ---- R4: block on unmet deps (dependency_added, released) ---------------------

func TestEvaluate_R4_BlockOnUnmetDeps(t *testing.T) {
	const taskN = int64(11)

	for _, evType := range []string{"dependency_added", "released"} {
		evType := evType
		t.Run(evType+": ready + unmet dep -> block", func(t *testing.T) {
			ev := orch.Event{ID: 400, TaskID: taskN, Type: evType, Payload: fbPayload("depends_on_task_id", float64(3))}
			f := orch.Facts{Task: orch.TaskFacts{ID: taskN, Status: "ready", HasUnmetDep: true}}
			block := exactlyOneExecute(t, orch.Evaluate(ev, f, orch.Config{}), "task_block")
			if got := argInt(t, block.Args, "task_id"); got != taskN {
				t.Errorf("task_block task_id = %d, want %d", got, taskN)
			}
		})
		t.Run(evType+": ready + all deps met -> no-op", func(t *testing.T) {
			ev := orch.Event{ID: 401, TaskID: taskN, Type: evType}
			f := orch.Facts{Task: orch.TaskFacts{ID: taskN, Status: "ready", HasUnmetDep: false}}
			if actions := orch.Evaluate(ev, f, orch.Config{}); len(actions) != 0 {
				t.Fatalf("met deps must be a no-op; got %s", dump(actions))
			}
		})
		t.Run(evType+": holding is triage's lane -> no-op even with unmet dep", func(t *testing.T) {
			ev := orch.Event{ID: 402, TaskID: taskN, Type: evType}
			f := orch.Facts{Task: orch.TaskFacts{ID: taskN, Status: "holding", HasUnmetDep: true}}
			if actions := orch.Evaluate(ev, f, orch.Config{}); len(actions) != 0 {
				t.Fatalf("only ready->blocked; holding must be untouched; got %s", dump(actions))
			}
		})
	}
}

// ---- R5: unblock on satisfied deps (done_local, status_changed to delivered/closed)

func TestEvaluate_R5_UnblockOnSatisfiedDeps(t *testing.T) {
	const depTask = int64(20)
	dependents := func(ds ...orch.DependentTask) orch.Facts {
		return orch.Facts{Task: orch.TaskFacts{ID: depTask}, Dependents: ds}
	}

	t.Run("done_local: blocked dependent with all deps satisfied -> unblock", func(t *testing.T) {
		ev := orch.Event{ID: 500, TaskID: depTask, Type: "done_local", Payload: fbPayload("summary", "x", "worker_id", "w")}
		f := dependents(orch.DependentTask{ID: 30, Status: "blocked", AllDepsSatisfied: true})
		unblock := exactlyOneExecute(t, orch.Evaluate(ev, f, orch.Config{}), "task_unblock")
		if got := argInt(t, unblock.Args, "task_id"); got != 30 {
			t.Errorf("task_unblock task_id = %d, want 30", got)
		}
	})

	t.Run("done_local: partial deps (one still unsatisfied) -> no-op", func(t *testing.T) {
		ev := orch.Event{ID: 501, TaskID: depTask, Type: "done_local", Payload: fbPayload("summary", "x", "worker_id", "w")}
		f := dependents(orch.DependentTask{ID: 30, Status: "blocked", AllDepsSatisfied: false})
		if actions := orch.Evaluate(ev, f, orch.Config{}); len(actions) != 0 {
			t.Fatalf("partial deps must not unblock; got %s", dump(actions))
		}
	})

	t.Run("dependent not blocked -> skip (only blocked->ready)", func(t *testing.T) {
		ev := orch.Event{ID: 502, TaskID: depTask, Type: "done_local", Payload: fbPayload("summary", "x", "worker_id", "w")}
		f := dependents(orch.DependentTask{ID: 30, Status: "ready", AllDepsSatisfied: true})
		if actions := orch.Evaluate(ev, f, orch.Config{}); len(actions) != 0 {
			t.Fatalf("a non-blocked dependent must be skipped; got %s", dump(actions))
		}
	})

	t.Run("status_changed to closed -> unblock; to ready -> no-op", func(t *testing.T) {
		f := dependents(orch.DependentTask{ID: 30, Status: "blocked", AllDepsSatisfied: true})

		closed := orch.Event{ID: 503, TaskID: depTask, Type: "status_changed", Payload: fbPayload("from", "ready", "to", "closed")}
		if n := len(executesOf(orch.Evaluate(closed, f, orch.Config{}), "task_unblock")); n != 1 {
			t.Fatalf("status_changed to closed must unblock a satisfied dependent; got %d", n)
		}
		delivered := orch.Event{ID: 504, TaskID: depTask, Type: "status_changed", Payload: fbPayload("from", "done_locally", "to", "delivered")}
		if n := len(executesOf(orch.Evaluate(delivered, f, orch.Config{}), "task_unblock")); n != 1 {
			t.Fatalf("status_changed to delivered must unblock; got %d", n)
		}
		toReady := orch.Event{ID: 505, TaskID: depTask, Type: "status_changed", Payload: fbPayload("from", "blocked", "to", "ready")}
		if actions := orch.Evaluate(toReady, f, orch.Config{}); len(actions) != 0 {
			t.Fatalf("status_changed to a non-terminal status must not unblock; got %s", dump(actions))
		}
	})

	t.Run("multiple satisfied blocked dependents -> one unblock each", func(t *testing.T) {
		ev := orch.Event{ID: 506, TaskID: depTask, Type: "done_local", Payload: fbPayload("summary", "x", "worker_id", "w")}
		f := dependents(
			orch.DependentTask{ID: 30, Status: "blocked", AllDepsSatisfied: true},
			orch.DependentTask{ID: 31, Status: "blocked", AllDepsSatisfied: true},
			orch.DependentTask{ID: 32, Status: "blocked", AllDepsSatisfied: false}, // not yet
		)
		unblocks := executesOf(orch.Evaluate(ev, f, orch.Config{}), "task_unblock")
		if len(unblocks) != 2 {
			t.Fatalf("want 2 unblocks (30, 31); got %d: %s", len(unblocks), dump(unblocks))
		}
	})
}

// ---- R3 + R5 co-fire on one done_local event ---------------------------------

func TestEvaluate_DoneLocal_FiresR3AndR5Together(t *testing.T) {
	const taskN = int64(40)
	ev := orch.Event{ID: 600, TaskID: taskN, Type: "done_local", Payload: fbPayload("summary", "done", "worker_id", "w")}
	f := orch.Facts{
		Task:       orch.TaskFacts{ID: taskN, ProjectSlug: "acme", ProjectDelivery: "dashboard", Title: "t"},
		Dependents: []orch.DependentTask{{ID: 50, Status: "blocked", AllDepsSatisfied: true}},
	}
	actions := orch.Evaluate(ev, f, orch.Config{})
	if n := len(executesOf(actions, "create_task")); n != 1 {
		t.Errorf("R3 delivery task missing on done_local; create_task count = %d", n)
	}
	if n := len(executesOf(actions, "task_unblock")); n != 1 {
		t.Errorf("R5 unblock missing on done_local; task_unblock count = %d", n)
	}
}

// ---- R6: claim expiry (tick) -------------------------------------------------

func TestEvaluate_R6_ClaimExpiry(t *testing.T) {
	tick := orch.Event{Type: orch.EventTick, Now: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)}

	t.Run("releases expired claimed/in_progress; exempts needs_feedback", func(t *testing.T) {
		f := orch.Facts{ExpiredClaims: []orch.ExpiredClaim{
			{TaskID: 1, WorkerID: "wa", Status: "claimed"},
			{TaskID: 2, WorkerID: "wb", Status: "in_progress"},
			{TaskID: 3, WorkerID: "wc", Status: "needs_feedback"}, // parked, exempt
		}}
		actions := orch.Evaluate(tick, f, orch.Config{})
		releases := executesOf(actions, "task_release")
		if len(releases) != 2 {
			t.Fatalf("want 2 releases (tasks 1,2); got %d: %s", len(releases), dump(releases))
		}
		for _, r := range releases {
			id := argInt(t, r.Args, "task_id")
			if id == 3 {
				t.Error("needs_feedback claim must be exempt from the sweep")
			}
			if got := argStr(t, r.Args, "reason"); got != "claim expired (orchestrator sweep)" {
				t.Errorf("release reason = %q, want the pinned sweep reason", got)
			}
			if wid := argStr(t, r.Args, "worker_id"); wid == "" {
				t.Error("release must carry the holder's worker_id from the claim row")
			}
		}
	})

	t.Run("no expired claims -> no-op", func(t *testing.T) {
		if actions := orch.Evaluate(tick, orch.Facts{}, orch.Config{}); len(actions) != 0 {
			t.Fatalf("empty expiry facts must be a no-op; got %s", dump(actions))
		}
	})
}

// ---- R7: morning brief (tick) ------------------------------------------------

func TestEvaluate_R7_MorningBrief(t *testing.T) {
	const briefSlug = "internal"
	counts := []orch.ProjectCounts{{ProjectSlug: "acme", Ready: 3, Blocked: 1, NeedsFeedback: 2, DoneLocally: 0, OpenFeedback: 1}}
	cfg := orch.Config{BriefProject: briefSlug, BriefHour: 7}
	at := func(h int) time.Time { return time.Date(2026, 7, 11, h, 30, 0, 0, time.Local) }

	t.Run("enabled, at/after hour, not yet created -> one dated brief task", func(t *testing.T) {
		ev := orch.Event{Type: orch.EventTick, Now: at(7)}
		f := orch.Facts{BriefExists: false, BriefCounts: counts}
		create := exactlyOneExecute(t, orch.Evaluate(ev, f, cfg), "create_task")
		if got := argStr(t, create.Args, "project"); got != briefSlug {
			t.Errorf("brief project = %q, want %q", got, briefSlug)
		}
		if got := argStr(t, create.Args, "assignee_type"); got != "human" {
			t.Errorf("brief assignee_type = %q, want human", got)
		}
		wantTitle := "Morning brief " + ev.Now.Format("2006-01-02")
		if got := argStr(t, create.Args, "title"); got != wantTitle {
			t.Errorf("brief title = %q, want %q", got, wantTitle)
		}
		body := argStr(t, create.Args, "body")
		// deterministic snapshot: the acme counts must be rendered somewhere.
		for _, want := range []string{"acme", "3", "1", "2"} {
			if !strings.Contains(body, want) {
				t.Errorf("brief body missing %q; body=%q", want, body)
			}
		}
	})

	t.Run("disabled (BriefProject unset) -> no-op", func(t *testing.T) {
		ev := orch.Event{Type: orch.EventTick, Now: at(9)}
		f := orch.Facts{BriefExists: false, BriefCounts: counts}
		if actions := orch.Evaluate(ev, f, orch.Config{BriefHour: 7}); len(actions) != 0 {
			t.Fatalf("unset BriefProject must disable R7; got %s", dump(actions))
		}
	})

	t.Run("before hour -> no-op", func(t *testing.T) {
		ev := orch.Event{Type: orch.EventTick, Now: at(6)}
		f := orch.Facts{BriefExists: false, BriefCounts: counts}
		if actions := orch.Evaluate(ev, f, cfg); len(actions) != 0 {
			t.Fatalf("before BriefHour must be a no-op; got %s", dump(actions))
		}
	})

	t.Run("already created today -> no-op", func(t *testing.T) {
		ev := orch.Event{Type: orch.EventTick, Now: at(8)}
		f := orch.Facts{BriefExists: true, BriefCounts: counts}
		if actions := orch.Evaluate(ev, f, cfg); len(actions) != 0 {
			t.Fatalf("dated brief already exists -> no-op; got %s", dump(actions))
		}
	})
}

// ---- non-firing / vocabulary edges -------------------------------------------

func TestEvaluate_NoRuleFires(t *testing.T) {
	f := orch.Facts{
		Task:           orch.TaskFacts{ID: 1, ProjectSlug: "acme", ProjectDelivery: "dashboard", Status: "ready", HasUnmetDep: true},
		Dependents:     []orch.DependentTask{{ID: 2, Status: "blocked", AllDepsSatisfied: true}},
		ExpiredClaims:  []orch.ExpiredClaim{{TaskID: 3, WorkerID: "w", Status: "claimed"}},
		Orchestrations: []orch.Orchestration{{Rule: "feedback_task", FeedbackRequestID: 9, TaskID: 1}},
	}

	t.Run("orchestrated event matches no rule (no loops)", func(t *testing.T) {
		ev := orch.Event{ID: 700, TaskID: 1, Type: "orchestrated", Payload: fbPayload("rule", "feedback_task")}
		if actions := orch.Evaluate(ev, f, orch.Config{BriefProject: "x", BriefHour: 0}); len(actions) != 0 {
			t.Fatalf("'orchestrated' must fire nothing (dedup/cursor read only); got %s", dump(actions))
		}
	})

	t.Run("unknown event type -> no actions", func(t *testing.T) {
		ev := orch.Event{ID: 701, TaskID: 1, Type: "session", Payload: fbPayload("session_id", "abc")}
		if actions := orch.Evaluate(ev, f, orch.Config{}); len(actions) != 0 {
			t.Fatalf("unknown event type must be a no-op; got %s", dump(actions))
		}
	})

	t.Run("claimed event -> no actions", func(t *testing.T) {
		ev := orch.Event{ID: 702, TaskID: 1, Type: "claimed", Payload: fbPayload("worker_id", "w")}
		if actions := orch.Evaluate(ev, f, orch.Config{}); len(actions) != 0 {
			t.Fatalf("'claimed' must not fire a rule; got %s", dump(actions))
		}
	})
}
