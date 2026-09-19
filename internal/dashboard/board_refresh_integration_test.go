//go:build integration

package dashboard_test

// SWT-52 (docs/tickets/board-status-lights_SPEC.md) D15, criteria 32 (end to
// end) and 34-37: the board's opt-in auto-refresh against a real database, the
// REAL dashboard.Server (dev-login auth) and the real executor. Build-tagged
// `integration` AND env-gated on DATABASE_URL. NO LLM, NO network.
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_isolights?sslmode=disable' \
//	  TZ=UTC go test -tags integration -p 1 -count=1 -run BoardRefresh ./internal/dashboard/
//
// Reuses dashGuard / newDashServer / get / snippet (dashboard_integration_test.go)
// and bdInsID / bdCount (board_dismiss_integration_test.go). Own prefix
// `itest-refresh-%`; FK-ordered cleanup (audit rows by task_id first — the
// Done POST fills audit_events.task_id).
//
// GREENFIELD NOTE — EXPECTED RED: no toggle, indicator, script or refresh key
// exists, so every rendering assertion fails and boardBack drops refresh.

import (
	"context"
	"html"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/dashboard"
)

const (
	rfSlug   = "itest-refresh-proj"
	rfClient = "itest-refresh-client"
)

func cleanupRefresh(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug = '` + rfSlug + `'))`
	for _, q := range []string{
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + `)`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug = '` + rfSlug + `')`,
		`DELETE FROM projects WHERE slug = '` + rfSlug + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func rfSeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (human, claude int64) {
	t.Helper()
	proj := bdInsID(t, ctx, pool,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,$2,'manual','dashboard','/tmp/itest-refresh','any') RETURNING id`, rfSlug, rfClient)
	human = bdInsID(t, ctx, pool,
		`INSERT INTO tasks (project_id, title, assignee_type, status, priority) VALUES ($1,'REFRESH human task','human','ready',1) RETURNING id`, proj)
	claude = bdInsID(t, ctx, pool,
		`INSERT INTO tasks (project_id, title, assignee_type, status) VALUES ($1,'REFRESH claude task','claude','ready') RETURNING id`, proj)
	return human, claude
}

// noFollow is a client sharing c's session that does NOT follow redirects.
func noFollow(c *http.Client) *http.Client {
	return &http.Client{Jar: c.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func attr(t *testing.T, body, re string) string {
	t.Helper()
	m := regexp.MustCompile(re).FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return html.UnescapeString(m[1])
}

// ---- criterion 37: rendering ------------------------------------------------------

func TestBoardRefresh_Integration_Rendering(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupRefresh(t, ctx, pool)
	defer cleanupRefresh(t, ctx, pool)
	rfSeed(t, ctx, pool)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	code, on := get(t, client, ts.URL+"/tasks?project="+rfSlug+"&refresh=on")
	if code != http.StatusOK {
		t.Fatalf("GET refresh=on = %d\n%s", code, snippet(on))
	}
	if !regexp.MustCompile(`id="auto-refresh"[^>]*>[^<]*\b\d{2}:\d{2}:\d{2}\b`).MatchString(on) {
		t.Errorf("refresh=on page lacks the indicator id=\"auto-refresh\" with an HH:MM:SS time (criterion 37)\n%s", snippet(on))
	}
	if n := strings.Count(on, "<script"); n != 1 {
		t.Errorf("refresh=on page has %d <script, want exactly 1 (criterion 37)", n)
	}
	toggle := attr(t, on, `id="auto-refresh-toggle" href="([^"]*)"`)
	if tu, err := url.Parse(toggle); err != nil || toggle == "" || tu.Query().Get("project") != rfSlug || tu.Query().Has("refresh") {
		t.Errorf("refresh=on toggle href = %q, want project=%s and no refresh (criterion 37)", toggle, rfSlug)
	}
	// filter form + the human row's Dismiss and Done + the claude row's Dismiss.
	if n := strings.Count(on, `name="refresh" value="on"`); n < 4 {
		t.Errorf("refresh=on page has %d hidden refresh inputs valued on, want >= 4 (filter form, and every row's "+
			"Dismiss and Done forms) — criterion 37", n)
	}

	_, off := get(t, client, ts.URL+"/tasks?project="+rfSlug)
	// SWT-67 B12: the one script renders on every board page (the clock, the
	// paging and FULL need it); the reload loop arms only from data-refresh="on".
	if !strings.Contains(off, `data-refresh=""`) || strings.Contains(off, `data-refresh="on"`) {
		t.Errorf("the plain board's script is armed; data-refresh is \"on\" only when refresh=on (criterion 37, B12)")
	}
	if !strings.Contains(on, `data-refresh="on"`) {
		t.Errorf("the refresh=on board's script does not carry data-refresh=\"on\" (B12)")
	}
	if strings.Contains(off, `id="auto-refresh"`) {
		t.Errorf("the plain board renders the indicator (criterion 37)")
	}
	toggle = attr(t, off, `id="auto-refresh-toggle" href="([^"]*)"`)
	if tu, err := url.Parse(toggle); err != nil || tu.Query().Get("project") != rfSlug || tu.Query().Get("refresh") != "on" {
		t.Errorf("plain toggle href = %q, want project=%s&refresh=on (criterion 37)", toggle, rfSlug)
	}
	// refresh=1 means OFF (D15).
	if _, one := get(t, client, ts.URL+"/tasks?project="+rfSlug+"&refresh=1"); strings.Contains(one, `data-refresh="on"`) {
		t.Errorf("refresh=1 turned auto-refresh on; only refresh=on counts (criterion 30)")
	}
}

// ---- criteria 32 and 35: the five-key redirect, the flash shown once ----------------

func TestBoardRefresh_Integration_DoneKeepsRefreshAndShowsFlashOnce(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupRefresh(t, ctx, pool)
	defer cleanupRefresh(t, ctx, pool)
	human, claude := rfSeed(t, ctx, pool)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()
	nf := noFollow(client)

	post := func(path string, form url.Values) *url.URL {
		t.Helper()
		resp, err := nf.PostForm(ts.URL+path, form)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST %s = %d, want 303", path, resp.StatusCode)
		}
		loc, err := url.Parse(resp.Header.Get("Location"))
		if err != nil {
			t.Fatalf("Location %q: %v", resp.Header.Get("Location"), err)
		}
		return loc
	}
	allowed := map[string]bool{"project": true, "status": true, "assignee_type": true, "subproject": true, "refresh": true, "flash": true}

	loc := post("/tasks/"+strconv.FormatInt(human, 10)+"/close",
		url.Values{"project": {rfSlug}, "refresh": {"on"}, "note": {"shipped"}, "next": {"//evil.example"}})
	q := loc.Query()
	if q.Get("flash") != "task_close ok" || q.Get("refresh") != "on" || q.Get("project") != rfSlug {
		t.Errorf("Done's 303 Location = %s, want flash=task_close ok, refresh=on and project=%s (criteria 32, 35)", loc, rfSlug)
	}
	for k := range q {
		if !allowed[k] {
			t.Errorf("Done's Location carries %q; boardBack rebuilds boardKeys only, plus the flash (criterion 32)", k)
		}
	}

	// The landed page: the flash shows, and the reload URL drops it.
	_, page := get(t, client, ts.URL+loc.RequestURI())
	if !strings.Contains(page, "task_close ok") {
		t.Errorf("the landed page does not show the flash once")
	}
	reload := attr(t, page, `data-reload="([^"]*)"`)
	ru, err := url.Parse(reload)
	if err != nil || reload == "" || ru.Query().Get("refresh") != "on" || ru.Query().Get("project") != rfSlug || ru.Query().Has("flash") {
		t.Errorf("data-reload = %q, want refresh=on and project=%s and NO flash — criterion 35: the flash shows once", reload, rfSlug)
	}

	// Dismiss rebuilds the same five keys.
	loc = post("/tasks/"+strconv.FormatInt(claude, 10)+"/dismiss",
		url.Values{"project": {rfSlug}, "refresh": {"on"}, "reason_code": {"duplicate"}})
	if loc.Query().Get("refresh") != "on" || loc.Query().Get("flash") != "task_dismiss ok" {
		t.Errorf("Dismiss's 303 Location = %s, want refresh=on and flash=task_dismiss ok (criterion 32)", loc)
	}
}

// ---- criterion 36: auth during refresh ----------------------------------------------

func TestBoardRefresh_Integration_LoginKeepsRefresh(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	ts, _ := newDashServer(t, ctx, pool)
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	anon := noFollow(&http.Client{Jar: jar})
	resp, err := anon.Get(ts.URL + "/tasks?project=x&refresh=on")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("unauthenticated GET = %d, want 302 to the login page", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("Location: %v", err)
	}
	if next := loc.Query().Get("next"); next != "/tasks?project=x&refresh=on" {
		t.Errorf("login next = %q, want /tasks?project=x&refresh=on (criterion 36)", next)
	}
	resp, err = anon.Get(ts.URL + loc.RequestURI())
	if err != nil {
		t.Fatalf("GET login: %v", err)
	}
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		b.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	resp.Body.Close()
	if strings.Contains(b.String(), "<script") {
		t.Errorf("the login page contains a <script: a reload landing there must not loop (criterion 36)")
	}
}

// ---- criterion 34: one refresh is exactly one ordinary render -------------------------

type countTracer struct {
	mu   sync.Mutex
	sqls []string
}

func (c *countTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	c.mu.Lock()
	c.sqls = append(c.sqls, d.SQL)
	c.mu.Unlock()
	return ctx
}
func (c *countTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *countTracer) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.sqls
	c.sqls = nil
	return out
}

func TestBoardRefresh_Integration_NoExtraQueries(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	plainPool := dashPool(t, ctx)
	defer plainPool.Close()
	cleanupRefresh(t, ctx, plainPool)
	defer cleanupRefresh(t, ctx, plainPool)
	rfSeed(t, ctx, plainPool)

	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	tr := &countTracer{}
	cfg.ConnConfig.Tracer = tr
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	defer pool.Close()
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	get(t, client, ts.URL+"/tasks?project="+rfSlug) // warm-up
	tr.take()
	get(t, client, ts.URL+"/tasks?project="+rfSlug)
	plain := tr.take()
	code, body := get(t, client, ts.URL+"/tasks?project="+rfSlug+"&refresh=on")
	refresh := tr.take()
	if code != http.StatusOK {
		t.Fatalf("GET refresh=on = %d\n%s", code, snippet(body))
	}
	if len(plain) == 0 {
		t.Fatalf("POSITIVE CONTROL FAILED: the tracer saw no statement for a plain render")
	}
	if len(refresh) != len(plain) {
		t.Errorf("a refresh render issued %d statements, a plain render %d; criterion 34: one refresh is exactly one "+
			"ordinary render\nplain: %q\nrefresh: %q", len(refresh), len(plain), plain, refresh)
	}
	lights, rendered := 0, 0
	for _, s := range refresh {
		if strings.Contains(s, "working_state") {
			lights++
		}
		if strings.Contains(s, "HH24:MI:SS") {
			rendered++
		}
		if strings.Contains(strings.ToLower(s), "refresh") {
			t.Errorf("a statement mentions refresh: %q", s)
		}
	}
	if lights < 1 || lights > 2 {
		t.Errorf("%d statements read working_state during one render, want 1..2 (boardLightFacts: row facts, then the "+
			"queue-head candidates)", lights)
	}
	if rendered != 1 {
		t.Errorf("%d statements return the HH24:MI:SS render time, want exactly 1 — RenderedAt rides on boardLightFacts' "+
			"first statement, never its own query (criterion 30, 34)", rendered)
	}
	_ = dashboard.BoardTimeZone
	_ = httptest.NewRecorder
}
