package policy_test

// SWT-40 Part B criterion B8's policy half (docs/tickets/inquiry-promote_SPEC.md,
// B-D1, "API / MCP tool changes"): route_candidate_add and
// route_candidate_remove are humanOnly, over EVERY actor shape in the repo — the
// SWT-37 fourteen-actor corpus (mcpVerbsCorpus), which is the IK's enumeration
// ("an actor-prefix check is a transport label": dashboard:, opsctl:,
// mcp:worker:, mcp:manual:, drafts:gpt, bare worker:, and the rest). Pure
// matrix core, ZERO I/O (invariant 7).
//
// WHY humanOnly: a candidate row is the AUTHORISATION to move a message into a
// project whose ai_locality may be wider than its origin (B-D1, IK SWT-21). An
// automated caller that could add one could route any mailbox's traffic into a
// project of its choosing — the capture_rule_add argument, verbatim.
//
// IMPOSED SURFACE:
//
//	// internal/policy/matrix.go
//	var humanOnly = map[string]bool{ …, "route_candidate_add": true, "route_candidate_remove": true }
//	// Not mcpHumanOnly, not sendShaped, not snapshotGated: nothing leaves the system.
//
// GREENFIELD NOTE — EXPECTED RED: neither name is in humanOnly, so Decide answers
// allow for the nine non-human actors; and neither is registered, so Check's
// static fallback denies the five humans (static-default).

import (
	"context"
	"testing"

	"github.com/sspataro57/switchboard/internal/policy"
)

var routeCandidateTools = []string{"route_candidate_add", "route_candidate_remove"}

func TestDecide_RouteCandidateTools_AreHumanOnly(t *testing.T) {
	for _, tool := range routeCandidateTools {
		for _, tc := range mcpVerbsCorpus {
			d := policy.Decide(policy.Request{Tool: tool, Actor: tc.actor}, policy.Snapshot{})
			if tc.human {
				if d.Decision != "allow" {
					t.Errorf("%s by %q = %s/%s, want allow (a human edits the candidate set)", tool, tc.actor, d.Decision, d.Rule)
				}
				continue
			}
			if d.Decision != "deny" || d.Rule != "human_only" {
				t.Errorf("%s by %q = %s/%s, want deny/human_only. B-D1: the candidate row is the authorisation to "+
					"move mail into a project; no automated or worker caller may write it", tool, tc.actor, d.Decision, d.Rule)
			}
		}
	}
}

// Through the production wiring: the matrix in front of the static allow-list
// built from the REAL registry. The loader fails the test if it ever runs:
// neither tool is snapshot-gated.
func TestCheck_RouteCandidateTools_AreHumanOnlyThroughTheRealRegistry(t *testing.T) {
	checker := mcpVerbsChecker(t)
	for _, tool := range routeCandidateTools {
		for _, tc := range mcpVerbsCorpus {
			d, err := checker.Check(context.Background(), policy.Request{Tool: tool, Actor: tc.actor})
			if err != nil {
				t.Fatalf("Check(%s, %q): %v", tool, tc.actor, err)
			}
			if tc.human {
				if d.Decision != "allow" {
					t.Errorf("Check %s by %q = %s/%s, want allow (is the tool registered?)", tool, tc.actor, d.Decision, d.Rule)
				}
				continue
			}
			if d.Decision != "deny" || d.Rule != "human_only" {
				t.Errorf("Check %s by %q = %s/%s, want deny/human_only", tool, tc.actor, d.Decision, d.Rule)
			}
		}
	}
}
