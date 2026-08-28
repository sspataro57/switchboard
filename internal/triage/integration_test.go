//go:build integration

package triage_test

// Integration test for the GPT triage worker (SPEC 06-gpt-triage, verification
// protocol step 2; acceptance criteria 2, 3, 4, 5, 6, 7, 8). Build-tagged
// `integration` AND env-gated on DATABASE_URL: excluded from the default
// zero-network `go test ./...`, skips cleanly when the DB env is unset. Uses a
// FAKE provider (canned responses) — NEVER a live LLM. Run with:
//
//   DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//     go test -tags integration ./internal/triage/
//
// Rerunnable against the persistent compose db: it deletes its own leftovers
// first, in FK order, scoped by test-owned prefixes (source_accounts.provider
// 'itest-triage-src', people.display_name 'itest-triage-%', projects.slug
// 'itest-triage-acme', ai_runs.model 'itest-triage-model').
//
// GREENFIELD NOTE: under `-tags integration` this compile-FAILs until migration
// 0004 is applied AND internal/triage exists — the expected failure mode.
// Imposed surface beyond worker_test.go's:
//
//   func triage.NewStore(pool *pgxpool.Pool) *triage.PGStore // implements Store
//
// (fakeProvider, okResp, mustTime are shared with worker_test.go — same package.)

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/triage"
)

const itestModel = "itest-triage-model"

func scanInt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

func insID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", sql, err)
	}
	return id
}

// cleanupTriage removes this test's leftovers in FK order so it is rerunnable.
// It ALSO neutralizes foreign pending inbound messages left behind by other
// integration suites (e.g. the upwork_crm connector's sim corpus persists in
// the shared compose db): the triage pending filter is global by design, so
// any un-extracted inbound row anywhere would pollute this test's exact-count
// assertions. Neutralize = delete the connector-test corpus (it recreates its
// own state at its next start; compose-only — the real db never runs tests).
// SWT-7 pact-join: the google connector's itest-google-% corpus (inbound Gmail
// messages) is likewise visible to the global pending filter, so it is
// neutralized here too.
func cleanupTriage(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	stmts := []string{
		// foreign corpus first (upwork_crm connector integration leftovers)
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='upwork_crm'))`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='upwork_crm'))`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'upwork_crm:%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='upwork_crm')`,
		// foreign corpus (SWT-7 google connector integration leftovers)
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-google-%'))`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-google-%'))`,
		`DELETE FROM normalized_events WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-google-%'))`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'gmail:itest-google-%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-google-%')`,
		`DELETE FROM sync_runs WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-google-%')`,
		`DELETE FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-google-%'`,
		// foreign corpus (SWT-11 IMAP mail connector leftovers). Its suite creates
		// INBOUND gmail-channel rows, which triage's GLOBAL pending filter sees —
		// the pact obligation named in imap_integration_test.go's header.
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-imap-%'))`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-imap-%'))`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'gmail:itest-imap-%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-imap-%')`,
		`DELETE FROM sync_runs WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-imap-%')`,
		`DELETE FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-imap-%'`,
		// foreign corpus (Slack Web connector integration leftovers)
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='slack_web' AND account_email='titest@slack-web.local'))`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='slack_web' AND account_email='titest@slack-web.local'))`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'slack:TITEST:%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='slack_web' AND account_email='titest@slack-web.local')`,
		`DELETE FROM sync_runs WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='slack_web' AND account_email='titest@slack-web.local')`,
		`DELETE FROM source_accounts WHERE provider='slack_web' AND account_email='titest@slack-web.local'`,
		`DELETE FROM ai_extractions WHERE ai_run_id IN (SELECT id FROM ai_runs WHERE model = 'itest-triage-model')`,
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='itest-triage-src'))`,
		`DELETE FROM ai_runs WHERE model = 'itest-triage-model'`,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='itest-triage-acme')`,
		`DELETE FROM projects WHERE slug='itest-triage-acme'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='itest-triage-src'))`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'itest-triage:%'`,
		`DELETE FROM person_identities WHERE person_id IN (SELECT id FROM people WHERE display_name LIKE 'itest-triage-%')`,
		`DELETE FROM people WHERE display_name LIKE 'itest-triage-%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='itest-triage-src')`,
		`DELETE FROM sync_runs WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='itest-triage-src')`,
		`DELETE FROM source_accounts WHERE provider='itest-triage-src'`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
}

type seeded struct {
	mappedPersonID   int64
	unmappedPersonID int64
	projectID        int64
	candidate1       int64
	candidate2       int64
	rawMapped        int64
	rawUnmapped      int64
	rawOutbound      int64
}

// seedCorpus builds a mini corpus: one mapped person (→ project with 2 open
// tasks) and one unmapped person; a thread each; a mapped inbound message, a
// mapped OUTBOUND message (must NOT be triaged — invariant 5), and an unmapped
// inbound message. sent_at is ordered so processing is deterministic.
func seedCorpus(t *testing.T, ctx context.Context, pool *pgxpool.Pool) seeded {
	t.Helper()
	var s seeded

	accountID := insID(t, ctx, pool,
		`INSERT INTO source_accounts (provider, account_email, send_enabled)
		 VALUES ('itest-triage-src', 'itest-triage@pg-main', false) RETURNING id`)

	s.mappedPersonID = insID(t, ctx, pool,
		`INSERT INTO people (display_name) VALUES ('itest-triage-Acme Corp') RETURNING id`)
	s.unmappedPersonID = insID(t, ctx, pool,
		`INSERT INTO people (display_name) VALUES ('itest-triage-Stranger') RETURNING id`)

	// The project no longer carries a person mapping — 0015 dropped the column.
	// mappedPersonID still exists and still drives identity resolution; what it
	// no longer does is select a project.
	s.projectID = insID(t, ctx, pool,
		`INSERT INTO projects (name, slug, client)
		 VALUES ('itest-triage Acme', 'itest-triage-acme', 'Acme') RETURNING id`)

	// Two open tasks = attach candidates for the mapped project.
	s.candidate1 = insID(t, ctx, pool,
		`INSERT INTO tasks (project_id, title, status) VALUES ($1, 'Login flow revamp', 'ready') RETURNING id`, s.projectID)
	s.candidate2 = insID(t, ctx, pool,
		`INSERT INTO tasks (project_id, title, status) VALUES ($1, 'Billing page polish', 'in_progress') RETURNING id`, s.projectID)

	// Raw items (raw-first linkage target for extractions).
	s.rawMapped = insID(t, ctx, pool,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1, 'itest-triage-raw-mapped', '{"k":"mapped"}', 'h-mapped') RETURNING id`, accountID)
	s.rawOutbound = insID(t, ctx, pool,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1, 'itest-triage-raw-outbound', '{"k":"outbound"}', 'h-outbound') RETURNING id`, accountID)
	s.rawUnmapped = insID(t, ctx, pool,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1, 'itest-triage-raw-unmapped', '{"k":"unmapped"}', 'h-unmapped') RETURNING id`, accountID)

	// Threads with participants pointing at the resolved person.
	mappedThread := insID(t, ctx, pool,
		`INSERT INTO normalized_threads (thread_key, subject, participants)
		 VALUES ('itest-triage:acme', 'login broken', $1) RETURNING id`,
		[]byte(fmt.Sprintf("[%d]", s.mappedPersonID)))
	unmappedThread := insID(t, ctx, pool,
		`INSERT INTO normalized_threads (thread_key, subject, participants)
		 VALUES ('itest-triage:stranger', 'thanks', $1) RETURNING id`,
		[]byte(fmt.Sprintf("[%d]", s.unmappedPersonID)))

	// Messages. Mapped inbound is OLDEST (processed first); mapped outbound is
	// thread context only; unmapped inbound is newest inbound.
	insID(t, ctx, pool,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1, $2, 'inbound', 'itm-1', '2026-07-01T10:00:00Z', 'please fix the login bug on staging', 'login broken', 'client@acme.example', 'upwork') RETURNING id`,
		s.rawMapped, mappedThread)
	insID(t, ctx, pool,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1, $2, 'outbound', 'itm-2', '2026-07-02T10:00:00Z', 'we are on it', 'login broken', 'me@sb.example', 'upwork') RETURNING id`,
		s.rawOutbound, mappedThread)
	insID(t, ctx, pool,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1, $2, 'inbound', 'itm-3', '2026-07-03T10:00:00Z', 'thanks, all good!', 'thanks', 'stranger@x.example', 'email') RETURNING id`,
		s.rawUnmapped, unmappedThread)

	// Put the two INBOUND messages in triage's inbox (SWT-17 §8b).
	//
	// Since 0015, triage consumes messages whose LATEST capture decision is
	// 'unmatched' — evaluated by the rule engine and covered by no rule. A
	// message with NO decision row is UNSEEN, not unmatched, and is deliberately
	// skipped: the capture pass hitchhikes on connector CronJobs, so a triage run
	// can easily outrun it, and treating unseen as unroutable would hand every
	// fresh message to the model before the deterministic rules ever looked. That
	// inversion is the whole thing §8b exists to prevent.
	//
	// So this seeding is not scaffolding — it encodes the ordering the design
	// depends on. Without it triage processes nothing here, correctly, and the
	// suite fails with counts of zero.
	//
	// The outbound message gets no decision on purpose: it is never triaged
	// regardless, and leaving it undecided keeps that independent of this filter.
	for _, msg := range []string{"itm-1", "itm-3"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO capture_decisions (message_id, mode, action, reason)
			 SELECT id, 'shadow', 'unmatched', 'itest-triage: no rule covers this'
			   FROM normalized_messages WHERE external_message_id = $1`, msg); err != nil {
			t.Fatalf("seed capture decision for %s: %v", msg, err)
		}
	}

	return s
}

func TestTriage_Integration_ShadowMode(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db (cleanup deletes corpus rows); use the compose db on :5433")
	}
	ctx := context.Background()

	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	defer pool.Close()

	// SWT-6's criterion 2 pinned projects.client_person_id as a migration-0004
	// artifact. Migration 0015 DROPS that column: project membership is a
	// capture_decisions question now (SWT-17 §8a), not a person mapping. The
	// assertion is removed rather than inverted — "the column is absent" pins
	// nothing useful, and the replacement mechanism has its own suite in
	// capturedecisions_integration_test.go.

	cleanupTriage(t, ctx, pool)
	sd := seedCorpus(t, ctx, pool)

	// Shadow snapshots: tasks / task_events / deliveries must not move.
	tasksBefore := scanInt(t, ctx, pool, `SELECT count(*) FROM tasks`)
	eventsBefore := scanInt(t, ctx, pool, `SELECT count(*) FROM task_events`)
	deliveriesBefore := scanInt(t, ctx, pool, `SELECT count(*) FROM deliveries`)

	st := triage.NewStore(pool)

	// Fake provider: response[0] for the mapped inbound (attach to candidate1),
	// response[1] for the unmapped inbound (fyi, not actionable). Processing is
	// oldest-first, so the mapped message (older sent_at) is call 0.
	mappedResp := fmt.Sprintf(`{"actionable":{"value":true,"confidence":0.92},"kind":{"value":"action_request","confidence":0.88},"title":{"value":"Fix staging login","confidence":0.81},"body":{"value":"login broken on staging","confidence":0.7},"priority":{"value":2,"confidence":0.6},"attach_to_task_id":{"value":%d,"confidence":0.75},"summary":"clear bug report"}`, sd.candidate1)
	unmappedResp := `{"actionable":{"value":false,"confidence":0.95},"kind":{"value":"fyi","confidence":0.9},"title":{"value":"Thanks note","confidence":0.9},"body":{"value":"thanks","confidence":0.9},"priority":{"value":0,"confidence":0.9},"attach_to_task_id":{"value":null,"confidence":0.9},"summary":"acknowledgement"}`

	prov := &fakeProvider{scripts: []scriptedResp{
		{resp: okResp(mappedResp)},
		{resp: okResp(unmappedResp)},
	}}

	cfg := triage.Config{Model: itestModel, MaxTokens: 512}

	stats, err := triage.Run(ctx, st, prov, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Criterion 3/5: exactly the 2 inbound messages triaged; outbound skipped.
	if prov.calls != 2 {
		t.Errorf("provider calls = %d, want 2 (2 inbound; outbound never triaged)", prov.calls)
	}
	if stats.Processed != 2 {
		t.Errorf("stats.Processed = %d, want 2", stats.Processed)
	}

	// Criterion 4: two ai_runs (worker_type triage, model, status ok, tokens/latency).
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM ai_runs WHERE model=$1 AND worker_type='triage' AND provider='openai' AND status='ok'`, itestModel); got != 2 {
		t.Errorf("ai_runs (triage, ok) = %d, want 2", got)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM ai_runs WHERE model=$1 AND prompt_tokens IS NOT NULL AND latency_ms IS NOT NULL`, itestModel); got != 2 {
		t.Errorf("ai_runs with token/latency columns populated = %d, want 2", got)
	}

	// Criterion 4: two ai_extractions linked to the inbound raw items; outbound has none.
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM ai_extractions WHERE raw_source_item_id = ANY($1)`,
		[]int64{sd.rawMapped, sd.rawUnmapped}); got != 2 {
		t.Errorf("ai_extractions for inbound raw items = %d, want 2", got)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM ai_extractions WHERE raw_source_item_id = $1`, sd.rawOutbound); got != 0 {
		t.Errorf("outbound message got an extraction (%d); it must never be triaged (invariant 5)", got)
	}

	// Criterion 5/6: mapped extraction preserves confidences + attaches to candidate1.
	mappedFields := fetchFields(t, ctx, pool, sd.rawMapped)
	if got := fieldObjI(t, mappedFields, "actionable")["confidence"]; got != 0.92 {
		t.Errorf("mapped actionable.confidence = %v, want 0.92 (verbatim)", got)
	}
	// CHANGED BY SWT-17, and the change is a real one rather than a fixture nit.
	//
	// This used to assert `attach` to candidate1. It cannot any more, and not
	// because of how the fixture is seeded: triage's pending loop consumes ONLY
	// messages whose latest capture decision is 'unmatched' (§8b), an unmatched
	// decision carries no project (§8a), and AssembleContext loads attach
	// candidates only when a project resolved (store.go:166-172). So every
	// message the live pass sees has no project, no candidates, and therefore
	// cannot produce an attach verdict.
	//
	// That makes attach-vs-create UNREACHABLE FROM THE PASS THAT RUNS. It is a
	// consequence of §8b rather than a defect — if capture routed a message,
	// capture owns creating its task; if capture did not, there is no project to
	// attach within — but the SPEC states the token saving and not this, so it is
	// written down here and in the SPEC's own §8 notes.
	//
	// AssembleContext's project lookup is still exercised for attributed and
	// routed messages, directly, in capturedecisions_integration_test.go. What is
	// gone is the route from the pending loop to a candidate list.
	if got := mappedFields["verdict"]; got != "create" {
		t.Errorf("mapped verdict = %v, want create: an unmatched message has no project (§8a) and so no "+
			"attach candidates, which is the only verdict shape the live pass can now produce", got)
	}
	if v, ok := mappedFields["attach_to_task_id"]; ok {
		if obj, isObj := v.(map[string]any); isObj && obj["value"] != nil {
			t.Errorf("attach_to_task_id.value = %v on a projectless message; there are no candidates to "+
				"attach to, so proposing one would be the model inventing a task id", obj["value"])
		}
	}
	_ = sd.candidate1
	// project_id is deliberately NOT asserted here any more. It used to arrive
	// via projects.client_person_id, which migration 0015 drops; triage now reads
	// it from capture_decisions (SWT-17 §8a), and that path is owned by
	// capturedecisions_integration_test.go. Asserting it here without seeding a
	// decision would pin the absence of a mechanism rather than the presence of
	// one — and this suite is about shadow-mode EXTRACTION, which is unaffected.

	// Criterion: unmapped person still extracted, project null.
	unmappedFields := fetchFields(t, ctx, pool, sd.rawUnmapped)
	if _, ok := unmappedFields["project_id"]; !ok {
		t.Errorf("unmapped extraction missing project_id key")
	} else if unmappedFields["project_id"] != nil {
		t.Errorf("unmapped project_id = %v, want null", unmappedFields["project_id"])
	}
	if got := unmappedFields["verdict"]; got != "none" {
		t.Errorf("unmapped verdict = %v, want none (not actionable)", got)
	}

	// Criterion 7 (shadow): tasks / task_events / deliveries unchanged.
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM tasks`); got != tasksBefore {
		t.Errorf("tasks changed: before=%d after=%d (shadow mode writes zero tasks)", tasksBefore, got)
	}
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM task_events`); got != eventsBefore {
		t.Errorf("task_events changed: before=%d after=%d", eventsBefore, got)
	}
	if got := scanInt(t, ctx, pool, `SELECT count(*) FROM deliveries`); got != deliveriesBefore {
		t.Errorf("deliveries changed: before=%d after=%d", deliveriesBefore, got)
	}

	// Criterion 8: idempotent second run — 0 processed, 0 provider calls.
	prov2 := &fakeProvider{scripts: []scriptedResp{{resp: okResp(`{}`)}}}
	stats2, err := triage.Run(ctx, st, prov2, cfg)
	if err != nil {
		t.Fatalf("Run (rerun): %v", err)
	}
	if prov2.calls != 0 {
		t.Errorf("second run provider calls = %d, want 0 (already-extracted messages excluded by the filter)", prov2.calls)
	}
	if stats2.Processed != 0 {
		t.Errorf("second run processed = %d, want 0", stats2.Processed)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM ai_extractions WHERE raw_source_item_id = ANY($1)`,
		[]int64{sd.rawMapped, sd.rawUnmapped}); got != 2 {
		t.Errorf("ai_extractions after rerun = %d, want 2 (no duplicates)", got)
	}
}

func fetchFields(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rawItemID int64) map[string]any {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(ctx,
		`SELECT fields FROM ai_extractions WHERE raw_source_item_id=$1 ORDER BY id DESC LIMIT 1`, rawItemID).Scan(&raw); err != nil {
		t.Fatalf("select fields for raw %d: %v", rawItemID, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("fields for raw %d not valid JSON: %v", rawItemID, err)
	}
	return m
}

func fieldObjI(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	o, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("fields[%q] is not a {value,confidence} object: %v", key, m[key])
	}
	return o
}
