package main

// slack-watch-sweep (SWT-75) criteria 23 and 24: GET /healthz on :8093.
// ZERO I/O — the handler is driven through httptest.ResponseRecorder with an
// injected clock, an injected last-pass reader and a fake lock handle, exactly
// as cmd/orchestratord/main_test.go drives its twin.
//
// IMPOSED SURFACE (package main; the SPEC fixes the behaviour, these names are
// chosen here and copied from cmd/orchestratord, which D9 names as the pattern
// to re-spell locally — a connector must not import the orchestrator,
// invariant 7):
//
//	// Anything with Alive — the watcher's lock handle in production, a fake here.
//	type aliveChecker interface{ Alive(ctx context.Context) error }
//
//	// errStandby is what the lock handle reports while ANOTHER watcher holds
//	// lockkeys.SlackWatch: /healthz answers 503 "standby" (D9), which is a
//	// different line from a LOST lock — standby is correct and expected on a
//	// second replica, a lost lock is a restart.
//	var errStandby error
//
//	// GET /healthz: 200 "ok" iff now()-lastPass() <= 3*interval AND lock.Alive
//	// returns nil; else 503 with a one-line reason naming which condition failed.
//	func newWatchHealthHandler(interval time.Duration, now func() time.Time,
//	    lastPass func() time.Time, lock aliveChecker) http.Handler
//
// GREENFIELD NOTE — EXPECTED RED: neither symbol exists, so package main's test
// build compile-FAILS ("undefined: newWatchHealthHandler", "undefined:
// errStandby").
//
// MUTATION: make /healthz fail when the bridge answers 503 -> criterion 24.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
)

type fakeWatchLock struct {
	err   error
	calls int
}

func (f *fakeWatchLock) Alive(context.Context) error {
	f.calls++
	return f.err
}

// Criterion 23's table: {fresh, stale, never} x {alive, dead, standby}.
// The clock is deliberately far from wall time — an implementation that
// consults time.Now() instead of the injected clock sees every "fresh" pass as
// years stale and fails the 200 rows.
func TestWatchHealthz_PassFreshnessAndLockState(t *testing.T) {
	const interval = 60 * time.Second
	now := time.Date(2026, 9, 22, 6, 0, 0, 0, time.UTC)
	lostConn := errors.New("FATAL: terminating connection due to administrator command (SQLSTATE 57P01)")

	cases := []struct {
		name        string
		lastPassAgo time.Duration
		never       bool
		lockErr     error
		wantCode    int
		wantReason  string // substring of the 503 body, case-insensitive
	}{
		{"fresh pass, lock alive", 20 * time.Second, false, nil, http.StatusOK, ""},
		{"pass just inside 3x interval, lock alive", 3*interval - time.Second, false, nil, http.StatusOK, ""},
		{"stale pass, lock alive", 3*interval + time.Second, false, nil, http.StatusServiceUnavailable, "pass"},
		{"no pass has ever completed, lock alive", 0, true, nil, http.StatusServiceUnavailable, "pass"},
		{"fresh pass, lock connection lost", 20 * time.Second, false, lostConn, http.StatusServiceUnavailable, "lock"},
		{"fresh pass, standby", 20 * time.Second, false, errStandby, http.StatusServiceUnavailable, "standby"},
		{"stale pass, standby", 10 * time.Minute, false, errStandby, http.StatusServiceUnavailable, ""},
		{"never, standby", 0, true, errStandby, http.StatusServiceUnavailable, ""},
		{"never, lock lost", 0, true, lostConn, http.StatusServiceUnavailable, ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			lock := &fakeWatchLock{err: tc.lockErr}
			last := now.Add(-tc.lastPassAgo)
			if tc.never {
				last = time.Time{} // the zero instant: no pass has completed
			}
			h := newWatchHealthHandler(interval, func() time.Time { return now },
				func() time.Time { return last }, lock)

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			body := strings.TrimSpace(rec.Body.String())

			if rec.Code != tc.wantCode {
				t.Fatalf("GET /healthz = %d (%q), want %d. D9: 200 iff a pass COMPLETED within 3 x "+
					"SLACK_WATCH_INTERVAL AND the lock is alive", rec.Code, body, tc.wantCode)
			}
			if tc.wantCode == http.StatusOK {
				if body != "ok" {
					t.Errorf("200 body = %q, want exactly \"ok\"", body)
				}
				if lock.calls == 0 {
					t.Error("the handler answered 200 without calling lock.Alive: a standby replica would then " +
						"pass its own liveness probe while doing nothing at all")
				}
				return
			}
			if body == "" || body == "ok" {
				t.Errorf("503 body = %q, want a one-line reason naming which condition failed (criterion 23)", body)
			}
			if strings.Contains(body, "\n") || strings.ContainsAny(body, "{}") {
				t.Errorf("503 body = %q: one line naming the failing condition, not a JSON document", body)
			}
			if tc.wantReason != "" && !strings.Contains(strings.ToLower(body), tc.wantReason) {
				t.Errorf("503 body = %q, want it to name the failing check (%q). \"standby\" in particular must "+
					"be its own word: a second replica losing the race is CORRECT, while a lost lock is a "+
					"restart, and the two must not read the same in a log", body, tc.wantReason)
			}
		})
	}
}

// Criterion 24, the whole point of the health verdict's narrowness: with every
// /export call answering 503 and the loop still cycling, /healthz stays 200
// until the pass-completion window lapses. A wedged mini must NOT crash-loop a
// pod in the cluster — restarting the pod cannot fix Chrome, and the mini's own
// launchd recovery plus sync_runs error rows already own that failure (D9).
//
// The watcher is real; only the clock and the pass are fakes. The pass always
// answers with the leaf's busy 503.
func TestWatchHealthz_IgnoresABridgeThatAnswersOnly503(t *testing.T) {
	const interval = 60 * time.Second
	clock := &healthClock{now: time.Date(2026, 9, 22, 6, 0, 0, 0, time.UTC)}
	start := clock.now

	passes := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := slackweb.NewWatcher(
		slackweb.WatchConfig{Interval: interval, RotationInterval: 30 * time.Minute,
			BudgetMS: 150000, BridgeGrace: 120 * time.Second, HealthAddr: ":8093"},
		slackweb.WatchDeps{
			Now:   clock.Now,
			Sleep: clock.Sleep,
			Pass: func(context.Context, slackweb.PassKind) error {
				passes++
				if passes >= 2 {
					cancel()
				}
				return &slackweb.BridgeBusyError{RetryAfter: 30 * time.Second, Body: "sweep already running"}
			},
		})
	if err := w.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil: a bridge that only 503s is a skipped pass, never a failed process", err)
	}
	if passes < 2 {
		t.Fatalf("the loop stopped after %d passes; it must keep cycling against a busy bridge (criterion 14)", passes)
	}

	lock := &fakeWatchLock{}
	serve := func(at time.Time) int {
		h := newWatchHealthHandler(interval, func() time.Time { return at }, w.LastPassAt, lock)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		return rec.Code
	}

	if code := serve(start.Add(2 * interval)); code != http.StatusOK {
		t.Errorf("GET /healthz = %d two intervals into a wholly-503 bridge, want 200. Criterion 24: the "+
			"verdict deliberately EXCLUDES whether the bridge is reachable, whether the pass found messages, "+
			"and whether any single conversation was readable. It also means LastPassAt starts at process "+
			"start, not at the zero instant", code)
	}
	if code := serve(start.Add(10 * interval)); code != http.StatusServiceUnavailable {
		t.Errorf("GET /healthz = %d ten intervals in with no completed pass, want 503: the window is what "+
			"eventually reports a watcher that is achieving nothing (criterion 23)", code)
	}
}

// healthClock is the same deterministic clock watch_test.go uses: Sleep
// advances it, so the loop runs at CPU speed on an exact schedule.
type healthClock struct{ now time.Time }

func (c *healthClock) Now() time.Time { return c.now }

func (c *healthClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.now = c.now.Add(d)
	return nil
}
