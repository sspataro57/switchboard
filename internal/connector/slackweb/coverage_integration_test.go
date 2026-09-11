//go:build integration

package slackweb_test

// Integration regression tests for bug slackweb-collab-export-stale (Jira
// SWT-39): fixes D (coverage in sync_runs.stats, status 'partial'), E
// (ReconcileUnconfirmed counts only passes that READ the target conversation)
// and F (the known set is loaded from raw_source_items).
// docs/bugs/slackweb-collab-export-stale_DIAGNOSIS.md.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run SWT39 ./internal/connector/slackweb/
//
// Build-tagged `integration` AND env-gated on DATABASE_URL, FATAL on
// 192.168.50.49. No Slack, no browser: exports are leaf-shaped fixtures (see
// coverage_test.go's builders, compiled into this build too).
//
// Why integration: the reconciler's guard is fed by a COLUMN (sync_runs.stats)
// that Ingest writes. A fixture-only test would prove the reader against a
// shape the writer may never produce, so
// TestRegression_SWT39_ReconcileUsesTheReadSetIngestWrote binds the two.
//
// Cross-suite discipline: this suite owns every source_accounts row whose
// account_email starts with 'tswt39', and the project 'itest-swt39-proj'. It
// cleans its own corpus in FK order, at start and end, so it can be rerun on
// a persistent db.
//
// EXPECTED RED, in order:
//   - compile failure (see coverage_test.go / export_request_test.go);
//   - once it compiles, on a db still at 0026: every test that writes
//     'partial' stops at swt39RequirePartialStatus;
//   - after 0027: the reconcile tests flag on passes that never read the
//     target, and the stats/known-set assertions find nothing.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	swt39Slug = "itest-swt39-proj"

	// Reconciler corpus.
	swt39RCWorkspace = "TSWT39RC"
	swt39RCAccount   = "tswt39rc@slack-web.local"
	swt39TargetConv  = "DSWT39TGT"
	swt39Body        = "SWT-39 reply that no export ever confirmed"

	// Ingest corpus.
	swt39INWorkspace = "TSWT39IN"
	swt39INAccount   = "tswt39in@slack-web.local"

	// Known-set corpus: two slack_web workspaces and one foreign provider.
	swt39KNWorkspace = "TSWT39KN"
	swt39KNAccount   = "tswt39kn@slack-web.local"
	swt39KBWorkspace = "TSWT39KB"
	swt39KBAccount   = "tswt39kb@slack-web.local"
	swt39FOAccount   = "tswt39-foreign@local"

	swt39MGAccount = "tswt39mg@slack-web.local"
)

func newSWT39Pool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must never run against the real ops database")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	cleanupSWT39(t, ctx, pool)
	t.Cleanup(func() { cleanupSWT39(t, context.Background(), pool) })
	return pool
}

func cleanupSWT39(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE account_email LIKE 'tswt39%')`
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`
	const tasks = `(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + swt39Slug + `'))`
	for _, q := range []string{
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM capture_decisions WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM task_events WHERE task_id IN ` + tasks,
		`DELETE FROM deliveries WHERE task_id IN ` + tasks,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + swt39Slug + `')`,
		`DELETE FROM projects WHERE slug='` + swt39Slug + `'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE account_email LIKE 'tswt39%'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

// swt39RequirePartialStatus fails with one sentence when migration 0027 is not
// applied, instead of letting a CHECK violation surface as a confusing seed or
// FinishRun error. Probes inside a rolled-back transaction.
func swt39RequirePartialStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin probe: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var accountID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, send_enabled, calendar_in_availability)
		 VALUES ('slack_web','tswt39probe@slack-web.local',false,false) RETURNING id`).Scan(&accountID); err != nil {
		t.Fatalf("probe account: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO sync_runs (source_account_id, status) VALUES ($1,'partial')`, accountID); err != nil {
		t.Fatalf("sync_runs rejects status 'partial' (%v). Apply migration 0027 (go run ./cmd/tools/migrate "+
			"--dir migrations against the compose db): SWT-39 records a run whose leaf deferred or could not "+
			"read conversations as 'partial', not 'ok'", err)
	}
}

func swt39Account(t *testing.T, ctx context.Context, pool *pgxpool.Pool, provider, email string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, send_enabled, calendar_in_availability)
		 VALUES ($1,$2,false,false) RETURNING id`, provider, email).Scan(&id); err != nil {
		t.Fatalf("seed account %s: %v", email, err)
	}
	return id
}

// swt39SeedRun writes one COMPLETED pass that started `startedAgo` before now.
// stats is the literal jsonb the run carries.
func swt39SeedRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, accountID int64, startedAgo, status, stats string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO sync_runs (source_account_id, started_at, finished_at, status, stats)
		 VALUES ($1, now() - $2::interval, now() - $2::interval + interval '1 minute', $3, $4::jsonb)`,
		accountID, startedAgo, status, stats); err != nil {
		t.Fatalf("seed sync_run (%s ago, %s, %s): %v", startedAgo, status, stats, err)
	}
}

// swt39SeedDelivery is a 'sent' slack_reply to targetRef, 30 minutes ago, never
// confirmed: the row ReconcileUnconfirmed exists to flag.
func swt39SeedDelivery(t *testing.T, ctx context.Context, pool *pgxpool.Pool, targetRef string) (taskID, deliveryID int64) {
	t.Helper()
	var projectID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,'itest-swt39','manual','dashboard','/tmp/itest','any') RETURNING id`, swt39Slug).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'SWT-39 unconfirmed reply','claude','delivered') RETURNING id`, projectID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, sent_at, approval_source)
		 VALUES ($1,'slack_reply',$2,$3,'sent', now() - interval '30 minutes', 'switchboard') RETURNING id`,
		taskID, targetRef, swt39Body).Scan(&deliveryID); err != nil {
		t.Fatalf("seed slack_reply delivery: %v", err)
	}
	return taskID, deliveryID
}

func swt39Reconcile(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	flagged, err := slackweb.ReconcileUnconfirmed(ctx, slackweb.NewSink(pool), 3)
	if err != nil {
		t.Fatalf("ReconcileUnconfirmed: %v", err)
	}
	return flagged
}

func swt39UnconfirmedPasses(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64) (events int, passes float64) {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT payload::text FROM task_events WHERE task_id=$1 AND event_type='delivery_unconfirmed'`, taskID)
	if err != nil {
		t.Fatalf("read delivery_unconfirmed: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan payload: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			t.Fatalf("decode payload %s: %v", raw, err)
		}
		events++
		passes, _ = payload["passes"].(float64)
	}
	return events, passes
}

// ---- D: migration 0027 --------------------------------------------------------

func TestRegression_SWT39_SyncRunsAcceptPartialStatus(t *testing.T) {
	ctx := context.Background()
	pool := newSWT39Pool(t, ctx)
	accountID := swt39Account(t, ctx, pool, "slack_web", swt39MGAccount)

	// Positive control: the fixture itself can write a run.
	if _, err := pool.Exec(ctx, `INSERT INTO sync_runs (source_account_id, status) VALUES ($1,'ok')`, accountID); err != nil {
		t.Fatalf("POSITIVE CONTROL FAILED: cannot insert an 'ok' run: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sync_runs (source_account_id, status) VALUES ($1,'partial')`, accountID); err != nil {
		t.Errorf("sync_runs rejects status 'partial': %v. Migration 0027 must widen sync_runs_status_check", err)
	}
	// The CHECK is widened, not dropped.
	if _, err := pool.Exec(ctx, `INSERT INTO sync_runs (source_account_id, status) VALUES ($1,'bogus')`, accountID); err == nil {
		t.Errorf("sync_runs accepted status 'bogus': 0027 dropped the CHECK instead of widening it")
	}
}

// ---- D: Ingest writes coverage into the column ----------------------------------

func TestRegression_SWT39_IngestStoresCoverageInSyncRunsStats(t *testing.T) {
	ctx := context.Background()
	pool := newSWT39Pool(t, ctx)
	swt39RequirePartialStatus(t, ctx, pool)
	sink := slackweb.NewSink(pool)

	unreadable := []map[string]string{{"id": "DSWT39BAD", "name": "asunda45", "reason": "conversation did not open"}}
	partial := &swt39Source{export: swt39Export(t, swt39Workspace(t, swt39INWorkspace, []string{"CSWT39IN1"},
		swt39CoverageFields(t, swt39INWorkspace, []string{"CSWT39IN1"}, []string{"DSWT39DEF"}, unreadable, 38, false)))}
	if _, err := slackweb.Ingest(ctx, partial, sink); err != nil {
		t.Fatalf("Ingest (partial coverage): %v", err)
	}

	var status, phase string
	var finished bool
	var read, deferred, unreadableID, unreadableReason, enumerated, exhausted *string
	if err := pool.QueryRow(ctx,
		`SELECT r.status, r.finished_at IS NOT NULL, COALESCE(r.stats->>'phase',''),
		        (r.stats->'read')::text, (r.stats->'deferred')::text,
		        r.stats->'unreadable'->0->>'id', r.stats->'unreadable'->0->>'reason',
		        r.stats->'coverage'->>'enumerated_count', r.stats->'coverage'->>'budget_exhausted'
		   FROM sync_runs r JOIN source_accounts a ON a.id = r.source_account_id
		  WHERE a.account_email = $1 ORDER BY r.id DESC LIMIT 1`, swt39INAccount).
		Scan(&status, &finished, &phase, &read, &deferred, &unreadableID, &unreadableReason, &enumerated, &exhausted); err != nil {
		t.Fatalf("read the run Ingest wrote: %v", err)
	}
	if status != "partial" || !finished {
		t.Errorf("run status = %q (finished=%v), want a finished 'partial' run: one conversation deferred and "+
			"one unreadable, so this run did not read everything in scope", status, finished)
	}
	if phase != "slack_web" {
		t.Errorf("stats.phase = %q, want slack_web kept (the dashboard groups by it)", phase)
	}
	var readIDs []string
	if read == nil || json.Unmarshal([]byte(*read), &readIDs) != nil || !reflect.DeepEqual(readIDs, []string{"CSWT39IN1"}) {
		t.Errorf("stats.read = %v, want [\"CSWT39IN1\"] in the column ReconcileUnconfirmed reads", read)
	}
	if deferred == nil || !strings.Contains(*deferred, `"DSWT39DEF"`) {
		t.Errorf("stats.deferred = %v, want it to hold DSWT39DEF", deferred)
	}
	if unreadableID == nil || *unreadableID != "DSWT39BAD" || unreadableReason == nil || *unreadableReason != "conversation did not open" {
		t.Errorf("stats.unreadable[0] = id %v reason %v, want DSWT39BAD / 'conversation did not open'. Before "+
			"SWT-39 this failure was a warn in the mini's log and nowhere else", unreadableID, unreadableReason)
	}
	if enumerated == nil || *enumerated != "3" || exhausted == nil || *exhausted != "false" {
		t.Errorf("stats.coverage enumerated_count=%v budget_exhausted=%v, want 3 / false", enumerated, exhausted)
	}
	if n := slackScanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items r JOIN source_accounts a ON a.id=r.source_account_id WHERE a.account_email=$1`,
		swt39INAccount); n != 2 {
		t.Errorf("raw rows = %d, want 2: what WAS read is still ingested raw-first", n)
	}

	// The same workspace read in full finishes 'ok'.
	full := &swt39Source{export: swt39Export(t, swt39Workspace(t, swt39INWorkspace, []string{"CSWT39IN1"},
		swt39CoverageFields(t, swt39INWorkspace, []string{"CSWT39IN1"}, nil, nil, 1, false)))}
	if _, err := slackweb.Ingest(ctx, full, sink); err != nil {
		t.Fatalf("Ingest (full coverage): %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT r.status FROM sync_runs r JOIN source_accounts a ON a.id = r.source_account_id
		  WHERE a.account_email = $1 ORDER BY r.id DESC LIMIT 1`, swt39INAccount).Scan(&status); err != nil {
		t.Fatalf("read the second run: %v", err)
	}
	if status != "ok" {
		t.Errorf("fully read run status = %q, want ok", status)
	}
}

// ---- E: ReconcileUnconfirmed counts only passes that read the target ---------------

// The diagnosis's invariant-5 consequence, verbatim: "A reply sent to asunda45
// during the freeze would have been flagged delivery_unconfirmed after 3
// passes although it had landed." Those passes never read asunda45.
func TestRegression_SWT39_ReconcileCountsOnlyPassesThatReadTheTarget(t *testing.T) {
	ctx := context.Background()
	pool := newSWT39Pool(t, ctx)
	accountID := swt39Account(t, ctx, pool, "slack_web", swt39RCAccount)
	// A THREAD target: the conversation id comes from ParseTargetURL, not from
	// comparing target_ref strings.
	taskID, _ := swt39SeedDelivery(t, ctx, pool,
		"https://app.slack.com/client/"+swt39RCWorkspace+"/"+swt39TargetConv+"/p1789073346665869")

	// Three completed 'ok' passes after the send, none of which read the target.
	// "DSWT39TGT2" is a different conversation whose id merely starts with the
	// target's; a substring match on the stats text would count it.
	swt39SeedRun(t, ctx, pool, accountID, "25 minutes", "ok", `{"phase":"slack_web","read":["CSWT39OTH"]}`)
	swt39SeedRun(t, ctx, pool, accountID, "22 minutes", "ok", `{"phase":"slack_web","read":[]}`)
	swt39SeedRun(t, ctx, pool, accountID, "19 minutes", "ok", `{"phase":"slack_web","read":["DSWT39TGT2","CSWT39OTH"]}`)

	if flagged := swt39Reconcile(t, ctx, pool); flagged != 0 {
		t.Fatalf("flagged = %d after 3 `ok` passes that never READ %s, want 0. A pass that did not export "+
			"the conversation could not have observed the send; flagging it tells a human a reply may have "+
			"failed when the fact is the poller never looked (SWT-39)", flagged, swt39TargetConv)
	}

	// Three that did.
	swt39SeedRun(t, ctx, pool, accountID, "15 minutes", "ok", `{"phase":"slack_web","read":["DSWT39TGT"]}`)
	swt39SeedRun(t, ctx, pool, accountID, "12 minutes", "ok", `{"phase":"slack_web","read":["CSWT39OTH","DSWT39TGT"]}`)
	swt39SeedRun(t, ctx, pool, accountID, "9 minutes", "ok", `{"phase":"slack_web","read":["DSWT39TGT"]}`)

	if flagged := swt39Reconcile(t, ctx, pool); flagged != 1 {
		t.Fatalf("flagged = %d after 3 passes that read %s, want 1", flagged, swt39TargetConv)
	}
	if events, passes := swt39UnconfirmedPasses(t, ctx, pool, taskID); events != 1 || passes != 3 {
		t.Errorf("delivery_unconfirmed events=%d passes=%v, want 1 event reporting 3 passes: only the passes "+
			"that read the target are passes", events, passes)
	}
}

// 'partial' runs count exactly like 'ok' ones, subject to the same read-set
// rule: a partial run that read the target conversation DID observe it.
func TestRegression_SWT39_ReconcileCountsPartialPassesThatReadTheTarget(t *testing.T) {
	ctx := context.Background()
	pool := newSWT39Pool(t, ctx)
	swt39RequirePartialStatus(t, ctx, pool)
	accountID := swt39Account(t, ctx, pool, "slack_web", swt39RCAccount)
	taskID, _ := swt39SeedDelivery(t, ctx, pool, "https://app.slack.com/client/"+swt39RCWorkspace+"/"+swt39TargetConv)

	const notRead = `{"phase":"slack_web","read":["CSWT39OTH"],"deferred":["DSWT39TGT"]}`
	swt39SeedRun(t, ctx, pool, accountID, "25 minutes", "partial", notRead)
	swt39SeedRun(t, ctx, pool, accountID, "22 minutes", "partial", notRead)
	swt39SeedRun(t, ctx, pool, accountID, "19 minutes", "partial", notRead)
	if flagged := swt39Reconcile(t, ctx, pool); flagged != 0 {
		t.Fatalf("flagged = %d after 3 partial passes that DEFERRED the target, want 0", flagged)
	}

	const didRead = `{"phase":"slack_web","read":["DSWT39TGT"],"deferred":["CSWT39OTH"]}`
	swt39SeedRun(t, ctx, pool, accountID, "15 minutes", "partial", didRead)
	swt39SeedRun(t, ctx, pool, accountID, "12 minutes", "partial", didRead)
	swt39SeedRun(t, ctx, pool, accountID, "9 minutes", "partial", didRead)
	if flagged := swt39Reconcile(t, ctx, pool); flagged != 1 {
		t.Fatalf("flagged = %d after 3 PARTIAL passes that read %s, want 1. A partial run is a real pass for "+
			"the conversations it read; counting only 'ok' would delay every flag in a workspace that is "+
			"always partial, forever", flagged, swt39TargetConv)
	}
	if events, passes := swt39UnconfirmedPasses(t, ctx, pool, taskID); events != 1 || passes != 3 {
		t.Errorf("delivery_unconfirmed events=%d passes=%v, want 1 event reporting 3 passes", events, passes)
	}
}

// Backward compatibility: runs from before SWT-39 (and from an old leaf) carry
// no `read` key and keep counting as they do today. Mixed with a new-shape run
// that did NOT read the target, only the legacy runs count.
func TestRegression_SWT39_ReconcileLegacyRunsWithoutReadKeyStillCount(t *testing.T) {
	ctx := context.Background()
	pool := newSWT39Pool(t, ctx)
	accountID := swt39Account(t, ctx, pool, "slack_web", swt39RCAccount)
	taskID, _ := swt39SeedDelivery(t, ctx, pool, "https://app.slack.com/client/"+swt39RCWorkspace+"/"+swt39TargetConv)

	swt39SeedRun(t, ctx, pool, accountID, "25 minutes", "ok", `{"phase":"slack_web"}`)
	swt39SeedRun(t, ctx, pool, accountID, "22 minutes", "ok", `{"phase":"slack_web","read":["CSWT39OTH"]}`)
	swt39SeedRun(t, ctx, pool, accountID, "19 minutes", "ok", `{"phase":"slack_web","conversations_seen":7}`)
	if flagged := swt39Reconcile(t, ctx, pool); flagged != 0 {
		t.Fatalf("flagged = %d with 2 legacy passes + 1 pass that did not read the target, want 0", flagged)
	}

	swt39SeedRun(t, ctx, pool, accountID, "15 minutes", "ok", `{"phase":"slack_web"}`)
	if flagged := swt39Reconcile(t, ctx, pool); flagged != 1 {
		t.Fatalf("flagged = %d after 3 LEGACY passes (no `read` key), want 1. Runs recorded before coverage "+
			"existed must keep counting, or every send in flight at deploy time is never flagged", flagged)
	}
	if events, passes := swt39UnconfirmedPasses(t, ctx, pool, taskID); events != 1 || passes != 3 {
		t.Errorf("delivery_unconfirmed events=%d passes=%v, want 1 event reporting 3 passes", events, passes)
	}
}

// Writer and reader bound together: the read set ReconcileUnconfirmed consults
// is the one Ingest actually stored, not a fixture of it.
func TestRegression_SWT39_ReconcileUsesTheReadSetIngestWrote(t *testing.T) {
	ctx := context.Background()
	pool := newSWT39Pool(t, ctx)
	sink := slackweb.NewSink(pool)
	taskID, _ := swt39SeedDelivery(t, ctx, pool, "https://app.slack.com/client/"+swt39RCWorkspace+"/"+swt39TargetConv)

	// The freeze's shape: every run fully reads the conversations it enumerated,
	// finishes 'ok', and the target is simply not among them.
	elsewhere := &swt39Source{export: swt39Export(t, swt39Workspace(t, swt39RCWorkspace, []string{"CSWT39OTH"},
		swt39CoverageFields(t, swt39RCWorkspace, []string{"CSWT39OTH"}, nil, nil, 1, false)))}
	for i := 0; i < 3; i++ {
		if _, err := slackweb.Ingest(ctx, elsewhere, sink); err != nil {
			t.Fatalf("Ingest %d: %v", i, err)
		}
	}
	if flagged := swt39Reconcile(t, ctx, pool); flagged != 0 {
		t.Fatalf("flagged = %d after 3 real Ingest passes that never read %s, want 0 (the asunda45 case: "+
			"34 `ok` runs, none of which exported the DM)", flagged, swt39TargetConv)
	}

	withTarget := &swt39Source{export: swt39Export(t, swt39Workspace(t, swt39RCWorkspace, []string{"CSWT39OTH", swt39TargetConv},
		swt39CoverageFields(t, swt39RCWorkspace, []string{"CSWT39OTH", swt39TargetConv}, nil, nil, 2, false)))}
	for i := 0; i < 3; i++ {
		if _, err := slackweb.Ingest(ctx, withTarget, sink); err != nil {
			t.Fatalf("Ingest with target %d: %v", i, err)
		}
	}
	if flagged := swt39Reconcile(t, ctx, pool); flagged != 1 {
		t.Fatalf("flagged = %d after 3 real Ingest passes that read %s with no matching message, want 1", flagged, swt39TargetConv)
	}
	if events, passes := swt39UnconfirmedPasses(t, ctx, pool, taskID); events != 1 || passes != 3 {
		t.Errorf("delivery_unconfirmed events=%d passes=%v, want 1 event reporting 3 passes", events, passes)
	}
}

// ---- F: the known set, loaded from raw_source_items ---------------------------------

func swt39Raw(t *testing.T, ctx context.Context, pool *pgxpool.Pool, accountID int64, workspaceID, conversationID, name, messageID, ingestedAgo string) {
	t.Helper()
	kind, externalID := "conversation", "conversation:"+conversationID
	observation := map[string]any{
		"workspace": map[string]any{"id": workspaceID, "name": "SWT-39 fixture", "url": "https://app.slack.com/client/" + workspaceID, "own_user_id": "U0OWNER001"},
		"conversation": map[string]any{"id": conversationID, "name": name, "type": swt39ConvType(conversationID),
			"url": "https://app.slack.com/client/" + workspaceID + "/" + conversationID},
	}
	if messageID != "" {
		kind, externalID = "message", "message:"+conversationID+":"+messageID
		observation["message"] = map[string]any{"id": messageID, "direction": "inbound", "text": "fixture"}
	}
	observation["kind"] = kind
	raw, err := json.Marshal(observation)
	if err != nil {
		t.Fatalf("marshal raw: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, ingested_at, normalized_at)
		 VALUES ($1,$2,$3::jsonb,$4, now() - $5::interval, now())`,
		accountID, externalID, string(raw), "swt39-"+externalID, ingestedAgo); err != nil {
		t.Fatalf("seed raw %s: %v", externalID, err)
	}
}

// Fixed run instants, so expected LastReadAt values are exact.
var (
	swt39T1 = time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	swt39T2 = time.Date(2026, 9, 11, 11, 0, 0, 0, time.UTC)
	swt39T3 = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	swt39T4 = time.Date(2026, 9, 11, 13, 0, 0, 0, time.UTC)
)

func swt39SeedRunAt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, accountID int64, at time.Time, status, stats string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO sync_runs (source_account_id, started_at, finished_at, status, stats)
		 VALUES ($1, $2::timestamptz, $2::timestamptz + interval '5 minutes', $3, $4::jsonb)`, accountID, at, status, stats); err != nil {
		t.Fatalf("seed sync_run (%s, %s): %v", at, status, err)
	}
}

// swt39SeedKnownCorpus: two slack_web workspaces and one foreign provider.
// LastReadAt = the start of the latest ok|partial run whose stats.read holds
// the conversation, for THAT account (SWT-39 review: last-read, not newest
// message):
//   - DSWT39KN1: read at T1 (ok) and T2 (partial) -> T2.
//   - CSWT39KN2: read at T1 (ok); an 'error' run at T3 also lists it and must
//     not count -> T1. A legacy run with no read key at T4 counts for nothing.
//   - GSWT39KB1: never read -> zero. TSWT39KB's own run at T4 reads DSWT39KN1,
//     another account's conversation id, and must not leak into TSWT39KN.
//   - A message row for DSWT39KN1 exists and is irrelevant to the answer.
func swt39SeedKnownCorpus(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	swt39RequirePartialStatus(t, ctx, pool)
	kn := swt39Account(t, ctx, pool, "slack_web", swt39KNAccount)
	swt39Raw(t, ctx, pool, kn, swt39KNWorkspace, "DSWT39KN1", "asunda45", "", "3 hours")
	swt39Raw(t, ctx, pool, kn, swt39KNWorkspace, "DSWT39KN1", "asunda45", "p1799999999999999", "1 minute")
	swt39Raw(t, ctx, pool, kn, swt39KNWorkspace, "CSWT39KN2", "rd-asu-collaboratory", "", "3 hours")
	swt39SeedRunAt(t, ctx, pool, kn, swt39T1, "ok", `{"phase":"slack_web","read":["DSWT39KN1","CSWT39KN2"]}`)
	swt39SeedRunAt(t, ctx, pool, kn, swt39T2, "partial", `{"phase":"slack_web","read":["DSWT39KN1"],"deferred":["CSWT39KN2"]}`)
	swt39SeedRunAt(t, ctx, pool, kn, swt39T3, "error", `{"phase":"slack_web","read":["CSWT39KN2"]}`)
	swt39SeedRunAt(t, ctx, pool, kn, swt39T4, "ok", `{"phase":"slack_web"}`)

	kb := swt39Account(t, ctx, pool, "slack_web", swt39KBAccount)
	swt39Raw(t, ctx, pool, kb, swt39KBWorkspace, "GSWT39KB1", "avviato-ops", "", "3 hours")
	swt39SeedRunAt(t, ctx, pool, kb, swt39T4, "ok", `{"phase":"slack_web","read":["DSWT39KN1"]}`)

	fo := swt39Account(t, ctx, pool, "itest-swt39", swt39FOAccount)
	swt39Raw(t, ctx, pool, fo, "TSWT39FO", "CSWT39FOR", "not slack", "", "3 hours")
}

func TestRegression_SWT39_KnownConversationsLoadsEveryConversationWithLastRead(t *testing.T) {
	ctx := context.Background()
	pool := newSWT39Pool(t, ctx)
	swt39SeedKnownCorpus(t, ctx, pool)

	rows, err := slackweb.NewSink(pool).KnownConversations(ctx)
	if err != nil {
		t.Fatalf("KnownConversations: %v", err)
	}
	var mine []slackweb.KnownConversationRow
	for _, r := range rows {
		if r.ConversationID == "CSWT39FOR" || r.WorkspaceID == "TSWT39FO" {
			t.Errorf("KnownConversations returned %+v from a non-slack_web account", r)
		}
		if strings.HasPrefix(r.WorkspaceID, "TSWT39") {
			mine = append(mine, r)
		}
	}
	sort.Slice(mine, func(i, j int) bool {
		if mine[i].WorkspaceID != mine[j].WorkspaceID {
			return mine[i].WorkspaceID < mine[j].WorkspaceID
		}
		return mine[i].ConversationID < mine[j].ConversationID
	})
	want := []slackweb.KnownConversationRow{
		{WorkspaceID: swt39KBWorkspace, ConversationID: "GSWT39KB1", Name: "avviato-ops"},
		{WorkspaceID: swt39KNWorkspace, ConversationID: "CSWT39KN2", Name: "rd-asu-collaboratory", LastReadAt: swt39T1},
		{WorkspaceID: swt39KNWorkspace, ConversationID: "DSWT39KN1", Name: "asunda45", LastReadAt: swt39T2},
	}
	if len(mine) != len(want) {
		t.Fatalf("KnownConversations (this suite's rows) = %+v, want %+v", mine, want)
	}
	for i := range want {
		g, w := mine[i], want[i]
		if g.WorkspaceID != w.WorkspaceID || g.ConversationID != w.ConversationID || g.Name != w.Name ||
			!g.LastReadAt.Equal(w.LastReadAt) {
			t.Errorf("row %d = %+v, want %+v. LastReadAt = start of the latest ok|partial run of THAT account "+
				"whose stats.read holds the id; error runs, legacy runs and other accounts do not count", i, g, w)
		}
	}
}

// Fail fast when 0027 is missing (SWT-39 review, HIGH 2); on the compose db,
// which has it, the check passes.
func TestRegression_SWT39_CheckPartialStatusPassesWith0027(t *testing.T) {
	ctx := context.Background()
	pool := newSWT39Pool(t, ctx)
	swt39RequirePartialStatus(t, ctx, pool)
	if err := slackweb.NewSink(pool).CheckPartialStatus(ctx); err != nil {
		t.Errorf("CheckPartialStatus with 0027 applied = %v, want nil", err)
	}
}

// The db loader wired through Ingest: the request the leaf receives carries
// the known set, converted and filtered, from the real table.
func TestRegression_SWT39_IngestSendsTheKnownSetFromTheDatabase(t *testing.T) {
	t.Setenv("SLACK_WEB_EXPORT_BUDGET_MS", "")
	t.Setenv("SLACK_WEB_EXPORT_MAX_CONVERSATIONS", "")
	ctx := context.Background()
	pool := newSWT39Pool(t, ctx)
	swt39SeedKnownCorpus(t, ctx, pool)

	source := &swt39Source{export: slackweb.Export{SchemaVersion: slackweb.SchemaVersion}}
	if _, err := slackweb.Ingest(ctx, source, slackweb.NewSink(pool)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(source.requests) != 1 {
		t.Fatalf("source.Export called %d times, want 1", len(source.requests))
	}
	req := source.requests[0]
	if req.BudgetMS != 900000 || req.MaxConversations != 60 {
		t.Errorf("request budget = %d / %d, want 900000 / 60", req.BudgetMS, req.MaxConversations)
	}
	kn := map[string]slackweb.KnownConversation{}
	for _, k := range req.Known[swt39KNWorkspace] {
		kn[k.ID] = k
	}
	wantKN1 := fmt.Sprintf("%d.000000", swt39T2.Unix())
	if got := kn["DSWT39KN1"]; got.LastSeenTS != wantKN1 || got.Name != "asunda45" {
		t.Errorf("known[%s][DSWT39KN1] = %+v, want last_seen_ts %s (its last read, T2) and name asunda45 "+
			"(request: %+v)", swt39KNWorkspace, got, wantKN1, req.Known[swt39KNWorkspace])
	}
	if got, ok := kn["CSWT39KN2"]; !ok || got.LastSeenTS != fmt.Sprintf("%d.000000", swt39T1.Unix()) {
		t.Errorf("known[%s][CSWT39KN2] = %+v (present=%v), want its last read T1", swt39KNWorkspace, got, ok)
	}
	if len(req.Known[swt39KBWorkspace]) != 1 || req.Known[swt39KBWorkspace][0].ID != "GSWT39KB1" ||
		req.Known[swt39KBWorkspace][0].LastSeenTS != "" {
		t.Errorf("known[%s] = %+v, want exactly GSWT39KB1", swt39KBWorkspace, req.Known[swt39KBWorkspace])
	}
	if _, ok := req.Known["TSWT39FO"]; ok {
		t.Errorf("known includes TSWT39FO from a non-slack_web account")
	}
}
