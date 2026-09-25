//go:build integration

package dashboard_test

// board-streaming (SWT-89, docs/tickets/board-streaming_SPEC.md) criteria 3, 5,
// 22, 23 and 24 against a real database: the migration-0044 triggers over real
// LISTEN/NOTIFY, the executor paths that write no task_events row, and the
// /tasks render the browser will fetch. The REAL dashboard.Server (dev login),
// the REAL executor and policy matrix (lightsExecutor). NO LLM, NO network.
// This file names no new Go symbol; the hub's suite is
// board_live_hub_integration_test.go.
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_boardstream?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run 'BoardLive|BoardStream|BoardHub' ./internal/dashboard/
//
// USE AN ISOLATED DATABASE (SPEC Verification 2; IK 2026-09-12). Every wait
// filters on ITS OWN row's payload (`<table>:<id>`), so another suite's writes
// cannot satisfy or fail it — but negative waits on a busy shared db are still
// slower to trust. Build-tagged `integration`, env-gated on DATABASE_URL, FATAL
// on 192.168.50.49 (dashGuard).
//
// "TEST THE COLUMN, NOT THE FIXTURE": criterion 5 is the reason the channel
// exists — task_mark_activity, create_task and a task_signal refresh write no
// task_events row, so a board LISTENing on task_events would never hear them.
// Each is driven through the executor, and the notification is read from
// Postgres.
//
// GREENFIELD NOTE, EXPECTED RED: migration 0044 is not applied, and every
// criterion-3/5 test FAILS on blRequire0044's one sentence. Criteria 22-24 fail
// because the page carries no data-stream / data-live markup yet.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - drop WHEN (OLD.* IS DISTINCT FROM NEW.*) -> NoOpUpdateIsSilent.
//   - drop the classify_promotions triggers -> EveryRowChangeOnTheFourTablesNotifies, TriggersOnExactlyFourTables,
//     PathsWithoutTaskEventsWakeTheBoard/promotion.
//   - add a trigger on normalized_messages -> UntriggeredTablesAreSilent, TriggersOnExactlyFourTables.
//   - the indicator gains a child <span> before the time -> PageCarriesTheStreamContract.
//
// CLEANUP PACT: owns project itest-boardlive-proj, source_accounts provider
// itest-boardlive, threads itest-boardlive:%, ai_runs model itest-boardlive.
// FK-ordered and rerunnable, at start AND end.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/dashboard"
	"github.com/sspataro57/switchboard/internal/executor"
)

const (
	blSlug     = "itest-boardlive-proj"
	blClient   = "itest-boardlive-client"
	blProvider = "itest-boardlive"
	blAccount  = "itest-boardlive@local"
	blModel    = "itest-boardlive"
	blCapture  = "capture:itest-boardlive" // the capture:{connector} shape
	blHuman    = "dashboard:salvo"
	blSession  = "mcp:manual:salvo" // task_signal's caller shape (lsSignal)

	blNegativeWait = 500 * time.Millisecond // criterion 3(b): "within 500 ms"
	blPositiveWait = 2 * time.Second
)

// blRequire0044 fails with one sentence when the migration is missing, so a
// pre-0044 database reads as exactly that and not as a trigger bug.
func blRequire0044(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var fn, trig int
	if err := pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM pg_proc WHERE proname = 'board_changed_notify'),
		        (SELECT count(*) FROM pg_trigger tg JOIN pg_proc p ON p.oid = tg.tgfoid
		          WHERE p.proname = 'board_changed_notify' AND NOT tg.tgisinternal)`).Scan(&fn, &trig); err != nil {
		t.Fatalf("probe migration 0044: %v", err)
	}
	if fn == 0 || trig == 0 {
		var maxV string
		_ = pool.QueryRow(ctx, `SELECT COALESCE(max(version)::text, '?') FROM schema_migrations`).Scan(&maxV)
		t.Fatalf("migration 0044 is NOT applied to this database (schema_migrations max version %s; "+
			"board_changed_notify() found %d time(s), %d trigger(s) using it). Apply "+
			"migrations/0044_board_changed_notify.sql: `make migrate LOCAL_DB_URL=$DATABASE_URL`", maxV, fn, trig)
	}
}

func cleanupBoardLive(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const projs = `(SELECT id FROM projects WHERE slug = '` + blSlug + `')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	const accts = `(SELECT id FROM source_accounts WHERE provider = '` + blProvider + `')`
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`
	const msgs = `(SELECT id FROM normalized_messages WHERE raw_source_item_id IN ` + raws + `)`
	const audits = `(SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor = '` + blCapture +
		`' OR args::text LIKE '%itest-boardlive%')`
	for _, q := range []string{
		`DELETE FROM classify_promotions WHERE task_id IN ` + tasksOf + ` OR normalized_message_id IN ` + msgs +
			` OR project_id IN ` + projs,
		`DELETE FROM external_refs WHERE task_id IN ` + tasksOf,
		`DELETE FROM policy_decisions WHERE audit_event_id IN ` + audits,
		`DELETE FROM audit_events WHERE id IN ` + audits,
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'itest-boardlive:%'`,
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN ` + raws +
			` OR ai_run_id IN (SELECT id FROM ai_runs WHERE model = '` + blModel + `')`,
		`DELETE FROM ai_runs WHERE model = '` + blModel + `'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider = '` + blProvider + `'`,
		`DELETE FROM projects WHERE slug = '` + blSlug + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

type blFixture struct {
	pool          *pgxpool.Pool
	ex            *executor.Executor
	project, acct int64
	seq           int
}

func newBLFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *blFixture {
	t.Helper()
	f := &blFixture{pool: pool, ex: lightsExecutor(pool)}
	f.project = bdInsID(t, ctx, pool,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,$2,'manual','dashboard','/tmp/itest-boardlive','any') RETURNING id`, blSlug, blClient)
	f.acct = bdInsID(t, ctx, pool,
		`INSERT INTO source_accounts (provider, account_email, send_enabled) VALUES ($1,$2,false) RETURNING id`,
		blProvider, blAccount)
	return f
}

func (f *blFixture) task(t *testing.T, ctx context.Context, title, assignee, status string) int64 {
	t.Helper()
	return bdInsID(t, ctx, f.pool,
		`INSERT INTO tasks (project_id, title, body, assignee_type, status, priority)
		 VALUES ($1,$2,'',$3,$4,0) RETURNING id`, f.project, title, assignee, status)
}

// message seeds one INBOUND normalized message on its own thread and returns
// the message and its raw row.
func (f *blFixture) message(t *testing.T, ctx context.Context, label string) (msg, raw int64) {
	t.Helper()
	f.seq++
	tag := label + "-" + strconv.Itoa(f.seq) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	raw = bdInsID(t, ctx, f.pool,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,'{}',$3, now()) RETURNING id`, f.acct, "itest-boardlive-"+tag, "itest-boardlive-h-"+tag)
	th := bdInsID(t, ctx, f.pool,
		`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,'itest-boardlive','[]') RETURNING id`,
		"itest-boardlive:"+tag)
	msg = bdInsID(t, ctx, f.pool,
		`INSERT INTO normalized_messages (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		                                  body_text, subject, sender, channel)
		 VALUES ($1,$2,'inbound',$3, now(),'itest-boardlive body','itest-boardlive',
		         'Katie Evans <katie@boardlive.example>','gmail') RETURNING id`,
		raw, th, "<itest-boardlive-"+tag+"@mail.example>")
	return msg, raw
}

// promotion inserts a classify verdict and the classify_promotions row the
// promoter writes AFTER create_task (S2's "What exists"): the row that moves a
// task into the arrivals panel.
func (f *blFixture) promotion(t *testing.T, ctx context.Context, task int64) int64 {
	t.Helper()
	msg, raw := f.message(t, ctx, "promo")
	run := bdInsID(t, ctx, f.pool,
		`INSERT INTO ai_runs (worker_type, provider, model, input, output, status)
		 VALUES ('classify','itest-boardlive',$1,'{}','{}','ok') RETURNING id`, blModel)
	ext := bdInsID(t, ctx, f.pool,
		`INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields) VALUES ($1,$2,'{"actionable":true}') RETURNING id`,
		run, raw)
	return bdInsID(t, ctx, f.pool,
		`INSERT INTO classify_promotions (normalized_message_id, raw_source_item_id, ai_extraction_id, project_id,
		                                  kind, action, task_id, reason)
		 VALUES ($1,$2,$3,$4,'itest-boardlive','task',$5,'itest-boardlive') RETURNING id`,
		msg, raw, ext, f.project, task)
}

func (f *blFixture) exec(t *testing.T, ctx context.Context, q string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(ctx, q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// call runs one tool through the real executor and returns its output.
func (f *blFixture) call(t *testing.T, ctx context.Context, actor, tool string, task int64, args map[string]any) []byte {
	t.Helper()
	raw, _ := json.Marshal(args)
	c := executor.Call{Tool: tool, Actor: actor, Args: raw}
	if task != 0 {
		c.TaskID = &task
	}
	res, err := f.ex.Execute(ctx, c)
	if err != nil {
		t.Fatalf("%s %s as %s: %v", tool, raw, actor, err)
	}
	return res.Output
}

// mark is task_mark_activity through the executor, as capture calls it.
func (f *blFixture) mark(t *testing.T, ctx context.Context, task, msg int64) {
	t.Helper()
	out := f.call(t, ctx, blCapture, "task_mark_activity", task,
		map[string]any{"task_id": task, "message_id": msg, "reason": "capture: itest-boardlive"})
	if !regexp.MustCompile(`"marked":\s*true`).Match(out) {
		t.Fatalf("task_mark_activity(task %d, message %d) returned %s, want marked:true", task, msg, out)
	}
}

// ---- the dedicated LISTEN connection -------------------------------------------------

type blNote struct{ channel, payload string }

// blListener is a dedicated connection (never a pool's) that LISTENs BEFORE the
// write (the orchestrator criterion-3 pattern) and keeps what it has read, so a
// wait for one row's payload never loses another's.
type blListener struct {
	conn *pgx.Conn
	buf  []blNote
}

func blListen(t *testing.T, ctx context.Context, channels ...string) *blListener {
	t.Helper()
	conn, err := pgx.Connect(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("dedicated LISTEN connection: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	for _, ch := range channels {
		if _, err := conn.Exec(ctx, "LISTEN "+ch); err != nil {
			t.Fatalf("LISTEN %s: %v", ch, err)
		}
	}
	return &blListener{conn: conn}
}

// take removes and reports the first buffered note matching (channel, pred).
func (l *blListener) take(channel string, pred func(string) bool) bool {
	for i, n := range l.buf {
		if n.channel == channel && pred(n.payload) {
			l.buf = append(l.buf[:i], l.buf[i+1:]...)
			return true
		}
	}
	return false
}

// await consumes one matching notification within d.
func (l *blListener) await(ctx context.Context, channel string, pred func(string) bool, d time.Duration) bool {
	if l.take(channel, pred) {
		return true
	}
	deadline := time.Now().Add(d)
	for {
		wctx, cancel := context.WithDeadline(ctx, deadline)
		n, err := l.conn.WaitForNotification(wctx)
		cancel()
		if err != nil {
			return false
		}
		l.buf = append(l.buf, blNote{n.Channel, n.Payload})
		if l.take(channel, pred) {
			return true
		}
	}
}

func (l *blListener) awaitPayload(ctx context.Context, channel, payload string, d time.Duration) bool {
	return l.await(ctx, channel, func(p string) bool { return p == payload }, d)
}

// drain reads until the connection has been quiet for d, then forgets everything.
func (l *blListener) drain(ctx context.Context, d time.Duration) {
	for {
		wctx, cancel := context.WithTimeout(ctx, d)
		_, err := l.conn.WaitForNotification(wctx)
		cancel()
		if err != nil {
			break
		}
	}
	l.buf = nil
}

func (l *blListener) seen() []blNote { return append([]blNote(nil), l.buf...) }

func blPayload(table string, id int64) string { return table + ":" + strconv.FormatInt(id, 10) }

func blSetup(t *testing.T) (context.Context, *pgxpool.Pool, *blFixture) {
	t.Helper()
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	t.Cleanup(pool.Close)
	cleanupBoardLive(t, ctx, pool)
	t.Cleanup(func() { cleanupBoardLive(t, context.Background(), pool) })
	return ctx, pool, newBLFixture(t, ctx, pool)
}

// ---- criterion 3(a): INSERT, a real UPDATE and DELETE on each of the four tables ------

func TestBoardLive_Integration_EveryRowChangeOnTheFourTablesNotifies(t *testing.T) {
	ctx, pool, f := blSetup(t)
	blRequire0044(t, ctx, pool)
	l := blListen(t, ctx, "board_changed")

	expect := func(step, table string, id int64) {
		t.Helper()
		if !l.awaitPayload(ctx, "board_changed", blPayload(table, id), blPositiveWait) {
			t.Errorf("%s: no NOTIFY board_changed %q within %v (criterion 3a). Seen: %v", step, blPayload(table, id),
				blPositiveWait, l.seen())
		}
	}

	// tasks
	tk := f.task(t, ctx, "BOARDLIVE row", "human", "ready")
	expect("INSERT tasks", "tasks", tk)
	f.exec(t, ctx, `UPDATE tasks SET title = title || ' (edited)' WHERE id = $1`, tk)
	expect("UPDATE tasks", "tasks", tk)

	// task_dismissals (on its own parent, so the parent's writes are not the ones counted)
	parent := f.task(t, ctx, "BOARDLIVE parent", "human", "ready")
	expect("INSERT parent task", "tasks", parent)
	dis := bdInsID(t, ctx, pool, `INSERT INTO task_dismissals (task_id, reason_code, dismissed_by)
	                              VALUES ($1,'duplicate','dashboard:salvo') RETURNING id`, parent)
	expect("INSERT task_dismissals", "task_dismissals", dis)
	f.exec(t, ctx, `UPDATE task_dismissals SET note = 'itest-boardlive edited' WHERE id = $1`, dis)
	expect("UPDATE task_dismissals", "task_dismissals", dis)
	f.exec(t, ctx, `DELETE FROM task_dismissals WHERE id = $1`, dis)
	expect("DELETE task_dismissals", "task_dismissals", dis)

	// classify_promotions
	cp := f.promotion(t, ctx, parent)
	expect("INSERT classify_promotions", "classify_promotions", cp)
	f.exec(t, ctx, `UPDATE classify_promotions SET reason = 'itest-boardlive edited' WHERE id = $1`, cp)
	expect("UPDATE classify_promotions", "classify_promotions", cp)
	f.exec(t, ctx, `DELETE FROM classify_promotions WHERE id = $1`, cp)
	expect("DELETE classify_promotions", "classify_promotions", cp)

	// external_refs
	er := bdInsID(t, ctx, pool, `INSERT INTO external_refs (task_id, system, external_key)
	                             VALUES ($1,'github','itest-boardlive/pull/1') RETURNING id`, parent)
	expect("INSERT external_refs", "external_refs", er)
	f.exec(t, ctx, `UPDATE external_refs SET external_url = 'https://github.example/itest-boardlive/pull/1' WHERE id = $1`, er)
	expect("UPDATE external_refs", "external_refs", er)
	f.exec(t, ctx, `DELETE FROM external_refs WHERE id = $1`, er)
	expect("DELETE external_refs", "external_refs", er)

	// DELETE tasks last: the row has no children left.
	f.exec(t, ctx, `DELETE FROM tasks WHERE id = $1`, tk)
	expect("DELETE tasks (payload from OLD)", "tasks", tk)
}

// ---- criterion 3(b): a no-op UPDATE is silent ------------------------------------------

func TestBoardLive_Integration_NoOpUpdateIsSilent(t *testing.T) {
	ctx, pool, f := blSetup(t)
	blRequire0044(t, ctx, pool)
	tk := f.task(t, ctx, "BOARDLIVE no-op", "human", "ready")
	l := blListen(t, ctx, "board_changed")

	// POSITIVE CONTROL: the same connection hears a real change to the same row.
	f.exec(t, ctx, `UPDATE tasks SET title = title || '.' WHERE id = $1`, tk)
	if !l.awaitPayload(ctx, "board_changed", blPayload("tasks", tk), blPositiveWait) {
		t.Fatalf("CONTROL: a real UPDATE of task %d delivered no %q; the listener hears nothing (criterion 3a)", tk,
			blPayload("tasks", tk))
	}
	l.drain(ctx, 200*time.Millisecond)

	f.exec(t, ctx, `UPDATE tasks SET title = title WHERE id = $1`, tk)
	if l.awaitPayload(ctx, "board_changed", blPayload("tasks", tk), blNegativeWait) {
		t.Errorf("`UPDATE tasks SET title = title` on task %d delivered %q. Criterion 3b / S2: the UPDATE trigger is "+
			"WHEN (OLD.* IS DISTINCT FROM NEW.*), so a no-op UPDATE wakes no board", tk, blPayload("tasks", tk))
	}
}

// ---- criterion 3(c): a rolled-back UPDATE is silent ----------------------------------------

func TestBoardLive_Integration_RolledBackUpdateIsSilent(t *testing.T) {
	ctx, pool, f := blSetup(t)
	blRequire0044(t, ctx, pool)
	tk := f.task(t, ctx, "BOARDLIVE rollback", "human", "ready")
	l := blListen(t, ctx, "board_changed")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE tasks SET title = title || ' (never)' WHERE id = $1`, tk); err != nil {
		t.Fatalf("update in tx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if l.awaitPayload(ctx, "board_changed", blPayload("tasks", tk), blNegativeWait) {
		t.Errorf("a ROLLED BACK update of task %d delivered %q (criterion 3c: NOTIFY is transactional)", tk,
			blPayload("tasks", tk))
	}
	// The same update committed does notify (the control that makes the negative mean something).
	f.exec(t, ctx, `UPDATE tasks SET title = title || ' (committed)' WHERE id = $1`, tk)
	if !l.awaitPayload(ctx, "board_changed", blPayload("tasks", tk), blPositiveWait) {
		t.Errorf("CONTROL: the committed update of task %d delivered nothing", tk)
	}
}

// ---- criterion 3(d): normalized_messages and projects are left to the tick -----------------

func TestBoardLive_Integration_UntriggeredTablesAreSilent(t *testing.T) {
	ctx, pool, f := blSetup(t)
	blRequire0044(t, ctx, pool)
	l := blListen(t, ctx, "board_changed")

	msg, _ := f.message(t, ctx, "silent")
	f.exec(t, ctx, `UPDATE projects SET repo_path = repo_path || '/moved' WHERE id = $1`, f.project)
	for _, tc := range []struct{ table, payload string }{
		{"normalized_messages", blPayload("normalized_messages", msg)},
		{"projects", blPayload("projects", f.project)},
	} {
		if l.await(ctx, "board_changed", func(p string) bool { return strings.HasPrefix(p, tc.table+":") }, blNegativeWait) {
			t.Errorf("a write to %s delivered a board_changed notification (%s). Criterion 3d / S2: %s is deliberately "+
				"NOT triggered — the ingestion firehose and the once-a-month slugs are covered by the 60 s tick",
				tc.table, tc.payload, tc.table)
		}
	}
}

// ---- criterion 3(e): the orchestrator's channel is untouched --------------------------------

func TestBoardLive_Integration_TaskEventsChannelUnchanged(t *testing.T) {
	ctx, pool, f := blSetup(t)
	blRequire0044(t, ctx, pool)
	tk := f.task(t, ctx, "BOARDLIVE events", "human", "ready")
	l := blListen(t, ctx, "board_changed", "task_events")

	ev := bdInsID(t, ctx, pool,
		`INSERT INTO task_events (task_id, event_type, payload) VALUES ($1,'itest_boardlive','{}') RETURNING id`, tk)
	if !l.awaitPayload(ctx, "task_events", strconv.FormatInt(ev, 10), blPositiveWait) {
		t.Errorf("a task_events insert (id %d) did not deliver exactly its id on channel task_events. Criterion 3e / "+
			"invariant 7: 0003's trigger, payload contract and the orchestrator's wake-up are untouched. Seen: %v", ev, l.seen())
	}
	if l.await(ctx, "board_changed", func(p string) bool { return strings.HasPrefix(p, "task_events:") }, blNegativeWait) {
		t.Errorf("a task_events insert delivered a board_changed notification; task_events is not a board table (S2)")
	}
}

// ---- criterion 3(f): the function is attached to exactly the four tables ---------------------

func TestBoardLive_Integration_TriggersOnExactlyFourTables(t *testing.T) {
	ctx, pool, _ := blSetup(t)
	blRequire0044(t, ctx, pool)
	rows, err := pool.Query(ctx,
		`SELECT c.relname, count(*) FROM pg_trigger tg
		   JOIN pg_proc p ON p.oid = tg.tgfoid
		   JOIN pg_class c ON c.oid = tg.tgrelid
		  WHERE p.proname = 'board_changed_notify' AND NOT tg.tgisinternal
		  GROUP BY c.relname ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("read pg_trigger: %v", err)
	}
	defer rows.Close()
	got := map[string]int{}
	for rows.Next() {
		var rel string
		var n int
		if err := rows.Scan(&rel, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[rel] = n
	}
	want := map[string]int{"tasks": 2, "task_dismissals": 2, "classify_promotions": 2, "external_refs": 2}
	if len(got) != len(want) {
		t.Errorf("board_changed_notify is attached to %v, want exactly the four tables with two triggers each %v "+
			"(criterion 3f)", got, want)
	}
	for tb, n := range want {
		if got[tb] != n {
			t.Errorf("%s carries %d board_changed_notify trigger(s), want %d (INSERT OR DELETE, UPDATE WHEN changed)", tb, got[tb], n)
		}
	}
}

// ---- criterion 5: the paths that write NO task_events row wake the board ---------------------

func TestBoardLive_Integration_PathsWithoutTaskEventsWakeTheBoard(t *testing.T) {
	ctx, pool, f := blSetup(t)
	blRequire0044(t, ctx, pool)

	expectTask := func(t *testing.T, l *blListener, what string, id int64) {
		t.Helper()
		if !l.awaitPayload(ctx, "board_changed", blPayload("tasks", id), blPositiveWait) {
			t.Errorf("%s on task %d delivered no %q within %v. Criterion 5: this is the path the board_changed channel "+
				"exists for; LISTENing on task_events would miss it. Seen: %v", what, id, blPayload("tasks", id),
				blPositiveWait, l.seen())
		}
	}

	t.Run("task_mark_activity", func(t *testing.T) {
		tk := f.task(t, ctx, "BOARDLIVE activity", "human", "ready")
		msg, _ := f.message(t, ctx, "activity")
		l := blListen(t, ctx, "board_changed")
		f.mark(t, ctx, tk, msg)
		expectTask(t, l, "task_mark_activity", tk)
	})

	t.Run("create_task", func(t *testing.T) {
		l := blListen(t, ctx, "board_changed")
		out := f.call(t, ctx, blHuman, "create_task", 0,
			map[string]any{"project": blSlug, "title": "BOARDLIVE created through the executor"})
		var res struct {
			TaskID int64 `json:"task_id"`
		}
		if err := json.Unmarshal(out, &res); err != nil || res.TaskID == 0 {
			t.Fatalf("create_task returned %s, want {\"task_id\":N}", out)
		}
		expectTask(t, l, "create_task", res.TaskID)
	})

	t.Run("task_signal refresh", func(t *testing.T) {
		tk := f.task(t, ctx, "BOARDLIVE signal", "human", "ready")
		l := blListen(t, ctx, "board_changed")
		args := map[string]any{"task_id": tk, "state": "working", "worker_id": "manual:salvo", "session": "itest-boardlive"}
		f.call(t, ctx, blSession, "task_signal", tk, args)
		expectTask(t, l, "the first task_signal working", tk)
		l.drain(ctx, 300*time.Millisecond)

		out := f.call(t, ctx, blSession, "task_signal", tk, args)
		if !regexp.MustCompile(`"changed":\s*false`).Match(out) {
			t.Fatalf("CONTROL: the second identical task_signal returned %s, want changed:false (a refresh writes no event)", out)
		}
		expectTask(t, l, "a task_signal REFRESH (same state and session, changed:false, no event)", tk)
	})

	t.Run("task_set_priority", func(t *testing.T) {
		tk := f.task(t, ctx, "BOARDLIVE priority", "human", "ready")
		l := blListen(t, ctx, "board_changed")
		f.call(t, ctx, blHuman, "task_set_priority", tk, map[string]any{"task_id": tk, "priority": 2, "reason": "itest-boardlive"})
		expectTask(t, l, "task_set_priority", tk)
	})

	t.Run("task_requeue", func(t *testing.T) {
		tk := f.task(t, ctx, "BOARDLIVE requeue", "human", "ready")
		l := blListen(t, ctx, "board_changed")
		f.call(t, ctx, blHuman, "task_requeue", tk, map[string]any{"task_id": tk, "note": "itest-boardlive"})
		expectTask(t, l, "task_requeue", tk)
	})

	t.Run("classify_promotions insert", func(t *testing.T) {
		tk := f.task(t, ctx, "BOARDLIVE promoted", "human", "ready")
		l := blListen(t, ctx, "board_changed")
		cp := f.promotion(t, ctx, tk)
		if !l.awaitPayload(ctx, "board_changed", blPayload("classify_promotions", cp), blPositiveWait) {
			t.Errorf("the promotion row %d delivered no %q: the promoter writes it AFTER create_task, and it is what "+
				"moves the task into the arrivals panel (criterion 5). Seen: %v", cp, blPayload("classify_promotions", cp), l.seen())
		}
	})

	t.Run("link_external_ref", func(t *testing.T) {
		tk := f.task(t, ctx, "BOARDLIVE pr ref", "human", "ready")
		l := blListen(t, ctx, "board_changed")
		const key = "itest-boardlive/pull/7"
		f.call(t, ctx, blHuman, "link_external_ref", tk, map[string]any{"task_id": tk, "system": "github", "external_key": key})
		var ref int64
		if err := pool.QueryRow(ctx, `SELECT id FROM external_refs WHERE task_id = $1 AND external_key = $2`, tk, key).
			Scan(&ref); err != nil {
			t.Fatalf("link_external_ref wrote no external_refs row: %v", err)
		}
		if !l.awaitPayload(ctx, "board_changed", blPayload("external_refs", ref), blPositiveWait) {
			t.Errorf("link_external_ref delivered no %q (criterion 5: the pr_review fact). Seen: %v",
				blPayload("external_refs", ref), l.seen())
		}
	})
}

// ---- criteria 22-24: the render the browser fetches ----------------------------------------

var blLiveNames = []string{"alert", "tally", "main", "counts", "indicator"}

// blRegion returns the rendered element carrying data-live="name".
func blRegion(body, name string) (string, bool) {
	i := strings.Index(body, `data-live="`+name+`"`)
	if i < 0 {
		return "", false
	}
	start := strings.LastIndex(body[:i], "<")
	m := regexp.MustCompile(`^<([a-zA-Z0-9]+)`).FindStringSubmatch(body[start:])
	if m == nil {
		return "", false
	}
	end, ok := depElement(body, start, m[1])
	if !ok {
		return "", false
	}
	return body[start:end], true
}

func blBoard(t *testing.T, client *http.Client, base, query string) string {
	t.Helper()
	code, body := get(t, client, base+"/tasks?"+query)
	if code != http.StatusOK {
		t.Fatalf("GET /tasks?%s = %d\n%s", query, code, snippet(body))
	}
	return body
}

func TestBoardLive_Integration_PageCarriesTheStreamContract(t *testing.T) {
	ctx, pool, f := blSetup(t)
	f.task(t, ctx, "BOARDLIVE page row", "human", "ready")
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	on := blBoard(t, client, ts.URL, "project="+blSlug+"&refresh=on")
	if v := attr(t, on, `data-stream="([^"]*)"`); v != "/tasks/stream" {
		t.Errorf("the script's data-stream = %q, want /tasks/stream (criterion 22)", v)
	}
	if v := attr(t, on, `data-retry="([^"]*)"`); v != "5" {
		t.Errorf("the script's data-retry = %q, want 5 (criterion 22: boardLiveRetry in seconds)", v)
	}
	if v := attr(t, on, `data-version="([^"]*)"`); !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(v) {
		t.Errorf("the script's data-version = %q, want 12 hex characters (criterion 22 / 16)", v)
	}
	for _, n := range blLiveNames {
		if c := strings.Count(on, `data-live="`+n+`"`); c != 1 {
			t.Errorf("refresh=on renders data-live=%q %d times, want exactly once (criterion 22)", n, c)
		}
	}
	if !regexp.MustCompile(`<p id="live-down"[^>]*\shidden[\s>]`).MatchString(on) {
		t.Errorf("refresh=on renders no <p id=\"live-down\" … hidden> (criterion 22: server-rendered words, the script "+
			"only toggles hidden)\n%s", snippet(on))
	}
	if !regexp.MustCompile(`id="auto-refresh"[^>]*>[^<]*\d{2}:\d{2}:\d{2}`).MatchString(on) {
		t.Errorf("refresh=on's indicator does not read an HH:MM:SS directly after its opening tag (criterion 22: a " +
			"child-free <p>)")
	}

	off := blBoard(t, client, ts.URL, "project="+blSlug)
	if !strings.Contains(off, `data-refresh=""`) {
		t.Errorf("the plain board does not render data-refresh=\"\" (criterion 22: nothing streams without refresh=on)")
	}
	if strings.Contains(off, `id="auto-refresh"`) || strings.Contains(off, `data-live="indicator"`) {
		t.Errorf("the plain board renders the indicator (criterion 22)")
	}
	for _, n := range []string{"alert", "tally", "main", "counts"} {
		if !strings.Contains(off, `data-live="`+n+`"`) {
			t.Errorf("the plain board lacks data-live=%q (criterion 22: the other four regions always render)", n)
		}
	}
}

var blClock = regexp.MustCompile(`\d{2}:\d{2}:\d{2}`)

func TestBoardLive_Integration_TwoRendersSwapNothing(t *testing.T) {
	ctx, pool, f := blSetup(t)
	f.task(t, ctx, "BOARDLIVE parity a", "human", "ready")
	f.task(t, ctx, "BOARDLIVE parity b", "claude", "ready")
	f.task(t, ctx, "BOARDLIVE parity c", "human", "holding")
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	q := "project=" + blSlug + "&refresh=on"
	a := blBoard(t, client, ts.URL, q)
	b := blBoard(t, client, ts.URL, q)
	for _, n := range blLiveNames {
		ra, okA := blRegion(a, n)
		rb, okB := blRegion(b, n)
		if !okA || !okB {
			t.Errorf("the rendered page has no data-live=%q region (first render %v, second %v) — criterion 23 compares "+
				"the five regions", n, okA, okB)
			continue
		}
		if n == "indicator" {
			ra, rb = blClock.ReplaceAllString(ra, "HH:MM:SS"), blClock.ReplaceAllString(rb, "HH:MM:SS")
		}
		if ra != rb {
			t.Errorf("two renders of the same URL with no write between differ in the %s region (criterion 23: a tick "+
				"swap must flip no row)\nfirst:  %s\nsecond: %s", n, snippet(ra), snippet(rb))
		}
	}
}

// blPanelHas reports whether the panel whose <h2 id="section-key"> sits in
// region lists the row href.
func blPanelHas(t *testing.T, region, key string, id int64) bool {
	t.Helper()
	h := strings.Index(region, `id="section-`+key+`"`)
	if h < 0 {
		return false
	}
	start := strings.LastIndex(region[:h], "<section")
	if start < 0 {
		t.Fatalf("the panel section-%s has no enclosing <section>", key)
	}
	end, ok := depElement(region, start, "section")
	if !ok {
		t.Fatalf("the panel section-%s is never closed", key)
	}
	return strings.Contains(region[start:end], `href="/tasks/`+strconv.FormatInt(id, 10)+`"`)
}

func TestBoardLive_Integration_ActivityMovesTheRowAcrossPanels(t *testing.T) {
	ctx, pool, f := blSetup(t)
	keys := dashboard.BoardLiveSectionKeys()
	if len(keys) == 0 {
		t.Fatalf("BoardLiveSectionKeys is empty")
	}
	arrivals := keys[0] // SWT-59 I3: the arrivals panel is the first section
	queue := ""
	for _, k := range keys {
		if k == "queue" {
			queue = k
		}
	}
	if queue == "" {
		t.Fatalf("Go's section keys %v carry no queue key", keys)
	}
	tk := f.task(t, ctx, "BOARDLIVE mover", "human", "ready")
	msg, _ := f.message(t, ctx, "mover")
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	q := "project=" + blSlug + "&refresh=on"
	before, ok := blRegion(blBoard(t, client, ts.URL, q), "main")
	if !ok {
		t.Fatalf("the board has no data-live=\"main\" region (criterion 24: the swap replaces <main data-live=\"main\">, " +
			"so the move must show inside it)")
	}
	if !blPanelHas(t, before, queue, tk) || blPanelHas(t, before, arrivals, tk) {
		t.Fatalf("CONTROL: before the mark, task %d is not (only) in the %s panel", tk, queue)
	}
	f.mark(t, ctx, tk, msg)
	after, ok := blRegion(blBoard(t, client, ts.URL, q), "main")
	if !ok {
		t.Fatalf("the re-render has no data-live=\"main\" region")
	}
	if !blPanelHas(t, after, arrivals, tk) {
		t.Errorf("after task_mark_activity, task %d is not in the first (arrivals) panel of the main region (criterion 24: "+
			"the fetch the browser makes carries the move)", tk)
	}
	if blPanelHas(t, after, queue, tk) {
		t.Errorf("after task_mark_activity, task %d is still in the %s panel (criterion 24)", tk, queue)
	}
}
