//go:build integration

package upworkcrm_test

// Integration test for upwork assisted-tier loop closure (SPEC 08-draft-
// deliveries, criterion 8 + invariant 5). Build-tagged `integration`, env-gated
// on DATABASE_URL. It seeds a manually-marked-sent upwork delivery
// (sent_external_id NULL) and then normalizes an OUTBOUND communication for the
// same client whose body prefix matches — the post-hoc matcher fills
// sent_external_id + confirmed_at + delivery_confirmed. NEVER touches the real
// upwork_crm source (normalize reads only raw_source_items).
//
//   DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//     go test -tags integration -run LoopClosure ./internal/connector/upworkcrm/
//
// GREENFIELD NOTE: the upwork confirmation matcher (sink/normalize hook) does not
// exist yet, so under `-tags integration` the confirmed_at / sent_external_id /
// delivery_confirmed assertions fail until it is added — the expected failure
// mode. No new exported surface is imposed: the matcher lives inside the existing
// upworkcrm sink/normalize path (SPEC "Files likely to touch").
//
// Cross-suite discipline: the seeded communication is OUTBOUND, so triage's
// inbound-only filter ignores it. This suite cleans its OWN corpus (itest-ucl-%)
// in FK order and never deletes the shared upwork_crm source account.

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/upworkcrm"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	uclClientUUID = "cccccccc-0000-0000-0000-0000000000cl"
	uclCommUUID   = "cccccccc-0000-0000-0000-0000000000co"
	uclCommExtID  = "upwork-room-msg-itest-ucl-777" // becomes the delivery's sent_external_id
	uclSlug       = "itest-ucl-proj"
	uclBody       = "thanks, will push the fix to staging tonight"
)

func uclThreadKey() string { return upworkcrm.Provider + ":" + uclClientUUID + ":chat" }

func cleanupUpworkClosure(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM task_events WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{uclSlug}},
		{`DELETE FROM deliveries WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{uclSlug}},
		{`DELETE FROM normalized_messages WHERE external_message_id=$1`, []any{uclCommExtID}},
		{`DELETE FROM normalized_threads WHERE thread_key=$1`, []any{uclThreadKey()}},
		{`DELETE FROM raw_source_items WHERE external_id=$1 AND source_account_id IN
			(SELECT id FROM source_accounts WHERE provider=$2 AND account_email=$3)`,
			[]any{"communications:" + uclCommUUID, upworkcrm.Provider, upworkcrm.AccountEmail}},
		{`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, []any{uclSlug}},
		{`DELETE FROM projects WHERE slug=$1`, []any{uclSlug}},
	}
	for _, st := range stmts {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}

}

func TestUpwork_Integration_LoopClosure(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	defer pool.Close()

	sink := upworkcrm.NewSink(pool)
	acctID, err := sink.EnsureAccount(ctx)
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}

	cleanupUpworkClosure(t, ctx, pool)
	defer cleanupUpworkClosure(t, ctx, pool)

	// Seed project + work task + a manually-sent upwork delivery: sent, but
	// sent_external_id still NULL (the human copied+marked it), scoped to the
	// client via target_ref (the thread_key).
	var projID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path)
		 VALUES ($1,$1,'itest-ucl-client','manual','dashboard','/tmp/itest') RETURNING id`, uclSlug).Scan(&projID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	var taskID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status) VALUES ($1,'ucl work','claude','delivered') RETURNING id`, projID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	var deliveryID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, sent_external_id, sent_at)
		 VALUES ($1, 'upwork_chat', $2, $3, 'sent', NULL, now()) RETURNING id`,
		taskID, uclThreadKey(), uclBody).Scan(&deliveryID); err != nil {
		t.Fatalf("seed delivery: %v", err)
	}

	// Ingest a raw OUTBOUND communication for the same client whose body matches.
	raw := `{"id":"` + uclCommUUID + `","client_id":"` + uclClientUUID + `","direction":"outbound",` +
		`"channel":"chat","subject":null,"body":"` + uclBody + `","communicated_at":"2026-07-11T10:00:00Z",` +
		`"sender":"me","external_id":"` + uclCommExtID + `"}`
	if _, err := pool.Exec(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1, $2, $3, 'itest-ucl-hash-1')`, acctID, "communications:"+uclCommUUID, raw); err != nil {
		t.Fatalf("seed raw communication: %v", err)
	}

	if _, err := upworkcrm.Normalize(ctx, sink, upworkcrm.Config{}); err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	// Post-hoc match: sent_external_id filled from the communication external_id,
	// confirmed_at set, delivery_confirmed event on the task.
	var sentExtID, confirmedAt *string
	if err := pool.QueryRow(ctx,
		`SELECT sent_external_id, confirmed_at::text FROM deliveries WHERE id=$1`, deliveryID).Scan(&sentExtID, &confirmedAt); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if sentExtID == nil || *sentExtID != uclCommExtID {
		t.Errorf("sent_external_id = %v, want the matched communication external_id %q (post-hoc)", sentExtID, uclCommExtID)
	}
	if confirmedAt == nil {
		t.Errorf("confirmed_at is NULL; the body-prefix match must confirm the send")
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_confirmed'`, taskID); got != 1 {
		t.Errorf("delivery_confirmed task_events = %d, want 1", got)
	}
}
