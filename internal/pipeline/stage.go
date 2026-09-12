package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sspataro57/switchboard/internal/fleet"
)

// ErrLockHeld is what a pass returns (possibly wrapped) when another holder has
// its advisory lock. The loop treats it as "not now", never as an error: it
// retries once after LockRetry, then waits for the sweep.
var ErrLockHeld = errors.New("pipeline: advisory lock held elsewhere")

// ErrPassWedged is Run's only error: a pass outlived its timeout and then
// ignored its cancelled context for PassWedgeGrace more. The loop cannot start
// another pass without risking two at once, so it gives up and the pod restarts
// (cmd/pipelined exits non-zero).
var ErrPassWedged = errors.New("pipeline: a pass ignored its cancellation")

// PassFunc runs one --limit-bounded pass over a stage's inbox and reports how
// many rows it processed. processed counts rows that LEFT the inbox (decided,
// promoted, resolved): a pass that counts rows it leaves in place repeats until
// MaxDrainPasses. It must honour ctx; the loop cancels it after PassTimeout.
type PassFunc func(ctx context.Context) (processed int, err error)

const (
	// PassTimeout bounds one pass. A DB or model call that hangs must not
	// silence the stage's wakes, sweep and shutdown forever.
	PassTimeout = 15 * time.Minute
	// PassWedgeGrace is how long a cancelled pass gets to return before Run
	// gives up with ErrPassWedged. Deliberately not 60 s: no two loop timers
	// share a duration, so a test's fake clock can fire one without the other.
	PassWedgeGrace = 90 * time.Second
	// MaxDrainPasses caps consecutive full passes in one drain, so an inbox
	// that never empties costs one burst per sweep instead of a hot loop.
	MaxDrainPasses = 50
)

// StageConfig configures one stage loop. Zero values take the defaults above.
type StageConfig struct {
	Stage       Stage
	Pass        PassFunc
	Limit       int           // a pass with processed >= Limit is repeated at once
	Sweep       time.Duration // 0 → PipelineSweep
	LockRetry   time.Duration // 0 → LockRetryDelay
	PassTimeout time.Duration // 0 → PassTimeout
	// Status is the heartbeat sink; nil → no heartbeat.
	Status StatusPublisher
	// After is the clock for the sweep, the lock retry, the pass timeout AND
	// the heartbeat cadence; nil → time.After.
	After func(time.Duration) <-chan time.Time
}

// StageLoop is one stage: a coalescing wake, a sweep fallback, a lost-lock
// retry, a pass timeout and a heartbeat, around an injected pass.
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
	if cfg.PassTimeout <= 0 {
		cfg.PassTimeout = PassTimeout
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
// stays up and retries on the next wake or sweep. The one exception is a
// wedged pass (ErrPassWedged). Before returning it waits for its last
// heartbeat to go out, so a caller publishing a final status goes last.
func (l *StageLoop) Run(ctx context.Context) error {
	status, statusDone := l.startStatus(ctx)
	defer func() { <-statusDone }()
	status(fleet.StateIdle)
	hb := l.cfg.After(fleet.HeartbeatInterval)

	var sweep, retry <-chan time.Time
	run := func(fromRetry bool) error {
		lost, err := l.drain(ctx, status, &hb)
		if err != nil {
			return err
		}
		retry = nil
		// One retry after LockRetry, then the sweep (E-D4): a lock a classify
		// CronJob holds for an hour costs one attempt per sweep, not one every
		// 30 s.
		if lost && !fromRetry {
			retry = l.cfg.After(l.cfg.LockRetry)
		}
		// Re-armed after every pass, never on a heartbeat tick: the 60 s
		// heartbeat must not keep pushing the 5 m sweep out of reach.
		sweep = l.cfg.After(l.cfg.Sweep)
		return nil
	}
	if err := run(false); err != nil {
		return err
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		var err error
		select {
		case <-ctx.Done():
			return nil
		case <-l.wake:
			err = run(false)
		case <-sweep:
			err = run(false)
		case <-retry:
			err = run(true)
		case <-hb:
			status(fleet.StateIdle)
			hb = l.cfg.After(fleet.HeartbeatInterval)
		}
		if err != nil {
			return err
		}
	}
}

// drain runs passes until one comes back short of the limit, fails, loses its
// lock (lockLost) or hits MaxDrainPasses. working for the whole drain, idle
// after.
func (l *StageLoop) drain(ctx context.Context, status func(string), hb *<-chan time.Time) (lockLost bool, err error) {
	status(fleet.StateWorking)
	defer status(fleet.StateIdle)
	for i := 0; ctx.Err() == nil; i++ {
		if i == MaxDrainPasses {
			slog.Warn("pipeline stage stopped after consecutive full passes; the sweep resumes it",
				"stage", l.cfg.Stage, "passes", MaxDrainPasses, "limit", l.cfg.Limit)
			return false, nil
		}
		n, perr := l.pass(ctx, status, hb)
		switch {
		case errors.Is(perr, ErrPassWedged):
			return false, perr
		case errors.Is(perr, ErrLockHeld):
			slog.Info("pipeline stage lock held elsewhere; retrying later", "stage", l.cfg.Stage)
			return true, nil
		case perr != nil:
			if ctx.Err() == nil {
				slog.Error("pipeline stage pass failed; retrying on the next wake or sweep", "stage", l.cfg.Stage, "err", perr)
			}
			return false, nil
		case l.cfg.Limit > 0 && n >= l.cfg.Limit:
			continue // a full pass: the inbox may hold more
		default:
			return false, nil
		}
	}
	return false, nil
}

// pass runs one pass while keeping the heartbeat alive (an inquiry pass at
// ~4.5 s/verdict outlives several heartbeats). It always waits for the pass
// to return, so two passes of one stage never overlap. The one exception is a
// pass that ignores cancellation: past PassTimeout (or shutdown) plus
// PassWedgeGrace, pass gives up with ErrPassWedged.
func (l *StageLoop) pass(ctx context.Context, status func(string), hb *<-chan time.Time) (int, error) {
	type result struct {
		n   int
		err error
	}
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan result, 1)
	go func() {
		n, err := l.cfg.Pass(pctx)
		done <- result{n, err}
	}()
	timeout := l.cfg.After(l.cfg.PassTimeout)
	ctxDone := ctx.Done()
	var wedged <-chan time.Time
	for {
		select {
		case r := <-done:
			return r.n, r.err
		case <-*hb:
			status(fleet.StateWorking)
			*hb = l.cfg.After(fleet.HeartbeatInterval)
		case <-timeout:
			slog.Error("pipeline stage pass exceeded its timeout; cancelling it", "stage", l.cfg.Stage, "timeout", l.cfg.PassTimeout)
			cancel()
			timeout = nil
			if wedged == nil {
				wedged = l.cfg.After(PassWedgeGrace)
			}
		case <-ctxDone:
			ctxDone = nil
			if wedged == nil {
				wedged = l.cfg.After(PassWedgeGrace)
			}
		case <-wedged:
			return 0, fmt.Errorf("stage %s: %w", l.cfg.Stage, ErrPassWedged)
		}
	}
}

// startStatus returns the loop's heartbeat function and a channel closed when
// its publisher goroutine has exited. Publishing runs on its own goroutine, in
// order: a publish can block on a reconnecting broker, and a broker outage must
// never stall a stage's passes (the sweep keeps working without MQTT). When the
// queue is full, the OLDEST queued state is dropped: the newest always wins.
func (l *StageLoop) startStatus(ctx context.Context) (func(string), <-chan struct{}) {
	done := make(chan struct{})
	if l.cfg.Status == nil {
		close(done)
		return func(string) {}, done
	}
	states := make(chan string, 64)
	go func() {
		defer close(done)
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
		for {
			select {
			case states <- s:
				return
			default:
				select {
				case <-states: // drop the oldest queued state
				default:
				}
			}
		}
	}, done
}
