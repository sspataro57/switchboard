//go:build integration

package main

// slack-watch-sweep (SWT-75) criteria 21 and 22 against a real Postgres: the
// singleton lock and the one-shot stand-down, in BOTH directions.
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_slackwatch?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run WatchSingleton ./cmd/connectors/slackweb/
//
// An advisory lock is a session fact, so it cannot be faked: this is exactly
// the kind of predicate the IK rule "test the column, not the fixture" is about
// — the contention is produced by Postgres, on a second connection.
//
// IMPOSED SURFACE (names chosen here; D9 says "straight from
// internal/orchestrator/engine.go:215-265, re-spelled locally — a connector
// must not import the orchestrator (invariant 7)"):
//
//	// tryWatchLock takes lockkeys.SlackWatch as a SESSION lock on a dedicated
//	// connection. ok=false means another watcher holds it: STANDBY, not an
//	// error and not an exit (D9).
//	func tryWatchLock(ctx context.Context, pool *pgxpool.Pool) (*watchLock, bool, error)
//	func (l *watchLock) Alive(ctx context.Context) error // non-nil once the conn is gone
//	func (l *watchLock) Release()
//
//	// standDown is the CronJob's half (D4): pg_try_advisory_lock, released
//	// immediately. true = a watcher is live, so this one-shot pass skips and
//	// exits 0.
//	func standDown(ctx context.Context, pool *pgxpool.Pool) (bool, error)
//
// RED TODAY: neither function exists, so this file compile-FAILs under
// -tags integration.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/lockkeys"
	"github.com/sspataro57/switchboard/internal/store"
)

func singletonPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must never run against the real ops database")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// holdTheKey takes lockkeys.SlackWatch on a dedicated connection and returns a
// release func — a stand-in for "another watcher is running".
func holdTheKey(t *testing.T, ctx context.Context, pool *pgxpool.Pool) func() {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockkeys.SlackWatch).Scan(&got); err != nil {
		t.Fatalf("pg_try_advisory_lock: %v", err)
	}
	if !got {
		conn.Release()
		t.Fatalf("lockkeys.SlackWatch (%#x) is already held by something else; this suite cannot run", lockkeys.SlackWatch)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, lockkeys.SlackWatch)
		conn.Release()
	}
	t.Cleanup(release)
	return release
}

// Criterion 21: held elsewhere means STANDBY, not exit. The SPEC's mutation row
// is "exit instead of standby when the singleton lock is held" — a second
// replica (or a rolling update's overlap) must sit quietly and take over, not
// crash-loop.
func TestWatchSingleton_HeldElsewhereIsStandbyNotAnError(t *testing.T) {
	ctx := context.Background()
	pool := singletonPool(t, ctx)
	release := holdTheKey(t, ctx, pool)

	lock, ok, err := tryWatchLock(ctx, pool)
	if err != nil {
		t.Fatalf("tryWatchLock while the key is held = %v; contention is not an ERROR, it is standby (D9)", err)
	}
	if ok {
		t.Fatalf("tryWatchLock returned ok=true while another session holds %#x; two watchers would present "+
			"the mini's single browser with two callers (criterion 21, D3)", lockkeys.SlackWatch)
	}
	if lock != nil {
		t.Errorf("tryWatchLock returned a non-nil handle it does not hold (%v)", lock)
	}

	// Taking over: the same call succeeds once the other session lets go — the
	// SPEC's smoke step "it takes over within 15 s of the first being killed".
	release()
	lock, ok, err = tryWatchLock(ctx, pool)
	if err != nil || !ok {
		t.Fatalf("tryWatchLock after the key was released = (%v, %v, %v), want a held lock", lock, ok, err)
	}
	defer lock.Release()
	if err := lock.Alive(ctx); err != nil {
		t.Errorf("Alive() on a freshly taken lock = %v, want nil — /healthz calls this on every probe", err)
	}
}

// Criterion 22, both directions. With the lock held, the one-shot CronJob path
// must skip; with it free, it must behave exactly as today. This is what makes
// the net a net: without it a two-hourly tick lands on an idle browser, holds
// it for fifteen minutes doing work the watcher already does, and blocks the
// watch list for those fifteen minutes, twice a day (D4).
func TestWatchSingleton_OneShotStandsDownOnlyWhileTheWatcherLives(t *testing.T) {
	ctx := context.Background()
	pool := singletonPool(t, ctx)

	// Direction 1: no watcher. Today's behaviour, unchanged.
	live, err := standDown(ctx, pool)
	if err != nil {
		t.Fatalf("standDown with the key free = %v", err)
	}
	if live {
		t.Fatalf("standDown reported a live watcher while %#x is free; the CronJob would then never run and "+
			"Slack ingestion would stop entirely whenever the Deployment is scaled to 0 — which is the "+
			"SPEC's own rollback (criterion 22)", lockkeys.SlackWatch)
	}

	// standDown must RELEASE what it took, or the watcher it is checking for
	// could never start.
	lock, ok, err := tryWatchLock(ctx, pool)
	if err != nil || !ok {
		t.Fatalf("after standDown the key could not be taken (%v, %v): standDown takes it with "+
			"pg_try_advisory_lock and releases it IMMEDIATELY (D4)", ok, err)
	}
	defer lock.Release()

	// Direction 2: a watcher is live (this very handle).
	live, err = standDown(ctx, pool)
	if err != nil {
		t.Fatalf("standDown with the key held = %v; it is a check, not a failure", err)
	}
	if !live {
		t.Errorf("standDown did not see a live watcher while %#x is held. Criterion 22: the one-shot logs "+
			"`slack watch is live; skipping this pass`, exits 0, opens NO bridge connection and writes no "+
			"sync_runs row", lockkeys.SlackWatch)
	}
}
