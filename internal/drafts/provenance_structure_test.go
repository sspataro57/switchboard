package drafts_test

// SWT-20 criterion 5 (structural half) and criterion 17's second clause, plus
// verification-protocol step 3, as a test rather than a grep somebody has to
// remember to run.
//
// Plain unit test: no build tag, no database. It reads this package's own
// source, in the mould of internal/capture/rules_structure_test.go and
// internal/connector/upworkcrm/keyspelling_test.go.
//
// THE RULE: after SWT-20, `internal/drafts` resolves an upwork target from the
// TASK'S RECORDED SOURCE THREAD and from nothing else. No query in the package
// may enumerate a client's threads any more.
//
// Why a structural test and not only the behavioural ones next door: criterion
// 5's promise is a NEGATIVE — "the client-wide candidate query, the roomed
// preference and the multi-room refusal are deleted". A behavioural test can
// show that one fixture resolves to one key; it cannot show that no other code
// path looks at another thread. The SPEC's own phrasing for criterion 3 is
// "'never to the most recent room' is satisfied STRUCTURALLY: after this change
// no code path in drafts looks at any thread other than the recorded one" — and
// a structural claim wants a structural check. Verification step 3 says the same
// thing in shell form (`git grep -n ClientThreadPrefix`), which is a step that
// gets skipped.
//
// GREENFIELD NOTE: red today. store.go still calls upworkcrm.ClientThreadPrefix
// to enumerate the client's threads, orders them by max(m.sent_at), prefers a
// roomed one and refuses when more than one exists (store.go:292-362) — and it
// does not mention store.TaskSourceThread at all.

import (
	"os"
	"strings"
	"testing"
)

// draftsSources lists the package's non-test .go files. Scanning the whole
// package rather than store.go alone is deliberate: "moved to another file in
// the same package" is the cheapest way to defeat a single-file scan.
func draftsSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/drafts: %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatalf("read internal/drafts/%s: %v", n, err)
		}
		out[n] = string(b)
	}
	if len(out) == 0 {
		t.Fatalf("no non-test sources found in internal/drafts; a scan with nothing to scan proves nothing")
	}
	return out
}

// Criterion 5 / 17: the client-wide candidate query is gone.
//
// ClientThreadPrefix itself STAYS in upworkcrm — it is still the safe
// bind-parameter builder, and it is the only spelling of a client prefix
// anywhere. What must go is this package's use of it, because using it means
// asking "which threads does this client have?", which is the question SWT-20
// deletes: the task records which conversation raised it, so there is nothing to
// choose between.
func TestDrafts_NoLongerEnumeratesAClientsThreads(t *testing.T) {
	srcs := draftsSources(t)

	// Positive control: the package must still contain the resolver this rule is
	// about, or the scan is checking an empty room.
	joined := strings.Join(valuesOf(srcs), "\n")
	if !strings.Contains(joined, "upworkTarget") && !strings.Contains(joined, "func (s *PGStore) resolve") {
		t.Fatalf("internal/drafts declares neither upworkTarget nor PGStore.resolve; this scan has stopped " +
			"looking at the targeting code")
	}

	for name, src := range srcs {
		if strings.Contains(src, "ClientThreadPrefix") {
			t.Errorf("internal/drafts/%s still calls upworkcrm.ClientThreadPrefix. Criterion 5: the upwork "+
				"target comes from store.TaskSourceThread and NOTHING else — enumerating the client's threads "+
				"is the act of CHOOSING a room, and the choice is what SWT-19's mitigation had to refuse. "+
				"Verification step 3 puts it plainly: if a caller remains, targeting is still choosing", name)
		}
		// The ordering that decided which room to reply into for the two
		// multi-room production clients. Its deletion is criterion 7's whole
		// point: a legacy provenance must resolve to the LEGACY key, not to
		// whichever room happens to carry the newest message.
		if strings.Contains(src, "max(m.sent_at)") {
			t.Errorf("internal/drafts/%s still orders thread candidates by max(m.sent_at). There are no "+
				"candidates any more: the recorded source thread IS the target (criterion 7)", name)
		}
	}
}

// Criterion 5, positively: this package reads provenance through the ONE shared
// reader.
//
// Two spellings of the provenance query is the drift this repo has paid for four
// times, and internal/store exists for exactly this consolidation (SPEC §2, the
// UnconfirmedNoteMarker precedent). A second query here — even a correct one —
// is how `drafts` and `draft_delivery` come to disagree about which conversation
// a task belongs to, which is a wrong-room send neither test suite would catch.
func TestDrafts_ReadsProvenanceThroughTheSharedResolver(t *testing.T) {
	srcs := draftsSources(t)
	joined := strings.Join(valuesOf(srcs), "\n")

	if !strings.Contains(joined, "TaskSourceThread") {
		t.Errorf("internal/drafts never calls store.TaskSourceThread. Criterion 5: the upwork branch resolves " +
			"its target ONLY from the task's recorded source thread, through the single reader in " +
			"internal/store/provenance.go")
	}
	// The external_refs join STAYS — it is gmail's and jira's only route and
	// nothing about their delivery correctness changes here (SPEC §3, D2). If it
	// disappears, this ticket has quietly taken on the follow-up it explicitly
	// deferred.
	if !strings.Contains(joined, "external_refs") {
		t.Errorf("internal/drafts no longer queries external_refs. That join is gmail's and jira's ONLY " +
			"thread route and SPEC §3 keeps it: migrating them to provenance is named as out of scope, with " +
			"its own smoke surface")
	}
}

func valuesOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
