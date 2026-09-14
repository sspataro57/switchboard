//go:build integration

package main

// SWT-54 criterion 17, the wiring half: `opsctl capture-rules try` takes
// capture-rules add's flags plus --since and --show, prints the rollup, and
// writes nothing. The capture suite covers the outcomes in depth
// (internal/capture/dryrun_integration_test.go); this is flags, pool and stdout.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_isopr?sslmode=disable TZ=UTC \
//	  go test -tags integration -p 1 -count=1 -run OpsctlTry ./cmd/opsctl/
//
// Cleanup deletes capture_decisions WHOLESALE (the capture suites' precedent),
// private database only.
//
// RED TODAY: runCaptureRules has no "try" verb ("unknown capture-rules command").

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/store"
)

const (
	otSlug    = "itest-opstry"
	otAccount = "itest-opstry@gmail.example.test"
	otRepo    = "treetopllc/itest-opstry-www"
	otKeyRe   = `<(treetopllc/[A-Za-z0-9._-]+/pull/[0-9]+)@github\.com>$`
)

func otCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const acct = `(SELECT id FROM source_accounts WHERE provider='google' AND account_email='` + otAccount + `')`
	for _, q := range []string{
		`DELETE FROM capture_decisions`,
		`DELETE FROM capture_rules WHERE project_id IN (SELECT id FROM projects WHERE slug='` + otSlug + `')`,
		`DELETE FROM projects WHERE slug='` + otSlug + `'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + acct + `)`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'gmail:` + otAccount + `:%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + acct,
		`DELETE FROM source_accounts WHERE provider='google' AND account_email='` + otAccount + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func TestOpsctlTry_Integration_PrintsTheRollupAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use a private compose database on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	otCleanup(t, ctx, pool)
	t.Cleanup(func() { otCleanup(t, ctx, pool) })

	scalar := func(q string, args ...any) int64 {
		var v int64
		if err := pool.QueryRow(ctx, q, args...).Scan(&v); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
		return v
	}
	scalar(`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
	        VALUES ($1,$1,'itest-opstry-client','manual','dashboard','/tmp/itest-opstry','any') RETURNING id`, otSlug)
	acct := scalar(`INSERT INTO source_accounts (provider, account_email, scopes, send_enabled, calendar_in_availability)
	                VALUES ('google',$1,'{}',false,false) RETURNING id`, otAccount)
	root := fmt.Sprintf("<%s/pull/9951@github.com>", otRepo)
	thread := scalar(`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,$1,'[]') RETURNING id`,
		"gmail:"+otAccount+":"+root)
	subject := "[" + otRepo + "] Ranking widget (PR #9951)"
	rfc := "From: joseg-avviato <notifications@github.com>\r\n" +
		"To: " + otRepo + " <itest-opstry-www@noreply.github.com>\r\n" +
		"Subject: " + subject + "\r\n" +
		"Message-ID: " + root + "\r\n" +
		"Date: Mon, 14 Sep 2026 12:00:00 +0000\r\n" +
		"X-GitHub-Reason: subscribed\r\n" +
		"X-GitHub-Sender: joseg-avviato\r\n" +
		"X-GitHub-Recipient: sspataro57\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n\r\n" +
		"joseg-avviato opened this pull request.\r\n"
	env, err := json.Marshal(map[string]any{
		"source": "imap", "folder": "INBOX", "uidvalidity": 1, "uid": 99511, "internaldate": "2026-09-14T12:00:01Z",
		"flags": []string{}, "size": len(rfc), "truncated": false, "rfc822_b64": base64.StdEncoding.EncodeToString([]byte(rfc)),
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	raw := scalar(`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
	               VALUES ($1,'itest-opstry-1',$2::jsonb,'itest-opstry-h-1', now()) RETURNING id`, acct, string(env))
	scalar(`INSERT INTO normalized_messages
	          (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
	        VALUES ($1,$2,'inbound',$3, now() - interval '5 minutes', 'joseg-avviato opened this pull request.', $4,
	                'joseg-avviato <notifications@github.com>','gmail') RETURNING id`, raw, thread, root, subject)

	tables := []string{"capture_decisions", "capture_rules", "tasks", "external_refs", "task_events", "audit_events"}
	counts := func() map[string]int64 {
		out := map[string]int64{}
		for _, tb := range tables {
			out[tb] = scalar(`SELECT count(*) FROM ` + tb)
		}
		return out
	}
	before := counts()

	out, err := captureStdout(t, func() error {
		return runCaptureRules("try", []string{
			"--project", otSlug, "--type", "thread_key_contains", "--pattern", "<treetopllc/",
			"--external-system", "github", "--key-regex", otKeyRe, "--priority", "91", "--pr-review",
			"--since", "720h", "--show", "all",
		})
	})
	if err != nil {
		t.Fatalf("opsctl capture-rules try: %v\noutput:\n%s", err, out)
	}
	after := counts()
	for _, tb := range tables {
		if after[tb] != before[tb] {
			t.Errorf("%s changed across `capture-rules try`: %d -> %d (criterion 17)", tb, before[tb], after[tb])
		}
	}
	for _, want := range []string{otRepo + "#9951", "joseg-avviato", "Review PR #9951 — itest-opstry-www: Ranking widget"} {
		if !strings.Contains(out, want) {
			t.Errorf("try output lacks %q\noutput:\n%s", want, out)
		}
	}

	// The verb is listed where the others are.
	if err := runCaptureRules("bogus", nil); err == nil || !strings.Contains(err.Error(), "try") {
		t.Errorf("unknown-verb error %v does not list try", err)
	}
}
