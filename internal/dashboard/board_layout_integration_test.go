//go:build integration

package dashboard_test

// board-layout-compact (SWT-57, docs/tickets/board-layout-compact_SPEC.md)
// criteria 19-22, against a real database, the REAL dashboard.Server
// (dev-login), the real executor and the REAL policy matrix. Build-tagged
// `integration` AND env-gated on DATABASE_URL. NO LLM, NO network, NO
// orchestrator running. Run it in an ISOLATED database (the IK 2026-09-12
// landmine: the compose `ops` db is shared):
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_boardlayout?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run BoardLayout ./internal/dashboard/
//
// Reuses dashGuard / dashPool / newDashServer / get / snippet
// (dashboard_integration_test.go), bdInsID (board_dismiss_integration_test.go),
// lightsExecutor / lsSignal / boardLight / onBoard / lsDayStart
// (board_lights_integration_test.go) and attr (board_refresh_integration_test.go).
// Own slug `itest-layout-proj`, FK-ordered cleanup (policy_decisions and
// audit_events by task_id first — the SWT-37 landmine), rerunnable. Fixture
// instants are SQL (lsDayStart), so it passes at any hour, 20:00-24:00 EDT
// included (SWT-48).
//
// GREENFIELD NOTE — EXPECTED RED: the board renders one <h2>{status} (n)</h2>
// per status, no section-* headers, no topbar, no advanced popup, the legend
// above the rows and the raw updated_at, so the first assertion of each test
// fails.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - sectionFor by status only → SectionsFollowTheLights (D lands in queue).
//   - a row appended to two sections → SectionsFollowTheLights (exactly once).
//   - queue sorted by id, or the QueueRank assignment dropped from boardLightFacts
//     → SectionsFollowTheLights (Q2 before Q1: only Postgres supplies the rank).
//   - the advanced <details> rendered open, the inputs moved to a second form, the
//     summary marker dropped → AdvancedFilterMarkedRoundTripsAndEscaped.
//   - the legend back above the sections → SectionsFollowTheLights.
//   - {{.UpdatedAt}} as the cell text, or '' for the updated expression →
//     UpdatedStampIsShortAndColumnFed.

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	lySlug   = "itest-layout-proj"
	lyClient = "itest-layout-client"
)

func cleanupLayout(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const projs = `(SELECT id FROM projects WHERE slug = '` + lySlug + `')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	for _, q := range []string{
		`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + `)`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM projects WHERE slug = '` + lySlug + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

type layoutSeed struct{ a, b, e, c, d, q1, q2, h, x int64 }

func (s layoutSeed) names() map[int64]string {
	return map[int64]string{s.a: "A", s.b: "B", s.e: "E", s.c: "C", s.d: "D", s.q1: "Q1", s.q2: "Q2", s.h: "H", s.x: "X"}
}

// seedLayout is criterion 19's table. Q1 and H carry criterion 22's updated_at.
func seedLayout(t *testing.T, ctx context.Context, pool *pgxpool.Pool) layoutSeed {
	t.Helper()
	proj := bdInsID(t, ctx, pool,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,$2,'manual','dashboard','/tmp/itest-layout','any') RETURNING id`, lySlug, lyClient)
	task := func(title, assignee, status string, priority int) int64 {
		return bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, title, body, assignee_type, status, priority)
			VALUES ($1,$2,'',$3,$4,$5) RETURNING id`, proj, title, assignee, status, priority)
	}
	var s layoutSeed
	s.a = task("LAYOUT-A", "human", "ready", 0)
	s.b = task("LAYOUT-B", "human", "blocked", 0)
	s.e = task("LAYOUT-E", "claude", "needs_feedback", 0)
	s.c = task("LAYOUT-C", "claude", "in_progress", 0)
	s.d = task("LAYOUT-D", "human", "ready", 0)
	// Q1 first (the lower id), priority 0; Q2 after it, priority 2: id order puts
	// Q1 first, tools.TaskQueueOrder puts Q2 first.
	s.q1 = bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, title, body, assignee_type, status, priority, created_at, updated_at)
		VALUES ($1,'LAYOUT-Q1','','human','ready',0, now() - interval '1 minute', `+lsDayStart+` + interval '2 hours') RETURNING id`, proj)
	s.q2 = task("LAYOUT-Q2", "human", "ready", 2)
	s.h = bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, title, body, assignee_type, status, priority, updated_at)
		VALUES ($1,'LAYOUT-H','','human','holding',0, `+lsDayStart+` - interval '2 hours') RETURNING id`, proj)
	s.x = bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, title, body, assignee_type, status, priority, closed_at)
		VALUES ($1,'LAYOUT-X','','human','closed',0, `+lsDayStart+` + interval '1 hour') RETURNING id`, proj)
	ex := lightsExecutor(pool)
	lsSignal(t, ctx, ex, s.a, "needs_input") // session kube-c7
	lsSignal(t, ctx, ex, s.d, "working")
	return s
}

var (
	lyH2    = regexp.MustCompile(`<h2 id="section-([a-z_]+)">([^<]*?) \((\d+)\)</h2>`)
	lyRowID = regexp.MustCompile(`<span class="light light-[a-z]+" role="img" aria-label="[^"]*" title="[^"]*"></span>\s*<a href="/tasks/(\d+)">\d+</a>`)
)

type lySection struct {
	key, title string
	count      int
	ids        []int64
}

// layoutSections slices the body between the <h2 id="section-…"> markers and
// reads the row ids (by their light span) in each slice, in document order.
func layoutSections(body string) []lySection {
	locs := lyH2.FindAllStringSubmatchIndex(body, -1)
	var out []lySection
	for i, l := range locs {
		end := len(body)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		sec := lySection{key: body[l[2]:l[3]], title: body[l[4]:l[5]]}
		sec.count, _ = strconv.Atoi(body[l[6]:l[7]])
		for _, m := range lyRowID.FindAllStringSubmatch(body[l[1]:end], -1) {
			id, _ := strconv.ParseInt(m[1], 10, 64)
			sec.ids = append(sec.ids, id)
		}
		out = append(out, sec)
	}
	return out
}

func lyRender(secs []lySection, names map[int64]string) string {
	var parts []string
	for _, s := range secs {
		var ids []string
		for _, id := range s.ids {
			n, ok := names[id]
			if !ok {
				n = strconv.FormatInt(id, 10)
			}
			ids = append(ids, n)
		}
		parts = append(parts, s.key+"/"+s.title+"("+strconv.Itoa(s.count)+")=["+strings.Join(ids, " ")+"]")
	}
	return strings.Join(parts, " | ")
}

// lyRowHTML is task id's <tr> … </tr> on a board page, "" when absent.
func lyRowHTML(body string, id int64) string {
	ids := strconv.FormatInt(id, 10)
	i := strings.Index(body, `<a href="/tasks/`+ids+`">`+ids+`</a>`)
	if i < 0 {
		return ""
	}
	start := strings.LastIndex(body[:i], "<tr")
	end := strings.Index(body[i:], "</tr>")
	if start < 0 || end < 0 {
		return ""
	}
	return body[start : i+end]
}

var lyAdvancedSummary = regexp.MustCompile(`(?s)<details class="advanced-filter">\s*(<summary[^>]*>)(.*?)</summary>`)

// L1: the light classes each section may hold.
var lyAllowed = map[string]map[string]bool{
	"blocked":   {"input": true, "none": true},
	"in_flight": {"working": true, "stale": true},
	"queue":     {"next": true, "none": true},
	"holding":   {"none": true},
	"done":      {"done": true},
	"other":     {"none": true},
}

func layoutBoard(t *testing.T, client *http.Client, base, query string) string {
	t.Helper()
	code, body := get(t, client, base+"/tasks?"+query)
	if code != http.StatusOK {
		t.Fatalf("GET /tasks?%s = %d\n%s", query, code, snippet(body))
	}
	return body
}

// ---- criterion 19 ---------------------------------------------------------------------

func TestBoardLayout_Integration_SectionsFollowTheLights(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupLayout(t, ctx, pool)
	defer cleanupLayout(t, ctx, pool)
	s := seedLayout(t, ctx, pool)
	names := s.names()
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	body := layoutBoard(t, client, ts.URL, "project="+lySlug)
	got := lyRender(layoutSections(body), names)
	want := "blocked/blocked(3)=[A E B] | in_flight/in flight(2)=[D C] | queue/queue(2)=[Q2 Q1] | " +
		"holding/holding(1)=[H] | done/done(1)=[X]"
	if got != want {
		t.Errorf("sections =\n  %s\nwant\n  %s\n(criterion 19: red before grey in blocked; Q2 before Q1 — id order would "+
			"put Q1 first, so the rank comes from Postgres; no section-other)", got, want)
	}
	if strings.Contains(body, `id="section-other"`) {
		t.Errorf("the default board renders section-other; nothing seeded belongs there (criterion 19)")
	}
	for id, name := range names {
		re := regexp.MustCompile(`<span class="light light-[a-z]+" role="img" aria-label="[^"]*" title="[^"]*"></span>\s*` +
			`<a href="/tasks/` + strconv.FormatInt(id, 10) + `">`)
		if n := len(re.FindAllString(body, -1)); n != 1 {
			t.Errorf("row %s (task %d) renders %d times, want exactly once (criterion 19: one section per task)", name, id, n)
		}
	}
	for _, row := range []struct {
		id    int64
		class string
	}{{s.a, "input"}, {s.e, "input"}, {s.b, "none"}, {s.d, "working"}, {s.c, "working"},
		{s.q2, "next"}, {s.q1, "none"}, {s.h, "none"}, {s.x, "done"}} {
		if c, _, ok := boardLight(body, row.id); !ok || c != row.class {
			t.Errorf("row %s light = %q (found %v), want %q (criterion 19)", names[row.id], c, ok, row.class)
		}
	}
	for _, sec := range layoutSections(body) {
		for _, id := range sec.ids {
			if c, _, _ := boardLight(body, id); !lyAllowed[sec.key][c] {
				t.Errorf("row %s (light %s) sits in section %s; L1 puts that light elsewhere (criterion 19)", names[id], c, sec.key)
			}
		}
	}
	lastTable := strings.LastIndex(body, "</table>")
	legend := strings.Index(body, `id="light-legend"`)
	note := strings.Index(body, "Queues are filters on the one tasks table")
	if lastTable < 0 || legend < lastTable || note < lastTable {
		t.Errorf("the legend (at %d) and the note (at %d) do not come after the last </table> (at %d) (criterion 19, L9)",
			legend, note, lastTable)
	}
	top := strings.Index(body, `<div class="topbar">`)
	blocked := strings.Index(body, `id="section-blocked"`)
	if top < 0 || blocked < 0 || top > blocked {
		t.Errorf("the topbar (at %d) does not come before section-blocked (at %d) (criterion 19)", top, blocked)
	}
}

// ---- criterion 20 ---------------------------------------------------------------------

func TestBoardLayout_Integration_StatusFilterKeepsGrouping(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupLayout(t, ctx, pool)
	defer cleanupLayout(t, ctx, pool)
	s := seedLayout(t, ctx, pool)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	body := layoutBoard(t, client, ts.URL, "project="+lySlug+"&status=ready")
	got := lyRender(layoutSections(body), s.names())
	want := "blocked/blocked(1)=[A] | in_flight/in flight(1)=[D] | queue/queue(2)=[Q2 Q1]"
	if got != want {
		t.Errorf("?status=ready sections =\n  %s\nwant\n  %s\n(criterion 20, L3: a filter narrows; the sections still apply)", got, want)
	}
}

// ---- criterion 21 ---------------------------------------------------------------------

func TestBoardLayout_Integration_AdvancedFilterMarkedRoundTripsAndEscaped(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupLayout(t, ctx, pool)
	defer cleanupLayout(t, ctx, pool)
	s := seedLayout(t, ctx, pool)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	body := layoutBoard(t, client, ts.URL, "project="+lySlug+"&assignee_type=human&refresh=on")
	sum := lyAdvancedSummary.FindStringSubmatch(body)
	if sum == nil {
		t.Fatalf("the board has no <details class=\"advanced-filter\"> with a <summary> (criterion 21)\n%s", snippet(body))
	}
	if !strings.Contains(sum[1], "advanced-active") {
		t.Errorf("the summary tag %s is not marked advanced-active with assignee_type=human set (criterion 21)", sum[1])
	}
	if !strings.Contains(html.UnescapeString(sum[2]), "assignee_type=human") {
		t.Errorf("the summary reads %q, want it to name assignee_type=human (criterion 21)", sum[2])
	}
	clear := attr(t, body, `id="advanced-clear" href="([^"]*)"`)
	if cu, err := url.Parse(clear); clear == "" || err != nil || cu.Path != "/tasks" || cu.RawQuery != "project="+lySlug+"&refresh=on" {
		t.Errorf("advanced-clear href = %q, want /tasks?project=%s&refresh=on exactly (criterion 21)", clear, lySlug)
	}
	for _, tag := range regexp.MustCompile(`<details\b[^>]*>`).FindAllString(body, -1) {
		if regexp.MustCompile(`\bopen\b`).MatchString(tag) {
			t.Errorf("the page renders %s: no <details> is ever rendered open (criterion 21)", tag)
		}
	}
	fi := strings.Index(body, `<form class="filters"`)
	if fi < 0 {
		t.Fatalf("the board has no filter form")
	}
	form := body[fi : fi+strings.Index(body[fi:], "</form>")]
	if !regexp.MustCompile(`<input[^>]*name="assignee_type"[^>]*value="human"`).MatchString(form) {
		t.Errorf("the assignee_type input inside <form class=\"filters\"> does not carry value=\"human\" (criterion 21: the " +
			"advanced inputs stay inside the one filter form, so a project change keeps them)")
	}
	row := lyRowHTML(body, s.q1)
	done := regexp.MustCompile(`(?s)<form class="inline" method="post" action="/tasks/` + strconv.FormatInt(s.q1, 10) + `/close">(.*?)</form>`).
		FindStringSubmatch(row)
	if done == nil {
		t.Errorf("row Q1 has no Done form (criterion 21)")
	} else {
		for _, frag := range []string{`name="project" value="` + lySlug + `"`, `name="assignee_type" value="human"`, `name="refresh" value="on"`} {
			if !strings.Contains(done[1], frag) {
				t.Errorf("row Q1's Done form lacks %s: the filter survives a Done (criterion 21)", frag)
			}
		}
	}
	for _, id := range []int64{s.c, s.e} {
		if onBoard(body, id) {
			t.Errorf("claude task %d is on the assignee_type=human board (criterion 21)", id)
		}
	}

	// A hostile value is escaped in the summary.
	esc := layoutBoard(t, client, ts.URL, "project="+lySlug+"&subproject="+url.QueryEscape("<b>x</b>"))
	if m := lyAdvancedSummary.FindStringSubmatch(esc); m == nil || !strings.Contains(m[2], "subproject=&lt;b&gt;x&lt;/b&gt;") {
		t.Errorf("the summary does not show subproject=&lt;b&gt;x&lt;/b&gt; escaped (criterion 21): %v", m)
	}
	if strings.Contains(esc, "<b>x") {
		t.Errorf("the page carries the raw <b>x: html/template escaping bypassed (criterion 21)")
	}

	// No advanced key: no marker, no clear link.
	plain := layoutBoard(t, client, ts.URL, "project="+lySlug)
	pm := lyAdvancedSummary.FindStringSubmatch(plain)
	if pm == nil {
		t.Errorf("CONTROL: the plain board has no advanced <details>; the negative below would be vacuous")
	} else if strings.Contains(pm[1], "advanced-active") {
		t.Errorf("with no advanced key the summary is still marked advanced-active (criterion 21)")
	}
	if strings.Contains(plain, `id="advanced-clear"`) {
		t.Errorf("with no advanced key the board still renders advanced-clear (criterion 21)")
	}
}

// ---- criterion 22 ---------------------------------------------------------------------

func TestBoardLayout_Integration_UpdatedStampIsShortAndColumnFed(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupLayout(t, ctx, pool)
	defer cleanupLayout(t, ctx, pool)
	s := seedLayout(t, ctx, pool)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	body := layoutBoard(t, client, ts.URL, "project="+lySlug)
	cell := regexp.MustCompile(`<td class="muted" title="([^"]*)">([^<]*)</td>`)
	for _, row := range []struct {
		name   string
		id     int64
		format string
		shape  *regexp.Regexp
	}{
		{"Q1 (updated today)", s.q1, "HH24:MI", regexp.MustCompile(`^\d{2}:\d{2}$`)},
		{"H (updated before local midnight)", s.h, "YYYY-MM-DD", regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)},
	} {
		var wantText, wantTitle string
		if err := pool.QueryRow(ctx, `SELECT to_char(updated_at AT TIME ZONE 'America/New_York', $2), updated_at::text
			FROM tasks WHERE id=$1`, row.id, row.format).Scan(&wantText, &wantTitle); err != nil {
			t.Fatalf("read %s updated_at: %v", row.name, err)
		}
		if !row.shape.MatchString(wantText) {
			t.Fatalf("CONTROL: %s expected stamp %q is not %s-shaped", row.name, wantText, row.format)
		}
		tr := lyRowHTML(body, row.id)
		if tr == "" {
			t.Errorf("%s (task %d) is not on the board", row.name, row.id)
			continue
		}
		m := cell.FindStringSubmatch(tr)
		if m == nil {
			t.Errorf("%s has no updated cell <td class=\"muted\" title=\"…\">…</td> (criterion 22, L8)\n%s", row.name, tr)
			continue
		}
		title, text := html.UnescapeString(m[1]), html.UnescapeString(m[2])
		if text != wantText {
			t.Errorf("%s updated cell text = %q, want %q (criterion 22: %s in America/New_York)", row.name, text, wantText, row.format)
		}
		if title != wantTitle {
			t.Errorf("%s updated cell title = %q, want the raw updated_at::text %q (criterion 22)", row.name, title, wantTitle)
		}
		if strings.Contains(text, "+00") {
			t.Errorf("%s updated cell text %q carries the raw offset (criterion 22)", row.name, text)
		}
	}
}
