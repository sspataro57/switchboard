//go:build integration

package dashboard_test

// watch-json (SWT-101, docs/tickets/watch-json_SPEC.md) criteria 14-19 against a
// REAL database and the REAL dashboard.Server (dev-login auth for /tasks, the
// bearer token for /watch.json). Build-tagged `integration` AND env-gated on
// DATABASE_URL (dashGuard: never 192.168.50.49). NO LLM, NO network, NO broker.
// Run it ONLY in the branch-owned database (the IK compose landmine):
//
//	psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c 'CREATE DATABASE ops_watch_json'
//	make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_watch_json?sslmode=disable'
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_watch_json?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run 'TestWatch' ./internal/dashboard/
//
// Reuses dashGuard / dashPool / get / snippet (dashboard_integration_test.go),
// bdInsID / bdCount (board_dismiss_integration_test.go) and depElement
// (board_departures_integration_test.go), which reads the sign header exactly
// as TestBoardDepartures_Integration_TalliesAndFooter does.
//
// CLEANUP PACT: owns project itest-wj-proj and every session name itest-wj-*;
// deletes the demo_mode ops_flags row it may create. FK-ordered, before AND in
// t.Cleanup. Session markers are written by FIXTURE SQL (the SPEC's criterion
// 14), aged in SQL, so the lease and "today" are on the DB clock.
//
// The watch is always called WITHOUT a cookie jar: the courier has no session,
// so a route wrapped in s.auth.Require answers 302 here (M4), never a 200 that a
// logged-in client would get.
//
// THE ORACLE. Criterion 15 compares the watch with a bare /tasks header in the
// same DB state (exact), and with a hand count of the fixture. The hand count is
// asserted as a DELTA over the header read before seeding: the branch DB holds
// only test fixtures, but another package's suite may leave rows behind, and the
// delta is exact either way. When the DB is clean the baseline is all zeros and
// the delta IS the absolute count.
//
// EXPECTED RED: the package's test binary does not compile until watch.go
// exists (SetWatchToken is undefined). Once it compiles and before the route
// exists, /watch.json falls through to GET / and answers 302 to the login page,
// so every test fails at its first status check.
//
// MUTATIONS THAT MUST TURN THIS FILE RED: M4 (302), M5 (s-stale listed), M7 (all
// zeros; demo), M8 (the query string changes the reply), M9 (no no-store), and a
// state_unix column replaced by a literal (the since check reads the column back).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/dashboard"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	wjSlug   = "itest-wj-proj"
	wjClient = "itest-wj-client"
	wjOther  = "itest-wj-other"
	wjToken  = "wj0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd" // 64 bytes

	wjWait  = "itest-wj-s-wait"
	wjWork  = "itest-wj-s-work"
	wjStale = "itest-wj-s-stale"
)

// wjPayload is the SPEC's whole reply; DisallowUnknownFields holds it to exactly
// these six keys (criterion 14), and the nested objects to session + since.
type wjPayload struct {
	NeedYou   *int     `json:"need_you"`
	InFlight  *int     `json:"in_flight"`
	Incoming  *int     `json:"incoming"`
	DoneToday *int     `json:"done_today"`
	Waiting   []wjSess `json:"waiting"`
	Working   []wjSess `json:"working"`
}

type wjSess struct {
	Session string `json:"session"`
	Since   int64  `json:"since"`
}

func (p wjPayload) counts() [4]int { return [4]int{*p.NeedYou, *p.InFlight, *p.Incoming, *p.DoneToday} }

func wjCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const projs = `(SELECT id FROM projects WHERE slug = '` + wjSlug + `')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	for _, q := range []string{
		`DELETE FROM ops_flags WHERE name = 'demo_mode'`,
		`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + `)`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM external_refs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`UPDATE tasks SET parent_id = NULL WHERE id IN ` + tasksOf + ` AND parent_id IS NOT NULL`,
		`DELETE FROM tasks WHERE id IN ` + tasksOf,
		`DELETE FROM projects WHERE slug = '` + wjSlug + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func wjStart(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	wjCleanup(t, ctx, pool)
	t.Cleanup(func() {
		wjCleanup(t, ctx, pool)
		pool.Close()
	})
	return ctx, pool
}

// wjServer is newDashServer with the token wired the way cmd/dashboard wires it:
// SetWatchToken BEFORE Handler(). It returns the base URL, a logged-in client for
// /tasks and a cookie-less, non-following client for /watch.json.
func wjServer(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (string, *http.Client, *http.Client) {
	t.Helper()
	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	ex := executor.New(reg, policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...)), audit.NewPGStore(pool))
	auth, err := dashboard.NewAuth(ctx, "", "", "", "") // issuer "" -> dev mode
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	srv, err := dashboard.NewServer(pool, ex, auth)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetWatchToken(wjToken + "\n") // a Secret value's trailing newline (D3: trimmed)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	board := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	if _, err := board.Get(ts.URL + "/dev/login?user=salvo"); err != nil {
		t.Fatalf("dev login: %v", err)
	}
	courier := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return ts.URL, board, courier
}

// wjCall is one courier request.
func wjCall(t *testing.T, c *http.Client, method, u string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+wjToken)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, u, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// wjWatch GETs /watch.json(?query), requires a 200 and decodes it strictly.
func wjWatch(t *testing.T, c *http.Client, base, query string) (wjPayload, []byte, http.Header) {
	t.Helper()
	u := base + "/watch.json"
	if query != "" {
		u += "?" + query
	}
	resp, body := wjCall(t, c, http.MethodGet, u)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s with the token = %d (Location %q), want 200. A 302 is the route behind s.auth.Require or not "+
			"registered (M4)\n%s", u, resp.StatusCode, resp.Header.Get("Location"), snippet(string(body)))
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		t.Fatalf("the 200 body is not a JSON object: %v\n%s", err, snippet(string(body)))
	}
	if len(keys) != 6 {
		t.Errorf("the reply has %d keys, want exactly the six (need_you, in_flight, incoming, done_today, waiting, "+
			"working) (criterion 14): %s", len(keys), body)
	}
	for _, k := range []string{"waiting", "working"} {
		if string(bytes.TrimSpace(keys[k])) == "null" {
			t.Errorf("%s is null, want an array (criterion 10: never null)", k)
		}
	}
	var p wjPayload
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		t.Fatalf("strict decode (criterion 14: exactly the six keys; each list entry exactly session + since): %v\n%s", err, body)
	}
	if p.NeedYou == nil || p.InFlight == nil || p.Incoming == nil || p.DoneToday == nil {
		t.Fatalf("a count is missing from %s (criterion 14)", body)
	}
	return p, body, resp.Header
}

// wjHeader is the bare /tasks sign header's five numbers, read the way
// TestBoardDepartures_Integration_TalliesAndFooter reads them.
func wjHeader(t *testing.T, board *http.Client, base string) []int {
	t.Helper()
	code, body := get(t, board, base+"/tasks")
	if code != http.StatusOK {
		t.Fatalf("GET /tasks = %d\n%s", code, snippet(body))
	}
	hi := strings.Index(body, `<header class="sign">`)
	if hi < 0 {
		t.Fatalf("the board has no <header class=\"sign\">\n%s", snippet(body))
	}
	he, ok := depElement(body, hi, "header")
	if !ok {
		t.Fatalf("the sign <header> is never closed")
	}
	var got []int
	for _, m := range regexp.MustCompile(`<b[^>]*>(\d+)</b>`).FindAllStringSubmatch(body[hi:he], -1) {
		n, _ := strconv.Atoi(m[1])
		got = append(got, n)
	}
	if len(got) != 5 {
		t.Fatalf("the sign header carries %d numbers, want 5 (need you, in flight, incoming, queued, done today): %v", len(got), got)
	}
	return got
}

// headerFour is the header's first, second, third and fifth numbers: the four
// the watch sends (criterion 15; Queued is not sent).
func headerFour(h []int) [4]int { return [4]int{h[0], h[1], h[2], h[4]} }

// wjSeed is one task per board section plus the four session markers of
// criterion 14. Every task hangs off parent P (holding), so one demo_hidden on P
// hides the whole fixture in demo mode (criterion 18).
type wjSeed struct {
	p                                  int64 // holding, the fixture parent
	wait, nullWait, work, stale        int64 // the session markers
	grey, fb, worker, incoming, queued int64
	doneToday, closedOld               int64
}

// wjOracle is the fixture's hand count: need you, in flight, incoming, done today.
//
//	need you   : wait, nullWait (red, session), fb (red, worker), grey (blocked)   = 4
//	in flight  : work (yellow), stale (stale ring), worker (claude in_progress)    = 3
//	incoming   : incoming (a human task with a github PR ref)                       = 1
//	done today : doneToday                                                          = 1
//	(queued    : queued, p — not sent; closedOld is not on the default board)
var wjOracle = [4]int{4, 3, 1, 1}

func seedWatch(t *testing.T, ctx context.Context, pool *pgxpool.Pool) wjSeed {
	t.Helper()
	proj := bdInsID(t, ctx, pool, `INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		VALUES ($1,$1,$2,'manual','dashboard','/tmp/itest-wj','any') RETURNING id`, wjSlug, wjClient)
	var s wjSeed
	s.p = bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, title, body, assignee_type, status, priority)
		VALUES ($1,'WJ-P parent','','human','holding',0) RETURNING id`, proj)
	task := func(title, assignee, status string, priority int) int64 {
		return bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, parent_id, title, body, assignee_type, status, priority)
			VALUES ($1,$2,$3,'',$4,$5,$6) RETURNING id`, proj, s.p, title, assignee, status, priority)
	}
	marker := func(id int64, state, age string, session any) {
		if _, err := pool.Exec(ctx, `UPDATE tasks SET working_state = $2, working_state_at = now() - $3::interval,
			working_session = $4 WHERE id = $1`, id, state, age, session); err != nil {
			t.Fatalf("marker on %d: %v", id, err)
		}
	}
	s.wait = task("WJ-W1 waiting", "human", "ready", 0)
	marker(s.wait, "needs_input", "10 minutes", wjWait)
	s.nullWait = task("WJ-W2 waiting, no name", "human", "ready", 0)
	marker(s.nullWait, "needs_input", "5 minutes", nil)
	s.work = task("WJ-W3 working", "human", "ready", 0)
	marker(s.work, "working", "20 minutes", wjWork)
	s.stale = task("WJ-W4 stale lease", "human", "ready", 0)
	marker(s.stale, "working", "3 hours", wjStale) // > tools.WorkingLease (2 h)
	s.grey = task("WJ-G1 blocked on a dependency", "human", "blocked", 0)
	s.fb = task("WJ-F1 worker parked", "claude", "needs_feedback", 0)
	s.worker = task("WJ-C1 worker running", "claude", "in_progress", 0)
	s.incoming = task("WJ-I1 pr review", "human", "ready", 0)
	if _, err := pool.Exec(ctx,
		`INSERT INTO external_refs (task_id, system, external_key) VALUES ($1,'github','itest-wj/repo#1')`, s.incoming); err != nil {
		t.Fatalf("seed I1's github ref: %v", err)
	}
	s.queued = task("WJ-Q1 queued", "human", "ready", 5)
	s.doneToday = bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, parent_id, title, body, assignee_type, status, priority, closed_at)
		VALUES ($1,$2,'WJ-D1 done','','human','closed',0, now()) RETURNING id`, proj, s.p)
	s.closedOld = bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, parent_id, title, body, assignee_type, status, priority, closed_at)
		VALUES ($1,$2,'WJ-X1 closed long ago','','human','closed',0, now() - interval '3 days') RETURNING id`, proj, s.p)
	return s
}

func wjHas(l []wjSess, name string) (wjSess, bool) {
	for _, s := range l {
		if s.Session == name {
			return s, true
		}
	}
	return wjSess{}, false
}

// ---- criteria 14, 15, 16, 19 --------------------------------------------------------

func TestWatch_Integration_CountsEqualTheBoardHeader(t *testing.T) {
	ctx, pool := wjStart(t)
	base, board, courier := wjServer(t, ctx, pool)
	baseline := headerFour(wjHeader(t, board, base))
	s := seedWatch(t, ctx, pool)

	p, _, hdr := wjWatch(t, courier, base, "")
	// Criterion 15: the same computation as a bare /tasks in the same DB state.
	head := headerFour(wjHeader(t, board, base))
	if p.counts() != head {
		t.Errorf("watch counts (need_you, in_flight, incoming, done_today) = %v, bare /tasks header = %v. Criterion "+
			"15 / D1: both are boardTallies over the same boardView (M1, M7)", p.counts(), head)
	}
	var delta [4]int
	for i := range delta {
		delta[i] = p.counts()[i] - baseline[i]
	}
	if delta != wjOracle {
		t.Errorf("the fixture moved the watch counts by %v (from %v), want the hand count %v (criterion 15's oracle; "+
			"see wjOracle)", delta, baseline, wjOracle)
	}
	if baseline != [4]int{} {
		t.Logf("NOTE: the branch DB was not empty before seeding (header %v); the oracle was checked as a delta", baseline)
	}

	// Criterion 16: who is listed.
	w, ok := wjHas(p.Waiting, wjWait)
	if !ok {
		t.Errorf("waiting %+v does not list %s (criterion 16)", p.Waiting, wjWait)
	}
	for _, e := range p.Waiting {
		if e.Session == "" || strings.Contains(e.Session, "unknown") {
			t.Errorf("waiting lists %+v; criterion 16 / D2: the NULL-session marker is counted, never listed (M12)", e)
		}
	}
	if _, ok := wjHas(p.Working, wjWork); !ok {
		t.Errorf("working %+v does not list %s (criterion 16)", p.Working, wjWork)
	}
	if _, ok := wjHas(p.Working, wjStale); ok {
		t.Errorf("working lists %s, whose signal is 3 h old; criterion 16 / D2: a stale lease is not yellow (M5)", wjStale)
	}
	if _, ok := wjHas(p.Waiting, wjStale); ok {
		t.Errorf("waiting lists %s (criterion 16)", wjStale)
	}
	// "Test the column, not the fixture": since is read back from the row.
	want := int64(bdCount(t, ctx, pool, `SELECT EXTRACT(EPOCH FROM working_state_at)::bigint FROM tasks WHERE id = $1`, s.wait))
	if ok && w.Since != want {
		t.Errorf("%s's since = %d, want %d = EXTRACT(EPOCH FROM working_state_at)::bigint (criterion 16 / D2: Unix "+
			"seconds of the stored signal)", wjWait, w.Since, want)
	}
	if wk, ok := wjHas(p.Working, wjWork); ok {
		wantW := int64(bdCount(t, ctx, pool, `SELECT EXTRACT(EPOCH FROM working_state_at)::bigint FROM tasks WHERE id = $1`, s.work))
		if wk.Since != wantW {
			t.Errorf("%s's since = %d, want %d (criterion 16)", wjWork, wk.Since, wantW)
		}
	}

	// Criterion 19: the headers, and HEAD.
	if got := hdr.Get("Cache-Control"); got != "no-store" {
		t.Errorf("the 200's Cache-Control = %q, want no-store (criterion 19, M9)", got)
	}
	if got := hdr.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("the 200's Content-Type = %q, want application/json (criterion 19)", got)
	}
	resp, body := wjCall(t, courier, http.MethodHead, base+"/watch.json")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("HEAD /watch.json = %d, want 200 (criterion 19 / D5: the GET pattern serves HEAD)", resp.StatusCode)
	}
	if len(body) != 0 {
		t.Errorf("HEAD /watch.json returned a %d-byte body, want none (criterion 19)", len(body))
	}

	// The gate still stands on the real server: no token, no data.
	resp, body = func() (*http.Response, []byte) {
		r, err := courier.Get(base + "/watch.json")
		if err != nil {
			t.Fatalf("GET without a token: %v", err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r, b
	}()
	if resp.StatusCode != http.StatusUnauthorized || string(body) != "unauthorized\n" {
		t.Errorf("GET /watch.json with no token = %d %q, want 401 \"unauthorized\\n\" (criterion 2 on the real mux)",
			resp.StatusCode, body)
	}
}

// ---- criterion 17: a query string never changes the reply --------------------------

func TestWatch_Integration_QueryStringIsIgnored(t *testing.T) {
	ctx, pool := wjStart(t)
	base, _, courier := wjServer(t, ctx, pool)
	seedWatch(t, ctx, pool)
	_, plain, _ := wjWatch(t, courier, base, "")
	for _, q := range []string{
		url.Values{"project": {wjOther}, "status": {"closed"}}.Encode(),
		url.Values{"project": {wjSlug}, "assignee_type": {"claude"}}.Encode(),
		"refresh=on&subproject=nope",
	} {
		_, got, _ := wjWatch(t, courier, base, q)
		if !bytes.Equal(got, plain) {
			t.Errorf("GET /watch.json?%s = %s\nbare                  = %s\nCriterion 17 / D1: the watch always renders the "+
				"unfiltered default board; boardView gets a clone with RawQuery = \"\" (M8)", q, got, plain)
		}
	}
}

// ---- ordering and dedup against the real read (criteria 9, 10 end to end) ---------------

func TestWatch_Integration_OrderAndDedup(t *testing.T) {
	ctx, pool := wjStart(t)
	base, _, courier := wjServer(t, ctx, pool)
	s := seedWatch(t, ctx, pool)
	proj := int64(bdCount(t, ctx, pool, `SELECT id FROM projects WHERE slug = $1`, wjSlug))
	add := func(title, state, age, session string) int64 {
		id := bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, parent_id, title, body, assignee_type, status, priority,
			working_state, working_state_at, working_session)
			VALUES ($1,$2,$3,'','human','ready',0,$4, now() - $5::interval, $6) RETURNING id`, proj, s.p, title, state, age, session)
		return id
	}
	// An OLDER waiting session than s-wait (10 min), a second task on s-wait that
	// is newer (1 min, must not replace the 10-min since), and a name that is
	// waiting on one task and working on another (waiting wins).
	add("WJ-O1 old wait", "needs_input", "40 minutes", "itest-wj-s-old")
	add("WJ-O2 s-wait again", "needs_input", "1 minute", wjWait)
	add("WJ-O3 both, waiting", "needs_input", "15 minutes", "itest-wj-s-both")
	add("WJ-O4 both, working", "working", "50 minutes", "itest-wj-s-both")

	p, _, _ := wjWatch(t, courier, base, "")
	var mine []wjSess
	for _, e := range p.Waiting {
		if strings.HasPrefix(e.Session, "itest-wj-") {
			mine = append(mine, e)
		}
	}
	var names []string
	for _, e := range mine {
		names = append(names, e.Session)
	}
	if want := []string{"itest-wj-s-old", "itest-wj-s-both", wjWait}; strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("waiting (this fixture's names) = %v, want %v: one entry per name, oldest since first (criteria 9, 10)",
			names, want)
	}
	for i := 1; i < len(p.Waiting); i++ {
		if p.Waiting[i-1].Since > p.Waiting[i].Since {
			t.Errorf("waiting is not since-ascending at %d: %+v (criterion 10, M10)", i, p.Waiting)
		}
	}
	for i := 1; i < len(p.Working); i++ {
		if p.Working[i-1].Since > p.Working[i].Since {
			t.Errorf("working is not since-ascending at %d: %+v (criterion 10, M10)", i, p.Working)
		}
	}
	oldest := int64(bdCount(t, ctx, pool, `SELECT EXTRACT(EPOCH FROM working_state_at)::bigint FROM tasks WHERE id = $1`, s.wait))
	if e, ok := wjHas(p.Waiting, wjWait); ok && e.Since != oldest {
		t.Errorf("%s waits on two tasks; since = %d, want the OLDER one's %d (criterion 9, M6)", wjWait, e.Since, oldest)
	}
	if _, ok := wjHas(p.Working, "itest-wj-s-both"); ok {
		t.Errorf("itest-wj-s-both is listed in working as well as waiting; criterion 9: waiting wins (M6)")
	}
}

// ---- criterion 18: demo mode is OFF for the watch ------------------------------------

func TestWatch_Integration_DemoModeNeverFiltersTheWatch(t *testing.T) {
	ctx, pool := wjStart(t)
	base, board, courier := wjServer(t, ctx, pool)
	s := seedWatch(t, ctx, pool)
	realP, realBody, _ := wjWatch(t, courier, base, "")
	if realP.counts() == [4]int{} {
		t.Fatalf("CONTROL: the watch shows all zeros before demo mode; the fixture is not on the board")
	}

	setFlag := func(v string) {
		if _, err := pool.Exec(ctx, `INSERT INTO ops_flags (name, value) VALUES ('demo_mode', $1::jsonb)
			ON CONFLICT (name) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, v); err != nil {
			t.Fatalf("set demo_mode %s: %v", v, err)
		}
	}
	setFlag(`{"on": true, "projects": []}`)
	if _, err := pool.Exec(ctx, `UPDATE tasks SET demo_hidden = true WHERE id = $1`, s.p); err != nil {
		t.Fatalf("mark the fixture parent demo_hidden: %v", err)
	}

	for _, tc := range []struct{ name, flag string }{
		{"demo on, nothing allowlisted, fixture parent hidden", ""},
		{"an undecodable demo_mode value (fails closed on the dashboard)", `"not an object"`},
	} {
		if tc.flag != "" {
			setFlag(tc.flag)
		}
		// The dashboard is filtered: zero rows.
		h := wjHeader(t, board, base)
		if h[0]+h[1]+h[2]+h[3]+h[4] != 0 {
			t.Errorf("%s: CONTROL: a logged-in /tasks header reads %v, want all zeros (demo mode hides everything)", tc.name, h)
		}
		if _, body := get(t, board, base+"/tasks"); strings.Contains(body, "WJ-W1 waiting") {
			t.Errorf("%s: CONTROL: /tasks still shows a fixture row in demo mode", tc.name)
		}
		// The watch is not: 200, same numbers, same names — never 503.
		resp, _ := wjCall(t, courier, http.MethodGet, base+"/watch.json")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: /watch.json = %d, want 200 (criterion 18 / D4: the watch never reads the flag, so it has no "+
				"503 path)", tc.name, resp.StatusCode)
			continue
		}
		got, gotBody, _ := wjWatch(t, courier, base, "")
		if got.counts() != realP.counts() {
			t.Errorf("%s: watch counts %v, want the real board's %v (criterion 18 / D4, M7)", tc.name, got.counts(), realP.counts())
		}
		if !bytes.Equal(gotBody, realBody) {
			t.Errorf("%s: watch reply %s\nwant the demo-off reply %s (criterion 18: same counts AND lists)", tc.name, gotBody, realBody)
		}
	}
}
