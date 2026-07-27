package availability

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// LoadEvents is the one SQL read: normalized events joined to their account's
// calendar_in_availability flag, within the window. The busy filter itself is
// pure (Busy).
func LoadEvents(ctx context.Context, pool *pgxpool.Pool, windowStart, windowEnd time.Time) ([]Event, error) {
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
