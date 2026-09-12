package pipeline_test

// SWT-40 Part E, review fixes (go-reviewer + Codex, 2026-09-12): a hung pass is
// cancelled and a wedged one ends Run; a lost lock retries ONCE, then waits for
// the sweep (E-D4); consecutive full passes are capped; working is published
// once per drain; announce client ids are unique per connection. Same fake
// clock and fake pass as stage_test.go.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/fleet"
	"github.com/sspataro57/switchboard/internal/pipeline"
)

// pending counts armed, unfired timers of duration d.
func (c *fakeClock) pending(d time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, ft := range c.timers {
		if ft.d == d && !ft.fired {
			n++
		}
	}
	return n
}

// The loop timers must not share a duration: the fake clock fires by
// duration, and two timers on one duration would fire together in a test.
func TestLoopTimers_DistinctDurations(t *testing.T) {
	seen := map[time.Duration]string{}
	for name, d := range map[string]time.Duration{
		"PipelineSweep": pipeline.PipelineSweep, "LockRetryDelay": pipeline.LockRetryDelay,
		"PassTimeout": pipeline.PassTimeout, "PassWedgeGrace": pipeline.PassWedgeGrace,
		"fleet.HeartbeatInterval": fleet.HeartbeatInterval,
	} {
		if prev, dup := seen[d]; dup {
			t.Errorf("%s and %s are both %v", prev, name, d)
		}
		seen[d] = name
	}
}

// A pass that hangs past PassTimeout is cancelled; the loop carries on.
func TestStageLoop_PassTimeoutCancelsAHungPass(t *testing.T) {
	p := newFakePass(true) // blocks until released or its ctx ends
	h := startLoop(t, p, 10)
	p.waitStarted(t, 1)

	h.clock.fire(t, pipeline.PassTimeout)
	p.assertNoMorePasses(t, 1, "a cancelled pass is an error, not a reason to run again")
	h.assertRunning(t)

	h.loop.Notify()
	p.waitStarted(t, 2)
	p.release(t)
	h.stopClean(t)
}

// A pass that ignores its cancellation ends Run with ErrPassWedged: another
// pass could not start without two running at once, so the pod restarts.
func TestStageLoop_WedgedPassEndsRun(t *testing.T) {
	clock := &fakeClock{}
	block := make(chan struct{})
	defer close(block)
	started := make(chan struct{}, 1)
	// A heartbeat sink, as in production: without one the status goroutine
	// is never started, and a Run that waits on it cannot be caught
	// deadlocking on the wedge path (go-reviewer, 2026-09-12).
	loop := pipeline.NewStageLoop(pipeline.StageConfig{
		Stage: pipeline.StageGate, Limit: 10, After: clock.After, Status: &fakeStatus{},
		Pass: func(context.Context) (int, error) {
			started <- struct{}{}
			<-block // ignores ctx on purpose
			return 0, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the catch-up pass never started")
	}

	clock.fire(t, pipeline.PassTimeout)
	select {
	case err := <-done:
		t.Fatalf("Run returned (%v) before the wedge grace elapsed", err)
	case <-time.After(grace):
	}
	clock.fire(t, pipeline.PassWedgeGrace)
	select {
	case err := <-done:
		if !errors.Is(err, pipeline.ErrPassWedged) {
			t.Fatalf("Run returned %v, want ErrPassWedged", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not give up on a pass that ignored its cancellation")
	}
}

// E-D4: a lost lock retries once after LockRetryDelay; a retry that loses
// again waits for the sweep, it does not re-arm the 30 s retry.
func TestStageLoop_LockRetryOnceThenTheSweep(t *testing.T) {
	p := newFakePass(false,
		passResult{}, // 1: catch-up
		passResult{err: pipeline.ErrLockHeld},
		passResult{err: pipeline.ErrLockHeld},
	)
	h := startLoop(t, p, 10)
	p.waitStarted(t, 1)
	p.assertNoMorePasses(t, 1, "settle")

	h.loop.Notify()
	p.waitStarted(t, 2)
	h.clock.fire(t, pipeline.LockRetryDelay)
	p.waitStarted(t, 3)
	p.assertNoMorePasses(t, 3, "the retry lost the lock again")
	if n := h.clock.pending(pipeline.LockRetryDelay); n != 0 {
		t.Fatalf("a retry that lost the lock armed another retry (%d pending); the next attempt is the sweep", n)
	}
	h.clock.fire(t, pipeline.PipelineSweep)
	p.waitStarted(t, 4)
}

// An inbox that never drains costs MaxDrainPasses per wake or sweep, not a
// hot loop.
func TestStageLoop_FullPassesAreCapped(t *testing.T) {
	script := make([]passResult, 3*pipeline.MaxDrainPasses)
	for i := range script {
		script[i] = passResult{processed: 10}
	}
	p := newFakePass(false, script...)
	h := startLoop(t, p, 10)
	p.waitStarted(t, pipeline.MaxDrainPasses)
	p.assertNoMorePasses(t, pipeline.MaxDrainPasses, "the drain must stop at the cap")

	h.clock.fire(t, pipeline.PipelineSweep)
	p.waitStarted(t, pipeline.MaxDrainPasses+1)
}

// working is published once per drain, not once per repeated pass.
func TestStageLoop_WorkingOncePerDrain(t *testing.T) {
	p := newFakePass(false,
		passResult{},              // drain 1: catch-up
		passResult{processed: 10}, // drain 2 …
		passResult{processed: 10},
		passResult{processed: 3}, // … drained
	)
	h := startLoop(t, p, 10)
	p.waitStarted(t, 1)
	h.status.waitIdleAfterWorking(t)
	h.loop.Notify()
	p.waitStarted(t, 4)
	p.assertNoMorePasses(t, 4, "drained")
	h.status.waitLast(t, fleet.StateIdle)

	working := 0
	for _, s := range h.status.snapshot() {
		if s == fleet.StateWorking {
			working++
		}
	}
	if working != 2 {
		t.Errorf("working published %d times over two drains, want 2 (once per drain)", working)
	}
}

// Two announces of one connector never share a client id (the google CronJob
// and its IDLE watcher can overlap; a shared id kicks one off the broker).
func TestAnnounceClientID_UniquePerConnection(t *testing.T) {
	a, b := pipeline.AnnounceClientID("google"), pipeline.AnnounceClientID("google")
	if !strings.HasPrefix(a, pipeline.CaptureClientID("google")+"-") {
		t.Errorf("AnnounceClientID = %q, want prefix %q", a, pipeline.CaptureClientID("google")+"-")
	}
	if a == b {
		t.Errorf("two announce client ids are equal (%q)", a)
	}
}
