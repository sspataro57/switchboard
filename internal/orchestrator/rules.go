// Package orchestrator is the deterministic spine loop (SPEC
// 05-orchestrator-loop / SWT-5): a Postgres event drain + cron ticker whose
// rules are PURE functions of (event, facts, config) — invariant 7. This file
// contains only the pure core: no pgx, no paho, no clock reads except ev.Now,
// no LLM, ever. SQL lives in facts.go, mutation in apply.go.
package orchestrator

import (
	"fmt"
	"strings"
	"time"
)

// EventTick is the synthetic event the cron ticker evaluates (R6, R7).
const EventTick = "tick"

// Action kinds.
const (
	ActionExecute = "execute" // a registered tool call via Executor.Execute
	ActionPublish = "publish" // a fleet command via PublishCommand
)

// Event is one task_events row (or a synthetic tick).
type Event struct {
	ID      int64          // task_events.id (trigger_event_id); 0 for a tick
	TaskID  int64          // the triggering task
	Type    string         // event_type, or EventTick
	Payload map[string]any // parsed payload (JSON numbers are float64)
	Now     time.Time      // evaluation clock (tick rules only)
}

// Action is a typed intent the applier executes.
type Action struct {
	Kind        string
	Tool        string         // ActionExecute
	Args        map[string]any // ActionExecute
	WorkerID    string         // ActionPublish
	PublishVerb string         // ActionPublish
	PublishArgs map[string]any // ActionPublish
}

// Config is the orchestrator's static configuration.
type Config struct {
	BriefProject string // project slug for R7; "" disables the brief
	BriefHour    int    // local hour at/after which the brief fires
}

// Facts is the read-only world snapshot a loader gathered for one event.
type Facts struct {
	Task                TaskFacts
	Orchestrations      []Orchestration
	ActiveClaimWorkerID string
	Dependents          []DependentTask
	ExpiredClaims       []ExpiredClaim
	BriefExists         bool
	BriefCounts         []ProjectCounts
}

type TaskFacts struct {
	ID              int64
	ProjectSlug     string
	ProjectDelivery string
	Title           string
	Status          string
	HasUnmetDep     bool
}

// Orchestration is one prior 'orchestrated' decision row (dedup facts).
type Orchestration struct {
	Rule              string
	FeedbackRequestID int64
	CreatedTaskID     int64
	TaskID            int64
}

type DependentTask struct {
	ID               int64
	Status           string
	AllDepsSatisfied bool
}

type ExpiredClaim struct {
	TaskID   int64
	WorkerID string
	Status   string
}

type ProjectCounts struct {
	ProjectSlug   string
	Ready         int
	Blocked       int
	NeedsFeedback int
	DoneLocally   int
	OpenFeedback  int
}

// SweepReason is R6's pinned release reason.
const SweepReason = "claim expired (orchestrator sweep)"

// Evaluate is the pure rule core. 'orchestrated' and unknown event types
// return nil — no rule fires on the orchestrator's own records, no loops.
func Evaluate(ev Event, f Facts, cfg Config) []Action {
	switch ev.Type {
	case "feedback_requested":
		return ruleFeedbackTask(ev, f)
	case "feedback_answered":
		return ruleFeedbackResume(ev, f)
	case "done_local":
		return append(ruleDeliveryTask(ev, f), ruleUnblockDependents(f)...)
	case "delivery_sent":
		return ruleDeliveryLifecycle(ev, f)
	case "dependency_added", "released":
		return ruleBlockOnUnmetDeps(f)
	case "status_changed":
		to, _ := ev.Payload["to"].(string)
		if to == "delivered" || to == "closed" {
			return ruleUnblockDependents(f)
		}
		return nil
	case EventTick:
		return append(ruleClaimExpiry(f), ruleMorningBrief(ev, f, cfg)...)
	default:
		return nil
	}
}

func payloadInt(p map[string]any, key string) int64 {
	switch v := p[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

func payloadStr(p map[string]any, key string) string {
	s, _ := p[key].(string)
	return s
}

func orchestrated(f Facts, rule string, match func(Orchestration) bool) bool {
	for _, o := range f.Orchestrations {
		if o.Rule == rule && match(o) {
			return true
		}
	}
	return false
}

// R1 — feedback task on feedback_requested.
func ruleFeedbackTask(ev Event, f Facts) []Action {
	m := payloadInt(ev.Payload, "feedback_request_id")
	if m == 0 {
		return nil
	}
	if orchestrated(f, "feedback_task", func(o Orchestration) bool { return o.FeedbackRequestID == m }) {
		return nil
	}
	question := payloadStr(ev.Payload, "question")
	body := fmt.Sprintf(
		"Task #%d asked:\n\n%s\n\nAnswer with:\n  opsctl answer-feedback --id %d --answer \"...\"",
		ev.TaskID, question, m)
	return []Action{
		{Kind: ActionExecute, Tool: "create_task", Args: map[string]any{
			"project":       f.Task.ProjectSlug,
			"parent_id":     ev.TaskID,
			"assignee_type": "human",
			"title":         fmt.Sprintf("Answer feedback #%d on task #%d", m, ev.TaskID),
			"body":          body,
		}},
		{Kind: ActionExecute, Tool: "record_orchestration", Args: map[string]any{
			"task_id":          ev.TaskID,
			"rule":             "feedback_task",
			"trigger_event_id": ev.ID,
			"payload":          map[string]any{"feedback_request_id": m},
		}},
	}
}

// R2 — resume on answer + close the R1 answer task.
func ruleFeedbackResume(ev Event, f Facts) []Action {
	m := payloadInt(ev.Payload, "feedback_request_id")
	if m == 0 {
		return nil
	}
	if orchestrated(f, "feedback_resume", func(o Orchestration) bool { return o.FeedbackRequestID == m }) {
		return nil
	}

	var actions []Action
	recPayload := map[string]any{"feedback_request_id": m}

	if f.ActiveClaimWorkerID != "" {
		actions = append(actions, Action{
			Kind:        ActionPublish,
			WorkerID:    f.ActiveClaimWorkerID,
			PublishVerb: "resume",
			PublishArgs: map[string]any{"task_id": ev.TaskID, "feedback_request_id": m},
		})
	} else {
		// worker died while parked (LWT fired) — the task stays needs_feedback
		// for manual dispatch; the skip carries state replay cannot infer.
		recPayload["skipped"] = "no_active_claim"
	}

	for _, o := range f.Orchestrations {
		if o.Rule == "feedback_task" && o.FeedbackRequestID == m && o.CreatedTaskID != 0 {
			actions = append(actions, Action{
				Kind: ActionExecute, Tool: "task_close", Args: map[string]any{
					"task_id": o.CreatedTaskID,
					"reason":  "feedback answered",
				},
			})
			break
		}
	}

	actions = append(actions, Action{
		Kind: ActionExecute, Tool: "record_orchestration", Args: map[string]any{
			"task_id":          ev.TaskID,
			"rule":             "feedback_resume",
			"trigger_event_id": ev.ID,
			"payload":          recPayload,
		},
	})
	return actions
}

// R3 — delivery task on done_local (console projects skip).
func ruleDeliveryTask(ev Event, f Facts) []Action {
	// console: operator delivers as part of the work; "": no project facts.
	if f.Task.ProjectDelivery == "console" || f.Task.ProjectDelivery == "" {
		return nil
	}
	if orchestrated(f, "delivery_task", func(o Orchestration) bool { return o.TaskID == ev.TaskID }) {
		return nil
	}
	summary := payloadStr(ev.Payload, "summary")
	body := fmt.Sprintf("Task #%d finished locally.\n\nSummary: %s\n\nProject delivery mode: %s",
		ev.TaskID, summary, f.Task.ProjectDelivery)
	return []Action{
		{Kind: ActionExecute, Tool: "create_task", Args: map[string]any{
			"project":       f.Task.ProjectSlug,
			"parent_id":     ev.TaskID,
			"assignee_type": "human",
			"title":         fmt.Sprintf("Deliver #%d: %s", ev.TaskID, f.Task.Title),
			"body":          body,
		}},
		{Kind: ActionExecute, Tool: "record_orchestration", Args: map[string]any{
			"task_id":          ev.TaskID,
			"rule":             "delivery_task",
			"trigger_event_id": ev.ID,
			"payload":          map[string]any{"delivery": f.Task.ProjectDelivery},
		}},
	}
}

// R8 — delivery lifecycle on delivery_sent (SWT-8): the delivered work task
// flips done_locally -> delivered, R3's Deliver task is retired, and the
// decision is recorded (the dedup key).
func ruleDeliveryLifecycle(ev Event, f Facts) []Action {
	if orchestrated(f, "delivery_lifecycle", func(o Orchestration) bool { return o.TaskID == ev.TaskID }) {
		return nil
	}
	actions := []Action{{Kind: ActionExecute, Tool: "task_mark_delivered", Args: map[string]any{
		"task_id": ev.TaskID,
		"reason":  "delivery sent",
	}}}
	for _, o := range f.Orchestrations {
		if o.Rule == "delivery_task" && o.TaskID == ev.TaskID && o.CreatedTaskID != 0 {
			actions = append(actions, Action{Kind: ActionExecute, Tool: "task_close", Args: map[string]any{
				"task_id": o.CreatedTaskID,
				"reason":  "delivery sent",
			}})
			break
		}
	}
	actions = append(actions, Action{Kind: ActionExecute, Tool: "record_orchestration", Args: map[string]any{
		"task_id":          ev.TaskID,
		"rule":             "delivery_lifecycle",
		"trigger_event_id": ev.ID,
		"payload":          map[string]any{"delivery_id": payloadInt(ev.Payload, "delivery_id")},
	}})
	return actions
}

// R4 — block on unmet deps: only ready -> blocked.
func ruleBlockOnUnmetDeps(f Facts) []Action {
	if f.Task.Status != "ready" || !f.Task.HasUnmetDep {
		return nil
	}
	return []Action{{Kind: ActionExecute, Tool: "task_block", Args: map[string]any{
		"task_id": f.Task.ID,
		"reason":  "unmet dependency",
	}}}
}

// R5 — unblock dependents whose deps are now all satisfied.
func ruleUnblockDependents(f Facts) []Action {
	var actions []Action
	for _, d := range f.Dependents {
		if d.Status == "blocked" && d.AllDepsSatisfied {
			actions = append(actions, Action{Kind: ActionExecute, Tool: "task_unblock", Args: map[string]any{
				"task_id": d.ID,
				"reason":  "dependencies satisfied",
			}})
		}
	}
	return actions
}

// R6 — claim expiry sweep. needs_feedback is deliberately exempt: a parked
// worker awaiting a human answer is not a crashed worker.
func ruleClaimExpiry(f Facts) []Action {
	var actions []Action
	for _, c := range f.ExpiredClaims {
		if c.Status != "claimed" && c.Status != "in_progress" {
			continue
		}
		actions = append(actions, Action{Kind: ActionExecute, Tool: "task_release", Args: map[string]any{
			"task_id":   c.TaskID,
			"worker_id": c.WorkerID,
			"reason":    SweepReason,
		}})
	}
	return actions
}

// R7 — morning brief: deterministic SQL snapshot rendered by Go, no LLM ever.
func ruleMorningBrief(ev Event, f Facts, cfg Config) []Action {
	if cfg.BriefProject == "" || ev.Now.Hour() < cfg.BriefHour || f.BriefExists {
		return nil
	}
	return []Action{{Kind: ActionExecute, Tool: "create_task", Args: map[string]any{
		"project":       cfg.BriefProject,
		"assignee_type": "human",
		"title":         "Morning brief " + ev.Now.Format("2006-01-02"),
		"body":          renderBrief(f.BriefCounts),
	}}}
}

func renderBrief(counts []ProjectCounts) string {
	if len(counts) == 0 {
		return "No active projects."
	}
	var b strings.Builder
	b.WriteString("Task snapshot by project:\n\n")
	for _, c := range counts {
		fmt.Fprintf(&b, "- %s: ready %d, blocked %d, needs_feedback %d, done_locally %d, open feedback %d\n",
			c.ProjectSlug, c.Ready, c.Blocked, c.NeedsFeedback, c.DoneLocally, c.OpenFeedback)
	}
	return b.String()
}
