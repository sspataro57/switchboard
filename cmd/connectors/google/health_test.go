package main

// imap-idle-watch (SWT-73) criterion 6 / D7: GET /healthz on :8092. ZERO I/O —
// the handler is driven through httptest.ResponseRecorder with an injected
// clock, an injected last-pass reader and a fake lock handle, exactly as
// cmd/orchestratord/main_test.go:14-17 and cmd/connectors/slackweb/health_test.go
// drive their twins.
//
// IK, the SWT-41 landmine this ticket answers: "A daemon a ticket depends on
// must be in the Dockerfile build line, have a manifest, and have a health
// signal judged from OUTSIDE the process, or the ticket is not delivered."
//
// IMPOSED SURFACE (package main, cmd/connectors/google/health.go (new); copied
// from cmd/orchestratord, which D7 names as the pattern to re-spell locally —
// a connector must not import the orchestrator, invariant 7):
//
//	// Anything with Alive — the watcher's lock handle in production, a fake here.
//	type aliveChecker interface{ Alive(ctx context.Context) error }
//
//	// errStandby is what the lock handle reports while ANOTHER watcher holds
//	// lockkeys.MailWatch: /healthz answers 503 "standby" (D4), a DIFFERENT line
//	// from a LOST lock — standby is correct and expected on a second replica
//	// during a node drain, a lost lock is a restart.
//	var errStandby error
//	type standbyLock struct{} // Alive always answers errStandby
//
//	// GET /healthz: 200 "ok" iff now()-lastPass() <= 3*reconcile AND lock.Alive
//	// returns nil; else 503 with exactly one line naming which condition failed.
//	func newWatchHealthHandler(reconcile time.Duration, now func() time.Time,
//	    lastPass func() time.Time, lock aliveChecker) http.Handler
//
// GREENFIELD NOTE — EXPECTED RED: neither symbol exists, so package main's test
// build compile-FAILS ("undefined: newWatchHealthHandler", "undefined:
// errStandby", "undefined: standbyLock").
//
// MUTATION: make /healthz 200 whenever the process is up -> criterion 6.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type mwHealthLock struct {
	err   error
	calls int
}

func (f *mwHealthLock) Alive(context.Context) error {
	f.calls++
	return f.err
}

// Criterion 6's table: {fresh, stale, never-completed} x {alive, dead, standby}.
// The clock is injected and deliberately not wall time: an implementation that
// reaches for time.Now()/time.Since instead sees every "fresh" pass as stale
// and fails the 200 rows.
func TestMailWatchHealthz_PassFreshnessAndLockState(t *testing.T) {
	const reconcile = 10 * time.Minute
	now := time.Date(2026, 9, 22, 6, 0, 0, 0, time.UTC)
	lostConn := errors.New("FATAL: terminating connection due to administrator command (SQLSTATE 57P01)")

	cases := []struct {
		name        string
		lastPassAgo time.Duration
		never       bool
		lockErr     error
		wantCode    int
		wantReason  string // substring of the 503 body, lower-cased
	}{
		{"fresh pass, lock alive", 30 * time.Second, false, nil, http.StatusOK, ""},
		{"a quiet mailbox is not a sick one", 9 * time.Minute, false, nil, http.StatusOK, ""},
		{"pass just inside 3x reconcile", 3*reconcile - time.Second, false, nil, http.StatusOK, ""},
		{"stale pass, lock alive", 3*reconcile + time.Second, false, nil, http.StatusServiceUnavailable, "pass"},
		{"no pass has ever completed", 0, true, nil, http.StatusServiceUnavailable, "pass"},
		{"fresh pass, lock connection lost", 30 * time.Second, false, lostConn, http.StatusServiceUnavailable, "lock"},
		{"fresh pass, standby", 30 * time.Second, false, errStandby, http.StatusServiceUnavailable, "standby"},
		{"stale pass, standby", time.Hour, false, errStandby, http.StatusServiceUnavailable, ""},
		{"never, lock lost", 0, true, lostConn, http.StatusServiceUnavailable, ""},
		// 2026-09-22 (review): the REAL standby state — a replica that lost the
		// race has never completed a pass. The lock is consulted first so the
		// body says standby, not "no pass has completed yet" (D4).
		{"never, standby", 0, true, errStandby, http.StatusServiceUnavailable, "standby"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			lock := &mwHealthLock{err: tc.lockErr}
			last := now.Add(-tc.lastPassAgo)
			if tc.never {
				last = time.Time{}
			}
			h := newWatchHealthHandler(reconcile, func() time.Time { return now },
				func() time.Time { return last }, lock)

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			body := strings.TrimSpace(rec.Body.String())

			if rec.Code != tc.wantCode {
				t.Fatalf("GET /healthz = %d (%q), want %d. D7: 200 iff a watchPass COMPLETED within "+
					"3 x MAIL_RECONCILE_INTERVAL AND the singleton lock is Alive", rec.Code, body, tc.wantCode)
			}
			if tc.wantCode == http.StatusOK {
				if body != "ok" {
					t.Errorf("200 body = %q, want exactly \"ok\"", body)
				}
				if lock.calls == 0 {
					t.Error("the handler answered 200 without calling lock.Alive. A watcher whose lock " +
						"connection died would then pass its own liveness probe forever while a second pod " +
						"holds the key and does the work (criterion 6)")
				}
				return
			}
			if body == "" || body == "ok" {
				t.Errorf("503 body = %q, want exactly one line naming which condition failed (criterion 6)", body)
			}
			if strings.Count(body, "\n") > 0 || strings.ContainsAny(body, "{}") {
				t.Errorf("503 body = %q: one line naming the failing condition, not a JSON document or a "+
					"stack of them (criterion 6)", body)
			}
			if tc.wantReason != "" && !strings.Contains(strings.ToLower(body), tc.wantReason) {
				t.Errorf("503 body = %q, want it to name the failing check (%q). \"standby\" in particular "+
					"must be its own word (D4): a second replica losing the race during a node drain is "+
					"CORRECT and must not read like a lost lock, which is a restart", body, tc.wantReason)
			}
		})
	}
}

// D4: the handle a standing-by replica exposes to /healthz answers errStandby,
// so the 503 reason is "standby" before the lock has ever been acquired — not
// "no pass has completed", which would be true but would read as a broken
// watcher in a log.
func TestMailWatchHealthz_StandbyLockIsItsOwnReason(t *testing.T) {
	if err := (standbyLock{}).Alive(context.Background()); !errors.Is(err, errStandby) {
		t.Fatalf("standbyLock.Alive() = %v, want errStandby (D4)", err)
	}
	now := time.Date(2026, 9, 22, 6, 0, 0, 0, time.UTC)
	h := newWatchHealthHandler(10*time.Minute, func() time.Time { return now },
		func() time.Time { return now.Add(-time.Minute) }, standbyLock{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /healthz on a standby replica = %d, want 503 (criterion 2)", rec.Code)
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "standby") {
		t.Errorf("the standby 503 reads %q; criterion 2 names the reason `standby`, and D4 is explicit that "+
			"a node drain leaving two pods briefly alive is normal, not a fault", rec.Body.String())
	}
}
