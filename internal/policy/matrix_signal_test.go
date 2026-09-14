package policy_test

// SWT-52 (docs/tickets/board-status-lights_SPEC.md) criterion 21: task_signal
// is humanOnly. Pure matrix core, ZERO I/O (invariant 7). Reuses assertDeny
// (matrix_test.go) and mcpVerbsChecker (matrix_mcpverbs_test.go) — the REAL
// registry's static allow-list behind policy.NewMatrix, with a loader that fails
// the test if it ever runs.
//
// IMPOSED SURFACE (SPEC D7, criterion 21):
//
//	// internal/policy/matrix.go
//	var humanOnly = map[string]bool{ …, "task_signal": true }
//	// NOT mcpHumanOnly, sendShaped, freezeGated or snapshotGated
//	// (matrix_signal_internal_test.go pins the maps).
//
// GREENFIELD NOTE — EXPECTED RED: task_signal is in no map, so Decide allows
// the nine automated actors; and it is not registered, so Check's static
// fallback denies the four humans with static-default.
//
// MUTATIONS THAT MUST TURN THIS FILE RED:
//   - drop task_signal from humanOnly → every deny row.
//   - put it in mcpHumanOnly instead → orchestrator, drafts:gpt, worker:acme,
//     capture:slackweb, promote:classify and ticketstatus:jira (not on the MCP
//     transport) are allowed, and the MCP rows carry mcp_human_only.
//   - make it snapshotGated → mcpVerbsLoader fails the Check test.

import (
	"context"
	"testing"

	"github.com/sspataro57/switchboard/internal/policy"
)

const signalTool = "task_signal"

// The SPEC's corpus, verbatim (criterion 21).
var signalAllowed = []string{"dashboard:salvo", "opsctl:salvo", "manual:salvo", "mcp:manual:salvo"}
var signalDenied = []string{"mcp:acme", "mcp:acme.web", "mcp:worker:acme", "worker:acme", "drafts:gpt",
	"orchestrator", "capture:slackweb", "promote:classify", "ticketstatus:jira"}

func TestDecide_TaskSignal_ActorCorpus(t *testing.T) {
	for _, a := range signalAllowed {
		d := policy.Decide(policy.Request{Tool: signalTool, Actor: a}, policy.Snapshot{})
		if d.Decision != "allow" {
			t.Errorf("task_signal by %q = %s/%s (%s), want allow: Salvador's sessions signal their own work", a,
				d.Decision, d.Rule, d.Reason)
		}
	}
	for _, a := range signalDenied {
		a := a
		t.Run(a, func(t *testing.T) {
			// human_only, NOT mcp_human_only: no spine caller sets a session state
			// (D7), so every automated caller is refused, not only MCP ones.
			assertDeny(t, policy.Decide(policy.Request{Tool: signalTool, Actor: a}, policy.Snapshot{}), "human_only")
		})
	}
}

func TestMatrix_TaskSignal_ThroughCheck(t *testing.T) {
	checker := mcpVerbsChecker(t)
	ctx := context.Background()
	for _, a := range signalAllowed {
		d, err := checker.Check(ctx, policy.Request{Tool: signalTool, Actor: a})
		if err != nil {
			t.Fatalf("Check(task_signal, %q): %v", a, err)
		}
		if d.Decision != "allow" || d.Rule != "matrix-human" {
			t.Errorf("Check(task_signal, %q) = %s/%s (%s), want allow/matrix-human — deny/static-default means "+
				"tools.Register does not register task_signal", a, d.Decision, d.Rule, d.Reason)
		}
	}
	for _, a := range signalDenied {
		d, err := checker.Check(ctx, policy.Request{Tool: signalTool, Actor: a})
		if err != nil {
			t.Fatalf("Check(task_signal, %q): %v", a, err)
		}
		assertDeny(t, d, "human_only")
	}
}

// A state signal transmits nothing: the kill switch and the rate limit have no
// claim on it.
func TestDecide_TaskSignal_IgnoresKillSwitchAndRateLimit(t *testing.T) {
	snap := policy.Snapshot{SendingFrozen: true, Channel: "gmail", HourlyLimit: 10, SentLastHour: map[string]int{"gmail": 99}}
	if d := policy.Decide(policy.Request{Tool: signalTool, Actor: "mcp:manual:salvo"}, snap); d.Decision != "allow" {
		t.Errorf("task_signal with the kill switch on = %s/%s (%s), want allow (invariant 4: nothing is sent)",
			d.Decision, d.Rule, d.Reason)
	}
}
