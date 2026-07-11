// fleetd mirrors ops/workers/+/status into worker_heartbeats (SPEC
// 03-mqtt-heartbeats). Trusted telemetry spine service — no executor tools,
// no task writes. Because status is retained, a fresh fleetd against an empty
// table rebuilds current fleet state from the broker alone.
//
//	MQTT_BROKER  e.g. tcp://192.168.50.45:1883, required
//	DATABASE_URL ops db, required
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sspataro57/switchboard/internal/fleet"
	"github.com/sspataro57/switchboard/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(); err != nil {
		slog.Error("fleetd failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	broker := os.Getenv("MQTT_BROKER")
	if broker == "" {
		return fmt.Errorf("MQTT_BROKER is not set")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer pool.Close()

	mirror := fleet.NewMirror(fleet.NewPGStore(pool))
	client, err := fleet.NewMirrorClient(ctx, broker)
	if err != nil {
		return fmt.Errorf("connect broker: %w", err)
	}
	defer client.Disconnect()

	if err := client.SubscribeStatus(func(topic string, payload []byte) {
		hctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := mirror.Handle(hctx, topic, payload); err != nil {
			slog.Error("mirror handle failed", "topic", topic, "err", err)
		}
	}); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	slog.Info("fleetd running", "broker", broker, "filter", fleet.StatusFilter)

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("fleetd stopping", "stats", mirror.Stats())
			return nil
		case <-ticker.C:
			slog.Info("fleetd stats", "stats", mirror.Stats())
		}
	}
}
