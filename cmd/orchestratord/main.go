// orchestratord is the switchboard spine loop (SPEC 05-orchestrator-loop):
// LISTEN on task_events + a cron ticker, pure rules, actions applied through
// the executor and the fleet command topic. Never calls an LLM (invariant 7).
//
//	orchestratord [--tick 60s] [--once]
//
//	DATABASE_URL       ops db, required
//	MQTT_BROKER        required (resume publishes)
//	ORCH_BRIEF_PROJECT optional; unset disables the morning brief
//	ORCH_BRIEF_HOUR    default 7 (process-local TZ)
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
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

	if err := run(*tick, *once); err != nil {
		slog.Error("orchestratord failed", "err", err)
		os.Exit(1)
	}
}

func run(tick time.Duration, once bool) error {
	broker := os.Getenv("MQTT_BROKER")
	if broker == "" {
		return fmt.Errorf("MQTT_BROKER is not set")
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

	ok, release, err := orchestrator.TryAdvisoryLock(ctx, pool)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("another orchestratord holds the advisory lock; exiting")
	}
	defer release()

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

	slog.Info("orchestratord running", "tick", tick.String(), "brief_project", cfg.BriefProject)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("orchestratord stopping")
			return nil
		case <-wake:
		case <-ticker.C:
			if err := engine.TickOnce(ctx, time.Now()); err != nil {
				slog.Error("tick failed", "err", err)
			}
		}
		if n, err := engine.DrainOnce(ctx); err != nil {
			slog.Error("drain failed", "err", err)
		} else if n > 0 {
			slog.Info("drained", "events", n)
		}
	}
}
