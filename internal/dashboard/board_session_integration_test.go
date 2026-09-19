//go:build integration

package dashboard_test

// SWT-56 (docs/tickets/signal-session-name_SPEC.md) criteria 25 and 11 (the
// board half): the session tag end to end, through the REAL task_signal
// (lightsExecutor: the real matrix) and the REAL board handler (dev-login).
// NO LLM, NO network, NO orchestrator.
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_isosess?sslmode=disable' \
//	  TZ=UTC go test -tags integration -p 1 -count=1 -run BoardSession ./internal/dashboard/
//
// Reuses dashGuard / dashPool / newDashServer / get / snippet, bdInsID / bdCount,
// and lightsExecutor / lsProject / boardLight / assertBoardLight / onBoard
// (board_lights_integration_test.go). Own slug, FK-ordered cleanup.
//
// GREENFIELD NOTE — EXPECTED RED: the column does not exist before 0036, the
// board renders no tag, and task_signal ignores `session`.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC criterion 26):
//   - '' in place of COALESCE(t.working_session,'') in boardLightFacts → the tag steps.
//   - drop working_session = $3 from the set → the tag steps.
//   - validateSignal accepting a missing session → the refused step.
//   - {{.Light.Session}} through template.HTML → the escaping step.

import (
	"context"
	"encoding/json"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
)

const stSlug = "itest-sesstag-proj"

func cleanupSessTag(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const projs = `(SELECT id FROM projects WHERE slug = '` + stSlug + `')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	for _, q := range []string{
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + `)`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM projects WHERE slug = '` + stSlug + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

// sessionTag reads the tag rendered in task id's row: its class suffix, its RAW
// title attribute, and its (unescaped) text. SWT-67 Part 7: read inside
// boardRow — the tag's markup is byte-unchanged, it just moved from the title
// cell into its own Gate cell (B10).
func sessionTag(body string, id int64) (class, rawTitle, text string, ok bool) {
	row := boardRow(body, id)
	if row == "" {
		return "", "", "", false
	}
	m := regexp.MustCompile(`<span class="session-tag session-([a-z]+)" title="([^"]*)">([^<]*)</span>`).
		FindStringSubmatch(row)
	if m == nil {
		return "", "", "", false
	}
	return m[1], m[2], html.UnescapeString(m[3]), true
}

func stSignal(ctx context.Context, ex *executor.Executor, task int64, state, session string) error {
	a := map[string]any{"task_id": task, "state": state, "worker_id": "manual:salvo"}
	if session != "" {
		a["session"] = session
	}
	raw, _ := json.Marshal(a)
	_, err := ex.Execute(ctx, executor.Call{Tool: "task_signal", Actor: "mcp:manual:salvo", Args: raw, TaskID: &task})
	return err
}

func assertTag(t *testing.T, step, body string, id int64, class, text string) {
	t.Helper()
	c, rawTitle, got, ok := sessionTag(body, id)
	if !ok {
		t.Errorf("%s: task %d has no session tag in its Gate cell (S8, SWT-67 B10)\n%s", step, id, snippet(body))
		return
	}
	if c != class || got != text {
		t.Errorf("%s: task %d tag = (session-%s, %q), want (session-%s, %q)", step, id, c, got, class, text)
	}
	if _, label, _ := boardLight(body, id); html.UnescapeString(rawTitle) != label {
		t.Errorf("%s: the tag's title %q is not the light's label %q (S8: title carries the full label)", step,
			html.UnescapeString(rawTitle), label)
	}
}

func TestBoardSession_Integration_Walkthrough(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupSessTag(t, ctx, pool)
	defer cleanupSessTag(t, ctx, pool)

	p := lsProject(t, ctx, pool, stSlug)
	a := bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, title, assignee_type, status) VALUES ($1,'SESSTAG-A','human','ready') RETURNING id`, p)
	g := bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, title, assignee_type, status) VALUES ($1,'SESSTAG-GHOST','human','ready') RETURNING id`, p)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()
	ex := lightsExecutor(pool)
	board := func(q string) string {
		t.Helper()
		_, body := get(t, client, ts.URL+"/tasks?project="+stSlug+q)
		return body
	}
	cols := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT COALESCE(working_state,'') || '|' || COALESCE(working_state_at::text,'') || '|' ||
			COALESCE(working_session,'') FROM tasks WHERE id=$1`, a).Scan(&s); err != nil {
			t.Fatalf("read columns: %v", err)
		}
		return s
	}

	// 1. working as switchboard-67: yellow, tag switchboard-67.
	if err := stSignal(ctx, ex, a, "working", "switchboard-67"); err != nil {
		t.Fatalf("working as switchboard-67: %v", err)
	}
	body := board("")
	assertBoardLight(t, "working", body, a, "working", "in progress (session switchboard-67, last signal ")
	assertTag(t, "working", body, a, "working", "switchboard-67")

	// 2. needs_input as kube-c7 (a takeover): red, tag kube-c7, event from_session.
	if err := stSignal(ctx, ex, a, "needs_input", "kube-c7"); err != nil {
		t.Fatalf("needs_input as kube-c7: %v", err)
	}
	body = board("")
	assertBoardLight(t, "needs_input", body, a, "input", "waiting on your input (session kube-c7, since ")
	assertTag(t, "needs_input", body, a, "input", "kube-c7")
	var fromSession string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(payload->>'from_session','?') FROM task_events WHERE task_id=$1
		AND event_type='working_state_changed' ORDER BY id DESC LIMIT 1`, a).Scan(&fromSession); err != nil {
		t.Fatalf("event: %v", err)
	}
	if fromSession != "switchboard-67" {
		t.Errorf("takeover event from_session = %q, want switchboard-67 (S5)", fromSession)
	}
	// Auto-refresh renders the same tag (D15: one ordinary render).
	assertTag(t, "refresh=on", board("&refresh=on"), a, "input", "kube-c7")

	// 3. needs_input WITHOUT a session: refused; the row stays red with kube-c7.
	before := cols()
	if err := stSignal(ctx, ex, a, "needs_input", ""); err == nil || !strings.Contains(err.Error(), "missing session") {
		t.Errorf("needs_input without a session = %v, want the missing-session refusal (S2)", err)
	}
	if after := cols(); after != before {
		t.Errorf("a refused signal changed the columns: %s → %s", before, after)
	}
	assertTag(t, "after the refusal", board(""), a, "input", "kube-c7")

	// 4. working again, aged 3 h: the stale ring, tag kube-c7.
	if err := stSignal(ctx, ex, a, "working", "kube-c7"); err != nil {
		t.Fatalf("working as kube-c7: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET working_state_at = now() - interval '3 hours' WHERE id=$1`, a); err != nil {
		t.Fatalf("age: %v", err)
	}
	body = board("")
	assertBoardLight(t, "stale", body, a, "stale", "in progress? no session signal since ")
	assertTag(t, "stale", body, a, "stale", "kube-c7")
	if _, l, _ := boardLight(body, a); !strings.HasSuffix(l, "(session kube-c7)") {
		t.Errorf("stale label %q does not end `(session kube-c7)` (S8)", l)
	}

	// 5. An old marker (no name): `session unknown` (S9).
	if _, err := pool.Exec(ctx, `UPDATE tasks SET working_session = NULL WHERE id=$1`, a); err != nil {
		t.Fatalf("drop the name: %v", err)
	}
	body = board("")
	assertTag(t, "old marker", body, a, "stale", "session unknown")
	if _, l, _ := boardLight(body, a); !strings.Contains(l, "(session unknown)") {
		t.Errorf("old-marker label %q does not say `(session unknown)` (S9)", l)
	}

	// Criterion 11, board half: a dangling name under no state shows nothing.
	if _, err := pool.Exec(ctx, `UPDATE tasks SET working_session = 'ghost' WHERE id=$1`, g); err != nil {
		t.Fatalf("seed ghost: %v", err)
	}
	body = board("")
	if _, _, _, ok := sessionTag(body, g); ok {
		t.Errorf("a ready task with a dangling name (state NULL) renders a session tag (S4 read gating)")
	}
	if _, l, _ := boardLight(body, g); strings.Contains(l, "ghost") || strings.Contains(l, "session") {
		t.Errorf("the ghost row's label %q names a session (S4 read gating)", l)
	}

	// 6. Done: green, no tag, all three columns NULL.
	resp, err := client.PostForm(ts.URL+"/tasks/"+strconv.FormatInt(a, 10)+"/close", url.Values{"project": {stSlug}})
	if err != nil {
		t.Fatalf("POST Done: %v", err)
	}
	resp.Body.Close()
	body = board("")
	assertBoardLight(t, "done", body, a, "done", "done today")
	if _, _, _, ok := sessionTag(body, a); ok {
		t.Errorf("a done task still renders a session tag")
	}
	if n := bdCount(t, ctx, pool, `SELECT count(*) FROM tasks WHERE id=$1 AND working_state IS NULL
		AND working_state_at IS NULL AND working_session IS NULL`, a); n != 1 {
		t.Errorf("after Done the three columns are not all NULL (S7): %s", cols())
	}
}

// Criterion 25, escaping: a hostile name reaches the page escaped in the text
// node and in the title attribute (html/template), never as markup.
func TestBoardSession_Integration_HostileNameIsEscaped(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupSessTag(t, ctx, pool)
	defer cleanupSessTag(t, ctx, pool)

	p := lsProject(t, ctx, pool, stSlug)
	c := bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, title, assignee_type, status) VALUES ($1,'SESSTAG-C','human','ready') RETURNING id`, p)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()
	const hostile = `<img src=x onerror=alert(1)>&"'`
	if err := stSignal(ctx, stEx(pool), c, "working", hostile); err != nil {
		t.Fatalf("working with the hostile name: %v (it is printable ASCII under the cap: S3 accepts it)", err)
	}
	_, body := get(t, client, ts.URL+"/tasks?project="+stSlug)
	if strings.Contains(body, "<img src=x") {
		t.Errorf("the board carries the raw substring `<img src=x`: the session name is not escaped (criterion 25)")
	}
	if !strings.Contains(body, "&lt;img src=x onerror=alert(1)&gt;&amp;") {
		t.Errorf("the board does not carry the escaped name `&lt;img src=x onerror=alert(1)&gt;&amp;`\n%s", snippet(body))
	}
	_, rawTitle, text, ok := sessionTag(body, c)
	if !ok {
		t.Fatalf("task C has no session tag")
	}
	if !strings.Contains(rawTitle, "&#34;") {
		t.Errorf("the tag's title attribute %q does not carry &#34; for the quote (criterion 25)", rawTitle)
	}
	if text != hostile {
		t.Errorf("the tag's unescaped text = %q, want the stored name %q verbatim (no Go truncation or rewrite)", text, hostile)
	}
}

func stEx(pool *pgxpool.Pool) *executor.Executor { return lightsExecutor(pool) }
