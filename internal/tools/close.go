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

// activeWorkRefusal is close's ONE refusal phrase for work that must not be
// closed out from under its holder or its live send. The Jira reconciler
// matches it as a non-fatal skip (ticketstatus.activeWorkRefusal, pinned by its
// statusset_test), so every such refusal must carry it verbatim.
const activeWorkRefusal = "refusing to close active work"

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
	status, err := lockTask(ctx, tx, taskID)
	if err != nil {
		return "", false, err
	}
	if to == "closed" {
		switch status {
		case "closed":
			return status, false, nil // idempotent
		case "claimed", "in_progress", "needs_feedback":
			return status, false, fmt.Errorf("task %d is %s; %s", taskID, status, activeWorkRefusal)
		}
		// SWT-37 (Codex pass 4): a send reserves 'sending' in a committed phase 1
		// and dispatches after its transaction ends, so a close that lands in
		// between would let words reach a client for CLOSED work. Phase 1 takes a
		// SHARE lock on this task (refuseClosedTask) before writing 'sending', and
		// this close holds the exclusive row lock taken above, so exactly one of
		// them wins: close first → the send refuses; send first → this refuses
		// until the delivery settles.
		//
		// Only a LIVE attempt counts (Codex pass 5, go-reviewer): gmail, Jira and
		// calendar commit 'sending' before the network call, so a crash there
		// leaves the row in 'sending' with no settle path, and an unbounded fence
		// would make the task uncloseable forever. Live = unsettled
		// (send_settled_at IS NULL: a settled Slack attempt can put no new words
		// anywhere) and started within sendAttemptLease — the SAME lease
		// mark_delivery_failed honours. The attempt clock is send_attempted_at
		// (gmail, calendar, Slack stamp it) else updated_at (Jira's phase 1 sets
		// only that); nothing refreshes either on a 'sending' row except its own
		// settlement or loop-closure confirmation.
		//
		// The message carries activeWorkRefusal on purpose: it is the substring
		// the Jira reconciler treats as a non-fatal refusal, so one task's live
		// send cannot abort a whole reconciliation pass.
		var inFlight int64
		var channel, attemptedAt string
		err := tx.QueryRow(ctx,
			`SELECT id, channel, COALESCE(send_attempted_at, updated_at)::text FROM deliveries
			  WHERE task_id=$1 AND status='sending' AND send_settled_at IS NULL
			    AND COALESCE(send_attempted_at, updated_at) > now() - make_interval(secs => $2)
			  ORDER BY id LIMIT 1`, taskID, sendAttemptLease.Seconds()).Scan(&inFlight, &channel, &attemptedAt)
		if err == nil {
			return status, false, fmt.Errorf("task %d has %s delivery %d in flight (sending since %s); %s until it "+
				"settles or its %s send lease runs out", taskID, channel, inFlight, attemptedAt, activeWorkRefusal,
				sendAttemptLease)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return status, false, fmt.Errorf("check in-flight deliveries for task %d: %w", taskID, err)
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

// lockTask is the ONE row-lock spelling in this file (SWT-32 criterion 37):
// closeTransition takes it, and so does the guarded reopen before its checks
// (SWT-36 criterion 2) — re-taking a lock the transaction already holds is a
// no-op, so the guarded path's later closeTransition cannot deadlock on it.
func lockTask(ctx context.Context, tx pgx.Tx, taskID int64) (string, error) {
	var status string
	if err := tx.QueryRow(ctx,
		`SELECT status FROM tasks WHERE id=$1 FOR UPDATE`, taskID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("task %d not found", taskID)
		}
		return "", fmt.Errorf("lock task %d: %w", taskID, err)
	}
	return status, nil
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

// reopenArgs: DismissalID and MessageID are SWT-36's activity guard — both or
// neither. With them the call is GUARDED: the handler decides, under the tasks
// row lock, whether that inbound message overtook that dismissal (D4). The
// caller supplies ids only, never a time or a direction.
type reopenArgs struct {
	TaskID      int64  `json:"task_id"`
	Status      string `json:"status,omitempty"`
	Reason      string `json:"reason"`
	DismissalID int64  `json:"dismissal_id,omitempty"`
	MessageID   int64  `json:"message_id,omitempty"`
}

func (a reopenArgs) guarded() bool { return a.DismissalID != 0 || a.MessageID != 0 }

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
	if a.guarded() {
		// SWT-36 criterion 1. A half-guarded call must never validate: it would
		// run as an UNGUARDED reopen and skip the dismissal check entirely.
		switch {
		case a.DismissalID < 0:
			return fmt.Errorf("dismissal_id %d: must be > 0", a.DismissalID)
		case a.MessageID < 0:
			return fmt.Errorf("message_id %d: must be > 0", a.MessageID)
		case a.DismissalID == 0 || a.MessageID == 0:
			return fmt.Errorf("dismissal_id and message_id: both or neither (got dismissal_id=%d, message_id=%d)",
				a.DismissalID, a.MessageID)
		case a.Status != "":
			// D5: the dismissal decides the target (closed_from_status, else ready).
			return fmt.Errorf("status %q is forbidden with dismissal_id: a guarded reopen restores the "+
				"dismissal's closed_from_status", a.Status)
		}
		return nil
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
//
// SWT-36: a reopen that transitions a task stamps its OPEN dismissal (at most
// one — the partial index), so "an open dismissal implies a closed task" stays
// true and a later re-dismissal inserts a fresh row instead of conflicting. The
// unguarded stamp carries no message id: that is D6's mis-click signal. With
// dismissal_id + message_id the call is guarded (reopenGuarded).
func reopenTask(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a reopenArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	if a.guarded() {
		return reopenGuarded(ctx, pool, a)
	}
	target := a.Status
	if target == "" {
		target = "ready"
	}
	actor := executor.ActorFrom(ctx)

	var from string
	var reopened bool
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		f, done, err := closeTransition(ctx, tx, a.TaskID, target, a.Reason)
		from, reopened = f, done
		if err != nil || !done {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE task_dismissals SET reopened_at = now(), reopened_by = $2
			  WHERE task_id = $1 AND reopened_at IS NULL`, a.TaskID, actor); err != nil {
			return fmt.Errorf("stamp open dismissal of task %d: %w", a.TaskID, err)
		}
		return nil
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

// Guarded-reopen skip answers (SWT-36 criterion 2). A skip is a SUCCESS with
// reopened:false: the caller's claim is spent either way and the log line it
// already appended is the whole record.
const (
	skipNotClosed        = "not_closed"
	skipDismissalNotOpen = "dismissal_not_open"
	skipMessagePredates  = "message_predates_dismissal"
)

// reopenGuarded is the activity reopen (SWT-36 D4), in criterion 2's order,
// inside ONE transaction that first takes the tasks row lock task_dismiss also
// takes — so a dismissal and a reopen of one task are serialized, in the same
// lock order, and cannot deadlock:
//
//	(a) the message must exist and be INBOUND — else an ERROR (invariant 5 at
//	    the verb, gated on the column; checked first so a caller bug is never
//	    laundered into a quiet skip)
//	(b) the task must be closed                  — else skipped: not_closed
//	(c) the dismissal must be this task's, open  — else skipped: dismissal_not_open
//	(d) message.created_at > dismissal.created_at, strictly, on the ONE Postgres
//	    clock (D2: ingest time, never sent_at)   — else skipped: message_predates_dismissal
//	(e) transition to closed_from_status when it is an open status, else ready
//	    (D5), and stamp the dismissal with the message.
func reopenGuarded(ctx context.Context, pool *pgxpool.Pool, a reopenArgs) ([]byte, error) {
	actor := executor.ActorFrom(ctx)
	result := map[string]any{"task_id": a.TaskID, "reopened": false}

	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		status, err := lockTask(ctx, tx, a.TaskID)
		if err != nil {
			return err
		}
		result["status"] = status

		// (a)
		var direction, sender string
		var ingestedAt time.Time
		if err := tx.QueryRow(ctx,
			`SELECT direction, COALESCE(sender,''), created_at FROM normalized_messages WHERE id=$1`,
			a.MessageID).Scan(&direction, &sender, &ingestedAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("message_id %d: no such normalized message", a.MessageID)
			}
			return fmt.Errorf("read message %d: %w", a.MessageID, err)
		}
		if direction != "inbound" {
			return fmt.Errorf("message_id %d is %s; only an INBOUND message can reopen a dismissed task "+
				"(our own sends re-enter via ingestion — invariant 5)", a.MessageID, direction)
		}

		// (b)
		if status != "closed" {
			result["skipped"] = skipNotClosed
			return nil
		}

		// (c) + (d). The comparison is SQL on the two columns — one clock, one
		// spelling; the caller never supplies a time.
		var code, dismissedBy, closedFrom string
		var dismissedAt time.Time
		var overtaken bool
		err = tx.QueryRow(ctx,
			`SELECT d.reason_code, d.dismissed_by, COALESCE(d.closed_from_status,''), d.created_at,
			        m.created_at > d.created_at
			   FROM task_dismissals d, normalized_messages m
			  WHERE d.id = $1 AND d.task_id = $2 AND d.reopened_at IS NULL AND m.id = $3`,
			a.DismissalID, a.TaskID, a.MessageID).Scan(&code, &dismissedBy, &closedFrom, &dismissedAt, &overtaken)
		if errors.Is(err, pgx.ErrNoRows) {
			result["skipped"] = skipDismissalNotOpen
			return nil
		}
		if err != nil {
			return fmt.Errorf("read dismissal %d of task %d: %w", a.DismissalID, a.TaskID, err)
		}
		if !overtaken {
			result["skipped"] = skipMessagePredates
			return nil
		}

		// (e)
		target := "ready"
		for _, s := range openStatuses {
			if closedFrom == s {
				target = closedFrom
				break
			}
		}
		// Codex re-reviews: dependency gating is event-driven (R4 blocks a READY
		// task when a dependency is added; R5 unblocks a BLOCKED one when its
		// dependencies complete), and neither fires for a CLOSED task. So while
		// it was dismissed, a task's dependencies may have been satisfied (a
		// verbatim `blocked` would strand it) or a new unmet one added (a
		// verbatim `ready` would let a worker claim it early), and
		// closed → ready|blocked fires neither rule. For those two targets the
		// guarded reopen therefore re-derives the gate from the dependencies
		// themselves (depUnsatisfiedPredicate, the tools package's one
		// spelling): blocked while any is unmet, else ready.
		if target == "blocked" || target == "ready" {
			var unmet bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM task_dependencies d
				   JOIN tasks dt ON dt.id = d.depends_on_task_id
				   WHERE d.task_id=$1 AND dt.status `+depUnsatisfiedPredicate+`)`, a.TaskID).Scan(&unmet); err != nil {
				return fmt.Errorf("check dependencies of task %d: %w", a.TaskID, err)
			}
			target = "ready"
			if unmet {
				target = "blocked"
			}
		}
		reason := fmt.Sprintf("reopened after dismissal (%s, dismissed %s by %s): message %d from %s, ingested %s — %s",
			code, dismissedAt.UTC().Format(time.RFC3339), dismissedBy,
			a.MessageID, orNoneStr(sender), ingestedAt.UTC().Format(time.RFC3339), a.Reason)
		if _, _, err := closeTransition(ctx, tx, a.TaskID, target, reason); err != nil {
			return err
		}
		// Exec + RowsAffected (the dismissTask note): under the row lock the
		// dismissal cannot have changed since (c), so anything but one row is a
		// broken invariant and rolls the transition back with it.
		tag, err := tx.Exec(ctx,
			`UPDATE task_dismissals SET reopened_at = now(), reopened_by = $2, reopened_by_message_id = $3
			  WHERE id = $1 AND reopened_at IS NULL`, a.DismissalID, actor, a.MessageID)
		if err != nil {
			return fmt.Errorf("stamp dismissal %d: %w", a.DismissalID, err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("stamp dismissal %d: %d rows affected, want 1", a.DismissalID, tag.RowsAffected())
		}
		result["status"] = target
		result["reopened"] = true
		result["dismissal_id"] = a.DismissalID
		return nil
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(result)
}

func orNoneStr(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
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
//
// SWT-36: the row records closed_from_status (closeTransition's `from` when it
// transitioned, NULL when the task was already closed) — the status an activity
// reopen restores (D5). The unique index is now PARTIAL (one OPEN dismissal per
// task, 0026), so the arbiter RESTATES `WHERE reopened_at IS NULL`: omitting it
// is a runtime "no unique or exclusion constraint matching the ON CONFLICT
// specification" on a human's click. After a reopen, a re-dismissal inserts a
// second row.
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
		from, transitioned, err := closeTransition(ctx, tx, a.TaskID, "closed", reason)
		if err != nil {
			return err
		}
		var closedFrom *string
		if transitioned {
			closedFrom = &from
		}
		// Exec, not QueryRow+RETURNING: the conflict no-op must NOT ride on an
		// error, because an error here rolls the whole transaction back — and
		// task_reopen makes "second dismiss silently un-commits the close it
		// just performed" reachable (go-reviewer, 2026-09-09). RowsAffected
		// carries the same fact with no rollback and no ErrNoRows special case.
		tag, err := tx.Exec(ctx,
			`INSERT INTO task_dismissals (task_id, reason_code, note, dismissed_by, closed_from_status)
			 VALUES ($1,$2, NULLIF($3,''), $4, $5)
			 ON CONFLICT (task_id) WHERE reopened_at IS NULL DO NOTHING`,
			a.TaskID, a.ReasonCode, a.Note, actor, closedFrom)
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
