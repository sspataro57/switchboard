package policy_test

// slack-watch-sweep (SWT-75) criterion 3's policy half: slack_watch_add,
// slack_watch_set_enabled and slack_watch_list are humanOnly — "an agent must
// not be able to point the browser at a conversation of its choosing, and
// browser time is a scarce shared resource" (SPEC, "API / MCP tool changes").
// Pure matrix core, ZERO I/O (invariant 7). Reuses humanActor / botActor /
// assertDeny from matrix_test.go (same package).
//
// The actor list is the SPEC's own demand: "refused for a `mcp:` actor AND for
// a bare `drafts:gpt`-shaped actor — the six-actor-shape test of IK's 'an
// actor-prefix check is a transport label' entry, not just the `mcp:` one".
// drafts:gpt is the counter-example that mattered: it calls the executor
// directly, off the MCP transport entirely, so a `ViaMCP`-shaped gate would do
// nothing to it.
//
// GREENFIELD NOTE, EXPECTED RED: none of the three names is in internal/policy's
// humanOnly map, so Decide returns allow ("matrix-human") for every actor.

import (
	"testing"

	"github.com/sspataro57/switchboard/internal/policy"
)

var slackWatchPolicyTools = []string{"slack_watch_add", "slack_watch_set_enabled", "slack_watch_list"}

func TestDecide_SlackWatchTools_HumanOnly(t *testing.T) {
	bots := []string{
		botActor,          // drafts:gpt — the direct, non-MCP autonomous caller
		"mcp:opsworker-x", // the MCP transport
		"mcp:worker:collab",
		"worker:collab",        // bare worker, no transport prefix
		"capture:slackweb",     // a connector-shaped actor: the watch loop's own pass
		"ghpoll:github",        // another connector shape
		"mcp:mcp:manual:salvo", // a doubled prefix is not a human
	}
	for _, tool := range slackWatchPolicyTools {
		for _, actor := range bots {
			tool, actor := tool, actor
			t.Run(tool+"/"+actor, func(t *testing.T) {
				d := policy.Decide(policy.Request{Tool: tool, Actor: actor}, policy.Snapshot{})
				assertDeny(t, d, "human_only")
			})
		}
	}
}

// The seeding path must work: `opsctl slack-watch add` (criterion 4) and a
// future dashboard form. If these were denied, the two rows the whole ticket
// exists to sweep could never be created.
func TestDecide_SlackWatchTools_HumanPrefixes(t *testing.T) {
	for _, tool := range slackWatchPolicyTools {
		for _, actor := range []string{"dashboard:salvo", "opsctl:salvo", "manual:salvo", "mcp:manual:salvo", humanActor} {
			d := policy.Decide(policy.Request{Tool: tool, Actor: actor}, policy.Snapshot{})
			if d.Decision != "allow" {
				t.Errorf("%s by %q = %q (rule %s), want allow — criterion 4 drives exactly these tools from "+
					"opsctl, audited as opsctl:$USER", tool, actor, d.Decision, d.Rule)
			}
		}
	}
}

// The watch LOOP itself calls no slack_watch_* tool: it READS the table
// directly, exactly as capture's loadRules reads capture_rules (invariant 3 in
// the SPEC's words — "a read of its own configuration"). What it does call
// through the executor is capture's pass tools, with a connector-shaped actor.
// Those must stay allowed, or a targeted pass could ingest a message and then
// create nothing.
func TestDecide_WatchPassTools_AllowTheWatchConnectorActor(t *testing.T) {
	for _, tool := range []string{"create_task", "link_external_ref", "task_append_log"} {
		d := policy.Decide(policy.Request{Tool: tool, Actor: "capture:slackweb-watch"}, policy.Snapshot{})
		if d.Decision != "allow" {
			t.Errorf("%s by capture:slackweb-watch = %q (rule %s), want allow. D9 gives the watcher its own "+
				"connector string so its MQTT client id cannot collide with the CronJob's; that must not "+
				"change what its capture pass is allowed to do", tool, d.Decision, d.Rule)
		}
	}
}
