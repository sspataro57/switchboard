//go:build integration

package dashboard_test

// Integration regression tests for bug slackweb-collab-export-stale (Jira
// SWT-39), the dashboard half of fix D. docs/bugs/slackweb-collab-export-stale_DIAGNOSIS.md.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run SWT39 ./internal/dashboard/
//
// After SWT-39, slackweb finishes a run whose leaf deferred or could not read
// conversations as 'partial' (migration 0027), not 'ok'. The /funnel connector
// health query computes last_ok from `status='ok'` only (funnel.go), so a
// workspace that is ALWAYS partial would render `never` (or go `stale`)
// although it syncs every 30 minutes. Two rules:
//
//  1. a 'partial' run is a successful run for freshness (last_ok);
//  2. when the latest run is partial the row SAYS `partial`, so the coverage
//     gap stays visible instead of hiding behind a green `ok`.
//
// Reuses funSuite / funRow / newDashServer from funnel_integration_test.go (same
// package, same tag). Accounts are 'itest-funnel-p*', inside that suite's
// cleanup pattern, and on its private provider so no slackweb code sees them.
// Every assertion is about a ROW of this suite's own seeded account.
//
// EXPECTED RED: on a db at 0026 every test stops at funRequirePartialStatus.
// After 0027: the partial-only account shows `never`, and no row ever says
// `partial` (the template renders any verdict other than ok/stale as never).

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	funAcctPA = "itest-funnel-pa@local"
	funAcctPB = "itest-funnel-pb@local"
	funPhase  = "slack_web"
)

func funRequirePartialStatus(t *testing.T, ctx context.Context, s *funSuite) {
	t.Helper()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin probe: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, send_enabled, calendar_in_availability)
		 VALUES ($1,'itest-funnel-probe@local',false,false) RETURNING id`, funProvider).Scan(&id); err != nil {
		t.Fatalf("probe account: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sync_runs (source_account_id, status) VALUES ($1,'partial')`, id); err != nil {
		t.Fatalf("sync_runs rejects status 'partial' (%v). Apply migration 0027 to the compose db first", err)
	}
}

func TestRegression_SWT39_FunnelPartialRunCountsAsSuccessfulSync(t *testing.T) {
	ctx := context.Background()
	s := newFunSuite(t, ctx)
	funRequirePartialStatus(t, ctx, s)

	// The Collaboratory shape after SWT-39: every run partial, every 30 minutes.
	pa := s.account(t, ctx, funProvider, funAcctPA, false)
	s.syncRun(t, ctx, pa, funPhase, "partial", 40*time.Minute)
	s.syncRun(t, ctx, pa, funPhase, "partial", 10*time.Minute)

	ts, client := newDashServer(t, ctx, s.pool)
	defer ts.Close()
	code, body := get(t, client, ts.URL+"/funnel")
	if code != http.StatusOK {
		t.Fatalf("GET /funnel = %d, want 200\n%s", code, snippet(body))
	}
	row := funRow(t, body, funAcctPA, funPhase)
	if strings.Contains(row, "never") {
		t.Errorf("%s / %s shows `never` with a partial run 10 minutes ago:\n%s\n\nA 'partial' run synced; it "+
			"just did not read every conversation. Computing last_ok from status='ok' alone makes a workspace "+
			"that is always partial look like a connector that has never worked", funAcctPA, funPhase, row)
	}
	if strings.Contains(row, "stale") {
		t.Errorf("%s / %s is marked stale with a partial run 10 minutes ago (threshold 3h):\n%s",
			funAcctPA, funPhase, row)
	}
}

func TestRegression_SWT39_FunnelShowsPartialWhenLatestRunIsPartial(t *testing.T) {
	ctx := context.Background()
	s := newFunSuite(t, ctx)
	funRequirePartialStatus(t, ctx, s)

	// Latest run partial: the row must say so.
	pa := s.account(t, ctx, funProvider, funAcctPA, false)
	s.syncRun(t, ctx, pa, funPhase, "ok", 30*time.Minute)
	s.syncRun(t, ctx, pa, funPhase, "partial", 5*time.Minute)

	// Control: latest run ok after an older partial. Nothing to warn about.
	pb := s.account(t, ctx, funProvider, funAcctPB, false)
	s.syncRun(t, ctx, pb, funPhase, "partial", 30*time.Minute)
	s.syncRun(t, ctx, pb, funPhase, "ok", 5*time.Minute)

	ts, client := newDashServer(t, ctx, s.pool)
	defer ts.Close()
	code, body := get(t, client, ts.URL+"/funnel")
	if code != http.StatusOK {
		t.Fatalf("GET /funnel = %d, want 200\n%s", code, snippet(body))
	}

	rowA := funRow(t, body, funAcctPA, funPhase)
	if !strings.Contains(rowA, "partial") {
		t.Errorf("%s / %s: the latest run is partial and the row does not say `partial`:\n%s\n\n"+
			"SWT-39's lesson is that a green `ok` over a 7-of-38 export hid a 17-hour delay. Counting partial "+
			"as fresh must not hide it again", funAcctPA, funPhase, rowA)
	}
	if strings.Contains(rowA, "never") || strings.Contains(rowA, "stale") {
		t.Errorf("%s / %s synced 5 minutes ago and renders never/stale:\n%s", funAcctPA, funPhase, rowA)
	}

	rowB := funRow(t, body, funAcctPB, funPhase)
	if strings.Contains(rowB, "partial") {
		t.Errorf("%s / %s: the latest run is ok, yet the row says `partial`:\n%s\nThe verdict follows the "+
			"LATEST run; an old partial is history", funAcctPB, funPhase, rowB)
	}
	if strings.Contains(rowB, "never") || strings.Contains(rowB, "stale") {
		t.Errorf("%s / %s synced 5 minutes ago and renders never/stale:\n%s", funAcctPB, funPhase, rowB)
	}
}
