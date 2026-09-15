//go:build integration

package dashboard_test

// board-incoming-first (SWT-59, docs/tickets/board-incoming-first_SPEC.md)
// criteria 15-17, against a real database, the REAL dashboard.Server
// (dev-login), the real executor and the REAL policy matrix. Build-tagged
// `integration` AND env-gated on DATABASE_URL. NO LLM, NO network, NO
// orchestrator and NO promoter running: the message -> verdict -> promotion
// chain is seeded directly, in the shape of internal/promote's
// reopen_integration_test.go (message / verdict). Run it ONLY in an ISOLATED
// database (the IK 2026-09-12 landmine: the compose `ops` db is shared):
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_boardincoming?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run BoardIncoming ./internal/dashboard/
//
// Reuses dashGuard / dashPool / newDashServer / get / snippet
// (dashboard_integration_test.go), bdInsID / bdCount
// (board_dismiss_integration_test.go), lightsExecutor / lsSignal / boardLight /
// onBoard / lsDayStart (board_lights_integration_test.go) and layoutBoard /
// layoutSections / lyRender (board_layout_integration_test.go).
//
// CLEANUP PACT: owns project itest-incoming-proj, source_accounts provider
// itest-incoming, threads itest-incoming:%, ai_runs model itest-incoming.
// FK-ordered and rerunnable: classify_promotions and external_refs by task or
// message; policy_decisions / audit_events by task_id (the SWT-37 landmine);
// task_dismissals, task_claims, task_events, tasks; the message chain; the
// project. Fixture instants are SQL (lsDayStart), so it passes at any hour.
//
// "TEST THE COLUMN, NOT THE FIXTURE": both facts come only from Postgres
// (classify_promotions, external_refs); criterion 16 deletes each row and
// re-renders, so a SELECT column replaced with `false` turns this red.
//
// GREENFIELD NOTE, EXPECTED RED: the board has no incoming section, so the
// promoter rows render in queue / blocked / holding and the first section
// assertion fails.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - boardSections calls sectionFor -> SectionAboveBlocked (M-rows in queue/blocked/holding).
//   - the closed / done guard dropped -> SectionAboveBlocked (M4 in incoming).
//   - 'attached' in the action list -> SectionAboveBlocked (A1 in incoming).
//   - cp.task_id IS NOT NULL and the COALESCE both removed -> SectionAboveBlocked (C0 breaks the render).
//   - t.assignee_type = 'human' dropped -> SectionAboveBlocked (P2 in incoming).
//   - t.source_thread_id IS NOT NULL as the message fact -> SectionAboveBlocked (J1, S1 in incoming).
//   - from_message or pr_review replaced with false -> SectionAboveBlocked and its column-fed half.
//   - incoming by id ASC -> SectionAboveBlocked (M1 before M3).
//   - PRs before messages -> SectionAboveBlocked (P1 before M1).
//   - incoming after blocked in boardSectionOrder -> SectionAboveBlocked (section-incoming not first).

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	incSlug     = "itest-incoming-proj"
	incClient   = "itest-incoming-client"
	incProvider = "itest-incoming"
	incAccount  = "itest-incoming@local"
	incModel    = "itest-incoming"
)

func cleanupIncoming(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const projs = `(SELECT id FROM projects WHERE slug = '` + incSlug + `')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	const accts = `(SELECT id FROM source_accounts WHERE provider = '` + incProvider + `')`
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`
	const msgs = `(SELECT id FROM normalized_messages WHERE raw_source_item_id IN ` + raws + `)`
	for _, q := range []string{
		`DELETE FROM classify_promotions WHERE task_id IN ` + tasksOf + ` OR normalized_message_id IN ` + msgs +
			` OR project_id IN ` + projs,
		`DELETE FROM external_refs WHERE task_id IN ` + tasksOf,
		`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + `)`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'itest-incoming:%'`,
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN ` + raws +
			` OR ai_run_id IN (SELECT id FROM ai_runs WHERE model = '` + incModel + `')`,
		`DELETE FROM ai_runs WHERE model = '` + incModel + `'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider = '` + incProvider + `'`,
		`DELETE FROM projects WHERE slug = '` + incSlug + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

type incFixture struct {
	pool          *pgxpool.Pool
	project, acct int64
}

// thread seeds a normalized_threads row keyed itest-incoming:<label>, holding
// one inbound message on channel; it returns the thread, the message, its raw row.
func (f *incFixture) thread(t *testing.T, ctx context.Context, label, channel string) (thread, msg, raw int64) {
	t.Helper()
	thread = bdInsID(t, ctx, f.pool,
		`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,'itest-incoming','[]') RETURNING id`,
		"itest-incoming:"+label)
	raw = bdInsID(t, ctx, f.pool,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,'{}',$3, now()) RETURNING id`, f.acct, "itest-incoming-"+label, "itest-incoming-h-"+label)
	msg = bdInsID(t, ctx, f.pool,
		`INSERT INTO normalized_messages (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		                                  body_text, subject, sender, channel)
		 VALUES ($1,$2,'inbound',$3, now(),'itest-incoming body','itest-incoming','Client <client@example.test>',$4) RETURNING id`,
		raw, thread, "<itest-incoming-"+label+"@mail.example>", channel)
	return thread, msg, raw
}

// promotion seeds the classify verdict (ai_runs + ai_extractions) for a fresh
// message on its own thread, and the classify_promotions row naming task (0 =
// task_id NULL, the crash artifact). It returns the message's thread.
func (f *incFixture) promotion(t *testing.T, ctx context.Context, label, channel, action string, task int64) int64 {
	t.Helper()
	thread, msg, raw := f.thread(t, ctx, label, channel)
	run := bdInsID(t, ctx, f.pool,
		`INSERT INTO ai_runs (worker_type, provider, model, input, output, status)
		 VALUES ('classify','itest-incoming',$1,'{}','{}','ok') RETURNING id`, incModel)
	ext := bdInsID(t, ctx, f.pool,
		`INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields) VALUES ($1,$2,'{"actionable":true}') RETURNING id`, run, raw)
	var taskID any
	if task != 0 {
		taskID = task
	}
	bdInsID(t, ctx, f.pool,
		`INSERT INTO classify_promotions (normalized_message_id, raw_source_item_id, ai_extraction_id, project_id,
		                                  kind, action, task_id, reason)
		 VALUES ($1,$2,$3,$4,'itest-incoming',$5,$6,'itest-incoming') RETURNING id`,
		msg, raw, ext, f.project, action, taskID)
	return thread
}

func (f *incFixture) task(t *testing.T, ctx context.Context, title, assignee, status string, priority int) int64 {
	t.Helper()
	return bdInsID(t, ctx, f.pool,
		`INSERT INTO tasks (project_id, title, body, assignee_type, status, priority) VALUES ($1,$2,'',$3,$4,$5) RETURNING id`,
		f.project, title, assignee, status, priority)
}

func (f *incFixture) exec(t *testing.T, ctx context.Context, q string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(ctx, q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

type incSeed struct{ j1, m1, m2, m3, p1, p2, a1, s1, m4 int64 }

func (s incSeed) names() map[int64]string {
	return map[int64]string{s.j1: "J1", s.m1: "M1", s.m2: "M2", s.m3: "M3", s.p1: "P1", s.p2: "P2", s.a1: "A1", s.s1: "S1", s.m4: "M4"}
}

// seedIncoming is criterion 15's table.
func seedIncoming(t *testing.T, ctx context.Context, pool *pgxpool.Pool) incSeed {
	t.Helper()
	f := &incFixture{pool: pool}
	f.project = bdInsID(t, ctx, pool,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,$2,'manual','dashboard','/tmp/itest-incoming','any') RETURNING id`, incSlug, incClient)
	f.acct = bdInsID(t, ctx, pool,
		`INSERT INTO source_accounts (provider, account_email, send_enabled) VALUES ($1,$2,false) RETURNING id`,
		incProvider, incAccount)

	var s incSeed
	// J1 FIRST: a Jira ticket's task, raised by a Jira notification mail (the
	// capture shape: a jira ref plus source_thread_id on a gmail thread), no
	// promotion. Priority 1 makes it the human lane's blue head.
	s.j1 = f.task(t, ctx, "INCOMING-J1", "human", "ready", 1)
	jThread, _, _ := f.thread(t, ctx, "j1-jira-notification", "gmail")
	f.exec(t, ctx, `UPDATE tasks SET source_thread_id=$2 WHERE id=$1`, s.j1, jThread)
	f.exec(t, ctx, `INSERT INTO external_refs (task_id, system, external_key) VALUES ($1,'jira','ITINC-1')`, s.j1)

	// Promoter tasks carry source_thread_id too (task_set_source_thread): the
	// production shape.
	promoted := func(title, status, label, channel, action string) int64 {
		id := f.task(t, ctx, title, "human", status, 0)
		th := f.promotion(t, ctx, label, channel, action, id)
		f.exec(t, ctx, `UPDATE tasks SET source_thread_id=$2 WHERE id=$1`, id, th)
		return id
	}
	s.m1 = promoted("INCOMING-M1", "ready", "m1", "gmail", "task")
	s.m2 = promoted("INCOMING-M2", "holding", "m2", "slack", "review")
	s.m3 = promoted("INCOMING-M3", "blocked", "m3", "gmail", "task") // created after M1

	s.p1 = f.task(t, ctx, "INCOMING-P1", "human", "ready", 0)
	f.exec(t, ctx, `INSERT INTO external_refs (task_id, system, external_key) VALUES ($1,'github','itest-incoming/repo#1')`, s.p1)
	s.p2 = f.task(t, ctx, "INCOMING-P2", "claude", "in_progress", 0) // a worker's own PR
	f.exec(t, ctx, `INSERT INTO external_refs (task_id, system, external_key) VALUES ($1,'github','itest-incoming/repo#2')`, s.p2)

	s.a1 = f.task(t, ctx, "INCOMING-A1", "human", "ready", 0)
	f.promotion(t, ctx, "a1-attached", "gmail", "attached", s.a1) // created nothing: NOT incoming

	s.s1 = f.task(t, ctx, "INCOMING-S1", "human", "ready", 0)
	sThread, _, _ := f.thread(t, ctx, "s1-thread-only", "gmail")
	f.exec(t, ctx, `UPDATE tasks SET source_thread_id=$2 WHERE id=$1`, s.s1, sThread)

	s.m4 = bdInsID(t, ctx, pool, `INSERT INTO tasks (project_id, title, body, assignee_type, status, priority, closed_at)
		VALUES ($1,'INCOMING-M4','','human','closed',0, `+lsDayStart+` + interval '1 hour') RETURNING id`, f.project)
	f.promotion(t, ctx, "m4", "gmail", "task", s.m4)

	// C0: the crash artifact, a promotion with task_id NULL on its own message.
	f.promotion(t, ctx, "c0-crash-artifact", "gmail", "task", 0)

	lsSignal(t, ctx, lightsExecutor(pool), s.m2, "needs_input")
	return s
}

func incSection(secs []lySection, key string) (lySection, bool) {
	for _, s := range secs {
		if s.key == key {
			return s, true
		}
	}
	return lySection{}, false
}

func incNames(ids []int64, names map[int64]string) string {
	var out []string
	for _, id := range ids {
		n, ok := names[id]
		if !ok {
			n = strconv.FormatInt(id, 10)
		}
		out = append(out, n)
	}
	return strings.Join(out, " ")
}

// assertIncomingQueue checks queue = {want...} as a set, with J1 first (its
// tail is TaskQueueOrder's, not this ticket's).
func assertIncomingQueue(t *testing.T, step string, secs []lySection, names map[int64]string, j1 int64, want ...int64) {
	t.Helper()
	q, ok := incSection(secs, "queue")
	if !ok {
		t.Errorf("%s: no queue section (%s)", step, lyRender(secs, names))
		return
	}
	got := append([]int64(nil), q.ids...)
	w := append([]int64(nil), want...)
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	sort.Slice(w, func(i, j int) bool { return w[i] < w[j] })
	if incNames(got, names) != incNames(w, names) || q.count != len(want) {
		t.Errorf("%s: queue(%d) = [%s], want exactly {%s}", step, q.count, incNames(q.ids, names), incNames(w, names))
	}
	if len(q.ids) == 0 || q.ids[0] != j1 {
		t.Errorf("%s: queue = [%s], want J1 (the blue head) first", step, incNames(q.ids, names))
	}
}

func assertIncomingIDs(t *testing.T, step string, secs []lySection, names map[int64]string, want ...int64) {
	t.Helper()
	s, ok := incSection(secs, "incoming")
	if !ok {
		t.Errorf("%s: no section-incoming (sections: %s)", step, lyRender(secs, names))
		return
	}
	if got, w := incNames(s.ids, names), incNames(want, names); got != w || s.count != len(want) || s.title != "incoming" {
		t.Errorf("%s: incoming/%s(%d) = [%s], want incoming/incoming(%d) = [%s]", step, s.title, s.count, got, len(want), w)
	}
}

// ---- criteria 15 and 16 -----------------------------------------------------------------

func TestBoardIncoming_Integration_SectionAboveBlocked(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupIncoming(t, ctx, pool)
	defer cleanupIncoming(t, ctx, pool)
	s := seedIncoming(t, ctx, pool)
	names := s.names()
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	if n := bdCount(t, ctx, pool, `SELECT count(*) FROM classify_promotions WHERE task_id IS NULL
		AND project_id = (SELECT id FROM projects WHERE slug = $1)`, incSlug); n != 1 {
		t.Fatalf("CONTROL: %d crash-artifact promotions (task_id NULL) seeded, want 1; the NULL-safety check would be vacuous", n)
	}

	body := layoutBoard(t, client, ts.URL, "project="+incSlug) // fatal unless HTTP 200, C0 present
	secs := layoutSections(body)
	var shape []string
	for _, sec := range secs {
		shape = append(shape, sec.key+"/"+sec.title+"("+strconv.Itoa(sec.count)+")")
	}
	if got, want := strings.Join(shape, " | "), "incoming/incoming(4) | in_flight/in flight(1) | queue/queue(3) | done/done(1)"; got != want {
		t.Errorf("sections =\n  %s\nwant\n  %s\n(criterion 15; full: %s)", got, want, lyRender(secs, names))
	}
	assertIncomingIDs(t, "default board", secs, names, s.m2, s.m3, s.m1, s.p1)
	if f, _ := incSection(secs, "in_flight"); incNames(f.ids, names) != "P2" {
		t.Errorf("in_flight = [%s], want [P2]: a claude task's own PR is not waiting on his review (I2)", incNames(f.ids, names))
	}
	if d, _ := incSection(secs, "done"); incNames(d.ids, names) != "M4" {
		t.Errorf("done = [%s], want [M4]: a finished incoming task stays in done (I4, I8)", incNames(d.ids, names))
	}
	assertIncomingQueue(t, "default board", secs, names, s.j1, s.j1, s.a1, s.s1)
	if strings.Contains(body, `id="section-blocked"`) {
		t.Errorf("the board renders section-blocked; M3 (status blocked) belongs to incoming (criterion 15)")
	}
	if strings.Contains(body, `id="section-holding"`) {
		t.Errorf("the board renders section-holding; M2 (status holding) belongs to incoming (criterion 15)")
	}
	if inc, h2 := strings.Index(body, `<h2 id="section-incoming">`), strings.Index(body, "<h2"); inc < 0 || inc != h2 {
		t.Errorf("section-incoming (at %d) is not the first <h2 (at %d) on the page (criterion 15, I3)", inc, h2)
	}
	for id, name := range names {
		re := regexp.MustCompile(`<span class="light light-[a-z]+" role="img" aria-label="[^"]*" title="[^"]*"></span>\s*` +
			`<a href="/tasks/` + strconv.FormatInt(id, 10) + `">`)
		if n := len(re.FindAllString(body, -1)); n != 1 {
			t.Errorf("row %s (task %d) renders %d times, want exactly once (criterion 15: the partition holds)", name, id, n)
		}
	}
	for _, row := range []struct {
		id    int64
		class string
	}{{s.m2, "input"}, {s.m3, "none"}, {s.m1, "none"}, {s.p1, "none"}, {s.j1, "next"}, {s.p2, "working"}, {s.m4, "done"},
		{s.a1, "none"}, {s.s1, "none"}} {
		if c, _, ok := boardLight(body, row.id); !ok || c != row.class {
			t.Errorf("row %s light = %q (found %v), want %q (criterion 15: incoming keeps each row's light)", names[row.id], c, ok, row.class)
		}
	}

	// Criterion 16: the facts come from the columns. Delete each provenance row
	// and re-render.
	if _, err := pool.Exec(ctx, `DELETE FROM classify_promotions WHERE task_id = $1`, s.m1); err != nil {
		t.Fatalf("delete M1's promotion: %v", err)
	}
	secs = layoutSections(layoutBoard(t, client, ts.URL, "project="+incSlug))
	assertIncomingIDs(t, "M1's promotion deleted", secs, names, s.m2, s.m3, s.p1)
	assertIncomingQueue(t, "M1's promotion deleted", secs, names, s.j1, s.j1, s.m1, s.a1, s.s1)

	if _, err := pool.Exec(ctx, `DELETE FROM external_refs WHERE task_id = $1`, s.p1); err != nil {
		t.Fatalf("delete P1's github ref: %v", err)
	}
	secs = layoutSections(layoutBoard(t, client, ts.URL, "project="+incSlug))
	assertIncomingIDs(t, "P1's github ref deleted", secs, names, s.m2, s.m3)
	assertIncomingQueue(t, "P1's github ref deleted", secs, names, s.j1, s.j1, s.m1, s.p1, s.a1, s.s1)
}

// ---- criterion 17 -----------------------------------------------------------------------

func TestBoardIncoming_Integration_StatusFilterKeepsGrouping(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupIncoming(t, ctx, pool)
	defer cleanupIncoming(t, ctx, pool)
	s := seedIncoming(t, ctx, pool)
	names := s.names()
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	secs := layoutSections(layoutBoard(t, client, ts.URL, "project="+incSlug+"&status=ready"))
	var keys []string
	for _, sec := range secs {
		keys = append(keys, sec.key)
	}
	if strings.Join(keys, ",") != "incoming,queue" {
		t.Errorf("?status=ready sections = %v, want [incoming queue] (criterion 17, L3: a filter narrows; the grouping "+
			"still applies): %s", keys, lyRender(secs, names))
	}
	assertIncomingIDs(t, "?status=ready", secs, names, s.m1, s.p1)
	assertIncomingQueue(t, "?status=ready", secs, names, s.j1, s.j1, s.a1, s.s1)

	closed := layoutSections(layoutBoard(t, client, ts.URL, "project="+incSlug+"&status=closed"))
	if got := lyRender(closed, names); got != "done/done(1)=[M4]" {
		t.Errorf("?status=closed sections = %s, want done/done(1)=[M4] (criterion 17)", got)
	}
}
