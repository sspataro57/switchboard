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
	if req.WindowStart.Before(req.Now.Add(-req.HorizonPast)) ||
		req.WindowEnd.After(req.Now.Add(req.HorizonFuture)) {
		return nil, fmt.Errorf(
			"calendar not ready: window %s..%s is not fully inside the synced horizon [now-%s, now+%s]; "+
				"nothing was ever fetched for that span, so an empty busy set there means \"we never looked\"",
			req.WindowStart.Format(time.RFC3339), req.WindowEnd.Format(time.RFC3339),
			req.HorizonPast, req.HorizonFuture)
	}

	states, err := loadAccountStates(ctx, pool)
	if err != nil {
		return nil, err
	}
	if len(states) == 0 {
		return nil, &NotReadyError{MaxAge: req.MaxSyncAge}
	}
	if offenders := NotReady(states, req.Now, req.MaxSyncAge); len(offenders) > 0 {
		return nil, &NotReadyError{Accounts: offenders, MaxAge: req.MaxSyncAge}
	}

	events, err := loadEvents(ctx, pool, req.WindowStart, req.WindowEnd)
	if err != nil {
		return nil, err
	}
	reserved, err := loadReservations(ctx, pool, req.WindowStart, req.WindowEnd)
	if err != nil {
		return nil, err
	}
	return Merge(append(Busy(events), reserved...)), nil
}

// loadReservations treats an UNCONFIRMED calendar booking as busy (SWT-28,
// codex finding 2026-09-07): between reserving its event id and the first
// poll that OBSERVES the created event, a booked block may exist on the
// calendar while nothing in normalized_events says so — a stale snapshot can
// even supersede the send-time record. The delivery row itself is therefore
// the reservation: sending (mid-flight), sent (live, awaiting observation)
// and failed-with-id (the send MAY have landed — over-busy, the direction
// that cannot double-book) all hold their interval until confirmed_at, when
// the observed event takes over. Confirmed rows drop out so a block deleted
// by hand frees its slot at the next poll, exactly as before.
func loadReservations(ctx context.Context, pool *pgxpool.Pool, windowStart, windowEnd time.Time) ([]Interval, error) {
	rows, err := pool.Query(ctx,
		`SELECT d.starts_at, d.ends_at
		   FROM deliveries d
		   JOIN source_accounts a ON a.id = d.from_account_id
		  WHERE d.channel = 'calendar'
		    AND d.sent_external_id IS NOT NULL
		    AND d.confirmed_at IS NULL
		    AND d.status IN ('sending','sent','failed')
		    AND a.provider = 'google' AND a.calendar_in_availability
		    AND d.ends_at > $1 AND d.starts_at < $2`,
		windowStart, windowEnd)
	if err != nil {
		return nil, fmt.Errorf("select calendar reservations: %w", err)
	}
	defer rows.Close()
	var out []Interval
	for rows.Next() {
		var iv Interval
		if err := rows.Scan(&iv.Start, &iv.End); err != nil {
			return nil, fmt.Errorf("scan calendar reservation: %w", err)
		}
		out = append(out, iv)
	}
	return out, rows.Err()
}

// loadAccountStates is the SQL half of the readiness check: the in-scope rows
// (provider='google' AND calendar_in_availability — the same two predicates
// loadEvents applies, criterion 3) with each one's last SUCCESSFUL calendar
// sync. The discriminators live in Postgres columns on purpose; the decision
// over the result is the pure NotReady.
func loadAccountStates(ctx context.Context, pool *pgxpool.Pool) ([]AccountState, error) {
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
