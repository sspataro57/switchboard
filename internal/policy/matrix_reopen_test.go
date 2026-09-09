package policy_test

// Unit test for SWT-32 (docs/tickets/jira-status-sync_SPEC.md) criterion 39 and
// D7: `task_reopen` is NOT in policy.humanOnly, is not sendShaped and is not
// snapshotGated — it falls through internal/policy's matrix to the static
// allow-list, exactly as `task_close` does. Pure matrix core, ZERO I/O
// (invariant 7). Reuses humanActor / botActor / workerMCPActor from
// matrix_test.go (same package) and copies matrix_dismiss_test.go's actor
// enumeration wholesale, plus this ticket's own actor.
//
// GREENFIELD NOTE — EXPECTED RED. The allow-list below is built from the REAL
// registry (tools.Register, exactly as every main builds it), and `task_reopen`
// is not registered — so every case returns deny/"static-default", "tool not in
// registered set". Building the list from the registry rather than from a
// hand-written NewStatic("task_reopen") is what makes this test red at all: a
// list the test supplies itself would allow the tool no matter what the repo
// does, which is the fixture-shaped-like-the-assertion landmine.
//
// WHY THE VERB IS *NOT* HUMAN-ONLY, which is the opposite of its sibling
// task_dismiss and therefore the thing most likely to be "tidied up" later: the
// reconciler calls it as `ticketstatus:jira`, exactly as the orchestrator calls
// task_close as `orchestrator`. Gating on a human actor would make the return
// path — a reopened ticket bringing its task back on the next run — impossible.
//
// AND THIS SPEC DOES NOT CLAIM THE ACTOR PREFIX IS A BOUNDARY. The gate that
// matters is the MCP surface (criterion 39's other half, asserted in
// internal/mcpserver/adapter_test.go's spineTools): an agent that could reopen
// tasks could resurrect its own closed work. The institutional rule is explicit
// — "an actor-prefix check is a transport label, not a trust boundary" — so the
// enumeration below exists to prove that NOTHING here keys on the caller, not to
// certify a gate.

import (
	"context"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

// reopenChecker is the production wiring: the matrix in front of the static
// allow-list built from the real registry (deliveryExecutor's shape, and every
// main's). nil pool — Register only builds closures.
func reopenChecker(t *testing.T) policy.Checker {
	t.Helper()
	reg := executor.NewRegistry()
	tools.Register(reg, nil)
	return policy.NewMatrix(reopenLoader{t}, policy.NewStatic(reg.Names()...))
}

// reopenLoader fails the test if the matrix ever tries to load a snapshot for
// this tool. Nothing leaves the system, so neither the kill switch nor the
// per-channel rate limit has any claim on it (the mark_delivery_failed argument,
// verbatim) — and a verb that reached the loader would be a verb the kill switch
// could freeze, which would mean the operator's stop button also stops a board
// from being cleaned up.
type reopenLoader struct{ t *testing.T }

func (l reopenLoader) Load(context.Context, policy.Request) (policy.Snapshot, error) {
	l.t.Errorf("policy.Matrix loaded a delivery snapshot for task_reopen: the verb has become " +
		"snapshotGated. It creates, reads and mutates no deliveries row and imports no send adapter " +
		"(invariant 4)")
	return policy.Snapshot{}, nil
}

// EVERY actor shape that exists in this repo, exactly as criterion 39 lists
// them, because the recorded landmine is precise about why: "when writing a test
// for such a gate, enumerate the actor shapes that exist in the repo ... because
// one of them is usually the hole". Here the expectation is uniform — all of
// them allowed — which is what "nothing pretends an actor prefix is a gate"
// means in practice.
func TestDecide_TaskReopen_FallsThroughForEveryActorShape(t *testing.T) {
	checker := reopenChecker(t)
	ctx := context.Background()

	for _, actor := range []string{
		humanActor,          // dashboard:salvo@example.com
		"dashboard:salvo",   //
		"opsctl:salvo",      // the hand-run reconciliation
		"manual:salvo",      //
		"mcp:manual:salvo",  // an interactive session
		workerMCPActor,      // mcp:worker:avviato
		"mcp:worker:x",      //
		"worker:collab",     // bare worker, no transport prefix
		botActor,            // drafts:gpt
		"orchestrator",      // no prefix at all
		"capture:jira",      // the connector-main pass that creates these tasks
		"promote:classify",  // the SWT-30 promoter
		"ticketstatus:jira", // THIS ticket's pass — the one that must work
	} {
		actor := actor
		t.Run(actor, func(t *testing.T) {
			d, err := checker.Check(ctx, policy.Request{Tool: "task_reopen", Actor: actor})
			if err != nil {
				t.Fatalf("Check(task_reopen, %q) errored: %v", actor, err)
			}
			if d.Decision != "allow" {
				t.Errorf("task_reopen by %q = %q (rule %s), want allow. D7: the pass calls this verb as "+
					"ticketstatus:jira exactly as the orchestrator calls task_close as orchestrator; a "+
					"human-actor gate here would make the return path impossible, and the gate that "+
					"matters is the MCP surface (spineTools)", actor, d.Decision, d.Rule)
			}
			if d.Rule == "human_only" {
				t.Errorf("task_reopen by %q was denied human_only — the verb has been added to "+
					"policy.humanOnly. task_dismiss is human-only because a dismissal is a human "+
					"JUDGEMENT recorded as training data; a reopen is a state transition the spine "+
					"makes on its own", actor)
			}
		})
	}
}

// The kill switch and the rate limit have no claim on it — asserted rather than
// assumed, because the natural place to add a new gated verb is beside the
// delivery verbs it sits next to in the matrix. A frozen snapshot that denied a
// reopen would mean the switch whose job is to stop SENDING also stops a ticket
// coming back onto the board.
func TestDecide_TaskReopen_IgnoresTheKillSwitchAndRateLimit(t *testing.T) {
	frozenAndAtLimit := policy.Snapshot{
		SendingFrozen: true,
		Channel:       "gmail",
		HourlyLimit:   10,
		SentLastHour:  map[string]int{"gmail": 99},
	}
	d := policy.Decide(policy.Request{Tool: "task_reopen", Actor: "ticketstatus:jira"}, frozenAndAtLimit)
	if d.Decision != "allow" {
		t.Errorf("task_reopen with the kill switch ON and the gmail rate limit exceeded = %q (rule "+
			"%s), want allow. Nothing outbound exists here: the pass makes two READ calls to Jira and "+
			"no write call of any kind (invariant 4)", d.Decision, d.Rule)
	}
}

// task_close must not acquire a gate as a side effect of gaining a sibling —
// matrix_dismiss_test.go's assertion, restated for this ticket because the two
// verbs now share a transition helper and the temptation to "treat them the
// same" is one refactor away.
func TestDecide_TaskCloseStaysUngatedAlongsideReopen(t *testing.T) {
	for _, actor := range []string{"orchestrator", botActor, "capture:jira", "ticketstatus:jira"} {
		d := policy.Decide(policy.Request{Tool: "task_close", Actor: actor}, policy.Snapshot{})
		if d.Decision != "allow" {
			t.Errorf("task_close by %q = %q (rule %s), want allow — the orchestrator calls it from R1, "+
				"R8 and the feedback rules, and this pass calls it for every dropped ticket",
				actor, d.Decision, d.Rule)
		}
	}
}
