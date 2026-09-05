//go:build integration

package main

// Integration tests for the Pipedream calendar phase's wiring in the connector
// main (pipedream-calendar / docs/tickets/pipedream-calendar_SPEC.md,
// acceptance criteria 3, 13, 15 and 18). Build-tagged `integration` AND
// env-gated on DATABASE_URL. The Pipedream endpoint is ALWAYS a local httptest
// server — no real Pipedream, no credential, no network (criterion 21).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 ./cmd/connectors/google/
//
// WHY THESE ARE INTEGRATION TESTS. The line criterion 3 draws — "a
// configuration error writes NOTHING AT ALL, a transport error writes
// per-account error runs" — is a statement about rows in sync_runs. A fake sink
// can be told to record whatever the code asks it to; only Postgres can show
// that the table is untouched. Same for criterion 15: "zero in-scope accounts
// writes nothing" is a count.
//
// CROSS-SUITE DISCIPLINE (premise 13, the tension this ticket inherits). The
// SWT-24 phase suite seeds calendar_in_availability=FALSE accounts to stay out
// of availability scope. This ticket's SELECTION IS THAT SCOPE, so these
// fixtures must be TRUE — which means that while they exist, every
// propose_slots assertion in the repo can see them. They are therefore cleaned
// before AND after, in FK order, and `make integration` runs -p 1.
//
// GREENFIELD NOTE: runPipedreamCalendarIngest does not exist yet, so this file
// compile-FAILs under -tags integration — the expected red. Imposed contract
// (cmd/connectors/google/calendarsource.go; the SPEC names the function in
// "Files likely to touch" — "runPipedreamCalendarIngest (selection, client
// construction, printing)" — the signature is imposed here, shaped after its
// sibling runCalendarIngest minus the injected factory, because the client is
// built from the ENVIRONMENT and the construction failure is the thing under
// test):
//
//	func runPipedreamCalendarIngest(ctx context.Context, pool *pgxpool.Pool,
//	    sink *google.PGSink, cfg google.Config) (google.Stats, error)

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	pdPrefix = "itest-pdcal-"
	// 43 characters, distinctive, and never a real credential.
	pdToken = "pdtok-ITEST-0123456789abcdefghijklmnopqrstu"
)

func pdPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use the compose db on :5433. " +
			"Criterion 21 says it in the sharpest form: fabricated calendar data must never reach production")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	cleanupPD(t, ctx, pool)
	t.Cleanup(func() { cleanupPD(t, context.Background(), pool) })
	return pool
}

func cleanupPD(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	owned := `(SELECT id FROM source_accounts WHERE account_email LIKE '` + pdPrefix + `%')`
	for _, s := range []string{
		`DELETE FROM normalized_events WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + owned + `)`,
		`DELETE FROM raw_source_items  WHERE source_account_id IN ` + owned,
		`DELETE FROM sync_runs         WHERE source_account_id IN ` + owned,
		`DELETE FROM source_accounts   WHERE account_email LIKE '` + pdPrefix + `%'`,
	} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
}

// pdAccount inserts one in-scope google account. Note what it does NOT carry:
// no refresh_token_encrypted and no scopes. Criterion 15 — under Pipedream the
// credential lives at Pipedream and our rows keep neither, so a selection that
// is credential-gated (ListCalendarCredentialedAccounts) would return zero
// accounts here and every assertion below would go red.
func pdAccount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, suffix string, inScope bool) (int64, string) {
	t.Helper()
	email := pdPrefix + suffix + "@example.com"
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
	return id, email
}

func pdRuns(t *testing.T, ctx context.Context, pool *pgxpool.Pool, accountID int64, status string) int {
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

func pdCountAll(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sync_runs`).Scan(&n); err != nil {
		t.Fatalf("count sync_runs: %v", err)
	}
	return n
}

// pdEndpoint serves the workflow. handler decides the response; the request is
// recorded so the "no poll was attempted" assertions have something to read.
func pdEndpoint(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// echoOK answers every request with a verified, empty-for-every-calendar
// snapshot echoing the window it was given. Empty is a legitimate ok
// (criterion 11) and keeps this file about the WIRING, not the payload.
func echoOK(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TimeMin   string   `json:"time_min"`
		TimeMax   string   `json:"time_max"`
		Calendars []string `json:"calendars"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	entries := make([]map[string]any, 0, len(req.Calendars))
	for _, c := range req.Calendars {
		entries = append(entries, map[string]any{
			"calendar_id": c, "status": "ok", "event_count": 0, "events": []any{},
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"schema_version": 1, "time_min": req.TimeMin, "time_max": req.TimeMax, "calendars": entries,
	})
}

// ---------------------------------------------------------------------------
// Criterion 3, side one: A CONFIGURATION ERROR WRITES NO sync_runs ROW.
//
// The distinction the SPEC draws is "did we attempt a poll". An absent run and
// an error run both keep propose_slots refusing after an hour, but an error row
// says WHY — and a missing URL is not a calendar fact. Writing three error runs
// because a secret was not mounted would put a deployment mistake in the
// calendar's health record.
// ---------------------------------------------------------------------------

func TestRunPipedreamCalendarIngest_Integration_MisconfigurationWritesNoRun(t *testing.T) {
	ctx := context.Background()
	pool := pdPool(t, ctx)
	sink := google.NewPGSink(pool)

	aID, _ := pdAccount(t, ctx, pool, "cfg-a", true)
	bID, _ := pdAccount(t, ctx, pool, "cfg-b", true)

	cases := map[string]struct{ url, token, tokenFile string }{
		"no url at all":       {"", pdToken, ""},
		"url is not http":     {"ftp://eo.pipedream.net/p_abc", pdToken, ""},
		"url has no host":     {"http://", pdToken, ""},
		"no token at all":     {"https://eo.pipedream.net/p_abc", "", ""},
		"token far too short": {"https://eo.pipedream.net/p_abc", "short", ""},
		"token file missing":  {"https://eo.pipedream.net/p_abc", "", "/nonexistent/itest/pipedream.token"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			before := pdCountAll(t, ctx, pool)
			t.Setenv("PIPEDREAM_CALENDAR_URL", tc.url)
			t.Setenv("PIPEDREAM_CALENDAR_TOKEN", tc.token)
			t.Setenv("PIPEDREAM_CALENDAR_TOKEN_FILE", tc.tokenFile)

			_, err := runPipedreamCalendarIngest(ctx, pool, sink, google.Config{})
			if err == nil {
				t.Fatalf("runPipedreamCalendarIngest succeeded with %s. A misconfigured deployment must "+
					"fail at startup rather than mid-ingest (criterion 3)", name)
			}
			if strings.Contains(err.Error(), pdToken) {
				t.Errorf("the configuration error leaks the token: %v", err)
			}

			if after := pdCountAll(t, ctx, pool); after != before {
				t.Errorf("sync_runs went from %d to %d on a CONFIGURATION error (%s). Criterion 3: a "+
					"configuration error writes nothing at all — the run row must be reachable only after "+
					"we have actually attempted a poll, or a missing secret is recorded as a calendar fact",
					before, after, name)
			}
			if got := pdRuns(t, ctx, pool, aID, "error") + pdRuns(t, ctx, pool, bID, "error"); got != 0 {
				t.Errorf("per-account error runs after a configuration failure = %d, want 0", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Criterion 3, side two, and criterion 13: A TRANSPORT ERROR — we did attempt a
// poll — WRITES A per-account error run, and never an ok run.
// ---------------------------------------------------------------------------

func TestRunPipedreamCalendarIngest_Integration_TransportFailureWritesPerAccountErrorRuns(t *testing.T) {
	ctx := context.Background()
	pool := pdPool(t, ctx)
	sink := google.NewPGSink(pool)

	aID, aEmail := pdAccount(t, ctx, pool, "out-a", true)
	bID, bEmail := pdAccount(t, ctx, pool, "out-b", true)

	srv, calls := pdEndpoint(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "workflow is rate limited", http.StatusServiceUnavailable)
	})
	t.Setenv("PIPEDREAM_CALENDAR_URL", srv.URL+"/p_itest")
	t.Setenv("PIPEDREAM_CALENDAR_TOKEN", pdToken)
	t.Setenv("PIPEDREAM_CALENDAR_TOKEN_FILE", "")

	if _, err := runPipedreamCalendarIngest(ctx, pool, sink, google.Config{}); err == nil {
		t.Fatal("a 503 from the workflow was reported as a successful pass")
	}
	if *calls == 0 {
		t.Fatal("no HTTP request reached the endpoint; this case is supposed to exercise a poll that was ATTEMPTED")
	}
	for _, acct := range []struct {
		id    int64
		email string
	}{{aID, aEmail}, {bID, bEmail}} {
		if got := pdRuns(t, ctx, pool, acct.id, "error"); got != 1 {
			t.Errorf("error calendar runs for %s = %d, want 1. We attempted a poll and it failed: that is a "+
				"calendar fact, and the row is what tells an operator WHY availability went quiet",
				acct.email, got)
		}
		if got := pdRuns(t, ctx, pool, acct.id, "ok"); got != 0 {
			t.Errorf("ok calendar runs for %s = %d, want 0. A Pipedream outage must NEVER produce an ok run "+
				"(criterion 13) — an ok row is a freshness signal, and propose_slots would answer from a "+
				"busy set nobody fetched", acct.email, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Criteria 15 + 18: the selection is the availability scope, it reads no
// credential, and the path needs neither OPS_TOKEN_KEY nor
// GOOGLE_CLIENT_SECRET_FILE — a strictly smaller secret surface than OAuth.
// ---------------------------------------------------------------------------

func TestRunPipedreamCalendarIngest_Integration_PollsTheScopeWithNoDecryptionSecrets(t *testing.T) {
	ctx := context.Background()
	pool := pdPool(t, ctx)
	sink := google.NewPGSink(pool)

	inID, inEmail := pdAccount(t, ctx, pool, "scope-in", true)
	outID, outEmail := pdAccount(t, ctx, pool, "scope-out", false)

	var asked []string
	srv, _ := pdEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Calendars []string `json:"calendars"`
		}
		_ = json.Unmarshal(body, &req)
		asked = append(asked, req.Calendars...)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		echoOK(w, r)
	})

	t.Setenv("PIPEDREAM_CALENDAR_URL", srv.URL+"/p_itest")
	t.Setenv("PIPEDREAM_CALENDAR_TOKEN", pdToken)
	t.Setenv("PIPEDREAM_CALENDAR_TOKEN_FILE", "")
	// The whole point of criterion 18: this path decrypts nothing.
	t.Setenv("OPS_TOKEN_KEY", "")
	t.Setenv("GOOGLE_CLIENT_SECRET_FILE", "/nonexistent/itest/client_secret.json")

	if _, err := runPipedreamCalendarIngest(ctx, pool, sink, google.Config{}); err != nil {
		t.Fatalf("runPipedreamCalendarIngest with no OPS_TOKEN_KEY and no client secret file: %v — "+
			"criterion 18 says it needs NEITHER; a path that decrypts nothing must not read a decryption key", err)
	}

	if !slices.Contains(asked, inEmail) {
		t.Errorf("the poll asked for %v and not for the in-scope account %s. Criterion 15/16: the polled "+
			"set IS the availability scope (provider='google' AND calendar_in_availability), so there is no "+
			"configuration in which propose_slots requires a calendar the poller never asks for", asked, inEmail)
	}
	if slices.Contains(asked, outEmail) {
		t.Errorf("the poll asked for %s, which has calendar_in_availability=false. Polling every "+
			"provider='google' row was rejected precisely because it produces perpetual error runs for "+
			"accounts nobody wants a calendar for", outEmail)
	}
	if got := pdRuns(t, ctx, pool, inID, "ok"); got != 1 {
		t.Errorf("ok calendar runs for the in-scope account = %d, want 1", got)
	}
	if got := pdRuns(t, ctx, pool, outID, "ok") + pdRuns(t, ctx, pool, outID, "error"); got != 0 {
		t.Errorf("the out-of-scope account got %d calendar runs, want 0", got)
	}
}

// Criterion 15: zero in-scope accounts prints a line and exits 0, writing
// nothing — a zero-work pass must never look like a working one, and must never
// look like a failure either.
func TestRunPipedreamCalendarIngest_Integration_ZeroInScopeAccountsIsNotAnError(t *testing.T) {
	ctx := context.Background()
	pool := pdPool(t, ctx)
	sink := google.NewPGSink(pool)

	srv, calls := pdEndpoint(t, echoOK)
	t.Setenv("PIPEDREAM_CALENDAR_URL", srv.URL+"/p_itest")
	t.Setenv("PIPEDREAM_CALENDAR_TOKEN", pdToken)
	t.Setenv("PIPEDREAM_CALENDAR_TOKEN_FILE", "")

	before := pdCountAll(t, ctx, pool)
	// Narrowed to an address that does not exist: deterministic zero-account
	// selection on a shared compose db. --account is for debugging only.
	cfg := google.Config{AccountEmail: pdPrefix + "nobody@example.com"}
	if _, err := runPipedreamCalendarIngest(ctx, pool, sink, cfg); err != nil {
		t.Fatalf("runPipedreamCalendarIngest errored with zero in-scope accounts: %v", err)
	}
	if after := pdCountAll(t, ctx, pool); after != before {
		t.Errorf("sync_runs went from %d to %d on a zero-account pass; a run row written for no work is a "+
			"freshness signal for a calendar nobody read (criterion 15)", before, after)
	}
	if *calls != 0 {
		t.Errorf("the endpoint was polled %d times with zero in-scope accounts; the request would carry an "+
			"empty calendars list and burn a Pipedream invocation to learn nothing", *calls)
	}
}
