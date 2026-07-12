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

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sspataro57/switchboard/internal/policy"
)

// humanActor / botActor exercise the actor-prefix rule (SPEC criterion 4:
// dashboard: / opsctl: / manual: are human; anything else is a bot).
const (
	humanActor = "dashboard:salvo@example.com"
	botActor   = "drafts:gpt"
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

// ---- kill switch denies EVERYTHING send-shaped --------------------------------

func TestDecide_KillSwitch_DeniesAllSendShaped(t *testing.T) {
	for _, tool := range []string{"send_delivery", "mark_delivery_sent"} {
		t.Run(tool, func(t *testing.T) {
			snap := policy.Snapshot{
				SendingFrozen: true,
				SentLastHour:  map[string]int{},
				Channel:       "gmail", // channel irrelevant; kill switch is global
				HourlyLimit:   10,
			}
			d := policy.Decide(policy.Request{Tool: tool, Actor: humanActor}, snap)
			assertDeny(t, d, "kill_switch")
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
	// The five spine-facing mutators are human-only (SPEC criterion 4). A bot
	// actor prefix (not dashboard:/opsctl:/manual:) is denied regardless of the
	// (otherwise-permissive) snapshot.
	snap := gmailSnap(0, false)
	for _, tool := range []string{
		"update_delivery", "approve_delivery", "send_delivery",
		"mark_delivery_sent", "set_sending_frozen",
	} {
		t.Run(tool+"/bot denied", func(t *testing.T) {
			d := policy.Decide(policy.Request{Tool: tool, Actor: botActor}, snap)
			assertDeny(t, d, "human_only")
		})
		t.Run(tool+"/human allowed", func(t *testing.T) {
			// approve/update/set_frozen have no channel/rate gate; send passes
			// because the snapshot is gmail/under-limit/not-frozen.
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
	fallback := policy.NewStatic("create_task", "draft_delivery", "send_delivery")

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
}
