package main

// GET /healthz for the resident watcher (SWT-75 D9): 200 "ok" iff a pass
// COMPLETED within 3 x SLACK_WATCH_INTERVAL and the singleton lock's
// connection answers; else 503 with a one-line reason naming which condition
// failed. Deliberately NOT in the verdict: whether the bridge is reachable,
// whether a pass found messages, whether any single conversation was
// readable — a wedged Mac mini must not crash-loop a pod in the cluster.
// Restarting the pod cannot fix Chrome; sync_runs error rows and the mini's
// own launchd recovery own that failure.
//
// A twin of cmd/orchestratord's newHealthHandler, re-spelled locally: a
// connector must not import the orchestrator (invariant 7).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// aliveChecker is anything with Alive: the watcher's lock handle in
// production, a fake in tests.
type aliveChecker interface {
	Alive(ctx context.Context) error
}

// errStandby is what the lock handle reports while ANOTHER watcher holds
// lockkeys.SlackWatch: this replica is correct and idle, not broken. It is its
// own word on /healthz so a log never reads it as a lost lock (a restart).
var errStandby = errors.New("standby: another slack watcher holds the lock")

func newWatchHealthHandler(interval time.Duration, now func() time.Time,
	lastPass func() time.Time, lock aliveChecker) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last := lastPass()
		if last.IsZero() {
			http.Error(w, "no pass has completed yet", http.StatusServiceUnavailable)
			return
		}
		if age := now().Sub(last); age > 3*interval {
			http.Error(w, fmt.Sprintf("no completed pass within 3 intervals (last %s ago)", age.Truncate(time.Second)),
				http.StatusServiceUnavailable)
			return
		}
		actx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := lock.Alive(actx); err != nil {
			if errors.Is(err, errStandby) {
				http.Error(w, "standby: another slack watcher holds the lock", http.StatusServiceUnavailable)
				return
			}
			slog.Error("healthz: slack watch lock connection lost", "err", err)
			http.Error(w, "slack watch lock connection lost", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
}
