package policy_test

// The slack_reply AUTO tier in the delivery policy matrix (slack-auto-tier /
// SWT-77, docs/tickets/slack-auto-tier_SPEC.md, decisions D4, D5a, D7 and
// acceptance criteria 10, 11, 12, 14, 20, 24). Pure functions of (Request,
// Snapshot) plus the matrix's routing with a recording loader — ZERO I/O,
// invariant 7. The pgloader half (criterion 13, the channel pin) needs Postgres
// and lives in pgloader_slack_integration_test.go.
//
// WHY THE MATRIX IS THE GATE HERE. send_slack_reply drafts, approves and SENDS
// in one call, from both MCP profiles — including the worker consoles' (D6).
// There is no human between the caller and a real Slack conversation, so the
// kill switch, the hourly limit and the channel test are what remain. The
// calendar auto tier (matrix_calendar_test.go) is the precedent and this file
// copies its shape.
//
// GREENFIELD NOTE — EXPECTED RED. matrix.go does not know the tool name:
// send_slack_reply is in no map, so Decide returns allow/matrix-human for it
// BEFORE any channel test, kill switch or rate limit (matrix.go's
// `!sendShaped` fallthrough), and matrix.Check sends it to the static
// allow-list without ever running the loader. Every case below that asserts a
// deny, a matrix-send rule, or a loader call fails today — which is exactly
// the "auto tier with both brakes missing" D4 names.

import (
	"context"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/policy"
)

const slackAutoTool = "send_slack_reply"

// slackAutoActorShapes is every actor shape this repo produces (IK: "An
// actor-prefix check is a transport label, not a trust boundary" — enumerate,
// because one of them is usually the hole). D6: the verb is NOT actor-gated,
// so it must behave identically for all of them.
var slackAutoActorShapes = []string{
	"dashboard:salvo@example.com",
	"opsctl:salvo",
	"manual:salvo",
	"mcp:manual:salvo",
	"mcp:worker:acme",
	"mcp:avviato",
	"worker:acme",
	"drafts:gpt",
	"capture:google",
}

// slackAutoOtherChannels is criterion 14's list verbatim: every legal
// deliveries.channel value other than slack_reply, plus "" — what the loader
// produces when no channel resolves.
var slackAutoOtherChannels = []string{"gmail", "jira_comment", "upwork_chat", "calendar", "github_review", ""}

// ---------------------------------------------------------------------------
// Criterion 14 / D5a: denied BY NAME with channel_mismatch on every channel
// other than slack_reply, before the switch, with a reason naming the tool.
// ---------------------------------------------------------------------------

func TestDecide_SendSlackReply_ChannelMismatchOnEveryOtherChannel(t *testing.T) {
	for _, ch := range slackAutoOtherChannels {
		ch := ch
		name := ch
		if name == "" {
			name = "(empty channel)"
		}
		t.Run(name, func(t *testing.T) {
			snap := policy.Snapshot{SentLastHour: map[string]int{}, Channel: ch, HourlyLimit: 10}
			d := policy.Decide(policy.Request{Tool: slackAutoTool, Actor: "mcp:manual:salvo"}, snap)
			if d.Decision == "allow" {
				t.Fatalf("send_slack_reply on channel %q = allow/%q. D5a: it is denied BY NAME on every channel "+
					"but slack_reply. Once the verb is sendShaped the %q branch allows any sendShaped tool it "+
					"does not deny explicitly — and this verb drafts, approves AND sends in one call", ch, d.Rule, ch)
			}
			if d.Rule != "channel_mismatch" {
				t.Errorf("send_slack_reply on channel %q denied with rule %q, want channel_mismatch (its own rule "+
					"string, distinguishable from channel_not_live / channel_assisted in an audit)", ch, d.Rule)
			}
			if !strings.Contains(d.Reason, slackAutoTool) {
				t.Errorf("channel_mismatch reason %q does not name send_slack_reply (criterion 14: \"with a "+
					"reason naming this tool\" — book_calendar_block's reason names itself, so an operator must "+
					"be able to tell the two apart)", d.Reason)
			}
		})
	}
}

// The by-name deny must hold for automated actors too: the verb exists so they
// can call it, so the channel guard is the thing standing in that path — not
// an actor check (the calendar variant, matrix_calendar_test.go).
func TestDecide_SendSlackReply_ChannelGuardHoldsForEveryActor(t *testing.T) {
	for _, actor := range slackAutoActorShapes {
		actor := actor
		t.Run(actor, func(t *testing.T) {
			snap := policy.Snapshot{SentLastHour: map[string]int{}, Channel: "gmail", HourlyLimit: 10}
			d := policy.Decide(policy.Request{Tool: slackAutoTool, Actor: actor}, snap)
			if d.Decision == "allow" || d.Rule != "channel_mismatch" {
				t.Errorf("send_slack_reply on a gmail snapshot by %q = %s/%s, want deny/channel_mismatch", actor,
					d.Decision, d.Rule)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Criteria 10 and 12 (Decide half) and D6: on its own channel the verb reaches
// the slack_reply branch for EVERY actor — allowed with rule matrix-send, not
// the matrix-human fallthrough, and not human_only.
// ---------------------------------------------------------------------------

func TestDecide_SendSlackReply_AllowedOnSlackReplyForEveryActor(t *testing.T) {
	for _, actor := range slackAutoActorShapes {
		actor := actor
		t.Run(actor, func(t *testing.T) {
			d := policy.Decide(policy.Request{Tool: slackAutoTool, Actor: actor}, slackSnap(2, false))
			if d.Rule == "human_only" || d.Rule == "mcp_human_only" {
				t.Fatalf("send_slack_reply by %q = deny/%s. D6/criterion 10: the verb is in neither humanOnly nor "+
					"mcpHumanOnly — Salvador's call is \"auto for every conversation\", from both profiles", actor, d.Rule)
			}
			if d.Decision != "allow" {
				t.Fatalf("send_slack_reply by %q on a clean slack_reply snapshot = %s/%s/%s, want allow", actor,
					d.Decision, d.Rule, d.Reason)
			}
			if d.Rule != "matrix-send" {
				t.Errorf("send_slack_reply by %q allowed with rule %q, want matrix-send. \"matrix-human\" is the "+
					"not-sendShaped fallthrough: an allow BEFORE the channel switch, i.e. with no hourly limit "+
					"and no kill switch (D4)", actor, d.Rule)
			}
		})
	}
}

// Criterion 12, Decide half: the hourly limit binds at and over the limit.
func TestDecide_SendSlackReply_RateLimited(t *testing.T) {
	for _, n := range []int{10, 11, 50} {
		d := policy.Decide(policy.Request{Tool: slackAutoTool, Actor: "mcp:manual:salvo"}, slackSnap(n, false))
		assertDeny(t, d, "rate_limit")
	}
	if d := policy.Decide(policy.Request{Tool: slackAutoTool, Actor: "mcp:manual:salvo"}, slackSnap(9, false)); d.Decision != "allow" {
		t.Errorf("send_slack_reply at 9/10 = %s/%s, want allow", d.Decision, d.Rule)
	}
}

// HourlyLimit 0 means unset and falls back to 10, never "unlimited" — a zero
// that read as no-limit would remove the auto tier's only volume brake.
func TestDecide_SendSlackReply_UnsetHourlyLimitFallsBackToTen(t *testing.T) {
	snap := policy.Snapshot{SentLastHour: map[string]int{"slack_reply": 10}, Channel: "slack_reply", HourlyLimit: 0}
	assertDeny(t, policy.Decide(policy.Request{Tool: slackAutoTool, Actor: "mcp:manual:salvo"}, snap), "rate_limit")
}

// Criterion 11, Decide half: the kill switch stops it — for every actor, and
// even when the channel is under its limit. D4: with no human gate,
// set_sending_frozen is the only thing that can halt a session that decided
// to post.
func TestDecide_SendSlackReply_KillSwitch(t *testing.T) {
	for _, actor := range slackAutoActorShapes {
		d := policy.Decide(policy.Request{Tool: slackAutoTool, Actor: actor}, slackSnap(0, true))
		if d.Decision != "deny" || d.Rule != "kill_switch" {
			t.Errorf("send_slack_reply by %q with sending frozen = %s/%s, want deny/kill_switch (criterion 11)",
				actor, d.Decision, d.Rule)
		}
	}
}

// ---------------------------------------------------------------------------
// Criterion 20 / D7: matrix.Check routes send_slack_reply THROUGH the snapshot
// loader. The trap D7 names: a tool that reaches the static allow-list (as
// every mcpHumanOnly or unmapped tool does) never loads a snapshot, and so
// skips the kill switch, the rate limit and the channel branch while the
// static list happily allows it.
// ---------------------------------------------------------------------------

func TestMatrix_SendSlackReply_RoutesThroughTheLoader(t *testing.T) {
	ctx := context.Background()
	fallback := policy.NewStatic(slackAutoTool) // the static list WOULD allow it — routing is what's under test
	for _, actor := range slackAutoActorShapes {
		actor := actor
		t.Run(actor, func(t *testing.T) {
			l := &recordingLoader{snap: slackSnap(0, false)}
			d, err := policy.NewMatrix(l, fallback).Check(ctx, policy.Request{
				Tool: slackAutoTool, Actor: actor,
				Args: []byte(`{"task_id":1,"target_ref":"https://app.slack.com/client/T0360B84U/DSA806DHA","text":"hi"}`),
			})
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if !l.called {
				t.Fatalf("matrix.Check never ran the snapshot loader for send_slack_reply (actor %q); decision "+
					"%s/%s. D7: that is the static allow-list, which means no kill switch, no hourly limit and no "+
					"channel test for a verb that sends to Slack with no human in the loop", actor, d.Decision, d.Rule)
			}
			if d.Rule == "static-default" {
				t.Errorf("send_slack_reply decided by the static fallback (rule %q), want Decide's matrix-send", d.Rule)
			}
			if d.Decision != "allow" || d.Rule != "matrix-send" {
				t.Errorf("send_slack_reply by %q through Check = %s/%s, want allow/matrix-send", actor, d.Decision, d.Rule)
			}
		})
	}
}

// Criteria 11 and 12 through Check: the loader's frozen / at-limit snapshot
// reaches Decide, so the deny is the matrix's — not the static list's allow.
func TestMatrix_SendSlackReply_FrozenAndLimitedThroughCheck(t *testing.T) {
	ctx := context.Background()
	fallback := policy.NewStatic(slackAutoTool)
	for _, tc := range []struct {
		name string
		snap policy.Snapshot
		rule string
	}{
		{"frozen", slackSnap(0, true), "kill_switch"},
		{"at limit", slackSnap(10, false), "rate_limit"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			l := &recordingLoader{snap: tc.snap}
			d, err := policy.NewMatrix(l, fallback).Check(ctx, policy.Request{Tool: slackAutoTool, Actor: "mcp:manual:salvo"})
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			assertDeny(t, d, tc.rule)
		})
	}
}

// ---------------------------------------------------------------------------
// Criterion 24: the existing Slack verbs keep their policy entries. Every one
// of them stays human-gated for a worker console on a slack_reply row; adding
// an agent-callable send verb must not have widened any of them.
// ---------------------------------------------------------------------------

func TestDecide_SlackAutoTier_ExistingVerbsUnchanged(t *testing.T) {
	for _, tool := range []string{
		"send_delivery", "approve_delivery", "update_delivery", "prefill_delivery",
		"mark_delivery_sent", "mark_delivery_failed", "reject_delivery",
	} {
		for _, actor := range []string{"mcp:worker:acme", "drafts:gpt", "worker:acme"} {
			d := policy.Decide(policy.Request{Tool: tool, Actor: actor}, slackSnap(0, false))
			if d.Decision != "deny" || d.Rule != "human_only" {
				t.Errorf("%s by %q on a slack_reply row = %s/%s, want deny/human_only (criterion 24: D11 — an "+
					"agent may not send a row somebody else drafted)", tool, actor, d.Decision, d.Rule)
			}
		}
	}
	// And the human two-step still works on the channel.
	if d := policy.Decide(policy.Request{Tool: "send_delivery", Actor: humanActor}, slackSnap(0, false)); d.Decision != "allow" {
		t.Errorf("send_delivery on slack_reply by a human = %s/%s, want allow (the approve-tier path stays)", d.Decision, d.Rule)
	}
}
