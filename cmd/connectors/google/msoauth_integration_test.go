//go:build integration

package main

// Postgres integration tests for D9's LOUD credential failures (SPEC
// docs/tickets/microsoft-oauth-mail_SPEC.md, acceptance criteria 10, 6 (the
// sync_runs.stats half) and 15). Build-tagged `integration`, env-gated on
// DATABASE_URL, with the production-db refusal.
//
// Run it in the ISOLATED scratch database:
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_msoauth?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 ./cmd/connectors/google/
//
// The Microsoft token endpoint is an httptest server; NO live Microsoft, no live
// IMAP. Every failure here happens at credential-resolution time, before any
// socket to a mail server is opened — which is precisely why these three causes
// are testable offline at all.
//
// WHY THIS MATTERS (D9): today a credential failure in runIMAPIngest does
// `continue` with NO sync_runs row. If another account succeeds, the pass exits 0
// and the failure exists only in stdout, which nothing queries. For a revoked
// refresh token — the expected long-run failure of this ticket — that is a
// mailbox that silently stops ingesting. The error row is not bookkeeping; it is
// the only thing that distinguishes "revoked consent" from "the CronJob has not
// run yet". Sibling precedent: watchAccount's imap_idle error run (watch.go).
//
// EXPECTED FAILURE MODE: **compile error** (google.AuthTypeXOAuth2,
// google.ListIMAPAccounts are undefined), then ASSERTION failures
// ("error imap sync_runs for … = 0, want 1") until runIMAPIngest writes the row.
//
// IMPOSED SURFACE: none beyond the SPEC's — runIMAPIngest keeps its signature
// (pool, sink, cfg) and resolves credentials through google.OpenIMAPSource.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	msxRunPrefix = "itest-msoauth-run-"
	msxRunKey    = "itest-msoauth-run-key"
	msxRunClient = "11111111-2222-3333-4444-555555555555"
	// Fed through every failure path; criterion 6 greps for it in the rendered
	// error AND in sync_runs.stats.
	msxRunSecret = "0.AXoA-SENTINEL-REFRESH-TOKEN-DO-NOT-LEAK"
)

func msxRunPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use an isolated scratch database on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	cleanupMSXRun(t, ctx, pool)
	t.Cleanup(func() { cleanupMSXRun(t, context.Background(), pool) })
	return pool
}

func cleanupMSXRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	owned := `(SELECT id FROM source_accounts WHERE account_email LIKE '` + msxRunPrefix + `%')`
	for _, s := range []string{
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + owned + `)`,
		`DELETE FROM normalized_events  WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + owned + `)`,
		`DELETE FROM raw_source_items   WHERE source_account_id IN ` + owned,
		`DELETE FROM sync_runs          WHERE source_account_id IN ` + owned,
		`DELETE FROM source_accounts    WHERE account_email LIKE '` + msxRunPrefix + `%'`,
	} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
}

// msxRunSeed inserts one mailbox of the given credential kind. The xoauth2 rows
// carry the SENTINEL refresh token, so every failure path below is fed a real
// secret to leak.
//
// The IMAP endpoint is 127.0.0.1:9 ON PURPOSE. These cases fail at credential
// resolution, before any dial — but seeding outlook.office365.com would mean
// that the day the implementation dials first, this suite starts contacting
// Microsoft from a unit-of-work that claims to be offline. A refused local
// connection is instant and provably local.
func msxRunSeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, suffix, authType, key string) (int64, string) {
	t.Helper()
	email := msxRunPrefix + suffix + "@example.com"
	var id int64
	var refresh, appPass any
	switch authType {
	case google.AuthTypeXOAuth2:
		refresh = msxRunSecret
	case google.AuthTypeAppPassword:
		appPass = "app-password-" + suffix
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts
		   (provider, account_email, auth_type, refresh_token_encrypted, app_password_encrypted,
		    scopes, send_enabled, calendar_in_availability, imap_host, imap_port, smtp_host, smtp_port)
		 VALUES ('google', $1, $2,
		         CASE WHEN $3::text IS NULL THEN NULL ELSE pgp_sym_encrypt($3::text,$5) END,
		         CASE WHEN $4::text IS NULL THEN NULL ELSE pgp_sym_encrypt($4::text,$5) END,
		         '{}', false, false, '127.0.0.1', 9, '127.0.0.1', 9)
		 RETURNING id`,
		email, authType, refresh, appPass, key).Scan(&id); err != nil {
		t.Fatalf("seed %s account %s: %v", authType, email, err)
	}
	return id, email
}

func msxRunRuns(t *testing.T, ctx context.Context, pool *pgxpool.Pool, accountID int64, status string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM sync_runs WHERE source_account_id=$1 AND status=$2 AND stats->>'phase'='imap'`,
		accountID, status).Scan(&n); err != nil {
		t.Fatalf("count %s imap runs: %v", status, err)
	}
	return n
}

// msxRunErrorText returns the error message AND the raw stats payload of the one
// error run, so criterion 6 can be checked against both.
func msxRunErrorText(t *testing.T, ctx context.Context, pool *pgxpool.Pool, accountID int64) (string, string) {
	t.Helper()
	var msg *string
	var stats json.RawMessage
	if err := pool.QueryRow(ctx,
		`SELECT error, stats FROM sync_runs
		  WHERE source_account_id=$1 AND status='error' ORDER BY id DESC LIMIT 1`, accountID).
		Scan(&msg, &stats); err != nil {
		t.Fatalf("read the error run: %v", err)
	}
	if msg == nil {
		return "", string(stats)
	}
	return *msg, string(stats)
}

// msxRunAuthority is a token endpoint that always refuses, the way a revoked
// consent does.
func msxRunAuthority(t *testing.T, code, description string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": code, "error_description": description})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// ---- criterion 10 --------------------------------------------------------------

func TestRunIMAPIngest_Integration_CredentialFailureWritesOneErrorRun(t *testing.T) {
	ctx := context.Background()
	pool := msxRunPool(t, ctx)
	sink := google.NewPGSink(pool)

	tasksBefore := scanIntRun(t, ctx, pool, `SELECT count(*) FROM tasks`)
	deliveriesBefore := scanIntRun(t, ctx, pool, `SELECT count(*) FROM deliveries`)

	cases := []struct {
		name     string
		suffix   string
		authType string
		// seedKey is the pgcrypto key the credential is stored under; OPS_TOKEN_KEY
		// is always msxRunKey, so a different seedKey is a key-rotation failure.
		seedKey   string
		setup     func(t *testing.T)
		wantCause []string
	}{
		{
			name:     "MS_OAUTH_CLIENT_ID is not set",
			suffix:   "noclientid",
			authType: google.AuthTypeXOAuth2,
			seedKey:  msxRunKey,
			setup: func(t *testing.T) {
				t.Setenv("MS_OAUTH_CLIENT_ID", "")
				t.Setenv("MS_OAUTH_AUTHORITY", "")
			},
			wantCause: []string{"MS_OAUTH_CLIENT_ID"},
		},
		{
			name:     "refresh token rejected",
			suffix:   "revoked",
			authType: google.AuthTypeXOAuth2,
			seedKey:  msxRunKey,
			setup: func(t *testing.T) {
				srv := msxRunAuthority(t, "invalid_grant", "AADSTS700082: The refresh token has expired due to inactivity.")
				t.Setenv("MS_OAUTH_CLIENT_ID", msxRunClient)
				t.Setenv("MS_OAUTH_AUTHORITY", srv.URL)
			},
			wantCause: []string{"invalid_grant"},
		},
		{
			name:     "no usable credential (app-password mailbox, rotated key)",
			suffix:   "badkey",
			authType: google.AuthTypeAppPassword,
			// D9 is a deliberate behaviour change for app-password accounts too:
			// from silence to a recorded error.
			seedKey:   "itest-msoauth-run-OTHER-key",
			setup:     func(t *testing.T) { t.Setenv("MS_OAUTH_CLIENT_ID", msxRunClient) },
			wantCause: nil,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OPS_TOKEN_KEY", msxRunKey)
			tc.setup(t)

			id, email := msxRunSeed(t, ctx, pool, tc.suffix, tc.authType, tc.seedKey)

			// Narrowed to this one account: deterministic on a database that may
			// hold other mailboxes.
			cfg := google.Config{AccountEmail: email}
			_, err := runIMAPIngest(ctx, pool, sink, cfg)
			if err == nil {
				t.Errorf("runIMAPIngest returned nil with its ONLY account failing; the one-shot pass must " +
					"exit non-zero when no account succeeded, or a CronJob logs a green run for a mailbox " +
					"that ingested nothing (criterion 10)")
			}

			if got := msxRunRuns(t, ctx, pool, id, "error"); got != 1 {
				t.Errorf("error imap sync_runs for %s = %d, want exactly 1. Zero means the failure exists "+
					"only on stdout (today's `continue`), and more than one means the pass retried a "+
					"credential that cannot work", email, got)
			}
			if got := msxRunRuns(t, ctx, pool, id, "ok"); got != 0 {
				t.Errorf("ok imap sync_runs for %s = %d, want 0", email, got)
			}

			msg, stats := msxRunErrorText(t, ctx, pool, id)
			if !strings.Contains(msg, email) {
				t.Errorf("the sync_runs message %q does not name the account; with several mailboxes in one "+
					"pass, a message that does not say WHICH is unactionable (D9)", msg)
			}
			for _, cause := range tc.wantCause {
				if !strings.Contains(msg, cause) {
					t.Errorf("the sync_runs message %q does not carry the cause %q", msg, cause)
				}
			}
			// Criterion 6: neither the message nor the stats payload may carry a
			// secret. Both are read by anything querying sync_runs.
			for _, field := range []string{msg, stats} {
				for _, secret := range []string{msxRunSecret, msxRunKey, "app-password-" + tc.suffix} {
					if strings.Contains(field, secret) {
						t.Errorf("a secret (%q) reached sync_runs: %q", secret, field)
					}
				}
			}
		})
	}

	// Criterion 15, over the whole set of failing passes.
	if got := scanIntRun(t, ctx, pool, `SELECT count(*) FROM tasks`); got != tasksBefore {
		t.Errorf("tasks changed: before=%d after=%d", tasksBefore, got)
	}
	if got := scanIntRun(t, ctx, pool, `SELECT count(*) FROM deliveries`); got != deliveriesBefore {
		t.Errorf("deliveries changed: before=%d after=%d", deliveriesBefore, got)
	}
}

// One bad mailbox must not blind the others: the SUCCESSFUL accounts of a pass
// still run, and the pass itself only fails when NONE succeeded. This is the
// existing rule (mailsource.go) and D9 must not change it — otherwise the day the
// MSN token is revoked, the three Gmail mailboxes stop ingesting too.
func TestRunIMAPIngest_Integration_OneBadCredentialDoesNotStopTheOthers(t *testing.T) {
	ctx := context.Background()
	pool := msxRunPool(t, ctx)
	sink := google.NewPGSink(pool)

	t.Setenv("OPS_TOKEN_KEY", msxRunKey)
	srv := msxRunAuthority(t, "invalid_grant", "revoked")
	t.Setenv("MS_OAUTH_CLIENT_ID", msxRunClient)
	t.Setenv("MS_OAUTH_AUTHORITY", srv.URL)

	badID, badEmail := msxRunSeed(t, ctx, pool, "mixed-bad", google.AuthTypeXOAuth2, msxRunKey)
	_, goodEmail := msxRunSeed(t, ctx, pool, "mixed-good", google.AuthTypeAppPassword, msxRunKey)

	// Both accounts in one pass (the prefix is this suite's own).
	_, err := runIMAPIngest(ctx, pool, sink, google.Config{})
	// The app-password account resolves its credential and then fails at DIAL
	// time against 127.0.0.1:9 (connection refused, instantly, locally), so the
	// pass legitimately returns an error. What must NOT happen is the bad
	// credential aborting the loop before the second account is attempted.
	_ = err

	if got := msxRunRuns(t, ctx, pool, badID, "error"); got != 1 {
		t.Errorf("error imap sync_runs for the revoked account %s = %d, want 1", badEmail, got)
	}
	var attempted int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM sync_runs WHERE source_account_id IN
		   (SELECT id FROM source_accounts WHERE account_email=$1)`, goodEmail).Scan(&attempted); err != nil {
		t.Fatalf("count runs for %s: %v", goodEmail, err)
	}
	if attempted == 0 {
		t.Errorf("the second account %s was never attempted: one unreachable mailbox stopped the pass. "+
			"With the MSN token revoked that would silently stop the three live Gmail mailboxes too", goodEmail)
	}
}

func scanIntRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}
