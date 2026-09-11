// orchestratord is the switchboard spine loop (SPEC 05-orchestrator-loop):
// LISTEN on task_events + a cron ticker, pure rules, actions applied through
// the executor and the fleet command topic. Never calls an LLM (invariant 7).
//
//	orchestratord [--tick 60s] [--once]
//
//	DATABASE_URL       ops db, required
//	MQTT_BROKER        required (resume publishes)
//	ORCH_HEALTH_ADDR   default :8091 — GET /healthz, the liveness probe (SWT-41 D5)
//	ORCH_BRIEF_PROJECT optional; unset disables the morning brief
//	ORCH_BRIEF_HOUR    default 7 (process-local TZ)
//
// It arms no tools.Set* seam: every sender stays nil, so this process cannot
// send (invariant 4, SWT-41 D2).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/fleet"
	"github.com/sspataro57/switchboard/internal/orchestrator"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	tick := flag.Duration("tick", 60*time.Second, "ticker interval (drain + expiry/brief)")
	once := flag.Bool("once", false, "single drain+tick pass and exit (smokes)")
	flag.Parse()

	if err := run(*tick, *once, os.Exit); err != nil {
		slog.Error("orchestratord failed", "err", err)
		os.Exit(1)
	}
}

// aliveChecker is the lock handle's liveness surface: *orchestrator.LockHandle
// in production, a fake in tests.
type aliveChecker interface {
	Alive(ctx context.Context) error
}

// newHealthHandler serves GET /healthz: 200 "ok" iff a loop iteration
// completed within 3 x tick AND the lock connection answers; else 503 with a
// one-line reason and nothing else. It catches a wedged loop, which the
// dashboard would only show as "stalled" later (SWT-41 D5).
func newHealthHandler(tick time.Duration, now func() time.Time, lastTick func() time.Time, lock aliveChecker) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if age := now().Sub(lastTick()); age > 3*tick {
			http.Error(w, fmt.Sprintf("no loop iteration within 3 ticks (last %s ago)", age.Truncate(time.Second)),
				http.StatusServiceUnavailable)
			return
		}
		actx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := lock.Alive(actx); err != nil {
			slog.Error("healthz: orchestrator lock connection lost", "err", err)
			http.Error(w, "orchestrator lock connection lost", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
}

// checkLockOrExit runs on every tick (SWT-41 D3). A lost lock connection
// (a CNPG switchover, say) must become a restart, not an unlocked engine that
// keeps draining: log and exit non-zero. Kubernetes restarts the pod, which
// re-takes the lock or exits on contention.
func checkLockOrExit(ctx context.Context, lock aliveChecker, exit func(code int)) bool {
	// Bounded: a half-open connection must become a prompt exit, not a hang
	// that also blocks /healthz on the lock handle's mutex.
	actx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := lock.Alive(actx); err != nil {
		if ctx.Err() != nil {
			return false // shutting down: the cancelled context is the error, not the lock
		}
		slog.Error("orchestrator lock connection lost; exiting so the pod restarts and re-takes it", "err", err)
		exit(1)
		return false
	}
	return true
}

func run(tick time.Duration, once bool, exit func(code int)) error {
	broker := os.Getenv("MQTT_BROKER")
	if broker == "" {
		return fmt.Errorf("MQTT_BROKER is not set")
	}
	healthAddr := strings.TrimSpace(os.Getenv("ORCH_HEALTH_ADDR"))
	if healthAddr == "" {
		healthAddr = ":8091"
	}
	cfg := orchestrator.Config{BriefProject: os.Getenv("ORCH_BRIEF_PROJECT"), BriefHour: 7}
	if h := os.Getenv("ORCH_BRIEF_HOUR"); h != "" {
		n, err := strconv.Atoi(h)
		if err != nil {
			return fmt.Errorf("ORCH_BRIEF_HOUR %q: %w", h, err)
		}
		cfg.BriefHour = n
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer pool.Close()

	lock, ok, err := orchestrator.TryAdvisoryLock(ctx, pool)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("another orchestratord holds the advisory lock; exiting")
	}
	defer lock.Release()

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	ex := executor.New(reg, checker, audit.NewPGStore(pool))

	spine, err := fleet.NewSpineClient(ctx, broker, "switchboard-orchestratord")
	if err != nil {
		return fmt.Errorf("connect broker: %w", err)
	}
	defer spine.Disconnect()

	engine := orchestrator.NewEngine(pool, ex, spine, cfg)

	var lastTick atomic.Int64
	markTick := func() { lastTick.Store(time.Now().UnixNano()) }
	markTick()
	// A catch-up drain marks progress per event, so /healthz sees a busy loop,
	// not a wedged one; and it re-checks the lock before every batch.
	engine.SetDrainHooks(orchestrator.DrainHooks{
		Progress: markTick,
		Guard: func(ctx context.Context) error {
			if !checkLockOrExit(ctx, lock, exit) {
				return errors.New("orchestrator lock lost")
			}
			return nil
		},
	})

	if once {
		n, err := engine.DrainOnce(ctx)
		if err != nil {
			return err
		}
		if err := engine.TickOnce(ctx, time.Now()); err != nil {
			return err
		}
		slog.Info("once pass complete", "events_processed", n)
		return nil
	}

	mux := http.NewServeMux()
	mux.Handle("/healthz", newHealthHandler(tick, time.Now,
		func() time.Time { return time.Unix(0, lastTick.Load()) }, lock))
	srv := &http.Server{Addr: healthAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("healthz server stopped", "addr", healthAddr, "err", err)
		}
	}()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	wake := make(chan struct{}, 1)
	go func() {
		for ctx.Err() == nil {
			if err := engine.Listen(ctx, func() {
				select {
				case wake <- struct{}{}:
				default:
				}
			}); err != nil {
				slog.Error("listen loop error; reconnecting", "err", err)
				time.Sleep(2 * time.Second)
			}
		}
	}()

	slog.Info("orchestratord running", "tick", tick.String(), "brief_project", cfg.BriefProject, "health_addr", healthAddr)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		ticked := false
		select {
		case <-ctx.Done():
			slog.Info("orchestratord stopping")
			return nil
		case <-wake:
		case <-ticker.C:
			ticked = true
		}
		// Before ANY mutation — a notification wake as much as a tick — the lock
		// must still be ours (SWT-41 review; D3).
		if !checkLockOrExit(ctx, lock, exit) {
			return nil
		}
		if ticked {
			if err := engine.TickOnce(ctx, time.Now()); err != nil {
				slog.Error("tick failed", "err", err)
			}
		}
		if n, err := engine.DrainOnce(ctx); err != nil {
			slog.Error("drain failed", "err", err)
		} else if n > 0 {
			slog.Info("drained", "events", n)
		}
		markTick()
	}
}
