//go:build integration

package main

// Integration test for the calendar phase's per-account loop (SWT-24 /
// docs/tickets/calendar-availability_SPEC.md, acceptance criteria 17 and 21).
// Build-tagged `integration` AND env-gated on DATABASE_URL. The Calendar API is
// a local httptest server and the failing account's client comes from a factory
// that returns an error — NO live Google credential, exactly as criterion 21
// requires ("proven against the existing fake server and a failing token
// source").
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 ./cmd/connectors/google/
//
// WHY CRITERION 21 MATTERS AT ALL: it is the seam between the two halves of
// this ticket. If a token refresh fails and the phase records nothing, the
// account's last successful calendar sync stays where it was and Part A keeps
// refusing — correct. If instead the phase swallowed the failure and wrote an
// ok run, propose_slots would answer from a busy set nobody fetched. The error
// row is not bookkeeping; it is the thing that keeps the refusal honest.
//
// GREENFIELD NOTE (historical): runCalendarIngest did not exist when this
// file was written (it compile-FAILed under `-tags integration`, the expected
// red) until the implementation landed. Imposed surface
// (package-internal, cmd/connectors/google/calendarsource.go), shaped after its
// sibling runIMAPIngest (mailsource.go:65-134) with the client construction
// injected so the failure path is reachable without a Google credential:
//
//	type calendarClientFactory func(ctx context.Context, acct google.Account) (*google.CalendarClient, error)
//
//	// runCalendarIngest ingests every calendar-credentialed account: one
//	// per-account advisory lock (sink.LockAccount), one failing account
//	// recorded without aborting the others, the pass failing only if NONE
//	// succeeded, and a printed line when there are zero credentialed accounts
//	// so a zero-work pass never looks like a working one.
//	func runCalendarIngest(ctx context.Context, pool *pgxpool.Pool, sink *google.PGSink,
//	    newClient calendarClientFactory, cfg google.Config) (google.Stats, error)
//
// Cross-suite discipline: accounts are 'itest-swt24-phase-%' with
// calendar_in_availability=FALSE (this suite is about ingest; leaving unsynced
// accounts in availability scope would break other suites' propose_slots
// assertions for unrelated reasons). Cleanup runs before and after, in FK order.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	phasePrefix = "itest-swt24-phase-"
	phaseKey    = "itest-swt24-phase-key"
)

func phasePool(t *testing.T, ctx context.Context) *pgxpool.Pool {
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
	cleanupPhase(t, ctx, pool)
	t.Cleanup(func() { cleanupPhase(t, context.Background(), pool) })
	return pool
}

func cleanupPhase(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	owned := `(SELECT id FROM source_accounts WHERE account_email LIKE '` + phasePrefix + `%')`
	for _, s := range []string{
		`DELETE FROM normalized_events WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + owned + `)`,
		`DELETE FROM raw_source_items  WHERE source_account_id IN ` + owned,
		`DELETE FROM sync_runs         WHERE source_account_id IN ` + owned,
		`DELETE FROM source_accounts   WHERE account_email LIKE '` + phasePrefix + `%'`,
	} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
}

func phaseAccount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, suffix string) (int64, string) {
	t.Helper()
	email := phasePrefix + suffix + "@example.com"
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts
		   (provider, account_email, auth_type, refresh_token_encrypted, app_password_encrypted,
		    scopes, send_enabled, calendar_in_availability)
		 VALUES ('google', $1, 'app_password', pgp_sym_encrypt('refresh-'||$1, $2),
		         pgp_sym_encrypt('app-password', $2),
		         ARRAY['https://www.googleapis.com/auth/calendar.readonly'], false, false)
		 RETURNING id`, email, phaseKey).Scan(&id); err != nil {
		t.Fatalf("insert account %s: %v", email, err)
	}
	return id, email
}

// fakeCalendarAPI is a minimal stand-in for the events endpoint. The rich fake
// (internal/connector/google/fake_google_test.go) is in that package's test
// binary and cannot be imported here; all this test needs of a WORKING account
// is a successful, empty page with a sync token.
func fakeCalendarAPI(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/events") {
			http.Error(w, "no route: "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items":         []json.RawMessage{},
			"nextSyncToken": "SYNCTOK-PHASE",
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runsFor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, accountID int64, status string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM sync_runs
		  WHERE source_account_id=$1 AND status=$2 AND stats->>'phase'='calendar'`,
		accountID, status).Scan(&n); err != nil {
		t.Fatalf("count %s calendar runs: %v", status, err)
	}
	return n
}

// ---------------------------------------------------------------------------
// Criterion 21: a failing token source leaves an error run and no ok run, and
// does not stop the other accounts.
// ---------------------------------------------------------------------------

func TestRunCalendarIngest_Integration_FailingTokenLeavesAnErrorRun(t *testing.T) {
	ctx := context.Background()
	pool := phasePool(t, ctx)
	sink := google.NewPGSink(pool)
	srv := fakeCalendarAPI(t)

	badID, badEmail := phaseAccount(t, ctx, pool, "revoked")
	goodID, goodEmail := phaseAccount(t, ctx, pool, "working")

	factory := func(_ context.Context, acct google.Account) (*google.CalendarClient, error) {
		if acct.Email == badEmail {
			// What a revoked consent looks like: the refresh exchange fails
			// before any Calendar request is made.
			return nil, fmt.Errorf("oauth2: cannot fetch token: 400 Bad Request (invalid_grant)")
		}
		return google.NewCalendarClient(http.DefaultClient, srv.URL), nil
	}

	// One account failed and one succeeded: the pass does NOT fail, because
	// one bad mailbox must not blind the others (mailsource.go's rule).
	if _, err := runCalendarIngest(ctx, pool, sink, factory, google.Config{}); err != nil {
		t.Fatalf("runCalendarIngest failed the whole pass because ONE account's token is bad: %v", err)
	}

	if got := runsFor(t, ctx, pool, badID, "error"); got != 1 {
		t.Errorf("error calendar sync_runs for %s = %d, want 1. A token failure that records nothing is "+
			"invisible: the account simply looks like it was never polled, and nothing distinguishes a "+
			"revoked consent from a CronJob that has not run yet", badEmail, got)
	}
	if got := runsFor(t, ctx, pool, badID, "ok"); got != 0 {
		t.Errorf("ok calendar sync_runs for %s = %d, want 0 — an ok row here is exactly what would make "+
			"propose_slots answer from a calendar nobody could read (criterion 21)", badEmail, got)
	}
	if got := runsFor(t, ctx, pool, goodID, "ok"); got != 1 {
		t.Errorf("ok calendar sync_runs for %s = %d, want 1; the working account must still be ingested",
			goodEmail, got)
	}

	// Every account failing IS a failed pass: silence there would let a
	// completely broken consent exit 0 in a CronJob log.
	allBad := func(_ context.Context, _ google.Account) (*google.CalendarClient, error) {
		return nil, fmt.Errorf("oauth2: cannot fetch token: 400 Bad Request (invalid_grant)")
	}
	if _, err := runCalendarIngest(ctx, pool, sink, allBad, google.Config{}); err == nil {
		t.Errorf("runCalendarIngest returned nil with EVERY account failing; the pass fails only if none " +
			"succeeded, and here none did")
	}
}

// ---------------------------------------------------------------------------
// Criterion 17 (the "zero credentialed accounts" half): a pass that finds
// nothing to do is a success, and it must not invent a sync_runs row. This is
// the state production is in until Salvador runs the consent flow.
// ---------------------------------------------------------------------------

func TestRunCalendarIngest_Integration_NoCredentialedAccountsIsNotAnError(t *testing.T) {
	ctx := context.Background()
	pool := phasePool(t, ctx)
	sink := google.NewPGSink(pool)

	var before int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sync_runs`).Scan(&before); err != nil {
		t.Fatalf("count sync_runs: %v", err)
	}

	factory := func(_ context.Context, acct google.Account) (*google.CalendarClient, error) {
		t.Fatalf("a client was built for %s; no account should have been selected", acct.Email)
		return nil, nil
	}
	// Narrowed to an address that does not exist: deterministic zero-account
	// selection on a shared compose db.
	cfg := google.Config{AccountEmail: phasePrefix + "nobody@example.com"}
	if _, err := runCalendarIngest(ctx, pool, sink, factory, cfg); err != nil {
		t.Fatalf("runCalendarIngest errored with zero credentialed accounts: %v — "+
			"a deployment with no consent yet is the CURRENT state of production, not a failure", err)
	}

	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sync_runs`).Scan(&after); err != nil {
		t.Fatalf("count sync_runs: %v", err)
	}
	if after != before {
		t.Errorf("sync_runs went from %d to %d on a zero-account pass; a run row written for no work is a "+
			"freshness signal for a calendar nobody read", before, after)
	}
}
