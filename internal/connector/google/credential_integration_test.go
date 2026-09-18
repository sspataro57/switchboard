//go:build integration

package google_test

// Postgres integration tests for the one credential seam (SPEC
// docs/tickets/microsoft-oauth-mail_SPEC.md, acceptance criteria 7, 9, 13 and
// 15; decisions D7 and D8). Build-tagged `integration` AND env-gated on
// DATABASE_URL, with the dashGuard-style refusal of the production db.
//
// RUN IT IN AN ISOLATED SCRATCH DATABASE (IK 2026-09-12: the compose `ops` db is
// shared by every worktree):
//
//	psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c 'CREATE DATABASE ops_msoauth'
//	make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_msoauth?sslmode=disable'
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_msoauth?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 ./internal/connector/google/
//
// The Microsoft token endpoint is the in-process httptest fake from
// msoauth_test.go (no build tag, so it compiles into this binary too). NO live
// Microsoft, no live IMAP: OpenIMAPSource resolves a credential and returns a
// source; the socket is opened lazily by Folders, which nothing here calls.
//
// EXPECTED FAILURE MODE: **compile error** — google.ListIMAPAccounts,
// google.OpenIMAPSource, google.SaveRefreshToken and google.AuthTypeXOAuth2 do
// not exist. After they do, the assertions are the contract.
//
// IMPOSED SURFACE (D7, D8; the SPEC spells OpenIMAPSource and SaveRefreshToken):
//
//	func ListIMAPAccounts(ctx context.Context, pool *pgxpool.Pool, onlyEmail string) ([]Account, error)
//	func OpenIMAPSource(ctx context.Context, pool *pgxpool.Pool, acct Account, tokenKey string) (*IMAPClientSource, error)
//	func SaveRefreshToken(ctx context.Context, pool *pgxpool.Pool, accountID int64, refreshToken, key string) error
//
// CROSS-SUITE DISCIPLINE (the SWT-6 pact): accounts are provider='google' (the
// PRODUCTION value — D3's whole point, and the only way ownEmailSet and
// PendingRaw are exercised for real) with test-scoped emails 'itest-msoauth-%'.
// Cleanup runs before AND after, in FK order, and is rerunnable. This corpus
// joins the pact in internal/triage/integration_test.go's cleanupTriage.
//
// ONE HAZARD, STATED (criterion 9 requires it): the rotation test sets
// calendar_in_availability=TRUE on a provider='google' row as its sentinel. A
// google row with that flag and no calendar sync makes propose_slots refuse for
// EVERY account (SWT-24 readiness). That is why the row exists only inside one
// test, is deleted before and after, and why the suite is run with -p 1.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	msxPrefix = "itest-msoauth-"
	msxKey    = "itest-msoauth-token-key"
	// The scopes sentinel for criterion 9: a value UpsertGoogleAccount would
	// overwrite and a single-column write cannot.
	msxScopeSentinel = "itest-msoauth-scope-sentinel"
)

func msxPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	// The suite DELETES rows. It may never touch production, and it may not
	// share the compose `ops` db with another worktree's capture suite either —
	// the refusal below catches only the first of those; the runbook line at the
	// top of this file is the second.
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db (cleanup deletes corpus rows); " +
			"use an isolated scratch database on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	cleanupMSX(t, ctx, pool)
	t.Cleanup(func() { cleanupMSX(t, context.Background(), pool) })
	return pool
}

func cleanupMSX(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	owned := `(SELECT id FROM source_accounts WHERE provider='google' AND account_email LIKE '` + msxPrefix + `%')`
	stmts := []string{
		`DELETE FROM capture_decisions WHERE message_id IN (SELECT id FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + owned + `))`,
		`DELETE FROM ai_extractions     WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + owned + `)`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + owned + `)`,
		`DELETE FROM normalized_events  WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + owned + `)`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'gmail:` + msxPrefix + `%'`,
		`DELETE FROM raw_source_items   WHERE source_account_id IN ` + owned,
		`DELETE FROM sync_runs          WHERE source_account_id IN ` + owned,
		`DELETE FROM deliveries         WHERE from_account_id IN ` + owned,
		`DELETE FROM source_accounts    WHERE provider='google' AND account_email LIKE '` + msxPrefix + `%'`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
}

// msxSeed inserts one account with the given credential shape and returns its id
// and email. The INSERT names the columns explicitly rather than going through a
// helper, so the fixture cannot drift with the helper it is testing.
func msxSeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, suffix, authType string) (int64, string) {
	t.Helper()
	email := msxPrefix + suffix + "@example.com"

	var (
		id      int64
		refresh any = nil
		appPass any = nil
	)
	switch authType {
	case google.AuthTypeXOAuth2, google.AuthTypeOAuth:
		refresh = "refresh-token-for-" + suffix
	case google.AuthTypeAppPassword:
		appPass = "app-password-for-" + suffix
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts
		   (provider, account_email, auth_type,
		    refresh_token_encrypted, app_password_encrypted,
		    scopes, send_enabled, calendar_in_availability,
		    imap_host, imap_port, smtp_host, smtp_port)
		 VALUES ('google', $1, $2,
		         CASE WHEN $3::text IS NULL THEN NULL ELSE pgp_sym_encrypt($3::text, $6) END,
		         CASE WHEN $4::text IS NULL THEN NULL ELSE pgp_sym_encrypt($4::text, $6) END,
		         $5, false, false,
		         'outlook.office365.com', 993, 'smtp-mail.outlook.com', 587)
		 RETURNING id`,
		email, authType, refresh, appPass, []string{msxScopeSentinel}, msxKey).Scan(&id); err != nil {
		t.Fatalf("seed %s account %s: %v", authType, email, err)
	}
	return id, email
}

func msxAccount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, email string) google.Account {
	t.Helper()
	accts, err := google.ListIMAPAccounts(ctx, pool, email)
	if err != nil {
		t.Fatalf("ListIMAPAccounts(%s): %v", email, err)
	}
	if len(accts) != 1 {
		t.Fatalf("ListIMAPAccounts(%s) returned %d rows, want 1", email, len(accts))
	}
	return accts[0]
}

// ---- criterion 7: both credential kinds, read from the COLUMN -----------------

// MUTATION (SPEC verification protocol step 4, first bullet): drop auth_type
// from ListIMAPAccounts' SELECT and replace it with a literal. THIS test must go
// red — on the per-row column comparison below if the PROJECTION is faked, and
// on the membership assertions if the PREDICATE is. Two rows of DIFFERENT
// auth_type are seeded precisely so that no single literal satisfies both.
func TestListIMAPAccounts_Integration_ReturnsBothKindsAndNothingElse(t *testing.T) {
	ctx := context.Background()
	pool := msxPool(t, ctx)

	appID, appEmail := msxSeed(t, ctx, pool, "listapp", google.AuthTypeAppPassword)
	msID, msEmail := msxSeed(t, ctx, pool, "listms", google.AuthTypeXOAuth2)
	_, oauthEmail := msxSeed(t, ctx, pool, "listoauth", google.AuthTypeOAuth)

	all, err := google.ListIMAPAccounts(ctx, pool, "")
	if err != nil {
		t.Fatalf("ListIMAPAccounts: %v", err)
	}
	ours := map[string]google.Account{}
	for _, a := range all {
		if strings.HasPrefix(a.Email, msxPrefix) {
			ours[a.Email] = a
		}
	}
	// POSITIVE CONTROL: without this, a query returning zero rows satisfies every
	// exclusion assertion below.
	if len(ours) == 0 {
		t.Fatalf("ListIMAPAccounts returned none of this suite's accounts; every assertion below would "+
			"pass vacuously (seeded: %s, %s, %s)", appEmail, msEmail, oauthEmail)
	}
	if _, ok := ours[appEmail]; !ok {
		t.Errorf("the app_password account %s is missing — the three live Gmail mailboxes ingest through "+
			"exactly this predicate (criterion 12)", appEmail)
	}
	if _, ok := ours[msEmail]; !ok {
		t.Errorf("the xoauth2 account %s is missing. Criterion 7: the predicate is "+
			"auth_type = ANY('{app_password,xoauth2}') — one spelling, both kinds", msEmail)
	}
	if _, ok := ours[oauthEmail]; ok {
		t.Errorf("the oauth account %s was returned; an OAuth row has no IMAP credential to resolve, and "+
			"handing it to the IMAP source produces a login failure that reads as a credential problem "+
			"rather than a configuration one", oauthEmail)
	}
	if len(ours) != 2 {
		t.Errorf("ListIMAPAccounts returned %d of this suite's accounts, want exactly 2: %v", len(ours), ours)
	}

	// TEST THE COLUMN, NOT THE FIXTURE (IK). Each returned AuthType must equal
	// what Postgres holds for that id. Two rows with DIFFERENT auth_types means
	// no literal in the SELECT can satisfy both: replacing the projection with
	// 'app_password' turns the xoauth2 row red, and vice versa.
	for email, a := range ours {
		var fromDB string
		if err := pool.QueryRow(ctx, `SELECT auth_type FROM source_accounts WHERE id=$1`, a.ID).Scan(&fromDB); err != nil {
			t.Fatalf("read auth_type for %s: %v", email, err)
		}
		if a.AuthType != fromDB {
			t.Errorf("%s: ListIMAPAccounts reported auth_type=%q while the column holds %q. The value must "+
				"come from the SELECT, not from a literal — SWT-21's inert guard passed for weeks because "+
				"only the fixture ever supplied it", email, a.AuthType, fromDB)
		}
	}
	if ours[appEmail].AuthType == ours[msEmail].AuthType {
		t.Errorf("both accounts report auth_type=%q; the discriminating values are no longer distinct and "+
			"a single literal would satisfy this test", ours[appEmail].AuthType)
	}

	// The per-account narrowing every caller uses.
	narrowed, err := google.ListIMAPAccounts(ctx, pool, msEmail)
	if err != nil {
		t.Fatalf("ListIMAPAccounts(%s): %v", msEmail, err)
	}
	if len(narrowed) != 1 || narrowed[0].ID != msID {
		t.Errorf("narrowed lookup returned %v, want exactly account %d", narrowed, msID)
	}
	// The endpoints travel with the row (D3): no Outlook host is hard-coded.
	if h := narrowed[0].Hosts(); h.IMAPHost != "outlook.office365.com" || h.IMAPPort != 993 {
		t.Errorf("the xoauth2 account's IMAP endpoint = %s:%d, want outlook.office365.com:993 from the "+
			"imap_host/imap_port columns", h.IMAPHost, h.IMAPPort)
	}
	_ = appID
}

// ---- criterion 9: rotation is a SINGLE-COLUMN write ---------------------------

// MUTATION (verification protocol step 4, second bullet): remove the
// SaveRefreshToken call. THIS test must go red on "stored refresh token = … want
// the rotated …". A second mutation worth running: replace SaveRefreshToken with
// UpsertGoogleAccount — the scopes and calendar_in_availability assertions below
// are what catch that one, and they are the reason D8 insists on a single-column
// write (the SaveCursorField vs SaveCursor lesson).
func TestOpenIMAPSource_Integration_RotationWritesOnlyTheTokenColumn(t *testing.T) {
	ctx := context.Background()
	pool := msxPool(t, ctx)

	id, email := msxSeed(t, ctx, pool, "rotate", google.AuthTypeXOAuth2)
	// The second sentinel, set explicitly: UpsertGoogleAccount / an upsert-shaped
	// rotation sink would write this column too and silently reset it.
	if _, err := pool.Exec(ctx, `UPDATE source_accounts SET calendar_in_availability=true WHERE id=$1`, id); err != nil {
		t.Fatalf("set the calendar sentinel: %v", err)
	}

	const rotated = "0.AXoA-ROTATED-BY-MICROSOFT"
	auth := newMSXAuthority(t, 1, msxTokenReply{http.StatusOK, map[string]any{
		"token_type":    "Bearer",
		"expires_in":    3600,
		"access_token":  "ACCESS-TOKEN-ROTATE",
		"refresh_token": rotated,
	}})
	t.Setenv("MS_OAUTH_CLIENT_ID", msxClientID)
	t.Setenv("MS_OAUTH_AUTHORITY", auth.srv.URL)

	tasksBefore := scanInt(t, ctx, pool, `SELECT count(*) FROM tasks`)
	deliveriesBefore := scanInt(t, ctx, pool, `SELECT count(*) FROM deliveries`)

	acct := msxAccount(t, ctx, pool, email)
	src, err := google.OpenIMAPSource(ctx, pool, acct, msxKey)
	if err != nil {
		t.Fatalf("OpenIMAPSource: %v", err)
	}
	if src == nil {
		t.Fatalf("OpenIMAPSource returned a nil source and no error")
	}
	defer func() { _ = src.Close() }()

	// D8: the NEW refresh token is persisted, or the mailbox dies when the old
	// one ages out — silently, weeks later.
	stored, err := google.DecryptRefreshToken(ctx, pool, id, msxKey)
	if err != nil {
		t.Fatalf("DecryptRefreshToken: %v", err)
	}
	if stored != rotated {
		t.Errorf("stored refresh token = %q, want the rotated %q. Microsoft rotates on redemption for "+
			"personal accounts; dropping the new one is a mailbox that stops ingesting at some "+
			"unpredictable future date, with no error until it does", stored, rotated)
	}

	// …and NOTHING else moved. This is the SaveCursorField-vs-SaveCursor lesson
	// (SWT-24): a whole-row upsert resurrects stale values.
	var scopes []string
	var calendarInAvailability, sendEnabled bool
	var imapHost string
	if err := pool.QueryRow(ctx,
		`SELECT scopes, calendar_in_availability, send_enabled, COALESCE(imap_host,'')
		   FROM source_accounts WHERE id=$1`, id).
		Scan(&scopes, &calendarInAvailability, &sendEnabled, &imapHost); err != nil {
		t.Fatalf("read the account back: %v", err)
	}
	if len(scopes) != 1 || scopes[0] != msxScopeSentinel {
		t.Errorf("scopes = %v, want [%s] — the rotation wrote a column it had no business writing (D8)",
			scopes, msxScopeSentinel)
	}
	if !calendarInAvailability {
		t.Errorf("calendar_in_availability was reset to false by the rotation; a whole-blob write is how a " +
			"deliberate operator setting silently disappears")
	}
	if sendEnabled {
		t.Errorf("send_enabled became true; nothing in this ticket may enable sending (invariant 4)")
	}
	if imapHost != "outlook.office365.com" {
		t.Errorf("imap_host = %q, want the column's value untouched", imapHost)
	}

	// The access token is NEVER persisted (SPEC 07 criterion 3's rule, unchanged).
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM source_accounts
		  WHERE id=$1 AND (array_to_string(scopes,',') LIKE '%'||$2||'%'
		                   OR COALESCE(sync_cursor::text,'') LIKE '%'||$2||'%'
		                   OR encode(COALESCE(refresh_token_encrypted,''::bytea),'escape') LIKE '%'||$2||'%')`,
		id, "ACCESS-TOKEN-ROTATE"); got != 0 {
		t.Errorf("the ACCESS token was persisted somewhere on the row; only the refresh token is stored, " +
			"encrypted")
	}

	// Criterion 15.
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM tasks`); got != tasksBefore {
		t.Errorf("tasks changed: before=%d after=%d (resolving a credential creates none)", tasksBefore, got)
	}
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM deliveries`); got != deliveriesBefore {
		t.Errorf("deliveries changed: before=%d after=%d", deliveriesBefore, got)
	}
}

// A provider that does NOT rotate must not blank the stored token: "" means
// "keep what you have", never "overwrite with nothing".
func TestOpenIMAPSource_Integration_NoRotationLeavesTheTokenIntact(t *testing.T) {
	ctx := context.Background()
	pool := msxPool(t, ctx)

	id, email := msxSeed(t, ctx, pool, "norotate", google.AuthTypeXOAuth2)
	before, err := google.DecryptRefreshToken(ctx, pool, id, msxKey)
	if err != nil {
		t.Fatalf("DecryptRefreshToken (before): %v", err)
	}

	auth := newMSXAuthority(t, 1, msxTokenReply{http.StatusOK, map[string]any{
		"token_type":   "Bearer",
		"expires_in":   3600,
		"access_token": "ACCESS-TOKEN-NOROTATE",
		// no refresh_token in the reply
	}})
	t.Setenv("MS_OAUTH_CLIENT_ID", msxClientID)
	t.Setenv("MS_OAUTH_AUTHORITY", auth.srv.URL)

	src, err := google.OpenIMAPSource(ctx, pool, msxAccount(t, ctx, pool, email), msxKey)
	if err != nil {
		t.Fatalf("OpenIMAPSource: %v", err)
	}
	defer func() { _ = src.Close() }()

	after, err := google.DecryptRefreshToken(ctx, pool, id, msxKey)
	if err != nil {
		t.Fatalf("DecryptRefreshToken (after): %v", err)
	}
	if after != before {
		t.Errorf("the stored refresh token changed from %q to %q although the provider returned none; "+
			"an empty rotation must be a no-op, not a write (the 0037 CHECK would also refuse NULL)", before, after)
	}
}

// ---- D9's cause classes at the seam, and criterion 6 --------------------------

func TestOpenIMAPSource_Integration_CredentialFailuresNameTheCauseAndLeakNothing(t *testing.T) {
	ctx := context.Background()
	pool := msxPool(t, ctx)

	_, msEmail := msxSeed(t, ctx, pool, "fail-ms", google.AuthTypeXOAuth2)
	_, appEmail := msxSeed(t, ctx, pool, "fail-app", google.AuthTypeAppPassword)
	oauthID, oauthEmail := msxSeed(t, ctx, pool, "fail-oauth", google.AuthTypeOAuth)
	// The production shape: all three live google rows have a NULL
	// refresh_token_encrypted and empty scopes (SPEC, "What is already true").
	if _, err := pool.Exec(ctx,
		`UPDATE source_accounts SET refresh_token_encrypted=NULL, scopes='{}' WHERE id=$1`, oauthID); err != nil {
		t.Fatalf("blank the oauth row's credential: %v", err)
	}

	msAcct := msxAccount(t, ctx, pool, msEmail)
	appAcct := msxAccount(t, ctx, pool, appEmail)
	oauthAcct := google.Account{ID: oauthID, Email: oauthEmail, AuthType: google.AuthTypeOAuth}

	t.Run("MS_OAUTH_CLIENT_ID unset", func(t *testing.T) {
		t.Setenv("MS_OAUTH_CLIENT_ID", "")
		t.Setenv("MS_OAUTH_AUTHORITY", "")
		_, err := google.OpenIMAPSource(ctx, pool, msAcct, msxKey)
		if err == nil {
			t.Fatalf("OpenIMAPSource succeeded with no client id configured")
		}
		if !strings.Contains(err.Error(), "MS_OAUTH_CLIENT_ID") {
			t.Errorf("error %q does not name MS_OAUTH_CLIENT_ID (D10: absent means a loud per-account "+
				"error, never a skip)", err)
		}
	})

	t.Run("revoked refresh token", func(t *testing.T) {
		auth := newMSXAuthority(t, 1, msxTokenReply{http.StatusBadRequest, map[string]any{
			"error":             "invalid_grant",
			"error_description": "AADSTS700082: The refresh token has expired due to inactivity.",
		}})
		t.Setenv("MS_OAUTH_CLIENT_ID", msxClientID)
		t.Setenv("MS_OAUTH_AUTHORITY", auth.srv.URL)

		_, err := google.OpenIMAPSource(ctx, pool, msAcct, msxKey)
		if err == nil {
			t.Fatalf("OpenIMAPSource succeeded although the refresh token was rejected")
		}
		if !strings.Contains(err.Error(), "invalid_grant") {
			t.Errorf("error %q does not carry the cause class invalid_grant — the expected long-run "+
				"failure of this ticket, whose remedy is re-running google-auth add-microsoft (D9)", err)
		}
		if !strings.Contains(err.Error(), msEmail) {
			t.Errorf("error %q does not name the account", err)
		}
		// Criterion 6: this string becomes a sync_runs message.
		for _, secret := range []string{msxKey, "refresh-token-for-fail-ms"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("the error leaks %q: %v", secret, err)
			}
		}
	})

	t.Run("wrong OPS_TOKEN_KEY", func(t *testing.T) {
		t.Setenv("MS_OAUTH_CLIENT_ID", msxClientID)
		t.Setenv("MS_OAUTH_AUTHORITY", "")
		for _, acct := range []google.Account{msAcct, appAcct} {
			_, err := google.OpenIMAPSource(ctx, pool, acct, "itest-msoauth-WRONG-key")
			if err == nil {
				t.Fatalf("OpenIMAPSource(%s) succeeded with the wrong pgcrypto key", acct.Email)
			}
			if !strings.Contains(err.Error(), acct.Email) {
				t.Errorf("error %q does not name the account %s", err, acct.Email)
			}
			if strings.Contains(err.Error(), "itest-msoauth-WRONG-key") || strings.Contains(err.Error(), msxKey) {
				t.Errorf("the pgcrypto key is rendered in the error (criterion 6): %v", err)
			}
		}
	})

	t.Run("no credential stored", func(t *testing.T) {
		// The legacy oauth shape: a row with no IMAP credential at all. It is not
		// in ListIMAPAccounts' set, so reaching here means a caller passed it —
		// which must fail by NAME, not with a confusing protocol error later.
		t.Setenv("MS_OAUTH_CLIENT_ID", msxClientID)
		t.Setenv("MS_OAUTH_AUTHORITY", "")
		_, err := google.OpenIMAPSource(ctx, pool, oauthAcct, msxKey)
		if err == nil {
			t.Fatalf("OpenIMAPSource succeeded for a row with no stored credential")
		}
		if !strings.Contains(err.Error(), oauthEmail) {
			t.Errorf("error %q does not name the account", err)
		}
	})
}

// ---- criterion 13 (the integration half): the send router refuses the row -----

// MUTATION (verification protocol step 4, third bullet): make MailSender.Send's
// new xoauth2 case fall through to `default`. THIS test must go red on
// "the xoauth2 account was routed to the OAuth sender (1 call(s))" — which is
// also why the fake RETURNS SUCCESS rather than an error: a fake that failed
// would leave the test passing on the returned error alone and hide the misroute.
//
// recordingSender stands in for the Gmail-API sender. If the xoauth2 case ever
// falls through to `default`, this records the call that would have handed a
// MICROSOFT refresh token to the GMAIL API (D6) — which is exactly the mutation
// the SPEC's verification protocol asks for.
type recordingSender struct{ calls int }

func (r *recordingSender) Send(ctx context.Context, fromUserID string, rawMIME []byte, threadID string) (string, error) {
	r.calls++
	return "<should-never-happen@example.com>", nil
}

func TestMailSender_Integration_RefusesAnXOAuth2AccountByName(t *testing.T) {
	ctx := context.Background()
	pool := msxPool(t, ctx)

	_, email := msxSeed(t, ctx, pool, "send", google.AuthTypeXOAuth2)

	oauth := &recordingSender{}
	sender := &google.MailSender{Pool: pool, TokenKey: msxKey, OAuth: oauth}

	id, err := sender.Send(ctx, email, []byte("Message-ID: <x@example.com>\r\nFrom: "+email+"\r\n\r\nbody"), "")
	if err == nil {
		t.Fatalf("MailSender.Send accepted an xoauth2 account and returned %q", id)
	}
	if oauth.calls != 0 {
		t.Errorf("the xoauth2 account was routed to the OAuth sender (%d call(s)). D6: an xoauth2 row "+
			"landing in `default` hands a Microsoft refresh token to the Gmail-API sender — the exact "+
			"\"new value lands in default\" trap", oauth.calls)
	}
	var rejected *google.SendRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("error %v (%T) is not *google.SendRejectedError; nothing reached the network, so the "+
			"reserved Message-ID must be released", err, err)
	}
	if !strings.Contains(err.Error(), email) {
		t.Errorf("refusal %q does not name the account", err)
	}
	lower := strings.ToLower(err.Error())
	if !strings.Contains(lower, "smtp") || !strings.Contains(lower, "xoauth2") {
		t.Errorf("refusal %q does not name the missing transport (D6: \"no SMTP XOAUTH2 transport is wired\")", err)
	}
}
