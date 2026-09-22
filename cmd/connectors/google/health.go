package main

// GET /healthz for the resident mail watcher (SWT-73 D7): 200 "ok" iff a pass
// COMPLETED within 3 x MAIL_RECONCILE_INTERVAL and the singleton lock's
// connection answers; else 503 with a one-line reason naming which condition
// failed. Deliberately NOT in the verdict: any mailbox's IDLE state (one
// mailbox in backoff must not restart the pod — a restart cannot fix
// invalid_grant and would thrash the healthy mailboxes; its failure is visible
// as imap_idle sync_runs rows instead) and mail volume (a quiet mailbox is not
// a sick one).
//
// A twin of cmd/orchestratord's newHealthHandler, re-spelled locally: a
// connector must not import the orchestrator (invariant 7).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// aliveChecker is anything with Alive: the watcher's lock handle in
// production, a fake in tests.
type aliveChecker interface {
	Alive(ctx context.Context) error
}

// errStandby is what the lock handle reports while ANOTHER watcher holds
// lockkeys.MailWatch: this replica is correct and waiting, not broken. It is
// its own word on /healthz so a log never reads it as a lost lock (a restart).
var errStandby = errors.New("standby: another mail watcher holds the lock")

// standbyLock is the handle a replica exposes to /healthz before it holds the
// singleton: Alive always answers errStandby.
type standbyLock struct{}

func (standbyLock) Alive(context.Context) error { return errStandby }

func newWatchHealthHandler(reconcile time.Duration, now func() time.Time,
	lastPass func() time.Time, lock aliveChecker) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The lock FIRST: a standby replica has never completed a pass, so
		// checking freshness first would report "no pass has completed yet" —
		// true, but it reads as a broken watcher, and D4 gave standby its own
		// word precisely so a node drain's overlap is not read as an outage.
		actx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := lock.Alive(actx); err != nil {
			if errors.Is(err, errStandby) {
				http.Error(w, errStandby.Error(), http.StatusServiceUnavailable)
				return
			}
			http.Error(w, "mail watch lock connection lost", http.StatusServiceUnavailable)
			return
		}
		last := lastPass()
		if last.IsZero() {
			http.Error(w, "no pass has completed yet", http.StatusServiceUnavailable)
			return
		}
		if age := now().Sub(last); age > 3*reconcile {
			http.Error(w, fmt.Sprintf("no completed pass within 3 reconcile intervals (last %s ago)",
				age.Truncate(time.Second)), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
}
