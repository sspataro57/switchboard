package policy_test

// SWT-43 (docs/tickets/delivery-deny_SPEC.md) criterion 6: reject_delivery is
// in policy.humanOnly and in neither sendShaped, freezeGated nor
// snapshotGated. Pure matrix core, ZERO I/O (invariant 7). Reuses
// mcpVerbsCorpus, mcpVerbsChecker and assertDeny (same package).
//
// GREENFIELD NOTE — EXPECTED RED. reject_delivery is not in humanOnly, so
// Decide answers allow/matrix-human for every non-human shape in the corpus;
// and it is not registered, so the matrix's static fallback refuses even the
// human shapes.
//
// WHY humanOnly (D9, the mark_delivery_failed argument): a delivery verdict on
// a worker's own words belongs to the human. The dismissal precedent applies:
// the verdict is LABELLED DATA (approvals + rejection_note), and an automated
// caller that could reject could mint labels — or clear its own drafts.
//
// WHY NOT send-shaped: a reject moves a row AWAY from the world. The kill
// switch (whose job is to stop SENDING) and the per-channel rate limit have no
// claim on it — a freeze that also stopped Salvador denying a bad draft would
// leave that draft sitting one Unfreeze away from the client.

import (
	"context"
	"testing"

	"github.com/sspataro57/switchboard/internal/policy"
)

// Human shapes allow; EVERY other shape denies with human_only, the same
// fourteen shapes TestDecide_TaskDismiss_FullActorCorpus pins. Mutation:
// remove reject_delivery from humanOnly → the nine non-human rows go red.
func TestDecide_RejectDelivery_FullActorCorpus(t *testing.T) {
	for _, tc := range mcpVerbsCorpus {
		tc := tc
		t.Run(tc.actor, func(t *testing.T) {
			d := policy.Decide(policy.Request{Tool: "reject_delivery", Actor: tc.actor}, policy.Snapshot{})
			if tc.human {
				if d.Decision != "allow" {
					t.Errorf("reject_delivery by %q = %s/%s, want allow (a human identity)", tc.actor, d.Decision, d.Rule)
				}
				return
			}
			// human_only, not mcp_human_only: this is not a transport rule. The
			// draft worker (drafts:gpt) and the orchestrator are refused too.
			assertDeny(t, d, "human_only")
		})
	}
}

// A frozen, over-limit snapshot still allows it. Pinned rather than assumed
// because the natural place to add the verb is beside the send verbs in the
// matrix, and a frozen board where Salvador cannot deny a draft is the wrong
// failure. (Green before implementation: Decide already allows a tool it does
// not know; the rows above are the red ones.)
func TestDecide_RejectDelivery_IgnoresKillSwitchAndRateLimit(t *testing.T) {
	frozenAndOverLimit := policy.Snapshot{
		SendingFrozen: true,
		Channel:       "gmail",
		HourlyLimit:   10,
		SentLastHour:  map[string]int{"gmail": 99},
	}
	for _, actor := range []string{"dashboard:salvo", "opsctl:salvo", "mcp:manual:salvo"} {
		d := policy.Decide(policy.Request{Tool: "reject_delivery", Actor: actor}, frozenAndOverLimit)
		if d.Decision != "allow" {
			t.Errorf("reject_delivery by %s with the kill switch ON and gmail over its limit = %s/%s (%s), want "+
				"allow — it moves a row away from the world (criterion 6)", actor, d.Decision, d.Rule, d.Reason)
		}
	}
}

// Through the production matrix with the REAL registry: humans get
// allow/matrix-human (humanOnly and not snapshotGated → Decide on an empty
// snapshot), workers get human_only, and the snapshot loader NEVER runs
// (mcpVerbsChecker's loader fails the test if it does) — which is what
// "not in snapshotGated" means observably.
func TestMatrix_RejectDelivery_ThroughCheck_NeverLoadsASnapshot(t *testing.T) {
	checker := mcpVerbsChecker(t)
	ctx := context.Background()
	for _, tc := range mcpVerbsCorpus {
		tc := tc
		t.Run(tc.actor, func(t *testing.T) {
			d, err := checker.Check(ctx, policy.Request{Tool: "reject_delivery", Actor: tc.actor})
			if err != nil {
				t.Fatalf("Check(reject_delivery, %q): %v", tc.actor, err)
			}
			if !tc.human {
				assertDeny(t, d, "human_only")
				return
			}
			if d.Decision != "allow" || d.Rule != "matrix-human" {
				t.Errorf("Check(reject_delivery, %q) = %s/%s (%s), want allow/matrix-human — the humanOnly branch "+
					"of the matrix, reached without a snapshot. A static-fallback answer means the tool is not "+
					"registered or not in humanOnly", tc.actor, d.Decision, d.Rule, d.Reason)
			}
		})
	}
}
