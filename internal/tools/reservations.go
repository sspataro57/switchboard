package tools

// swb-first calendar reservations (SWT-109, calendar-mcp-tools). Salvador,
// 2026-10-21: "I would like to be able to book times and swb to keep them saved
// somewhere until they can sync to the actual calendar."
//
// A reservation IS a deliveries row with channel='calendar' (no new table,
// invariant 2): booked in switchboard = 'approved', pushed to Google = 'sent',
// observed by a read poll = confirmed_at. book_time writes one at once and
// from that instant every free/busy answer treats it as busy
// (availability.loadReservationRows); the pipelined calendar_push stage
// (calendar_push.go) writes it to Google when it can and never loses it.
//
// WHICH CALENDAR is never the caller's choice (Q1, owner 2026-10-21: "by
// default use sspataro@gmail.com unless the meeting request comes from
// avviato in that case try to sync to handsonconnect"). reservationTarget
// derives it from the TASK'S PROJECT: projects.calendar_account_id, else the
// single source_accounts.calendar_default row. book_time has no account
// argument, and the push stage recomputes the target before every write.
//
// WHAT GOOGLE SEES is fixed server-side: "Busy" for a booking, "Hold" for an
// offer hold, and one fixed description. The session's label lives only in
// switchboard (reservation_label). No client or task name ever reaches a
// calendar colleagues can see (the Avviato disclosure rule, invariant 6).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/availability"
	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
)

const (
	// ReservationSummaryBooking and ReservationSummaryHold are the ONLY
	// summaries a reservation is pushed with (criterion 32).
	ReservationSummaryBooking = "Busy"
	ReservationSummaryHold    = "Hold"
	// ReservationDescription is the ONLY description a reservation is pushed
	// with. Neutral on purpose: it names no person, client or task.
	ReservationDescription = "Reserved."

	reservationMaxLen      = 12 * time.Hour
	reservationLabelMax    = 120
	reservationHourlyLimit = 10
)

// reservationText is the neutral-text builder (criterion 32): the summary and
// description pushed to Google depend ONLY on the reservation's kind. There
// is no parameter through which a label could arrive.
func reservationText(isHold bool) (summary, description string) {
	if isHold {
		return ReservationSummaryHold, ReservationDescription
	}
	return ReservationSummaryBooking, ReservationDescription
}

// reservationTarget is the ONE resolution of which calendar a task's
// reservations are pushed to (criterion 30), used by book_time, ensureHoldRows
// and the push stage's re-check. The project's calendar_account_id wins;
// otherwise the single calendar_default account. Either way it must be a
// google account in availability scope, or the call refuses by name.
func reservationTarget(ctx context.Context, q querier, taskID int64) (int64, string, error) {
	var projectSlug string
	var mapped *int64
	err := q.QueryRow(ctx,
		`SELECT p.slug, p.calendar_account_id FROM tasks t JOIN projects p ON p.id = t.project_id WHERE t.id=$1`,
		taskID).Scan(&projectSlug, &mapped)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", fmt.Errorf("task %d not found", taskID)
	}
	if err != nil {
		return 0, "", fmt.Errorf("resolve task %d project: %w", taskID, err)
	}

	var (
		id                int64
		email, provider   string
		inAvailability    bool
	)
	if mapped != nil {
		err = q.QueryRow(ctx,
			`SELECT id, account_email, provider, calendar_in_availability FROM source_accounts WHERE id=$1`,
			*mapped).Scan(&id, &email, &provider, &inAvailability)
		if err != nil {
			return 0, "", fmt.Errorf("project %s's calendar account %d: %w", projectSlug, *mapped, err)
		}
	} else {
		err = q.QueryRow(ctx,
			`SELECT id, account_email, provider, calendar_in_availability FROM source_accounts WHERE calendar_default`).
			Scan(&id, &email, &provider, &inAvailability)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, "", fmt.Errorf("no default calendar: no source_accounts row has calendar_default, and project %s maps no calendar_account_id", projectSlug)
		}
		if err != nil {
			return 0, "", fmt.Errorf("default calendar account: %w", err)
		}
	}
	if provider != "google" || !inAvailability {
		return 0, "", fmt.Errorf("calendar account %s for project %s is not a google calendar in availability scope", email, projectSlug)
	}
	return id, email, nil
}

// ---- book_time ----

type bookTimeArgs struct {
	TaskID int64  `json:"task_id"`
	Start  string `json:"start"`
	End    string `json:"end"`
	Label  string `json:"label"`
	// RequireAssigneeType is the user-profile pin (criterion 11): only narrows,
	// absent from every schema.
	RequireAssigneeType string `json:"require_assignee_type,omitempty"`
}

func parseInterval(start, end string) (time.Time, time.Time, error) {
	s, err := time.Parse(time.RFC3339, start)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("start: %w", err)
	}
	e, err := time.Parse(time.RFC3339, end)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("end: %w", err)
	}
	if !e.After(s) {
		return time.Time{}, time.Time{}, errors.New("end must be after start")
	}
	return s, e, nil
}

func validateBookTime(args []byte) error {
	var a bookTimeArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID <= 0 {
		return errors.New("missing task_id")
	}
	s, e, err := parseInterval(a.Start, a.End)
	if err != nil {
		return err
	}
	if e.Sub(s) > reservationMaxLen {
		return fmt.Errorf("a booking is at most %s; this one is %s", reservationMaxLen, e.Sub(s))
	}
	label := strings.TrimSpace(a.Label)
	if label == "" {
		return errors.New("missing label: say in a few words what the time is for (it stays in switchboard; Google sees only \"Busy\")")
	}
	if utf8.RuneCountInString(label) > reservationLabelMax {
		return fmt.Errorf("label is longer than %d characters", reservationLabelMax)
	}
	if a.RequireAssigneeType != "" && a.RequireAssigneeType != "human" {
		return fmt.Errorf("require_assignee_type %q: only \"human\" is supported", a.RequireAssigneeType)
	}
	return nil
}

// horizonCheck is the interval-inside-the-synced-horizon refusal, shared by
// book_time and offered_windows (a reservation outside it could never be
// verified against Google).
func horizonCheck(start, end, now time.Time) error {
	if start.Before(now.Add(-google.CalendarWindowPast)) || end.After(now.Add(google.CalendarWindowFuture)) {
		return fmt.Errorf("%s..%s is outside the synced calendar horizon [now-%s, now+%s]",
			start.Format(time.RFC3339), end.Format(time.RFC3339), google.CalendarWindowPast, google.CalendarWindowFuture)
	}
	return nil
}

// overlapError names the first busy interval a window collides with.
func overlapError(start, end time.Time, busy []availability.Interval) error {
	for _, iv := range busy {
		if iv.Start.Before(end) && iv.End.After(start) {
			return &SlotConflictError{Start: start, End: end, BusyStart: iv.Start, BusyEnd: iv.End}
		}
	}
	return nil
}

// SlotConflictError is the typed overlap refusal (SWT-109): the push stage
// treats it as TERMINAL for a reservation, unlike a stale calendar.
type SlotConflictError struct {
	Start, End, BusyStart, BusyEnd time.Time
}

func (e *SlotConflictError) Error() string {
	return fmt.Sprintf("slot %s..%s conflicts with an existing busy interval %s..%s; re-run propose_slots and pick a free slot",
		e.Start.Format(time.RFC3339), e.End.Format(time.RFC3339),
		e.BusyStart.Format(time.RFC3339), e.BusyEnd.Format(time.RFC3339))
}

func bookTime(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a bookTimeArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	start, end, err := parseInterval(a.Start, a.End)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if err := horizonCheck(start, end, now); err != nil {
		return nil, err
	}
	label := google.ScrubAIAttribution(strings.TrimSpace(a.Label))
	if label == "" {
		return nil, errors.New("label is empty after scrubbing")
	}
	_, maxSyncAge, err := availabilityConfig()
	if err != nil {
		return nil, err
	}

	var (
		deliveryID int64
		email      string
		verified   bool
	)
	err = inTx(ctx, pool, func(tx pgx.Tx) error {
		// The SWT-28 allocation lock: a booking and a push cannot both see the
		// same slot free.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, calendarBookingLock); err != nil {
			return fmt.Errorf("acquire calendar booking lock: %w", err)
		}
		var status, assignee string
		if err := tx.QueryRow(ctx, `SELECT status, assignee_type FROM tasks WHERE id=$1`, a.TaskID).
			Scan(&status, &assignee); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("task %d not found", a.TaskID)
			}
			return fmt.Errorf("load task %d: %w", a.TaskID, err)
		}
		if status == "closed" {
			return fmt.Errorf("task %d is closed: book time on open work", a.TaskID)
		}
		if a.RequireAssigneeType == "human" && assignee != "human" {
			return fmt.Errorf("task %d is a %s task; this session can book time only on human tasks", a.TaskID, assignee)
		}
		var accountID int64
		accountID, email, err = reservationTarget(ctx, tx, a.TaskID)
		if err != nil {
			return err
		}
		var recent int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM deliveries WHERE channel='calendar' AND created_at >= now() - interval '1 hour'`).
			Scan(&recent); err != nil {
			return fmt.Errorf("count recent reservations: %w", err)
		}
		if recent >= reservationHourlyLimit {
			return fmt.Errorf("%d calendar reservations were made in the last hour (limit %d); try again later", recent, reservationHourlyLimit)
		}

		// A known conflict refuses. A stale calendar does not: the booking is
		// saved unverified after checking switchboard's own reservations, and
		// the push re-verifies with the full fail-closed LoadBusy first.
		busy, err := availability.LoadBusy(ctx, pool, availability.Request{
			WindowStart: start, WindowEnd: end, Now: now, MaxSyncAge: maxSyncAge,
			HorizonPast: google.CalendarWindowPast, HorizonFuture: google.CalendarWindowFuture,
		})
		var notReady *availability.NotReadyError
		switch {
		case err == nil:
			verified = true
			if err := overlapError(start, end, busy); err != nil {
				return err
			}
		case errors.As(err, &notReady):
			verified = false
			own, err := availability.LoadReservations(ctx, pool, start, end)
			if err != nil {
				return err
			}
			for _, r := range own {
				if r.Start.Before(end) && r.End.After(start) {
					return &SlotConflictError{Start: start, End: end, BusyStart: r.Start, BusyEnd: r.End}
				}
			}
		default:
			return err
		}

		summary, description := reservationText(false)
		if err := tx.QueryRow(ctx,
			`INSERT INTO deliveries (task_id, channel, target_ref, body, subject, status, approval_source,
			                         from_account_id, created_by, starts_at, ends_at,
			                         reservation_label, booking_verified, push_next_attempt_at)
			 VALUES ($1, 'calendar', $2, $3, $4, 'approved', 'switchboard', $5, $6, $7, $8, $9, $10, now())
			 RETURNING id`,
			a.TaskID, email, description, summary, accountID, executor.ActorFrom(ctx),
			start, end, label, verified).Scan(&deliveryID); err != nil {
			return fmt.Errorf("insert reservation: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO approvals (subject_type, subject_id, status, decided_by, decided_at)
			 VALUES ('delivery', $1, 'approved', $2, now())`, deliveryID, executor.ActorFrom(ctx)); err != nil {
			return fmt.Errorf("insert approval: %w", err)
		}
		if _, err := insertTaskEvent(ctx, tx, a.TaskID, "reservation_booked", map[string]any{
			"delivery_id": deliveryID, "start": start.Format(time.RFC3339), "end": end.Format(time.RFC3339),
			"calendar": email, "verified": verified}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"delivery_id": deliveryID, "state": "pending", "calendar": email, "verified": verified})
}

// ---- cancel_reservation ----

func validateCancelReservation(args []byte) error {
	var a deliveryIDOnlyArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.DeliveryID <= 0 {
		return errors.New("missing delivery_id")
	}
	return nil
}

// cancelReservation is DB-only (criterion 12): a reservation never pushed is
// released at once with no external call; a pushed one is marked
// release-requested and the push stage deletes its Google event.
func cancelReservation(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a deliveryIDOnlyArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	var out map[string]any
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		var (
			taskID         int64
			channel        string
			extID          *string
			requested      *time.Time
			reason         *string
			kept           *time.Time
			holdFor        *int64
		)
		err := tx.QueryRow(ctx,
			`SELECT task_id, channel, sent_external_id, release_requested_at, release_reason, hold_kept_at, hold_for_delivery_id
			   FROM deliveries WHERE id=$1 FOR UPDATE`, a.DeliveryID).
			Scan(&taskID, &channel, &extID, &requested, &reason, &kept, &holdFor)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("delivery %d not found", a.DeliveryID)
		}
		if err != nil {
			return fmt.Errorf("lock delivery %d: %w", a.DeliveryID, err)
		}
		if channel != "calendar" {
			return fmt.Errorf("delivery %d is a %s delivery, not a calendar reservation", a.DeliveryID, channel)
		}
		if requested != nil {
			if reason != nil && *reason == "cancelled" {
				out = map[string]any{"delivery_id": a.DeliveryID, "changed": 0}
				return nil
			}
			return fmt.Errorf("delivery %d was already released (%s)", a.DeliveryID, derefReason(reason))
		}
		if kept != nil {
			return fmt.Errorf("delivery %d is the hold kept for offer %d; it is the meeting time they picked", a.DeliveryID, derefID(holdFor))
		}
		if extID == nil {
			_, err = tx.Exec(ctx,
				`UPDATE deliveries SET release_requested_at=now(), released_at=now(), release_reason='cancelled', updated_at=now()
				  WHERE id=$1`, a.DeliveryID)
		} else {
			_, err = tx.Exec(ctx,
				`UPDATE deliveries SET release_requested_at=now(), release_reason='cancelled', updated_at=now()
				  WHERE id=$1`, a.DeliveryID)
		}
		if err != nil {
			return fmt.Errorf("cancel reservation %d: %w", a.DeliveryID, err)
		}
		if _, err := insertTaskEvent(ctx, tx, taskID, "reservation_cancelled",
			map[string]any{"delivery_id": a.DeliveryID, "pushed": extID != nil}); err != nil {
			return err
		}
		state := "released"
		if extID != nil {
			state = "delete_pending"
		}
		out = map[string]any{"delivery_id": a.DeliveryID, "changed": 1, "state": state}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(out)
}

func derefReason(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefID(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// ---- offer holds ----

// OfferWindow is one offered meeting window on a gmail offer
// (deliveries.offered_windows), stored in UTC.
type OfferWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

const (
	offerWindowsMax   = 3
	offerWindowMaxLen = 4 * time.Hour
)

// offerHoldTTL is OFFER_HOLD_TTL (default 72h); a bad value is an error.
func offerHoldTTL() (time.Duration, error) {
	return durationEnv("OFFER_HOLD_TTL", 72*time.Hour)
}

// normalizeOfferWindows validates and sorts windows (criterion 13): 1-3, each
// at most 4h, non-overlapping. The horizon is checked by the caller with now.
// offerWindowArg is one window as a caller passes it (RFC3339 strings).
type offerWindowArg struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

func normalizeOfferWindows(raw []offerWindowArg) ([]OfferWindow, error) {
	if len(raw) == 0 || len(raw) > offerWindowsMax {
		return nil, fmt.Errorf("offered_windows takes 1 to %d windows; got %d", offerWindowsMax, len(raw))
	}
	out := make([]OfferWindow, 0, len(raw))
	for i, w := range raw {
		s, e, err := parseInterval(w.Start, w.End)
		if err != nil {
			return nil, fmt.Errorf("offered_windows[%d]: %w", i, err)
		}
		if e.Sub(s) > offerWindowMaxLen {
			return nil, fmt.Errorf("offered_windows[%d] is %s; a window is at most %s", i, e.Sub(s), offerWindowMaxLen)
		}
		out = append(out, OfferWindow{Start: s.UTC(), End: e.UTC()})
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Start.Before(out[j-1].Start); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	for i := 1; i < len(out); i++ {
		if out[i].Start.Before(out[i-1].End) {
			return nil, fmt.Errorf("offered_windows overlap: %s..%s and %s..%s",
				out[i-1].Start.Format(time.RFC3339), out[i-1].End.Format(time.RFC3339),
				out[i].Start.Format(time.RFC3339), out[i].End.Format(time.RFC3339))
		}
	}
	return out, nil
}

// checkOfferWindowsFree is the STRICT fail-closed check offers get at draft
// and again before send (criteria 13 and 15): LoadBusy must answer, and no
// window may overlap. A stale calendar refuses — an offer is a promise to
// someone, so it is never made from a copy nobody vouched for.
func checkOfferWindowsFree(ctx context.Context, pool *pgxpool.Pool, windows []OfferWindow, exclude []int64) error {
	_, maxSyncAge, err := availabilityConfig()
	if err != nil {
		return err
	}
	now := time.Now()
	for _, w := range windows {
		if err := horizonCheck(w.Start, w.End, now); err != nil {
			return fmt.Errorf("offered window: %w", err)
		}
		busy, err := availability.LoadBusy(ctx, pool, availability.Request{
			WindowStart: w.Start, WindowEnd: w.End, Now: now, MaxSyncAge: maxSyncAge,
			HorizonPast: google.CalendarWindowPast, HorizonFuture: google.CalendarWindowFuture,
			ExcludeDeliveryIDs: exclude,
		})
		if err != nil {
			return err
		}
		if err := overlapError(w.Start, w.End, busy); err != nil {
			return fmt.Errorf("offered window is no longer free: %w", err)
		}
	}
	return nil
}

// ensureHoldRows creates one hold reservation per offered window of a SENT
// gmail offer (criterion 16), at most once: a second call finds the set and
// returns. Target from the offer task's project (criterion 30), neutral text,
// approved, expiring OFFER_HOLD_TTL after the offer's sent_at.
func ensureHoldRows(ctx context.Context, q querier, offerID int64, actor string) (int, error) {
	var (
		taskID  int64
		raw     []byte
		sentAt  *time.Time
		channel string
	)
	if err := q.QueryRow(ctx,
		`SELECT task_id, channel, offered_windows, sent_at FROM deliveries WHERE id=$1 FOR UPDATE`, offerID).
		Scan(&taskID, &channel, &raw, &sentAt); err != nil {
		return 0, fmt.Errorf("load offer %d: %w", offerID, err)
	}
	if channel != "gmail" || len(raw) == 0 {
		return 0, nil
	}
	var existing int
	if err := q.QueryRow(ctx,
		`SELECT count(*) FROM deliveries WHERE hold_for_delivery_id=$1 AND (release_requested_at IS NULL OR hold_kept_at IS NOT NULL)`,
		offerID).Scan(&existing); err != nil {
		return 0, fmt.Errorf("count holds of offer %d: %w", offerID, err)
	}
	if existing > 0 {
		return 0, nil
	}
	var windows []OfferWindow
	if err := json.Unmarshal(raw, &windows); err != nil {
		return 0, fmt.Errorf("offer %d offered_windows: %w", offerID, err)
	}
	if len(windows) == 0 {
		return 0, nil
	}
	ttl, err := offerHoldTTL()
	if err != nil {
		return 0, err
	}
	accountID, email, err := reservationTarget(ctx, q, taskID)
	if err != nil {
		return 0, err
	}
	base := time.Now()
	if sentAt != nil {
		base = *sentAt
	}
	summary, description := reservationText(true)
	for _, w := range windows {
		var id int64
		if err := q.QueryRow(ctx,
			`INSERT INTO deliveries (task_id, channel, target_ref, body, subject, status, approval_source,
			                         from_account_id, created_by, starts_at, ends_at,
			                         hold_for_delivery_id, hold_expires_at, booking_verified, push_next_attempt_at)
			 VALUES ($1, 'calendar', $2, $3, $4, 'approved', 'switchboard', $5, $6, $7, $8, $9, $10, true, now())
			 RETURNING id`,
			taskID, email, description, summary, accountID, actor, w.Start, w.End, offerID, base.Add(ttl)).Scan(&id); err != nil {
			return 0, fmt.Errorf("insert hold for offer %d: %w", offerID, err)
		}
		if _, err := q.Exec(ctx,
			`INSERT INTO approvals (subject_type, subject_id, status, decided_by, decided_at)
			 VALUES ('delivery', $1, 'approved', $2, now())`, id, actor); err != nil {
			return 0, fmt.Errorf("insert hold approval: %w", err)
		}
	}
	if _, err := insertTaskEvent(ctx, q, taskID, "reservation_booked",
		map[string]any{"offer_delivery_id": offerID, "holds": len(windows), "calendar": email}); err != nil {
		return 0, err
	}
	return len(windows), nil
}

// ---- release_offer_holds ----

type releaseOfferHoldsArgs struct {
	DeliveryID int64  `json:"delivery_id"`
	KeepStart  string `json:"keep_start,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

func validateReleaseOfferHolds(args []byte) error {
	var a releaseOfferHoldsArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.DeliveryID <= 0 {
		return errors.New("missing delivery_id")
	}
	if a.KeepStart != "" {
		if _, err := time.Parse(time.RFC3339, a.KeepStart); err != nil {
			return fmt.Errorf("keep_start: %w", err)
		}
	}
	switch a.Reason {
	case "", "picked_other", "not_needed", "expired":
	default:
		return fmt.Errorf("reason %q: want picked_other, not_needed (or expired, the orchestrator's)", a.Reason)
	}
	return nil
}

// releaseOfferHolds (criterion 17) keeps the hold for the picked time as a
// plain booking and releases the rest; DB-only and idempotent.
func releaseOfferHolds(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a releaseOfferHoldsArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	actor := executor.ActorFrom(ctx)
	if a.Reason == "expired" && policy.ViaMCPActor(actor) {
		return nil, errors.New("reason \"expired\" belongs to the orchestrator's expiry rule; over MCP say picked_other or not_needed")
	}
	reason := a.Reason
	if reason == "" {
		reason = "not_needed"
		if a.KeepStart != "" {
			reason = "picked_other"
		}
	}
	var keep *time.Time
	if a.KeepStart != "" {
		k, _ := time.Parse(time.RFC3339, a.KeepStart)
		keep = &k
	}

	changed := 0
	var keptID int64
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		var taskID int64
		if err := tx.QueryRow(ctx, `SELECT task_id FROM deliveries WHERE id=$1 FOR UPDATE`, a.DeliveryID).Scan(&taskID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("offer delivery %d not found", a.DeliveryID)
			}
			return fmt.Errorf("lock offer %d: %w", a.DeliveryID, err)
		}
		rows, err := tx.Query(ctx,
			`SELECT id, starts_at, ends_at, sent_external_id, hold_kept_at IS NOT NULL
			   FROM deliveries
			  WHERE hold_for_delivery_id=$1 AND release_requested_at IS NULL
			  ORDER BY starts_at FOR UPDATE`, a.DeliveryID)
		if err != nil {
			return fmt.Errorf("load holds of offer %d: %w", a.DeliveryID, err)
		}
		type hold struct {
			id         int64
			start, end time.Time
			extID      *string
			kept       bool
		}
		var holds []hold
		for rows.Next() {
			var h hold
			if err := rows.Scan(&h.id, &h.start, &h.end, &h.extID, &h.kept); err != nil {
				rows.Close()
				return fmt.Errorf("scan hold: %w", err)
			}
			holds = append(holds, h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		if keep != nil {
			found := false
			var spans []string
			for _, h := range holds {
				spans = append(spans, h.start.Format(time.RFC3339))
				if h.start.Equal(*keep) {
					found = true
					if !h.kept {
						if _, err := tx.Exec(ctx,
							`UPDATE deliveries SET hold_kept_at=now(), hold_expires_at=NULL, updated_at=now() WHERE id=$1`, h.id); err != nil {
							return fmt.Errorf("keep hold %d: %w", h.id, err)
						}
						changed++
					}
					keptID = h.id
				}
			}
			if !found {
				var already int64
				_ = tx.QueryRow(ctx, `SELECT id FROM deliveries WHERE hold_for_delivery_id=$1 AND hold_kept_at IS NOT NULL AND starts_at=$2`,
					a.DeliveryID, *keep).Scan(&already)
				if already == 0 {
					return fmt.Errorf("keep_start %s matches none of offer %d's live holds (%s)",
						keep.Format(time.RFC3339), a.DeliveryID, strings.Join(spans, ", "))
				}
				keptID = already
			}
		}
		for _, h := range holds {
			if h.kept || h.id == keptID {
				continue
			}
			var err error
			if h.extID == nil {
				_, err = tx.Exec(ctx,
					`UPDATE deliveries SET release_requested_at=now(), released_at=now(), release_reason=$2, updated_at=now() WHERE id=$1`,
					h.id, reason)
			} else {
				_, err = tx.Exec(ctx,
					`UPDATE deliveries SET release_requested_at=now(), release_reason=$2, updated_at=now() WHERE id=$1`,
					h.id, reason)
			}
			if err != nil {
				return fmt.Errorf("release hold %d: %w", h.id, err)
			}
			changed++
		}
		if changed > 0 {
			if _, err := insertTaskEvent(ctx, tx, taskID, "reservation_released",
				map[string]any{"offer_delivery_id": a.DeliveryID, "reason": reason, "kept_delivery_id": keptID, "changed": changed}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := map[string]any{"delivery_id": a.DeliveryID, "changed": changed, "reason": reason}
	if keptID != 0 {
		out["kept_delivery_id"] = keptID
	}
	return marshalResult(out)
}

// nullableJSON passes nil as SQL NULL and anything else as JSON text.
func nullableJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// ensureHoldsAfterSend places an offer's holds once the email is SENT
// (criterion 16), in its own transaction after the sent write. Best-effort on
// purpose: the email already left, so a failure here must not turn the send
// into an error; it is logged on the task, and the push stage's
// repairMissingHolds makes it eventual.
func ensureHoldsAfterSend(ctx context.Context, pool *pgxpool.Pool, offerID int64) {
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := ensureHoldRows(ctx, tx, offerID, executor.ActorFrom(ctx))
		return err
	})
	if err != nil {
		var taskID int64
		if pool.QueryRow(ctx, `SELECT task_id FROM deliveries WHERE id=$1`, offerID).Scan(&taskID) == nil {
			_, _ = insertTaskEvent(ctx, pool, taskID, "log", map[string]any{
				"delivery_id": offerID,
				"message":     "offer sent; its calendar holds were not created yet (the calendar_push stage retries)",
				"error":       err.Error()})
		}
	}
}

// reserveOfferWindows re-checks an offer's windows STRICTLY and places its
// holds in ONE transaction under the calendar allocation lock (criterion 15,
// codex review). The offer's own live holds (a retried send) are excluded
// from the check and reused.
func reserveOfferWindows(ctx context.Context, pool *pgxpool.Pool, offerID int64, raw []byte) error {
	var windows []OfferWindow
	if err := json.Unmarshal(raw, &windows); err != nil {
		return fmt.Errorf("offered_windows: %w", err)
	}
	return inTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, calendarBookingLock); err != nil {
			return fmt.Errorf("acquire calendar booking lock: %w", err)
		}
		var own []int64
		rows, err := tx.Query(ctx,
			`SELECT id FROM deliveries WHERE hold_for_delivery_id=$1 AND release_requested_at IS NULL`, offerID)
		if err != nil {
			return fmt.Errorf("load holds of offer %d: %w", offerID, err)
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("scan hold: %w", err)
			}
			own = append(own, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if err := checkOfferWindowsFree(ctx, pool, windows, own); err != nil {
			return err
		}
		_, err = ensureHoldRows(ctx, tx, offerID, executor.ActorFrom(ctx))
		return err
	})
}

// releaseUnsentOfferHolds frees an offer's holds when its email DEFINITELY did
// not go out — and only while NO send has claimed the offer (codex review): a
// losing concurrent send fails phase 1 on the winner's Message-ID and must not
// free the winner's holds. Unpushed holds are released at once; a pushed one
// is deleted by the stage. Best-effort: otherwise the holds simply expire.
//
// Codex review 3: it takes the OFFER row lock — the same lock ensureHoldRows
// takes when a retry reserves — and releases only while the offer is still
// 'failed' with no Message-ID. A retry that re-approved the offer in the
// meantime keeps its holds; a phase-1 refusal (still approved) leaves them to
// expire rather than open that window.
func releaseUnsentOfferHolds(ctx context.Context, pool *pgxpool.Pool, offerID int64) {
	_ = inTx(ctx, pool, func(tx pgx.Tx) error {
		var status string
		var ext *string
		if err := tx.QueryRow(ctx, `SELECT status, sent_external_id FROM deliveries WHERE id=$1 FOR UPDATE`, offerID).
			Scan(&status, &ext); err != nil {
			return err
		}
		if status != "failed" || ext != nil {
			return nil
		}
		_, err := tx.Exec(ctx,
			`UPDATE deliveries
			    SET release_requested_at=now(), release_reason='not_needed',
			        released_at = CASE WHEN sent_external_id IS NULL THEN now() ELSE NULL END, updated_at=now()
			  WHERE hold_for_delivery_id=$1 AND release_requested_at IS NULL AND hold_kept_at IS NULL`, offerID)
		return err
	})
}
