//go:build integration

package dashboard_test

// SWT-52 (docs/tickets/board-status-lights_SPEC.md) criteria 12, 15, 20 (the
// board half) and 29, against a real database, the REAL dashboard.Server
// (dev-login), the real executor and the REAL policy matrix. Build-tagged
// `integration` AND env-gated on DATABASE_URL. NO LLM, NO network, NO
// orchestrator running.
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_isolights?sslmode=disable' \
//	  TZ=UTC go test -tags integration -p 1 -count=1 -run BoardLights ./internal/dashboard/
//
// Fixture instants are computed IN SQL from the board's own day-start
// expression (lsDayStart), so these pass at any hour, 20:00–24:00 EDT included
// (the SWT-48 lesson). Reuses dashGuard / dashPool / newDashServer / get /
// snippet (dashboard_integration_test.go) and bdInsID / bdCount
// (board_dismiss_integration_test.go). Own prefix `itest-lights-%`,
// FK-ordered cleanup: audit rows by task_id first (executeTask and these
// executor calls fill audit_events.task_id, which has no cascade).
//
// GREENFIELD NOTE — EXPECTED RED: tasks.html renders no light span, the
// default predicate hides every closed task, and task_signal is unregistered
// ("unknown tool") — the first assertion of each test fails.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - drop working_state_at from boardLightFacts, or a literal stale comparison → EndToEnd stale step.
//   - drop working_state from that SELECT → EndToEnd red step.
//   - remove `reopened_at IS NULL` from the default WHERE → DoneUntilMidnight, row G.
//   - t.updated_at instead of COALESCE(t.closed_at, t.updated_at) → DoneUntilMidnight, row B.
//   - queue heads over the displayed rows only → EndToEnd's filtered-lane checks.

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	lsSlug   = "itest-lights-proj"
	lsSlug2  = "itest-lights-proj2"
	lsClient = "itest-lights-client"
	// The board's local midnight, spelled as boardDayStart spells it, with the
	// zone inlined (fixtures only).
	lsDayStart = `(date_trunc('day', now() AT TIME ZONE 'America/New_York') AT TIME ZONE 'America/New_York')`
)

func cleanupLights(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const projs = `(SELECT id FROM projects WHERE slug IN ('` + lsSlug + `','` + lsSlug2 + `'))`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	for _, q := range []string{
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + `)`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM projects WHERE slug IN ('` + lsSlug + `','` + lsSlug2 + `')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func lsProject(t *testing.T, ctx context.Context, pool *pgxpool.Pool, slug string) int64 {
	t.Helper()
	return bdInsID(t, ctx, pool,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,$2,'manual','dashboard','/tmp/itest-lights','any') RETURNING id`, slug, lsClient)
}

// boardLight reads the light rendered before task id's link on a board page.
func boardLight(body string, id int64) (class, label string, ok bool) {
	ids := strconv.FormatInt(id, 10)
	m := regexp.MustCompile(`<span class="light light-([a-z]+)" role="img" aria-label="([^"]*)" title="[^"]*"></span>\s*` +
		`<a href="/tasks/` + ids + `">` + ids + `</a>`).FindStringSubmatch(body)
	if m == nil {
		return "", "", false
	}
	return m[1], html.UnescapeString(m[2]), true
}

func onBoard(body string, id int64) bool {
	return strings.Contains(body, `href="/tasks/`+strconv.FormatInt(id, 10)+`"`)
}

// lightsExecutor is the production wiring (tools' queueMatrixExecutor shape):
// the matrix in front of the static allow-list from the real registry.
func lightsExecutor(pool *pgxpool.Pool) *executor.Executor {
	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	return executor.New(reg, policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...)),
		audit.NewPGStore(pool))
}

func lsCall(t *testing.T, ctx context.Context, ex *executor.Executor, actor, tool string, task int64, args map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(args)
	if _, err := ex.Execute(ctx, executor.Call{Tool: tool, Actor: actor, Args: raw, TaskID: &task}); err != nil {
		t.Fatalf("%s %s on task %d: %v", tool, raw, task, err)
	}
}

// AMENDED by SWT-56 (signal-session-name): every signal names its session
// (required on working and needs_input, S2), so the row-5 labels read
// "session kube-c7" in place of "a session" (S8).
func lsSignal(t *testing.T, ctx context.Context, ex *executor.Executor, task int64, state string) {
	t.Helper()
	lsCall(t, ctx, ex, "mcp:manual:salvo", "task_signal", task,
		map[string]any{"task_id": task, "state": state, "worker_id": "manual:salvo", "session": "kube-c7"})
}

func assertBoardLight(t *testing.T, step, body string, id int64, class, labelPrefix string) {
	t.Helper()
	c, l, ok := boardLight(body, id)
	if !ok {
		t.Errorf("%s: task %d has no light span before its id\n%s", step, id, snippet(body))
		return
	}
	if c != class || !strings.HasPrefix(l, labelPrefix) {
		t.Errorf("%s: task %d light = (%q, %q), want (%q, %q…)", step, id, c, l, class, labelPrefix)
	}
}

// ---- criteria 12 and 15: done until local midnight --------------------------------

func TestBoardLights_Integration_DoneUntilMidnight(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupLights(t, ctx, pool)
	defer cleanupLights(t, ctx, pool)
	proj := lsProject(t, ctx, pool, lsSlug)

	closed := func(title, closedAt, updatedAt string) int64 {
		return bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, title, assignee_type, status, closed_at, updated_at)
			VALUES ($1,$2,'human','closed',`+closedAt+`,`+updatedAt+`) RETURNING id`, proj, title)
	}
	a := closed("LIGHTS-ROW-A", lsDayStart+" + interval '1 second'", "now()")
	b := closed("LIGHTS-ROW-B", lsDayStart+" - interval '1 second'", "now()") // COALESCE guard: updated_at is today
	c := closed("LIGHTS-ROW-C", lsDayStart, "now()")
	d := closed("LIGHTS-ROW-D", "NULL", "now()")
	e := closed("LIGHTS-ROW-E", "NULL", lsDayStart+" - interval '1 hour'")
	f := closed("LIGHTS-ROW-F", lsDayStart+" + interval '1 second'", "now()")
	g := closed("LIGHTS-ROW-G", lsDayStart+" + interval '1 second'", "now()")
	h := bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, title, assignee_type, status, created_at, updated_at)
		VALUES ($1,'LIGHTS-ROW-H','human','delivered', now() - interval '40 days', now() - interval '30 days') RETURNING id`, proj)
	r := bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, title, assignee_type, status) VALUES ($1,'LIGHTS-ROW-R','human','ready') RETURNING id`, proj)
	if _, err := pool.Exec(ctx, `INSERT INTO task_dismissals (task_id, reason_code, dismissed_by) VALUES ($1,'duplicate','dashboard:salvo')`, f); err != nil {
		t.Fatalf("seed open dismissal: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO task_dismissals (task_id, reason_code, dismissed_by, reopened_at, reopened_by)
		VALUES ($1,'not_actionable','dashboard:salvo', now(), 'dashboard:salvo')`, g); err != nil {
		t.Fatalf("seed reopened dismissal: %v", err)
	}

	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	code, body := get(t, client, ts.URL+"/tasks?project="+lsSlug)
	if code != http.StatusOK {
		t.Fatalf("GET /tasks = %d\n%s", code, snippet(body))
	}
	for _, row := range []struct {
		name  string
		id    int64
		shown bool
	}{
		{"A closed at day start + 1 s", a, true}, {"B closed at day start - 1 s", b, false},
		{"C closed at exactly day start", c, true}, {"D closed_at NULL, updated today", d, true},
		{"E closed_at NULL, updated yesterday", e, false}, {"F closed today, open dismissal", f, false},
		{"G closed today, dismissal reopened", g, true}, {"H delivered long ago", h, true}, {"R ready", r, true},
	} {
		if got := onBoard(body, row.id); got != row.shown {
			t.Errorf("default board: %s (task %d) shown = %v, want %v (criterion 12)", row.name, row.id, got, row.shown)
		}
	}
	for _, id := range []int64{a, c, d, g} {
		c, l, ok := boardLight(body, id)
		if !ok || c != "done" || l != "done today" {
			t.Errorf("default board: task %d light = (%q, %q), want (done, done today) (criterion 12)", id, c, l)
		}
	}
	assertBoardLight(t, "default board", body, h, "done", "delivered")

	_, closedBoard := get(t, client, ts.URL+"/tasks?project="+lsSlug+"&status=closed")
	for _, id := range []int64{a, b, c, d, e, f, g} {
		if !onBoard(closedBoard, id) {
			t.Errorf("?status=closed does not show closed task %d (every close, dismissed included)", id)
		}
	}
	if cl, l, _ := boardLight(closedBoard, f); cl != "none" || l != "dismissed (duplicate)" {
		t.Errorf("?status=closed: dismissed task light = (%q, %q), want (none, dismissed (duplicate)) (rule 1a)", cl, l)
	}
	if cl, l, _ := boardLight(closedBoard, b); cl != "done" || l != "done" {
		t.Errorf("?status=closed: yesterday's close light = (%q, %q), want (done, done)", cl, l)
	}
	_, readyBoard := get(t, client, ts.URL+"/tasks?project="+lsSlug+"&status=ready")
	for _, id := range []int64{a, b, c, d, e, f, g} {
		if onBoard(readyBoard, id) {
			t.Errorf("?status=ready shows closed task %d", id)
		}
	}

	// Criterion 15: the exports follow the board.
	code, csv := get(t, client, ts.URL+"/export/tasks.csv?project="+lsSlug)
	if code != http.StatusOK {
		t.Fatalf("GET /export/tasks.csv = %d", code)
	}
	if first, _, _ := strings.Cut(csv, "\n"); strings.TrimSuffix(first, "\r") != pinnedHeader {
		t.Errorf("CSV header = %q, want the pinned header (criterion 15)", first)
	}
	if !strings.Contains(csv, "LIGHTS-ROW-A") {
		t.Errorf("the default CSV export lacks the task closed today (criterion 15)")
	}
	if strings.Contains(csv, "LIGHTS-ROW-F") {
		t.Errorf("the default CSV export carries the dismissed task (criterion 15)")
	}
}

// ---- criterion 29: end-to-end truthfulness ------------------------------------------

func TestBoardLights_Integration_EndToEnd(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupLights(t, ctx, pool)
	defer cleanupLights(t, ctx, pool)

	p := lsProject(t, ctx, pool, lsSlug)
	p2 := lsProject(t, ctx, pool, lsSlug2) // same client: one claude lane spans both
	a := bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, title, assignee_type, status, priority) VALUES ($1,'LIGHTS-E2E-A','human','ready',2) RETURNING id`, p)
	b := bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, subproject, title, assignee_type, status, priority) VALUES ($1,'sub-b','LIGHTS-E2E-B','human','ready',0) RETURNING id`, p)
	c1 := bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, title, assignee_type, status, priority) VALUES ($1,'LIGHTS-E2E-C1','claude','ready',3) RETURNING id`, p2)
	c2 := bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, title, assignee_type, status, priority) VALUES ($1,'LIGHTS-E2E-C2','claude','ready',0) RETURNING id`, p)

	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()
	ex := lightsExecutor(pool)
	board := func(q string) string {
		t.Helper()
		code, body := get(t, client, ts.URL+"/tasks?project="+lsSlug+q)
		if code != http.StatusOK {
			t.Fatalf("GET /tasks?project=%s%s = %d\n%s", lsSlug, q, code, snippet(body))
		}
		return body
	}
	bIsNext := func(step, body string) {
		t.Helper()
		assertBoardLight(t, step+" (B)", body, b, "next", "next in queue ("+lsSlug+")")
	}

	// start: A blue, B grey.
	body := board("")
	assertBoardLight(t, "start (A)", body, a, "next", "next in queue ("+lsSlug+")")
	assertBoardLight(t, "start (B)", body, b, "none", "ready, queued")
	// The claude lane's head C1 is in ANOTHER project of the same client: the
	// project filter hides it, and cannot promote C2 (D2).
	assertBoardLight(t, "start (C2 behind a hidden head)", body, c2, "none", "ready, queued")
	_, body2 := get(t, client, ts.URL+"/tasks?project="+lsSlug2)
	assertBoardLight(t, "start (C1)", body2, c1, "next", "next in queue ("+lsClient+" console)")
	// Under ?assignee_type=claude no human task is blue (none is shown), and C2
	// is still not promoted by the filter.
	cl := board("&assignee_type=claude")
	if onBoard(cl, a) || onBoard(cl, b) {
		t.Errorf("?assignee_type=claude shows a human task")
	}
	assertBoardLight(t, "?assignee_type=claude (C2)", cl, c2, "none", "ready, queued")
	// Under ?subproject=sub-b the human head A is hidden; B is still not blue.
	assertBoardLight(t, "?subproject=sub-b (B behind a hidden head)", board("&subproject=sub-b"), b, "none", "ready, queued")

	lsSignal(t, ctx, ex, a, "working")
	body = board("")
	assertBoardLight(t, "working (A)", body, a, "working", "in progress (session kube-c7, last signal ")
	bIsNext("working", body)

	lsSignal(t, ctx, ex, a, "needs_input")
	body = board("")
	assertBoardLight(t, "needs_input (A)", body, a, "input", "waiting on your input (session kube-c7, since ")
	if _, l, _ := boardLight(body, a); !strings.Contains(l, "since") {
		t.Errorf("needs_input label %q does not say since", l)
	}
	bIsNext("needs_input", body)
	// D1 amendment (2026-09-14): a red signalled today shows HH:MM only; one
	// signalled before today's local midnight shows its date too, because
	// needs_input never goes stale and yesterday's red must not read as today's.
	if _, l, _ := boardLight(body, a); !regexp.MustCompile(`since \d{2}:\d{2}\)$`).MatchString(l) {
		t.Errorf("today's needs_input label %q, want `since HH:MM)` with no date", l)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET working_state_at = `+lsDayStart+` - interval '1 hour' WHERE id=$1`, a); err != nil {
		t.Fatalf("move the red to yesterday: %v", err)
	}
	body = board("")
	assertBoardLight(t, "needs_input from yesterday (A)", body, a, "input", "waiting on your input (session kube-c7, since ")
	if _, l, _ := boardLight(body, a); !regexp.MustCompile(`since \d{4}-\d{2}-\d{2} \d{2}:\d{2}\)$`).MatchString(l) {
		t.Errorf("yesterday's needs_input label %q, want `since YYYY-MM-DD HH:MM)` (D1 amendment)", l)
	}

	lsSignal(t, ctx, ex, a, "working")
	body = board("")
	assertBoardLight(t, "working again (A)", body, a, "working", "in progress (session kube-c7, last signal ")
	bIsNext("working again", body)

	if _, err := pool.Exec(ctx, `UPDATE tasks SET working_state_at = now() - interval '3 hours' WHERE id=$1`, a); err != nil {
		t.Fatalf("age the signal: %v", err)
	}
	body = board("")
	assertBoardLight(t, "stale (A)", body, a, "stale", "in progress? no session signal since ")
	bIsNext("stale", body)

	resp, err := client.PostForm(ts.URL+"/tasks/"+strconv.FormatInt(a, 10)+"/close", url.Values{"project": {lsSlug}})
	if err != nil {
		t.Fatalf("POST Done: %v", err)
	}
	resp.Body.Close()
	if resp.Request.URL.Query().Get("flash") != "task_close ok" {
		t.Fatalf("Done on the stale task flashed %q, want task_close ok (Done works on a signalled task, D4)",
			resp.Request.URL.Query().Get("flash"))
	}
	body = board("")
	assertBoardLight(t, "done (A)", body, a, "done", "done today")
	bIsNext("done", body)
	if n := bdCount(t, ctx, pool, `SELECT count(*) FROM tasks WHERE id=$1 AND working_state IS NULL AND working_state_at IS NULL AND working_session IS NULL`, a); n != 1 {
		t.Errorf("after Done the marker is not NULL (D9: a real close clears it)")
	}

	if _, err := pool.Exec(ctx, `UPDATE tasks SET closed_at = `+lsDayStart+` - interval '1 hour' WHERE id=$1`, a); err != nil {
		t.Fatalf("move the close to yesterday: %v", err)
	}
	body = board("")
	if onBoard(body, a) {
		t.Errorf("a task closed yesterday is still on the default board (D5)")
	}
	bIsNext("after midnight", body)
}

// ---- criterion 20, board half: every close path clears the marker ---------------------

func TestBoardLights_Integration_ClosePathsClearTheMarker(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupLights(t, ctx, pool)
	defer cleanupLights(t, ctx, pool)
	p := lsProject(t, ctx, pool, lsSlug)
	x := bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, title, assignee_type, status) VALUES ($1,'LIGHTS-CLOSE-X','human','ready') RETURNING id`, p)
	y := bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, title, assignee_type, status) VALUES ($1,'LIGHTS-CLOSE-Y','human','ready') RETURNING id`, p)
	z := bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, title, assignee_type, status) VALUES ($1,'LIGHTS-CLOSE-Z','human','ready') RETURNING id`, p)

	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()
	ex := lightsExecutor(pool)
	for _, id := range []int64{x, y, z} {
		lsSignal(t, ctx, ex, id, "needs_input")
	}

	lsCall(t, ctx, ex, "mcp:manual:salvo", "task_close", x, map[string]any{"task_id": x, "reason": "swb done"})
	for _, pf := range []struct {
		id   int64
		path string
		form url.Values
	}{
		{y, "/dismiss", url.Values{"reason_code": {"duplicate"}}},
		{z, "/close", url.Values{}},
	} {
		resp, err := client.PostForm(ts.URL+"/tasks/"+strconv.FormatInt(pf.id, 10)+pf.path, pf.form)
		if err != nil {
			t.Fatalf("POST %s: %v", pf.path, err)
		}
		resp.Body.Close()
	}

	for _, id := range []int64{x, y, z} {
		if n := bdCount(t, ctx, pool,
			`SELECT count(*) FROM tasks WHERE id=$1 AND status='closed' AND working_state IS NULL AND working_state_at IS NULL AND working_session IS NULL`, id); n != 1 {
			t.Errorf("task %d: not closed with a NULL marker (criterion 20 / D9)", id)
		}
		var keys []string
		if err := pool.QueryRow(ctx, `SELECT ARRAY(SELECT jsonb_object_keys(payload) ORDER BY 1) FROM task_events
			WHERE task_id=$1 AND event_type='status_changed' ORDER BY id DESC LIMIT 1`, id).Scan(&keys); err != nil {
			t.Fatalf("status_changed for %d: %v", id, err)
		}
		if strings.Join(keys, ",") != "from,reason,to" {
			t.Errorf("task %d status_changed keys = %v, want exactly [from reason to] (criterion 20)", id, keys)
		}
	}
	body := func(q string) string { _, b := get(t, client, ts.URL+"/tasks?project="+lsSlug+q); return b }
	def := body("")
	assertBoardLight(t, "task_close from needs_input", def, x, "done", "done today")
	assertBoardLight(t, "board Done from needs_input", def, z, "done", "done today")
	if onBoard(def, y) {
		t.Errorf("the dismissed task is on the default board (D5: dismissed tasks leave at once)")
	}
	assertBoardLight(t, "board Dismiss from needs_input", body("&status=closed"), y, "none", "dismissed (duplicate)")
}
