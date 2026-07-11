//go:build integration

package upworkcrm_test

// Integration test for the upwork_crm connector (SPEC 02-upwork-crm-connector,
// verification protocol step 2; acceptance criteria 2, 3, 4, 5, 6, 7, 10).
// Build-tagged `integration` AND env-gated on DATABASE_URL: excluded from the
// default zero-network `go test ./...`, skips cleanly when the DB env is unset.
// Run with:
//
//   DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//     go test -tags integration ./internal/connector/upworkcrm/
//
// The source (upwork_crm.clients/communications) is SIMULATED as a dedicated
// schema `upwork_crm_sim` on the same compose Postgres — the compose `ops` user
// is superuser locally, so it can CREATE SCHEMA. The source pool's search_path
// points at that schema; the sink pool writes canonical rows to public. This
// mirrors the real two-DSN split without needing a second cluster.
//
// GREENFIELD NOTE: under `-tags integration` this compile-FAILs until the
// connector + store.NewPoolDSN exist — the expected failure mode. Expected
// exported surface exercised here (SPEC files source.go/ingest.go/normalize.go
// + internal/store):
//
//   func store.NewPoolDSN(ctx context.Context, dsn string) (*pgxpool.Pool, error)
//   func upworkcrm.NewSource(pool *pgxpool.Pool) *upworkcrm.PGSource   // implements SourceReader; reads unqualified clients/communications (search_path)
//   func upworkcrm.NewSink(pool *pgxpool.Pool) *upworkcrm.PGSink       // implements the ingest Sink AND the normalize store
//   func upworkcrm.Ingest(ctx, SourceReader, Sink, Config) (Stats, error)
//   func upworkcrm.Normalize(ctx, <normalize store>, Config) (Stats, error) // Normalize takes no source (criterion 7)

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/upworkcrm"
	"github.com/sspataro57/switchboard/internal/store"
)

// Fixed source UUIDs (clientUUID is declared in normalize_test.go).
const (
	igClient2 = "22222222-2222-2222-2222-222222222222"
	igMsg1    = "c0000001-0000-0000-0000-000000000001"
	igMsg2    = "c0000002-0000-0000-0000-000000000002"
	igMsg3    = "c0000003-0000-0000-0000-000000000003" // draft — never ingested
	igMsg4    = "c0000004-0000-0000-0000-000000000004"
)

func scanInt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

// cleanupConnector removes this connector's leftovers so the test is rerunnable
// against the persistent compose db (FK order: messages -> threads ->
// identities/people -> raw -> sync_runs -> source_accounts; then the sim schema).
func cleanupConnector(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	stmts := []string{
		`DELETE FROM normalized_messages WHERE thread_id IN (SELECT id FROM normalized_threads WHERE thread_key LIKE 'upwork_crm:%')`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='upwork_crm'))`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'upwork_crm:%'`,
		`DELETE FROM person_identities WHERE person_id IN (SELECT DISTINCT person_id FROM person_identities WHERE provider='upwork_crm')`,
		`DELETE FROM people WHERE id NOT IN (SELECT person_id FROM person_identities)`,
		`DELETE FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='upwork_crm')`,
		`DELETE FROM sync_runs WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='upwork_crm')`,
		`DELETE FROM source_accounts WHERE provider='upwork_crm'`,
		`DROP SCHEMA IF EXISTS upwork_crm_sim CASCADE`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
}

// seedSource creates the simulated upwork_crm tables and rows. m3 is a draft
// (must never be ingested); non-draft communications = 3, clients = 2.
func seedSource(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	ddl := `
CREATE SCHEMA upwork_crm_sim;
CREATE TABLE upwork_crm_sim.clients (
  id             uuid PRIMARY KEY,
  name           text NOT NULL,
  email          text,
  company        text,
  upwork_room_id text,
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE upwork_crm_sim.communications (
  id              uuid PRIMARY KEY,
  client_id       uuid NOT NULL REFERENCES upwork_crm_sim.clients(id),
  direction       text,
  channel         text,
  subject         text,
  body            text,
  communicated_at timestamptz NOT NULL,
  created_at      timestamptz NOT NULL,
  sender          text,
  external_id     text,
  is_draft        boolean NOT NULL DEFAULT false
);
INSERT INTO upwork_crm_sim.clients (id, name, email, company, upwork_room_id) VALUES
  ('` + clientUUID + `', 'Acme Corp', 'ops@acme.example', 'Acme', 'room-777'),
  ('` + igClient2 + `', 'Beta LLC', 'hi@beta.example', 'Beta', NULL);
INSERT INTO upwork_crm_sim.communications
  (id, client_id, direction, channel, subject, body, communicated_at, created_at, sender, external_id, is_draft) VALUES
  ('` + igMsg1 + `', '` + clientUUID + `', 'inbound',  'upwork', 's1', 'hello-1', '2026-07-01T10:00:00Z', '2026-07-01T10:00:01Z', 'a@acme.example', 'ext-1', false),
  ('` + igMsg2 + `', '` + clientUUID + `', 'outbound', 'email',  's2', 'hello-2', '2026-07-02T10:00:00Z', '2026-07-02T10:00:01Z', 'me@sb.example',  'ext-2', false),
  ('` + igMsg3 + `', '` + clientUUID + `', 'outbound', 'upwork', 's3', 'draft-3', '2026-07-03T10:00:00Z', '2026-07-03T10:00:01Z', 'me@sb.example',  'ext-3', true),
  ('` + igMsg4 + `', '` + igClient2 + `', 'inbound',  'upwork', 's4', 'hello-4', '2026-07-04T10:00:00Z', '2026-07-04T10:00:01Z', 'b@beta.example', 'ext-4', false);
`
	if _, err := pool.Exec(ctx, ddl); err != nil {
		t.Fatalf("seed source: %v", err)
	}
}

// sourceDSN derives the source pool DSN from DATABASE_URL by pointing search_path
// at the simulated schema.
func sourceDSN(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	q := u.Query()
	q.Set("options", "-c search_path=upwork_crm_sim,public")
	u.RawQuery = q.Encode()
	return u.String()
}

func TestUpworkCRM_Integration_EndToEnd(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()

	opsPool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	defer opsPool.Close()

	cleanupConnector(t, ctx, opsPool)
	seedSource(t, ctx, opsPool)

	srcPool, err := store.NewPoolDSN(ctx, sourceDSN(t))
	if err != nil {
		t.Fatalf("store.NewPoolDSN (source): %v", err)
	}
	defer srcPool.Close()

	// Criterion 10: tasks are never touched by any run.
	tasksBefore := scanInt(t, ctx, opsPool, `SELECT count(*) FROM tasks`)

	src := upworkcrm.NewSource(srcPool)
	sink := upworkcrm.NewSink(opsPool)

	// ---- First run: ingest phase, then normalize phase ---------------------
	istats, err := upworkcrm.Ingest(ctx, src, sink, upworkcrm.Config{Overlap: 24 * time.Hour})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// Criterion 2: a source_accounts row exists; sync_runs ended ok; raw holds
	// one row per client + per non-draft communication with hashes + external ids.
	if got := scanInt(t, ctx, opsPool, `SELECT count(*) FROM source_accounts WHERE provider='upwork_crm'`); got != 1 {
		t.Errorf("source_accounts(upwork_crm) = %d, want 1", got)
	}
	if got := scanInt(t, ctx, opsPool, `SELECT count(*) FROM sync_runs WHERE status='ok'`); got < 1 {
		t.Errorf("sync_runs status=ok = %d, want >= 1", got)
	}
	rawCount := scanInt(t, ctx, opsPool, `SELECT count(*) FROM raw_source_items`)
	if rawCount != 5 { // 2 clients + 3 non-draft communications
		t.Errorf("raw_source_items = %d, want 5 (draft excluded)", rawCount)
	}
	if istats.RawInserted != 5 {
		t.Errorf("stats.RawInserted = %d, want 5", istats.RawInserted)
	}
	// Criterion 8: nothing derived from the draft communication.
	if got := scanInt(t, ctx, opsPool, `SELECT count(*) FROM raw_source_items WHERE external_id = $1`, "communications:"+igMsg3); got != 0 {
		t.Errorf("draft communication was ingested (external_id communications:%s)", igMsg3)
	}
	if got := scanInt(t, ctx, opsPool, `SELECT count(*) FROM raw_source_items WHERE content_hash IS NULL OR content_hash = ''`); got != 0 {
		t.Errorf("%d raw rows missing content_hash", got)
	}
	if got := scanInt(t, ctx, opsPool, `SELECT count(*) FROM raw_source_items WHERE external_id LIKE 'clients:%'`); got != 2 {
		t.Errorf("raw client rows = %d, want 2", got)
	}
	if got := scanInt(t, ctx, opsPool, `SELECT count(*) FROM raw_source_items WHERE external_id LIKE 'communications:%'`); got != 3 {
		t.Errorf("raw communication rows = %d, want 3", got)
	}

	// Criterion 3: raw-first is observable — after ingest and BEFORE normalize,
	// every raw row is pending (normalized_at IS NULL).
	if got := scanInt(t, ctx, opsPool, `SELECT count(*) FROM raw_source_items WHERE normalized_at IS NULL`); got != 5 {
		t.Errorf("pending raw rows after ingest = %d, want 5 (raw lands before normalize)", got)
	}

	if _, err := upworkcrm.Normalize(ctx, sink, upworkcrm.Config{}); err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	// Criterion 3 (cont.): all raw rows now normalized; ingested_at <= normalized_at.
	if got := scanInt(t, ctx, opsPool, `SELECT count(*) FROM raw_source_items WHERE normalized_at IS NULL`); got != 0 {
		t.Errorf("pending raw rows after normalize = %d, want 0", got)
	}
	if got := scanInt(t, ctx, opsPool, `SELECT count(*) FROM raw_source_items WHERE normalized_at IS NOT NULL AND ingested_at > normalized_at`); got != 0 {
		t.Errorf("%d raw rows have ingested_at > normalized_at (raw must land first)", got)
	}

	// Criterion 4: complete deterministic normalization.
	if got := scanInt(t, ctx, opsPool, `SELECT count(*) FROM normalized_messages`); got != 3 {
		t.Errorf("normalized_messages = %d, want 3", got)
	}
	if got := scanInt(t, ctx, opsPool, `SELECT count(*) FROM normalized_threads WHERE thread_key LIKE 'upwork_crm:%'`); got != 3 {
		t.Errorf("normalized_threads = %d, want 3 (one per client+channel)", got)
	}
	if got := scanInt(t, ctx, opsPool,
		`SELECT count(*) FROM normalized_threads WHERE thread_key = $1`,
		"upwork_crm:"+clientUUID+":upwork"); got != 1 {
		t.Errorf("expected thread_key upwork_crm:%s:upwork", clientUUID)
	}
	// Each message carries the new columns + verbatim external id.
	if got := scanInt(t, ctx, opsPool,
		`SELECT count(*) FROM normalized_messages WHERE external_message_id='ext-1' AND channel='upwork' AND direction='inbound' AND subject='s1' AND sender='a@acme.example' AND body_text='hello-1'`); got != 1 {
		t.Errorf("message ext-1 not fully mapped (direction/channel/subject/sender/body/external_message_id)")
	}
	// People + identities: upwork_crm always; email/room when present.
	if got := scanInt(t, ctx, opsPool, `SELECT count(*) FROM people`); got != 2 {
		t.Errorf("people = %d, want 2", got)
	}
	if got := scanInt(t, ctx, opsPool, `SELECT count(*) FROM person_identities WHERE provider='upwork_crm' AND value IN ($1,$2)`, clientUUID, igClient2); got != 2 {
		t.Errorf("upwork_crm identities = %d, want 2", got)
	}
	if got := scanInt(t, ctx, opsPool, `SELECT count(*) FROM person_identities WHERE provider='upwork_room' AND value='room-777'`); got != 1 {
		t.Errorf("expected upwork_room identity for client with a room")
	}

	// ---- Second run: full no-op (criterion 5) ------------------------------
	istats2, err := upworkcrm.Ingest(ctx, src, sink, upworkcrm.Config{Overlap: 24 * time.Hour})
	if err != nil {
		t.Fatalf("Ingest (rerun): %v", err)
	}
	if istats2.RawInserted != 0 || istats2.RawUpdated != 0 {
		t.Errorf("rerun stats RawInserted=%d RawUpdated=%d, want 0/0 (content_hash short-circuit)", istats2.RawInserted, istats2.RawUpdated)
	}
	if _, err := upworkcrm.Normalize(ctx, sink, upworkcrm.Config{}); err != nil {
		t.Fatalf("Normalize (rerun): %v", err)
	}
	if got := scanInt(t, ctx, opsPool, `SELECT count(*) FROM raw_source_items`); got != 5 {
		t.Errorf("raw rows after rerun = %d, want 5 (no new rows)", got)
	}
	if got := scanInt(t, ctx, opsPool, `SELECT count(*) FROM normalized_messages`); got != 3 {
		t.Errorf("messages after rerun = %d, want 3 (no duplicates)", got)
	}

	// ---- Change handling: mutate a source row, --full (criterion 6) --------
	if _, err := opsPool.Exec(ctx, `UPDATE upwork_crm_sim.communications SET body='CHANGED-1' WHERE id=$1::uuid`, igMsg1); err != nil {
		t.Fatalf("mutate source comm: %v", err)
	}
	istats3, err := upworkcrm.Ingest(ctx, src, sink, upworkcrm.Config{Full: true, Overlap: 24 * time.Hour})
	if err != nil {
		t.Fatalf("Ingest (--full after mutation): %v", err)
	}
	if istats3.RawUpdated < 1 {
		t.Errorf("stats.RawUpdated = %d after mutation, want >= 1", istats3.RawUpdated)
	}
	// Raw updated in place (still 5 rows), normalized_at reset for that row.
	if got := scanInt(t, ctx, opsPool, `SELECT count(*) FROM raw_source_items`); got != 5 {
		t.Errorf("raw rows after mutation run = %d, want 5 (update in place)", got)
	}
	if got := scanInt(t, ctx, opsPool, `SELECT count(*) FROM raw_source_items WHERE normalized_at IS NULL`); got < 1 {
		t.Errorf("changed raw row should have normalized_at reset to NULL")
	}
	if _, err := upworkcrm.Normalize(ctx, sink, upworkcrm.Config{}); err != nil {
		t.Fatalf("Normalize (after mutation): %v", err)
	}
	if got := scanInt(t, ctx, opsPool, `SELECT count(*) FROM normalized_messages`); got != 3 {
		t.Errorf("messages after mutation = %d, want 3 (updated, not duplicated)", got)
	}
	if got := scanInt(t, ctx, opsPool, `SELECT count(*) FROM normalized_messages WHERE external_message_id='ext-1' AND body_text='CHANGED-1'`); got != 1 {
		t.Errorf("mutated message body not re-normalized to CHANGED-1")
	}

	// ---- Re-normalize from raw alone, no source DSN (criterion 7) ----------
	beforeThreads := scanInt(t, ctx, opsPool, `SELECT count(*) FROM normalized_threads WHERE thread_key LIKE 'upwork_crm:%'`)
	beforeMsgs := scanInt(t, ctx, opsPool, `SELECT count(*) FROM normalized_messages`)
	beforePeople := scanInt(t, ctx, opsPool, `SELECT count(*) FROM people`)

	// Collect connector-owned person ids to delete precisely.
	var personIDs []int64
	rows, err := opsPool.Query(ctx, `SELECT DISTINCT person_id FROM person_identities WHERE provider='upwork_crm'`)
	if err != nil {
		t.Fatalf("collect person ids: %v", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan person id: %v", err)
		}
		personIDs = append(personIDs, id)
	}
	rows.Close()

	wipes := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM normalized_messages WHERE thread_id IN (SELECT id FROM normalized_threads WHERE thread_key LIKE 'upwork_crm:%')`, nil},
		{`DELETE FROM normalized_threads WHERE thread_key LIKE 'upwork_crm:%'`, nil},
		{`DELETE FROM person_identities WHERE person_id = ANY($1)`, []any{personIDs}},
		{`DELETE FROM people WHERE id = ANY($1)`, []any{personIDs}},
	}
	for _, w := range wipes {
		if _, err := opsPool.Exec(ctx, w.sql, w.args...); err != nil {
			t.Fatalf("wipe %q: %v", w.sql, err)
		}
	}
	if got := scanInt(t, ctx, opsPool, `SELECT count(*) FROM normalized_messages`); got != 0 {
		t.Fatalf("precondition: normalized_messages should be empty before renormalize, got %d", got)
	}

	// Normalize-only, --all, NO source pool involved: rebuild purely from raw.
	sink2 := upworkcrm.NewSink(opsPool)
	if _, err := upworkcrm.Normalize(ctx, sink2, upworkcrm.Config{All: true}); err != nil {
		t.Fatalf("Normalize (--all from raw alone): %v", err)
	}
	if got := scanInt(t, ctx, opsPool, `SELECT count(*) FROM normalized_threads WHERE thread_key LIKE 'upwork_crm:%'`); got != beforeThreads {
		t.Errorf("threads after renormalize = %d, want %d (identical row set)", got, beforeThreads)
	}
	if got := scanInt(t, ctx, opsPool, `SELECT count(*) FROM normalized_messages`); got != beforeMsgs {
		t.Errorf("messages after renormalize = %d, want %d", got, beforeMsgs)
	}
	if got := scanInt(t, ctx, opsPool, `SELECT count(*) FROM people`); got != beforePeople {
		t.Errorf("people after renormalize = %d, want %d", got, beforePeople)
	}
	if got := scanInt(t, ctx, opsPool, `SELECT count(*) FROM normalized_messages WHERE external_message_id='ext-1' AND body_text='CHANGED-1'`); got != 1 {
		t.Errorf("renormalized message ext-1 body = wrong; raw is the source of truth")
	}

	// Criterion 10: tasks untouched across every run.
	if tasksAfter := scanInt(t, ctx, opsPool, `SELECT count(*) FROM tasks`); tasksAfter != tasksBefore {
		t.Errorf("tasks count changed: before=%d after=%d (connector must create zero tasks)", tasksBefore, tasksAfter)
	}
}
