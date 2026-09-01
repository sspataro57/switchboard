package policy_test

// SWT-20 acceptance criterion 18: `policy.HumanActor` is EXPORTED, and
// `humanOnly`'s gate and the new draft_delivery room-choice check call the SAME
// function.
//
// ZERO I/O — the matrix core is a pure function of (Request, Snapshot),
// invariant 7. Nothing here touches Postgres, a provider or the network.
//
// WHY THIS TEST EXISTS AT ALL, and why it is not "an actor-prefix test", which
// the IK entry rightly warns against:
//
//	SWT-20 reopens `upwork_chat` drafting behind a SERVER-SIDE binding — the
//	target must belong to the client the task's recorded source thread belongs
//	to. That binding is UNCONDITIONAL: every actor, including `drafts:gpt`
//	(SPEC D8, and Q1's answer (a)). The only thing keyed on the caller is who
//	may pick a DIFFERENT ROOM of the already-bound client, which is finding 3's
//	"explicit human choice".
//
//	The danger is drift: if the handler restates the human prefixes instead of
//	calling the same predicate `Decide` uses, the two definitions diverge and
//	the divergence is invisible. `humanActor` is unexported today
//	(matrix.go:73-89), so the handler CANNOT call it — hence the export, and
//	hence this test, which pins the exported form against `Decide`'s own gate
//	over the identical actor corpus.
//
// GREENFIELD NOTE: `policy.HumanActor` does not exist yet, so this file is a
// COMPILE failure for the whole `policy_test` package until matrix.go exports
// it. For greenfield surface the SPEC's contract is the signature; the shape
// imposed here is exactly the existing unexported one:
//
//	func HumanActor(actor string) bool
//
// Expected red state.

import (
	"testing"

	"github.com/sspataro57/switchboard/internal/policy"
)

// humanActorCases is the repo's full actor corpus, the same enumeration the IK
// entry demands ("dashboard:, opsctl:, mcp:worker:, mcp:manual:, drafts:gpt,
// bare worker: — because one of them is usually the hole").
var humanActorCases = []struct {
	actor string
	want  bool
	why   string
}{
	{"dashboard:salvo@example.com", true, "the dashboard is a person clicking"},
	{"opsctl:salvo", true, "the CLI is a person typing"},
	{"manual:salvo", true, "an interactive session, direct"},
	{"mcp:manual:salvo", true,
		"an interactive session over MCP. ONE transport prefix is stripped — this is the case criterion 18 " +
			"names explicitly, and it is why the handler must call this function rather than test prefixes"},
	{"mcp:dashboard:salvo@example.com", true, "dashboard identity over MCP"},
	{"mcp:opsctl:salvo", true, "opsctl identity over MCP"},
	{"drafts:gpt", false,
		"the draft worker. Criterion 18 names it: it is the counter-example the IK entry on actor prefixes is " +
			"built from — it reaches the executor directly, so executor.ViaMCP is FALSE for it and any gate " +
			"built on ViaMCP does nothing for the one component that acts automatically"},
	{"mcp:worker:acme", false, "an autonomous worker over MCP"},
	{"worker:acme", false, "an autonomous worker, direct"},
	{"capture:upworkcrm", false, "the capture engine — an observer, not a human"},
	{"mcp:mcp:manual:salvo", false, "exactly ONE transport prefix is stripped"},
	{"", false, "no actor at all"},
}

// Criterion 18, first half: the exported predicate answers the two cases the
// criterion spells out, plus the rest of the corpus.
func TestHumanActor_ExportedAnswersTheWholeActorCorpus(t *testing.T) {
	for _, tc := range humanActorCases {
		tc := tc
		t.Run(tc.actor, func(t *testing.T) {
			if got := policy.HumanActor(tc.actor); got != tc.want {
				t.Errorf("policy.HumanActor(%q) = %v, want %v — %s", tc.actor, got, tc.want, tc.why)
			}
		})
	}
}

// Criterion 18, second half and the whole point: the exported form CANNOT drift
// from `Decide`'s gate, because it is the same function.
//
// Asserted behaviourally rather than by reading the source: for every actor in
// the corpus, `Decide` on a humanOnly tool must allow exactly when
// `HumanActor` is true, and deny with rule `human_only` exactly when it is
// false. If someone later "exports" a copy, this test goes red the first time
// the two lists differ by one prefix.
func TestHumanActor_IsTheSameGateDecideUses(t *testing.T) {
	// approve_delivery is humanOnly and NOT send-shaped, so the snapshot cannot
	// influence the outcome — the human gate is the only thing under test.
	for _, tc := range humanActorCases {
		tc := tc
		t.Run(tc.actor, func(t *testing.T) {
			d := policy.Decide(policy.Request{Tool: "approve_delivery", Actor: tc.actor}, policy.Snapshot{})
			allowed := d.Decision == "allow"
			if allowed != policy.HumanActor(tc.actor) {
				t.Errorf("Decide(approve_delivery, %q) = %q (rule %q) but policy.HumanActor(%q) = %v. "+
					"The exported predicate and the matrix gate have DIFFERENT definitions of a human — which is "+
					"the drift criterion 18 exists to prevent. Export the existing humanActor; do not copy it",
					tc.actor, d.Decision, d.Rule, tc.actor, policy.HumanActor(tc.actor))
			}
			if !allowed && d.Rule != "human_only" {
				t.Errorf("Decide(approve_delivery, %q) denied with rule %q, want human_only — the refusal must "+
					"come from the human gate, or this test is comparing HumanActor against something else",
					tc.actor, d.Rule)
			}
		})
	}
}
