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

func NewPGSnapshotLoader(pool *pgxpool.Pool) SnapshotLoader {
	return &pgSnapshotLoader{pool: pool}
}

func (l *pgSnapshotLoader) Load(ctx context.Context, req Request) (Snapshot, error) {
	snap := Snapshot{SentLastHour: map[string]int{}, HourlyLimit: hourlyLimit()}

	// kill switch
	var frozen *bool
	err := l.pool.QueryRow(ctx,
		`SELECT (value->>'frozen')::boolean FROM ops_flags WHERE name='sending_frozen'`).Scan(&frozen)
	if err == nil && frozen != nil {
		snap.SendingFrozen = *frozen
	} // absent row / NULL means not frozen

	// the delivery's channel
	if id := deliveryIDArgs(req.Args); id != 0 {
		if err := l.pool.QueryRow(ctx,
			`SELECT channel FROM deliveries WHERE id=$1`, id).Scan(&snap.Channel); err != nil {
			return snap, fmt.Errorf("resolve delivery %d channel: %w", id, err)
		}
	}

	// per-channel sends in the last hour
	rows, err := l.pool.Query(ctx,
		`SELECT channel, count(*) FROM deliveries
		 WHERE status='sent' AND sent_at >= now() - interval '1 hour'
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

func hourlyLimit() int {
	if v := os.Getenv("OPS_SEND_HOURLY_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 10
}
