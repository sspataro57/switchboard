package policy_test

// Unit tests for the SWT-10 plan-import policy additions (SPEC 10-plan-import,
// criterion 6 / API changes): approve_plan_import, reject_plan_import, and
// apply_plan_import join the `humanOnly` map (dashboard:/opsctl:/manual: actors
// only — a worker: actor is denied). propose_plan_import is NOT human-only and
// NOT send-shaped, so it falls through NewMatrix to the static allow-list.
// None is send-shaped: no snapshot loader involvement, no kill-switch/rate-limit
// interaction. Pure matrix core — ZERO I/O (invariant 7). This file extends the
// existing internal/policy matrix_test.go (it reuses humanActor/botActor and
// assertDeny defined there).
//
// GREENFIELD NOTE: internal/policy already compiles; this test ASSERT-FAILs
// today — the three plan tools are not yet in the humanOnly map, so Decide
// returns allow ("matrix-human") for a bot actor instead of denying human_only.
// Adding the three names to matrix.go's humanOnly map makes it pass.

import (
	"context"
	"testing"

	"github.com/sspataro57/switchboard/internal/policy"
)

// TestDecide_PlanImportTools_HumanOnly: the three gated plan-import tools deny a
// worker/bot actor (human_only) and allow a human actor; none touches the send
// snapshot (an empty Snapshot suffices — they are not send-shaped).
func TestDecide_PlanImportTools_HumanOnly(t *testing.T) {
	for _, tool := range []string{"approve_plan_import", "reject_plan_import", "apply_plan_import"} {
		t.Run(tool+"/bot denied", func(t *testing.T) {
			d := policy.Decide(policy.Request{Tool: tool, Actor: botActor}, policy.Snapshot{})
			assertDeny(t, d, "human_only")
		})
		t.Run(tool+"/human allowed", func(t *testing.T) {
			d := policy.Decide(policy.Request{Tool: tool, Actor: humanActor}, policy.Snapshot{})
			if d.Decision != "allow" {
				t.Errorf("%s by human = %q (rule %s), want allow", tool, d.Decision, d.Rule)
			}
		})
	}
}

// TestDecide_PlanImportTools_HumanPrefixes: every human prefix
// (dashboard:/opsctl:/manual:) is accepted — the CLI apply uses manual:, the
// dashboard approve/reject uses dashboard:.
func TestDecide_PlanImportTools_HumanPrefixes(t *testing.T) {
	for _, actor := range []string{"dashboard:salvo", "opsctl:salvo", "manual:salvo"} {
		d := policy.Decide(policy.Request{Tool: "apply_plan_import", Actor: actor}, policy.Snapshot{})
		if d.Decision != "allow" {
			t.Errorf("apply_plan_import by %q = %q (rule %s), want allow", actor, d.Decision, d.Rule)
		}
	}
}

// TestMatrix_ProposePlanImport_FallsThroughToStatic: propose_plan_import is
// neither human-only nor send-shaped, so NewMatrix routes it to the fallback
// allow-list WITHOUT consulting the send snapshot loader. The CLI's
// planimport:{user} actor (not a human prefix) is fine here — propose is not
// gated on actor.
func TestMatrix_ProposePlanImport_FallsThroughToStatic(t *testing.T) {
	l := &recordingLoader{snap: gmailSnap(0, false)}
	fallback := policy.NewStatic("propose_plan_import")
	m := policy.NewMatrix(l, fallback)

	d, err := m.Check(context.Background(),
		policy.Request{Tool: "propose_plan_import", Actor: "planimport:salvo"})
	if err != nil {
		t.Fatalf("Check(propose_plan_import): %v", err)
	}
	if d.Decision != "allow" {
		t.Errorf("propose_plan_import = %q, want allow (static fallthrough)", d.Decision)
	}
	if l.called {
		t.Errorf("propose_plan_import must NOT consult the send snapshot loader (not send-shaped)")
	}
}

// TestMatrix_ApprovePlanImport_DeniesBotViaMatrix: routed through NewMatrix, a
// bot actor on approve_plan_import is denied human_only WITHOUT the loader
// running (human-only is decided before any snapshot).
func TestMatrix_ApprovePlanImport_DeniesBotViaMatrix(t *testing.T) {
	l := &recordingLoader{snap: gmailSnap(0, false)}
	m := policy.NewMatrix(l, policy.NewStatic("approve_plan_import"))

	d, err := m.Check(context.Background(),
		policy.Request{Tool: "approve_plan_import", Actor: botActor})
	if err != nil {
		t.Fatalf("Check(approve_plan_import): %v", err)
	}
	assertDeny(t, d, "human_only")
	if l.called {
		t.Errorf("human-only denial must not consult the send snapshot loader")
	}
}
