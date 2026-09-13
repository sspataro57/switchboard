package policy_test

// SWT-45 (docs/tickets/jira-activity-revive_SPEC.md) J7 and criterion 15's
// policy half: task_mark_surfaced is NOT humanOnly (capture calls it as
// capture:{connector}), is not snapshotGated, and has no rule of its own — it
// falls through the matrix to the static allow-list, exactly as task_reopen and
// task_set_source_thread do. Pure matrix core, ZERO I/O (invariant 7). Same
// shape as matrix_reopen_test.go, whose reopenChecker / reopenLoader this file
// reuses.
//
// RED TODAY: the allow-list is built from the REAL registry, where the tool is
// not registered — so the capture actor gets deny/static-default "tool not in
// registered set". A list the test supplied itself would allow the tool no
// matter what the repo does.
//
// AS WITH task_reopen, NOTHING HERE CLAIMS THE ACTOR PREFIX IS A BOUNDARY. The
// gate is the MCP surface (surfaced_test.go in internal/tools); the enumeration
// below proves the matrix does not key on the caller.

import (
	"context"
	"testing"

	"github.com/sspataro57/switchboard/internal/policy"
)

func TestDecide_TaskMarkSurfaced_FallsThroughToStaticDefault(t *testing.T) {
	checker := reopenChecker(t)
	ctx := context.Background()
	for _, actor := range []string{
		"capture:google", // the J2 caller — Treetop Jira mail arrives on a google account
		"capture:jira",
		"capture:slackweb", // the owner's 2026-09-12 answer: a Slack mention is activity too
		"capture:upworkcrm",
		"capture:rules", // capture.DefaultRulesActor
		"opsctl:salvo",  // a hand-run `capture-rules run --live`
	} {
		actor := actor
		t.Run(actor, func(t *testing.T) {
			d, err := checker.Check(ctx, policy.Request{Tool: "task_mark_surfaced", Actor: actor})
			if err != nil {
				t.Fatalf("Check(task_mark_surfaced, %q) errored: %v", actor, err)
			}
			if d.Decision != "allow" || d.Rule != "static-default" {
				t.Errorf("task_mark_surfaced by %q = %s/%s, want allow/static-default. J7: capture calls it as "+
					"capture:{connector}; a human-only gate would make every overriding creation die in the same "+
					"jira tick (SWT-32 D8's same-tick close)", actor, d.Decision, d.Rule)
			}
		})
	}
}

// Asserted, not assumed: the natural place to add a new tool is beside
// task_dismiss in humanOnly.
func TestDecide_TaskMarkSurfaced_IsNotHumanOnly(t *testing.T) {
	d := policy.Decide(policy.Request{Tool: "task_mark_surfaced", Actor: "capture:google"}, policy.Snapshot{})
	if d.Decision != "allow" || d.Rule == "human_only" {
		t.Errorf("policy.Decide(task_mark_surfaced, capture:google) = %s/%s, want allow and no human_only rule — "+
			"J7: 'It is not humanOnly (capture calls it)'", d.Decision, d.Rule)
	}
}
