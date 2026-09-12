package pipeline_test

// SWT-40 Part E, criteria E3 (the stage loop) and E4 (per-stage heartbeats),
// unit half (docs/tickets/inquiry-promote_SPEC.md E-D4). The stage function,
// the clock and the heartbeat publisher are all injected: no DB, no broker, no
// real 5-minute wait.
//
// GREENFIELD NOTE: compile-FAILs until internal/pipeline/stage.go exists.
// Imposed surface:
//
//	// ErrLockHeld is the sentinel a pass returns (possibly wrapped) when another
//	// holder has its advisory lock. The loop treats it as "not now", never as an
//	// error: it retries after LockRetryDelay, then on the next sweep.
//	var ErrLockHeld error
//
//	// PassFunc runs one --limit-bounded pass and reports how many inbox rows it
//	// processed.
//	type PassFunc func(ctx context.Context) (processed int, err error)
//
//	type StageConfig struct {
//	    Stage     Stage
//	    Pass      PassFunc
//	    Limit     int             // a pass with processed >= Limit is repeated at once
//	    Sweep     time.Duration   // 0 → PipelineSweep
//	    LockRetry time.Duration   // 0 → LockRetryDelay
//	    Status    StatusPublisher // heartbeat sink; nil → no heartbeat
//	    After     func(time.Duration) <-chan time.Time // 0 → time.After; used for
//	                                                  // the sweep, the lock retry
//	                                                  // AND the heartbeat cadence
//	}
//	func NewStageLoop(cfg StageConfig) *StageLoop
//	// Notify is the coalescing wake: non-blocking, buffer 1, safe from any
//	// goroutine (the MQTT handler calls it).
//	func (l *StageLoop) Notify()
//	// Run starts with one catch-up pass, then waits for a wake, a sweep or a lock
//	// retry. It returns nil (or context.Canceled) when ctx ends, and never returns
//	// because a pass failed.
//	func (l *StageLoop) Run(ctx context.Context) error
//
// IMPOSED BY THESE TESTS, NOT SPELLED BY THE SPEC: the catch-up pass at start
// (a restarted pod must not sit on a backlog for up to one sweep), and the
// heartbeat cadence running on the injected After (so the 60 s cadence is
// testable without waiting 60 s).

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/fleet"
	"github.com/sspataro57/switchboard/internal/pipeline"
)

// grace is how long "nothing else happens" is watched for. The loop runs on
// injected timers, so nothing legitimate is waiting on the wall clock.
const grace = 150 * time.Millisecond

// ---- fake clock -----------------------------------------------------------------

type fakeTimer struct {
	d     time.Duration
	ch    chan time.Time
	fired bool
}

type fakeClock struct {
	mu     sync.Mutex
	timers []*fakeTimer
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ft := &fakeTimer{d: d, ch: make(chan time.Time, 1)}
	c.timers = append(c.timers, ft)
	return ft.ch
}

// fire waits until at least one unfired timer of duration d is pending, then
// fires every pending one of that duration. Abandoned timers (a loop that
// re-arms each wait) fire into buffered channels nobody reads, harmlessly.
func (c *fakeClock) fire(t *testing.T, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		n := 0
		for _, ft := range c.timers {
			if ft.d == d && !ft.fired {
				ft.fired = true
				ft.ch <- time.Now()
				n++
			}
		}
		c.mu.Unlock()
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no timer of %v was ever armed; the loop does not use the injected After for it", d)
		}
		time.Sleep(time.Millisecond)
	}
}

// ---- fake pass -------------------------------------------------------------------

type passResult struct {
	processed int
	err       error
}

// fakePass records calls. When blocking, each call waits for release() (or ctx).
// script[i] is call i+1's result; calls past the script return (0, nil).
type fakePass struct {
	mu       sync.Mutex
	calls    int
	script   []passResult
	blocking bool
	started  chan int
	releaseC chan struct{}
}

func newFakePass(blocking bool, script ...passResult) *fakePass {
	return &fakePass{script: script, blocking: blocking, started: make(chan int, 256), releaseC: make(chan struct{})}
}

func (p *fakePass) Pass(ctx context.Context) (int, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	var r passResult
	if n <= len(p.script) {
		r = p.script[n-1]
	}
	p.mu.Unlock()
	p.started <- n
	if p.blocking {
		select {
		case <-p.releaseC:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return r.processed, r.err
}

func (p *fakePass) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// waitStarted waits for call n to begin.
func (p *fakePass) waitStarted(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case got := <-p.started:
			if got == n {
				return
			}
			if got > n {
				t.Fatalf("pass %d started while waiting for pass %d", got, n)
			}
		case <-deadline:
			t.Fatalf("pass %d never started (calls so far: %d)", n, p.count())
		}
	}
}

func (p *fakePass) release(t *testing.T) {
	t.Helper()
	select {
	case p.releaseC <- struct{}{}:
	case <-time.After(2 * time.Second):
		t.Fatalf("no pass was waiting to be released")
	}
}

// assertNoMorePasses watches for grace and fails if any pass beyond want starts.
func (p *fakePass) assertNoMorePasses(t *testing.T, want int, why string) {
	t.Helper()
	select {
	case n := <-p.started:
		t.Fatalf("pass %d started, want exactly %d: %s", n, want, why)
	case <-time.After(grace):
	}
	if got := p.count(); got != want {
		t.Fatalf("passes = %d, want %d: %s", got, want, why)
	}
}

// ---- fake heartbeat sink ---------------------------------------------------------

type fakeStatus struct {
	mu     sync.Mutex
	states []string
}

func (f *fakeStatus) PublishStatus(s fleet.Status) error {
	if _, err := s.Marshal(); err != nil {
		return fmt.Errorf("fake status: %w", err)
	}
	f.mu.Lock()
	f.states = append(f.states, s.State)
	f.mu.Unlock()
	return nil
}

func (f *fakeStatus) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.states...)
}

func (f *fakeStatus) waitLast(t *testing.T, state string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s := f.snapshot()
		if len(s) > 0 && s[len(s)-1] == state {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("last published state never became %q; published: %v", state, s)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitIdleAfterWorking waits until a pass has been reported (working) and the
// latest state is idle again. A bare waitLast(idle) could match the idle a
// loop publishes at startup, before its catch-up pass says working.
func (f *fakeStatus) waitIdleAfterWorking(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s := f.snapshot()
		sawWorking := false
		for _, x := range s {
			if x == fleet.StateWorking {
				sawWorking = true
			}
		}
		if sawWorking && s[len(s)-1] == fleet.StateIdle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never saw working followed by idle; published: %v", s)
		}
		time.Sleep(time.Millisecond)
	}
}

// ---- harness ---------------------------------------------------------------------

type harness struct {
	loop   *pipeline.StageLoop
	pass   *fakePass
	clock  *fakeClock
	status *fakeStatus
	cancel context.CancelFunc
	done   chan error
}

func startLoop(t *testing.T, p *fakePass, limit int) *harness {
	t.Helper()
	h := &harness{pass: p, clock: &fakeClock{}, status: &fakeStatus{}, done: make(chan error, 1)}
	h.loop = pipeline.NewStageLoop(pipeline.StageConfig{
		Stage:  pipeline.StageGate,
		Pass:   p.Pass,
		Limit:  limit,
		Status: h.status,
		After:  h.clock.After,
	})
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- h.loop.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(2 * time.Second):
			t.Errorf("Run did not return after its context was cancelled")
		}
	})
	return h
}

// stopClean cancels and requires Run to come back without an error of its own.
func (h *harness) stopClean(t *testing.T) {
	t.Helper()
	h.cancel()
	select {
	case err := <-h.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want nil on shutdown", err)
		}
		h.done <- nil // let the cleanup's receive succeed
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after cancel")
	}
}

func (h *harness) assertRunning(t *testing.T) {
	t.Helper()
	select {
	case err := <-h.done:
		t.Fatalf("Run returned (%v) while it should still be looping", err)
	default:
	}
}

// ---- E3 --------------------------------------------------------------------------

// Run begins with one catch-up pass and then waits: no wake, no sweep, no pass.
func TestStageLoop_StartsWithOneCatchUpPassThenWaits(t *testing.T) {
	p := newFakePass(false)
	h := startLoop(t, p, 10)
	p.waitStarted(t, 1)
	p.assertNoMorePasses(t, 1, "with no wake and no sweep the loop must wait")
	h.assertRunning(t)
}

// E3: one wake runs one pass.
func TestStageLoop_OneWakeRunsOnePass(t *testing.T) {
	p := newFakePass(false)
	h := startLoop(t, p, 10)
	p.waitStarted(t, 1)
	p.assertNoMorePasses(t, 1, "settle after the catch-up pass")

	h.loop.Notify()
	p.waitStarted(t, 2)
	p.assertNoMorePasses(t, 2, "one wake must run exactly one pass")
}

// E3: ten wakes during a pass coalesce into ONE follow-up pass (buffer 1).
func TestStageLoop_TenWakesDuringAPassCoalesceToOneFollowUp(t *testing.T) {
	p := newFakePass(true)
	h := startLoop(t, p, 10)
	p.waitStarted(t, 1)
	p.release(t)

	h.loop.Notify()
	p.waitStarted(t, 2)
	for i := 0; i < 10; i++ {
		h.loop.Notify() // must never block, even with the pass still running
	}
	p.release(t)

	p.waitStarted(t, 3)
	p.release(t)
	p.assertNoMorePasses(t, 3, "ten wakes during pass 2 are one pending run, not ten")
}

// Notify never blocks, whatever the loop is doing (the MQTT callback calls it).
func TestStageLoop_NotifyNeverBlocks(t *testing.T) {
	p := newFakePass(true)
	h := startLoop(t, p, 10)
	p.waitStarted(t, 1) // the loop is stuck inside a pass
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			h.loop.Notify()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("Notify blocked while a pass was running")
	}
	p.release(t)
}

// E3: with no wake, the sweep timer runs the pass.
func TestStageLoop_SweepRunsThePassWithoutAWake(t *testing.T) {
	p := newFakePass(false)
	h := startLoop(t, p, 10)
	p.waitStarted(t, 1)
	p.assertNoMorePasses(t, 1, "before the sweep fires")

	h.clock.fire(t, pipeline.PipelineSweep)
	p.waitStarted(t, 2)
	p.assertNoMorePasses(t, 2, "one sweep, one pass")
}

// The sweep keeps firing: a lost wake costs at most one sweep, every sweep.
func TestStageLoop_SweepIsRecurring(t *testing.T) {
	p := newFakePass(false)
	h := startLoop(t, p, 10)
	p.waitStarted(t, 1)
	for n := 2; n <= 4; n++ {
		h.clock.fire(t, pipeline.PipelineSweep)
		p.waitStarted(t, n)
	}
	p.assertNoMorePasses(t, 4, "three sweeps, three passes after the catch-up")
}

// E3: a lost advisory lock is not an error. The loop does not exit, does not
// spin, and retries when LockRetryDelay elapses, with no wake needed.
func TestStageLoop_LostLockRetriesWithoutError(t *testing.T) {
	p := newFakePass(false,
		passResult{}, // 1: catch-up
		passResult{err: fmt.Errorf("gate: %w", pipeline.ErrLockHeld)}, // 2: lost the lock
	)
	h := startLoop(t, p, 10)
	p.waitStarted(t, 1)
	p.assertNoMorePasses(t, 1, "settle")

	h.loop.Notify()
	p.waitStarted(t, 2)
	p.assertNoMorePasses(t, 2, "a lost lock must not be retried in a hot loop")
	h.assertRunning(t)

	h.clock.fire(t, pipeline.LockRetryDelay)
	p.waitStarted(t, 3)
	h.assertRunning(t)
	h.stopClean(t)
}

// E-D4: a stage that errors logs, stays up and retries on the next wake. It
// never crash-loops on a DB error.
func TestStageLoop_PassErrorKeepsTheLoopUp(t *testing.T) {
	p := newFakePass(false,
		passResult{err: errors.New("db: connection refused")}, // the catch-up fails
	)
	h := startLoop(t, p, 10)
	p.waitStarted(t, 1)
	p.assertNoMorePasses(t, 1, "an error is not a reason to spin")
	h.assertRunning(t)

	h.loop.Notify()
	p.waitStarted(t, 2)
	h.assertRunning(t)
	h.stopClean(t)
}

// E-D4: a pass that processed its full --limit is repeated until one comes back
// short: the inbox drains without waiting for more wakes or sweeps.
func TestStageLoop_FullLimitPassRepeatsUntilDrained(t *testing.T) {
	p := newFakePass(false,
		passResult{processed: 0},  // 1: catch-up, empty
		passResult{processed: 10}, // 2: full
		passResult{processed: 10}, // 3: full
		passResult{processed: 3},  // 4: short, drained
	)
	h := startLoop(t, p, 10)
	p.waitStarted(t, 1)
	p.assertNoMorePasses(t, 1, "settle")

	h.loop.Notify()
	p.waitStarted(t, 2)
	p.waitStarted(t, 3)
	p.waitStarted(t, 4)
	p.assertNoMorePasses(t, 4, "a short pass means the inbox is drained")
}

// ---- E4 unit half ------------------------------------------------------------------

// Working during a pass, idle otherwise.
func TestStageLoop_HeartbeatWorkingDuringPassIdleOtherwise(t *testing.T) {
	p := newFakePass(true)
	h := startLoop(t, p, 10)

	p.waitStarted(t, 1)
	h.status.waitLast(t, fleet.StateWorking)
	p.release(t)
	h.status.waitLast(t, fleet.StateIdle)

	h.loop.Notify()
	p.waitStarted(t, 2)
	h.status.waitLast(t, fleet.StateWorking)
	p.release(t)
	h.status.waitLast(t, fleet.StateIdle)

	for _, s := range h.status.snapshot() {
		if s != fleet.StateIdle && s != fleet.StateWorking {
			t.Errorf("stage published state %q; a stage is only ever idle or working (LWT dead is the broker's)", s)
		}
	}
}

// The status is republished every fleet.HeartbeatInterval while idle...
func TestStageLoop_HeartbeatRepublishesIdleOnCadence(t *testing.T) {
	p := newFakePass(false)
	h := startLoop(t, p, 10)
	p.waitStarted(t, 1)
	h.status.waitIdleAfterWorking(t) // the catch-up pass has come and gone
	before := len(h.status.snapshot())

	h.clock.fire(t, fleet.HeartbeatInterval)
	deadline := time.Now().Add(2 * time.Second)
	for len(h.status.snapshot()) <= before {
		if time.Now().After(deadline) {
			t.Fatalf("no heartbeat after fleet.HeartbeatInterval elapsed (still %d publishes)", before)
		}
		time.Sleep(time.Millisecond)
	}
	s := h.status.snapshot()
	if s[len(s)-1] != fleet.StateIdle {
		t.Errorf("idle heartbeat republished as %q", s[len(s)-1])
	}
	if p.count() != 1 {
		t.Errorf("a heartbeat tick ran a pass (passes=%d); the heartbeat is not the sweep", p.count())
	}
}

// ...and while a long pass runs (the z4 answers at ~4.5 s/verdict, so an
// inquiry pass outlives several heartbeats and must not look dead or idle).
func TestStageLoop_HeartbeatRepublishesWorkingDuringALongPass(t *testing.T) {
	p := newFakePass(true)
	h := startLoop(t, p, 10)
	p.waitStarted(t, 1)
	h.status.waitLast(t, fleet.StateWorking)
	before := len(h.status.snapshot())

	h.clock.fire(t, fleet.HeartbeatInterval)
	deadline := time.Now().Add(2 * time.Second)
	for len(h.status.snapshot()) <= before {
		if time.Now().After(deadline) {
			t.Fatalf("no heartbeat during a pass after fleet.HeartbeatInterval elapsed")
		}
		time.Sleep(time.Millisecond)
	}
	s := h.status.snapshot()
	if s[len(s)-1] != fleet.StateWorking {
		t.Errorf("heartbeat during a pass published %q, want working", s[len(s)-1])
	}
	p.release(t)
}
