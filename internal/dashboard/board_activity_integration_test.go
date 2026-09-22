//go:build integration

package dashboard_test

// activity-resurfaces (SWT-72, docs/tickets/activity-resurfaces_SPEC.md)
// criteria 40, 41 and 43, against a real database, the REAL dashboard.Server
// (dev-login), the real executor and the REAL policy matrix.
//
// The trigger, end to end: an inbound comment lands on an open `ready` human
// task sitting in QUEUE; the task moves to INCOMING with remark `new comment`
// and the sender in its title cell; `actions` -> Requeue with
// `priority: unchanged` drops it back to QUEUE with its priority intact; and a
// `holding` row Requeued lands in QUEUE as READY (OQ-1 = B).
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_swt72?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run BoardActivity ./internal/dashboard/
//
// USE AN ISOLATED DATABASE (IK 2026-09-12: the compose `ops` db is shared by
// every worktree and agent). Build-tagged `integration`, env-gated on
// DATABASE_URL, FATAL on 192.168.50.49 (dashGuard).
//
// "TEST THE COLUMN, NOT THE FIXTURE" (IK: test-the-column-not-the-fixture). The
// fixture supplies NEITHER the section nor the remark nor the sender: all three
// come from tasks.activity_at / reviewed_at and a PK join to
// normalized_messages, and criterion 41 MUTATES THOSE COLUMNS between renders
// with nothing else changing —
//
//	UPDATE tasks SET reviewed_at = now()  -> the row leaves INCOMING
//	UPDATE tasks SET activity_at = now()  -> it comes back
//
// so dropping `reviewed_at` (or `activity_at`) from boardLightFacts' SELECT, or
// replacing the needs_review expression with `false`, turns this red. The mark
// itself is made through the EXECUTOR (task_mark_activity), never raw SQL.
//
// Reuses dashGuard / dashPool / newDashServer / get / snippet
// (dashboard_integration_test.go), bdInsID / bdCount
// (board_dismiss_integration_test.go), lightsExecutor
// (board_lights_integration_test.go) and layoutBoard / layoutSections / lyRender
// (board_layout_integration_test.go).
//
// GREENFIELD NOTE, EXPECTED RED: migration 0039 is not applied (baRequire0039
// fails every test with one sentence), then task_mark_activity and task_requeue
// are not registered.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - replace the needs_review expression with false -> SurfacesAnOpenTaskIntoIncoming, ColumnFed.
//   - drop the nm PK join -> the remark and sender assertions.
//   - task_requeue accepts a closed task -> RequeueRefusesAClosedTask.
//   - task_requeue lifts a status other than holding -> RequeueLiftsHoldingToReady.
//   - remove task_requeue from humanOnly -> RequeueIsHumanOnly.
//
// CLEANUP PACT: owns project itest-activity-proj, source_accounts provider
// itest-activity, threads itest-activity:%. FK-ordered and rerunnable, at start
// AND end; tasks go BEFORE messages (activity_by_message_id is SET NULL, but
// the dependency is real).

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
)

const (
	baSlug     = "itest-activity-proj"
	baClient   = "itest-activity-client"
	baProvider = "itest-activity"
	baAccount  = "itest-activity@local"
	baCapture  = "capture:itest-activity" // the capture:{connector} shape
	baKatie    = "Katie Evans (JIRA) <jira@activity.example>"
	baJose     = "José Garcia <jose.g@avviato.example>"
)

func baRequire0039(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name='tasks' AND column_name IN ('activity_at','activity_by_message_id','reviewed_at')`).Scan(&n); err != nil {
		t.Fatalf("probe 0039's columns: %v", err)
	}
	if n != 3 {
		t.Fatalf("found %d of the 3 columns migration 0039 adds. Criterion 1: apply "+
			"migrations/0039_task_activity_review.sql (`make migrate LOCAL_DB_URL=...`); the new dashboard "+
			"SELECTS them on every /tasks render (Verification Step 5's barrier)", n)
	}
}

func cleanupActivity(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const projs = `(SELECT id FROM projects WHERE slug = '` + baSlug + `')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	const accts = `(SELECT id FROM source_accounts WHERE provider = '` + baProvider + `')`
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`
	for _, q := range []string{
		`DELETE FROM classify_promotions WHERE task_id IN ` + tasksOf + ` OR project_id IN ` + projs,
		`DELETE FROM external_refs WHERE task_id IN ` + tasksOf,
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor LIKE 'itest-activity%'
		      OR actor = '` + baCapture + `')`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor LIKE 'itest-activity%'
		   OR actor = '` + baCapture + `'`,
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'itest-activity:%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider = '` + baProvider + `'`,
		`DELETE FROM projects WHERE slug = '` + baSlug + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

type baFixture struct {
	pool          *pgxpool.Pool
	ex            *executor.Executor
	project, acct int64
	seq           int
}

func newBAFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *baFixture {
	t.Helper()
	f := &baFixture{pool: pool, ex: lightsExecutor(pool)}
	f.project = bdInsID(t, ctx, pool,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,$2,'manual','dashboard','/tmp/itest-activity','any') RETURNING id`, baSlug, baClient)
	f.acct = bdInsID(t, ctx, pool,
		`INSERT INTO source_accounts (provider, account_email, send_enabled) VALUES ($1,$2,false) RETURNING id`,
		baProvider, baAccount)
	return f
}

func (f *baFixture) task(t *testing.T, ctx context.Context, title, assignee, status string, priority int) int64 {
	t.Helper()
	return bdInsID(t, ctx, f.pool,
		`INSERT INTO tasks (project_id, title, body, assignee_type, status, priority)
		 VALUES ($1,$2,'',$3,$4,$5) RETURNING id`, f.project, title, assignee, status, priority)
}

// message seeds one INBOUND normalized message on its own thread, with the
// channel and sender the board is going to read back through the PK join.
func (f *baFixture) message(t *testing.T, ctx context.Context, label, channel, sender string) int64 {
	t.Helper()
	f.seq++
	tag := label + "-" + strconv.Itoa(f.seq)
	raw := bdInsID(t, ctx, f.pool,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,'{}',$3, now()) RETURNING id`, f.acct, "itest-activity-"+tag, "itest-activity-h-"+tag)
	th := bdInsID(t, ctx, f.pool,
		`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,'itest-activity','[]') RETURNING id`,
		"itest-activity:"+tag)
	return bdInsID(t, ctx, f.pool,
		`INSERT INTO normalized_messages (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		                                  body_text, subject, sender, channel)
		 VALUES ($1,$2,'inbound',$3, now(),'itest-activity body','itest-activity',$4,$5) RETURNING id`,
		raw, th, "<itest-activity-"+tag+"@mail.example>", sender, channel)
}

// mark is the ONE way activity gets onto a task here: through the executor, as
// capture does (invariant 3, and the SPEC's smoke step says the same — "seed
// through the executor, never raw SQL").
func (f *baFixture) mark(t *testing.T, ctx context.Context, task, msg int64) {
	t.Helper()
	args := `{"task_id":` + strconv.FormatInt(task, 10) + `,"message_id":` + strconv.FormatInt(msg, 10) +
		`,"reason":"capture: itest-activity"}`
	res, err := f.ex.Execute(ctx, executor.Call{Tool: "task_mark_activity", Actor: baCapture,
		Args: []byte(args), TaskID: &task})
	if err != nil {
		t.Fatalf("task_mark_activity(task %d, message %d) as %s: %v (criteria 2, 7)", task, msg, baCapture, err)
	}
	if !strings.Contains(string(res.Output), `"marked":true`) &&
		!strings.Contains(string(res.Output), `"marked": true`) {
		t.Fatalf("task_mark_activity returned %s, want marked:true", res.Output)
	}
}

func (f *baFixture) exec(t *testing.T, ctx context.Context, q string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(ctx, q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// baSection returns the named section's row ids on a rendered board.
func baSection(secs []lySection, key string) (lySection, bool) {
	for _, s := range secs {
		if s.key == key {
			return s, true
		}
	}
	return lySection{}, false
}

func baIn(secs []lySection, key string, id int64) bool {
	s, ok := baSection(secs, key)
	if !ok {
		return false
	}
	for _, v := range s.ids {
		if v == id {
			return true
		}
	}
	return false
}

// baRequeue posts the board's per-row Requeue form and returns the flash the
// redirect carries. Nothing is faked: the route, the executor, the real policy
// matrix and the handler.
func baRequeue(t *testing.T, client *http.Client, base string, taskID int64, priority, note string) string {
	t.Helper()
	resp, err := client.PostForm(base+"/tasks/"+strconv.FormatInt(taskID, 10)+"/requeue",
		url.Values{"priority": {priority}, "note": {note}, "project": {baSlug}})
	if err != nil {
		t.Fatalf("POST /tasks/%d/requeue: %v", taskID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasSuffix(resp.Request.URL.Path, "/tasks") {
		t.Fatalf("POST /tasks/%d/requeue landed on %s with status %d, want /tasks (200 after the 303). "+
			"Criterion 33: the route is POST /tasks/{id}/requeue on the AUTH-REQUIRED mux, redirecting 303 "+
			"back to the filtered board with a flash", taskID, resp.Request.URL, resp.StatusCode)
	}
	return resp.Request.URL.Query().Get("flash")
}

// ---- criteria 40 and 41 ------------------------------------------------------------

func TestBoardActivity_Integration_SurfacesAnOpenTaskIntoIncoming(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	baRequire0039(t, ctx, pool)
	cleanupActivity(t, ctx, pool)
	defer cleanupActivity(t, ctx, pool)

	f := newBAFixture(t, ctx, pool)
	// The trigger's own row: #73, ready, human, priority 0, sitting in QUEUE.
	jira := f.task(t, ctx, "ACTIVITY-JIRA", "human", "ready", 0)
	// Shape 2: José's DIRECT mail, no Jira in the path.
	mail := f.task(t, ctx, "ACTIVITY-MAIL", "human", "ready", 0)
	// Shape 4: Slack.
	slack := f.task(t, ctx, "ACTIVITY-SLACK", "human", "ready", 0)
	// A control row with no activity at all: it must stay in QUEUE throughout.
	quiet := f.task(t, ctx, "ACTIVITY-QUIET", "human", "ready", 0)

	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	// BEFORE: everything is in QUEUE. The rows are identical apart from the
	// activity that is about to land on three of them.
	before := layoutSections(layoutBoard(t, client, ts.URL, "project="+baSlug))
	for _, id := range []int64{jira, mail, slack, quiet} {
		if !baIn(before, "queue", id) {
			t.Fatalf("CONTROL: task %d is not in QUEUE before any activity (%s)", id, lyRender(before, nil))
		}
	}
	if _, ok := baSection(before, "incoming"); ok {
		t.Fatalf("CONTROL: the board already renders an incoming section before any activity (%s)",
			lyRender(before, nil))
	}

	f.mark(t, ctx, jira, f.message(t, ctx, "jira", "jira", baKatie))
	f.mark(t, ctx, mail, f.message(t, ctx, "mail", "gmail", baJose))
	f.mark(t, ctx, slack, f.message(t, ctx, "slack", "slack", baJose))

	body := layoutBoard(t, client, ts.URL, "project="+baSlug)
	secs := layoutSections(body)
	for _, id := range []int64{jira, mail, slack} {
		if !baIn(secs, "incoming", id) {
			t.Errorf("task %d is not in section-incoming after inbound activity (%s). Criterion 40: \"when there "+
				"is a comment we need to move to incoming to review\"", id, lyRender(secs, nil))
		}
		if baIn(secs, "queue", id) {
			t.Errorf("task %d is STILL in QUEUE after activity; a row lives in exactly one section", id)
		}
	}
	if !baIn(secs, "queue", quiet) {
		t.Errorf("the quiet control row left QUEUE (%s); only rows with unreviewed activity surface", lyRender(secs, nil))
	}

	// The remark words come from the message's CHANNEL, and the sender from the
	// PK join — neither is in the fixture's task row.
	for _, c := range []struct {
		id     int64
		remark string
		sender string
	}{
		{jira, "new comment", "Katie Evans (JIRA)"},
		{mail, "new email", "José Garcia"},
		{slack, "new slack", "José Garcia"},
	} {
		row := boardRow(body, c.id)
		if row == "" {
			t.Errorf("task %d renders no row", c.id)
			continue
		}
		if !strings.Contains(row, c.remark) {
			t.Errorf("task %d's row does not carry the remark %q (criteria 31, 40 — the channel decides the "+
				"words):\n%s", c.id, c.remark, snippet(row))
		}
		if !strings.Contains(row, c.sender) {
			t.Errorf("task %d's row does not carry the sender %q in its title cell. D5: the sender is how he "+
				"recognises Katie's comment, José's mail, or his own `Anonymous (JIRA)` edit, and Requeues "+
				"that one in a tap (D10):\n%s", c.id, c.sender, snippet(row))
		}
	}

	// Criterion 40's audit half: the mark is an executor call with an audit row,
	// and surfaced_at is STILL NULL (D1 / criterion 26).
	if n := bdCount(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE tool='task_mark_activity'
	                                AND actor=$1 AND status='ok' AND task_id=$2`, baCapture, jira); n != 1 {
		t.Errorf("task_mark_activity audit rows for task %d = %d, want 1 (invariant 3)", jira, n)
	}
	if n := bdCount(t, ctx, pool, `SELECT count(*) FROM tasks WHERE id=ANY($1) AND surfaced_at IS NOT NULL`,
		[]int64{jira, mail, slack}); n != 0 {
		t.Errorf("%d of the surfaced tasks carry surfaced_at; D1: the SWT-45 column is NOT reused, so a done "+
			"ticket's task still closes (criterion 26)", n)
	}

	// ---- criterion 41: COLUMN-FED. Nothing but the columns changes. ------------

	f.exec(t, ctx, `UPDATE tasks SET reviewed_at = now() WHERE id=$1`, jira)
	afterReview := layoutSections(layoutBoard(t, client, ts.URL, "project="+baSlug))
	if baIn(afterReview, "incoming", jira) || !baIn(afterReview, "queue", jira) {
		t.Errorf("after `UPDATE tasks SET reviewed_at = now()` task %d is still in INCOMING (%s). Criterion 41: "+
			"needs_review is COALESCE(activity_at IS NOT NULL AND (reviewed_at IS NULL OR activity_at > "+
			"reviewed_at), false) — replacing that expression with `false`, or dropping reviewed_at from the "+
			"SELECT, turns this red and the fixture supplies neither", jira, lyRender(afterReview, nil))
	}
	if !baIn(afterReview, "incoming", mail) || !baIn(afterReview, "incoming", slack) {
		t.Errorf("reviewing task %d moved the OTHER rows too (%s); the flag is per row", jira, lyRender(afterReview, nil))
	}

	f.exec(t, ctx, `UPDATE tasks SET activity_at = now() WHERE id=$1`, jira)
	afterNew := layoutSections(layoutBoard(t, client, ts.URL, "project="+baSlug))
	if !baIn(afterNew, "incoming", jira) {
		t.Errorf("after `UPDATE tasks SET activity_at = now()` task %d did not come back to INCOMING (%s). D2: "+
			"reviewed_at is MONOTONE, so a LATER activity re-surfaces the row without a reset", jira,
			lyRender(afterNew, nil))
	}

	// ---- criterion 40's second half: Requeue from the board --------------------

	f.exec(t, ctx, `UPDATE tasks SET priority=2 WHERE id=$1`, mail)
	flash := baRequeue(t, client, ts.URL, mail, "", "not mine today") // priority: unchanged
	if flash == "" {
		t.Errorf("Requeue produced no flash; the board's verbs all report what they did")
	}
	secs = layoutSections(layoutBoard(t, client, ts.URL, "project="+baSlug))
	if baIn(secs, "incoming", mail) || !baIn(secs, "queue", mail) {
		t.Errorf("after Requeue task %d is not back in QUEUE (%s). \"even if the review is to put it back on "+
			"the q\"", mail, lyRender(secs, nil))
	}
	var prio int
	if err := pool.QueryRow(ctx, `SELECT priority FROM tasks WHERE id=$1`, mail).Scan(&prio); err != nil {
		t.Fatalf("read priority: %v", err)
	}
	if prio != 2 {
		t.Errorf("Requeue with `priority: unchanged` left priority=%d, want 2. D6: the select's first option is "+
			"empty-valued and OMITTED from the args; a default of 0 would silently demote an elevated task", prio)
	}
	if n := bdCount(t, ctx, pool, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='reviewed'`,
		mail); n != 1 {
		t.Errorf("`reviewed` events on task %d = %d, want 1 (D6 step 5)", mail, n)
	}
	if n := bdCount(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool='task_requeue'
	                                AND actor LIKE 'dashboard:%' AND status='ok'`, mail); n != 1 {
		t.Errorf("task_requeue audit rows as dashboard:… for task %d = %d, want 1 (invariant 3, criterion 40)", mail, n)
	}

	// A second tap is a clean no-op (IK: the owner works the board concurrently).
	baRequeue(t, client, ts.URL, mail, "", "")
	secs = layoutSections(layoutBoard(t, client, ts.URL, "project="+baSlug))
	if !baIn(secs, "queue", mail) {
		t.Errorf("a second Requeue moved task %d out of QUEUE (%s)", mail, lyRender(secs, nil))
	}

	// Requeue with an explicit priority DOES apply it (the ▲ mark's half).
	baRequeue(t, client, ts.URL, slack, "3", "urgent after all")
	if err := pool.QueryRow(ctx, `SELECT priority FROM tasks WHERE id=$1`, slack).Scan(&prio); err != nil {
		t.Fatalf("read priority: %v", err)
	}
	if prio != 3 {
		t.Errorf("Requeue with priority 3 left priority=%d, want 3 (D6 step 4: raising is allowed)", prio)
	}
	if !regexp.MustCompile(`priority 3`).MatchString(boardRow(layoutBoard(t, client, ts.URL, "project="+baSlug), slack)) {
		t.Errorf("task %d's row shows no priority-3 mark after Requeue (SWT-67 B8's ▲)", slack)
	}
}

// ---- criterion 43 --------------------------------------------------------------

func TestBoardActivity_Integration_RequeueLiftsHoldingToReady(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	baRequire0039(t, ctx, pool)
	cleanupActivity(t, ctx, pool)
	defer cleanupActivity(t, ctx, pool)

	f := newBAFixture(t, ctx, pool)
	// The inquiry review lane's exit that it has never had — which matters now
	// that D11 makes every ask a `holding` task of its own (OQ-1 = B).
	held := f.task(t, ctx, "ACTIVITY-HOLD", "human", "holding", 0)
	f.mark(t, ctx, held, f.message(t, ctx, "hold", "slack", baJose))

	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	secs := layoutSections(layoutBoard(t, client, ts.URL, "project="+baSlug))
	if !baIn(secs, "incoming", held) {
		t.Fatalf("CONTROL: the holding task is not in INCOMING after activity (%s)", lyRender(secs, nil))
	}

	baRequeue(t, client, ts.URL, held, "", "back on the queue")

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, held).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "ready" {
		t.Errorf("Requeue left the holding task in %q, want ready (OQ-1 = B: \"allow for claude to requeue on "+
			"low priority\", and it gives the inquiry review lane the exit it has never had)", status)
	}
	if n := bdCount(t, ctx, pool, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='status_changed'
	                                AND payload->>'from'='holding' AND payload->>'to'='ready'
	                                AND payload->>'rule'='requeue'`, held); n != 1 {
		t.Errorf("status_changed {holding->ready, rule:requeue} events = %d, want 1 (D6 step 3)", n)
	}
	secs = layoutSections(layoutBoard(t, client, ts.URL, "project="+baSlug))
	if !baIn(secs, "queue", held) || baIn(secs, "holding", held) || baIn(secs, "incoming", held) {
		t.Errorf("after Requeue the holding row is not in QUEUE (%s); it went holding -> ready AND its review "+
			"flag was cleared", lyRender(secs, nil))
	}
}

// Criterion 43: a CLOSED task errors by name — task_reopen is the verb for that.
func TestBoardActivity_Integration_RequeueRefusesAClosedTask(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	baRequire0039(t, ctx, pool)
	cleanupActivity(t, ctx, pool)
	defer cleanupActivity(t, ctx, pool)

	f := newBAFixture(t, ctx, pool)
	closed := f.task(t, ctx, "ACTIVITY-CLOSED", "human", "closed", 0)
	task := closed
	_, err := f.ex.Execute(ctx, executor.Call{Tool: "task_requeue", Actor: "dashboard:itest-activity",
		Args: []byte(`{"task_id":` + strconv.FormatInt(closed, 10) + `}`), TaskID: &task})
	if err == nil {
		t.Fatalf("task_requeue on a closed task succeeded; D6 step 1 refuses it BY NAME")
	}
	if !strings.Contains(err.Error(), "task_reopen") {
		t.Errorf("task_requeue on a closed task = %q, want a refusal naming task_reopen (criterion 43)", err)
	}
}

// Criterion 43: as a WORKER console the verb is denied `human_only`, with a
// policy_decisions row — "workers never choose their own work", and this verb
// can lift a status and raise a priority.
func TestBoardActivity_Integration_RequeueIsHumanOnly(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	baRequire0039(t, ctx, pool)
	cleanupActivity(t, ctx, pool)
	defer cleanupActivity(t, ctx, pool)

	f := newBAFixture(t, ctx, pool)
	held := f.task(t, ctx, "ACTIVITY-WORKER", "claude", "holding", 0)
	task := held
	const worker = "mcp:treetop"
	_, err := f.ex.Execute(ctx, executor.Call{Tool: "task_requeue", Actor: worker,
		Args: []byte(`{"task_id":` + strconv.FormatInt(held, 10) + `}`), TaskID: &task})
	if err == nil {
		t.Fatalf("task_requeue as %s succeeded; D6: the verb is humanOnly", worker)
	}
	var status string
	if e := pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, held).Scan(&status); e != nil {
		t.Fatalf("read status: %v", e)
	}
	if status != "holding" {
		t.Errorf("a denied task_requeue moved the task to %q; the policy gate is BEFORE the handler", status)
	}
	if n := bdCount(t, ctx, pool, `SELECT count(*) FROM policy_decisions p JOIN audit_events a ON a.id = p.audit_event_id
	                                WHERE a.actor=$1 AND a.tool='task_requeue' AND p.decision='deny'
	                                  AND p.rule='human_only'`, worker); n != 1 {
		t.Errorf("policy_decisions deny/human_only rows for task_requeue as %s = %d, want 1 (criterion 43; "+
			"invariant 3: every decision leaves a row)", worker, n)
	}

	// The POSITIVE CONTROL: the same call as an interactive session passes.
	const human = "mcp:manual:salvo"
	if _, err := f.ex.Execute(ctx, executor.Call{Tool: "task_requeue", Actor: human,
		Args: []byte(`{"task_id":` + strconv.FormatInt(held, 10) + `}`), TaskID: &task}); err != nil {
		t.Errorf("task_requeue as %s: %v. D6: `swb requeue 452` from any Claude session is exactly the split "+
			"asked for — an interactive session is mcp:manual:salvo, which policy.HumanActor passes", human, err)
	}
}
