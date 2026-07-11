//go:build integration

package google_test

// Integration test for gmail own-message loop closure (SPEC 08-draft-deliveries,
// criterion 7 + invariant 5). Build-tagged `integration`, env-gated on
// DATABASE_URL, uses the shared httptest corpus helpers (gmailFull, scanInt) —
// NEVER a live Google call.
//
//   DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//     go test -tags integration -run LoopClosure ./internal/connector/google/
//
// GREENFIELD NOTE: the sink.upsertMessage loop-closure hook does not exist yet,
// so under `-tags integration` the confirmed_at / delivery_confirmed assertions
// fail until it is added — the expected failure mode. No new exported surface is
// imposed: the hook lives inside the existing PGSink.upsertMessage (SPEC "Files
// likely to touch").
//
// Cross-suite discipline: the seeded message is OUTBOUND (From is our own
// account), so triage's inbound-only pending filter never sees it — no pact-join
// needed. All assertions are scoped to this suite's corpus (itest-gcl-%).

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	gclEmail   = "itest-gcl-a@example.com"
	gclSlug    = "itest-gcl-proj"
	gclMsgID   = "<sb-itest-gcl-42-abc@example.com>" // = the delivery's sent_external_id
	gclRawExt  = "gmail:itest-gcl-msg-1"
	gclGThread = "itest-gcl-thread-1"
)

func cleanupGmailClosure(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM task_events WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{gclSlug}},
		{`DELETE FROM deliveries WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{gclSlug}},
		{`DELETE FROM normalized_messages WHERE external_message_id=$1`, []any{gclMsgID}},
		{`DELETE FROM normalized_threads WHERE thread_key LIKE 'gmail:itest-gcl-%'`, nil},
		{`DELETE FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE account_email=$1)`, []any{gclEmail}},
		{`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, []any{gclSlug}},
		{`DELETE FROM projects WHERE slug=$1`, []any{gclSlug}},
		{`DELETE FROM source_accounts WHERE account_email=$1`, []any{gclEmail}},
	}
	for _, st := range stmts {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}

}

func TestGmail_Integration_LoopClosure(t *testing.T) {
	requireCompose(t)
	ctx := context.Background()

	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	defer pool.Close()

	cleanupGmailClosure(t, ctx, pool)
	defer cleanupGmailClosure(t, ctx, pool)

	// Seed a google account, project + work task, and a SENT gmail delivery whose
	// self-chosen Message-ID we will feed back through ingestion.
	var acctID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, send_enabled) VALUES ('google', $1, true) RETURNING id`,
		gclEmail).Scan(&acctID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	var projID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path)
		 VALUES ($1,$1,'itest-gcl-client','manual','dashboard','/tmp/itest') RETURNING id`, gclSlug).Scan(&projID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	var taskID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status) VALUES ($1,'gcl work','claude','delivered') RETURNING id`, projID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	var deliveryID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, status, sent_external_id, sent_at)
		 VALUES ($1, 'gmail', 'sent', $2, now()) RETURNING id`, taskID, gclMsgID).Scan(&deliveryID); err != nil {
		t.Fatalf("seed delivery: %v", err)
	}

	// Ingest a raw outbound gmail message carrying that Message-ID (From is our
	// own account -> direction outbound at normalize).
	full := gmailFull("gcl-msg", gclGThread, gclMsgID, gclEmail, "client@itest-gcl.example", 1751364000000, "our reply body")
	if _, err := pool.Exec(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1, $2, $3, 'itest-gcl-hash-1')`, acctID, gclRawExt, full); err != nil {
		t.Fatalf("seed raw item: %v", err)
	}

	tasksBefore := scanInt(t, ctx, pool, `SELECT count(*) FROM tasks`)

	if _, err := google.Normalize(ctx, google.NewPGSink(pool), google.Config{}); err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	// The message normalizes OUTBOUND (belt: triage is inbound-only, so it can
	// never be re-triaged — invariant 5).
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_messages WHERE external_message_id=$1 AND direction='outbound'`, gclMsgID); got != 1 {
		t.Errorf("outbound normalized rows for our send = %d, want 1", got)
	}

	// Loop closure: the delivery is confirmed and a delivery_confirmed event lands
	// on its task.
	var confirmedAt *string
	if err := pool.QueryRow(ctx, `SELECT confirmed_at::text FROM deliveries WHERE id=$1`, deliveryID).Scan(&confirmedAt); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if confirmedAt == nil {
		t.Errorf("deliveries.confirmed_at is NULL; the Message-ID match must confirm the send")
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_confirmed'`, taskID); got != 1 {
		t.Errorf("delivery_confirmed task_events = %d, want 1", got)
	}
	// No new task was created from our own send.
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM tasks`); got != tasksBefore {
		t.Errorf("tasks changed: before=%d after=%d (own send must not create a task)", tasksBefore, got)
	}

	// Re-normalize is idempotent: confirmed_at stays, no duplicate event
	// (first-match guard, confirmed_at IS NULL).
	if _, err := pool.Exec(ctx, `UPDATE raw_source_items SET normalized_at=NULL WHERE external_id=$1`, gclRawExt); err != nil {
		t.Fatalf("reset normalized_at: %v", err)
	}
	if _, err := google.Normalize(ctx, google.NewPGSink(pool), google.Config{}); err != nil {
		t.Fatalf("Normalize (rerun): %v", err)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_confirmed'`, taskID); got != 1 {
		t.Errorf("delivery_confirmed events after rerun = %d, want still 1 (first-match guard)", got)
	}
}
