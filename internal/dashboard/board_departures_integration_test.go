//go:build integration

package dashboard_test

// board-departures (SWT-67, docs/tickets/board-departures_SPEC.md) Part 6,
// criteria 33-39, against a real database, the REAL dashboard.Server
// (dev-login), the real executor and the REAL policy matrix. Build-tagged
// `integration` AND env-gated on DATABASE_URL. NO LLM, NO network, NO
// orchestrator running. Run it ONLY in an ISOLATED database (the IK 2026-09-12
// landmine: the compose `ops` db is shared by every worktree and agent):
//
//	psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c "CREATE DATABASE ops_boarddep"
//	make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_boarddep?sslmode=disable'
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_boarddep?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run BoardDepartures ./internal/dashboard/
//
// Reuses dashGuard / dashPool / newDashServer / get / snippet
// (dashboard_integration_test.go), bdInsID / bdCount
// (board_dismiss_integration_test.go), lightsExecutor / lsCall / lsDayStart /
// boardRow / boardLight / onBoard (board_lights_integration_test.go), attr
// (board_refresh_integration_test.go) and layoutBoard / layoutSections /
// lyRender (board_layout_integration_test.go).
//
// CLEANUP PACT: owns project itest-dep-proj. FK-ordered and rerunnable —
// policy_decisions and audit_events by task_id FIRST (the SWT-37 landmine),
// then external_refs, task_dismissals, task_claims, task_events, tasks, the
// project. Fixture instants are computed IN SQL (lsDayStart, now() - interval),
// so this passes at any hour, 20:00-24:00 EDT included (the SWT-48 lesson).
//
// "TEST THE COLUMN, NOT THE FIXTURE": the elapsed cell's value is asserted
// against a signal aged 65 minutes IN SQL, so replacing state_age_min's
// expression with a literal 0 turns criterion 34 red (the unit test cannot
// catch that, by construction — SPEC "Mutations").
//
// The pure helpers are unexported and this is package dashboard_test, so the
// oracles here are written INDEPENDENTLY: projectHue("itest-dep-proj") is
// spelled 109 by hand (the mock's fold over the slug's bytes) and the tallies
// are counted by hand from the seed. A test that called the helper it checks
// would agree with any bug in it.
//
// GREENFIELD NOTE — EXPECTED RED: the board renders one <table> per section with
// no panes, no row wrapper, no remark, no elapsed cell, no chip and no
// /static/ route, so the first assertion of each test here fails.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - boardPanes drops a section key, or reorders a pane by Order → Panes.
//   - incoming given order: 0 → Panes.
//   - remarkFor returns a constant, or loses pr_open → RowShape.
//   - elapsedFor returns a value for done → RowShape.
//   - state_age_min replaced by a literal 0 → RowShape (N1 reads 00:00).
//   - projectHue returns a constant → PriorityChipAndGate.
//   - the Dismiss form dropped from claude rows → VerbsSurviveTheRestyle.
//   - the <details class="row-verbs"> moved inside the row <a> → VerbsSurviveTheRestyle.
//   - the @font-face src put back on fonts.gstatic.com → AssetsAreSelfHostedAndOpen.
//   - /static/ wrapped in s.auth.Require → AssetsAreSelfHostedAndOpen (the cookie-less 302).

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
	depSlug   = "itest-dep-proj"
	depClient = "itest-dep-client"
	// The mock's fold, h = (h*31 + b) % 360, over the bytes of depSlug — written
	// out here so this test is an independent oracle of projectHue (B9).
	depHue = 109
)

func cleanupDep(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const projs = `(SELECT id FROM projects WHERE slug = '` + depSlug + `')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	for _, q := range []string{
		`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + `)`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM external_refs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM projects WHERE slug = '` + depSlug + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

// depSeed is one task per SECTION, so the panes, the tallies and the remark
// table are all exercised by one render.
type depSeed struct {
	n1, n2, f1, f2, i1, q1, q2, h1, d1 int64
}

func (s depSeed) names() map[int64]string {
	return map[int64]string{s.n1: "N1", s.n2: "N2", s.f1: "F1", s.f2: "F2", s.i1: "I1",
		s.q1: "Q1", s.q2: "Q2", s.h1: "H1", s.d1: "D1"}
}

func seedDep(t *testing.T, ctx context.Context, pool *pgxpool.Pool) depSeed {
	t.Helper()
	proj := bdInsID(t, ctx, pool,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,$2,'manual','dashboard','/tmp/itest-dep','any') RETURNING id`, depSlug, depClient)
	task := func(title, assignee, status string, priority int) int64 {
		return bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, title, body, assignee_type, status, priority)
			VALUES ($1,$2,'',$3,$4,$5) RETURNING id`, proj, title, assignee, status, priority)
	}
	var s depSeed
	// NEEDS YOU: a red session signal (priority 2 — B8's mark) and a grey
	// dependency block.
	s.n1 = task("DEP-N1 waiting", "human", "ready", 2)
	s.n2 = task("DEP-N2 blocked", "human", "blocked", 0)
	// IN FLIGHT: a signalled human row and a claude row whose STATUS is the light.
	s.f1 = task("DEP-F1 signalled", "human", "ready", 0)
	s.f2 = task("DEP-F2 worker", "claude", "in_progress", 0)
	// ARRIVALS: a human task carrying a github PR ref (SWT-59 I2) — the incoming
	// shape that needs no promoter chain.
	s.i1 = task("DEP-I1 pr review", "human", "ready", 0)
	if _, err := pool.Exec(ctx,
		`INSERT INTO external_refs (task_id, system, external_key) VALUES ($1,'github','itest-dep/repo#1')`, s.i1); err != nil {
		t.Fatalf("seed I1's github ref: %v", err)
	}
	// DEPARTURES: priority 5 makes Q1 the human lane's head (tools.TaskQueueOrder
	// is priority DESC first), so Q1 is blue and Q2 is grey — deterministically.
	s.q1 = task("DEP-Q1 head", "human", "ready", 5)
	s.q2 = task("DEP-Q2 behind", "human", "ready", 0)
	// HOLDING, updated BEFORE local midnight: criterion 34's YYYY-MM-DD stamp.
	s.h1 = bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, title, body, assignee_type, status, priority, updated_at)
		VALUES ($1,'DEP-H1 review','','human','holding',0, `+lsDayStart+` - interval '2 hours') RETURNING id`, proj)
	// LANDED TODAY.
	s.d1 = bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, title, body, assignee_type, status, priority, closed_at)
		VALUES ($1,'DEP-D1 done','','human','closed',0, `+lsDayStart+` + interval '1 hour') RETURNING id`, proj)

	// The signals go through the REAL task_signal (invariant 3), then their
	// instants are aged IN SQL so the elapsed cell has a value no literal 0 can
	// produce. 65 min 30 s and 3 min 30 s keep FLOOR(minutes) off a boundary.
	ex := lightsExecutor(pool)
	lsCall(t, ctx, ex, "mcp:manual:salvo", "task_signal", s.n1,
		map[string]any{"task_id": s.n1, "state": "needs_input", "worker_id": "manual:salvo", "session": "dep-console"})
	lsCall(t, ctx, ex, "mcp:manual:salvo", "task_signal", s.f1,
		map[string]any{"task_id": s.f1, "state": "working", "worker_id": "manual:salvo", "session": "dep-console"})
	if _, err := pool.Exec(ctx,
		`UPDATE tasks SET working_state_at = now() - interval '65 minutes 30 seconds' WHERE id=$1`, s.n1); err != nil {
		t.Fatalf("age N1's signal: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE tasks SET working_state_at = now() - interval '3 minutes 30 seconds' WHERE id=$1`, s.f1); err != nil {
		t.Fatalf("age F1's signal: %v", err)
	}
	return s
}

// ---- row-local readers, all on boardRow (Part 7's ONE slicer) -------------------

// depElement returns the index just past the close tag of the element opening at
// s[start:], counting nested tags of the same name. (elementEnd lives in the
// internal package's own test files; this is dashboard_test.)
func depElement(s string, start int, tag string) (int, bool) {
	open, closeTag := "<"+tag, "</"+tag+">"
	if start < 0 || !strings.HasPrefix(s[start:], open) {
		return 0, false
	}
	depth, i := 0, start
	for i < len(s) {
		o := strings.Index(s[i:], open)
		c := strings.Index(s[i:], closeTag)
		if c < 0 {
			return 0, false
		}
		if o >= 0 && o < c {
			depth++
			i += o + len(open)
			continue
		}
		depth--
		i += c + len(closeTag)
		if depth == 0 {
			return i, true
		}
	}
	return 0, false
}

var (
	depRemRE  = regexp.MustCompile(`<span class="rem"[^>]*>\s*<span class="light light-[a-z]+"[^>]*></span>\s*([^<]*)`)
	depElRE   = regexp.MustCompile(`<span class="el"[^>]*>([^<]*)</span>`)
	depTimeRE = regexp.MustCompile(`<span class="time" title="([^"]*)">([^<]*)</span>`)
	depChipRE = regexp.MustCompile(`<span class="chip"[^>]*style="([^"]*)"[^>]*>([^<]*)</span>`)
	depPrioRE = regexp.MustCompile(`<span class="prio"[^>]*aria-label="([^"]*)"[^>]*>`)
	depTagRE  = regexp.MustCompile(`<span class="session-tag session-[a-z]+" title="[^"]*">([^<]*)</span>`)
	depAnchor = regexp.MustCompile(`<a class="r" href="/tasks/(\d+)">`)
)

func depRemark(row string) string {
	m := depRemRE.FindStringSubmatch(row)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(html.UnescapeString(m[1]))
}

func depElapsed(row string) string {
	m := depElRE.FindStringSubmatch(row)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// ---- criterion 33: the panes -----------------------------------------------------

func TestBoardDepartures_Integration_Panes(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupDep(t, ctx, pool)
	defer cleanupDep(t, ctx, pool)
	s := seedDep(t, ctx, pool)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	body := layoutBoard(t, client, ts.URL, "project="+depSlug)

	for _, col := range []string{"left", "right"} {
		if n := strings.Count(body, `<div class="col col-`+col+`">`); n != 1 {
			t.Fatalf(`the page has %d <div class="col col-%s"> wrappers, want exactly 1 (criterion 33)`+"\n%s",
				n, col, snippet(body))
		}
	}
	li := strings.Index(body, `<div class="col col-left">`)
	le, ok := depElement(body, li, "div")
	if !ok {
		t.Fatalf("the left pane <div> is never closed")
	}
	ri := strings.Index(body, `<div class="col col-right">`)
	re, ok := depElement(body, ri, "div")
	if !ok {
		t.Fatalf("the right pane <div> is never closed")
	}
	left, right := body[li:le], body[ri:re]
	for _, c := range []struct {
		key, pane string
	}{
		{"incoming", "left"}, {"blocked", "left"}, {"in_flight", "left"},
		{"queue", "right"}, {"holding", "right"}, {"done", "right"},
	} {
		h2 := `<h2 id="section-` + c.key + `">`
		inLeft, inRight := strings.Contains(left, h2), strings.Contains(right, h2)
		if !inLeft && !inRight {
			t.Errorf("section-%s is on neither pane (criterion 33)", c.key)
			continue
		}
		if (c.pane == "left") != inLeft || (c.pane == "right") != inRight {
			t.Errorf("section-%s is in the %s pane, want %s (criterion 33 / B2's table)", c.key,
				map[bool]string{true: "left", false: "right"}[inLeft], c.pane)
		}
	}

	// SWT-59 I3 survives the two columns: the DOM order is untouched and only CSS
	// `order` moves things (B2).
	if inc, first := strings.Index(body, `<h2 id="section-incoming">`), strings.Index(body, "<h2"); inc < 0 || inc != first {
		t.Errorf("section-incoming (at %d) is not the FIRST <h2 (at %d) on the page (criterion 33, SWT-59 I3)", inc, first)
	}
	var keys []string
	for _, sec := range layoutSections(body) {
		keys = append(keys, sec.key)
	}
	if got, want := strings.Join(keys, ","), "incoming,blocked,in_flight,queue,holding,done"; got != want {
		t.Errorf("the <h2> document order is [%s], want [%s] — boardSectionOrder, NOT the visual order (criterion 33)",
			got, want)
	}

	// Each panel's CSS order is B2's table.
	panel := regexp.MustCompile(`<section class="([^"]*)" style="order:(\d+)">\s*<div class="panelhead">` +
		`<h2 id="section-([a-z_]+)">`)
	got := map[string][2]string{}
	for _, m := range panel.FindAllStringSubmatch(body, -1) {
		got[m[3]] = [2]string{m[1], m[2]}
	}
	for key, want := range map[string][2]string{
		"incoming": {"panel grow", "3"}, "blocked": {"panel alarm", "1"}, "in_flight": {"panel", "2"},
		"queue": {"panel grow", "1"}, "holding": {"panel", "2"}, "done": {"panel", "3"},
	} {
		if got[key] != want {
			t.Errorf("panel %s renders class/order %v, want %v (criterion 33 / B2: incoming is drawn LAST in the left "+
				"column and stays FIRST in the document)", key, got[key], want)
		}
	}
	if strings.Contains(body, `id="section-other"`) {
		t.Errorf("the default board renders section-other; nothing seeded belongs there (criterion 33)")
	}
	// Every seeded row is on the page exactly once.
	for id, name := range s.names() {
		if n := len(regexp.MustCompile(`<a class="r" href="/tasks/`+strconv.FormatInt(id, 10)+`">`).
			FindAllString(body, -1)); n != 1 {
			t.Errorf("row %s (task %d) renders %d times, want exactly once (criterion 33)", name, id, n)
		}
	}
}

// ---- criterion 34: the row's shape ------------------------------------------------

func TestBoardDepartures_Integration_RowShape(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupDep(t, ctx, pool)
	defer cleanupDep(t, ctx, pool)
	s := seedDep(t, ctx, pool)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	body := layoutBoard(t, client, ts.URL, "project="+depSlug)
	hhmm := regexp.MustCompile(`^\d{2}:\d{2}$`)
	for _, row := range []struct {
		name          string
		id            int64
		class, remark string
		elapsed       string // "" = the cell is empty; "HH:MM" = shaped; else exact
	}{
		{"N1 red", s.n1, "input", "waiting on you", "01:05"},
		{"N2 blocked", s.n2, "none", "blocked", ""},
		{"F1 working", s.f1, "working", "in progress", "00:03"},
		{"F2 worker in_progress", s.f2, "working", "in progress", "HH:MM"},
		{"I1 incoming", s.i1, "none", "queued", ""},
		{"Q1 head", s.q1, "next", "next up", ""},
		{"Q2 behind", s.q2, "none", "queued", ""},
		{"H1 holding", s.h1, "none", "holding", ""},
		{"D1 done", s.d1, "done", "done", ""},
	} {
		r := boardRow(body, row.id)
		if r == "" {
			t.Errorf("%s (task %d) has no row wrapper carrying href=\"/tasks/%d\" (criterion 34)", row.name, row.id, row.id)
			continue
		}
		class, label, ok := boardLight(body, row.id)
		if !ok || class != row.class {
			t.Errorf("%s light = %q (found %v), want %q — lightFor's class, unchanged (criterion 34, B1)",
				row.name, class, ok, row.class)
		}
		if label == "" {
			t.Errorf("%s: the light's full sentence is gone from aria-label/title (B5: Light.Label stays on the span)", row.name)
		}
		if got := depRemark(r); got != row.remark {
			t.Errorf("%s remark = %q, want %q — lowercase in the markup, uppercased by CSS (criterion 34 / B7)",
				row.name, got, row.remark)
		}
		got := depElapsed(r)
		switch {
		case row.elapsed == "":
			if got != "" {
				t.Errorf("%s elapsed = %q, want empty: a done, queued or unsignalled row shows no clock (criterion 34 / B6)",
					row.name, got)
			}
		case row.elapsed == "HH:MM":
			if !hhmm.MatchString(got) {
				t.Errorf("%s elapsed = %q, want an HH:MM-shaped clock (criterion 34)", row.name, got)
			}
		default:
			if got != row.elapsed {
				t.Errorf("%s elapsed = %q, want %q. Criterion 34, THE COLUMN NOT THE FIXTURE: the signal was aged 65 "+
					"minutes IN SQL, so a state_age_min expression replaced by a literal 0 reads 00:00 here and the "+
					"unit test cannot catch it", row.name, got, row.elapsed)
			}
		}
	}

	// SWT-57 criterion 22, carried over: the `updated` stamp is HH:MM for a row
	// updated today and YYYY-MM-DD for one updated before local midnight, with the
	// raw updated_at::text in the title.
	for _, row := range []struct {
		name   string
		id     int64
		format string
		shape  *regexp.Regexp
	}{
		{"Q1 (updated today)", s.q1, "HH24:MI", regexp.MustCompile(`^\d{2}:\d{2}$`)},
		{"H1 (updated before local midnight)", s.h1, "YYYY-MM-DD", regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)},
	} {
		var wantText, wantTitle string
		if err := pool.QueryRow(ctx, `SELECT to_char(updated_at AT TIME ZONE 'America/New_York', $2), updated_at::text
			FROM tasks WHERE id=$1`, row.id, row.format).Scan(&wantText, &wantTitle); err != nil {
			t.Fatalf("read %s updated_at: %v", row.name, err)
		}
		if !row.shape.MatchString(wantText) {
			t.Fatalf("CONTROL: %s expected stamp %q is not %s-shaped", row.name, wantText, row.format)
		}
		m := depTimeRE.FindStringSubmatch(boardRow(body, row.id))
		if m == nil {
			t.Errorf("%s has no <span class=\"time\" title=\"…\">…</span> cell (criterion 34)", row.name)
			continue
		}
		if text := html.UnescapeString(m[2]); text != wantText {
			t.Errorf("%s time cell = %q, want %q (criterion 34: SWT-57 L8's stamp is unchanged)", row.name, text, wantText)
		}
		if title := html.UnescapeString(m[1]); title != wantTitle {
			t.Errorf("%s time cell title = %q, want the raw updated_at::text %q (criterion 34)", row.name, title, wantTitle)
		}
	}
}

// ---- criterion 35: the verbs survive the restyle -----------------------------------

func TestBoardDepartures_Integration_VerbsSurviveTheRestyle(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupDep(t, ctx, pool)
	defer cleanupDep(t, ctx, pool)
	s := seedDep(t, ctx, pool)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	q := "project=" + depSlug + "&assignee_type=&refresh=on"
	body := layoutBoard(t, client, ts.URL, q)
	human := map[int64]bool{s.n1: true, s.n2: true, s.f1: true, s.i1: true, s.q1: true, s.q2: true, s.h1: true, s.d1: true}
	for id, name := range s.names() {
		r := boardRow(body, id)
		if r == "" {
			t.Errorf("row %s (task %d) is not on the board", name, id)
			continue
		}
		ids := strconv.FormatInt(id, 10)
		dismiss := regexp.MustCompile(`(?s)<form[^>]*action="/tasks/` + ids + `/dismiss"[^>]*>(.*?)</form>`).FindStringSubmatch(r)
		if dismiss == nil {
			t.Errorf("row %s has no Dismiss form (criterion 35: Dismiss renders on EVERY row, claude rows included — "+
				"SWT-31 D6)", name)
		} else {
			for _, frag := range []string{`name="project" value="` + depSlug + `"`, `name="refresh" value="on"`,
				`name="status" value="`, `name="assignee_type" value="`, `name="subproject" value="`} {
				if !strings.Contains(dismiss[1], frag) {
					t.Errorf("row %s's Dismiss form lacks the hidden filter input %s (criterion 35: five keys, so the "+
						"verb lands back on the same filtered board)", name, frag)
				}
			}
		}
		close := regexp.MustCompile(`<form[^>]*action="/tasks/` + ids + `/close"`).MatchString(r)
		if close != human[id] {
			t.Errorf("row %s has a Done form = %v, want %v (criterion 35 / SWT-51 D2: human rows only)", name, close, human[id])
		}
		// B4: the verbs are the anchor's SIBLING — a <form> inside an <a> is
		// invalid HTML and steals the row's touch target.
		ai := strings.Index(r, `<a class="r" href="/tasks/`+ids+`">`)
		ae, ok := depElement(r, ai, "a")
		if ai < 0 || !ok {
			t.Errorf("row %s has no closed row anchor (criterion 35)", name)
			continue
		}
		di := strings.Index(r, `<details class="row-verbs">`)
		if di < 0 || di < ae {
			t.Errorf("row %s: the <details class=\"row-verbs\"> (at %d) does not open AFTER the row anchor closes (at "+
				"%d) (criterion 35 / B4)", name, di, ae)
		}
		if strings.Contains(r[ai:ae], "<form") || strings.Contains(r[ai:ae], "<button") {
			t.Errorf("row %s has a form or a button INSIDE the row link (criterion 35 / B4)", name)
		}
	}

	// The markup reaches the handlers: a real POST of each verb through the real
	// executor still flashes and still lands on the same filtered board. (The
	// dismiss/close integration suites own the semantics; this owns the wiring.)
	for _, v := range []struct {
		id    int64
		path  string
		form  url.Values
		flash string
	}{
		{s.q2, "/dismiss", url.Values{"reason_code": {"duplicate"}, "project": {depSlug}, "refresh": {"on"}}, "task_dismiss ok"},
		{s.h1, "/close", url.Values{"project": {depSlug}, "refresh": {"on"}}, "task_close ok"},
	} {
		resp, err := client.PostForm(ts.URL+"/tasks/"+strconv.FormatInt(v.id, 10)+v.path, v.form)
		if err != nil {
			t.Fatalf("POST %s: %v", v.path, err)
		}
		resp.Body.Close()
		back := resp.Request.URL
		if got := back.Query().Get("flash"); got != v.flash {
			t.Errorf("POST %s flashed %q, want %q (criterion 35: same route, same handler, same executor call)", v.path, got, v.flash)
		}
		if back.Path != "/tasks" || back.Query().Get("project") != depSlug || back.Query().Get("refresh") != "on" {
			t.Errorf("POST %s landed on %q, want the same filtered board (/tasks?project=%s&refresh=on) (criterion 35)",
				v.path, back.String(), depSlug)
		}
	}
	// Invariant 3: each verb wrote its audit rows, with the task on them.
	for _, id := range []int64{s.q2, s.h1} {
		if n := bdCount(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE task_id=$1`, id); n == 0 {
			t.Errorf("task %d has no audit_events row; every verb still goes validate → policy → audit → handler "+
				"(invariant 3, criterion 35)", id)
		}
	}
}

// ---- criterion 36: the priority mark, the chip and the gate -------------------------

func TestBoardDepartures_Integration_PriorityChipAndGate(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupDep(t, ctx, pool)
	defer cleanupDep(t, ctx, pool)
	s := seedDep(t, ctx, pool)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	body := layoutBoard(t, client, ts.URL, "project="+depSlug)
	hot, cold := boardRow(body, s.n1), boardRow(body, s.q2)
	if hot == "" || cold == "" {
		t.Fatalf("N1 or Q2 is not on the board\n%s", snippet(body))
	}
	if m := depPrioRE.FindStringSubmatch(hot); m == nil {
		t.Errorf("the priority-2 row carries no <span class=\"prio\" … aria-label=…> mark (criterion 36 / B8: real "+
			"text with an accessible name, not a CSS ::after)\n%s", hot)
	} else if m[1] != "priority 2" {
		t.Errorf("the priority mark's aria-label = %q, want \"priority 2\" (criterion 36)", m[1])
	}
	if depPrioRE.MatchString(cold) {
		t.Errorf("the priority-0 row carries a priority mark (criterion 36: the mark is decoration on >= 2)")
	}
	for _, r := range []struct {
		name, row string
	}{{"N1", hot}, {"Q2", cold}} {
		m := depChipRE.FindStringSubmatch(r.row)
		if m == nil {
			t.Errorf("row %s has no project chip (criterion 36)\n%s", r.name, r.row)
			continue
		}
		if want := "--chip-h:" + strconv.Itoa(depHue); !strings.Contains(m[1], want) {
			t.Errorf("row %s chip style = %q, want it to carry %q — projectHue(%q) is the mock's fold over the slug's "+
				"bytes, so the accepted screenshots' colours are the colours that ship (criterion 36 / B9)",
				r.name, m[1], want, depSlug)
		}
		if strings.Contains(m[1], "ZgotmplZ") {
			t.Errorf("row %s chip style = %q: html/template refused the value. B9 puts DIGITS in a custom property "+
				"precisely because that is safe in a CSS context", r.name, m[1])
		}
		if text := strings.TrimSpace(html.UnescapeString(m[2])); text != depSlug {
			t.Errorf("row %s chip text = %q, want the FULL slug %q — the board ellipsizes in CSS only; Go never "+
				"truncates a name (criterion 36 / B9, the SWT-56 precedent)", r.name, text, depSlug)
		}
	}
	// The Gate cell: a signalled row carries the tag, an unsignalled one carries
	// none (SWT-56 S8, unchanged; B10 only moves it).
	if m := depTagRE.FindStringSubmatch(hot); m == nil {
		t.Errorf("the signalled row carries no session tag (criterion 36 / B10)\n%s", hot)
	} else if html.UnescapeString(m[1]) != "dep-console" {
		t.Errorf("the session tag reads %q, want \"dep-console\" (criterion 36)", m[1])
	}
	if depTagRE.MatchString(cold) {
		t.Errorf("an unsignalled row carries a session tag (criterion 36: S8 sets Session only on input/working/stale)")
	}
	// It is in its OWN cell now, not in the title cell (B10).
	gi := strings.Index(hot, `<span class="gate">`)
	ti := strings.Index(hot, `<span class="title"`)
	si := strings.Index(hot, `class="session-tag`)
	if gi < 0 || si < gi || (ti >= 0 && si > ti && si < gi) {
		t.Errorf("the session tag is not inside the row's `gate` cell (criterion 36 / B10)\n%s", hot)
	}
}

// ---- criterion 37: self-hosted assets, reachable WITHOUT a session -------------------

func TestBoardDepartures_Integration_AssetsAreSelfHostedAndOpen(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupDep(t, ctx, pool)
	defer cleanupDep(t, ctx, pool)
	seedDep(t, ctx, pool)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	body := layoutBoard(t, client, ts.URL, "project="+depSlug)
	for _, banned := range []string{"fonts.googleapis.com", "fonts.gstatic.com", "https://", "http://"} {
		if strings.Contains(body, banned) {
			t.Errorf("the rendered board contains %q. Criterion 37 / invariant 4: the page makes NO outbound request "+
				"at all — the fonts are served from the dashboard itself", banned)
		}
	}

	// Every /static/ URL the page references, from the markup AND from the
	// <style> block's @font-face rules.
	refs := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?:href|src)="(/static/[^"]*)"`).FindAllStringSubmatch(body, -1) {
		refs[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`url\(\s*['"]?(/static/[^'")]*)`).FindAllStringSubmatch(body, -1) {
		refs[m[1]] = true
	}
	if len(refs) < 6 {
		t.Fatalf("the page references %d /static/ assets, want at least 6 (five woff2 files and the manifest) "+
			"(criterion 37): %v", len(refs), refs)
	}
	// A FRESH client: no cookie jar, no session, and redirects NOT followed — an
	// authenticated route would 302 these to the login page and silently break
	// the installability path (B17).
	plain := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	wantType := map[string]string{".woff2": "font/woff2", ".webmanifest": "application/manifest+json", ".png": "image/png"}
	for ref := range refs {
		resp, err := plain.Get(ts.URL + ref)
		if err != nil {
			t.Errorf("GET %s with no session: %v", ref, err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s with NO session cookie = %d, want 200. Criterion 37 / B17: a manifest, its icons and a "+
				"@font-face file are fetched by the browser WITHOUT credentials, so /static/ sits beside /healthz "+
				"and is not wrapped in s.auth.Require", ref, resp.StatusCode)
			continue
		}
		for ext, want := range wantType {
			if strings.HasSuffix(ref, ext) {
				if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, want) {
					t.Errorf("GET %s Content-Type = %q, want %q (criterion 37)", ref, ct, want)
				}
			}
		}
		if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Errorf("GET %s Cache-Control = %q, want `public, max-age=31536000, immutable` (B17)", ref, cc)
		}
	}
	// fs.Sub over an embedded FS cannot traverse out of static/ — assert it.
	for _, evil := range []string{"/static/../server.go", "/static/..%2fserver.go", "/static/../../go.mod"} {
		resp, err := plain.Get(ts.URL + evil)
		if err != nil {
			continue // a client-side rejection of the path is also a pass
		}
		buf := make([]byte, 400)
		n, _ := resp.Body.Read(buf)
		resp.Body.Close()
		if strings.Contains(string(buf[:n]), "package dashboard") || strings.Contains(string(buf[:n]), "module ") {
			t.Errorf("GET %s returned repository source (criterion 37)", evil)
		}
	}
	// The manifest is reachable at the exact path the <head> links.
	if !refs["/static/manifest-v2.webmanifest"] {
		t.Errorf("the page does not link /static/manifest-v2.webmanifest (criterion 17): %v", refs)
	}
}

// ---- criterion 38: the tallies and the ticker footer ---------------------------------

func TestBoardDepartures_Integration_TalliesAndFooter(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupDep(t, ctx, pool)
	defer cleanupDep(t, ctx, pool)
	seedDep(t, ctx, pool)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	// Counted by hand from seedDep — an independent oracle of boardTallies (B16):
	// need you (blocked) 2, in flight 2, incoming 1, queued (queue + holding) 3,
	// done today 1.
	for _, tc := range []struct {
		name, query string
		want        []string
	}{
		{"every row", "project=" + depSlug + "&refresh=on", []string{"2", "2", "1", "3", "1"}},
		// ?assignee_type=human drops F2, the one claude row, from in flight.
		{"human only", "project=" + depSlug + "&assignee_type=human&refresh=on", []string{"2", "1", "1", "3", "1"}},
	} {
		body := layoutBoard(t, client, ts.URL, tc.query)
		hi := strings.Index(body, `<header class="sign">`)
		if hi < 0 {
			t.Fatalf("the board has no <header class=\"sign\"> (criterion 38 / B15)\n%s", snippet(body))
		}
		he, ok := depElement(body, hi, "header")
		if !ok {
			t.Fatalf("the sign <header> is never closed")
		}
		var got []string
		for _, m := range regexp.MustCompile(`<b[^>]*>(\d+)</b>`).FindAllStringSubmatch(body[hi:he], -1) {
			got = append(got, m[1])
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: the header's tallies = %v, want %v (need you, in flight, incoming, queued, done today) "+
				"(criterion 38 / B16: counts of what the board is SHOWING, i.e. after the filters)", tc.name, got, tc.want)
		}
		if !strings.Contains(body[hi:he], ">"+depSlug+"<") {
			t.Errorf("%s: the sign header does not name the active project %q (B15: .ProjectLabel)", tc.name, depSlug)
		}
	}

	body := layoutBoard(t, client, ts.URL, "project="+depSlug+"&refresh=on")
	fi := strings.Index(body, `<footer class="ticker">`)
	if fi < 0 {
		t.Fatalf("the board has no <footer class=\"ticker\"> (criterion 38 / B15)")
	}
	fe, ok := depElement(body, fi, "footer")
	if !ok {
		t.Fatalf("the ticker <footer> is never closed")
	}
	foot := body[fi:fe]
	if !strings.Contains(foot, `id="light-legend"`) {
		t.Errorf("the ticker does not carry the legend (criterion 38: the legend must remain)")
	}
	if n := strings.Count(foot, `class="legend-light light-`); n != 6 {
		t.Errorf("the legend carries %d legend-light spans, want 6 (criterion 38: one per light)", n)
	}
	ind := regexp.MustCompile(`<p id="auto-refresh"[^>]*>([^<]*)</p>`).FindStringSubmatch(foot)
	if ind == nil {
		t.Fatalf("with refresh=on the ticker carries no #auto-refresh indicator (criterion 38)")
	}
	if !regexp.MustCompile(`last refreshed \d{2}:\d{2}:\d{2}\)`).MatchString(ind[1]) {
		t.Errorf("the indicator reads %q, want an HH:MM:SS render time from the DB clock (criterion 38, SWT-52 D15)", ind[1])
	}
	if n := strings.Count(body, "<script"); n != 1 {
		t.Errorf("the rendered page has %d <script, want exactly 1 (criterion 38 / B12)", n)
	}
	// B12: the one script renders with auto-refresh OFF too — the clock, the
	// paging, FULL and the wake lock do not depend on it.
	off := layoutBoard(t, client, ts.URL, "project="+depSlug)
	if n := strings.Count(off, "<script"); n != 1 {
		t.Errorf("with auto-refresh off the page has %d <script, want 1 (criterion 38 / B12)", n)
	}
	if strings.Contains(off, `id="auto-refresh"`) {
		t.Errorf("with auto-refresh off the indicator still renders (criterion 20)")
	}
	if !regexp.MustCompile(`data-refresh="(on)?"`).MatchString(off) {
		t.Errorf("the script carries no data-refresh attribute with auto-refresh off (B12: the loop arms from the " +
			"attribute, not from a second {{if .AutoRefresh}})")
	}
}

// ---- criterion 39: the filters still round-trip --------------------------------------

func TestBoardDepartures_Integration_FiltersRoundTrip(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupDep(t, ctx, pool)
	defer cleanupDep(t, ctx, pool)
	s := seedDep(t, ctx, pool)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	body := layoutBoard(t, client, ts.URL, "project="+depSlug+"&assignee_type=human&refresh=on")
	sum := regexp.MustCompile(`(?s)<details class="advanced-filter">\s*(<summary[^>]*>)(.*?)</summary>`).FindStringSubmatch(body)
	if sum == nil {
		t.Fatalf("the board has no <details class=\"advanced-filter\"> with a <summary> (criterion 39)\n%s", snippet(body))
	}
	if !strings.Contains(sum[1], "advanced-active") {
		t.Errorf("the summary tag %s is not marked advanced-active with assignee_type=human set (criterion 39)", sum[1])
	}
	clear := attr(t, body, `id="advanced-clear" href="([^"]*)"`)
	if cu, err := url.Parse(clear); clear == "" || err != nil || cu.Path != "/tasks" ||
		cu.RawQuery != "project="+depSlug+"&refresh=on" {
		t.Errorf("advanced-clear href = %q, want /tasks?project=%s&refresh=on exactly (criterion 39)", clear, depSlug)
	}
	fi := strings.Index(body, `<form class="filters"`)
	if fi < 0 {
		t.Fatalf("the board has no filter form")
	}
	form := body[fi : fi+strings.Index(body[fi:], "</form>")]
	if !regexp.MustCompile(`<input[^>]*name="assignee_type"[^>]*value="human"`).MatchString(form) {
		t.Errorf("the assignee_type input inside the ONE <form class=\"filters\"> does not carry value=\"human\" " +
			"(criterion 39: the advanced inputs stay inside the one filter form, so a project change keeps them)")
	}
	if n := strings.Count(body, `<form class="filters"`); n != 1 {
		t.Errorf("the page has %d filter forms, want exactly 1 (criterion 39)", n)
	}
	for _, tag := range regexp.MustCompile(`<details\b[^>]*>`).FindAllString(body, -1) {
		if regexp.MustCompile(`\bopen\b`).MatchString(tag) {
			t.Errorf("the page renders %s: no <details> is ever rendered open (criterion 39, SWT-57 L5/L6)", tag)
		}
	}
	if onBoard(body, s.f2) {
		t.Errorf("the claude task is on the ?assignee_type=human board (criterion 39)")
	}
	// The row anchors still point at the detail page, filters or not. The count is
	// a CONTROL: with no row anchor at all every row-local assertion in this file
	// would be vacuous.
	anchors := depAnchor.FindAllStringSubmatch(body, -1)
	if len(anchors) != 8 {
		t.Errorf("the ?assignee_type=human board carries %d row anchors <a class=\"r\" href=\"/tasks/{id}\">, want 8 "+
			"(the seeded human rows) (criterion 39; CONTROL for every row-local assertion in this file)", len(anchors))
	}
	for _, m := range anchors {
		if m[1] == "" {
			t.Errorf("a row anchor has no task id (criterion 39)")
		}
	}
}
