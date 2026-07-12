//go:build integration

package jira_test

// Integration walk for the Jira connector end-to-end (SPEC
// 09-jira-github-connectors, criteria 3, 4, 5, 8 + invariants 1, 2, 5).
// Build-tagged `integration` AND env-gated on DATABASE_URL; uses the httptest
// fakeJira (fake_jira_test.go) — NEVER a live Jira call. Run with:
//
//   DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//     go test -tags integration ./internal/connector/jira/
//
// GREENFIELD NOTE: under `-tags integration` this compile-FAILs until the jira
// connector, PGSink, Run/Normalize, and migration 0007 exist — the expected
// failure mode. Imposed exported surface beyond the poller unit surface:
//
//   func NewSink(pool *pgxpool.Pool) *PGSink        // ingest Sink AND normalize/list store
//   func (*PGSink) ListAccounts(ctx context.Context) ([]Account, error) // provider='jira' rows
//   type ClientFactory func(ctx context.Context, acct Account) (*Client, error)
//   // Run ingests (raw-first) every provider='jira' account. No normalize.
//   func Run(ctx context.Context, sink *PGSink, factory ClientFactory, cfg Config) (Stats, error)
//   // Normalize reads ONLY raw_source_items: thread+message upserts (channel
//   // 'jira'), identity reconcile, own-message loop closure by
//   // external_message_id == sent_external_id (exact), plus the 120-char prefix
//   // matcher for failed sends with a NULL sent_external_id (criterion 8).
//   func Normalize(ctx context.Context, sink *PGSink, cfg Config) (Stats, error)
//
// The account's domain_default is the REAL base URL (so site_host in the keys is
// sspataro.atlassian.net) while the ClientFactory points the HTTP client at the
// fake — mirroring the google suite's factory indirection.
//
// Cross-suite discipline (SWT-6 mutual-cleanup pact): this suite creates ONE
// INBOUND jira normalized_messages row (criterion 5 shadow-triage proxy), which
// is visible to triage's GLOBAL pending filter — so cleanupTriage must gain
// matching 'jira:itest-jira-%' / 'itest-jira-%' deletes (the pact-join
// obligation on the triage side). This file cleans only its OWN corpus, FK
// ordered, rerunnably.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/jira"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	itSite       = "itest-jira.atlassian.net"
	itBaseURL    = "https://itest-jira.atlassian.net"
	itAcct       = "itest-jira@example.com"
	itSlug       = "itest-jira-proj"
	itIssueKey   = "IJ-1"
	itOwnAcc     = "acc-itest-own"
	itOtherAcc   = "acc-itest-client"
	itExactComm  = "700701" // own comment, matched by exact id equality
	itPrefixCom  = "700702" // own comment, matched by body-prefix (post-hoc)
	itPrefixBody = "shipped the itest-jira fix to staging tonight"
)

func itThreadKey() string   { return "jira:" + itSite + ":" + itIssueKey }
func itTargetRef() string   { return "jira:" + itSite + ":" + itIssueKey }
func itExactExtID() string  { return "jira:" + itSite + ":comment:" + itExactComm }
func itPrefixExtID() string { return "jira:" + itSite + ":comment:" + itPrefixCom }

func scanIntJ(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

func cleanupJiraConn(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM task_events WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{itSlug}},
		{`DELETE FROM deliveries WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{itSlug}},
		{`DELETE FROM normalized_messages WHERE thread_id IN (SELECT id FROM normalized_threads WHERE thread_key LIKE 'jira:'||$1||':%')`, []any{itSite}},
		{`DELETE FROM normalized_threads WHERE thread_key LIKE 'jira:'||$1||':%'`, []any{itSite}},
		{`DELETE FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='jira' AND account_email=$1)`, []any{itAcct}},
		{`DELETE FROM sync_runs WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='jira' AND account_email=$1)`, []any{itAcct}},
		{`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, []any{itSlug}},
		{`DELETE FROM projects WHERE slug=$1`, []any{itSlug}},
		{`DELETE FROM source_accounts WHERE provider='jira' AND account_email=$1`, []any{itAcct}},
	}
	for _, st := range stmts {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

func TestJira_Integration_PollNormalizeLoopClosure(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	defer pool.Close()

	// Migration 0007 artifacts (criterion 1).
	if got := scanIntJ(t, ctx, pool,
		`SELECT count(*) FROM information_schema.columns WHERE table_name='external_refs' AND column_name='created_at'`); got != 1 {
		t.Fatalf("external_refs.created_at missing — apply migration 0007 (make migrate)")
	}

	cleanupJiraConn(t, ctx, pool)
	defer cleanupJiraConn(t, ctx, pool)

	// jira account: real domain_default (drives site_host in keys), token stored
	// pgp-encrypted; send_enabled true (loop closure attaches to sent deliveries).
	var acctID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, refresh_token_encrypted, scopes, domain_default, send_enabled)
		 VALUES ('jira', $1, pgp_sym_encrypt('dummy','k'), ARRAY['IJ'], $2, true) RETURNING id`,
		itAcct, itBaseURL).Scan(&acctID); err != nil {
		t.Fatalf("seed jira account: %v", err)
	}

	// Project + work task + two prior deliveries on the same issue: one SENT with
	// an exact comment-id sent_external_id, one FAILED with a NULL id (the
	// ambiguous-send hole the prefix matcher must close).
	var projID, taskID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path)
		 VALUES ($1,$1,'itest-jira-client','manual','dashboard','/tmp/itest') RETURNING id`, itSlug).Scan(&projID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status) VALUES ($1,'ij work','claude','delivered') RETURNING id`, projID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	var sentDelivery, failedDelivery int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, sent_external_id, sent_at)
		 VALUES ($1,'jira_comment',$2,'exact-id delivery','sent', $3, now()) RETURNING id`,
		taskID, itTargetRef(), itExactExtID()).Scan(&sentDelivery); err != nil {
		t.Fatalf("seed sent delivery: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, sent_external_id)
		 VALUES ($1,'jira_comment',$2,$3,'failed', NULL) RETURNING id`,
		taskID, itTargetRef(), itPrefixBody).Scan(&failedDelivery); err != nil {
		t.Fatalf("seed failed delivery: %v", err)
	}

	// Fake site corpus: one issue with an inbound comment + our two own comments.
	fj := newFakeJira()
	defer fj.close()
	fj.ownAccountID = itOwnAcc
	fj.add(fakeIssue{
		key: itIssueKey, updated: "2026-07-10T09:00:00.000+0000", created: "2026-07-10T08:00:00.000+0000",
		summary: "IJ login broken", description: "500 on staging", reporter: itOtherAcc, assignee: itOwnAcc,
		comments: []fakeComment{
			{id: "500", author: itOtherAcc, body: "still broken on my side", created: "2026-07-10T08:30:00.000+0000"},
			{id: itExactComm, author: itOwnAcc, body: "looking into it", created: "2026-07-10T08:40:00.000+0000"},
			{id: itPrefixCom, author: itOwnAcc, body: itPrefixBody, created: "2026-07-10T08:50:00.000+0000"},
		},
	})

	factory := func(_ context.Context, a jira.Account) (*jira.Client, error) {
		return jira.NewClient(nil, fj.url(), a.Email, "tok"), nil
	}
	cfg := jira.Config{Overlap: time.Hour, Now: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)}

	// ---- Poll (raw-first) --------------------------------------------------
	if _, err := jira.Run(ctx, jira.NewSink(pool), factory, cfg); err != nil {
		t.Fatalf("Run (ingest): %v", err)
	}
	// 1 issue item + 3 comment items = 4 raw rows, all pending, all hashed.
	if got := scanIntJ(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items WHERE source_account_id=$1`, acctID); got != 4 {
		t.Errorf("raw rows = %d, want 4 (1 issue + 3 comments; raw-first, invariant 1)", got)
	}
	if got := scanIntJ(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items WHERE source_account_id=$1 AND normalized_at IS NULL`, acctID); got != 4 {
		t.Errorf("pending raw after ingest = %d, want 4 (raw lands before normalize)", got)
	}
	if got := scanIntJ(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items WHERE source_account_id=$1 AND external_id=$2`, acctID, "issue:"+itIssueKey); got != 1 {
		t.Errorf("issue raw item issue:%s missing", itIssueKey)
	}

	// ---- Normalize (one funnel) -------------------------------------------
	if _, err := jira.Normalize(ctx, jira.NewSink(pool), jira.Config{}); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	// One thread for the issue; channel 'jira' messages (1 description + 3 comments).
	if got := scanIntJ(t, ctx, pool, `SELECT count(*) FROM normalized_threads WHERE thread_key=$1`, itThreadKey()); got != 1 {
		t.Errorf("normalized_threads for the issue = %d, want 1", got)
	}
	if got := scanIntJ(t, ctx, pool,
		`SELECT count(*) FROM normalized_messages WHERE channel='jira' AND thread_id=(SELECT id FROM normalized_threads WHERE thread_key=$1)`, itThreadKey()); got != 4 {
		t.Errorf("jira-channel messages = %d, want 4 (description + 3 comments)", got)
	}
	// Criterion 5 proxy: the client comment is INBOUND (shadow triage sees it);
	// our own comments are OUTBOUND (never re-triaged, invariant 5).
	if got := scanIntJ(t, ctx, pool,
		`SELECT count(*) FROM normalized_messages WHERE external_message_id=$1 AND direction='inbound'`,
		"jira:"+itSite+":comment:500"); got != 1 {
		t.Errorf("client comment must be inbound (shadow-triage lane); got %d", got)
	}
	if got := scanIntJ(t, ctx, pool,
		`SELECT count(*) FROM normalized_messages WHERE external_message_id=$1 AND direction='outbound'`, itExactExtID()); got != 1 {
		t.Errorf("own comment must be outbound; got %d", got)
	}

	// ---- Loop closure by EXACT comment-id equality (invariant 5) ----------
	var exactConfirmed *string
	if err := pool.QueryRow(ctx, `SELECT confirmed_at::text FROM deliveries WHERE id=$1`, sentDelivery).Scan(&exactConfirmed); err != nil {
		t.Fatalf("read sent delivery: %v", err)
	}
	if exactConfirmed == nil {
		t.Errorf("sent delivery not confirmed by exact comment-id equality (external_message_id == sent_external_id)")
	}

	// ---- Loop closure by POST-HOC PREFIX match (criterion 8) --------------
	var prefixExtID, prefixConfirmed *string
	if err := pool.QueryRow(ctx,
		`SELECT sent_external_id, confirmed_at::text FROM deliveries WHERE id=$1`, failedDelivery).Scan(&prefixExtID, &prefixConfirmed); err != nil {
		t.Fatalf("read failed delivery: %v", err)
	}
	if prefixExtID == nil || *prefixExtID != itPrefixExtID() {
		t.Errorf("failed delivery sent_external_id = %v, want %q filled by the body-prefix matcher (blocks a duplicate re-send)", prefixExtID, itPrefixExtID())
	}
	if prefixConfirmed == nil {
		t.Errorf("failed delivery confirmed_at is NULL; the prefix match must confirm it")
	}
	if got := scanIntJ(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_confirmed'`, taskID); got < 2 {
		t.Errorf("delivery_confirmed events = %d, want >= 2 (exact-id + prefix closures)", got)
	}

	// ---- Second run: idempotent -------------------------------------------
	rstats, err := jira.Run(ctx, jira.NewSink(pool), factory, cfg)
	if err != nil {
		t.Fatalf("Run (rerun): %v", err)
	}
	if rstats.RawInserted != 0 {
		t.Errorf("rerun RawInserted = %d, want 0 (content_hash short-circuit)", rstats.RawInserted)
	}
	if _, err := jira.Normalize(ctx, jira.NewSink(pool), jira.Config{}); err != nil {
		t.Fatalf("Normalize (rerun): %v", err)
	}
	if got := scanIntJ(t, ctx, pool, `SELECT count(*) FROM raw_source_items WHERE source_account_id=$1`, acctID); got != 4 {
		t.Errorf("raw rows after rerun = %d, want 4 (no new rows)", got)
	}
	if got := scanIntJ(t, ctx, pool,
		`SELECT count(*) FROM normalized_messages WHERE channel='jira' AND thread_id=(SELECT id FROM normalized_threads WHERE thread_key=$1)`, itThreadKey()); got != 4 {
		t.Errorf("jira messages after rerun = %d, want 4 (no duplicates)", got)
	}
}
