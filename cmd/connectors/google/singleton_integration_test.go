//go:build integration

package main

// imap-idle-watch (SWT-73) criteria 2 and 3 against a real Postgres: the
// singleton lock, in both directions. An advisory lock is a SESSION fact, so it
// cannot be faked — this is exactly what the IK rule "test the column, not the
// fixture" is about: the contention is produced by Postgres, on a second
// connection.
//
// Run it in an ISOLATED scratch database (IK landmine: the compose Postgres is
// shared by every worktree, and advisory locks are per-database, so this
// isolates them too):
//
//	psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c "CREATE DATABASE ops_idlewatch"
//	make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_idlewatch?sslmode=disable'
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_idlewatch?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run MailWatchSingleton ./cmd/connectors/google/
//
// This suite seeds NOTHING, so there is nothing to clean up: it takes and
// releases one advisory key and writes no row.
//
// IMPOSED SURFACE (names chosen here; D4 says the shape is
// internal/orchestrator/engine.go:215-265 "re-spelled locally in
// cmd/connectors/google — a connector must not import internal/orchestrator",
// invariant 7):
//
//	// tryMailWatchLock takes lockkeys.MailWatch as a SESSION lock on a
//	// dedicated pooled connection. ok=false (nil handle) means another watcher
//	// holds it: STANDBY, not an error and not an exit (D4).
//	func tryMailWatchLock(ctx context.Context, pool *pgxpool.Pool) (*watchLock, bool, error)
//	func (l *watchLock) Alive(ctx context.Context) error // non-nil once the conn is gone
//	func (l *watchLock) Release()
//
// GREENFIELD NOTE — EXPECTED RED: neither tryMailWatchLock nor watchLock nor
// lockkeys.MailWatch exists, so this file compile-FAILS under -tags integration.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/lockkeys"
	"github.com/sspataro57/switchboard/internal/store"
)

func mwPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use an isolated scratch database on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// mwHoldTheKey takes lockkeys.MailWatch on a dedicated connection and returns a
// release func — the stand-in for "another watcher is running".
func mwHoldTheKey(t *testing.T, ctx context.Context, pool *pgxpool.Pool) func() {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockkeys.MailWatch).Scan(&got); err != nil {
		t.Fatalf("pg_try_advisory_lock: %v", err)
	}
	if !got {
		conn.Release()
		t.Fatalf("lockkeys.MailWatch (%#x) is already held by something else; this suite cannot run", lockkeys.MailWatch)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, lockkeys.MailWatch)
		conn.Release()
	}
	t.Cleanup(release)
	return release
}

// Criterion 2: held elsewhere is STANDBY, not an error and not an exit, and the
// key is taken as soon as the holder goes away — the SPEC's smoke step "it must
// log standby, answer 503 on its own health port, ingest nothing, and take over
// within 15 s of the first being killed".
//
// MUTATIONS: drop the singleton lock acquisition / exit instead of standby when
// the lock is held -> criterion 2.
func TestMailWatchSingleton_HeldElsewhereIsStandbyNotAnError(t *testing.T) {
	ctx := context.Background()
	pool := mwPool(t, ctx)
	release := mwHoldTheKey(t, ctx, pool)

	lock, ok, err := tryMailWatchLock(ctx, pool)
	if err != nil {
		t.Fatalf("tryMailWatchLock while the key is held = %v; contention is not an ERROR, it is standby "+
			"(D4: a node drain can leave the old pod terminating while the new one starts, and a crash-loop "+
			"there would be self-inflicted)", err)
	}
	if ok {
		t.Fatalf("tryMailWatchLock returned ok=true while another session holds %#x. Two watchers would each "+
			"hold four IDLE connections and both would resolve the MSN credential (criterion 2, D5)",
			lockkeys.MailWatch)
	}
	if lock != nil {
		t.Errorf("tryMailWatchLock returned a non-nil handle it does not hold (%v)", lock)
	}

	release()
	lock, ok, err = tryMailWatchLock(ctx, pool)
	if err != nil || !ok {
		t.Fatalf("tryMailWatchLock after the key was released = (%v, %v, %v), want a held lock: criterion 2 "+
			"takes over WITHOUT restarting the process", lock, ok, err)
	}
	defer lock.Release()
	if err := lock.Alive(ctx); err != nil {
		t.Errorf("Alive() on a freshly taken lock = %v, want nil — /healthz calls this on every probe "+
			"(criteria 3, 6)", err)
	}

	// The key really is held: a second attempt on the SAME pool (a different
	// session) must be refused. pg_advisory_lock is database-wide, which is the
	// property the whole design rests on.
	second, ok2, err := tryMailWatchLock(ctx, pool)
	if err != nil {
		t.Fatalf("second tryMailWatchLock = %v", err)
	}
	if ok2 {
		second.Release()
		t.Errorf("two sessions hold %#x at once; the lock is not a session lock on a DEDICATED connection "+
			"(D4). A pooled connection shared with the loop would release the key the moment the query "+
			"finished", lockkeys.MailWatch)
	}
}

// Criterion 3: Alive reports the truth, and Release is idempotent. Losing the
// connection (a CNPG switchover kills it silently) is what makes runWatch return
// a non-nil error and the process exit non-zero — the unit half of that is in
// watch_test.go; this half is that Alive on a RELEASED handle is an error rather
// than a cheerful nil.
func TestMailWatchSingleton_AliveAndReleaseAreHonest(t *testing.T) {
	ctx := context.Background()
	pool := mwPool(t, ctx)

	lock, ok, err := tryMailWatchLock(ctx, pool)
	if err != nil || !ok {
		t.Fatalf("tryMailWatchLock on a free key = (%v, %v)", ok, err)
	}
	actx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := lock.Alive(actx); err != nil {
		t.Fatalf("Alive on a held lock = %v, want nil", err)
	}

	lock.Release()
	lock.Release() // idempotent: the shutdown path and a deferred Release both run

	if err := lock.Alive(ctx); err == nil {
		t.Error("Alive() on a RELEASED handle returned nil. /healthz would then answer 200 for a watcher " +
			"that holds nothing, while a second pod does the work (criteria 3, 6)")
	}

	// And the key is genuinely free again, so the next pod's standby loop takes
	// it within 15 s rather than standing by forever.
	again, ok, err := tryMailWatchLock(ctx, pool)
	if err != nil || !ok {
		t.Fatalf("the key was not free after Release (%v, %v): a watcher that exits without unlocking "+
			"leaves the next pod standing by until the connection is reaped", ok, err)
	}
	again.Release()
}
