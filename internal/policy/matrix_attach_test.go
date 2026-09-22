package policy_test

// comms-inbox (SWT-74, docs/tickets/comms-inbox_SPEC.md) D7 and criterion 35:
// task_attach is humanOnly. It CLOSES a task and writes on another one, so it
// is the board's Done button's blast radius — an interactive session
// (mcp:manual:salvo, which policy.HumanActor passes), the dashboard and opsctl
// pass; every worker console and the orchestrator are refused.
//
// Pure matrix core, ZERO I/O (invariant 7). The table runs over SWT-37's
// fourteen-actor corpus (mcpVerbsCorpus) AND the six NAMED shapes criterion 35
// enumerates, because the corpus has no capture: row and the SPEC names one —
// the IK's "an actor-prefix check is a transport label, not a trust boundary"
// rule: when a test is about a gate, enumerate the shapes that exist in the
// repo, because one of them is usually the hole.
// matrix_requeue_test.go is the template, task_requeue the sibling verb.
//
// IMPOSED SURFACE (SPEC D7, "API / MCP tool changes"):
//
//	// internal/policy/matrix.go
//	var humanOnly = map[string]bool{ …, "task_attach": true }
//	// Nothing else changes: not mcpHumanOnly, not sendShaped, not freezeGated.
//	// internal/tools: task_attach registered in tools.Register, so the static
//	// fallback built from reg.Names() knows it.
//
// GREENFIELD NOTE — EXPECTED RED. Today task_attach is in no map, so Decide
// answers allow/matrix-human for every actor and the deny rows fail. Through
// Check it is not even registered, so the static fallback answers
// deny/static-default and the allow rows fail as well as the deny rows' RULE.
//
// WHY humanOnly AND NOT mcpHumanOnly (D7). A transport rule would let
// `orchestrator`, `drafts:gpt` and `capture:google` through, and no spine
// caller routes a comm: capture CREATES the comm and never decides where it
// belongs. If a future rule needs to attach, it must MOVE the tool deliberately.
//
// WHAT THIS GATE DOES NOT CLAIM: it stops worker consoles on the MCP path, not
// a worker's shell running opsctl, and not injected text inside an interactive
// session. That is why the verb is deliberately general and reversible —
// "the blast radius equals the board's existing Done button, it is audited, and
// task_reopen undoes it" (D7).
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - remove task_attach from humanOnly -> every deny row of both tables.
//   - move it to mcpHumanOnly instead -> the orchestrator, drafts:gpt,
//     worker:treetop and capture:google rows, and the RULE string of the MCP rows.
//   - make it sendShaped / snapshotGated -> mcpVerbsLoader fails the Check table.

import (
	"context"
	"testing"

	"github.com/sspataro57/switchboard/internal/policy"
)

const attachToolName = "task_attach"

// The six shapes criterion 35 names, plus the three it names as ALLOWED.
var attachNamedActors = []struct {
	actor string
	allow bool
	why   string
}{
	{"mcp:manual:salvo", true, "an interactive Claude Code session — `swb attach 481 452`"},
	{"dashboard:salvo", true, "the board's actions -> Attach form"},
	{"opsctl:salvo", true, "a hand-run opsctl call"},
	{"mcp:treetop", false, "a worker console: it may ASK (task_match is on both profiles) but never route"},
	{"worker:treetop", false, "the same console off the MCP path"},
	{"capture:google", false, "capture CREATES the comm; deciding where it belongs is his judgement, not a pass's"},
	{"drafts:gpt", false, "a GPT queue consumer"},
	{"orchestrator", false, "D9: no orchestrator rule reads the new names, and one that called this would die " +
		"on every tick"},
}

// Criterion 35 through the pure core.
func TestDecide_TaskAttach_NamedActorShapes(t *testing.T) {
	for _, tc := range attachNamedActors {
		tc := tc
		t.Run(tc.actor, func(t *testing.T) {
			d := policy.Decide(policy.Request{Tool: attachToolName, Actor: tc.actor}, policy.Snapshot{})
			if tc.allow {
				if d.Decision != "allow" {
					t.Errorf("%s by %q = %s/%s (%s), want allow — %s", attachToolName, tc.actor, d.Decision,
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

// Criterion 35 through the PRODUCTION matrix (the real registry behind
// policy.NewMatrix, with a loader that fails the test if it ever runs). An
// allowed human returns matrix-human; deny/static-default here means
// tools.Register does not register task_attach.
func TestMatrix_TaskAttach_ThroughCheck(t *testing.T) {
	checker := mcpVerbsChecker(t)
	ctx := context.Background()
	for _, tc := range attachNamedActors {
		tc := tc
		t.Run(tc.actor, func(t *testing.T) {
			d, err := checker.Check(ctx, policy.Request{Tool: attachToolName, Actor: tc.actor})
			if err != nil {
				t.Fatalf("Check(%s, %q): %v", attachToolName, tc.actor, err)
			}
			if tc.allow {
				if d.Decision != "allow" || d.Rule != "matrix-human" {
					t.Errorf("Check(%s, %q) = %s/%s (%s), want allow/matrix-human exactly — a humanOnly tool "+
						"that is not snapshot-gated is decided by Decide. deny/static-default here means "+
						"tools.Register does not register %s", attachToolName, tc.actor, d.Decision, d.Rule,
						d.Reason, attachToolName)
				}
				return
			}
			assertDeny(t, d, "human_only")
		})
	}
}

// The same gate over SWT-37's shared fourteen-actor corpus, so a later edit to
// that corpus cannot quietly narrow what this verb is tested against.
func TestDecide_TaskAttach_FullActorCorpus(t *testing.T) {
	if len(mcpVerbsCorpus) != 14 {
		t.Fatalf("POSITIVE CONTROL FAILED: mcpVerbsCorpus has %d actors, want SWT-37's 14", len(mcpVerbsCorpus))
	}
	for _, tc := range mcpVerbsCorpus {
		tc := tc
		t.Run(tc.actor, func(t *testing.T) {
			d := policy.Decide(policy.Request{Tool: attachToolName, Actor: tc.actor}, policy.Snapshot{})
			if tc.human {
				if d.Decision != "allow" {
					t.Errorf("%s by %q = %s/%s (%s), want allow: routing a comm onto a task is his judgement",
						attachToolName, tc.actor, d.Decision, d.Rule, d.Reason)
				}
				return
			}
			assertDeny(t, d, "human_only")
		})
	}
}

// GREEN BY DESIGN AND MUST STAY GREEN: an attach transmits nothing, so the kill
// switch and the per-channel rate limit have no claim on it (invariant 4).
func TestDecide_TaskAttach_IgnoresKillSwitchAndRateLimit(t *testing.T) {
	frozen := policy.Snapshot{SendingFrozen: true, Channel: "gmail", HourlyLimit: 10,
		SentLastHour: map[string]int{"gmail": 99}}
	d := policy.Decide(policy.Request{Tool: attachToolName, Actor: "mcp:manual:salvo"}, frozen)
	if d.Decision != "allow" {
		t.Errorf("%s by mcp:manual:salvo with the kill switch ON and gmail over its limit = %s/%s (%s), want "+
			"allow — routing a row and closing it sends nothing (invariant 4)", attachToolName,
			d.Decision, d.Rule, d.Reason)
	}
}
