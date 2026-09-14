//go:build integration

package dashboard

// SWT-52 (docs/tickets/board-status-lights_SPEC.md) criterion 13 and the
// America/New_York midnight boundary. Internal (package dashboard) so the test
// runs the board's OWN spelling, boardDayStart, with now() replaced by a fixed
// instant — plus the SPEC's literal statement. Env-gated on DATABASE_URL.
//
// Instants either side of ET midnight are the SWT-48 trap: 22:00 EDT on
// 2026-09-14 is already 2026-09-15 in UTC, and its day start must still be
// 2026-09-14 00:00 EDT.
//
// GREENFIELD NOTE — EXPECTED RED: boardDayStart and BoardTimeZone do not exist
// (compile failure under -tags integration).

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/store"
)

func TestBoardDayStart_Integration_DSTAndMidnight(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("never run against the real ops db")
	}
	ctx := context.Background()
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	defer pool.Close()

	expr := boardDayStart("$2")
	if !strings.Contains(expr, "now()") {
		t.Fatalf("boardDayStart(\"$2\") = %q has no now(): the day start is on the DB clock (criterion 10)", expr)
	}
	helperSQL := "SELECT " + strings.Replace(expr, "now()", "$1::timestamptz", 1)
	const literalSQL = `SELECT date_trunc('day', $1::timestamptz AT TIME ZONE $2) AT TIME ZONE $2`

	utc := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("parse %s: %v", s, err)
		}
		return v
	}
	for _, tc := range []struct{ name, at, want string }{
		// 2026-03-08: spring forward at 02:00 EST; midnight is still EST (-05).
		{"DST starts, afternoon", "2026-03-08T16:00:00Z", "2026-03-08T05:00:00Z"},
		{"DST starts, 01:30 EST", "2026-03-08T06:30:00Z", "2026-03-08T05:00:00Z"},
		// 2026-11-01: fall back at 02:00 EDT; midnight is still EDT (-04).
		{"DST ends, afternoon", "2026-11-01T20:00:00Z", "2026-11-01T04:00:00Z"},
		{"DST ends, the repeated 01:30", "2026-11-01T06:30:00Z", "2026-11-01T04:00:00Z"},
		{"DST ends, 23:30 EST", "2026-11-02T04:30:00Z", "2026-11-01T04:00:00Z"},
		// Either side of ET midnight (EDT), with the UTC date already rolled over.
		{"22:00 EDT, UTC is tomorrow", "2026-09-15T02:00:00Z", "2026-09-14T04:00:00Z"},
		{"23:59:59 EDT", "2026-09-15T03:59:59Z", "2026-09-14T04:00:00Z"},
		{"00:00:00 EDT exactly", "2026-09-15T04:00:00Z", "2026-09-15T04:00:00Z"},
		{"00:00:01 EDT", "2026-09-15T04:00:01Z", "2026-09-15T04:00:00Z"},
	} {
		for name, q := range map[string]string{"literal": literalSQL, "boardDayStart": helperSQL} {
			var got time.Time
			if err := pool.QueryRow(ctx, q, utc(tc.at), BoardTimeZone).Scan(&got); err != nil {
				t.Fatalf("%s (%s): %v", tc.name, name, err)
			}
			if !got.Equal(utc(tc.want)) {
				t.Errorf("%s (%s): day start of %s = %s, want %s (criterion 13)", tc.name, name, tc.at,
					got.UTC().Format(time.RFC3339), tc.want)
			}
		}
	}
}
