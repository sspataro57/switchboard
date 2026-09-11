package orchestrator_test

// SWT-41 (docs/tickets/orchestrator-deploy_SPEC.md) criterion 7: HealthVerdict
// is PURE and table-tested (invariant 7 — no db, no clock read, no network),
// plus compile-time pins of the new exported surface so a signature drift is a
// build error here rather than a surprise in cmd/orchestratord or the dashboard.
//
// IMPOSED SURFACE (internal/orchestrator; names chosen here, behaviour from D3/D5):
//
//	const HealthStallAfter = 5 * time.Minute
//	// "not_running" | "stalled" | "ok". oldest is the zero time when backlog == 0.
//	func HealthVerdict(running bool, backlog int64, oldest, now time.Time) string
//
//	type HealthState struct {
//	    Running             bool      // orchestrator lock held by some session (pg_locks)
//	    Backlog             int64     // count(*) task_events WHERE id > cursor
//	    OldestUnprocessedAt time.Time // min(created_at) of those; zero when Backlog == 0
//	    Cursor              int64     // orchestrator_cursor.last_event_id
//	    CursorUpdatedAt     time.Time
//	    Head                int64     // max(task_events.id), 0 when empty
//	    Verdict             string    // HealthVerdict(Running, Backlog, OldestUnprocessedAt, now)
//	}
//	func Health(ctx context.Context, pool *pgxpool.Pool, now time.Time) (HealthState, error)
//	// The pg_locks read, parameterised by key so the high/low split is testable
//	// (see health_integration_test.go for why the real key cannot test it).
//	func AdvisoryLockHeld(ctx context.Context, pool *pgxpool.Pool, key int64) (bool, error)
//
//	type LockHandle struct{ ... }
//	func (*LockHandle) Alive(ctx context.Context) error // SELECT 1 on the held conn
//	func (*LockHandle) Release()
//	func TryAdvisoryLock(ctx context.Context, pool *pgxpool.Pool) (lock *LockHandle, ok bool, err error)
//
// GREENFIELD NOTE — EXPECTED RED: none of these exist (TryAdvisoryLock returns
// a release func today), so the orchestrator_test unit build compile-FAILS.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	orch "github.com/sspataro57/switchboard/internal/orchestrator"
)

// Compile-time pins of the imposed surface.
var (
	_ func(bool, int64, time.Time, time.Time) string                            = orch.HealthVerdict
	_ func(context.Context, *pgxpool.Pool, time.Time) (orch.HealthState, error) = orch.Health
	_ func(context.Context, *pgxpool.Pool, int64) (bool, error)                 = orch.AdvisoryLockHeld
	_ func(context.Context, *pgxpool.Pool) (*orch.LockHandle, bool, error)      = orch.TryAdvisoryLock
	_ interface {
		Alive(context.Context) error
		Release()
	} = (*orch.LockHandle)(nil)
)

func TestHealthStallAfter_IsFiveTicks(t *testing.T) {
	if orch.HealthStallAfter != 5*time.Minute {
		t.Errorf("HealthStallAfter = %v, want 5m (D5: five ticks; a package constant, not env)", orch.HealthStallAfter)
	}
}

// Criterion 7. The boundary rows use the literal 5m, not the constant, so
// changing the constant turns this table red too.
func TestHealthVerdict_Table(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		running bool
		backlog int64
		oldest  time.Time
		want    string
	}{
		// Lock not held -> not_running, WHATEVER the backlog: "not running" is
		// the silent two-month gap, and an empty backlog does not excuse it.
		{"not held, no backlog", false, 0, time.Time{}, "not_running"},
		{"not held, fresh backlog", false, 3, now.Add(-time.Second), "not_running"},
		{"not held, 866 events two days old", false, 866, now.Add(-48 * time.Hour), "not_running"},

		// Held + backlog 0 -> ok. Cursor age is deliberately NOT the signal: an
		// idle system never moves the cursor.
		{"held, backlog 0", true, 0, time.Time{}, "ok"},

		// Held + oldest unprocessed <= 5m -> ok; > 5m -> stalled.
		{"held, oldest 1m", true, 4, now.Add(-time.Minute), "ok"},
		{"held, oldest exactly 5m (inclusive)", true, 4, now.Add(-5 * time.Minute), "ok"},
		{"held, oldest 5m + 1ns", true, 4, now.Add(-5*time.Minute - time.Nanosecond), "stalled"},
		{"held, oldest 5m + 1s", true, 1, now.Add(-5*time.Minute - time.Second), "stalled"},
		{"held, oldest 2h", true, 1, now.Add(-2 * time.Hour), "stalled"},
		// Postgres now() vs the dashboard's clock: a slightly future oldest is
		// "otherwise" -> ok, never stalled.
		{"held, oldest slightly in the future (clock skew)", true, 1, now.Add(time.Second), "ok"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := orch.HealthVerdict(tc.running, tc.backlog, tc.oldest, now); got != tc.want {
				t.Errorf("HealthVerdict(running=%v, backlog=%d, oldest=now-%v) = %q, want %q",
					tc.running, tc.backlog, now.Sub(tc.oldest), got, tc.want)
			}
		})
	}
}
