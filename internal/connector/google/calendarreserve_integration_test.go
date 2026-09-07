//go:build integration

package google

// The stale-poll fence and the LoadBusy-visible reservation (SWT-28, codex
// adversarial finding 2, 2026-09-07). Two deterministic tests instead of an
// interleaving race:
//
//  1. SupersedeAbsentCalendar must NOT supersede a raw row that matches an
//     UNCONFIRMED calendar delivery of the same account — that is exactly the
//     poll-fetched-before-booking-applied-after interleaving, replayed as a
//     direct call with a keep set that omits the block. Once the delivery is
//     confirmed, normal replacement semantics resume (a hand-deleted block
//     still frees its slot).
//  2. availability.LoadBusy must treat an unconfirmed booked delivery as busy
//     even when NO normalized_events row backs it — the reservation that
//     closes the gap between reserving an event id and the first poll that
//     observes the created event.
//
// ANTI-DATE-ROT: all instants derive from time.Now().

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/availability"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	calResvAcct = "itest-calresv-a@example.com"
	calResvSlug = "itest-calresv-proj"
)

func calResvCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	owned := `(SELECT id FROM source_accounts WHERE account_email LIKE 'itest-calresv-%')`
	ourTasks := `(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + calResvSlug + `'))`
	for _, s := range []string{
		`DELETE FROM task_events WHERE task_id IN ` + ourTasks,
		`DELETE FROM deliveries WHERE task_id IN ` + ourTasks,
		`DELETE FROM normalized_events WHERE raw_source_item_id IN
			(SELECT id FROM raw_source_items WHERE source_account_id IN ` + owned + `)`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + owned,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + owned,
		`DELETE FROM sync_runs WHERE stats->>'itest' = 'itest-calresv'`,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + calResvSlug + `')`,
		`DELETE FROM projects WHERE slug='` + calResvSlug + `'`,
		`DELETE FROM source_accounts WHERE account_email LIKE 'itest-calresv-%'`,
	} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
}

type calResvFixture struct {
	pool      *pgxpool.Pool
	accountID int64
	taskID    int64
}

func newCalResvFixture(t *testing.T, ctx context.Context) *calResvFixture {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use the compose db on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	calResvCleanup(t, ctx, pool)
	t.Cleanup(func() { calResvCleanup(t, context.Background(), pool) })

	f := &calResvFixture{pool: pool}
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, calendar_in_availability, calendar_write_enabled)
		 VALUES ('google', $1, true, true) RETURNING id`, calResvAcct).Scan(&f.accountID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	// Fresh calendar sync for ours AND every other in-scope account: LoadBusy's
	// readiness is global by design, and a stale foreign account would refuse
	// every case here for the wrong reason.
	if _, err := pool.Exec(ctx,
		`INSERT INTO sync_runs (source_account_id, started_at, finished_at, status, stats)
		 SELECT id, now(), now(), 'ok', jsonb_build_object('phase','calendar','itest','itest-calresv')
		   FROM source_accounts WHERE provider='google' AND calendar_in_availability`); err != nil {
		t.Fatalf("freshen calendars: %v", err)
	}
	var projectID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, ai_locality) VALUES ('itest calresv', $1, 'any') RETURNING id`,
		calResvSlug).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, status) VALUES ($1, 'itest calresv task', 'in_progress') RETURNING id`,
		projectID).Scan(&f.taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return f
}

// seedBooked writes an unconfirmed booked delivery for [start, end); when
// withRecord is true it also runs the send-time RecordOwnCalendarEvent, i.e.
// the raw + normalized rows exist exactly as after a successful send.
func (f *calResvFixture) seedBooked(t *testing.T, ctx context.Context, start, end time.Time, withRecord bool) (int64, string) {
	t.Helper()
	eventID := "sbitestcalresv" + strconv.FormatInt(time.Now().UnixNano(), 32)
	extID := CalendarExternalID(eventID)
	var deliveryID int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, subject, body, status, approval_source,
		                          from_account_id, starts_at, ends_at, sent_external_id, sent_at, created_by)
		 VALUES ($1,'calendar',$2,'Focus block','reserved','sent','switchboard',$3,$4,$5,$6, now(),
		         'itest-calresv') RETURNING id`,
		f.taskID, calResvAcct, f.accountID, start, end, extID).Scan(&deliveryID); err != nil {
		t.Fatalf("seed booked delivery: %v", err)
	}
	if withRecord {
		resource := `{"kind":"calendar#event","id":` + mustJSON(t, eventID) +
			`,"status":"confirmed","summary":"itest calresv block","start":{"dateTime":` +
			mustJSON(t, start.Format(time.RFC3339)) + `},"end":{"dateTime":` +
			mustJSON(t, end.Format(time.RFC3339)) + `}}`
		if err := NewPGSink(f.pool).RecordOwnCalendarEvent(ctx, f.accountID, json.RawMessage(resource)); err != nil {
			t.Fatalf("send-time record: %v", err)
		}
	}
	return deliveryID, extID
}

func TestSupersedeAbsentCalendar_Integration_FencesUnconfirmedBookedBlock(t *testing.T) {
	ctx := context.Background()
	f := newCalResvFixture(t, ctx)
	start := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Hour)
	end := start.Add(15 * time.Minute)
	deliveryID, extID := f.seedBooked(t, ctx, start, end, true)
	sink := NewPGSink(f.pool)

	// The stale snapshot: fetched before the booking, so its keep set does not
	// carry the block. Window covers the block's start.
	keep := []string{"calendar:itest-calresv-unrelated"}
	windowFrom, windowTo := start.Add(-time.Hour), end.Add(time.Hour)

	if _, err := sink.SupersedeAbsentCalendar(ctx, f.accountID, keep, windowFrom, windowTo); err != nil {
		t.Fatalf("supersede (unconfirmed block present): %v", err)
	}
	var superseded bool
	var status string
	if err := f.pool.QueryRow(ctx,
		`SELECT r.superseded_at IS NOT NULL, COALESCE(e.status,'')
		   FROM raw_source_items r LEFT JOIN normalized_events e ON e.raw_source_item_id = r.id
		  WHERE r.source_account_id=$1 AND r.external_id=$2`, f.accountID, extID).
		Scan(&superseded, &status); err != nil {
		t.Fatalf("read raw row: %v", err)
	}
	if superseded {
		t.Fatalf("a stale snapshot superseded the UNCONFIRMED booked block %s. This is codex finding 2: the "+
			"next snapshot carries identical bytes, the content_hash short-circuits, and the block stays "+
			"cancelled forever while the event lives on the real calendar", extID)
	}
	if status == "cancelled" {
		t.Fatalf("the unconfirmed block's normalized event was cancelled by a stale snapshot")
	}

	// Confirm the delivery — a later poll observed the event — and the fence
	// lifts: a snapshot without the id now supersedes it (the hand-deleted
	// block frees its slot).
	if _, err := f.pool.Exec(ctx,
		`UPDATE deliveries SET confirmed_at=now() WHERE id=$1`, deliveryID); err != nil {
		t.Fatalf("confirm delivery: %v", err)
	}
	n, err := sink.SupersedeAbsentCalendar(ctx, f.accountID, keep, windowFrom, windowTo)
	if err != nil {
		t.Fatalf("supersede (confirmed block absent): %v", err)
	}
	if n != 1 {
		t.Fatalf("supersede after confirmation superseded %d rows, want 1 — the fence must lift once the "+
			"loop is closed, or a genuinely deleted block never frees its slot", n)
	}
}

func TestLoadBusy_Integration_UnconfirmedBookingReservesItsSlot(t *testing.T) {
	ctx := context.Background()
	f := newCalResvFixture(t, ctx)
	start := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Hour)
	end := start.Add(15 * time.Minute)
	// NO send-time record: the reservation must hold even when nothing in
	// normalized_events backs it (the busy-set record failed, or a stale poll
	// would have superseded it before the fence existed).
	deliveryID, _ := f.seedBooked(t, ctx, start, end, false)

	req := availability.Request{
		WindowStart:   start.Add(-time.Hour),
		WindowEnd:     end.Add(time.Hour),
		Now:           time.Now(),
		MaxSyncAge:    time.Hour,
		HorizonPast:   CalendarWindowPast,
		HorizonFuture: CalendarWindowFuture,
	}
	busy, err := availability.LoadBusy(ctx, f.pool, req)
	if err != nil {
		t.Fatalf("LoadBusy: %v", err)
	}
	overlaps := func(busy []availability.Interval) bool {
		for _, iv := range busy {
			if iv.Start.Before(end) && iv.End.After(start) {
				return true
			}
		}
		return false
	}
	if !overlaps(busy) {
		t.Fatalf("LoadBusy does not treat the unconfirmed booked delivery %d as busy (got %v). The "+
			"reservation is what stops a second booking taking the same slot between the reserve and the "+
			"first observing poll", deliveryID, busy)
	}

	// Confirmed and still with no normalized event (the hand-deleted case):
	// the reservation lifts and the slot frees.
	if _, err := f.pool.Exec(ctx,
		`UPDATE deliveries SET confirmed_at=now() WHERE id=$1`, deliveryID); err != nil {
		t.Fatalf("confirm delivery: %v", err)
	}
	busy, err = availability.LoadBusy(ctx, f.pool, req)
	if err != nil {
		t.Fatalf("LoadBusy after confirm: %v", err)
	}
	if overlaps(busy) {
		t.Fatalf("a CONFIRMED delivery with no backing normalized event still reserves its slot; the "+
			"reservation must end at confirmed_at or a deleted block blocks its slot forever (got %v)", busy)
	}
}
