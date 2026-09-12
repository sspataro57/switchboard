// Command pipelined hosts SWT-40's message-level pipeline stages (Part E;
// docs/runbooks/pipeline.md). One goroutine per enabled stage. Each is woken by
// its upstream ops/pipeline wakes (pipeline.Upstream) and by the sweep, owns its
// MQTT client, heartbeat and dead LWT, and re-queries its own Postgres inbox on
// every pass: MQTT only wakes, Postgres is the queue of record (E-D1). Only
// capture stays on cron.
//
// Env:
//
//	MQTT_BROKER      required (tcp://192.168.50.45:1883 in prod)
//	PIPELINE_STAGES  comma list of stages to run. Empty runs none: the Part E
//	                 smoke, where the daemon heartbeats and logs every wake-up
//	DATABASE_URL     required once any stage is enabled
//
// Flags: --stages overrides PIPELINE_STAGES; --sweep overrides the 5 m sweep.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/fleet"
	"github.com/sspataro57/switchboard/internal/pipeline"
	"github.com/sspataro57/switchboard/internal/store"
)

// The daemon's own identity, apart from any stage: its heartbeat says the
// process is up even with no stage enabled, and its LWT says when it is not.
const (
	daemonWorkerID = "pipeline.daemon"
	daemonClientID = "switchboard-pipelined"
)

// stageImpl is one stage this build can run: its pass over the pool and the
// --limit that pass is bounded by (a pass that fills it is repeated at once).
type stageImpl struct {
	limit int
	pass  func(*pgxpool.Pool) pipeline.PassFunc
}

// stageImpls is every stage this build implements. Parts D, C and B add theirs.
// A stage named in PIPELINE_STAGES but absent here is refused at startup,
// never silently skipped.
var stageImpls = map[pipeline.Stage]stageImpl{}

func main() {
	if err := run(); err != nil {
		slog.Error("pipelined", "err", err)
		os.Exit(1)
	}
}

func run() error {
	stagesFlag := flag.String("stages", os.Getenv("PIPELINE_STAGES"), "comma-separated stages to run (empty: none)")
	sweep := flag.Duration("sweep", pipeline.PipelineSweep, "sweep fallback interval")
	flag.Parse()

	broker := os.Getenv("MQTT_BROKER")
	if broker == "" {
		return errors.New("MQTT_BROKER is not set")
	}
	stages, err := parseStages(*stagesFlag)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	daemon, err := fleet.NewWillClient(ctx, broker, daemonClientID, daemonWorkerID)
	if err != nil {
		return fmt.Errorf("connect daemon client: %w", err)
	}
	defer daemon.Disconnect()
	for _, e := range pipeline.Events() {
		if err := daemon.Subscribe(pipeline.Topic(e), logWake); err != nil {
			return fmt.Errorf("subscribe wake log: %w", err)
		}
	}

	var pool *pgxpool.Pool
	if len(stages) > 0 {
		if pool, err = store.NewPool(ctx); err != nil {
			return fmt.Errorf("connect db: %w", err)
		}
		defer pool.Close()
	}

	var wg sync.WaitGroup
	for _, s := range stages {
		impl := stageImpls[s]
		client, err := pipeline.DialStage(ctx, broker, s)
		if err != nil {
			return fmt.Errorf("connect stage %s: %w", s, err)
		}
		defer client.Disconnect()
		loop := pipeline.NewStageLoop(pipeline.StageConfig{
			Stage: s, Pass: impl.pass(pool), Limit: impl.limit, Sweep: *sweep, Status: client,
		})
		if err := pipeline.SubscribeWakes(client, s, func(pipeline.Wake) { loop.Notify() }); err != nil {
			return fmt.Errorf("stage %s: %w", s, err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = loop.Run(ctx)
		}()
		slog.Info("stage started", "stage", s, "upstream", pipeline.Upstream(s))
	}

	go daemonHeartbeat(ctx, daemon)
	slog.Info("pipelined serving", "stages", stages, "sweep", *sweep, "broker", broker)
	<-ctx.Done()
	wg.Wait()
	return nil
}

// parseStages reads the comma list: known stages only, each implemented by
// this build, no duplicates. Empty means none.
func parseStages(list string) ([]pipeline.Stage, error) {
	known := map[pipeline.Stage]bool{}
	for _, s := range pipeline.Stages() {
		known[s] = true
	}
	seen := map[pipeline.Stage]bool{}
	var out []pipeline.Stage
	for _, part := range strings.Split(list, ",") {
		s := pipeline.Stage(strings.TrimSpace(part))
		switch {
		case s == "":
			continue
		case !known[s]:
			return nil, fmt.Errorf("unknown stage %q (known: %v)", s, pipeline.Stages())
		case seen[s]:
			return nil, fmt.Errorf("stage %q listed twice", s)
		}
		if _, ok := stageImpls[s]; !ok {
			return nil, fmt.Errorf("stage %q is not implemented in this build", s)
		}
		seen[s] = true
		out = append(out, s)
	}
	return out, nil
}

func logWake(topic string, payload []byte) {
	w, err := pipeline.ParseWake(payload)
	if err != nil {
		slog.Warn("wake with a malformed payload", "topic", topic, "err", err)
		return
	}
	slog.Info("wake", "topic", topic, "source", w.Source, "counts", w.Counts)
}

// daemonHeartbeat republishes idle every fleet.HeartbeatInterval until ctx
// ends. Its own goroutine: a publish blocked on a reconnecting broker must not
// hold up shutdown.
func daemonHeartbeat(ctx context.Context, c *fleet.Client) {
	t := time.NewTicker(fleet.HeartbeatInterval)
	defer t.Stop()
	for {
		if err := c.PublishStatus(fleet.Status{State: fleet.StateIdle, TS: time.Now().UTC()}); err != nil {
			slog.Warn("daemon heartbeat publish failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
