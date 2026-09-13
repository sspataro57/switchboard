//go:build integration

package tools_test

// The revive form of task_reopen, the close record and human surfacing against a
// real database — SWT-45 (docs/tickets/jira-activity-revive_SPEC.md) criteria
// 3 (the FK's SET NULL), 8 (closeTransition writes/clears closed_at and
// closed_from_status), 10-13 (reviveGuarded, one test per branch of J6), 16
// (a human's plain reopen surfaces; nobody else's does) and 17's persistence
// half (capture_rule_add stores the two flags). Every mutation goes through
// executor.Execute with the REAL registry and the REAL policy matrix
// (deliveryExecutor).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_iso45?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run 'Revive|CloseTransition|HumanPlainReopen|PersistsTheActivityFlags|SetNullOnDelete' ./internal/tools/
//
// "TEST THE COLUMN, NOT THE FIXTURE": the callers pass IDS ONLY. Every guard
// turns on a value Postgres produces — normalized_messages.created_at and
// .direction, tasks.closed_at (written by closeTransition), tasks.updated_at,
// tasks.closed_from_status, task_dismissals.created_at. Mutations are named
// inline; the deliver step applies each by hand.
//
// ---- IMPOSED SURFACE ----------------------------------------------------------
//
//	task_reopen {task_id, message_id, revive:true, reason}
//	  -> {task_id, reopened, status, skipped?, dismissal_id?}
//	  skipped ∈ not_closed | message_predates_close
//	  an outbound or missing message is an ERROR (checked first).
//	  guard = GREATEST(COALESCE(t.closed_at, t.updated_at), open dismissal's created_at);
//	  reopen iff m.created_at > guard, strictly.
//	  target = restoreTarget(tasks.closed_from_status) with dependency re-derivation;
//	  stamps the open dismissal (reopened_by_message_id); sets surfaced_at/_by_message_id.
//	tasks.closed_at / closed_from_status: written by closeTransition on -> closed,
//	  NULLed on -> open; an already-closed close keeps them.
//	plain task_reopen by policy.HumanActor(actor): surfaced_at = now(), message NULL.
//
// RED TODAY: migration 0030 is not applied, so rvRequire0030 fails every test
// with one sentence. After it is applied, `revive` is ignored by validateReopen
// and the SWT-36 pair check refuses every revive call.
//
// CLEANUP PACT: projects itest-revive-%, source_accounts provider
// itest-revive-src, threads itest-revive:%, audit rows by task plus this
// suite's own actors. FK-ordered, at start AND end. Tasks go BEFORE threads and
// messages (source_thread_id; surfaced_by_message_id is SET NULL).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
)

const (
	rvSlug      = "itest-revive-proj"
	rvProvider  = "itest-revive-src"
	rvAccount   = "itest-revive@local"
	rvCloser    = "opsctl:itest-revive"    // a human close
	rvDismisser = "dashboard:itest-revive" // task_dismiss is humanOnly
	rvSpine     = "capture:itest-revive"   // capture's shape: the revive's real caller
	rvSender    = "Katie Evans (JIRA) <jira@revive.example>"
)

func rvRequire0030(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE (table_name='tasks' AND column_name IN ('closed_at','closed_from_status','surfaced_at','surfaced_by_message_id'))
		     OR (table_name='capture_rules' AND column_name IN ('revive','addressed'))`).Scan(&n); err != nil {
		t.Fatalf("probe 0030's columns: %v", err)
	}
	if n != 6 {
		t.Fatalf("found %d of the 6 tasks/capture_rules columns migration 0030 adds. Criteria 2-3: "+
			"0030_jira_activity_revive.sql adds capture_rules.revive/addressed and tasks.closed_at, closed_from_status, "+
			"surfaced_at, surfaced_by_message_id; `make migrate LOCAL_DB_URL=...` applies it. Merging a migration is "+
			"not applying it", n)
	}
}

func rvCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const projs = `(SELECT id FROM projects WHERE slug LIKE 'itest-revive-%')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	const accts = `(SELECT id FROM source_accounts WHERE provider = '` + rvProvider + `')`
	const actors = `('` + rvCloser + `','` + rvDismisser + `','` + rvSpine + `')`
	for _, q := range []string{
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_dependencies WHERE task_id IN ` + tasksOf + ` OR depends_on_task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor IN ` + actors + `)`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor IN ` + actors,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM capture_rules WHERE project_id IN ` + projs,
		`DELETE FROM projects WHERE slug LIKE 'itest-revive-%'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN
		   (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'itest-revive:%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider = '` + rvProvider + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

type rvSuite struct {
	pool    *pgxpool.Pool
	ex      *executor.Executor
	project int64
	account int64
	seq     int
}

func newRVSuite(t *testing.T, ctx context.Context) *rvSuite {
	t.Helper()
	pool := newToolsPool(t, ctx)
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use the compose db on :5433")
	}
	t.Cleanup(pool.Close)
	rvRequire0030(t, ctx, pool)
	rvCleanup(t, ctx, pool)
	t.Cleanup(func() { rvCleanup(t, ctx, pool) })

	s := &rvSuite{pool: pool, ex: deliveryExecutor(pool)}
	s.project = seedProject(t, ctx, pool, rvSlug, "itest-revive-client")
	s.account = s.id(t, ctx,
		`INSERT INTO source_accounts (provider, account_email, send_enabled) VALUES ($1,$2,false) RETURNING id`,
		rvProvider, rvAccount)
	return s
}

func (s *rvSuite) id(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", q, err)
	}
	return id
}

func (s *rvSuite) n(t *testing.T, ctx context.Context, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

func (s *rvSuite) exec(t *testing.T, ctx context.Context, q string, args ...any) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func (s *rvSuite) dbNow(t *testing.T, ctx context.Context) time.Time {
	t.Helper()
	var now time.Time
	if err := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatalf("read db clock: %v", err)
	}
	return now
}

// task seeds a task on its own thread. status is written directly: these are
// the fixtures F5 says exist across the repo.
func (s *rvSuite) task(t *testing.T, ctx context.Context, label, status string) (task, thread int64) {
	t.Helper()
	s.seq++
	thread = s.id(t, ctx,
		`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,'itest-revive','[]') RETURNING id`,
		fmt.Sprintf("itest-revive:%s:%d", label, s.seq))
	task = s.id(t, ctx,
		`INSERT INTO tasks (project_id, title, status, source_thread_id) VALUES ($1,$2,$3,$4) RETURNING id`,
		s.project, "itest-revive "+label, status, thread)
	return task, thread
}

func (s *rvSuite) message(t *testing.T, ctx context.Context, label string, thread int64, direction string,
	createdAt, sentAt time.Time) int64 {
	t.Helper()
	s.seq++
	tag := fmt.Sprintf("%s-%d", label, s.seq)
	raw := s.id(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,'{}',$3, now()) RETURNING id`, s.account, "itest-revive-"+tag, "itest-revive-h-"+tag)
	sender := rvSender
	if direction == "outbound" {
		sender = "Salvador <salvador@example.test>"
	}
	return s.id(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		    body_text, subject, sender, channel, created_at)
		 VALUES ($1,$2,$3,$4,$5,'itest-revive body','Katie Evans mentioned you on RVV-1',$6,'gmail',$7) RETURNING id`,
		raw, thread, direction, "<itest-revive-"+tag+"@mail.example>", sentAt, sender, createdAt)
}

func (s *rvSuite) run(ctx context.Context, tool, actor string, taskID int64, args string) (map[string]any, error) {
	res, err := s.ex.Execute(ctx, executor.Call{Tool: tool, Actor: actor, Args: json.RawMessage(args), TaskID: &taskID})
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		return nil, fmt.Errorf("decode %s result %s: %w", tool, res.Output, err)
	}
	return out, nil
}

func (s *rvSuite) call(t *testing.T, ctx context.Context, tool, actor string, taskID int64, args string) map[string]any {
	t.Helper()
	out, err := s.run(ctx, tool, actor, taskID, args)
	if err != nil {
		t.Fatalf("%s(%s) as %s: %v", tool, args, actor, err)
	}
	return out
}

func (s *rvSuite) close(t *testing.T, ctx context.Context, taskID int64) {
	t.Helper()
	s.call(t, ctx, "task_close", rvCloser, taskID, `{"task_id":`+itoa(taskID)+`,"reason":"itest-revive: done"}`)
}

func reviveArgs(taskID, messageID int64) string {
	b, _ := json.Marshal(map[string]any{"task_id": taskID, "message_id": messageID, "revive": true,
		"reason": "itest-revive: Jira activity"})
	return string(b)
}

func (s *rvSuite) revive(t *testing.T, ctx context.Context, taskID, messageID int64) map[string]any {
	t.Helper()
	return s.call(t, ctx, "task_reopen", rvSpine, taskID, reviveArgs(taskID, messageID))
}

type rvRow struct {
	status     string
	closedAt   *time.Time
	closedFrom *string
	surfacedAt *time.Time
	surfacedBy *int64
	updatedAt  time.Time
}

func (s *rvSuite) row(t *testing.T, ctx context.Context, taskID int64) rvRow {
	t.Helper()
	var r rvRow
	if err := s.pool.QueryRow(ctx,
		`SELECT status, closed_at, closed_from_status, surfaced_at, surfaced_by_message_id, updated_at
		   FROM tasks WHERE id=$1`, taskID).
		Scan(&r.status, &r.closedAt, &r.closedFrom, &r.surfacedAt, &r.surfacedBy, &r.updatedAt); err != nil {
		t.Fatalf("read task %d: %v", taskID, err)
	}
	return r
}

func rvStr(p *string) string {
	if p == nil {
		return "<NULL>"
	}
	return *p
}

func rvWantSkipped(t *testing.T, what string, out map[string]any, want string) {
	t.Helper()
	if out["reopened"] != false || out["skipped"] != want {
		t.Errorf("%s: task_reopen = %v, want {reopened:false, skipped:%q}", what, out, want)
	}
}

// ---- criterion 8: the close record ---------------------------------------------

// MUTATION: drop closed_at from closeTransition's UPDATE -> closed_at stays NULL
// here, and criterion 11's NULL-fallback case shows the guard silently moving to
// updated_at.
func TestCloseTransition_RecordsAndClearsTheCloseRecord(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)

	task, _ := s.task(t, ctx, "rec", "holding")
	before := s.dbNow(t, ctx)
	s.close(t, ctx, task)
	after := s.dbNow(t, ctx)
	r := s.row(t, ctx, task)
	if r.closedAt == nil || r.closedAt.Before(before) || r.closedAt.After(after) {
		t.Errorf("after task_close closed_at = %v, want an instant within [%s, %s] (J5: closeTransition writes "+
			"closed_at = now())", r.closedAt, before, after)
	}
	if rvStr(r.closedFrom) != "holding" {
		t.Errorf("after task_close closed_from_status = %s, want holding (J5: the status it was closed FROM)", rvStr(r.closedFrom))
	}

	// An already-closed close is a no-op that keeps them.
	s.close(t, ctx, task)
	if r2 := s.row(t, ctx, task); r2.closedAt == nil || !r2.closedAt.Equal(*r.closedAt) || rvStr(r2.closedFrom) != "holding" {
		t.Errorf("a replayed close rewrote the close record: closed_at %v -> %v, closed_from_status %s. An "+
			"idempotent replay must not move the guard instant", r.closedAt, r2.closedAt, rvStr(r2.closedFrom))
	}

	// -> open NULLs both. A non-human actor, so J8's surfacing is not in play.
	s.call(t, ctx, "task_reopen", "ticketstatus:jira", task, `{"task_id":`+itoa(task)+`,"reason":"ticket left done"}`)
	if r3 := s.row(t, ctx, task); r3.closedAt != nil || r3.closedFrom != nil {
		t.Errorf("after a reopen closed_at=%v closed_from_status=%s, want both NULL (J5)", r3.closedAt, rvStr(r3.closedFrom))
	} else if r3.surfacedAt != nil {
		t.Errorf("the reconciler's plain reopen set surfaced_at=%v. J8: only a HUMAN's plain reopen surfaces", r3.surfacedAt)
	}

	// task_dismiss closes through the same helper, so it records too.
	d, _ := s.task(t, ctx, "rec-dismiss", "ready")
	s.call(t, ctx, "task_dismiss", rvDismisser, d, `{"task_id":`+itoa(d)+`,"reason_code":"not_actionable"}`)
	if r4 := s.row(t, ctx, d); r4.closedAt == nil || rvStr(r4.closedFrom) != "ready" {
		t.Errorf("after task_dismiss closed_at=%v closed_from_status=%s, want set / ready — closeTransition is the ONE "+
			"writer of status='closed' (F5)", r4.closedAt, rvStr(r4.closedFrom))
	}
}

// ---- criterion 10: reviveGuarded's branches ------------------------------------

func TestRevive_RestoresThePreCloseStatusAndSurfacesInOneCall(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)

	for _, from := range []string{"holding", "delivered"} { // J14: a task closed FROM delivered restores delivered
		t.Run(from, func(t *testing.T) {
			task, thread := s.task(t, ctx, "main-"+from, from)
			s.close(t, ctx, task)
			now := s.dbNow(t, ctx)
			m := s.message(t, ctx, "main-"+from, thread, "inbound", now, now)

			out := s.revive(t, ctx, task, m)
			if out["reopened"] != true || out["status"] != from {
				t.Errorf("revive = %v, want {reopened:true, status:%s} (J6 e: tasks.closed_from_status)", out, from)
			}
			if _, present := out["skipped"]; present {
				t.Errorf("a successful revive carries skipped=%v", out["skipped"])
			}
			r := s.row(t, ctx, task)
			if r.status != from {
				t.Errorf("tasks.status = %q, want %q", r.status, from)
			}
			if r.surfacedAt == nil || r.surfacedBy == nil || *r.surfacedBy != m {
				t.Errorf("surfaced_at=%v surfaced_by_message_id=%v, want set / %d. J6 (h): the revive ALWAYS surfaces — "+
					"reviving without surfacing is the bounce (the next jira tick re-closes it)", r.surfacedAt, r.surfacedBy, m)
			}
			if r.closedAt != nil || r.closedFrom != nil {
				t.Errorf("closed_at=%v closed_from_status=%s after the revive, want NULL / NULL", r.closedAt, rvStr(r.closedFrom))
			}
			var reason string
			if err := s.pool.QueryRow(ctx, `SELECT payload->>'reason' FROM task_events WHERE task_id=$1
			     AND event_type='status_changed' ORDER BY id DESC LIMIT 1`, task).Scan(&reason); err != nil {
				t.Fatalf("read status_changed: %v", err)
			}
			if !strings.Contains(reason, strconv.FormatInt(m, 10)) {
				t.Errorf("the revive's status_changed reason %q does not name message %d — the task page is where a "+
					"human asks why a closed task came back", reason, m)
			}
			if n := s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool='task_reopen' AND actor=$2 AND status='ok'`,
				task, rvSpine); n != 1 {
				t.Errorf("task_reopen audit rows = %d, want 1 (invariant 3)", n)
			}
		})
	}
}

// "an ingested-after message -> reopen to closed_from_status, else ready, with
// dependency re-derivation." The fixtures are closed tasks INSERTed directly —
// F5's shape, closed_at NULL — so the updated_at fallback is also exercised.
func TestRevive_FallsBackToReadyAndRederivesDependencies(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)

	closedFixture := func(label, closedFrom string) (int64, int64) {
		task, thread := s.task(t, ctx, label, "closed")
		s.exec(t, ctx, `UPDATE tasks SET closed_from_status = NULLIF($2,''), updated_at = now() - interval '1 hour'
		                  WHERE id=$1`, task, closedFrom)
		return task, thread
	}
	dep := func(task int64, depStatus string) {
		d, _ := s.task(t, ctx, "dep", depStatus)
		s.exec(t, ctx, `INSERT INTO task_dependencies (task_id, depends_on_task_id) VALUES ($1,$2)`, task, d)
	}

	for _, tc := range []struct {
		name, closedFrom, depStatus, want string
	}{
		{"NULL closed_from_status, no dependencies", "", "", "ready"},
		{"NULL closed_from_status, an UNMET dependency", "", "ready", "blocked"},
		{"blocked, its dependency satisfied while closed", "blocked", "closed", "ready"},
		{"a non-open closed_from_status", "in_progress", "", "ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task, thread := closedFixture("dep-"+tc.name[:4], tc.closedFrom)
			if tc.depStatus != "" {
				dep(task, tc.depStatus)
			}
			now := s.dbNow(t, ctx)
			m := s.message(t, ctx, "dep", thread, "inbound", now, now)
			out := s.revive(t, ctx, task, m)
			if got := s.row(t, ctx, task).status; got != tc.want || out["status"] != tc.want {
				t.Errorf("revive of a task closed from %q with dependency %q: status %q (result %v), want %q. SWT-36's "+
					"re-derivation: a verbatim `blocked` strands a task whose dependencies completed; a verbatim `ready` "+
					"lets a worker claim it early", tc.closedFrom, tc.depStatus, got, out["status"], tc.want)
			}
		})
	}
}

// "an outbound message -> ERROR and no write." Checked FIRST (J6 a), before
// not_closed, so a caller bug is never laundered into a quiet skip.
func TestRevive_AnOutboundOrMissingMessageIsAnErrorAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)

	task, thread := s.task(t, ctx, "out", "ready")
	s.close(t, ctx, task)
	now := s.dbNow(t, ctx)
	ours := s.message(t, ctx, "out", thread, "outbound", now, now)
	for _, tc := range []struct {
		name string
		msg  int64
	}{{"an outbound message ingested after the close", ours}, {"a message that does not exist", 987654321}} {
		if out, err := s.run(ctx, "task_reopen", rvSpine, task, reviveArgs(task, tc.msg)); err == nil || strings.Contains(err.Error(), "validate ") {
			t.Errorf("revive naming %s = %v, want an ERROR. Invariant 5 at the verb: Jira can mail him about his OWN "+
				"change, and our own sends re-enter via ingestion. A VALIDATION refusal does not count (err=%v)", tc.name, out, err)
		}
	}
	r := s.row(t, ctx, task)
	if r.status != "closed" || r.surfacedAt != nil {
		t.Errorf("after refused revives the task is %s surfaced_at=%v, want closed / NULL (nothing written)", r.status, r.surfacedAt)
	}

	open, othread := s.task(t, ctx, "out-open", "ready")
	late := s.message(t, ctx, "out-open", othread, "outbound", s.dbNow(t, ctx), s.dbNow(t, ctx))
	if out, err := s.run(ctx, "task_reopen", rvSpine, open, reviveArgs(open, late)); err == nil || strings.Contains(err.Error(), "validate ") {
		t.Errorf("revive with an OUTBOUND message on an OPEN task = %v, want an ERROR — J6's order puts (a) before (b)", out)
	}
}

// "a task not closed (incl. delivered) -> not_closed."
func TestRevive_ANotClosedTaskIsSkipped(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)
	for _, status := range []string{"ready", "holding", "delivered", "in_progress"} {
		task, thread := s.task(t, ctx, "open-"+status, status)
		m := s.message(t, ctx, "open-"+status, thread, "inbound", s.dbNow(t, ctx), s.dbNow(t, ctx))
		rvWantSkipped(t, "a revive of a "+status+" task", s.revive(t, ctx, task, m), "not_closed")
		if r := s.row(t, ctx, task); r.status != status || r.surfacedAt != nil {
			t.Errorf("a skipped revive of a %s task left status %s surfaced_at %v, want unchanged / NULL. J10: activity "+
				"on an OPEN task never surfaces, or the ticket-closed email would pin every done task open", status, r.status, r.surfacedAt)
		}
	}
}

// ---- criterion 11: the guard is ingest time against the close -----------------

// MUTATIONS, each of which must turn a case red:
//   - compare sent_at instead of created_at -> (a) revives and (b) skips;
//   - `>=` instead of `>` -> (c) revives;
//   - COALESCE(t.closed_at, t.updated_at) -> t.closed_at alone -> (d)'s revive skips.
func TestRevive_TheGuardIsIngestTimeAgainstTheClose(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)

	closed := func(label string) (task, thread int64, closedAt time.Time) {
		task, thread = s.task(t, ctx, label, "ready")
		s.close(t, ctx, task)
		r := s.row(t, ctx, task)
		if r.closedAt == nil {
			t.Fatalf("setup: task_close wrote no closed_at (criterion 8)")
		}
		return task, thread, *r.closedAt
	}

	t.Run("(a) sent after the close, ingested before it: skips", func(t *testing.T) {
		task, thread, at := closed("a")
		m := s.message(t, ctx, "a", thread, "inbound", at.Add(-time.Minute), at.Add(5*time.Minute))
		rvWantSkipped(t, "a message ingested before the close", s.revive(t, ctx, task, m), "message_predates_close")
		if s.row(t, ctx, task).status != "closed" {
			t.Errorf("a message ingested BEFORE the close revived the task")
		}
	})
	t.Run("(b) sent before the close, ingested after it: revives", func(t *testing.T) {
		task, thread, at := closed("b")
		m := s.message(t, ctx, "b", thread, "inbound", at.Add(time.Minute), at.Add(-5*time.Minute))
		if out := s.revive(t, ctx, task, m); out["reopened"] != true {
			t.Errorf("a message SENT before the close but INGESTED after it = %v, want a revive. Decision 2: he could "+
				"not have seen it when he closed the task (the batching lag)", out)
		}
	})
	t.Run("(c) ingested at the close's own instant: strictly greater, so it skips", func(t *testing.T) {
		task, thread, at := closed("c")
		m := s.message(t, ctx, "c", thread, "inbound", at, at.Add(time.Minute))
		rvWantSkipped(t, "a message ingested at the close instant", s.revive(t, ctx, task, m), "message_predates_close")
	})
	t.Run("(d) closed_at NULL: the guard is updated_at", func(t *testing.T) {
		task, thread := s.task(t, ctx, "d", "closed") // a pre-0030 / old-binary close: closed_at NULL
		s.exec(t, ctx, `UPDATE tasks SET updated_at = now() - interval '1 hour' WHERE id=$1`, task)
		updated := s.row(t, ctx, task).updatedAt
		early := s.message(t, ctx, "d-early", thread, "inbound", updated.Add(-time.Hour), updated.Add(-time.Hour))
		rvWantSkipped(t, "a message ingested before updated_at on a closed_at-NULL task", s.revive(t, ctx, task, early),
			"message_predates_close")
		late := s.message(t, ctx, "d-late", thread, "inbound", updated.Add(30*time.Minute), updated.Add(30*time.Minute))
		if out := s.revive(t, ctx, task, late); out["reopened"] != true {
			t.Errorf("a message ingested after updated_at on a closed_at-NULL task = %v, want a revive. J5: the fallback "+
				"is spelled ONCE, in the handler's SQL — without it no pre-0030 close could ever be revived", out)
		}
	})
}

// ---- criterion 12: an open dismissal is handled inside the revive -------------

func TestRevive_AnOpenDismissalIsHandledInsideTheRevive(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)

	t.Run("the guard is the LATER of the close and the dismissal", func(t *testing.T) {
		task, thread := s.task(t, ctx, "dz", "holding")
		s.close(t, ctx, task) // T1: closed_at, closed_from_status=holding
		now := s.dbNow(t, ctx)
		between := s.message(t, ctx, "dz-between", thread, "inbound", now, now)
		// T2: a label on the already-closed task (SWT-31 criterion 14): no transition.
		s.call(t, ctx, "task_dismiss", rvDismisser, task, `{"task_id":`+itoa(task)+`,"reason_code":"handled_elsewhere"}`)
		var dismissal int64
		if err := s.pool.QueryRow(ctx, `SELECT id FROM task_dismissals WHERE task_id=$1 AND reopened_at IS NULL`, task).
			Scan(&dismissal); err != nil {
			t.Fatalf("setup: no open dismissal: %v", err)
		}
		now = s.dbNow(t, ctx)
		after := s.message(t, ctx, "dz-after", thread, "inbound", now, now)

		rvWantSkipped(t, "a message between the close and the dismissal", s.revive(t, ctx, task, between),
			"message_predates_close")
		var stamped int
		stamped = s.n(t, ctx, `SELECT count(*) FROM task_dismissals WHERE id=$1 AND reopened_at IS NOT NULL`, dismissal)
		if stamped != 0 {
			t.Errorf("a skipped revive stamped the dismissal")
		}

		out := s.revive(t, ctx, task, after)
		if out["reopened"] != true || out["status"] != "holding" {
			t.Errorf("a message after both = %v, want {reopened:true, status:holding} — the target is TASKS."+
				"closed_from_status (holding), not the dismissal's (NULL here, which would give ready)", out)
		}
		if out["dismissal_id"] != float64(dismissal) {
			t.Errorf("revive result dismissal_id = %v, want %d", out["dismissal_id"], dismissal)
		}
		var by *string
		var byMsg *int64
		if err := s.pool.QueryRow(ctx, `SELECT reopened_by, reopened_by_message_id FROM task_dismissals WHERE id=$1`, dismissal).
			Scan(&by, &byMsg); err != nil {
			t.Fatalf("read the stamp: %v", err)
		}
		if by == nil || *by != rvSpine || byMsg == nil || *byMsg != after {
			t.Errorf("dismissal stamp = (by %s, msg %v), want (%s, %d). J6 (g): the ONE typed place 'overtaken by "+
				"activity' lives", rvStr(by), byMsg, rvSpine, after)
		}
	})

	t.Run("a dismissal that closed the task", func(t *testing.T) {
		task, thread := s.task(t, ctx, "dz2", "ready")
		s.call(t, ctx, "task_dismiss", rvDismisser, task, `{"task_id":`+itoa(task)+`,"reason_code":"not_actionable"}`)
		now := s.dbNow(t, ctx)
		m := s.message(t, ctx, "dz2", thread, "inbound", now, now)
		if out := s.revive(t, ctx, task, m); out["reopened"] != true || out["status"] != "ready" {
			t.Errorf("revive of a dismissed task = %v, want {reopened:true, status:ready} (S11)", out)
		}
		if n := s.n(t, ctx, `SELECT count(*) FROM task_dismissals WHERE task_id=$1 AND reopened_by_message_id=$2`, task, m); n != 1 {
			t.Errorf("the dismissal was not stamped with the reviving message")
		}
	})
}

// ---- criterion 13: one message revives a task at most once -------------------

func TestRevive_OneMessageRevivesATaskAtMostOnce(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)

	task, thread := s.task(t, ctx, "once", "ready")
	s.close(t, ctx, task)
	now := s.dbNow(t, ctx)
	m := s.message(t, ctx, "once", thread, "inbound", now, now)
	if out := s.revive(t, ctx, task, m); out["reopened"] != true {
		t.Fatalf("first revive = %v, want reopened:true", out)
	}
	rvWantSkipped(t, "the same message again", s.revive(t, ctx, task, m), "not_closed")

	s.close(t, ctx, task) // he closes it again: that close is newer than the message
	rvWantSkipped(t, "the same message after a re-close", s.revive(t, ctx, task, m), "message_predates_close")
	if s.row(t, ctx, task).status != "closed" {
		t.Errorf("an old message revived a re-closed task — J12: every flip consumes a NEW message")
	}
}

// ---- criterion 16: a human's plain reopen surfaces; nobody else's does -------

// "for exactly the human actor shapes dashboard:x, opsctl:x, manual:salvo and
// mcp:manual:salvo. It does NOT surface for ticketstatus:jira, capture:google,
// capture:gate, promote:classify, drafts:gpt or mcp:acme, all enumerated per
// the IK actor-prefix rule."
func TestReopen_AHumanPlainReopenSurfacesAndNoOtherActorDoes(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)

	for _, tc := range []struct {
		actor string
		human bool
	}{
		{"dashboard:x", true}, {"opsctl:x", true}, {"manual:salvo", true}, {"mcp:manual:salvo", true},
		{"ticketstatus:jira", false}, {"capture:google", false}, {"capture:gate", false},
		{"promote:classify", false}, {"drafts:gpt", false}, {"mcp:acme", false},
	} {
		t.Run(tc.actor, func(t *testing.T) {
			task, _ := s.task(t, ctx, "actor", "closed")
			out := s.call(t, ctx, "task_reopen", tc.actor, task, `{"task_id":`+itoa(task)+`,"reason":"itest-revive"}`)
			if len(out) != 3 || out["reopened"] != true {
				t.Errorf("plain task_reopen result = %v, want exactly {task_id, status, reopened:true} (unchanged)", out)
			}
			r := s.row(t, ctx, task)
			if tc.human && (r.surfacedAt == nil || r.surfacedBy != nil) {
				t.Errorf("a plain reopen by %q left surfaced_at=%v by=%v, want set / NULL. J8: a human's reopen of a done "+
					"ticket's task must STICK, or the next jira tick re-closes it (F4's pre-existing bounce)",
					tc.actor, r.surfacedAt, r.surfacedBy)
			}
			if !tc.human && r.surfacedAt != nil {
				t.Errorf("a plain reopen by %q set surfaced_at=%v. J8: the reconciler's own reopen and every non-human "+
					"actor never surface — keyed on policy.HumanActor, one definition", tc.actor, r.surfacedAt)
			}
		})
	}
}

// ---- criterion 3: the FK is SET NULL -----------------------------------------

func TestMigration0030_SurfacedByMessageIsSetNullOnDelete(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)

	task, thread := s.task(t, ctx, "fk", "ready")
	s.close(t, ctx, task)
	now := s.dbNow(t, ctx)
	m := s.message(t, ctx, "fk", thread, "inbound", now, now)
	s.revive(t, ctx, task, m)
	s.exec(t, ctx, `DELETE FROM normalized_messages WHERE id=$1`, m)
	r := s.row(t, ctx, task)
	if r.surfacedBy != nil || r.surfacedAt == nil {
		t.Errorf("after deleting the surfacing message: surfaced_by_message_id=%v surfaced_at=%v, want NULL / still set "+
			"(ON DELETE SET NULL — the task and its hold outlive the message)", r.surfacedBy, r.surfacedAt)
	}
}

// ---- criterion 17 (persistence half): capture_rule_add stores the flags ------

func TestCaptureRuleAdd_PersistsTheActivityFlags(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)

	add := func(args map[string]any) (int64, error) {
		args["project"] = rvSlug
		b, _ := json.Marshal(args)
		res, err := s.ex.Execute(ctx, executor.Call{Tool: "capture_rule_add", Actor: rvCloser, Args: b})
		if err != nil {
			return 0, err
		}
		var out struct {
			RuleID int64 `json:"rule_id"`
		}
		_ = json.Unmarshal(res.Output, &out)
		return out.RuleID, nil
	}
	flags := func(id int64) (bool, bool) {
		var revive, addressed bool
		if err := s.pool.QueryRow(ctx, `SELECT revive, addressed FROM capture_rules WHERE id=$1`, id).Scan(&revive, &addressed); err != nil {
			t.Fatalf("read rule %d: %v", id, err)
		}
		return revive, addressed
	}

	j2, err := add(map[string]any{"criteria_type": "sender", "pattern": "jira@revive.example", "external_system": "jira",
		"key_regex": `^[^\n]*?\b(RVV-[0-9]+)\b`, "priority": 92, "revive": true})
	if err != nil {
		t.Fatalf("capture_rule_add J2 shape --revive: %v", err)
	}
	if r, a := flags(j2); !r || a {
		t.Errorf("J2 rule stored revive=%v addressed=%v, want true/false", r, a)
	}
	j4, err := add(map[string]any{"criteria_type": "body_regex", "pattern": `\A[^\n]*mentioned you on RVV-[0-9]+`,
		"external_system": "jira", "key_regex": `\A[^\n]*?\b(RVV-[0-9]+)\b`, "priority": 93, "revive": true, "addressed": true})
	if err != nil {
		t.Fatalf("capture_rule_add J4 shape --revive --addressed: %v", err)
	}
	if r, a := flags(j4); !r || !a {
		t.Errorf("J4 rule stored revive=%v addressed=%v, want true/true", r, a)
	}
	plain, err := add(map[string]any{"criteria_type": "sender", "pattern": "someone@revive.example", "priority": 5})
	if err != nil {
		t.Fatalf("capture_rule_add plain: %v", err)
	}
	if r, a := flags(plain); r || a {
		t.Errorf("a rule added without flags stored revive=%v addressed=%v, want false/false (today's behaviour)", r, a)
	}

	before := s.n(t, ctx, `SELECT count(*) FROM capture_rules WHERE project_id=$1`, s.project)
	if _, err := add(map[string]any{"criteria_type": "body_regex", "pattern": `(RVV|RVW)-[0-9]+`, "external_system": "jira",
		"priority": 90, "revive": true}); err == nil {
		t.Errorf("capture_rule_add accepted --revive on a rule with no key_regex (the rule-10 shape)")
	}
	if after := s.n(t, ctx, `SELECT count(*) FROM capture_rules WHERE project_id=$1`, s.project); after != before {
		t.Errorf("a refused capture_rule_add wrote %d row(s)", after-before)
	}
}
