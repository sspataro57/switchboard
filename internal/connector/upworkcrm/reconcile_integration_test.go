//go:build integration

package upworkcrm_test

// Integration test for upworkcrm.ReconcileUnconfirmed (SWT-19 acceptance
// criterion 16).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run UpworkReconcile ./internal/connector/upworkcrm/
//
// The reconciler exists because EVERY failure mode around this matcher has one
// signature: a row at status='sent', sent_external_id NULL, forever, with
// nothing anywhere saying so. That covers a target_ref that no longer parses, a
// refusal on an ambiguous body prefix, and the pre-existing one-shot gap
// (confirmUpworkDelivery runs only from upsertMessage, only for PENDING raw
// items, so a message normalized before its delivery reached 'sent' is never
// re-examined — and on the assisted tier a human clicks "mark sent" minutes or
// hours after pasting). Changing the matcher's scoping without shipping the
// detector would be shipping a silent failure mode on purpose. IK records that
// slackweb has this detector and upworkcrm did not, which is why an upwork
// refusal was SILENT.
//
// It FLAGS and moves nothing. Retrying is the one thing that must not happen:
// there is no automated upwork send to retry, so a "retry" means a human
// double-posting into a client's chat.
//
// Passes, not wall time: a suspended CronJob accumulates no passes and therefore
// cannot false-flag, whereas wall time would raise "the send may have failed"
// when the fact is "the connector didn't run". And SIX passes, not slackweb's
// three, because one upworkcrm invocation writes TWO sync_runs rows.
//
// GREENFIELD NOTE: compile-FAILs until reconcile.go lands. Expected red state.
//
// Cross-suite discipline: scoped to itest-rc-*, cleaned in FK order before AND
// after; the sync_runs this suite inserts are tagged in stats so cleanup finds
// exactly them and never another suite's.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/upworkcrm"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	rcSlug   = "itest-rc-proj"
	rcClient = "cccc0001-0000-0000-0000-00000000rc01"
	rcRoom   = "room_deadbeef01"
	rcBody   = "Pushed the fix to staging; the queue is draining and I will confirm once the backlog clears."
)

func rcRoomedKey() string { return "upwork_crm:" + rcClient + ":room:" + rcRoom }

func cleanupUpworkReconcile(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	stmts := []string{
		`DELETE FROM task_events WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + rcSlug + `'))`,
		`DELETE FROM deliveries WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + rcSlug + `'))`,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + rcSlug + `')`,
		`DELETE FROM projects WHERE slug='` + rcSlug + `'`,
		// The SWT-20 fixture shape seeds the target thread (deliveries are
		// already gone by this point, so the FK order holds). Exact key, built
		// by the Go helper — never a LIKE over the key format.
		`DELETE FROM normalized_threads WHERE thread_key='` + rcRoomedKey() + `'`,
		`DELETE FROM sync_runs WHERE stats->>'itest_rc' IS NOT NULL`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
}

func rcOpen(t *testing.T) (context.Context, *pgxpool.Pool, int64) {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	acctID, err := upworkcrm.NewSink(pool).EnsureAccount(ctx)
	if err != nil {
		pool.Close()
		t.Fatalf("EnsureAccount: %v", err)
	}
	return ctx, pool, acctID
}

// rcSeedRuns inserts n completed 'ok' sync_runs for the upwork account, started
// after `after` — only runs that STARTED after the send can have observed the
// message, so a run already in flight at send time must not count.
func rcSeedRuns(t *testing.T, ctx context.Context, pool *pgxpool.Pool, acctID int64, n int, after time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO sync_runs (source_account_id, status, started_at, finished_at, stats)
			 VALUES ($1,'ok',$2,$2,'{"itest_rc":true}'::jsonb)`,
			acctID, after.Add(time.Duration(i+1)*time.Minute)); err != nil {
			t.Fatalf("seed sync run %d: %v", i, err)
		}
	}
}

type rcRow struct {
	status  string
	extID   *string
	confirm *string
	errText *string
}

func rcRead(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) rcRow {
	t.Helper()
	var r rcRow
	if err := pool.QueryRow(ctx,
		`SELECT status, sent_external_id, confirmed_at::text, error FROM deliveries WHERE id=$1`, id).
		Scan(&r.status, &r.extID, &r.confirm, &r.errText); err != nil {
		t.Fatalf("read delivery %d: %v", id, err)
	}
	return r
}

func TestUpworkReconcile_Integration_FlagsOnceAfterThePassThreshold(t *testing.T) {
	ctx, pool, acctID := rcOpen(t)
	defer pool.Close()

	cleanupUpworkReconcile(t, ctx, pool)
	defer cleanupUpworkReconcile(t, ctx, pool)

	var projID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,'itest-rc-client','manual','dashboard','/tmp/itest', 'any') RETURNING id`, rcSlug).Scan(&projID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	var taskID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'itest-rc work','claude','delivered') RETURNING id`, projID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	// The fixture's sent_at must sit AFTER every sync_run this database already
	// holds, or another suite's runs count toward our threshold and the
	// below-threshold assertion below is meaningless. The compose db is shared
	// and persistent (IK: "integration suites cross-pollute"), and this suite
	// deletes only the runs it inserted, so leftovers are the normal state.
	var floor time.Time
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(max(started_at), now() - interval '1 hour') FROM sync_runs WHERE source_account_id=$1`,
		acctID).Scan(&floor); err != nil {
		t.Fatalf("read the existing sync_runs high-water mark: %v", err)
	}
	sentAt := floor.Add(time.Minute)
	if n := scanInt(t, ctx, pool,
		`SELECT count(*) FROM sync_runs WHERE source_account_id=$1 AND status='ok' AND started_at > $2`,
		acctID, sentAt); n != 0 {
		t.Fatalf("fixture invalid: %d pre-existing sync runs already postdate this delivery's sent_at, so the "+
			"pass count under test is not the one this suite seeded", n)
	}

	// Post-0019 fixture shape (SWT-20 criterion 13): the identity columns are
	// mandatory for this channel, so the thread exists first.
	if _, err := pool.Exec(ctx,
		`INSERT INTO normalized_threads (thread_key, participants) VALUES ($1,'[]')
		 ON CONFLICT (thread_key) WHERE thread_key IS NOT NULL DO NOTHING`, rcRoomedKey()); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	var rcThreadID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM normalized_threads WHERE thread_key=$1`, rcRoomedKey()).Scan(&rcThreadID); err != nil {
		t.Fatalf("read thread id: %v", err)
	}
	var stuckID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, target_client_ref, thread_id, body, status, sent_external_id, sent_at)
		 VALUES ($1,'upwork_chat',$2,$3,$4,$5,'sent',NULL,$6) RETURNING id`,
		taskID, rcRoomedKey(), rcClient, rcThreadID, rcBody, sentAt).Scan(&stuckID); err != nil {
		t.Fatalf("seed stuck delivery: %v", err)
	}
	// Out of scope, both ways: an already-confirmed upwork row and a row on
	// another channel. A reconciler that flagged either would generate noise
	// about deliveries that are fine.
	var confirmedID, slackID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, target_client_ref, thread_id, body, status, sent_external_id, sent_at, confirmed_at)
		 VALUES ($1,'upwork_chat',$2,$3,$4,$5,'sent','story_itest_rc_confirmed',$6,$6) RETURNING id`,
		taskID, rcRoomedKey(), rcClient, rcThreadID, rcBody, sentAt).Scan(&confirmedID); err != nil {
		t.Fatalf("seed confirmed delivery: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, sent_at)
		 VALUES ($1,'slack_reply','https://app.slack.com/client/TRC/CRC',$2,'sent',$3) RETURNING id`,
		taskID, rcBody, sentAt).Scan(&slackID); err != nil {
		t.Fatalf("seed slack delivery: %v", err)
	}

	passes := upworkcrm.DefaultUnconfirmedFlagPasses
	sink := upworkcrm.NewSink(pool)

	// One pass short of the threshold: nothing is flagged. Without this the
	// threshold could be zero and every assertion below would still hold.
	rcSeedRuns(t, ctx, pool, acctID, passes-1, sentAt)
	flagged, err := upworkcrm.ReconcileUnconfirmed(ctx, sink, passes)
	if err != nil {
		t.Fatalf("ReconcileUnconfirmed (below threshold): %v", err)
	}
	if flagged != 0 {
		t.Errorf("flagged = %d after %d of %d passes, want 0: a row is only unconfirmed once enough runs COULD "+
			"have seen the message", flagged, passes-1, passes)
	}
	if r := rcRead(t, ctx, pool, stuckID); r.errText != nil && *r.errText != "" {
		t.Errorf("delivery %d already carries a note below the threshold: %q", stuckID, *r.errText)
	}

	// The pass that crosses it.
	rcSeedRuns(t, ctx, pool, acctID, 1, sentAt.Add(time.Hour))

	flagged, err = upworkcrm.ReconcileUnconfirmed(ctx, sink, passes)
	if err != nil {
		t.Fatalf("ReconcileUnconfirmed: %v", err)
	}
	if flagged != 1 {
		t.Errorf("flagged = %d, want 1 (the stuck row only)", flagged)
	}

	got := rcRead(t, ctx, pool, stuckID)
	if got.errText == nil || !strings.Contains(*got.errText, "unconfirmed after") {
		t.Errorf("delivery %d error = %s, want a note beginning with the fire-once marker \"unconfirmed after\". "+
			"The marker doubles as the guard: an ambiguous-failure row already carries a send error, so "+
			"\"flag only when error IS NULL\" would never flag the rows that most need it", stuckID, rcStr(got.errText))
	}
	// Invariant 4: the reconciler annotates and raises a signal. It must never
	// write status, sent_external_id or sent_at — inventing an external id would
	// burn it under deliveries_sent_external_idx and lock the correct row out
	// permanently.
	if got.status != "sent" {
		t.Errorf("delivery %d status = %q, want it UNCHANGED at sent", stuckID, got.status)
	}
	if got.extID != nil {
		t.Errorf("delivery %d sent_external_id = %q: the reconciler must never invent one", stuckID, *got.extID)
	}
	if got.confirm != nil {
		t.Errorf("delivery %d confirmed_at = %q: flagging is not confirming", stuckID, *got.confirm)
	}
	if n := scanInt(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_unconfirmed'`, taskID); n != 1 {
		t.Errorf("delivery_unconfirmed task_events = %d, want exactly 1", n)
	}
	if r := rcRead(t, ctx, pool, confirmedID); r.errText != nil && *r.errText != "" {
		t.Errorf("the already-confirmed upwork row picked up a note: %q", *r.errText)
	}
	if r := rcRead(t, ctx, pool, slackID); r.errText != nil && *r.errText != "" {
		t.Errorf("the slack_reply row picked up a note from the UPWORK reconciler: %q", *r.errText)
	}

	// Fire-once: a second run flags nothing and writes no second event, even
	// though the row is still unconfirmed and the passes have only grown. A
	// detector that re-fires every 15 minutes is an alarm nobody reads.
	before := *got.errText
	flagged, err = upworkcrm.ReconcileUnconfirmed(ctx, sink, passes)
	if err != nil {
		t.Fatalf("ReconcileUnconfirmed (second run): %v", err)
	}
	if flagged != 0 {
		t.Errorf("second run flagged = %d, want 0 (fire-once on the marker)", flagged)
	}
	after := rcRead(t, ctx, pool, stuckID)
	if after.errText == nil || *after.errText != before {
		t.Errorf("the note changed on the second run:\n before: %q\n after:  %q", before, rcStr(after.errText))
	}
	if n := scanInt(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_unconfirmed'`, taskID); n != 1 {
		t.Errorf("delivery_unconfirmed task_events after the second run = %d, want still 1", n)
	}
}

func rcStr(p *string) string {
	if p == nil {
		return "<NULL>"
	}
	return *p
}
