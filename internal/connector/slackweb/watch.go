package slackweb

// The resident Slack watcher's loop (SWT-75 D3, D5, D10): two cadences on one
// browser, strictly sequential, and a failure discipline that keeps the
// process alive whatever the Mac mini does.
//
//   - a TARGETED pass every Interval: the enabled slack_watch rows as
//     `targets`, budget_ms = BudgetMS, ingested raw-first under
//     PhaseSlackWebWatch;
//   - a ROTATION pass every RotationInterval: today's full export, unchanged;
//   - when both are due, the rotation wins (its export covers every
//     conversation), and the first targeted pass runs at STARTUP;
//   - a pass that overruns is followed by the next DUE pass, never by the
//     passes it missed — the browser is not to be hammered;
//   - any bridge error — 503, 500, EOF, a killed process, a non-targeted
//     answer — is a SKIPPED pass, counted, never a failed process: restarting
//     the pod cannot fix Chrome. A 503 sleeps min(Retry-After, Interval).
//
// The pass functions are injected (WatchDeps) so the loop is unit-tested with
// a fake clock and no I/O; cmd/connectors/slackweb wires the real passes.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"
)

// PassKind names the two cadences (the counter line's pass= value).
type PassKind string

const (
	PassTargeted PassKind = "targeted"
	PassRotation PassKind = "rotation"
)

// WatchConfig is the loop's five knobs, read by WatchConfigFromEnv.
type WatchConfig struct {
	Interval         time.Duration // SLACK_WATCH_INTERVAL, default 60s; 0 disables targeted passes
	RotationInterval time.Duration // SLACK_ROTATION_INTERVAL, default 30m
	BudgetMS         int           // SLACK_WATCH_BUDGET_MS, default 150000
	BridgeGrace      time.Duration // SLACK_BRIDGE_GRACE, default 120s
	HealthAddr       string        // SLACK_WATCH_HEALTH_ADDR, default :8093
}

const (
	DefaultWatchInterval    = 60 * time.Second
	DefaultRotationInterval = 30 * time.Minute
	DefaultWatchBudgetMS    = 150000
	DefaultBridgeGrace      = 120 * time.Second
	DefaultWatchHealthAddr  = ":8093"
	// disabledTargeted is what SLACK_WATCH_INTERVAL=0 becomes: a cadence so
	// long it never fires, never a zero sleep (D10's rollback lever).
	disabledTargeted = 100 * 365 * 24 * time.Hour
)

// WatchConfigFromEnv reads the knobs with the positiveEnv discipline: an
// unparseable or non-positive value falls back to the default, never to a
// zero the leaf would 500 on or a sleep the loop would spin on. The one
// deliberate exception: SLACK_WATCH_INTERVAL=0 (exactly) DISABLES targeted
// passes — rotation only, no image roll.
func WatchConfigFromEnv() WatchConfig {
	cfg := WatchConfig{
		Interval:         positiveDurationEnv("SLACK_WATCH_INTERVAL", DefaultWatchInterval),
		RotationInterval: positiveDurationEnv("SLACK_ROTATION_INTERVAL", DefaultRotationInterval),
		BudgetMS:         positiveEnv("SLACK_WATCH_BUDGET_MS", DefaultWatchBudgetMS),
		BridgeGrace:      positiveDurationEnv("SLACK_BRIDGE_GRACE", DefaultBridgeGrace),
		HealthAddr:       os.Getenv("SLACK_WATCH_HEALTH_ADDR"),
	}
	if cfg.HealthAddr == "" {
		cfg.HealthAddr = DefaultWatchHealthAddr
	}
	if v := os.Getenv("SLACK_WATCH_INTERVAL"); v == "0" {
		cfg.Interval = disabledTargeted
	}
	return cfg
}

// TargetedDisabled reports whether SLACK_WATCH_INTERVAL=0 turned the targeted
// cadence off.
func (c WatchConfig) TargetedDisabled() bool { return c.Interval >= disabledTargeted }

// PassTimeout is the Go context bound for a targeted pass: the leaf's budget
// PLUS the grace, so the client deadline can only fire after the leaf has
// already given up (D5: a disconnecting /export caller kills the bridge).
func (c WatchConfig) PassTimeout() time.Duration {
	return time.Duration(c.BudgetMS)*time.Millisecond + c.BridgeGrace
}

func positiveDurationEnv(name string, fallback time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	// A bare number is the realistic typo ("720"): not a Go duration, and
	// time.ParseDuration refuses it, so it falls back rather than becoming 720ns.
	if _, err := strconv.Atoi(v); err == nil {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// WatchDeps are the loop's injectable parts: a clock, a cancellable sleep and
// the pass function.
type WatchDeps struct {
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	Pass  func(ctx context.Context, kind PassKind) error
}

// Watcher runs the two cadences.
type Watcher struct {
	cfg  WatchConfig
	deps WatchDeps

	mu       sync.Mutex
	lastPass time.Time // last COMPLETED pass; /healthz's input
	skipped  int
	// stoodDown: the leaf answered a targeted request without coverage.mode
	// "targeted" (an old leaf, D8). Targeted passes stop until the next
	// COMPLETED rotation, which is the next time the leaf is known to answer.
	stoodDown bool
}

// NewWatcher builds a watcher. LastPassAt starts at construction: a watcher
// that has just started is not stale, and only a completed pass advances it.
func NewWatcher(cfg WatchConfig, deps WatchDeps) *Watcher {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Sleep == nil {
		deps.Sleep = sleepCtx
	}
	return &Watcher{cfg: cfg, deps: deps, lastPass: deps.Now()}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// LastPassAt is the instant the last pass COMPLETED (or the construction
// instant while none has).
func (w *Watcher) LastPassAt() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastPass
}

// Skipped counts passes a bridge failure skipped.
func (w *Watcher) Skipped() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.skipped
}

// StoodDown reports whether targeted passes are suspended after a
// non-targeted answer from the leaf (D8).
func (w *Watcher) StoodDown() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stoodDown
}

// Run loops until ctx ends and returns nil then (criterion 25: SIGTERM is
// exit 0; an in-flight pass finishes, a new one never starts).
func (w *Watcher) Run(ctx context.Context) error {
	start := w.deps.Now()
	nextTargeted := start
	if w.cfg.TargetedDisabled() {
		// SLACK_WATCH_INTERVAL=0: rotation only, not even one pass at startup.
		nextTargeted = start.Add(disabledTargeted)
	}
	nextRotation := start.Add(w.cfg.RotationInterval)
	// rotationYields: a rotation the bridge refused (503) gives the next tie to
	// the targeted pass, so a foreign export holding the browser never turns
	// the per-minute cadence into a per-minute rotation retry.
	rotationYields := false
	for {
		if ctx.Err() != nil {
			return nil
		}
		now := w.deps.Now()
		// The next DUE pass, once. A pass that outlived its interval is followed
		// by the next scheduled one, never by the ones it missed. On a tie the
		// rotation wins (it subsumes the targeted read) unless it just yielded.
		due := nextTargeted
		kind := PassTargeted
		if nextRotation.Before(nextTargeted) || (nextRotation.Equal(nextTargeted) && !rotationYields) {
			due, kind = nextRotation, PassRotation
		}
		if wait := due.Sub(now); wait > 0 {
			if err := w.deps.Sleep(ctx, wait); err != nil {
				return nil
			}
			continue
		}
		if kind == PassTargeted && w.StoodDown() {
			// D8: the leaf did not honour targets; wait for the next rotation to
			// prove it answers before asking again. The rotation cadence carries on.
			nextTargeted = nextAfter(nextTargeted, now, w.cfg.Interval)
			continue
		}
		if kind == PassRotation {
			rotationYields = false
		}
		passErr := w.deps.Pass(ctx, kind)
		after := w.deps.Now()
		// Both cadences advance from the instant this pass STARTED: the rotation
		// subsumes the targeted read (its export covers every conversation), so
		// a targeted pass due at the same instant is not run behind it.
		switch kind {
		case PassRotation:
			nextRotation = nextAfter(nextRotation, after, w.cfg.RotationInterval)
			nextTargeted = nextAfter(nextTargeted, after, w.cfg.Interval)
		default:
			nextTargeted = nextAfter(nextTargeted, after, w.cfg.Interval)
		}
		if passErr != nil {
			w.mu.Lock()
			w.skipped++
			w.mu.Unlock()
			if ctx.Err() != nil {
				return nil
			}
			var busy *BridgeBusyError
			switch {
			case errors.As(passErr, &busy):
				// The queue told us when to come back: honour it, bounded by the
				// interval so a long Retry-After never idles the watch list.
				delay := busy.RetryAfter
				if delay <= 0 || delay > w.cfg.Interval {
					delay = w.cfg.Interval
				}
				slog.Info("slack watch: bridge busy; pass skipped", "pass", kind, "retry_after", busy.RetryAfter, "sleep", delay)
				if kind == PassRotation {
					// A busy ROTATION must not starve the targeted cadence: it retries
					// no sooner than two targeted intervals (a sixth of its own
					// interval in production), yields the next tie, and the targeted
					// schedule is left exactly where nextAfter put it.
					retry := w.cfg.RotationInterval / 6
					if retry < delay {
						retry = delay
					}
					if min := 2 * w.cfg.Interval; retry < min && !w.cfg.TargetedDisabled() {
						retry = min
					}
					nextRotation = after.Add(retry)
					rotationYields = true
				} else {
					nextTargeted = after.Add(delay)
				}
			case errors.Is(passErr, ErrNotTargeted):
				w.mu.Lock()
				w.stoodDown = true
				w.mu.Unlock()
				slog.Error("slack watch: the leaf did not honour targets; targeted passes stand down until the next "+
					"completed rotation (an old leaf answers a targeted request with a FULL export)", "err", passErr)
			default:
				slog.Warn("slack watch: pass skipped", "pass", kind, "err", passErr)
			}
			continue
		}
		w.mu.Lock()
		w.lastPass = after
		if kind == PassRotation {
			w.stoodDown = false // the leaf answered a full export: try targets again
		}
		w.mu.Unlock()
	}
}

// nextAfter advances a due instant by whole intervals until it is after now —
// the next scheduled slot, with no catch-up burst.
func nextAfter(due, now time.Time, interval time.Duration) time.Time {
	if interval <= 0 {
		return now
	}
	next := due.Add(interval)
	for !next.After(now) {
		next = next.Add(interval)
	}
	return next
}

// ErrNoTargets: the enabled watch list is empty, so no targeted pass is issued
// at all — the browser is never asked for nothing (criterion 6).
var ErrNoTargets = errors.New("slack watch: no enabled targets")

// RunTargeted is one targeted pass, end to end: the request from the watch
// rows (dropped rows logged by name), a refusal WITHOUT touching the bridge
// when nothing is enabled, a refusal of any response that is not
// coverage.mode "targeted" (criterion 7), and otherwise the SAME raw-first
// ingest under PhaseSlackWebWatch with D6's quiet run rows.
func RunTargeted(ctx context.Context, source Source, sink Sink, rows []WatchRow, cfg WatchConfig) (Stats, error) {
	req, dropped := BuildTargetedRequest(rows, cfg.BudgetMS)
	for _, d := range dropped {
		slog.Warn("slack watch: row fails the leaf's id rules; not sent",
			"id", d.ID, "workspace", d.WorkspaceID, "conversation", d.ConversationID)
	}
	if len(req.Targets) == 0 {
		return Stats{}, ErrNoTargets
	}
	stats, err := IngestWith(ctx, source, sink, IngestOptions{
		Request: req, Phase: PhaseSlackWebWatch, Quiet: true, Targeted: true,
	})
	if err != nil {
		return stats, fmt.Errorf("targeted pass: %w", err)
	}
	return stats, nil
}
