//go:build integration

package google_test

// Integration test for the Google connector end-to-end (SPEC
// 07-google-oauth-pollers, verification protocol step 2; acceptance criteria
// 4, 6, 7, 8, 9, 10, 11, 12, 13). Build-tagged `integration` AND env-gated on
// DATABASE_URL; uses the httptest fakeGoogle (fake_google_test.go) — NEVER a
// live Google call. Run with:
//
//   DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//     go test -tags integration ./internal/connector/google/
//
// Cross-suite discipline (SWT-6 mutual-cleanup pact): this suite's synthetic
// accounts are provider='google' (production value — so the criterion-9 partial
// unique index is exercised for real) with test-scoped emails
// 'itest-google-conn-%'. ALL count assertions are scoped to those account ids —
// NO global counts — so this suite needs no foreign-corpus cleanup for its own
// correctness. But its inbound normalized_messages ARE visible to triage's
// GLOBAL pending filter, so cleanupTriage (internal/triage/integration_test.go)
// gains matching 'itest-google-%' deletes — the pact-join obligation on the
// triage side (this file only cleans its OWN corpus, in FK order, rerunnably).
//
// GREENFIELD NOTE: under `-tags integration` this compile-FAILs until the google
// connector, PGSink, Run/Normalize, the availability tool, and migration 0005
// exist — the expected failure mode. Imposed exported surface beyond the poller
// unit surface:
//
//   func NewPGSink(pool *pgxpool.Pool) *PGSink   // implements the ingest Sink AND the normalize/list store
//   func (*PGSink) ListAccounts(ctx context.Context) ([]Account, error) // provider='google' rows
//
//   type Clients       struct { Gmail *GmailClient; Calendar *CalendarClient }
//   type ClientFactory func(ctx context.Context, acct Account) (Clients, error)
//
//   // Run ingests (raw-first) every provider='google' account: gmail phase then
//   // calendar phase, per account. It errors if no accounts exist. No normalize.
//   func Run(ctx context.Context, sink *PGSink, factory ClientFactory, cfg Config) (Stats, error)
//   // Normalize is the second phase: reads ONLY raw_source_items, loads the
//   // own-email set (all provider='google' account_emails) once, dedups gmail by
//   // Message-ID (partial unique index + SELECT-first), upserts events on
//   // raw_source_item_id, stamps every processed raw normalized_at.
//   func Normalize(ctx context.Context, sink *PGSink, cfg Config) (Stats, error)

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	connA = "itest-google-conn-a@example.com"
	connB = "itest-google-conn-b@example.com"

	// internalDate (unix ms) — all within a 90d backfill of the test's Now.
	dtShared = int64(1751364000000) // 2026-07-01T10:00:00Z
	dtAtoB   = int64(1751367600000)
	dtAonly  = int64(1751371200000)
	dtBonly  = int64(1751374800000)

	msgidShared = "<shared-inbound@mail.example>"
	msgidAtoB   = "<a-to-b@mail.example>"
	msgidAonly  = "<a-only@mail.example>"
	msgidBonly  = "<b-only@mail.example>"
)

func scanInt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

func insAccount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, email string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, refresh_token_encrypted, scopes, send_enabled, calendar_in_availability)
		 VALUES ('google', $1, pgp_sym_encrypt('dummy','k'), $2, false, true) RETURNING id`,
		email, google.ReadonlyScopes).Scan(&id); err != nil {
		t.Fatalf("insert account %s: %v", email, err)
	}
	return id
}

// cleanupGoogleConn removes THIS suite's corpus in FK order so it is rerunnable
// against the persistent compose db.
func cleanupGoogleConn(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	stmts := []string{
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-google-conn-%'))`,
		`DELETE FROM normalized_events   WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-google-conn-%'))`,
		`DELETE FROM normalized_threads  WHERE thread_key LIKE 'gmail:itest-google-conn-%'`,
		`DELETE FROM raw_source_items    WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-google-conn-%')`,
		`DELETE FROM sync_runs           WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-google-conn-%')`,
		`DELETE FROM source_accounts     WHERE provider='google' AND account_email LIKE 'itest-google-conn-%'`,
		`DELETE FROM policy_decisions    WHERE audit_event_id IN (SELECT id FROM audit_events WHERE actor='itest-google')`,
		`DELETE FROM audit_events        WHERE actor='itest-google'`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
}

func TestGoogle_Integration_EndToEnd(t *testing.T) {
	requireCompose(t)
	ctx := context.Background()

	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	defer pool.Close()

	// ---- Migration 0005 artifacts (criteria 9, 10) -------------------------
	for _, col := range []string{"title", "status", "transparency", "all_day"} {
		if got := scanInt(t, ctx, pool,
			`SELECT count(*) FROM information_schema.columns WHERE table_name='normalized_events' AND column_name=$1`, col); got != 1 {
			t.Fatalf("normalized_events.%s missing — apply migration 0005 (make migrate)", col)
		}
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM pg_indexes WHERE indexname='normalized_messages_gmail_msgid_idx'`); got != 1 {
		t.Fatalf("partial unique index normalized_messages_gmail_msgid_idx missing — apply migration 0005")
	}

	cleanupGoogleConn(t, ctx, pool)
	defer cleanupGoogleConn(t, ctx, pool)

	aID := insAccount(t, ctx, pool, connA)
	bID := insAccount(t, ctx, pool, connB)

	// ---- Fake Google corpus ------------------------------------------------
	fg := newFakeGoogle()
	defer fg.close()

	// Shared inbound (same Message-ID delivered to BOTH mailboxes): distinct
	// gmailMessageIds, one RFC Message-ID — the cross-account dedup case.
	fg.addGmail(connA, fakeGmailMsg{id: "a-shared", threadID: "ta1", full: gmailFull("a-shared", "ta1", msgidShared, "stranger@world.example", connA, dtShared, "shared body")})
	fg.addGmail(connB, fakeGmailMsg{id: "b-shared", threadID: "tb1", full: gmailFull("b-shared", "tb1", msgidShared, "stranger@world.example", connB, dtShared, "shared body")})
	// A -> B: From is account A (our own send); lands in both mailboxes; must be
	// OUTBOUND in the winning copy and never triageable inbound (invariant 5).
	fg.addGmail(connA, fakeGmailMsg{id: "a-sent", threadID: "ta2", full: gmailFull("a-sent", "ta2", msgidAtoB, connA, connB, dtAtoB, "our reply")})
	fg.addGmail(connB, fakeGmailMsg{id: "b-recv", threadID: "tb2", full: gmailFull("b-recv", "tb2", msgidAtoB, connA, connB, dtAtoB, "our reply")})
	// Per-account uniques.
	fg.addGmail(connA, fakeGmailMsg{id: "a-only", threadID: "ta3", full: gmailFull("a-only", "ta3", msgidAonly, "client@acme.example", connA, dtAonly, "a only")})
	fg.addGmail(connB, fakeGmailMsg{id: "b-only", threadID: "tb3", full: gmailFull("b-only", "tb3", msgidBonly, "client@beta.example", connB, dtBonly, "b only")})

	// Calendar: one busy event per account (both in the availability window).
	fg.addCalendar(connA, calFull("evt-a", "Standup A", "2026-07-13T09:00:00+02:00", "2026-07-13T09:30:00+02:00"))
	fg.addCalendar(connB, calFull("evt-b", "Standup B", "2026-07-13T11:00:00+02:00", "2026-07-13T11:30:00+02:00"))

	factory := func(_ context.Context, a google.Account) (google.Clients, error) {
		return google.Clients{
			Gmail:    google.NewGmailClient(http.DefaultClient, fg.url(), a.Email),
			Calendar: google.NewCalendarClient(userHTTPClient(a.Email), fg.url()),
		}, nil
	}

	cfg := google.Config{
		Overlap:  time.Hour,
		Backfill: 90 * 24 * time.Hour,
		Now:      time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC),
	}

	// Snapshots for criterion 13 (zero tasks/deliveries).
	tasksBefore := scanInt(t, ctx, pool, `SELECT count(*) FROM tasks`)
	deliveriesBefore := scanInt(t, ctx, pool, `SELECT count(*) FROM deliveries`)

	sink := google.NewPGSink(pool)

	// ---- Ingest (raw-first) ------------------------------------------------
	if _, err := google.Run(ctx, sink, factory, cfg); err != nil {
		t.Fatalf("Run (ingest): %v", err)
	}

	scoped := `SELECT count(*) FROM raw_source_items WHERE source_account_id IN ($1,$2)`
	if got := scanInt(t, ctx, pool, scoped, aID, bID); got != 8 {
		t.Errorf("raw rows = %d, want 8 (6 gmail + 2 calendar; raw is NOT deduped — invariant 1)", got)
	}
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM raw_source_items WHERE source_account_id=$1`, aID); got != 4 {
		t.Errorf("account A raw rows = %d, want 4", got)
	}
	// Criterion 4: raw-first — after ingest, before normalize, all pending.
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items WHERE source_account_id IN ($1,$2) AND normalized_at IS NULL`, aID, bID); got != 8 {
		t.Errorf("pending raw after ingest = %d, want 8 (raw lands before normalize)", got)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items WHERE source_account_id IN ($1,$2) AND (content_hash IS NULL OR content_hash='')`, aID, bID); got != 0 {
		t.Errorf("%d raw rows missing content_hash", got)
	}

	// ---- Normalize (dedup + direction) -------------------------------------
	nstats, err := google.Normalize(ctx, sink, google.Config{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	rawIn := `raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ($1,$2))`
	// Criterion 9: 4 distinct Message-IDs -> 4 normalized_messages (two shared
	// pairs collapsed); dedup_skipped counts the two losers.
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM normalized_messages WHERE `+rawIn, aID, bID); got != 4 {
		t.Errorf("normalized_messages = %d, want 4 (cross-account Message-ID dedup)", got)
	}
	if nstats.DedupSkipped != 2 {
		t.Errorf("stats.DedupSkipped = %d, want 2 (the two shared Message-IDs)", nstats.DedupSkipped)
	}
	// Criterion 9: losers still stamped normalized_at (nothing left pending).
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items WHERE source_account_id IN ($1,$2) AND normalized_at IS NULL`, aID, bID); got != 0 {
		t.Errorf("pending raw after normalize = %d, want 0 (losing raw item still stamped normalized)", got)
	}

	// Criterion 8 (invariant 5): the A->B message is OUTBOUND in the winning
	// copy and has ZERO inbound copies — it can never be re-triaged as inbound.
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_messages WHERE external_message_id=$1 AND direction='outbound'`, msgidAtoB); got != 1 {
		t.Errorf("A->B message: outbound rows = %d, want 1 (From is our account)", got)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_messages WHERE external_message_id=$1 AND direction='inbound'`, msgidAtoB); got != 0 {
		t.Errorf("A->B message: inbound rows = %d, want 0 (our own send is never inbound)", got)
	}
	// Shared inbound: exactly one row, inbound.
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_messages WHERE external_message_id=$1`, msgidShared); got != 1 {
		t.Errorf("shared inbound message: normalized rows = %d, want 1", got)
	}
	// Message-ID preserved verbatim (brackets kept) — the step-8 delivery seam.
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_messages WHERE external_message_id=$1`, msgidAonly); got != 1 {
		t.Errorf("verbatim Message-ID %q not stored", msgidAonly)
	}

	// Criterion 10: 2 calendar events normalized with the new columns.
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM normalized_events WHERE `+rawIn, aID, bID); got != 2 {
		t.Errorf("normalized_events = %d, want 2", got)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_events WHERE `+rawIn+` AND title IS NOT NULL AND status='confirmed' AND transparency='opaque' AND all_day=false`, aID, bID); got != 2 {
		t.Errorf("normalized events missing the 0005 columns (title/status/transparency/all_day)")
	}

	// Threads created per account (criterion 7): at least one per winning
	// message, both account prefixes represented.
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM normalized_threads WHERE thread_key LIKE 'gmail:itest-google-conn-%'`); got < 4 {
		t.Errorf("gmail threads = %d, want >= 4 (per-account thread_key gmail:{email}:{threadId})", got)
	}
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM normalized_threads WHERE thread_key LIKE 'gmail:'||$1||':%'`, connA); got < 1 {
		t.Errorf("no thread for account A")
	}
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM normalized_threads WHERE thread_key LIKE 'gmail:'||$1||':%'`, connB); got < 1 {
		t.Errorf("no thread for account B")
	}

	// ---- Second run: idempotent (criterion 4/9 stability) ------------------
	rstats, err := google.Run(ctx, sink, factory, cfg)
	if err != nil {
		t.Fatalf("Run (rerun): %v", err)
	}
	if rstats.RawInserted != 0 {
		t.Errorf("rerun RawInserted = %d, want 0 (content_hash short-circuit)", rstats.RawInserted)
	}
	if _, err := google.Normalize(ctx, sink, google.Config{}); err != nil {
		t.Fatalf("Normalize (rerun): %v", err)
	}
	if got := scanInt(t, ctx, pool, scoped, aID, bID); got != 8 {
		t.Errorf("raw rows after rerun = %d, want 8 (no new rows)", got)
	}
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM normalized_messages WHERE `+rawIn, aID, bID); got != 4 {
		t.Errorf("normalized_messages after rerun = %d, want 4 (no duplicates)", got)
	}

	// ---- propose_slots through the executor (criterion 12) -----------------
	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	ex := executor.New(reg, policy.NewStatic(reg.Names()...), audit.NewPGStore(pool))

	args := []byte(`{"duration_minutes":30,"window_start":"2026-07-13T09:00:00+02:00","window_end":"2026-07-13T12:00:00+02:00","count":3}`)
	res, err := ex.Execute(ctx, executor.Call{Tool: "propose_slots", Actor: "itest-google", Args: args})
	if err != nil {
		t.Fatalf("Execute propose_slots: %v", err)
	}

	var out struct {
		Slots []struct {
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"slots"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatalf("propose_slots output not {slots:[...]}: %v (%s)", err, res.Output)
	}
	if len(out.Slots) == 0 {
		t.Errorf("propose_slots returned no slots in a window with free time")
	}
	// The two busy events (09:00-09:30, 11:00-11:30 Rome) must be dodged.
	busyA0, _ := time.Parse(time.RFC3339, "2026-07-13T09:00:00+02:00")
	busyA1, _ := time.Parse(time.RFC3339, "2026-07-13T09:30:00+02:00")
	busyB0, _ := time.Parse(time.RFC3339, "2026-07-13T11:00:00+02:00")
	busyB1, _ := time.Parse(time.RFC3339, "2026-07-13T11:30:00+02:00")
	for _, s := range out.Slots {
		st, err1 := time.Parse(time.RFC3339, s.Start)
		en, err2 := time.Parse(time.RFC3339, s.End)
		if err1 != nil || err2 != nil {
			t.Errorf("slot times not RFC3339: %v / %v", s.Start, s.End)
			continue
		}
		if st.Before(busyA1) && en.After(busyA0) {
			t.Errorf("slot %s-%s overlaps busy 09:00-09:30", s.Start, s.End)
		}
		if st.Before(busyB1) && en.After(busyB0) {
			t.Errorf("slot %s-%s overlaps busy 11:00-11:30", s.Start, s.End)
		}
	}
	// Criterion 12 (invariant 3): the call left an audit row.
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM audit_events WHERE tool='propose_slots' AND actor='itest-google' AND status='ok'`); got < 1 {
		t.Errorf("no ok audit_events row for propose_slots (invariant 3: every tool call is audited)")
	}

	// ---- Criterion 13: zero tasks, zero deliveries -------------------------
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM tasks`); got != tasksBefore {
		t.Errorf("tasks changed: before=%d after=%d (connector/propose_slots create zero tasks)", tasksBefore, got)
	}
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM deliveries`); got != deliveriesBefore {
		t.Errorf("deliveries changed: before=%d after=%d", deliveriesBefore, got)
	}
}
