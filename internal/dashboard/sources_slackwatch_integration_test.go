//go:build integration

package dashboard_test

// slack-watch-sweep (SWT-75) criterion 28: /sources lists each slack_watch row
// — label, workspace, conversation, enabled, last read — and renders correctly
// with an EMPTY table.
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_slackwatch?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run SourcesSlackWatch ./internal/dashboard/
//
// The REAL dashboard.Server runs under httptest with dev-login (OIDC_ISSUER
// unset), reusing dashGuard / dashPool / newDashServer / get from
// dashboard_integration_test.go (same package, same tag). Read-only: the panel
// writes nothing, so it needs no executor path (invariant 3 governs actions,
// and there are none here). Editing is `opsctl slack-watch`, deliberately —
// the SPEC lists a dashboard form as Future work.
//
// CROSS-POLLUTION PACT (IK): /sources is a GLOBAL page, so every assertion here
// is a CONTAINMENT assertion about this suite's own rows. There is no "the page
// shows 2 watch rows" anywhere.
//
// This suite owns the workspace 'TDASHWATCH'.
//
// RED TODAY: migration 0041 is not applied and /sources has no watch panel.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	dwWorkspace = "TDASHWATCH"
	dwConv      = "DDASHWATCH1"
	dwLabel     = "itest-dash-watch-jose"
)

func dwCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM slack_watch WHERE workspace_id = $1`, dwWorkspace); err != nil {
		if strings.Contains(err.Error(), "slack_watch") {
			t.Fatalf("cleanup slack_watch: %v — criterion 28's panel reads a table migration 0041 creates; "+
				"apply it first", err)
		}
		t.Fatalf("cleanup slack_watch: %v", err)
	}
}

// Criterion 28: the row, with every field an operator needs to answer "is this
// conversation being swept, and when was it last read?" without opening psql.
func TestSourcesSlackWatch_ListsTheWatchedConversations(t *testing.T) {
	ctx := context.Background()
	dashGuard(t)
	pool := dashPool(t, ctx)
	t.Cleanup(pool.Close)
	dwCleanup(t, ctx, pool)
	t.Cleanup(func() { dwCleanup(t, context.Background(), pool) })

	if _, err := pool.Exec(ctx,
		`INSERT INTO slack_watch (workspace_id, conversation_id, label, enabled) VALUES ($1,$2,$3,true)`,
		dwWorkspace, dwConv, dwLabel); err != nil {
		t.Fatalf("seed slack_watch: %v", err)
	}

	server, client := newDashServer(t, ctx, pool)
	code, body := get(t, client, server.URL+"/sources")
	if code != 200 {
		t.Fatalf("GET /sources = %d, want 200", code)
	}
	for _, want := range []string{dwLabel, dwWorkspace, dwConv} {
		if !strings.Contains(body, want) {
			t.Errorf("/sources does not contain %q. Criterion 28: the page lists each slack_watch row (label, "+
				"workspace, conversation, enabled, last read) — the only place the watch list is visible "+
				"without psql, since the tools are humanOnly and off every MCP profile", want)
		}
	}
}

// Criterion 28's second half, and the state the page is opened in on day one:
// an EMPTY slack_watch must render, not 500 and not vanish. /sources already
// 500s the whole page when any one query errors
// (internal/dashboard/funnel_test.go:262), so a new query that cannot cope with
// zero rows takes the operator's only ingestion view down at exactly the moment
// they are trying to work out why nothing is arriving.
func TestSourcesSlackWatch_RendersWithAnEmptyWatchList(t *testing.T) {
	ctx := context.Background()
	dashGuard(t)
	pool := dashPool(t, ctx)
	t.Cleanup(pool.Close)
	dwCleanup(t, ctx, pool)

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM slack_watch`).Scan(&n); err != nil {
		t.Fatalf("count slack_watch: %v", err)
	}
	if n != 0 {
		t.Skipf("slack_watch holds %d rows from another suite; the empty-table render is only assertable on "+
			"an isolated database (SPEC Verification step 3)", n)
	}

	server, client := newDashServer(t, ctx, pool)
	code, body := get(t, client, server.URL+"/sources")
	if code != 200 {
		t.Fatalf("GET /sources with an empty slack_watch = %d, want 200 (criterion 28)", code)
	}
	if !strings.Contains(body, "Ingestion") {
		t.Errorf("/sources rendered %d bytes without its own <h1>Ingestion</h1>; the page must degrade to "+
			"today's content, never to an error page (criterion 28)", len(body))
	}
}
