//go:build integration

package main

// SWT-40 Part D wiring against the compose db (docs/tickets/inquiry-promote_SPEC.md,
// D-D4, D-D5, E-D4). Exercises the REGISTERED pass — stageImpls[gate].pass(pool)
// — exactly as run() builds it, with OPS_TOKEN_KEY unset so its lookup factory
// is nil (no credential: every unreadable hold stays held).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run PipelinedGate ./cmd/pipelined/
//
//   - capture's lock 0x5157_0015 held elsewhere → the pass returns
//     pipeline.ErrLockHeld (the loop's "not now": retry in 30 s, then the sweep);
//   - processed counts RESOLUTIONS, never holds that stay pending: with no
//     credential a fresh hold stays held (not counted) while an expired one
//     resolves attributed (gate_unverified_expired, counted), so processed is
//     exactly 1. A pending hold re-counted every pass would make the stage loop
//     re-run the pass at once over the same unreadable rows (D-D5: nothing
//     retries faster than the sweep); the expired control keeps a do-nothing
//     pass from passing.
//
// The held row is inserted directly: the thing under test is the ADAPTER's
// error mapping and processed count, not how held rows arise (the capture suite
// covers that through the column).
//
// GREENFIELD NOTE — EXPECTED RED: no gate entry in stageImpls (fatal at lookup),
// and the held INSERT violates capture_decisions_action_check until migration
// 0029 is applied.
//
// Cross-suite discipline: deletes capture_decisions WHOLESALE (the capture
// suites' precedent), compose db only. Owns project itest-pipegate, account
// (google, itest-pipegate@pipegate.example.test), threads gmail:itest-pipegate:%.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/pipeline"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	pgSlug    = "itest-pipegate"
	pgAccount = "itest-pipegate@pipegate.example.test"
	pgLockKey = int64(0x5157_0015)
)

func pgSetup(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db (cleanup deletes capture_decisions " +
			"wholesale); use the compose db on :5433")
	}
	t.Setenv("OPS_TOKEN_KEY", "") // no credential: the pass's lookup factory is nil
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	pgCleanup(t, ctx, pool)
	t.Cleanup(func() { pgCleanup(t, ctx, pool) })

	scalar := func(q string, args ...any) int64 {
		var v int64
		if err := pool.QueryRow(ctx, q, args...).Scan(&v); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
		return v
	}
	project := scalar(`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality, ticket_assignee_gate)
	                   VALUES ($1,$1,'itest-pipegate-client','manual','dashboard','/tmp/itest-pipegate','any',true) RETURNING id`, pgSlug)
	rule := scalar(`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, priority, enabled, note)
	                VALUES ($1,'body_regex','PGT-[0-9]+','jira',100,true,'itest-pipegate') RETURNING id`, project)
	acct := scalar(`INSERT INTO source_accounts (provider, account_email, scopes, send_enabled, calendar_in_availability)
	                VALUES ('google',$1,'{}',false,false) RETURNING id`, pgAccount)
	thread := scalar(`INSERT INTO normalized_threads (thread_key, subject, participants)
	                  VALUES ('gmail:itest-pipegate:1','itest','[]') RETURNING id`)
	raw := scalar(`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
	               VALUES ($1,'itest-pipegate-1','{}','itest-pipegate-h1', now()) RETURNING id`, acct)
	msg := scalar(`INSERT INTO normalized_messages
	                 (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
	               VALUES ($1,$2,'inbound','<itest-pipegate-1@x>', now() - interval '5 minutes',
	                       'look at PGT-1','PGT-1','Someone <s@pipegate.example.test>','gmail') RETURNING id`, raw, thread)
	scalar(`INSERT INTO capture_decisions (message_id, raw_source_item_id, mode, matched_rule_id, project_id, action,
	                                       external_system, external_key, reason)
	        VALUES ($1,$2,'live',$3,$4,'held','jira','PGT-1','itest-pipegate held') RETURNING id`, msg, raw, rule, project)
	// The control: a hold whose MESSAGE is older than capture.GateMaxAge (72h).
	raw2 := scalar(`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
	                VALUES ($1,'itest-pipegate-2','{}','itest-pipegate-h2', now()) RETURNING id`, acct)
	msg2 := scalar(`INSERT INTO normalized_messages
	                  (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
	                VALUES ($1,$2,'inbound','<itest-pipegate-2@x>', now() - interval '73 hours',
	                        'look at PGT-2','PGT-2','Someone <s@pipegate.example.test>','gmail') RETURNING id`, raw2, thread)
	scalar(`INSERT INTO capture_decisions (message_id, raw_source_item_id, mode, matched_rule_id, project_id, action,
	                                       external_system, external_key, reason)
	        VALUES ($1,$2,'live',$3,$4,'held','jira','PGT-2','itest-pipegate held (expired)') RETURNING id`, msg2, raw2, rule, project)
	return pool
}

func pgCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const acct = `(SELECT id FROM source_accounts WHERE provider='google' AND account_email='` + pgAccount + `')`
	const projs = `(SELECT id FROM projects WHERE slug='` + pgSlug + `')`
	for _, q := range []string{
		`DELETE FROM capture_decisions`,
		`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE actor='capture:gate')`,
		`DELETE FROM audit_events WHERE actor='capture:gate'`,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM capture_rules WHERE project_id IN ` + projs,
		`DELETE FROM projects WHERE slug='` + pgSlug + `'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + acct + `)`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'gmail:itest-pipegate:%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + acct,
		`DELETE FROM source_accounts WHERE provider='google' AND account_email='` + pgAccount + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func gateStagePass(t *testing.T, pool *pgxpool.Pool) pipeline.PassFunc {
	t.Helper()
	impl, ok := stageImpls[pipeline.StageGate]
	if !ok || impl.pass == nil {
		t.Fatalf("stageImpls has no gate stage; Part D registers it (D-D4)")
	}
	return impl.pass(pool)
}

func TestPipelinedGate_Integration_LockHeldIsErrLockHeld(t *testing.T) {
	ctx := context.Background()
	pool := pgSetup(t, ctx)
	pass := gateStagePass(t, pool)

	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer holder.Release()
	var taken bool
	if err := holder.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, pgLockKey).Scan(&taken); err != nil || !taken {
		t.Fatalf("take 0x%X: taken=%v err=%v", pgLockKey, taken, err)
	}
	defer func() {
		var ok bool
		_ = holder.QueryRow(context.Background(), `SELECT pg_advisory_unlock($1)`, pgLockKey).Scan(&ok)
	}()

	n, err := pass(ctx)
	if !errors.Is(err, pipeline.ErrLockHeld) {
		t.Errorf("gate pass with capture's lock 0x%X held elsewhere: err = %v, want errors.Is(err, "+
			"pipeline.ErrLockHeld) — the loop's retry-later, never a logged failure (E-D4)", pgLockKey, err)
	}
	if n != 0 {
		t.Errorf("processed = %d while the lock was held, want 0", n)
	}
}

func TestPipelinedGate_Integration_ProcessedCountsResolutionsNotPendingHolds(t *testing.T) {
	ctx := context.Background()
	pool := pgSetup(t, ctx)
	pass := gateStagePass(t, pool)

	n, err := pass(ctx)
	if err != nil {
		t.Fatalf("gate pass: %v", err)
	}
	if n != 1 {
		t.Errorf("processed = %d, want exactly 1: the expired hold resolves (attributed, gate_unverified_expired) "+
			"and counts; the fresh unreadable hold stays held and must NOT count — the stage loop repeats a pass "+
			"at once while it reports >= its limit, so a re-counted pending hold would re-try faster than the "+
			"sweep (D-D5)", n)
	}
	var gateRows, pendingKey int
	if err := pool.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE external_key='PGT-1') FROM capture_decisions WHERE mode='gate'`).
		Scan(&gateRows, &pendingKey); err != nil {
		t.Fatalf("count gate rows: %v", err)
	}
	if gateRows != 1 || pendingKey != 0 {
		t.Errorf("gate rows = %d (PGT-1: %d), want exactly 1, for the expired PGT-2 only — with no credential "+
			"the fresh hold stays held", gateRows, pendingKey)
	}
}
