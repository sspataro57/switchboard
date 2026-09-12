package policy_test

// SWT-41 (docs/tickets/orchestrator-deploy_SPEC.md) criterion 3:
// `orchestrator_cursor_advance` is in policy.humanOnly. Moving the cursor is a
// human deciding to DISCARD lifecycle events, so no automated caller — the
// orchestrator itself included — may do it. Pure matrix core, ZERO I/O
// (invariant 7). Reuses assertDeny (matrix_test.go) and mcpVerbsChecker (the
// REAL registry's static allow-list behind policy.NewMatrix, with a loader that
// fails the test if it runs — matrix_mcpverbs_test.go).
//
// IMPOSED SURFACE:
//
//	// internal/policy/matrix.go
//	var humanOnly = map[string]bool{ …, "orchestrator_cursor_advance": true }
//	// Not sendShaped, not freezeGated, not mcpHumanOnly.
//	// internal/tools: registered in tools.Register.
//
// THE ACTOR CORPUS (IK "an actor-prefix check is a transport label, not a trust
// boundary": enumerate the shapes that exist, one of them is usually the hole).
// `mcp:manual:x` is ALLOWED: HumanActor strips exactly one `mcp:` transport
// prefix, so an interactive session is a human (the SWT-11/SWT-20 contract).
// The SPEC lists the tool as reachable via `opsctl call`; the MCP transport is
// closed separately by its absence from internal/mcpserver's allowlist
// (internal/mcpserver/cursor_advance_test.go), not by policy.
// TestHumanActor_AgreesWithCursorAdvanceCorpus pins that the labels below are
// HumanActor's real answers, not this file's opinion.
//
// GREENFIELD NOTE — EXPECTED RED. The tool is in no map, so Decide answers
// allow/matrix-human for all six denied shapes; through Check it is not even
// registered, so the static fallback denies every human shape with
// static-default and the six bot rows fail on the rule.
//
// MUTATIONS THAT MUST TURN THIS FILE RED:
//   - drop it from humanOnly -> the six deny rows (both tests).
//   - move it to mcpHumanOnly -> orchestrator, drafts:gpt, promote:inquiry,
//     worker:x (a transport rule lets non-MCP callers through) and the rule
//     string of the MCP rows.
//   - make it snapshotGated -> mcpVerbsLoader fails the Check test.

import (
	"context"
	"testing"

	"github.com/sspataro57/switchboard/internal/policy"
)

const cursorAdvanceTool = "orchestrator_cursor_advance"

var cursorAdvanceActors = []struct {
	actor string
	human bool
}{
	{"dashboard:x", true},
	{"opsctl:x", true},
	{"manual:x", true},
	{"mcp:manual:x", true}, // one transport prefix stripped — HumanActor's contract
	{"orchestrator", false},
	{"drafts:gpt", false},
	{"promote:inquiry", false},
	{"mcp:switchboard", false}, // a worker console for the switchboard client
	{"mcp:worker:x", false},
	{"worker:x", false},
}

// Positive control on the corpus labels: they are HumanActor's real answers.
func TestHumanActor_AgreesWithCursorAdvanceCorpus(t *testing.T) {
	for _, tc := range cursorAdvanceActors {
		if got := policy.HumanActor(tc.actor); got != tc.human {
			t.Errorf("policy.HumanActor(%q) = %v, but this file labels it human=%v; fix the corpus, not the "+
				"predicate", tc.actor, got, tc.human)
		}
	}
}

func TestDecide_OrchestratorCursorAdvance_HumanOnly(t *testing.T) {
	for _, tc := range cursorAdvanceActors {
		tc := tc
		t.Run(tc.actor, func(t *testing.T) {
			d := policy.Decide(policy.Request{Tool: cursorAdvanceTool, Actor: tc.actor}, policy.Snapshot{})
			if tc.human {
				if d.Decision != "allow" {
					t.Errorf("%s by %q = %s/%s (%s), want allow", cursorAdvanceTool, tc.actor, d.Decision, d.Rule, d.Reason)
				}
				return
			}
			// human_only, NOT mcp_human_only: the orchestrator and every other
			// in-process caller is refused too.
			assertDeny(t, d, "human_only")
		})
	}
}

func TestMatrix_OrchestratorCursorAdvance_ThroughCheck(t *testing.T) {
	checker := mcpVerbsChecker(t)
	ctx := context.Background()
	for _, tc := range cursorAdvanceActors {
		tc := tc
		t.Run(tc.actor, func(t *testing.T) {
			d, err := checker.Check(ctx, policy.Request{Tool: cursorAdvanceTool, Actor: tc.actor})
			if err != nil {
				t.Fatalf("Check(%s, %q): %v", cursorAdvanceTool, tc.actor, err)
			}
			if tc.human {
				if d.Decision != "allow" || d.Rule != "matrix-human" {
					t.Errorf("Check(%s, %q) = %s/%s (%s), want allow/matrix-human exactly — a humanOnly tool "+
						"that is not snapshot-gated is decided by Decide. deny/static-default here means "+
						"tools.Register does not register %s", cursorAdvanceTool, tc.actor, d.Decision, d.Rule,
						d.Reason, cursorAdvanceTool)
				}
				return
			}
			assertDeny(t, d, "human_only")
		})
	}
}
