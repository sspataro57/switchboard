package policy_test

// activity-resurfaces (SWT-72, docs/tickets/activity-resurfaces_SPEC.md) D3 and
// criterion 7's policy half: task_mark_activity is NOT humanOnly (capture calls
// it as capture:{connector} and the promoter as promote:{lane}), is not
// mcpHumanOnly and not snapshotGated, and has no rule of its own — it falls
// through the matrix to the static allow-list, exactly as its sibling
// task_mark_surfaced does (matrix_surfaced_test.go's shape, which this file
// copies down to its reason).
//
// Pure matrix core, ZERO I/O (invariant 7). Reuses reopenChecker /
// reopenLoader from matrix_reopen_test.go.
//
// RED TODAY: the allow-list is built from the REAL registry, where the tool is
// not registered — so every actor gets deny/static-default "tool not in
// registered set". A list the test supplied itself would allow the tool no
// matter what the repo does.
//
// NOTHING HERE CLAIMS THE ACTOR PREFIX IS A BOUNDARY (the standing IK rule).
// The gate is the MCP surface (internal/tools' TestTaskMarkActivity_StaysOffBothMCPProfiles);
// the enumeration below proves the matrix does not key on the caller.
//
// MUTATION: add task_mark_activity to humanOnly -> every capture and promote
// row here, and criteria 8-9's capture/promote hooks die in production on the
// first pass.

import (
	"context"
	"testing"

	"github.com/sspataro57/switchboard/internal/policy"
)

const markActivityTool = "task_mark_activity"

// Criterion 7: "Decide returns allow/static-default for capture:google,
// promote:classify, dashboard:salvo and mcp:treetop alike."
func TestDecide_TaskMarkActivity_FallsThroughToStaticDefault(t *testing.T) {
	checker := reopenChecker(t)
	ctx := context.Background()
	for _, actor := range []string{
		"capture:google",    // shapes 1 and 2: Jira notification mail and José's direct mail
		"capture:jira",      //
		"capture:slackweb",  // shape 4: a Slack rule files onto the same task
		"capture:upworkcrm", //
		"capture:rules",     // capture.DefaultRulesActor
		"promote:classify",  // D3's personal-lane caller
		"promote:inquiry",   // the dismissed-task attach (a skip by construction)
		"dashboard:salvo",   // the SPEC's own list
		"mcp:treetop",       // a worker console: allowed by the MATRIX, refused by the TRANSPORT
		"opsctl:salvo",      // a hand-run `capture-rules run --live`
	} {
		actor := actor
		t.Run(actor, func(t *testing.T) {
			d, err := checker.Check(ctx, policy.Request{Tool: markActivityTool, Actor: actor})
			if err != nil {
				t.Fatalf("Check(%s, %q) errored: %v", markActivityTool, actor, err)
			}
			if d.Decision != "allow" || d.Rule != "static-default" {
				t.Errorf("%s by %q = %s/%s, want allow/static-default. D3: capture calls it as capture:{connector} "+
					"and promote as promote:{lane}; a human-only gate would make every rule-filed attach fail its "+
					"pass (the appendRuleLog policy: an error fails the pass)", markActivityTool, actor, d.Decision, d.Rule)
			}
		})
	}
}

// Asserted, not assumed: the natural place to add a new tool is beside
// task_dismiss in humanOnly, and D3 says explicitly it does not go there.
func TestDecide_TaskMarkActivity_IsNotHumanOnly(t *testing.T) {
	for _, actor := range []string{"capture:google", "promote:classify", "orchestrator"} {
		d := policy.Decide(policy.Request{Tool: markActivityTool, Actor: actor}, policy.Snapshot{})
		if d.Decision != "allow" || d.Rule == "human_only" || d.Rule == "mcp_human_only" {
			t.Errorf("policy.Decide(%s, %s) = %s/%s, want allow with no human gate — D3: \"not humanOnly "+
				"(capture and promote call it)\"", markActivityTool, actor, d.Decision, d.Rule)
		}
	}
}

// GREEN BY DESIGN AND MUST STAY GREEN: marking activity transmits nothing, so
// the kill switch (whose job is to stop SENDING) and the per-channel rate limit
// have no claim on it (invariant 4). reopenLoader already fails the test if the
// matrix reaches the snapshot loader at all.
func TestDecide_TaskMarkActivity_IgnoresKillSwitchAndRateLimit(t *testing.T) {
	frozen := policy.Snapshot{SendingFrozen: true, Channel: "gmail", HourlyLimit: 10,
		SentLastHour: map[string]int{"gmail": 99}}
	d := policy.Decide(policy.Request{Tool: markActivityTool, Actor: "capture:google"}, frozen)
	if d.Decision != "allow" {
		t.Errorf("%s with the kill switch ON = %s/%s (%s), want allow — a board marker sends nothing "+
			"(invariant 4)", markActivityTool, d.Decision, d.Rule, d.Reason)
	}
}
