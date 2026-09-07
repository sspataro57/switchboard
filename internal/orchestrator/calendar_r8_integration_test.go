//go:build integration

package orchestrator_test

// Criterion 29 at the INTEGRATION level (SWT-28 /
// docs/tickets/calendar-booking_SPEC.md; the verification protocol's step-2
// last bullet). The pure-rule cases live in rules_r8_calendar_test.go; this
// file drives a real DrainOnce over real task_events and asserts what the pure
// test cannot: that no `orchestrated` row with rule='delivery_lifecycle' is
// written, and — the payoff — that a LATER real delivery on the same task still
// fires R8 normally.
//
// WHY THE SECOND DRAIN IS THE POINT. Q2's reasoning is not "a booking should
// not mark a task delivered"; it is that firing R8 here BURNS THE DEDUP KEY.
// task_mark_delivered refuses a task that is not done_locally, the engine logs
// that failure and carries on (engine.go:110-141), and the record_orchestration
// that follows is suppressed only after a failed create_task — so the key lands
// anyway and the task's next, real delivery is deduped into silence. A test
// that stopped at "status unchanged" would pass against an implementation that
// skipped the two mutations and still recorded.
//
// Build-tagged `integration` AND env-gated on DATABASE_URL. Reuses
// integration_test.go's helpers (newOrchPool, cleanupOrch, seedProjectDelivery,
// newOrchExecutor, drain, setCursor, maxEventID, orchStatus, itoa) and its
// recordingPublisher — no broker.
//
// GREENFIELD NOTE: ruleDeliveryLifecycle has no channel test, and the calendar
// deliveries row this fixture seeds needs migration 0020's starts_at/ends_at.
// So this file first fails on the MISSING SCHEMA (an insert error naming
// starts_at), and after 0020 lands it fails on the rule.
//
// ANTI-DATE-ROT: the block's instants come from time.Now().

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	orch "github.com/sspataro57/switchboard/internal/orchestrator"
)

const (
	calR8Slug  = "itest-orch-calendar-book"
	calR8Acct  = "itest-orch-calendar-book@example.com"
	calR8Actor = "opsctl:itest-orch-calendar-book"
)

// cleanupCalR8 removes this file's own rows. cleanupOrch handles the tasks,
// events and project (slug LIKE 'itest-orch-%'), but it knows nothing about
// deliveries or source_accounts, and deliveries.task_id has an FK to tasks —
// so these must go FIRST, before cleanupOrch deletes the tasks.
func cleanupCalR8(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	stmts := []string{
		`DELETE FROM deliveries WHERE task_id IN
			(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + calR8Slug + `'))`,
		`DELETE FROM source_accounts WHERE account_email='` + calR8Acct + `'`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
	cleanupOrch(t, ctx, pool)
}

// seedCalendarDelivery writes a production-shaped calendar deliveries row:
// interval, target and account all present, as
// deliveries_calendar_identity_check (0020) demands. Seeding a bare row would
// be a fixture shaped like the assertion rather than like production — the
// mistake IK records twice.
func seedCalendarDelivery(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64) (int64, string) {
	t.Helper()
	var acctID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, calendar_in_availability, calendar_write_enabled)
		 VALUES ('google', $1, true, true) RETURNING id`, calR8Acct).Scan(&acctID); err != nil {
		t.Fatalf("seed calendar account (0020's calendar_write_enabled is expected missing until the "+
			"migration lands): %v", err)
	}
	d := time.Now().UTC().Add(24 * time.Hour)
	start := time.Date(d.Year(), d.Month(), d.Day(), 10, 0, 0, 0, time.UTC)
	extID := "calendar:sb" + itoa(taskID) + "titest"

	var deliveryID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, subject, body, status, approval_source,
		                          from_account_id, starts_at, ends_at, sent_external_id, sent_at, created_by)
		 VALUES ($1,'calendar',$2,'Focus block','reserved','sent','switchboard',$3,$4,$5,$6, now(), $7)
		 RETURNING id`,
		taskID, calR8Acct, acctID, start, start.Add(15*time.Minute), extID, calR8Actor).Scan(&deliveryID); err != nil {
		t.Fatalf("seed calendar delivery (migration 0020 adds starts_at/ends_at): %v", err)
	}
	return deliveryID, extID
}

func insertDeliverySent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID, deliveryID int64, channel, extID string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO task_events (task_id, event_type, payload)
		 VALUES ($1, 'delivery_sent', jsonb_build_object(
		     'delivery_id', $2::bigint, 'channel', $3::text, 'sent_external_id', $4::text))`,
		taskID, deliveryID, channel, extID); err != nil {
		t.Fatalf("insert delivery_sent (%s): %v", channel, err)
	}
}

func lifecycleRecords(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_events
		  WHERE task_id=$1 AND event_type='orchestrated' AND payload->>'rule'='delivery_lifecycle'`,
		taskID).Scan(&n); err != nil {
		t.Fatalf("count delivery_lifecycle records for task %d: %v", taskID, err)
	}
	return n
}

func TestOrchestrator_Integration_CalendarDeliveryDoesNotBurnTheLifecycleKey(t *testing.T) {
	ctx := context.Background()
	pool := newOrchPool(t, ctx)
	defer pool.Close()

	cleanupCalR8(t, ctx, pool)
	defer cleanupCalR8(t, ctx, pool)

	seedProjectDelivery(t, ctx, pool, calR8Slug, "itest-orch-cal-client", "dashboard")
	ex := newOrchExecutor(pool)

	// The task is done_locally so R8 WOULD succeed if it fired: with an
	// in_progress task, task_mark_delivered fails and "status unchanged" would
	// be true for the wrong reason.
	taskID := createReadyTask(t, ctx, ex, calR8Actor, calR8Slug, "calendar booking R8 skip")
	if _, err := pool.Exec(ctx, `UPDATE tasks SET status='done_locally' WHERE id=$1`, taskID); err != nil {
		t.Fatalf("set task done_locally: %v", err)
	}
	deliveryID, extID := seedCalendarDelivery(t, ctx, pool, taskID)

	engine := orch.NewEngine(pool, ex, &recordingPublisher{}, orch.Config{})
	setCursor(t, ctx, pool, maxEventID(t, ctx, pool))

	// ---- drain 1: the calendar booking ------------------------------------
	insertDeliverySent(t, ctx, pool, taskID, deliveryID, "calendar", extID)
	drain(t, ctx, engine)

	if s := orchStatus(t, ctx, pool, taskID); s != "done_locally" {
		t.Errorf("after a CALENDAR delivery_sent the task is %q, want done_locally unchanged. A booking is "+
			"usually incidental to work in progress (\"reserve two hours to finish this\") and never "+
			"advances a task's lifecycle (Q2 = b)", s)
	}
	if n := lifecycleRecords(t, ctx, pool, taskID); n != 0 {
		t.Fatalf("the calendar booking wrote %d delivery_lifecycle orchestration records, want 0. This is "+
			"the half that costs something later: the record is R8's dedup key, so the task's NEXT real "+
			"delivery is deduped and never marks it delivered", n)
	}

	// ---- drain 2: a REAL delivery on the same task -------------------------
	// The payoff. If drain 1 burned the key, this does nothing at all — and it
	// does nothing SILENTLY, which is why it has to be asserted rather than
	// reasoned about.
	var gmailDeliveryID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, subject, body, status, approval_source, sent_external_id,
		                          sent_at, created_by)
		 VALUES ($1,'gmail','Re: the fix','shipped','sent','switchboard','<sb-itest-orch-cal@example.com>',
		         now(), $2) RETURNING id`, taskID, calR8Actor).Scan(&gmailDeliveryID); err != nil {
		t.Fatalf("seed gmail delivery: %v", err)
	}
	insertDeliverySent(t, ctx, pool, taskID, gmailDeliveryID, "gmail", "<sb-itest-orch-cal@example.com>")
	drain(t, ctx, engine)

	if s := orchStatus(t, ctx, pool, taskID); s != "delivered" {
		t.Errorf("after a GMAIL delivery_sent on the same task the status is %q, want delivered. If this is "+
			"still done_locally, the earlier calendar booking burned the delivery_lifecycle dedup key and "+
			"R8 skipped the real delivery — exactly the failure Q2 was answered to prevent", s)
	}
	if n := lifecycleRecords(t, ctx, pool, taskID); n != 1 {
		t.Errorf("delivery_lifecycle records after the real delivery = %d, want exactly 1", n)
	}
}
