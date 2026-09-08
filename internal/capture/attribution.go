package capture

// AttributionTrend is capture's OWN spelling of "the latest decision for this
// message", exported for the /funnel page (SWT-29 criteria 14-16). The
// dashboard adds no SQL that picks a message's newest capture_decisions row.
//
// THREE states, never two. No decision row is UNSEEN — the engine has not
// looked — which is neither matched nor the residue; conflating it with
// unmatched is the named capture trap. The LEFT join is what keeps the third
// state a state instead of a silent merge, and `cd.id DESC` is the same
// "latest" latestDecisions and triage's project lookup use — shadow is
// re-runnable by design, so a message routinely carries several decisions and
// anything but the newest makes the numbers depend on how many times someone
// ran the pass.
//
// Inbound only (criterion 15): rules_store.go filters direction='inbound' —
// that line IS invariant 5 here — so an OUTBOUND message can never carry a
// decision on any pass in any mode. Its absence is absent-because-impossible,
// and counting it as "not yet evaluated" would be a lie about 21k rows.
//
// The fold keys on `action`, positively spelled — never on the NULLness of
// the project column. 0015's CHECK ties the two together as a schema fact
// today, and a predicate that reads one column to infer another's value is
// the constant-discriminator landmine waiting for the CHECK to change.

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DayAttribution is one day of the trend, bucketed on the PIPELINE clock
// (normalized_messages.created_at) — the axis every windowed /funnel section
// shares. sent_at is the provider's clock and answers a different question.
type DayAttribution struct {
	Day         time.Time
	Matched     int
	Unmatched   int
	Unevaluated int
}

// AttributionTrend buckets INBOUND normalized_messages by created_at::date
// over the last `days` days by the LATEST capture decision for each message.
// Rows are one per day that has messages, newest first; the caller renders
// gaps (the /funnel page zero-fills like its intake section).
func AttributionTrend(ctx context.Context, pool *pgxpool.Pool, days int) ([]DayAttribution, error) {
	rows, err := pool.Query(ctx, `
		SELECT nm.created_at::date AS day,
		       count(*) FILTER (WHERE latest.action IS NOT NULL AND latest.action <> 'unmatched') AS matched,
		       count(*) FILTER (WHERE latest.action = 'unmatched') AS unmatched,
		       count(*) FILTER (WHERE latest.action IS NULL) AS unevaluated
		  FROM normalized_messages nm
		  LEFT JOIN LATERAL (SELECT cd.action FROM capture_decisions cd
		                      WHERE cd.message_id = nm.id
		                      ORDER BY cd.id DESC LIMIT 1) latest ON true
		 WHERE nm.direction = 'inbound'
		   AND nm.created_at >= now() - make_interval(days => $1)
		 GROUP BY 1 ORDER BY 1 DESC`, days)
	if err != nil {
		return nil, fmt.Errorf("select attribution trend: %w", err)
	}
	defer rows.Close()

	var out []DayAttribution
	for rows.Next() {
		var d DayAttribution
		if err := rows.Scan(&d.Day, &d.Matched, &d.Unmatched, &d.Unevaluated); err != nil {
			return nil, fmt.Errorf("scan attribution day: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
