package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// task_signal (SWT-52, board-status-lights): a Claude session's STATE SIGNAL on
// a HUMAN task — working, needs_input (waiting on Salvador), or clear. It is an
// annotation on the tasks row (0033: working_state + working_state_at), not a
// status: nothing routes on it, no claim backs it, and the orchestrator never
// reads it. It records nothing but the state — Salvador answers in the
// session's own console, never through switchboard (SPEC D4, D14).
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

type signalArgs struct {
	TaskID int64  `json:"task_id"`
	State  string `json:"state"`
	// WorkerID is injected by the MCP adapter and recorded in the event; it is
	// never used as authority (the actor on the call is).
	WorkerID string `json:"worker_id,omitempty"`
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
	for _, s := range signalStates {
		if a.State == s {
			return nil
		}
	}
	// By name, both ways: the offending value AND the allowed set.
	return fmt.Errorf("state %q: must be one of %s", a.State, strings.Join(signalStates, ", "))
}

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
	changed := false
	stateAt := ""
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		status, err := lockTask(ctx, tx, a.TaskID)
		if err != nil {
			return err
		}
		var assignee, from string
		if err := tx.QueryRow(ctx,
			`SELECT assignee_type, COALESCE(working_state,'') FROM tasks WHERE id=$1`, a.TaskID).
			Scan(&assignee, &from); err != nil {
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
				`UPDATE tasks SET working_state = NULL, working_state_at = NULL WHERE id=$1`, a.TaskID); err != nil {
				return fmt.Errorf("clear the state of task %d: %w", a.TaskID, err)
			}
		} else {
			// (a) refusals by name, then (b) set: the timestamp moves on every
			// call that sets a state, a repeat included.
			if why := signalRefusal(status); why != "" {
				return fmt.Errorf("task %d is %s; %s", a.TaskID, status, why)
			}
			if err := tx.QueryRow(ctx,
				`UPDATE tasks SET working_state = $2, working_state_at = now() WHERE id=$1
				 RETURNING working_state_at::text`, a.TaskID, to).Scan(&stateAt); err != nil {
				return fmt.Errorf("set the state of task %d: %w", a.TaskID, err)
			}
			if from == to {
				return nil // a refresh is not news: no event
			}
		}
		if _, err := insertTaskEvent(ctx, tx, a.TaskID, "working_state_changed",
			map[string]any{"from": from, "to": to, "worker_id": a.WorkerID}); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"task_id": a.TaskID, "state": to, "changed": changed, "state_at": stateAt})
}
