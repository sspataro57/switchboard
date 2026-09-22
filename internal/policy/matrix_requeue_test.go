package policy_test

// activity-resurfaces (SWT-72, docs/tickets/activity-resurfaces_SPEC.md) D6 and
// criterion 22: task_requeue is humanOnly. That is exactly the split the owner
// asked for — an interactive session is mcp:manual:salvo, which policy.HumanActor
// passes; a worker console is mcp:{client}, refused ("workers never choose their
// own work", and this verb can lift a status and raise a priority). dashboard:
// and opsctl: pass.
//
// Pure matrix core, ZERO I/O (invariant 7). The table runs over SWT-37's
// fourteen-actor corpus (mcpVerbsCorpus) AND the five NAMED shapes criterion 22
// enumerates, because the corpus has no capture: row and the SPEC names one.
// matrix_priority_test.go is the template, task_set_priority the sibling verb.
//
// IMPOSED SURFACE (SPEC D6, "API / MCP tool changes"):
//
//	// internal/policy/matrix.go
//	var humanOnly = map[string]bool{ …, "task_requeue": true }
//	// Nothing else changes: not mcpHumanOnly, not sendShaped, not freezeGated.
//	// internal/tools: task_requeue registered in tools.Register, so the static
//	// fallback built from reg.Names() knows it.
//
// GREENFIELD NOTE — EXPECTED RED. Today task_requeue is in no map, so Decide
// answers allow/matrix-human for every actor and the deny rows fail. Through
// Check it is not even registered, so the static fallback answers
// deny/static-default and the allow rows fail as well as the deny rows' RULE.
//
// WHY humanOnly AND NOT mcpHumanOnly (D6). A transport rule would let
// `orchestrator`, `drafts:gpt` and `capture:google` through, and no spine
// caller requeues: the only callers are the board, an interactive session and
// opsctl. If a future rule needs to requeue, it must MOVE the tool deliberately.
//
// WHAT THIS GATE DOES NOT CLAIM (IK: "an actor-prefix check is a transport
// label, not a trust boundary"): it stops worker consoles on the MCP path, not
// a worker's shell running opsctl, and not injected text inside an interactive
// session.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - remove task_requeue from humanOnly -> every deny row of both tables.
//   - move it to mcpHumanOnly instead -> the orchestrator, drafts:gpt,
//     worker:treetop and capture:google rows, and the RULE string of the MCP rows.
//   - make it sendShaped / snapshotGated -> mcpVerbsLoader fails the Check table.

import (
	"context"
	"testing"

	"github.com/sspataro57/switchboard/internal/policy"
)

const requeueToolName = "task_requeue"

// The five shapes criterion 22 names as DENIED, plus the three it names as
// ALLOWED. Enumerated here rather than leaning on the shared corpus, because
// the corpus has no capture: row and D6's argument is about automated callers
// of every shape, MCP and non-MCP alike.
var requeueNamedActors = []struct {
	actor string
	allow bool
	why   string
}{
	{"mcp:manual:salvo", true, "an interactive Claude Code session — `swb requeue 452`"},
	{"dashboard:salvo", true, "the board's actions -> Requeue form"},
	{"opsctl:salvo", true, "a hand-run opsctl call"},
	{"mcp:treetop", false, "a worker console: never chooses its own work"},
	{"worker:treetop", false, "the same console off the MCP path"},
	{"capture:google", false, "the capture engine marks activity; it never reviews it"},
	{"drafts:gpt", false, "a GPT queue consumer"},
	{"orchestrator", false, "no orchestrator rule requeues (D9: the spine never reads these columns)"},
}

// Criterion 22 through the pure core.
func TestDecide_TaskRequeue_NamedActorShapes(t *testing.T) {
	for _, tc := range requeueNamedActors {
		tc := tc
		t.Run(tc.actor, func(t *testing.T) {
			d := policy.Decide(policy.Request{Tool: requeueToolName, Actor: tc.actor}, policy.Snapshot{})
			if tc.allow {
				if d.Decision != "allow" {
					t.Errorf("%s by %q = %s/%s (%s), want allow — %s", requeueToolName, tc.actor, d.Decision,
						d.Rule, d.Reason, tc.why)
				}
				return
			}
			// human_only, NOT mcp_human_only: a transport rule would let
			// orchestrator, drafts:gpt and capture:google through.
			assertDeny(t, d, "human_only")
		})
	}
}

// Criterion 22 through the PRODUCTION matrix (the real registry behind
// policy.NewMatrix, with a loader that fails the test if it ever runs). An
// allowed human returns matrix-human; deny/static-default here means
// tools.Register does not register task_requeue.
func TestMatrix_TaskRequeue_ThroughCheck(t *testing.T) {
	checker := mcpVerbsChecker(t)
	ctx := context.Background()
	for _, tc := range requeueNamedActors {
		tc := tc
		t.Run(tc.actor, func(t *testing.T) {
			d, err := checker.Check(ctx, policy.Request{Tool: requeueToolName, Actor: tc.actor})
			if err != nil {
				t.Fatalf("Check(%s, %q): %v", requeueToolName, tc.actor, err)
			}
			if tc.allow {
				if d.Decision != "allow" || d.Rule != "matrix-human" {
					t.Errorf("Check(%s, %q) = %s/%s (%s), want allow/matrix-human exactly — a humanOnly tool that "+
						"is not snapshot-gated is decided by Decide. deny/static-default here means tools.Register "+
						"does not register %s", requeueToolName, tc.actor, d.Decision, d.Rule, d.Reason, requeueToolName)
				}
				return
			}
			assertDeny(t, d, "human_only")
		})
	}
}

// The same gate over SWT-37's shared fourteen-actor corpus, so a later edit to
// that corpus cannot quietly narrow what this verb is tested against (the
// positive control matrix_priority_test.go runs for task_set_priority).
func TestDecide_TaskRequeue_FullActorCorpus(t *testing.T) {
	if len(mcpVerbsCorpus) != 14 {
		t.Fatalf("POSITIVE CONTROL FAILED: mcpVerbsCorpus has %d actors, want SWT-37's 14", len(mcpVerbsCorpus))
	}
	for _, tc := range mcpVerbsCorpus {
		tc := tc
		t.Run(tc.actor, func(t *testing.T) {
			d := policy.Decide(policy.Request{Tool: requeueToolName, Actor: tc.actor}, policy.Snapshot{})
			if tc.human {
				if d.Decision != "allow" {
					t.Errorf("%s by %q = %s/%s (%s), want allow: a human session clears its own review flag",
						requeueToolName, tc.actor, d.Decision, d.Rule, d.Reason)
				}
				return
			}
			assertDeny(t, d, "human_only")
		})
	}
}

// GREEN BY DESIGN AND MUST STAY GREEN: Requeue transmits nothing, so the kill
// switch and the per-channel rate limit have no claim on it (invariant 4).
func TestDecide_TaskRequeue_IgnoresKillSwitchAndRateLimit(t *testing.T) {
	frozen := policy.Snapshot{SendingFrozen: true, Channel: "gmail", HourlyLimit: 10,
		SentLastHour: map[string]int{"gmail": 99}}
	d := policy.Decide(policy.Request{Tool: requeueToolName, Actor: "mcp:manual:salvo"}, frozen)
	if d.Decision != "allow" {
		t.Errorf("%s by mcp:manual:salvo with the kill switch ON and gmail over its limit = %s/%s (%s), want "+
			"allow — putting a row back on the queue sends nothing (invariant 4)", requeueToolName,
			d.Decision, d.Rule, d.Reason)
	}
}
