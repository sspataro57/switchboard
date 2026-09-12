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
//	OPS_TOKEN_KEY    the gate stage's Jira lookup credential. Unset: no lookup;
//	                 holds stay pending until they expire (fail closed)
//
// Flags: --stages overrides PIPELINE_STAGES; --sweep overrides the 5 m sweep.
//
// Exit: 0 on SIGTERM/SIGINT; non-zero when a stage's pass wedges
// (pipeline.ErrPassWedged), so the pod restarts rather than sit silent.
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
// process is up even with no stage enabled, and its dead says when it is not.
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

// stageImpls is every stage this build implements. Parts C and B add theirs.
// A stage named in PIPELINE_STAGES but absent here is refused at startup,
// never silently skipped.
var stageImpls = map[pipeline.Stage]stageImpl{
	pipeline.StageGate: {limit: gateStageLimit, pass: gatePass}, // Part D (gate.go)
}

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
	clients := []*fleet.Client{daemon}
	var (
		wg   sync.WaitGroup
		pool *pgxpool.Pool
	)
	// finish ends every loop, THEN publishes dead: a clean DISCONNECT suppresses
	// the LWT, so a stopped pipelined says dead deliberately, and only after
	// every heartbeat goroutine has returned (nothing publishes idle after it).
	// The publishes run in parallel, so a dead broker costs one ack timeout,
	// not one per client, inside terminationGracePeriodSeconds.
	errCh := make(chan error, len(stages))
	finish := func(runErr error) error {
		stop()
		wg.Wait()
		// A stage that wedged while shutdown was already under way sent its
		// ErrPassWedged after the main select stopped reading: keep it, so the
		// pool is not closed under the wedged pass and the exit is non-zero.
		if runErr == nil {
			select {
			case runErr = <-errCh:
			default:
			}
		}
		var dead sync.WaitGroup
		for _, c := range clients {
			dead.Add(1)
			go func() {
				defer dead.Done()
				if err := c.PublishDead(); err != nil {
					slog.Warn("final dead status not published", "err", err)
				}
				c.Disconnect()
			}()
		}
		dead.Wait()
		// A wedged pass still holds a pooled connection (a GPU stage's advisory
		// lock), and pool.Close blocks until every connection returns: closing
		// it would hang the exit the wedge path exists to force. The process
		// ends right after, which releases everything.
		if pool != nil && !errors.Is(runErr, pipeline.ErrPassWedged) {
			pool.Close()
		}
		return runErr
	}

	for _, e := range pipeline.Events() {
		if err := daemon.Subscribe(pipeline.Topic(e), logWake); err != nil {
			return finish(fmt.Errorf("subscribe wake log: %w", err))
		}
	}

	if len(stages) > 0 {
		if pool, err = store.NewPool(ctx); err != nil {
			return finish(fmt.Errorf("connect db: %w", err))
		}
	}

	for _, s := range stages {
		impl := stageImpls[s]
		client, err := pipeline.DialStage(ctx, broker, s)
		if err != nil {
			return finish(fmt.Errorf("connect stage %s: %w", s, err))
		}
		clients = append(clients, client)
		loop := pipeline.NewStageLoop(pipeline.StageConfig{
			Stage: s, Pass: impl.pass(pool), Limit: impl.limit, Sweep: *sweep, Status: client,
		})
		if err := pipeline.SubscribeWakes(client, s, func(pipeline.Wake) { loop.Notify() }); err != nil {
			return finish(fmt.Errorf("stage %s: %w", s, err))
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := loop.Run(ctx); err != nil {
				errCh <- fmt.Errorf("stage %s: %w", s, err)
			}
		}()
		slog.Info("stage started", "stage", s, "upstream", pipeline.Upstream(s))
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		daemonHeartbeat(ctx, daemon)
	}()
	slog.Info("pipelined serving", "stages", stages, "sweep", *sweep, "broker", broker)

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errCh:
		slog.Error("a stage gave up; stopping so the pod restarts", "err", runErr)
	}
	return finish(runErr)
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
// ends. Each publish is bounded (fleet's ack timeout), so shutdown never waits
// on a reconnecting broker for long.
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
