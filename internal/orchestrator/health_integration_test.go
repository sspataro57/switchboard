//go:build integration

package orchestrator_test

// SWT-41 (docs/tickets/orchestrator-deploy_SPEC.md) criteria 5 and 8, the
// Postgres half: the Health loader and the lock handle's liveness, against the
// compose db. TEST THE COLUMN, NOT THE FIXTURE (IK): every value asserted here
// is produced by Postgres — pg_locks rows, task_events counts, created_at —
// and compared against an independent query, never against a constant the
// test supplied to the code under test.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run 'Health|AdvisoryLock' ./internal/orchestrator/
//
// Build-tagged `integration` AND env-gated on DATABASE_URL (newOrchPool skips),
// with a FATAL guard on 192.168.50.49: these tests move the global
// orchestrator_cursor row and terminate a backend.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (criterion 8):
//   - M1: replace the pg_locks predicate with a literal `true` →
//     TestHealth_Integration_RunningReadsPgLocks goes red on the RELEASED case
//     (running=true with nobody holding the lock), and
//     TestAdvisoryLockHeld_Integration_SplitsTheBigintKey on its not-held rows.
//   - M2: compare objid against the FULL 64-bit key instead of the low-32 split
//     → TestAdvisoryLockHeld_Integration_SplitsTheBigintKey goes red on the
//     HELD case. NOTE: M2 CANNOT be caught through orch.AdvisoryLockKey.
//     0x5157_0005 < 2^32, so its high half is 0: classid = 0 and objid equals
//     the full key, and the mutated comparison is identical for the real
//     constant. The SPEC's mutation claim holds only for a key with non-zero
//     high bits, which is why the pg_locks read is exposed as
//     AdvisoryLockHeld(ctx, pool, key) and exercised with such a key below.
//     (Dropping the classid clause is caught by the "same low half, different
//     high half" row.)
//
// GREENFIELD NOTE — EXPECTED RED: orch.Health, orch.AdvisoryLockHeld and the
// LockHandle returned by TryAdvisoryLock do not exist, so the integration build
// of this package compile-FAILS (which also stops the pre-existing integration
// tests in this package from building until they land).

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	orch "github.com/sspataro57/switchboard/internal/orchestrator"
	"github.com/sspataro57/switchboard/internal/store"
)

func healthGuard(t *testing.T) {
	t.Helper()
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db (this suite moves the " +
			"orchestrator cursor and terminates a backend); use the compose db on :5433")
	}
}

// holdSessionLock takes a SESSION advisory lock on its own connection, the way
// a running orchestratord does. It fails the test if the key is already held
// (a stray orchestratord on the compose db would make every assertion here
// meaningless). The returned release is idempotent and also runs at cleanup.
func holdSessionLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, key int64) (release func()) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire lock conn: %v", err)
	}
	var ok bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&ok); err != nil {
		conn.Release()
		t.Fatalf("pg_try_advisory_lock(%#x): %v", key, err)
	}
	if !ok {
		conn.Release()
		t.Fatalf("advisory key %#x is already held by another session on the compose db — stop whatever "+
			"holds it (an orchestratord pointed at :5433?) before running this suite", key)
	}
	done := false
	release = func() {
		if done {
			return
		}
		done = true
		var unlocked bool
		_ = conn.QueryRow(context.Background(), `SELECT pg_advisory_unlock($1)`, key).Scan(&unlocked)
		conn.Release()
	}
	t.Cleanup(release)
	return release
}

// ---- criterion 8: running = the orchestrator lock is held (pg_locks) ---------

func TestHealth_Integration_RunningReadsPgLocks(t *testing.T) {
	ctx := context.Background()
	pool := newOrchPool(t, ctx)
	t.Cleanup(pool.Close) // registered first so it runs LAST, after the fixture cleanups and lock releases
	healthGuard(t)

	key := int64(orch.AdvisoryLockKey)
	release := holdSessionLock(t, ctx, pool, key) // succeeding proves nobody else held it

	h, err := orch.Health(ctx, pool, time.Now())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !h.Running {
		t.Errorf("Health.Running = false while pg_try_advisory_lock(%#x) is held on a separate connection. "+
			"D5: running = the orchestrator lock is held by SOME session, read from pg_locks", key)
	}

	release()
	h, err = orch.Health(ctx, pool, time.Now())
	if err != nil {
		t.Fatalf("Health after release: %v", err)
	}
	if h.Running {
		t.Errorf("Health.Running = true after the lock was released — nobody holds %#x. This is mutation M1 "+
			"(a predicate that is always true): the dashboard would show `ok` for an orchestratord that is "+
			"not running, which is exactly the two-month gap this ticket closes", key)
	}
	if h.Verdict != "not_running" {
		t.Errorf("Health.Verdict = %q with the lock released, want not_running", h.Verdict)
	}
}

// ---- criterion 8, mutation M2: the bigint key is split high/low --------------

func TestAdvisoryLockHeld_Integration_SplitsTheBigintKey(t *testing.T) {
	ctx := context.Background()
	pool := newOrchPool(t, ctx)
	t.Cleanup(pool.Close) // registered first so it runs LAST, after the fixture cleanups and lock releases
	healthGuard(t)

	// A key with NON-ZERO high bits (see the header: the real key has none).
	// Built by shifting, not written as a 0x5157 literal.
	const high, low = int64(7), int64(42)
	key := high<<32 | low
	sameLowOtherHigh := int64(9)<<32 | low

	held := func(k int64) bool {
		t.Helper()
		ok, err := orch.AdvisoryLockHeld(ctx, pool, k)
		if err != nil {
			t.Fatalf("AdvisoryLockHeld(%#x): %v", k, err)
		}
		return ok
	}

	if held(key) {
		t.Fatalf("AdvisoryLockHeld(%#x) = true before anything took it (mutation M1?)", key)
	}
	release := holdSessionLock(t, ctx, pool, key)

	// Premise check — Postgres's own representation, so the test is not
	// asserting a fixture: a bigint advisory key is classid = high 32 bits,
	// objid = low 32 bits, objsubid = 1.
	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND classid::bigint=$1
		   AND objid::bigint=$2 AND objsubid=1 AND granted`, high, low).Scan(&rows); err != nil {
		t.Fatalf("premise query: %v", err)
	}
	if rows != 1 {
		t.Fatalf("PREMISE FAILED: pg_locks has %d rows for classid=%d objid=%d objsubid=1; the split the "+
			"SPEC describes does not hold on this server", rows, high, low)
	}

	if !held(key) {
		t.Errorf("AdvisoryLockHeld(%#x) = false while held. classid must be compared to the HIGH 32 bits "+
			"(%d) and objid to the LOW 32 bits (%d), objsubid = 1 — comparing objid against the full "+
			"64-bit key (mutation M2) never matches a key with high bits set", key, high, low)
	}
	if held(sameLowOtherHigh) {
		t.Errorf("AdvisoryLockHeld(%#x) = true: it shares only the LOW half with the held key %#x, so the "+
			"classid (high half) clause is missing", sameLowOtherHigh, key)
	}

	release()
	if held(key) {
		t.Errorf("AdvisoryLockHeld(%#x) = true after release", key)
	}
}

// ---- criterion 8: backlog and oldest match inserted events ------------------

func TestHealth_Integration_BacklogAndOldestMatchInsertedEvents(t *testing.T) {
	ctx := context.Background()
	pool := newOrchPool(t, ctx)
	t.Cleanup(pool.Close) // registered first so it runs LAST, after the fixture cleanups and lock releases
	healthGuard(t)
	cleanupOrch(t, ctx, pool)
	t.Cleanup(func() { cleanupOrch(t, context.Background(), pool) })

	const slug = "itest-orch-health"
	seedProjectDelivery(t, ctx, pool, slug, "itest-orch-health-client", "console")
	var taskID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, status)
		 VALUES ((SELECT id FROM projects WHERE slug=$1), 'health fixture', 'ready') RETURNING id`,
		slug).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	head0 := maxEventID(t, ctx, pool)
	setCursor(t, ctx, pool, head0)

	now := time.Now()
	h, err := orch.Health(ctx, pool, now)
	if err != nil {
		t.Fatalf("Health (empty backlog): %v", err)
	}
	if h.Backlog != 0 || !h.OldestUnprocessedAt.IsZero() {
		t.Errorf("empty backlog: Backlog=%d OldestUnprocessedAt=%v, want 0 and the zero time", h.Backlog, h.OldestUnprocessedAt)
	}
	if h.Cursor != head0 || h.Head != head0 {
		t.Errorf("Cursor=%d Head=%d, want both %d", h.Cursor, h.Head, head0)
	}

	insert := func(ago string) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO task_events (task_id, event_type, payload, created_at)
			 VALUES ($1, 'log', '{}', now() - $2::interval) RETURNING id`, taskID, ago).Scan(&id); err != nil {
			t.Fatalf("insert event: %v", err)
		}
		return id
	}
	insert("10 minutes")
	second := insert("7 minutes")
	last := insert("1 minute")

	// Independent measurement of what Health must report.
	measure := func(cursor int64) (int64, time.Time) {
		t.Helper()
		var n int64
		var oldest time.Time
		if err := pool.QueryRow(ctx,
			`SELECT count(*), min(created_at) FROM task_events WHERE id > $1`, cursor).Scan(&n, &oldest); err != nil {
			t.Fatalf("independent backlog query: %v", err)
		}
		return n, oldest
	}

	wantN, wantOldest := measure(head0)
	if wantN != 3 {
		t.Fatalf("POSITIVE CONTROL: %d events past the cursor, want the 3 this test inserted (another "+
			"suite writing task_events concurrently? `make integration` runs -p 1)", wantN)
	}
	now = time.Now()
	h, err = orch.Health(ctx, pool, now)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.Backlog != wantN {
		t.Errorf("Backlog = %d, want %d (count(*) FROM task_events WHERE id > cursor)", h.Backlog, wantN)
	}
	if !h.OldestUnprocessedAt.Equal(wantOldest) {
		t.Errorf("OldestUnprocessedAt = %v, want %v (min(created_at) of the unprocessed rows)", h.OldestUnprocessedAt, wantOldest)
	}
	if h.Cursor != head0 || h.Head != last {
		t.Errorf("Cursor=%d Head=%d, want %d and %d", h.Cursor, h.Head, head0, last)
	}
	var updatedAt time.Time
	if err := pool.QueryRow(ctx,
		`SELECT updated_at FROM orchestrator_cursor WHERE name='orchestrator'`).Scan(&updatedAt); err != nil {
		t.Fatalf("read cursor updated_at: %v", err)
	}
	if !h.CursorUpdatedAt.Equal(updatedAt) {
		t.Errorf("CursorUpdatedAt = %v, want %v", h.CursorUpdatedAt, updatedAt)
	}
	if want := orch.HealthVerdict(h.Running, h.Backlog, h.OldestUnprocessedAt, now); h.Verdict != want {
		t.Errorf("Health.Verdict = %q, want HealthVerdict of its own fields = %q", h.Verdict, want)
	}

	// Move the cursor past two of them: the oldest must follow the cursor, not
	// stay pinned to the oldest row in the table.
	setCursor(t, ctx, pool, second)
	wantN, wantOldest = measure(second)
	h, err = orch.Health(ctx, pool, time.Now())
	if err != nil {
		t.Fatalf("Health after cursor move: %v", err)
	}
	if h.Backlog != wantN || wantN != 1 {
		t.Errorf("after moving the cursor to %d: Backlog = %d, want %d (=1)", second, h.Backlog, wantN)
	}
	if !h.OldestUnprocessedAt.Equal(wantOldest) {
		t.Errorf("after moving the cursor: OldestUnprocessedAt = %v, want %v", h.OldestUnprocessedAt, wantOldest)
	}
}

// ---- criterion 5: Alive is nil while held, errors once the backend dies -------

func TestTryAdvisoryLock_Integration_AliveUntilBackendTerminated(t *testing.T) {
	ctx := context.Background()
	admin := newOrchPool(t, ctx)
	defer admin.Close()
	healthGuard(t)

	// A dedicated pool for the lock, so terminating its backend cannot poison
	// the connections this test uses to observe and terminate.
	lockPool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("lock pool: %v", err)
	}
	defer lockPool.Close()

	lock, ok, err := orch.TryAdvisoryLock(ctx, lockPool)
	if err != nil {
		t.Fatalf("TryAdvisoryLock: %v", err)
	}
	if !ok {
		t.Fatal("TryAdvisoryLock: key already held on the compose db (a stray orchestratord?)")
	}
	defer lock.Release()

	if err := lock.Alive(ctx); err != nil {
		t.Fatalf("Alive while held = %v, want nil", err)
	}

	// Unchanged contract: a second instance is refused while the first holds it.
	if other, ok2, err := orch.TryAdvisoryLock(ctx, admin); err != nil {
		t.Fatalf("second TryAdvisoryLock: %v", err)
	} else if ok2 {
		other.Release()
		t.Fatal("a second TryAdvisoryLock succeeded while the first held the key")
	}

	// Find the holder from pg_locks, computing the split from the constant.
	key := int64(orch.AdvisoryLockKey)
	var pid int32
	if err := admin.QueryRow(ctx,
		`SELECT pid FROM pg_locks WHERE locktype='advisory' AND classid::bigint=$1
		   AND objid::bigint=$2 AND objsubid=1 AND granted`,
		key>>32, key&0xFFFFFFFF).Scan(&pid); err != nil {
		t.Fatalf("find the lock holder's pid in pg_locks: %v", err)
	}
	var terminated bool
	if err := admin.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, pid).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("pg_terminate_backend(%d) = %v, %v", pid, terminated, err)
	}

	// Termination is asynchronous: poll.
	deadline := time.Now().Add(10 * time.Second)
	var aliveErr error
	for time.Now().Before(deadline) {
		if aliveErr = lock.Alive(ctx); aliveErr != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if aliveErr == nil {
		t.Fatal("Alive still returns nil 10s after pg_terminate_backend on the lock connection. D3: a CNPG " +
			"switchover kills that connection silently, and without this signal the process keeps " +
			"draining UNLOCKED")
	}

	// And the lock really is gone with the backend: a fresh instance can take it.
	fresh, ok, err := orch.TryAdvisoryLock(ctx, admin)
	if err != nil {
		t.Fatalf("TryAdvisoryLock after termination: %v", err)
	}
	if !ok {
		t.Fatal("the key is still held after its backend was terminated")
	}
	fresh.Release()
}
