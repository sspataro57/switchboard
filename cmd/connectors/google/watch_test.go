package main

// imap-idle-watch (SWT-73) — the resident loop. ZERO I/O: no Postgres, no IMAP,
// no broker, no MQTT. Every seam the loop needs is injected, exactly as
// internal/connector/slackweb's Watcher injects its pass function, so the
// behaviour a resident process is judged on (standby, lost lock, a bounded
// pass, per-account failure isolation, the account set) is testable at CPU
// speed. The loop has NO tests today (SPEC "What exists": grep for
// runWatch|idleOnce|watchPass finds two non-test files and nothing else).
//
// Criteria covered here: 2 (standby), 3 (per-tick Alive), 4 (clean shutdown),
// 5 (MAIL_PASS_TIMEOUT), 7 (health ignores IDLE), 9 (one account's failure),
// 10 (D9's recovery row), 14 (the account set, unit half).
//
// IMPOSED SURFACE (package main, cmd/connectors/google/watch.go; the SPEC fixes
// the behaviour, these names are chosen here and mirror
// cmd/connectors/slackweb + internal/connector/slackweb, which D1/D4/D7 name as
// the pattern to re-spell locally — a connector must not import
// internal/orchestrator, invariant 7):
//
//	// The four knobs, read with watch.go:51-63's defensive envDuration: an
//	// unparseable value falls back, never to a zero interval that would spin.
//	type watchConfig struct {
//	    Reconcile    time.Duration // MAIL_RECONCILE_INTERVAL, default 10m
//	    IdleRefresh  time.Duration // MAIL_IDLE_REFRESH,       default 25m
//	    PassTimeout  time.Duration // MAIL_PASS_TIMEOUT,       default 10m  (D6)
//	    StandbyRetry time.Duration // D4: 15s, not an env knob
//	    HealthAddr   string        // MAIL_WATCH_HEALTH_ADDR,  default ":8092"
//	}
//	func watchConfigFromEnv() watchConfig
//
//	// The singleton lock as the loop sees it (*watchLock in production).
//	type lockHandle interface {
//	    Alive(ctx context.Context) error
//	    Release()
//	}
//
//	// The per-account advisory lock and the sync_runs rows (*google.PGSink).
//	type accountSink interface {
//	    LockAccount(ctx context.Context, accountID int64) (release func(), ok bool, err error)
//	    StartRun(ctx context.Context, accountID int64, phase string) (int64, error)
//	    FinishRun(ctx context.Context, runID int64, status string, stats google.Stats, errMsg string) error
//	}
//
//	type watchDeps struct {
//	    Now      func() time.Time
//	    Sleep    func(ctx context.Context, d time.Duration) error
//	    Acquire  func(ctx context.Context) (lockHandle, bool, error)          // tryMailWatchLock
//	    Accounts func(ctx context.Context) ([]google.Account, error)          // google.ListIMAPAccounts
//	    Pass     func(ctx context.Context, cfg google.Config, why string)     // watchPass
//	    Idle     func(ctx context.Context, acct google.Account, wake chan<- string) error // idleOnce
//	    Sink     accountSink
//	}
//
//	func newWatcher(cfg watchConfig, mail google.Config, deps watchDeps) *watcher
//	func (w *watcher) Run(ctx context.Context) error   // nil on ctx end, non-nil on a LOST lock
//	func (w *watcher) LastPassAt() time.Time           // last COMPLETED pass; /healthz's input
//	func (w *watcher) Health() aliveChecker            // standbyLock{} until the lock is held
//	func (w *watcher) Watching() []string              // emails with a live IDLE goroutine
//	func (w *watcher) watchAccount(ctx context.Context, acct google.Account, wake chan<- string)
//
// GREENFIELD NOTE — EXPECTED RED: none of these exist. This file compile-FAILS
// ("undefined: watchConfigFromEnv", "undefined: newWatcher", …), which is the
// expected failure mode for greenfield symbols.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

// ---- fakes -------------------------------------------------------------------

// mwFakeLock is the singleton lock handle. AliveErr is consulted per call
// through the errAt script, so a lock can be alive for N checks and then gone —
// which is what a CNPG switchover looks like from inside the process.
type mwFakeLock struct {
	mu       sync.Mutex
	calls    int
	errAfter int // Alive fails from this call number on; 0 = never
	released int
}

func (l *mwFakeLock) Alive(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.errAfter > 0 && l.calls >= l.errAfter {
		return errors.New("FATAL: terminating connection due to administrator command (SQLSTATE 57P01)")
	}
	return nil
}

func (l *mwFakeLock) Release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.released++
}

func (l *mwFakeLock) counts() (alive, released int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls, l.released
}

// mwRunRow is one sync_runs row the fake sink recorded.
type mwRunRow struct {
	accountID int64
	phase     string
	status    string
	errMsg    string
}

// mwFakeSink records the per-account lock traffic and every sync_runs row.
// Nothing here touches Postgres: the point of criterion 10 is the SHAPE of the
// row sequence, which is a pure property of the loop.
type mwFakeSink struct {
	mu       sync.Mutex
	rows     []mwRunRow
	open     map[int64]mwRunRow
	nextID   int64
	locks    int
	released int
	busy     bool // LockAccount answers ok=false
	lockErr  error
	order    []string // "lock", "release", per call, in order
}

func newMWFakeSink() *mwFakeSink { return &mwFakeSink{open: map[int64]mwRunRow{}} }

func (s *mwFakeSink) LockAccount(ctx context.Context, accountID int64) (func(), bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lockErr != nil {
		return nil, false, s.lockErr
	}
	if s.busy {
		return nil, false, nil
	}
	s.locks++
	s.order = append(s.order, "lock")
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.released++
		s.order = append(s.order, "release")
	}, true, nil
}

func (s *mwFakeSink) StartRun(ctx context.Context, accountID int64, phase string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	s.open[s.nextID] = mwRunRow{accountID: accountID, phase: phase}
	return s.nextID, nil
}

func (s *mwFakeSink) FinishRun(ctx context.Context, runID int64, status string, stats google.Stats, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.open[runID]
	if !ok {
		return fmt.Errorf("FinishRun on run %d that was never started", runID)
	}
	row.status, row.errMsg = status, errMsg
	s.rows = append(s.rows, row)
	delete(s.open, runID)
	return nil
}

func (s *mwFakeSink) snapshot() []mwRunRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]mwRunRow, len(s.rows))
	copy(out, s.rows)
	return out
}

func (s *mwFakeSink) lockOrder() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.order))
	copy(out, s.order)
	return out
}

// mwClock is a deterministic clock: Sleep advances it instead of waiting, and
// every duration slept is recorded so the backoff schedule can be asserted.
// After limit sleeps it cancels, so a loop under test always terminates.
type mwClock struct {
	mu     sync.Mutex
	now    time.Time
	slept  []time.Duration
	limit  int
	cancel context.CancelFunc
}

func newMWClock(limit int) *mwClock {
	return &mwClock{now: time.Date(2026, 9, 22, 6, 0, 0, 0, time.UTC), limit: limit}
}

func (c *mwClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *mwClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.slept = append(c.slept, d)
	over := c.limit > 0 && len(c.slept) >= c.limit
	cancel := c.cancel
	c.mu.Unlock()
	if over && cancel != nil {
		cancel()
	}
	return ctx.Err()
}

func (c *mwClock) sleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Duration, len(c.slept))
	copy(out, c.slept)
	return out
}

func mwAccount(id int64, email string) google.Account {
	return google.Account{ID: id, Email: email, AuthType: google.AuthTypeAppPassword}
}

// ---- criterion 5 (the knobs) --------------------------------------------------

// Criterion 5: MAIL_PASS_TIMEOUT defaults to 10m — the one-shot path's own
// bound (main.go:84), proven on these four mailboxes every ten minutes for two
// months — and is read with watch.go:51-63's defensive parse, so a typo falls
// back instead of producing a zero timeout that would cancel every pass before
// it began.
//
// MUTATION: remove the pass timeout -> criterion 5.
func TestWatchConfigFromEnv_DefaultsAndDefensiveParse(t *testing.T) {
	t.Run("unset", func(t *testing.T) {
		for _, name := range []string{"MAIL_PASS_TIMEOUT", "MAIL_RECONCILE_INTERVAL", "MAIL_IDLE_REFRESH", "MAIL_WATCH_HEALTH_ADDR"} {
			t.Setenv(name, "")
		}
		cfg := watchConfigFromEnv()
		if cfg.PassTimeout != 10*time.Minute {
			t.Errorf("PassTimeout = %s, want 10m (D6: the one-shot path's own bound, main.go:84)", cfg.PassTimeout)
		}
		if cfg.Reconcile != defaultReconcile {
			t.Errorf("Reconcile = %s, want %s", cfg.Reconcile, defaultReconcile)
		}
		if cfg.IdleRefresh != defaultIdleRefresh {
			t.Errorf("IdleRefresh = %s, want %s (under RFC 2177's 29m ceiling)", cfg.IdleRefresh, defaultIdleRefresh)
		}
		if cfg.HealthAddr != ":8092" {
			t.Errorf("HealthAddr = %q, want \":8092\" (D7: hooksd :8090, orchestratord :8091, slackweb :8093)", cfg.HealthAddr)
		}
		if cfg.StandbyRetry <= 0 || cfg.StandbyRetry > time.Minute {
			t.Errorf("StandbyRetry = %s, want D4's 15s: a node drain leaves the old pod terminating while "+
				"the new one starts, and the new one must take over promptly without crash-looping", cfg.StandbyRetry)
		}
	})

	t.Run("garbage falls back, never to zero", func(t *testing.T) {
		// "600" is the realistic typo: not a Go duration. Read as 600ns it would
		// cancel every pass 600 nanoseconds in, and every pass would be logged as
		// a timeout with no mail ever ingested.
		for _, bad := range []string{"600", "ten minutes", "-5m", "0"} {
			t.Setenv("MAIL_PASS_TIMEOUT", bad)
			if got := watchConfigFromEnv().PassTimeout; got != 10*time.Minute {
				t.Errorf("MAIL_PASS_TIMEOUT=%q gave PassTimeout=%s, want the 10m fallback (watch.go:51-63's "+
					"envDuration discipline: a typo must never produce a zero or negative interval)", bad, got)
			}
		}
	})

	t.Run("a real duration is honoured", func(t *testing.T) {
		t.Setenv("MAIL_PASS_TIMEOUT", "90s")
		t.Setenv("MAIL_WATCH_HEALTH_ADDR", ":9999")
		cfg := watchConfigFromEnv()
		if cfg.PassTimeout != 90*time.Second {
			t.Errorf("PassTimeout = %s, want 90s", cfg.PassTimeout)
		}
		if cfg.HealthAddr != ":9999" {
			t.Errorf("HealthAddr = %q, want \":9999\"", cfg.HealthAddr)
		}
	})
}

// ---- criterion 2: standby ------------------------------------------------------

// Criterion 2: the lock is acquired BEFORE the initial pass. Held elsewhere, the
// watcher stands by — it logs once, answers /healthz 503 "standby", retries
// every 15 s, and runs NO pass meanwhile (no sync_runs row, no raw write) — and
// it takes over when the holder goes away, without restarting.
//
// D4 spells out why this is not orchestratord's exit: "a node drain can leave
// the old pod terminating while the new one starts, and a crash-loop there
// would be self-inflicted".
//
// MUTATIONS: drop the singleton lock acquisition / exit instead of standby ->
// criterion 2.
func TestWatcherRun_StandsByUntilTheLockIsFree(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clock := newMWClock(0)
	sink := newMWFakeSink()
	lock := &mwFakeLock{}

	var w *watcher
	attempts := 0
	acquire := func(context.Context) (lockHandle, bool, error) {
		attempts++
		if attempts <= 3 {
			// While standing by, /healthz must say STANDBY — its own word. A
			// second replica that lost the race is CORRECT and idle; a lock that
			// was LOST is a restart, and the two must not read the same in a log.
			if err := w.Health().Alive(ctx); !errors.Is(err, errStandby) {
				t.Errorf("while standing by, Health().Alive() = %v, want errStandby. Criterion 2: /healthz "+
					"answers 503 with reason \"standby\" for as long as another watcher holds the lock", err)
			}
			return nil, false, nil
		}
		return lock, true, nil
	}

	passes := 0
	var passesBeforeLock int
	w = newWatcher(
		watchConfig{Reconcile: time.Hour, IdleRefresh: 25 * time.Minute, PassTimeout: time.Minute,
			StandbyRetry: 15 * time.Second, HealthAddr: ":0"},
		google.Config{},
		watchDeps{
			Now:      clock.Now,
			Sleep:    clock.Sleep,
			Acquire:  acquire,
			Accounts: func(context.Context) ([]google.Account, error) { return nil, nil },
			Idle: func(ctx context.Context, _ google.Account, _ chan<- string) error {
				<-ctx.Done()
				return ctx.Err()
			},
			Pass: func(context.Context, google.Config, string) {
				passes++
				if attempts <= 3 {
					passesBeforeLock++
				}
				cancel() // one pass is all this test needs
			},
			Sink: sink,
		})

	if err := w.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil. Criterion 2: contention on the singleton lock is STANDBY, never an "+
			"error and never an exit — D4 chose this over orchestratord's exit precisely so a node drain "+
			"cannot crash-loop the new pod", err)
	}

	if passesBeforeLock != 0 {
		t.Errorf("%d pass(es) ran while the lock was held elsewhere. Criterion 2: a standby watcher runs no "+
			"pass at all — no sync_runs row, no raw_source_items write — or two processes are reading the "+
			"same four mailboxes and the singleton lock buys nothing", passesBeforeLock)
	}
	if len(sink.snapshot()) != 0 {
		t.Errorf("a standby watcher wrote sync_runs rows: %v (criterion 2)", sink.snapshot())
	}
	if passes == 0 {
		t.Error("the watcher never ran a pass after the lock came free. Criterion 2: it acquires the lock " +
			"when the holder goes away, WITHOUT restarting the process")
	}
	if attempts != 4 {
		t.Errorf("Acquire was called %d times, want 4 (three refusals then the take-over)", attempts)
	}

	waits := clock.sleeps()
	if len(waits) != 3 {
		t.Fatalf("the standby loop slept %d times for %v, want one 15s wait per refusal (criterion 2). A "+
			"standby that spins on pg_try_advisory_lock burns a pooled connection per round trip", len(waits), waits)
	}
	for i, d := range waits {
		if d != 15*time.Second {
			t.Errorf("standby wait %d = %s, want 15s (D4)", i, d)
		}
	}
}

// ---- criterion 3: the lock is checked on every tick ----------------------------

// Criterion 3: the lock is Alive-checked on every reconcile tick, and LOSING it
// makes Run return a non-nil error so main can exit non-zero and the kubelet can
// restart the pod. A CNPG switchover kills the holding connection silently —
// the session lock is gone, a second watcher could take it, and this one would
// go on IDLE-ing four mailboxes believing it is the singleton.
//
// MUTATION: skip the per-tick Alive check -> criterion 3.
func TestWatcherRun_LostLockEndsTheLoopWithAnError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	lock := &mwFakeLock{errAfter: 2} // alive for the first check, gone for the second
	sink := newMWFakeSink()
	clock := newMWClock(0)

	var mu sync.Mutex
	passes := 0
	w := newWatcher(
		watchConfig{Reconcile: 2 * time.Millisecond, IdleRefresh: time.Minute, PassTimeout: time.Second,
			StandbyRetry: 15 * time.Second, HealthAddr: ":0"},
		google.Config{},
		watchDeps{
			Now:      clock.Now,
			Sleep:    clock.Sleep,
			Acquire:  func(context.Context) (lockHandle, bool, error) { return lock, true, nil },
			Accounts: func(context.Context) ([]google.Account, error) { return nil, nil },
			Idle: func(ctx context.Context, _ google.Account, _ chan<- string) error {
				<-ctx.Done()
				return ctx.Err()
			},
			Pass: func(context.Context, google.Config, string) {
				mu.Lock()
				passes++
				mu.Unlock()
			},
			Sink: sink,
		})

	err := w.Run(ctx)
	if err == nil {
		t.Fatal("Run returned nil after the singleton lock was lost. Criterion 3: loss makes runWatch return " +
			"a non-nil error and the process exit non-zero (the os.Exit stays in main). A watcher that " +
			"kept going would be a SECOND watcher the moment another pod took the freed key")
	}
	if ctx.Err() != nil {
		t.Fatalf("Run only ended because the test's own 5s deadline fired (%v); the lost-lock check never "+
			"ran on a tick", ctx.Err())
	}
	alive, released := lock.counts()
	if alive < 2 {
		t.Errorf("Alive was called %d times; criterion 3 checks it on EVERY reconcile tick", alive)
	}
	if released == 0 {
		t.Error("the lock connection was not released on the way out; the pooled connection leaks for the " +
			"life of the pod (criterion 3/4)")
	}
	mu.Lock()
	defer mu.Unlock()
	if passes == 0 {
		t.Error("POSITIVE CONTROL FAILED: the loop never ran a pass, so this test proves nothing about the " +
			"per-tick check")
	}
}

// ---- criterion 4: clean shutdown ------------------------------------------------

// Criterion 4: SIGTERM/SIGINT (the existing signal.NotifyContext, watch.go:67)
// shuts down cleanly — no pass is STARTED after cancellation, the lock
// connection is released, exit code 0. D2 gives the Deployment
// terminationGracePeriodSeconds: 120 for exactly this.
func TestWatcherRun_ShutdownStartsNoPassAndReleasesTheLock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lock := &mwFakeLock{}
	clock := newMWClock(0)

	var mu sync.Mutex
	var whys []string
	afterCancel := 0
	w := newWatcher(
		watchConfig{Reconcile: 2 * time.Millisecond, IdleRefresh: time.Minute, PassTimeout: time.Second,
			StandbyRetry: 15 * time.Second, HealthAddr: ":0"},
		google.Config{},
		watchDeps{
			Now:      clock.Now,
			Sleep:    clock.Sleep,
			Acquire:  func(context.Context) (lockHandle, bool, error) { return lock, true, nil },
			Accounts: func(context.Context) ([]google.Account, error) { return nil, nil },
			Idle: func(ctx context.Context, _ google.Account, _ chan<- string) error {
				<-ctx.Done()
				return ctx.Err()
			},
			Pass: func(pctx context.Context, _ google.Config, why string) {
				mu.Lock()
				whys = append(whys, why)
				if ctx.Err() != nil {
					afterCancel++
				}
				n := len(whys)
				mu.Unlock()
				if n == 1 {
					cancel() // SIGTERM arrives during the first pass
				}
			},
			Sink: newMWFakeSink(),
		})

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v after cancellation, want nil (criterion 4: exit code 0)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of cancellation. Criterion 4: SIGTERM shuts down cleanly; " +
			"D2's terminationGracePeriodSeconds:120 lets an in-flight pass finish, it does not license a " +
			"loop that ignores ctx.Done()")
	}

	mu.Lock()
	defer mu.Unlock()
	if afterCancel > 0 {
		t.Errorf("%d pass(es) were STARTED after the context was cancelled (whys: %v). Criterion 4: no pass "+
			"is started after cancellation", afterCancel, whys)
	}
	if len(whys) == 0 || whys[0] == "" {
		t.Errorf("the first pass ran with why=%q; every pass carries a why string (\"initial\", "+
			"\"reconcile\", \"wake <address>\") because that is what a log reader greps for", whys)
	}
	if _, released := lock.counts(); released != 1 {
		t.Errorf("the lock was released %d times, want exactly 1 on the way out (criterion 4)", released)
	}
}

// ---- criterion 5: every pass is bounded -----------------------------------------

// Criterion 5 / D6: MAIL_PASS_TIMEOUT bounds EVERY watchPass. A pass that
// exceeds it is cancelled, the loop continues, and the next wake or tick runs a
// fresh pass. Today watchMain runs on context.Background() (main.go:66) and
// client.DialTLS sets no deadline (imap.go:339), so a wedged fetch stalls the
// loop forever with nothing to notice — cron's per-run process death is what
// covers that in production right now, and this is the single change that most
// reduces the risk of moving off it.
//
// The cursor discipline is what makes a cancelled pass safe: it advances only
// after a complete folder pass, so a truncated pass re-fetches rather than skips.
//
// MUTATION: remove the pass timeout -> this test hangs on the first pass and
// fails on the 10s deadline below.
func TestWatcherRun_BoundsEveryPassAndKeepsGoing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const passTimeout = 80 * time.Millisecond
	lock := &mwFakeLock{}
	clock := newMWClock(0)

	var mu sync.Mutex
	var deadlines []time.Duration
	var errs []error
	passes := 0

	w := newWatcher(
		watchConfig{Reconcile: 2 * time.Millisecond, IdleRefresh: time.Minute, PassTimeout: passTimeout,
			StandbyRetry: 15 * time.Second, HealthAddr: ":0"},
		google.Config{},
		watchDeps{
			Now:      clock.Now,
			Sleep:    clock.Sleep,
			Acquire:  func(context.Context) (lockHandle, bool, error) { return lock, true, nil },
			Accounts: func(context.Context) ([]google.Account, error) { return nil, nil },
			Idle: func(ctx context.Context, _ google.Account, _ chan<- string) error {
				<-ctx.Done()
				return ctx.Err()
			},
			Pass: func(pctx context.Context, _ google.Config, why string) {
				mu.Lock()
				passes++
				n := passes
				if dl, ok := pctx.Deadline(); ok {
					deadlines = append(deadlines, time.Until(dl))
				} else {
					deadlines = append(deadlines, 0)
				}
				mu.Unlock()
				if n == 1 {
					// The wedge: a fetch that never returns. Only the pass bound
					// can end it.
					<-pctx.Done()
					mu.Lock()
					errs = append(errs, pctx.Err())
					mu.Unlock()
					return
				}
				if n >= 3 {
					cancel()
				}
			},
			Sink: newMWFakeSink(),
		})

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil: a cancelled pass is not an exit (watch.go:36, rule 3)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the loop never got past the wedged pass. Criterion 5: watchPass runs under " +
			"context.WithTimeout(ctx, MAIL_PASS_TIMEOUT); without it a stalled IMAP fetch stops the resident " +
			"loop forever and nothing notices — /funnel goes stale hours later, if anyone looks")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(deadlines) == 0 {
		t.Fatal("no pass ran at all")
	}
	if deadlines[0] == 0 {
		t.Error("the first pass received a context with NO deadline. Criterion 5: EVERY watchPass is bounded " +
			"by MAIL_PASS_TIMEOUT, not just the ones after the first")
	} else if deadlines[0] > passTimeout {
		t.Errorf("the pass context's remaining time was %s, longer than MAIL_PASS_TIMEOUT (%s)", deadlines[0], passTimeout)
	}
	if len(errs) == 0 || !errors.Is(errs[0], context.DeadlineExceeded) {
		t.Errorf("the wedged pass ended with %v, want context.DeadlineExceeded — the bound, not the loop's "+
			"own cancellation (criterion 5)", errs)
	}
	if passes < 3 {
		t.Errorf("only %d passes ran; after a cancelled pass the loop must carry on and the NEXT wake or "+
			"tick must run a fresh pass (criterion 5)", passes)
	}
}

// ---- criteria 7 and 9: health ignores IDLE --------------------------------------

// Criterion 7: with EVERY account's IDLE goroutine failing and backing off, and
// the sweep still completing, /healthz is 200. D7: "one mailbox in backoff must
// not restart the pod: a restart cannot fix invalid_grant, and restarting would
// thrash the three healthy mailboxes". Criterion 9's last clause — "never fails
// the loop or /healthz" — is the same fact from the other side.
//
// MUTATION: make /healthz fail when any account is in backoff -> criteria 7, 9.
func TestWatcherRun_HealthIsGreenWhileEveryMailboxIsInBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 2026-09-22 (implementation): the test ended after twelve backoff sleeps
	// SHARED across the four goroutines, and the scheduler let one account burn
	// all twelve before another had run its first cycle — that account then
	// had no error row, for a scheduling reason, not a loop one. The end is now
	// "every account has failed at least twice", judged per account.
	clock := newMWClock(0)
	lock := &mwFakeLock{}
	sink := newMWFakeSink()

	accounts := []google.Account{
		mwAccount(1, "salvador@handsonconnect.org"),
		mwAccount(2, "sspataro@gmail.com"),
		mwAccount(3, "developer@sspataro.com"),
		mwAccount(4, "sspataro57@msn.com"),
	}

	var mu sync.Mutex
	passes := 0
	failures := map[string]int{}
	w := newWatcher(
		watchConfig{Reconcile: 2 * time.Millisecond, IdleRefresh: time.Minute, PassTimeout: time.Second,
			StandbyRetry: 15 * time.Second, HealthAddr: ":0"},
		google.Config{},
		watchDeps{
			Now:      clock.Now,
			Sleep:    clock.Sleep,
			Acquire:  func(context.Context) (lockHandle, bool, error) { return lock, true, nil },
			Accounts: func(context.Context) ([]google.Account, error) { return accounts, nil },
			// Every mailbox refuses to authenticate, the way a revoked consent does.
			Idle: func(ctx context.Context, acct google.Account, _ chan<- string) error {
				mu.Lock()
				failures[acct.Email]++
				all := len(failures) == len(accounts)
				for _, n := range failures {
					if n < 2 {
						all = false
					}
				}
				mine := failures[acct.Email]
				mu.Unlock()
				if all {
					cancel()
				}
				if mine > 3 {
					<-ctx.Done() // this mailbox has proven its point; let the others catch up
					return ctx.Err()
				}
				return errors.New("no credential for this mailbox: refresh token rejected: invalid_grant")
			},
			Pass: func(context.Context, google.Config, string) {
				mu.Lock()
				passes++
				mu.Unlock()
			},
			Sink: sink,
		})

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil. Criterion 9: one mailbox's auth failure backs off; it never fails "+
				"the loop. With all four failing that is still four backoffs, not an exit", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return; the per-account failures wedged the loop (criterion 9)")
	}

	mu.Lock()
	ran := passes
	mu.Unlock()
	if ran == 0 {
		t.Fatal("POSITIVE CONTROL FAILED: the reconcile sweep never completed, so /healthz would be 503 for " +
			"the right reason and this test would prove nothing")
	}

	// The health verdict, at an instant one reconcile interval after the last
	// completed pass: still 200, with every account in backoff.
	h := newWatchHealthHandler(10*time.Minute,
		func() time.Time { return w.LastPassAt().Add(10 * time.Minute) },
		w.LastPassAt, lock)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d (%q) with all four mailboxes in backoff and the sweep completing, want "+
			"200. Criterion 7: the verdict is the pass clock and the lock, and NOTHING about IDLE — a "+
			"restart cannot fix invalid_grant and would thrash the healthy mailboxes",
			rec.Code, rec.Body.String())
	}

	// Criterion 9's visible half: each failing account got its own imap_idle
	// error row. Without it the only evidence a mailbox stopped listening is
	// stdout, which nothing queries.
	byAccount := map[int64]int{}
	for _, row := range sink.snapshot() {
		if row.phase == "imap_idle" && row.status == "error" {
			byAccount[row.accountID]++
		}
	}
	for _, acct := range accounts {
		if byAccount[acct.ID] == 0 {
			t.Errorf("no imap_idle error sync_runs row for %s. Criterion 9: a failing account records its "+
				"own row (watch.go:200-202) and the other mailboxes carry on", acct.Email)
		}
	}
}

// ---- criterion 9: failure isolation and jittered backoff -------------------------

// Criterion 9: one account failing to authenticate leaves the OTHERS IDLE-ing,
// writes its imap_idle error row, and backs off with jitter. The jitter is not
// decoration: every account starts at the same floor and doubles identically and
// the goroutines are started in one loop, so without a random spread a network
// blip reconnects all four in lockstep and hammers the server at each step.
//
// MUTATION: return an error instead of backing off on one account's auth failure
// -> criterion 9.
func TestWatchAccount_BacksOffWithJitterAndNeverFailsTheLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const failures = 8
	clock := newMWClock(failures)
	clock.cancel = cancel
	sink := newMWFakeSink()

	w := newWatcher(
		watchConfig{Reconcile: time.Hour, IdleRefresh: 25 * time.Minute, PassTimeout: time.Minute,
			StandbyRetry: 15 * time.Second, HealthAddr: ":0"},
		google.Config{},
		watchDeps{
			Now:   clock.Now,
			Sleep: clock.Sleep,
			Idle: func(context.Context, google.Account, chan<- string) error {
				return errors.New("imap login as sspataro57@msn.com failed: invalid_grant")
			},
			Sink: sink,
		})

	done := make(chan struct{})
	go func() {
		w.watchAccount(ctx, mwAccount(4, "sspataro57@msn.com"), make(chan string, 1))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("watchAccount never returned after its context was cancelled (criterion 4/9)")
	}

	waits := clock.sleeps()
	if len(waits) < 4 {
		t.Fatalf("only %d backoff waits were recorded (%v); the account loop must retry, not give up "+
			"(criterion 9: \"one mailbox's failure cannot stop the others\" — and it must not stop itself "+
			"either, or a transient outage silently retires a mailbox)", len(waits), waits)
	}
	// Exponential from the floor, with up to a full extra interval of jitter,
	// capped: watch.go:191-196 computes backoff + rand(0..backoff].
	base := backoffMin
	allAtTheFloor := true
	for i, d := range waits {
		lo, hi := base, 2*base
		if d < lo || d > hi {
			t.Errorf("backoff wait %d = %s, want it in [%s, %s] — exponential from backoffMin (%s), "+
				"doubling, capped at backoffMax (%s), plus up to one extra interval of jitter "+
				"(watch.go:191-196). Waits: %v", i, d, lo, hi, backoffMin, backoffMax, waits)
		}
		if d != base {
			allAtTheFloor = false
		}
		if base < backoffMax {
			base *= 2
			if base > backoffMax {
				base = backoffMax
			}
		}
	}
	if allAtTheFloor {
		t.Errorf("every backoff wait was exactly its base (%v): the jitter is gone. All four goroutines are "+
			"started in one loop from the same floor, so without a random spread a network blip reconnects "+
			"them in lockstep and hammers the server at every step (criterion 9)", waits)
	}

	rows := sink.snapshot()
	if len(rows) < 4 {
		t.Fatalf("%d sync_runs rows for %d failures, want one error row each (criterion 9)", len(rows), len(waits))
	}
	for i, row := range rows {
		if row.phase != "imap_idle" || row.status != "error" {
			t.Errorf("row %d = phase %q status %q, want imap_idle/error (criteria 9, 20)", i, row.phase, row.status)
		}
		if row.errMsg == "" {
			t.Errorf("row %d carries no error text; the row IS the operator's only evidence (D9)", i)
		}
	}
}

// ---- criterion 10: D9's recovery row ---------------------------------------------

// Criterion 10 / D9: failure then success writes EXACTLY ONE ok imap_idle row,
// at the transition; a run of successes with no preceding failure writes none;
// a run of failures writes one error row each.
//
// The shape matters because /funnel computes last_ok = max(finished_at) FILTER
// (status IN ('ok','partial')) per (account, phase): with only error rows ever
// written, imap_idle displays `never` FOREVER — the same cosmetic trap
// imap_refetch already has. And a row per wake would be ~230 rows/day per
// account for nothing.
//
// MUTATION: write an imap_idle ok row on every cycle -> criterion 10.
func TestWatchAccount_RecoveryRowOnlyAtTheTransition(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	authFail := errors.New("refresh token rejected: invalid_grant")
	// cycle outcomes, in order: fail, fail, recover, quiet success, fail, recover
	script := []error{authFail, authFail, nil, nil, authFail, nil}

	clock := newMWClock(0)
	sink := newMWFakeSink()

	var mu sync.Mutex
	i := 0
	w := newWatcher(
		watchConfig{Reconcile: time.Hour, IdleRefresh: 25 * time.Minute, PassTimeout: time.Minute,
			StandbyRetry: 15 * time.Second, HealthAddr: ":0"},
		google.Config{},
		watchDeps{
			Now:   clock.Now,
			Sleep: clock.Sleep,
			Idle: func(context.Context, google.Account, chan<- string) error {
				mu.Lock()
				defer mu.Unlock()
				if i >= len(script) {
					cancel()
					return nil
				}
				err := script[i]
				i++
				return err
			},
			Sink: sink,
		})

	done := make(chan struct{})
	go func() {
		w.watchAccount(ctx, mwAccount(2, "sspataro@gmail.com"), make(chan string, 1))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("watchAccount never returned (criterion 10)")
	}

	var got []string
	for _, row := range sink.snapshot() {
		if row.phase != "imap_idle" {
			t.Errorf("the account loop wrote a %q row; criterion 20 allows imap_idle only here", row.phase)
			continue
		}
		got = append(got, row.status)
	}
	want := []string{"error", "error", "ok", "error", "ok"}
	if len(got) != len(want) {
		t.Fatalf("imap_idle rows = %v, want %v for the cycle script fail,fail,SUCCESS,success,fail,SUCCESS. "+
			"D9 adds ONE row: on the first successful cycle AFTER a failure (the existing backoff reset "+
			"point, watch.go:214). An account that never fails writes NO imap_idle row at all — no phantom "+
			"phase on /funnel — and a run of successes writes one row, not one per wake (~230/day at a 25m "+
			"refresh)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("imap_idle row %d has status %q, want %q (sequence %v, want %v)", i, got[i], want[i], got, want)
		}
	}
}

// ---- criterion 14: the account set ------------------------------------------------

// Criterion 14 / D10: on each reconcile tick the account list is RE-READ, an
// account added between ticks gets an IDLE goroutine within one tick without a
// restart, and an account already watched does not get a second goroutine.
//
// Today runWatch lists accounts once (watch.go:88-95), so a mailbox onboarded
// later is ingested only by the sweep, silently at ten-minute latency — which is
// precisely the complaint this ticket answers.
//
// MUTATION: list accounts once at startup -> criterion 14.
func TestWatcherRun_RelistsAccountsOnEveryTick(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	lock := &mwFakeLock{}
	clock := newMWClock(0)

	first := mwAccount(1, "sspataro@gmail.com")
	second := mwAccount(2, "developer@sspataro.com")

	var mu sync.Mutex
	lists := 0
	starts := map[string]int{}
	watchedBoth := make(chan struct{})
	var once sync.Once

	w := newWatcher(
		watchConfig{Reconcile: 2 * time.Millisecond, IdleRefresh: time.Minute, PassTimeout: time.Second,
			StandbyRetry: 15 * time.Second, HealthAddr: ":0"},
		google.Config{},
		watchDeps{
			Now:     clock.Now,
			Sleep:   clock.Sleep,
			Acquire: func(context.Context) (lockHandle, bool, error) { return lock, true, nil },
			Accounts: func(context.Context) ([]google.Account, error) {
				mu.Lock()
				defer mu.Unlock()
				lists++
				if lists == 1 {
					return []google.Account{first}, nil
				}
				// The second mailbox is onboarded mid-run.
				return []google.Account{first, second}, nil
			},
			Idle: func(ctx context.Context, acct google.Account, _ chan<- string) error {
				mu.Lock()
				starts[acct.Email]++
				both := starts[first.Email] > 0 && starts[second.Email] > 0
				mu.Unlock()
				if both {
					once.Do(func() { close(watchedBoth) })
				}
				<-ctx.Done() // one long-lived IDLE per goroutine
				return ctx.Err()
			},
			Pass: func(context.Context, google.Config, string) {},
			Sink: newMWFakeSink(),
		})

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	select {
	case <-watchedBoth:
	case err := <-done:
		t.Fatalf("Run returned early (%v) before the second account was watched", err)
	case <-time.After(5 * time.Second):
		mu.Lock()
		t.Fatalf("after %d account listings the second mailbox still has no IDLE goroutine (starts: %v). "+
			"Criterion 14: the list is re-read on every reconcile tick, so a mailbox onboarded at 10:05 is "+
			"listening by 10:15 instead of at the next pod restart", lists, starts)
	}

	// Let a few more ticks pass, then assert nobody was started twice.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	mu.Lock()
	defer mu.Unlock()
	if lists < 2 {
		t.Errorf("the account list was read %d time(s). Criterion 14 / D10: \"watch the accounts table, "+
			"not a snapshot of it\"", lists)
	}
	for email, n := range starts {
		if n != 1 {
			t.Errorf("%s got %d IDLE goroutines, want exactly 1. A second goroutine per account doubles the "+
				"resident connection count (D12: 1 IDLE + 1 transient per account, at most 8 for four "+
				"mailboxes) and duplicates every wake (criterion 14)", email, n)
		}
	}
	got := w.Watching()
	sort.Strings(got)
	want := []string{second.Email, first.Email}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Errorf("Watching() = %v, want %v", got, want)
	}
}

// ---- criterion 9, the isolation half (added 2026-09-22 at review) -----------------

// Criterion 9: ONE account failing to authenticate leaves the OTHERS IDLE-ing.
// The failing mailbox writes its rows and backs off; the healthy one is
// entered once, holds its IDLE undisturbed, and gets no row at all.
func TestWatcherRun_OneFailingMailboxLeavesTheOthersIdling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clock := newMWClock(0)
	lock := &mwFakeLock{}
	sink := newMWFakeSink()
	healthy := mwAccount(2, "sspataro@gmail.com")
	broken := mwAccount(4, "sspataro57@msn.com")

	var mu sync.Mutex
	entered := map[string]int{}
	w := newWatcher(
		watchConfig{Reconcile: time.Hour, IdleRefresh: time.Minute, PassTimeout: time.Second,
			StandbyRetry: 15 * time.Second, HealthAddr: ":0"},
		google.Config{},
		watchDeps{
			Now:      clock.Now,
			Sleep:    clock.Sleep,
			Acquire:  func(context.Context) (lockHandle, bool, error) { return lock, true, nil },
			Accounts: func(context.Context) ([]google.Account, error) { return []google.Account{healthy, broken}, nil },
			Idle: func(ctx context.Context, acct google.Account, _ chan<- string) error {
				mu.Lock()
				entered[acct.Email]++
				n := entered[acct.Email]
				healthyIn := entered[healthy.Email] > 0
				mu.Unlock()
				if acct.Email == broken.Email {
					// End only once the healthy goroutine has been scheduled too —
					// the per-goroutine end condition, not a shared budget.
					if n >= 4 && healthyIn {
						cancel()
					}
					return errors.New("no credential for this mailbox: refresh token rejected: invalid_grant")
				}
				<-ctx.Done() // the healthy mailbox holds its IDLE until shutdown
				return ctx.Err()
			},
			Pass: func(context.Context, google.Config, string) {},
			Sink: sink,
		})

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil (criterion 9: one mailbox's failure never fails the loop)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}

	mu.Lock()
	defer mu.Unlock()
	if entered[healthy.Email] != 1 {
		t.Errorf("the healthy mailbox's IDLE was entered %d times, want exactly 1: the other mailbox's "+
			"backoff must not disturb it (criterion 9)", entered[healthy.Email])
	}
	if entered[broken.Email] < 4 {
		t.Errorf("POSITIVE CONTROL FAILED: the broken mailbox was entered only %d times", entered[broken.Email])
	}
	for _, row := range sink.snapshot() {
		if row.accountID == healthy.ID {
			t.Errorf("the healthy mailbox got a sync_runs row (%+v); only the failing one writes imap_idle rows", row)
		}
	}
}
