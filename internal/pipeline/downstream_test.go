package pipeline_test

// SWT-40 Part C, C-D1 and C13, plus the `gated` publish Part D deferred to its
// only consumer (docs/tickets/inquiry-promote_SPEC.md, E-D3 "published by",
// "MQTT topics"). Pure: no broker, no db.
//
// THE DESIGN THESE TESTS IMPOSE. The stage loop (stage.go) has no hook for a
// downstream wake, and it must not grow one: the loop core is "wake, drain,
// sweep, heartbeat", and a publish inside it would couple every stage to the
// broker's health. So the publish is a WRAPPER around the PassFunc, applied by
// cmd/pipelined when it builds each stage:
//
//	// Downstream is E-D3's "published by" column as a pure table: the wake a
//	// stage publishes after a pass that moved rows out of its inbox.
//	//   gate -> gated, route -> route_classified, route_apply -> routed,
//	//   inquiry -> inquiry_classified, inquiry_promote -> promoted
//	func Downstream(stage Stage) (event string, ok bool)
//
//	// PublishAfterPass wraps pass: when it reports processed > 0, ONE wake for
//	// Downstream(stage) is published through pub (PublishWake: QoS 1, NEVER
//	// retained; Source = the stage name; Counts {"processed": n}). The pass's
//	// (n, err) is returned UNCHANGED: a publish failure is logged, never
//	// returned (E-D3, "a publish never fails its stage"), and a pass that
//	// committed rows and then errored still announces them (the rows are in the
//	// downstream inbox either way; the wake is only latency). nil pub → no publish.
//	func PublishAfterPass(stage Stage, pub Publisher, pass PassFunc) PassFunc
//
// GREENFIELD NOTE — EXPECTED RED: pipeline.Downstream and
// pipeline.PublishAfterPass do not exist, so package pipeline_test
// compile-FAILS ("undefined: pipeline.Downstream").
//
// Reuses fakePublisher/pubCall from contract_test.go (same test package).

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/pipeline"
)

func TestDownstream_EveryStageEmitsItsEDThreeEvent(t *testing.T) {
	want := map[pipeline.Stage]string{
		pipeline.StageGate:           pipeline.EventGated,
		pipeline.StageRoute:          pipeline.EventRouteClassified,
		pipeline.StageRouteApply:     pipeline.EventRouted,
		pipeline.StageInquiry:        pipeline.EventInquiryClassified,
		pipeline.StageInquiryPromote: pipeline.EventPromoted,
	}
	for _, s := range pipeline.Stages() {
		got, ok := pipeline.Downstream(s)
		w, known := want[s]
		if !known {
			t.Fatalf("stage %q is not in this test's table; E-D3 names a publisher for every consumer", s)
		}
		if !ok || got != w {
			t.Errorf("Downstream(%q) = (%q, %v), want (%q, true) — E-D3's 'published by' column", s, got, ok, w)
		}
	}
	if e, ok := pipeline.Downstream("bogus"); ok || e != "" {
		t.Errorf("Downstream(bogus) = (%q, %v), want (\"\", false)", e, ok)
	}
}

// The two halves of the graph must agree: what a stage publishes is what its
// successor subscribes to. `gated` wakes the inquiry stage (a gate outcome can
// attribute), `inquiry_classified` wakes inquiry_promote, and `promoted` wakes
// nobody (the task boundary is the orchestrator's, E-D2).
func TestDownstream_FeedsTheSubscribersTable(t *testing.T) {
	next := map[pipeline.Stage][]pipeline.Stage{
		pipeline.StageGate:           {pipeline.StageInquiry},
		pipeline.StageRoute:          {pipeline.StageRouteApply},
		pipeline.StageRouteApply:     {pipeline.StageInquiry},
		pipeline.StageInquiry:        {pipeline.StageInquiryPromote},
		pipeline.StageInquiryPromote: nil,
	}
	for s, want := range next {
		e, ok := pipeline.Downstream(s)
		if !ok {
			t.Errorf("Downstream(%q) has no event", s)
			continue
		}
		got := pipeline.Subscribers(e)
		if len(got) != len(want) {
			t.Errorf("Downstream(%q) = %q wakes %v, want %v", s, e, got, want)
			continue
		}
		for _, w := range want {
			if !containsStage(got, w) {
				t.Errorf("Downstream(%q) = %q does not wake %q", s, e, w)
			}
		}
	}
}

func TestPublishAfterPass_PublishesOneNonRetainedWakeWhenRowsMoved(t *testing.T) {
	for _, s := range pipeline.Stages() {
		fp := &fakePublisher{}
		pass := pipeline.PublishAfterPass(s, fp, func(context.Context) (int, error) { return 3, nil })
		n, err := pass(context.Background())
		if n != 3 || err != nil {
			t.Errorf("stage %q: wrapped pass returned (%d, %v), want the pass's own (3, nil)", s, n, err)
		}
		if len(fp.calls) != 1 {
			t.Fatalf("stage %q: %d publishes after a pass that processed 3, want exactly 1", s, len(fp.calls))
		}
		c := fp.calls[0]
		event, _ := pipeline.Downstream(s)
		if c.topic != pipeline.Topic(event) {
			t.Errorf("stage %q published on %q, want %q", s, c.topic, pipeline.Topic(event))
		}
		if c.qos != 1 {
			t.Errorf("stage %q published QoS %d, want 1", s, c.qos)
		}
		if c.retained {
			t.Errorf("stage %q published its %q wake RETAINED. ops/pipeline/* is never retained: a retained "+
				"wake re-fires on every reconnect, and retained state is global on the production broker (IK)", s, event)
		}
		w, err := pipeline.ParseWake(c.payload)
		if err != nil {
			t.Fatalf("payload %s: %v", c.payload, err)
		}
		if w.Event != event || w.Source != string(s) {
			t.Errorf("stage %q wake = {event:%q source:%q}, want {event:%q source:%q}", s, w.Event, w.Source, event, s)
		}
		if w.Counts["processed"] != 3 {
			t.Errorf("stage %q wake counts = %v, want processed=3 (diagnostic only; consumers never branch on it)", s, w.Counts)
		}
	}
}

func TestPublishAfterPass_NoWakeWhenNothingMoved(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
		err  error
	}{
		{"empty inbox", 0, nil},
		{"lock held elsewhere", 0, pipeline.ErrLockHeld},
		{"failed before any row", 0, errors.New("db down")},
	} {
		fp := &fakePublisher{}
		pass := pipeline.PublishAfterPass(pipeline.StageGate, fp, func(context.Context) (int, error) { return tc.n, tc.err })
		n, err := pass(context.Background())
		if n != tc.n || !errors.Is(err, tc.err) {
			t.Errorf("%s: wrapped pass returned (%d, %v), want the pass's own (%d, %v) — the loop needs "+
				"errors.Is(err, ErrLockHeld) to reach it unchanged", tc.name, n, err, tc.n, tc.err)
		}
		if len(fp.calls) != 0 {
			t.Errorf("%s: %d wake(s) published after a pass that moved nothing — the gate stage publishes "+
				"`gated` only after a pass that resolved >= 1 hold (E-D3)", tc.name, len(fp.calls))
		}
	}
}

// Rows that left the inbox before a later error ARE in the downstream inbox;
// announcing them is correct (a wake is latency only) and returning the error
// unchanged keeps the loop's error handling intact.
func TestPublishAfterPass_PartialProgressStillAnnouncesAndKeepsTheError(t *testing.T) {
	boom := errors.New("third row failed")
	fp := &fakePublisher{}
	pass := pipeline.PublishAfterPass(pipeline.StageInquiry, fp, func(context.Context) (int, error) { return 2, boom })
	n, err := pass(context.Background())
	if n != 2 || !errors.Is(err, boom) {
		t.Errorf("wrapped pass returned (%d, %v), want (2, %v)", n, err, boom)
	}
	if len(fp.calls) != 1 {
		t.Errorf("%d wakes after a pass that committed 2 rows then failed, want 1", len(fp.calls))
	}
}

// "A publish never fails its stage" (E-D3): a broker error is logged, and the
// pass's own result goes back to the loop untouched.
func TestPublishAfterPass_BrokerErrorNeverFailsTheStage(t *testing.T) {
	fp := &fakePublisher{err: errors.New("broker gone")}
	pass := pipeline.PublishAfterPass(pipeline.StageInquiryPromote, fp, func(context.Context) (int, error) { return 1, nil })
	n, err := pass(context.Background())
	if n != 1 || err != nil {
		t.Errorf("wrapped pass returned (%d, %v) when only the PUBLISH failed, want (1, nil). A broker outage "+
			"must never turn a committed pass into a logged failure; the sweep covers the lost wake", n, err)
	}
}

func TestPublishAfterPass_NilPublisherIsSafe(t *testing.T) {
	pass := pipeline.PublishAfterPass(pipeline.StageGate, nil, func(context.Context) (int, error) { return 5, nil })
	if n, err := pass(context.Background()); n != 5 || err != nil {
		t.Errorf("nil publisher: (%d, %v), want (5, nil)", n, err)
	}
}

// "Never inside the loop core". stage.go publishes heartbeats only
// (PublishStatus); a wake publish there would make every stage's drain depend
// on the broker. Scanned lexically, with a positive control on the heartbeat.
func TestStageLoopCore_NeverPublishesAWake(t *testing.T) {
	b, err := os.ReadFile("stage.go")
	if err != nil {
		t.Fatalf("read stage.go: %v", err)
	}
	src := string(b)
	if !strings.Contains(src, "PublishStatus(") {
		t.Fatalf("POSITIVE CONTROL FAILED: stage.go no longer publishes heartbeats; the scan below is blind")
	}
	for i, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		if regexp.MustCompile(`PublishWake\(|\.Publish\(|PublishAfterPass\(`).MatchString(line) {
			t.Errorf("stage.go line %d publishes a wake inside the loop core: %s — the downstream publish is the "+
				"PassFunc wrapper's job (PublishAfterPass), applied by cmd/pipelined", i+1, strings.TrimSpace(line))
		}
	}
}
