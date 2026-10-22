package tools

// The calendar_push stage's two verbs (SWT-109 criteria 18-23):
// push_calendar_reservations writes due reservations to Google in ONE batched
// invocation, and apply_hold_releases deletes released ones in ONE more. Both
// are sendShaped + freezeGated with their policy channel pinned to calendar
// (internal/policy), off both MCP profiles, called only by cmd/pipelined as
// pipeline:calendar_push. Every Google write for a reservation goes through
// here; book_calendar_block keeps its own synchronous single-row path, and
// both reach the network only through calendarBooker.
//
// NEVER LOSE ONE. A reservation that cannot be pushed stays approved (or
// failed-with-id), busy in switchboard, with push_pending_reason saying why:
// account_not_write_enabled, target_mismatch, credit_budget, credit_cap,
// calendar_stale, transport_error. Only a conflict found at push time (terminal)
// and an explicit release take it out of the busy set.
//
// THE SAME-ID RETRY (amends SWT-28's "a failed booking is never reopened"; for
// codex review). A row's event id is minted once and never replaced. A failed
// row that carries an id is retried with that SAME id, and only after a read
// poll of its account that STARTED after the failed attempt finished ok without
// observing the event (the poll would have confirmed it, taking the row out of
// this inbox). Google answers a duplicate id with 409, which the workflow
// resolves with events.get + created:false, so a retry cannot double-book.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/availability"
	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/policy"
)

const (
	pushStageDefaultLimit = 25
	pushStageMaxLimit     = 100
	pushBackoffBase       = 5 * time.Minute
	pushBackoffMax        = 6 * time.Hour
)

// durationEnv parses a Go duration env var; a bad or non-positive value is an
// error, never a silent default.
func durationEnv(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a Go duration: %w", name, v, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s %q must be positive", name, v)
	}
	return d, nil
}

// calendarWriteBudget is CALENDAR_WRITE_MONTHLY_BUDGET (default 35): the write
// invocations a UTC month may spend, under Pipedream's 100-credit cap.
func calendarWriteBudget() (int, error) {
	v := os.Getenv("CALENDAR_WRITE_MONTHLY_BUDGET")
	if v == "" {
		return 35, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("CALENDAR_WRITE_MONTHLY_BUDGET %q is not a non-negative integer", v)
	}
	return n, nil
}

// pushBackoff is min(5m * 2^attempts, 6h), attempts counted before this failure.
func pushBackoff(attempts int) time.Duration {
	d := pushBackoffBase
	for i := 0; i < attempts; i++ {
		d *= 2
		if d >= pushBackoffMax {
			return pushBackoffMax
		}
	}
	return d
}

// isCreditCap recognises Pipedream's spent-credits answer: every call returns
// HTTP 400 "Error in workflow" until the month turns (runbook).
func isCreditCap(err error) bool {
	return err != nil && strings.Contains(err.Error(), "HTTP 400") && strings.Contains(err.Error(), "Error in workflow")
}

// calendarWriteCounter is the ops_flags row that counts Google write
// invocations this UTC month, APPEND-ONLY in effect: every dispatch
// increments it before the call, retries included (codex review: counting
// distinct attempt stamps on mutable rows forgot every overwritten retry).
const calendarWriteCounter = "calendar_write_invocations"

// admitCalendarWrite counts one write invocation, atomically, under the
// counter row's lock. With enforce it refuses — counting nothing — once the
// month's budget is spent; without (the synchronous book_calendar_block path,
// which predates the budget) it only counts.
//
// weight is how many workflow invocations the call can cost: create_event's
// client retries once on a transport failure (SWT-28 criterion 7), and a
// request the workflow ran whose answer was lost is billed twice, so a
// single-row create counts 2. The batch calls never retry and count 1.
func admitCalendarWrite(ctx context.Context, q querier, budget, weight int, enforce bool) (bool, int, error) {
	admitted, n := false, 0
	err := func(tx querier) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO ops_flags (name, value) VALUES ($1, '{}'::jsonb) ON CONFLICT (name) DO NOTHING`,
			calendarWriteCounter); err != nil {
			return fmt.Errorf("seed calendar write counter: %w", err)
		}
		var month, current string
		if err := tx.QueryRow(ctx,
			`SELECT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM'), COALESCE(value->>'month','') || ':' || COALESCE(value->>'n','0')
			   FROM ops_flags WHERE name=$1 FOR UPDATE`, calendarWriteCounter).Scan(&month, &current); err != nil {
			return fmt.Errorf("lock calendar write counter: %w", err)
		}
		used := 0
		if parts := strings.SplitN(current, ":", 2); len(parts) == 2 && parts[0] == month {
			used, _ = strconv.Atoi(parts[1])
		}
		if enforce && used+weight > budget {
			n = used
			return nil
		}
		n = used + weight
		if _, err := tx.Exec(ctx,
			`UPDATE ops_flags SET value=jsonb_build_object('month', $2::text, 'n', $3::int), updated_at=now() WHERE name=$1`,
			calendarWriteCounter, month, n); err != nil {
			return fmt.Errorf("count calendar write: %w", err)
		}
		admitted = true
		return nil
	}(q)
	return admitted, n, err
}

type pushLimitArgs struct {
	Limit int `json:"limit,omitempty"`
}

func validatePushLimit(args []byte) error {
	var a pushLimitArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.Limit < 0 || a.Limit > pushStageMaxLimit {
		return fmt.Errorf("limit must be 0..%d", pushStageMaxLimit)
	}
	return nil
}

func (a pushLimitArgs) effective() int {
	if a.Limit <= 0 {
		return pushStageDefaultLimit
	}
	return a.Limit
}

// PushInboxSQL is the ONE spelling of the push inbox (criterion 19): due,
// not on its way out, and either booked in switchboard or failed with an id
// and in the retry schedule (push_next_attempt_at set — SWT-28's synchronous
// book_calendar_block rows never set it, so their "never reopened" holds).
const PushInboxSQL = `channel = 'calendar'
	   AND push_next_attempt_at IS NOT NULL AND push_next_attempt_at <= now()
	   AND release_requested_at IS NULL AND released_at IS NULL
	   AND confirmed_at IS NULL
	   AND push_pending_reason IS DISTINCT FROM 'conflict'
	   AND ends_at > now()
	   AND (status = 'approved'
	        OR (status = 'failed' AND sent_external_id IS NOT NULL)
	        -- a push interrupted after it committed 'sending' (process exit, a
	        -- lost result write): past the send lease it is retried like a
	        -- failed row — same id, only after verified absence (codex review)
	        OR (status = 'sending' AND sent_external_id IS NOT NULL
	            AND send_attempted_at < now() - interval '15 minutes'))`

// ReleaseInboxSQL is the ONE spelling of the delete inbox (criterion 22).
const ReleaseInboxSQL = `channel = 'calendar'
	   AND release_requested_at IS NOT NULL AND released_at IS NULL
	   AND (status <> 'sending' OR send_attempted_at < now() - interval '15 minutes')
	   AND (push_next_attempt_at IS NULL OR push_next_attempt_at <= now())`

// PushInboxCount and ReleaseInboxCount let the stage skip a call (and its
// audit row) when nothing is due.
func PushInboxCount(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM deliveries WHERE `+PushInboxSQL+`) + (SELECT count(*) FROM deliveries o WHERE `+missingHoldsSQL+`)`).
		Scan(&n); err != nil {
		return 0, fmt.Errorf("count push inbox: %w", err)
	}
	return n, nil
}

func ReleaseInboxCount(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE `+ReleaseInboxSQL).Scan(&n); err != nil {
		return 0, fmt.Errorf("count release inbox: %w", err)
	}
	return n, nil
}

// missingHoldsSQL (over deliveries o) is a sent offer with windows and no
// holds yet: the at-send insert failed, and the stage repairs it.
const missingHoldsSQL = `o.channel='gmail' AND o.status='sent' AND o.offered_windows IS NOT NULL
	    AND o.sent_at >= now() - interval '30 days'
	    AND NOT EXISTS (SELECT 1 FROM deliveries h WHERE h.hold_for_delivery_id = o.id)`

// gateRecheck is how long a row waiting on a gate (a calendar not yet
// write-enabled, a target mismatch, a poll that has not yet vouched for a
// retry) waits before the next look: each look is an executor call with its
// audit rows, and these gates change by hand or twice a day.
const gateRecheck = 30 * time.Minute

type pushRow struct {
	id, taskID, fromAccount int64
	status                  string
	extID                   *string
	attemptedAt             *time.Time
	start, end              time.Time
	isHold                  bool
	attempts                int
	calendarID              string
}

func (r pushRow) eventID() string {
	if r.extID == nil {
		return ""
	}
	return strings.TrimPrefix(*r.extID, "calendar:")
}

func setPending(ctx context.Context, q querier, id int64, reason string, backoff time.Duration) error {
	if backoff > 0 {
		_, err := q.Exec(ctx,
			`UPDATE deliveries SET push_pending_reason=$2, push_next_attempt_at=now()+$3::interval, updated_at=now() WHERE id=$1`,
			id, reason, fmt.Sprintf("%d seconds", int(backoff.Seconds())))
		if err != nil {
			return fmt.Errorf("set pending %s on %d: %w", reason, id, err)
		}
		return nil
	}
	if _, err := q.Exec(ctx,
		`UPDATE deliveries SET push_pending_reason=$2, updated_at=now() WHERE id=$1 AND push_pending_reason IS DISTINCT FROM $2`,
		id, reason); err != nil {
		return fmt.Errorf("set pending %s on %d: %w", reason, id, err)
	}
	return nil
}

// verifiedAbsent is the same-id retry's precondition: a read poll of the
// row's account STARTED after the failed attempt and finished ok, and the
// event was not observed (an observed event would have confirmed the row and
// taken it out of the inbox, and would have a raw row).
func verifiedAbsent(ctx context.Context, q querier, r pushRow) (bool, error) {
	if r.attemptedAt == nil || r.extID == nil {
		return false, nil
	}
	var ok bool
	err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM sync_runs
		                 WHERE source_account_id=$1 AND status='ok' AND stats->>'phase'='calendar'
		                   AND finished_at IS NOT NULL AND started_at > $2)
		    AND NOT EXISTS (SELECT 1 FROM raw_source_items
		                     WHERE source_account_id=$1 AND external_id=$3 AND superseded_at IS NULL)`,
		r.fromAccount, *r.attemptedAt, *r.extID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("verify absence of %s: %w", *r.extID, err)
	}
	return ok, nil
}

// pushCalendarReservations is the push half of the stage (criteria 19-21).
func pushCalendarReservations(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a pushLimitArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	if calendarBooker == nil {
		return nil, fmt.Errorf("no calendar booking adapter wired (SetCalendarBooker; is PIPEDREAM_CALENDAR_URL configured?)")
	}
	stats := map[string]int{}

	// Offers sent with windows but without hold rows (a best-effort insert at
	// send failed) are repaired first, so no offered window is ever lost.
	if n, err := repairMissingHolds(ctx, pool); err != nil {
		return nil, err
	} else if n > 0 {
		stats["holds_repaired"] = n
	}

	rows, err := pool.Query(ctx,
		`SELECT id, task_id, from_account_id, status, sent_external_id, send_attempted_at,
		        starts_at, ends_at, hold_for_delivery_id IS NOT NULL, push_attempts, target_ref
		   FROM deliveries WHERE `+PushInboxSQL+`
		  ORDER BY push_next_attempt_at, id LIMIT $1`, a.effective())
	if err != nil {
		return nil, fmt.Errorf("select push inbox: %w", err)
	}
	var due []pushRow
	for rows.Next() {
		var r pushRow
		if err := rows.Scan(&r.id, &r.taskID, &r.fromAccount, &r.status, &r.extID, &r.attemptedAt,
			&r.start, &r.end, &r.isHold, &r.attempts, &r.calendarID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan push inbox: %w", err)
		}
		due = append(due, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(due) == 0 {
		return marshalResult(map[string]any{"due": 0, "stats": stats})
	}

	// Cheap DB checks first: none spends a credit, each re-checks every pass.
	var eligible []pushRow
	for _, r := range due {
		target, targetEmail, terr := reservationTarget(ctx, pool, r.taskID)
		if terr != nil || target != r.fromAccount || !strings.EqualFold(strings.TrimSpace(r.calendarID), targetEmail) {
			// Criterion 31: never pushed to a calendar the task's project does
			// not dictate — a re-mapped project or a hand-edited row is held.
			// BOTH columns must agree with the project: from_account_id (the
			// write gate below) and target_ref (what a hand edit could aim).
			if err := setPending(ctx, pool, r.id, "target_mismatch", gateRecheck); err != nil {
				return nil, err
			}
			stats["target_mismatch"]++
			continue
		}
		// What goes on the wire is the project's calendar as resolved NOW,
		// never the row's stored spelling.
		r.calendarID = targetEmail
		var writeEnabled bool
		if err := pool.QueryRow(ctx, `SELECT calendar_write_enabled FROM source_accounts WHERE id=$1`, r.fromAccount).
			Scan(&writeEnabled); err != nil {
			return nil, fmt.Errorf("read write gate of account %d: %w", r.fromAccount, err)
		}
		if !writeEnabled {
			// No silent fallback (criterion 32): it waits for THIS calendar.
			if err := setPending(ctx, pool, r.id, "account_not_write_enabled", gateRecheck); err != nil {
				return nil, err
			}
			stats["account_not_write_enabled"]++
			continue
		}
		if r.status != "approved" {
			ok, err := verifiedAbsent(ctx, pool, r)
			if err != nil {
				return nil, err
			}
			if !ok {
				if _, err := pool.Exec(ctx,
					`UPDATE deliveries SET push_next_attempt_at=now()+$2::interval, updated_at=now() WHERE id=$1`,
					r.id, fmt.Sprintf("%d seconds", int(gateRecheck.Seconds()))); err != nil {
					return nil, fmt.Errorf("defer retry %d: %w", r.id, err)
				}
				stats["awaiting_verified_absence"]++
				continue
			}
		}
		eligible = append(eligible, r)
	}
	if len(eligible) == 0 {
		return marshalResult(map[string]any{"due": len(due), "stats": stats})
	}
	// The channel's hourly limit counts ROWS (policy.Decide), and one batch
	// carries many: cap the batch at what is left this hour, so a push never
	// overshoots it; the rest wait for the next sweep.
	var sentLastHour int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM deliveries WHERE channel='calendar' AND status IN ('sent','sending')
		    AND COALESCE(sent_at, send_attempted_at) >= now() - interval '1 hour'`).Scan(&sentLastHour); err != nil {
		return nil, fmt.Errorf("count calendar sends this hour: %w", err)
	}
	if room := policy.HourlyLimit() - sentLastHour; room < len(eligible) {
		if room <= 0 {
			stats["hourly_limit"] = len(eligible)
			return marshalResult(map[string]any{"due": len(due), "stats": stats})
		}
		stats["hourly_limit"] = len(eligible) - room
		eligible = eligible[:room]
	}
	budget, err := calendarWriteBudget()
	if err != nil {
		return nil, err
	}

	// Reserve under the allocation lock: per-row fail-closed pre-flight
	// against everything except this batch, then one shared attempt stamp.
	_, maxSyncAge, err := availabilityConfig()
	if err != nil {
		return nil, err
	}
	var survivors []pushRow
	err = inTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, calendarBookingLock); err != nil {
			return fmt.Errorf("acquire calendar booking lock: %w", err)
		}
		now := time.Now()
		for _, r := range eligible {
			// CLAIM: lock the row and re-check the whole inbox predicate under
			// the lock (codex review). cancel_reservation and
			// release_offer_holds lock the same row, so a release that won
			// the race is seen here and the row is never pushed after it.
			var claimed int64
			err := tx.QueryRow(ctx, `SELECT id FROM deliveries WHERE id=$1 AND `+PushInboxSQL+` FOR UPDATE`, r.id).Scan(&claimed)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return fmt.Errorf("claim reservation %d: %w", r.id, err)
			}
			busy, err := availability.LoadBusy(ctx, pool, availability.Request{
				WindowStart: r.start, WindowEnd: r.end, Now: now, MaxSyncAge: maxSyncAge,
				HorizonPast: google.CalendarWindowPast, HorizonFuture: google.CalendarWindowFuture,
				// Only itself: the batch's OTHER rows are reservations too, and
				// two overlapping rows must not both pass in one batch.
				ExcludeDeliveryIDs: []int64{r.id},
			})
			var notReady *availability.NotReadyError
			if errors.As(err, &notReady) || (err != nil && strings.HasPrefix(err.Error(), "calendar not ready")) {
				if err := setPending(ctx, tx, r.id, "calendar_stale", gateRecheck); err != nil {
					return err
				}
				stats["calendar_stale"]++
				continue
			}
			if err != nil {
				return err
			}
			if cerr := overlapError(r.start, r.end, busy); cerr != nil {
				// TERMINAL: the slot was taken while the reservation waited.
				// Out of the busy set (push_pending_reason='conflict'), logged
				// on the task so the person who booked it finds out.
				if _, err := tx.Exec(ctx,
					`UPDATE deliveries SET status='failed', push_pending_reason='conflict', error=$2, updated_at=now() WHERE id=$1`,
					r.id, cerr.Error()); err != nil {
					return fmt.Errorf("mark conflict %d: %w", r.id, err)
				}
				if _, err := insertTaskEvent(ctx, tx, r.taskID, "log", map[string]any{
					"delivery_id": r.id, "message": "calendar reservation dropped at push: the slot is no longer free",
					"error": cerr.Error()}); err != nil {
					return err
				}
				stats["conflict"]++
				continue
			}
			survivors = append(survivors, r)
		}
		if len(survivors) == 0 {
			return nil
		}
		// Count the invocation ONLY now that something will be dispatched, in
		// this same transaction (codex review): a pass whose rows all dropped
		// out (stale, conflict) spends nothing.
		weight := 1
		if len(survivors) == 1 {
			weight = 2
		}
		admitted, used, err := admitCalendarWrite(ctx, tx, budget, weight, true)
		if err != nil {
			return err
		}
		if !admitted {
			for _, r := range survivors {
				if err := setPending(ctx, tx, r.id, "credit_budget", gateRecheck); err != nil {
					return err
				}
			}
			stats["credit_budget"] = len(survivors)
			stats["writes_this_month"] = used
			survivors = nil
			return nil
		}
		for i := range survivors {
			r := &survivors[i]
			ext := ""
			if r.extID != nil {
				ext = *r.extID
			} else {
				ext = google.CalendarExternalID(google.CalendarEventID(r.id, time.Now().UnixNano()))
			}
			if _, err := tx.Exec(ctx,
				`UPDATE deliveries SET status='sending', sent_external_id=$2, send_attempted_at=now(), updated_at=now()
				  WHERE id=$1`, r.id, ext); err != nil {
				return fmt.Errorf("reserve %d: %w", r.id, err)
			}
			r.extID = &ext
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(survivors) == 0 {
		return marshalResult(map[string]any{"due": len(due), "stats": stats})
	}

	results := sendReservationBatch(ctx, survivors)
	for _, r := range survivors {
		res := results[r.eventID()]
		if res.err == nil {
			if err := finalizeCalendarSent(ctx, pool, r.id, r.taskID, r.fromAccount, *r.extID, res.event); err != nil {
				return nil, err
			}
			stats["pushed"]++
			continue
		}
		reason := "transport_error"
		if isCreditCap(res.err) {
			reason = "credit_cap"
		}
		if _, err := pool.Exec(ctx,
			`UPDATE deliveries SET status='failed', error=$2, push_pending_reason=$3, push_attempts=push_attempts+1,
			        push_next_attempt_at=now()+$4::interval, updated_at=now()
			  WHERE id=$1 AND status='sending'`,
			r.id, res.err.Error(), reason, fmt.Sprintf("%d seconds", int(pushBackoff(r.attempts).Seconds()))); err != nil {
			return nil, fmt.Errorf("record push failure %d: %w", r.id, err)
		}
		stats[reason]++
	}
	return marshalResult(map[string]any{"due": len(due), "stats": stats})
}

type pushResult struct {
	event json.RawMessage
	err   error
}

// reservationRequest builds a reservation's Google event from its kind and
// interval ONLY (criterion 32): the summary and description are fixed text,
// and nothing about the row's label can reach the wire.
func reservationRequest(r pushRow) google.CalendarEventSpec {
	summary, description := reservationText(r.isHold)
	return google.CalendarEventSpec{
		CalendarID: r.calendarID, EventID: r.eventID(),
		Start: r.start.UTC().Format(time.RFC3339), End: r.end.UTC().Format(time.RFC3339),
		Summary: summary, Description: description,
	}
}

// sendReservationBatch is the network call: one row sends the unchanged
// create_event request, more send one create_events.
func sendReservationBatch(ctx context.Context, rows []pushRow) map[string]pushResult {
	out := map[string]pushResult{}
	callCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if len(rows) == 1 {
		spec := reservationRequest(rows[0])
		resp, err := calendarBooker.CreateEvent(callCtx, google.CreateEventRequest{
			SchemaVersion: google.PipedreamCalendarSchemaVersion, Action: "create_event",
			CalendarID: spec.CalendarID, EventID: spec.EventID, Start: spec.Start, End: spec.End,
			Summary: spec.Summary, Description: spec.Description,
		})
		out[spec.EventID] = pushResult{event: resp.Event, err: err}
		return out
	}
	req := google.CreateEventsRequest{SchemaVersion: google.PipedreamCalendarSchemaVersion, Action: "create_events"}
	for _, r := range rows {
		req.Events = append(req.Events, reservationRequest(r))
	}
	resp, err := calendarBooker.CreateEvents(callCtx, req)
	if err != nil {
		for _, e := range req.Events {
			out[e.EventID] = pushResult{err: err}
		}
		return out
	}
	for _, r := range resp.Results {
		if r.Status == "ok" {
			out[r.EventID] = pushResult{event: r.Event}
		} else {
			out[r.EventID] = pushResult{err: fmt.Errorf("pipedream create_events: %s", r.Error)}
		}
	}
	return out
}

// finalizeCalendarSent is the shared success half of every calendar create:
// sent, the busy set learns the block at once (best-effort), and the
// delivery_sent event whose channel key R8's calendar skip reads.
func finalizeCalendarSent(ctx context.Context, pool *pgxpool.Pool, deliveryID, taskID, accountID int64, extID string, event json.RawMessage) error {
	tag, err := pool.Exec(ctx,
		`UPDATE deliveries SET status='sent', sent_at=now(), error=NULL, push_pending_reason=NULL, updated_at=now()
		  WHERE id=$1 AND status='sending'`, deliveryID)
	if err != nil {
		return fmt.Errorf("finalize sent %d: %w", deliveryID, err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if err := google.NewPGSink(pool).RecordOwnCalendarEvent(ctx, accountID, event); err != nil {
		_, _ = insertTaskEvent(ctx, pool, taskID, "log", map[string]any{
			"delivery_id": deliveryID,
			"message":     "calendar busy-set record failed; the block is live but invisible to propose_slots until the next read poll",
			"error":       err.Error(),
		})
	}
	_, err = insertTaskEvent(ctx, pool, taskID, "delivery_sent",
		map[string]any{"delivery_id": deliveryID, "channel": "calendar", "sent_external_id": extID})
	return err
}

// repairMissingHolds creates the hold rows of any sent offer that has windows
// and no holds (the insert at send is best-effort; this makes it eventual).
func repairMissingHolds(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	rows, err := pool.Query(ctx,
		`SELECT o.id FROM deliveries o WHERE `+missingHoldsSQL+` ORDER BY o.id LIMIT 20`)
	if err != nil {
		return 0, fmt.Errorf("select offers missing holds: %w", err)
	}
	var offers []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan offer missing holds: %w", err)
		}
		offers = append(offers, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate offers missing holds: %w", err)
	}
	n := 0
	for _, id := range offers {
		var made int
		err := inTx(ctx, pool, func(tx pgx.Tx) error {
			var err error
			made, err = ensureHoldRows(ctx, tx, id, "pipeline:calendar_push")
			return err
		})
		if err != nil {
			// One bad offer (no default calendar, an out-of-scope mapping)
			// must not stall every other push: log it and move on.
			slog.Warn("calendar_push: holds for a sent offer could not be created", "offer_delivery_id", id, "err", err)
			continue
		}
		n += made
	}
	return n, nil
}

// applyHoldReleases is the delete half (criteria 22-23).
func applyHoldReleases(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a pushLimitArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	rows, err := pool.Query(ctx,
		`SELECT d.id, d.task_id, a.account_email, d.sent_external_id, d.ends_at <= now(), d.push_attempts
		   FROM deliveries d JOIN source_accounts a ON a.id = d.from_account_id
		  WHERE d.id IN (SELECT id FROM deliveries WHERE `+ReleaseInboxSQL+`)
		  ORDER BY d.id LIMIT $1`, a.effective())
	if err != nil {
		return nil, fmt.Errorf("select release inbox: %w", err)
	}
	type rel struct {
		id, taskID int64
		calendarID string
		extID      *string
		past       bool
		attempts   int
	}
	var due []rel
	for rows.Next() {
		var r rel
		if err := rows.Scan(&r.id, &r.taskID, &r.calendarID, &r.extID, &r.past, &r.attempts); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan release inbox: %w", err)
		}
		due = append(due, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	stats := map[string]int{}
	released := func(r rel, how string) error {
		if _, err := pool.Exec(ctx, `UPDATE deliveries SET released_at=now(), updated_at=now() WHERE id=$1 AND released_at IS NULL`, r.id); err != nil {
			return fmt.Errorf("mark released %d: %w", r.id, err)
		}
		_, err := insertTaskEvent(ctx, pool, r.taskID, "reservation_released", map[string]any{"delivery_id": r.id, "how": how})
		stats[how]++
		return err
	}
	var call []rel
	for _, r := range due {
		if r.extID == nil || r.past {
			if err := released(r, "no_call"); err != nil {
				return nil, err
			}
			continue
		}
		call = append(call, r)
	}
	if len(call) == 0 {
		return marshalResult(map[string]any{"due": len(due), "stats": stats})
	}
	if calendarBooker == nil {
		return nil, fmt.Errorf("no calendar booking adapter wired (SetCalendarBooker; is PIPEDREAM_CALENDAR_URL configured?)")
	}
	budget, err := calendarWriteBudget()
	if err != nil {
		return nil, err
	}
	var admitted bool
	var used int
	if err := inTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		admitted, used, err = admitCalendarWrite(ctx, tx, budget, 1, true)
		return err
	}); err != nil {
		return nil, err
	}
	if !admitted {
		stats["credit_budget"] = len(call)
		return marshalResult(map[string]any{"due": len(due), "stats": stats, "writes_this_month": used, "budget": budget})
	}
	ids := make([]int64, 0, len(call))
	req := google.DeleteEventsRequest{SchemaVersion: google.PipedreamCalendarSchemaVersion, Action: "delete_events"}
	for _, r := range call {
		ids = append(ids, r.id)
		req.Events = append(req.Events, google.CalendarDeleteSpec{CalendarID: r.calendarID, EventID: strings.TrimPrefix(*r.extID, "calendar:")})
	}
	if _, err := pool.Exec(ctx, `UPDATE deliveries SET release_attempted_at=now(), updated_at=now() WHERE id = ANY($1)`, ids); err != nil {
		return nil, fmt.Errorf("stamp release attempt: %w", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, derr := calendarBooker.DeleteEvents(callCtx, req)
	byID := map[string]google.DeleteEventsResult{}
	for _, r := range resp.Results {
		byID[r.EventID] = r
	}
	for _, r := range call {
		ev := strings.TrimPrefix(*r.extID, "calendar:")
		res, ok := byID[ev]
		if derr == nil && ok && (res.Status == "deleted" || res.Status == "gone") {
			if err := released(r, res.Status); err != nil {
				return nil, err
			}
			continue
		}
		msg := res.Error
		if derr != nil {
			msg = derr.Error()
		}
		// Waits with backoff (a failed delete is a spent credit; retrying every
		// sweep would drain the month's budget); never writes deliveries.error
		// (that column is the create's).
		if _, err := pool.Exec(ctx,
			`UPDATE deliveries SET push_attempts=push_attempts+1, push_next_attempt_at=now()+$2::interval, updated_at=now() WHERE id=$1`,
			r.id, fmt.Sprintf("%d seconds", int(pushBackoff(r.attempts).Seconds()))); err != nil {
			return nil, fmt.Errorf("defer delete %d: %w", r.id, err)
		}
		if _, err := insertTaskEvent(ctx, pool, r.taskID, "log", map[string]any{
			"delivery_id": r.id, "message": "calendar reservation delete failed; retrying next pass", "error": msg}); err != nil {
			return nil, err
		}
		stats["delete_error"]++
	}
	return marshalResult(map[string]any{"due": len(due), "stats": stats})
}
