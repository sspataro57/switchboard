//go:build integration

package tools_test

// SWT-109 (calendar-mcp-tools) integration: swb-first reservations, the
// calendar_push stage's verbs, and the Q1 calendar targeting, against the
// compose db on :5433 with a COUNTING fake booker. Every verb goes through the
// real executor and policy matrix.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	resPrefix     = "itest-res-"
	resDefault    = "itest-res-default@example.com"
	resAvviato    = "itest-res-hoc@example.com"
	resMappedSlug = "itest-res-avviato"
	resOtherSlug  = "itest-res-foundry"
	resMarker     = "itest-res"
	resActor      = "mcp:manual:itest-res"
	pushActor     = "pipeline:calendar_push"
)

// countingBooker records every Google call the push stage makes.
type countingBooker struct {
	mu        sync.Mutex
	creates   []google.CreateEventRequest
	batches   []google.CreateEventsRequest
	deletes   []google.DeleteEventsRequest
	createErr error
	deleteErr error
}

func (b *countingBooker) calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.creates) + len(b.batches) + len(b.deletes)
}

func eventFor(id, start, end, summary, description string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"kind": "calendar#event", "id": id, "status": "confirmed", "summary": summary, "description": description,
		"start": map[string]any{"dateTime": start}, "end": map[string]any{"dateTime": end},
	})
	return raw
}

func (b *countingBooker) CreateEvent(ctx context.Context, req google.CreateEventRequest) (google.CreateEventResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.creates = append(b.creates, req)
	if b.createErr != nil {
		return google.CreateEventResponse{}, b.createErr
	}
	return google.CreateEventResponse{SchemaVersion: google.PipedreamCalendarSchemaVersion, Action: "create_event",
		CalendarID: req.CalendarID, Status: "ok", Created: true,
		Event: eventFor(req.EventID, req.Start, req.End, req.Summary, req.Description)}, nil
}

func (b *countingBooker) CreateEvents(ctx context.Context, req google.CreateEventsRequest) (google.CreateEventsResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.batches = append(b.batches, req)
	if b.createErr != nil {
		return google.CreateEventsResponse{}, b.createErr
	}
	out := google.CreateEventsResponse{SchemaVersion: google.PipedreamCalendarSchemaVersion, Action: "create_events"}
	for _, e := range req.Events {
		out.Results = append(out.Results, google.CreateEventsResult{CalendarID: e.CalendarID, EventID: e.EventID,
			Status: "ok", Created: true, Event: eventFor(e.EventID, e.Start, e.End, e.Summary, e.Description)})
	}
	return out, nil
}

func (b *countingBooker) DeleteEvents(ctx context.Context, req google.DeleteEventsRequest) (google.DeleteEventsResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deletes = append(b.deletes, req)
	if b.deleteErr != nil {
		return google.DeleteEventsResponse{}, b.deleteErr
	}
	out := google.DeleteEventsResponse{SchemaVersion: google.PipedreamCalendarSchemaVersion, Action: "delete_events"}
	for _, e := range req.Events {
		out.Results = append(out.Results, google.DeleteEventsResult{CalendarID: e.CalendarID, EventID: e.EventID, Status: "deleted"})
	}
	return out, nil
}

type resFixture struct {
	pool                  *pgxpool.Pool
	ex                    *executor.Executor
	booker                *countingBooker
	defaultAcct, hocAcct  int64
	mappedTask, otherTask int64
	prevDefault           *int64
	auditMin              int64
	slot                  time.Time // a free hour tomorrow, UTC
}

func newResFixture(t *testing.T, ctx context.Context) *resFixture {
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
	f := &resFixture{pool: pool, booker: &countingBooker{}}
	if err := pool.QueryRow(ctx, `SELECT COALESCE(max(id),0) FROM audit_events`).Scan(&f.auditMin); err != nil {
		t.Fatal(err)
	}
	f.cleanup(t, ctx)
	t.Cleanup(func() { f.cleanup(t, context.Background()) })

	t.Setenv("AVAIL_TZ", "UTC")
	t.Setenv("AVAIL_WORK_START", "0")
	t.Setenv("AVAIL_WORK_END", "24")
	t.Setenv("AVAIL_WORK_DAYS", "sun,mon,tue,wed,thu,fri,sat")
	os.Unsetenv("AVAIL_MAX_SYNC_AGE")
	os.Unsetenv("CALENDAR_WRITE_MONTHLY_BUDGET")
	os.Unsetenv("OPS_SEND_HOURLY_LIMIT")
	t.Setenv("OPS_SEND_HOURLY_LIMIT", "1000")

	// The default calendar is a singleton (partial unique index): borrow it.
	var prev int64
	if err := pool.QueryRow(ctx, `SELECT id FROM source_accounts WHERE calendar_default`).Scan(&prev); err == nil {
		f.prevDefault = &prev
		if _, err := pool.Exec(ctx, `UPDATE source_accounts SET calendar_default=false WHERE id=$1`, prev); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, calendar_in_availability, calendar_write_enabled, calendar_default)
		 VALUES ('google', $1, true, true, true) RETURNING id`, resDefault).Scan(&f.defaultAcct); err != nil {
		t.Fatalf("seed default account: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, calendar_in_availability, calendar_write_enabled)
		 VALUES ('google', $1, true, false) RETURNING id`, resAvviato).Scan(&f.hocAcct); err != nil {
		t.Fatalf("seed avviato account: %v", err)
	}
	f.freshen(t, ctx)

	mapped := seedProject(t, ctx, pool, resMappedSlug, resMappedSlug)
	if _, err := pool.Exec(ctx, `UPDATE projects SET calendar_account_id=$2 WHERE id=$1`, mapped, f.hocAcct); err != nil {
		t.Fatal(err)
	}
	other := seedProject(t, ctx, pool, resOtherSlug, resOtherSlug)
	for _, p := range []struct {
		project int64
		dst     *int64
	}{{mapped, &f.mappedTask}, {other, &f.otherTask}} {
		if err := pool.QueryRow(ctx,
			`INSERT INTO tasks (project_id, title, assignee_type, status) VALUES ($1, 'itest-res task', 'human', 'ready') RETURNING id`,
			p.project).Scan(p.dst); err != nil {
			t.Fatal(err)
		}
	}

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	f.ex = executor.New(reg, checker, audit.NewPGStore(pool))
	f.slot = time.Now().UTC().Add(26 * time.Hour).Truncate(time.Hour)
	return f
}

// freshen gives every in-scope calendar a successful sync just now: readiness
// is global, and one stale foreign account would refuse for the wrong reason.
func (f *resFixture) freshen(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO sync_runs (source_account_id, started_at, finished_at, status, stats)
		 SELECT id, now(), now(), 'ok', jsonb_build_object('phase','calendar','itest',$1::text)
		   FROM source_accounts WHERE provider='google' AND calendar_in_availability`, resMarker); err != nil {
		t.Fatal(err)
	}
}

func (f *resFixture) cleanup(t *testing.T, ctx context.Context) {
	t.Helper()
	owned := `(SELECT id FROM source_accounts WHERE account_email LIKE '` + resPrefix + `%')`
	ourTasks := `(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug LIKE '` + resPrefix + `%'))`
	wm := strconv.FormatInt(f.auditMin, 10)
	stmts := []string{
		`UPDATE ops_flags SET value='{"frozen": false}' WHERE name='sending_frozen'`,
		`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE id > ` + wm + `)`,
		`DELETE FROM audit_events WHERE id > ` + wm,
		`DELETE FROM approvals WHERE subject_type='delivery' AND subject_id IN (SELECT id FROM deliveries WHERE task_id IN ` + ourTasks + `)`,
		`DELETE FROM task_events WHERE task_id IN ` + ourTasks,
		`DELETE FROM deliveries WHERE task_id IN ` + ourTasks + ` AND hold_for_delivery_id IS NOT NULL`,
		`DELETE FROM deliveries WHERE task_id IN ` + ourTasks,
		`DELETE FROM normalized_events WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + owned + `)`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + owned,
		`DELETE FROM sync_runs WHERE stats->>'itest' = '` + resMarker + `'`,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + owned,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug LIKE '` + resPrefix + `%')`,
		`DELETE FROM projects WHERE slug LIKE '` + resPrefix + `%'`,
		`DELETE FROM source_accounts WHERE account_email LIKE '` + resPrefix + `%'`,
	}
	for _, s := range stmts {
		if _, err := f.pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
	if f.prevDefault != nil {
		_, _ = f.pool.Exec(ctx, `UPDATE source_accounts SET calendar_default=true WHERE id=$1`, *f.prevDefault)
	}
}

func (f *resFixture) call(t *testing.T, ctx context.Context, tool, actor string, args any) (map[string]any, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	res, err := f.ex.Execute(ctx, executor.Call{Tool: tool, Actor: actor, Args: raw})
	if err != nil {
		return nil, err
	}
	var out map[string]any
	_ = json.Unmarshal(res.Output, &out)
	return out, nil
}

func (f *resFixture) book(t *testing.T, ctx context.Context, task int64, start time.Time, label string) int64 {
	t.Helper()
	out, err := f.call(t, ctx, "book_time", resActor, map[string]any{"task_id": task,
		"start": start.Format(time.RFC3339), "end": start.Add(time.Hour).Format(time.RFC3339), "label": label})
	if err != nil {
		t.Fatalf("book_time: %v", err)
	}
	return int64(out["delivery_id"].(float64))
}

func (f *resFixture) push(t *testing.T, ctx context.Context) error {
	t.Helper()
	_, err := f.call(t, ctx, "push_calendar_reservations", pushActor, map[string]any{})
	return err
}

func (f *resFixture) row(t *testing.T, ctx context.Context, id int64) (status, reason string, ext *string) {
	t.Helper()
	if err := f.pool.QueryRow(ctx,
		`SELECT status, COALESCE(push_pending_reason,''), sent_external_id FROM deliveries WHERE id=$1`, id).
		Scan(&status, &reason, &ext); err != nil {
		t.Fatal(err)
	}
	return
}

// criteria 9 and 10: a booking is busy the instant it is saved — with no
// booker wired at all.
func TestReservations_BookingIsBusyAtOnce(t *testing.T) {
	ctx := context.Background()
	f := newResFixture(t, ctx)
	tools.SetCalendarBooker(nil)
	id := f.book(t, ctx, f.otherTask, f.slot, "Client call prep")

	out, err := f.call(t, ctx, "propose_slots", resActor, map[string]any{"duration_minutes": 60,
		"window_start": f.slot.Format(time.RFC3339), "window_end": f.slot.Add(time.Hour).Format(time.RFC3339)})
	if err != nil {
		t.Fatalf("propose_slots: %v", err)
	}
	if slots := out["slots"].([]any); len(slots) != 0 {
		t.Errorf("propose_slots offered %v over a slot booked a moment ago", slots)
	}
	if _, err := f.call(t, ctx, "book_time", resActor, map[string]any{"task_id": f.mappedTask,
		"start": f.slot.Add(30 * time.Minute).Format(time.RFC3339), "end": f.slot.Add(90 * time.Minute).Format(time.RFC3339),
		"label": "x"}); err == nil {
		t.Errorf("a second booking overlapping the first was accepted")
	}
	ag, err := f.call(t, ctx, "calendar_list_events", resActor, map[string]any{
		"window_start": f.slot.Format(time.RFC3339), "window_end": f.slot.Add(time.Hour).Format(time.RFC3339)})
	if err != nil {
		t.Fatalf("calendar_list_events: %v", err)
	}
	found := false
	for _, e := range ag["events"].([]any) {
		r, ok := e.(map[string]any)["reservation"].(map[string]any)
		if ok && int64(r["delivery_id"].(float64)) == id && r["state"] == "pending" {
			found = true
		}
	}
	if !found {
		t.Errorf("the agenda does not show booking %d as pending: %v", id, ag["events"])
	}
	status, _, ext := f.row(t, ctx, id)
	if status != "approved" || ext != nil {
		t.Errorf("a fresh booking is %s/%v, want approved with no id", status, ext)
	}
}

// criterion 30: the target comes from the task's project, else the default.
func TestReservations_TargetFromProject(t *testing.T) {
	ctx := context.Background()
	f := newResFixture(t, ctx)
	out, err := f.call(t, ctx, "book_time", resActor, map[string]any{"task_id": f.mappedTask,
		"start": f.slot.Format(time.RFC3339), "end": f.slot.Add(time.Hour).Format(time.RFC3339), "label": "a"})
	if err != nil || out["calendar"] != resAvviato {
		t.Errorf("mapped project booked to %v (%v), want %s", out["calendar"], err, resAvviato)
	}
	out, err = f.call(t, ctx, "book_time", resActor, map[string]any{"task_id": f.otherTask,
		"start": f.slot.Add(2 * time.Hour).Format(time.RFC3339), "end": f.slot.Add(3 * time.Hour).Format(time.RFC3339), "label": "b"})
	if err != nil || out["calendar"] != resDefault {
		t.Errorf("unmapped project booked to %v (%v), want the default %s", out["calendar"], err, resDefault)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE source_accounts SET calendar_default=false WHERE id=$1`, f.defaultAcct); err != nil {
		t.Fatal(err)
	}
	if _, err := f.call(t, ctx, "book_time", resActor, map[string]any{"task_id": f.otherTask,
		"start": f.slot.Add(4 * time.Hour).Format(time.RFC3339), "end": f.slot.Add(5 * time.Hour).Format(time.RFC3339), "label": "c"}); err == nil ||
		!strings.Contains(err.Error(), "no default calendar") {
		t.Errorf("with no default calendar an unmapped booking = %v, want a refusal naming it", err)
	}
}

// criterion 31: a row aimed at a calendar its task's project does not dictate
// is NEVER pushed — ten passes, zero calls. Mutation: drop the re-check in
// pushCalendarReservations and this goes red.
func TestReservations_PrivacyGuardNeverPushesToTheWrongCalendar(t *testing.T) {
	ctx := context.Background()
	f := newResFixture(t, ctx)
	tools.SetCalendarBooker(f.booker)
	t.Cleanup(func() { tools.SetCalendarBooker(nil) })
	// The Avviato calendar IS write-enabled here, so only the guard can stop it.
	if _, err := f.pool.Exec(ctx, `UPDATE source_accounts SET calendar_write_enabled=true WHERE id=$1`, f.hocAcct); err != nil {
		t.Fatal(err)
	}
	id := f.book(t, ctx, f.otherTask, f.slot, "Foundry: Lyle review")
	// Forge: point the unmapped (foundry) task's row at the Avviato calendar.
	if _, err := f.pool.Exec(ctx, `UPDATE deliveries SET from_account_id=$2, target_ref=$3 WHERE id=$1`,
		id, f.hocAcct, resAvviato); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if err := f.push(t, ctx); err != nil {
			t.Fatalf("push pass %d: %v", i, err)
		}
	}
	if n := f.booker.calls(); n != 0 {
		t.Fatalf("the forged row reached the booker %d time(s); an Avviato calendar must never see other work", n)
	}
	if _, reason, _ := f.row(t, ctx, id); reason != "target_mismatch" {
		t.Errorf("reason = %q, want target_mismatch", reason)
	}
}

// criterion 32: no silent fallback, then the push once writes are on; and the
// label never reaches Google.
func TestReservations_NoFallbackAndNeutralText(t *testing.T) {
	ctx := context.Background()
	f := newResFixture(t, ctx)
	tools.SetCalendarBooker(f.booker)
	t.Cleanup(func() { tools.SetCalendarBooker(nil) })
	label := "Prep for Esteban re LlamaSite audit"
	id := f.book(t, ctx, f.mappedTask, f.slot, label)
	for i := 0; i < 3; i++ {
		if err := f.push(t, ctx); err != nil {
			t.Fatal(err)
		}
	}
	if status, reason, _ := f.row(t, ctx, id); status != "approved" || reason != "account_not_write_enabled" {
		t.Errorf("Avviato booking with writes off = %s/%s, want approved/account_not_write_enabled", status, reason)
	}
	if n := f.booker.calls(); n != 0 {
		t.Fatalf("%d call(s) while the target was not write-enabled; it must never fall back to the default", n)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE source_accounts SET calendar_write_enabled=true WHERE id=$1`, f.hocAcct); err != nil {
		t.Fatal(err)
	}
	// Gated rows are re-checked every 30 minutes: move the clock.
	if _, err := f.pool.Exec(ctx, `UPDATE deliveries SET push_next_attempt_at=now() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := f.push(t, ctx); err != nil {
		t.Fatal(err)
	}
	status, reason, ext := f.row(t, ctx, id)
	if status != "sent" || reason != "" || ext == nil {
		t.Fatalf("after writes on: %s/%s/%v, want sent", status, reason, ext)
	}
	if len(f.booker.creates) != 1 {
		t.Fatalf("creates = %d, want 1 (a single row sends create_event)", len(f.booker.creates))
	}
	req := f.booker.creates[0]
	if req.CalendarID != resAvviato {
		t.Errorf("pushed to %s, want %s", req.CalendarID, resAvviato)
	}
	if req.Summary != tools.ReservationSummaryBooking || req.Description != tools.ReservationDescription {
		t.Errorf("summary/description = %q/%q, want the fixed neutral text", req.Summary, req.Description)
	}
	wire, _ := json.Marshal(req)
	for _, word := range []string{"Esteban", "LlamaSite", "Prep"} {
		if strings.Contains(string(wire), word) {
			t.Errorf("the label word %q reached the outbound request: %s", word, wire)
		}
	}
}

// The kill switch stops pushes, not booking.
func TestReservations_KillSwitchStopsPushNotBooking(t *testing.T) {
	ctx := context.Background()
	f := newResFixture(t, ctx)
	tools.SetCalendarBooker(f.booker)
	t.Cleanup(func() { tools.SetCalendarBooker(nil) })
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO ops_flags (name, value) VALUES ('sending_frozen', '{"frozen": true}')
		 ON CONFLICT (name) DO UPDATE SET value='{"frozen": true}'`); err != nil {
		t.Fatal(err)
	}
	id := f.book(t, ctx, f.otherTask, f.slot, "frozen but saved")
	if err := f.push(t, ctx); err == nil {
		t.Errorf("push ran under the kill switch")
	}
	if n := f.booker.calls(); n != 0 {
		t.Errorf("%d call(s) under the kill switch", n)
	}
	if status, _, ext := f.row(t, ctx, id); status != "approved" || ext != nil {
		t.Errorf("row under the kill switch = %s/%v, want untouched approved", status, ext)
	}
}

// criterion 12 and 22: cancel releases an unpushed row with no call and
// deletes a pushed one in one batched call.
func TestReservations_CancelAndDelete(t *testing.T) {
	ctx := context.Background()
	f := newResFixture(t, ctx)
	tools.SetCalendarBooker(f.booker)
	t.Cleanup(func() { tools.SetCalendarBooker(nil) })
	a := f.book(t, ctx, f.otherTask, f.slot, "a")
	b := f.book(t, ctx, f.otherTask, f.slot.Add(2*time.Hour), "b")
	if err := f.push(t, ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.booker.batches) != 1 || len(f.booker.batches[0].Events) != 2 {
		t.Fatalf("two due rows should go in ONE create_events call: creates=%d batches=%d", len(f.booker.creates), len(f.booker.batches))
	}
	c := f.book(t, ctx, f.otherTask, f.slot.Add(4*time.Hour), "c")
	for _, id := range []int64{a, c} {
		if _, err := f.call(t, ctx, "cancel_reservation", resActor, map[string]any{"delivery_id": id}); err != nil {
			t.Fatalf("cancel %d: %v", id, err)
		}
	}
	if out, err := f.call(t, ctx, "cancel_reservation", resActor, map[string]any{"delivery_id": a}); err != nil || out["changed"].(float64) != 0 {
		t.Errorf("a second cancel = %v, %v; want changed 0", out, err)
	}
	var cReleased, aReleased bool
	_ = f.pool.QueryRow(ctx, `SELECT released_at IS NOT NULL FROM deliveries WHERE id=$1`, c).Scan(&cReleased)
	if !cReleased {
		t.Errorf("an unpushed booking is not released at once on cancel")
	}
	before := f.booker.calls()
	if _, err := f.call(t, ctx, "apply_hold_releases", pushActor, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if f.booker.calls() != before+1 || len(f.booker.deletes) != 1 || len(f.booker.deletes[0].Events) != 1 {
		t.Fatalf("deletes = %+v, want one call deleting the one pushed row", f.booker.deletes)
	}
	_ = f.pool.QueryRow(ctx, `SELECT released_at IS NOT NULL FROM deliveries WHERE id=$1`, a).Scan(&aReleased)
	if !aReleased {
		t.Errorf("the pushed booking is not released after the delete")
	}
	if status, _, _ := f.row(t, ctx, b); status != "sent" {
		t.Errorf("the untouched booking is %s, want sent", status)
	}
}

// The same-id retry: a failed push keeps its id and is retried with it only
// after a read poll that started after the attempt.
func TestReservations_SameIDRetryOnlyAfterVerifiedAbsence(t *testing.T) {
	ctx := context.Background()
	f := newResFixture(t, ctx)
	tools.SetCalendarBooker(f.booker)
	t.Cleanup(func() { tools.SetCalendarBooker(nil) })
	id := f.book(t, ctx, f.otherTask, f.slot, "retry me")
	f.booker.createErr = errors.New("pipedream calendar workflow unreachable (transport failure)")
	if err := f.push(t, ctx); err != nil {
		t.Fatal(err)
	}
	status, reason, ext := f.row(t, ctx, id)
	if status != "failed" || reason != "transport_error" || ext == nil {
		t.Fatalf("after a transport error: %s/%s/%v, want failed/transport_error with the id kept", status, reason, ext)
	}
	first := f.booker.creates[0].EventID
	f.booker.createErr = nil
	if _, err := f.pool.Exec(ctx, `UPDATE deliveries SET push_next_attempt_at=now() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := f.push(t, ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.booker.creates) != 1 {
		t.Fatalf("retried before any poll could say the event never landed (creates=%d)", len(f.booker.creates))
	}
	f.freshen(t, ctx) // a poll that started after the failed attempt
	if _, err := f.pool.Exec(ctx, `UPDATE deliveries SET push_next_attempt_at=now() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := f.push(t, ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.booker.creates) != 2 || f.booker.creates[1].EventID != first {
		t.Fatalf("retry creates = %d, id %q; want 2 with the SAME id %q", len(f.booker.creates), f.booker.creates[len(f.booker.creates)-1].EventID, first)
	}
	if status, _, _ := f.row(t, ctx, id); status != "sent" {
		t.Errorf("after the retry the row is %s, want sent", status)
	}
}

// The credit guard holds rows as pending instead of spending a call.
func TestReservations_CreditBudget(t *testing.T) {
	ctx := context.Background()
	f := newResFixture(t, ctx)
	tools.SetCalendarBooker(f.booker)
	t.Cleanup(func() { tools.SetCalendarBooker(nil) })
	t.Setenv("CALENDAR_WRITE_MONTHLY_BUDGET", "0")
	id := f.book(t, ctx, f.otherTask, f.slot, "budget")
	if err := f.push(t, ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.booker.calls(); n != 0 {
		t.Errorf("%d call(s) over budget", n)
	}
	if status, reason, _ := f.row(t, ctx, id); status != "approved" || reason != "credit_budget" {
		t.Errorf("over budget: %s/%s, want approved/credit_budget", status, reason)
	}
}

// criterion 24: a released reservation is never confirmed by a poll that
// still shows its event. Mutation: drop the released_at predicate in
// ConfirmObservedCalendarDeliveries and this goes red.
func TestReservations_ReleasedIsNeverConfirmed(t *testing.T) {
	ctx := context.Background()
	f := newResFixture(t, ctx)
	tools.SetCalendarBooker(f.booker)
	t.Cleanup(func() { tools.SetCalendarBooker(nil) })
	id := f.book(t, ctx, f.otherTask, f.slot, "release me")
	if err := f.push(t, ctx); err != nil {
		t.Fatal(err)
	}
	_, _, ext := f.row(t, ctx, id)
	if _, err := f.pool.Exec(ctx,
		`UPDATE deliveries SET release_requested_at=now(), released_at=now(), release_reason='cancelled' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := google.NewPGSink(f.pool).ConfirmObservedCalendarDeliveries(ctx, f.defaultAcct, []string{*ext}); err != nil {
		t.Fatal(err)
	}
	var confirmed bool
	_ = f.pool.QueryRow(ctx, `SELECT confirmed_at IS NOT NULL FROM deliveries WHERE id=$1`, id).Scan(&confirmed)
	if confirmed {
		t.Errorf("a released reservation was confirmed by a stale observation")
	}
}

// criteria 16 and 17: a sent offer's windows become holds (repaired by the
// push stage if the at-send insert was missed), and release_offer_holds keeps
// the picked one and releases the rest.
func TestReservations_OfferHolds(t *testing.T) {
	ctx := context.Background()
	f := newResFixture(t, ctx)
	tools.SetCalendarBooker(f.booker)
	t.Cleanup(func() { tools.SetCalendarBooker(nil) })
	w1, w2 := f.slot, f.slot.Add(3*time.Hour)
	windows, _ := json.Marshal([]tools.OfferWindow{{Start: w1, End: w1.Add(time.Hour)}, {Start: w2, End: w2.Add(time.Hour)}})
	var offer int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, body, subject, status, from_account_id, sent_at, sent_external_id, offered_windows)
		 VALUES ($1, 'gmail', 'times', 'Re: meeting', 'sent', $2, now(), '<itest-res-offer@example.com>', $3::jsonb) RETURNING id`,
		f.otherTask, f.defaultAcct, string(windows)).Scan(&offer); err != nil {
		t.Fatalf("seed sent offer: %v", err)
	}
	if err := f.push(t, ctx); err != nil {
		t.Fatal(err)
	}
	var holds, pushed int
	_ = f.pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE status='sent') FROM deliveries WHERE hold_for_delivery_id=$1`, offer).
		Scan(&holds, &pushed)
	if holds != 2 || pushed != 2 {
		t.Fatalf("holds=%d pushed=%d, want 2 and 2", holds, pushed)
	}
	for _, r := range f.booker.batches[0].Events {
		if r.Summary != tools.ReservationSummaryHold {
			t.Errorf("a hold was pushed as %q, want Hold", r.Summary)
		}
	}
	if _, err := f.call(t, ctx, "release_offer_holds", resActor, map[string]any{"delivery_id": offer, "reason": "expired"}); err == nil {
		t.Errorf("reason expired was accepted over MCP")
	}
	if _, err := f.call(t, ctx, "release_offer_holds", resActor, map[string]any{"delivery_id": offer,
		"keep_start": w1.Add(30 * time.Minute).Format(time.RFC3339)}); err == nil {
		t.Errorf("a keep_start matching no window was accepted")
	}
	out, err := f.call(t, ctx, "release_offer_holds", resActor, map[string]any{"delivery_id": offer,
		"keep_start": w2.Format(time.RFC3339)})
	if err != nil {
		t.Fatalf("release_offer_holds: %v", err)
	}
	if out["changed"].(float64) != 2 {
		t.Errorf("changed = %v, want 2 (one kept, one released)", out["changed"])
	}
	again, _ := f.call(t, ctx, "release_offer_holds", resActor, map[string]any{"delivery_id": offer, "keep_start": w2.Format(time.RFC3339)})
	if again["changed"].(float64) != 0 {
		t.Errorf("a repeat call changed %v rows; it must be idempotent", again["changed"])
	}
	var kept, requested int
	_ = f.pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE hold_kept_at IS NOT NULL AND hold_expires_at IS NULL),
		        count(*) FILTER (WHERE release_requested_at IS NOT NULL)
		   FROM deliveries WHERE hold_for_delivery_id=$1`, offer).Scan(&kept, &requested)
	if kept != 1 || requested != 1 {
		t.Errorf("kept=%d release_requested=%d, want 1 and 1", kept, requested)
	}
	if _, err := f.call(t, ctx, "apply_hold_releases", pushActor, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if len(f.booker.deletes) != 1 || len(f.booker.deletes[0].Events) != 1 {
		t.Errorf("deletes = %+v, want one call for the released hold", f.booker.deletes)
	}
}

// criterion 33: migration 0049's seed matches accounts on provider AND email.
func TestMigration0049_SeedSQLMatchesProviderAndEmail(t *testing.T) {
	b, err := os.ReadFile("../../migrations/0049_calendar_reservations.sql")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"provider = 'google' AND lower(account_email) = 'sspataro@gmail.com'",
		"provider = 'google' AND lower(account_email) = 'salvador@handsonconnect.org'",
		"'collaboratory', 'a-millon', 'reengine', 'llamasite-sec'",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("0049 seed is missing %q", want)
		}
	}
	_ = fmt.Sprint
}

// Review finding 1: target_ref is what goes on the wire, so forging ONLY it
// (from_account_id still the project's own) must be held too, and the call
// must carry the project's calendar.
func TestReservations_PrivacyGuardChecksTargetRefToo(t *testing.T) {
	ctx := context.Background()
	f := newResFixture(t, ctx)
	tools.SetCalendarBooker(f.booker)
	t.Cleanup(func() { tools.SetCalendarBooker(nil) })
	id := f.book(t, ctx, f.otherTask, f.slot, "Foundry review")
	if _, err := f.pool.Exec(ctx, `UPDATE deliveries SET target_ref=$2 WHERE id=$1`, id, resAvviato); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_, _ = f.pool.Exec(ctx, `UPDATE deliveries SET push_next_attempt_at=now() WHERE id=$1`, id)
		if err := f.push(t, ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.booker.calls(); n != 0 {
		t.Fatalf("a row whose target_ref names the Avviato calendar reached the booker %d time(s)", n)
	}
	if _, reason, _ := f.row(t, ctx, id); reason != "target_mismatch" {
		t.Errorf("reason = %q, want target_mismatch", reason)
	}
}

// Review finding 6 / criterion 19: a conflict at push time is terminal — out
// of the busy set and never retried.
func TestReservations_ConflictIsTerminal(t *testing.T) {
	ctx := context.Background()
	f := newResFixture(t, ctx)
	tools.SetCalendarBooker(f.booker)
	t.Cleanup(func() { tools.SetCalendarBooker(nil) })
	a := f.book(t, ctx, f.otherTask, f.slot, "a")
	// A hand-inserted overlapping reservation, not yet due: busy, so A conflicts.
	var b int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, subject, status, approval_source, from_account_id,
		                         starts_at, ends_at, reservation_label, booking_verified, push_next_attempt_at)
		 VALUES ($1,'calendar',$2,'Reserved.','Busy','approved','switchboard',$3,$4,$5,'b',true, now()+interval '1 day')
		 RETURNING id`, f.otherTask, resDefault, f.defaultAcct, f.slot.Add(30*time.Minute), f.slot.Add(90*time.Minute)).Scan(&b); err != nil {
		t.Fatal(err)
	}
	if err := f.push(t, ctx); err != nil {
		t.Fatal(err)
	}
	status, reason, _ := f.row(t, ctx, a)
	if status != "failed" || reason != "conflict" {
		t.Fatalf("A after a conflict = %s/%s, want failed/conflict", status, reason)
	}
	_, _ = f.pool.Exec(ctx, `UPDATE deliveries SET push_next_attempt_at=now() WHERE id=$1`, a)
	if _, err := f.pool.Exec(ctx, `DELETE FROM deliveries WHERE id=$1`, b); err != nil {
		t.Fatal(err)
	}
	if err := f.push(t, ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.booker.calls(); n != 0 {
		t.Errorf("a conflicted reservation was pushed later (%d calls)", n)
	}
}

// Review finding 2: a failed delete backs off instead of spending a credit
// every sweep.
func TestReservations_FailedDeleteBacksOff(t *testing.T) {
	ctx := context.Background()
	f := newResFixture(t, ctx)
	tools.SetCalendarBooker(f.booker)
	t.Cleanup(func() { tools.SetCalendarBooker(nil) })
	id := f.book(t, ctx, f.otherTask, f.slot, "x")
	if err := f.push(t, ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.call(t, ctx, "cancel_reservation", resActor, map[string]any{"delivery_id": id}); err != nil {
		t.Fatal(err)
	}
	f.booker.deleteErr = errors.New("pipedream calendar workflow: HTTP 400: Error in workflow")
	for i := 0; i < 3; i++ {
		if _, err := f.call(t, ctx, "apply_hold_releases", pushActor, map[string]any{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.booker.deletes) != 1 {
		t.Errorf("deletes after a failure = %d, want 1 (backoff, not a credit per sweep)", len(f.booker.deletes))
	}
	if n, _ := tools.ReleaseInboxCount(ctx, f.pool); n != 0 {
		t.Errorf("the failed delete is still due right away (%d)", n)
	}
}

// Review finding 9 / criterion 4: a pushed, unconfirmed reservation shows
// ONCE, as its Google event carrying the reservation.
func TestReservations_AgendaShowsAPushedBookingOnce(t *testing.T) {
	ctx := context.Background()
	f := newResFixture(t, ctx)
	tools.SetCalendarBooker(f.booker)
	t.Cleanup(func() { tools.SetCalendarBooker(nil) })
	id := f.book(t, ctx, f.otherTask, f.slot, "once")
	if err := f.push(t, ctx); err != nil {
		t.Fatal(err)
	}
	ag, err := f.call(t, ctx, "calendar_list_events", resActor, map[string]any{
		"window_start": f.slot.Format(time.RFC3339), "window_end": f.slot.Add(time.Hour).Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range ag["events"].([]any) {
		m := e.(map[string]any)
		if r, ok := m["reservation"].(map[string]any); ok && int64(r["delivery_id"].(float64)) == id {
			n++
			if r["state"] != "pushed" || m["on_google"] != true {
				t.Errorf("pushed booking rendered as %v", m)
			}
		}
	}
	if n != 1 {
		t.Errorf("the pushed booking appears %d times, want 1", n)
	}
}

// criterion 3: the agenda refuses when an in-scope calendar's last sync is
// older than the max age, and returns no events.
func TestReservations_AgendaRefusesWhenStale(t *testing.T) {
	ctx := context.Background()
	f := newResFixture(t, ctx)
	var stale int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, calendar_in_availability) VALUES ('google', $1, true) RETURNING id`,
		resPrefix+"stale@example.com").Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO sync_runs (source_account_id, started_at, finished_at, status, stats)
		 VALUES ($1, now()-interval '25 hours', now()-interval '25 hours', 'ok', jsonb_build_object('phase','calendar','itest',$2::text))`,
		stale, resMarker); err != nil {
		t.Fatal(err)
	}
	out, err := f.call(t, ctx, "calendar_list_events", resActor, map[string]any{})
	if err == nil {
		t.Fatalf("a 25h-stale calendar answered: %v", out)
	}
	if !strings.Contains(err.Error(), "calendar not ready") || !strings.Contains(err.Error(), "stale@example.com") {
		t.Errorf("refusal %q does not read like LoadBusy's, naming the account", err)
	}
}

// criterion 6, the delivery 236 incident: a 15:00 New York event on a
// Tuesday is listed at 15:00 local, and propose_slots over 14:00-16:00 never
// overlaps it.
func TestReservations_IncidentRegressionTuesdayStandup(t *testing.T) {
	ctx := context.Background()
	f := newResFixture(t, ctx)
	t.Setenv("AVAIL_TZ", "America/New_York")
	ny, _ := time.LoadLocation("America/New_York")
	day := time.Now().In(ny).AddDate(0, 0, 1)
	for day.Weekday() != time.Tuesday {
		day = day.AddDate(0, 0, 1)
	}
	at := func(h, m int) time.Time { return time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, ny) }
	ev, _ := json.Marshal(map[string]any{"kind": "calendar#event", "id": "itestresstandup", "status": "confirmed",
		"summary": "SE/Collab Weekly Standup", "start": map[string]any{"dateTime": at(15, 0).Format(time.RFC3339)},
		"end": map[string]any{"dateTime": at(15, 30).Format(time.RFC3339)}})
	if err := google.NewPGSink(f.pool).RecordOwnCalendarEvent(ctx, f.hocAcct, ev); err != nil {
		t.Fatalf("seed standup: %v", err)
	}
	ag, err := f.call(t, ctx, "calendar_list_events", resActor, map[string]any{
		"window_start": at(0, 0).Format("2006-01-02"), "window_end": at(0, 0).AddDate(0, 0, 1).Format("2006-01-02")})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range ag["events"].([]any) {
		m := e.(map[string]any)
		if m["title"] == "SE/Collab Weekly Standup" {
			found = true
			if !strings.HasPrefix(m["start"].(string), at(15, 0).Format("2006-01-02")+"T15:00:00-0") {
				t.Errorf("standup listed at %v, want 15:00 New York", m["start"])
			}
		}
	}
	if !found {
		t.Fatalf("the standup is not on the agenda: %v", ag["events"])
	}
	out, err := f.call(t, ctx, "propose_slots", resActor, map[string]any{"duration_minutes": 30, "count": 10,
		"window_start": at(14, 0).Format(time.RFC3339), "window_end": at(16, 0).Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	for _, sl := range out["slots"].([]any) {
		m := sl.(map[string]any)
		st, _ := time.Parse(time.RFC3339, m["start"].(string))
		en, _ := time.Parse(time.RFC3339, m["end"].(string))
		if st.Before(at(15, 30)) && en.After(at(15, 0)) {
			t.Errorf("propose_slots offered %v..%v over the 15:00 standup", m["start"], m["end"])
		}
	}
	if out["timezone"] != "America/New_York" {
		t.Errorf("timezone = %v", out["timezone"])
	}
}

// criteria 13 and 15: offered windows are checked STRICTLY when a draft is
// edited and again before send; a busy window refuses the send before any
// Message-ID is reserved, and the row stays approved.
func TestReservations_OfferWindowsFailClosedAtDraftAndSend(t *testing.T) {
	ctx := context.Background()
	f := newResFixture(t, ctx)
	human := "opsctl:itest-res"
	var draft int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, body, subject, status, from_account_id, created_by)
		 VALUES ($1,'gmail','times','Re: meeting','drafted',$2,$3) RETURNING id`, f.otherTask, f.defaultAcct, human).Scan(&draft); err != nil {
		t.Fatal(err)
	}
	f.book(t, ctx, f.otherTask, f.slot, "already busy")
	busyWin := []map[string]string{{"start": f.slot.Add(30 * time.Minute).Format(time.RFC3339), "end": f.slot.Add(90 * time.Minute).Format(time.RFC3339)}}
	if _, err := f.call(t, ctx, "update_delivery", human, map[string]any{"delivery_id": draft, "offered_windows": busyWin}); err == nil {
		t.Errorf("a draft offering a busy window was accepted")
	}
	freeWin := []map[string]string{{"start": f.slot.Add(3 * time.Hour).Format(time.RFC3339), "end": f.slot.Add(4 * time.Hour).Format(time.RFC3339)}}
	if _, err := f.call(t, ctx, "update_delivery", human, map[string]any{"delivery_id": draft, "offered_windows": freeWin}); err != nil {
		t.Fatalf("a free window was refused: %v", err)
	}
	var stored string
	_ = f.pool.QueryRow(ctx, `SELECT COALESCE(offered_windows::text,'') FROM deliveries WHERE id=$1`, draft).Scan(&stored)
	if !strings.Contains(stored, f.slot.Add(3*time.Hour).UTC().Format("2006-01-02T15")) {
		t.Errorf("offered_windows stored as %q", stored)
	}

	// Approve it, then make the window busy behind its back: the send must refuse.
	if _, err := f.pool.Exec(ctx, `UPDATE deliveries SET status='approved', approval_source='switchboard' WHERE id=$1`, draft); err != nil {
		t.Fatal(err)
	}
	f.book(t, ctx, f.otherTask, f.slot.Add(3*time.Hour), "took the window")
	g := &fakeGmailSender{pool: f.pool}
	tools.SetGmailSender(g)
	t.Cleanup(func() { tools.SetGmailSender(nil) })
	if _, err := f.call(t, ctx, "send_delivery", human, map[string]any{"delivery_id": draft}); err == nil ||
		!strings.Contains(err.Error(), "no longer free") {
		t.Fatalf("send over a now-busy offered window = %v, want a refusal", err)
	}
	if g.calls != 0 {
		t.Errorf("the email went out (%d calls) although its offered window was taken", g.calls)
	}
	status, _, ext := f.row(t, ctx, draft)
	if status != "approved" || ext != nil {
		t.Errorf("after the refusal the offer is %s/%v, want approved with no Message-ID", status, ext)
	}
}

// holdWatchingSender reports, at the moment of SMTP, how many live holds the
// offer has — the codex finding: they must exist BEFORE the email leaves.
type holdWatchingSender struct {
	pool    *pgxpool.Pool
	offer   int64
	atSend  int
	calls   int
	outcome error
}

func (s *holdWatchingSender) Send(ctx context.Context, _ string, _ []byte, _ string) (string, error) {
	s.calls++
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE hold_for_delivery_id=$1 AND release_requested_at IS NULL`,
		s.offer).Scan(&s.atSend)
	return "", s.outcome
}

// Codex review: an offer's windows are reserved under the allocation lock
// BEFORE SMTP; a definite rejection frees them, an ambiguous failure keeps
// them (the email may have gone out).
func TestReservations_OfferHoldsReservedBeforeTheEmailLeaves(t *testing.T) {
	ctx := context.Background()
	f := newResFixture(t, ctx)
	cleanupDeliveryData(t, ctx, f.pool)
	t.Cleanup(func() { cleanupDeliveryData(t, context.Background(), f.pool) })
	fx := seedDeliveryFixture(t, ctx, f.pool)
	ex := deliveryExecutor(f.pool)
	f.freshen(t, ctx)
	// Runs BEFORE cleanupDeliveryData (LIFO): our sync runs reference its account.
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM sync_runs WHERE stats->>'itest' = $1`, resMarker)
	})

	offer := func(start time.Time) int64 {
		d := draftGmail(t, ctx, ex, fx.parentID, fx.threadID)
		w, _ := json.Marshal([]tools.OfferWindow{{Start: start, End: start.Add(time.Hour)}})
		if _, err := f.pool.Exec(ctx, `UPDATE deliveries SET offered_windows=$2::jsonb WHERE id=$1`, d, string(w)); err != nil {
			t.Fatal(err)
		}
		approve(t, ctx, ex, d)
		return d
	}

	d1 := offer(f.slot)
	rej := &holdWatchingSender{pool: f.pool, offer: d1, outcome: &google.SendRejectedError{Status: 550, Body: "rejected"}}
	tools.SetGmailSender(rej)
	t.Cleanup(func() { tools.SetGmailSender(nil) })
	if _, err := ex.Execute(ctx, executor.Call{Tool: "send_delivery", Actor: delActor, Args: []byte(`{"delivery_id":` + itoa(d1) + `}`)}); err == nil {
		t.Fatal("a rejecting sender must fail the send")
	}
	if rej.calls != 1 || rej.atSend != 1 {
		t.Fatalf("at SMTP the offer had %d live hold(s) (calls=%d), want 1: the window must be reserved before the email leaves", rej.atSend, rej.calls)
	}
	var live int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE hold_for_delivery_id=$1 AND released_at IS NULL`, d1).Scan(&live)
	if live != 0 {
		t.Errorf("after a definite rejection %d hold(s) are still live; nothing went out, so nothing is held", live)
	}

	d2 := offer(f.slot.Add(3 * time.Hour))
	amb := &holdWatchingSender{pool: f.pool, offer: d2, outcome: context.DeadlineExceeded}
	tools.SetGmailSender(amb)
	_, _ = ex.Execute(ctx, executor.Call{Tool: "send_delivery", Actor: delActor, Args: []byte(`{"delivery_id":` + itoa(d2) + `}`)})
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE hold_for_delivery_id=$1 AND release_requested_at IS NULL`, d2).Scan(&live)
	if amb.atSend != 1 || live != 1 {
		t.Errorf("ambiguous send: holds at SMTP=%d, live after=%d; want 1 and 1 (the email may have gone out)", amb.atSend, live)
	}
	// A booking can no longer take the held window.
	if _, err := f.call(t, ctx, "book_time", resActor, map[string]any{"task_id": f.otherTask,
		"start": f.slot.Add(3 * time.Hour).Format(time.RFC3339), "end": f.slot.Add(4 * time.Hour).Format(time.RFC3339), "label": "x"}); err == nil {
		t.Errorf("book_time took a window an offer is holding")
	}
}

// Codex review: a push interrupted after it committed 'sending' is recovered
// past the send lease — same id, only after verified absence — and a cancel
// of such a row is eventually deleted.
func TestReservations_StuckSendingIsRecovered(t *testing.T) {
	ctx := context.Background()
	f := newResFixture(t, ctx)
	tools.SetCalendarBooker(f.booker)
	t.Cleanup(func() { tools.SetCalendarBooker(nil) })
	id := f.book(t, ctx, f.otherTask, f.slot, "stuck")
	ext := google.CalendarExternalID(google.CalendarEventID(id, 42))
	if _, err := f.pool.Exec(ctx,
		`UPDATE deliveries SET status='sending', sent_external_id=$2, send_attempted_at=now()-interval '20 minutes' WHERE id=$1`,
		id, ext); err != nil {
		t.Fatal(err)
	}
	f.freshen(t, ctx) // a poll after the attempt that did not see it
	if err := f.push(t, ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.booker.creates) != 1 || "calendar:"+f.booker.creates[0].EventID != ext {
		t.Fatalf("stuck row retried with %d create(s) %+v; want 1 with the SAME id", len(f.booker.creates), f.booker.creates)
	}
	if status, _, _ := f.row(t, ctx, id); status != "sent" {
		t.Errorf("recovered row is %s, want sent", status)
	}
}

// Codex review: every write invocation is counted durably, so retries cannot
// slip past the monthly budget.
func TestReservations_WriteCounterCountsEveryInvocation(t *testing.T) {
	ctx := context.Background()
	f := newResFixture(t, ctx)
	tools.SetCalendarBooker(f.booker)
	t.Cleanup(func() { tools.SetCalendarBooker(nil) })
	if _, err := f.pool.Exec(ctx, `DELETE FROM ops_flags WHERE name='calendar_write_invocations'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM ops_flags WHERE name='calendar_write_invocations'`)
	})
	// A single-row create counts 2 (create_event may POST twice).
	t.Setenv("CALENDAR_WRITE_MONTHLY_BUDGET", "4")
	id := f.book(t, ctx, f.otherTask, f.slot, "budget")
	f.booker.createErr = errors.New("pipedream calendar workflow unreachable (transport failure)")
	for i := 0; i < 2; i++ {
		_, _ = f.pool.Exec(ctx, `UPDATE deliveries SET status=CASE WHEN status='failed' THEN status ELSE status END, push_next_attempt_at=now() WHERE id=$1`, id)
		f.freshen(t, ctx)
		if err := f.push(t, ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.booker.creates) != 2 {
		t.Fatalf("creates = %d, want 2 (one first try, one verified retry)", len(f.booker.creates))
	}
	_, _ = f.pool.Exec(ctx, `UPDATE deliveries SET push_next_attempt_at=now() WHERE id=$1`, id)
	f.freshen(t, ctx)
	if err := f.push(t, ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.booker.creates) != 2 {
		t.Errorf("a third invocation went out over a budget of 4 (creates=%d): retries must be counted", len(f.booker.creates))
	}
	if _, reason, _ := f.row(t, ctx, id); reason != "credit_budget" {
		t.Errorf("reason = %q, want credit_budget", reason)
	}
}

// Codex review 2: a pass whose rows all drop out (stale calendar) spends no
// budget, however many times it repeats.
func TestReservations_StalePassesSpendNoBudget(t *testing.T) {
	ctx := context.Background()
	f := newResFixture(t, ctx)
	tools.SetCalendarBooker(f.booker)
	t.Cleanup(func() { tools.SetCalendarBooker(nil) })
	_, _ = f.pool.Exec(ctx, `DELETE FROM ops_flags WHERE name='calendar_write_invocations'`)
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM ops_flags WHERE name='calendar_write_invocations'`) })
	id := f.book(t, ctx, f.otherTask, f.slot, "stale")
	t.Setenv("AVAIL_MAX_SYNC_AGE", "1ns") // every calendar is now stale
	for i := 0; i < 5; i++ {
		_, _ = f.pool.Exec(ctx, `UPDATE deliveries SET push_next_attempt_at=now() WHERE id=$1`, id)
		if err := f.push(t, ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, reason, _ := f.row(t, ctx, id); reason != "calendar_stale" {
		t.Errorf("reason = %q, want calendar_stale", reason)
	}
	var counted string
	_ = f.pool.QueryRow(ctx, `SELECT COALESCE(value->>'n','0') FROM ops_flags WHERE name='calendar_write_invocations'`).Scan(&counted)
	if counted != "" && counted != "0" {
		t.Errorf("stale passes counted %s write invocations; nothing was dispatched", counted)
	}
	if f.booker.calls() != 0 {
		t.Errorf("%d calls on a stale calendar", f.booker.calls())
	}
}

// Codex review 2: the legacy synchronous path refuses a swb-first
// reservation — a cancelled booking must never be created through it.
func TestReservations_LegacyPathRefusesReservations(t *testing.T) {
	ctx := context.Background()
	f := newResFixture(t, ctx)
	tools.SetCalendarBooker(f.booker)
	t.Cleanup(func() { tools.SetCalendarBooker(nil) })
	id := f.book(t, ctx, f.otherTask, f.slot, "via the side door")
	if _, err := f.call(t, ctx, "cancel_reservation", resActor, map[string]any{"delivery_id": id}); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"book_calendar_block", "send_delivery"} {
		actor := resActor
		if tool == "send_delivery" {
			actor = "opsctl:itest-res"
		}
		if _, err := f.call(t, ctx, tool, actor, map[string]any{"delivery_id": id}); err == nil {
			t.Errorf("%s created a cancelled reservation", tool)
		}
	}
	if n := f.booker.calls(); n != 0 {
		t.Errorf("%d Google call(s) for a cancelled reservation through the legacy path", n)
	}
}

// Codex review 3: the legacy synchronous path counts its write (2: create_event
// may POST twice) in the transaction that reserves it.
func TestReservations_LegacyBookingIsCounted(t *testing.T) {
	ctx := context.Background()
	f := newResFixture(t, ctx)
	tools.SetCalendarBooker(f.booker)
	t.Cleanup(func() { tools.SetCalendarBooker(nil) })
	_, _ = f.pool.Exec(ctx, `DELETE FROM ops_flags WHERE name='calendar_write_invocations'`)
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM ops_flags WHERE name='calendar_write_invocations'`) })
	out, err := f.call(t, ctx, "draft_delivery", resActor, map[string]any{"task_id": f.otherTask, "channel": "calendar",
		"target_ref": resDefault, "subject": "Focus", "body": "legacy block",
		"start": f.slot.Format(time.RFC3339), "end": f.slot.Add(time.Hour).Format(time.RFC3339)})
	if err != nil {
		t.Fatalf("draft legacy block: %v", err)
	}
	id := int64(out["delivery_id"].(float64))
	if _, err := f.call(t, ctx, "book_calendar_block", resActor, map[string]any{"delivery_id": id}); err != nil {
		t.Fatalf("book_calendar_block: %v", err)
	}
	var n string
	_ = f.pool.QueryRow(ctx, `SELECT COALESCE(value->>'n','') FROM ops_flags WHERE name='calendar_write_invocations'`).Scan(&n)
	if n != "2" {
		t.Errorf("legacy booking counted %q, want 2", n)
	}
	if len(f.booker.creates) != 1 {
		t.Errorf("creates = %d, want 1", len(f.booker.creates))
	}
}
