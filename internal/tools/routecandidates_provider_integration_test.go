//go:build integration

package tools_test

// B8 amendment 2026-09-13: route_candidate_add / route_candidate_remove take an
// optional `provider`, so an address that exists under several providers can
// still be targeted. Prod refused the collaboratory seed because
// salvador@handsonconnect.org is a google, a jira and a jira_lookup account
// (source_accounts is unique on (provider, account_email)).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_isoprov?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run RouteCandidates ./internal/tools/
//
// Build-tagged `integration`, env-gated on DATABASE_URL, FATAL on 192.168.50.49.
//
// CLEANUP PACT: owns accounts whose account_email is itest-routeprov-%@example.test
// (under the REAL provider names google / jira / jira_lookup: the provider list
// is the DB's, and the point is the prod shape), projects itest-routeprov-%, and
// audit rows of the two tools whose args name itest-routeprov. FK order:
// source_account_projects, policy_decisions, audit_events, projects,
// source_accounts.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
)

const (
	rpHuman  = "opsctl:itest-routeprov"
	rpTriple = "itest-routeprov-triple@example.test" // google + jira + jira_lookup, the prod shape
	rpSingle = "itest-routeprov-single@example.test" // google only
)

func routeProvCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE account_email LIKE 'itest-routeprov-%@example.test')`
	const audited = `(SELECT id FROM audit_events WHERE tool LIKE 'route_candidate_%' AND args::text LIKE '%itest-routeprov%')`
	for _, q := range []string{
		`DELETE FROM source_account_projects WHERE source_account_id IN ` + accts +
			` OR project_id IN (SELECT id FROM projects WHERE slug LIKE 'itest-routeprov-%')`,
		`DELETE FROM policy_decisions WHERE audit_event_id IN ` + audited,
		`DELETE FROM audit_events WHERE id IN ` + audited,
		`DELETE FROM projects WHERE slug LIKE 'itest-routeprov-%'`,
		`DELETE FROM source_accounts WHERE account_email LIKE 'itest-routeprov-%@example.test'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func TestRouteCandidates_Integration_ProviderDisambiguates(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use an isolated compose db on :5433")
	}
	t.Cleanup(pool.Close)
	var present bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('source_account_projects') IS NOT NULL`).Scan(&present); err != nil || !present {
		t.Fatalf("source_account_projects does not exist (%v): migrations/0032_route_tier.sql is not applied", err)
	}
	routeProvCleanup(t, ctx, pool)
	t.Cleanup(func() { routeProvCleanup(t, context.Background(), pool) })

	ex := deliveryExecutor(pool)
	account := func(provider, email string) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO source_accounts (provider, account_email, send_enabled)
		                              VALUES ($1,$2,false) RETURNING id`, provider, email).Scan(&id); err != nil {
			t.Fatalf("seed account: %v", err)
		}
		return id
	}
	// The resolver lists accounts ORDER BY provider, id, so google sorts first
	// whatever the seeding order. A "first match" bug is therefore caught by the
	// provider=jira add below, not by the google one.
	jiraID := account("jira", rpTriple)
	googleID := account("google", rpTriple)
	lookupID := account("jira_lookup", rpTriple)
	singleID := account("google", rpSingle)
	pOne := seedProject(t, ctx, pool, "itest-routeprov-one", "itest-routeprov-client")
	seedProject(t, ctx, pool, "itest-routeprov-two", "itest-routeprov-client")

	call := func(tool string, args map[string]any) error {
		t.Helper()
		raw, _ := json.Marshal(args)
		_, err := ex.Execute(ctx, executor.Call{Tool: tool, Actor: rpHuman, Args: raw})
		return err
	}
	// candidates maps source_account_id -> project_ids for every account under email.
	candidates := func(email string) map[int64][]int64 {
		t.Helper()
		out := map[int64][]int64{}
		r, err := pool.Query(ctx, `SELECT sap.source_account_id, sap.project_id
		                             FROM source_account_projects sap JOIN source_accounts sa ON sa.id = sap.source_account_id
		                            WHERE sa.account_email = $1 ORDER BY sap.id`, email)
		if err != nil {
			t.Fatalf("read candidates: %v", err)
		}
		defer r.Close()
		for r.Next() {
			var acct, proj int64
			if err := r.Scan(&acct, &proj); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out[acct] = append(out[acct], proj)
		}
		if err := r.Err(); err != nil {
			t.Fatalf("iterate candidates: %v", err)
		}
		return out
	}
	addArgs := func(email string, provider any) map[string]any {
		m := map[string]any{"account_email": email, "project": "itest-routeprov-one",
			"description": "university integrations (itest-routeprov)", "is_default": true}
		if provider != nil {
			m["provider"] = provider
		}
		return m
	}

	// ---- add without provider: still ambiguous, and the error says what to do ----
	err := call("route_candidate_add", addArgs(rpTriple, nil))
	if err == nil {
		t.Fatalf("add on a three-provider address with no provider succeeded; want the ambiguous refusal")
	}
	if !strings.Contains(err.Error(), "pass provider") {
		t.Errorf("ambiguous refusal = %q; want it to tell the caller to pass provider", err)
	}
	for _, p := range []string{"google", "jira", "jira_lookup"} {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("ambiguous refusal = %q; want it to name provider %q", err, p)
		}
	}
	if n := len(candidates(rpTriple)); n != 0 {
		t.Fatalf("a refused add wrote candidate rows: %+v", candidates(rpTriple))
	}

	// ---- an unknown provider: refused, naming the providers that do exist ----
	err = call("route_candidate_add", addArgs(rpTriple, "gmail"))
	if err == nil {
		t.Fatalf("add with provider=gmail (no such account) succeeded")
	}
	for _, want := range []string{`"gmail"`, "google", "jira", "jira_lookup"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("unknown-provider refusal = %q; want it to contain %s", err, want)
		}
	}
	// On a SINGLE-provider address a wrong provider must not fall through to the
	// one account that exists: that would route the google mailbox for a caller
	// who asked for jira.
	err = call("route_candidate_add", addArgs(rpSingle, "jira"))
	if err == nil {
		t.Errorf("add on a google-only address with provider=jira succeeded; want refused")
	} else if !strings.Contains(err.Error(), "google") {
		t.Errorf("unknown-provider refusal = %q; want it to name the existing provider google", err)
	}
	if n := len(candidates(rpSingle)); n != 0 {
		t.Fatalf("a refused add on the single address wrote rows: %+v", candidates(rpSingle))
	}

	// ---- add with provider=google: one row, on the google account only ----
	if err := call("route_candidate_add", addArgs(rpTriple, "google")); err != nil {
		t.Fatalf("add with provider=google: %v", err)
	}
	got := candidates(rpTriple)
	if len(got) != 1 || len(got[googleID]) != 1 || got[googleID][0] != pOne {
		t.Errorf("candidates = %+v; want exactly project %d on the google account %d (jira %d, jira_lookup %d untouched)",
			got, pOne, googleID, jiraID, lookupID)
	}

	// ---- remove without provider: ambiguous, nothing removed ----
	err = call("route_candidate_remove", map[string]any{"account_email": rpTriple, "project": "itest-routeprov-one"})
	if err == nil {
		t.Errorf("remove on a three-provider address with no provider succeeded; want the ambiguous refusal")
	} else if !strings.Contains(err.Error(), "pass provider") {
		t.Errorf("ambiguous refusal on remove = %q; want it to tell the caller to pass provider", err)
	}
	if n := len(candidates(rpTriple)[googleID]); n != 1 {
		t.Fatalf("a refused remove changed the google account's candidates: %+v", candidates(rpTriple))
	}

	// ---- remove with provider=google: removes it ----
	if err := call("route_candidate_remove", map[string]any{"account_email": rpTriple, "project": "itest-routeprov-one",
		"provider": "google"}); err != nil {
		t.Fatalf("remove with provider=google: %v", err)
	}
	if got := candidates(rpTriple); len(got) != 0 {
		t.Errorf("after remove with provider=google the address still lists %+v", got)
	}

	// ---- add with a provider that does NOT sort first: the row lands on THAT account ----
	// google is ids[0] under ORDER BY provider, id, so only a non-first provider
	// proves the resolver returns the named account rather than the first match.
	if err := call("route_candidate_add", addArgs(rpTriple, "jira")); err != nil {
		t.Fatalf("add with provider=jira: %v", err)
	}
	got = candidates(rpTriple)
	if len(got) != 1 || len(got[jiraID]) != 1 || got[jiraID][0] != pOne {
		t.Errorf("candidates = %+v; want exactly project %d on the jira account %d (google %d, jira_lookup %d untouched)",
			got, pOne, jiraID, googleID, lookupID)
	}
	if err := call("route_candidate_remove", map[string]any{"account_email": rpTriple, "project": "itest-routeprov-one",
		"provider": "jira"}); err != nil {
		t.Fatalf("remove with provider=jira: %v", err)
	}
	if got := candidates(rpTriple); len(got) != 0 {
		t.Errorf("after remove with provider=jira the address still lists %+v", got)
	}

	// ---- a single-provider address without provider: today's behaviour ----
	if err := call("route_candidate_add", map[string]any{"account_email": rpSingle, "project": "itest-routeprov-two",
		"description": "engine rebuild (itest-routeprov)"}); err != nil {
		t.Fatalf("add on a single-provider address with no provider: %v", err)
	}
	if got := candidates(rpSingle); len(got) != 1 || len(got[singleID]) != 1 {
		t.Errorf("single-provider candidates = %+v; want one row on account %d", got, singleID)
	}
	if err := call("route_candidate_remove", map[string]any{"account_email": rpSingle, "project": "itest-routeprov-two"}); err != nil {
		t.Fatalf("remove on a single-provider address with no provider: %v", err)
	}
	if got := candidates(rpSingle); len(got) != 0 {
		t.Errorf("after remove the single address still lists %+v", got)
	}

	// ---- audited (invariant 3): the provider travels in the audited args ----
	var withProvider int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events
	                               WHERE tool LIKE 'route_candidate_%' AND actor = $1 AND status = 'ok'
	                                 AND args->>'provider' = 'google'`, rpHuman).Scan(&withProvider); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if withProvider != 2 {
		t.Errorf("%d ok audit rows carry provider=google, want 2 (the add and the remove)", withProvider)
	}
}
