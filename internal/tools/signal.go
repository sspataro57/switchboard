package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// task_signal (SWT-52, board-status-lights): a Claude session's STATE SIGNAL on
// a HUMAN task — working, needs_input (waiting on Salvador), or clear. It is an
// annotation on the tasks row (0033: working_state + working_state_at; 0036:
// working_session), not a status: nothing routes on it, no claim backs it, and
// the orchestrator never reads it. It records the state and the signalling
// session's self-reported NAME (SWT-56), so the board says where to reply —
// Salvador answers in that session's own console, never through switchboard
// (SPEC D4, D14). The name is data, never authority: nothing verifies it.
//
// humanOnly in policy (D7): no spine caller sets a session state. The handler
// refuses a non-human task for EVERY caller: a claude task's in-progress signal
// is its claim, and a marker there would be a second signal task_get_next
// cannot see. Only this file, closeTransition (close.go: a real close and every
// reopen clear them) and task_claim (claim.go: a claim clears them) write the
// columns (criterion 19, TestWorkingState_OnlySignalAndCloseWriteIt).

// WorkingLease is how long a 'working' signal stays fresh (D11). Older, the
// board shows the task as a stale ring — at READ time; nothing writes on
// staleness. It equals ClaimTTL but is a separate lease, so changing the claim
// TTL never moves the board's staleness. needs_input never goes stale.
const WorkingLease = 2 * time.Hour

// signalStates is D7's state set, spelled ONCE. "clear" is a verb, not a stored
// state: it NULLs both columns.
var signalStates = []string{"working", "needs_input", "clear"}

// SignalStates returns a copy of task_signal's states, in order: the one source
// of the MCP schema's enum (criterion 23, TestTaskSignalSchema). A copy, so no
// caller can rewrite the validator's set (the DismissReasonCodes precedent).
func SignalStates() []string { return append([]string(nil), signalStates...) }

// signalSettable is where working / needs_input may be set (D7): the statuses a
// session's human task sits in while it is worked outside any claim.
var signalSettable = map[string]bool{"holding": true, "ready": true, "blocked": true}

// SessionNameMax is the longest session name task_signal stores, in runes
// (SWT-56 S3): Claude Code's own session-name cap, so any name ListAgents
// prints fits. The board truncates visually, never in storage. Spelled once.
const SessionNameMax = 200

type signalArgs struct {
	TaskID int64  `json:"task_id"`
	State  string `json:"state"`
	// Session is the calling session's name from ListAgents' first line, "This
	// session is <name> [ref]" (SWT-56 S1): required on working and needs_input,
	// optional on clear. Self-reported data, never authority.
	Session string `json:"session,omitempty"`
	// WorkerID is injected by the MCP adapter and recorded in the event; it is
	// never used as authority (the actor on the call is).
	WorkerID string `json:"worker_id,omitempty"`
}

// NormalizeSessionName is SWT-56 S3, in order: trim; empty is "missing"; the
// whole ListAgents line is refused; at most SessionNameMax runes; every rune
// printable (unicode.IsPrint) or the ZWJ that joined emoji need. It returns the
// trimmed name, the only rewrite. A refused value is echoed by %.64q, so a
// pasted blob cannot flood the error or the audit row.
func NormalizeSessionName(s string) (string, error) {
	if sessionBlank(s) {
		return "", errors.New(`missing session: working and needs_input need this session's name — call ListAgents ` +
			`once and pass the <name> from its first line, "This session is <name> [ref]"`)
	}
	name := strings.TrimSpace(s)
	if strings.HasPrefix(strings.ToLower(name), "this session is") {
		return "", fmt.Errorf(`session %.64q: pass only the <name> from "This session is <name> [ref]", not the whole line`, name)
	}
	if n := utf8.RuneCountInString(name); n > SessionNameMax {
		return "", fmt.Errorf("session %.64q is %d characters; the cap is %d", name, n, SessionNameMax)
	}
	for _, r := range name {
		if !unicode.IsPrint(r) && r != 0x200D {
			return "", fmt.Errorf("session %.64q contains %U, which is not a printable character; "+
				"pass the plain <name> from ListAgents", name, r)
		}
	}
	return name, nil
}

func validateSignal(args []byte) error {
	var a signalArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID <= 0 {
		return errors.New("missing or zero task_id")
	}
	if a.State == "" {
		return fmt.Errorf("missing state: must be one of %s", strings.Join(signalStates, ", "))
	}
	known := false
	for _, s := range signalStates {
		if a.State == s {
			known = true
		}
	}
	if !known {
		// By name, both ways: the offending value AND the allowed set.
		return fmt.Errorf("state %q: must be one of %s", a.State, strings.Join(signalStates, ", "))
	}
	// SWT-56 S2: clear may omit the session (the opsctl recovery path; a blank
	// one is the same as none); when it carries one, it is validated like any
	// other.
	if a.State == "clear" && sessionBlank(a.Session) {
		return nil
	}
	_, err := NormalizeSessionName(a.Session)
	return err
}

// sessionBlank reports whether a session argument is absent: empty or only
// whitespace, which NormalizeSessionName calls "missing". Only a clear may
// carry one.
func sessionBlank(s string) bool { return strings.TrimSpace(s) == "" }

// signalRefusal names why working / needs_input cannot be set on a task in
// status (D10 a), or "" when it can.
func signalRefusal(status string) string {
	switch {
	case signalSettable[status]:
		return ""
	case status == "closed":
		return "reopen it first"
	case status == "done_locally" || status == "delivered":
		return "already done"
	case status == "claimed" || status == "in_progress" || status == "needs_feedback" ||
		strings.HasPrefix(status, "pr_") || strings.HasPrefix(status, "awaiting_"):
		return "held by a claim; its status is its signal"
	}
	return "only holding, ready or blocked tasks take a session signal"
}

// signalTask implements D10 in ONE transaction, under lockTask's row lock taken
// first — the lock closeTransition takes, so a signal and a close serialize: if
// the close wins, the signal refuses `closed`; if the signal wins, the close
// clears it. It never writes status, updated_at, closed_*, surfaced_*,
// priority, task_claims, feedback_requests or deliveries (D10 d).
func signalTask(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a signalArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	to := a.State
	if to == "clear" {
		to = ""
	}
	// SWT-56 S3, one spelling: the handler normalizes itself and never trusts
	// that validation ran.
	session := ""
	if to != "" || !sessionBlank(a.Session) {
		name, err := NormalizeSessionName(a.Session)
		if err != nil {
			return nil, err
		}
		session = name
	}
	changed := false
	stateAt := ""
	stored := ""
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		status, err := lockTask(ctx, tx, a.TaskID)
		if err != nil {
			return err
		}
		// from_session only under a state (S4 read gating): an old binary's
		// clear leaves a dangling name under a NULL state, which is no one's.
		var assignee, from, fromSession string
		if err := tx.QueryRow(ctx,
			`SELECT assignee_type, COALESCE(working_state,''),
			        COALESCE(CASE WHEN working_state IS NOT NULL THEN working_session END, '')
			   FROM tasks WHERE id=$1`, a.TaskID).
			Scan(&assignee, &from, &fromSession); err != nil {
			return fmt.Errorf("read task %d: %w", a.TaskID, err)
		}
		if assignee != "human" {
			return fmt.Errorf("task %d is a %s task; task_signal is for human tasks only (a worker task's "+
				"in-progress signal is its claim)", a.TaskID, assignee)
		}
		if to == "" {
			// (c) clear: accepted on any status; a no-op success with no event
			// when nothing was set.
			if from == "" {
				return nil
			}
			if _, err := tx.Exec(ctx,
				`UPDATE tasks SET working_state = NULL, working_state_at = NULL, working_session = NULL WHERE id=$1`,
				a.TaskID); err != nil {
				return fmt.Errorf("clear the state of task %d: %w", a.TaskID, err)
			}
		} else {
			// (a) refusals by name, then (b) set: the timestamp moves on every
			// call that sets a state, a repeat included; the last writer's name
			// wins (S6 takeover).
			if why := signalRefusal(status); why != "" {
				return fmt.Errorf("task %d is %s; %s", a.TaskID, status, why)
			}
			if err := tx.QueryRow(ctx,
				`UPDATE tasks SET working_state = $2, working_state_at = now(), working_session = $3 WHERE id=$1
				 RETURNING working_state_at::text`, a.TaskID, to, session).Scan(&stateAt); err != nil {
				return fmt.Errorf("set the state of task %d: %w", a.TaskID, err)
			}
			stored = session
			if from == to && fromSession == session {
				return nil // a refresh is not news: no event
			}
		}
		// S5: exactly these five keys on every working_state_changed event.
		if _, err := insertTaskEvent(ctx, tx, a.TaskID, "working_state_changed",
			map[string]any{"from": from, "from_session": fromSession, "session": session, "to": to,
				"worker_id": a.WorkerID}); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"task_id": a.TaskID, "state": to, "changed": changed, "state_at": stateAt,
		"session": stored})
}
