package main

// SWT-40 Part B wiring (docs/tickets/inquiry-promote_SPEC.md, B-D6, B-D8, B11,
// E-D3, E-D4): the `route` and `route_apply` stages are registered in
// stageImpls, woken by their E-D3 upstream events, publish their downstream
// wake ONLY through pipeline.PublishAfterPass (buildPass), serialize on the
// right advisory lock, and cannot reach a non-local provider. Pure: no broker,
// no db — the stage functions are injected, and the rest is a source scan.
//
// ---- IMPOSED SURFACE (cmd/pipelined/route.go) --------------------------------
//
//	stageImpls[pipeline.StageRoute]      = {limit: routeStageLimit,      pass: routePass}
//	stageImpls[pipeline.StageRouteApply] = {limit: routeApplyStageLimit, pass: routeApplyPass}
//
//	routePass (GPU): classify.Run with classify.LaneRoute and a Since > 0, under
//	  the shared classify lock 0x5157_0022 (classify store TryLock; lost →
//	  pipeline.ErrLockHeld), with localRouter() — general = nil, ALWAYS (B-D8).
//	routeApplyPass: capture.RunRouteApply under capture's lock 0x5157_0015
//	  (capture.ErrRouteLockHeld → pipeline.ErrLockHeld). No executor: B5, no
//	  tool call.
//
// Downstream wakes come from buildPass: route → route_classified, route_apply →
// routed. route.go publishes nothing itself.
//
// GREENFIELD NOTE — EXPECTED RED: route.go and the two stageImpls entries do not
// exist. TestBuildPass_RouteStagesPublishTheirDownstreamWakes is GREEN today
// (it injects fake passes, and the downstream table already names both stages);
// it is the guard that the two stages keep going through buildPass.

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/pipeline"
)

func TestStageImpls_RegistersTheRouteStages(t *testing.T) {
	for stage, woken := range map[pipeline.Stage]string{
		pipeline.StageRoute:      pipeline.EventCaptured,
		pipeline.StageRouteApply: pipeline.EventRouteClassified,
	} {
		impl, ok := stageImpls[stage]
		if !ok {
			t.Errorf("stageImpls has no %q entry: B11 runs it as a pipelined stage. Registered: %v", stage, registered())
			continue
		}
		if impl.pass == nil || impl.limit <= 0 {
			t.Errorf("stage %q: pass=%v limit=%d, want a pass constructor and a positive --limit", stage, impl.pass != nil, impl.limit)
		}
		found := false
		for _, e := range pipeline.Upstream(stage) {
			if e == woken {
				found = true
			}
		}
		if !found {
			t.Errorf("stage %q is not woken by %q (E-D3); upstream = %v", stage, woken, pipeline.Upstream(stage))
		}
	}
}

func TestParseStages_AcceptsEveryStage(t *testing.T) {
	got, err := parseStages("gate,route,route_apply,inquiry,inquiry_promote")
	if err != nil {
		t.Fatalf(`parseStages(every stage): %v — V6.5 enables route + route_apply beside the others`, err)
	}
	if len(got) != 5 || got[1] != pipeline.StageRoute || got[2] != pipeline.StageRouteApply {
		t.Errorf("parseStages = %v", got)
	}
}

func TestBuildPass_RouteStagesPublishTheirDownstreamWakes(t *testing.T) {
	orig := stageImpls
	t.Cleanup(func() { stageImpls = orig })
	processed := 0
	fake := stageImpl{limit: 10, pass: func(*pgxpool.Pool) pipeline.PassFunc {
		return func(context.Context) (int, error) { return processed, nil }
	}}
	stageImpls = map[pipeline.Stage]stageImpl{pipeline.StageRoute: fake, pipeline.StageRouteApply: fake}
	for stage, event := range map[pipeline.Stage]string{
		pipeline.StageRoute:      pipeline.EventRouteClassified,
		pipeline.StageRouteApply: pipeline.EventRouted,
	} {
		pub := &pdPub{}
		pass := buildPass(stage, nil, pub)
		processed = 0
		if _, err := pass(context.Background()); err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		if len(pub.calls) != 0 {
			t.Errorf("stage %q published after a pass that moved nothing", stage)
		}
		processed = 3
		if n, err := pass(context.Background()); n != 3 || err != nil {
			t.Errorf("stage %q: buildPass changed the result to (%d, %v)", stage, n, err)
		}
		if len(pub.calls) != 1 {
			t.Fatalf("stage %q: %d publishes after a pass that moved rows, want 1", stage, len(pub.calls))
		}
		if c := pub.calls[0]; c.topic != pipeline.Topic(event) || c.qos != 1 || c.retained {
			t.Errorf("stage %q published {%s qos %d retained %v}, want {%s qos 1 NOT retained}", stage,
				c.topic, c.qos, c.retained, pipeline.Topic(event))
		}
	}
}

func TestRouteStages_Source(t *testing.T) {
	b, err := os.ReadFile("route.go")
	if err != nil {
		t.Fatalf("read cmd/pipelined/route.go: %v (B11: the two route stages live there)", err)
	}
	src := string(b)
	for _, want := range []struct{ token, why string }{
		{"classify.LaneRoute", "the route stage runs the route lane (B-D3)"},
		{"localRouter()", "B-D8: the lane is local-only — the same general=nil router the inquiry stage uses"},
		{"TryLock(", "E-D4: the GPU stage takes the shared classify lock 0x5157_0022 per pass"},
		{"capture.RunRouteApply(", "B-D6: route_apply is capture's driver"},
		{"capture.ErrRouteLockHeld", "a lost capture lock maps to a retry, never a failure"},
		{"pipeline.ErrLockHeld", "both stages map a lost lock to pipeline.ErrLockHeld (retry after 30 s, then the sweep)"},
	} {
		if !strings.Contains(src, want.token) {
			t.Errorf("route.go does not contain %q — %s", want.token, want.why)
		}
	}
	for _, bad := range []struct{ token, why string }{
		{"executor.", "B5: route application makes no executor call; the route lane writes verdicts, not tasks"},
		{"tools.Register", "no tool registry: nothing here writes a task"},
		{".Publish(", "downstream wakes are published ONLY by pipeline.PublishAfterPass, applied in buildPass"},
		{"PublishWake(", "same: never publish from the stage body"},
		{"pipeline.Topic(", "same"},
		{"NewOpenAI", "B-D8: no hosted client, ever"},
		{"OPENAI_API_KEY", "B-D8: no hosted credential is read"},
		{"deliver", "invariant 4: nothing here reaches a delivery"},
	} {
		if strings.Contains(src, bad.token) {
			t.Errorf("route.go contains %q — %s", bad.token, bad.why)
		}
	}
}

// B-D8, structural: no router this daemon builds can reach a non-local provider.
// Every provider.NewRouter call passes nil as the general client, and nothing
// in cmd/pipelined constructs a hosted client.
func TestPipelined_EveryRouterIsLocalOnly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	call := regexp.MustCompile(`provider\.NewRouter\(\s*([^,\s]+)\s*,`)
	routers := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := string(b)
		for _, m := range call.FindAllStringSubmatch(src, -1) {
			routers++
			if m[1] != "nil" {
				t.Errorf("%s builds a router with general client %q; B-D8: the route lane is local-only (general = nil), "+
					"and so is every lane this daemon runs", f, m[1])
			}
		}
		for _, bad := range []string{"NewOpenAI", "OPENAI_API_KEY", "api.openai.com"} {
			if strings.Contains(src, bad) {
				t.Errorf("%s mentions %q; pipelined carries no hosted client", f, bad)
			}
		}
	}
	if routers == 0 {
		t.Fatalf("POSITIVE CONTROL FAILED: no provider.NewRouter call found in cmd/pipelined; the scan is blind")
	}
}
