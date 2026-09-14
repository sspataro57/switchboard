package dashboard

import (
	"strconv"
	"strings"
)

// SWT-52 (board-status-lights) D1/D2: the light before every /tasks row id.
// lightFor is a PURE function of the row's status plus facts from a separate
// read ((*Server).boardLightFacts): no I/O, no clock — "today" and "stale" are
// decided on the Postgres clock in BoardTimeZone and arrive here as booleans.
// The template never branches on the status; it renders Light.Class and
// Light.Label only.

// light is one row's light: Class is one of done, working, stale, input, next,
// none (the six CSS classes); Label is the accessible text (aria-label, title).
type light struct{ Class, Label string }

// lightFacts are what boardLightFacts reads beside the status.
type lightFacts struct {
	// OpenDismissalCode is the reason_code of a closed task's newest OPEN
	// dismissal (reopened_at IS NULL), "" when none.
	OpenDismissalCode string
	// ClosedToday: COALESCE(closed_at, updated_at) is at or after today's local
	// midnight (boardDayStart).
	ClosedToday bool
	// State is the session state (working | needs_input | ""), and StateAt its
	// last signal as "YYYY-MM-DD HH:MM" in BoardTimeZone.
	State, StateAt string
	// StateToday: working_state_at is at or after today's local midnight
	// (boardDayStart, the DB clock). The fresh labels show HH:MM only then; an
	// earlier day's signal shows the full date (D1 amendment, 2026-09-14).
	StateToday bool
	// Stale: a working state older than tools.WorkingLease. QueueHead: the first
	// eligible ready task of its queue, whose name is Lane (D2).
	Stale, QueueHead bool
	Lane             string
}

// lightFor is D1's table, top to bottom; the first match wins. The session
// state counts only in holding, ready or blocked — the statuses task_signal
// accepts; in every other status the status row wins.
func lightFor(status string, f lightFacts) light {
	switch status {
	case "closed":
		if f.OpenDismissalCode != "" {
			return light{"none", "dismissed (" + f.OpenDismissalCode + ")"}
		}
		if f.ClosedToday {
			return light{"done", "done today"}
		}
		return light{"done", "done"}
	case "needs_feedback":
		return light{"input", "waiting on your input: worker parked on a question"}
	case "done_locally":
		return light{"done", "done locally; delivery pending"}
	case "delivered":
		return light{"done", "delivered"}
	case "claimed":
		return light{"working", "in progress (claimed)"}
	case "in_progress":
		return light{"working", "in progress"}
	case "pr_open":
		return light{"working", "in progress: PR open"}
	case "awaiting_ci":
		return light{"working", "in progress: awaiting CI"}
	case "awaiting_merge":
		return light{"working", "in progress: awaiting merge"}
	case "holding", "ready", "blocked":
		switch {
		case f.State == "needs_input": // never stale: a waiting session cannot re-signal
			return light{"input", "waiting on your input (a session, since " + signalStamp(f) + ")"}
		case f.State == "working" && f.Stale:
			return light{"stale", "in progress? no session signal since " + f.StateAt}
		case f.State == "working":
			return light{"working", "in progress (a session, last signal " + signalStamp(f) + ")"}
		}
		switch {
		case status == "ready" && f.QueueHead:
			return light{"next", "next in queue (" + f.Lane + ")"}
		case status == "holding":
			return light{"none", "holding (review lane; not queued)"}
		case status == "blocked":
			return light{"none", "blocked on a dependency"}
		}
		return light{"none", "ready, queued"}
	}
	return light{"none", status}
}

// signalStamp is the time a fresh session label shows (D1 amendment,
// 2026-09-14): HH:MM for a signal since today's local midnight, else the full
// "YYYY-MM-DD HH:MM" — needs_input never goes stale, so a red set on an earlier
// day must not read as today's.
func signalStamp(f lightFacts) string {
	if f.StateToday {
		return clockPart(f.StateAt)
	}
	return f.StateAt
}

// clockPart is the HH:MM of a "YYYY-MM-DD HH:MM" stamp.
func clockPart(stamp string) string {
	if i := strings.LastIndexByte(stamp, ' '); i >= 0 {
		return stamp[i+1:]
	}
	return stamp
}

// headCandidate is one ready task, handed to pickQueueHeads in
// tools.TaskQueueOrder. Subproject is carried but is NOT part of any lane key
// (D2 amendment, 2026-09-14); the unit tests use it to prove it splits nothing.
type headCandidate struct {
	ID           int64
	AssigneeType string // "human" | "claude"
	ProjectID    int64
	ProjectSlug  string
	Client       string
	Subproject   string
}

// pickQueueHeads is D2: the FIRST eligible candidate per queue, in input order,
// mapped to its queue's name. Queues: the human lane is one per project
// (h/<project_id>, named by the slug); the claude lane is ONE per client over
// all its claude ready tasks, whatever their subproject (c/<client>, named
// "<client> console") — exactly what task_get_next(client) picks, since an
// empty subproject there is no filter. A subproject console may therefore see
// no blue for its own next task: an accepted under-report, because a missing
// blue is safer than a false "next" (D2 amendment, 2026-09-14). eligible is
// the caller's lightFor check, so a red or yellow task never heads a queue.
func pickQueueHeads(cands []headCandidate, eligible func(int64) bool) map[int64]string {
	heads := map[int64]string{}
	done := map[string]bool{}
	for _, c := range cands {
		var key, name string
		switch c.AssigneeType {
		case "human":
			key, name = "h/"+strconv.FormatInt(c.ProjectID, 10), c.ProjectSlug
		case "claude":
			key, name = "c/"+c.Client, c.Client+" console"
		default:
			continue
		}
		if done[key] || !eligible(c.ID) {
			continue
		}
		done[key] = true
		heads[c.ID] = name
	}
	return heads
}
