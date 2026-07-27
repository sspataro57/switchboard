//go:build integration

package slackweb_test

// Database integration walk for the Slack Web connector. The browser is
// replaced by a normalized export source; Playwright behavior is tested in the
// TypeScript project. This suite proves raw-first persistence, deterministic
// normalization, own-message direction, idempotent re-ingest, and assisted
// delivery loop closure without requiring a Slack account.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	itWorkspaceID = "TITEST"
	itChannelID   = "CITEST"
	itOwnUserID   = "UITESTOWN"
	itAccount     = "titest@slack-web.local"
	itThreadKey   = "slack:TITEST:CITEST"
	itTarget      = "https://app.slack.com/client/TITEST/CITEST"
	itSlug        = "itest-slack-web-proj"
	itOutbound    = "the Slack Web connector integration reply"
)

type fixtureSource struct{}

func (fixtureSource) Export(context.Context) (slackweb.Export, error) {
	return slackweb.Export{
		SchemaVersion: slackweb.SchemaVersion,
		Workspaces: []slackweb.Workspace{{
			ID: itWorkspaceID, Name: "Integration Slack", URL: "https://app.slack.com/client/" + itWorkspaceID,
			OwnUserID: itOwnUserID,
			Conversations: []slackweb.Conversation{{
				ID: itChannelID, Name: "integration", Type: "public_channel", URL: itTarget,
				Messages: []slackweb.Message{
					{ID: "p1784800800000001", Timestamp: "2026-07-23T14:00:00Z", Author: "Client", AuthorID: "UITESTCLIENT", Text: "please test the Slack bridge"},
					{ID: "p1784800860000002", Timestamp: "2026-07-23T14:01:00Z", Author: "Salvo", AuthorID: itOwnUserID, Text: itOutbound},
				},
			}},
		}},
	}, nil
}

func slackScanInt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var value int
	if err := pool.QueryRow(ctx, query, args...).Scan(&value); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return value
}

func cleanupSlackWeb(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	queries := []string{
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='slack_web' AND account_email='` + itAccount + `'))`,
		`DELETE FROM task_events WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + itSlug + `'))`,
		`DELETE FROM deliveries WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + itSlug + `'))`,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + itSlug + `')`,
		`DELETE FROM projects WHERE slug='` + itSlug + `'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='slack_web' AND account_email='` + itAccount + `'))`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE '` + itThreadKey + `%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='slack_web' AND account_email='` + itAccount + `')`,
		`DELETE FROM sync_runs WHERE source_account_id IN (SELECT id FROM source_accounts WHERE provider='slack_web' AND account_email='` + itAccount + `')`,
		`DELETE FROM source_accounts WHERE provider='slack_web' AND account_email='` + itAccount + `'`,
	}
	for _, query := range queries {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatalf("cleanup %q: %v", query, err)
		}
	}
}

func TestSlackWeb_Integration_RawNormalizeLoopClosure(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must never run against the real ops database")
	}
	ctx := context.Background()
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	defer pool.Close()
	cleanupSlackWeb(t, ctx, pool)
	defer cleanupSlackWeb(t, ctx, pool)

	var projectID, taskID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path)
		 VALUES ($1,$1,'itest-slack-web','manual','dashboard','/tmp/itest') RETURNING id`, itSlug).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'Slack integration work','claude','delivered') RETURNING id`, projectID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	var deliveryID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, sent_at)
		 VALUES ($1,'slack_reply',$2,$3,'sent',now()) RETURNING id`, taskID, itTarget, itOutbound).Scan(&deliveryID); err != nil {
		t.Fatalf("seed slack_reply delivery (apply migration 0009): %v", err)
	}

	sink := slackweb.NewSink(pool)
	stats, err := slackweb.Ingest(ctx, fixtureSource{}, sink)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if stats.RawInserted != 3 {
		t.Fatalf("raw inserted = %d, want 3 (conversation + two messages)", stats.RawInserted)
	}
	if got := slackScanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items WHERE source_account_id=(SELECT id FROM source_accounts WHERE provider='slack_web' AND account_email=$1) AND normalized_at IS NULL`, itAccount); got != 3 {
		t.Fatalf("pending raw after ingest = %d, want 3 (raw-first boundary)", got)
	}
	if got := slackScanInt(t, ctx, pool, `SELECT count(*) FROM normalized_messages WHERE channel='slack' AND external_message_id LIKE 'slack:TITEST:%'`); got != 0 {
		t.Fatalf("normalized Slack messages before Normalize = %d, want 0", got)
	}

	stats, err = slackweb.Normalize(ctx, sink, slackweb.Config{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if stats.Normalized != 3 {
		t.Errorf("normalized = %d, want 3", stats.Normalized)
	}
	if got := slackScanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_messages WHERE thread_id=(SELECT id FROM normalized_threads WHERE thread_key=$1) AND channel='slack'`, itThreadKey); got != 2 {
		t.Errorf("Slack messages = %d, want 2", got)
	}
	if got := slackScanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_messages WHERE external_message_id='slack:TITEST:CITEST:p1784800800000001' AND direction='inbound'`); got != 1 {
		t.Errorf("client message must enter the inbound triage lane; got %d", got)
	}
	if got := slackScanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_messages WHERE external_message_id='slack:TITEST:CITEST:p1784800860000002' AND direction='outbound'`); got != 1 {
		t.Errorf("own message must be outbound; got %d", got)
	}
	var sentExternalID, confirmedAt *string
	if err := pool.QueryRow(ctx,
		`SELECT sent_external_id, confirmed_at::text FROM deliveries WHERE id=$1`, deliveryID).Scan(&sentExternalID, &confirmedAt); err != nil {
		t.Fatalf("read confirmed delivery: %v", err)
	}
	if sentExternalID == nil || *sentExternalID != "slack:TITEST:CITEST:p1784800860000002" || confirmedAt == nil {
		t.Errorf("Slack loop closure sent_external_id=%v confirmed_at=%v", sentExternalID, confirmedAt)
	}

	stats, err = slackweb.Ingest(ctx, fixtureSource{}, sink)
	if err != nil {
		t.Fatalf("second Ingest: %v", err)
	}
	if stats.RawUnchanged != 3 || stats.RawInserted != 0 || stats.RawUpdated != 0 {
		t.Errorf("second ingest stats = %+v, want exactly 3 unchanged", stats)
	}
	stats, err = slackweb.Normalize(ctx, sink, slackweb.Config{})
	if err != nil || stats.Normalized != 0 {
		t.Errorf("second normalize stats=%+v err=%v, want no pending work", stats, err)
	}
}
