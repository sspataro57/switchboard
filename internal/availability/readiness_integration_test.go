//go:build integration

package availability_test

// Integration tests for the fail-closed free/busy read (SWT-24 /
// docs/tickets/calendar-availability_SPEC.md, acceptance criteria 3, 4, 5, 6, 7
// and 13). Build-tagged `integration` AND env-gated on DATABASE_URL. Run with:
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 ./internal/availability/
//
// WHY THESE ARE INTEGRATION TESTS AND NOT UNIT TESTS. The readiness predicate
// is fed by COLUMNS — source_accounts.calendar_in_availability, sync_runs.status
// and sync_runs.stats->>'phase'. IK landmine 6, verbatim: "for any predicate
// whose input comes from a column, the regression test belongs in the
// integration suite, and it must fail when the column is dropped from the
// SELECT. Mutate the SELECT to a literal and watch it go red; if it stays green
// you have tested your fixture." A unit test over NotReady cannot catch a
// readiness SELECT that forgot `a.calendar_in_availability`, because the unit
// test is the thing supplying the value — that is exactly how the drafts
// locality guard shipped inert. So every case below makes POSTGRES produce the
// discriminating value, and each has a CONTROL that flips the column and
// asserts the opposite verdict.
//
// Cross-suite discipline (SWT-6 mutual-cleanup pact): every account this suite
// creates is 'itest-swt24-avail-%' and every sync_runs row it writes carries
// stats->>'itest' = 'swt24-avail', so cleanup is exact and rerunnable in FK
// order. But readiness scope is GLOBAL by design (criterion 3: every
// provider='google' account with the flag), so a foreign leftover account with
// no calendar sync would make the "must answer" cases refuse. freshenOthers
// therefore stamps a fresh ok calendar run on every in-scope account this suite
// does not own, and removes it again in cleanup. That is not a fudge: it is the
// production precondition ("every in-scope calendar is current") being set up
// explicitly, and it is removed afterwards.
//
// GREENFIELD NOTE (historical): LoadBusy / Request / AccountState /
// NotReadyError did not exist when this file was written, so under
// `-tags integration` it compile-FAILed — the expected red for SPEC-named
// greenfield surface — until the implementation landed. Imposed surface (SPEC
// "Files likely to touch": internal/availability/store.go), with field names
// chosen here because the SPEC names only LoadBusy's shape:
//
//	// Request is everything LoadBusy needs that is not in the database. Now,
//	// MaxSyncAge and the horizon are INJECTED, never read here: the package doc
//	// forbids clock and env reads (criterion 10), AVAIL_MAX_SYNC_AGE is read
//	// only in tools.availabilityConfig (criterion 11), and the horizon is
//	// google.CalendarWindowPast/Future passed by the tool wiring (criterion 7).
//	type Request struct {
//	    WindowStart, WindowEnd time.Time
//	    Now                    time.Time
//	    MaxSyncAge             time.Duration
//	    HorizonPast            time.Duration
//	    HorizonFuture          time.Duration
//	}
//
//	// LoadBusy is the ONE database-backed entry point. It runs the readiness
//	// check BEFORE reading normalized_events and returns *NotReadyError when
//	// any in-scope account is unready or the scope is empty; it refuses a
//	// window not fully inside [Now-HorizonPast, Now+HorizonFuture].
//	func LoadBusy(ctx context.Context, pool *pgxpool.Pool, req Request) ([]Interval, error)
//
//	// loadAccountStates (unexported since the review pass: criterion 1 says
//	// EXACTLY one database-backed entry point) is the SQL half of the
//	// readiness check: the in-scope rows (provider='google' AND
//	// calendar_in_availability) with each one's last successful calendar sync.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/availability"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	availPrefix = "itest-swt24-avail-"
	availMarker = "swt24-avail"
	// The production default (SPEC decision "AVAIL_MAX_SYNC_AGE defaults to 1h").
	availMaxAge = time.Hour
	// The synced horizon, spelled here as the durations the tool wiring will
	// pass from google.CalendarWindowPast/Future. This package must not import
	// a provider adapter.
	horizonPast   = 30 * 24 * time.Hour
	horizonFuture = 90 * 24 * time.Hour
)

func requireComposeAvail(t *testing.T) {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	// Real-db refusal guard (SWT-6 idiom): this suite deletes rows and
	// temporarily rewrites calendar_in_availability, so it must NEVER run
	// against the production ops db.
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use the compose db on :5433")
	}
}

type availFixture struct {
	pool *pgxpool.Pool
	// window is a weekday 09:00-18:00 UTC comfortably inside the horizon.
	winStart, winEnd time.Time
	now              time.Time
}

func newAvailFixture(t *testing.T, ctx context.Context) *availFixture {
	t.Helper()
	requireComposeAvail(t)

	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)

	f := &availFixture{pool: pool, now: time.Now().UTC()}
	// A window a few days out: inside [now-30d, now+90d] so the coverage rule
	// (criterion 7) is satisfied for every case except the one that tests it.
	day := f.now.AddDate(0, 0, 3).Truncate(24 * time.Hour)
	for day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
		day = day.AddDate(0, 0, 1)
	}
	f.winStart = time.Date(day.Year(), day.Month(), day.Day(), 9, 0, 0, 0, time.UTC)
	f.winEnd = time.Date(day.Year(), day.Month(), day.Day(), 18, 0, 0, 0, time.UTC)

	f.cleanup(t, ctx)
	t.Cleanup(func() { f.cleanup(t, context.Background()) })
	return f
}

func (f *availFixture) cleanup(t *testing.T, ctx context.Context) {
	t.Helper()
	owned := `(SELECT id FROM source_accounts WHERE provider='google' AND account_email LIKE '` + availPrefix + `%')`
	stmts := []string{
		`DELETE FROM normalized_events WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + owned + `)`,
		`DELETE FROM raw_source_items  WHERE source_account_id IN ` + owned,
		`DELETE FROM sync_runs         WHERE stats->>'itest' = '` + availMarker + `'`,
		`DELETE FROM sync_runs         WHERE source_account_id IN ` + owned,
		`DELETE FROM source_accounts   WHERE account_email LIKE '` + availPrefix + `%'`,
	}
	for _, s := range stmts {
		if _, err := f.pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
}

// account inserts one source_accounts row and returns its id.
func (f *availFixture) account(t *testing.T, ctx context.Context, provider, suffix string, inAvailability bool) (int64, string) {
	t.Helper()
	email := availPrefix + suffix + "@example.com"
	var id int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, calendar_in_availability, send_enabled)
		 VALUES ($1, $2, $3, false) RETURNING id`, provider, email, inAvailability).Scan(&id); err != nil {
		t.Fatalf("insert %s account %s: %v", provider, email, err)
	}
	return id, email
}

// syncRun writes one finished sync_runs row, exactly as PGSink.StartRun +
// FinishRun would: the phase lives in stats->>'phase' (SPEC premise 6).
func (f *availFixture) syncRun(t *testing.T, ctx context.Context, accountID int64, phase, status string, finishedAgo time.Duration) {
	t.Helper()
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO sync_runs (source_account_id, started_at, finished_at, status, stats)
		 VALUES ($1, now() - $2::interval, now() - $2::interval, $3,
		         jsonb_build_object('phase', $4::text, 'itest', $5::text))`,
		accountID, finishedAgo.String(), status, phase, availMarker); err != nil {
		t.Fatalf("insert sync_run(%s,%s) for account %d: %v", phase, status, accountID, err)
	}
}

// runningRun writes an unfinished run (finished_at NULL): the poller started
// and never came back. "It is running" is not "we hold current data".
func (f *availFixture) runningRun(t *testing.T, ctx context.Context, accountID int64, phase string) {
	t.Helper()
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO sync_runs (source_account_id, status, stats)
		 VALUES ($1, 'running', jsonb_build_object('phase', $2::text, 'itest', $3::text))`,
		accountID, phase, availMarker); err != nil {
		t.Fatalf("insert running sync_run for account %d: %v", accountID, err)
	}
}

// event writes a normalized calendar event through a raw row, the only legal
// shape (invariant 1: raw first, and loadEvents joins through it).
func (f *availFixture) event(t *testing.T, ctx context.Context, accountID int64, externalID string, start, end time.Time) {
	t.Helper()
	var rawID int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1, $2, '{"kind":"itest-calendar"}'::jsonb, $3, now()) RETURNING id`,
		accountID, externalID, "hash-"+externalID).Scan(&rawID); err != nil {
		t.Fatalf("insert raw item %s: %v", externalID, err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO normalized_events (raw_source_item_id, starts_at, ends_at, title, status, transparency, all_day)
		 VALUES ($1, $2, $3, 'itest event', 'confirmed', 'opaque', false)`,
		rawID, start, end); err != nil {
		t.Fatalf("insert normalized event %s: %v", externalID, err)
	}
}

// freshenOthers makes every in-scope account this suite does NOT own current,
// so a foreign leftover cannot decide this suite's verdicts. Removed by
// cleanup (the rows carry the itest marker).
func (f *availFixture) freshenOthers(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO sync_runs (source_account_id, started_at, finished_at, status, stats)
		 SELECT id, now(), now(), 'ok', jsonb_build_object('phase','calendar','itest',$2::text)
		   FROM source_accounts
		  WHERE provider='google' AND calendar_in_availability AND account_email NOT LIKE $1`,
		availPrefix+"%", availMarker); err != nil {
		t.Fatalf("freshen foreign calendars: %v", err)
	}
}

func (f *availFixture) request() availability.Request {
	return availability.Request{
		WindowStart:   f.winStart,
		WindowEnd:     f.winEnd,
		Now:           time.Now().UTC(),
		MaxSyncAge:    availMaxAge,
		HorizonPast:   horizonPast,
		HorizonFuture: horizonFuture,
	}
}

func (f *availFixture) loadBusy(ctx context.Context) ([]availability.Interval, error) {
	return availability.LoadBusy(ctx, f.pool, f.request())
}

// mustRefuse asserts a typed refusal naming the accounts it is refusing for.
func mustRefuse(t *testing.T, err error, wantEmails ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("LoadBusy returned no error; an unready calendar must REFUSE. Returning an empty busy set "+
			"instead makes propose_slots answer \"free all week\" from a table nothing wrote (SPEC premise 1); "+
			"expected the refusal to name %v", wantEmails)
	}
	var nre *availability.NotReadyError
	if !errors.As(err, &nre) {
		t.Fatalf("LoadBusy error is not *availability.NotReadyError: %v", err)
	}
	for _, want := range wantEmails {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %s: %v", want, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Criterion 5(a) + 3: an in-scope google calendar that never synced refuses.
// ---------------------------------------------------------------------------

func TestAvailabilityReadiness_Integration_NeverSyncedRefuses(t *testing.T) {
	ctx := context.Background()
	f := newAvailFixture(t, ctx)
	f.freshenOthers(t, ctx)

	_, email := f.account(t, ctx, "google", "never", true)

	_, err := f.loadBusy(ctx)
	mustRefuse(t, err, email)
	if !strings.Contains(err.Error(), "never") {
		t.Errorf("refusal for an account with no successful calendar sync must say %q: %v", "never", err)
	}
}

// ---------------------------------------------------------------------------
// Criterion 4: the discriminator is a successful CALENDAR sync_run. Each
// sub-case is a mutation guard: drop the phase clause, the status clause or the
// finished_at clause and the matching case goes green when it must be red.
// ---------------------------------------------------------------------------

func TestAvailabilityReadiness_Integration_OnlyASuccessfulCalendarRunCounts(t *testing.T) {
	ctx := context.Background()
	f := newAvailFixture(t, ctx)
	f.freshenOthers(t, ctx)

	cases := []struct {
		name    string
		suffix  string
		seed    func(t *testing.T, id int64)
		refuses bool
	}{
		{
			name:   "fresh ok calendar run is READY",
			suffix: "ok-cal",
			seed:   func(t *testing.T, id int64) { f.syncRun(t, ctx, id, "calendar", "ok", 5*time.Minute) },
		},
		{
			name:   "fresh ok IMAP run is not a calendar sync",
			suffix: "imap-phase",
			// The mail pass runs every 15 minutes and always succeeds; without
			// the phase clause it would certify calendar data that was never
			// fetched. SPEC premise 6: the phase is written at StartRun and has
			// three distinct values, so this IS discriminating (unlike the
			// upworkcrm stats payload, IK).
			seed:    func(t *testing.T, id int64) { f.syncRun(t, ctx, id, "imap", "ok", 5*time.Minute) },
			refuses: true,
		},
		{
			name:    "fresh ok gmail run is not a calendar sync",
			suffix:  "gmail-phase",
			seed:    func(t *testing.T, id int64) { f.syncRun(t, ctx, id, "gmail", "ok", 5*time.Minute) },
			refuses: true,
		},
		{
			name:    "fresh FAILED calendar run does not count",
			suffix:  "err-cal",
			seed:    func(t *testing.T, id int64) { f.syncRun(t, ctx, id, "calendar", "error", 5*time.Minute) },
			refuses: true,
		},
		{
			name:    "unfinished calendar run does not count",
			suffix:  "running-cal",
			seed:    func(t *testing.T, id int64) { f.runningRun(t, ctx, id, "calendar") },
			refuses: true,
		},
		{
			name:    "stale ok calendar run does not count",
			suffix:  "stale-cal",
			seed:    func(t *testing.T, id int64) { f.syncRun(t, ctx, id, "calendar", "ok", 26*time.Hour) },
			refuses: true,
		},
		{
			name:   "a stale run plus a fresh one is READY (the newest wins)",
			suffix: "stale-then-fresh",
			seed: func(t *testing.T, id int64) {
				f.syncRun(t, ctx, id, "calendar", "ok", 26*time.Hour)
				f.syncRun(t, ctx, id, "calendar", "ok", 2*time.Minute)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, email := f.account(t, ctx, "google", tc.suffix, true)
			tc.seed(t, id)
			defer func() {
				if _, err := f.pool.Exec(ctx, `DELETE FROM sync_runs WHERE source_account_id=$1`, id); err != nil {
					t.Fatalf("drop runs: %v", err)
				}
				if _, err := f.pool.Exec(ctx, `DELETE FROM source_accounts WHERE id=$1`, id); err != nil {
					t.Fatalf("drop account: %v", err)
				}
			}()

			_, err := f.loadBusy(ctx)
			if tc.refuses {
				mustRefuse(t, err, email)
				return
			}
			if err != nil {
				t.Fatalf("LoadBusy refused a READY account (%s): %v", email, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Criterion 6: a ready account with ZERO events is ANSWERED, not refused. The
// SPEC's central decision — the freshness signal is the sync, never the count
// of events — and the reason the issue's own phrasing was rejected: it would
// refuse forever for a genuinely empty calendar and could never tell "quiet
// week" from "poller dead".
// ---------------------------------------------------------------------------

func TestAvailabilityReadiness_Integration_FreshSyncWithNoEventsStillAnswers(t *testing.T) {
	ctx := context.Background()
	f := newAvailFixture(t, ctx)
	f.freshenOthers(t, ctx)

	id, email := f.account(t, ctx, "google", "empty-week", true)
	f.syncRun(t, ctx, id, "calendar", "ok", time.Minute)

	busy, err := f.loadBusy(ctx)
	if err != nil {
		t.Fatalf("LoadBusy refused an empty but freshly synced week (%s): %v — the discriminator is the "+
			"freshness of the SYNC, never the count of events", email, err)
	}
	for _, iv := range busy {
		if iv.Start.Before(f.winEnd) && f.winStart.Before(iv.End) {
			t.Errorf("busy interval %v-%v inside a window whose only account has no events; a foreign corpus "+
				"is leaking into this suite", iv.Start, iv.End)
		}
	}

	slots := availability.ProposeSlots(busy, availability.Config{
		WorkStart: 9, WorkEnd: 18,
		Days:        []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday},
		Location:    time.UTC,
		Duration:    30 * time.Minute,
		WindowStart: f.winStart,
		WindowEnd:   f.winEnd,
		Count:       3,
	})
	if len(slots) == 0 {
		t.Errorf("a genuinely empty working day produced no slots; a fresh sync with no meetings must still "+
			"be answerable (window %v-%v)", f.winStart, f.winEnd)
	}
}

// ---------------------------------------------------------------------------
// Criterion 13 (IK landmine 6, verbatim rule) + criterion 3: the readiness
// scope is fed by the calendar_in_availability COLUMN and by provider.
//
// Replace `a.calendar_in_availability` with the literal `true` in the readiness
// SELECT and the first sub-test goes red; drop `provider='google'` and the
// second goes red. Both are column-fed, so neither can be caught by a unit test
// — the unit test would be supplying the value itself.
// ---------------------------------------------------------------------------

func TestAvailabilityReadiness_Integration_ScopeIsTheColumn(t *testing.T) {
	ctx := context.Background()
	f := newAvailFixture(t, ctx)
	f.freshenOthers(t, ctx)

	// An in-scope ANCHOR with a fresh sync, so the scope is never empty: on a
	// pristine compose db this suite's excluded account may be the ONLY google
	// row, and without an anchor the empty-scope refusal (criterion 5b,
	// correct) fires before the assertion this test is actually about. The
	// anchor weakens nothing — the control below flips the excluded flag and
	// still demands a refusal naming the excluded account.
	anchorID, _ := f.account(t, ctx, "google", "anchor", true)
	f.syncRun(t, ctx, anchorID, "calendar", "ok", time.Minute)

	// An excluded google calendar: flag false, never synced, and it owns an
	// event squarely inside the window.
	id, email := f.account(t, ctx, "google", "excluded", false)
	evStart := f.winStart.Add(2 * time.Hour)
	evEnd := evStart.Add(time.Hour)
	f.event(t, ctx, id, "calendar:itest-swt24-excluded", evStart, evEnd)

	busy, err := f.loadBusy(ctx)
	if err != nil {
		t.Fatalf("LoadBusy refused because of an account with calendar_in_availability=FALSE (%s): %v — "+
			"the readiness SELECT is ignoring the column (criterion 13)", email, err)
	}
	for _, iv := range busy {
		if iv.Start.Equal(evStart) && iv.End.Equal(evEnd) {
			t.Errorf("an event on an account with calendar_in_availability=FALSE contributed busy time "+
				"%v-%v; loadEvents must keep the same two predicates the readiness check uses", iv.Start, iv.End)
		}
	}

	// CONTROL. Without this the assertion above proves nothing: an
	// implementation that never refuses at all would pass it. Flip the one
	// column and the same fixture must refuse.
	if _, err := f.pool.Exec(ctx,
		`UPDATE source_accounts SET calendar_in_availability = true WHERE id=$1`, id); err != nil {
		t.Fatalf("flip flag on: %v", err)
	}
	_, err = f.loadBusy(ctx)
	mustRefuse(t, err, email)
	if _, err := f.pool.Exec(ctx,
		`UPDATE source_accounts SET calendar_in_availability = false WHERE id=$1`, id); err != nil {
		t.Fatalf("flip flag back: %v", err)
	}

	// Criterion 3: other providers are out of scope even though almost all of
	// them carry calendar_in_availability=true (SPEC premise 5 — 0001 defaults
	// it true and only slackweb sets it false). A readiness rule phrased as
	// "every account with the flag" would demand calendar data from a GitHub
	// account and refuse forever.
	_, ghEmail := f.account(t, ctx, "github", "github-acct", true)
	if _, err := f.loadBusy(ctx); err != nil {
		t.Fatalf("LoadBusy refused because of a non-google account (%s): %v — scope is provider='google', "+
			"exactly as loadEvents already spells it (criterion 3)", ghEmail, err)
	}
}

// ---------------------------------------------------------------------------
// Criterion 5(b): an EMPTY scope refuses too. "No calendar is in availability
// scope" is not "you have no meetings", and the honest answer to a question we
// have no data source for is a refusal, not an empty busy set.
// ---------------------------------------------------------------------------

func TestAvailabilityReadiness_Integration_EmptyScopeRefuses(t *testing.T) {
	ctx := context.Background()
	f := newAvailFixture(t, ctx)

	// Take every in-scope account out of scope, then put it back. Scoped to
	// this one test and restored in a defer; the suite already refuses to run
	// against the production db.
	rows, err := f.pool.Query(ctx,
		`SELECT id FROM source_accounts WHERE provider='google' AND calendar_in_availability`)
	if err != nil {
		t.Fatalf("list in-scope accounts: %v", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatalf("scan account id: %v", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate accounts: %v", err)
	}

	if _, err := f.pool.Exec(ctx,
		`UPDATE source_accounts SET calendar_in_availability=false WHERE id = ANY($1)`, ids); err != nil {
		t.Fatalf("empty the scope: %v", err)
	}
	defer func() {
		if _, err := f.pool.Exec(context.Background(),
			`UPDATE source_accounts SET calendar_in_availability=true WHERE id = ANY($1)`, ids); err != nil {
			t.Fatalf("restore the scope: %v", err)
		}
	}()

	_, err = f.loadBusy(ctx)
	if err == nil {
		t.Fatalf("LoadBusy answered with NO google calendar in availability scope. That is the fail-open this " +
			"ticket removes wearing its last costume: nothing to read, so nothing is busy, so everything is " +
			"free (criterion 5b)")
	}
	var nre *availability.NotReadyError
	if !errors.As(err, &nre) {
		t.Errorf("empty-scope refusal is not *availability.NotReadyError: %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "calendar") {
		t.Errorf("empty-scope refusal does not mention calendars, so an operator reading audit_events.error "+
			"cannot act on it: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Criterion 7: window coverage. The same fail-open on a second axis — the
// connector only ever fetched [now-30d, now+90d] (SPEC premise 12), so a
// question about day 120 gets an empty busy set from a PERFECTLY fresh sync.
// ---------------------------------------------------------------------------

func TestAvailabilityReadiness_Integration_WindowOutsideTheSyncedHorizonRefused(t *testing.T) {
	ctx := context.Background()
	f := newAvailFixture(t, ctx)
	f.freshenOthers(t, ctx)

	id, _ := f.account(t, ctx, "google", "horizon", true)
	f.syncRun(t, ctx, id, "calendar", "ok", time.Minute)

	// Control: the in-horizon window is answerable, so the refusals below are
	// about the horizon and not about readiness.
	if _, err := f.loadBusy(ctx); err != nil {
		t.Fatalf("control: LoadBusy refused an in-horizon window with a fresh sync: %v", err)
	}

	now := time.Now().UTC()
	cases := []struct {
		name       string
		start, end time.Time
	}{
		{
			name:  "beyond the future horizon",
			start: now.Add(horizonFuture + 24*time.Hour),
			end:   now.Add(horizonFuture + 25*time.Hour),
		},
		{
			name:  "straddling the future horizon",
			start: now.Add(horizonFuture - time.Hour),
			end:   now.Add(horizonFuture + time.Hour),
		},
		{
			name:  "before the past horizon",
			start: now.Add(-horizonPast - 48*time.Hour),
			end:   now.Add(-horizonPast - 47*time.Hour),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := f.request()
			req.WindowStart, req.WindowEnd = tc.start, tc.end
			if _, err := availability.LoadBusy(ctx, f.pool, req); err == nil {
				t.Fatalf("LoadBusy answered for %v-%v, outside [now-%v, now+%v]. Nothing was ever fetched for "+
					"that span, so an empty busy set there means \"we never looked\" — criterion 7",
					tc.start, tc.end, horizonPast, horizonFuture)
			} else if !strings.Contains(strings.ToLower(err.Error()), "window") {
				t.Errorf("the horizon refusal must say which window it is refusing (it lands in "+
					"audit_events.error verbatim): %v", err)
			}
		})
	}
}
