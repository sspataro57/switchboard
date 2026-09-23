package policy

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

// pgSnapshotLoader gathers the delivery-policy snapshot from Postgres.
type pgSnapshotLoader struct {
	pool *pgxpool.Pool
}

// toolChannel pins the snapshot channel for send-shaped verbs that insert
// their own delivery row, so there is no delivery_id to resolve it from.
var toolChannel = map[string]string{"send_slack_reply": "slack_reply"}

func NewPGSnapshotLoader(pool *pgxpool.Pool) SnapshotLoader {
	return &pgSnapshotLoader{pool: pool}
}

func (l *pgSnapshotLoader) Load(ctx context.Context, req Request) (Snapshot, error) {
	snap := Snapshot{SentLastHour: map[string]int{}, HourlyLimit: HourlyLimit()}

	// kill switch
	var frozen *bool
	err := l.pool.QueryRow(ctx,
		`SELECT (value->>'frozen')::boolean FROM ops_flags WHERE name='sending_frozen'`).Scan(&frozen)
	if err == nil && frozen != nil {
		snap.SendingFrozen = *frozen
	} // absent row / NULL means not frozen

	// the delivery's channel. A verb that creates its own row has no
	// delivery_id at policy time, so its channel is pinned by tool name — and
	// the pin wins over any delivery_id in the args, so a caller cannot change
	// the channel it is judged on (SWT-77 D5).
	if ch, ok := toolChannel[req.Tool]; ok {
		snap.Channel = ch
	} else if id := deliveryIDArgs(req.Args); id != 0 {
		if err := l.pool.QueryRow(ctx,
			`SELECT channel FROM deliveries WHERE id=$1`, id).Scan(&snap.Channel); err != nil {
			return snap, fmt.Errorf("resolve delivery %d channel: %w", id, err)
		}
	}

	// Per-channel sends in the last hour. 'sending' counts too, and the window
	// falls back to send_attempted_at: a Slack click that landed but answered
	// ambiguously stays 'sending' with sent_at NULL forever, so counting only
	// 'sent' let every degraded send consume no allowance and real traffic exceed
	// the limit indefinitely. Counting an attempt that turns out not to have sent
	// is the safe direction for a rate limit.
	rows, err := l.pool.Query(ctx,
		`SELECT channel, count(*) FROM deliveries
		 WHERE status IN ('sent','sending')
		   AND COALESCE(sent_at, send_attempted_at) >= now() - interval '1 hour'
		 GROUP BY channel`)
	if err != nil {
		return snap, fmt.Errorf("count recent sends: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ch string
		var n int
		if err := rows.Scan(&ch, &n); err != nil {
			return snap, fmt.Errorf("scan recent sends: %w", err)
		}
		snap.SentLastHour[ch] = n
	}
	return snap, rows.Err()
}

// HourlyLimit is the per-channel send limit (OPS_SEND_HOURLY_LIMIT, default
// 10). EXPORTED since SWT-77: the Slack send half re-counts against it under a
// lock at send time, and a restated default would drift from the snapshot's.
func HourlyLimit() int {
	if v := os.Getenv("OPS_SEND_HOURLY_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 10
}
