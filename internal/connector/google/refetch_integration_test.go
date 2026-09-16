//go:build integration

package google_test

// Integration suite for the targeted IMAP re-fetch (SPEC mail-refetch-targeted /
// SWT-64, test plan I1-I6; acceptance criteria 2, 4, 5, 6, 12, 13, 14, 15, 16,
// 18). Build-tagged `integration` AND env-gated on DATABASE_URL. The mail server
// is the in-memory fakeIMAP (fake_imap_test.go) — NEVER a live IMAP connection,
// never a live send, never the broker.
//
// Run against an ISOLATED database, never the shared compose `ops` and never
// production (IK landmine 2026-09-12; SPEC "Sibling patterns to copy"):
//
//	psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c "CREATE DATABASE ops_mailrefetch"
//	make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_mailrefetch?sslmode=disable'
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_mailrefetch?sslmode=disable' TZ=UTC \
//	  go test -tags integration -p 1 -count=1 -run Refetch ./internal/connector/google/
//
// I1 is the column-level test the SPEC requires (mutation M2): the predicate's
// inputs — folder, uidvalidity, uid — come from raw_json, so the regression test
// belongs here and nowhere else (IK: "test the column, not the fixture"). The
// fixture is built so that NO literal satisfies it: the two rows deliberately
// disagree on folder AND on uidvalidity AND on uid.
//
// GREENFIELD NOTE: internal/connector/google/refetch.go does not exist yet, so
// this compile-FAILs under -tags integration with "undefined: google.Refetch*"
// until it does — the expected failure mode. The imposed surface is documented
// at the top of refetch_test.go.
//
// Cross-suite discipline (SWT-6 mutual-cleanup pact): this suite's accounts are
// provider='google' with test-scoped emails 'itest-mailrefetch-%'; every count
// assertion is scoped to them; cleanup runs in FK order, rerunnably, before AND
// after each test. Its INBOUND normalized_messages are visible to triage's
// GLOBAL pending filter, so cleanupTriage (internal/triage/integration_test.go)
// carries matching 'itest-mailrefetch-%' deletes — the pact-join obligation on
// the triage side.
//
// No client content anywhere below: every address, subject and body is a
// PLACEHOLDER.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/chash"
	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	mrAcctA  = "itest-mailrefetch-a@example.com"
	mrSlug   = "itest-mailrefetch-proj"
	mrKey    = "itest-mailrefetch-token-key"
	mrClient = "@itest-mailrefetch-client.example"
	mrOther  = "@itest-mailrefetch-other.example"
	mrSent   = "[Gmail]/Sent Mail"
	// The stored cursor. It must be byte-identical after a live pass
	// (criterion 13): the refetch reads UIDs from raw_source_items and has no
	// business moving the always-on connector's incremental window.
	mrCursor = `{"imap_folders": {"INBOX": {"uidvalidity": 12, "uid_next": 88231}}}`
)

var mrNow = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

func cleanupMailRefetch(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-mailrefetch-%')`
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`
	const msgs = `(SELECT id FROM normalized_messages WHERE raw_source_item_id IN ` + raws + `)`
	const projs = `(SELECT id FROM projects WHERE slug LIKE 'itest-mailrefetch-%')`
	const tsks = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	stmts := []string{
		`DELETE FROM task_events WHERE task_id IN ` + tsks,
		`DELETE FROM deliveries WHERE task_id IN ` + tsks,
		// deliveries.from_account_id references source_accounts with no ON DELETE.
		// Every delivery this suite seeds also has a task in the project, so the
		// line above covers today — but a future test seeding a delivery whose
		// task sits outside the project would fail the source_accounts delete.
		`DELETE FROM deliveries WHERE from_account_id IN ` + accts,
		`DELETE FROM capture_decisions WHERE message_id IN ` + msgs + ` OR project_id IN ` + projs + ` OR task_id IN ` + tsks,
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_events WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'gmail:itest-mailrefetch-%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM projects WHERE slug LIKE 'itest-mailrefetch-%'`,
		`DELETE FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-mailrefetch-%'`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
}

// mrSetup opens the pool and leaves a clean, app-password account behind.
func mrSetup(t *testing.T) (context.Context, *pgxpool.Pool, int64) {
	t.Helper()
	requireCompose(t)
	ctx := context.Background()
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)

	cleanupMailRefetch(t, ctx, pool)
	t.Cleanup(func() { cleanupMailRefetch(t, context.Background(), pool) })

	hosts := google.MailHosts{IMAPHost: "imap.invalid", IMAPPort: 993, SMTPHost: "smtp.invalid", SMTPPort: 587}
	acctID, err := google.UpsertAppPasswordAccount(ctx, pool, mrAcctA, "placeholder app password", mrKey, hosts, false)
	if err != nil {
		t.Fatalf("UpsertAppPasswordAccount: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE source_accounts SET sync_cursor=$2::jsonb WHERE id=$1`, acctID, mrCursor); err != nil {
		t.Fatalf("seed sync_cursor: %v", err)
	}
	return ctx, pool, acctID
}

// mrMessage builds one RFC822 message. PLACEHOLDER content only.
func mrMessage(messageID, from, subject string, date time.Time, body string) []byte {
	return rfc822([]string{
		`Message-ID: ` + messageID,
		`Date: ` + date.Format(time.RFC1123Z),
		`From: ` + from,
		`To: placeholder-recipient@example.com`,
		`Subject: ` + subject,
	}, body)
}

// mrFullMessage is the same message as it sits on the server: a multipart whose
// attachment bytes the 1 MiB fetch-time cap dropped.
func mrFullMessage(messageID, from, subject string, date time.Time) []byte {
	return rfc822([]string{
		`Message-ID: ` + messageID,
		`Date: ` + date.Format(time.RFC1123Z),
		`From: ` + from,
		`To: placeholder-recipient@example.com`,
		`Subject: ` + subject,
		`Content-Type: multipart/mixed; boundary="ph-boundary"`,
	}, strings.Join([]string{
		`--ph-boundary`,
		`Content-Type: text/plain; charset=utf-8`,
		``,
		`PLACEHOLDER body text.`,
		`--ph-boundary`,
		`Content-Type: application/octet-stream; name="PLACEHOLDER-attachment.bin"`,
		`Content-Transfer-Encoding: base64`,
		``,
		`UExBQ0VIT0xERVItQVRUQUNITUVOVC1CWVRFUw==`,
		`--ph-boundary--`,
		``,
	}, "\r\n"))
}

// mrInsertRaw stores one already-ingested imap row exactly as the connector
// would have: the envelope buildIMAPEnvelope writes, hashed with chash.
func mrInsertRaw(t *testing.T, ctx context.Context, pool *pgxpool.Pool, acctID int64,
	folder string, uidValidity, uid uint32, at time.Time, msg []byte, truncated bool) int64 {
	t.Helper()
	raw := newIMAPEnvelope(folder, uidValidity, uid, at, nil, msg, truncated)
	hash, err := chash.ContentHash(raw)
	if err != nil {
		t.Fatalf("chash.ContentHash: %v", err)
	}
	extID := fmt.Sprintf("imap:%s:%d:%d", folder, uidValidity, uid)
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1,$2,$3,$4) RETURNING id`, acctID, extID, raw, hash).Scan(&id); err != nil {
		t.Fatalf("insert raw %s: %v", extID, err)
	}
	return id
}

func mrNormalize(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := google.Normalize(ctx, google.NewPGSink(pool), google.Config{}); err != nil {
		t.Fatalf("google.Normalize: %v", err)
	}
}

// mrScanInt reads one integer, so a column assertion can compare against
// Postgres's own answer rather than a constant that drifts from the fixture.
func mrScanInt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

func mrScanString(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return s
}

func mrAccount(id int64) google.Account {
	return google.Account{ID: id, Email: mrAcctA, AuthType: "app_password"}
}

// mrTargetFor selects exactly one target by raw id.
func mrTargetFor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rawID int64) []google.RefetchTarget {
	t.Helper()
	targets, err := google.SelectRefetchTargets(ctx, pool, google.RefetchQuery{RawIDs: []int64{rawID}, Limit: 1})
	if err != nil {
		t.Fatalf("SelectRefetchTargets(raw %d): %v", rawID, err)
	}
	if len(targets) != 1 {
		t.Fatalf("SelectRefetchTargets(raw %d) returned %d targets, want 1", rawID, len(targets))
	}
	return targets
}

// ---- I1: selection reads the COLUMNS (criteria 1, 2; mutation M2) -----------

func TestRefetch_Integration_SelectionReadsTheColumns(t *testing.T) {
	ctx, pool, acctID := mrSetup(t)

	// Three rows that disagree on EVERY coordinate a literal could stand in for:
	// folder, uidvalidity, uid, sender and sent_at.
	//   A  INBOX / 12 / 7001, client sender, newest  (truncated capture)
	//   B  Sent  / 34 / 5002, client sender, oldest
	//   C  INBOX / 12 / 7002, other sender,  middle
	atA, atB, atC := mrNow.Add(-24*time.Hour), mrNow.Add(-72*time.Hour), mrNow.Add(-48*time.Hour)
	rawA := mrInsertRaw(t, ctx, pool, acctID, "INBOX", 12, 7001, atA,
		mrMessage("<ph-a@placeholder.example>", "Placeholder Client <client"+mrClient+">", "PLACEHOLDER subject A", atA,
			"PLACEHOLDER body A\n\n[Attachments not stored: PLACEHOLDER.docx]"), true)
	rawB := mrInsertRaw(t, ctx, pool, acctID, mrSent, 34, 5002, atB,
		mrMessage("<ph-b@placeholder.example>", "Placeholder Client <client"+mrClient+">", "PLACEHOLDER subject B", atB,
			"PLACEHOLDER body B"), false)
	rawC := mrInsertRaw(t, ctx, pool, acctID, "INBOX", 12, 7002, atC,
		mrMessage("<ph-c@placeholder.example>", "Placeholder Other <other"+mrOther+">", "PLACEHOLDER subject C", atC,
			"PLACEHOLDER body C"), false)
	mrNormalize(t, ctx, pool)

	byRaw := func(targets []google.RefetchTarget) map[int64]google.RefetchTarget {
		m := map[int64]google.RefetchTarget{}
		for _, tg := range targets {
			m[tg.RawID] = tg
		}
		return m
	}

	t.Run("folder, uidvalidity and uid come out of raw_json", func(t *testing.T) {
		targets, err := google.SelectRefetchTargets(ctx, pool,
			google.RefetchQuery{RawIDs: []int64{rawA, rawB}, Limit: 10})
		if err != nil {
			t.Fatalf("SelectRefetchTargets: %v", err)
		}
		if len(targets) != 2 {
			t.Fatalf("got %d targets, want 2: %+v", len(targets), targets)
		}
		got := byRaw(targets)
		want := map[int64]struct {
			folder    string
			uidv, uid uint32
			truncated bool
		}{
			rawA: {"INBOX", 12, 7001, true},
			rawB: {mrSent, 34, 5002, false},
		}
		for rawID, w := range want {
			tg, ok := got[rawID]
			if !ok {
				t.Fatalf("raw %d missing from the selection: %+v", rawID, targets)
			}
			if tg.Folder != w.folder || tg.UIDValidity != w.uidv || tg.UID != w.uid {
				t.Errorf("raw %d selected as %s/%d/%d, want %s/%d/%d — folder, uidvalidity and uid must be read "+
					"out of raw_json for THIS row (mutation M2: a literal here makes one of these two rows wrong)",
					rawID, tg.Folder, tg.UIDValidity, tg.UID, w.folder, w.uidv, w.uid)
			}
			if wantExt := fmt.Sprintf("imap:%s:%d:%d", w.folder, w.uidv, w.uid); tg.ExternalID != wantExt {
				t.Errorf("raw %d external_id = %q, want %q", rawID, tg.ExternalID, wantExt)
			}
			if tg.Truncated != w.truncated {
				t.Errorf("raw %d truncated = %v, want %v (read from raw_json->>'truncated')", rawID, tg.Truncated, w.truncated)
			}
			if tg.StoredSize <= 0 {
				t.Errorf("raw %d StoredSize = %d, want the stored size from raw_json", rawID, tg.StoredSize)
			}
			// StoredB64Len feeds the BYTE FLOOR, so it must come from the column
			// for THIS row. Compared against Postgres's own octet_length rather
			// than a constant, so the assertion cannot drift from the fixture.
			// Mutation M15 (replace the octet_length select with a literal) must
			// turn this red — without it the floor is silently inert, because
			// every `newLen < 0` is false.
			wantB64 := mrScanInt(t, ctx, pool,
				`SELECT COALESCE(octet_length(raw_json->>'rfc822_b64'), 0) FROM raw_source_items WHERE id=$1`, rawID)
			if wantB64 <= 0 {
				t.Fatalf("fixture drift: raw %d stores no rfc822_b64, so the byte floor has nothing to compare", rawID)
			}
			if tg.StoredB64Len != wantB64 {
				t.Errorf("raw %d StoredB64Len = %d, want %d (octet_length of THIS row's rfc822_b64)",
					rawID, tg.StoredB64Len, wantB64)
			}
		}
		// The two rows must differ in stored length, or a literal could satisfy
		// both and the assertion above would certify nothing.
		if a, b := got[rawA].StoredB64Len, got[rawB].StoredB64Len; a == b {
			t.Errorf("both rows store %d b64 bytes; the fixture must make them differ for the column "+
				"assertion to mean anything", a)
		}
		for rawID := range want {
			if tg := got[rawID]; tg.AccountID != acctID || tg.AccountEmail != mrAcctA {
				t.Errorf("raw %d account = %d/%q, want %d/%q", rawID, tg.AccountID, tg.AccountEmail, acctID, mrAcctA)
			}
		}
	})

	t.Run("finder matches normalized_messages.sender", func(t *testing.T) {
		targets, err := google.SelectRefetchTargets(ctx, pool,
			google.RefetchQuery{From: mrClient, Limit: 10})
		if err != nil {
			t.Fatalf("SelectRefetchTargets: %v", err)
		}
		got := byRaw(targets)
		if _, ok := got[rawA]; !ok {
			t.Errorf("--from %q did not select raw %d (D7: the finder matches normalized_messages.sender)", mrClient, rawA)
		}
		if _, ok := got[rawB]; !ok {
			t.Errorf("--from %q did not select raw %d", mrClient, rawB)
		}
		if _, ok := got[rawC]; ok {
			t.Errorf("--from %q selected raw %d, whose sender is %q", mrClient, rawC, mrOther)
		}
	})

	t.Run("limit takes the N oldest by sent_at", func(t *testing.T) {
		// All three match; the two OLDEST are B (-72h) and C (-48h), which is
		// NOT the same as the two lowest raw ids (A, B).
		targets, err := google.SelectRefetchTargets(ctx, pool,
			google.RefetchQuery{From: "@itest-mailrefetch", Limit: 2})
		if err != nil {
			t.Fatalf("SelectRefetchTargets: %v", err)
		}
		if len(targets) != 2 {
			t.Fatalf("got %d targets, want exactly 2 (criterion 2: the limit is honoured exactly): %+v", len(targets), targets)
		}
		got := byRaw(targets)
		if _, ok := got[rawB]; !ok {
			t.Errorf("the oldest row (raw %d, -72h) is not in the limited selection: %+v", rawB, targets)
		}
		if _, ok := got[rawC]; !ok {
			t.Errorf("the second-oldest row (raw %d, -48h) is not in the limited selection: %+v", rawC, targets)
		}
		if _, ok := got[rawA]; ok {
			t.Errorf("the NEWEST row (raw %d, -24h) is in a --limit 2 selection; criterion 2 takes the N oldest by sent_at", rawA)
		}
	})

	t.Run("since narrows the window", func(t *testing.T) {
		targets, err := google.SelectRefetchTargets(ctx, pool,
			google.RefetchQuery{From: "@itest-mailrefetch", Since: mrNow.Add(-60 * time.Hour), Limit: 10})
		if err != nil {
			t.Fatalf("SelectRefetchTargets: %v", err)
		}
		got := byRaw(targets)
		if _, ok := got[rawB]; ok {
			t.Errorf("--since now-60h selected raw %d, sent at -72h", rawB)
		}
		if _, ok := got[rawA]; !ok {
			t.Errorf("--since now-60h did not select raw %d, sent at -24h", rawA)
		}
		if _, ok := got[rawC]; !ok {
			t.Errorf("--since now-60h did not select raw %d, sent at -48h", rawC)
		}
	})

	t.Run("an unbounded selection is refused", func(t *testing.T) {
		if _, err := google.SelectRefetchTargets(ctx, pool,
			google.RefetchQuery{From: "@itest-mailrefetch"}); err == nil {
			t.Errorf("SelectRefetchTargets with Limit 0 returned no error; criterion 2: this tool never runs unbounded")
		}
	})
}

// Criterion 4: a gmail:-shaped row named by --raw-id is refused BY NAME. Those
// rows never stored attachment bytes at all, so "re-fetch it" has no meaning
// there and a silent skip would read as "nothing to do".
func TestRefetch_Integration_RefusesNonIMAPRowNamedByRawID(t *testing.T) {
	ctx, pool, acctID := mrSetup(t)

	var gmailRawID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1, 'gmail:itest-mailrefetch-1', '{"id":"itest-mailrefetch-1"}'::jsonb, 'itest-mailrefetch-gmail-hash')
		 RETURNING id`, acctID).Scan(&gmailRawID); err != nil {
		t.Fatalf("seed gmail raw row: %v", err)
	}

	_, err := google.SelectRefetchTargets(ctx, pool,
		google.RefetchQuery{RawIDs: []int64{gmailRawID}, Limit: 1})
	if err == nil {
		t.Fatalf("SelectRefetchTargets accepted a gmail: row named by --raw-id; criterion 4 refuses it by name")
	}
	if !strings.Contains(err.Error(), "not an IMAP-sourced row") {
		t.Errorf("error = %q, want it to carry %q", err, "not an IMAP-sourced row")
	}
	if !strings.Contains(err.Error(), fmt.Sprint(gmailRawID)) {
		t.Errorf("error = %q, want it to name raw id %d", err, gmailRawID)
	}
}

// ---- I2: identity survives the refetch (criteria 14, 15; mutation M8) -------

// The load-bearing one. capture_decisions reference normalized_messages.id; if a
// refetch produced a SECOND normalized row (or moved the id), those decisions
// would be orphaned or duplicated and the message would re-enter the funnel as
// new work.
func TestRefetch_Integration_IdentitySurvivesRefetch(t *testing.T) {
	ctx, pool, acctID := mrSetup(t)

	const msgID = "<ph-identity@placeholder.example>"
	at := mrNow.Add(-8 * 24 * time.Hour)
	from := "Placeholder Client <client" + mrClient + ">"
	rawID := mrInsertRaw(t, ctx, pool, acctID, "INBOX", 12, 7001, at,
		mrMessage(msgID, from, "PLACEHOLDER subject", at,
			"PLACEHOLDER body\n\n[Attachments not stored: PLACEHOLDER.docx]"), true)
	mrNormalize(t, ctx, pool)

	msgRowID := int64(scanInt(t, ctx, pool, `SELECT id FROM normalized_messages WHERE raw_source_item_id=$1`, rawID))
	threadKey := mrScanString(t, ctx, pool,
		`SELECT t.thread_key FROM normalized_threads t
		   JOIN normalized_messages m ON m.thread_id=t.id WHERE m.raw_source_item_id=$1`, rawID)

	// A live capture decision filed against that message id — the shape the 80
	// Foundry messages are in.
	var projID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,'itest-mailrefetch-client','manual','dashboard','/tmp/itest','local_only') RETURNING id`,
		mrSlug).Scan(&projID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	var decisionID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO capture_decisions (message_id, raw_source_item_id, mode, action, project_id, reason)
		 VALUES ($1,$2,'live','attributed',$3,'itest-mailrefetch') RETURNING id`,
		msgRowID, rawID, projID).Scan(&decisionID); err != nil {
		t.Fatalf("seed capture decision: %v", err)
	}

	rawCountBefore := scanInt(t, ctx, pool, `SELECT count(*) FROM raw_source_items WHERE source_account_id=$1`, acctID)
	tasksBefore := scanInt(t, ctx, pool, `SELECT count(*) FROM tasks`)
	threadsBefore := scanInt(t, ctx, pool, `SELECT count(*) FROM normalized_threads WHERE thread_key=$1`, threadKey)

	// The server still holds the message, in full, at the same coordinates.
	src := newFakeIMAP()
	src.folders = []google.Folder{{Name: "INBOX", UIDValidity: 12}, {Name: mrSent, UIDValidity: 34, Sent: true}}
	src.add("INBOX", imapMsg(7001, at, nil, mrFullMessage(msgID, from, "PLACEHOLDER subject", at)))

	stats, err := google.RefetchMessages(ctx, src, google.NewPGSink(pool), mrAccount(acctID),
		mrTargetFor(t, ctx, pool, rawID), google.RefetchConfig{})
	if err != nil {
		t.Fatalf("RefetchMessages: %v", err)
	}
	if stats.RawUpdated != 1 {
		t.Fatalf("stats.RawUpdated = %d, want 1 (%+v)", stats.RawUpdated, stats)
	}

	// Criterion 12: the SAME row, upserted. Criterion 14: re-normalization is
	// queued, not performed.
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM raw_source_items WHERE source_account_id=$1`, acctID); got != rawCountBefore {
		t.Errorf("raw_source_items for the account = %d, want %d unchanged (criterion 12: no new raw row)", got, rawCountBefore)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items WHERE id=$1 AND normalized_at IS NULL AND superseded_at IS NULL`, rawID); got != 1 {
		t.Errorf("raw row %d is not pending re-normalization; criterion 14 wants normalized_at NULL and superseded_at NULL", rawID)
	}
	if got := mrScanString(t, ctx, pool, `SELECT raw_json->>'truncated' FROM raw_source_items WHERE id=$1`, rawID); got != "false" {
		t.Errorf("raw_json.truncated = %q after a 100 MiB refetch, want \"false\"", got)
	}
	storedB64 := mrScanString(t, ctx, pool, `SELECT raw_json->>'rfc822_b64' FROM raw_source_items WHERE id=$1`, rawID)
	storedBytes, err := base64.StdEncoding.DecodeString(storedB64)
	if err != nil {
		t.Fatalf("stored rfc822_b64 is not base64: %v", err)
	}
	if !bytes.Contains(storedBytes, []byte("PLACEHOLDER-attachment.bin")) {
		t.Errorf("the refetched row does not carry the attachment part; recovering those bytes is the point of the pass")
	}

	// The next connector pass re-normalizes it.
	mrNormalize(t, ctx, pool)

	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM normalized_messages WHERE raw_source_item_id=$1`, rawID); got != 1 {
		t.Fatalf("normalized_messages for raw %d = %d, want exactly 1 (mutation M8: upsertMessage's conflict target is raw_source_item_id)", rawID, got)
	}
	if got := int64(scanInt(t, ctx, pool, `SELECT id FROM normalized_messages WHERE raw_source_item_id=$1`, rawID)); got != msgRowID {
		t.Errorf("normalized_messages.id = %d, want %d UNCHANGED — capture_decisions reference this id", got, msgRowID)
	}
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM normalized_messages WHERE external_message_id=$1`, msgID); got != 1 {
		t.Errorf("normalized_messages carrying %s = %d, want 1 (no second row for the message)", msgID, got)
	}
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM normalized_threads WHERE thread_key=$1`, threadKey); got != threadsBefore {
		t.Errorf("normalized_threads for %q = %d, want %d unchanged", threadKey, got, threadsBefore)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM capture_decisions WHERE id=$1 AND message_id=$2`, decisionID, msgRowID); got != 1 {
		t.Errorf("capture decision %d no longer resolves to message %d (orphaned by the refetch)", decisionID, msgRowID)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM capture_decisions WHERE message_id=$1 AND mode='live'`, msgRowID); got != 1 {
		t.Errorf("live capture decisions for message %d = %d, want exactly 1 (no second decision, no re-capture)", msgRowID, got)
	}
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM tasks`); got != tasksBefore {
		t.Errorf("tasks = %d, want %d unchanged: a refetch creates no task (criterion 15, 19)", got, tasksBefore)
	}
}

// ---- I3: no new raw row, no cursor movement (criteria 12, 13; M5) -----------

func TestRefetch_Integration_NoNewRawRowAndCursorUnmoved(t *testing.T) {
	ctx, pool, acctID := mrSetup(t)

	const msgID = "<ph-cursor@placeholder.example>"
	at := mrNow.Add(-5 * 24 * time.Hour)
	from := "Placeholder Client <client" + mrClient + ">"
	rawID := mrInsertRaw(t, ctx, pool, acctID, "INBOX", 12, 7001, at,
		mrMessage(msgID, from, "PLACEHOLDER subject", at, "PLACEHOLDER body"), true)
	mrNormalize(t, ctx, pool)

	cursorBefore := mrScanString(t, ctx, pool, `SELECT sync_cursor::text FROM source_accounts WHERE id=$1`, acctID)
	rawsBefore := scanInt(t, ctx, pool, `SELECT count(*) FROM raw_source_items WHERE source_account_id=$1`, acctID)
	if cursorBefore == "" || !strings.Contains(cursorBefore, "88231") {
		t.Fatalf("fixture: sync_cursor is %q, want the seeded folder position (the test is worthless without one)", cursorBefore)
	}

	src := newFakeIMAP()
	src.folders = []google.Folder{{Name: "INBOX", UIDValidity: 12}}
	src.add("INBOX", imapMsg(7001, at, nil, mrFullMessage(msgID, from, "PLACEHOLDER subject", at)))

	if _, err := google.RefetchMessages(ctx, src, google.NewPGSink(pool), mrAccount(acctID),
		mrTargetFor(t, ctx, pool, rawID), google.RefetchConfig{}); err != nil {
		t.Fatalf("RefetchMessages: %v", err)
	}

	// Anti-vacuity: a pass that refused or skipped its target would leave the
	// cursor untouched for the wrong reason. Prove the write happened first.
	if got := mrScanString(t, ctx, pool, `SELECT raw_json->>'truncated' FROM raw_source_items WHERE id=$1`, rawID); got != "false" {
		t.Fatalf("raw %d was not actually refetched (truncated=%q); the cursor assertion below would pass vacuously", rawID, got)
	}

	cursorAfter := mrScanString(t, ctx, pool, `SELECT sync_cursor::text FROM source_accounts WHERE id=$1`, acctID)
	if cursorAfter != cursorBefore {
		t.Errorf("sync_cursor changed:\n before %s\n after  %s\ncriterion 13: byte-identical before and after a live run", cursorBefore, cursorAfter)
	}
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM raw_source_items WHERE source_account_id=$1`, acctID); got != rawsBefore {
		t.Errorf("raw_source_items = %d, want %d unchanged", got, rawsBefore)
	}
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM raw_source_items WHERE id=$1`, rawID); got != 1 {
		t.Errorf("the target raw row %d no longer exists; the refetch upserts it in place", rawID)
	}
}

// ---- I4: the sync_runs row is its own phase (criterion 18) ------------------

func TestRefetch_Integration_SyncRunPhaseIsIMAPRefetch(t *testing.T) {
	ctx, pool, acctID := mrSetup(t)

	const msgID = "<ph-run@placeholder.example>"
	at := mrNow.Add(-3 * 24 * time.Hour)
	from := "Placeholder Client <client" + mrClient + ">"
	rawID := mrInsertRaw(t, ctx, pool, acctID, "INBOX", 12, 7001, at,
		mrMessage(msgID, from, "PLACEHOLDER subject", at, "PLACEHOLDER body"), true)
	mrNormalize(t, ctx, pool)

	src := newFakeIMAP()
	src.folders = []google.Folder{{Name: "INBOX", UIDValidity: 12}}
	src.add("INBOX", imapMsg(7001, at, nil, mrFullMessage(msgID, from, "PLACEHOLDER subject", at)))

	if _, err := google.RefetchMessages(ctx, src, google.NewPGSink(pool), mrAccount(acctID),
		mrTargetFor(t, ctx, pool, rawID), google.RefetchConfig{}); err != nil {
		t.Fatalf("RefetchMessages: %v", err)
	}

	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM sync_runs WHERE source_account_id=$1 AND status='ok'
		   AND stats->>'phase'='imap_refetch' AND finished_at IS NOT NULL`, acctID); got != 1 {
		t.Errorf("sync_runs rows with phase imap_refetch and status ok = %d, want exactly 1 (criterion 18)", got)
	}
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM sync_runs WHERE source_account_id=$1`, acctID); got != 1 {
		t.Errorf("sync_runs for the account = %d, want exactly 1 per live run", got)
	}
	// The phase value is load-bearing elsewhere: availability keys calendar
	// readiness on stats->>'phase' = 'calendar' (internal/availability/store.go:125),
	// so a refetch run must never present as one.
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM sync_runs WHERE source_account_id=$1 AND status='ok' AND stats->>'phase'='calendar'`, acctID); got != 0 {
		t.Errorf("the refetch wrote %d run(s) reading as phase 'calendar'; that would move calendar readiness", got)
	}
	var stats map[string]any
	rawStats := mrScanString(t, ctx, pool,
		`SELECT stats::text FROM sync_runs WHERE source_account_id=$1 AND stats->>'phase'='imap_refetch'`, acctID)
	if err := json.Unmarshal([]byte(rawStats), &stats); err != nil {
		t.Fatalf("sync_runs.stats is not json: %v", err)
	}
	for _, key := range []string{"imap_fetched", "raw_updated", "raw_unchanged"} {
		if _, ok := stats[key]; !ok {
			t.Errorf("sync_runs.stats is missing %q; criterion 18 carries it: %s", key, rawStats)
		}
	}
}

// ---- I5: a refetched Sent copy must not re-confirm (criterion 16) -----------

// The Sent-folder hazard. A refetched outbound row re-runs loop closure on
// re-normalization; the confirmed_at IS NULL guards are what make that a no-op.
// Without them one real send confirms twice and emits a second
// delivery_confirmed event, which R8 reads as a second delivery.
func TestRefetch_Integration_OutboundRefetchDoesNotDoubleConfirm(t *testing.T) {
	ctx, pool, acctID := mrSetup(t)

	const outMsgID = "<sb-itest-mailrefetch-out-1@example.com>"
	at := mrNow.Add(-2 * 24 * time.Hour)
	from := "Placeholder Sender <" + mrAcctA + ">" // our own account => outbound

	var projID, taskID, deliveryID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,'itest-mailrefetch-client','manual','dashboard','/tmp/itest','local_only') RETURNING id`,
		mrSlug).Scan(&projID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'itest-mailrefetch work','claude','delivered') RETURNING id`, projID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, status, body, sent_external_id, from_account_id, sent_at, send_attempted_at)
		 VALUES ($1,'gmail','sent','PLACEHOLDER outbound body',$2,$3,$4,$4) RETURNING id`,
		taskID, outMsgID, acctID, at).Scan(&deliveryID); err != nil {
		t.Fatalf("seed delivery: %v", err)
	}

	rawID := mrInsertRaw(t, ctx, pool, acctID, mrSent, 34, 5002, at,
		mrMessage(outMsgID, from, "PLACEHOLDER subject", at, "PLACEHOLDER outbound body"), true)

	// First normalization closes the loop, exactly as the connector does today.
	mrNormalize(t, ctx, pool)
	if got := mrScanString(t, ctx, pool, `SELECT direction FROM normalized_messages WHERE raw_source_item_id=$1`, rawID); got != "outbound" {
		t.Fatalf("fixture: the Sent copy normalized %q, want outbound (the whole criterion rests on it)", got)
	}
	confirmedBefore := mrScanString(t, ctx, pool, `SELECT COALESCE(confirmed_at::text,'') FROM deliveries WHERE id=$1`, deliveryID)
	if confirmedBefore == "" {
		t.Fatalf("fixture: delivery %d was not confirmed by the first normalize; the idempotence test needs a confirmed row", deliveryID)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_confirmed'`, taskID); got != 1 {
		t.Fatalf("fixture: delivery_confirmed events after the first normalize = %d, want 1", got)
	}

	// Now refetch that Sent row at the raised cap and re-normalize it.
	src := newFakeIMAP()
	src.folders = []google.Folder{{Name: "INBOX", UIDValidity: 12}, {Name: mrSent, UIDValidity: 34, Sent: true}}
	src.add(mrSent, imapMsg(5002, at, nil, mrFullMessage(outMsgID, from, "PLACEHOLDER subject", at)))

	stats, err := google.RefetchMessages(ctx, src, google.NewPGSink(pool), mrAccount(acctID),
		mrTargetFor(t, ctx, pool, rawID), google.RefetchConfig{})
	if err != nil {
		t.Fatalf("RefetchMessages: %v", err)
	}
	// Anti-vacuity: "no second confirmation" is trivially true if the Sent row
	// was never refetched, so the re-normalization below must have something to
	// chew on.
	if stats.RawUpdated != 1 {
		t.Fatalf("stats.RawUpdated = %d, want 1 (%+v); without a real refetch the idempotence assertions below prove nothing", stats.RawUpdated, stats)
	}
	mrNormalize(t, ctx, pool)

	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_confirmed'`, taskID); got != 1 {
		t.Errorf("delivery_confirmed events after refetch + re-normalize = %d, want 1 (criterion 16: the confirmed_at IS NULL guards make it idempotent)", got)
	}
	if got := mrScanString(t, ctx, pool, `SELECT COALESCE(confirmed_at::text,'') FROM deliveries WHERE id=$1`, deliveryID); got != confirmedBefore {
		t.Errorf("deliveries.confirmed_at moved from %q to %q; a re-fetch must not re-confirm a closed loop", confirmedBefore, got)
	}
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM deliveries WHERE sent_external_id=$1`, outMsgID); got != 1 {
		t.Errorf("deliveries carrying %s = %d, want 1 (no second delivery row)", outMsgID, got)
	}
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM tasks WHERE project_id=$1`, projID); got != 1 {
		t.Errorf("tasks for the project = %d, want 1: our own send is never re-triaged (invariant 5)", got)
	}
}

// ---- I6: --dry-run writes nothing (criterion 5; mutation M4) ----------------

func TestRefetch_Integration_DryRunWritesNothing(t *testing.T) {
	ctx, pool, acctID := mrSetup(t)

	const msgID = "<ph-dryrun@placeholder.example>"
	at := mrNow.Add(-6 * 24 * time.Hour)
	from := "Placeholder Client <client" + mrClient + ">"
	rawID := mrInsertRaw(t, ctx, pool, acctID, "INBOX", 12, 7001, at,
		mrMessage(msgID, from, "PLACEHOLDER subject", at, "PLACEHOLDER body"), true)
	mrNormalize(t, ctx, pool)

	rawBefore := mrScanString(t, ctx, pool,
		`SELECT raw_json::text || '|' || content_hash || '|' || COALESCE(normalized_at::text,'') FROM raw_source_items WHERE id=$1`, rawID)
	rawsBefore := scanInt(t, ctx, pool, `SELECT count(*) FROM raw_source_items WHERE source_account_id=$1`, acctID)
	runsBefore := scanInt(t, ctx, pool, `SELECT count(*) FROM sync_runs WHERE source_account_id=$1`, acctID)
	cursorBefore := mrScanString(t, ctx, pool, `SELECT sync_cursor::text FROM source_accounts WHERE id=$1`, acctID)

	src := newFakeIMAP()
	src.folders = []google.Folder{{Name: "INBOX", UIDValidity: 12}}
	src.add("INBOX", imapMsg(7001, at, nil, mrFullMessage(msgID, from, "PLACEHOLDER subject", at)))

	var out bytes.Buffer
	if _, err := google.RefetchMessages(ctx, src, google.NewPGSink(pool), mrAccount(acctID),
		mrTargetFor(t, ctx, pool, rawID), google.RefetchConfig{DryRun: true, Out: &out}); err != nil {
		t.Fatalf("RefetchMessages(dry run): %v", err)
	}

	if got := mrScanString(t, ctx, pool,
		`SELECT raw_json::text || '|' || content_hash || '|' || COALESCE(normalized_at::text,'') FROM raw_source_items WHERE id=$1`, rawID); got != rawBefore {
		t.Errorf("the dry run modified raw row %d (criterion 5: it writes NOTHING)\n before %.120s\n after  %.120s", rawID, rawBefore, got)
	}
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM raw_source_items WHERE source_account_id=$1`, acctID); got != rawsBefore {
		t.Errorf("raw_source_items = %d, want %d unchanged after a dry run", got, rawsBefore)
	}
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM sync_runs WHERE source_account_id=$1`, acctID); got != runsBefore {
		t.Errorf("sync_runs = %d, want %d unchanged: a dry run writes no run row", got, runsBefore)
	}
	if got := mrScanString(t, ctx, pool, `SELECT sync_cursor::text FROM source_accounts WHERE id=$1`, acctID); got != cursorBefore {
		t.Errorf("sync_cursor changed during a dry run")
	}
	if len(src.fetches) != 0 {
		t.Errorf("the dry run issued a FETCH: %+v (D5: it contacts IMAP for the live UIDVALIDITY only)", src.fetches)
	}
	if !strings.Contains(out.String(), fmt.Sprint(rawID)) {
		t.Errorf("the dry-run plan does not name raw id %d; got:\n%s", rawID, out.String())
	}
}
