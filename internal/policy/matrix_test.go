package policy_test

// Unit tests for the SWT-8 delivery policy matrix (SPEC 08-draft-deliveries,
// acceptance criterion 4). The matrix CORE is a pure function of
// (Request, Snapshot) with ZERO I/O (invariant 7): kill switch, per-channel
// hourly rate limit, channel tiers (assisted / not-live), and the human-only
// action gate. The pg snapshot loader owns the I/O and is exercised in the
// delivery lifecycle integration test — here everything is offline.
//
// GREENFIELD NOTE: internal/policy gains matrix.go this step; this file
// compile-FAILs under `go test ./...` until it exists. For greenfield code the
// SPEC's contract IS the signature. Imposed exported surface (matrix.go):
//
//   // Request gains Args so the loader can parse the delivery id -> channel
//   // (SPEC "policy.Request gains Args"). The executor passes call.Args through.
//   type Request struct { Tool, Actor string; TaskID *int64; Args json.RawMessage }
//
//   // Snapshot is the read-only world the loader gathered for one delivery call.
//   type Snapshot struct {
//       SendingFrozen bool           // ops_flags 'sending_frozen'
//       SentLastHour  map[string]int // channel -> deliveries sent in the last hour
//       Channel       string         // the delivery's channel (resolved from Args)
//       HourlyLimit   int            // default 10, OPS_SEND_HOURLY_LIMIT override
//   }
//
//   // Decide is the pure matrix core over the send-shaped / human-only tools.
//   func Decide(req Request, snap Snapshot) Decision
//
//   // Matrix wraps the static allow-list: delivery-gated tools go through Decide
//   // (snapshot from the loader); every other tool falls through to the fallback.
//   type SnapshotLoader interface { Load(ctx context.Context, req Request) (Snapshot, error) }
//   func NewMatrix(loader SnapshotLoader, fallback Checker) Checker
//
// Rule names are pinned by the SPEC (kill_switch, rate_limit, channel_assisted,
// channel_not_live, human_only); reasons are only checked non-empty on deny.
//
// NOTE (deviation from the launching task): the task asked for a "needs_approval
// when not approved" Decide case. The SPEC places the approved-status gate in the
// send_delivery HANDLER (criterion 5: "In-tx: ... require approved"), not in the
// pure matrix — the matrix rule set is exactly {kill_switch, rate_limit,
// channel_assisted, channel_not_live, human_only}. That approval gate is
// therefore encoded in the integration test (send-before-approve refused), not
// here. Imposing a matrix approval rule the implementer is not building would
// leave a test permanently red.
//
// SWT-12 (slack-send-promotion) adds the slack_reply promotion below: see
// TestDecide_SlackReplySend_Promoted, TestDecide_SlackReply_Q4FourCorners, and
// TestDecide_MarkDeliveryFailed_HumanOnlyAndNotFreezeGated.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sspataro57/switchboard/internal/policy"
)

// humanActor / botActor exercise the actor-prefix rule (SPEC criterion 4:
// dashboard: / opsctl: / manual: are human; anything else is a bot).
// workerMCPActor is an autonomous execution worker arriving over the MCP
// transport — the identity the human-only gate exists to refuse (SWT-12 Q1:
// MCP-LISTING a tool must not make it worker-callable).
const (
	humanActor     = "dashboard:salvo@example.com"
	botActor       = "drafts:gpt"
	workerMCPActor = "mcp:worker:avviato"
)

func gmailSnap(sentThisHour int, frozen bool) policy.Snapshot {
	return policy.Snapshot{
		SendingFrozen: frozen,
		SentLastHour:  map[string]int{"gmail": sentThisHour},
		Channel:       "gmail",
		HourlyLimit:   10,
	}
}

// jiraSnap is the SWT-9 jira_comment snapshot (now a LIVE channel, gmail shape).
func jiraSnap(sentThisHour int, frozen bool) policy.Snapshot {
	return policy.Snapshot{
		SendingFrozen: frozen,
		SentLastHour:  map[string]int{"jira_comment": sentThisHour},
		Channel:       "jira_comment",
		HourlyLimit:   10,
	}
}

// slackSnap is the SWT-12 slack_reply snapshot (promoted assisted -> approve).
func slackSnap(sentThisHour int, frozen bool) policy.Snapshot {
	return policy.Snapshot{
		SendingFrozen: frozen,
		SentLastHour:  map[string]int{"slack_reply": sentThisHour},
		Channel:       "slack_reply",
		HourlyLimit:   10,
	}
}

// ---- SWT-9: jira_comment is live (criterion 6) --------------------------------

// send_delivery on jira_comment is ALLOWED for a human under limit + not frozen
// — the first channel_not_live graduation. Kill switch + the shared per-channel
// hourly rate limit still apply (gmail shape).
func TestDecide_JiraCommentSend_LiveLikeGmail(t *testing.T) {
	req := policy.Request{Tool: "send_delivery", Actor: humanActor}

	t.Run("under limit, not frozen -> allow", func(t *testing.T) {
		d := policy.Decide(req, jiraSnap(2, false))
		if d.Decision != "allow" {
			t.Fatalf("jira_comment send (human, under limit) = %q/%q, want allow (channel is live now)", d.Decision, d.Rule)
		}
	})
	t.Run("at limit -> rate_limit", func(t *testing.T) {
		assertDeny(t, policy.Decide(req, jiraSnap(10, false)), "rate_limit")
	})
	t.Run("frozen -> kill_switch", func(t *testing.T) {
		assertDeny(t, policy.Decide(req, jiraSnap(0, true)), "kill_switch")
	})
}

func assertDeny(t *testing.T, d policy.Decision, wantRule string) {
	t.Helper()
	if d.Decision != "deny" {
		t.Errorf("Decision = %q, want deny (rule %s)", d.Decision, wantRule)
	}
	if d.Rule != wantRule {
		t.Errorf("Rule = %q, want %q", d.Rule, wantRule)
	}
	if d.Reason == "" {
		t.Errorf("deny (%s) must carry a non-empty reason", wantRule)
	}
}

// ---- gmail send: the happy path -----------------------------------------------

func TestDecide_GmailSend_AllowedWhenHumanUnderLimitNotFrozen(t *testing.T) {
	req := policy.Request{Tool: "send_delivery", Actor: humanActor}
	d := policy.Decide(req, gmailSnap(3, false))
	if d.Decision != "allow" {
		t.Fatalf("gmail send (human, under limit, not frozen) = %q/%q/%q, want allow",
			d.Decision, d.Rule, d.Reason)
	}
	if d.Rule == "" {
		t.Errorf("allow must record which rule allowed it (empty Rule)")
	}
}

// ---- kill switch --------------------------------------------------------------

// SWT-12 Q4: "the kill switch is for switchboard." The freeze governs what
// switchboard itself puts in front of a client, so it denies send_delivery on
// every channel — and deliberately does NOT deny mark_delivery_sent, which
// transmits nothing and records a send another route already made.
func TestDecide_KillSwitch_DeniesSendingNotRecording(t *testing.T) {
	for _, snap := range []policy.Snapshot{gmailSnap(0, true), jiraSnap(0, true), slackSnap(0, true)} {
		snap := snap
		t.Run(snap.Channel+"/send_delivery denied", func(t *testing.T) {
			d := policy.Decide(policy.Request{Tool: "send_delivery", Actor: humanActor}, snap)
			assertDeny(t, d, "kill_switch")
		})
		t.Run(snap.Channel+"/mark_delivery_sent allowed", func(t *testing.T) {
			d := policy.Decide(policy.Request{Tool: "mark_delivery_sent", Actor: humanActor}, snap)
			if d.Decision != "allow" {
				t.Fatalf("mark_delivery_sent on frozen %s = %q/%q, want allow — recording a send made by "+
					"another route was never switchboard's to prevent (SWT-12 Q4); refusing it only leaves a "+
					"message provably in a client channel with no row saying so",
					snap.Channel, d.Decision, d.Rule)
			}
		})
	}
}

// ---- per-channel hourly rate limit --------------------------------------------

func TestDecide_RateLimit_DeniesAtOrOverLimit(t *testing.T) {
	req := policy.Request{Tool: "send_delivery", Actor: humanActor}

	t.Run("under limit allows", func(t *testing.T) {
		if d := policy.Decide(req, gmailSnap(9, false)); d.Decision != "allow" {
			t.Errorf("9/10 sent this hour = %q (rule %s), want allow", d.Decision, d.Rule)
		}
	})
	t.Run("at limit denies", func(t *testing.T) {
		assertDeny(t, policy.Decide(req, gmailSnap(10, false)), "rate_limit")
	})
	t.Run("over limit denies", func(t *testing.T) {
		assertDeny(t, policy.Decide(req, gmailSnap(11, false)), "rate_limit")
	})
}

// ---- channel tiers ------------------------------------------------------------

func TestDecide_UpworkChatSend_DeniedAssisted(t *testing.T) {
	snap := policy.Snapshot{SentLastHour: map[string]int{}, Channel: "upwork_chat", HourlyLimit: 10}
	d := policy.Decide(policy.Request{Tool: "send_delivery", Actor: humanActor}, snap)
	assertDeny(t, d, "channel_assisted")
}

// SWT-12 criterion 13: slack_reply DROPS the channel_assisted denial of
// send_delivery. Switchboard now clicks Send through the Slack Web bridge after
// approval; the assisted verbs (prefill_delivery / mark_delivery_sent) survive as
// fallbacks. Shape: hourly rate_limit for both send_delivery and
// mark_delivery_sent, kill switch for send_delivery ONLY, then allow.
func TestDecide_SlackReplySend_Promoted(t *testing.T) {
	req := policy.Request{Tool: "send_delivery", Actor: humanActor}

	t.Run("under limit, not frozen -> allow", func(t *testing.T) {
		d := policy.Decide(req, slackSnap(0, false))
		if d.Decision != "allow" {
			t.Fatalf("slack_reply send_delivery (human, under limit, not frozen) = %q/%q/%q, want allow — "+
				"the channel_assisted denial is gone (SWT-12 criterion 13)", d.Decision, d.Rule, d.Reason)
		}
		if d.Rule == "channel_assisted" {
			t.Fatalf("slack_reply still carries the channel_assisted rule; the promotion did not land")
		}
	})
	t.Run("at limit -> rate_limit", func(t *testing.T) {
		assertDeny(t, policy.Decide(req, slackSnap(10, false)), "rate_limit")
	})
	t.Run("over limit -> rate_limit", func(t *testing.T) {
		assertDeny(t, policy.Decide(req, slackSnap(11, false)), "rate_limit")
	})
	t.Run("frozen -> kill_switch", func(t *testing.T) {
		assertDeny(t, policy.Decide(req, slackSnap(0, true)), "kill_switch")
	})
	t.Run("bot actor -> human_only", func(t *testing.T) {
		assertDeny(t, policy.Decide(policy.Request{Tool: "send_delivery", Actor: botActor}, slackSnap(0, false)), "human_only")
	})
	t.Run("mark_delivery_sent survives as the assisted fallback", func(t *testing.T) {
		d := policy.Decide(policy.Request{Tool: "mark_delivery_sent", Actor: humanActor}, slackSnap(0, false))
		if d.Decision != "allow" {
			t.Fatalf("slack_reply mark_delivery_sent = %q/%q, want allow", d.Decision, d.Rule)
		}
	})
	t.Run("prefill_delivery keeps its human-only allow", func(t *testing.T) {
		// Not send-shaped, so it never reaches the channel switch (SWT-13's
		// recorded accepted risk: the freeze does not gate prefill).
		if d := policy.Decide(policy.Request{Tool: "prefill_delivery", Actor: humanActor}, slackSnap(0, true)); d.Decision != "allow" {
			t.Errorf("prefill_delivery = %q/%q, want allow (the assisted verb survives as a fallback)", d.Decision, d.Rule)
		}
		assertDeny(t, policy.Decide(policy.Request{Tool: "prefill_delivery", Actor: botActor}, slackSnap(0, false)), "human_only")
	})
}

// TestDecide_SlackReply_Q4FourCorners is the SUBTLE one. SWT-12 Q4 splits two
// concepts that matrix.go currently conflates (`snapshotGated = sendShaped`,
// matrix.go:34):
//
//   - snapshot-gated: the loader runs, the hourly rate limit applies, and the
//     channel branch is reached. BOTH send_delivery and mark_delivery_sent.
//   - freeze-gated: snap.SendingFrozen denies. send_delivery ONLY.
//
// IMPLEMENTATION TRAP the SPEC names explicitly: deleting mark_delivery_sent
// from `sendShaped` makes the frozen+record corner pass while SILENTLY dropping
// the tool's rate limit and its whole channel branch — Decide returns
// allow/matrix-human for anything not in sendShaped BEFORE the channel switch
// (matrix.go:60). That widens the tool while appearing to narrow it. The
// over-limit+record corner below is the one that catches it: if it fails while
// frozen+record passes, the one-line deletion was the implementation.
func TestDecide_SlackReply_Q4FourCorners(t *testing.T) {
	t.Run("frozen + send_delivery = deny/kill_switch", func(t *testing.T) {
		d := policy.Decide(policy.Request{Tool: "send_delivery", Actor: humanActor}, slackSnap(0, true))
		assertDeny(t, d, "kill_switch")
	})

	t.Run("frozen + mark_delivery_sent = ALLOW", func(t *testing.T) {
		d := policy.Decide(policy.Request{Tool: "mark_delivery_sent", Actor: humanActor}, slackSnap(0, true))
		if d.Decision != "allow" {
			t.Fatalf("frozen + mark_delivery_sent = %q/%q/%q, want allow — recording is exempt from the "+
				"kill switch (Q4: the kill switch is for switchboard)", d.Decision, d.Rule, d.Reason)
		}
	})

	t.Run("over-limit + mark_delivery_sent = deny/rate_limit", func(t *testing.T) {
		d := policy.Decide(policy.Request{Tool: "mark_delivery_sent", Actor: humanActor}, slackSnap(10, true))
		if d.Decision == "allow" {
			t.Fatalf("over-limit + mark_delivery_sent = allow/%q. This is the trap the SPEC names: "+
				"mark_delivery_sent was probably REMOVED from sendShaped instead of being kept "+
				"snapshot-gated-but-not-freeze-gated, so it no longer reaches the channel branch and lost "+
				"its hourly rate limit entirely. Keep it in a snapshotGated map holding BOTH tools and "+
				"consult snap.SendingFrozen only for send_delivery.", d.Rule)
		}
		assertDeny(t, d, "rate_limit")
	})

	t.Run("worker actor + mark_delivery_sent = deny/human_only", func(t *testing.T) {
		// SWT-12 Q1: mark_delivery_sent becomes MCP-LISTED so an interactive
		// session can record a manual send in one call. Listing must NOT make it
		// worker-callable — humanOnly is what keeps an autonomous console out.
		d := policy.Decide(policy.Request{Tool: "mark_delivery_sent", Actor: workerMCPActor}, slackSnap(0, false))
		assertDeny(t, d, "human_only")
	})
}

// SWT-12 criterion 13: mark_delivery_failed joins humanOnly and is NOT
// send-shaped — it moves a row AWAY from the world (verified-unsent -> failed),
// so the kill switch has nothing to prevent and must not block the one verb that
// un-sticks a frozen-in-'sending' row.
func TestDecide_MarkDeliveryFailed_HumanOnlyAndNotFreezeGated(t *testing.T) {
	t.Run("human, frozen -> allow (not freeze-gated)", func(t *testing.T) {
		d := policy.Decide(policy.Request{Tool: "mark_delivery_failed", Actor: humanActor}, slackSnap(0, true))
		if d.Decision != "allow" {
			t.Fatalf("mark_delivery_failed while frozen = %q/%q/%q, want allow — it is not send-shaped; "+
				"freeze-gating it would make a stuck 'sending' row unrecoverable during exactly the "+
				"incident a freeze accompanies", d.Decision, d.Rule, d.Reason)
		}
	})
	t.Run("human, at limit -> allow (no rate limit on un-sending)", func(t *testing.T) {
		if d := policy.Decide(policy.Request{Tool: "mark_delivery_failed", Actor: humanActor}, slackSnap(99, false)); d.Decision != "allow" {
			t.Errorf("mark_delivery_failed at limit = %q/%q, want allow (not send-shaped)", d.Decision, d.Rule)
		}
	})
	t.Run("bot actor -> deny/human_only", func(t *testing.T) {
		assertDeny(t, policy.Decide(policy.Request{Tool: "mark_delivery_failed", Actor: botActor}, slackSnap(0, false)), "human_only")
	})
	t.Run("worker over MCP -> deny/human_only", func(t *testing.T) {
		assertDeny(t, policy.Decide(policy.Request{Tool: "mark_delivery_failed", Actor: workerMCPActor}, slackSnap(0, false)), "human_only")
	})
}

// SWT-9: jira_comment graduated OUT of channel_not_live (see
// TestDecide_JiraCommentSend_LiveLikeGmail). Only calendar + github_review remain.
func TestDecide_NotLiveChannels_DeniedNotLive(t *testing.T) {
	for _, ch := range []string{"calendar", "github_review"} {
		t.Run(ch, func(t *testing.T) {
			snap := policy.Snapshot{SentLastHour: map[string]int{}, Channel: ch, HourlyLimit: 10}
			d := policy.Decide(policy.Request{Tool: "send_delivery", Actor: humanActor}, snap)
			assertDeny(t, d, "channel_not_live")
		})
	}
}

// ---- human-only actions -------------------------------------------------------

func TestDecide_HumanOnly_DeniesBotActors(t *testing.T) {
	// The spine-facing mutators are human-only (SPEC criterion 4; SWT-12 adds
	// mark_delivery_failed). A bot actor prefix (not dashboard:/opsctl:/manual:)
	// is denied regardless of the (otherwise-permissive) snapshot.
	snap := gmailSnap(0, false)
	for _, tool := range []string{
		"update_delivery", "approve_delivery", "send_delivery",
		"mark_delivery_sent", "mark_delivery_failed", "prefill_delivery", "set_sending_frozen",
	} {
		t.Run(tool+"/bot denied", func(t *testing.T) {
			d := policy.Decide(policy.Request{Tool: tool, Actor: botActor}, snap)
			assertDeny(t, d, "human_only")
		})
		t.Run(tool+"/worker over MCP denied", func(t *testing.T) {
			d := policy.Decide(policy.Request{Tool: tool, Actor: workerMCPActor}, snap)
			assertDeny(t, d, "human_only")
		})
		t.Run(tool+"/human allowed", func(t *testing.T) {
			// approve/update/set_frozen/mark_failed have no channel/rate gate;
			// send passes because the snapshot is gmail/under-limit/not-frozen.
			if d := policy.Decide(policy.Request{Tool: tool, Actor: humanActor}, snap); d.Decision != "allow" {
				t.Errorf("%s by human = %q (rule %s), want allow", tool, d.Decision, d.Rule)
			}
		})
	}
}

// ---- Matrix routing: delivery tools -> Decide, everything else -> fallback -----

// recordingLoader is a fake SnapshotLoader: it records whether Load ran and
// returns a canned snapshot — no pg, no I/O.
type recordingLoader struct {
	called bool
	snap   policy.Snapshot
}

func (l *recordingLoader) Load(_ context.Context, _ policy.Request) (policy.Snapshot, error) {
	l.called = true
	return l.snap, nil
}

func TestMatrix_RoutesDeliveryToolsThroughLoader(t *testing.T) {
	ctx := context.Background()
	// fallback statically allows all these names, so any denial/allow difference
	// is the ROUTING (Decide vs fallback), not the allow-list.
	fallback := policy.NewStatic("create_task", "draft_delivery", "send_delivery", "mark_delivery_sent")

	t.Run("non-delivery tool falls through to fallback (loader untouched)", func(t *testing.T) {
		l := &recordingLoader{snap: gmailSnap(0, false)}
		m := policy.NewMatrix(l, fallback)
		d, err := m.Check(ctx, policy.Request{Tool: "create_task", Actor: humanActor})
		if err != nil {
			t.Fatalf("Check(create_task): %v", err)
		}
		if d.Decision != "allow" {
			t.Errorf("create_task = %q, want allow (static fallback preserved)", d.Decision)
		}
		if l.called {
			t.Errorf("loader must NOT run for a non-delivery tool")
		}
	})

	t.Run("draft_delivery is agent-facing: falls through, not human-gated", func(t *testing.T) {
		l := &recordingLoader{snap: gmailSnap(0, false)}
		m := policy.NewMatrix(l, fallback)
		// The draft worker actor is a bot; draft_delivery must still be allowed
		// (it is NOT in the human-only set) — so it cannot route through Decide.
		d, err := m.Check(ctx, policy.Request{Tool: "draft_delivery", Actor: botActor})
		if err != nil {
			t.Fatalf("Check(draft_delivery): %v", err)
		}
		if d.Decision != "allow" {
			t.Errorf("draft_delivery by drafts:gpt = %q, want allow", d.Decision)
		}
		if l.called {
			t.Errorf("draft_delivery must not route through the send-snapshot loader")
		}
	})

	t.Run("send_delivery routes through the loader + Decide", func(t *testing.T) {
		l := &recordingLoader{snap: gmailSnap(0, true /*frozen*/)}
		m := policy.NewMatrix(l, fallback)
		d, err := m.Check(ctx, policy.Request{
			Tool:  "send_delivery",
			Actor: humanActor,
			Args:  json.RawMessage(`{"delivery_id":1}`),
		})
		if err != nil {
			t.Fatalf("Check(send_delivery): %v", err)
		}
		if !l.called {
			t.Errorf("send_delivery must consult the snapshot loader")
		}
		// Frozen snapshot -> Decide denies kill_switch (proves routing to Decide,
		// NOT the static fallback which would have allowed it).
		assertDeny(t, d, "kill_switch")
	})

	// SWT-12 Q4: mark_delivery_sent stays SNAPSHOT-gated even though it stops
	// being freeze-gated. If the implementation drops it from the snapshotGated
	// map, the loader never runs and the rate limit disappears — this is the
	// Matrix-level twin of the four-corners rate_limit case.
	t.Run("mark_delivery_sent still routes through the loader (rate limit alive)", func(t *testing.T) {
		l := &recordingLoader{snap: slackSnap(10, false)}
		m := policy.NewMatrix(l, fallback)
		d, err := m.Check(ctx, policy.Request{
			Tool:  "mark_delivery_sent",
			Actor: humanActor,
			Args:  json.RawMessage(`{"delivery_id":1}`),
		})
		if err != nil {
			t.Fatalf("Check(mark_delivery_sent): %v", err)
		}
		if !l.called {
			t.Fatalf("mark_delivery_sent did not consult the snapshot loader; it must stay snapshot-gated " +
				"(only the FREEZE check is dropped, not the channel/rate logic)")
		}
		assertDeny(t, d, "rate_limit")
	})

	// mark_delivery_failed is human-only but NOT snapshot-gated: no loader, no
	// channel, no rate limit, no freeze.
	t.Run("mark_delivery_failed is human-gated without the loader", func(t *testing.T) {
		l := &recordingLoader{snap: slackSnap(99, true)}
		m := policy.NewMatrix(l, policy.NewStatic("mark_delivery_failed"))
		d, err := m.Check(ctx, policy.Request{
			Tool:  "mark_delivery_failed",
			Actor: humanActor,
			Args:  json.RawMessage(`{"delivery_id":1}`),
		})
		if err != nil {
			t.Fatalf("Check(mark_delivery_failed): %v", err)
		}
		if d.Decision != "allow" {
			t.Errorf("mark_delivery_failed by human = %q/%q, want allow", d.Decision, d.Rule)
		}
		if l.called {
			t.Errorf("mark_delivery_failed must not consult the send-snapshot loader")
		}
		dd, err := m.Check(ctx, policy.Request{Tool: "mark_delivery_failed", Actor: workerMCPActor})
		if err != nil {
			t.Fatalf("Check(mark_delivery_failed, worker): %v", err)
		}
		assertDeny(t, dd, "human_only")
	})
}
