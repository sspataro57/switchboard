package availability

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Request is everything LoadBusy needs that is not in the database. Now,
// MaxSyncAge and the horizon are INJECTED, never read here: the package doc
// forbids clock and env reads (SWT-24 criterion 10), AVAIL_MAX_SYNC_AGE is
// read only in tools.availabilityConfig (criterion 11), and the horizon is
// google.CalendarWindowPast/Future passed by the tool wiring (criterion 7) —
// this package must not import a provider adapter.
type Request struct {
	WindowStart, WindowEnd time.Time
	Now                    time.Time
	MaxSyncAge             time.Duration
	HorizonPast            time.Duration
	HorizonFuture          time.Duration
	// ExcludeDeliveryIDs drops these reservations from the busy set (SWT-109
	// criterion 9): the push stage's pre-flight checks a batch against
	// everything EXCEPT the batch's own rows, which are reservations already.
	ExcludeDeliveryIDs []int64
}

// LoadBusy is the ONE database-backed entry point to free/busy (SWT-24
// criterion 1). It refuses BEFORE it reads a single row of normalized_events:
//
//   - a window not fully inside [Now-HorizonPast, Now+HorizonFuture] — nothing
//     was ever fetched there, so an empty busy set would mean "we never
//     looked", not "you are free" (criterion 7);
//   - any in-scope account without a fresh successful calendar sync, and the
//     empty scope itself — "no calendar is in availability scope" is not "you
//     have no meetings" (criterion 5).
//
// Only after every account is vouched for does it read events, and a READY
// account with zero events is answered, not refused: the discriminator is the
// freshness of the SYNC, never the count of events (criterion 6).
func LoadBusy(ctx context.Context, pool *pgxpool.Pool, req Request) ([]Interval, error) {
	if err := checkReady(ctx, pool, req); err != nil {
		return nil, err
	}

	events, err := loadEvents(ctx, pool, req.WindowStart, req.WindowEnd)
	if err != nil {
		return nil, err
	}
	reserved, err := loadReservationRows(ctx, pool, req.WindowStart, req.WindowEnd, req.ExcludeDeliveryIDs)
	if err != nil {
		return nil, err
	}
	busy := Busy(events)
	for _, r := range reserved {
		busy = append(busy, Interval{Start: r.Start, End: r.End})
	}
	return Merge(busy), nil
}

// checkReady is the readiness half LoadBusy and LoadAgenda SHARE (SWT-109
// criterion 2): the horizon, the empty scope and every in-scope account's last
// successful sync. Both callers run it before their first read of
// normalized_events; callsites_test.go pins the order.
func checkReady(ctx context.Context, pool *pgxpool.Pool, req Request) error {
	if req.WindowStart.Before(req.Now.Add(-req.HorizonPast)) ||
		req.WindowEnd.After(req.Now.Add(req.HorizonFuture)) {
		return fmt.Errorf(
			"calendar not ready: window %s..%s is not fully inside the synced horizon [now-%s, now+%s]; "+
				"nothing was ever fetched for that span, so an empty busy set there means \"we never looked\"",
			req.WindowStart.Format(time.RFC3339), req.WindowEnd.Format(time.RFC3339),
			req.HorizonPast, req.HorizonFuture)
	}

	states, err := CalendarSyncStates(ctx, pool)
	if err != nil {
		return err
	}
	if len(states) == 0 {
		return &NotReadyError{MaxAge: req.MaxSyncAge}
	}
	if offenders := NotReady(states, req.Now, req.MaxSyncAge); len(offenders) > 0 {
		return &NotReadyError{Accounts: offenders, MaxAge: req.MaxSyncAge}
	}
	return nil
}

// Reservation is one live switchboard reservation: a channel='calendar'
// deliveries row that holds its interval (SWT-28 for pushed rows, SWT-109 for
// swb-first ones).
type Reservation struct {
	DeliveryID, TaskID int64
	Start, End         time.Time
	Account            string // the target calendar's account email
	Status             string
	Label              string
	ExternalID         string // sent_external_id, "" before a push
	PendingReason      string
	Verified           bool
	HoldFor            int64 // the offer delivery a hold belongs to; 0 for a booking
	HoldExpiresAt      *time.Time
	Kept               bool
}

// reservationSelect is the busy set (SWT-109 criterion 9; SWT-28 before it).
// A row reserves its interval when it is not released and not a terminal
// conflict, and EITHER it is a swb-first reservation booked in switchboard and
// not on its way out (approved with push_next_attempt_at set — only book_time
// and offer holds set it; an SWT-28 draft approved by book_calendar_block is
// not a reservation until its own send reserves it — no release requested) OR it may be on Google and is not yet
// observed there (sending/sent/failed with an id, unconfirmed). A pushed row
// whose release is requested stays busy until the delete lands — over-busy is
// the direction that cannot double-book. Confirmed rows drop out: the observed
// event takes over, so a block deleted by hand frees its slot at the next poll.
const reservationSelect = `SELECT d.id, d.task_id, d.starts_at, d.ends_at, a.account_email, d.status,
		        COALESCE(d.reservation_label,''), COALESCE(d.sent_external_id,''),
		        COALESCE(d.push_pending_reason,''), COALESCE(d.booking_verified, true),
		        COALESCE(d.hold_for_delivery_id, 0), d.hold_expires_at, d.hold_kept_at IS NOT NULL
		   FROM deliveries d
		   JOIN source_accounts a ON a.id = d.from_account_id
		  WHERE d.channel = 'calendar'
		    AND d.released_at IS NULL
		    AND d.push_pending_reason IS DISTINCT FROM 'conflict'
		    AND ((d.status = 'approved' AND d.push_next_attempt_at IS NOT NULL AND d.release_requested_at IS NULL)
		      OR (d.status IN ('sending','sent','failed') AND d.sent_external_id IS NOT NULL
		          AND d.confirmed_at IS NULL))
		    AND a.provider = 'google' AND a.calendar_in_availability
		    AND d.ends_at > $1 AND d.starts_at < $2
		    AND NOT (d.id = ANY($3::bigint[]))
		  ORDER BY d.starts_at, d.id`

// LoadReservations returns switchboard's own live reservations in the window
// WITHOUT touching normalized_events (SWT-109 criterion 9). It is not a
// free/busy answer — Google events are missing from it — so its callers are
// the ones that must still decide when the calendar copy is stale: book_time
// checks it alone on NotReadyError and marks the booking unverified, and the
// push re-verifies with the full LoadBusy before any write.
func LoadReservations(ctx context.Context, pool *pgxpool.Pool, windowStart, windowEnd time.Time) ([]Reservation, error) {
	return loadReservationRows(ctx, pool, windowStart, windowEnd, nil)
}

func loadReservationRows(ctx context.Context, pool *pgxpool.Pool, windowStart, windowEnd time.Time, exclude []int64) ([]Reservation, error) {
	if exclude == nil {
		exclude = []int64{}
	}
	rows, err := pool.Query(ctx, reservationSelect, windowStart, windowEnd, exclude)
	if err != nil {
		return nil, fmt.Errorf("select calendar reservations: %w", err)
	}
	defer rows.Close()
	var out []Reservation
	for rows.Next() {
		var r Reservation
		if err := rows.Scan(&r.DeliveryID, &r.TaskID, &r.Start, &r.End, &r.Account, &r.Status,
			&r.Label, &r.ExternalID, &r.PendingReason, &r.Verified,
			&r.HoldFor, &r.HoldExpiresAt, &r.Kept); err != nil {
			return nil, fmt.Errorf("scan calendar reservation: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CalendarSyncStates is the SQL half of the readiness check: the in-scope rows
// (provider='google' AND calendar_in_availability — the same two predicates
// loadEvents applies, criterion 3) with each one's last SUCCESSFUL calendar
// sync. The discriminators live in Postgres columns on purpose; the decision
// over the result is the pure NotReady.
//
// EXPORTED since SWT-29: the /funnel page judges calendar rows with the same
// rows and the same NotReady predicate propose_slots uses, so the page cannot
// say green while the tool refuses. This is NOT the door SWT-24 closed — it
// reads sync_runs only and produces no free/busy answer; loadEvents stays
// unexported and LoadBusy stays the one entry to the busy set.
func CalendarSyncStates(ctx context.Context, pool *pgxpool.Pool) ([]AccountState, error) {
	rows, err := pool.Query(ctx,
		`SELECT a.id, a.account_email, MAX(r.finished_at)
		   FROM source_accounts a
		   LEFT JOIN sync_runs r
		     ON r.source_account_id = a.id
		    AND r.status = 'ok'
		    AND r.stats->>'phase' = 'calendar'
		    AND r.finished_at IS NOT NULL
		  WHERE a.provider = 'google' AND a.calendar_in_availability
		  GROUP BY a.id, a.account_email
		  ORDER BY a.id`)
	if err != nil {
		return nil, fmt.Errorf("select calendar account states: %w", err)
	}
	defer rows.Close()

	var out []AccountState
	for rows.Next() {
		var s AccountState
		var last *time.Time
		if err := rows.Scan(&s.AccountID, &s.Email, &last); err != nil {
			return nil, fmt.Errorf("scan calendar account state: %w", err)
		}
		if last != nil {
			s.LastCalendarSync = *last
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// loadEvents is the one normalized_events read: events joined to their
// account's calendar_in_availability flag, within the window. UNEXPORTED
// (SWT-24 criterion 1): it performs no readiness check, so any caller holding
// it can be told "you are free all week" by an empty table. LoadBusy is the
// only door. The busy filter itself is pure (Busy).
func loadEvents(ctx context.Context, pool *pgxpool.Pool, windowStart, windowEnd time.Time) ([]Event, error) {
	rows, err := pool.Query(ctx,
		`SELECT e.starts_at, e.ends_at, COALESCE(e.status,''), COALESCE(e.transparency,'opaque'),
		        a.calendar_in_availability
		 FROM normalized_events e
		 JOIN raw_source_items r ON r.id = e.raw_source_item_id
		 JOIN source_accounts a ON a.id = r.source_account_id
		 WHERE a.provider='google'
		   -- Defence in depth: a superseded observation is one a Calendar reset
		   -- said no longer exists. Its event is marked cancelled too, but
		   -- free/busy must never depend on that second write having landed.
		   AND r.superseded_at IS NULL
		   AND e.starts_at IS NOT NULL AND e.ends_at IS NOT NULL
		   AND e.ends_at > $1 AND e.starts_at < $2`,
		windowStart, windowEnd)
	if err != nil {
		return nil, fmt.Errorf("select busy events: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Start, &e.End, &e.Status, &e.Transparency, &e.InAvailability); err != nil {
			return nil, fmt.Errorf("scan busy event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AgendaEvent is one Google event for the read-only agenda (SWT-109).
type AgendaEvent struct {
	Title        string
	Start, End   time.Time
	AllDay       bool
	Account      string
	Transparency string
	ExternalID   string // raw_source_items.external_id; matches a pushed reservation's sent_external_id
}

// AgendaAccount is one in-scope calendar with its freshness and write gate.
type AgendaAccount struct {
	AccountState
	WriteEnabled bool
}

// Agenda is LoadAgenda's answer: every in-scope account, the Google events in
// the window (cancelled and superseded excluded), and switchboard's live
// reservations. Rendering (time zone, merging a reservation into its observed
// event, the 300 cap) is the caller's.
type Agenda struct {
	Accounts     []AgendaAccount
	Events       []AgendaEvent
	Reservations []Reservation
}

// AgendaEventLimit bounds one read: one more than the rendered cap, so the
// caller can say "truncated" honestly.
const AgendaEventLimit = 301

// LoadAgenda is the agenda read (SWT-109 criterion 2). It runs the SAME
// readiness check as LoadBusy, shared rather than copied, before its first
// read of normalized_events: a stale or empty calendar refuses with LoadBusy's
// exact error and returns no events. There is no override.
func LoadAgenda(ctx context.Context, pool *pgxpool.Pool, req Request) (Agenda, error) {
	if err := checkReady(ctx, pool, req); err != nil {
		return Agenda{}, err
	}
	var ag Agenda

	states, err := CalendarSyncStates(ctx, pool)
	if err != nil {
		return Agenda{}, err
	}
	write := map[int64]bool{}
	rows, err := pool.Query(ctx,
		`SELECT id, calendar_write_enabled FROM source_accounts
		  WHERE provider = 'google' AND calendar_in_availability`)
	if err != nil {
		return Agenda{}, fmt.Errorf("select calendar write gates: %w", err)
	}
	for rows.Next() {
		var id int64
		var w bool
		if err := rows.Scan(&id, &w); err != nil {
			rows.Close()
			return Agenda{}, fmt.Errorf("scan calendar write gate: %w", err)
		}
		write[id] = w
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Agenda{}, err
	}
	for _, s := range states {
		ag.Accounts = append(ag.Accounts, AgendaAccount{AccountState: s, WriteEnabled: write[s.AccountID]})
	}

	if ag.Events, err = loadAgendaEvents(ctx, pool, req.WindowStart, req.WindowEnd); err != nil {
		return Agenda{}, err
	}
	if ag.Reservations, err = loadReservationRows(ctx, pool, req.WindowStart, req.WindowEnd, req.ExcludeDeliveryIDs); err != nil {
		return Agenda{}, err
	}
	return ag, nil
}

// loadAgendaEvents is the agenda's normalized_events read. Unexported for the
// reason loadEvents is: it performs no readiness check, so LoadAgenda is its
// only caller (callsites_test.go pins that the check runs first).
func loadAgendaEvents(ctx context.Context, pool *pgxpool.Pool, windowStart, windowEnd time.Time) ([]AgendaEvent, error) {
	rows, err := pool.Query(ctx,
		`SELECT COALESCE(e.title,''), e.starts_at, e.ends_at, e.all_day, a.account_email,
		        COALESCE(e.transparency,'opaque'), r.external_id
		   FROM normalized_events e
		   JOIN raw_source_items r ON r.id = e.raw_source_item_id
		   JOIN source_accounts a ON a.id = r.source_account_id
		  WHERE a.provider = 'google' AND a.calendar_in_availability
		    AND r.superseded_at IS NULL
		    AND COALESCE(e.status,'') <> 'cancelled'
		    AND e.starts_at IS NOT NULL AND e.ends_at IS NOT NULL
		    AND e.ends_at > $1 AND e.starts_at < $2
		  ORDER BY e.starts_at, e.id
		  LIMIT $3`,
		windowStart, windowEnd, AgendaEventLimit)
	if err != nil {
		return nil, fmt.Errorf("select agenda events: %w", err)
	}
	defer rows.Close()
	var out []AgendaEvent
	for rows.Next() {
		var e AgendaEvent
		if err := rows.Scan(&e.Title, &e.Start, &e.End, &e.AllDay, &e.Account, &e.Transparency, &e.ExternalID); err != nil {
			return nil, fmt.Errorf("scan agenda event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
