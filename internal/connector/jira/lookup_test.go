package jira_test

// Unit tests for SWT-32 (docs/tickets/jira-status-sync_SPEC.md) criteria 9, 16
// and 19: the candidate-driven lookup — GET exactly the keys handed to it, write
// each snapshot raw-first through the SAME upsertRaw the poller uses, cache
// /myself in the cursor by merge, and record one sync_runs row. No JQL, no
// pagination, no cursor.
//
// Offline: the httptest fakeJira (fake_jira_test.go) and the in-memory
// jiraFakeSink (poller_test.go), both already shared by this package's tests.
// NEVER a live Jira call.
//
// GREENFIELD NOTE — EXPECTED RED. internal/connector/jira/lookup.go does not
// exist, so this file compile-FAILs the jira_test package with "undefined:
// jira.LookupIssues", and the structural scans below Fatal on a missing
// lookup.go. Verified in the authoring session against a throwaway stub that
// fetched nothing: every assertion fires on its own merits (issueGets = []
// want [ILK-1 ILK-2], runs = [] want [start finish:ok], and so on).
//
// IMPOSED SURFACE — criterion 9 fixes the signature verbatim; `cfg` is the
// package's existing Config (the lookup uses none of its cursor fields, which
// is D20's point: there is nothing to page and no watermark to keep):
//
//	// LookupIssues GETs each key by name and stores it raw-first. The key set
//	// comes from the reconciler's candidates (D16) — never from the provider.
//	func LookupIssues(ctx context.Context, c *Client, sink Sink, acct Account,
//	    keys []string, cfg Config) (Stats, error)
//
// WHY THE COUNTERS ARE jira.Stats AND NOT A NEW TYPE: the raw-first write is the
// existing one, so its counters (IssuesFetched / RawInserted / RawUpdated /
// RawUnchanged) already mean exactly the right things, and criterion 19 puts
// them in the sync_runs.stats payload the poller already writes.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/connector/jira"
)

// readJiraSource reads one of this package's non-test source files. Shared with
// rawid_test.go's call-site check.
func readJiraSource(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(rel))
	if err != nil {
		t.Fatalf("read internal/connector/jira/%s: %v — the file this criterion is about does not "+
			"exist, and a scan with nothing to scan proves nothing", rel, err)
	}
	return string(b)
}

// lookupAccount is a provider='jira_lookup' row as the reconciler hands it over:
// the same Account struct the poller uses (D17 changes the provider VALUE, not
// the shape), scoped to the prefixes it is allowed to fetch.
func lookupAccount(base string) jira.Account {
	return jira.Account{ID: 42, Email: "itest-lookup@example.com", SiteBaseURL: base, Projects: []string{"ILK"}}
}

func lookupFixtures(f *fakeJira) {
	f.add(fakeIssue{key: "ILK-1", updated: "2026-09-01T10:00:00.000+0000", created: "2026-08-01T10:00:00.000+0000",
		summary: "one", description: "d1", reporter: fakeOtherAcc, assignee: fakeOwnAcc})
	f.add(fakeIssue{key: "ILK-2", updated: "2026-09-02T10:00:00.000+0000", created: "2026-08-02T10:00:00.000+0000",
		summary: "two", description: "d2", reporter: fakeOtherAcc, assignee: fakeOtherAcc})
	f.add(fakeIssue{key: "ILK-3", updated: "2026-09-03T10:00:00.000+0000", created: "2026-08-03T10:00:00.000+0000",
		summary: "three", description: "d3", reporter: fakeOtherAcc, assignee: fakeOtherAcc})
}

func newLookupClient(f *fakeJira) *jira.Client {
	return jira.NewClient(http.DefaultClient, f.url(), "itest-lookup@example.com", "token")
}

// ---- criterion 9: exactly these keys, by GET, and no JQL ----------------------

// "The key set is the reconciler's candidate list (D16) ... the lookup fetches
// exactly the other two, in key order, and no others."
//
// The candidate set being bounded by the number of jira-keyed TASKS is the whole
// reason this shape was chosen over a poll (Q1's answer), so "fetched exactly
// these" is not a tidiness assertion: a third GET here is the first symptom of
// the project-wide sweep the SPEC put in Out of scope.
func TestLookupIssues_FetchesExactlyTheGivenKeysAndNothingElse(t *testing.T) {
	f := newFakeJira()
	defer f.close()
	lookupFixtures(f)
	sink := newJiraFakeSink()

	stats, err := jira.LookupIssues(context.Background(), newLookupClient(f), sink,
		lookupAccount(f.url()), []string{"ILK-1", "ILK-3"}, jira.Config{})
	if err != nil {
		t.Fatalf("LookupIssues: %v", err)
	}

	if got := strings.Join(f.issueGets, ","); got != "ILK-1,ILK-3" {
		t.Errorf("issue GETs = [%s], want [ILK-1,ILK-3] in that order. The candidate list IS the key "+
			"set (D16); fetching more means something other than external_refs chose the keys", got)
	}
	if len(f.searchJQLs) != 0 {
		t.Errorf("the lookup issued %d JQL search(es) (%v). Criterion 9: it issues NO JQL — a search "+
			"is how the rejected project-wide poll would announce itself, and it would drag every LHH "+
			"issue through the priority-100 capture rule", len(f.searchJQLs), f.searchJQLs)
	}
	if len(f.commentGets) != 0 {
		t.Errorf("the lookup paged comments for %v. The snapshot exists to answer two questions — "+
			"statusCategory and assignee — and comment raws under a jira_lookup account are rows "+
			"nothing will ever read (they are never normalized, D17)", f.commentGets)
	}
	if stats.IssuesFetched != 2 {
		t.Errorf("Stats.IssuesFetched = %d, want 2 — the counter criterion 19 puts in sync_runs.stats",
			stats.IssuesFetched)
	}
}

// ---- criterion 9 + invariant 1: ONE raw write, the existing one --------------

// "writes through the existing upsertRaw — so the raw-first write for this
// ticket happens at exactly ONE place, the same one ingestIssue uses."
//
// Two properties in one test, because they are the same fact seen from both
// ends: the external_id is IssueRawID(key) (so the lookup's snapshot and the
// poller's snapshot are the SAME row for the same issue, never two), and the
// stored bytes have had fields.comment split off (so the hash short-circuit
// below can ever fire — an issue whose embedded comment list changes would
// otherwise re-write the row on every pass).
func TestLookupIssues_WritesRawFirstUnderTheSharedIssueID(t *testing.T) {
	f := newFakeJira()
	defer f.close()
	f.add(fakeIssue{key: "ILK-1", updated: "2026-09-01T10:00:00.000+0000", created: "2026-08-01T10:00:00.000+0000",
		summary: "one", description: "d1", reporter: fakeOtherAcc, assignee: fakeOwnAcc,
		comments: []fakeComment{{id: "900", author: fakeOtherAcc, body: "a comment", created: "2026-08-05T10:00:00.000+0000"}}})
	sink := newJiraFakeSink()

	if _, err := jira.LookupIssues(context.Background(), newLookupClient(f), sink,
		lookupAccount(f.url()), []string{"ILK-1"}, jira.Config{}); err != nil {
		t.Fatalf("LookupIssues: %v", err)
	}

	if len(sink.inserts) != 1 {
		t.Fatalf("the lookup wrote %d raw rows for one issue, want exactly 1: %+v. The comments of a "+
			"lookup snapshot are deliberately not stored — those rows exist to be read by the "+
			"reconciler and by nothing else", len(sink.inserts), sink.inserts)
	}
	w := sink.inserts[0]
	if want := jira.IssueRawID("ILK-1"); w.externalID != want {
		t.Errorf("raw external_id = %q, want %q. Criterion 5: ONE spelling, shared with the poller — "+
			"two spellings would mean two rows for one issue and criterion 31's ambiguity refusal "+
			"firing on a key that has only ever had one snapshot", w.externalID, want)
	}
	if w.hash == "" {
		t.Errorf("raw content_hash is empty; upsertRaw computes it with chash.ContentHash and it is " +
			"what makes the re-fetch of an unchanged issue a no-op")
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(w.raw, &doc); err != nil {
		t.Fatalf("stored raw is not an object: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(doc["fields"], &fields); err != nil {
		t.Fatalf("stored raw has no fields object: %v", err)
	}
	if _, present := fields["comment"]; present {
		t.Errorf("the stored snapshot still carries fields.comment. Criterion 9: the lookup splits " +
			"comments off with the existing splitIssueComments — the poller does, and the two halves " +
			"of the reconciler must produce byte-comparable rows for the same issue")
	}
	for _, want := range []string{"status", "assignee"} {
		if _, present := fields[want]; !present {
			t.Errorf("the stored snapshot has no fields.%s. GetIssue sends NO `fields` parameter, so "+
				"every navigable field survives (fact 1) — losing either one means the decision "+
				"cannot be made from the stored row at all (D19)", want)
		}
	}

	// The hash short-circuit: a second pass over an unchanged issue rewrites
	// nothing. This is what keeps a */15 CronJob from churning raw_source_items
	// (and from resetting normalized_at, which UpdateRaw does).
	stats, err := jira.LookupIssues(context.Background(), newLookupClient(f), sink,
		lookupAccount(f.url()), []string{"ILK-1"}, jira.Config{})
	if err != nil {
		t.Fatalf("LookupIssues (second pass): %v", err)
	}
	if stats.RawUnchanged != 1 || stats.RawUpdated != 0 || len(sink.updates) != 0 {
		t.Errorf("second pass over an unchanged issue: RawUnchanged=%d RawUpdated=%d updates=%d, "+
			"want 1/0/0. upsertRaw short-circuits on the content hash; a lookup that re-wrote the "+
			"row every hour would also reset normalized_at on rows the normalizer must never see",
			stats.RawUnchanged, stats.RawUpdated, len(sink.updates))
	}
}

// ---- criterion 16: /myself once, merged into the cursor ----------------------

// "The lookup calls Myself once per lookup account per pass and merges the
// result into sync_cursor.own_account_id with the existing SaveCursor."
//
// Once per PASS, not once per key: it is the identity of the site, not of the
// issue. And the cursor it writes carries no watermark — D20 is explicit that
// the lookup has no cursor, and a jira_updated_at appearing here would be state
// whose only job is to be wrong after a re-assignment.
func TestLookupIssues_CachesOwnAccountIDOncePerPass(t *testing.T) {
	f := newFakeJira()
	defer f.close()
	lookupFixtures(f)
	sink := newJiraFakeSink()

	if _, err := jira.LookupIssues(context.Background(), newLookupClient(f), sink,
		lookupAccount(f.url()), []string{"ILK-1", "ILK-2", "ILK-3"}, jira.Config{}); err != nil {
		t.Fatalf("LookupIssues: %v", err)
	}

	if f.myselfCalls != 1 {
		t.Errorf("/myself called %d times for 3 keys, want exactly 1 — Ingest's shape (ingest.go: one "+
			"call per run, cached in the cursor). Per key it is three round trips for one unchanging "+
			"fact", f.myselfCalls)
	}
	if len(sink.savedCursors) == 0 {
		t.Fatalf("the lookup saved no cursor. D12: 'me' on the lookup site is the account's own " +
			"accountId, and until it is stored the assignee gate is unevaluable — every reengine ref " +
			"counts unreadable and nothing ever drops")
	}
	last := sink.savedCursors[len(sink.savedCursors)-1]
	if last.OwnAccountID != fakeOwnAcc {
		t.Errorf("saved cursor own_account_id = %q, want %q — the value /myself returned, stored the "+
			"way Ingest stores it so 'who are we on this site' has ONE spelling",
			last.OwnAccountID, fakeOwnAcc)
	}
	if last.JiraUpdatedAt != "" {
		t.Errorf("saved cursor jira_updated_at = %q, want empty. D20: the lookup has NO cursor — "+
			"there is nothing to page, and a watermark on a candidate-driven fetch is state whose "+
			"only job is to be wrong after a re-assignment", last.JiraUpdatedAt)
	}
}

// ---- criterion 19: one sync_runs row per lookup account per pass -------------

func TestLookupIssues_RecordsOneSyncRun(t *testing.T) {
	f := newFakeJira()
	defer f.close()
	lookupFixtures(f)
	sink := newJiraFakeSink()

	if _, err := jira.LookupIssues(context.Background(), newLookupClient(f), sink,
		lookupAccount(f.url()), []string{"ILK-1"}, jira.Config{}); err != nil {
		t.Fatalf("LookupIssues: %v", err)
	}
	if got := strings.Join(sink.runs, ","); got != "start,finish:ok" {
		t.Errorf("sync run trail = [%s], want [start,finish:ok]. Criterion 19: ONE row per lookup "+
			"account per pass, status ok/error, counters in stats — and nothing branches on that "+
			"payload (the upworkcrm two-rows landmine)", got)
	}
}

// ---- D18's safety property, at the fetch primitive ---------------------------

// "an account can only ever fetch keys whose prefix it declares, so no ref can
// make us GET an arbitrary issue" — 09-jira-github-connectors' mandatory-scoping
// refusal, preserved verbatim. The routing half is the reconciler's (criterion
// 14); this is the same refusal at the place that actually holds the token, so a
// future caller that skips the router cannot widen it.
func TestLookupIssues_NeverFetchesOutsideTheAccountsScopes(t *testing.T) {
	t.Run("unscoped account is refused", func(t *testing.T) {
		f := newFakeJira()
		defer f.close()
		lookupFixtures(f)
		acct := lookupAccount(f.url())
		acct.Projects = nil

		if _, err := jira.LookupIssues(context.Background(), newLookupClient(f), newJiraFakeSink(),
			acct, []string{"ILK-1"}, jira.Config{}); err == nil {
			t.Errorf("LookupIssues with an account carrying no scopes = nil error, want a refusal. " +
				"Ingest refuses the same shape in the same words ('an unscoped poll is refused'), and " +
				"it is what keeps the SWT build tracker — and every other project on the site — out " +
				"of the product funnel")
		}
		if len(f.issueGets) != 0 {
			t.Errorf("an unscoped account still fetched %v; the refusal must come BEFORE the token is "+
				"spent", f.issueGets)
		}
	})

	t.Run("a key outside the declared prefixes is never fetched", func(t *testing.T) {
		f := newFakeJira()
		defer f.close()
		lookupFixtures(f)
		f.add(fakeIssue{key: "SWT-32", updated: "2026-09-04T10:00:00.000+0000",
			created: "2026-08-04T10:00:00.000+0000", summary: "the build tracker", reporter: fakeOwnAcc})

		_, _ = jira.LookupIssues(context.Background(), newLookupClient(f), newJiraFakeSink(),
			lookupAccount(f.url()), []string{"SWT-32"}, jira.Config{})

		for _, got := range f.issueGets {
			if got == "SWT-32" {
				t.Errorf("the lookup GET SWT-32 with an account declaring only [ILK]. D18: routing is " +
					"by declared prefix precisely so a ref — whose external_key is agent-facing free " +
					"text written by link_external_ref — can never aim this token at an issue of the " +
					"caller's choosing")
			}
		}
	})
}

// ---- criterion 9, structurally: no JQL on the lookup path --------------------

// "It issues no JQL: a structural test asserts SearchJQL is not referenced from
// the lookup path."
//
// Structural because the behavioural version above only proves that TODAY's
// implementation makes no search. The failure this guards against is a later
// "while we're here, also refresh the project" — which is the rejected option
// (a) arriving through the back door, one line at a time.
func TestLookupPath_ReferencesNoJQL(t *testing.T) {
	src := readJiraSource(t, "lookup.go")

	if !strings.Contains(src, "func LookupIssues(") {
		t.Fatalf("internal/connector/jira/lookup.go does not declare LookupIssues; the SPEC puts it " +
			"in this file (Files likely to touch) so the scan below has a subject")
	}
	for _, banned := range []struct{ token, why string }{
		{"SearchJQL", "criterion 9: the lookup issues no JQL — the key set comes from external_refs (D16)"},
		{"jql", "same, in any spelling: a JQL string built here is a project-wide poll in disguise"},
		{"GetComments", "the snapshot answers two questions; paging a ticket's comments under a " +
			"jira_lookup account writes rows nothing will ever read"},
	} {
		if strings.Contains(src, banned.token) {
			t.Errorf("internal/connector/jira/lookup.go mentions %q — %s", banned.token, banned.why)
		}
	}
	// And the raw write is the SHARED one, not a second writer. `upsertRaw`
	// carries the content hash, the insert/update split and the counters; a
	// direct sink.InsertRaw here would be a second raw-first path to keep
	// honest (invariant 1's concrete demand for this ticket).
	if !strings.Contains(src, "upsertRaw(") {
		t.Errorf("internal/connector/jira/lookup.go never calls upsertRaw. Criterion 9: the raw-first " +
			"write for this ticket happens at exactly ONE place, the one ingestIssue uses")
	}
	for _, direct := range []string{"sink.InsertRaw(", "sink.UpdateRaw("} {
		if strings.Contains(src, direct) {
			t.Errorf("internal/connector/jira/lookup.go calls %s directly, bypassing upsertRaw's hash "+
				"short-circuit — the second raw writer invariant 1 exists to prevent", direct)
		}
	}
}

// Per-key resilience and its counter (go-reviewer F2, 2026-09-09; added with
// the fix): one missing issue must not abort the account's other fetches, and
// the miss must be COUNTED — a 404 that only surfaced as `unpolled` would be
// indistinguishable from "no account claims this prefix".
func TestLookupIssues_AMissingIssueIsCountedAndDoesNotAbortTheRest(t *testing.T) {
	f := newFakeJira()
	defer f.close()
	lookupFixtures(f) // ILK-1..3 exist; ILK-9 does not
	sink := newJiraFakeSink()

	stats, err := jira.LookupIssues(context.Background(), newLookupClient(f), sink,
		lookupAccount(f.url()), []string{"ILK-1", "ILK-9", "ILK-3"}, jira.Config{})
	if err != nil {
		t.Fatalf("LookupIssues: %v — a per-key miss is a counted skip, not a whole-call failure", err)
	}
	if stats.IssuesFetched != 2 || stats.FetchFailed != 1 {
		t.Errorf("Stats = fetched %d / fetch_failed %d, want 2 / 1", stats.IssuesFetched, stats.FetchFailed)
	}
	if len(sink.inserts) != 2 {
		t.Errorf("raw rows written = %d, want 2 (the two issues that exist)", len(sink.inserts))
	}
}

// SWT-40 Part D review fix 1: the caller learns WHICH keys were fetched, not
// just how many. An unchanged refetch leaves ingested_at alone (upsertRaw's
// hash short-circuit), so the capture-time gate needs the per-key fact to know
// a snapshot was verified by this pass. Never serialized into sync_runs.stats.
func TestLookupIssues_ReportsWhichKeysItFetched(t *testing.T) {
	f := newFakeJira()
	defer f.close()
	lookupFixtures(f) // ILK-1..3 exist; ILK-9 does not
	sink := newJiraFakeSink()

	stats, err := jira.LookupIssues(context.Background(), newLookupClient(f), sink,
		lookupAccount(f.url()), []string{"ILK-1", "ILK-9", "ILK-3"}, jira.Config{})
	if err != nil {
		t.Fatalf("LookupIssues: %v", err)
	}
	if got := strings.Join(stats.FetchedKeys, ","); got != "ILK-1,ILK-3" {
		t.Errorf("Stats.FetchedKeys = [%s], want [ILK-1,ILK-3] (the failed ILK-9 excluded)", got)
	}
	raw, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("marshal stats: %v", err)
	}
	if strings.Contains(string(raw), "ILK-") {
		t.Errorf("Stats marshals its fetched keys into sync_runs.stats: %s", raw)
	}
}
