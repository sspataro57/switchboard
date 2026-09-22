//go:build integration

package slackweb_test

// slack-send-queue (SWT-76) Part 5, criterion 29 — the reconciler is UNCHANGED,
// and a queued row is an ORDINARY candidate for it.
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_sendqueue?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run SlackQueuedReconcile ./internal/connector/slackweb/
//
// Why this needs its own test rather than leaning on phase_integration_test.go:
// that suite's fixture is a `sent` row with sent_at set. A QUEUED row is
// `sending`, sent_at NULL, send_queued_at set and error NULL, so it reaches the
// reconciler through a different column —
// COALESCE(sent_at, send_attempted_at, updated_at) (reconcile.go:66) — and the
// new columns must not knock it out of the candidate set. D6 makes the
// reconciler one of only two horizons a queued send has, so "still a candidate"
// is load-bearing, not incidental.
//
// GREENFIELD NOTE: this file needs send_queued_at / send_queue_job_id
// (migration 0042) to seed its fixture, so it fails at the INSERT until 0042 is
// applied. Nothing else here is new: ReconcileUnconfirmed's behaviour is
// D11-frozen and these assertions must pass unchanged after implementation.
//
// THE NAMED RESIDUAL, recorded here so nobody "fixes" it later by accident
// (SPEC D6): for a queued row the reconciler's floor is send_attempted_at — the
// ENQUEUE instant — while the message only appears at the CLICK, up to
// max_queue_ms later. A rotation pass falling in that window counts as "could
// have observed" when it could not, so a queued row that waited across a pass
// boundary can be flagged after 2 real passes instead of 3. It FLAGS, never
// acts. The only way to do better is to know the click instant, which is D2's
// deliberately rejected status endpoint. Not asserted; recorded.
//
// This suite owns 'itest-slack-rq-%', tsdrq01@slack-web.local and TSDRQ01.
//
// MUTATION: count slack_web_watch runs in ReconcileUnconfirmed -> criterion 29.

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
	rqSlug      = "itest-slack-rq-proj"
	rqWorkspace = "TSDRQ01"
	rqConv      = "CSDRQ01"
	rqAccount   = "tsdrq01@slack-web.local"
	rqTarget    = "https://app.slack.com/client/TSDRQ01/CSDRQ01"
	rqBody      = "a queued send no export has confirmed yet"
)

func newRqPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
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
	cleanupRq(t, ctx, pool)
	t.Cleanup(func() { cleanupRq(t, context.Background(), pool) })
	return pool
}

func cleanupRq(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE provider='slack_web' AND account_email='` + rqAccount + `')`
	const tasks = `(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + rqSlug + `'))`
	for _, q := range []string{
		`DELETE FROM task_events WHERE task_id IN ` + tasks,
		`DELETE FROM deliveries WHERE task_id IN ` + tasks,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + rqSlug + `')`,
		`DELETE FROM projects WHERE slug='` + rqSlug + `'`,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider='slack_web' AND account_email='` + rqAccount + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

// rqRuns inserts n finished runs of one phase, all of which READ rqConv.
func rqRuns(t *testing.T, ctx context.Context, pool *pgxpool.Pool, accountID int64, phase string, n int, ago time.Duration) {
	t.Helper()
	stats := fmt.Sprintf(`{"phase":%q,"read":[%q],"workspaces_seen":1,"conversations_seen":1}`, phase, rqConv)
	for i := 0; i < n; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO sync_runs (source_account_id, started_at, finished_at, status, stats)
			 VALUES ($1, now() - $2::interval, now() - $2::interval + interval '20 seconds', 'ok', $3::jsonb)`,
			accountID, fmt.Sprintf("%d seconds", int(ago.Seconds())), stats); err != nil {
			t.Fatalf("seed %s run: %v", phase, err)
		}
	}
}

func TestSlackQueuedReconcile_Integration_QueuedRowIsAnOrdinaryCandidate(t *testing.T) {
	ctx := context.Background()
	pool := newRqPool(t, ctx)
	sink := slackweb.NewSink(pool)

	var accountID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, domain_default, scopes, send_enabled, calendar_in_availability)
		 VALUES ('slack_web',$1,'https://app.slack.com/client/TSDRQ01','{}',true,false) RETURNING id`,
		rqAccount).Scan(&accountID); err != nil {
		t.Fatalf("seed source account: %v", err)
	}
	var projectID, taskID, deliveryID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,'itest-slack-rq','manual','dashboard','/tmp/itest','any') RETURNING id`, rqSlug).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'Slack queued reconcile work','claude','done_locally') RETURNING id`, projectID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	// The queued send: accepted by the leaf 20 minutes ago, still `sending`,
	// attempt unsettled, error NULL (the queued UPDATE re-armed it), sent_at NULL
	// because nothing has clicked as far as switchboard knows.
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, approval_source,
		                         send_attempted_at, send_settled_at, send_queued_at, send_queue_job_id, error)
		 VALUES ($1,'slack_reply',$2,$3,'sending','switchboard',
		         now() - interval '20 minutes', NULL, now() - interval '20 minutes', 'send-itest-rq', NULL)
		 RETURNING id`, taskID, rqTarget, rqBody).Scan(&deliveryID); err != nil {
		t.Fatalf("seed queued delivery (apply migration 0042): %v", err)
	}

	// Ten per-minute TARGETED passes that all read the conversation. SWT-75 D6:
	// they must count for nothing, or a healthy queued send is flagged three
	// minutes after Salvador approved it.
	rqRuns(t, ctx, pool, accountID, slackweb.PhaseSlackWebWatch, 10, 5*time.Minute)

	flagged, err := slackweb.ReconcileUnconfirmed(ctx, sink, slackweb.DefaultUnconfirmedFlagPasses)
	if err != nil {
		t.Fatalf("ReconcileUnconfirmed: %v", err)
	}
	if flagged != 0 {
		t.Errorf("ReconcileUnconfirmed flagged %d rows after TEN slack_web_watch passes, want 0 — the phase "+
			"exclusion (SWT-75 D6) stays, and D6 here restates why: a per-minute targeted pass satisfies a "+
			"3-pass threshold chosen for a 30-minute cadence in three minutes (criterion 29)", flagged)
	}
	var errText *string
	if err := pool.QueryRow(ctx, `SELECT error FROM deliveries WHERE id=$1`, deliveryID).Scan(&errText); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if errText != nil {
		t.Errorf("a healthy queued send carries error=%q after only watch passes, want NULL", *errText)
	}

	// Three ROTATION passes that read the conversation, started after the enqueue:
	// the queued row is an ordinary candidate and gets flagged exactly as a
	// synchronous send would.
	rqRuns(t, ctx, pool, accountID, slackweb.PhaseSlackWeb, slackweb.DefaultUnconfirmedFlagPasses, 4*time.Minute)
	flagged, err = slackweb.ReconcileUnconfirmed(ctx, sink, slackweb.DefaultUnconfirmedFlagPasses)
	if err != nil {
		t.Fatalf("ReconcileUnconfirmed (rotation passes): %v", err)
	}
	if flagged != 1 {
		t.Fatalf("ReconcileUnconfirmed flagged %d after three slack_web passes, want 1. A queued row must stay "+
			"an ordinary candidate: with D6 refusing any automatic failure, the reconciler is one of only TWO "+
			"horizons a lost queued send has (criterion 29)", flagged)
	}

	// It FLAGS and moves nothing: no status change, no invented id, and the queue
	// columns are left alone so the dashboard can still say what happened.
	var status string
	var sentExternalID, jobID *string
	var queuedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT status, sent_external_id, send_queue_job_id, send_queued_at, error FROM deliveries WHERE id=$1`,
		deliveryID).Scan(&status, &sentExternalID, &jobID, &queuedAt, &errText); err != nil {
		t.Fatalf("read flagged delivery: %v", err)
	}
	if status != "sending" || sentExternalID != nil {
		t.Errorf("the reconciler moved the row (status=%q id=%v); it raises a human signal and moves NOTHING "+
			"— a retry of a click that did land is a double post (D6)", status, sentExternalID)
	}
	if jobID == nil || queuedAt == nil {
		t.Errorf("the flag cleared the queue columns (job=%v queued_at=%v); the human resolving this row needs "+
			"the job id to find it in the mini's log", jobID, queuedAt)
	}
	if errText == nil || !strings.Contains(*errText, "unconfirmed after") {
		t.Errorf("error = %v, want the reconciler's note naming mark_delivery_sent / mark_delivery_failed", errText)
	}
	var events int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_unconfirmed'`, taskID).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Errorf("delivery_unconfirmed events = %d, want 1", events)
	}
}
