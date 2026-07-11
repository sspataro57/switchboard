//go:build integration

package google_test

// Integration test for the Google OAuth token plumbing (SPEC
// 07-google-oauth-pollers, acceptance criteria 2 & 3; verification protocol
// step 2 "pgcrypto round-trip"). Build-tagged `integration` AND env-gated on
// DATABASE_URL: excluded from the default zero-network `go test ./...`, skips
// cleanly when the DB env is unset. Run with:
//
//   DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//     go test -tags integration ./internal/connector/google/
//
// It exercises the REAL pgcrypto round-trip on the compose db: SQL-side
// pgp_sym_encrypt on write, pgp_sym_decrypt on read (the pinned encryption
// idiom; the key never touches the db and never appears in the row). A wrong
// key must fail decryption, proving the column is genuinely encrypted.
//
// GREENFIELD NOTE: under `-tags integration` this compile-FAILs until the
// google connector's oauth/sink code exists — the expected failure mode.
// Imposed exported surface (the SPEC's oauth.go / sink.go; google-auth is the
// ONLY writer of source_accounts rows):
//
//   const Provider = "google"
//   var ReadonlyScopes = []string{
//       "https://www.googleapis.com/auth/gmail.readonly",
//       "https://www.googleapis.com/auth/calendar.readonly",
//   }
//
//   // UpsertGoogleAccount writes exactly one provider='google' source_accounts
//   // row: account_email, refresh_token_encrypted = pgp_sym_encrypt(token,key),
//   // scopes, send_enabled=false, calendar_in_availability per the flag.
//   func UpsertGoogleAccount(ctx context.Context, pool *pgxpool.Pool,
//       email, refreshToken, tokenKey string, scopes []string, calendarInAvailability bool) (accountID int64, err error)
//
//   // DecryptRefreshToken returns pgp_sym_decrypt(refresh_token_encrypted, key).
//   // A wrong key returns a non-nil error (pgcrypto raises on bad key).
//   func DecryptRefreshToken(ctx context.Context, pool *pgxpool.Pool, accountID int64, tokenKey string) (token string, err error)

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	oauthEmail = "itest-google-oauth-a@example.com"
	oauthToken = "1//0g-super-secret-refresh-token-xyz"
	oauthKey   = "itest-token-key-correct"
	oauthWrong = "itest-token-key-WRONG"
)

func requireCompose(t *testing.T) {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	// Real-db refusal guard (copy of the SWT-6 idiom): cleanup deletes corpus
	// rows, so the suite must NEVER touch the production ops db.
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db (cleanup deletes corpus rows); use the compose db on :5433")
	}
}

func cleanupOAuth(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	// FK order: no children reference these bespoke accounts (no raw ingested).
	if _, err := pool.Exec(ctx,
		`DELETE FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-google-oauth-%'`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

func TestGoogleOAuth_TokenRoundTrip(t *testing.T) {
	requireCompose(t)
	ctx := context.Background()

	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	defer pool.Close()

	cleanupOAuth(t, ctx, pool)
	defer cleanupOAuth(t, ctx, pool)

	id, err := google.UpsertGoogleAccount(ctx, pool, oauthEmail, oauthToken, oauthKey, google.ReadonlyScopes, true)
	if err != nil {
		t.Fatalf("UpsertGoogleAccount: %v", err)
	}

	// Criterion 3: read the token back decrypted with the RIGHT key.
	got, err := google.DecryptRefreshToken(ctx, pool, id, oauthKey)
	if err != nil {
		t.Fatalf("DecryptRefreshToken (right key): %v", err)
	}
	if got != oauthToken {
		t.Errorf("decrypted token = %q, want %q", got, oauthToken)
	}

	// A WRONG key must fail — proves the column is genuinely encrypted.
	if _, err := google.DecryptRefreshToken(ctx, pool, id, oauthWrong); err == nil {
		t.Errorf("DecryptRefreshToken with the wrong key returned no error; the column is not encrypted or the key is ignored")
	}

	// The plaintext token must never appear in the stored bytea.
	var plaintextLeak int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM source_accounts
		  WHERE id=$1 AND encode(refresh_token_encrypted,'escape') LIKE '%'||$2||'%'`,
		id, oauthToken).Scan(&plaintextLeak); err != nil {
		t.Fatalf("scan plaintext-leak check: %v", err)
	}
	if plaintextLeak != 0 {
		t.Errorf("refresh_token_encrypted contains the plaintext token; encryption is not applied")
	}

	// Criterion 2: the row shape — provider google, exactly the two readonly
	// scopes, send_enabled=false, calendar_in_availability=true (default here).
	var provider string
	var scopes []string
	var sendEnabled, calAvail bool
	if err := pool.QueryRow(ctx,
		`SELECT provider, scopes, send_enabled, calendar_in_availability
		   FROM source_accounts WHERE id=$1`, id).
		Scan(&provider, &scopes, &sendEnabled, &calAvail); err != nil {
		t.Fatalf("scan account row: %v", err)
	}
	if provider != "google" {
		t.Errorf("provider = %q, want google", provider)
	}
	if sendEnabled {
		t.Errorf("send_enabled = true, want false (readonly scopes only, invariant 4)")
	}
	if !calAvail {
		t.Errorf("calendar_in_availability = false, want true (default)")
	}
	if !equalStringSet(scopes, google.ReadonlyScopes) {
		t.Errorf("scopes = %v, want exactly the two readonly scopes %v", scopes, google.ReadonlyScopes)
	}
	for _, s := range scopes {
		if strings.Contains(s, "gmail.send") || strings.Contains(s, "calendar.events") {
			t.Errorf("scope %q is a WRITE scope; this step is readonly only", s)
		}
	}
}

// The --no-availability flag stores calendar_in_availability=false.
func TestGoogleOAuth_NoAvailabilityFlag(t *testing.T) {
	requireCompose(t)
	ctx := context.Background()

	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	defer pool.Close()

	cleanupOAuth(t, ctx, pool)
	defer cleanupOAuth(t, ctx, pool)

	id, err := google.UpsertGoogleAccount(ctx, pool,
		"itest-google-oauth-noavail@example.com", oauthToken, oauthKey, google.ReadonlyScopes, false)
	if err != nil {
		t.Fatalf("UpsertGoogleAccount: %v", err)
	}
	var calAvail bool
	if err := pool.QueryRow(ctx, `SELECT calendar_in_availability FROM source_accounts WHERE id=$1`, id).Scan(&calAvail); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if calAvail {
		t.Errorf("calendar_in_availability = true, want false (--no-availability)")
	}
}

func equalStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
