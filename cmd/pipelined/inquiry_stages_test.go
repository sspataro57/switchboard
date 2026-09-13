package main

// SWT-40 Part C wiring (docs/tickets/inquiry-promote_SPEC.md, C-D1, C-D6, C13,
// plus the `gated` publish Part D deferred): the `inquiry` and
// `inquiry_promote` stages are registered in stageImpls, and EVERY stage's pass
// is built through buildPass, which wraps it with pipeline.PublishAfterPass so
// a pass that moved rows publishes its downstream wake (gate → gated,
// inquiry → inquiry_classified, inquiry_promote → promoted). Pure: no broker,
// no db.
//
// ---- IMPOSED SURFACE (cmd/pipelined) -----------------------------------------
//
//	// inquiryStageSince is the inquiry stage's classify --since (C-D1: 72h).
//	// C-D6: it is >= promote.InquiryMaxAge, or a verdict the promoter could
//	// still act on would never be produced.
//	const inquiryStageSince = 72 * time.Hour
//
//	// buildPass is the ONE place a stage's PassFunc is built: impl.pass(pool)
//	// wrapped with pipeline.PublishAfterPass(s, pub, …). run() calls it with the
//	// stage's own client as pub.
//	func buildPass(s pipeline.Stage, pool *pgxpool.Pool, pub pipeline.Publisher) pipeline.PassFunc
//
// GREENFIELD NOTE — EXPECTED RED: inquiryStageSince, buildPass and the two
// stageImpls entries do not exist, so cmd/pipelined's test build compile-FAILS
// ("undefined: buildPass").

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/pipeline"
	"github.com/sspataro57/switchboard/internal/promote"
)

func TestStageImpls_RegistersTheInquiryStages(t *testing.T) {
	for stage, woken := range map[pipeline.Stage][]string{
		pipeline.StageInquiry:        {pipeline.EventCaptured, pipeline.EventGated, pipeline.EventRouted},
		pipeline.StageInquiryPromote: {pipeline.EventInquiryClassified},
	} {
		impl, ok := stageImpls[stage]
		if !ok {
			t.Errorf("stageImpls has no %q entry: C-D1 runs it as a pipelined stage. Registered: %v", stage, registered())
			continue
		}
		if impl.pass == nil || impl.limit <= 0 {
			t.Errorf("stage %q: pass=%v limit=%d, want a pass constructor and a positive --limit", stage, impl.pass != nil, impl.limit)
		}
		up := pipeline.Upstream(stage)
		for _, e := range woken {
			found := false
			for _, u := range up {
				if u == e {
					found = true
				}
			}
			if !found {
				t.Errorf("stage %q is not woken by %q (C-D1); upstream = %v", stage, e, up)
			}
		}
	}
}

func TestParseStages_AcceptsTheInquiryStages(t *testing.T) {
	got, err := parseStages("gate,inquiry,inquiry_promote")
	if err != nil {
		t.Fatalf(`parseStages("gate,inquiry,inquiry_promote"): %v — the V6.4 go-live value`, err)
	}
	if len(got) != 3 || got[1] != pipeline.StageInquiry || got[2] != pipeline.StageInquiryPromote {
		t.Errorf("parseStages = %v", got)
	}
}

func TestInquiryStageSince_CoversInquiryMaxAge(t *testing.T) {
	if inquiryStageSince < promote.InquiryMaxAge {
		t.Errorf("inquiryStageSince = %v < promote.InquiryMaxAge (%v). C-D6: the inquiry stage's --since is >= "+
			"InquiryMaxAge, or asks the promoter may still act on never get a verdict", inquiryStageSince, promote.InquiryMaxAge)
	}
}

type pdPub struct {
	calls []pdCall
}

type pdCall struct {
	topic    string
	qos      byte
	retained bool
	payload  []byte
}

func (p *pdPub) Publish(topic string, qos byte, retained bool, payload []byte) error {
	p.calls = append(p.calls, pdCall{topic, qos, retained, append([]byte(nil), payload...)})
	return nil
}

// Every stage's pass is built through buildPass, so the gate publishes `gated`
// after a pass that resolved >= 1 hold, the inquiry stage `inquiry_classified`
// after >= 1 verdict, and inquiry_promote `promoted` after >= 1 task created or
// attached. Never retained. Nothing after a pass that moved nothing.
func TestBuildPass_PublishesEachStagesDownstreamWake(t *testing.T) {
	orig := stageImpls
	t.Cleanup(func() { stageImpls = orig })
	processed := 0
	fake := stageImpl{limit: 10, pass: func(*pgxpool.Pool) pipeline.PassFunc {
		return func(context.Context) (int, error) { return processed, nil }
	}}
	stageImpls = map[pipeline.Stage]stageImpl{
		pipeline.StageGate: fake, pipeline.StageInquiry: fake, pipeline.StageInquiryPromote: fake,
	}
	for stage, event := range map[pipeline.Stage]string{
		pipeline.StageGate:           pipeline.EventGated,
		pipeline.StageInquiry:        pipeline.EventInquiryClassified,
		pipeline.StageInquiryPromote: pipeline.EventPromoted,
	} {
		pub := &pdPub{}
		pass := buildPass(stage, nil, pub)

		processed = 0
		if _, err := pass(context.Background()); err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		if len(pub.calls) != 0 {
			t.Errorf("stage %q published %d wake(s) after a pass that moved nothing", stage, len(pub.calls))
		}

		processed = 2
		if n, err := pass(context.Background()); n != 2 || err != nil {
			t.Errorf("stage %q: buildPass changed the pass's result to (%d, %v)", stage, n, err)
		}
		if len(pub.calls) != 1 {
			t.Fatalf("stage %q: %d publishes after a pass that moved 2 rows, want 1", stage, len(pub.calls))
		}
		c := pub.calls[0]
		if c.topic != pipeline.Topic(event) || c.qos != 1 || c.retained {
			t.Errorf("stage %q published {%s qos %d retained %v}, want {%s qos 1 NOT retained}", stage,
				c.topic, c.qos, c.retained, pipeline.Topic(event))
		}
	}
}

// run() must build every stage through buildPass — the loop core never
// publishes, and a stage built from impl.pass(pool) directly would publish
// nothing downstream (the `gated` wake Part D deferred would stay missing).
func TestRun_BuildsEveryStagePassThroughBuildPass(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := string(b)
	if !strings.Contains(src, "pipeline.NewStageLoop(") {
		t.Fatalf("POSITIVE CONTROL FAILED: main.go no longer builds stage loops")
	}
	if !strings.Contains(src, "buildPass(s, pool, client)") {
		t.Errorf("main.go does not build stage passes with buildPass(s, pool, client) (the stage's own client is the publisher)")
	}
	if strings.Contains(src, "Pass: impl.pass(pool)") {
		t.Errorf("main.go still hands NewStageLoop impl.pass(pool) directly; downstream wakes would never publish")
	}
}
