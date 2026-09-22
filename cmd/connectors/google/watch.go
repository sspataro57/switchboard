package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/pipeline"
	"github.com/sspataro57/switchboard/internal/store"
)

// Watch mode (criterion 18): the same one-shot pass, driven by IMAP IDLE instead
// of by a CronJob schedule.
//
// This makes the connector switchboard's first long-running workload, so the
// design question is what happens when things go wrong rather than when they go
// right. Three rules:
//
//  1. An IDLE notification is a WAKE-UP, never a payload. It triggers exactly the
//     bounded pass a CronJob would run. A missed notification costs latency until
//     the next reconcile; a duplicated one costs a no-op pass. Neither can lose
//     mail — the same discipline the orchestrator applies to Postgres NOTIFY.
//  2. A reconcile sweep runs regardless, on a timer. IDLE is an optimisation on
//     top of polling, not a replacement for it: a silently dead connection would
//     otherwise mean silence that looks exactly like an empty mailbox.
//  3. Nothing here exits on a transient failure. Connection loss backs off and
//     reconnects; the process ends only on a signal — or on losing the singleton
//     lock (SWT-73 D4), which is the one failure a restart genuinely fixes.
//
// SWT-73 deployed this loop as connector-google-watch and gave it an operational
// skin — the singleton lock, a bound on every pass, /healthz, a startup refusal
// and a per-tick re-read of the account set — without touching watchPass. The
// loop's seams (clock, sleep, lock, accounts, pass, idle, sink) are injected so
// the behaviour a resident process is judged on is testable without a database.

const (
	// defaultIdleRefresh sits under RFC 2177's 29-minute ceiling. Servers are
	// entitled to drop an IDLE that runs longer, and a dropped IDLE that nobody
	// re-issues is a connector that has stopped listening without saying so.
	defaultIdleRefresh = 25 * time.Minute
	// defaultReconcile is the safety net that makes rule 2 true.
	defaultReconcile = 10 * time.Minute
	// defaultPassTimeout bounds every pass (D6). It is the one-shot path's own
	// bound (main.go's ten-minute context), proven on these mailboxes every ten
	// minutes for two months. A wedged fetch is cancelled, logged and counted;
	// the cursor advances only after a complete folder pass, so a truncated
	// pass re-fetches rather than skips.
	defaultPassTimeout = 10 * time.Minute
	// defaultHealthAddr: hooksd owns :8090, orchestratord :8091, the Slack
	// watcher :8093.
	defaultHealthAddr = ":8092"
	// standbyRetry is how often a replica that lost the singleton race re-tries
	// the lock (D4). Not an env knob: a node drain leaves the old pod
	// terminating while the new one starts, and 15 s bounds the takeover.
	standbyRetry = 15 * time.Second
	// backoff bounds. Capped so a long outage still retries promptly once the
	// server returns, rather than sleeping for an hour.
	backoffMin = 5 * time.Second
	backoffMax = 5 * time.Minute
)

func envDuration(name string, def time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		// Same defensive shape as every other duration knob in the repo: a typo
		// must not silently produce a zero interval, which would spin.
		return def
	}
	return d
}

// watchConfig is the resident loop's four knobs plus the standby cadence.
type watchConfig struct {
	Reconcile    time.Duration // MAIL_RECONCILE_INTERVAL, default 10m
	IdleRefresh  time.Duration // MAIL_IDLE_REFRESH, default 25m
	PassTimeout  time.Duration // MAIL_PASS_TIMEOUT, default 10m (D6)
	StandbyRetry time.Duration // D4: 15s, not an env knob
	HealthAddr   string        // MAIL_WATCH_HEALTH_ADDR, default :8092 (D7)
}

func watchConfigFromEnv() watchConfig {
	addr := os.Getenv("MAIL_WATCH_HEALTH_ADDR")
	if addr == "" {
		addr = defaultHealthAddr
	}
	return watchConfig{
		Reconcile:    envDuration("MAIL_RECONCILE_INTERVAL", defaultReconcile),
		IdleRefresh:  envDuration("MAIL_IDLE_REFRESH", defaultIdleRefresh),
		PassTimeout:  envDuration("MAIL_PASS_TIMEOUT", defaultPassTimeout),
		StandbyRetry: standbyRetry,
		HealthAddr:   addr,
	}
}

// checkCaptureConfig is D8's refusal: a LIVE capture pass whose horizon is
// under capture.MinLiveRulesHorizon fails every pass. A CronJob shows that as
// one red run an operator sees; a resident loop shows it as a pod that logs an
// error on every wake and looks alive while capturing nothing. Shadow is not
// refused — it is a legitimate configuration and RulesMode's fail-safe must
// not be inverted; the startup line prints it instead.
func checkCaptureConfig(cfg capture.RulesConfig) error {
	if cfg.Mode == capture.RulesModeLive && cfg.Horizon < capture.MinLiveRulesHorizon {
		return fmt.Errorf("CAPTURE_RULES_MODE=live with a %s horizon is below capture.MinLiveRulesHorizon (%s); "+
			"every pass would be a silent no-op — set CAPTURE_RULES_SINCE to at least %s",
			cfg.Horizon, capture.MinLiveRulesHorizon, capture.MinLiveRulesHorizon)
	}
	return nil
}

// startupLine is the one line (criterion 12) that lets an operator explain the
// watcher's behaviour from the first ten lines of kubectl logs.
func startupLine(cfg watchConfig, rules capture.RulesConfig, accounts int) string {
	horizon := "unbounded"
	if rules.Horizon > 0 {
		horizon = shortDur(rules.Horizon)
	}
	return fmt.Sprintf("watch: mode=%s horizon=%s reconcile=%s idle_refresh=%s pass_timeout=%s accounts=%d health=%s",
		rules.Mode, horizon, shortDur(cfg.Reconcile), shortDur(cfg.IdleRefresh), shortDur(cfg.PassTimeout),
		accounts, cfg.HealthAddr)
}

// shortDur prints a duration in its largest whole unit (720h, 10m, 90s) — the
// runbook's spelling, not Go's 720h0m0s.
func shortDur(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%dm", int64(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", int64(d/time.Second))
	}
}

// lockHandle is the singleton lock as the loop sees it (*watchLock in
// production).
type lockHandle interface {
	Alive(ctx context.Context) error
	Release()
}

// accountSink is the per-account advisory lock and the sync_runs rows
// (*google.PGSink in production).
type accountSink interface {
	LockAccount(ctx context.Context, accountID int64) (release func(), ok bool, err error)
	StartRun(ctx context.Context, accountID int64, phase string) (int64, error)
	FinishRun(ctx context.Context, runID int64, status string, stats google.Stats, errMsg string) error
}

// watchDeps are the loop's seams. Production wires them in runWatch; the tests
// wire fakes.
type watchDeps struct {
	Now      func() time.Time
	Sleep    func(ctx context.Context, d time.Duration) error
	Acquire  func(ctx context.Context) (lockHandle, bool, error)                      // tryMailWatchLock
	Accounts func(ctx context.Context) ([]google.Account, error)                      // google.ListIMAPAccounts
	Pass     func(ctx context.Context, cfg google.Config, why string)                 // watchPass
	Idle     func(ctx context.Context, acct google.Account, wake chan<- string) error // idleOnce
	Sink     accountSink
}

// watcher is the resident loop's state.
type watcher struct {
	cfg  watchConfig
	mail google.Config
	deps watchDeps

	lock lockHolder // standbyLock until the singleton is taken, the real handle after

	mu       sync.Mutex
	lastPass time.Time
	watching map[string]bool // emails with a live IDLE goroutine
	timedOut int
}

func newWatcher(cfg watchConfig, mail google.Config, deps watchDeps) *watcher {
	w := &watcher{cfg: cfg, mail: mail, deps: deps, watching: map[string]bool{}}
	w.lock.set(standbyLock{})
	return w
}

// lockHolder publishes the current lock handle to /healthz atomically: a
// standbyLock until the singleton is taken, the real handle after. /healthz
// reads it concurrently for the whole standby window.
type lockHolder struct{ v atomic.Value }

func (h *lockHolder) set(c aliveChecker) { h.v.Store(&c) }

func (h *lockHolder) Alive(ctx context.Context) error {
	c, _ := h.v.Load().(*aliveChecker)
	if c == nil {
		return errStandby
	}
	return (*c).Alive(ctx)
}

// Health is /healthz's lock input: errStandby until the lock is held.
func (w *watcher) Health() aliveChecker { return &w.lock }

// LastPassAt is the last COMPLETED pass — /healthz's other input. A pass the
// bound cancelled does not count: liveness means the sweep gets through.
func (w *watcher) LastPassAt() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastPass
}

// Watching lists the mailboxes that have been given an IDLE goroutine.
func (w *watcher) Watching() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(w.watching))
	for email := range w.watching {
		out = append(out, email)
	}
	return out
}

// Run is the loop. It returns nil when ctx ends and a non-nil error only when
// the singleton lock is LOST — the one condition a restart fixes.
func (w *watcher) Run(ctx context.Context) error {
	// A child context so the account goroutines stop when Run returns for any
	// reason, not only when the caller's context ends.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The singleton, BEFORE the initial pass (criterion 2). Held elsewhere is
	// standby: log once, run nothing, retry every StandbyRetry, take over when
	// the holder goes away — without restarting the process.
	var lock lockHandle
	standbyLogged := false
	for lock == nil {
		l, ok, err := w.deps.Acquire(ctx)
		if err != nil {
			return fmt.Errorf("mail watch lock: %w", err)
		}
		if ok {
			lock = l
			break
		}
		if !standbyLogged {
			fmt.Printf("watch: another mail watcher holds the lock; standing by\n")
			standbyLogged = true
		}
		if err := w.deps.Sleep(ctx, w.cfg.StandbyRetry); err != nil {
			return nil
		}
	}
	defer lock.Release()
	w.lock.set(lock)
	fmt.Printf("watch: lock held; running\n")

	// One pass immediately: a process that has just started should not wait a
	// full interval before doing anything, and this is also the fastest way to
	// surface a credential or connectivity problem at deploy time.
	w.pass(ctx, w.mail, "initial")

	// A buffered wake channel: idleOnce's send is non-blocking and one queued
	// wake covers any number of notifications for the same account.
	wake := make(chan string, 64)
	w.refreshAccounts(ctx, wake)

	ticker := time.NewTicker(w.cfg.Reconcile)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Printf("watch: signal received, shutting down\n")
			return nil
		case email := <-wake:
			// Scoped to the account that woke us; the sweep below covers the rest.
			passCfg := w.mail
			passCfg.AccountEmail = email
			w.pass(ctx, passCfg, "wake "+email)
		case <-ticker.C:
			// Criterion 3: the lock connection is checked on every tick. A CNPG
			// switchover kills it silently, the session lock is gone with it, and
			// a second watcher could take the key while this one carried on
			// believing it is the singleton.
			if err := lock.Alive(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("mail watch lock lost: %w", err)
			}
			// D10: watch the accounts table, not a snapshot of it.
			w.refreshAccounts(ctx, wake)
			w.pass(ctx, w.mail, "reconcile")
		}
	}
}

// pass runs one watchPass under the D6 bound and stamps it as completed only
// when the bound did not fire. Nothing starts after cancellation (criterion 4).
func (w *watcher) pass(ctx context.Context, mail google.Config, why string) {
	if ctx.Err() != nil {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, w.cfg.PassTimeout)
	defer cancel()
	w.deps.Pass(pctx, mail, why)
	if ctx.Err() != nil {
		return // shutting down mid-pass; not a completed pass, not an error
	}
	if errors.Is(pctx.Err(), context.DeadlineExceeded) {
		w.mu.Lock()
		w.timedOut++
		n := w.timedOut
		w.mu.Unlock()
		fmt.Printf("watch: pass (%s) exceeded MAIL_PASS_TIMEOUT=%s and was cancelled (timed_out=%d)\n",
			why, w.cfg.PassTimeout, n)
		return
	}
	w.mu.Lock()
	w.lastPass = w.deps.Now()
	w.mu.Unlock()
}

// refreshAccounts re-reads the account set and starts an IDLE goroutine for
// each mailbox that does not have one. Existing goroutines are never stopped
// except by ctx: an account whose row disappears fails, backs off and logs,
// which is simpler than a supervision protocol for a thing that does not
// happen quietly. A listing failure is logged, never fatal (rule 3).
func (w *watcher) refreshAccounts(ctx context.Context, wake chan<- string) {
	accounts, err := w.deps.Accounts(ctx)
	if err != nil {
		fmt.Printf("watch: list accounts failed: %v\n", err)
		return
	}
	for _, acct := range accounts {
		w.mu.Lock()
		already := w.watching[acct.Email]
		if !already {
			w.watching[acct.Email] = true
		}
		w.mu.Unlock()
		if already {
			continue
		}
		go w.watchAccount(ctx, acct, wake)
	}
}

// watchAccount holds one IDLE connection on INBOX and signals on every change.
//
// INBOX only. Sent gets the reconcile sweep instead: it halves the connection
// count and the failure surface, and Sent-folder latency only delays delivery
// confirmation, which no rule waits on.
func (w *watcher) watchAccount(ctx context.Context, acct google.Account, wake chan<- string) {
	backoff := backoffMin
	failing := false
	for {
		if ctx.Err() != nil {
			return
		}
		err := w.deps.Idle(ctx, acct, wake)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			// Real jitter, not just exponential growth. Every account starts at
			// the same floor and doubles identically, and the goroutines are
			// started in one loop — so a network blip would reconnect all of them
			// in lockstep and hammer the server at each step. The random spread
			// is up to a full extra interval, which is what breaks the alignment.
			wait := backoff + time.Duration(rand.Int63n(int64(backoff)+1))
			fmt.Printf("watch: idle for %s failed: %v (retry in %s)\n", acct.Email, err, wait.Round(time.Second))
			// A failure an operator can see: without this the only evidence of a
			// mailbox that stopped listening is stdout, which nothing queries.
			if runID, e := w.deps.Sink.StartRun(ctx, acct.ID, "imap_idle"); e == nil {
				_ = w.deps.Sink.FinishRun(ctx, runID, "error", google.Stats{}, err.Error())
			}
			failing = true
			if err := w.deps.Sleep(ctx, wait); err != nil {
				return
			}
			backoff *= 2
			if backoff > backoffMax {
				backoff = backoffMax
			}
			continue
		}
		backoff = backoffMin
		if failing {
			// D9: ONE recovery row at the transition, so /funnel's last_ok for
			// imap_idle can read something other than `never`. No row per wake
			// and none for an account that never failed.
			failing = false
			if runID, e := w.deps.Sink.StartRun(ctx, acct.ID, "imap_idle"); e == nil {
				_ = w.deps.Sink.FinishRun(ctx, runID, "ok", google.Stats{}, "")
			}
		}
	}
}

// idleConn is one IMAP connection as idleOnce sees it; *google.IMAPClientSource
// satisfies it in production.
type idleConn interface {
	google.MailSource
	Close() error
}

// openIdleConn opens EXACTLY ONE connection for the account, minting the
// credential: google.OpenIMAPSource in production.
type openIdleConn func(ctx context.Context, acct google.Account) (idleConn, error)

type idleDeps struct {
	Sink        accountSink
	Open        openIdleConn
	Sleep       func(ctx context.Context, d time.Duration) error
	IdleRefresh time.Duration
}

// idleOnce opens one IDLE and returns when it fires, refreshes, or fails.
func idleOnce(ctx context.Context, deps idleDeps, acct google.Account, wake chan<- string) error {
	// Resolve the credential under the SAME per-account advisory lock the ingest
	// pass takes, then release it before going idle.
	//
	// An OAuth mailbox makes this necessary. Microsoft rotates the refresh token
	// every time it is redeemed, and in watch mode two paths in THIS process
	// redeem it: an IDLE wake re-enters idleOnce while the wake it published
	// sends watchPass into runIMAPIngest. Unsynchronised, both read the same
	// stored token and both redeem it, so the loser's redemption comes back
	// invalid_grant — and D9 dutifully records a per-account error row that
	// looks exactly like a genuinely revoked consent. An alarm that fires on
	// ordinary traffic is worse than no alarm. Under SWT-73 D3 the 2-hourly
	// CronJob is a second PROCESS on the same token, and pg_advisory_lock is
	// database-wide, so the same lock serializes that too.
	//
	// The lock is NOT held across the IDLE itself: that would block the ingest
	// pass for the whole refresh interval. It covers only the mint.
	release, ok, err := deps.Sink.LockAccount(ctx, acct.ID)
	if err != nil {
		return err
	}
	if !ok {
		// An ingest pass holds it and is about to read this mailbox anyway, so
		// skipping this cycle is correct — but WAIT first. watchAccount treats a
		// nil return as success and re-enters immediately, so returning straight
		// away spins on pg_try_advisory_lock for the whole pass: thousands of
		// round trips a second, each acquiring a pooled connection, contending
		// with the very pass being waited on and lengthening it.
		//
		// A wait and not an error: an error here would write an imap_idle error
		// run every time an ingest pass overlaps, which is the cry-wolf failure
		// this locking was added to avoid.
		_ = deps.Sleep(ctx, backoffMin)
		return nil
	}
	src, err := deps.Open(ctx, acct)
	// Safe to release here ONLY because this cycle makes exactly one connection:
	// src.Idle is the single connect, and it consumes the token minted above. A
	// second connect after this point (say, fetching the new message here rather
	// than through watchPass) would mint outside the lock and put the rotation
	// race back, intermittently. idle_test.go pins it (SWT-73 criterion 8).
	release()
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	// Bounded by the refresh interval: IDLE is re-issued rather than held past
	// the point a server would drop it silently.
	idleCtx, cancel := context.WithTimeout(ctx, deps.IdleRefresh)
	defer cancel()

	ch, err := src.Idle(idleCtx, google.InboxFolder)
	if err != nil {
		return err
	}
	select {
	case <-idleCtx.Done():
		// Refresh interval elapsed with no news; reopen. Not an error.
		return nil
	case _, ok := <-ch:
		if !ok {
			return nil
		}
		select {
		case wake <- acct.Email:
		default: // a wake-up is already queued; one pass covers both
		}
		return nil
	}
}

// sleepCtx is the production Sleep seam.
func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// runWatch is the long-lived loop. It returns nil on a signal and a non-nil
// error when the singleton lock is lost or the configuration is refused; main
// turns that into the exit code.
func runWatch(pool *pgxpool.Pool, cfg google.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	key := os.Getenv("OPS_TOKEN_KEY")
	if key == "" {
		return fmt.Errorf("OPS_TOKEN_KEY is not set (required to decrypt app passwords)")
	}
	wcfg := watchConfigFromEnv()
	// D8: resolve the capture config ONCE, through the reader the one-shot pass
	// shares, and refuse a configuration that would make every pass a no-op.
	rules := captureRulesConfig()
	if err := checkCaptureConfig(rules); err != nil {
		return err
	}
	accounts, err := google.ListIMAPAccounts(ctx, pool, cfg.AccountEmail)
	if err != nil {
		return fmt.Errorf("list IMAP accounts: %w", err)
	}
	fmt.Println(startupLine(wcfg, rules, len(accounts)))

	sink := google.NewPGSink(pool)
	// Built once, not per pass: the registry and policy matrix are stateless and
	// a resident loop must not rebuild them every ten minutes.
	ex := newExecutor(pool)

	deps := watchDeps{
		Now:   time.Now,
		Sleep: sleepCtx,
		Acquire: func(ctx context.Context) (lockHandle, bool, error) {
			l, ok, err := tryMailWatchLock(ctx, pool)
			if err != nil || !ok {
				return nil, false, err // a nil *watchLock must not become a non-nil interface
			}
			return l, true, nil
		},
		Accounts: func(ctx context.Context) ([]google.Account, error) {
			return google.ListIMAPAccounts(ctx, pool, cfg.AccountEmail)
		},
		Pass: func(pctx context.Context, mail google.Config, why string) {
			watchPass(pctx, pool, sink, ex, mail, why)
		},
		Idle: func(ctx context.Context, acct google.Account, wake chan<- string) error {
			return idleOnce(ctx, idleDeps{
				Sink: sink,
				Open: func(ctx context.Context, acct google.Account) (idleConn, error) {
					src, err := google.OpenIMAPSource(ctx, pool, acct, key)
					if err != nil {
						return nil, err
					}
					return src, nil
				},
				Sleep:       sleepCtx,
				IdleRefresh: wcfg.IdleRefresh,
			}, acct, wake)
		},
		Sink: sink,
	}
	w := newWatcher(wcfg, cfg, deps)

	// /healthz (D7): judged on the pass clock and the lock, nothing else.
	health := &http.Server{Addr: wcfg.HealthAddr,
		Handler: newWatchHealthHandler(wcfg.Reconcile, time.Now, w.LastPassAt, w.Health())}
	go func() {
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Printf("watch: healthz server: %v\n", err)
		}
	}()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = health.Shutdown(sctx)
	}()

	return w.Run(ctx)
}

// watchPass is one complete cycle: ingest, normalize, then the two capture passes.
//
// Normalize is NOT optional here, which is the whole reason this helper exists.
// Ingest writes raw_source_items and nothing else; every consequence that makes
// this connector useful happens in normalize — normalized_messages is what
// mail_search and the draft worker read, and delivery loop closure (invariant 5)
// runs inside upsertMessage. A watch loop that only ingested would fill the raw
// table while confirming zero deliveries and leaving every mail tool empty, and
// would look healthy the entire time.
//
// Errors are logged and the loop continues: a resident process must not exit on
// a transient failure, and each phase already records its own sync_runs row.
func watchPass(ctx context.Context, pool *pgxpool.Pool, sink *google.PGSink, ex *executor.Executor,
	cfg google.Config, why string) {

	if _, err := runIMAPIngest(ctx, pool, sink, cfg); err != nil {
		fmt.Printf("watch: ingest (%s) failed: %v\n", why, err)
		// Normalize anyway: a partial ingest still wrote raw rows, and leaving
		// them unnormalized would hide messages that did land.
	}
	stats, err := google.Normalize(ctx, sink, cfg)
	if err != nil {
		fmt.Printf("watch: normalize (%s) failed: %v\n", why, err)
		return
	}
	fmt.Printf("watch: %s normalized=%d dedup_skipped=%d\n", why, stats.Normalized, stats.DedupSkipped)

	// Same ordering as the one-shot path: after Normalize, so this pass's own
	// delivery confirmations are already stamped and a message switchboard sent
	// is never mislabelled as sent by hand (SWT-16).
	observed, err := capture.ObserveOutbound(ctx, pool, capture.Gmail)
	if err != nil {
		fmt.Printf("watch: observe outbound (%s) failed: %v\n", why, err)
		// Not a return: the two capture passes are independent, and a failure to
		// log an externally-sent reply must not also stop project assignment.
	} else if observed > 0 {
		fmt.Printf("watch: outbound_observed=%d\n", observed)
	}

	// Deterministic project assignment (SWT-17), same position as the one-shot
	// path: after Normalize, and channel-agnostic, so a resident mail watcher
	// also drains what the suspended connectors ingested. The advisory lock
	// inside EvaluateRules keeps it from racing the CronJobs.
	rulesCfg := captureRulesConfig()
	rules, err := capture.EvaluateRules(ctx, pool, ex, rulesCfg)
	if err != nil {
		// Logged, never fatal: a resident process must not exit on a transient
		// failure (rule 3 above).
		fmt.Printf("watch: capture rules (%s) failed: %v\n", why, err)
	}
	// Printed unconditionally, zeros included, even after an error: a pass that
	// matched nothing and a pass that never ran must not look the same.
	printCaptureRules(rulesCfg, rules)
	// SWT-40 E2: an IDLE wake that decided mail wakes the pipeline stages too.
	// Empty passes (the common case) publish nothing.
	pipeline.AnnounceCaptured(ctx, os.Getenv("MQTT_BROKER"), "google", rules)
}

// newWatchPool builds the pool watch mode owns (the one-shot path's pool is
// scoped to a 10-minute context that must not bound a long-lived process).
func newWatchPool(ctx context.Context) (*pgxpool.Pool, error) {
	return store.NewPool(ctx)
}
