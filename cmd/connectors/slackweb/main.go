// slackweb is the Slack Web connector, in two shapes:
//
//	slackweb [--normalize-only] [--all]   one-shot: the CronJob's full export pass
//	slackweb --watch                       resident (SWT-75): a targeted pass of the
//	                                       slack_watch conversations every minute,
//	                                       the full export every half hour
//
// authenticated browser export -> raw observations -> normalized messages.
//
// DATABASE_URL            ops db, required
// SLACK_WEB_BRIDGE_URL    the HTTP bridge on the Mac mini (required for --watch)
// SLACK_WEB_BRIDGE_TOKEN  its bearer token
// SLACK_WEB_BRIDGE_SCRIPT absolute path to the compiled TypeScript bridge (one-shot, local only)
// SLACK_WEB_NODE          Node.js executable (default: node)
// CAPTURE_RULES_MODE      shadow (default) | live
// CAPTURE_RULES_SINCE     Go duration bounding the capture-rules pass
// SLACK_WATCH_INTERVAL    --watch: targeted cadence, default 60s; 0 disables targeted passes
// SLACK_ROTATION_INTERVAL --watch: full-export cadence, default 30m
// SLACK_WATCH_BUDGET_MS   --watch: the leaf's budget for a targeted pass, default 150000
// SLACK_BRIDGE_GRACE      --watch: added to the budget for the Go context, default 120s
// SLACK_WATCH_HEALTH_ADDR --watch: GET /healthz, default :8093
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/connector/slackweb"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/pipeline"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

// The two connector strings. The watcher announces as its OWN connector so its
// MQTT client id (switchboard-capture-slackweb-watch) can never collide with
// the CronJob's: a shared id kicks the other holder off the broker, and the
// CronJob still runs every two hours (SWT-75 D4, D9).
const (
	connectorOneShot = "slackweb"
	connectorWatch   = "slackweb-watch"
	// The one-shot's stand-down line, quoted in the runbook and the hand-off.
	standDownLine = "slack watch is live; skipping this pass"
)

func main() {
	normalizeOnly := flag.Bool("normalize-only", false, "skip browser export; normalize from raw alone")
	all := flag.Bool("all", false, "normalize every Slack raw row, not only pending")
	watch := flag.Bool("watch", false, "stay resident: targeted passes of the slack_watch list plus a periodic full export")
	flag.Parse()

	if *watch {
		if err := watchMain(); err != nil {
			fmt.Fprintln(os.Stderr, "slackweb:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(*normalizeOnly, *all); err != nil {
		fmt.Fprintln(os.Stderr, "slackweb:", err)
		os.Exit(1)
	}
}

// run is the one-shot CronJob pass: today's full sequence, behind the D4
// stand-down check.
func run(normalizeOnly, all bool) error {
	// 30m, not 15m: a full bridge export legitimately runs ~12m now that the
	// bridge recycles the Slack tab every 2 reloads (2026-09-09), and a client
	// deadline that fires mid-export also terminates the bridge process.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	// SWT-75 D4: the CronJob is a net, never a co-worker on the mini's one
	// browser. While the resident watcher holds lockkeys.SlackWatch this pass
	// opens no bridge connection and writes no run row; it exits 0.
	live, err := standDown(ctx, pool)
	if err != nil {
		return fmt.Errorf("slack watch lock probe: %w", err)
	}
	if live {
		fmt.Println(standDownLine)
		return nil
	}

	sink := slackweb.NewSink(pool)
	var source slackweb.Source
	if !normalizeOnly {
		if err := sink.CheckPartialStatus(ctx); err != nil {
			return err
		}
		source, err = newSource()
		if err != nil {
			return err
		}
	}
	ex := newExecutor(pool)
	return rotationPass(ctx, pool, sink, source, ex, all, connectorOneShot)
}

// rotationPass is the full export pass — the one-shot's sequence, in the
// one-shot's order, which is behaviour: ReconcileUnconfirmed runs AFTER
// Normalize so this pass's own confirmations are already stamped, and
// ObserveOutbound after the reconciler so an in-flight send has had its chance.
// source == nil means --normalize-only (no export).
func rotationPass(ctx context.Context, pool *pgxpool.Pool, sink *slackweb.PGSink, source slackweb.Source,
	ex *executor.Executor, all bool, connector string) error {
	if source != nil {
		stats, err := slackweb.Ingest(ctx, source, sink)
		printStats("ingest", stats)
		if err != nil {
			return fmt.Errorf("ingest: %w", err)
		}
	}

	stats, err := slackweb.Normalize(ctx, sink, slackweb.Config{All: all})
	printStats("normalize", stats)
	if err != nil {
		return fmt.Errorf("normalize: %w", err)
	}

	// After normalize, so this pass's own confirmations are already stamped and
	// a delivery confirmed moments ago is never flagged.
	flagged, err := slackweb.ReconcileUnconfirmed(ctx, sink, slackweb.UnconfirmedFlagPasses())
	if err != nil {
		return fmt.Errorf("reconcile unconfirmed: %w", err)
	}
	if flagged > 0 {
		fmt.Printf("reconcile: {\"flagged_unconfirmed\":%d}\n", flagged)
	}

	// Slack messages Salvador sent by hand (phone, desktop app) get logged on the
	// tasks they correspond to (SWT-16). Runs after the reconciler so an in-flight
	// switchboard send has already had its chance to be confirmed or flagged.
	observed, err := capture.ObserveOutbound(ctx, pool, capture.Slack)
	if err != nil {
		return fmt.Errorf("observe outbound: %w", err)
	}
	// Printed unconditionally: a silent pass and a pass that did not run look
	// identical in the logs, and this one is expected to find nothing most times.
	fmt.Printf("capture: {\"outbound_observed\":%d}\n", observed)

	// Deterministic project assignment (SWT-17), through the executor
	// (invariant 3), then the pipeline wake once the decisions are committed.
	// The audit actor names the process (connector); the wake is announced as
	// the one-shot's identity in both cases — the rotation IS the one-shot's
	// pass, and the CronJob stands down while the watcher lives, so the two
	// never hold the broker at once. Only the targeted pass has its own id.
	rulesCfg := rulesConfigFor(connector)
	rules, err := capture.EvaluateRules(ctx, pool, ex, rulesCfg)
	printRules(rulesCfg, rules)
	if err != nil {
		return fmt.Errorf("capture rules: %w", err)
	}
	pipeline.AnnounceCaptured(ctx, os.Getenv("MQTT_BROKER"), "slackweb", rules)
	return nil
}

// targetedPass is the per-minute pass (SWT-75 D3): the watch list re-read
// from the table on EVERY pass, one targeted export, then normalize, capture
// and the announce — and nothing else. No reconciler (it would count
// per-minute passes as observation passes and flag a healthy send in three
// minutes) and no outbound observation (it scans the whole corpus; this pass
// read two conversations).
func targetedPass(ctx context.Context, pool *pgxpool.Pool, sink *slackweb.PGSink, source slackweb.Source,
	ex *executor.Executor, cfg slackweb.WatchConfig) error {
	rows, err := sink.WatchTargets(ctx)
	if err != nil {
		return fmt.Errorf("load watch list: %w", err)
	}
	stats, err := slackweb.RunTargeted(ctx, source, sink, rows, cfg)
	if errors.Is(err, slackweb.ErrNoTargets) {
		slog.Info("slack watch: no enabled targets; nothing to sweep")
		return nil
	}
	printStats("ingest_targeted", stats)
	if err != nil {
		return err
	}
	// Only when something arrived: normalize and capture cost a few queries,
	// and the rotation covers whatever a quiet minute did not.
	if stats.RawInserted == 0 && stats.RawUpdated == 0 {
		return nil
	}
	nstats, err := slackweb.Normalize(ctx, sink, slackweb.Config{})
	printStats("normalize", nstats)
	if err != nil {
		return fmt.Errorf("normalize: %w", err)
	}
	// Capture through the executor, announced as the WATCH connector (D9).
	rulesCfg := rulesConfigFor(connectorWatch)
	rules, err := capture.EvaluateRules(ctx, pool, ex, rulesCfg)
	printRules(rulesCfg, rules)
	if err != nil {
		return fmt.Errorf("capture rules: %w", err)
	}
	pipeline.AnnounceCaptured(ctx, os.Getenv("MQTT_BROKER"), "slackweb-watch", rules)
	return nil
}

// rulesConfigFor is the capture config both pass kinds share. Mode and horizon
// come from capture's own readers, not from a local os.Getenv: the "720 is not
// a Go duration" defence has ONE spelling. The actor names the connector so the
// audit trail says which process won the lock.
func rulesConfigFor(connector string) capture.RulesConfig {
	mode := capture.RulesMode()
	return capture.RulesConfig{Mode: mode, Horizon: capture.RulesHorizon(mode), Actor: "capture:" + connector}
}

// printRules prints the counter line unconditionally, zeros included, so
// "matched nothing" and "never ran" are different lines in a log.
func printRules(cfg capture.RulesConfig, rules capture.RulesStats) {
	fmt.Printf("capture_rules: {\"mode\":%q,\"considered\":%d,\"matched\":%d,\"unmatched\":%d,"+
		"\"tasks_created\":%d,\"appended\":%d,\"reopened\":%d,\"revived\":%d,\"surfaced_created\":%d,\"surfaced_open\":%d,\"deferred\":%d,\"blind\":%d,\"resurfaced\":%d,"+
		"\"pr_author_skipped\":%d,\"pr_closed\":%d,\"activity\":%d,\"comm_tasks\":%d}\n",
		cfg.Mode, rules.Considered, rules.Matched, rules.Unmatched, rules.TasksCreated, rules.Appended, rules.Reopened, rules.Revived, rules.SurfacedCreated, rules.SurfacedOpen, rules.Deferred, rules.Blind, rules.Resurfaced,
		rules.PRAuthorSkipped, rules.PRClosed, rules.Activity, rules.CommTasks)
}

// newExecutor builds the one path to tasks/external_refs/task_events
// (invariant 3). No tools.Set* seam is armed: every sender stays nil, so this
// process cannot send (invariant 4).
func newExecutor(pool *pgxpool.Pool) *executor.Executor {
	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	return executor.New(reg, checker, audit.NewPGStore(pool))
}

// ---- --watch: the resident loop (SWT-75) --------------------------------------

// watchMain owns its own pool and context: the one-shot path bounds itself to
// thirty minutes, which must never bound a resident process.
func watchMain() error {
	cfg := slackweb.WatchConfigFromEnv()
	if cfg.HealthAddr == "" {
		cfg.HealthAddr = ":8093" // D9: hooksd :8090, orchestratord :8091, the mail watcher :8092
	}
	source, err := watchSource()
	if err != nil {
		return err
	}
	mode := capture.RulesMode()
	horizon := capture.RulesHorizon(mode)
	if err := checkCaptureConfig(mode, horizon); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	sink := slackweb.NewSink(pool)
	if err := sink.CheckPartialStatus(ctx); err != nil {
		return err
	}
	targets, err := sink.WatchTargets(ctx)
	if err != nil {
		return err // a missing 0041 fails here, by name, not as an empty list forever
	}
	fmt.Println(startupLine(cfg, len(targets), mode, horizon))

	// The singleton: held elsewhere is STANDBY (log once, /healthz 503
	// "standby", retry every 15 s, run nothing); a lost connection later is a
	// restart (D9). The lock handle is published to the health goroutine
	// through an atomic cell, never a bare interface write: /healthz reads it
	// concurrently for the whole standby window.
	lockCell := &lockHolder{}
	lockCell.set(standbyLock{})
	ex := newExecutor(pool)
	var lock *watchLock
	// Each pass is bounded: a targeted pass by the leaf's budget plus the grace
	// (D5 — the Go deadline may only fire after the leaf has given up), a
	// rotation by the one-shot's thirty minutes. Built BEFORE the watcher so
	// the watcher is constructed exactly once (its mutex is never copied).
	deps := slackweb.WatchDeps{
		Pass: func(pctx context.Context, kind slackweb.PassKind) error {
			// A lost lock connection is a restart, never an unlocked loop.
			if err := lock.Alive(pctx); err != nil {
				slog.Error("slack watch: lock connection lost; exiting for a restart", "err", err)
				os.Exit(1)
			}
			switch kind {
			case slackweb.PassRotation:
				rctx, cancel := context.WithTimeout(pctx, 30*time.Minute)
				defer cancel()
				return rotationPass(rctx, pool, sink, source, ex, false, connectorWatch)
			default:
				tctx, cancel := context.WithTimeout(pctx, cfg.PassTimeout())
				defer cancel()
				return targetedPass(tctx, pool, sink, source, ex, cfg)
			}
		},
	}
	watcher := slackweb.NewWatcher(cfg, deps)
	health := &http.Server{Addr: cfg.HealthAddr, Handler: newWatchHealthHandler(cfg.Interval, time.Now,
		watcher.LastPassAt, lockCell)}
	go func() {
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("slack watch: healthz server", "err", err)
		}
	}()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = health.Shutdown(sctx)
	}()

	standbyLogged := false
	for lock == nil {
		l, ok, err := tryWatchLock(ctx, pool)
		if err != nil {
			return fmt.Errorf("slack watch lock: %w", err)
		}
		if ok {
			lock = l
			break
		}
		if !standbyLogged {
			slog.Info("slack watch: another watcher holds the lock; standing by")
			standbyLogged = true
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(15 * time.Second):
		}
	}
	defer lock.Release()
	lockCell.set(lock)
	slog.Info("slack watch: lock held; running")
	return watcher.Run(ctx)
}

// lockHolder publishes the current lock handle to /healthz atomically: a
// standbyLock until the singleton is taken, the real handle after.
type lockHolder struct{ v atomic.Value }

func (h *lockHolder) set(c aliveChecker) { h.v.Store(&c) }

func (h *lockHolder) Alive(ctx context.Context) error {
	c, _ := h.v.Load().(*aliveChecker)
	if c == nil {
		return errStandby
	}
	return (*c).Alive(ctx)
}

// watchSource is D8's first refusal: the watcher runs ONLY over the HTTP
// bridge. Unset SLACK_WEB_BRIDGE_URL would fall back to CommandBridge, whose
// Export IGNORES the request entirely — a targeted pass over it would silently
// run a FULL export every minute.
func watchSource() (slackweb.Source, error) {
	rawURL := os.Getenv("SLACK_WEB_BRIDGE_URL")
	if rawURL == "" {
		return nil, errors.New("--watch requires SLACK_WEB_BRIDGE_URL: the CommandBridge transport discards the " +
			"export request, so a targeted pass over it would be a full export every minute")
	}
	token, err := slackweb.TokenFromEnv()
	if err != nil {
		return nil, err
	}
	return slackweb.NewHTTPBridge(rawURL, token, nil)
}

// startupLine is the one line that lets an operator explain the watcher's
// behaviour without reading the manifest.
func startupLine(cfg slackweb.WatchConfig, targets int, mode string, horizon time.Duration) string {
	interval := shortDur(cfg.Interval)
	if cfg.TargetedDisabled() {
		interval = "off"
	}
	return fmt.Sprintf("slack watch: interval=%s rotation=%s budget=%s targets=%d mode=%s horizon=%s health=%s",
		interval, shortDur(cfg.RotationInterval), shortDur(time.Duration(cfg.BudgetMS)*time.Millisecond), targets,
		mode, shortDur(horizon), cfg.HealthAddr)
}

// shortDur prints a duration in its largest whole unit (720h, 30m, 150s) —
// the runbook's spelling, not Go's 720h0m0s.
func shortDur(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	case d >= 2*time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%dm", int64(d/time.Minute))
	default: // the per-minute cadence reads as 60s, the runbook's spelling
		return fmt.Sprintf("%ds", int64(d/time.Second))
	}
}

// checkCaptureConfig is D8's second refusal: a LIVE capture pass whose horizon
// is under capture.MinLiveRulesHorizon fails every pass — a CronJob shows that
// as one red run, a resident loop as a process that looks alive while
// capturing nothing. Shadow is not refused.
func checkCaptureConfig(mode string, horizon time.Duration) error {
	if mode == capture.RulesModeLive && horizon < capture.MinLiveRulesHorizon {
		return fmt.Errorf("CAPTURE_RULES_MODE=live with a %s horizon is below the %s floor; every pass would be a "+
			"silent no-op — set CAPTURE_RULES_SINCE to at least %s", horizon, capture.MinLiveRulesHorizon,
			capture.MinLiveRulesHorizon)
	}
	return nil
}

// newSource picks the transport for the ONE-SHOT pass.
//
// The connector needs an authenticated browser, which is host-bound to the Mac
// mini, so a cluster-resident poller cannot exec it locally: SLACK_WEB_BRIDGE_URL
// selects the HTTP bridge on that host. The local command bridge remains for
// running the poller on the same machine as the browser.
func newSource() (slackweb.Source, error) {
	if rawURL := os.Getenv("SLACK_WEB_BRIDGE_URL"); rawURL != "" {
		token, err := slackweb.TokenFromEnv()
		if err != nil {
			return nil, err
		}
		return slackweb.NewHTTPBridge(rawURL, token, nil)
	}
	return slackweb.NewCommandBridge(os.Getenv("SLACK_WEB_NODE"), os.Getenv("SLACK_WEB_BRIDGE_SCRIPT"))
}

func printStats(phase string, stats slackweb.Stats) {
	out, _ := json.Marshal(stats)
	fmt.Printf("%s: %s\n", phase, out)
}
