package main

// SWT-41 (docs/tickets/orchestrator-deploy_SPEC.md) criteria 5 and 6, unit
// half. ZERO I/O: no db, no broker, no listener — the handler is driven through
// httptest.ResponseRecorder with an injected clock, an injected last-tick
// reader and a fake lock handle.
//
// IMPOSED SURFACE (package main; the SPEC fixes the behaviour, these names are
// chosen here and the implementation adopts them):
//
//	// Anything with Alive — *orchestrator.LockHandle in production, a fake here.
//	type aliveChecker interface{ Alive(ctx context.Context) error }
//
//	// GET /healthz: 200 "ok" iff now()-lastTick() <= 3*tick AND lock.Alive
//	// returns nil; else 503 with a one-line reason and nothing else.
//	func newHealthHandler(tick time.Duration, now func() time.Time,
//	    lastTick func() time.Time, lock aliveChecker) http.Handler
//
//	// D3: called on every tick. Alive error -> log, exit(non-zero), return
//	// false. Alive nil -> return true, exit never called. main passes os.Exit.
//	func checkLockOrExit(ctx context.Context, lock aliveChecker, exit func(code int)) bool
//
// GREENFIELD NOTE — EXPECTED RED. None of these exist, so package main's test
// build compile-FAILS ("undefined: newHealthHandler", "undefined:
// checkLockOrExit").

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeLock struct {
	err   error
	calls int
}

func (f *fakeLock) Alive(context.Context) error {
	f.calls++
	return f.err
}

// Criterion 6. The clock is deliberately far from wall time (2020): an
// implementation that consults time.Now()/time.Since instead of the injected
// clock sees every "fresh" tick as years stale and fails the 200 rows.
func TestHealthz_TickFreshnessAndLockLiveness(t *testing.T) {
	const tick = 60 * time.Second
	now := time.Date(2020, 3, 1, 12, 0, 0, 0, time.UTC)
	lostConn := errors.New("FATAL: terminating connection due to administrator command (SQLSTATE 57P01)")

	cases := []struct {
		name        string
		lastTickAgo time.Duration
		lockErr     error
		wantCode    int
		wantReason  string // substring of the 503 body, case-insensitive
	}{
		{"fresh tick, lock alive", 10 * time.Second, nil, http.StatusOK, ""},
		{"tick just inside 3x tick, lock alive", 3*tick - time.Second, nil, http.StatusOK, ""},
		{"tick older than 3x tick (wedged loop), lock alive", 3*tick + time.Second, nil, http.StatusServiceUnavailable, "tick"},
		{"fresh tick, lock connection lost", 10 * time.Second, lostConn, http.StatusServiceUnavailable, "lock"},
		{"both failing", time.Hour, lostConn, http.StatusServiceUnavailable, ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			lock := &fakeLock{err: tc.lockErr}
			last := now.Add(-tc.lastTickAgo)
			h := newHealthHandler(tick, func() time.Time { return now }, func() time.Time { return last }, lock)

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			body := strings.TrimSpace(rec.Body.String())

			if rec.Code != tc.wantCode {
				t.Fatalf("GET /healthz = %d (%q), want %d. D5: 200 iff a tick iteration completed within "+
					"3 x tick AND the lock handle is Alive, else 503", rec.Code, body, tc.wantCode)
			}
			if tc.wantCode == http.StatusOK {
				if body != "ok" {
					t.Errorf("200 body = %q, want exactly \"ok\" (criterion 6: no data beyond ok / the reason)", body)
				}
				if lock.calls == 0 {
					t.Error("the handler answered 200 without calling lock.Alive: a dead lock connection " +
						"would then pass the liveness probe")
				}
				return
			}
			if body == "" || body == "ok" {
				t.Errorf("503 body = %q, want a one-line failing reason", body)
			}
			if strings.Contains(body, "\n") || strings.ContainsAny(body, "{}") {
				t.Errorf("503 body = %q: criterion 6 says the body carries no data beyond the failing reason "+
					"(one line, not a JSON document)", body)
			}
			if tc.wantReason != "" && !strings.Contains(strings.ToLower(body), tc.wantReason) {
				t.Errorf("503 body = %q, want it to name the failing check (%q)", body, tc.wantReason)
			}
		})
	}
}

// Criterion 5: a lost lock exits NON-ZERO through the injected func.
func TestCheckLockOrExit_LostLockExitsNonZero(t *testing.T) {
	var codes []int
	lock := &fakeLock{err: errors.New("conn closed")}
	alive := checkLockOrExit(context.Background(), lock, func(code int) { codes = append(codes, code) })

	if alive {
		t.Error("checkLockOrExit returned true for a lock whose Alive errored")
	}
	if len(codes) != 1 {
		t.Fatalf("exit called %d times, want exactly 1. D3: losing the lock connection must turn into a "+
			"restart, or a CNPG switchover leaves an unlocked engine draining", len(codes))
	}
	if codes[0] == 0 {
		t.Error("exit(0) on a lost lock: Kubernetes treats that as success. D3 says exit non-zero")
	}
}

func TestCheckLockOrExit_AliveLockDoesNotExit(t *testing.T) {
	exited := false
	lock := &fakeLock{}
	alive := checkLockOrExit(context.Background(), lock, func(int) { exited = true })
	if !alive || exited {
		t.Errorf("checkLockOrExit(alive lock) = %v, exited=%v; want true and no exit", alive, exited)
	}
	if lock.calls != 1 {
		t.Errorf("Alive called %d times, want 1 (one SELECT 1 per tick)", lock.calls)
	}
}
