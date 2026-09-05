//go:build integration

package google_test

// Integration tests for the Pipedream calendar transport against real Postgres
// (pipedream-calendar / docs/tickets/pipedream-calendar_SPEC.md, acceptance
// criteria 9, 10, 12, 13, 15 and 16). Build-tagged `integration` AND env-gated
// on DATABASE_URL. The Pipedream endpoint is a local httptest server in every
// case — no real Pipedream, no credential, no network (criterion 21).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 ./internal/connector/google/
//
// CRITERION 21, IN THE FORM THAT MATTERS: never point the fake endpoint at
// OPS_DATABASE_URL. Fabricated calendar data must never reach production, and
// this suite is trivially runnable against any DSN — hence the explicit refusal
// in requireCompose (shared with the other google integration suites).
//
// WHY THESE ARE INTEGRATION TESTS AND NOT UNIT TESTS. Two of them are
// column-fed predicates, which IK says can only be tested against Postgres:
//
//   - criterion 16 (the polled set equals the demanded set) compares two SQL
//     predicates. A unit test cannot catch a predicate drift when the unit test
//     is the thing supplying the value — that is exactly how the drafts
//     locality guard shipped inert.
//   - criterion 12 (no cursor write) is a claim about a jsonb COLUMN being
//     byte-identical afterwards. A fake sink can only prove a method was not
//     called; the column proves nothing wrote it by another route.
//
// CROSS-SUITE DISCIPLINE (premise 13). These fixtures are IN availability scope
// (calendar_in_availability=true) because this ticket's selection IS that
// scope, so they are visible to every propose_slots assertion in the repo while
// they exist: cleanup runs before AND after in FK order, `make integration`
// runs -p 1, and the propose_slots case joins the freshen-foreign-calendars
// pact (integration_test.go:303-316).
//
// ANTI-ROT: every event time is RELATIVE to now, for the reason recorded in
// bridge_pg_integration_test.go — a frozen calendar fixture in this exact area
// failed on 2026-09-02 when it aged out of the now-30d window overnight.
//
// GREENFIELD NOTE: RunPipedreamCalendar, NewPipedreamCalendarClient and
// ListAvailabilityScopeAccounts do not exist yet, so this file compile-FAILs
// under -tags integration — the expected red. The imposed contracts are
// documented in pipedream_test.go and pipedream_ingest_test.go, plus this one
// (internal/connector/google/calendaraccounts.go, named by the SPEC in
// criterion 15 with its arguments):
//
//	// ListAvailabilityScopeAccounts returns provider='google' AND
//	// calendar_in_availability rows — the SAME set availability.LoadBusy
//	// demands freshness for. Deliberately NOT credential-gated: under
//	// Pipedream the credential lives at Pipedream. See its sibling
//	// ListCalendarCredentialedAccounts and the equality test in this file.
//	func ListAvailabilityScopeAccounts(ctx context.Context, pool *pgxpool.Pool, onlyEmail string) ([]Account, error)

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/availability"
	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	pdcPrefix = "itest-pdc-"
	pdcMarker = "itest-pdc"
	pdcToken  = "pdtok-ITEST-0123456789abcdefghijklmnopqrstu"
)

func pdcPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	requireCompose(t)
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	cleanupPDC(t, ctx, pool)
	t.Cleanup(func() { cleanupPDC(t, context.Background(), pool) })
	return pool
}

func cleanupPDC(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	owned := `(SELECT id FROM source_accounts WHERE account_email LIKE '` + pdcPrefix + `%')`
	for _, s := range []string{
		`DELETE FROM normalized_events WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + owned + `)`,
		`DELETE FROM raw_source_items  WHERE source_account_id IN ` + owned,
		`DELETE FROM sync_runs         WHERE stats->>'itest' = '` + pdcMarker + `'`,
		`DELETE FROM sync_runs         WHERE source_account_id IN ` + owned,
		`DELETE FROM source_accounts   WHERE account_email LIKE '` + pdcPrefix + `%'`,
	} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
}

// pdcAccount inserts one google account with NO refresh token and NO scopes —
// production's shape under Pipedream, and the shape that makes a
// credential-gated selection return nothing.
func pdcAccount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, suffix string, inScope bool) google.Account {
	t.Helper()
	email := pdcPrefix + suffix + "@example.com"
	var id int64
	// auth_type='app_password' with an app password and NO refresh token and NO
	// scopes is production's actual shape (IK, "A google row can be dual-auth"):
	// the app password is the MAIL credential and says nothing about calendar.
	// The CHECK source_accounts_app_password_present requires it.
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts
		   (provider, account_email, auth_type, app_password_encrypted, scopes,
		    send_enabled, calendar_in_availability)
		 VALUES ('google', $1, 'app_password', pgp_sym_encrypt('app-password', 'itest-key'),
		         ARRAY[]::text[], false, $2)
		 RETURNING id`, email, inScope).Scan(&id); err != nil {
		t.Fatalf("insert account %s: %v", email, err)
	}
	return google.Account{ID: id, Email: email, CalendarInAvailability: inScope, AuthType: "app_password"}
}

// pdcEvent is a Google Calendar v3 event object, RELATIVE to now.
func pdcEvent(id string, hoursFromNow int) json.RawMessage {
	start := time.Now().UTC().Add(time.Duration(hoursFromNow) * time.Hour).Truncate(time.Hour)
	return json.RawMessage(fmt.Sprintf(
		`{"id":%q,"status":"confirmed","summary":"pdc %s","start":{"dateTime":%q},"end":{"dateTime":%q}}`,
		id, id, start.Format(time.RFC3339), start.Add(time.Hour).Format(time.RFC3339)))
}

// pdcWorkflow serves the endpoint from a per-calendar event map, echoing the
// window it was asked for — what a correct workflow does.
type pdcWorkflow struct {
	events map[string][]json.RawMessage
	// dropCalendars are calendar ids the workflow silently omits from its
	// response — what a disconnected Google account in Pipedream looks like.
	dropCalendars []string
	status        int
	asked         []string
	calls         int
}

func (wf *pdcWorkflow) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wf.calls++
		if wf.status != 0 && wf.status != http.StatusOK {
			http.Error(w, "workflow unavailable", wf.status)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			TimeMin   string   `json:"time_min"`
			TimeMax   string   `json:"time_max"`
			Calendars []string `json:"calendars"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		wf.asked = append(wf.asked, req.Calendars...)

		entries := make([]map[string]any, 0, len(req.Calendars))
		for _, cal := range req.Calendars {
			if slices.Contains(wf.dropCalendars, strings.ToLower(cal)) {
				continue
			}
			events := wf.events[strings.ToLower(cal)]
			if events == nil {
				events = []json.RawMessage{}
			}
			entries = append(entries, map[string]any{
				"calendar_id": cal, "status": "ok",
				"event_count": len(events), "events": events,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": 1, "time_min": req.TimeMin, "time_max": req.TimeMax, "calendars": entries,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func pdcClient(t *testing.T, srv *httptest.Server) google.PipedreamCalendarSource {
	t.Helper()
	client, err := google.NewPipedreamCalendarClient(srv.URL+"/p_itest", pdcToken, srv.Client())
	if err != nil {
		t.Fatalf("NewPipedreamCalendarClient: %v", err)
	}
	return client
}

// ---------------------------------------------------------------------------
// Criterion 16: THE POLLED SET AND THE DEMANDED SET ARE THE SAME SET, PROVEN ON
// REAL POSTGRES.
//
// This is the column-fed regression test the institutional rule requires. Both
// fixtures are auth_type='app_password' with NO refresh token and NO scopes, so
// the mutation the SPEC names — swapping the predicate for
// auth_type='app_password' — makes the out-of-scope account appear and this
// test go red; and a copy of ListCalendarCredentialedAccounts' predicate
// returns zero accounts and it goes red too.
// ---------------------------------------------------------------------------

func TestPipedreamCalendar_Integration_PolledSetEqualsTheDemandedSet(t *testing.T) {
	ctx := context.Background()
	pool := pdcPool(t, ctx)
	sink := google.NewPGSink(pool)

	inScope := pdcAccount(t, ctx, pool, "scope-in", true)
	outScope := pdcAccount(t, ctx, pool, "scope-out", false)

	// --- the poller's half -------------------------------------------------
	selected, err := google.ListAvailabilityScopeAccounts(ctx, pool, "")
	if err != nil {
		t.Fatalf("ListAvailabilityScopeAccounts: %v", err)
	}
	var emails []string
	for _, a := range selected {
		emails = append(emails, a.Email)
	}
	if !slices.Contains(emails, inScope.Email) {
		t.Errorf("the selection does not include %s, which has calendar_in_availability=true. Criterion 15: "+
			"the Pipedream path uses only Account.ID and Account.Email and reads no auth_type, no scopes and "+
			"no encrypted anything — this fixture deliberately holds none of those, exactly as production "+
			"does under Pipedream", inScope.Email)
	}
	if slices.Contains(emails, outScope.Email) {
		t.Errorf("the selection includes %s, which has calendar_in_availability=false. The operator has ONE "+
			"lever and it must move both sides together", outScope.Email)
	}

	// --- the demand's half -------------------------------------------------
	// Freshen every in-scope calendar this suite does not own (the pact), poll
	// only the in-scope fixture, and assert LoadBusy is satisfied. The stale
	// out-of-scope account must cause no refusal.
	freshenForeignPDC(t, ctx, pool)

	wf := &pdcWorkflow{events: map[string][]json.RawMessage{
		strings.ToLower(inScope.Email): {pdcEvent("in-scope-1", 30)},
	}}
	if _, err := google.RunPipedreamCalendar(ctx, pdcClient(t, wf.serve(t)), sink, selected, google.Config{}); err != nil {
		t.Fatalf("RunPipedreamCalendar over the selected accounts: %v", err)
	}
	if slices.Contains(wf.asked, outScope.Email) {
		t.Errorf("the workflow was asked for %s, which is out of availability scope", outScope.Email)
	}

	now := time.Now().UTC()
	_, err = availability.LoadBusy(ctx, pool, availability.Request{
		WindowStart: now.Add(24 * time.Hour), WindowEnd: now.Add(48 * time.Hour),
		Now: now, MaxSyncAge: time.Hour,
		HorizonPast: google.CalendarWindowPast, HorizonFuture: google.CalendarWindowFuture,
	})
	if err != nil {
		t.Fatalf("LoadBusy refused after the poller freshened every account it demands: %v\n"+
			"Criterion 16: the polled set and the demanded set must be EQUAL. A refusal here naming an "+
			"account the poller never asked for is the exact configuration the SPEC says must not exist",
			err)
	}

	// The control: make the in-scope account stale again and LoadBusy must
	// refuse, naming it. Without this the assertion above passes on a LoadBusy
	// that demands nothing.
	if _, err := pool.Exec(ctx,
		`UPDATE sync_runs SET finished_at = now() - interval '3 hours'
		  WHERE source_account_id = $1`, inScope.ID); err != nil {
		t.Fatalf("age the in-scope run: %v", err)
	}
	_, err = availability.LoadBusy(ctx, pool, availability.Request{
		WindowStart: now.Add(24 * time.Hour), WindowEnd: now.Add(48 * time.Hour),
		Now: now, MaxSyncAge: time.Hour,
		HorizonPast: google.CalendarWindowPast, HorizonFuture: google.CalendarWindowFuture,
	})
	if err == nil {
		t.Fatal("LoadBusy answered with the in-scope account's calendar three hours stale; the control " +
			"proves the demand is real, and without it the equality above is vacuous")
	}
	if !strings.Contains(err.Error(), inScope.Email) {
		t.Errorf("the refusal does not name %s: %v", inScope.Email, err)
	}
	if strings.Contains(err.Error(), outScope.Email) {
		t.Errorf("the refusal names %s, which is OUT of availability scope: %v. A stale out-of-scope "+
			"calendar must cause no refusal (criterion 16)", outScope.Email, err)
	}
}

// freshenForeignPDC is the pact: readiness scope is GLOBAL, so a leftover
// in-scope google account in a dirty compose db would refuse this suite's
// "must answer" cases for reasons that have nothing to do with it. The rows
// carry the itest marker and cleanup removes them.
func freshenForeignPDC(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO sync_runs (source_account_id, started_at, finished_at, status, stats)
		 SELECT id, now(), now(), 'ok', jsonb_build_object('phase','calendar','itest',$2::text)
		   FROM source_accounts
		  WHERE provider='google' AND calendar_in_availability AND account_email NOT LIKE $1`,
		pdcPrefix+"%", pdcMarker); err != nil {
		t.Fatalf("freshen foreign calendars: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Criterion 12: THE PIPEDREAM PATH WRITES NO CURSOR.
//
// The cursor blob is shared: it holds imap_folders, which the resident watch
// loop moves underneath any calendar pass. A whole-blob write rolls the IMAP
// position back, and skipped mail is a delivery confirmation that never lands
// (invariant 5). This transport has no sync token to save, so the correct
// number of cursor writes is zero — asserted on the COLUMN, byte for byte,
// because a fake sink can only prove a method was not called.
// ---------------------------------------------------------------------------

func TestPipedreamCalendar_Integration_LeavesSyncCursorByteIdentical(t *testing.T) {
	ctx := context.Background()
	pool := pdcPool(t, ctx)
	sink := google.NewPGSink(pool)

	acct := pdcAccount(t, ctx, pool, "cursor", true)
	const seeded = `{"gmail_internal_date_ms": 1784995200123, "calendar_sync_token": "CPjJ-DO-NOT-TOUCH", "imap_folders": {"INBOX": {"uid_validity": 42, "last_uid": 9137}}}`
	if _, err := pool.Exec(ctx,
		`UPDATE source_accounts SET sync_cursor = $2::jsonb WHERE id = $1`, acct.ID, seeded); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}
	var before string
	if err := pool.QueryRow(ctx,
		`SELECT sync_cursor::text FROM source_accounts WHERE id=$1`, acct.ID).Scan(&before); err != nil {
		t.Fatalf("read cursor before: %v", err)
	}

	wf := &pdcWorkflow{events: map[string][]json.RawMessage{
		strings.ToLower(acct.Email): {pdcEvent("cursor-1", 26), pdcEvent("cursor-2", 50)},
	}}
	if _, err := google.RunPipedreamCalendar(ctx, pdcClient(t, wf.serve(t)), sink,
		[]google.Account{acct}, google.Config{}); err != nil {
		t.Fatalf("RunPipedreamCalendar: %v", err)
	}

	// The pass must have DONE something, or this test is vacuously green: a
	// no-op ingest also leaves the cursor byte-identical.
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items WHERE source_account_id=$1 AND external_id LIKE 'calendar:%'`,
		acct.ID); got != 2 {
		t.Fatalf("raw calendar rows after the pass = %d, want 2; without real work the cursor assertion "+
			"below proves nothing", got)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM sync_runs WHERE source_account_id=$1 AND status='ok' AND stats->>'phase'='calendar'`,
		acct.ID); got != 1 {
		t.Fatalf("ok calendar runs after the pass = %d, want 1", got)
	}

	var after string
	if err := pool.QueryRow(ctx,
		`SELECT sync_cursor::text FROM source_accounts WHERE id=$1`, acct.ID).Scan(&after); err != nil {
		t.Fatalf("read cursor after: %v", err)
	}
	if after != before {
		t.Errorf("sync_cursor changed across a full Pipedream pass.\nbefore: %s\n after: %s\n"+
			"Criterion 12: this path calls neither SaveCursor nor SaveCursorField. The blob holds "+
			"imap_folders, which the resident watch loop moves underneath it — a rolled-back UID position "+
			"SKIPS mail, and a skipped mail is a delivery confirmation that never lands (invariant 5)",
			before, after)
	}
}

// ---------------------------------------------------------------------------
// Criteria 9 + 10 on real SQL: raw-first, then the snapshot as a REPLACEMENT.
//
// SupersedeAbsentCalendar's SQL reads the event's own start out of the raw JSON
// and bounds the sweep to the queried window. A fake sink cannot exercise any
// of that; this is the test that would catch a subtly wrong predicate silently
// cancelling real events.
// ---------------------------------------------------------------------------

func TestPipedreamCalendar_Integration_SnapshotSupersedesDeletedEventsAndKeepsHistory(t *testing.T) {
	ctx := context.Background()
	pool := pdcPool(t, ctx)
	sink := google.NewPGSink(pool)

	acct := pdcAccount(t, ctx, pool, "snapshot", true)
	kept, deleted := pdcEvent("kept", 26), pdcEvent("deleted", 50)

	wf := &pdcWorkflow{events: map[string][]json.RawMessage{
		strings.ToLower(acct.Email): {kept, deleted},
	}}
	srv := wf.serve(t)
	source := pdcClient(t, srv)

	// --- first poll: raw-first, then normalize -----------------------------
	if _, err := google.RunPipedreamCalendar(ctx, source, sink, []google.Account{acct}, google.Config{}); err != nil {
		t.Fatalf("first RunPipedreamCalendar: %v", err)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items WHERE source_account_id=$1 AND external_id LIKE 'calendar:%'
		   AND content_hash IS NOT NULL AND normalized_at IS NULL`, acct.ID); got != 2 {
		t.Fatalf("pending raw calendar rows after the poll = %d, want 2. Invariant 1: the ONLY write into "+
			"the funnel is the raw row — provider JSON plus content_hash — BEFORE anything normalizes", got)
	}
	if _, err := google.Normalize(ctx, sink, google.Config{}); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_events e JOIN raw_source_items r ON r.id=e.raw_source_item_id
		  WHERE r.source_account_id=$1 AND e.status='confirmed'`, acct.ID); got != 2 {
		t.Fatalf("confirmed normalized events = %d, want 2 — the transport-agnostic half is unchanged "+
			"(premise 1: whatever puts calendar:{id} raw rows in front of Normalize produces events)", got)
	}

	// --- an out-of-window event that must survive every poll ---------------
	old := json.RawMessage(`{"id":"ancient","status":"confirmed","start":{"dateTime":"2019-01-01T10:00:00Z"},"end":{"dateTime":"2019-01-01T11:00:00Z"}}`)
	if err := sink.InsertRaw(ctx, acct.ID, "calendar:ancient", old, "hash-pdc-ancient"); err != nil {
		t.Fatalf("seed ancient: %v", err)
	}

	// --- second poll: the event was deleted in Google ----------------------
	wf.events[strings.ToLower(acct.Email)] = []json.RawMessage{kept}
	if _, err := google.RunPipedreamCalendar(ctx, source, sink, []google.Account{acct}, google.Config{}); err != nil {
		t.Fatalf("second RunPipedreamCalendar: %v", err)
	}

	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items
		  WHERE source_account_id=$1 AND external_id='calendar:deleted' AND superseded_at IS NOT NULL`,
		acct.ID); got != 1 {
		t.Errorf("the deleted event was not superseded (%d). Criterion 10: every poll is a full snapshot, "+
			"so every poll is the bridge's reset case — an event deleted in Google is absent from the next "+
			"snapshot and must leave the busy set, or propose_slots refuses time that is actually free "+
			"forever", got)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_events e JOIN raw_source_items r ON r.id=e.raw_source_item_id
		  WHERE r.source_account_id=$1 AND r.external_id='calendar:deleted' AND e.status='cancelled'`,
		acct.ID); got != 1 {
		t.Errorf("the superseded event's normalized_events row was not cancelled (%d)", got)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items
		  WHERE source_account_id=$1 AND external_id='calendar:kept' AND superseded_at IS NULL`,
		acct.ID); got != 1 {
		t.Errorf("the event PRESENT in the snapshot was superseded (%d)", got)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items
		  WHERE source_account_id=$1 AND external_id='calendar:ancient' AND superseded_at IS NULL`,
		acct.ID); got != 1 {
		t.Errorf("a 2019 event OUTSIDE the polled window was superseded (%d). It was never asked for, so "+
			"its absence proves nothing; an account-wide sweep would rewrite history on every poll", got)
	}
}

// ---------------------------------------------------------------------------
// Criterion 13, the whole chain: a Pipedream outage never produces an ok run,
// and propose_slots — through executor.Execute, so the refusal is AUDITED —
// still refuses and names the account.
//
// Three failure shapes in sequence, because they arrive by different routes:
// a 503 (transport), an account missing from the response (attribution), and an
// item the normalizer cannot parse (per-entry integrity). None may leave an ok
// run behind.
// ---------------------------------------------------------------------------

func TestPipedreamCalendar_Integration_OutageKeepsProposeSlotsRefusingAndAudited(t *testing.T) {
	ctx := context.Background()
	pool := pdcPool(t, ctx)
	sink := google.NewPGSink(pool)

	acct := pdcAccount(t, ctx, pool, "outage", true)
	other := pdcAccount(t, ctx, pool, "outage-b", true)
	accounts := []google.Account{acct, other}
	freshenForeignPDC(t, ctx, pool)

	// (1) transport: 503.
	down := &pdcWorkflow{status: http.StatusServiceUnavailable}
	if _, err := google.RunPipedreamCalendar(ctx, pdcClient(t, down.serve(t)), sink, accounts, google.Config{}); err == nil {
		t.Error("a 503 was reported as a successful pass")
	}

	// (2) attribution: one account missing from an otherwise valid response.
	missing := &pdcWorkflow{
		events:        map[string][]json.RawMessage{strings.ToLower(other.Email): {pdcEvent("b-1", 30)}},
		dropCalendars: []string{strings.ToLower(acct.Email)},
	}
	// The pass's return value is not asserted: the OTHER account succeeded, and
	// criterion 14 fails the pass only if none did. The run rows below are the
	// contract.
	_, _ = google.RunPipedreamCalendar(ctx, pdcClient(t, missing.serve(t)), sink, accounts, google.Config{})

	// (3) per-entry integrity: an item the normalizer cannot parse.
	bad := &pdcWorkflow{events: map[string][]json.RawMessage{
		strings.ToLower(acct.Email):  {json.RawMessage(`{"id":"reshaped","status":"confirmed","summary":"no interval"}`)},
		strings.ToLower(other.Email): {pdcEvent("b-2", 31)},
	}}
	_, _ = google.RunPipedreamCalendar(ctx, pdcClient(t, bad.serve(t)), sink, accounts, google.Config{})

	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM sync_runs
		  WHERE source_account_id=$1 AND status='ok' AND stats->>'phase'='calendar'`, acct.ID); got != 0 {
		t.Fatalf("ok calendar runs for the broken account = %d, want 0. A Pipedream outage must NEVER "+
			"produce an ok run — that row is the ONLY thing standing between a dead poller and "+
			"propose_slots answering from a busy set nobody fetched", got)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM sync_runs
		  WHERE source_account_id=$1 AND status='error' AND stats->>'phase'='calendar'`, acct.ID); got != 3 {
		t.Errorf("error calendar runs for the broken account = %d, want 3 (one per failed pass): every "+
			"refusal path lands as status='error' with the reason", got)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items WHERE source_account_id=$1`, acct.ID); got != 0 {
		t.Errorf("the broken account has %d raw rows; every one of these refusals happens BEFORE a raw row "+
			"is written for that account (criteria 6 and 8)", got)
	}

	// --- and now the audited consumer --------------------------------------
	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	ex := executor.New(reg, checker, audit.NewPGStore(pool))

	var auditMin int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(max(id),0) FROM audit_events`).Scan(&auditMin); err != nil {
		t.Fatalf("audit watermark: %v", err)
	}
	// Registered BEFORE the call so an early Fatalf cannot leak audit rows into
	// the shared compose db (the mutual-cleanup pact).
	t.Cleanup(func() {
		bg := context.Background()
		if _, err := pool.Exec(bg,
			`DELETE FROM policy_decisions WHERE audit_event_id IN
			   (SELECT id FROM audit_events WHERE tool='propose_slots' AND id > $1)`, auditMin); err != nil {
			t.Errorf("cleanup policy_decisions: %v", err)
		}
		if _, err := pool.Exec(bg,
			`DELETE FROM audit_events WHERE tool='propose_slots' AND id > $1`, auditMin); err != nil {
			t.Errorf("cleanup audit_events: %v", err)
		}
	})
	_, execErr := ex.Execute(ctx, executor.Call{
		Tool: "propose_slots", Actor: "opsctl:itest-pdc",
		Args: []byte(`{"duration_minutes":30}`),
	})
	if execErr == nil {
		t.Fatal("propose_slots ANSWERED while an in-scope calendar has no successful sync. That is the " +
			"fail-open SWT-24 removed and this ticket must not reintroduce: the busy set is empty because " +
			"nobody could read the calendar, not because the week is free")
	}
	if !strings.Contains(execErr.Error(), acct.Email) {
		t.Errorf("the refusal does not name %s: %v — the operator's next action is reconnecting THAT "+
			"account in the Pipedream workflow", acct.Email, execErr)
	}
	var nre *availability.NotReadyError
	if !errors.As(execErr, &nre) {
		t.Errorf("the refusal is not *availability.NotReadyError (%T); criterion 13 keeps propose_slots' "+
			"SWT-24 behaviour verbatim, including its refusal text", execErr)
	}

	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM audit_events WHERE tool='propose_slots' AND id > $1 ORDER BY id DESC LIMIT 1`,
		auditMin).Scan(&status); err != nil {
		t.Fatalf("read the propose_slots audit row: %v", err)
	}
	if status != "error" {
		t.Errorf("audit_events.status = %q, want error. Invariant 3: the one audited consumer of this data "+
			"keeps its single door, and a refusal that leaves no audit row is a refusal nobody can see", status)
	}
}
