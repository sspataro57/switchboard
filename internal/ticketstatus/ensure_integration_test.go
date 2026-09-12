//go:build integration

package ticketstatus_test

// SWT-40 Part D, criterion D2's second half (docs/tickets/inquiry-promote_SPEC.md,
// D-D3): the snapshot half of Run is extracted into an exported
//
//	type Snapshot struct {
//	    Raw          []byte    // the STORED raw issue row (D19: decide from storage)
//	    IngestedAt   time.Time
//	    OwnAccountID string    // the storing account's sync_cursor->>'own_account_id'
//	    Count        int       // stored rows for this key; >1 is criterion 31's ambiguity
//	}
//
//	func EnsureSnapshots(ctx context.Context, pool *pgxpool.Pool, keys []string,
//	                     cfg Config) (map[string]Snapshot, Stats, error)
//
// covering routing (RouteLookup), TTL freshness, jira.LookupIssues and the
// read-back. The reconciler AND the capture-time gate call it, so a key either
// of them fetched inside the TTL is not fetched again by the other (D-D5).
//
// The SPEC writes the return as `(map[key]snapshot, Stats)`; this file adds the
// `error` every DB-touching function in this repo returns (IK: wrap errors, no
// bare returns). Stats is the existing ticketstatus.Stats — its Fetched,
// FetchSkippedTTL and FetchFailed fields.
//
// The reconciler's own suite (store_integration_test.go) is deliberately
// UNMODIFIED; criterion D2 is that it still passes after the extraction.
//
// Reuses that suite's fake Jira server, fixture helpers and cleanup (same
// package, same build tag). Owns source account itest-tstatus-ensure@example.com,
// which tsCleanup's LIKE 'itest-tstatus-%' already clears.
//
// GREENFIELD NOTE — EXPECTED RED: EnsureSnapshots and Snapshot do not exist, so
// this file compile-FAILs the integration build.

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/connector/jira"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/ticketstatus"
)

const esLookupAcct = "itest-tstatus-ensure@example.com"

func TestTicketStatus_Integration_EnsureSnapshotsFetchesStoresAndHonoursTheTTL(t *testing.T) {
	ctx := context.Background()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use the compose db on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	tsRequireStateTable(t, ctx, pool)
	tsCleanup(t, ctx, pool)
	t.Cleanup(func() { tsCleanup(t, ctx, pool) })

	fake := newTSFakeJira()
	t.Cleanup(fake.close)

	var acct int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, domain_default, scopes, send_enabled, sync_cursor)
		 VALUES ('jira_lookup',$1,$2,'{ENS}',false,'{}'::jsonb) RETURNING id`,
		esLookupAcct, fake.url()).Scan(&acct); err != nil {
		t.Fatalf("seed lookup account: %v", err)
	}
	// ENS-2 is already stored and FRESH: the TTL must skip it.
	if _, err := pool.Exec(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, ingested_at)
		 VALUES ($1,$2,$3::jsonb,'itest-tstatus-ensure-2', now())`,
		acct, jira.IssueRawID("ENS-2"), tsIssueJSON(tsIssue{"ENS-2", "indeterminate", tsLookupID, true})); err != nil {
		t.Fatalf("seed fresh snapshot: %v", err)
	}
	fake.put(tsIssue{"ENS-1", "indeterminate", tsLookupID, true})
	// ENS-3 is on nobody's server: a per-key 404, which leaves no snapshot.
	keys := []string{"ENS-1", "ENS-2", "ENS-3"}

	snaps, stats, err := ticketstatus.EnsureSnapshots(ctx, pool, keys, ticketstatus.Config{Lookup: fake.factory()})
	if err != nil {
		t.Fatalf("EnsureSnapshots: %v", err)
	}
	if got := fake.gets(); !reflect.DeepEqual(got, []string{"ENS-1"}) {
		t.Errorf("issue GETs = %v, want exactly [ENS-1]: ENS-2 is fresh inside the TTL and must not be re-fetched "+
			"(D-D5: the stored snapshot IS the cache)", got)
	}
	s1, ok := snaps["ENS-1"]
	if !ok {
		t.Fatalf("no snapshot for ENS-1 after fetching it; EnsureSnapshots must RE-READ what LookupIssues stored "+
			"(write-then-read-back, D19). got keys %v", mapKeys(snaps))
	}
	if s1.Count != 1 || len(s1.Raw) == 0 {
		t.Errorf("ENS-1 snapshot = (count %d, %d raw bytes), want one stored row with its raw JSON", s1.Count, len(s1.Raw))
	}
	if s1.OwnAccountID != tsLookupID {
		t.Errorf("ENS-1 own account id = %q, want %q (the STORING account's /myself identity, D12)", s1.OwnAccountID, tsLookupID)
	}
	if facts, err := jira.IssueFacts(s1.Raw); err != nil || facts.Assignee != tsLookupID || !facts.StatusKnown {
		t.Errorf("ENS-1 raw does not read back as the fetched issue: facts=%+v err=%v", facts, err)
	}
	if s2, ok := snaps["ENS-2"]; !ok || s2.Count != 1 {
		t.Errorf("ENS-2 (fresh, stored) snapshot = %+v (present %v), want the stored row returned without a fetch", s2, ok)
	}
	if _, ok := snaps["ENS-3"]; ok {
		t.Errorf("ENS-3 has a snapshot although its fetch 404'd; a per-key failure leaves NO snapshot (the " +
			"caller's evidence gap, never a verdict)")
	}
	if stats.Fetched != 1 || stats.FetchSkippedTTL != 1 || stats.FetchFailed != 1 {
		t.Errorf("stats = Fetched %d / FetchSkippedTTL %d / FetchFailed %d, want 1/1/1",
			stats.Fetched, stats.FetchSkippedTTL, stats.FetchFailed)
	}

	// Second call inside the TTL: nothing that was fetched is fetched again.
	_, again, err := ticketstatus.EnsureSnapshots(ctx, pool, keys, ticketstatus.Config{Lookup: fake.factory()})
	if err != nil {
		t.Fatalf("EnsureSnapshots (again): %v", err)
	}
	if got := fake.gets(); !reflect.DeepEqual(got, []string{"ENS-1"}) {
		t.Errorf("issue GETs after a second call inside the TTL = %v, want still [ENS-1]", got)
	}
	if again.Fetched != 0 || again.FetchSkippedTTL != 2 {
		t.Errorf("second call stats = Fetched %d / FetchSkippedTTL %d, want 0/2", again.Fetched, again.FetchSkippedTTL)
	}

	// No credential: stored rows still come back, and nothing reaches the server.
	before := fake.requests()
	nilSnaps, _, err := ticketstatus.EnsureSnapshots(ctx, pool, []string{"ENS-1", "ENS-4"}, ticketstatus.Config{})
	if err != nil {
		t.Fatalf("EnsureSnapshots (nil Lookup): %v", err)
	}
	if fake.requests() != before {
		t.Errorf("a nil Lookup factory reached the server (%d -> %d requests); D21: no credential, no fetch",
			before, fake.requests())
	}
	if _, ok := nilSnaps["ENS-1"]; !ok {
		t.Errorf("with no credential the STORED ENS-1 snapshot was not returned; the stored row still decides")
	}
}

func mapKeys(m map[string]ticketstatus.Snapshot) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
