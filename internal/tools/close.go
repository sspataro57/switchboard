package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
)

// task_close and record_orchestration — spine-facing (SPEC 05-orchestrator-loop) —
// and task_dismiss (SWT-31): the board's human-only close-with-a-label. The two
// close verbs share ONE transition helper so the active-work refusal cannot
// drift; what separates them is the policy gate (a dismissal is a human
// judgement recorded as training data, and task_close cannot be humanOnly —
// the orchestrator calls it) and the typed task_dismissals row.

type closeArgs struct {
	TaskID int64  `json:"task_id"`
	Reason string `json:"reason"`
}

// openStatuses is D6's set (SWT-32), spelled ONCE: exactly the statuses
// task_close accepts as a SOURCE and therefore exactly the ones task_reopen
// accepts as a TARGET. closeTransition and validateReopen both read it; a
// second spelling is how the two verbs drift.
var openStatuses = []string{"holding", "ready", "blocked", "done_locally", "delivered"}

// closeTransition is the ONE transition helper all three close-family verbs
// share (SWT-31 D3, SWT-32 criterion 37): one row lock, one active-work
// refusal, one status_changed writer. `to` is either "closed" (close/dismiss)
// or a member of openStatuses (reopen).
//
// Idempotence differs by direction, deliberately: closing an already-closed
// task is a no-op (orchestrator replays and stale dismiss pages), and
// reopening a task that is not closed is a no-op (the spine convention for
// replays — criterion 36). Active work refuses a close; nothing but `closed`
// can be reopened. Runs inside the caller's transaction so a caller can commit
// more (the dismissal label) atomically with the transition.
func closeTransition(ctx context.Context, tx pgx.Tx, taskID int64, to, reason string) (from string, transitioned bool, err error) {
	var status string
	if err := tx.QueryRow(ctx,
		`SELECT status FROM tasks WHERE id=$1 FOR UPDATE`, taskID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, fmt.Errorf("task %d not found", taskID)
		}
		return "", false, fmt.Errorf("lock task %d: %w", taskID, err)
	}
	if to == "closed" {
		switch status {
		case "closed":
			return status, false, nil // idempotent
		case "claimed", "in_progress", "needs_feedback":
			return status, false, fmt.Errorf("task %d is %s; refusing to close active work", taskID, status)
		}
	} else if status != "closed" {
		return status, false, nil // reopen replay: already open, no event
	}
	if _, err := tx.Exec(ctx,
		`UPDATE tasks SET status=$2, updated_at=now() WHERE id=$1`, taskID, to); err != nil {
		return status, false, fmt.Errorf("transition task %d to %s: %w", taskID, to, err)
	}
	if _, err := insertTaskEvent(ctx, tx, taskID, "status_changed",
		map[string]any{"from": status, "to": to, "reason": reason}); err != nil {
		return status, false, err
	}
	return status, true, nil
}

func validateClose(args []byte) error {
	var a closeArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID == 0 {
		return errors.New("missing task_id")
	}
	if a.Reason == "" {
		return errors.New("missing reason")
	}
	return nil
}

// closeTask is the terminal verb: -> closed from holding | ready | blocked |
// done_locally | delivered. Refuses from claimed | in_progress |
// needs_feedback (never close work out from under a holder). Already-closed
// is an idempotent success so orchestrator replays never stall.
func closeTask(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a closeArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		_, _, err := closeTransition(ctx, tx, a.TaskID, "closed", a.Reason)
		return err
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"task_id": a.TaskID, "status": "closed"})
}

// ---- task_reopen (SWT-32) -----------------------------------------------------

type reopenArgs struct {
	TaskID int64  `json:"task_id"`
	Status string `json:"status,omitempty"`
	Reason string `json:"reason"`
}

func validateReopen(args []byte) error {
	var a reopenArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID <= 0 {
		return errors.New("missing or zero task_id")
	}
	if a.Reason == "" {
		return errors.New("missing reason")
	}
	if a.Status == "" {
		return nil // optional; the handler falls back to ready (D6)
	}
	for _, allowed := range openStatuses {
		if a.Status == allowed {
			return nil
		}
	}
	// By name, both ways: a reopen to `claimed` would produce a claimed task
	// with no task_claims row and no worker — a shape nothing else in the
	// spine can produce.
	return fmt.Errorf("status %q: must be one of %s", a.Status, strings.Join(openStatuses, ", "))
}

// reopenTask restores a closed task to an open status (SWT-32 D6): the
// reconciler's return path, and the human remedy for a mis-click dismissal
// (SWT-31 named re-open as that remedy; the dismissal SUPPRESSION lives in the
// pass, not here — D5 — so a human can still undo one). A task that is not
// closed is an idempotent success with reopened:false and no event.
func reopenTask(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a reopenArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	target := a.Status
	if target == "" {
		target = "ready"
	}

	var from string
	var reopened bool
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		f, done, err := closeTransition(ctx, tx, a.TaskID, target, a.Reason)
		from, reopened = f, done
		return err
	})
	if err != nil {
		return nil, err
	}
	status := target
	if !reopened {
		status = from
	}
	return marshalResult(map[string]any{"task_id": a.TaskID, "status": status, "reopened": reopened})
}

// ---- task_dismiss (SWT-31) ----------------------------------------------------

// dismissReasonCodes is D4's enum — migration 0022's CHECK, spelled here for
// the validator so a code that would violate the constraint is refused before
// a policy check and two audit rows.
var dismissCodes = []string{"not_actionable", "wrong_kind", "duplicate", "handled_elsewhere"}

// DismissReasonCodes returns a copy of task_dismiss's reason codes, in order:
// the one source of the MCP schema's enum (SWT-37 V5), pinned there by
// TestTaskVerbSchemas. A copy, so no caller can rewrite the validator's set.
func DismissReasonCodes() []string { return append([]string(nil), dismissCodes...) }

type dismissArgs struct {
	TaskID     int64  `json:"task_id"`
	ReasonCode string `json:"reason_code"`
	Note       string `json:"note,omitempty"`
}

func validateDismiss(args []byte) error {
	var a dismissArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID <= 0 {
		return errors.New("missing or zero task_id")
	}
	if a.ReasonCode == "" {
		return errors.New("missing reason_code")
	}
	for _, code := range dismissCodes {
		if a.ReasonCode == code {
			return nil
		}
	}
	// By name, both ways: quote the offending value AND the allowed set — a
	// code that silently fell back would mint mislabelled training data.
	return fmt.Errorf("reason_code %q: must be one of %s",
		a.ReasonCode, strings.Join(dismissCodes, ", "))
}

// dismissTask closes a task AND records the human's judgement as a typed
// task_dismissals row, in one transaction (criterion 12). An already-closed
// task still records the label with no status_changed event (criterion 14 —
// refusing would lose the judgement, and the row makes no claim about the
// transition); a task that already carries a label is an idempotent success
// with dismissed:false, and the FIRST label survives (ON CONFLICT DO NOTHING —
// a replay is not a correction).
func dismissTask(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a dismissArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	actor := executor.ActorFrom(ctx)

	// The prose for the human trail: the machine-readable copy is the row.
	reason := "dismissed (" + a.ReasonCode + ")"
	if a.Note != "" {
		reason += ": " + a.Note
	}

	var dismissed bool
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		if _, _, err := closeTransition(ctx, tx, a.TaskID, "closed", reason); err != nil {
			return err
		}
		// Exec, not QueryRow+RETURNING: the conflict no-op must NOT ride on an
		// error, because an error here rolls the whole transaction back — and
		// while today's only replay path (task already closed, row exists)
		// discards nothing, a future task_reopen would make "second dismiss
		// silently un-commits the close it just performed" reachable
		// (go-reviewer, 2026-09-09). RowsAffected carries the same fact with
		// no rollback and no ErrNoRows special case.
		tag, err := tx.Exec(ctx,
			`INSERT INTO task_dismissals (task_id, reason_code, note, dismissed_by)
			 VALUES ($1,$2, NULLIF($3,''), $4)
			 ON CONFLICT (task_id) DO NOTHING`,
			a.TaskID, a.ReasonCode, a.Note, actor)
		if err != nil {
			return fmt.Errorf("record dismissal for task %d: %w", a.TaskID, err)
		}
		dismissed = tag.RowsAffected() == 1
		return nil
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"task_id": a.TaskID, "status": "closed", "dismissed": dismissed})
}

type recordOrchestrationArgs struct {
	TaskID         int64          `json:"task_id"`
	Rule           string         `json:"rule"`
	TriggerEventID int64          `json:"trigger_event_id"`
	Payload        map[string]any `json:"payload,omitempty"`
}

func validateRecordOrchestration(args []byte) error {
	var a recordOrchestrationArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID == 0 {
		return errors.New("missing task_id")
	}
	if a.Rule == "" {
		return errors.New("missing rule")
	}
	return nil
}

// recordOrchestration writes the orchestrator's decision record: an
// 'orchestrated' task_events row on the triggering task. It doubles as the
// replay-dedup key and surfaces in task_context.
func recordOrchestration(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a recordOrchestrationArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM tasks WHERE id=$1)`, a.TaskID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("verify task %d: %w", a.TaskID, err)
	}
	if !exists {
		return nil, fmt.Errorf("task %d not found", a.TaskID)
	}

	payload := map[string]any{"rule": a.Rule, "trigger_event_id": a.TriggerEventID}
	for k, v := range a.Payload {
		payload[k] = v
	}
	eventID, err := insertTaskEvent(ctx, pool, a.TaskID, "orchestrated", payload)
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"event_id": eventID})
}
