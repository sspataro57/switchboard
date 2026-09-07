package tools

// The calendar delivery channel (SWT-28): "book an own calendar block" as a
// policy-gated delivery, at the AUTO tier (Q1 = b in the ticket's decision
// record). Two registered entry points — book_calendar_block (agent-callable:
// approve + send in one audited call) and send_delivery's calendar branch (the
// human two-step) — both funnel into the unexported sendCalendarBlock, so
// exactly ONE code path talks to the Pipedream write route (invariant 3).
//
// WHAT AN INJECTED book_calendar_block CALL CAN AND CANNOT DO (the
// mark_delivery_sent register, delivery.go:680-698): it can put a block on
// Salvador's OWN calendar, at a time provably free (the LoadBusy conflict/
// freshness/horizon refusal below), on an account a human explicitly
// write-enabled (calendar_write_enabled, re-checked at SEND time), at most ten
// per hour (the channel rate limit), every call audited, stoppable with
// set_sending_frozen. It cannot choose a time, a calendar, a summary or an
// attendee: every field comes from the drafted row, which went through
// draft_delivery's validation and ScrubAIAttribution, and the write envelope
// has no attendees field at all.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/availability"
	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/executor"
)

// CalendarBooker is the ONE adapter seam to the Pipedream write route; its
// only caller is sendCalendarBlock, reached only from the two registered
// handlers (invariant 3).
type CalendarBooker interface {
	CreateEvent(ctx context.Context, req google.CreateEventRequest) (google.CreateEventResponse, error)
}

var calendarBooker CalendarBooker

// calendarBookingLock serializes slot ALLOCATION across the whole
// availability scope (codex finding, 2026-09-07). One global key, not
// per-account: busy is MERGED across calendars, so two overlapping blocks on
// two different accounts still double-book the same human. Bookings are rare
// (10/hour cap) and the lock spans only the pre-flight + reserve transaction
// — never the network call.
const calendarBookingLock = int64(0x53575432380001)

// SetCalendarBooker wires the Pipedream calendar write adapter (the
// NewDeliveryBridgeFromEnv shape: a construction error is fatal at wiring
// time, an absent configuration leaves this nil and booking refused by name).
func SetCalendarBooker(b CalendarBooker) { calendarBooker = b }

// bookCalendarBlock is the auto tier's verb: approve a drafted calendar row
// and send it, in one executor call. The approve half is approveDelivery's
// shape verbatim — approval_source='switchboard' in the same statement as the
// status transition, plus an approvals row naming executor.ActorFrom(ctx), so
// WHICH console decided to take the slot is recorded even when it is a worker
// (criterion 18). It COMMITS before the pre-flight, so a pre-flight refusal
// leaves the row approved and a retry after the conflict clears stays possible.
func bookCalendarBlock(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a deliveryIDOnlyArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		var channel, status string
		var extID *string
		if err := tx.QueryRow(ctx,
			`SELECT channel, status, sent_external_id FROM deliveries WHERE id=$1 FOR UPDATE`,
			a.DeliveryID).Scan(&channel, &status, &extID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("delivery %d not found", a.DeliveryID)
			}
			return fmt.Errorf("lock delivery %d: %w", a.DeliveryID, err)
		}
		if channel != "calendar" {
			// The INNER of criterion 15's two gates; policy's channel_mismatch
			// deny is the outer, pure one and normally fires first.
			return fmt.Errorf("book_calendar_block only acts on calendar deliveries; delivery %d is %s",
				a.DeliveryID, channel)
		}
		if extID != nil {
			return fmt.Errorf("delivery %d already carries sent_external_id; never resend (invariant 4)", a.DeliveryID)
		}
		switch status {
		case "drafted":
			if _, err := tx.Exec(ctx,
				`UPDATE deliveries SET status='approved', approval_source='switchboard', updated_at=now()
				 WHERE id=$1`, a.DeliveryID); err != nil {
				return fmt.Errorf("approve delivery %d: %w", a.DeliveryID, err)
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO approvals (subject_type, subject_id, status, decided_by, decided_at)
				 VALUES ('delivery', $1, 'approved', $2, now())`,
				a.DeliveryID, executor.ActorFrom(ctx)); err != nil {
				return fmt.Errorf("insert approval: %w", err)
			}
		case "approved":
			// A retry after a pre-flight refusal: already approved, already
			// recorded. Do not write a second approvals row.
		default:
			return fmt.Errorf("delivery %d is %s; only a drafted or approved calendar delivery can be booked",
				a.DeliveryID, status)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sendCalendarBlock(ctx, pool, a.DeliveryID)
}

// sendCalendarBlock is the one send half both verbs route to.
//
// Order is load-bearing. (1) The PRE-FLIGHT refusal (criterion 19) runs before
// any row mutation: availability.LoadBusy — the same fail-closed door
// propose_slots uses, with the same injected horizon — refuses a stale sync,
// an empty scope, a window outside the horizon (its error propagates VERBATIM
// so audit_events.error reads exactly like a propose_slots refusal), and any
// overlap with [starts_at, ends_at). The row stays approved and untouched.
// (2) Phase 1 (criterion 20), the gmail shape verbatim: lock, verify, re-check
// calendar_write_enabled AT SEND (a draft can predate a revocation; under the
// auto tier that column is the only per-account consent), then commit
// status='sending' + sent_external_id + send_attempted_at in ONE statement
// BEFORE the network call — a crash in the window leaves a row that can never
// resend (invariant 4), not one that can double-book. (3) Phase 2, the bounded
// network call; on ANY error the row goes failed with the id KEPT — the send
// may have landed, and reopening would depend on the third-party workflow's
// 409 handling. (4) The busy set learns about the block immediately
// (criterion 22), best-effort (criterion 23).
func sendCalendarBlock(ctx context.Context, pool *pgxpool.Pool, deliveryID int64) ([]byte, error) {
	if calendarBooker == nil {
		return nil, fmt.Errorf("no calendar booking adapter wired (SetCalendarBooker; is PIPEDREAM_CALENDAR_URL configured?)")
	}

	// A plain read: nothing may be locked or mutated before the pre-flight.
	var (
		taskID           int64
		status           string
		targetRef        string
		subject, body    string
		startsAt, endsAt time.Time
		fromAcct         *int64
		priorExtID       *string
	)
	err := pool.QueryRow(ctx,
		`SELECT task_id, status, COALESCE(target_ref,''), COALESCE(subject,''), body,
		        starts_at, ends_at, from_account_id, sent_external_id
		   FROM deliveries WHERE id=$1 AND channel='calendar'`, deliveryID).
		Scan(&taskID, &status, &targetRef, &subject, &body, &startsAt, &endsAt, &fromAcct, &priorExtID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("delivery %d is not a calendar delivery", deliveryID)
	}
	if err != nil {
		return nil, fmt.Errorf("load calendar delivery %d: %w", deliveryID, err)
	}
	if priorExtID != nil {
		return nil, fmt.Errorf("delivery %d already carries sent_external_id; never resend (invariant 4)", deliveryID)
	}
	if status != "approved" {
		return nil, fmt.Errorf("delivery %d is %s; only approved deliveries send", deliveryID, status)
	}

	// (1)+(2) Pre-flight AND reservation, SERIALIZED in one transaction under
	// a global advisory lock (codex finding, 2026-09-07): LoadBusy alone is an
	// unlocked snapshot, so two overlapping bookings could both see a free
	// slot and both reserve. Under the lock, allocation is one-at-a-time, and
	// because an unconfirmed reservation is LoadBusy-visible
	// (availability.loadReservations), the second booking SEES the first the
	// moment its reserve commits. A pre-flight refusal rolls the whole
	// transaction back, leaving the row approved and otherwise untouched
	// (criterion 19) — and the LoadBusy error still propagates VERBATIM so it
	// reads exactly like a propose_slots refusal.
	var (
		accountID int64
		eventID   string
		extID     string
	)
	err = inTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, calendarBookingLock); err != nil {
			return fmt.Errorf("acquire calendar booking lock: %w", err)
		}
		_, maxSyncAge, err := availabilityConfig()
		if err != nil {
			return err
		}
		busy, err := availability.LoadBusy(ctx, pool, availability.Request{
			WindowStart:   startsAt,
			WindowEnd:     endsAt,
			Now:           time.Now(),
			MaxSyncAge:    maxSyncAge,
			HorizonPast:   google.CalendarWindowPast,
			HorizonFuture: google.CalendarWindowFuture,
		})
		if err != nil {
			return err // VERBATIM: it must read exactly like a propose_slots refusal
		}
		for _, iv := range busy {
			if iv.Start.Before(endsAt) && iv.End.After(startsAt) {
				return fmt.Errorf(
					"slot %s..%s conflicts with an existing busy interval %s..%s; re-run propose_slots and draft a free slot",
					startsAt.Format(time.RFC3339), endsAt.Format(time.RFC3339),
					iv.Start.Format(time.RFC3339), iv.End.Format(time.RFC3339))
			}
		}

		var txStatus string
		var txExtID *string
		var txApprovalSource *string
		var txFromAcct *int64
		var writeEnabled *bool
		var accountEmail string
		if err := tx.QueryRow(ctx,
			`SELECT d.status, d.sent_external_id, d.approval_source, d.from_account_id,
			        a.calendar_write_enabled, COALESCE(a.account_email,'')
			   FROM deliveries d LEFT JOIN source_accounts a ON a.id = d.from_account_id
			  WHERE d.id=$1 FOR UPDATE OF d`, deliveryID).
			Scan(&txStatus, &txExtID, &txApprovalSource, &txFromAcct, &writeEnabled, &accountEmail); err != nil {
			return fmt.Errorf("lock delivery %d: %w", deliveryID, err)
		}
		if txExtID != nil {
			return fmt.Errorf("delivery %d already carries sent_external_id; never resend (invariant 4)", deliveryID)
		}
		if txStatus != "approved" {
			return fmt.Errorf("delivery %d is %s; only approved deliveries send", deliveryID, txStatus)
		}
		if txApprovalSource == nil || *txApprovalSource != "switchboard" {
			// Criterion 20: the write route is unreachable except from a row
			// whose gate is KNOWN (sendSlackReply's rule) — today only two
			// statements write 'approved' and both stamp 'switchboard', but
			// that property must be checked here, not assumed.
			return fmt.Errorf("delivery %d is approved but its approval_source is not 'switchboard'; refusing to send a row whose gate is unknown", deliveryID)
		}
		if txFromAcct == nil || accountEmail == "" {
			return fmt.Errorf("delivery %d has no from account", deliveryID)
		}
		if writeEnabled == nil || !*writeEnabled {
			return fmt.Errorf("account %s is not calendar_write_enabled; the go-live gate is checked at SEND "+
				"(a draft can predate a revocation, and under the auto tier this column is the only "+
				"per-account consent an unattended booking has)", accountEmail)
		}
		accountID = *txFromAcct
		eventID = google.CalendarEventID(deliveryID, time.Now().UnixNano())
		extID = google.CalendarExternalID(eventID)
		if _, err := tx.Exec(ctx,
			`UPDATE deliveries SET status='sending', sent_external_id=$2,
			        send_attempted_at=now(), updated_at=now()
			 WHERE id=$1`, deliveryID, extID); err != nil {
			return fmt.Errorf("mark sending: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// (3) Phase 2: the network call, bounded by opsctl's existing deadline shape.
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, sendErr := calendarBooker.CreateEvent(callCtx, google.CreateEventRequest{
		SchemaVersion: google.PipedreamCalendarSchemaVersion,
		Action:        "create_event",
		CalendarID:    targetRef,
		EventID:       eventID,
		Start:         startsAt.Format(time.RFC3339),
		End:           endsAt.Format(time.RFC3339),
		Summary:       subject,
		Description:   body,
	})
	if sendErr != nil {
		// The id is KEPT on every failure path: the send may have landed, and
		// reopening the row for a resend would depend on the workflow's 409
		// handling being trustworthy — it lives in a human-edited third-party
		// workflow. Recovery is the read poll, or a NEW draft.
		_, _ = pool.Exec(ctx,
			`UPDATE deliveries SET status='failed', error=$2, updated_at=now() WHERE id=$1`,
			deliveryID, sendErr.Error())
		return nil, fmt.Errorf("calendar booking: %w", sendErr)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE deliveries SET status='sent', sent_at=now(), error=NULL, updated_at=now() WHERE id=$1`,
		deliveryID); err != nil {
		return nil, fmt.Errorf("finalize sent: %w", err)
	}

	// (4) The busy set learns about the block NOW, not at the next */20 poll
	// (criterion 22) — without this, propose_slots keeps offering a slot
	// switchboard just booked, and under the auto tier that is a self-inflicted
	// double-booking loop. BEST-EFFORT (criterion 23): a failure here never
	// changes the delivery's status and never touches deliveries.error (that
	// column carries the reconcilers' fire-once markers); the next read poll
	// heals it. Deliberately BEFORE the delivery_sent insert: if that insert
	// errors, the handler returns — the busy set must already be current by
	// then (go-reviewer finding, 2026-09-07).
	busyPending := false
	if err := google.NewPGSink(pool).RecordOwnCalendarEvent(ctx, accountID, resp.Event); err != nil {
		busyPending = true
		_, _ = insertTaskEvent(ctx, pool, taskID, "log", map[string]any{
			"delivery_id": deliveryID,
			"message":     "calendar busy-set record failed; the block is live but invisible to propose_slots until the next read poll",
			"error":       err.Error(),
		})
	}

	// The channel key is LOAD-BEARING: R8's calendar skip (criterion 29) reads it.
	if _, err := insertTaskEvent(ctx, pool, taskID, "delivery_sent",
		map[string]any{"delivery_id": deliveryID, "channel": "calendar", "sent_external_id": extID}); err != nil {
		return nil, err
	}

	result := map[string]any{"delivery_id": deliveryID, "status": "sent", "sent_external_id": extID}
	if busyPending {
		result["busy_set_pending"] = true
	}
	return marshalResult(result)
}
