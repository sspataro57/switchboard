//go:build integration

package google_test

// Postgres integration tests for the stored Microsoft mailbox (SPEC
// docs/tickets/microsoft-oauth-mail_SPEC.md, acceptance criteria 5 (the
// pgcrypto round trip), 11 (invariant 5 — the seam D3 exists to protect), 14
// (migration 0037) and 15). Build-tagged `integration`, env-gated on
// DATABASE_URL, production-db refusal in msxPool (credential_integration_test.go).
//
// Run it in the ISOLATED scratch database — see credential_integration_test.go's
// header for the three commands.
//
// No live Microsoft and no live IMAP: the mailbox is the in-memory fakeIMAP
// (fake_imap_test.go), which is legitimate HERE because criterion 11 is about
// what the NORMALIZER does with a message from that address, not about how the
// socket was authenticated (that is criterion 4's wire-level test).
//
// EXPECTED FAILURE MODE: **compile error** (google.UpsertXOAuth2Account,
// google.AuthTypeXOAuth2 are undefined), then, once they exist, an ASSERTION
// failure in the migration test until migrations/0037_microsoft_oauth_mail.sql
// is written and applied to the scratch db.
//
// IMPOSED SURFACE (criterion 5 lists the row's fields but names no function;
// this is the UpsertAppPasswordAccount sibling):
//
//	// UpsertXOAuth2Account stores (or re-keys) a Microsoft IMAP mailbox: exactly
//	// ONE provider='google' row, auth_type='xoauth2', the refresh token
//	// pgcrypto-encrypted, send_enabled=false and calendar_in_availability=FALSE
//	// unconditionally (D3's mandatory belt-and-braces: UpsertAppPasswordAccount
//	// defaults that flag TRUE, and a google row carrying it with no calendar sync
//	// makes propose_slots refuse for every account, forever).
//	func UpsertXOAuth2Account(ctx context.Context, pool *pgxpool.Pool,
//	    email, refreshToken, tokenKey string, scopes []string, hosts MailHosts) (int64, error)

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

const (
	// A test-scoped address under the PRODUCTION provider value, exactly as
	// imap_integration_test.go does — that is the only way ownEmailSet and
	// PendingRaw (both `provider='google'`) are exercised for real.
	msxMSNEmail = msxPrefix + "msn@example.com"

	msxOwnMsgID     = "<sent-from-the-msn-mailbox@outlook.example>"
	msxStrangerMsgD = "<inbound-from-a-stranger@world.example>"
)

// ---- criterion 5: what add-microsoft stores ------------------------------------

func TestUpsertXOAuth2Account_Integration_StoresExactlyTheSpeccedRow(t *testing.T) {
	ctx := context.Background()
	pool := msxPool(t, ctx)

	const refresh = "0.AXoA-REFRESH-TOKEN-FROM-THE-DEVICE-FLOW"
	hosts := google.MailHosts{
		IMAPHost: "outlook.office365.com", IMAPPort: 993,
		SMTPHost: "smtp-mail.outlook.com", SMTPPort: 587,
	}

	id, err := google.UpsertXOAuth2Account(ctx, pool, msxMSNEmail, refresh, msxKey, google.MicrosoftScopes, hosts)
	if err != nil {
		t.Fatalf("UpsertXOAuth2Account: %v", err)
	}

	var (
		provider, email, authType  string
		refreshEnc, appPasswordEnc []byte
		scopes                     []string
		sendEnabled, calendarAvail bool
		imapHost, smtpHost         string
		imapPort, smtpPort         int
	)
	if err := pool.QueryRow(ctx,
		`SELECT provider, account_email, auth_type, refresh_token_encrypted, app_password_encrypted,
		        scopes, send_enabled, calendar_in_availability,
		        COALESCE(imap_host,''), COALESCE(imap_port,0), COALESCE(smtp_host,''), COALESCE(smtp_port,0)
		   FROM source_accounts WHERE id=$1`, id).
		Scan(&provider, &email, &authType, &refreshEnc, &appPasswordEnc, &scopes,
			&sendEnabled, &calendarAvail, &imapHost, &imapPort, &smtpHost, &smtpPort); err != nil {
		t.Fatalf("read the stored row: %v", err)
	}

	if provider != "google" {
		t.Errorf("provider = %q, want google. D3: six production predicates spell provider='google' — "+
			"ownEmailSet (invariant 5), PendingRaw, draft_delivery's from-account resolution, accountSelect, "+
			"the availability scope and opsctl mail refetch. A 'microsoft' row is invisible to all of them, "+
			"and the funnel stays empty with NO error anywhere", provider)
	}
	if authType != google.AuthTypeXOAuth2 {
		t.Errorf("auth_type = %q, want xoauth2", authType)
	}
	if len(refreshEnc) == 0 {
		t.Errorf("refresh_token_encrypted is empty; the token must be pgp_sym_encrypt'd at rest")
	}
	if appPasswordEnc != nil {
		t.Errorf("app_password_encrypted was written by the Microsoft path; Microsoft issues no app password " +
			"that IMAP accepts — that is the fact this whole ticket starts from")
	}
	if sendEnabled {
		t.Errorf("send_enabled = true. Out of scope: no SMTP.Send consent is requested, and send_delivery " +
			"enforces this flag (invariant 4)")
	}
	if calendarAvail {
		t.Errorf("calendar_in_availability = true. D3 calls this MANDATORY belt-and-braces: a " +
			"provider='google' row with that flag and no calendar sync makes propose_slots refuse for " +
			"EVERY account, forever (SWT-24 readiness)")
	}
	if imapHost != "outlook.office365.com" || imapPort != 993 || smtpHost != "smtp-mail.outlook.com" || smtpPort != 587 {
		t.Errorf("endpoints = imap %s:%d / smtp %s:%d, want the Outlook ones from the argument",
			imapHost, imapPort, smtpHost, smtpPort)
	}
	if len(scopes) != len(google.MicrosoftScopes) {
		t.Errorf("scopes = %v, want the four consented scopes %v", scopes, google.MicrosoftScopes)
	}

	// pgcrypto round trip, and the ciphertext must not contain the plaintext.
	got, err := google.DecryptRefreshToken(ctx, pool, id, msxKey)
	if err != nil {
		t.Fatalf("DecryptRefreshToken: %v", err)
	}
	if got != refresh {
		t.Errorf("DecryptRefreshToken = %q, want the stored token", got)
	}
	if _, err := google.DecryptRefreshToken(ctx, pool, id, "itest-msoauth-WRONG-key"); err == nil {
		t.Errorf("DecryptRefreshToken with the wrong key returned no error; the column is not really encrypted")
	}
	if n := scanInt(t, ctx, pool,
		`SELECT count(*) FROM source_accounts WHERE id=$1 AND encode(refresh_token_encrypted,'escape') LIKE '%'||$2||'%'`,
		id, refresh); n != 0 {
		t.Errorf("the refresh token is recoverable from the stored bytes without the key")
	}

	// Re-running the onboarding (a re-consent) must not resurrect the two flags.
	if _, err := pool.Exec(ctx, `UPDATE source_accounts SET send_enabled=true WHERE id=$1`, id); err != nil {
		t.Fatalf("flip send_enabled: %v", err)
	}
	again, err := google.UpsertXOAuth2Account(ctx, pool, msxMSNEmail, refresh+"-v2", msxKey, google.MicrosoftScopes, hosts)
	if err != nil {
		t.Fatalf("UpsertXOAuth2Account (re-consent): %v", err)
	}
	if again != id {
		t.Errorf("re-consent created a SECOND row (%d, was %d); the ON CONFLICT target is "+
			"(provider, account_email)", again, id)
	}
	if err := pool.QueryRow(ctx, `SELECT calendar_in_availability FROM source_accounts WHERE id=$1`, id).
		Scan(&calendarAvail); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if calendarAvail {
		t.Errorf("a re-consent set calendar_in_availability=true")
	}
	if got, err := google.DecryptRefreshToken(ctx, pool, id, msxKey); err != nil || got != refresh+"-v2" {
		t.Errorf("re-consent stored %q (err=%v), want the new token", got, err)
	}
	if n := scanInt(t, ctx, pool,
		`SELECT count(*) FROM source_accounts WHERE provider='google' AND lower(account_email)=lower($1)`,
		msxMSNEmail); n != 1 {
		t.Errorf("source_accounts holds %d rows for %s, want exactly ONE (criterion 5)", n, msxMSNEmail)
	}
}

// A personal Microsoft account can be registered against a gmail.com address, so
// `add-microsoft` on one of the LIVE app-password mailboxes would pass both
// identity checks and then re-key it to xoauth2 — orphaning its stored password
// and making every send from it refuse. Converting a mailbox between credential
// kinds has to be deliberate, never a side effect of a typo.
func TestUpsertXOAuth2Account_Integration_RefusesToConvertAnAppPasswordMailbox(t *testing.T) {
	ctx := context.Background()
	pool := msxPool(t, ctx)

	id, email := msxSeed(t, ctx, pool, "convert-guard", google.AuthTypeAppPassword)

	_, err := google.UpsertXOAuth2Account(ctx, pool, email, "0.AXoA-WOULD-CLOBBER", msxKey,
		google.MicrosoftScopes, google.MailHosts{})
	if err == nil {
		t.Fatalf("UpsertXOAuth2Account converted the app_password mailbox %s instead of refusing", email)
	}
	for _, want := range []string{email, google.AuthTypeAppPassword} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}

	// The row must be untouched — a refusal that already clobbered half the row
	// is not a refusal.
	var authType string
	var hasPassword, hasRefresh bool
	if err := pool.QueryRow(ctx,
		`SELECT auth_type, app_password_encrypted IS NOT NULL, refresh_token_encrypted IS NOT NULL
		   FROM source_accounts WHERE id=$1`, id).Scan(&authType, &hasPassword, &hasRefresh); err != nil {
		t.Fatalf("re-read the seeded account: %v", err)
	}
	if authType != google.AuthTypeAppPassword || !hasPassword || hasRefresh {
		t.Errorf("after the refusal the row is auth_type=%q app_password=%v refresh_token=%v; "+
			"want app_password, true, false", authType, hasPassword, hasRefresh)
	}
}

// ---- criterion 11: invariant 5, the seam D3 exists to protect ------------------

func TestMSNMailbox_Integration_OwnSendsNormalizeOutbound(t *testing.T) {
	ctx := context.Background()
	pool := msxPool(t, ctx)

	hosts := google.MailHosts{IMAPHost: "outlook.office365.com", IMAPPort: 993, SMTPHost: "smtp-mail.outlook.com", SMTPPort: 587}
	acctID, err := google.UpsertXOAuth2Account(ctx, pool, msxMSNEmail, "0.AXoA-REFRESH", msxKey, google.MicrosoftScopes, hosts)
	if err != nil {
		t.Fatalf("UpsertXOAuth2Account: %v", err)
	}

	tasksBefore := scanInt(t, ctx, pool, `SELECT count(*) FROM tasks`)
	deliveriesBefore := scanInt(t, ctx, pool, `SELECT count(*) FROM deliveries`)

	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	seen := now.Add(-24 * time.Hour)

	src := newFakeIMAP()
	// Outlook advertises \Sent on "Sent Items" (D11): SelectFolders picks it by
	// the RFC 6154 flag, never by name, which is why no gmailSentFolder sibling
	// is needed.
	src.folders = []google.Folder{
		{Name: imapINBOX, UIDValidity: 7},
		{Name: "Sent Items", Sent: true, UIDValidity: 8},
	}
	src.add("Sent Items", imapMsg(1, seen, []string{"\\Seen"}, rfc822([]string{
		`Message-ID: ` + msxOwnMsgID,
		`Subject: Re: dentist`,
		`From: ` + msxMSNEmail,
		`To: someone@world.example`,
	}, "Sent from the Outlook app on his phone.")))
	src.add(imapINBOX, imapMsg(1, seen, nil, rfc822([]string{
		`Message-ID: ` + msxStrangerMsgD,
		`Subject: your appointment`,
		`From: stranger@world.example`,
		`To: ` + msxMSNEmail,
	}, "See you Thursday.")))

	sink := google.NewPGSink(pool)
	acct := google.Account{ID: acctID, Email: msxMSNEmail, AuthType: google.AuthTypeXOAuth2}
	cfg := google.Config{Now: now, Backfill: google.DefaultBackfill, MaxMessageBytes: google.DefaultMaxMessageBytes}

	if _, err := google.IngestIMAP(ctx, src, sink, acct, cfg); err != nil {
		t.Fatalf("IngestIMAP: %v", err)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items WHERE source_account_id=$1 AND raw_json->>'source'='imap'`, acctID); got != 2 {
		t.Errorf("raw rows = %d, want 2 (raw-first, invariant 1 — unchanged by this ticket)", got)
	}
	if _, err := google.Normalize(ctx, sink, google.Config{}); err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	// Invariant 5. Concretely: the row is provider='google', so ownEmailSet
	// (sink.go) contains the msn address, so isOwnAddress marks this outbound.
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_messages WHERE external_message_id=$1 AND direction='outbound' AND channel='gmail'`,
		msxOwnMsgID); got != 1 {
		t.Errorf("our own MSN send: outbound gmail-channel rows = %d, want 1. If it is INBOUND, "+
			"ownEmailSet does not contain %s — which is what a provider='microsoft' row would have "+
			"caused — and the capture engine (direction='inbound', rules_store.go) plus triage would "+
			"mint tasks from his own mail", got, msxMSNEmail)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_messages WHERE external_message_id=$1 AND direction='inbound'`,
		msxOwnMsgID); got != 0 {
		t.Errorf("our own send has an inbound copy; it is eligible for capture and triage")
	}
	// The control, without which "everything is outbound" would also pass.
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_messages WHERE external_message_id=$1 AND direction='inbound'`,
		msxStrangerMsgD); got != 1 {
		t.Errorf("a stranger's mail to the MSN address = %d inbound rows, want 1; the direction rule is "+
			"not discriminating, it is answering the same way for everything", got)
	}

	// Criterion 12: thread keys and channel are UNCHANGED — the `gmail:` prefix
	// splitGmailThreadKey parses and capture_rules match on.
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_threads WHERE thread_key LIKE 'gmail:'||$1||':%'`, msxMSNEmail); got < 1 {
		t.Errorf("no thread_key of the shape gmail:%s:<root>. The capture rule Salvador adds is "+
			"--type thread_key_prefix --pattern 'gmail:sspataro57@msn.com:'; a re-keyed prefix would make "+
			"it match nothing and would need a data migration for ~16,500 stored rows", msxMSNEmail)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_messages WHERE raw_source_item_id IN
		   (SELECT id FROM raw_source_items WHERE source_account_id=$1) AND channel<>'gmail'`, acctID); got != 0 {
		t.Errorf("%d messages from the MSN mailbox carry a channel other than 'gmail'", got)
	}

	// Criterion 15.
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM tasks`); got != tasksBefore {
		t.Errorf("tasks changed: before=%d after=%d (ingest + normalize create zero tasks)", tasksBefore, got)
	}
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM deliveries`); got != deliveriesBefore {
		t.Errorf("deliveries changed: before=%d after=%d", deliveriesBefore, got)
	}
}

// ---- criterion 14: migration 0037 ----------------------------------------------

const msx0037 = "0037_microsoft_oauth_mail.sql"

func TestMigration0037_Integration_AppliesTwiceAndConstrainsAuthType(t *testing.T) {
	ctx := context.Background()
	pool := msxPool(t, ctx)

	path := filepath.Join(msxRepoRoot, "migrations", msx0037)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (criterion 14: the migration is part of this ticket, and merging one is not "+
			"applying it)", path, err)
	}
	sql := string(body)

	// Forward-only, numbered, and never edited after it is applied anywhere
	// (IK: the runner keys on schema_migrations.version with no checksum, so an
	// edited file is skipped SILENTLY and the schema diverges with no error).
	if strings.Contains(strings.ToLower(sql), "drop table") || strings.Contains(strings.ToLower(sql), "create table") {
		t.Errorf("%s creates or drops a table; the SPEC's data-model section says no new tables and no new "+
			"columns", msx0037)
	}

	tablesBefore := scanInt(t, ctx, pool,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema='public'`)

	// Applied twice, in the same session: the DO $$ guards make a re-apply a
	// no-op rather than an error.
	for i := 1; i <= 2; i++ {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("apply %s (pass %d): %v", msx0037, i, err)
		}
	}

	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema='public'`); got != tablesBefore {
		t.Errorf("public tables went from %d to %d; criterion 15 says zero new tables", tablesBefore, got)
	}

	for _, conname := range []string{"source_accounts_auth_type_check", "source_accounts_xoauth2_token_present"} {
		if got := scanInt(t, ctx, pool, `SELECT count(*) FROM pg_constraint WHERE conname=$1`, conname); got != 1 {
			t.Errorf("constraint %s is missing after applying %s", conname, msx0037)
		}
	}

	// The auth_type domain, exercised through real INSERTs.
	for i, tc := range []struct {
		authType string
		ok       bool
	}{
		{"oauth", true},
		{"app_password", true},
		{"xoauth2", true},
		{"microsoft", false},
		{"XOAUTH2", false}, // the value is lowercase; a case variant is a different value
	} {
		// A DISTINCT address per case: 'xoauth2' and 'XOAUTH2' would otherwise
		// collide on the (provider, account_email) unique index and the case
		// variant would be "refused" by the wrong constraint.
		email := fmt.Sprintf("%scheck-%d@example.com", msxPrefix, i)
		_, err := pool.Exec(ctx,
			`INSERT INTO source_accounts (provider, account_email, auth_type,
			   refresh_token_encrypted, app_password_encrypted, scopes, send_enabled, calendar_in_availability)
			 VALUES ('google', $1, $2, pgp_sym_encrypt('tok',$3), pgp_sym_encrypt('pw',$3), '{}', false, false)`,
			email, tc.authType, msxKey)
		if tc.ok && err != nil {
			t.Errorf("INSERT with auth_type=%q was refused: %v", tc.authType, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("INSERT with auth_type=%q was ACCEPTED; the CHECK must enumerate exactly "+
				"oauth, app_password, xoauth2", tc.authType)
		}
	}

	// An xoauth2 row with no refresh token is a mailbox that can never
	// authenticate (0014's app_password_present shape).
	_, err = pool.Exec(ctx,
		`INSERT INTO source_accounts (provider, account_email, auth_type, scopes, send_enabled, calendar_in_availability)
		 VALUES ('google', $1, 'xoauth2', '{}', false, false)`, msxPrefix+"tokenless@example.com")
	if err == nil {
		t.Errorf("an xoauth2 row with a NULL refresh_token_encrypted was accepted; every pass for it would " +
			"fail to authenticate and the cause would be a row state nothing refused")
	} else if !strings.Contains(err.Error(), "xoauth2_token_present") {
		t.Errorf("the refusal came from something other than source_accounts_xoauth2_token_present: %v", err)
	}
	// An app_password row is unaffected by the new CHECK.
	if _, err := pool.Exec(ctx,
		`INSERT INTO source_accounts (provider, account_email, auth_type, app_password_encrypted, scopes, send_enabled, calendar_in_availability)
		 VALUES ('google', $1, 'app_password', pgp_sym_encrypt('pw',$2), '{}', false, false)`,
		msxPrefix+"stillfine@example.com", msxKey); err != nil {
		t.Errorf("the new CHECK refuses an ordinary app-password row (criterion 12): %v", err)
	}
}

// The CHECK must FAIL CLOSED on a NULL auth_type (criterion 14, IK: "`col = 'x'`
// inside a CHECK passes on NULL").
//
// auth_type is NOT NULL today, so no INSERT can reach the NULL case — which is
// exactly why this evaluates the STORED EXPRESSION itself against NULLs instead
// of fabricating a row. A fail-open spelling (`auth_type <> 'xoauth2' OR
// refresh_token_encrypted IS NOT NULL`) yields UNKNOWN there, and UNKNOWN passes
// a CHECK; both of the spellings that merely READ like guards admit the row.
func TestMigration0037_Integration_TokenCheckIsNullSafe(t *testing.T) {
	ctx := context.Background()
	pool := msxPool(t, ctx)

	var def string
	if err := pool.QueryRow(ctx,
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='source_accounts_xoauth2_token_present'`).
		Scan(&def); err != nil {
		t.Fatalf("read source_accounts_xoauth2_token_present: %v (apply migration 0037 to this database first)", err)
	}
	expr := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(def), "CHECK"))
	if expr == "" {
		t.Fatalf("could not extract the CHECK expression from %q", def)
	}

	// FALSE, not merely "not UNKNOWN". Definiteness alone is satisfied by
	// `auth_type IS NULL OR ...`, which evaluates to TRUE and ADMITS the
	// tokenless row while reading like a guard — so asserting IS NOT NULL would
	// pass on the very spelling this constraint exists to avoid.
	var rejects bool
	q := `SELECT (` + expr + `) IS FALSE
	        FROM (SELECT NULL::text AS auth_type, NULL::bytea AS refresh_token_encrypted) t`
	if err := pool.QueryRow(ctx, q).Scan(&rejects); err != nil {
		t.Fatalf("evaluate %q: %v", q, err)
	}
	if !rejects {
		t.Errorf("with auth_type NULL the constraint %q does not evaluate to FALSE, so a tokenless row is "+
			"admitted: UNKNOWN passes a CHECK, and so does TRUE. Spell it "+
			"`auth_type IS NOT NULL AND (auth_type <> 'xoauth2' OR refresh_token_encrypted IS NOT NULL)`", def)
	}
}
