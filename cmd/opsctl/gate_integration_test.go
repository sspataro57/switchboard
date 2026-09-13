//go:build integration

package main

// SWT-40 Part D review fix 2, end to end through the command: `opsctl
// capture-rules gate --dry-run [--shadow]` against the compose db. It prints one
// line per hold and writes nothing. The capture suite covers the outcomes in
// depth (gate_freshness_integration_test.go); this is the wiring: flags, pool,
// the mode switch and stdout.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run OpsctlGate ./cmd/opsctl/
//
// The held rows are inserted directly (the pipelined gate suite's precedent):
// the thing under test is the command, not how held rows arise. Cleanup deletes
// capture_decisions WHOLESALE (the capture suites' precedent), compose db only.

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/store"
)

const (
	ogSlug    = "itest-opsgate"
	ogAccount = "itest-opsgate@opsgate.example.test"
)

func ogCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const acct = `(SELECT id FROM source_accounts WHERE provider='google' AND account_email='` + ogAccount + `')`
	const projs = `(SELECT id FROM projects WHERE slug='` + ogSlug + `')`
	for _, q := range []string{
		`DELETE FROM capture_decisions`,
		`DELETE FROM capture_rules WHERE project_id IN ` + projs,
		`DELETE FROM projects WHERE slug='` + ogSlug + `'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + acct + `)`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'gmail:itest-opsgate:%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + acct,
		`DELETE FROM source_accounts WHERE provider='google' AND account_email='` + ogAccount + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	runErr := fn()
	os.Stdout = orig
	_ = w.Close()
	return <-done, runErr
}

func TestOpsctlGate_Integration_DryRunPrintsAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use the compose db on :5433")
	}
	t.Setenv("OPS_TOKEN_KEY", "")
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	ogCleanup(t, ctx, pool)
	t.Cleanup(func() { ogCleanup(t, ctx, pool) })

	scalar := func(q string, args ...any) int64 {
		var v int64
		if err := pool.QueryRow(ctx, q, args...).Scan(&v); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
		return v
	}
	project := scalar(`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality, ticket_assignee_gate)
	                   VALUES ($1,$1,'itest-opsgate-client','manual','dashboard','/tmp/itest-opsgate','any',true) RETURNING id`, ogSlug)
	rule := scalar(`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, priority, enabled, note)
	                VALUES ($1,'body_regex','OGT-[0-9]+','jira',100,true,'itest-opsgate') RETURNING id`, project)
	acct := scalar(`INSERT INTO source_accounts (provider, account_email, scopes, send_enabled, calendar_in_availability)
	                VALUES ('google',$1,'{}',false,false) RETURNING id`, ogAccount)
	thread := scalar(`INSERT INTO normalized_threads (thread_key, subject, participants)
	                  VALUES ('gmail:itest-opsgate:1','itest','[]') RETURNING id`)
	hold := func(label, key, mode string) {
		raw := scalar(`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		               VALUES ($1,$2,'{}',$3, now()) RETURNING id`, acct, "itest-opsgate-"+label, "itest-opsgate-h-"+label)
		msg := scalar(`INSERT INTO normalized_messages
		                 (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		               VALUES ($1,$2,'inbound',$3, now() - interval '5 minutes', $4, $4, 'Someone <s@opsgate.example.test>','gmail')
		               RETURNING id`, raw, thread, "<itest-opsgate-"+label+"@x>", "look at "+key)
		scalar(`INSERT INTO capture_decisions (message_id, raw_source_item_id, mode, matched_rule_id, project_id, action,
		                                       external_system, external_key, reason)
		        VALUES ($1,$2,$3,$4,$5,'held','jira',$6,'itest-opsgate held') RETURNING id`, msg, raw, mode, rule, project, key)
	}
	hold("live", "OGT-1", "live")
	hold("shadow", "OGT-2", "shadow")

	counts := func() string {
		var s string
		if err := pool.QueryRow(ctx, `SELECT concat_ws(',',
		    (SELECT count(*) FROM capture_decisions), (SELECT count(*) FROM tasks),
		    (SELECT count(*) FROM raw_source_items), (SELECT count(*) FROM audit_events))`).Scan(&s); err != nil {
			t.Fatalf("counts: %v", err)
		}
		return s
	}
	before := counts()

	out, err := captureStdout(t, func() error { return runCaptureRules("gate", []string{"--dry-run"}) })
	if err != nil {
		t.Fatalf("capture-rules gate --dry-run: %v\n%s", err, out)
	}
	t.Logf("--dry-run:\n%s", out)
	if !strings.Contains(out, "key=OGT-1 outcome=pending_lookup") {
		t.Errorf("--dry-run output has no pending_lookup line for the live hold OGT-1 (no stored snapshot):\n%s", out)
	}
	if strings.Contains(out, "OGT-2") {
		t.Errorf("--dry-run without --shadow printed the SHADOW hold OGT-2:\n%s", out)
	}

	out, err = captureStdout(t, func() error { return runCaptureRules("gate", []string{"--dry-run", "--shadow"}) })
	if err != nil {
		t.Fatalf("capture-rules gate --dry-run --shadow: %v\n%s", err, out)
	}
	t.Logf("--dry-run --shadow:\n%s", out)
	if !strings.Contains(out, "key=OGT-2 outcome=pending_lookup") || strings.Contains(out, "OGT-1") {
		t.Errorf("--dry-run --shadow output wants the shadow hold OGT-2 only:\n%s", out)
	}

	if after := counts(); after != before {
		t.Errorf("the dry runs wrote: counts %s -> %s", before, after)
	}
}
