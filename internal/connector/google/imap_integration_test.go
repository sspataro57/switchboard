//go:build integration

package google_test

// Compose-db integration walk for the IMAP mail connector (SPEC
// imap-mail-connector, acceptance criteria 3, 4, 5, 8, 10, 11, 15, 19, 20;
// verification protocol step 3). Build-tagged `integration` AND env-gated on
// DATABASE_URL; the IMAP server is replaced by the in-memory fakeIMAP
// (fake_imap_test.go) — NEVER a live IMAP connection, never a live send.
//
//   DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//     go test -tags integration -run IMAP ./internal/connector/google/
//
// GREENFIELD NOTE: migration 0014, imap.go, imap_ingest.go, rfc822.go, the
// `imap:` branch of Normalize, UpsertAppPasswordAccount/DecryptAppPassword and
// the body-prefix loop-closure belt do not exist yet, so this compile-FAILs /
// fails at the first assertion until they do — the expected failure mode.
// Imposed surface beyond fake_imap_test.go's list (SPEC "Files likely to
// touch" → oauth.go, sink.go):
//
//   // Per-account mail endpoints (criterion 4 flags).
//   type MailHosts struct { IMAPHost string; IMAPPort int; SMTPHost string; SMTPPort int }
//   // Writes ONE provider='google' row with auth_type='app_password',
//   // app_password_encrypted = pgp_sym_encrypt(password, key), send_enabled=false,
//   // scopes='{}'. It NEVER touches refresh_token_encrypted.
//   func UpsertAppPasswordAccount(ctx context.Context, pool *pgxpool.Pool,
//       email, password, key string, hosts MailHosts, calendarInAvailability bool) (int64, error)
//   func DecryptAppPassword(ctx context.Context, pool *pgxpool.Pool, accountID int64, key string) (string, error)
//   // Account/ListAccounts gain AuthType, IMAPHost/IMAPPort, SMTPHost/SMTPPort.
//
// Cross-suite discipline (SWT-6 mutual-cleanup pact, criterion 20): this suite's
// accounts are provider='google' (production value — so the criterion-11 partial
// unique index is exercised for real) with test-scoped emails 'itest-imap-%'.
// Every count assertion is scoped to those account ids. Its INBOUND
// normalized_messages are visible to triage's GLOBAL pending filter, so
// cleanupTriage (internal/triage/integration_test.go) gains matching
// 'itest-imap-%' deletes — the pact-join obligation on the triage side; this
// file cleans only its OWN corpus, in FK order, rerunnably (before AND after).

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	imapAcctA = "itest-imap-a@example.com"
	imapAcctB = "itest-imap-b@example.com"
	imapSlug  = "itest-imap-proj"
	imapKey   = "itest-imap-token-key"

	imapSharedMsgID    = "<shared-imap@mail.example>"
	imapOwnMsgID       = "<sb-itest-imap-1@example.com>"          // = delivery 1's sent_external_id
	imapReservedMsgID  = "<sb-itest-imap-2-reserved@example.com>" // reserved, then REWRITTEN by the relay
	imapRewrittenMsgID = "<rewritten-by-relay@mail.example>"      // what actually landed in Sent
	imapBeltBody       = "Confirming the body-prefix belt: the relay rewrote our Message-ID, so the primary matcher can never fire."
)

func cleanupIMAP(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const scope = `(SELECT id FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-imap-%')`
	stmts := []string{
		`DELETE FROM task_events WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + imapSlug + `'))`,
		`DELETE FROM deliveries WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + imapSlug + `'))`,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + imapSlug + `')`,
		`DELETE FROM projects WHERE slug='` + imapSlug + `'`,
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + scope + `)`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + scope + `)`,
		`DELETE FROM normalized_events WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + scope + `)`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'gmail:itest-imap-%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + scope,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + scope,
		`DELETE FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-imap-%'`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
}

// ---- criterion 3 + 4: migration 0014 and the app-password credential path -----

func TestIMAP_Integration_Migration0014AndAppPassword(t *testing.T) {
	requireCompose(t)
	ctx := context.Background()
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	defer pool.Close()

	// Criterion 3: the columns migration 0014 adds to source_accounts.
	for _, col := range []string{"auth_type", "app_password_encrypted", "imap_host", "imap_port", "smtp_host", "smtp_port"} {
		if got := scanInt(t, ctx, pool,
			`SELECT count(*) FROM information_schema.columns WHERE table_name='source_accounts' AND column_name=$1`, col); got != 1 {
			t.Fatalf("source_accounts.%s missing — apply migration 0014 (make migrate)", col)
		}
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM pg_constraint WHERE conname='source_accounts_auth_type_check'`); got != 1 {
		t.Fatalf("CHECK constraint source_accounts_auth_type_check missing — apply migration 0014")
	}

	cleanupIMAP(t, ctx, pool)
	defer cleanupIMAP(t, ctx, pool)

	const pw = "abcd efgh ijkl mnop"
	hosts := google.MailHosts{IMAPHost: "imap.gmail.com", IMAPPort: 993, SMTPHost: "smtp.gmail.com", SMTPPort: 587}
	acctID, err := google.UpsertAppPasswordAccount(ctx, pool, imapAcctA, pw, imapKey, hosts, false)
	if err != nil {
		t.Fatalf("UpsertAppPasswordAccount: %v", err)
	}

	var authType string
	var encrypted []byte
	var refresh []byte
	var sendEnabled bool
	var imapHost string
	var imapPort, smtpPort int
	if err := pool.QueryRow(ctx,
		`SELECT auth_type, app_password_encrypted, refresh_token_encrypted, send_enabled,
		        COALESCE(imap_host,''), COALESCE(imap_port,0), COALESCE(smtp_port,0)
		 FROM source_accounts WHERE id=$1`, acctID).
		Scan(&authType, &encrypted, &refresh, &sendEnabled, &imapHost, &imapPort, &smtpPort); err != nil {
		t.Fatalf("read app-password account: %v", err)
	}
	if authType != "app_password" {
		t.Errorf("auth_type = %q, want app_password", authType)
	}
	if len(encrypted) == 0 {
		t.Errorf("app_password_encrypted is empty; the secret must be pgp_sym_encrypt'd at rest")
	}
	if refresh != nil {
		t.Errorf("refresh_token_encrypted was written by the app-password path; it must stay untouched")
	}
	if sendEnabled {
		t.Errorf("send_enabled = true on a fresh app-password account; sending is enabled by hand, per account")
	}
	if imapHost != "imap.gmail.com" || imapPort != 993 || smtpPort != 587 {
		t.Errorf("hosts = %s:%d smtp:%d, want imap.gmail.com:993 / 587", imapHost, imapPort, smtpPort)
	}
	// The ciphertext must not contain the plaintext.
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM source_accounts WHERE id=$1 AND encode(app_password_encrypted,'escape') LIKE '%'||$2||'%'`,
		acctID, pw); got != 0 {
		t.Errorf("the app password is recoverable from the stored bytes without the key")
	}

	got, err := google.DecryptAppPassword(ctx, pool, acctID, imapKey)
	if err != nil {
		t.Fatalf("DecryptAppPassword: %v", err)
	}
	if got != pw {
		t.Errorf("DecryptAppPassword = %q, want the stored secret", got)
	}
	if _, err := google.DecryptAppPassword(ctx, pool, acctID, "itest-imap-WRONG-key"); err == nil {
		t.Errorf("DecryptAppPassword with the wrong key returned no error")
	}

	// google-auth list surfaces auth_type per row.
	accounts, err := google.NewPGSink(pool).ListAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	found := false
	for _, a := range accounts {
		if a.Email == imapAcctA {
			found = true
			if a.AuthType != "app_password" {
				t.Errorf("ListAccounts AuthType = %q, want app_password", a.AuthType)
			}
			if a.IMAPHost != "imap.gmail.com" || a.SMTPPort != 587 {
				t.Errorf("ListAccounts host fields = %s / %d", a.IMAPHost, a.SMTPPort)
			}
		}
	}
	if !found {
		t.Errorf("ListAccounts did not return the app-password account")
	}
}

// ---- criteria 5, 8, 10, 11, 15, 19: the full ingest -> normalize walk ----------

func TestIMAP_Integration_RawFirstNormalizeDedupLoopClosure(t *testing.T) {
	requireCompose(t)
	ctx := context.Background()
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	defer pool.Close()

	cleanupIMAP(t, ctx, pool)
	defer cleanupIMAP(t, ctx, pool)

	hosts := google.MailHosts{IMAPHost: "imap.gmail.com", IMAPPort: 993, SMTPHost: "smtp.gmail.com", SMTPPort: 587}
	aID, err := google.UpsertAppPasswordAccount(ctx, pool, imapAcctA, "pw-a", imapKey, hosts, false)
	if err != nil {
		t.Fatalf("seed account A: %v", err)
	}
	bID, err := google.UpsertAppPasswordAccount(ctx, pool, imapAcctB, "pw-b", imapKey, hosts, false)
	if err != nil {
		t.Fatalf("seed account B: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE source_accounts SET send_enabled=true WHERE id=$1`, aID); err != nil {
		t.Fatalf("enable sending on A: %v", err)
	}

	// A delivered task carrying two SENT deliveries: one whose Message-ID
	// survived submission (primary matcher) and one the relay rewrote (belt).
	var projID, taskID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path)
		 VALUES ($1,$1,'itest-imap-client','manual','dashboard','/tmp/itest') RETURNING id`, imapSlug).Scan(&projID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'itest-imap work','claude','delivered') RETURNING id`, projID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	// The deliveries are dated just BEFORE the Sent-folder copies they produced.
	// Those copies carry no Date header, so their SentAt falls back to the fake
	// INTERNALDATE `seen` (now-24h, 2026-07-25 12:00). Seeding a delivery at
	// now() — or at any instant after `seen` — would describe a send that
	// happened after its own copy arrived, and the belt's attempt-time floor
	// (the SWT-16 rule: a content matcher needs a lower time bound) correctly
	// refuses to bind those.
	deliverySentAt := time.Date(2026, 7, 25, 11, 0, 0, 0, time.UTC)
	var deliveryPrimary, deliveryBelt int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, status, body, from_account_id, sent_external_id, sent_at)
		 VALUES ($1,'gmail','sent','our reply body',$2,$3,$4) RETURNING id`,
		taskID, aID, imapOwnMsgID, deliverySentAt).Scan(&deliveryPrimary); err != nil {
		t.Fatalf("seed primary delivery: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, status, body, from_account_id, sent_external_id, sent_at)
		 VALUES ($1,'gmail','sent',$2,$3,$4,$5) RETURNING id`,
		taskID, imapBeltBody, aID, imapReservedMsgID, deliverySentAt).Scan(&deliveryBelt); err != nil {
		t.Fatalf("seed belt delivery: %v", err)
	}

	tasksBefore := scanInt(t, ctx, pool, `SELECT count(*) FROM tasks`)
	deliveriesBefore := scanInt(t, ctx, pool, `SELECT count(*) FROM deliveries`)

	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	seen := now.Add(-24 * time.Hour)

	// Account A: INBOX has the shared inbound; Sent has both of our sends.
	srcA := newFakeIMAP()
	srcA.folders = []google.Folder{
		{Name: imapINBOX, UIDValidity: 12},
		{Name: imapSent, Sent: true, UIDValidity: 9},
	}
	srcA.add(imapINBOX, imapMsg(1, seen, nil, rfc822([]string{
		`Message-ID: ` + imapSharedMsgID,
		`Subject: shared inbound`,
		`From: stranger@world.example`,
		`To: ` + imapAcctA,
	}, "shared body")))
	srcA.add(imapSent, imapMsg(1, seen, []string{"\\Seen"}, rfc822([]string{
		`Message-ID: ` + imapOwnMsgID,
		`Subject: Re: shared inbound`,
		`From: ` + imapAcctA,
		`To: ` + imapAcctB,
	}, "our reply body")))
	srcA.add(imapSent, imapMsg(2, seen, []string{"\\Seen"}, rfc822([]string{
		`Message-ID: ` + imapRewrittenMsgID,
		`Subject: Re: belt`,
		`From: ` + imapAcctA,
		`To: client@acme.example`,
	}, imapBeltBody)))

	// Account B: the same shared inbound (cross-account dedup) plus the copy of
	// our own send it received (cross-folder + cross-account dedup).
	srcB := newFakeIMAP()
	srcB.folders = []google.Folder{{Name: imapINBOX, UIDValidity: 30}}
	srcB.add(imapINBOX, imapMsg(1, seen, nil, rfc822([]string{
		`Message-ID: ` + imapSharedMsgID,
		`Subject: shared inbound`,
		`From: stranger@world.example`,
		`To: ` + imapAcctB,
	}, "shared body")))
	srcB.add(imapINBOX, imapMsg(2, seen, nil, rfc822([]string{
		`Message-ID: ` + imapOwnMsgID,
		`Subject: Re: shared inbound`,
		`From: ` + imapAcctA,
		`To: ` + imapAcctB,
	}, "our reply body")))

	sink := google.NewPGSink(pool)
	cfg := google.Config{Now: now, Backfill: google.DefaultBackfill, MaxMessageBytes: google.DefaultMaxMessageBytes}

	if _, err := google.IngestIMAP(ctx, srcA, sink, google.Account{ID: aID, Email: imapAcctA}, cfg); err != nil {
		t.Fatalf("IngestIMAP(A): %v", err)
	}
	if _, err := google.IngestIMAP(ctx, srcB, sink, google.Account{ID: bID, Email: imapAcctB}, cfg); err != nil {
		t.Fatalf("IngestIMAP(B): %v", err)
	}

	// ---- criterion 5: raw-first ------------------------------------------------
	scoped := `SELECT count(*) FROM raw_source_items WHERE source_account_id IN ($1,$2)`
	if got := scanInt(t, ctx, pool, scoped, aID, bID); got != 5 {
		t.Errorf("raw rows = %d, want 5 (raw is NOT deduped — per-account/per-folder capture, invariant 1)", got)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items WHERE source_account_id IN ($1,$2) AND normalized_at IS NULL`, aID, bID); got != 5 {
		t.Errorf("pending raw after ingest = %d, want 5 (raw lands BEFORE any parsing)", got)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items WHERE source_account_id IN ($1,$2) AND (content_hash IS NULL OR content_hash='')`, aID, bID); got != 0 {
		t.Errorf("%d raw rows missing content_hash", got)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items WHERE source_account_id=$1 AND external_id='imap:INBOX:12:1'`, aID); got != 1 {
		t.Errorf("external_id imap:{folder}:{uidvalidity}:{uid} not written for A/INBOX uid 1")
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items WHERE source_account_id=$1 AND raw_json->>'rfc822_b64' IS NOT NULL AND raw_json->>'source'='imap'`, aID); got != 3 {
		t.Errorf("A's raw rows do not all carry the base64 imap envelope")
	}
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM normalized_messages WHERE external_message_id=$1`, imapSharedMsgID); got != 0 {
		t.Errorf("normalization happened during ingest; it is a separate phase")
	}
	// Criterion 19: one sync_runs row per account per pass, phase 'imap'.
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM sync_runs WHERE source_account_id IN ($1,$2) AND stats->>'phase'='imap' AND status='ok'`, aID, bID); got != 2 {
		t.Errorf("ok sync_runs rows (phase imap) = %d, want 2 (one per account per pass)", got)
	}

	// ---- criteria 10, 11: normalize + dedup ------------------------------------
	nstats, err := google.Normalize(ctx, sink, google.Config{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	rawIn := `raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ($1,$2))`
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM normalized_messages WHERE `+rawIn, aID, bID); got != 3 {
		t.Errorf("normalized_messages = %d, want 3 (5 raw copies, 3 distinct Message-IDs)", got)
	}
	if nstats.DedupSkipped != 2 {
		t.Errorf("stats.DedupSkipped = %d, want 2 (the shared inbound and our own send's second copy)", nstats.DedupSkipped)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items WHERE source_account_id IN ($1,$2) AND normalized_at IS NULL`, aID, bID); got != 0 {
		t.Errorf("pending raw after normalize = %d, want 0 (losers are still stamped)", got)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_messages WHERE external_message_id=$1 AND direction='outbound' AND channel='gmail'`, imapOwnMsgID); got != 1 {
		t.Errorf("our own send: outbound gmail-channel rows = %d, want 1 (invariant 5)", got)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_messages WHERE external_message_id=$1 AND direction='inbound'`, imapOwnMsgID); got != 0 {
		t.Errorf("our own send has an inbound copy; it could be re-triaged into a new task")
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_messages WHERE external_message_id=$1 AND direction='inbound'`, imapSharedMsgID); got != 1 {
		t.Errorf("shared inbound rows = %d, want exactly 1 inbound", got)
	}
	// thread_key keeps the gmail:{email}:{root} shape splitGmailThreadKey parses.
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_threads WHERE thread_key LIKE 'gmail:itest-imap-%:<%'`); got < 1 {
		t.Errorf("no thread_key of the shape gmail:{account_email}:{root Message-ID}")
	}

	// ---- criterion 15: loop closure, both matchers -----------------------------
	var confirmedPrimary *string
	if err := pool.QueryRow(ctx, `SELECT confirmed_at::text FROM deliveries WHERE id=$1`, deliveryPrimary).Scan(&confirmedPrimary); err != nil {
		t.Fatalf("read primary delivery: %v", err)
	}
	if confirmedPrimary == nil {
		t.Errorf("primary matcher did not fire: the Sent copy's Message-ID equals sent_external_id")
	}
	var confirmedBelt, beltExternalID *string
	if err := pool.QueryRow(ctx,
		`SELECT confirmed_at::text, sent_external_id FROM deliveries WHERE id=$1`, deliveryBelt).Scan(&confirmedBelt, &beltExternalID); err != nil {
		t.Fatalf("read belt delivery: %v", err)
	}
	if confirmedBelt == nil {
		t.Errorf("body-prefix belt did not fire for a delivery whose Message-ID the relay rewrote")
	}
	if beltExternalID == nil || *beltExternalID != imapReservedMsgID {
		t.Errorf("sent_external_id = %v, want the RESERVED %q untouched (it is the invariant-4 idempotency token)",
			beltExternalID, imapReservedMsgID)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_confirmed'`, taskID); got != 2 {
		t.Errorf("delivery_confirmed events = %d, want 2 (one per confirmed delivery)", got)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_confirmed' AND payload::text LIKE '%'||$2||'%'`,
		taskID, imapRewrittenMsgID); got != 1 {
		t.Errorf("the belt's delivery_confirmed payload does not carry the OBSERVED Message-ID %q", imapRewrittenMsgID)
	}

	// ---- criterion 19: zero tasks, zero deliveries created ---------------------
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM tasks`); got != tasksBefore {
		t.Errorf("tasks changed: before=%d after=%d (ingest + normalize create zero tasks)", tasksBefore, got)
	}
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM deliveries`); got != deliveriesBefore {
		t.Errorf("deliveries changed: before=%d after=%d", deliveriesBefore, got)
	}

	// ---- criterion 8: an immediate second pass is a no-op ----------------------
	statsA, err := google.IngestIMAP(ctx, srcA, sink, google.Account{ID: aID, Email: imapAcctA}, cfg)
	if err != nil {
		t.Fatalf("second IngestIMAP(A): %v", err)
	}
	if statsA.RawInserted != 0 {
		t.Errorf("second pass RawInserted = %d, want 0 (cursor + content_hash short-circuit)", statsA.RawInserted)
	}
	if got := scanInt(t, ctx, pool, scoped, aID, bID); got != 5 {
		t.Errorf("raw rows after a second pass = %d, want 5", got)
	}

	// ---- criterion 8: a UIDVALIDITY change forces a folder resync --------------
	srcA.folders[0].UIDValidity = 13
	if _, err := google.IngestIMAP(ctx, srcA, sink, google.Account{ID: aID, Email: imapAcctA}, cfg); err != nil {
		t.Fatalf("IngestIMAP(A) after UIDVALIDITY bump: %v", err)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items WHERE source_account_id=$1 AND external_id='imap:INBOX:13:1'`, aID); got != 1 {
		t.Errorf("no raw row under the new UIDVALIDITY; the folder was not resynced")
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items WHERE source_account_id=$1 AND external_id='imap:INBOX:12:1'`, aID); got != 1 {
		t.Errorf("the pre-rotation raw row was destroyed; old raw rows are kept as history")
	}
	if _, err := google.Normalize(ctx, sink, google.Config{}); err != nil {
		t.Fatalf("Normalize after resync: %v", err)
	}
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM normalized_messages WHERE `+rawIn, aID, bID); got != 3 {
		t.Errorf("normalized_messages after resync = %d, want still 3 (Message-ID dedup collapses the duplicates)", got)
	}

	// ---- criterion 5: --normalize-only --all with the source unreachable -------
	fetchesBefore := len(srcA.fetches) + len(srcB.fetches)
	srcA.unreachable, srcB.unreachable = true, true
	if _, err := pool.Exec(ctx,
		`UPDATE raw_source_items SET normalized_at=NULL WHERE source_account_id IN ($1,$2)`, aID, bID); err != nil {
		t.Fatalf("reset normalized_at: %v", err)
	}
	if _, err := google.Normalize(ctx, sink, google.Config{All: true}); err != nil {
		t.Fatalf("Normalize(--all) with no IMAP connection: %v", err)
	}
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM normalized_messages WHERE `+rawIn, aID, bID); got != 3 {
		t.Errorf("normalized_messages after a full rebuild from raw = %d, want 3", got)
	}
	if got := len(srcA.fetches) + len(srcB.fetches); got != fetchesBefore {
		t.Errorf("the normalize phase touched the IMAP source (%d -> %d fetches); it must read raw_json alone", fetchesBefore, got)
	}
	// Replaying normalize must not double-emit the confirmation events.
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_confirmed'`, taskID); got != 2 {
		t.Errorf("delivery_confirmed events after a replay = %d, want still 2 (confirmed_at IS NULL guard)", got)
	}
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM tasks`); got != tasksBefore {
		t.Errorf("tasks changed after the replay: before=%d after=%d", tasksBefore, got)
	}
}
