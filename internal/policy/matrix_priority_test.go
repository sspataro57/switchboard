package policy_test

// SWT-38 (docs/tickets/mcp-task-capture_SPEC.md) criteria 6, 7, 8 and 9: the
// policy gate on the new task_set_priority tool, over SWT-37's fourteen-actor
// corpus (mcpVerbsCorpus, matrix_mcpverbs_test.go), and the NON-gate on
// create_task / task_append_log. Pure matrix core, ZERO I/O (invariant 7).
// Reuses assertDeny (matrix_test.go) and mcpVerbsChecker — the REAL registry's
// static allow-list behind policy.NewMatrix, with a loader that fails the test
// if it ever runs.
//
// IMPOSED SURFACE (SPEC C6, "API / MCP tool changes"):
//
//	// internal/policy/matrix.go
//	var humanOnly = map[string]bool{ …, "task_set_priority": true } // SWT-38 C6
//	// Nothing else changes: not mcpHumanOnly, not sendShaped, not freezeGated.
//	// internal/tools: task_set_priority registered in tools.Register, so the
//	// static fallback built from reg.Names() knows it.
//
// GREENFIELD NOTE — EXPECTED RED. Today task_set_priority is in no map, so
// Decide answers allow/matrix-human for all fourteen actors and criterion 6's
// nine deny rows fail. Through Check it is not even registered, so the static
// fallback answers deny/static-default and criterion 7's five allow rows fail
// as well as its nine (wrong-rule) deny rows. Criteria 8 and 9 are GREEN today
// and must stay green: they pin what SWT-38 must NOT change.
//
// WHY THE ORCHESTRATOR IS DENIED (C6). No spine caller sets priority after
// creation (SPEC fact 1), so there is nothing to exempt, and humanOnly is the
// stricter, established gate (the SWT-31 task_dismiss precedent). It enforces
// CLAUDE.md's worker loop rule "never choose your own work" for EVERY automated
// caller, not only the MCP ones. If a future rule needs to set priority (triage
// escalation going live, say), it must MOVE the tool to mcpHumanOnly
// deliberately. The orchestrator row below exists so that the move is a
// conscious edit of this file, not a drive-by.
//
// WHAT THIS GATE DOES NOT CLAIM (IK "an actor-prefix check is a transport
// label, not a trust boundary"). It stops worker consoles on the MCP path. It
// does not stop a worker's shell running opsctl or psql (SWT-37 fact 10,
// pre-existing), and it does not stop injected text inside an interactive
// session (C9, the accepted risk).
//
// MUTATIONS THAT MUST TURN THIS FILE RED:
//   - M-d: remove task_set_priority from humanOnly → the nine deny rows of
//     criteria 6 and 7 (Decide and Check both answer allow).
//   - M-e: move it to mcpHumanOnly instead → the orchestrator, drafts:gpt,
//     worker:acme and ticketstatus:jira rows of criteria 6 and 7 (a transport
//     rule lets non-MCP callers through), and the rule string of the MCP rows
//     (mcp_human_only, not human_only).
//   - make it sendShaped / snapshotGated → mcpVerbsLoader fails criterion 7, and
//     criterion 9's frozen, over-limit snapshot denies.
//   - add create_task or task_append_log to humanOnly or mcpHumanOnly →
//     criterion 8 (the orchestrator, capture:gmail or mcp:acme rows): the
//     spine and the worker consoles keep calling both; the user-scope
//     restriction is the profile pin (C4), not policy.

import (
	"context"
	"testing"

	"github.com/sspataro57/switchboard/internal/policy"
)

const setPriorityTool = "task_set_priority"

// The automated callers C6 names by shape. Pinned as a positive control on the
// shared corpus: if SWT-37's corpus ever loses one of them, this file stops
// proving what its comment claims.
var setPriorityMustDeny = []string{
	"orchestrator", "ticketstatus:jira", "drafts:gpt", "worker:acme", "mcp:acme", "mcp:acme.main",
}

func assertCorpusCoversSetPriority(t *testing.T) {
	t.Helper()
	nonHuman := map[string]bool{}
	humans := 0
	for _, tc := range mcpVerbsCorpus {
		if tc.human {
			humans++
		} else {
			nonHuman[tc.actor] = true
		}
	}
	if len(mcpVerbsCorpus) != 14 || humans != 5 {
		t.Fatalf("POSITIVE CONTROL FAILED: mcpVerbsCorpus has %d actors / %d human, want SWT-37's 14 / 5",
			len(mcpVerbsCorpus), humans)
	}
	for _, a := range setPriorityMustDeny {
		if !nonHuman[a] {
			t.Fatalf("POSITIVE CONTROL FAILED: %q is not a non-human row of mcpVerbsCorpus; C6 names it", a)
		}
	}
}

// Criterion 6. Five human shapes allowed; the other nine denied with rule
// EXACTLY human_only — MCP and non-MCP callers alike, the orchestrator
// included (see the header: C6).
func TestDecide_TaskSetPriority_FullActorCorpus(t *testing.T) {
	assertCorpusCoversSetPriority(t)
	for _, tc := range mcpVerbsCorpus {
		tc := tc
		t.Run(tc.actor, func(t *testing.T) {
			d := policy.Decide(policy.Request{Tool: setPriorityTool, Actor: tc.actor}, policy.Snapshot{})
			if tc.human {
				if d.Decision != "allow" {
					t.Errorf("task_set_priority by %q = %s/%s (%s), want allow: a human session reorders its own "+
						"queue", tc.actor, d.Decision, d.Rule, d.Reason)
				}
				return
			}
			// human_only, NOT mcp_human_only: a transport rule would let the
			// orchestrator, drafts:gpt, worker:acme and ticketstatus:jira
			// through, and C6 refuses every automated caller.
			assertDeny(t, d, "human_only")
		})
	}
}

// Criterion 7. The same table through the production matrix. Allowed cases
// return matrix-human (humanOnly tools that are not snapshot-gated go through
// Decide — matrix.go's Check), denied cases human_only, and the loader never
// runs (mcpVerbsLoader fails the test if it does: nothing here is send-shaped).
func TestMatrix_TaskSetPriority_ThroughCheck(t *testing.T) {
	assertCorpusCoversSetPriority(t)
	checker := mcpVerbsChecker(t)
	ctx := context.Background()
	for _, tc := range mcpVerbsCorpus {
		tc := tc
		t.Run(tc.actor, func(t *testing.T) {
			d, err := checker.Check(ctx, policy.Request{Tool: setPriorityTool, Actor: tc.actor})
			if err != nil {
				t.Fatalf("Check(task_set_priority, %q): %v", tc.actor, err)
			}
			if tc.human {
				if d.Decision != "allow" || d.Rule != "matrix-human" {
					t.Errorf("Check(task_set_priority, %q) = %s/%s (%s), want allow/matrix-human exactly — a "+
						"humanOnly tool that is not snapshot-gated is decided by Decide. deny/static-default here "+
						"means tools.Register does not register task_set_priority", tc.actor, d.Decision, d.Rule, d.Reason)
				}
				return
			}
			assertDeny(t, d, "human_only")
		})
	}
}

// Criterion 8. GREEN TODAY, AND MUST STAY GREEN. create_task and
// task_append_log keep the static fallback's allow/static-default for every
// caller: the orchestrator's rules, the capture engine, the worker consoles and
// Salvador's sessions all call them. The user-scope gate is the profile pin
// (C4), enforced in the validator and handler — never an actor rule, because
// the actor cannot tell this repo's session from another repo's (SPEC fact 10).
func TestMatrix_CaptureToolsUngated(t *testing.T) {
	checker := mcpVerbsChecker(t)
	ctx := context.Background()
	for _, tool := range []string{"create_task", "task_append_log"} {
		for _, actor := range []string{"mcp:manual:salvo", "mcp:acme", "orchestrator", "capture:gmail"} {
			tool, actor := tool, actor
			t.Run(tool+"/"+actor, func(t *testing.T) {
				d, err := checker.Check(ctx, policy.Request{Tool: tool, Actor: actor})
				if err != nil {
					t.Fatalf("Check(%s, %q): %v", tool, actor, err)
				}
				if d.Decision != "allow" || d.Rule != "static-default" {
					t.Errorf("Check(%s, %q) = %s/%s (%s), want allow/static-default exactly: SWT-38 leaves the "+
						"worker and spine contract unchanged; the user-scope gate is the profile pin (C4), not "+
						"policy", tool, actor, d.Decision, d.Rule, d.Reason)
				}
			})
		}
	}
}

// Criterion 9. GREEN TODAY (Decide allows any non-send-shaped tool for a
// human), AND MUST STAY GREEN. task_set_priority is not a channel action: it
// transmits nothing, so the kill switch (whose job is to stop SENDING) and the
// per-channel rate limit have no claim on it.
func TestDecide_TaskSetPriority_IgnoresKillSwitchAndRateLimit(t *testing.T) {
	frozenAndOverLimit := policy.Snapshot{
		SendingFrozen: true,
		Channel:       "gmail",
		HourlyLimit:   10,
		SentLastHour:  map[string]int{"gmail": 99},
	}
	d := policy.Decide(policy.Request{Tool: setPriorityTool, Actor: "mcp:manual:salvo"}, frozenAndOverLimit)
	if d.Decision != "allow" {
		t.Errorf("task_set_priority by mcp:manual:salvo with the kill switch ON and gmail over its limit = %s/%s "+
			"(%s), want allow — reordering the queue sends nothing (invariant 4)", d.Decision, d.Rule, d.Reason)
	}
}
