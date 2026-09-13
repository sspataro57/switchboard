//go:build integration

package jira_test

// SWT-43 (docs/tickets/delivery-deny_SPEC.md) criterion 15, jira half:
// matchByBodyPrefix never stamps a REJECTED jira_comment row.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run JiraRejected ./internal/connector/jira/
//
// Scope is the matcher's own: same target_ref (issue), an OWN comment whose
// body has the SAME normalized 120-char prefix as the row's body but differs
// in raw whitespace (CRLF, trailing spaces). The control seeds the identical
// fixture at 'failed' — the matcher's candidate set, and the D4 reason a
// failed jira row is NOT rejectable — and must be stamped.
//
// Mutation: add 'rejected' to jira/sink.go's status list -> the stamp hits
// deliveries_rejected_unsent_check and normalize errors, or (with the CHECK
// dropped) the rejected row is stamped -> red either way.
//
// GREENFIELD NOTE — EXPECTED RED: before 0028 the rejected fixture cannot be
// seeded.
//
// Cross-suite discipline: owns site itest-jdeny.atlassian.net, account
// itest-jdeny@example.com and project itest-jdeny-proj; cleaned in FK order at
// start and end of each subtest (sync_runs included). Fake Jira only.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/jira"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/textmatch"
)

const (
	jdnSite      = "itest-jdeny.atlassian.net"
	jdnBaseURL   = "https://itest-jdeny.atlassian.net"
	jdnAcct      = "itest-jdeny@example.com"
	jdnSlug      = "itest-jdeny-proj"
	jdnKey       = "JDN-1"
	jdnOwn       = "acc-itest-jdeny-own"
	jdnClientAcc = "acc-itest-jdeny-client"
	jdnComm      = "880801"
	jdnStored    = "shipped the jdeny fix to staging tonight\n\nwill confirm once CI is green"
	jdnReturned  = "shipped the jdeny fix to staging tonight\r\n\r\nwill confirm once CI is green  "
)

func jdnTarget() string { return "jira:" + jdnSite + ":" + jdnKey }

func cleanupJiraDeny(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM task_events WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{jdnSlug}},
		{`DELETE FROM deliveries WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{jdnSlug}},
		{`DELETE FROM normalized_messages WHERE thread_id IN (SELECT id FROM normalized_threads WHERE thread_key LIKE 'jira:'||$1||':%')`, []any{jdnSite}},
		{`DELETE FROM normalized_threads WHERE thread_key LIKE 'jira:'||$1||':%'`, []any{jdnSite}},
		{`DELETE FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='jira' AND account_email=$1)`, []any{jdnAcct}},
		{`DELETE FROM sync_runs WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='jira' AND account_email=$1)`, []any{jdnAcct}},
		{`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, []any{jdnSlug}},
		{`DELETE FROM projects WHERE slug=$1`, []any{jdnSlug}},
		{`DELETE FROM source_accounts WHERE provider='jira' AND account_email=$1`, []any{jdnAcct}},
	}
	for _, st := range stmts {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

func TestJiraRejected_Integration_MatcherNeverStampsARejectedRow(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if jdnStored == jdnReturned ||
		textmatch.NormalizedPrefix(jdnStored, 120) != textmatch.NormalizedPrefix(jdnReturned, 120) {
		t.Fatalf("fixture invalid: the bodies must differ raw and agree normalized")
	}

	for _, tc := range []struct {
		name, status string
		wantStamped  bool
	}{
		{"control: a failed row IS stamped by the prefix matcher", "failed", true},
		{"a rejected row is never stamped", "rejected", false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool, err := store.NewPool(ctx)
			if err != nil {
				t.Fatalf("store.NewPool: %v", err)
			}
			defer pool.Close()
			cleanupJiraDeny(t, ctx, pool)
			defer cleanupJiraDeny(t, ctx, pool)

			var acctID, projID, taskID, deliveryID int64
			if err := pool.QueryRow(ctx,
				`INSERT INTO source_accounts (provider, account_email, refresh_token_encrypted, scopes, domain_default, send_enabled)
				 VALUES ('jira', $1, pgp_sym_encrypt('dummy','k'), ARRAY['JDN'], $2, true) RETURNING id`,
				jdnAcct, jdnBaseURL).Scan(&acctID); err != nil {
				t.Fatalf("seed jira account: %v", err)
			}
			if err := pool.QueryRow(ctx,
				`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
				 VALUES ($1,$1,'itest-jdeny-client','manual','dashboard','/tmp/itest','any') RETURNING id`, jdnSlug).Scan(&projID); err != nil {
				t.Fatalf("seed project: %v", err)
			}
			if err := pool.QueryRow(ctx,
				`INSERT INTO tasks (project_id, title, assignee_type, status) VALUES ($1,'jdeny work','claude','done_locally') RETURNING id`,
				projID).Scan(&taskID); err != nil {
				t.Fatalf("seed task: %v", err)
			}
			q := `INSERT INTO deliveries (task_id, channel, target_ref, body, status) VALUES ($1,'jira_comment',$2,$3,$4) RETURNING id`
			if tc.status == "rejected" {
				q = `INSERT INTO deliveries (task_id, channel, target_ref, body, status, rejection_note)
				     VALUES ($1,'jira_comment',$2,$3,$4,'denied before it went out') RETURNING id`
			}
			if err := pool.QueryRow(ctx, q, taskID, jdnTarget(), jdnStored, tc.status).Scan(&deliveryID); err != nil {
				t.Fatalf("seed %s jira_comment delivery: %v (a 'rejected' row needs migration 0028)", tc.status, err)
			}

			fj := newFakeJira()
			defer fj.close()
			fj.ownAccountID = jdnOwn
			fj.add(fakeIssue{
				key: jdnKey, updated: "2026-07-10T09:00:00.000+0000", created: "2026-07-10T08:00:00.000+0000",
				summary: "JDN deny", description: "staging 500", reporter: jdnClientAcc, assignee: jdnOwn,
				comments: []fakeComment{
					{id: jdnComm, author: jdnOwn, body: jdnReturned, created: "2026-07-10T08:50:00.000+0000"},
				},
			})
			factory := func(_ context.Context, a jira.Account) (*jira.Client, error) {
				return jira.NewClient(nil, fj.url(), a.Email, "tok"), nil
			}
			cfg := jira.Config{Overlap: time.Hour, Now: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)}
			if _, err := jira.Run(ctx, jira.NewSink(pool), factory, cfg); err != nil {
				t.Fatalf("Run (ingest): %v", err)
			}
			if _, err := jira.Normalize(ctx, jira.NewSink(pool), jira.Config{}); err != nil {
				t.Fatalf("Normalize: %v", err)
			}

			var status string
			var extID, confirmedAt *string
			if err := pool.QueryRow(ctx,
				`SELECT status, sent_external_id, confirmed_at::text FROM deliveries WHERE id=$1`, deliveryID).
				Scan(&status, &extID, &confirmedAt); err != nil {
				t.Fatalf("read delivery: %v", err)
			}
			events := scanIntJ(t, ctx, pool,
				`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_confirmed'`, taskID)

			if tc.wantStamped {
				want := "jira:" + jdnSite + ":comment:" + jdnComm
				if extID == nil || *extID != want || confirmedAt == nil {
					t.Fatalf("CONTROL FAILED: the prefix matcher did not stamp the 'failed' row (sent_external_id=%s, "+
						"confirmed_at=%v); the fixture never reaches matchByBodyPrefix", derefJ(extID), confirmedAt)
				}
				return
			}
			if status != "rejected" || extID != nil || confirmedAt != nil {
				t.Errorf("the jira matcher touched a REJECTED row: status=%q sent_external_id=%s confirmed_at=%v "+
					"(criterion 15)", status, derefJ(extID), confirmedAt)
			}
			if events != 0 {
				t.Errorf("delivery_confirmed events = %d for a rejected row, want 0", events)
			}
		})
	}
}
