package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/sspataro57/switchboard/internal/fleet"
)

// ErrLockHeld is what a pass returns (possibly wrapped) when another holder has
// its advisory lock. The loop treats it as "not now", never as an error: it
// retries after LockRetry, then on the next sweep.
var ErrLockHeld = errors.New("pipeline: advisory lock held elsewhere")

// PassFunc runs one --limit-bounded pass over a stage's inbox and reports how
// many rows it processed.
type PassFunc func(ctx context.Context) (processed int, err error)

// StageConfig configures one stage loop. Zero values take the E-D4 defaults.
type StageConfig struct {
	Stage     Stage
	Pass      PassFunc
	Limit     int           // a pass with processed >= Limit is repeated at once
	Sweep     time.Duration // 0 → PipelineSweep
	LockRetry time.Duration // 0 → LockRetryDelay
	// Status is the heartbeat sink; nil → no heartbeat.
	Status StatusPublisher
	// After is the clock for the sweep, the lock retry AND the heartbeat
	// cadence; nil → time.After.
	After func(time.Duration) <-chan time.Time
}

// StageLoop is one stage: a coalescing wake, a sweep fallback, a lost-lock
// retry and a heartbeat, around an injected pass.
type StageLoop struct {
	cfg  StageConfig
	wake chan struct{}
}

func NewStageLoop(cfg StageConfig) *StageLoop {
	if cfg.Sweep <= 0 {
		cfg.Sweep = PipelineSweep
	}
	if cfg.LockRetry <= 0 {
		cfg.LockRetry = LockRetryDelay
	}
	if cfg.After == nil {
		cfg.After = time.After
	}
	return &StageLoop{cfg: cfg, wake: make(chan struct{}, 1)}
}

// Notify is the coalescing wake: buffer 1, so a burst during a pass is ONE
// pending run. Non-blocking and safe from any goroutine (the MQTT handler).
func (l *StageLoop) Notify() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// Run starts with one catch-up pass (a restarted pod must not sit on a backlog
// for a sweep), then waits for a wake, the sweep or a lock retry. It returns
// nil when ctx ends and never because a pass failed: a stage that errors logs,
// stays up and retries on the next wake or sweep.
func (l *StageLoop) Run(ctx context.Context) error {
	status := l.startStatus(ctx)
	status(fleet.StateIdle)
	hb := l.cfg.After(fleet.HeartbeatInterval)

	var sweep, retry <-chan time.Time
	run := func() {
		lost := l.drain(ctx, status, &hb)
		retry = nil
		if lost {
			retry = l.cfg.After(l.cfg.LockRetry)
		}
		// Re-armed after every pass, never on a heartbeat tick: the 60 s
		// heartbeat must not keep pushing the 5 m sweep out of reach.
		sweep = l.cfg.After(l.cfg.Sweep)
	}
	run()
	for {
		if ctx.Err() != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-l.wake:
			run()
		case <-sweep:
			run()
		case <-retry:
			run()
		case <-hb:
			status(fleet.StateIdle)
			hb = l.cfg.After(fleet.HeartbeatInterval)
		}
	}
}

// drain runs passes until one comes back short of the limit, fails, or loses
// its lock (reported as lockLost). working while a pass runs, idle after.
func (l *StageLoop) drain(ctx context.Context, status func(string), hb *<-chan time.Time) (lockLost bool) {
	defer status(fleet.StateIdle)
	for ctx.Err() == nil {
		status(fleet.StateWorking)
		n, err := l.pass(ctx, status, hb)
		switch {
		case errors.Is(err, ErrLockHeld):
			slog.Info("pipeline stage lock held elsewhere; retrying later", "stage", l.cfg.Stage, "retry_in", l.cfg.LockRetry)
			return true
		case err != nil:
			if ctx.Err() == nil {
				slog.Error("pipeline stage pass failed; retrying on the next wake or sweep", "stage", l.cfg.Stage, "err", err)
			}
			return false
		case l.cfg.Limit > 0 && n >= l.cfg.Limit:
			continue // a full pass: the inbox may hold more
		default:
			return false
		}
	}
	return false
}

// pass runs one pass while keeping the heartbeat alive: an inquiry pass at
// ~4.5 s/verdict outlives several heartbeats and must not look dead or idle.
// It always waits for the pass to return, so two passes of one stage never
// overlap.
func (l *StageLoop) pass(ctx context.Context, status func(string), hb *<-chan time.Time) (int, error) {
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := l.cfg.Pass(ctx)
		done <- result{n, err}
	}()
	for {
		select {
		case r := <-done:
			return r.n, r.err
		case <-*hb:
			status(fleet.StateWorking)
			*hb = l.cfg.After(fleet.HeartbeatInterval)
		}
	}
}

// startStatus returns the loop's heartbeat function. Publishing happens on its
// own goroutine, in order: paho blocks a publish while it reconnects, and a
// broker outage must never stall a stage's passes (the sweep has to keep
// working without MQTT).
func (l *StageLoop) startStatus(ctx context.Context) func(string) {
	if l.cfg.Status == nil {
		return func(string) {}
	}
	states := make(chan string, 64)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case s := <-states:
				if err := l.cfg.Status.PublishStatus(fleet.Status{State: s, TS: time.Now().UTC()}); err != nil {
					slog.Warn("pipeline heartbeat publish failed", "stage", l.cfg.Stage, "err", err)
				}
			}
		}
	}()
	return func(s string) {
		select {
		case states <- s:
		default:
			slog.Warn("pipeline heartbeat queue full; dropping a status", "stage", l.cfg.Stage, "state", s)
		}
	}
}
