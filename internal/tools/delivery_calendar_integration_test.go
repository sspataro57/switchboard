//go:build integration

package tools_test

// Integration tests for the calendar booking path (SWT-28 /
// docs/tickets/calendar-booking_SPEC.md, acceptance criteria 8, 11, 12, 15,
// 18-22, and the verification protocol's step-2 list). Build-tagged
// `integration` AND env-gated on DATABASE_URL. Every call goes through
// executor.Execute with the REAL policy Matrix — the only route to a handler
// (invariant 3) — over the compose db. The Pipedream write route is replaced by
// an injected fake; NEVER a live workflow call, never a real Google calendar.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run CalendarBook ./internal/tools/
//
// GREENFIELD NOTE: migration 0020, the book_calendar_block tool, the
// tools.CalendarBooker seam and draft_delivery's calendar branch do not exist,
// so this file compile-FAILs and, once it compiles, fails on the MISSING SCHEMA
// (deliveries.starts_at / source_accounts.calendar_write_enabled) rather than on
// a nil pointer — the fixture seeds the columns explicitly so the first red is
// the honest one. Imposed seam (SPEC criterion 24, the NewDeliveryBridgeFromEnv
// shape):
//
//	package tools
//	// CalendarBooker is the ONE adapter seam to the Pipedream write route; its
//	// only caller is sendCalendarBlock, reached only from the two registered
//	// handlers (invariant 3).
//	type CalendarBooker interface {
//	    CreateEvent(ctx context.Context, req google.CreateEventRequest) (google.CreateEventResponse, error)
//	}
//	func SetCalendarBooker(b CalendarBooker)
//
// ANTI-DATE-ROT: every instant here is derived from time.Now(). A frozen
// calendar date drifts out of [now-30d, now+90d] and LoadBusy then refuses for
// the HORIZON reason, which would turn every case in this file green-for-the-
// wrong-reason (they are refusal tests) or red-for-no-reason (the success one).
//
// Cross-suite discipline (SWT-6 mutual-cleanup pact): everything this suite
// owns is scoped by the account email prefix 'itest-calendar-book-%', the
// project slug 'itest-calendar-book-proj', the sync_runs marker
// stats->>'itest', and an audit id watermark. Readiness is GLOBAL by design
// (SWT-24 criterion 3), so calBookFreshenForeign makes every calendar this
// suite does not own current — the production precondition, set explicitly.
// No assertion here is a global count.

import (
	"context"
	"encoding/json"
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
	calBookPrefix = "itest-calendar-book-"
	calBookAcct   = "itest-calendar-book-a@example.com"
	calBookOther  = "itest-calendar-book-out-of-scope@example.com"
	calBookSlug   = "itest-calendar-book-proj"
	calBookClient = "itest-calendar-book-client"
	calBookMarker = "itest-calendar-book"

	// THE CENTRAL ACTOR OF THIS TICKET. `mcp:worker:` is an autonomous Claude
	// Code console arriving over the MCP transport — precisely the identity
	// policy.humanOnly exists to refuse, and precisely the identity Q1's answer
	// (b) says must be able to book. Using a human actor for the success case
	// would assert nothing the approve tier did not already give us.
	calBookWorker = "mcp:worker:itest-calendar-book"
	// The human actor, needed for the verbs that stay human-only
	// (set_sending_frozen, send_delivery).
	calBookHuman = "opsctl:itest-calendar-book"
)

// ---- the injected write adapter -----------------------------------------------

// fakeCalendarBooker stands in for the Pipedream write route. It reads the
// deliveries row AT CALL TIME, which is how the test proves criterion 20's
// ordering: status='sending' and sent_external_id are committed BEFORE the
// network call, so a crash between the two leaves a row that can never resend
// (invariant 4) rather than one that can double-book.
type fakeCalendarBooker struct {
	pool *pgxpool.Pool

	// mu keeps the recorded fields race-clean: the concurrent-booking test
	// drives two goroutines, and although exactly one is supposed to reach
	// the write route, a regression of that very property must fail with the
	// crafted "want exactly 1" message — not a -race abort (delta review F5).
	mu sync.Mutex

	calls   int
	lastReq google.CreateEventRequest

	// observed inside CreateEvent
	preSendStatus     string
	preSendExternalID string

	// injectable failure; nil means success
	err error
}

func (f *fakeCalendarBooker) CreateEvent(ctx context.Context, req google.CreateEventRequest) (google.CreateEventResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastReq = req
	_ = f.pool.QueryRow(ctx,
		`SELECT status, COALESCE(sent_external_id,'') FROM deliveries WHERE sent_external_id=$1`,
		google.CalendarExternalID(req.EventID)).Scan(&f.preSendStatus, &f.preSendExternalID)

	if f.err != nil {
		return google.CreateEventResponse{}, f.err
	}
	// A Calendar v3 Event resource echoing exactly what was asked for, in a
	// DIFFERENT but equal serialization of the same instants (+02:00 for a Z
	// request) — the production shape, and the thing criterion 5 says must
	// compare equal.
	zone := time.FixedZone("+0200", 2*60*60)
	start, _ := time.Parse(time.RFC3339, req.Start)
	end, _ := time.Parse(time.RFC3339, req.End)
	event := map[string]any{
		"kind":        "calendar#event",
		"id":          req.EventID,
		"status":      "confirmed",
		"summary":     req.Summary,
		"description": req.Description,
		"start":       map[string]any{"dateTime": start.In(zone).Format(time.RFC3339)},
		"end":         map[string]any{"dateTime": end.In(zone).Format(time.RFC3339)},
	}
	raw, _ := json.Marshal(event)
	return google.CreateEventResponse{
		SchemaVersion: google.PipedreamCalendarSchemaVersion,
		Action:        "create_event",
		CalendarID:    req.CalendarID,
		Status:        "ok",
		Created:       true,
		Event:         raw,
	}, nil
}

// ---- fixture ------------------------------------------------------------------

type calBookFixture struct {
	pool      *pgxpool.Pool
	ex        *executor.Executor
	booker    *fakeCalendarBooker
	accountID int64
	projectID int64
	taskID    int64
	auditMin  int64
}

func newCalBookFixture(t *testing.T, ctx context.Context) *calBookFixture {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use the compose db on :5433. " +
			"This suite BOOKS CALENDAR EVENTS through a fake — against production it would also write " +
			"deliveries and raw_source_items rows into the live funnel")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)

	f := &calBookFixture{pool: pool, booker: &fakeCalendarBooker{pool: pool}}
	if err := pool.QueryRow(ctx, `SELECT COALESCE(max(id),0) FROM audit_events`).Scan(&f.auditMin); err != nil {
		t.Fatalf("audit watermark: %v", err)
	}
	f.cleanup(t, ctx)
	t.Cleanup(func() { f.cleanup(t, context.Background()) })

	// Working hours wide open, in UTC: this suite books at a fixed hour of
	// TOMORROW, and propose_slots' working-hours filter would otherwise make
	// the assertions depend on which weekday the suite happens to run.
	t.Setenv("AVAIL_TZ", "UTC")
	t.Setenv("AVAIL_WORK_START", "0")
	t.Setenv("AVAIL_WORK_END", "24")
	t.Setenv("AVAIL_WORK_DAYS", "sun,mon,tue,wed,thu,fri,sat")
	t.Setenv("AVAIL_MAX_SYNC_AGE", "")
	os.Unsetenv("AVAIL_MAX_SYNC_AGE")

	// The bookable account: google, IN availability scope, write-enabled.
	// calendar_write_enabled is a NEW column (0020) and deliberately NOT a
	// reuse of send_enabled — flipping calendar booking on must not also turn
	// SMTP sending on for the same mailbox.
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, calendar_in_availability,
		                              calendar_write_enabled, send_enabled)
		 VALUES ('google', $1, true, true, false) RETURNING id`, calBookAcct).Scan(&f.accountID); err != nil {
		t.Fatalf("seed source_account (0020's calendar_write_enabled is expected to be missing until the "+
			"migration lands): %v", err)
	}
	f.calendarRun(t, ctx, f.accountID, "ok", 0)
	f.freshenForeign(t, ctx)

	f.projectID = seedProject(t, ctx, pool, calBookSlug, calBookClient)

	// The work task is IN PROGRESS on purpose: the commonest real booking is
	// "reserve two hours to finish this", i.e. incidental to work that is not
	// done. That is the case Q2 answered (b) for — R8 firing here would try to
	// mark an in-progress task delivered, fail, and still burn the dedup key.
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1, 'itest-calendar-book work task', 'claude', 'in_progress') RETURNING id`,
		f.projectID).Scan(&f.taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	f.ex = executor.New(reg, checker, audit.NewPGStore(pool))

	tools.SetCalendarBooker(f.booker)
	t.Cleanup(func() { tools.SetCalendarBooker(nil) })
	return f
}

func (f *calBookFixture) cleanup(t *testing.T, ctx context.Context) {
	t.Helper()
	owned := `(SELECT id FROM source_accounts WHERE account_email LIKE '` + calBookPrefix + `%')`
	ourTasks := `(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + calBookSlug + `'))`
	watermark := strconv.FormatInt(f.auditMin, 10)
	stmts := []string{
		`UPDATE ops_flags SET value='{"frozen": false}' WHERE name='sending_frozen'`,
		`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE id > ` + watermark +
			` AND actor LIKE '%itest-calendar-book%')`,
		`DELETE FROM audit_events WHERE id > ` + watermark + ` AND actor LIKE '%itest-calendar-book%'`,
		`DELETE FROM approvals WHERE subject_type='delivery' AND subject_id IN
			(SELECT id FROM deliveries WHERE task_id IN ` + ourTasks + `)`,
		`DELETE FROM task_events WHERE task_id IN ` + ourTasks,
		`DELETE FROM deliveries WHERE task_id IN ` + ourTasks,
		`DELETE FROM normalized_events WHERE raw_source_item_id IN
			(SELECT id FROM raw_source_items WHERE source_account_id IN ` + owned + `)`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + owned,
		`DELETE FROM sync_runs WHERE stats->>'itest' = '` + calBookMarker + `'`,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + owned,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + calBookSlug + `')`,
		`DELETE FROM decisions WHERE project_id IN (SELECT id FROM projects WHERE slug='` + calBookSlug + `')`,
		`DELETE FROM projects WHERE slug='` + calBookSlug + `'`,
		`DELETE FROM source_accounts WHERE account_email LIKE '` + calBookPrefix + `%'`,
	}
	for _, s := range stmts {
		if _, err := f.pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
}

// calendarRun writes one calendar-phase sync_runs row `ago` in the past.
// RELATIVE to now(), never a literal timestamp — a literal ages past
// AVAIL_MAX_SYNC_AGE and turns the freshness case into a permanent pass.
func (f *calBookFixture) calendarRun(t *testing.T, ctx context.Context, accountID int64, status string, ago time.Duration) {
	t.Helper()
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO sync_runs (source_account_id, started_at, finished_at, status, stats)
		 VALUES ($1, now() - $2::interval, now() - $2::interval, $3,
		         jsonb_build_object('phase','calendar','itest',$4::text))`,
		accountID, ago.String(), status, calBookMarker); err != nil {
		t.Fatalf("insert calendar run: %v", err)
	}
}

// freshenForeign makes every in-scope calendar this suite does NOT own current.
// LoadBusy's readiness is global: one stale foreign account refuses every
// booking, which would make each refusal case pass for the wrong reason.
func (f *calBookFixture) freshenForeign(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO sync_runs (source_account_id, started_at, finished_at, status, stats)
		 SELECT id, now(), now(), 'ok', jsonb_build_object('phase','calendar','itest',$2::text)
		   FROM source_accounts
		  WHERE provider='google' AND calendar_in_availability AND account_email NOT LIKE $1`,
		calBookPrefix+"%", calBookMarker); err != nil {
		t.Fatalf("freshen foreign calendars: %v", err)
	}
}

// calBookBlock returns a [start, end) 15-minute block at a fixed hour of
// TOMORROW in UTC. Derived from time.Now() (never a frozen date) and pinned to
// an hour so it can never straddle midnight, which would put it outside the
// wide-open-but-still-per-day working window.
func calBookBlock(hour int) (time.Time, time.Time) {
	d := time.Now().UTC().Add(24 * time.Hour)
	start := time.Date(d.Year(), d.Month(), d.Day(), hour, 0, 0, 0, time.UTC)
	return start, start.Add(15 * time.Minute)
}

// draftCalendar drafts a calendar delivery as the given actor and returns its id.
func (f *calBookFixture) draftCalendar(t *testing.T, ctx context.Context, actor string, start, end time.Time) int64 {
	t.Helper()
	args := `{"task_id":` + itoa(f.taskID) + `,"channel":"calendar","target_ref":"` + calBookAcct + `",` +
		`"subject":"Focus block","body":"reserved for SWT-28 review",` +
		`"start":"` + start.Format(time.RFC3339) + `","end":"` + end.Format(time.RFC3339) + `"}`
	out := callOK(t, ctx, f.ex, actor, "draft_delivery", args)
	var r struct {
		DeliveryID int64 `json:"delivery_id"`
	}
	mustUnmarshal(t, out, &r)
	if r.DeliveryID == 0 {
		t.Fatal("draft_delivery returned delivery_id 0")
	}
	return r.DeliveryID
}

func (f *calBookFixture) book(ctx context.Context, actor string, deliveryID int64) ([]byte, error) {
	res, err := f.ex.Execute(ctx, executor.Call{
		Tool: "book_calendar_block", Actor: actor,
		Args: []byte(`{"delivery_id":` + itoa(deliveryID) + `}`),
	})
	if err != nil {
		return nil, err
	}
	return res.Output, nil
}

func (f *calBookFixture) row(t *testing.T, ctx context.Context, id int64) (status, extID, approvalSource, errText string) {
	t.Helper()
	if err := f.pool.QueryRow(ctx,
		`SELECT status, COALESCE(sent_external_id,''), COALESCE(approval_source,''), COALESCE(error,'')
		   FROM deliveries WHERE id=$1`, id).Scan(&status, &extID, &approvalSource, &errText); err != nil {
		t.Fatalf("read delivery %d: %v", id, err)
	}
	return
}

func (f *calBookFixture) lastAudit(t *testing.T, ctx context.Context, tool, actor string) (status, errText string) {
	t.Helper()
	if err := f.pool.QueryRow(ctx,
		`SELECT status, COALESCE(error,'') FROM audit_events
		  WHERE tool=$1 AND actor=$2 ORDER BY id DESC LIMIT 1`, tool, actor).Scan(&status, &errText); err != nil {
		t.Fatalf("read audit row for %s/%s: %v", tool, actor, err)
	}
	return
}

// ---------------------------------------------------------------------------
// Criterion 8: migration 0020. The CHECK is asserted BY NAME — a calendar row
// missing its interval would not error, it would silently become a delivery
// nothing can book and nothing can confirm.
// ---------------------------------------------------------------------------

func TestCalendarBook_Integration_Migration0020(t *testing.T) {
	ctx := context.Background()
	f := newCalBookFixture(t, ctx)

	var col string
	if err := f.pool.QueryRow(ctx,
		`SELECT column_name FROM information_schema.columns
		  WHERE table_name='source_accounts' AND column_name='calendar_write_enabled'`).Scan(&col); err != nil {
		t.Fatalf("source_accounts.calendar_write_enabled is missing (migration 0020): %v", err)
	}
	for _, c := range []string{"starts_at", "ends_at"} {
		if err := f.pool.QueryRow(ctx,
			`SELECT column_name FROM information_schema.columns
			  WHERE table_name='deliveries' AND column_name=$1`, c).Scan(&col); err != nil {
			t.Fatalf("deliveries.%s is missing (migration 0020): %v", c, err)
		}
	}

	// The default is FALSE and it is flipped by hand, per account. Under the
	// auto tier this column is the ONLY per-account consent an unattended
	// booking has, so a default of true would silently arm every google row.
	var def bool
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, calendar_in_availability)
		 VALUES ('google', $1, false) RETURNING calendar_write_enabled`, calBookOther).Scan(&def); err != nil {
		t.Fatalf("insert a bare google account: %v", err)
	}
	if def {
		t.Errorf("calendar_write_enabled defaults to TRUE; 0020 pins DEFAULT false (send_enabled's convention) " +
			"because it is the auto tier's only per-account consent")
	}

	start, end := calBookBlock(9)
	cases := []struct {
		name     string
		starts   any
		ends     any
		target   any
		fromAcct any
	}{
		{"NULL starts_at", nil, end, calBookAcct, f.accountID},
		{"NULL ends_at", start, nil, calBookAcct, f.accountID},
		{"ends_at before starts_at", end, start, calBookAcct, f.accountID},
		{"NULL target_ref", start, end, nil, f.accountID},
		{"NULL from_account_id", start, end, calBookAcct, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.pool.Exec(ctx,
				`INSERT INTO deliveries (task_id, channel, target_ref, body, subject, status,
				                          from_account_id, starts_at, ends_at, created_by)
				 VALUES ($1,'calendar',$2,'b','s','drafted',$3,$4,$5,'itest-calendar-book')`,
				f.taskID, tc.target, tc.fromAcct, tc.starts, tc.ends)
			if err == nil {
				t.Fatalf("the database accepted a calendar delivery with %s. Criterion 8's CHECK is what "+
					"makes the row's identity structural rather than hopeful: without it the row does not "+
					"error, it becomes a delivery nothing can book and nothing can ever confirm", tc.name)
			}
			if !strings.Contains(err.Error(), "deliveries_calendar_identity_check") {
				t.Errorf("insert failed with %v, want a violation of deliveries_calendar_identity_check. "+
					"Assert the constraint BY NAME — a NOT NULL or a different CHECK would pass this test "+
					"while leaving the channel-scoped rule unwritten", err)
			}
		})
	}

	// The constraint is CHANNEL-SCOPED: it must not start refusing the five
	// live channels, none of which has an interval.
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, created_by)
		 VALUES ($1,'jira_comment','ACME-1','pushed the fix','drafted','itest-calendar-book')`,
		f.taskID); err != nil {
		t.Errorf("a jira_comment delivery with no interval was refused: %v. The CHECK is scoped "+
			"`channel <> 'calendar' OR (...)`; an unscoped one would break every other channel", err)
	}
}

// ---------------------------------------------------------------------------
// THE CENTRAL CLAIM (criteria 18, 20, 21, 22): a NON-HUMAN actor books
// end to end. Asserted with an actor a human gate would have refused, because
// "an agent can book" is exactly what Q1's answer (b) bought and exactly what
// a test using a dashboard: actor would fail to notice going missing.
// ---------------------------------------------------------------------------

func TestCalendarBook_Integration_NonHumanActorBooksEndToEnd(t *testing.T) {
	ctx := context.Background()
	f := newCalBookFixture(t, ctx)
	start, end := calBookBlock(10)

	// Before: propose_slots offers this exact slot.
	if !calBookOffersSlot(t, ctx, f, start, end) {
		t.Fatalf("propose_slots does not offer %s before the booking; the fixture's own precondition is "+
			"broken, so the after-assertion would prove nothing", start.Format(time.RFC3339))
	}

	deliveryID := f.draftCalendar(t, ctx, calBookWorker, start, end)

	// Criterion 11: the account is resolved SERVER-SIDE from target_ref, and
	// target_ref is stored as the DATABASE's spelling of account_email — never
	// the caller's. A non-canonical target_ref is how a delivery becomes
	// permanently unconfirmable (the SWT-13 rule).
	var fromAcct *int64
	var storedTarget string
	var storedStart, storedEnd time.Time
	if err := f.pool.QueryRow(ctx,
		`SELECT from_account_id, target_ref, starts_at, ends_at FROM deliveries WHERE id=$1`,
		deliveryID).Scan(&fromAcct, &storedTarget, &storedStart, &storedEnd); err != nil {
		t.Fatalf("read draft: %v", err)
	}
	if fromAcct == nil || *fromAcct != f.accountID {
		t.Errorf("from_account_id = %v, want the resolved account %d (server-side, never caller-chosen)", fromAcct, f.accountID)
	}
	if storedTarget != calBookAcct {
		t.Errorf("target_ref = %q, want the account_email read back from source_accounts (%q)", storedTarget, calBookAcct)
	}
	if !storedStart.Equal(start) || !storedEnd.Equal(end) {
		t.Errorf("stored interval = %s..%s, want %s..%s (compared as INSTANTS — Postgres returns timestamptz "+
			"in the session zone, so a string compare here would be the round-trip landmine again)",
			storedStart, storedEnd, start, end)
	}

	// The booking itself, as a worker.
	out, err := f.book(ctx, calBookWorker, deliveryID)
	if err != nil {
		t.Fatalf("book_calendar_block as %q failed: %v\n\nThis is the ticket's central claim: Q1 answered "+
			"(b), so an autonomous console must be able to approve-and-send a drafted calendar row in one "+
			"audited call, with no human in the loop and without widening policy.humanOnly", calBookWorker, err)
	}
	var result struct {
		DeliveryID     int64  `json:"delivery_id"`
		Status         string `json:"status"`
		SentExternalID string `json:"sent_external_id"`
		BusySetPending bool   `json:"busy_set_pending"`
	}
	mustUnmarshal(t, out, &result)

	status, extID, approvalSource, errText := f.row(t, ctx, deliveryID)
	if status != "sent" {
		t.Fatalf("delivery %d status = %q (error=%q), want sent", deliveryID, status, errText)
	}
	if approvalSource != "switchboard" {
		t.Errorf("approval_source = %q, want \"switchboard\". Invariant 4: the write route is unreachable "+
			"except from a row that is approved with approval_source='switchboard' — the auto path WRITES "+
			"that approval rather than skipping it", approvalSource)
	}
	if !strings.HasPrefix(extID, "calendar:") {
		t.Errorf("sent_external_id = %q, want a calendar:{event_id} spelling. confirmDelivery matches "+
			"sent_external_id with NO channel predicate (sink.go:312-343), so a bare base32 id could in "+
			"principle be claimed by a passing Message-ID match", extID)
	}
	if result.SentExternalID != extID {
		t.Errorf("result sent_external_id = %q, row = %q", result.SentExternalID, extID)
	}
	if result.BusySetPending {
		t.Errorf("busy_set_pending = true on a successful local record; criterion 23 sets it only when the " +
			"raw+normalized write FAILED")
	}

	// Criterion 20: the id and 'sending' were committed BEFORE the POST.
	if f.booker.calls != 1 {
		t.Fatalf("the write route was called %d times, want exactly 1", f.booker.calls)
	}
	if f.booker.preSendStatus != "sending" {
		t.Errorf("at network-call time the row was %q, want \"sending\". Criterion 20 commits status + "+
			"sent_external_id + send_attempted_at in ONE statement before the POST: a crash in the window "+
			"must leave a row that can never resend (invariant 4), not one that can double-book",
			f.booker.preSendStatus)
	}
	if f.booker.preSendExternalID != extID {
		t.Errorf("at network-call time sent_external_id was %q, want the final %q — the id is OURS and is "+
			"reserved before the call; that is the entire reason a retried timeout is safe",
			f.booker.preSendExternalID, extID)
	}
	if f.booker.lastReq.CalendarID != calBookAcct {
		t.Errorf("write request calendar_id = %q, want %q", f.booker.lastReq.CalendarID, calBookAcct)
	}
	if got := google.CalendarExternalID(f.booker.lastReq.EventID); got != extID {
		t.Errorf("the event id sent to the workflow (%q -> %q) is not the one stored as sent_external_id "+
			"(%q); loop closure is a plain equality on that string", f.booker.lastReq.EventID, got, extID)
	}
	// Criterion 13 / invariant 6: the handler reads the STORED subject and
	// body, which draft_delivery already scrubbed. Nothing about the event may
	// say switchboard.
	if f.booker.lastReq.Summary != "Focus block" {
		t.Errorf("summary = %q, want the stored subject", f.booker.lastReq.Summary)
	}
	if !strings.Contains(f.booker.lastReq.Description, "reserved for SWT-28") {
		t.Errorf("description = %q, want the stored body", f.booker.lastReq.Description)
	}

	// Criterion 18: the approvals row names WHO booked, even when it is a worker.
	var apprCount int
	var decidedBy string
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(max(decided_by),'') FROM approvals
		  WHERE subject_type='delivery' AND subject_id=$1`, deliveryID).Scan(&apprCount, &decidedBy); err != nil {
		t.Fatalf("read approvals: %v", err)
	}
	if apprCount != 1 {
		t.Errorf("approvals rows = %d, want 1", apprCount)
	}
	if decidedBy != calBookWorker {
		t.Errorf("approvals.decided_by = %q, want %q. Under the auto tier this row is the only record of "+
			"WHICH console decided to take the slot", decidedBy, calBookWorker)
	}

	// Criterion 21: one delivery_sent event, carrying the channel key that
	// criterion 29's rule reads.
	if n := eventCount(t, ctx, f.pool, f.taskID, "delivery_sent"); n != 1 {
		t.Fatalf("delivery_sent events = %d, want 1", n)
	}
	var payload map[string]any
	var rawPayload []byte
	if err := f.pool.QueryRow(ctx,
		`SELECT payload FROM task_events WHERE task_id=$1 AND event_type='delivery_sent' ORDER BY id DESC LIMIT 1`,
		f.taskID).Scan(&rawPayload); err != nil {
		t.Fatalf("read delivery_sent payload: %v", err)
	}
	mustUnmarshal(t, rawPayload, &payload)
	if payload["channel"] != "calendar" {
		t.Errorf("delivery_sent payload channel = %v, want \"calendar\". The key is LOAD-BEARING: R8's "+
			"calendar skip (criterion 29) reads it, and an omitted key takes the normal path and burns the "+
			"task's delivery_lifecycle dedup key", payload["channel"])
	}
	if payload["sent_external_id"] != extID {
		t.Errorf("delivery_sent payload sent_external_id = %v, want %q", payload["sent_external_id"], extID)
	}

	// Criterion 22 (raw-first): the returned Event resource is stored VERBATIM
	// under calendar:{id} with a content_hash, and normalized behind it.
	var rawID int64
	var normalizedAt *time.Time
	if err := f.pool.QueryRow(ctx,
		`SELECT id, normalized_at FROM raw_source_items WHERE source_account_id=$1 AND external_id=$2`,
		f.accountID, extID).Scan(&rawID, &normalizedAt); err != nil {
		t.Fatalf("no raw_source_items row for %s: %v\n\nCriterion 22 + invariant 1: the handler stores the "+
			"Google Event resource through PGSink.RecordOwnCalendarEvent — content hash first, normalize "+
			"second. Without it the block is invisible to propose_slots for up to 20 minutes and "+
			"switchboard can propose, and book, the same slot twice", extID, err)
	}
	if normalizedAt == nil {
		t.Errorf("raw item %d has normalized_at NULL; RecordOwnCalendarEvent stamps it after upsertEvent", rawID)
	}
	var evStart, evEnd time.Time
	if err := f.pool.QueryRow(ctx,
		`SELECT starts_at, ends_at FROM normalized_events WHERE raw_source_item_id=$1`, rawID).Scan(&evStart, &evEnd); err != nil {
		t.Fatalf("no normalized_events row behind raw item %d: %v", rawID, err)
	}
	if !evStart.Equal(start) || !evEnd.Equal(end) {
		t.Errorf("normalized event = %s..%s, want the booked instants %s..%s. The fake echoes +02:00 for a Z "+
			"request, exactly as Google does; instants, never strings", evStart, evEnd, start, end)
	}

	// The point of criterion 22, asserted directly: the busy set knows.
	if calBookOffersSlot(t, ctx, f, start, end) {
		t.Errorf("propose_slots still offers %s immediately after booking it. Under the auto tier that is a "+
			"self-inflicted double-booking loop, not a rare race: the next worker proposes the same slot and "+
			"books it again", start.Format(time.RFC3339))
	}

	// Criterion 20 / invariant 4: never resend while sent_external_id is present.
	_, err = f.book(ctx, calBookWorker, deliveryID)
	if err == nil {
		t.Fatal("a second book_calendar_block on the same row SUCCEEDED; a present sent_external_id refuses " +
			"a resend forever (invariant 4)")
	}
	if !strings.Contains(err.Error(), "invariant 4") && !strings.Contains(err.Error(), "sent_external_id") {
		t.Errorf("the resend refusal = %v, want it to name sent_external_id / invariant 4", err)
	}
	if f.booker.calls != 1 {
		t.Errorf("the write route was called %d times after the second attempt, want still 1", f.booker.calls)
	}

	// Invariant 3: an unattended booking is still fully audited.
	if s, _ := f.lastAudit(t, ctx, "book_calendar_block", calBookWorker); s == "" {
		t.Error("no audit_events row for book_calendar_block; every call runs validate -> policy -> audit " +
			"start -> handler -> audit complete, and under the auto tier the audit row is the record")
	}
}

// calBookOffersSlot asks propose_slots for the exact block, over a window that
// contains it, and reports whether it comes back.
func calBookOffersSlot(t *testing.T, ctx context.Context, f *calBookFixture, start, end time.Time) bool {
	t.Helper()
	args := `{"duration_minutes":` + strconv.Itoa(int(end.Sub(start)/time.Minute)) +
		`,"count":6,"window_start":"` + start.Format(time.RFC3339) +
		`","window_end":"` + start.Add(2*time.Hour).Format(time.RFC3339) + `"}`
	res, err := f.ex.Execute(ctx, executor.Call{Tool: "propose_slots", Actor: calBookHuman, Args: []byte(args)})
	if err != nil {
		t.Fatalf("propose_slots refused: %v (the fixture's freshness/horizon preconditions are broken)", err)
	}
	var out struct {
		Slots []struct {
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"slots"`
	}
	mustUnmarshal(t, res.Output, &out)
	for _, s := range out.Slots {
		got, err := time.Parse(time.RFC3339, s.Start)
		if err != nil {
			t.Fatalf("propose_slots returned an unparseable start %q", s.Start)
		}
		if got.Equal(start) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Criterion 15 at the integration level: book_calendar_block aimed at a GMAIL
// row is denied by POLICY with rule channel_mismatch, and the gmail row is
// untouched.
//
// This is the hole premise 14 opens. Without the by-name deny, the gmail branch
// allows the verb the moment it becomes sendShaped — and the verb APPROVES AND
// SENDS in one call, so the handler's own channel check would be the only thing
// between an agent and an unapproved client email.
// ---------------------------------------------------------------------------

func TestCalendarBook_Integration_DeniedOnAGmailRowWithChannelMismatch(t *testing.T) {
	ctx := context.Background()
	f := newCalBookFixture(t, ctx)

	// A gmail delivery on the same task. from_account_id/thread are irrelevant
	// to the policy gate, which resolves only the CHANNEL from delivery_id.
	var gmailID int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, body, subject, status, created_by)
		 VALUES ($1,'gmail','the fix is live','Re: login broken','drafted','itest-calendar-book')
		 RETURNING id`, f.taskID).Scan(&gmailID); err != nil {
		t.Fatalf("seed gmail delivery: %v", err)
	}

	_, err := f.book(ctx, calBookWorker, gmailID)
	if err == nil {
		t.Fatal("book_calendar_block on a GMAIL delivery succeeded. That is an agent sending an unapproved " +
			"client email through the calendar verb — the outer, pure gate (criterion 15) is what must stop " +
			"it, before the handler is ever reached")
	}
	if !strings.Contains(err.Error(), "denied by policy") {
		t.Fatalf("book_calendar_block on gmail = %v, want a POLICY denial. Two gates, and the outer one is "+
			"the pure, unit-testable one; a handler-only refusal leaves the matrix saying yes", err)
	}

	var n int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM policy_decisions p JOIN audit_events a ON a.id=p.audit_event_id
		  WHERE a.actor=$1 AND p.tool='book_calendar_block' AND p.decision='deny' AND p.rule='channel_mismatch'`,
		calBookWorker).Scan(&n); err != nil {
		t.Fatalf("count policy_decisions: %v", err)
	}
	if n < 1 {
		t.Errorf("no deny/channel_mismatch policy_decisions row for book_calendar_block. The rule string is "+
			"what an operator greps for and what distinguishes this from channel_assisted and "+
			"channel_not_live (actor=%s)", calBookWorker)
	}

	status, extID, _, _ := f.row(t, ctx, gmailID)
	if status != "drafted" || extID != "" {
		t.Errorf("the gmail row is now status=%q sent_external_id=%q; a refused call must leave it exactly "+
			"as it was", status, extID)
	}
	if f.booker.calls != 0 {
		t.Errorf("the calendar write route was called %d times for a gmail row", f.booker.calls)
	}
}

// ---------------------------------------------------------------------------
// Criterion 19: the pre-flight refusal — the backstop the auto tier rests on.
// LoadBusy is called with the delivery's own [starts_at, ends_at) and refuses
// on a stale sync, an empty scope, the horizon, or an overlap. The row stays
// APPROVED and otherwise untouched, because a retry after the conflict clears
// must remain possible.
// ---------------------------------------------------------------------------

func TestCalendarBook_Integration_RefusesOnAStaleCalendarSync(t *testing.T) {
	ctx := context.Background()
	f := newCalBookFixture(t, ctx)
	start, end := calBookBlock(11)
	deliveryID := f.draftCalendar(t, ctx, calBookWorker, start, end)

	// Age the account's only successful calendar sync past the 1h default.
	// RELATIVE to now(): a literal timestamp would age out on its own and the
	// case would pass whether or not the guard exists.
	if _, err := f.pool.Exec(ctx,
		`DELETE FROM sync_runs WHERE source_account_id=$1`, f.accountID); err != nil {
		t.Fatalf("clear sync runs: %v", err)
	}
	f.calendarRun(t, ctx, f.accountID, "ok", 90*time.Minute)

	_, err := f.book(ctx, calBookWorker, deliveryID)
	if err == nil {
		t.Fatal("book_calendar_block booked against a calendar whose last successful sync is 90 minutes old. " +
			"A stale sync means the busy set may not know about a meeting that already exists; refusing is " +
			"the same fail-closed rule propose_slots applies, at the last possible moment")
	}
	// The error propagates VERBATIM so audit_events.error reads exactly like a
	// propose_slots refusal — the runbook tells the operator to read them the
	// same way.
	if !strings.Contains(err.Error(), calBookAcct) {
		t.Errorf("the refusal does not name the account it has no fresh data for (%s): %v", calBookAcct, err)
	}
	if !strings.Contains(err.Error(), "calendar not ready") {
		t.Errorf("the refusal = %v, want availability.NotReadyError's text propagated verbatim ("+
			"\"calendar not ready: ...\"); a re-worded refusal is a second spelling of a rule that already "+
			"has one", err)
	}

	status, extID, approvalSource, _ := f.row(t, ctx, deliveryID)
	if status != "approved" {
		t.Errorf("after a pre-flight refusal the row is %q, want \"approved\" and otherwise untouched. "+
			"Nothing is marked failed: a retry once the sync catches up must remain possible", status)
	}
	if extID != "" {
		t.Errorf("a pre-flight refusal reserved sent_external_id=%q; the refusal happens BEFORE any row "+
			"mutation, so no id is burned", extID)
	}
	if approvalSource != "switchboard" {
		t.Errorf("approval_source = %q; the approve half committed before the pre-flight, so it stays", approvalSource)
	}
	if f.booker.calls != 0 {
		t.Errorf("the write route was called %d times despite the pre-flight refusal", f.booker.calls)
	}
	if s, e := f.lastAudit(t, ctx, "book_calendar_block", calBookWorker); s != "error" || !strings.Contains(e, calBookAcct) {
		t.Errorf("audit_events for the refusal = status %q error %q, want status \"error\" carrying the "+
			"reason verbatim", s, e)
	}
}

// The overlap half of criterion 19.
//
// IK, "test the column, not the fixture": the guard's input is
// normalized_events, read through availability.LoadBusy — the ONE door
// (internal/availability/callsites_test.go bans a second reader). To verify
// this test is not merely testing its own fixture, mutate loadEvents' SELECT in
// internal/availability/store.go to return a literal empty set and watch this
// case go GREEN; that is the mutation that proves it bites.
func TestCalendarBook_Integration_RefusesOnAnOverlappingEvent(t *testing.T) {
	ctx := context.Background()
	f := newCalBookFixture(t, ctx)
	start, end := calBookBlock(12)

	// A meeting that starts 5 minutes into the requested block: a PARTIAL
	// overlap, not a containment, because a guard written as "an event starting
	// inside the block" and one written as "any intersection" differ exactly
	// here.
	f.seedBusyEvent(t, ctx, start.Add(5*time.Minute), end.Add(30*time.Minute))

	deliveryID := f.draftCalendar(t, ctx, calBookWorker, start, end)
	_, err := f.book(ctx, calBookWorker, deliveryID)
	if err == nil {
		t.Fatal("book_calendar_block booked over an existing meeting. This is the backstop the whole auto " +
			"tier rests on: the worst an injected call can do is put a block on Salvador's own calendar AT A " +
			"TIME THAT IS PROVABLY FREE — remove the overlap check and that sentence stops being true")
	}
	status, extID, _, _ := f.row(t, ctx, deliveryID)
	if status != "approved" {
		t.Errorf("after an overlap refusal the row is %q, want \"approved\" — a retry once the conflict "+
			"clears must remain possible", status)
	}
	if extID != "" {
		t.Errorf("an overlap refusal burned sent_external_id=%q", extID)
	}
	if f.booker.calls != 0 {
		t.Errorf("the write route was called %d times despite an overlap", f.booker.calls)
	}

	// The control: the same block one hour later, where nothing is busy, books.
	// Without it an implementation that refuses unconditionally passes above.
	freeStart, freeEnd := calBookBlock(15)
	freeID := f.draftCalendar(t, ctx, calBookWorker, freeStart, freeEnd)
	if _, err := f.book(ctx, calBookWorker, freeID); err != nil {
		t.Fatalf("book_calendar_block refused a demonstrably free slot: %v", err)
	}
}

// seedBusyEvent writes a raw item plus its normalized_events row for the owned
// account — raw-first, the shape the connector produces.
func (f *calBookFixture) seedBusyEvent(t *testing.T, ctx context.Context, start, end time.Time) {
	t.Helper()
	extID := google.CalendarExternalID("itestbusy" + strconv.FormatInt(start.UnixNano(), 32))
	var rawID int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,'{}',$3, now()) RETURNING id`,
		f.accountID, extID, "hash-"+extID).Scan(&rawID); err != nil {
		t.Fatalf("seed busy raw item: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO normalized_events (raw_source_item_id, starts_at, ends_at, title, status, transparency)
		 VALUES ($1,$2,$3,'itest existing meeting','confirmed','opaque')`, rawID, start, end); err != nil {
		t.Fatalf("seed busy event: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Criterion 20: calendar_write_enabled is re-read AT SEND, not only at draft.
//
// The column is mutated BETWEEN the two calls, which is the only way to tell a
// send-time check from a draft-time one. Under the auto tier this column is the
// only per-account consent an unattended booking has, and a draft can outlive a
// revocation by any amount of time.
// ---------------------------------------------------------------------------

func TestCalendarBook_Integration_WriteEnabledRevokedBetweenDraftAndSend(t *testing.T) {
	ctx := context.Background()
	f := newCalBookFixture(t, ctx)
	start, end := calBookBlock(13)

	deliveryID := f.draftCalendar(t, ctx, calBookWorker, start, end) // drafted while enabled

	if _, err := f.pool.Exec(ctx,
		`UPDATE source_accounts SET calendar_write_enabled=false WHERE id=$1`, f.accountID); err != nil {
		t.Fatalf("revoke calendar_write_enabled: %v", err)
	}

	_, err := f.book(ctx, calBookWorker, deliveryID)
	if err == nil {
		t.Fatal("book_calendar_block booked onto an account whose calendar_write_enabled was revoked after " +
			"the draft. The go-live gate must be checked at SEND: a draft predates a revocation, and under " +
			"the auto tier this column is the ONLY per-account consent an unattended booking has")
	}
	if !strings.Contains(err.Error(), calBookAcct) {
		t.Errorf("the refusal does not name the account (%s): %v", calBookAcct, err)
	}
	if f.booker.calls != 0 {
		t.Errorf("the write route was called %d times for a write-disabled account", f.booker.calls)
	}
	if _, extID, _, _ := f.row(t, ctx, deliveryID); extID != "" {
		t.Errorf("the refusal reserved sent_external_id=%q", extID)
	}
}

// Criterion 12: draft_delivery refuses a calendar target that is not a
// write-enabled, in-scope google account, and distinguishes the three causes.
// Requiring calendar_in_availability is load-bearing: LoadBusy only reads
// in-scope calendars, so booking onto an out-of-scope one is booking BLIND.
func TestCalendarBook_Integration_DraftRefusesAnUnbookableTarget(t *testing.T) {
	ctx := context.Background()
	f := newCalBookFixture(t, ctx)
	start, end := calBookBlock(14)

	// In scope but NOT write-enabled.
	var notWritable int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, calendar_in_availability, calendar_write_enabled)
		 VALUES ('google', $1, true, false) RETURNING id`, calBookPrefix+"readonly@example.com").Scan(&notWritable); err != nil {
		t.Fatalf("seed read-only account: %v", err)
	}
	f.calendarRun(t, ctx, notWritable, "ok", 0)
	// Write-enabled but OUT of availability scope.
	var outOfScope int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, calendar_in_availability, calendar_write_enabled)
		 VALUES ('google', $1, false, true) RETURNING id`, calBookOther).Scan(&outOfScope); err != nil {
		t.Fatalf("seed out-of-scope account: %v", err)
	}

	cases := []struct {
		name, target string
		wantAnyOf    []string
	}{
		{"no such google account", calBookPrefix + "nobody@example.com", []string{"no google account", "not found", "no such"}},
		{"not in availability scope", calBookOther, []string{"availability", "scope"}},
		{"not write-enabled", calBookPrefix + "readonly@example.com", []string{"write", "calendar_write_enabled"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := `{"task_id":` + itoa(f.taskID) + `,"channel":"calendar","target_ref":"` + tc.target + `",` +
				`"subject":"Focus block","body":"reserved","start":"` + start.Format(time.RFC3339) +
				`","end":"` + end.Format(time.RFC3339) + `"}`
			_, err := f.ex.Execute(ctx, executor.Call{Tool: "draft_delivery", Actor: calBookWorker, Args: []byte(args)})
			if err == nil {
				t.Fatalf("draft_delivery accepted %s (%s). Criterion 12 refuses by name and distinguishes "+
					"the three causes — and requiring calendar_in_availability is load-bearing: LoadBusy "+
					"only reads in-scope calendars, so booking onto an out-of-scope one is booking blind",
					tc.name, tc.target)
			}
			msg := strings.ToLower(err.Error())
			for _, want := range tc.wantAnyOf {
				if strings.Contains(msg, strings.ToLower(want)) {
					return
				}
			}
			t.Errorf("refusal for %s = %v; it must say WHICH of the three causes applies (any of %v)",
				tc.name, err, tc.wantAnyOf)
		})
	}

	// The case-insensitive match on account_email (criterion 11): a caller
	// spelling the address in a different case must still resolve, and the
	// stored target_ref must be the database's spelling.
	upper := strings.ToUpper(calBookAcct)
	args := `{"task_id":` + itoa(f.taskID) + `,"channel":"calendar","target_ref":"` + upper + `",` +
		`"subject":"Focus block","body":"reserved","start":"` + start.Format(time.RFC3339) +
		`","end":"` + end.Format(time.RFC3339) + `"}`
	out := callOK(t, ctx, f.ex, calBookWorker, "draft_delivery", args)
	var r struct {
		DeliveryID int64 `json:"delivery_id"`
	}
	mustUnmarshal(t, out, &r)
	var stored string
	if err := f.pool.QueryRow(ctx, `SELECT target_ref FROM deliveries WHERE id=$1`, r.DeliveryID).Scan(&stored); err != nil {
		t.Fatalf("read target_ref: %v", err)
	}
	if stored != calBookAcct {
		t.Errorf("target_ref stored as %q for a caller who wrote %q; criterion 11 stores the account_email "+
			"READ BACK from the row. A non-canonical target_ref is how a delivery becomes permanently "+
			"unconfirmable, silently (the SWT-13 rule)", stored, upper)
	}
}

// ---------------------------------------------------------------------------
// Criterion 17: the kill switch reaches the AUTO verb. This is the operator's
// stop button for an unattended booker — the one thing that halts a worker that
// has decided to book, since there is no human gate to withhold.
// ---------------------------------------------------------------------------

func TestCalendarBook_Integration_KillSwitchBlocksTheAutoVerb(t *testing.T) {
	ctx := context.Background()
	f := newCalBookFixture(t, ctx)
	start, end := calBookBlock(16)
	deliveryID := f.draftCalendar(t, ctx, calBookWorker, start, end)

	// set_sending_frozen is human-only, as it should be: a worker must not be
	// able to lift its own brake.
	callOK(t, ctx, f.ex, calBookHuman, "set_sending_frozen", `{"frozen":true}`)

	_, err := f.book(ctx, calBookWorker, deliveryID)
	if err == nil {
		t.Fatal("book_calendar_block booked while sending_frozen was set. Criterion 17 puts the verb in " +
			"freezeGated for exactly this: with no human gate on the auto tier, set_sending_frozen is the " +
			"ONLY way to stop a worker mid-loop")
	}
	if !strings.Contains(err.Error(), "denied by policy") {
		t.Errorf("the freeze refusal = %v, want a policy denial (the switch is a matrix rule, not a handler "+
			"check — a handler-only freeze leaves no policy_decisions row to read afterwards)", err)
	}
	var n int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM policy_decisions p JOIN audit_events a ON a.id=p.audit_event_id
		  WHERE a.actor=$1 AND p.tool='book_calendar_block' AND p.decision='deny' AND p.rule='kill_switch'`,
		calBookWorker).Scan(&n); err != nil {
		t.Fatalf("count policy_decisions: %v", err)
	}
	if n < 1 {
		t.Error("no deny/kill_switch policy_decisions row for book_calendar_block")
	}
	if f.booker.calls != 0 {
		t.Errorf("the write route was called %d times while frozen", f.booker.calls)
	}
	if s, _, _, _ := f.row(t, ctx, deliveryID); s != "drafted" {
		t.Errorf("a frozen refusal left the row %q, want it still drafted — the denial happens in policy, "+
			"before the handler's approve half", s)
	}

	// Unfreeze and confirm the same call now works: without this the case above
	// passes against a verb that is simply broken.
	callOK(t, ctx, f.ex, calBookHuman, "set_sending_frozen", `{"frozen":false}`)
	if _, err := f.book(ctx, calBookWorker, deliveryID); err != nil {
		t.Fatalf("book_calendar_block still refused after unfreezing: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Criterion 21, the failure half: a write-route error leaves the row FAILED
// with sent_external_id KEPT and nothing reopened.
//
// Gmail clears the id on a definite SendRejectedError so failed -> approved can
// retry. Here there is deliberately NO reopen path: the safety of a resend
// depends on the workflow's 409 handling, which lives in a human-edited
// third-party workflow. Keeping the id costs nothing (it is derived from this
// delivery and worthless to any other row) and keeps invariant 4 literal.
// ---------------------------------------------------------------------------

func TestCalendarBook_Integration_WriteFailureKeepsTheReservedID(t *testing.T) {
	ctx := context.Background()
	f := newCalBookFixture(t, ctx)
	start, end := calBookBlock(17)
	deliveryID := f.draftCalendar(t, ctx, calBookWorker, start, end)

	f.booker.err = errPipedreamUnreachable{}

	if _, err := f.book(ctx, calBookWorker, deliveryID); err == nil {
		t.Fatal("book_calendar_block reported success while the write route failed")
	}
	status, extID, _, errText := f.row(t, ctx, deliveryID)
	if status != "failed" {
		t.Errorf("after a write failure the row is %q, want \"failed\"", status)
	}
	if extID == "" {
		t.Errorf("the write failure CLEARED sent_external_id. No failure path may clear it here: the send " +
			"may have landed, and reopening the row for a resend would depend on the workflow's 409 handling " +
			"being trustworthy — it lives in a human-edited third-party workflow. Recovery is the read poll " +
			"or a NEW draft")
	}
	if errText == "" {
		t.Error("deliveries.error is empty after a write failure; the classified message is what the runbook " +
			"tells an operator to read")
	}
	if strings.Contains(errText, "pipedream-DISTINCTIVEHOST") {
		t.Errorf("deliveries.error carries the endpoint host: %q", errText)
	}
}

// errPipedreamUnreachable mimics the client's CLASSIFIED transport error:
// no host, no path, no token.
type errPipedreamUnreachable struct{}

func (errPipedreamUnreachable) Error() string {
	return "pipedream calendar workflow unreachable (transport failure; endpoint withheld)"
}
