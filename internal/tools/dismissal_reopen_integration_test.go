//go:build integration

package tools_test

// The guarded task_reopen against a real database — SWT-36
// (docs/tickets/dismiss-reopen-on-activity_SPEC.md) criteria 2, 3, 4, 5, 6,
// 18(d), 21, 22, 24, 25 and 26, plus the "Races and idempotence" replays.
// Every mutation goes through executor.Execute with the REAL registry and the
// REAL policy matrix (deliveryExecutor), so a policy denial or a missing audit
// row fails here rather than in production.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run DismissalReopen ./internal/tools/
//
// Build-tagged `integration` AND env-gated on DATABASE_URL, with a FATAL guard
// on 192.168.50.49 — this suite deletes rows.
//
// "TEST THE COLUMN, NOT THE FIXTURE" (IK; memory). Every assertion turns on a
// value POSTGRES produces: normalized_messages.created_at and .direction,
// task_dismissals.created_at and .closed_from_status, the tasks row lock. The
// callers pass IDS ONLY (D4) — a test that handed the handler a time or a
// direction would be testing the fixture. The mutation that must turn each
// test red is named inline, and the deliver step applies each one by hand.
//
// Criterion 26, the fixture's shape: every dismissed task sits on a thread
// carrying BOTH an inbound and an outbound message, and is dismissed through
// task_dismiss on the executor, never by INSERT — so closed_from_status and
// created_at come from the real code path.
//
// ---- IMPOSED SURFACE ----------------------------------------------------------
//
//	task_reopen args:   {task_id, reason, status?, dismissal_id?, message_id?}
//	task_reopen result: {task_id, status, reopened, skipped?, dismissal_id?}
//	  skipped ∈ not_closed | dismissal_not_open | message_predates_dismissal
//	  (an unguarded call keeps exactly {task_id, status, reopened} — criterion 4)
//	task_dismissals: closed_from_status, reopened_at, reopened_by,
//	                 reopened_by_message_id (migration 0026)
//	task_dismiss result: unchanged ({task_id, status, dismissed})
//
// GREENFIELD NOTE — EXPECTED RED. Migration 0026 is not applied, so
// droRequire0026 fails every test with one sentence; after it is applied,
// validateReopen ignores the two ids and every guarded call runs as an
// UNGUARDED reopen, which the assertions below catch on their own merits.
//
// CLEANUP PACT: this suite owns projects itest-dreopen-%, source_accounts
// provider itest-dreopen-src, threads itest-dreopen:%, and the audit rows of its
// two actors (plus everything by task). FK-ordered, at start AND end.

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
	droSlug     = "itest-dreopen-proj"
	droClient   = "itest-dreopen-client"
	droProvider = "itest-dreopen-src"
	droAccount  = "itest-dreopen@local"
	// A human dismisses (task_dismiss is humanOnly); the spine reopens, in the
	// capture:{connector} shape the SPEC names as a real caller.
	droHuman  = "dashboard:itest-dreopen"
	droSpine  = "capture:itest-dreopen"
	droSender = "Mario Cruz <mario@client.example>"
)

func droRequire0026(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name='task_dismissals'
		    AND column_name IN ('closed_from_status','reopened_at','reopened_by','reopened_by_message_id')`).
		Scan(&n); err != nil {
		t.Fatalf("probe task_dismissals columns: %v", err)
	}
	if n != 4 {
		t.Fatalf("task_dismissals has %d of the 4 columns migration 0026 adds. Criterion 19: "+
			"0026_dismissal_reopen.sql adds closed_from_status, reopened_at, reopened_by and "+
			"reopened_by_message_id; `make migrate` applies it to the compose db on :5433. Merging a "+
			"migration is not applying it", n)
	}
}

func droCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const projs = `(SELECT id FROM projects WHERE slug LIKE 'itest-dreopen-%')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	const accts = `(SELECT id FROM source_accounts WHERE provider = '` + droProvider + `')`
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`
	const actors = `('` + droHuman + `','` + droSpine + `')`
	for _, q := range []string{
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor IN ` + actors + `)`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor IN ` + actors,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM projects WHERE slug LIKE 'itest-dreopen-%'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'itest-dreopen:%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider = '` + droProvider + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

type droSuite struct {
	pool    *pgxpool.Pool
	ex      *executor.Executor
	project int64
	account int64
}

func newDroSuite(t *testing.T, ctx context.Context) *droSuite {
	t.Helper()
	pool := newToolsPool(t, ctx)
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db (this suite dismisses, reopens " +
			"and deletes); use the compose db on :5433")
	}
	t.Cleanup(pool.Close)
	droCleanup(t, ctx, pool)
	t.Cleanup(func() { droCleanup(t, ctx, pool) })
	droRequire0026(t, ctx, pool)

	s := &droSuite{pool: pool, ex: deliveryExecutor(pool)}
	s.project = seedProject(t, ctx, pool, droSlug, droClient)
	s.account = s.insID(t, ctx,
		`INSERT INTO source_accounts (provider, account_email, send_enabled) VALUES ($1,$2,false) RETURNING id`,
		droProvider, droAccount)
	return s
}

func (s *droSuite) insID(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", q, err)
	}
	return id
}

func (s *droSuite) count(t *testing.T, ctx context.Context, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

// dbNow is the DATABASE's clock — the one clock D2 compares on.
func (s *droSuite) dbNow(t *testing.T, ctx context.Context) time.Time {
	t.Helper()
	var now time.Time
	if err := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatalf("read db clock: %v", err)
	}
	return now
}

// message seeds one normalized message with its raw row (invariant 1). The two
// instants are explicit because D2 is precisely a statement about which of
// them the handler reads.
func (s *droSuite) message(t *testing.T, ctx context.Context, label string, thread int64,
	direction string, createdAt, sentAt time.Time) int64 {
	t.Helper()
	raw := s.insID(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,'{}',$3, now()) RETURNING id`,
		s.account, "itest-dreopen-"+label, "itest-dreopen-h-"+label)
	sender := droSender
	if direction == "outbound" {
		sender = "Salvador <salvador@example.test>"
	}
	return s.insID(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		    body_text, subject, sender, channel, created_at)
		 VALUES ($1,$2,$3,$4,$5,'itest-dreopen body','itest-dreopen',$6,'gmail',$7) RETURNING id`,
		raw, thread, direction, "<itest-dreopen-"+label+"@mail.example>", sentAt, sender, createdAt)
}

type droCase struct {
	task, thread      int64
	inbound, outbound int64 // on the thread BEFORE the dismissal (criterion 26)
	dismissal         int64
	dismissedAt       time.Time
}

// seedDismissed is criterion 26's production-shaped fixture.
func (s *droSuite) seedDismissed(t *testing.T, ctx context.Context, label, status, code string) droCase {
	t.Helper()
	var c droCase
	c.thread = s.insID(t, ctx,
		`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,'itest-dreopen','[]') RETURNING id`,
		"itest-dreopen:"+label)
	c.task = s.insID(t, ctx,
		`INSERT INTO tasks (project_id, title, status, source_thread_id) VALUES ($1,$2,$3,$4) RETURNING id`,
		s.project, "itest-dreopen "+label, status, c.thread)
	before := s.dbNow(t, ctx).Add(-2 * time.Hour)
	c.inbound = s.message(t, ctx, label+"-in0", c.thread, "inbound", before, before)
	c.outbound = s.message(t, ctx, label+"-out0", c.thread, "outbound", before.Add(time.Minute), before.Add(time.Minute))
	s.dismiss(t, ctx, c.task, code)
	c.dismissal, c.dismissedAt = s.latestDismissal(t, ctx, c.task)
	return c
}

func (s *droSuite) dismiss(t *testing.T, ctx context.Context, taskID int64, code string) map[string]any {
	t.Helper()
	return s.call(t, ctx, "task_dismiss", droHuman, taskID,
		`{"task_id":`+itoa(taskID)+`,"reason_code":"`+code+`","note":"itest-dreopen"}`)
}

func (s *droSuite) latestDismissal(t *testing.T, ctx context.Context, taskID int64) (int64, time.Time) {
	t.Helper()
	var id int64
	var at time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT id, created_at FROM task_dismissals WHERE task_id=$1 ORDER BY id DESC LIMIT 1`, taskID).
		Scan(&id, &at); err != nil {
		t.Fatalf("read the dismissal of task %d: %v — task_dismiss must write a row", taskID, err)
	}
	return id, at
}

func (s *droSuite) run(ctx context.Context, tool, actor string, taskID int64, args string) (map[string]any, error) {
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

func (s *droSuite) call(t *testing.T, ctx context.Context, tool, actor string, taskID int64, args string) map[string]any {
	t.Helper()
	out, err := s.run(ctx, tool, actor, taskID, args)
	if err != nil {
		t.Fatalf("%s(%s) as %s: %v", tool, args, actor, err)
	}
	return out
}

func guardedArgs(taskID, dismissalID, messageID int64, reason string) string {
	b, _ := json.Marshal(map[string]any{
		"task_id": taskID, "dismissal_id": dismissalID, "message_id": messageID, "reason": reason,
	})
	return string(b)
}

// guarded is the call the promote and capture passes make (D4): ids only.
func (s *droSuite) guarded(t *testing.T, ctx context.Context, taskID, dismissalID, messageID int64) map[string]any {
	t.Helper()
	return s.call(t, ctx, "task_reopen", droSpine, taskID,
		guardedArgs(taskID, dismissalID, messageID, "itest-dreopen: new inbound activity"))
}

func (s *droSuite) status(t *testing.T, ctx context.Context, taskID int64) string {
	t.Helper()
	var st string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, taskID).Scan(&st); err != nil {
		t.Fatalf("read task %d status: %v", taskID, err)
	}
	return st
}

func (s *droSuite) statusEvents(t *testing.T, ctx context.Context, taskID int64) int {
	t.Helper()
	return s.count(t, ctx,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='status_changed'`, taskID)
}

type droDismissal struct {
	id            int64
	reasonCode    string
	closedFrom    *string
	createdAt     time.Time
	reopenedAt    *time.Time
	reopenedBy    *string
	reopenedByMsg *int64
}

func (s *droSuite) dismissals(t *testing.T, ctx context.Context, taskID int64) []droDismissal {
	t.Helper()
	rows, err := s.pool.Query(ctx,
		`SELECT id, reason_code, closed_from_status, created_at, reopened_at, reopened_by, reopened_by_message_id
		   FROM task_dismissals WHERE task_id=$1 ORDER BY id`, taskID)
	if err != nil {
		t.Fatalf("read dismissals of task %d: %v", taskID, err)
	}
	defer rows.Close()
	var out []droDismissal
	for rows.Next() {
		var d droDismissal
		if err := rows.Scan(&d.id, &d.reasonCode, &d.closedFrom, &d.createdAt,
			&d.reopenedAt, &d.reopenedBy, &d.reopenedByMsg); err != nil {
			t.Fatalf("scan dismissal: %v", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate dismissals: %v", err)
	}
	return out
}

func droStr(p *string) string {
	if p == nil {
		return "<NULL>"
	}
	return *p
}

func droWantSkipped(t *testing.T, what string, out map[string]any, want string) {
	t.Helper()
	if out["reopened"] != false || out["skipped"] != want {
		t.Errorf("%s: task_reopen = %v, want {reopened:false, skipped:%q}", what, out, want)
	}
}

// ---- criterion 5: task_dismiss records where the task was closed FROM --------

// "task_dismiss writes closed_from_status = closeTransition's `from` when it
// transitioned, and NULL when the task was already closed."
//
// MUTATION: drop closed_from_status from dismissTask's INSERT -> every row reads
// NULL and the four open cases go red.
func TestDismissalReopen_DismissRecordsTheStatusItClosedFrom(t *testing.T) {
	ctx := context.Background()
	s := newDroSuite(t, ctx)

	for _, status := range []string{"holding", "ready", "blocked", "done_locally", "delivered", "closed"} {
		c := s.seedDismissed(t, ctx, "from-"+status, status, "not_actionable")
		rows := s.dismissals(t, ctx, c.task)
		if len(rows) != 1 {
			t.Fatalf("task dismissed from %s has %d dismissal rows, want 1", status, len(rows))
		}
		if status == "closed" {
			if rows[0].closedFrom != nil {
				t.Errorf("dismissing an ALREADY-closed task wrote closed_from_status=%q, want NULL. D5: nothing "+
					"transitioned (SWT-31 criterion 14), so there is no pre-dismissal status to restore and the "+
					"reopen falls back to ready", *rows[0].closedFrom)
			}
			continue
		}
		if got := droStr(rows[0].closedFrom); got != status {
			t.Errorf("dismissing a %s task wrote closed_from_status=%s, want %q. D5: the activity reopen "+
				"restores it, so a missing value flattens every restored task to ready", status, got, status)
		}
	}
}

// ---- criteria 2(e) + 3: the reopen itself ------------------------------------

func TestDismissalReopen_GuardedReopenRestoresStampsAndExplains(t *testing.T) {
	ctx := context.Background()
	s := newDroSuite(t, ctx)

	c := s.seedDismissed(t, ctx, "main", "ready", "handled_elsewhere")
	ingested := c.dismissedAt.Add(time.Minute)
	m := s.message(t, ctx, "main-in1", c.thread, "inbound", ingested, ingested)

	const callerReason = "itest-dreopen: a new message landed on the thread"
	out := s.call(t, ctx, "task_reopen", droSpine, c.task, guardedArgs(c.task, c.dismissal, m, callerReason))

	// The result contract.
	if out["reopened"] != true || out["status"] != "ready" {
		t.Errorf("guarded task_reopen = %v, want {reopened:true, status:ready}", out)
	}
	if out["dismissal_id"] != float64(c.dismissal) {
		t.Errorf("guarded task_reopen result dismissal_id = %v, want %d (criterion 2e)", out["dismissal_id"], c.dismissal)
	}
	if _, present := out["skipped"]; present {
		t.Errorf("a successful reopen carries skipped=%v; skipped is only for the three no-op answers", out["skipped"])
	}
	if got := s.status(t, ctx, c.task); got != "ready" {
		t.Errorf("after the guarded reopen tasks.status = %q, want ready (closed_from_status)", got)
	}

	// The stamp (D6).
	rows := s.dismissals(t, ctx, c.task)
	if len(rows) != 1 {
		t.Fatalf("dismissal rows = %d, want 1 — a reopen STAMPS the row, it never deletes or adds one", len(rows))
	}
	d := rows[0]
	if d.reopenedAt == nil || droStr(d.reopenedBy) != droSpine || d.reopenedByMsg == nil || *d.reopenedByMsg != m {
		t.Errorf("dismissal after the reopen = {reopened_at %v, reopened_by %s, reopened_by_message_id %v}, want "+
			"{set, %s, %d}. D7: reopened_by_message_id is the ONE typed place the outcome lives, and the "+
			"labelled-data consumer tells 'overtaken by activity' from 'a human undid it' by it alone",
			d.reopenedAt, droStr(d.reopenedBy), d.reopenedByMsg, droSpine, m)
	}

	// Criterion 3: the status_changed reason, asserted by ingredient.
	var from, to, reason string
	var keys []string
	if err := s.pool.QueryRow(ctx,
		`SELECT payload->>'from', payload->>'to', payload->>'reason',
		        ARRAY(SELECT jsonb_object_keys(payload) ORDER BY 1)
		   FROM task_events WHERE task_id=$1 AND event_type='status_changed' ORDER BY id DESC LIMIT 1`,
		c.task).Scan(&from, &to, &reason, &keys); err != nil {
		t.Fatalf("read the reopen's status_changed event: %v", err)
	}
	if from != "closed" || to != "ready" {
		t.Errorf("status_changed = (from %q, to %q), want (closed, ready)", from, to)
	}
	if strings.Join(keys, ",") != "from,reason,to" {
		t.Errorf("status_changed payload keys = %v, want exactly [from reason to]: the facts live in the "+
			"reason PROSE and in task_dismissals, never in a new jsonb key (SWT-31 criterion 20)", keys)
	}
	for _, want := range []struct{ frag, what string }{
		{"reopened after dismissal", "the literal"},
		{"handled_elsewhere", "the reason code"},
		{droHuman, "dismissed_by"},
		{strconv.FormatInt(m, 10), "the message id"},
		{"mario@client.example", "the sender"},
		{callerReason, "the caller's reason"},
	} {
		if !strings.Contains(reason, want.frag) {
			t.Errorf("status_changed reason %q does not contain %s (%q). Criterion 3: this line is what the task "+
				"page shows a human asking why a dismissed task came back", reason, want.what, want.frag)
		}
	}
	// The two instants, by date (the wording and format are the implementer's).
	for _, at := range []struct {
		t    time.Time
		what string
	}{{c.dismissedAt, "the dismissal time"}, {ingested, "the message's ingest time"}} {
		if !strings.Contains(reason, at.t.UTC().Format("2006-01-02")) &&
			!strings.Contains(reason, at.t.Local().Format("2006-01-02")) {
			t.Errorf("status_changed reason %q carries no date for %s (%s)", reason, at.what, at.t.UTC())
		}
	}

	// Invariant 3: one audit row for the call, carrying the task.
	if n := s.count(t, ctx,
		`SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool='task_reopen' AND actor=$2`,
		c.task, droSpine); n != 1 {
		t.Errorf("audit_events for the guarded task_reopen = %d, want 1", n)
	}
	if n := s.statusEvents(t, ctx, c.task); n != 2 {
		t.Errorf("status_changed events = %d, want 2 (the dismissal's close and this reopen)", n)
	}
}

// ---- criterion 22 (D5): restore the pre-dismissal status ---------------------

// "Dismiss a holding task, then an activity reopen must restore holding."
// Why it matters: a promote review-lane task would otherwise go LIVE on an
// inbound email, widening autonomy by message (D5).
//
// MUTATION: drop closed_from_status from dismiss's INSERT or from the handler's
// SELECT -> the task restores to `ready` and this goes red.
func TestDismissalReopen_RestoresHoldingNotReady(t *testing.T) {
	ctx := context.Background()
	s := newDroSuite(t, ctx)

	c := s.seedDismissed(t, ctx, "holding", "holding", "not_actionable")
	m := s.message(t, ctx, "holding-in1", c.thread, "inbound", c.dismissedAt.Add(time.Minute), c.dismissedAt.Add(time.Minute))
	out := s.guarded(t, ctx, c.task, c.dismissal, m)

	if got := s.status(t, ctx, c.task); got != "holding" {
		t.Errorf("a dismissed HOLDING task came back as %q, want holding. D5: always-ready would put a review-"+
			"lane task on the live board because an email arrived — the whitelist SWT-30 made a Go constant, "+
			"bypassed by message", got)
	}
	if out["status"] != "holding" {
		t.Errorf("guarded task_reopen result status = %v, want holding", out["status"])
	}
}

// D5's fallback: closed_from_status NULL (the task was already closed when it
// was dismissed, or a pre-0026 row) restores `ready`.
func TestDismissalReopen_NoClosedFromStatusFallsBackToReady(t *testing.T) {
	ctx := context.Background()
	s := newDroSuite(t, ctx)

	c := s.seedDismissed(t, ctx, "wasclosed", "closed", "wrong_kind")
	m := s.message(t, ctx, "wasclosed-in1", c.thread, "inbound", c.dismissedAt.Add(time.Minute), c.dismissedAt.Add(time.Minute))
	s.guarded(t, ctx, c.task, c.dismissal, m)
	if got := s.status(t, ctx, c.task); got != "ready" {
		t.Errorf("a task dismissed while already closed came back as %q, want ready (D5: NULL -> ready)", got)
	}
}

// ---- criterion 21 (D2): the clock is INGEST time ------------------------------

// "reopen iff normalized_messages.created_at > task_dismissals.created_at,
// strictly."
//
// MUTATIONS, each of which must turn this red:
//   - compare sent_at instead of created_at -> (a) skips and (b) reopens;
//   - compare COALESCE(sent_at, created_at) -> same;
//   - `>=` instead of `>` -> (c) reopens.
func TestDismissalReopen_TheClockIsIngestTimeNotSentAt(t *testing.T) {
	ctx := context.Background()
	s := newDroSuite(t, ctx)

	t.Run("(a) sent before the dismissal, ingested after: the lag case reopens", func(t *testing.T) {
		c := s.seedDismissed(t, ctx, "lag", "ready", "handled_elsewhere")
		m := s.message(t, ctx, "lag-in1", c.thread, "inbound",
			c.dismissedAt.Add(time.Minute), c.dismissedAt.Add(-5*time.Minute))
		out := s.guarded(t, ctx, c.task, c.dismissal, m)
		if out["reopened"] != true || s.status(t, ctx, c.task) != "ready" {
			t.Errorf("a message SENT at 09:55, dismissed at 10:00, INGESTED at 10:10 = %v (task %s), want a "+
				"reopen. D2: Salvador could not have seen it when he dismissed; a sent_at clock swallows it "+
				"silently — the exact failure this ticket fixes", out, s.status(t, ctx, c.task))
		}
	})

	t.Run("(b) sent after the dismissal, ingested before: skips", func(t *testing.T) {
		c := s.seedDismissed(t, ctx, "early", "ready", "handled_elsewhere")
		m := s.message(t, ctx, "early-in1", c.thread, "inbound",
			c.dismissedAt.Add(-time.Minute), c.dismissedAt.Add(5*time.Minute))
		out := s.guarded(t, ctx, c.task, c.dismissal, m)
		droWantSkipped(t, "a message ingested BEFORE the dismissal", out, "message_predates_dismissal")
		if got := s.status(t, ctx, c.task); got != "closed" {
			t.Errorf("task after a predating message = %q, want closed", got)
		}
		if rows := s.dismissals(t, ctx, c.task); len(rows) != 1 || rows[0].reopenedAt != nil {
			t.Errorf("the dismissal was stamped by a message that predates it: %+v", rows)
		}
		if n := s.statusEvents(t, ctx, c.task); n != 1 {
			t.Errorf("status_changed events = %d, want 1 (the dismissal only); a skip writes no event", n)
		}
	})

	t.Run("(c) the same instant: strictly greater, so it skips", func(t *testing.T) {
		c := s.seedDismissed(t, ctx, "tie", "ready", "handled_elsewhere")
		m := s.message(t, ctx, "tie-in1", c.thread, "inbound", c.dismissedAt, c.dismissedAt.Add(5*time.Minute))
		out := s.guarded(t, ctx, c.task, c.dismissal, m)
		droWantSkipped(t, "a message ingested at the dismissal's own instant", out, "message_predates_dismissal")
	})
}

// ---- criteria 2(a), 18(d), 25: outbound is an ERROR -----------------------------

// "If the message does not exist, or its direction <> 'inbound': ERROR, nothing
// written." Invariant 5 at the verb, gated on a COLUMN rather than on the
// caller (D13, layer 3). And it is checked FIRST — before `not_closed` — so a
// caller bug is never laundered into a quiet skip.
//
// MUTATION: remove the direction check -> the outbound case reopens the task and
// this goes red.
func TestDismissalReopen_AnOutboundMessageIsAnErrorAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	s := newDroSuite(t, ctx)

	c := s.seedDismissed(t, ctx, "out", "ready", "handled_elsewhere")
	ours := s.message(t, ctx, "out-out1", c.thread, "outbound", c.dismissedAt.Add(time.Minute), c.dismissedAt.Add(time.Minute))

	for _, tc := range []struct {
		name string
		msg  int64
	}{
		{"an outbound message ingested after the dismissal", ours},
		{"the fixture's own outbound message", c.outbound},
		{"a message that does not exist", 987654321},
	} {
		_, err := s.run(ctx, "task_reopen", droSpine, c.task,
			guardedArgs(c.task, c.dismissal, tc.msg, "itest-dreopen: must refuse"))
		if err == nil {
			t.Errorf("guarded task_reopen naming %s succeeded; want an ERROR (criterion 2a). Our own sends "+
				"re-enter via ingestion (invariant 5); a reopen triggered by one would resurrect a task on the "+
				"strength of Salvador's own reply", tc.name)
		}
	}
	if got := s.status(t, ctx, c.task); got != "closed" {
		t.Errorf("after refused calls the task is %q, want closed", got)
	}
	if rows := s.dismissals(t, ctx, c.task); len(rows) != 1 || rows[0].reopenedAt != nil {
		t.Errorf("a refused call stamped the dismissal: %+v — 'nothing written'", rows)
	}
	if n := s.statusEvents(t, ctx, c.task); n != 1 {
		t.Errorf("status_changed events = %d, want 1 (the dismissal only)", n)
	}

	// Order: (a) before (b). On an OPEN task an outbound message is still an
	// error, not `not_closed`.
	o := s.seedDismissed(t, ctx, "out-open", "ready", "handled_elsewhere")
	s.call(t, ctx, "task_reopen", droHuman, o.task, `{"task_id":`+itoa(o.task)+`,"reason":"mis-click"}`)
	late := s.message(t, ctx, "out-open-out1", o.thread, "outbound", o.dismissedAt.Add(time.Minute), o.dismissedAt.Add(time.Minute))
	if out, err := s.run(ctx, "task_reopen", droSpine, o.task,
		guardedArgs(o.task, o.dismissal, late, "itest-dreopen: must refuse")); err == nil {
		t.Errorf("guarded task_reopen with an OUTBOUND message on an open task = %v, want an ERROR. "+
			"Criterion 2's order puts the direction check (a) before not_closed (b)", out)
	}
}

// ---- criterion 2(b): not closed -> skipped -------------------------------------

func TestDismissalReopen_ANotClosedTaskIsSkipped(t *testing.T) {
	ctx := context.Background()
	s := newDroSuite(t, ctx)

	c := s.seedDismissed(t, ctx, "open", "ready", "duplicate")
	s.call(t, ctx, "task_reopen", droHuman, c.task, `{"task_id":`+itoa(c.task)+`,"reason":"mis-click"}`)
	m := s.message(t, ctx, "open-in1", c.thread, "inbound", c.dismissedAt.Add(time.Minute), c.dismissedAt.Add(time.Minute))

	out := s.guarded(t, ctx, c.task, c.dismissal, m)
	droWantSkipped(t, "a guarded call on an open task", out, "not_closed")
	if rows := s.dismissals(t, ctx, c.task); len(rows) != 1 || rows[0].reopenedByMsg != nil {
		t.Errorf("the human's plain-reopen stamp was overwritten by a skipped guarded call: %+v", rows)
	}
}

// ---- criterion 2(c): a stale or foreign dismissal id -> skipped ----------------

// "A stale dismissal id (reopened and re-dismissed between the read and the
// call) gets skipped: dismissal_not_open. The new dismissal is judged by the
// next message, not this one."
func TestDismissalReopen_AStaleOrForeignDismissalIsSkipped(t *testing.T) {
	ctx := context.Background()
	s := newDroSuite(t, ctx)

	c := s.seedDismissed(t, ctx, "stale", "ready", "not_actionable")
	m1 := s.message(t, ctx, "stale-in1", c.thread, "inbound", c.dismissedAt.Add(time.Minute), c.dismissedAt.Add(time.Minute))
	s.guarded(t, ctx, c.task, c.dismissal, m1)
	s.dismiss(t, ctx, c.task, "duplicate") // a second, OPEN dismissal
	d2, d2At := s.latestDismissal(t, ctx, c.task)
	if d2 == c.dismissal {
		t.Fatalf("the re-dismissal wrote no new row (criterion 6); cannot test staleness")
	}
	m2 := s.message(t, ctx, "stale-in2", c.thread, "inbound", d2At.Add(time.Minute), d2At.Add(time.Minute))

	out := s.guarded(t, ctx, c.task, c.dismissal, m2)
	droWantSkipped(t, "a guarded call naming the OLD, already-reopened dismissal", out, "dismissal_not_open")
	if got := s.status(t, ctx, c.task); got != "closed" {
		t.Errorf("a stale dismissal id reopened the task (%q). The new dismissal is judged by the NEXT call "+
			"that names it", got)
	}

	// A dismissal id belonging to ANOTHER task is not open FOR THIS TASK.
	other := s.seedDismissed(t, ctx, "stale-other", "ready", "not_actionable")
	out = s.guarded(t, ctx, c.task, other.dismissal, m2)
	droWantSkipped(t, "a guarded call naming another task's dismissal", out, "dismissal_not_open")
	if rows := s.dismissals(t, ctx, other.task); len(rows) != 1 || rows[0].reopenedAt != nil {
		t.Errorf("a call about task %d stamped task %d's dismissal: %+v", c.task, other.task, rows)
	}
	if got := s.status(t, ctx, other.task); got != "closed" {
		t.Errorf("the other task came back (%q)", got)
	}
}

// ---- criterion 4: the unguarded reopen stamps too -----------------------------

// "An unguarded task_reopen that transitions a task stamps any open dismissal of
// that task (reopened_by_message_id NULL). Its existing behaviour is otherwise
// byte-identical." D6: this is the MIS-CLICK signal — the only reopen that hints
// the label itself was wrong.
//
// MUTATION: skip the stamp on the unguarded path -> reopened_at stays NULL, and
// the next re-dismissal of this task conflicts on the partial index and loses
// its label (D14's reason, reached at runtime).
func TestDismissalReopen_UnguardedReopenStampsTheOpenDismissal(t *testing.T) {
	ctx := context.Background()
	s := newDroSuite(t, ctx)

	c := s.seedDismissed(t, ctx, "undo", "ready", "wrong_kind")
	out := s.call(t, ctx, "task_reopen", droHuman, c.task,
		`{"task_id":`+itoa(c.task)+`,"status":"ready","reason":"dismissed by mistake"}`)

	if len(out) != 3 || out["reopened"] != true || out["status"] != "ready" {
		t.Errorf("unguarded task_reopen result = %v, want exactly {task_id, status:ready, reopened:true} — "+
			"SWT-32 criteria 36-37, byte-identical", out)
	}
	rows := s.dismissals(t, ctx, c.task)
	if len(rows) != 1 {
		t.Fatalf("dismissal rows after an unguarded reopen = %d, want 1 (reopen_integration_test.go:326 pins "+
			"this too)", len(rows))
	}
	if rows[0].reopenedAt == nil || droStr(rows[0].reopenedBy) != droHuman || rows[0].reopenedByMsg != nil {
		t.Errorf("dismissal after a human's plain reopen = {reopened_at %v, reopened_by %s, msg %v}, want "+
			"{set, %s, NULL}. D6's table: reopened_at set with no message id IS the mis-click signal",
			rows[0].reopenedAt, droStr(rows[0].reopenedBy), rows[0].reopenedByMsg, droHuman)
	}
	var reason string
	if err := s.pool.QueryRow(ctx,
		`SELECT payload->>'reason' FROM task_events WHERE task_id=$1 AND event_type='status_changed'
		  ORDER BY id DESC LIMIT 1`, c.task).Scan(&reason); err != nil {
		t.Fatalf("read status_changed: %v", err)
	}
	if reason != "dismissed by mistake" {
		t.Errorf("unguarded reopen reason = %q, want the caller's prose verbatim (byte-identical)", reason)
	}
}

// ---- criteria 6 + 24: a second dismissal is a second row ----------------------

// "After an activity reopen, a second dismiss inserts a SECOND row. A replay of
// that second dismiss adds nothing."
//
// MUTATION: remove the restated `WHERE reopened_at IS NULL` from dismissTask's
// ON CONFLICT -> Postgres raises "no unique or exclusion constraint matching the
// ON CONFLICT specification" at runtime and the second dismiss fails here.
func TestDismissalReopen_ReDismissalAfterAnActivityReopenAddsASecondRow(t *testing.T) {
	ctx := context.Background()
	s := newDroSuite(t, ctx)

	c := s.seedDismissed(t, ctx, "again", "ready", "not_actionable")
	m := s.message(t, ctx, "again-in1", c.thread, "inbound", c.dismissedAt.Add(time.Minute), c.dismissedAt.Add(time.Minute))
	s.guarded(t, ctx, c.task, c.dismissal, m)

	out := s.dismiss(t, ctx, c.task, "wrong_kind")
	if out["dismissed"] != true {
		t.Errorf("the second dismiss = %v, want dismissed:true. D6: under the old total index this was a silent "+
			"label loss, and the task would then be plain-closed and never reopen again", out)
	}
	rows := s.dismissals(t, ctx, c.task)
	if len(rows) != 2 {
		t.Fatalf("dismissal rows after dismiss -> activity reopen -> dismiss = %d, want 2", len(rows))
	}
	if rows[0].reopenedByMsg == nil || *rows[0].reopenedByMsg != m {
		t.Errorf("the first row lost its stamp: %+v", rows[0])
	}
	if rows[1].reasonCode != "wrong_kind" || rows[1].reopenedAt != nil || droStr(rows[1].closedFrom) != "ready" {
		t.Errorf("the second row = {code %s, reopened_at %v, closed_from %s}, want {wrong_kind, NULL (open), ready}",
			rows[1].reasonCode, rows[1].reopenedAt, droStr(rows[1].closedFrom))
	}

	replay := s.dismiss(t, ctx, c.task, "not_actionable")
	if replay["dismissed"] != false {
		t.Errorf("a replayed dismiss = %v, want dismissed:false (a replay is not a correction)", replay)
	}
	if rows := s.dismissals(t, ctx, c.task); len(rows) != 2 || rows[1].reasonCode != "wrong_kind" {
		t.Errorf("after a replayed dismiss the rows are %+v, want the same two, second still wrong_kind", rows)
	}
}

// ---- Races and idempotence: one message reopens a task at most once ----------

// "A second guarded call for the same dismissal gets not_closed or
// dismissal_not_open; a re-dismissal after a reopen is newer than the message,
// so message_predates_dismissal. So one message id can reopen a task at most
// once, and never reopens a dismissal made after it was ingested."
func TestDismissalReopen_OneMessageReopensATaskAtMostOnce(t *testing.T) {
	ctx := context.Background()
	s := newDroSuite(t, ctx)

	c := s.seedDismissed(t, ctx, "once", "ready", "handled_elsewhere")
	// Ingested AFTER the first dismissal and BEFORE the re-dismissal below, as in
	// production: a re-dismissal made after the reopen is always newer than the
	// message that caused it. (A fixture stamped in the future would be newer
	// than the re-dismissal too, and 2(d) would then rightly reopen.)
	now := s.dbNow(t, ctx)
	m := s.message(t, ctx, "once-in1", c.thread, "inbound", now, now)
	if out := s.guarded(t, ctx, c.task, c.dismissal, m); out["reopened"] != true {
		t.Fatalf("first guarded call = %v, want reopened:true", out)
	}
	droWantSkipped(t, "the same call replayed", s.guarded(t, ctx, c.task, c.dismissal, m), "not_closed")

	s.dismiss(t, ctx, c.task, "handled_elsewhere")
	d2, _ := s.latestDismissal(t, ctx, c.task)
	droWantSkipped(t, "the old message against the NEW dismissal", s.guarded(t, ctx, c.task, d2, m),
		"message_predates_dismissal")
	if got := s.status(t, ctx, c.task); got != "closed" {
		t.Errorf("the re-dismissed task came back on an old message (%q)", got)
	}
}
