package main

// The watcher's single-instance lock and the CronJob's stand-down check
// (SWT-75 D4, D9). One key, lockkeys.SlackWatch, two readers:
//
//   - the resident watcher holds it as a SESSION lock on a dedicated
//     connection for its lifetime. Held elsewhere means STANDBY (a second
//     replica, a rolling update's overlap), never an exit; a LOST connection
//     (a CNPG switchover kills it silently) is a restart.
//   - the one-shot connector-slackweb CronJob probes it at startup with
//     pg_try_advisory_lock and releases it immediately: held means a watcher is
//     live, so the one-shot logs and exits 0 without touching the browser.
//     That is what makes the 2-hourly CronJob a net rather than a co-worker on
//     the mini's one browser.
//
// A twin of internal/orchestrator/engine.go's LockHandle, re-spelled locally:
// a connector must not import the orchestrator (invariant 7).

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/lockkeys"
)

// watchLock holds pg_advisory_lock(lockkeys.SlackWatch) for the process
// lifetime. Alive is checked by /healthz on every probe.
type watchLock struct {
	mu       sync.Mutex // pgx conns are not safe for concurrent use: the loop and /healthz both call Alive
	conn     *pgxpool.Conn
	released bool
}

// Alive runs SELECT 1 on the held connection. An error means the lock is gone.
func (l *watchLock) Alive(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return fmt.Errorf("slack watch lock already released")
	}
	var one int
	if err := l.conn.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
		return fmt.Errorf("slack watch lock connection: %w", err)
	}
	return nil
}

// Release unlocks and returns the connection. Idempotent.
func (l *watchLock) Release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return
	}
	l.released = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var unlocked bool
	_ = l.conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, lockkeys.SlackWatch).Scan(&unlocked)
	l.conn.Release()
}

// tryWatchLock takes lockkeys.SlackWatch as a session lock on a dedicated
// connection. ok=false (nil handle) means another watcher holds it: standby.
func tryWatchLock(ctx context.Context, pool *pgxpool.Pool) (lock *watchLock, ok bool, err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire slack watch lock conn: %w", err)
	}
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockkeys.SlackWatch).Scan(&ok); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("pg_try_advisory_lock(slack watch): %w", err)
	}
	if !ok {
		conn.Release()
		return nil, false, nil
	}
	return &watchLock{conn: conn}, true, nil
}

// standDown is the CronJob's half (D4): probe the key and release it at once.
// true means a watcher is live and this one-shot pass must skip.
func standDown(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	lock, ok, err := tryWatchLock(ctx, pool)
	if err != nil {
		return false, err
	}
	if !ok {
		return true, nil
	}
	lock.Release()
	return false, nil
}

// standbyLock is the lock handle a replica that lost the race exposes to
// /healthz: Alive always answers errStandby.
type standbyLock struct{}

func (standbyLock) Alive(context.Context) error { return errStandby }
