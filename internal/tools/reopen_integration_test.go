//go:build integration

package tools_test

// task_reopen against a real database — SWT-32
// (docs/tickets/jira-status-sync_SPEC.md) criteria 36, 37 and 38, plus D5 and
// D7. Every mutation goes through executor.Execute with the REAL registry and
// the REAL policy matrix (deliveryExecutor, defined in
// delivery_lifecycle_integration_test.go), so a policy denial or a missing audit
// row fails here rather than in production.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run Reopen ./internal/tools/
//
// Build-tagged `integration` AND env-gated on DATABASE_URL, with a FATAL guard
// on 192.168.50.49 — this suite deletes rows.
//
// WHY THESE ARE NOT UNIT TESTS: every one turns on a value POSTGRES produces —
// tasks.status under `SELECT ... FOR UPDATE`, the task_events row the same
// transaction wrote, the task_dismissals row that must SURVIVE a reopen, and the
// audit_events row the executor writes with a non-NULL task_id. A fake store
// would supply the very values the transitions are supposed to compute.
//
// GREENFIELD NOTE — EXPECTED RED. `task_reopen` is not registered, so the first
// Execute returns "unknown tool: task_reopen" and every case fails there.
//
// CLEANUP PACT (IK, "integration suites cross-pollute"; `make integration` runs
// -p 1 for this reason). This suite owns `itest-reopen-%` projects and cleans
// FK-ordered at start AND end. audit_events / policy_decisions are swept BY TASK
// rather than by actor, deliberately: one case calls as `ticketstatus:jira` —
// the real actor, because D7's claim is precisely that this verb is callable by
// the pass — and sweeping that actor globally would delete rows belonging to
// internal/ticketstatus's own suite.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
)

const (
	roSlug   = "itest-reopen-proj"
	roClient = "itest-reopen-client"
	// The pass's actor, spelled as the audit trail will store it. D7: task_reopen
	// is spine-facing but NOT humanOnly — the pass calls it exactly as the
	// orchestrator calls task_close, and gating on a human actor would make the
	// feature impossible.
	roPassActor  = "ticketstatus:jira"
	roHumanActor = "dashboard:itest-reopen"
)

func reopenCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const projs = `(SELECT id FROM projects WHERE slug LIKE 'itest-reopen-%')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	for _, q := range []string{
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + `)`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM projects WHERE slug LIKE 'itest-reopen-%'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

type reopenSuite struct {
	pool    *pgxpool.Pool
	ex      *executor.Executor
	project int64
}

func newReopenSuite(t *testing.T, ctx context.Context) *reopenSuite {
	t.Helper()
	pool := newToolsPool(t, ctx)
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db (this suite closes, " +
			"reopens and dismisses tasks); use the compose db on :5433")
	}
	t.Cleanup(pool.Close)
	reopenCleanup(t, ctx, pool)
	t.Cleanup(func() { reopenCleanup(t, ctx, pool) })

	s := &reopenSuite{pool: pool, ex: deliveryExecutor(pool)}
	s.project = seedProject(t, ctx, pool, roSlug, roClient)
	return s
}

func (s *reopenSuite) task(t *testing.T, ctx context.Context, title, status string) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, status) VALUES ($1,$2,$3) RETURNING id`,
		s.project, title, status).Scan(&id); err != nil {
		t.Fatalf("seed task %q: %v", title, err)
	}
	return id
}

func (s *reopenSuite) status(t *testing.T, ctx context.Context, taskID int64) string {
	t.Helper()
	var st string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, taskID).Scan(&st); err != nil {
		t.Fatalf("read task %d status: %v", taskID, err)
	}
	return st
}

func (s *reopenSuite) events(t *testing.T, ctx context.Context, taskID int64, eventType string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type=$2`,
		taskID, eventType).Scan(&n); err != nil {
		t.Fatalf("count %s events on task %d: %v", eventType, taskID, err)
	}
	return n
}

// call runs one tool and returns the decoded result.
func (s *reopenSuite) call(t *testing.T, ctx context.Context, tool, actor string, taskID int64, args string) map[string]any {
	t.Helper()
	res, err := s.ex.Execute(ctx, executor.Call{
		Tool: tool, Actor: actor, Args: json.RawMessage(args), TaskID: &taskID,
	})
	if err != nil {
		t.Fatalf("%s(%s) as %s: %v", tool, args, actor, err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatalf("decode %s result %s: %v", tool, res.Output, err)
	}
	return out
}

// ---- criteria 36 + 38: the round trip, in one transaction --------------------

// "One transaction: the tasks.status UPDATE and the status_changed
// {from:'closed', to:X, reason} event commit together. No new payload key."
//
// And D6's restore: `delivered` in, `delivered` out. A task this pass closed
// after delivery must come back delivered, not flattened to ready — the flatten
// is exactly the kind of quiet data loss nobody notices until a Deliver task is
// re-done.
func TestReopen_RestoresTheGivenStatusAndRecordsOneEvent(t *testing.T) {
	ctx := context.Background()
	s := newReopenSuite(t, ctx)

	id := s.task(t, ctx, "itest-reopen delivered work", "delivered")
	s.call(t, ctx, "task_close", roPassActor, id, `{"task_id":`+itoa(id)+`,"reason":"ITS-1 is done"}`)
	if got := s.status(t, ctx, id); got != "closed" {
		t.Fatalf("setup: task_close left status %q, want closed", got)
	}

	out := s.call(t, ctx, "task_reopen", roPassActor, id,
		`{"task_id":`+itoa(id)+`,"status":"delivered","reason":"ITS-1 left Done"}`)

	if got := s.status(t, ctx, id); got != "delivered" {
		t.Errorf("after task_reopen(status=delivered) tasks.status = %q, want \"delivered\". D6: the "+
			"reopen restores the status the task held when the pass closed it, which is the whole "+
			"reason closed_from_status is a column", got)
	}
	if out["reopened"] != true {
		t.Errorf("task_reopen result = %v, want reopened:true — the spine convention is that the "+
			"boolean says whether THIS call made the transition", out)
	}
	if out["status"] != "delivered" {
		t.Errorf("task_reopen result status = %v, want \"delivered\" (the status the task now holds, "+
			"so a caller need not re-read it)", out["status"])
	}

	var payload map[string]any
	var raw []byte
	if err := s.pool.QueryRow(ctx,
		`SELECT payload FROM task_events
		  WHERE task_id=$1 AND event_type='status_changed'
		  ORDER BY id DESC LIMIT 1`, id).Scan(&raw); err != nil {
		t.Fatalf("read the status_changed event: %v — criterion 38: the UPDATE and the event commit "+
			"TOGETHER, so a status with no event means the transaction was split", err)
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload["from"] != "closed" || payload["to"] != "delivered" {
		t.Errorf("status_changed payload = %v, want {from:closed, to:delivered, reason}", payload)
	}
	if payload["reason"] != "ITS-1 left Done" {
		t.Errorf("status_changed payload reason = %v, want the caller's prose. It is what the task page "+
			"shows a human asking why this came back", payload["reason"])
	}
	if len(payload) != 3 {
		t.Errorf("status_changed payload = %v, want exactly the three keys task_close already writes. "+
			"'No new payload key' (criterion 38): a fourth key here would be a label stored in jsonb, "+
			"which SWT-31 criterion 20 forbids and which nothing may ever query", payload)
	}

	// Invariant 3, concretely: the executor wrote an audit row carrying the task.
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool='task_reopen' AND actor=$2`,
		id, roPassActor).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if n != 1 {
		t.Errorf("audit_events for task_reopen on task %d by %s = %d, want 1. Callers pass "+
			"executor.Call.TaskID so audit start/complete carry the task — without it, 'why did this "+
			"task come back' is unanswerable from the audit trail", id, roPassActor, n)
	}
}

// "task_reopen refuses any task whose status is not closed; an already-open task
// is an idempotent success returning reopened:false with no event written (the
// spine convention for replays)."
//
// The convention exists because the spine replays: the orchestrator's drain and
// a re-run of this pass both re-issue calls whose effect already happened, and a
// verb that errored on a replay would stall the drain (task_block / task_unblock
// / task_close all made the same choice).
func TestReopen_OnAnOpenTaskIsAnIdempotentNoOp(t *testing.T) {
	ctx := context.Background()
	s := newReopenSuite(t, ctx)

	for _, status := range []string{"ready", "in_progress", "delivered"} {
		status := status
		t.Run(status, func(t *testing.T) {
			id := s.task(t, ctx, "itest-reopen already open "+status, status)
			out := s.call(t, ctx, "task_reopen", roPassActor, id,
				`{"task_id":`+itoa(id)+`,"status":"ready","reason":"ITS-2 left Done"}`)

			if out["reopened"] != false {
				t.Errorf("task_reopen on a %s task = %v, want reopened:false", status, out)
			}
			if got := s.status(t, ctx, id); got != status {
				t.Errorf("task_reopen on a %s task changed the status to %q. An already-open task is "+
					"a NO-OP, not a re-transition: moving an in_progress task to `ready` would take "+
					"the work away from the holder that claimed it", status, got)
			}
			if n := s.events(t, ctx, id, "status_changed"); n != 0 {
				t.Errorf("task_reopen on a %s task wrote %d status_changed event(s), want 0. Each one "+
					"NOTIFYs the orchestrator's drain and appears on the task page as a transition "+
					"that never happened", status, n)
			}
		})
	}
}

// A task that does not exist is an ERROR, not a silent success. The distinction
// is the same one task_close draws: a replay is a no-op, a nonexistent id is a
// caller bug that must not be swallowed.
func TestReopen_UnknownTaskIsAnError(t *testing.T) {
	ctx := context.Background()
	s := newReopenSuite(t, ctx)

	_, err := s.ex.Execute(ctx, executor.Call{
		Tool: "task_reopen", Actor: roPassActor,
		Args: json.RawMessage(`{"task_id":987654321,"reason":"nope"}`),
	})
	if err == nil {
		t.Fatal("task_reopen on a nonexistent task = nil error, want a failure naming the task")
	}
	if !strings.Contains(err.Error(), "987654321") {
		t.Errorf("task_reopen on a nonexistent task = %q, want the id in the message", err)
	}
}

// The default target, D6's fall-back: no `status` argument means `ready`.
func TestReopen_DefaultsToReady(t *testing.T) {
	ctx := context.Background()
	s := newReopenSuite(t, ctx)

	id := s.task(t, ctx, "itest-reopen no status given", "holding")
	s.call(t, ctx, "task_close", roPassActor, id, `{"task_id":`+itoa(id)+`,"reason":"ITS-3 done"}`)
	s.call(t, ctx, "task_reopen", roPassActor, id, `{"task_id":`+itoa(id)+`,"reason":"ITS-3 reopened"}`)

	if got := s.status(t, ctx, id); got != "ready" {
		t.Errorf("task_reopen with no status left the task %q, want \"ready\" — the fall-back the pass "+
			"uses when it never recorded a closed_from_status (criterion 27)", got)
	}
}

// ---- D5: the tool stays general; the dismissal check lives in the PASS -------

// "A human must be able to undo a mis-click; SWT-31 names re-open as that
// remedy. So the tool stays general and the pass is the thing that refuses to
// override a human."
//
// Two halves, both asserted: the reopen SUCCEEDS on a dismissed task, and the
// task_dismissals row SURVIVES it. The label is training data — a future
// precision ticket GROUPs BY it — so a reopen that deleted the row would quietly
// destroy the record of a judgement that WAS made, even if it was later undone.
func TestReopen_UndoesADismissalWithoutDeletingTheLabel(t *testing.T) {
	ctx := context.Background()
	s := newReopenSuite(t, ctx)

	id := s.task(t, ctx, "itest-reopen dismissed by mistake", "ready")
	s.call(t, ctx, "task_dismiss", roHumanActor, id,
		`{"task_id":`+itoa(id)+`,"reason_code":"wrong_kind","note":"mis-click"}`)
	if got := s.status(t, ctx, id); got != "closed" {
		t.Fatalf("setup: task_dismiss left status %q, want closed", got)
	}

	out := s.call(t, ctx, "task_reopen", roHumanActor, id,
		`{"task_id":`+itoa(id)+`,"status":"ready","reason":"dismissed by mistake"}`)
	if out["reopened"] != true {
		t.Errorf("task_reopen on a DISMISSED task = %v, want reopened:true. D5: the tool is general — "+
			"the refusal to override a human lives in the reconciler, not here, because a human must "+
			"be able to undo their own mis-click", out)
	}
	if got := s.status(t, ctx, id); got != "ready" {
		t.Errorf("after undoing a dismissal the task is %q, want \"ready\"", got)
	}

	var labels int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM task_dismissals WHERE task_id=$1`, id).Scan(&labels); err != nil {
		t.Fatalf("count dismissal labels: %v", err)
	}
	if labels != 1 {
		t.Errorf("task_dismissals rows for task %d after a reopen = %d, want 1. The label is LABELLED "+
			"DATA (SWT-31's whole purpose); a reopen records that the judgement was undone in "+
			"task_events, it does not erase that it was made", id, labels)
	}
}

// ---- criterion 37's behavioural half: ONE refusal, both verbs ---------------

// task_close refuses active work; so must anything sharing its transition
// helper. This is the assertion that would go red if a "reopen needs its own
// helper" refactor left the two verbs with two spellings of the active-work
// list — the drift SWT-31's structural test guards in source and this one
// guards in behaviour.
func TestReopen_ShareTheActiveWorkRefusalWithClose(t *testing.T) {
	ctx := context.Background()
	s := newReopenSuite(t, ctx)

	for _, status := range []string{"claimed", "in_progress", "needs_feedback"} {
		status := status
		t.Run(status, func(t *testing.T) {
			id := s.task(t, ctx, "itest-reopen active "+status, status)
			_, err := s.ex.Execute(ctx, executor.Call{
				Tool: "task_close", Actor: roPassActor, TaskID: &id,
				Args: json.RawMessage(`{"task_id":` + itoa(id) + `,"reason":"ITS-4 done"}`),
			})
			if err == nil {
				t.Fatalf("task_close on a %s task succeeded; the pass depends on this refusal to reach "+
					"its `refused_active` branch (criterion 24) instead of closing work out from "+
					"under a holder", status)
			}
			if !strings.Contains(err.Error(), "refusing to close active work") {
				t.Errorf("task_close on a %s task = %q, want the shared refusal message. Criterion 37: "+
					"ONE transition helper — a second spelling drifts silently", status, err)
			}
			if got := s.status(t, ctx, id); got != status {
				t.Errorf("the refused task moved to %q", got)
			}
		})
	}
}
