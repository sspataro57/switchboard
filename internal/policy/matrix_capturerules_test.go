package policy_test

// Unit test for SWT-17 acceptance criterion 15's policy half: capture_rule_add and
// capture_rule_set_enabled are humanOnly — "an agent must not be able to redirect
// the funnel". Pure matrix core, ZERO I/O (invariant 7). Reuses humanActor /
// botActor / assertDeny from matrix_test.go (same package).
//
// GREENFIELD NOTE: fails today — neither name is in internal/policy's humanOnly
// map, so Decide returns allow ("matrix-human") for every actor. Expected red.

import (
	"testing"

	"github.com/sspataro57/switchboard/internal/policy"
)

var captureRuleTools = []string{"capture_rule_add", "capture_rule_set_enabled"}

// Every actor shape that EXISTS in this repo, not one representative bot. The
// institutional landmine is precise about why: "an actor-prefix check is a
// transport label, not a trust boundary", and the hole is always the shape nobody
// enumerated. drafts:gpt is the counter-example that mattered — it calls the
// executor directly, off the MCP transport entirely.
func TestDecide_CaptureRuleTools_HumanOnly(t *testing.T) {
	bots := []string{
		botActor,          // drafts:gpt — the direct, non-MCP autonomous caller
		"mcp:opsworker-x", // criterion 15's own example
		"mcp:worker:collab",
		"worker:collab", // bare worker, no transport prefix
		"ghpoll:github", // a connector-shaped actor
		"capture:slackweb",
		"mcp:mcp:manual:salvo", // a doubled prefix is not a human
	}
	for _, tool := range captureRuleTools {
		for _, actor := range bots {
			t.Run(tool+"/"+actor, func(t *testing.T) {
				d := policy.Decide(policy.Request{Tool: tool, Actor: actor}, policy.Snapshot{})
				assertDeny(t, d, "human_only")
			})
		}
	}
}

func TestDecide_CaptureRuleTools_HumanPrefixes(t *testing.T) {
	// The runbook seeds rules through opsctl; the future /capture page would use
	// dashboard:. Both must pass, or seeding the fixture rules is impossible.
	for _, tool := range captureRuleTools {
		for _, actor := range []string{"dashboard:salvo", "opsctl:salvo", "manual:salvo", "mcp:manual:salvo", humanActor} {
			d := policy.Decide(policy.Request{Tool: tool, Actor: actor}, policy.Snapshot{})
			if d.Decision != "allow" {
				t.Errorf("%s by %q = %q (rule %s), want allow", tool, actor, d.Decision, d.Rule)
			}
		}
	}
}

// The pass itself uses create_task / link_external_ref / task_append_log with a
// connector-shaped actor ("capture:{connector}", the ghpoll:github shape). Those
// are neither humanOnly nor sendShaped, so they must keep falling through to the
// static allow-list — if one of them were gated on a human actor, the engine could
// never create the task the whole ticket exists to create.
func TestDecide_CapturePassTools_AllowAConnectorActor(t *testing.T) {
	for _, tool := range []string{"create_task", "link_external_ref", "task_append_log"} {
		d := policy.Decide(policy.Request{Tool: tool, Actor: "capture:slackweb"}, policy.Snapshot{})
		if d.Decision != "allow" {
			t.Errorf("%s by capture:slackweb = %q (rule %s), want allow", tool, d.Decision, d.Rule)
		}
	}
}
