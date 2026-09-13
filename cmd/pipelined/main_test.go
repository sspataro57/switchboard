package main

// SWT-40 Part D wiring (docs/tickets/inquiry-promote_SPEC.md, D-D4, E-D4): the
// gate runs as the `gate` stage in pipelined — registered in stageImpls, woken
// by `captured`, and enabled by naming it in PIPELINE_STAGES. Pure: no broker,
// no db.
//
// GREENFIELD NOTE — EXPECTED RED: stageImpls is empty on this branch (Part E
// shipped the host with no stage), so the gate is not registered and
// parseStages("gate") refuses it as "not implemented in this build".

import (
	"testing"

	"github.com/sspataro57/switchboard/internal/pipeline"
)

func TestStageImpls_RegistersTheGateStageWokenByCaptured(t *testing.T) {
	impl, ok := stageImpls[pipeline.StageGate]
	if !ok {
		t.Fatalf("stageImpls has no %q entry: Part D's gate runs as a pipelined stage (D-D4). Registered stages: %v",
			pipeline.StageGate, registered())
	}
	if impl.pass == nil {
		t.Errorf("the gate stage has a nil pass constructor")
	}
	if impl.limit <= 0 {
		t.Errorf("the gate stage's limit = %d, want > 0: a pass is --limit-bounded and repeats while it fills "+
			"its limit (E-D4)", impl.limit)
	}
	woken := false
	for _, e := range pipeline.Upstream(pipeline.StageGate) {
		if e == pipeline.EventCaptured {
			woken = true
		}
	}
	if !woken {
		t.Errorf("the gate stage is not woken by %q (E-D3's table: captured → D gate)", pipeline.EventCaptured)
	}
}

func TestParseStages_AcceptsTheGate(t *testing.T) {
	got, err := parseStages("gate")
	if err != nil {
		t.Fatalf(`parseStages("gate"): %v — PIPELINE_STAGES=gate is how Part D goes live (V6.3)`, err)
	}
	if len(got) != 1 || got[0] != pipeline.StageGate {
		t.Errorf(`parseStages("gate") = %v, want [gate]`, got)
	}
}

func registered() []pipeline.Stage {
	var out []pipeline.Stage
	for s := range stageImpls {
		out = append(out, s)
	}
	return out
}
