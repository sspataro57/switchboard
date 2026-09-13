//go:build integration

package tools_test

// SWT-40 Part B criterion B8 against a real database: route_candidate_add and
// route_candidate_remove through executor.Execute with the REAL registry and
// the REAL policy matrix (deliveryExecutor), so a missing refusal, a policy
// hole or a missing audit row fails here.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_isob?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run RouteCandidates ./internal/tools/
//
// Build-tagged `integration`, env-gated on DATABASE_URL, FATAL on 192.168.50.49.
//
// IMPOSED SURFACE: see routecandidates_test.go. Beyond it, the refusals B8 lists
// are the HANDLER's (they need the database): an unknown account, an unknown
// project, an empty description, a second default for one account. Two
// conservative readings are pinned too:
//   - an account_email that names accounts under two providers is AMBIGUOUS
//     and refused (source_accounts is unique on (provider, account_email));
//   - adding a project the account already lists is refused, and removing a
//     candidate that does not exist is an ERROR, never a silent no-op.
//
// CLEANUP PACT: owns accounts by provider itest-routecand-src / -src2, projects
// itest-routecand-%, and audit rows of the two tools whose args name
// itest-routecand. FK order: source_account_projects, policy_decisions,
// audit_events, projects, source_accounts.
//
// GREENFIELD NOTE — EXPECTED RED: the tools are not registered (every Execute
// returns "unknown tool"); past that, source_account_projects needs migration 0032.

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
	rcProvider  = "itest-routecand-src"
	rcProvider2 = "itest-routecand-src2"
	rcHuman     = "opsctl:itest-routecand"
	rcAccount   = "itest-routecand-a@example.test"
	rcDup       = "itest-routecand-dup@example.test"
)

func routeCandCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE provider IN ('` + rcProvider + `','` + rcProvider2 + `'))`
	const audited = `(SELECT id FROM audit_events WHERE tool LIKE 'route_candidate_%' AND args::text LIKE '%itest-routecand%')`
	for _, q := range []string{
		`DELETE FROM source_account_projects WHERE source_account_id IN ` + accts +
			` OR project_id IN (SELECT id FROM projects WHERE slug LIKE 'itest-routecand-%')`,
		`DELETE FROM policy_decisions WHERE audit_event_id IN ` + audited,
		`DELETE FROM audit_events WHERE id IN ` + audited,
		`DELETE FROM projects WHERE slug LIKE 'itest-routecand-%'`,
		`DELETE FROM source_accounts WHERE provider IN ('` + rcProvider + `','` + rcProvider2 + `')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func TestRouteCandidates_Integration_AddRemoveAndTheirRefusals(t *testing.T) {
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
	routeCandCleanup(t, ctx, pool)
	t.Cleanup(func() { routeCandCleanup(t, context.Background(), pool) })

	ex := deliveryExecutor(pool)
	account := func(provider, email string) int64 {
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO source_accounts (provider, account_email, send_enabled)
		                              VALUES ($1,$2,false) RETURNING id`, provider, email).Scan(&id); err != nil {
			t.Fatalf("seed account: %v", err)
		}
		return id
	}
	account(rcProvider, rcAccount)
	account(rcProvider, rcDup)
	account(rcProvider2, rcDup)
	p1 := seedProject(t, ctx, pool, "itest-routecand-one", "itest-routecand-client")
	p2 := seedProject(t, ctx, pool, "itest-routecand-two", "itest-routecand-client")
	p3 := seedProject(t, ctx, pool, "itest-routecand-three", "itest-routecand-client")

	call := func(actor, tool string, args map[string]any) error {
		t.Helper()
		raw, _ := json.Marshal(args)
		_, err := ex.Execute(ctx, executor.Call{Tool: tool, Actor: actor, Args: raw})
		return err
	}
	rows := func(account string) map[int64]struct {
		def  bool
		desc string
	} {
		t.Helper()
		out := map[int64]struct {
			def  bool
			desc string
		}{}
		r, err := pool.Query(ctx, `SELECT sap.project_id, sap.is_default, sap.description
		                             FROM source_account_projects sap JOIN source_accounts sa ON sa.id = sap.source_account_id
		                            WHERE sa.account_email = $1`, account)
		if err != nil {
			t.Fatalf("read candidates: %v", err)
		}
		defer r.Close()
		for r.Next() {
			var id int64
			var def bool
			var desc string
			if err := r.Scan(&id, &def, &desc); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out[id] = struct {
				def  bool
				desc string
			}{def, desc}
		}
		return out
	}

	// ---- add ----
	if err := call(rcHuman, "route_candidate_add", map[string]any{
		"account_email": rcAccount, "project": "itest-routecand-one",
		"description": "university integrations (itest-routecand)", "is_default": true}); err != nil {
		t.Fatalf("add a default candidate: %v", err)
	}
	if err := call(rcHuman, "route_candidate_add", map[string]any{
		"account_email": rcAccount, "project": "itest-routecand-two", "description": "engine rebuild (itest-routecand)"}); err != nil {
		t.Fatalf("add a second, non-default candidate: %v", err)
	}
	got := rows(rcAccount)
	if len(got) != 2 || !got[p1].def || got[p2].def || got[p1].desc != "university integrations (itest-routecand)" {
		t.Errorf("candidates = %+v, want p1 default with its description and p2 not default", got)
	}

	for _, tc := range []struct {
		name  string
		actor string
		args  map[string]any
	}{
		{"a second default", rcHuman, map[string]any{"account_email": rcAccount, "project": "itest-routecand-three",
			"description": "x itest-routecand", "is_default": true}},
		{"an unknown account", rcHuman, map[string]any{"account_email": "itest-routecand-nobody@example.test",
			"project": "itest-routecand-three", "description": "x itest-routecand"}},
		{"an unknown project", rcHuman, map[string]any{"account_email": rcAccount, "project": "itest-routecand-nope",
			"description": "x itest-routecand"}},
		{"an empty description", rcHuman, map[string]any{"account_email": rcAccount, "project": "itest-routecand-three",
			"description": ""}},
		{"a project the account already lists", rcHuman, map[string]any{"account_email": rcAccount,
			"project": "itest-routecand-one", "description": "again itest-routecand"}},
		{"an account email under two providers (ambiguous)", rcHuman, map[string]any{"account_email": rcDup,
			"project": "itest-routecand-three", "description": "x itest-routecand"}},
		{"a worker console (humanOnly)", "mcp:acme", map[string]any{"account_email": rcAccount,
			"project": "itest-routecand-three", "description": "x itest-routecand"}},
		{"the drafts worker (humanOnly)", "drafts:gpt", map[string]any{"account_email": rcAccount,
			"project": "itest-routecand-three", "description": "x itest-routecand"}},
	} {
		if err := call(tc.actor, "route_candidate_add", tc.args); err == nil {
			t.Errorf("route_candidate_add accepted %s; B8 refuses it", tc.name)
		}
	}
	if got := rows(rcAccount); len(got) != 2 {
		t.Errorf("after the refusals the account lists %d candidates, want still 2: %+v", len(got), got)
	}
	if _, ok := rows(rcAccount)[p3]; ok {
		t.Errorf("a refused add wrote a row for project three")
	}
	if n := len(rows(rcDup)); n != 0 {
		t.Errorf("the ambiguous account email got %d candidate row(s)", n)
	}

	// ---- audited (invariant 3) ----
	var okAdds int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events
	                               WHERE tool = 'route_candidate_add' AND actor = $1 AND status = 'ok'`, rcHuman).Scan(&okAdds); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if okAdds != 2 {
		t.Errorf("%d ok audit_events rows for route_candidate_add by %s, want 2 — every config write goes validate → "+
			"policy → audit → handler", okAdds, rcHuman)
	}

	// ---- remove ----
	if err := call("worker:acme", "route_candidate_remove", map[string]any{"account_email": rcAccount, "project": "itest-routecand-one"}); err == nil {
		t.Errorf("route_candidate_remove by worker:acme succeeded; humanOnly")
	}
	if err := call(rcHuman, "route_candidate_remove", map[string]any{"account_email": rcAccount, "project": "itest-routecand-two"}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	got = rows(rcAccount)
	if _, ok := got[p2]; ok || len(got) != 1 {
		t.Errorf("after removing project two the account lists %+v, want only project one", got)
	}
	if err := call(rcHuman, "route_candidate_remove", map[string]any{"account_email": rcAccount, "project": "itest-routecand-two"}); err == nil {
		t.Errorf("removing a candidate that no longer exists succeeded; want an error, never a silent no-op")
	}
	var okRemoves int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events
	                               WHERE tool = 'route_candidate_remove' AND actor = $1 AND status = 'ok'`, rcHuman).Scan(&okRemoves); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if okRemoves != 1 {
		t.Errorf("%d ok audit rows for route_candidate_remove, want 1", okRemoves)
	}
}
