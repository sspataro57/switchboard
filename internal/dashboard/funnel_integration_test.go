//go:build integration

package dashboard_test

// Integration tests for SWT-29 (docs/tickets/funnel-view_SPEC.md): the /funnel
// ingestion page, its four sections, its independent degradation, and the
// /sources deletion of criterion 22.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run Funnel ./internal/dashboard/
//
// Build-tagged `integration` AND env-gated on DATABASE_URL, with dashGuard's
// FATAL on 192.168.50.49 — this suite deletes rows. The REAL dashboard.Server
// runs under httptest with dev-login (OIDC_ISSUER unset), reusing
// dashGuard / dashPool / newDashServer / get / snippet from
// dashboard_integration_test.go (same package, same tag). No LLM, no live OIDC,
// no MQTT, and a page load must cost zero GPU seconds: the classify section
// reads STORED verdicts from ai_extractions and never reaches
// OPS_LOCAL_PROVIDER_URL.
//
// CROSS-POLLUTION PACT (IK). source_accounts, sync_runs, raw_source_items,
// normalized_messages, capture_decisions, ai_runs and ai_extractions are shared
// with 19 other suites under `-p 1`, and this page is GLOBAL by construction —
// it renders every account and every day. So every assertion here is a
// CONTAINMENT assertion about this suite's own seeded values; there is no
// "the page shows 3 accounts" anywhere. The numeric folds are pinned per-fixture
// in internal/capture/attribution_integration_test.go and
// internal/classify/summary_integration_test.go, where the population can be
// isolated.
//
// ANTI-DATE-ROT: every timestamp is now() or now() - interval '...'. The only
// literal dates in the file are computed from time.Now() at assertion time.
//
// GREENFIELD NOTE — EXPECTED RED. GET /funnel, templates/funnel.html,
// classify.Summarize, capture.AttributionTrend, availability.CalendarSyncStates
// and tools.MaxCalendarSyncAge do not exist. This file compile-FAILS the
// integration build of internal/dashboard; once it compiles, /funnel 404s and
// the criterion-22 assertions on /sources stay red until the deletion lands.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	funProvider = "itest-funnel-src"
	funAcctA    = "itest-funnel-a@local"
	funAcctB    = "itest-funnel-b@local"
	funAcctC    = "itest-funnel-c@local"
	funAcctCal  = "itest-funnel-cal@local"
	funProject  = "itest-funnel-proj"
	funMarker   = "itest-funnel"
	// The schema that shadows ONE table so exactly one section's query fails.
	funShadowSchema = "itest_funnel_shadow"
)

type funSuite struct {
	pool  *pgxpool.Pool
	accts map[string]int64
}

func newFunSuite(t *testing.T, ctx context.Context) *funSuite {
	t.Helper()
	dashGuard(t)
	pool := dashPool(t, ctx)
	t.Cleanup(pool.Close)
	s := &funSuite{pool: pool, accts: map[string]int64{}}
	s.cleanup(t, ctx)
	t.Cleanup(func() { s.cleanup(t, context.Background()) })
	return s
}

func (s *funSuite) cleanup(t *testing.T, ctx context.Context) {
	t.Helper()
	const owned = `(SELECT id FROM source_accounts WHERE account_email LIKE 'itest-funnel-%')`
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN ` + owned + `)`
	for _, q := range []string{
		// The propose_slots agreement test calls the executor, which audits.
		// Removed by actor so nothing foreign is touched.
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE actor = 'opsctl:itest-funnel')`,
		`DELETE FROM audit_events WHERE actor = 'opsctl:itest-funnel'`,
		// capture_decisions.message_id is ON DELETE CASCADE; deleted explicitly
		// so a failure here reads as a cleanup failure rather than as a
		// mysterious FK error two statements later.
		`DELETE FROM capture_decisions WHERE message_id IN
		   (SELECT id FROM normalized_messages WHERE raw_source_item_id IN ` + raws + `)`,
		`DELETE FROM ai_extractions WHERE ai_run_id IN
		   (SELECT id FROM ai_runs WHERE input->>'itest' = '` + funMarker + `')`,
		`DELETE FROM ai_runs WHERE input->>'itest' = '` + funMarker + `'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'itest-funnel:%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + owned,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + owned,
		`DELETE FROM source_accounts WHERE account_email LIKE 'itest-funnel-%'`,
		`DELETE FROM projects WHERE slug = '` + funProject + `'`,
		`DROP SCHEMA IF EXISTS ` + funShadowSchema + ` CASCADE`,
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func (s *funSuite) account(t *testing.T, ctx context.Context, provider, email string, inAvailability bool) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, send_enabled, calendar_in_availability)
		 VALUES ($1,$2,false,$3) RETURNING id`, provider, email, inAvailability).Scan(&id); err != nil {
		t.Fatalf("seed account %s: %v", email, err)
	}
	s.accts[email] = id
	return id
}

// syncRun writes one run. `phase` == "" inserts the DEFAULT '{}' stats, which is
// what jira and upworkcrm really do — sync_runs has no phase column, the phase
// is a key in stats, and those two connectors write none. The (none) bucket is a
// real, expected shape and the fixtures must carry it.
func (s *funSuite) syncRun(t *testing.T, ctx context.Context, accountID int64, phase, status string, ago time.Duration) {
	t.Helper()
	stats := `'{}'::jsonb`
	args := []any{accountID, ago.String(), status}
	if phase != "" {
		stats = `jsonb_build_object('phase', $4::text)`
		args = append(args, phase)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO sync_runs (source_account_id, started_at, finished_at, status, stats)
		 VALUES ($1, now() - $2::interval, now() - $2::interval, $3, `+stats+`)`, args...); err != nil {
		t.Fatalf("seed sync_run(%s,%s): %v", phase, status, err)
	}
}

func (s *funSuite) rawItem(t *testing.T, ctx context.Context, accountID int64, label string, daysAgo int) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, ingested_at, normalized_at)
		 VALUES ($1,$2,'{}'::jsonb,$3, now() - make_interval(days => $4), now()) RETURNING id`,
		accountID, "itest-funnel-"+label, "itest-funnel-h-"+label, daysAgo).Scan(&id); err != nil {
		t.Fatalf("seed raw item %s: %v", label, err)
	}
	return id
}

// message uses the PIPELINE clock for created_at and a deliberately ancient
// sent_at. Criterion 9: sent_at is the provider's clock and answers a different
// question; a section that bucketed by it would put every row below on one day
// 400 days ago and none on today.
func (s *funSuite) message(t *testing.T, ctx context.Context, accountID int64, label, direction string, daysAgo int) int64 {
	t.Helper()
	rawID := s.rawItem(t, ctx, accountID, label, daysAgo)
	var threadID int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,$2,'[]') RETURNING id`,
		"itest-funnel:"+label, "itest-funnel "+label).Scan(&threadID); err != nil {
		t.Fatalf("seed thread %s: %v", label, err)
	}
	var id int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, created_at,
		    body_text, subject, sender, channel)
		 VALUES ($1,$2,$3,$4, now() - interval '400 days', now() - make_interval(days => $5),
		         'itest body','itest-funnel '||$6,'itest@funnel.example','gmail') RETURNING id`,
		rawID, threadID, direction, "itest-funnel-"+label, daysAgo, label).Scan(&id); err != nil {
		t.Fatalf("seed message %s: %v", label, err)
	}
	return id
}

func (s *funSuite) project(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality, ai_classify)
		 VALUES ($1,$1,'itest-funnel','manual','dashboard','/tmp/itest','any',false) RETURNING id`,
		funProject).Scan(&id); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return id
}

func (s *funSuite) decision(t *testing.T, ctx context.Context, messageID, projectID int64, action string) {
	t.Helper()
	var project any
	if action != "unmatched" {
		project = projectID
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
		 VALUES ($1,'shadow',$2,$3,'itest-funnel')`, messageID, action, project); err != nil {
		t.Fatalf("seed decision %s: %v", action, err)
	}
}

func (s *funSuite) verdict(t *testing.T, ctx context.Context, workerType, fields string) {
	t.Helper()
	var runID int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO ai_runs (worker_type, provider, model, input, status)
		 VALUES ($1,'ollama','qwen3:8b',$2::jsonb,'ok') RETURNING id`,
		workerType, `{"itest":"`+funMarker+`"}`).Scan(&runID); err != nil {
		t.Fatalf("seed ai_run %s: %v", workerType, err)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields) VALUES ($1,NULL,$2::jsonb)`,
		runID, fields); err != nil {
		t.Fatalf("seed ai_extraction: %v", err)
	}
}

func (s *funSuite) skippedRun(t *testing.T, ctx context.Context, workerType, reason string) {
	t.Helper()
	input := fmt.Sprintf(`{"itest":%q,"avail_reasons":{%q:2},"skipped_count":2}`, funMarker, reason)
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO ai_runs (worker_type, provider, model, input, status)
		 VALUES ($1,'ollama','qwen3:8b',$2::jsonb,'skipped')`, workerType, input); err != nil {
		t.Fatalf("seed skipped run %s: %v", workerType, err)
	}
}

// ---- page helpers -------------------------------------------------------------

var funRowRe = regexp.MustCompile(`(?is)<tr[^>]*>.*?</tr>`)

// funRow returns the first table row containing every needle. Asserting on the
// ROW rather than the page is what makes "this account is stale" different from
// "the word stale appears somewhere on a page with four sections".
func funRow(t *testing.T, body string, needles ...string) string {
	t.Helper()
	for _, row := range funRowRe.FindAllString(body, -1) {
		ok := true
		for _, n := range needles {
			if !strings.Contains(row, n) {
				ok = false
				break
			}
		}
		if ok {
			return row
		}
	}
	t.Fatalf("no <tr> on the page contains all of %v.\n%s", needles, snippet(body))
	return ""
}

func funHasRow(body string, needles ...string) bool {
	for _, row := range funRowRe.FindAllString(body, -1) {
		ok := true
		for _, n := range needles {
			if !strings.Contains(row, n) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func funDay(offset int) string { return time.Now().AddDate(0, 0, offset).Format("2006-01-02") }

// ---- criteria 1 + 23: the page renders, in order ------------------------------

func TestFunnel_Integration_RendersFourSectionsInOrder(t *testing.T) {
	ctx := context.Background()
	s := newFunSuite(t, ctx)
	ts, client := newDashServer(t, ctx, s.pool)
	defer ts.Close()

	code, body := get(t, client, ts.URL+"/funnel")
	if code != http.StatusOK {
		t.Fatalf("GET /funnel = %d, want 200\n%s", code, snippet(body))
	}

	// Criterion 23: health -> intake -> capture -> classify, classify LAST.
	// Q2-a's accepted cost is that the classify block dominates the page, and
	// ordering is the ONLY mitigation taken.
	lower := strings.ToLower(body)
	prev := -1
	for _, keyword := range []string{"health", "intake", "capture", "classif"} {
		at := strings.Index(lower, keyword)
		if at < 0 {
			t.Fatalf("the page never mentions %q; criterion 21 wants all four section headings\n%s",
				keyword, snippet(body))
		}
		if at <= prev {
			t.Fatalf("section %q appears at %d, before the previous section (%d). Criterion 23 pins "+
				"health -> intake -> capture -> classify", keyword, at, prev)
		}
		prev = at
	}
}

// ---- criterion 3: last SUCCESSFUL run per (account, phase) --------------------

// The mutation this test exists for (Verification step 3): drop
// `AND r.status='ok'` from the health query and it must go red. Account B has
// ONLY failing runs, so under the /sources rule — newest run of ANY phase and
// ANY status — it would render as recently synced. It has never synced.
func TestFunnel_Integration_ConnectorHealthIsLastSuccessfulPerPhase(t *testing.T) {
	ctx := context.Background()
	s := newFunSuite(t, ctx)

	a := s.account(t, ctx, funProvider, funAcctA, false)
	s.syncRun(t, ctx, a, "imap", "ok", 10*time.Minute)
	// NEWER, and failed. The last SUCCESSFUL run is still the 10-minute-old one.
	s.syncRun(t, ctx, a, "imap", "error", time.Minute)

	b := s.account(t, ctx, funProvider, funAcctB, false)
	s.syncRun(t, ctx, b, "imap", "error", 2*time.Minute)

	// No runs at all: appears ONCE, under (none), showing never.
	s.account(t, ctx, funProvider, funAcctC, false)

	// A phase-less pair, the jira/upworkcrm shape: stats '{}' with no phase key,
	// and TWO rows for one invocation (ingest + normalize). Both group under
	// (none) and the run count is legitimately double the CronJob ticks.
	s.syncRun(t, ctx, a, "", "ok", 12*time.Minute)
	s.syncRun(t, ctx, a, "", "ok", 12*time.Minute)

	ts, client := newDashServer(t, ctx, s.pool)
	defer ts.Close()
	code, body := get(t, client, ts.URL+"/funnel")
	if code != http.StatusOK {
		t.Fatalf("GET /funnel = %d, want 200\n%s", code, snippet(body))
	}

	rowA := funRow(t, body, funAcctA, "imap")
	if strings.Contains(rowA, "never") {
		t.Errorf("%s / imap shows `never` despite a successful run 10 minutes ago:\n%s", funAcctA, rowA)
	}
	if strings.Contains(rowA, "stale") {
		t.Errorf("%s / imap is marked stale with a successful run 10 minutes ago (threshold 3h):\n%s",
			funAcctA, rowA)
	}

	rowB := funRow(t, body, funAcctB, "imap")
	if !strings.Contains(rowB, "never") {
		t.Errorf("%s / imap has ONLY failing runs and must show `never`; the row is:\n%s\n\n"+
			"Criterion 3: the health query is `status='ok' AND finished_at IS NOT NULL`, max(finished_at). "+
			"Without the status filter a connector that has failed every attempt for four days renders as "+
			"freshly synced — which is the exact question this page was built to answer, and the reason "+
			"/sources' 'newest run of ANY phase and ANY status' column is being deleted rather than moved",
			funAcctB, rowB)
	}

	rowC := funRow(t, body, funAcctC)
	if !strings.Contains(rowC, "(none)") || !strings.Contains(rowC, "never") {
		t.Errorf("%s has no sync_runs at all and must appear ONCE, under the (none) phase, showing never; "+
			"the row is:\n%s\n\nAn account that vanishes because it has no runs is exactly the connector an "+
			"operator is looking for", funAcctC, rowC)
	}

	if !funHasRow(body, funAcctA, "(none)") {
		t.Errorf("%s has two phase-less runs and no (none) row. sync_runs has no phase column — the phase "+
			"is a key in stats, and jira and upworkcrm insert the default '{}'. (none) is a real, expected "+
			"bucket, not a bug\n%s", funAcctA, snippet(body))
	}
}

// ---- criterion 5: the calendar verdict agrees with propose_slots --------------

// THE LOAD-BEARING TEST OF THIS SECTION. A page that says "green" while
// propose_slots refuses is the specific failure criterion 5 exists to prevent,
// and the only way to prove they agree is to ask BOTH in one test with one
// AVAIL_MAX_SYNC_AGE, one set of sync_runs rows and one clock.
func TestFunnel_Integration_CalendarVerdictAgreesWithProposeSlots(t *testing.T) {
	ctx := context.Background()
	s := newFunSuite(t, ctx)

	// A value no default could produce, so "the page printed the effective
	// AVAIL_MAX_SYNC_AGE" is checkable rather than a coincidence with 1h.
	t.Setenv("AVAIL_MAX_SYNC_AGE", "37m")

	cal := s.account(t, ctx, "google", funAcctCal, true)
	s.syncRun(t, ctx, cal, "calendar", "ok", 9*time.Hour)

	// ASK THE TOOL FIRST. It is the fixture's own precondition: if propose_slots
	// answers here, the seeded account is not in availability scope and every
	// page assertion below would be measuring nothing.
	reg := executor.NewRegistry()
	tools.Register(reg, s.pool)
	ex := executor.New(reg, policy.NewMatrix(policy.NewPGSnapshotLoader(s.pool), policy.NewStatic(reg.Names()...)),
		audit.NewPGStore(s.pool))
	_, err := ex.Execute(ctx, executor.Call{
		Tool: "propose_slots", Actor: "opsctl:itest-funnel", Args: []byte(`{"duration_minutes":30}`),
	})
	if err == nil {
		t.Fatalf("propose_slots ANSWERED with a 9-hour-old calendar sync under AVAIL_MAX_SYNC_AGE=37m. " +
			"Either the tool regressed or the fixture is not in availability scope; in both cases the " +
			"agreement this test asserts is untestable as written")
	}
	if !strings.Contains(err.Error(), funAcctCal) {
		t.Fatalf("propose_slots refused without naming %s: %v", funAcctCal, err)
	}
	if !strings.Contains(err.Error(), "not ready") {
		t.Errorf("propose_slots' refusal text changed (%v); the page's prose quotes it", err)
	}

	// Now the page. Same account, same env, same rows, same clock.
	ts, client := newDashServer(t, ctx, s.pool)
	defer ts.Close()
	code, body := get(t, client, ts.URL+"/funnel")
	if code != http.StatusOK {
		t.Fatalf("GET /funnel = %d, want 200\n%s", code, snippet(body))
	}

	if !strings.Contains(body, "37m") {
		t.Errorf("the page does not print the effective AVAIL_MAX_SYNC_AGE (37m). Criterion 5: the value is "+
			"shown next to the section, because a freshness verdict whose threshold is invisible cannot be "+
			"argued with\n%s", snippet(body))
	}
	// THE AGREEMENT. propose_slots refuses for this account; the page must not
	// call it green. The converse (green page => tool answers) is deliberately
	// NOT asserted: readiness scope is global and this db is shared, so another
	// suite's stale calendar would make it a flake. Refusal is the direction
	// that matters — it is the one an operator opens the page to explain.
	row := funRow(t, body, funAcctCal, "calendar")
	if !strings.Contains(row, "stale") {
		t.Errorf("propose_slots refuses for %s, and the page's calendar row is not marked stale:\n%s\n\n"+
			"A page that says green while the tool refuses is the specific failure criterion 5 exists to "+
			"prevent. Judge calendar rows with availability.NotReady over availability.CalendarSyncStates "+
			"and tools.MaxCalendarSyncAge() — the system's own rule, not a second spelling of it",
			funAcctCal, row)
	}
	if !strings.Contains(strings.ToLower(body), "propose_slots") {
		t.Errorf("the page never mentions propose_slots. Criterion 5: it states in prose that propose_slots " +
			"refuses while any in-scope account is stale — the number alone does not say what breaks")
	}
}

// ---- criteria 7, 8, 9: the intake trend ---------------------------------------

func TestFunnel_Integration_IntakeTrendFillsGapsAndHonoursTheWindow(t *testing.T) {
	ctx := context.Background()
	s := newFunSuite(t, ctx)
	a := s.account(t, ctx, funProvider, funAcctA, false)

	s.rawItem(t, ctx, a, "raw-today", 0)
	s.rawItem(t, ctx, a, "raw-2d", 2)
	s.rawItem(t, ctx, a, "raw-40d", 40)
	// A normalized message today, so the second intake axis has something of
	// ours on the same day.
	s.message(t, ctx, a, "msg-today", "inbound", 0)

	ts, client := newDashServer(t, ctx, s.pool)
	defer ts.Close()

	code, body := get(t, client, ts.URL+"/funnel?days=14")
	if code != http.StatusOK {
		t.Fatalf("GET /funnel?days=14 = %d, want 200\n%s", code, snippet(body))
	}

	if !strings.Contains(body, funAcctA) {
		t.Errorf("the intake table has no column for %s; criterion 7 wants one count per source_account", funAcctA)
	}
	for _, offset := range []int{0, -2} {
		if !strings.Contains(body, funDay(offset)) {
			t.Errorf("the page has no row for %s, where this suite seeded raw items", funDay(offset))
		}
	}
	// Criterion 8: the day BETWEEN them is rendered as a zero row, not omitted.
	// An absent row is exactly the signal the page exists to show, and a missing
	// table row is not a signal anyone notices.
	if !strings.Contains(body, funDay(-1)) {
		t.Errorf("the page omits %s entirely. Criterion 8: days with zero rows render as zeros — the gap "+
			"fill is the difference between 'nothing arrived' and 'the table stops here'", funDay(-1))
	}
	if strings.Contains(body, funDay(-40)) {
		t.Errorf("a 40-day-old day (%s) appears at ?days=14; the window is shared by every windowed "+
			"section on the page", funDay(-40))
	}

	// POSITIVE CONTROL for the window assertion, and criterion 7's clamp: 90 is
	// the ceiling, and 9999 must be clamped to it rather than scheduling a scan.
	if _, wide := get(t, client, ts.URL+"/funnel?days=90"); !strings.Contains(wide, funDay(-40)) {
		t.Errorf("POSITIVE CONTROL FAILED: %s is absent from ?days=90 too, so the exclusion above proves "+
			"nothing about the window", funDay(-40))
	}
	if _, huge := get(t, client, ts.URL+"/funnel?days=9999"); strings.Contains(huge, funDay(-91)) {
		t.Errorf("?days=9999 rendered a day 91 back; the parameter is clamped to [1,90] so a URL cannot " +
			"schedule an unbounded scan")
	}
	if code, bad := get(t, client, ts.URL+"/funnel?days=abc"); code != http.StatusOK {
		t.Errorf("GET /funnel?days=abc = %d, want 200 — an unparseable window falls back to the default, "+
			"it does not error the page\n%s", code, snippet(bad))
	}
}

// ---- criteria 14 + 15: capture attribution ------------------------------------

func TestFunnel_Integration_CaptureAttributionShowsThreeStatesInboundOnly(t *testing.T) {
	ctx := context.Background()
	s := newFunSuite(t, ctx)
	a := s.account(t, ctx, funProvider, funAcctA, false)
	proj := s.project(t, ctx)

	s.decision(t, ctx, s.message(t, ctx, a, "cap-matched", "inbound", 0), proj, "attributed")
	s.decision(t, ctx, s.message(t, ctx, a, "cap-unmatched", "inbound", 0), proj, "unmatched")
	s.message(t, ctx, a, "cap-unseen", "inbound", 0) // no decision at all
	s.message(t, ctx, a, "cap-outbound", "outbound", 0)

	ts, client := newDashServer(t, ctx, s.pool)
	defer ts.Close()
	code, body := get(t, client, ts.URL+"/funnel")
	if code != http.StatusOK {
		t.Fatalf("GET /funnel = %d, want 200\n%s", code, snippet(body))
	}
	lower := strings.ToLower(body)

	// THREE labelled states, never two. The per-fixture counts are pinned in
	// internal/capture/attribution_integration_test.go, where the population can
	// be isolated; on this page the assertion is that the third column EXISTS
	// and is labelled, because the trap is that it silently does not.
	for _, label := range []string{"matched", "unmatched", "not yet evaluated"} {
		if !strings.Contains(lower, label) {
			t.Errorf("the capture section has no %q column. Criterion 14: THREE states. No decision row is "+
				"UNSEEN — the engine has not looked — and conflating it with unmatched hands every fresh "+
				"message to the model before the rules run", label)
		}
	}
	if !strings.Contains(body, funDay(0)) {
		t.Errorf("the capture section has no row for today, where this suite seeded four messages")
	}
	// Criterion 15's prose, on the page rather than only in a comment: the
	// reader has to be told why outbound is missing, or its absence reads as a
	// bug in the page.
	if !strings.Contains(lower, "inbound") {
		t.Errorf("the capture section never says it counts inbound messages only. capture filters " +
			"direction='inbound' (that line IS invariant 5), so an outbound message can never carry a " +
			"decision — absent-because-impossible, not pending")
	}
}

// ---- criteria 10 + 13: the classify block --------------------------------------

func TestFunnel_Integration_ClassifyRendersBothLanesWithFlagDetail(t *testing.T) {
	ctx := context.Background()
	s := newFunSuite(t, ctx)

	const linkURL = "https://itest-funnel.example/pay"
	s.verdict(t, ctx, "classify", `{"actionable":true,"kind":"payment_due","title":"itest-funnel personal flag",
	    "reason":"itest","sender":"bills@itest-funnel.example","subject":"Amount due 12 Sep",
	    "normalized_message_id":7701,"link_candidates":2,"link_index":1,"link_url":"`+linkURL+`","link_text":"PAY NOW"}`)
	s.verdict(t, ctx, "classify", `{"actionable":true,"kind":"deadline","title":"itest-funnel personal nolink",
	    "reason":"itest","sender":"bills@itest-funnel.example","subject":"Second notice",
	    "normalized_message_id":7702,"link_candidates":0}`)
	s.verdict(t, ctx, "classify_residue", `{"actionable":true,"kind":"action_required","title":"itest-funnel residue flag",
	    "reason":"itest","sender":"alerts@itest-funnel.example","subject":"Verify your sign-in",
	    "normalized_message_id":7703,"link_candidates":0}`)
	s.skippedRun(t, ctx, "classify", "itest_funnel_local_provider_unreachable")
	s.skippedRun(t, ctx, "classify_residue", "itest_funnel_residue_restricted")

	ts, client := newDashServer(t, ctx, s.pool)
	defer ts.Close()
	code, body := get(t, client, ts.URL+"/funnel?days=1")
	if code != http.StatusOK {
		t.Fatalf("GET /funnel = %d, want 200\n%s", code, snippet(body))
	}

	// BOTH lanes. The residue is a separate population with its own prompt and
	// its own unmeasured numbers; a page that rendered only the personal lane
	// would be silently answering a different question.
	for _, want := range []string{"itest-funnel personal flag", "itest-funnel residue flag"} {
		if !strings.Contains(body, want) {
			t.Errorf("the classify section does not render %q (criterion 10: BOTH lanes)\n%s", want, snippet(body))
		}
	}
	for _, want := range []string{"itest_funnel_local_provider_unreachable", "itest_funnel_residue_restricted"} {
		if !strings.Contains(body, want) {
			t.Errorf("the skipped breakdown does not render %q. Without it a fully-skipped pass shows "+
				"classified: 0, which is indistinguishable from an empty inbox or a dead poller", want)
		}
	}

	// Criterion 13: sender, subject and the resolved link, from the STORED
	// fields — no normalized_messages row exists for id 7701, so a join back for
	// a second copy could only produce blanks.
	flagged := funRow(t, body, "itest-funnel personal flag")
	for _, want := range []string{"bills@itest-funnel.example", "Amount due 12 Sep", "7701"} {
		if !strings.Contains(flagged, want) {
			t.Errorf("the flagged row is missing %q:\n%s", want, flagged)
		}
	}
	if !strings.Contains(flagged, `href="`+linkURL+`"`) {
		t.Errorf("the flagged row does not render the resolved link as an <a>:\n%s\n\nA flagged notice has "+
			"to be actionable from the page instead of sending the reader back to the mailbox", flagged)
	}
	// And the placeholder, because no-candidates is the COMMON case and an empty
	// cell reads as a rendering bug.
	nolink := funRow(t, body, "itest-funnel personal nolink")
	if strings.Contains(nolink, "href=") {
		t.Errorf("a verdict with no resolved link rendered an anchor anyway:\n%s", nolink)
	}
	if !strings.Contains(nolink, "—") {
		t.Errorf("a verdict with no resolved link does not render the em-dash placeholder:\n%s", nolink)
	}
}

// ---- criterion 17: sections degrade independently ------------------------------

// One section's query is made to fail for real — not by injecting a fake loader,
// which is what funnel_test.go's runSections case already covers, but by putting
// a broken `ai_runs` in front of the real one on the search_path for ONE pool.
// Everything else still resolves to public, so this is the production shape of
// the failure: one query errors, three do not.
//
// /sources 500s the whole page when any one of its queries errors. On a
// four-section page that would mean a single broken aggregate takes away the
// operator's only ingestion view at exactly the moment they opened it.
func TestFunnel_Integration_OneBrokenSectionStillRendersTheOthers(t *testing.T) {
	ctx := context.Background()
	s := newFunSuite(t, ctx)

	a := s.account(t, ctx, funProvider, funAcctA, false)
	s.syncRun(t, ctx, a, "imap", "ok", 5*time.Minute)
	s.rawItem(t, ctx, a, "degrade-raw", 0)

	if _, err := s.pool.Exec(ctx, `CREATE SCHEMA `+funShadowSchema); err != nil {
		t.Fatalf("create shadow schema: %v", err)
	}
	// Same NAME, wrong shape: the classify query joins ai_runs and reads
	// r.worker_type, r.status and r.created_at, none of which exist here.
	if _, err := s.pool.Exec(ctx, `CREATE TABLE `+funShadowSchema+`.ai_runs (id BIGINT)`); err != nil {
		t.Fatalf("create shadow ai_runs: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, `SET search_path TO `+funShadowSchema+`, public`)
		return err
	}
	shadowPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open shadow pool: %v", err)
	}
	defer shadowPool.Close()

	ts, client := newDashServer(t, ctx, shadowPool)
	defer ts.Close()

	code, body := get(t, client, ts.URL+"/funnel")
	if code != http.StatusOK {
		t.Fatalf("GET /funnel with ONE failing section = %d, want 200. Criterion 17: each section's loader "+
			"returns an error instead of aborting the response; the request still returns 200 and the page "+
			"renders every section that loaded\n%s", code, snippet(body))
	}
	lower := strings.ToLower(body)
	if !strings.Contains(lower, "classif") {
		t.Errorf("the failing section's heading is gone from the page; a section that vanishes tells the "+
			"reader nothing about which number is missing\n%s", snippet(body))
	}
	if !strings.Contains(lower, "worker_type") && !strings.Contains(lower, "error") {
		t.Errorf("the page shows no error line for the failed section. Criterion 17: an inline, styled "+
			"error line NAMING the failed section — silence here is a section that renders as zeros, which "+
			"is the one reading this page must never produce\n%s", snippet(body))
	}
	// The three that still work must still be there.
	if !strings.Contains(body, funAcctA) {
		t.Errorf("connector health did not render while the classify section was broken\n%s", snippet(body))
	}
	if !strings.Contains(body, funDay(0)) {
		t.Errorf("the intake trend did not render while the classify section was broken\n%s", snippet(body))
	}
	if !strings.Contains(lower, "not yet evaluated") {
		t.Errorf("the capture section did not render while the classify section was broken\n%s", snippet(body))
	}
}

// ---- criterion 22: /sources still works, minus the run columns ----------------

func TestFunnel_Integration_SourcesKeepsItsTotalsAndLosesItsRunColumns(t *testing.T) {
	ctx := context.Background()
	s := newFunSuite(t, ctx)
	a := s.account(t, ctx, funProvider, funAcctA, false)
	s.syncRun(t, ctx, a, "imap", "ok", 5*time.Minute)
	s.message(t, ctx, a, "src-msg", "inbound", 0)

	ts, client := newDashServer(t, ctx, s.pool)
	defer ts.Close()

	code, body := get(t, client, ts.URL+"/sources")
	if code != http.StatusOK {
		t.Fatalf("GET /sources = %d, want 200 — criterion 22 is a deletion of two columns, not a removal "+
			"of the page\n%s", code, snippet(body))
	}
	// Out of scope: any OTHER change to /sources. Its lifetime totals, channel
	// table, headline numbers and empty states stay exactly as they are.
	if !strings.Contains(body, funAcctA) {
		t.Errorf("/sources no longer lists the seeded account %s", funAcctA)
	}
	for _, keep := range []string{"Channels", "awaiting normalize"} {
		if !strings.Contains(body, keep) {
			t.Errorf("/sources lost %q; criterion 22 removes the run columns and nothing else", keep)
		}
	}
	if strings.Contains(body, "Last run") {
		t.Errorf("/sources still renders a `Last run` column. Criterion 22 (Q1-a): connector health moves " +
			"to /funnel in THIS ticket. Two pages computing 'last run' with two different rules — one of " +
			"them the newest run of ANY phase and ANY status — is the repo's recurring defect, and leaving " +
			"both spellings live for one release is how the second one survives")
	}
	if !strings.Contains(body, "/funnel") {
		t.Errorf("/sources does not point at /funnel; a reader who used the deleted column has to be told " +
			"where it went")
	}
}
