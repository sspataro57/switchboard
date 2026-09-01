//go:build integration

package google_test

// Integration tests for the credential-gated calendar phase (SWT-24 /
// docs/tickets/calendar-availability_SPEC.md, acceptance criteria 16, 17 and
// 19). Build-tagged `integration` AND env-gated on DATABASE_URL; the Calendar
// API is httptest (the shared fake's canned items via calFull, served by a local
// handler that can also simulate a concurrent IMAP writer) — NEVER a live Google
// call, and no credential of any kind is needed to run this.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run CalendarPhase ./internal/connector/google/
//
// WHY INTEGRATION AND NOT UNIT. Criterion 17's account selection is a predicate
// over COLUMNS — refresh_token_encrypted and scopes — and IK landmine 6 is
// explicit that such a predicate's regression test belongs here and must fail
// when the column leaves the SELECT. Criterion 16 is entirely about what a write
// does NOT touch (auth_type, app_password_encrypted), which only Postgres can
// answer. Criterion 19 is about a read-modify-write race on one jsonb column,
// which is only observable with a real concurrent writer against a real row.
//
// Cross-suite discipline (SWT-6 mutual-cleanup pact): every account is
// 'itest-swt24-cal-%' and is created with calendar_in_availability=FALSE on
// purpose — this suite is about INGEST, and leaving unsynced accounts in
// availability scope would make the propose_slots assertions in
// integration_test.go refuse for reasons that have nothing to do with them.
// Cleanup runs before and after, in FK order.
//
// GREENFIELD NOTE (historical): ListCalendarCredentialedAccounts and
// CalendarScopes did not exist when this file was written (it compile-FAILed
// under `-tags integration`, the expected red) until the implementation landed.
// Imposed exported surface:
//
//	// ListCalendarCredentialedAccounts returns every provider='google' account
//	// that can actually read a calendar: a non-NULL refresh_token_encrypted AND
//	// calendar.readonly in scopes. CREDENTIAL-gated, never auth_type-gated —
//	// auth_type names the MAIL path and must keep saying 'app_password' after
//	// consent or mail breaks (SPEC decision "Credential-gated, not
//	// auth_type-gated"). Sibling of ListAppPasswordAccounts (mailsender.go).
//	func ListCalendarCredentialedAccounts(ctx context.Context, pool *pgxpool.Pool, onlyEmail string) ([]Account, error)
//
// The SPEC's "Files likely to touch" puts "which accounts are
// calendar-credentialed" in cmd/connectors/google/calendarsource.go. It is
// imposed in internal/connector/google here for one reason: it is a SQL
// predicate over columns, its regression test therefore has to be an
// integration test, and its only sibling (ListAppPasswordAccounts) already
// lives in this package. If the implementation keeps it in cmd, this test moves
// with it — the assertions, not the address, are the contract.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	calPrefix  = "itest-swt24-cal-"
	calKey     = "itest-swt24-token-key"
	calAppPass = "itest-app-password-value"
)

func calPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	requireCompose(t)
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	cleanupCalendarPhase(t, ctx, pool)
	t.Cleanup(func() { cleanupCalendarPhase(t, context.Background(), pool) })
	return pool
}

func cleanupCalendarPhase(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	owned := `(SELECT id FROM source_accounts WHERE account_email LIKE '` + calPrefix + `%')`
	stmts := []string{
		`DELETE FROM normalized_events WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + owned + `)`,
		`DELETE FROM raw_source_items  WHERE source_account_id IN ` + owned,
		`DELETE FROM sync_runs         WHERE source_account_id IN ` + owned,
		`DELETE FROM source_accounts   WHERE account_email LIKE '` + calPrefix + `%'`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
}

// calAccount inserts a row with the exact credential shape under test. Written
// as raw SQL rather than through a helper so the two discriminating columns are
// visible in the fixture.
func calAccount(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	suffix, authType string, withToken bool, scopes []string, withAppPassword bool) (int64, string) {
	t.Helper()
	email := calPrefix + suffix + "@example.com"
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts
		   (provider, account_email, auth_type, refresh_token_encrypted, app_password_encrypted,
		    scopes, send_enabled, calendar_in_availability)
		 VALUES ('google', $1, $2,
		         CASE WHEN $3 THEN pgp_sym_encrypt('refresh-'||$1, $4) ELSE NULL END,
		         CASE WHEN $5 THEN pgp_sym_encrypt($6, $4) ELSE NULL END,
		         $7, false, false)
		 RETURNING id`,
		email, authType, withToken, calKey, withAppPassword, calAppPass, scopes).Scan(&id); err != nil {
		t.Fatalf("insert account %s: %v", email, err)
	}
	return id, email
}

func credentialedEmails(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string]bool {
	t.Helper()
	accts, err := google.ListCalendarCredentialedAccounts(ctx, pool, "")
	if err != nil {
		t.Fatalf("ListCalendarCredentialedAccounts: %v", err)
	}
	out := map[string]bool{}
	for _, a := range accts {
		out[a.Email] = true
	}
	return out
}

// ---------------------------------------------------------------------------
// Criterion 17: the calendar phase runs for accounts that HAVE calendar
// credentials — a refresh token AND the calendar scope — regardless of
// auth_type. Both discriminating values come from Postgres, and each exclusion
// has a paired inclusion so the test cannot pass by selecting nothing.
// ---------------------------------------------------------------------------

func TestCalendarPhase_Integration_AccountSelectionIsCredentialGated(t *testing.T) {
	ctx := context.Background()
	pool := calPool(t, ctx)

	calScopes := []string{"https://www.googleapis.com/auth/calendar.readonly"}
	mailOnlyScopes := []string{"https://www.googleapis.com/auth/gmail.readonly"}

	_, dualAuth := calAccount(t, ctx, pool, "dual", "app_password", true, calScopes, true)
	_, oauthOnly := calAccount(t, ctx, pool, "oauth", "oauth", true, calScopes, false)
	_, noToken := calAccount(t, ctx, pool, "notoken", "app_password", false, calScopes, true)
	_, wrongScope := calAccount(t, ctx, pool, "wrongscope", "oauth", true, mailOnlyScopes, false)
	_, noScopes := calAccount(t, ctx, pool, "noscopes", "app_password", true, []string{}, true)

	got := credentialedEmails(t, ctx, pool)

	// THE criterion-17 assertion: a dual-auth row (IMAP/SMTP for mail, OAuth for
	// calendar) is legal and is exactly the shape this ticket targets. Gating on
	// auth_type='oauth' would skip it forever, which is the bug the SPEC's
	// "credential-gated, not auth_type-gated" decision exists to prevent.
	if !got[dualAuth] {
		t.Errorf("an auth_type='app_password' account WITH a refresh token and the calendar scope was not "+
			"selected (%s). The calendar phase must key on credentials; auth_type names the MAIL path and "+
			"must keep saying app_password after consent, or mail breaks", dualAuth)
	}
	if !got[oauthOnly] {
		t.Errorf("an auth_type='oauth' account with a token and the calendar scope was not selected (%s)", oauthOnly)
	}
	if got[noToken] {
		t.Errorf("an account with NO refresh token was selected (%s); there is nothing to build a client from, "+
			"and running it produces a login failure that reads as a credential problem rather than a "+
			"configuration one", noToken)
	}
	if got[wrongScope] {
		t.Errorf("an account whose scopes do not include calendar.readonly was selected (%s); its token cannot "+
			"read a calendar and Google answers 403 forever", wrongScope)
	}
	if got[noScopes] {
		t.Errorf("an account with empty scopes was selected (%s) — the production shape of all three google "+
			"rows before consent", noScopes)
	}

	// The onlyEmail narrowing (the --account flag), same idiom as
	// ListAppPasswordAccounts.
	narrowed, err := google.ListCalendarCredentialedAccounts(ctx, pool, dualAuth)
	if err != nil {
		t.Fatalf("ListCalendarCredentialedAccounts(onlyEmail): %v", err)
	}
	if len(narrowed) != 1 || narrowed[0].Email != dualAuth {
		t.Errorf("onlyEmail=%s selected %d account(s), want exactly that one", dualAuth, len(narrowed))
	}
}

// ---------------------------------------------------------------------------
// Criterion 16: add-calendar leaves the MAIL path alone. auth_type is what the
// send router and ListAppPasswordAccounts read; if consent flips it, mail stops.
// ---------------------------------------------------------------------------

func TestCalendarPhase_Integration_ConsentLeavesTheMailPathUntouched(t *testing.T) {
	ctx := context.Background()
	pool := calPool(t, ctx)

	id, email := calAccount(t, ctx, pool, "consent", "app_password", false, []string{}, true)

	// Before: the mailbox is an app-password mailbox with no OAuth at all —
	// the measured production shape of all three google rows.
	before, err := google.ListAppPasswordAccounts(ctx, pool, email)
	if err != nil {
		t.Fatalf("ListAppPasswordAccounts (before): %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("ListAppPasswordAccounts (before) = %d rows, want 1", len(before))
	}

	// add-calendar's store write: the refresh token and the calendar scope,
	// nothing else. This is UpsertGoogleAccount, which deliberately does not
	// name auth_type or app_password_encrypted (SPEC premise 9).
	if _, err := google.UpsertGoogleAccount(ctx, pool, email, "calendar-refresh-token", calKey,
		google.CalendarScopes, false); err != nil {
		t.Fatalf("UpsertGoogleAccount (add-calendar): %v", err)
	}

	var authType string
	var hasPassword, hasToken bool
	var scopes []string
	if err := pool.QueryRow(ctx,
		`SELECT auth_type, app_password_encrypted IS NOT NULL, refresh_token_encrypted IS NOT NULL, scopes
		   FROM source_accounts WHERE id=$1`, id).Scan(&authType, &hasPassword, &hasToken, &scopes); err != nil {
		t.Fatalf("read account back: %v", err)
	}
	if authType != "app_password" {
		t.Errorf("auth_type = %q after add-calendar, want app_password. Flipping it points the send router at "+
			"the Gmail API and ListAppPasswordAccounts stops returning the mailbox — mail breaks, silently, "+
			"for a change that was only about calendars", authType)
	}
	if !hasPassword {
		t.Errorf("app_password_encrypted was cleared by add-calendar; the mailbox can no longer authenticate")
	}
	if !hasToken {
		t.Errorf("refresh_token_encrypted is NULL after add-calendar; the consent stored nothing")
	}
	found := false
	for _, s := range scopes {
		if s == "https://www.googleapis.com/auth/calendar.readonly" {
			found = true
		}
	}
	if !found {
		t.Errorf("scopes = %v after add-calendar, want calendar.readonly present", scopes)
	}

	// The password still decrypts to the same secret — "present" is not "intact".
	var pw string
	if err := pool.QueryRow(ctx,
		`SELECT pgp_sym_decrypt(app_password_encrypted, $2) FROM source_accounts WHERE id=$1`,
		id, calKey).Scan(&pw); err != nil {
		t.Fatalf("decrypt app password: %v", err)
	}
	if pw != calAppPass {
		t.Errorf("app password changed across add-calendar")
	}

	// And the mail path still finds it.
	after, err := google.ListAppPasswordAccounts(ctx, pool, email)
	if err != nil {
		t.Fatalf("ListAppPasswordAccounts (after): %v", err)
	}
	if len(after) != 1 {
		t.Errorf("ListAppPasswordAccounts (after) = %d rows, want 1 — the account fell out of the IMAP pass", len(after))
	}

	// It is now also calendar-credentialed: the row is legitimately dual-auth.
	if !credentialedEmails(t, ctx, pool)[email] {
		t.Errorf("%s is not calendar-credentialed after add-calendar, so the consent bought nothing", email)
	}
}

// ---------------------------------------------------------------------------
// Criterion 19: the calendar pass writes ONE cursor key. SaveCursor replaces the
// whole blob, so a calendar save lands on top of whatever the IMAP pass wrote
// between its read and its write — and a lost IMAP position re-reads or SKIPS
// mail. Skipped mail is a delivery confirmation that never lands (invariant 5).
// bridge_ingest.go already guards this on the bridge path; the direct path does
// not.
// ---------------------------------------------------------------------------

func TestCalendarPhase_Integration_CursorWriteKeepsIMAPFolders(t *testing.T) {
	ctx := context.Background()
	pool := calPool(t, ctx)

	id, email := calAccount(t, ctx, pool, "cursor", "app_password", true,
		[]string{"https://www.googleapis.com/auth/calendar.readonly"}, true)

	// The IMAP pass's position at the moment the calendar pass STARTS, exactly
	// as imap_ingest writes it.
	if _, err := pool.Exec(ctx,
		`UPDATE source_accounts
		    SET sync_cursor = '{"imap_folders":{"INBOX":{"uidvalidity":12,"uid_next":9001}},
		                        "gmail_internal_date_ms":1751364000000}'::jsonb
		  WHERE id=$1`, id); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}

	// A calendar pass is not instantaneous: it reads the cursor, spends time in
	// Google, and writes back. THIS is the race, and it is not hypothetical —
	// the resident watch loop IDLEs on IMAP continuously while a CronJob runs
	// the calendar phase. The fake advances the IMAP position mid-flight,
	// standing in for a mail pass that finished in that gap. A whole-blob
	// SaveCursor then writes back the snapshot read before the gap and the mail
	// position is GONE; a field-scoped write leaves it alone.
	var raced int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.CompareAndSwapInt32(&raced, 0, 1) {
			if _, err := pool.Exec(context.Background(),
				`UPDATE source_accounts
				    SET sync_cursor = jsonb_set(sync_cursor, ARRAY['imap_folders'],
				        '{"INBOX":{"uidvalidity":12,"uid_next":9999}}'::jsonb, true)
				  WHERE id=$1`, id); err != nil {
				t.Errorf("concurrent IMAP cursor write: %v", err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items":         []json.RawMessage{calFull("itest-swt24-evt", "Standup", time.Now().Add(48*time.Hour).Format(time.RFC3339), time.Now().Add(49*time.Hour).Format(time.RFC3339))},
			"nextSyncToken": "SYNCTOK-SWT24",
		})
	}))
	defer srv.Close()

	sink := google.NewPGSink(pool)
	cc := google.NewCalendarClient(http.DefaultClient, srv.URL)
	if _, err := google.IngestCalendar(ctx, cc, sink, google.Account{ID: id, Email: email}, google.Config{}); err != nil {
		t.Fatalf("IngestCalendar: %v", err)
	}

	cur, err := sink.Cursor(ctx, id)
	if err != nil {
		t.Fatalf("read cursor back: %v", err)
	}
	if cur.CalendarSyncToken != "SYNCTOK-SWT24" {
		t.Errorf("calendar_sync_token = %q, want SYNCTOK-SWT24 (the pass must still record its own position)",
			cur.CalendarSyncToken)
	}
	inbox, ok := cur.IMAPFolders["INBOX"]
	if !ok {
		t.Fatalf("the calendar pass ERASED imap_folders (cursor now %+v)", cur)
	}
	if inbox.UIDNext != 9999 {
		t.Errorf("INBOX uid_next = %d, want 9999. The calendar pass wrote back the whole sync_cursor blob it "+
			"read BEFORE the mail pass moved, rolling the IMAP position back to %d. A rolled-back UID position "+
			"re-reads or skips mail, and skipped mail is a delivery confirmation that never lands (invariant "+
			"5). Write the one key: SaveCursorField(accountID, \"calendar_sync_token\", ...), as "+
			"bridge_ingest.go already does (criterion 19)", inbox.UIDNext, inbox.UIDNext)
	}
	if atomic.LoadInt32(&raced) != 1 {
		t.Errorf("the concurrent-writer hook never fired, so this test proved nothing about the cursor race")
	}

	// The gmail position is a sibling key on the same blob and survives too.
	if cur.GmailInternalDateMS != 1751364000000 {
		t.Errorf("gmail_internal_date_ms = %d, want 1751364000000", cur.GmailInternalDateMS)
	}

	// Raw-first still holds for the phase that wrote the cursor (invariant 1).
	var rawCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM raw_source_items WHERE source_account_id=$1 AND external_id LIKE 'calendar:%'`,
		id).Scan(&rawCount); err != nil {
		t.Fatalf("count raw calendar rows: %v", err)
	}
	if rawCount != 1 {
		t.Errorf("raw calendar rows = %d, want 1 (provider JSON lands before anything is normalized)", rawCount)
	}
	var okRuns int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM sync_runs WHERE source_account_id=$1 AND status='ok' AND stats->>'phase'='calendar'`,
		id).Scan(&okRuns); err != nil {
		t.Fatalf("count calendar runs: %v", err)
	}
	if okRuns != 1 {
		t.Errorf("ok calendar sync_runs = %d, want 1 — this row IS the freshness signal Part A reads", okRuns)
	}
	if !strings.HasPrefix(email, calPrefix) {
		t.Fatalf("fixture email escaped the cleanup scope: %s", email)
	}
}
