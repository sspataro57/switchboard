//go:build integration

package dashboard_test

// SWT-41 (docs/tickets/orchestrator-deploy_SPEC.md) criterion 9 and D5: the
// orchestrator health on /funnel (an "Orchestrator" section) and on /tasks (one
// red line when the verdict is not ok), read from Postgres — pg_locks,
// task_events and orchestrator_cursor — never from the process, because "not
// running" has no process to ask.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run OrchestratorHealth ./internal/dashboard/
//
// Build-tagged `integration` AND env-gated (dashGuard, with its FATAL on
// 192.168.50.49). The REAL dashboard.Server under httptest with dev-login,
// reusing dashGuard / dashPool / newDashServer / get / snippet from
// dashboard_integration_test.go, and the funnel suite's shadow-schema trick for
// a REAL failing query (funnel criterion 17's shape).
//
// The state is driven through the columns health reads, not a fake: the
// orchestrator's advisory lock is held on a separate connection ("running"),
// the cursor row is positioned, and a task_events row is backdated. The three
// verdicts are ok / not_running / stalled.
//
// IMPOSED MARKUP (names chosen here):
//   - /funnel: a `<h2>Orchestrator</h2>` section, rendering the verdict word
//     (ok | not_running | stalled), backlog, oldest unprocessed, cursor and head.
//     A failing health query shows up as the page's existing
//     `section failed — <name>: <err>` line with a name containing
//     "orchestrator"; the other sections still render.
//   - /tasks: when the verdict is not ok, ONE element carrying
//     `id="orchestrator-health"` that contains the verdict word. Absent when ok,
//     and absent when the health query fails (the board never breaks and shows
//     nothing).
//
// CLEANUP PACT: owns `itest-orchhealth-%` projects and the
// itest_orchhealth_shadow schema. The cursor row is positioned, not restored
// (every suite that depends on it positions it itself).
//
// GREENFIELD NOTE — EXPECTED RED: /funnel has no Orchestrator section and
// /tasks no health line, so every positive assertion fails. The failing-query
// test's /tasks half is green today (there is no line to leak) and must stay so.

import (
	"context"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	orch "github.com/sspataro57/switchboard/internal/orchestrator"
)

const (
	ohSlug         = "itest-orchhealth-proj"
	ohShadowSchema = "itest_orchhealth_shadow"
	ohBoardMarker  = `id="orchestrator-health"`
	ohSectionHead  = `<h2>Orchestrator</h2>`
)

type ohSuite struct {
	pool *pgxpool.Pool
	task int64
}

func newOHSuite(t *testing.T, ctx context.Context) *ohSuite {
	t.Helper()
	dashGuard(t)
	pool := dashPool(t, ctx)
	t.Cleanup(pool.Close)
	s := &ohSuite{pool: pool}
	s.cleanup(t, ctx)
	t.Cleanup(func() { s.cleanup(t, context.Background()) })

	if err := pool.QueryRow(ctx,
		`WITH p AS (INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		            VALUES ($1,$1,'itest-orchhealth','manual','console','/tmp/itest','any') RETURNING id)
		 INSERT INTO tasks (project_id, title, status) SELECT id, 'orchhealth fixture', 'ready' FROM p
		 RETURNING id`, ohSlug).Scan(&s.task); err != nil {
		t.Fatalf("seed project + task: %v", err)
	}
	return s
}

func (s *ohSuite) cleanup(t *testing.T, ctx context.Context) {
	t.Helper()
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug LIKE 'itest-orchhealth-%'))`
	for _, q := range []string{
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM tasks WHERE id IN ` + tasksOf,
		`DELETE FROM projects WHERE slug LIKE 'itest-orchhealth-%'`,
		`DROP SCHEMA IF EXISTS ` + ohShadowSchema + ` CASCADE`,
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

// holdLock: "orchestratord is running". Fails if someone else holds the key.
func (s *ohSuite) holdLock(t *testing.T, ctx context.Context) (release func()) {
	t.Helper()
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire lock conn: %v", err)
	}
	key := int64(orch.AdvisoryLockKey)
	var ok bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&ok); err != nil || !ok {
		conn.Release()
		t.Fatalf("take the orchestrator lock (ok=%v, err=%v) — an orchestratord running against :5433?", ok, err)
	}
	done := false
	release = func() {
		if done {
			return
		}
		done = true
		var unlocked bool
		_ = conn.QueryRow(context.Background(), `SELECT pg_advisory_unlock($1)`, key).Scan(&unlocked)
		conn.Release()
	}
	t.Cleanup(release)
	return release
}

// assertNotHeld proves "not running" really holds on the db (the key is free).
func (s *ohSuite) assertNotHeld(t *testing.T, ctx context.Context) {
	t.Helper()
	s.holdLock(t, ctx)() // take and immediately release
}

func (s *ohSuite) cursorToHead(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	var n int64
	if err := s.pool.QueryRow(ctx,
		`UPDATE orchestrator_cursor SET last_event_id=(SELECT COALESCE(max(id),0) FROM task_events), updated_at=now()
		 WHERE name='orchestrator' RETURNING last_event_id`).Scan(&n); err != nil {
		t.Fatalf("position cursor at head: %v", err)
	}
	return n
}

func (s *ohSuite) backdatedEvent(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO task_events (task_id, event_type, payload, created_at)
		 VALUES ($1,'log','{}', now() - interval '20 minutes') RETURNING id`, s.task).Scan(&id); err != nil {
		t.Fatalf("insert backdated event: %v", err)
	}
	return id
}

// orchestratorSection returns the /funnel Orchestrator section's markup, or "".
func orchestratorSection(body string) string {
	i := strings.Index(body, ohSectionHead)
	if i < 0 {
		return ""
	}
	rest := body[i+len(ohSectionHead):]
	if j := strings.Index(rest, "<h2"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func (s *ohSuite) pages(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (funnel, board string) {
	t.Helper()
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()
	code, funnel := get(t, client, ts.URL+"/funnel")
	if code != http.StatusOK {
		t.Fatalf("GET /funnel = %d\n%s", code, snippet(funnel))
	}
	code, board = get(t, client, ts.URL+"/tasks")
	if code != http.StatusOK {
		t.Fatalf("GET /tasks = %d — the board must never break on health\n%s", code, snippet(board))
	}
	return funnel, board
}

// ---- ok: lock held, backlog 0 ------------------------------------------------

func TestOrchestratorHealth_Integration_OkState_SectionOnFunnel_NoLineOnBoard(t *testing.T) {
	ctx := context.Background()
	s := newOHSuite(t, ctx)
	s.holdLock(t, ctx)
	head := s.cursorToHead(t, ctx)

	funnel, board := s.pages(t, ctx, s.pool)

	sec := orchestratorSection(funnel)
	if sec == "" {
		t.Fatalf("/funnel has no %s section (D5: verdict, backlog, oldest unprocessed, cursor, head)\n%s",
			ohSectionHead, snippet(funnel))
	}
	if !regexp.MustCompile(`\bok\b`).MatchString(sec) || strings.Contains(sec, "not_running") || strings.Contains(sec, "stalled") {
		t.Errorf("Orchestrator section does not read `ok` with the lock held and backlog 0:\n%s", sec)
	}
	if !strings.Contains(sec, strconv.FormatInt(head, 10)) {
		t.Errorf("Orchestrator section does not show the cursor/head value %d:\n%s", head, sec)
	}
	if strings.Contains(board, ohBoardMarker) {
		t.Errorf("/tasks renders the orchestrator health line while the verdict is ok; the red line is only "+
			"for a verdict that is not ok\n%s", snippet(board))
	}
}

// ---- not_running: nobody holds the lock ---------------------------------------

func TestOrchestratorHealth_Integration_NotRunning_RedLineOnBoard(t *testing.T) {
	ctx := context.Background()
	s := newOHSuite(t, ctx)
	s.assertNotHeld(t, ctx)
	s.cursorToHead(t, ctx) // backlog 0: not_running must show whatever the backlog

	funnel, board := s.pages(t, ctx, s.pool)

	if sec := orchestratorSection(funnel); !strings.Contains(sec, "not_running") {
		t.Errorf("Orchestrator section does not read not_running with the lock free:\n%s", sec)
	}
	i := strings.Index(board, ohBoardMarker)
	if i < 0 {
		t.Fatalf("/tasks has no %s line while nobody holds the orchestrator lock. D5: the board is where "+
			"Salvador looks — this is the silent two-month gap, made loud\n%s", ohBoardMarker, snippet(board))
	}
	if strings.Count(board, ohBoardMarker) != 1 {
		t.Errorf("/tasks renders %d health lines, want ONE", strings.Count(board, ohBoardMarker))
	}
	if line := board[i:min(len(board), i+400)]; !strings.Contains(line, "not_running") {
		t.Errorf("the board's health line does not name the verdict not_running:\n%s", line)
	}
}

// ---- stalled: lock held, oldest unprocessed 20m old ---------------------------

func TestOrchestratorHealth_Integration_Stalled_RedLineOnBoard(t *testing.T) {
	ctx := context.Background()
	s := newOHSuite(t, ctx)
	s.holdLock(t, ctx)
	cursor := s.cursorToHead(t, ctx)
	head := s.backdatedEvent(t, ctx)

	funnel, board := s.pages(t, ctx, s.pool)

	sec := orchestratorSection(funnel)
	if !strings.Contains(sec, "stalled") {
		t.Errorf("Orchestrator section does not read stalled (lock held, 1 event 20m past the cursor):\n%s", sec)
	}
	for _, v := range []int64{cursor, head} {
		if !strings.Contains(sec, strconv.FormatInt(v, 10)) {
			t.Errorf("Orchestrator section does not show %d (cursor %d, head %d):\n%s", v, cursor, head, sec)
		}
	}
	i := strings.Index(board, ohBoardMarker)
	if i < 0 {
		t.Fatalf("/tasks has no health line for a stalled orchestrator\n%s", snippet(board))
	}
	if line := board[i:min(len(board), i+400)]; !strings.Contains(line, "stalled") {
		t.Errorf("the board's health line does not name the verdict stalled:\n%s", line)
	}
}

// ---- a failing health query: inline on /funnel, nothing on /tasks -------------

func TestOrchestratorHealth_Integration_FailingQuery_InlineOnFunnel_NothingOnBoard(t *testing.T) {
	ctx := context.Background()
	s := newOHSuite(t, ctx)
	// Lock FREE on purpose: an implementation that turned a failed query into
	// not_running would render the red line here and fail the board half.
	s.assertNotHeld(t, ctx)

	if _, err := s.pool.Exec(ctx, `CREATE SCHEMA `+ohShadowSchema); err != nil {
		t.Fatalf("create shadow schema: %v", err)
	}
	// Same NAME, wrong shape: any read of last_event_id / updated_at fails.
	if _, err := s.pool.Exec(ctx, `CREATE TABLE `+ohShadowSchema+`.orchestrator_cursor (id BIGINT)`); err != nil {
		t.Fatalf("create shadow orchestrator_cursor: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, `SET search_path TO `+ohShadowSchema+`, public`)
		return err
	}
	shadow, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open shadow pool: %v", err)
	}
	defer shadow.Close()

	funnel, board := s.pages(t, ctx, shadow)

	inline := regexp.MustCompile(`section failed — [^<]*(?i:orchestrator)`)
	if !inline.MatchString(funnel) {
		t.Errorf("/funnel shows no inline `section failed — orchestrator…` line while the health query "+
			"fails (funnel criterion 17's shape)\n%s", snippet(funnel))
	}
	if !strings.Contains(funnel, "<h2>Connector health</h2>") {
		t.Errorf("the funnel's other sections stopped rendering because the health query failed\n%s", snippet(funnel))
	}
	if !strings.Contains(board, "<h1>Board</h1>") {
		t.Fatalf("/tasks did not render the board\n%s", snippet(board))
	}
	for _, leak := range []string{ohBoardMarker, "section failed", "last_event_id", "not_running"} {
		if strings.Contains(board, leak) {
			t.Errorf("/tasks contains %q while the health query fails; D5: the board shows NOTHING then "+
				"(no red line, no error)", leak)
		}
	}
}
