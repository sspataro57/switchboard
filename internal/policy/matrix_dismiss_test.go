package policy_test

// Unit test for SWT-31 (docs/tickets/board-dismissals_SPEC.md) criterion 9:
// `task_dismiss` is in policy.humanOnly. Pure matrix core, ZERO I/O
// (invariant 7). Reuses humanActor / botActor / assertDeny from matrix_test.go
// (same package), and copies matrix_capturerules_test.go's actor enumeration
// wholesale — the SPEC says so by name, and adds `orchestrator` and `promote:`
// to it.
//
// GREENFIELD NOTE — EXPECTED RED. `task_dismiss` is not in internal/policy's
// humanOnly map (and not registered at all), so Decide returns
// allow/"matrix-human" for EVERY actor below and every deny case fails.
//
// WHY THE VERB IS HUMAN-ONLY AT ALL (D3, because "reuse task_close" is the
// obvious first instinct): a dismissal is a human judgement recorded as training
// data. `task_close` cannot carry the gate — the orchestrator calls it as actor
// `orchestrator` from R1, R8 and the feedback rules, so gating it would break the
// orchestrator; and leaving `task_close` open while letting it write labels would
// let any automated caller mint training data with no gate at all. Hence a second
// verb whose ONLY difference is this gate and the typed row.

import (
	"testing"

	"github.com/sspataro57/switchboard/internal/policy"
)

// EVERY actor shape that exists in this repo, not one representative bot. The
// institutional landmine is precise about why: "an actor-prefix check is a
// transport label, not a trust boundary", and the hole is always the shape
// nobody enumerated. Two of these are named by the SPEC because they are the
// ones a capture-rules-shaped list would miss:
//
//   - `orchestrator` — the bare, prefix-less actor, and the very caller that
//     forced task_dismiss to be a separate verb (internal/orchestrator/engine.go).
//     It has no colon at all, so a gate written as "reject anything with a
//     non-human prefix" lets it through.
//   - `promote:classify` — the SWT-30 promoter, which reaches the executor
//     directly and creates the tasks a human is most likely to dismiss.
func TestDecide_TaskDismiss_HumanOnly(t *testing.T) {
	for _, actor := range []string{
		botActor,          // drafts:gpt — the direct, non-MCP autonomous caller
		"worker:collab",   // bare worker, no transport prefix
		workerMCPActor,    // mcp:worker:avviato
		"mcp:worker:x",    // the SPEC's spelling
		"mcp:opsworker-x", // an MCP worker id that is not "worker:"-shaped
		"orchestrator",    // no prefix at all
		"capture:slackweb",
		"promote:classify",
		"ghpoll:github",        // a connector-shaped actor
		"mcp:mcp:manual:salvo", // a doubled transport prefix is not a human
	} {
		actor := actor
		t.Run(actor, func(t *testing.T) {
			d := policy.Decide(policy.Request{Tool: "task_dismiss", Actor: actor}, policy.Snapshot{})
			assertDeny(t, d, "human_only")
		})
	}
}

// The allowed shapes. All four must pass or the board's only verb is unusable:
// the dashboard calls as `dashboard:{session user}` (criterion 15), opsctl and an
// interactive session are how a dismissal is made without a browser, and
// `mcp:manual:salvo` is what an interactive Claude session arrives as once
// policy.HumanActor strips the one leading transport prefix.
func TestDecide_TaskDismiss_HumanPrefixes(t *testing.T) {
	for _, actor := range []string{
		humanActor, // dashboard:salvo@example.com
		"dashboard:salvo",
		"opsctl:salvo",
		"manual:salvo",
		"mcp:manual:salvo",
	} {
		actor := actor
		t.Run(actor, func(t *testing.T) {
			d := policy.Decide(policy.Request{Tool: "task_dismiss", Actor: actor}, policy.Snapshot{})
			if d.Decision != "allow" {
				t.Errorf("task_dismiss by %q = %q (rule %s), want allow", actor, d.Decision, d.Rule)
			}
		})
	}
}

// "Not sendShaped and not snapshotGated — nothing leaves the system, so neither
// the kill switch nor the rate limit has any claim on it (the
// mark_delivery_failed argument, verbatim)."
//
// Asserted rather than assumed, because the natural place to add a new gated verb
// is beside the delivery verbs it sits next to in the matrix. A frozen snapshot
// that denied a dismissal would mean the kill switch — whose job is to stop
// SENDING — also stops a human from cleaning up a board.
func TestDecide_TaskDismiss_IgnoresTheKillSwitchAndRateLimit(t *testing.T) {
	frozenAndAtLimit := policy.Snapshot{
		SendingFrozen: true,
		Channel:       "gmail",
		HourlyLimit:   10,
		SentLastHour:  map[string]int{"gmail": 99},
	}
	d := policy.Decide(policy.Request{Tool: "task_dismiss", Actor: humanActor}, frozenAndAtLimit)
	if d.Decision != "allow" {
		t.Errorf("task_dismiss by a human with the kill switch ON and the gmail rate limit exceeded = "+
			"%q (rule %s), want allow. Nothing outbound exists here: no deliveries row is created, "+
			"read or mutated, and no adapter is imported (invariant 4)", d.Decision, d.Rule)
	}
}

// task_close must NOT acquire the gate as a side effect. This is the assertion
// that keeps D3 honest: the whole reason for a second verb is that the
// orchestrator closes tasks automatically, and a "tidy-up" that added task_close
// to humanOnly would stall R1, R8 and the feedback rules with a policy denial
// that looks like a permissions bug.
func TestDecide_TaskClose_StaysCallableByTheOrchestrator(t *testing.T) {
	for _, actor := range []string{"orchestrator", botActor, "capture:rules", "promote:classify"} {
		d := policy.Decide(policy.Request{Tool: "task_close", Actor: actor}, policy.Snapshot{})
		if d.Decision != "allow" {
			t.Errorf("task_close by %q = %q (rule %s), want allow — the orchestrator calls it from R1, "+
				"R8 and the feedback rules; gating it would break the spine, which is exactly why "+
				"task_dismiss exists as a separate verb (D3)", actor, d.Decision, d.Rule)
		}
	}
}
