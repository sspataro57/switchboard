//go:build integration

package google

// Loop closure for booked calendar blocks (SWT-28 criteria 26 + 28, and the
// §F amendment): a REAL PGSink over the compose db, a fake Pipedream source,
// and the actual RunPipedreamCalendar pass. This is the test go-reviewer
// flagged as missing: the amendment's ConfirmObservedCalendarDeliveries
// predicate (channel + confirmed_at IS NULL + from_account_id +
// sent_external_id = ANY(present)) had no automated coverage — its only
// evidence was the live smoke. A column rename or an id-spelling drift would
// have gone silently green.
//
// The delivery is seeded exactly as sendCalendarBlock leaves it after a send
// whose send-time record already ran: status='sent', approval_source=
// 'switchboard', the raw row present and normalized. The poll then OBSERVES
// the event id — the content_hash short-circuit keeps Normalize away, so the
// observation sweep is the only thing that can confirm.
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

	"github.com/sspataro57/switchboard/internal/store"
)

const (
	calLoopAcct = "itest-calloop-a@example.com"
	calLoopSlug = "itest-calloop-proj"
)

// calLoopSource is a verified one-calendar snapshot carrying exactly the
// events it is given, echoing the request window verbatim.
type calLoopSource struct {
	email  string
	events []string
	calls  int
}

func (s *calLoopSource) FetchCalendars(_ context.Context, req PipedreamCalendarRequest) (PipedreamCalendarResponse, error) {
	s.calls++
	n := len(s.events)
	raw := make([]json.RawMessage, 0, n)
	for _, e := range s.events {
		raw = append(raw, json.RawMessage(e))
	}
	return PipedreamCalendarResponse{
		SchemaVersion: PipedreamCalendarSchemaVersion,
		TimeMin:       req.TimeMin,
		TimeMax:       req.TimeMax,
		Calendars: []PipedreamCalendarEntry{
			{CalendarID: s.email, Status: "ok", EventCount: &n, Events: raw},
		},
	}, nil
}

func calLoopCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	owned := `(SELECT id FROM source_accounts WHERE account_email LIKE 'itest-calloop-%')`
	ourTasks := `(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + calLoopSlug + `'))`
	for _, s := range []string{
		`DELETE FROM task_events WHERE task_id IN ` + ourTasks,
		`DELETE FROM deliveries WHERE task_id IN ` + ourTasks,
		`DELETE FROM normalized_events WHERE raw_source_item_id IN
			(SELECT id FROM raw_source_items WHERE source_account_id IN ` + owned + `)`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + owned,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + owned,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + calLoopSlug + `')`,
		`DELETE FROM projects WHERE slug='` + calLoopSlug + `'`,
		`DELETE FROM source_accounts WHERE account_email LIKE 'itest-calloop-%'`,
	} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
}

func TestPipedreamPoll_Integration_ConfirmsObservedBookedBlock(t *testing.T) {
	ctx := context.Background()
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
	calLoopCleanup(t, ctx, pool)
	t.Cleanup(func() { calLoopCleanup(t, context.Background(), pool) })

	// Account, project, task.
	var accountID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, calendar_in_availability, calendar_write_enabled)
		 VALUES ('google', $1, true, true) RETURNING id`, calLoopAcct).Scan(&accountID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	var projectID, taskID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, ai_locality) VALUES ('itest calloop', $1, 'any') RETURNING id`, calLoopSlug).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, status) VALUES ($1, 'itest calloop task', 'in_progress') RETURNING id`,
		projectID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	// The booked block, exactly as sendCalendarBlock + RecordOwnCalendarEvent
	// leave it: delivery sent with the reserved id, raw row present AND
	// normalized (so the poll's content_hash short-circuits and Normalize's
	// confirm hook cannot be the thing that fires).
	start := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Hour)
	end := start.Add(15 * time.Minute)
	eventID := "sbitestcalloop" + strconv.FormatInt(time.Now().UnixNano(), 32)
	extID := CalendarExternalID(eventID)
	resource := `{"kind":"calendar#event","id":` + mustJSON(t, eventID) +
		`,"status":"confirmed","summary":"itest calloop block","start":{"dateTime":` +
		mustJSON(t, start.Format(time.RFC3339)) + `},"end":{"dateTime":` +
		mustJSON(t, end.Format(time.RFC3339)) + `}}`

	var deliveryID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, subject, body, status, approval_source,
		                          from_account_id, starts_at, ends_at, sent_external_id, sent_at, created_by)
		 VALUES ($1,'calendar',$2,'Focus block','reserved','sent','switchboard',$3,$4,$5,$6, now(),
		         'itest-calloop') RETURNING id`,
		taskID, calLoopAcct, accountID, start, end, extID).Scan(&deliveryID); err != nil {
		t.Fatalf("seed delivery: %v", err)
	}
	sink := NewPGSink(pool)
	if err := sink.RecordOwnCalendarEvent(ctx, accountID, json.RawMessage(resource)); err != nil {
		t.Fatalf("send-time record: %v", err)
	}

	// A second, UNOBSERVED delivery: its id is never in any snapshot, so it
	// must stay unconfirmed — the predicate matches observed ids, not "every
	// open calendar delivery of the account".
	var otherID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, subject, body, status, approval_source,
		                          from_account_id, starts_at, ends_at, sent_external_id, sent_at, created_by)
		 VALUES ($1,'calendar',$2,'Other block','reserved','sent','switchboard',$3,$4,$5,$6, now(),
		         'itest-calloop') RETURNING id`,
		taskID, calLoopAcct, accountID, start.Add(time.Hour), end.Add(time.Hour),
		CalendarExternalID(eventID+"x")).Scan(&otherID); err != nil {
		t.Fatalf("seed unobserved delivery: %v", err)
	}

	var tasksBefore int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks`).Scan(&tasksBefore); err != nil {
		t.Fatalf("count tasks: %v", err)
	}

	source := &calLoopSource{email: calLoopAcct, events: []string{resource}}
	accounts := []Account{{ID: accountID, Email: calLoopAcct, CalendarInAvailability: true}}

	// ---- poll 1: the snapshot observes the event id -----------------------
	if _, err := RunPipedreamCalendar(ctx, source, sink, accounts, Config{}); err != nil {
		t.Fatalf("poll 1: %v", err)
	}
	var confirmed bool
	if err := pool.QueryRow(ctx,
		`SELECT confirmed_at IS NOT NULL FROM deliveries WHERE id=$1`, deliveryID).Scan(&confirmed); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if !confirmed {
		t.Fatalf("delivery %d is unconfirmed after a poll whose verified snapshot carried %s. Criterion 26 "+
			"(§F amendment): the send-time record stamps normalized_at, the content_hash short-circuits, so "+
			"the poll's OBSERVATION is the only loop-closure evidence there is", deliveryID, extID)
	}
	confirmEvents := func() int {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_confirmed'`,
			taskID).Scan(&n); err != nil {
			t.Fatalf("count delivery_confirmed: %v", err)
		}
		return n
	}
	if n := confirmEvents(); n != 1 {
		t.Fatalf("delivery_confirmed events after poll 1 = %d, want exactly 1", n)
	}
	var payload struct {
		DeliveryID     int64  `json:"delivery_id"`
		MatchedEventID string `json:"matched_event_id"`
	}
	var rawPayload []byte
	if err := pool.QueryRow(ctx,
		`SELECT payload FROM task_events WHERE task_id=$1 AND event_type='delivery_confirmed'`,
		taskID).Scan(&rawPayload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if err := json.Unmarshal(rawPayload, &payload); err != nil {
		t.Fatalf("payload does not parse: %v (%s)", err, rawPayload)
	}
	if payload.DeliveryID != deliveryID || payload.MatchedEventID != extID {
		t.Errorf("payload = %+v, want delivery %d / %s", payload, deliveryID, extID)
	}

	if err := pool.QueryRow(ctx,
		`SELECT confirmed_at IS NOT NULL FROM deliveries WHERE id=$1`, otherID).Scan(&confirmed); err != nil {
		t.Fatalf("read unobserved delivery: %v", err)
	}
	if confirmed {
		t.Errorf("the UNOBSERVED delivery %d got confirmed; the sweep must match observed ids only", otherID)
	}

	// ---- poll 2: same snapshot — idempotent, no second event ---------------
	if _, err := RunPipedreamCalendar(ctx, source, sink, accounts, Config{}); err != nil {
		t.Fatalf("poll 2: %v", err)
	}
	if n := confirmEvents(); n != 1 {
		t.Errorf("delivery_confirmed events after poll 2 = %d, want still exactly 1 (confirmed_at IS NULL "+
			"is the replay guard)", n)
	}

	// Criterion 28: no task was ever created from a re-ingested calendar event.
	var tasksAfter int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks`).Scan(&tasksAfter); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if tasksAfter != tasksBefore {
		t.Errorf("count(*) FROM tasks went %d -> %d across the polls; the calendar path must create no tasks",
			tasksBefore, tasksAfter)
	}
	if source.calls != 2 {
		t.Errorf("the source was polled %d times, want 2 (the fixture is not exercising what it claims)", source.calls)
	}
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal %q: %v", s, err)
	}
	return string(b)
}
