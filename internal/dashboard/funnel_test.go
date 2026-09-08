package dashboard

// Unit + structural tests for SWT-29 (docs/tickets/funnel-view_SPEC.md): the
// read-only /funnel ingestion page. IN-PACKAGE on purpose — criteria 2 and 22
// scan the embedded templates through templateFS and the unexported helpers,
// which is the internal/dashboard/board_structure_test.go idiom.
//
// ZERO I/O beyond reading this repo's own source and the embedded templates.
// No Postgres, no LLM, no network: everything here is either a pure function or
// a source scan. The four sections' SQL is covered by
// funnel_integration_test.go, because a predicate whose input comes from a
// COLUMN gets its regression test where Postgres produces the value (IK: "the
// guard whose column no query selected" — the unit test cannot catch it, by
// construction, because the unit test is the thing supplying the value).
//
// GREENFIELD NOTE — EXPECTED RED. internal/dashboard/funnel.go and
// templates/funnel.html do not exist, so this file compile-FAILS the whole
// `dashboard` test binary (which also carries auth_test.go and export_test.go —
// they are not broken, they are unbuildable until funnel.go lands). Once it
// compiles, the /sources assertions of criterion 22 stay red until the five
// sync_runs subqueries and the two template columns are actually DELETED: that
// half is red-by-contradiction against shipped code, not greenfield.
//
// IMPOSED SURFACE (the SPEC names the helpers but not their signatures; these
// are the smallest shapes that work, and the ASSERTIONS are the contract — an
// implementer who prefers another spelling should change these call sites, not
// the behaviour):
//
//	const funnelDisplayStaleAfter = 3 * time.Hour
//	func funnelFreshness(last, now time.Time, max time.Duration) string  // "ok"|"stale"|"never"
//	func clampDays(raw string) int
//	type intakeDay struct { Day time.Time; PerAccount map[string]int; RawTotal, Messages int }
//	func fillDays(rows []intakeDay, now time.Time, days int) []intakeDay  // newest first
//	type funnelSection struct { Name string; Load func() error }
//	func runSections(sections []funnelSection) []string  // one line per failure

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/tools"
)

// ---- helpers -----------------------------------------------------------------

// funnelRepoFile reads a repo-relative file. It FATALS when the file is absent
// rather than skipping: a source scan whose subject does not exist is the
// "fixture that proves nothing" landmine wearing a lab coat.
func funnelRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	if len(b) < 200 {
		t.Fatalf("%s is %d bytes; every scan below would pass vacuously", rel, len(b))
	}
	return string(b)
}

func funnelTemplate(t *testing.T) string {
	t.Helper()
	raw, err := templateFS.ReadFile("templates/funnel.html")
	if err != nil {
		t.Fatalf("read embedded templates/funnel.html: %v", err)
	}
	if len(raw) < 400 {
		t.Fatalf("templates/funnel.html is %d bytes; the prose scans below would pass vacuously", len(raw))
	}
	return string(raw)
}

// funnelMentionsOutsideComments is internal/availability/callsites_test.go's
// rule, restated here because that scan does not reach a template: a needle on
// a whole-line `//` comment is prose and is allowed; anywhere else it counts.
// A TRAILING comment on a struct field therefore trips it, deliberately.
func funnelMentionsOutsideComments(src, needle string) bool {
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

// funnelHandler builds the real mux with dev auth and NO pool. Handler()
// registration touches neither, so this exercises the route table itself.
func funnelHandler(t *testing.T) http.Handler {
	t.Helper()
	auth, err := NewAuth(t.Context(), "", "", "", "") // issuer "" -> dev mode, no network
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	srv, err := NewServer(nil, nil, auth)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv.Handler()
}

// ---- criterion 4: funnelFreshness is pure ------------------------------------

// Relative times only (anti-date-rot): every instant below is derived from the
// injected `now`, so this test reads the same in 2027 as today.
//
// The boundary semantics mirror availability.NotReady deliberately — inclusive
// at the floor, and a FUTURE-dated run is ready rather than an error. Clock skew
// between us and Postgres is seconds; turning a second of skew into a red row on
// an ops page is how a display threshold teaches an operator to ignore it.
func TestFunnelFreshness_ClassifiesAgainstTheInjectedNow(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	const max = 3 * time.Hour

	cases := []struct {
		name string
		last time.Time
		want string
	}{
		{"a run one minute ago is ok", now.Add(-time.Minute), "ok"},
		{"a run exactly at the floor is ok (inclusive)", now.Add(-max), "ok"},
		{"a run one second past the floor is stale", now.Add(-max - time.Second), "stale"},
		{"a run four days ago is stale", now.Add(-96 * time.Hour), "stale"},
		{"the zero time is never, not stale", time.Time{}, "never"},
		{"a future-dated run is ok, not an error", now.Add(90 * time.Second), "ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := funnelFreshness(tc.last, now, max); got != tc.want {
				t.Errorf("funnelFreshness(%v, now, %v) = %q, want %q", tc.last, max, got, tc.want)
			}
		})
	}
}

// "never" and "stale" are DIFFERENT answers and the page prints them
// differently: an account that has never had a successful run of a phase it has
// attempted is a connector that has never worked, while stale is one that
// stopped. Collapsing them is the three-states trap in a freshness costume.
func TestFunnelFreshness_NeverIsNotStale(t *testing.T) {
	now := time.Now()
	if funnelFreshness(time.Time{}, now, time.Hour) == funnelFreshness(now.Add(-10*time.Hour), now, time.Hour) {
		t.Fatalf("funnelFreshness answers the same thing for \"no successful run has ever finished\" and " +
			"\"the last one is old\". Criterion 3 requires the page to show `never` for the first: a " +
			"connector that never worked and one that stopped four days ago need different fixes")
	}
}

// ---- criterion 6: the display-only threshold ---------------------------------

func TestFunnelDisplayStaleAfter_IsThreeHours(t *testing.T) {
	if funnelDisplayStaleAfter != 3*time.Hour {
		t.Fatalf("funnelDisplayStaleAfter = %v, want 3h (criterion 6). */15 CronJobs plus the resident mail "+
			"watch loop make 3h about 12 missed passes. It is a package CONSTANT and not an env var on "+
			"purpose: it gates nothing, and an env knob implies a contract", funnelDisplayStaleAfter)
	}
}

// ---- criterion 7: ?days= parsing ---------------------------------------------

func TestClampDays_DefaultsToFourteenAndClampsToNinety(t *testing.T) {
	cases := []struct {
		raw  string
		want int
		why  string
	}{
		{"", 14, "the default"},
		{"7", 7, "an in-range value passes through"},
		{"1", 1, "the low bound is inclusive"},
		{"90", 90, "the high bound is inclusive"},
		{"0", 1, "zero clamps up: a zero-day window renders nothing and reads as a broken page"},
		{"-5", 1, "negative clamps up"},
		{"91", 90, "above the bound clamps down"},
		{"100000", 90, "an unbounded URL parameter must not schedule a full scan"},
		{"abc", 14, "unparseable falls back to the default rather than erroring the page"},
		{"14d", 14, "a trailing unit is not a number"},
	}
	for _, tc := range cases {
		if got := clampDays(tc.raw); got != tc.want {
			t.Errorf("clampDays(%q) = %d, want %d — %s", tc.raw, got, tc.want, tc.why)
		}
	}
}

// ---- criterion 8: zero days are rendered, not omitted -------------------------

// The gap fill is the whole point of the intake section. An absent row is
// EXACTLY the signal the page exists to show ("quiet because a connector died
// four days ago"), and a missing table row is not a signal anyone notices.
func TestFillDays_RendersEmptyDaysAsZeroRowsNewestFirst(t *testing.T) {
	now := time.Date(2026, 9, 8, 15, 30, 0, 0, time.UTC)
	day := func(k int) time.Time {
		d := now.AddDate(0, 0, -k)
		return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, d.Location())
	}

	// Postgres returns only the days that HAVE rows: today and three days back.
	// Days 1 and 2 are the gap, and day 9 is outside the window entirely.
	rows := []intakeDay{
		{Day: day(0), PerAccount: map[string]int{"a@local": 4}, RawTotal: 4, Messages: 3},
		{Day: day(3), PerAccount: map[string]int{"a@local": 7}, RawTotal: 7, Messages: 7},
		{Day: day(9), PerAccount: map[string]int{"a@local": 99}, RawTotal: 99, Messages: 99},
	}

	got := fillDays(rows, now, 5)

	if len(got) != 5 {
		t.Fatalf("fillDays returned %d rows for days=5, want exactly 5 — the window is the table, and a "+
			"short table hides the very gap the section exists to show", len(got))
	}
	for i, r := range got {
		if want := day(i); !r.Day.Equal(want) {
			t.Fatalf("row %d is %s, want %s — rows are one per day, newest first, with no gaps",
				i, r.Day.Format("2006-01-02"), want.Format("2006-01-02"))
		}
	}
	if got[0].RawTotal != 4 || got[0].Messages != 3 || got[0].PerAccount["a@local"] != 4 {
		t.Errorf("today's row lost its counts: %+v", got[0])
	}
	if got[3].RawTotal != 7 || got[3].Messages != 7 {
		t.Errorf("the day-3 row lost its counts: %+v", got[3])
	}
	for _, i := range []int{1, 2, 4} {
		if got[i].RawTotal != 0 || got[i].Messages != 0 {
			t.Errorf("day %d should be a ZERO row, got %+v", i, got[i])
		}
	}
	for _, r := range got {
		if r.Day.Equal(day(9)) {
			t.Errorf("fillDays kept a row from outside the %d-day window (%s)", 5, r.Day.Format("2006-01-02"))
		}
	}
}

// A window with no rows at all must still render its days. This is the state
// the page is opened IN — "the board is stale" — and an empty table body says
// "the section is broken", which sends the reader to the wrong place.
func TestFillDays_EmptyResultStillRendersTheWholeWindow(t *testing.T) {
	now := time.Now()
	got := fillDays(nil, now, 14)
	if len(got) != 14 {
		t.Fatalf("fillDays(nil, now, 14) returned %d rows, want 14 zero rows", len(got))
	}
	for i, r := range got {
		if r.RawTotal != 0 || r.Messages != 0 {
			t.Errorf("row %d of an empty window is not zero: %+v", i, r)
		}
	}
}

// ---- criterion 17: sections degrade independently -----------------------------

// One deliberately failing loader and two succeeding ones. The failure this
// encodes is the shape /sources has today: sources.go 500s the WHOLE page when
// any one of its queries errors, so a single broken aggregate makes an
// operator's only ingestion view disappear at exactly the moment they need it.
func TestRunSections_OneFailureDoesNotStopTheOthers(t *testing.T) {
	var ran []string
	sections := []funnelSection{
		{Name: "connector health", Load: func() error { ran = append(ran, "health"); return nil }},
		{Name: "intake trend", Load: func() error {
			ran = append(ran, "intake")
			return errTestSectionFailed
		}},
		{Name: "capture attribution", Load: func() error { ran = append(ran, "capture"); return nil }},
	}

	errs := runSections(sections)

	if len(ran) != 3 {
		t.Fatalf("runSections ran %v, want all three loaders — a section that fails must not abort the "+
			"response (criterion 17); the page renders every section that loaded", ran)
	}
	if len(errs) != 1 {
		t.Fatalf("runSections returned %d error lines, want exactly 1: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0], "intake trend") {
		t.Errorf("the error line %q does not NAME the failed section. An unnamed error on a four-section "+
			"page tells the reader nothing about which number to distrust", errs[0])
	}
	if !strings.Contains(errs[0], errTestSectionFailed.Error()) {
		t.Errorf("the error line %q drops the underlying error text", errs[0])
	}
}

func TestRunSections_AllGreenYieldsNoErrorLines(t *testing.T) {
	errs := runSections([]funnelSection{
		{Name: "a", Load: func() error { return nil }},
		{Name: "b", Load: func() error { return nil }},
	})
	if len(errs) != 0 {
		t.Fatalf("runSections returned %v for two succeeding loaders, want none", errs)
	}
}

var errTestSectionFailed = &testSectionError{}

type testSectionError struct{}

func (*testSectionError) Error() string { return "itest: relation \"nope\" does not exist" }

// ---- criterion 2: the page is a window, not a control -------------------------

// Invariant 3 applies VACUOUSLY here and that is the point: the page performs no
// tool calls, so there is nothing to route through validate -> policy -> audit.
// The risk is a "small" action creeping onto a read page (retry a sync, requeue
// normalization) and reaching the DB directly because the page already holds a
// pool. This makes that mechanically visible.
func TestFunnelSource_ExecutesNothing(t *testing.T) {
	src := funnelRepoFile(t, filepath.Join("internal", "dashboard", "funnel.go"))

	for _, banned := range []string{"s.ex", "executor.Call", "s.execute(", "s.executeTo("} {
		if funnelMentionsOutsideComments(src, banned) {
			t.Errorf("internal/dashboard/funnel.go references %q. Criterion 2: /funnel is a window, not a "+
				"control — it performs NO tool calls. If an action is ever wanted here it goes through "+
				"s.execute(...) like /deliveries does, and this criterion gets renegotiated first", banned)
		}
	}
}

func TestFunnelTemplate_HasNoFormsAndNoHTMXPosts(t *testing.T) {
	tmpl := funnelTemplate(t)
	lower := strings.ToLower(tmpl)
	for _, banned := range []string{`<form method="post"`, "<form method='post'", "hx-post"} {
		if strings.Contains(lower, banned) {
			t.Errorf("templates/funnel.html contains %q. The page executes nothing (criterion 2): tables and "+
				"a manual reload, no charts, no JS, no HTMX polling", banned)
		}
	}
	if strings.Contains(lower, "hx-post") || strings.Contains(lower, "hx-trigger=\"every") {
		t.Errorf("templates/funnel.html has HTMX polling; auto-refresh is out of scope")
	}
}

// The route table, checked two ways because neither alone is enough.
//
// A RUNTIME PROBE ALONE IS NEARLY VACUOUS HERE, and saying why matters: the mux
// already carries `GET /` as a catch-all redirect to /tasks, and Go's ServeMux
// answers 405 for a POST to any path a GET pattern matches. So an unregistered
// POST /funnel looks identical to a deliberately-absent one, and an
// unregistered GET /funnel still 302s to the login stub through the catch-all.
// The source scan is what actually pins the two facts; the probe is the control
// that the mux has not grown a handler the scan's spelling missed.
func TestFunnelRoutes_RegisteredForGetOnlyAndSessionGated(t *testing.T) {
	src := funnelRepoFile(t, filepath.Join("internal", "dashboard", "server.go"))

	// POSITIVE CONTROL: this is the file that holds the route table.
	if !strings.Contains(src, `mux.Handle("GET /sources"`) {
		t.Fatalf("POSITIVE CONTROL FAILED: internal/dashboard/server.go does not register GET /sources; the " +
			"scans below are looking at the wrong file or the route idiom has changed")
	}

	var funnelLine string
	for _, line := range strings.Split(src, "\n") {
		if strings.Contains(line, `"GET /funnel"`) {
			funnelLine = line
			break
		}
	}
	if funnelLine == "" {
		t.Fatalf("internal/dashboard/server.go does not register `GET /funnel`. Criterion 1: the route is " +
			"registered on the mux next to the /sources line, and that line's comment block — which today " +
			"claims /sources is \"the only page that reads the raw/normalized tables\" — is UPDATED rather " +
			"than duplicated, because after criterion 22 that claim is shared with /funnel")
	}
	if !strings.Contains(funnelLine, "s.auth.Require") {
		t.Errorf("GET /funnel is registered WITHOUT s.auth.Require:\n  %s\nCriterion 1: wrapped exactly like "+
			"every other page route. With OIDC_ISSUER unset the dashboard hands a session to anyone who "+
			"reaches /dev/login, and an unwrapped route does not even need that", strings.TrimSpace(funnelLine))
	}
	if strings.Contains(src, `"POST /funnel`) {
		t.Errorf("internal/dashboard/server.go registers a POST route under /funnel. Criterion 2: the page " +
			"is a window, not a control — no retry-a-sync, no mark-run-ok, no requeue-normalization button. " +
			"If an action is ever wanted here it goes through s.execute(...) like /deliveries does, and " +
			"this criterion gets renegotiated first")
	}

	// The control: nothing under /funnel answers a POST. 404 (no pattern) and
	// 405 (a GET-only pattern matched) are both fine; anything else means a
	// handler ran.
	h := funnelHandler(t)
	for _, path := range []string{"/funnel", "/funnel/", "/funnel/refresh", "/funnel/normalize", "/funnel/1/retry"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 404 or 405 (no route)", path, rec.Code)
		}
	}
}

// ---- criterion 22: connector health MOVED, it was not copied ------------------

// Leaving both spellings live "for one release" is how the second one survives.
// The deletion ships with the addition, and this scan is what makes that
// mechanical — with its POSITIVE CONTROL, without which it certifies a
// sources.go that never had the subqueries against a funnel.go that never grew
// them (the failure mode of every source-scanning test).
func TestConnectorHealthMovedFromSourcesToFunnel(t *testing.T) {
	sources := funnelRepoFile(t, filepath.Join("internal", "dashboard", "sources.go"))
	funnel := funnelRepoFile(t, filepath.Join("internal", "dashboard", "funnel.go"))

	if !funnelMentionsOutsideComments(funnel, "sync_runs") {
		t.Fatalf("POSITIVE CONTROL FAILED: internal/dashboard/funnel.go does not name sync_runs. The " +
			"assertion below would then pass for the wrong reason — connector health has to LAND on " +
			"/funnel, not merely leave /sources")
	}
	if funnelMentionsOutsideComments(sources, "sync_runs") {
		t.Errorf("internal/dashboard/sources.go still names sync_runs. Criterion 22 (Q1-a): /sources gives " +
			"up connector health in THIS ticket — the five scalar subqueries, the five sourceRow fields " +
			"and their scans. Two pages computing 'last run' with two different rules is the repo's " +
			"recurring defect; the whole reason the funnel page exists is that /sources' rule (newest run " +
			"of ANY phase and ANY status) is not the question")
	}
}

// The Go struct, not only the SQL: a field kept "for now" grows a template
// reference, and the next reader restores the query to feed it.
func TestSourceRow_HasNoRunColumns(t *testing.T) {
	src := funnelRepoFile(t, filepath.Join("internal", "dashboard", "sources.go"))
	if !strings.Contains(src, "type sourceRow struct") {
		t.Fatalf("POSITIVE CONTROL FAILED: sources.go has no sourceRow struct; the scan below is vacuous")
	}
	for _, field := range []string{"LastRunAt", "LastRunPhase", "LastRunStatus", "LastRunError", "RunsTotal"} {
		if funnelMentionsOutsideComments(src, field) {
			t.Errorf("sourceRow still carries %s (criterion 22). Runs and freshness live on /funnel now", field)
		}
	}
}

func TestSourcesTemplate_LostItsRunColumnsAndPointsAtFunnel(t *testing.T) {
	raw, err := templateFS.ReadFile("templates/sources.html")
	if err != nil {
		t.Fatalf("read embedded sources.html: %v", err)
	}
	s := string(raw)

	// Control: the page must still be the ingestion page. Criterion 22 is a
	// deletion plus one line — "Any OTHER change to /sources" is out of scope,
	// so its lifetime totals and channel table stay exactly as they are.
	for _, keep := range []string{"Channels", "{{.TotalRaw}}", "{{.TotalMessages}}", "awaiting normalize"} {
		if !strings.Contains(s, keep) {
			t.Fatalf("POSITIVE CONTROL FAILED: sources.html no longer contains %q. Criterion 22 is a "+
				"deletion of two columns, not a rewrite of the page", keep)
		}
	}

	for _, gone := range []string{"<th>Last run</th>", "<th>Status</th>", ".LastRunStatus", ".LastRunAt",
		".LastRunPhase", ".LastRunError", ".RunsTotal"} {
		if strings.Contains(s, gone) {
			t.Errorf("templates/sources.html still contains %q (criterion 22)", gone)
		}
	}
	if strings.Contains(s, `colspan="13"`) {
		t.Errorf(`templates/sources.html still has colspan="13" on the empty-state row; two columns went ` +
			`away, so the empty state now spans 11. An empty state that spans the wrong number of columns ` +
			`is the one row nobody looks at until the table is empty, which is the moment it matters`)
	}
	if !strings.Contains(s, "/funnel") {
		t.Errorf("templates/sources.html does not point at /funnel. The pointer line is half of criterion " +
			"22: a reader who used the Last run column has to be told where it went, or they file a bug")
	}
}

// ---- criterion 18: the one-reader rule reaches the new page --------------------

// internal/availability/callsites_test.go bans a second normalized_events
// reader outside its two-file allowlist. That test would catch funnel.go too,
// but it names no reason a FUNNEL page would want the table — this one does, so
// the failure lands with its argument attached.
func TestFunnelNeverNamesNormalizedEvents(t *testing.T) {
	for _, rel := range []string{
		filepath.Join("internal", "dashboard", "funnel.go"),
		filepath.Join("internal", "dashboard", "sources.go"),
	} {
		if funnelMentionsOutsideComments(funnelRepoFile(t, rel), "normalized_events") {
			t.Errorf("%s names normalized_events. Criterion 18: the calendar appears on this page ONLY as a "+
				"sync-freshness row, read through availability.CalendarSyncStates, and the busy set is "+
				"never shown. Free/busy has ONE database-backed entry point and it refuses BEFORE it reads "+
				"the table; a second reader cannot tell a dead poller from an empty calendar", rel)
		}
	}
	if funnelMentionsOutsideComments(funnelTemplate(t), "normalized_events") {
		t.Errorf("templates/funnel.html names normalized_events")
	}
}

// ---- criterion 5: the calendar rule is REUSED, not re-spelled -----------------

// The specific failure this prevents: a page that says "green" while
// propose_slots refuses. That happens the moment the dashboard grows its own
// os.Getenv("AVAIL_MAX_SYNC_AGE") + ParseDuration, or its own freshness SQL over
// sync_runs for the calendar phase.
func TestFunnelUsesTheSharedCalendarSeams(t *testing.T) {
	src := funnelRepoFile(t, filepath.Join("internal", "dashboard", "funnel.go"))

	for _, want := range []string{"availability.CalendarSyncStates", "availability.NotReady", "tools.MaxCalendarSyncAge"} {
		if !strings.Contains(src, want) {
			t.Errorf("internal/dashboard/funnel.go never calls %s. Criterion 5: calendar freshness is the "+
				"system's own rule, not a second spelling of it — the same AVAIL_MAX_SYNC_AGE value and "+
				"the same predicate propose_slots uses", want)
		}
	}
	if funnelMentionsOutsideComments(src, `os.Getenv("AVAIL_MAX_SYNC_AGE")`) {
		t.Errorf("internal/dashboard/funnel.go parses AVAIL_MAX_SYNC_AGE itself. Rejected alternative, " +
			"explicitly: a second parse drifts, and the drift's symptom is a green page while propose_slots " +
			"refuses — which is worse than no page at all")
	}
	if funnelMentionsOutsideComments(src, "time.ParseDuration") {
		t.Errorf("internal/dashboard/funnel.go calls time.ParseDuration; the only duration this page reads " +
			"from the environment is AVAIL_MAX_SYNC_AGE and tools.MaxCalendarSyncAge owns that parse")
	}
}

// tools.MaxCalendarSyncAge is the seam the page and propose_slots share
// (SPEC "API / MCP tool changes" item 5). Its behaviour is pinned in
// internal/tools/maxcalendarsyncage_test.go; this asserts only that the
// dashboard can reach it and that it is not silently defaulting on a typo,
// because THAT is the direction that would make the page lie.
func TestMaxCalendarSyncAgeIsReachableAndFailsLoud(t *testing.T) {
	t.Setenv("AVAIL_MAX_SYNC_AGE", "")
	d, err := tools.MaxCalendarSyncAge()
	if err != nil || d != time.Hour {
		t.Fatalf("tools.MaxCalendarSyncAge() with the env unset = (%v, %v), want (1h, nil)", d, err)
	}
	t.Setenv("AVAIL_MAX_SYNC_AGE", "not-a-duration")
	if _, err := tools.MaxCalendarSyncAge(); err == nil {
		t.Fatalf("tools.MaxCalendarSyncAge() accepted a non-duration. A typo must not widen a safety " +
			"window, and on this page it must not turn a refusing calendar green")
	}
}

// ---- criterion 10: the lanes are referenced, never spelled --------------------

func TestFunnelReferencesTheLaneVarsNotTheirWorkerTypeLiterals(t *testing.T) {
	src := funnelRepoFile(t, filepath.Join("internal", "dashboard", "funnel.go"))

	for _, want := range []string{"classify.LanePersonal", "classify.LaneResidue"} {
		if !strings.Contains(src, want) {
			t.Errorf("internal/dashboard/funnel.go never references %s. Criterion 10: BOTH lanes render, "+
				"through those vars", want)
		}
	}
	for _, banned := range []string{`"classify_residue"`, `"classify"`} {
		if funnelMentionsOutsideComments(src, banned) {
			t.Errorf("internal/dashboard/funnel.go spells the worker_type %s as a literal. The two lanes' "+
				"worker_type values differ because both inbox filters key their NOT EXISTS on them (IK, "+
				"residue lane); a literal here is a second place that fact lives", banned)
		}
	}
}

// ---- criteria 11 + 16: no second copy of a fold that already exists -----------

// The latest-capture-decision predicate is capture's spelling, and the classify
// verdict fold is classify's. The dashboard adds neither.
func TestFunnelAddsNoSQLOverCaptureOrClassifyTables(t *testing.T) {
	src := funnelRepoFile(t, filepath.Join("internal", "dashboard", "funnel.go"))

	for _, table := range []string{"capture_decisions", "ai_extractions", "ai_runs"} {
		if funnelMentionsOutsideComments(src, table) {
			t.Errorf("internal/dashboard/funnel.go names %s in SQL. Criteria 11 and 16: those folds are "+
				"reused, not restated — capture.AttributionTrend owns the latest-decision lateral and "+
				"classify.Summarize owns the verdict query, the link-state fold and the skipped-run fold. "+
				"One spelling per fact is the repo's recurring defect and the reason those seams exist", table)
		}
	}
	for _, want := range []string{"capture.AttributionTrend", "classify.Summarize"} {
		if !strings.Contains(src, want) {
			t.Errorf("internal/dashboard/funnel.go never calls %s", want)
		}
	}
}

// ---- criterion 20: the nav link ----------------------------------------------

// Seven templates carry the shared nav line, and a page nobody can reach from
// the nav is a page nobody opens. Position is asserted too: "next to Sources",
// consistently, so the link is in the same place on every page.
func TestNavCarriesTheFunnelLinkNextToSources(t *testing.T) {
	files := []string{"deliveries.html", "sources.html", "tasks.html", "task.html",
		"briefs.html", "plans.html", "plan.html", "funnel.html"}
	navRe := regexp.MustCompile(`(?s)<nav>(.*?)</nav>`)
	linkRe := regexp.MustCompile(`href="(/[^"]*)"`)

	for _, name := range files {
		raw, err := templateFS.ReadFile("templates/" + name)
		if err != nil {
			t.Fatalf("read embedded %s: %v", name, err)
		}
		m := navRe.FindStringSubmatch(string(raw))
		if m == nil {
			t.Fatalf("POSITIVE CONTROL FAILED: templates/%s has no <nav> block; criterion 20 names it as "+
				"one of the files that carries one", name)
		}
		links := linkRe.FindAllStringSubmatch(m[1], -1)
		var order []string
		for _, l := range links {
			order = append(order, l[1])
		}
		iSources, iFunnel := -1, -1
		for i, href := range order {
			if href == "/sources" {
				iSources = i
			}
			if href == "/funnel" {
				iFunnel = i
			}
		}
		if iFunnel < 0 {
			t.Errorf("templates/%s nav has no /funnel link (criterion 20). Nav order was %v", name, order)
			continue
		}
		if iSources < 0 {
			t.Errorf("templates/%s nav lost its /sources link; the funnel page ADDS to the nav, it does "+
				"not replace /sources", name)
			continue
		}
		if iFunnel-iSources != 1 && iSources-iFunnel != 1 {
			t.Errorf("templates/%s puts /funnel at position %d and /sources at %d — criterion 20 wants ONE "+
				"consistent position, next to Sources. Nav order was %v", name, iFunnel, iSources, order)
		}
	}
}

// ---- criterion 23: section order ---------------------------------------------

// Q2-a's accepted cost is that the classify block dominates the page, and
// ordering is the ONLY mitigation taken. So the order is the contract.
func TestFunnelTemplate_SectionOrderIsHealthIntakeCaptureClassify(t *testing.T) {
	tmpl := funnelTemplate(t)
	headingRe := regexp.MustCompile(`(?is)<h2[^>]*>(.*?)</h2>`)
	var headings []string
	for _, m := range headingRe.FindAllStringSubmatch(tmpl, -1) {
		headings = append(headings, strings.ToLower(strings.Join(strings.Fields(m[1]), " ")))
	}
	if len(headings) < 4 {
		t.Fatalf("templates/funnel.html has %d <h2> headings (%v); criterion 23 wants four sections",
			len(headings), headings)
	}

	want := []string{"health", "intake", "capture", "classif"}
	at := make([]int, len(want))
	for i, keyword := range want {
		at[i] = -1
		for j, h := range headings {
			if strings.Contains(h, keyword) {
				at[i] = j
				break
			}
		}
		if at[i] < 0 {
			t.Fatalf("no <h2> heading mentions %q; headings were %v", keyword, headings)
		}
	}
	for i := 1; i < len(at); i++ {
		if at[i] <= at[i-1] {
			t.Fatalf("section order is wrong: headings %v. Criterion 23 pins health -> intake -> capture -> "+
				"classify, with the classify block LAST — it is the biggest block on the page and ordering "+
				"is the only mitigation Q2-a took", headings)
		}
	}
}

// ---- the page states the things a number cannot carry -------------------------

// Criteria 5, 6, 9, 14 and 15 each require the page to SAY something, because
// each is a place where the number alone is misleading. A caption is not
// decoration here: it is the difference between a reader trusting a threshold
// that gates nothing and a reader inventing an alarm from it.
func TestFunnelTemplate_CarriesTheCaptionsTheNumbersNeed(t *testing.T) {
	tmpl := funnelTemplate(t)
	lower := strings.ToLower(tmpl)

	cases := []struct {
		criterion string
		needles   []string // ALL must appear
		why       string
	}{
		{
			criterion: "5",
			needles:   []string{"avail_max_sync_age", "propose_slots", "refus"},
			why: "the calendar section prints the effective AVAIL_MAX_SYNC_AGE and states that propose_slots " +
				"refuses while any in-scope account is stale. A page that says green while the tool refuses " +
				"is the specific failure this criterion exists to prevent",
		},
		{
			criterion: "6",
			needles:   []string{"3h", "gate"},
			why: "every non-calendar phase is judged against a DISPLAY-ONLY 3h threshold and the page says " +
				"so. An unlabelled threshold on an ops page is read as a contract",
		},
		{
			criterion: "9",
			needles:   []string{"ingested_at", "created_at", "sent_at"},
			why: "both intake axes use the pipeline clock. sent_at is the provider's clock, answers a " +
				"different question (when the mail was written), and a backfill would scatter it across years",
		},
		{
			criterion: "14",
			needles:   []string{"not yet evaluated", "residue"},
			why: "the third attribution state is LABELLED and the page says it is not the residue. " +
				"Conflating unseen with unmatched is the named capture trap",
		},
		{
			criterion: "15",
			needles:   []string{"inbound", "direction"},
			why: "the attribution rows count inbound messages only, and the page says why: capture filters " +
				"direction='inbound' (that line IS invariant 5), so an outbound message can never carry a " +
				"decision on any pass in any mode — absent-because-impossible, not pending",
		},
		{
			criterion: "the upwork double-run clause (Data model)",
			needles:   []string{"upwork", "twice"},
			why: "one upworkcrm invocation writes TWO sync_runs rows (ingest + normalize), so the run COUNT " +
				"for upwork is double the number of CronJob ticks. The page says so in one clause rather " +
				"than silently presenting an inflated number",
		},
	}
	for _, tc := range cases {
		for _, needle := range tc.needles {
			if !strings.Contains(lower, needle) {
				t.Errorf("templates/funnel.html never mentions %q (criterion %s): %s", needle, tc.criterion, tc.why)
			}
		}
	}

	// The (none) phase bucket is a real, expected thing and the page must not
	// present it as a defect: jira and upworkcrm write NO phase key at all.
	if !strings.Contains(lower, "(none)") {
		t.Errorf("templates/funnel.html never mentions the (none) phase bucket. sync_runs has no phase " +
			"column — the phase is a key in stats, and jira/upworkcrm insert the default '{}', so (none) " +
			"is a real bucket, not a bug")
	}
}
