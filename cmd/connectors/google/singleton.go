package main

// The resident mail watcher's single-instance lock (SWT-73 D4). One key,
// lockkeys.MailWatch, held as a SESSION lock on a dedicated pooled connection
// for the process lifetime:
//
//   - held elsewhere means STANDBY (a second replica, a node drain's overlap),
//     never an exit: log once, /healthz 503 "standby", retry every 15 s;
//   - a LOST connection (a CNPG switchover kills it silently) is a restart —
//     the loop checks Alive on every reconcile tick and returns an error.
//
// It is watcher-versus-watcher only. The one-shot connector-google CronJob is
// kept off a mailbox by the per-account locks (mailsource.go), not by this key.
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

// watchLock holds pg_advisory_lock(lockkeys.MailWatch) for the process
// lifetime. Alive is checked by the loop on every tick and by /healthz on
// every probe.
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
		return fmt.Errorf("mail watch lock already released")
	}
	var one int
	if err := l.conn.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
		return fmt.Errorf("mail watch lock connection: %w", err)
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
	_ = l.conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, lockkeys.MailWatch).Scan(&unlocked)
	l.conn.Release()
}

// tryMailWatchLock takes lockkeys.MailWatch as a session lock on a dedicated
// connection. ok=false (nil handle) means another watcher holds it: standby.
func tryMailWatchLock(ctx context.Context, pool *pgxpool.Pool) (lock *watchLock, ok bool, err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire mail watch lock conn: %w", err)
	}
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockkeys.MailWatch).Scan(&ok); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("pg_try_advisory_lock(mail watch): %w", err)
	}
	if !ok {
		conn.Release()
		return nil, false, nil
	}
	return &watchLock{conn: conn}, true, nil
}
