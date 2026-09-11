package policy_test

// SWT-37 (docs/tickets/mcp-task-verbs_SPEC.md) criteria 1, 2, 3, 4 and 6
// (policy half): the `mcp_human_only` transport rule for task_close and
// task_mark_delivered, and task_dismiss's existing human_only gate, over EVERY
// actor shape in the repo. Pure matrix core, ZERO I/O (invariant 7). Reuses
// assertDeny from matrix_test.go (same package); copies matrix_reopen_test.go's
// real-registry checker with a loader that fails the test if it ever runs.
//
// IMPOSED SURFACE (SPEC V1, "API / MCP tool changes"):
//
//	// internal/policy/matrix.go
//	var mcpHumanOnly = map[string]bool{"task_close": true, "task_mark_delivered": true}
//	// Decide, right after the humanOnly check:
//	//   if mcpHumanOnly[req.Tool] && ViaMCPActor(req.Actor) && !HumanActor(req.Actor) →
//	//   Decision{"deny", "mcp_human_only",
//	//     fmt.Sprintf("%s over MCP requires a human session identity (mcp:manual:/mcp:dashboard:/mcp:opsctl:); got %q", tool, actor)}
//	// matrix.Check, BEFORE the snapshotGated branch:
//	//   if mcpHumanOnly[req.Tool] { if d := Decide(req, Snapshot{}); d.Decision == "deny" { return d, nil }; return m.fallback.Check(ctx, req) }
//	func ViaMCPActor(actor string) bool // strings.HasPrefix(actor, MCPTransportPrefix)
//
// GREENFIELD NOTE — EXPECTED RED. policy.ViaMCPActor does not exist, so the
// policy_test package compile-FAILS until matrix.go exports it. Once it
// compiles, and before the rule lands, every mcp_human_only row in criteria 1
// and 2 fails: Decide answers allow/matrix-human and the matrix answers
// allow/static-default for mcp:acme and every other worker shape.
//
// WHAT THIS RULE CLAIMS AND WHAT IT DOES NOT (V1; IK "an actor-prefix check is
// a transport label, not a trust boundary"). It is a TRANSPORT rule: this
// transport's non-human identities may not make these two transitions. It does
// NOT stop in-process callers — orchestrator (R2/R8), ticketstatus:jira (the
// reconciler), drafts:gpt, bare worker: — and it must not, because R2, R8 and
// the reconciler call task_close / task_mark_delivered and would stall on a
// denial that reads like a permissions bug. Those callers are Go code with
// fixed call sites; none takes a tool name from a model. The callers a model
// can steer are MCP sessions, which are exactly what the rule keys on. So the
// non-MCP rows below are ALLOWED on purpose, and pinned, so that nothing here
// is assumed.

import (
	"context"
	"fmt"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

// mcpVerbsCorpus is the SPEC's actor corpus, verbatim: fourteen shapes.
//
//   - human: HumanActor is true — dashboard:/opsctl:/manual:, after ONE
//     stripped mcp: prefix.
//   - mcp:   the actor carries the MCP transport prefix.
//
// A worker console is `mcp:{client}` or `mcp:{client}.{sub}` (opsworker sets
// OPS_WORKER_ID to the bare --client value); `mcp:worker:acme` exists only in
// tests and is kept because a test-only shape that got through would still be a
// hole. `mcp:mcp:manual:salvo` (a doubled prefix) and `mcp:` (an empty id) are
// the malformed MCP shapes.
var mcpVerbsCorpus = []struct {
	actor string
	human bool
	mcp   bool
}{
	{"dashboard:salvo", true, false},
	{"opsctl:salvo", true, false},
	{"mcp:manual:salvo", true, true},
	{"mcp:dashboard:salvo@example.com", true, true},
	{"mcp:opsctl:salvo", true, true},
	{"mcp:acme", false, true},
	{"mcp:acme.main", false, true},
	{"mcp:worker:acme", false, true},
	{"mcp:mcp:manual:salvo", false, true},
	{"mcp:", false, true},
	{"drafts:gpt", false, false},
	{"worker:acme", false, false},
	{"ticketstatus:jira", false, false},
	{"orchestrator", false, false},
}

// The two verbs the new rule covers. task_dismiss is NOT here: it stays in
// humanOnly (V2) and has its own test below.
var mcpHumanOnlyVerbs = []string{"task_close", "task_mark_delivered"}

func wantMCPHumanOnlyReason(tool, actor string) string {
	return fmt.Sprintf("%s over MCP requires a human session identity (mcp:manual:/mcp:dashboard:/mcp:opsctl:); got %q", tool, actor)
}

// mcpVerbsChecker is the production wiring: the matrix in front of the static
// allow-list built from the REAL registry (every main's shape). A list the test
// supplied itself would allow the tools no matter what the repo registers.
func mcpVerbsChecker(t *testing.T) policy.Checker {
	t.Helper()
	reg := executor.NewRegistry()
	tools.Register(reg, nil) // nil pool: Register only builds closures
	return policy.NewMatrix(mcpVerbsLoader{t}, policy.NewStatic(reg.Names()...))
}

// mcpVerbsLoader fails the test if the matrix ever loads a delivery snapshot
// for one of the task verbs. None of them creates, reads or mutates a
// deliveries row (invariant 4), so neither the kill switch nor the rate limit
// has any claim on them, and V1 says outright: "The snapshot loader never runs."
type mcpVerbsLoader struct{ t *testing.T }

func (l mcpVerbsLoader) Load(_ context.Context, req policy.Request) (policy.Snapshot, error) {
	l.t.Errorf("policy.Matrix loaded a delivery snapshot for %s (actor %q): the verb has become "+
		"snapshotGated, or mcpHumanOnly is routed after the snapshotGated branch. V1: the loader never "+
		"runs for these verbs", req.Tool, req.Actor)
	return policy.Snapshot{}, nil
}

// Criterion 1. Deny iff (tool in mcpHumanOnly) AND (actor carries mcp:) AND
// !HumanActor(actor). Every other combination allows.
//
// The non-MCP rows (drafts:gpt, worker:acme, ticketstatus:jira, orchestrator)
// are ALLOWED BY DESIGN: this is a transport rule, not a trust claim (V1).
// Mutation M-c (drop the MCP-prefix condition) turns exactly those rows red;
// mutation M-b (`!HumanActor` → true) turns the five human rows red.
func TestDecide_MCPHumanOnly_ActorMatrix(t *testing.T) {
	for _, tool := range mcpHumanOnlyVerbs {
		for _, tc := range mcpVerbsCorpus {
			tool, tc := tool, tc
			t.Run(tool+"/"+tc.actor, func(t *testing.T) {
				d := policy.Decide(policy.Request{Tool: tool, Actor: tc.actor}, policy.Snapshot{})
				if tc.mcp && !tc.human {
					assertDeny(t, d, "mcp_human_only")
					if want := wantMCPHumanOnlyReason(tool, tc.actor); d.Reason != want {
						t.Errorf("reason = %q, want %q (SPEC V1 spells it)", d.Reason, want)
					}
					return
				}
				if d.Decision != "allow" {
					t.Errorf("%s by %q = %s/%s (%s), want allow. The rule keys on the MCP TRANSPORT: human "+
						"sessions pass, and in-process callers (orchestrator R2/R8, the Jira reconciler, the "+
						"draft worker) are not on that transport and must keep closing and delivering tasks",
						tool, tc.actor, d.Decision, d.Rule, d.Reason)
				}
			})
		}
	}
}

// Criterion 2. The same table through the production matrix. Criterion 1 alone
// stays green under mutation M-a (Check stops routing mcpHumanOnly through
// Decide and falls straight to the fallback), which is why this test exists.
//
// Every allowed case must be EXACTLY static-default: that is what the
// orchestrator's and the reconciler's policy_decisions rows say on main today,
// and V1 promises them "byte for byte".
func TestMatrix_MCPHumanOnly_ThroughCheck(t *testing.T) {
	checker := mcpVerbsChecker(t)
	ctx := context.Background()
	for _, tool := range mcpHumanOnlyVerbs {
		for _, tc := range mcpVerbsCorpus {
			tool, tc := tool, tc
			t.Run(tool+"/"+tc.actor, func(t *testing.T) {
				d, err := checker.Check(ctx, policy.Request{Tool: tool, Actor: tc.actor})
				if err != nil {
					t.Fatalf("Check(%s, %q): %v", tool, tc.actor, err)
				}
				if tc.mcp && !tc.human {
					assertDeny(t, d, "mcp_human_only")
					return
				}
				if d.Decision != "allow" || d.Rule != "static-default" {
					t.Errorf("Check(%s, %q) = %s/%s, want allow/static-default exactly — every call the rule "+
						"does not deny must keep today's decision byte for byte (V1), so the spine's audit rows "+
						"do not change", tool, tc.actor, d.Decision, d.Rule)
				}
			})
		}
	}
}

// Criterion 3. task_dismiss over the same fourteen shapes: five allowed, nine
// human_only. SWT-31's table never named the REAL worker shapes (mcp:acme,
// mcp:acme.main) or the reconciler (ticketstatus:jira). Since this ticket lists
// task_dismiss in the full profile, humanOnly is now the ONLY gate between a
// worker console and a dismissal (V2's honest delta), so every shape is pinned.
// Mutation M-h (remove task_dismiss from humanOnly) turns the nine rows red.
func TestDecide_TaskDismiss_FullActorCorpus(t *testing.T) {
	for _, tc := range mcpVerbsCorpus {
		tc := tc
		t.Run(tc.actor, func(t *testing.T) {
			d := policy.Decide(policy.Request{Tool: "task_dismiss", Actor: tc.actor}, policy.Snapshot{})
			if tc.human {
				if d.Decision != "allow" {
					t.Errorf("task_dismiss by %q = %s/%s, want allow (a human identity)", tc.actor, d.Decision, d.Rule)
				}
				return
			}
			// human_only, NOT mcp_human_only: the dismiss gate is unchanged (V2),
			// and the humanOnly check runs first in Decide.
			assertDeny(t, d, "human_only")
		})
	}
}

// Criterion 4. None of the three verbs transmits anything, so the kill switch
// (whose job is to stop SENDING) and the per-channel rate limit have no claim
// on them. A frozen, over-limit snapshot must not stop a human cleaning up.
func TestDecide_TaskVerbs_IgnoreKillSwitchAndRateLimit(t *testing.T) {
	frozenAndOverLimit := policy.Snapshot{
		SendingFrozen: true,
		Channel:       "gmail",
		HourlyLimit:   10,
		SentLastHour:  map[string]int{"gmail": 99},
	}
	for _, tool := range []string{"task_dismiss", "task_close", "task_mark_delivered"} {
		d := policy.Decide(policy.Request{Tool: tool, Actor: "mcp:manual:salvo"}, frozenAndOverLimit)
		if d.Decision != "allow" {
			t.Errorf("%s by mcp:manual:salvo with the kill switch ON and gmail over its limit = %s/%s (%s), "+
				"want allow — nothing outbound exists (invariant 4)", tool, d.Decision, d.Rule, d.Reason)
		}
	}
}

// Criterion 6, policy half. ONE spelling of "arrived over MCP". Case-sensitive,
// anchored at byte 0, and "mcp:" with an empty id still counts (the rule then
// denies it, criterion 1).
func TestViaMCPActor(t *testing.T) {
	for _, tc := range viaMCPActorCases {
		if got := policy.ViaMCPActor(tc.actor); got != tc.want {
			t.Errorf("policy.ViaMCPActor(%q) = %v, want %v", tc.actor, got, tc.want)
		}
	}
}

// viaMCPActorCases are the SPEC's inputs for criterion 6. The executor twin
// (internal/executor/viamcp_test.go) spells the same list; the two must agree.
var viaMCPActorCases = []struct {
	actor string
	want  bool
}{
	{"mcp:x", true},
	{"mcp:", true},
	{"x", false},
	{"MCP:x", false},
	{"", false},
	{" mcp:x", false},
}
