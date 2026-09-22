//go:build integration

package slackweb_test

// slack-watch-sweep (SWT-75) criteria 18 and 19: the two existing sync_runs
// readers must IGNORE `slack_web_watch` runs. This is D6's load-bearing half —
// the reason the watch pass gets a phase of its own at all.
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_slackwatch?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run PhaseFilter ./internal/connector/slackweb/
//
// What goes wrong without the filter, in the SPEC's own words: "a delivery into
// a watched conversation is flagged 'unconfirmed after 3 export passes' THREE
// MINUTES after the send, an alarm meaning something entirely different from
// what it says". DefaultUnconfirmedFlagPasses = 3 was chosen against a
// 30-minute cadence (reconcile.go:19); at one pass a minute it is reached
// before a human has finished reading the reply they just sent. It is the "one
// upworkcrm invocation writes TWO sync_runs rows" landmine (IK) in a new
// costume, and this time the multiplier is 30x.
//
// Both tests assert the filter's effect on rows POSTGRES produced, never on a
// fixture-supplied field (IK: test the column, not the fixture). The run rows
// are seeded with explicit phases and read back through the production SQL.
//
// This suite owns 'twphase@slack-web.local', the workspace 'TWPHASE', the slug
// 'itest-slack-phase-proj' and the thread prefix 'slack:TWPHASE'.
//
// RED TODAY: the phase filters do not exist, so ten watch runs flag the
// delivery and KnownConversations reports the watch runs' instant.
//
// MUTATIONS: drop the phase filter from ReconcileUnconfirmed's pass count ->
// criterion 18; replace KnownConversations' phase filter with a literal true ->
// criterion 19.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	phWorkspace = "TWPHASE"
	phConv      = "CWPHASE"
	phAccount   = "twphase@slack-web.local"
	phTarget    = "https://app.slack.com/client/TWPHASE/CWPHASE"
	phSlug      = "itest-slack-phase-proj"
	phBody      = "the reply whose healthy send must not be flagged three minutes later"
)

func newPhasePool(t *testing.T, ctx context.Context) *pgxpool.Pool {
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
	cleanupPhase(t, ctx, pool)
	t.Cleanup(func() { cleanupPhase(t, context.Background(), pool) })
	return pool
}

func cleanupPhase(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE provider='slack_web' AND account_email='` + phAccount + `')`
	const tasks = `(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + phSlug + `'))`
	for _, q := range []string{
		`DELETE FROM task_events WHERE task_id IN ` + tasks,
		`DELETE FROM deliveries WHERE task_id IN ` + tasks,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + phSlug + `')`,
		`DELETE FROM projects WHERE slug='` + phSlug + `'`,
		// One statement, so the threads referenced only by this suite's messages
		// go with them (the FK is checked at statement end) — and no LIKE on the
		// thread key: its format has ONE spelling (threadscope_test.go).
		`WITH mine AS (SELECT id, thread_id FROM normalized_messages
		                WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)),
		      gone AS (DELETE FROM normalized_messages WHERE id IN (SELECT id FROM mine))
		 DELETE FROM normalized_threads t WHERE t.id IN (SELECT thread_id FROM mine)
		   AND NOT EXISTS (SELECT 1 FROM normalized_messages m WHERE m.thread_id = t.id AND m.id NOT IN (SELECT id FROM mine))`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider='slack_web' AND account_email='` + phAccount + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func phAccountID(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, domain_default, scopes, send_enabled, calendar_in_availability)
		 VALUES ('slack_web',$1,'https://app.slack.com/client/TWPHASE','{}',false,false)
		 ON CONFLICT (provider, account_email) DO UPDATE SET domain_default=EXCLUDED.domain_default
		 RETURNING id`, phAccount).Scan(&id); err != nil {
		t.Fatalf("seed source account: %v", err)
	}
	return id
}

// phRuns inserts n finished runs of one phase, all of which READ phConv, all
// started `ago` before now. The stats are the shape Ingest writes.
func phRuns(t *testing.T, ctx context.Context, pool *pgxpool.Pool, accountID int64, phase string, n int, ago time.Duration) {
	t.Helper()
	stats := fmt.Sprintf(`{"phase":%q,"read":[%q],"workspaces_seen":1,"conversations_seen":1}`, phase, phConv)
	for i := 0; i < n; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO sync_runs (source_account_id, started_at, finished_at, status, stats)
			 VALUES ($1, now() - $2::interval, now() - $2::interval + interval '20 seconds', 'ok', $3::jsonb)`,
			accountID, fmt.Sprintf("%d seconds", int(ago.Seconds())), stats); err != nil {
			t.Fatalf("seed %s run: %v", phase, err)
		}
	}
}

// Criterion 18: TEN watch runs that read the conversation flag NOTHING; three
// `slack_web` runs flag it exactly as today.
func TestPhaseFilter_ReconcileIgnoresWatchRuns(t *testing.T) {
	ctx := context.Background()
	pool := newPhasePool(t, ctx)
	accountID := phAccountID(t, ctx, pool)
	sink := slackweb.NewSink(pool)

	var projectID, taskID, deliveryID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,'itest-slack-phase','manual','dashboard','/tmp/itest','any') RETURNING id`, phSlug).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'Slack phase work','claude','delivered') RETURNING id`, projectID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	// A healthy send, clicked ten minutes ago and not yet confirmed by an export.
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, sent_at, send_attempted_at, approval_source)
		 VALUES ($1,'slack_reply',$2,$3,'sent', now() - interval '10 minutes', now() - interval '10 minutes','switchboard')
		 RETURNING id`, taskID, phTarget, phBody).Scan(&deliveryID); err != nil {
		t.Fatalf("seed delivery: %v", err)
	}

	// Ten per-minute watch passes, every one of which READ the conversation.
	phRuns(t, ctx, pool, accountID, slackweb.PhaseSlackWebWatch, 10, 5*time.Minute)

	flagged, err := slackweb.ReconcileUnconfirmed(ctx, sink, slackweb.DefaultUnconfirmedFlagPasses)
	if err != nil {
		t.Fatalf("ReconcileUnconfirmed: %v", err)
	}
	if flagged != 0 {
		t.Errorf("ReconcileUnconfirmed flagged %d rows after TEN slack_web_watch passes, want 0. Criterion 18 / "+
			"D6: the pass count means \"export passes that could have observed the message\", and a per-minute "+
			"targeted sweep satisfies a threshold chosen for a 30-minute cadence in three minutes — the alarm "+
			"would then mean something entirely different from what it says", flagged)
	}
	var errText *string
	if err := pool.QueryRow(ctx, `SELECT error FROM deliveries WHERE id=$1`, deliveryID).Scan(&errText); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if errText != nil {
		t.Errorf("the delivery carries error=%q after only watch passes; nothing may be written on a healthy "+
			"send (criterion 18)", *errText)
	}
	var events int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_unconfirmed'`, taskID).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 0 {
		t.Errorf("delivery_unconfirmed events after only watch passes = %d, want 0", events)
	}

	// The control, and the half that must NOT change: three rotation passes flag
	// it exactly as today. Without this the test above would pass for the wrong
	// reason — a reconciler that flags nothing at all.
	phRuns(t, ctx, pool, accountID, slackweb.PhaseSlackWeb, slackweb.DefaultUnconfirmedFlagPasses, 4*time.Minute)
	flagged, err = slackweb.ReconcileUnconfirmed(ctx, sink, slackweb.DefaultUnconfirmedFlagPasses)
	if err != nil {
		t.Fatalf("ReconcileUnconfirmed (rotation passes): %v", err)
	}
	if flagged != 1 {
		t.Fatalf("ReconcileUnconfirmed flagged %d after three slack_web passes, want 1 — D11: the reconciler's "+
			"behaviour on rotation passes is unchanged by this ticket", flagged)
	}
}

// Criterion 19: KnownConversations' `runs` CTE ignores watch runs, so the
// rotation's least-recently-visited ordering is byte-identical to today's for
// the same run set — and so that CTE does not grow to scan a per-minute row set
// over 30 days.
func TestPhaseFilter_KnownConversationsIgnoresWatchRuns(t *testing.T) {
	ctx := context.Background()
	pool := newPhasePool(t, ctx)
	accountID := phAccountID(t, ctx, pool)
	sink := slackweb.NewSink(pool)

	raw := fmt.Sprintf(`{"kind":"conversation","workspace":{"id":%q,"name":"Phase","url":"https://app.slack.com/client/%s","own_user_id":"UPHOWN"},`+
		`"conversation":{"id":%q,"name":"phase","type":"public_channel","url":%q}}`,
		phWorkspace, phWorkspace, phConv, phTarget)
	if _, err := pool.Exec(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1,$2,$3::jsonb,'itest-phase-hash')`, accountID, "conversation:"+phConv, raw); err != nil {
		t.Fatalf("seed conversation raw row: %v", err)
	}

	// One rotation pass two hours ago, then ten watch passes a minute ago.
	phRuns(t, ctx, pool, accountID, slackweb.PhaseSlackWeb, 1, 2*time.Hour)
	phRuns(t, ctx, pool, accountID, slackweb.PhaseSlackWebWatch, 10, time.Minute)

	rows, err := sink.KnownConversations(ctx)
	if err != nil {
		t.Fatalf("KnownConversations: %v", err)
	}
	var got *slackweb.KnownConversationRow
	for i := range rows {
		if rows[i].WorkspaceID == phWorkspace && rows[i].ConversationID == phConv {
			got = &rows[i]
		}
	}
	if got == nil {
		t.Fatalf("KnownConversations did not return %s/%s: %v", phWorkspace, phConv, rows)
	}
	age := time.Since(got.LastReadAt)
	if age < time.Hour {
		t.Errorf("last_read_at is %s old, i.e. it came from a WATCH run. Criterion 19: a watched conversation "+
			"is read every minute, so counting those runs would pin it to the BACK of the rotation's "+
			"least-recently-visited queue forever — and the rotation is what covers the other 49 conversations "+
			"(export.ts:188-208)", age.Truncate(time.Second))
	}
	if age > 3*time.Hour {
		t.Errorf("last_read_at is %s old, older than the one rotation run that read it: the phase filter must "+
			"EXCLUDE watch runs, not exclude everything (criterion 19)", age.Truncate(time.Second))
	}
}
