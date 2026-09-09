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

// closeTransition is the ONE spelling of "move this task to closed": lock,
// refuse active work, update, record the status_changed event with the given
// prose reason. Already-closed is an idempotent success with transitioned=false
// and no event — orchestrator replays and stale dismiss pages both depend on
// that. Runs inside the caller's transaction so a caller can commit more (the
// dismissal label) atomically with the transition.
func closeTransition(ctx context.Context, tx pgx.Tx, taskID int64, reason string) (transitioned bool, err error) {
	var status string
	if err := tx.QueryRow(ctx,
		`SELECT status FROM tasks WHERE id=$1 FOR UPDATE`, taskID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, fmt.Errorf("task %d not found", taskID)
		}
		return false, fmt.Errorf("lock task %d: %w", taskID, err)
	}
	switch status {
	case "closed":
		return false, nil // idempotent
	case "claimed", "in_progress", "needs_feedback":
		return false, fmt.Errorf("task %d is %s; refusing to close active work", taskID, status)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE tasks SET status='closed', updated_at=now() WHERE id=$1`, taskID); err != nil {
		return false, fmt.Errorf("close task %d: %w", taskID, err)
	}
	if _, err := insertTaskEvent(ctx, tx, taskID, "status_changed",
		map[string]any{"from": status, "to": "closed", "reason": reason}); err != nil {
		return false, err
	}
	return true, nil
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
		_, err := closeTransition(ctx, tx, a.TaskID, a.Reason)
		return err
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"task_id": a.TaskID, "status": "closed"})
}

// ---- task_dismiss (SWT-31) ----------------------------------------------------

// dismissReasonCodes is D4's enum — migration 0022's CHECK, spelled here for
// the validator so a code that would violate the constraint is refused before
// a policy check and two audit rows.
var dismissCodes = []string{"not_actionable", "wrong_kind", "duplicate", "handled_elsewhere"}

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
		if _, err := closeTransition(ctx, tx, a.TaskID, reason); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`INSERT INTO task_dismissals (task_id, reason_code, note, dismissed_by)
			 VALUES ($1,$2, NULLIF($3,''), $4)
			 ON CONFLICT (task_id) DO NOTHING
			 RETURNING true`,
			a.TaskID, a.ReasonCode, a.Note, actor).Scan(&dismissed)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// The label already exists: the replay is a success that changes nothing.
		err, dismissed = nil, false
	}
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
