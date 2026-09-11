package orchestrator

// SWT-41 D5: orchestrator health, judged from OUTSIDE the process — "not
// running" has no process to ask. The dashboard reads it from Postgres:
// whether some session holds the orchestrator's advisory lock (pg_locks), and
// how old the oldest unprocessed task_event is. Cursor age alone is
// deliberately NOT the signal: an idle system never moves the cursor.

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// HealthStallAfter is five ticks: an unprocessed event older than this while
// the lock is held means the loop is alive but not draining. A package
// constant, not env — it gates nothing (the funnelDisplayStaleAfter precedent).
const HealthStallAfter = 5 * time.Minute

// Verdicts rendered by the dashboard.
const (
	VerdictOK         = "ok"
	VerdictNotRunning = "not_running"
	VerdictStalled    = "stalled"
)

// HealthVerdict is pure (invariant 7). oldest is the zero time when backlog
// is 0. The boundary is inclusive: exactly HealthStallAfter is still ok. An
// oldest slightly in the future (Postgres now() vs this clock) is ok.
func HealthVerdict(running bool, backlog int64, oldest, now time.Time) string {
	if !running {
		return VerdictNotRunning
	}
	if backlog > 0 && !oldest.IsZero() && now.Sub(oldest) > HealthStallAfter {
		return VerdictStalled
	}
	return VerdictOK
}

// HealthState is one read of the orchestrator's health.
type HealthState struct {
	Running             bool      // the orchestrator lock is held by some session (pg_locks)
	Backlog             int64     // count(*) task_events WHERE id > cursor
	OldestUnprocessedAt time.Time // min(created_at) of those; zero when Backlog == 0
	Cursor              int64     // orchestrator_cursor.last_event_id
	CursorUpdatedAt     time.Time
	Head                int64 // max(task_events.id), 0 when empty
	Verdict             string
}

// Health reads the lock, the cursor and the backlog. SELECT-only.
func Health(ctx context.Context, pool *pgxpool.Pool, now time.Time) (HealthState, error) {
	var h HealthState
	running, err := AdvisoryLockHeld(ctx, pool, AdvisoryLockKey)
	if err != nil {
		return h, err
	}
	h.Running = running

	var oldest *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT c.last_event_id, c.updated_at,
		       (SELECT count(*)        FROM task_events e WHERE e.id > c.last_event_id),
		       (SELECT min(created_at) FROM task_events e WHERE e.id > c.last_event_id),
		       (SELECT COALESCE(max(id), 0) FROM task_events)
		  FROM orchestrator_cursor c WHERE c.name = 'orchestrator'`).Scan(
		&h.Cursor, &h.CursorUpdatedAt, &h.Backlog, &oldest, &h.Head); err != nil {
		return h, fmt.Errorf("read orchestrator cursor and backlog: %w", err)
	}
	if oldest != nil {
		h.OldestUnprocessedAt = *oldest
	}
	h.Verdict = HealthVerdict(h.Running, h.Backlog, h.OldestUnprocessedAt, now)
	return h, nil
}

// AdvisoryLockHeld reports whether any session holds the session-level
// advisory lock on key. pg_locks stores a bigint key split in two: classid =
// the high 32 bits, objid = the low 32 bits, objsubid = 1. Exposed with the
// key as a parameter because the real key fits in 32 bits (its high half is
// 0), so only a key with high bits set can prove the split is right.
func AdvisoryLockHeld(ctx context.Context, pool *pgxpool.Pool, key int64) (bool, error) {
	hi := int64(uint64(key) >> 32)
	lo := int64(uint32(uint64(key)))
	var held bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_locks
		                WHERE locktype = 'advisory' AND granted
		                  -- advisory locks are per database; pg-main hosts several
		                  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
		                  AND classid::bigint = $1 AND objid::bigint = $2 AND objsubid = 1)`,
		hi, lo).Scan(&held); err != nil {
		return false, fmt.Errorf("read pg_locks for advisory key %#x: %w", key, err)
	}
	return held, nil
}
