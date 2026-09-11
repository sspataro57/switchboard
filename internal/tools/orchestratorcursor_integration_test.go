//go:build integration

package tools_test

// orchestrator_cursor_advance against a real database — SWT-41
// (docs/tickets/orchestrator-deploy_SPEC.md) criterion 2 and D1. Every call
// goes through executor.Execute with the REAL registry and the REAL policy
// matrix (deliveryExecutor), so a policy denial or a missing audit row fails
// here rather than in production.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run OrchestratorCursorAdvance ./internal/tools/
//
// Build-tagged `integration` AND env-gated on DATABASE_URL (newToolsPool skips),
// with a FATAL guard on 192.168.50.49: this suite moves the global
// orchestrator_cursor row, the one that says which lifecycle events the
// engine has seen.
//
// WHY NOT UNIT TESTS: the contract IS storage behaviour — a compare-and-set on
// one row, a transaction-scoped advisory lock sharing a key space with the
// engine's session lock, and a histogram that must equal an independent GROUP
// BY. A fake store would supply the very values under test.
//
// IMPOSED CONTRACT (D1):
//
//	orchestrator_cursor_advance {expect_last_event_id: int >= 0, reason: non-empty}
//	  -> {from, to, skipped_total, skipped_by_type: {event_type: n}}
//	iff orchestrator_cursor.last_event_id == expect: set it to max(task_events.id).
//	Mismatch -> refused, naming the actual value. Engine lock held -> refused
//	("stop orchestratord first"). humanOnly; off MCP.
//
// THE LOCK KEY: this suite holds orch.AdvisoryLockKey — the ENGINE's constant,
// imported here (tools_test may import internal/orchestrator; internal/tools
// may not). So the refusal test is the behavioural pin that the tool locks the
// key a running orchestratord actually holds. lockkey_test.go in
// internal/orchestrator is the unit pin.
//
// CLEANUP PACT (IK): owns `itest-orchcur-%` projects and every audit row whose
// actor contains `itest-orchcur`, FK-ordered at start and in t.Cleanup. The
// cursor row is NOT restored: every suite that depends on it (orchestrator,
// dashboard health) positions it itself before use.
//
// GREENFIELD NOTE — EXPECTED RED: the tool is not registered, so every call
// fails with `unknown tool "orchestrator_cursor_advance"`.

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
	orch "github.com/sspataro57/switchboard/internal/orchestrator"
)

const (
	ocTool   = "orchestrator_cursor_advance"
	ocSlug   = "itest-orchcur-proj"
	ocClient = "itest-orchcur-client"
	ocHuman  = "opsctl:itest-orchcur"
	ocWorker = "worker:itest-orchcur"
)

func orchCursorCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const projs = `(SELECT id FROM projects WHERE slug LIKE 'itest-orchcur-%')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	const audits = `(SELECT id FROM audit_events WHERE actor LIKE '%itest-orchcur%' OR task_id IN ` + tasksOf + `)`
	for _, q := range []string{
		`DELETE FROM policy_decisions WHERE audit_event_id IN ` + audits,
		`DELETE FROM audit_events WHERE id IN ` + audits,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM projects WHERE slug LIKE 'itest-orchcur-%'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

type ocSuite struct {
	pool *pgxpool.Pool
	ex   *executor.Executor
	task int64
}

type advanceOut struct {
	From          int64            `json:"from"`
	To            int64            `json:"to"`
	SkippedTotal  int64            `json:"skipped_total"`
	SkippedByType map[string]int64 `json:"skipped_by_type"`
}

func newOCSuite(t *testing.T, ctx context.Context) *ocSuite {
	t.Helper()
	pool := newToolsPool(t, ctx)
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db (this suite moves the " +
			"orchestrator cursor); use the compose db on :5433")
	}
	t.Cleanup(pool.Close)
	orchCursorCleanup(t, ctx, pool)
	t.Cleanup(func() { orchCursorCleanup(t, context.Background(), pool) })

	s := &ocSuite{pool: pool, ex: deliveryExecutor(pool)}
	project := seedProject(t, ctx, pool, ocSlug, ocClient)
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, status) VALUES ($1, 'orchcur fixture', 'ready') RETURNING id`,
		project).Scan(&s.task); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return s
}

func (s *ocSuite) cursor(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	var n int64
	if err := s.pool.QueryRow(ctx,
		`SELECT last_event_id FROM orchestrator_cursor WHERE name='orchestrator'`).Scan(&n); err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	return n
}

func (s *ocSuite) head(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	var n int64
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(max(id),0) FROM task_events`).Scan(&n); err != nil {
		t.Fatalf("read head: %v", err)
	}
	return n
}

// setCursor positions the fixture. It is setup, not the path under test.
func (s *ocSuite) setCursor(t *testing.T, ctx context.Context, n int64) {
	t.Helper()
	if _, err := s.pool.Exec(ctx,
		`UPDATE orchestrator_cursor SET last_event_id=$1, updated_at=now() WHERE name='orchestrator'`, n); err != nil {
		t.Fatalf("set cursor: %v", err)
	}
}

func (s *ocSuite) event(t *testing.T, ctx context.Context, eventType string) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO task_events (task_id, event_type, payload) VALUES ($1,$2,'{}') RETURNING id`,
		s.task, eventType).Scan(&id); err != nil {
		t.Fatalf("insert %s event: %v", eventType, err)
	}
	return id
}

// seedBacklog puts the cursor at X = current head and writes a mixed backlog
// past it (the prod histogram's shape: mostly log, some status_changed, one
// delivery_sent). Returns X and the new head.
func (s *ocSuite) seedBacklog(t *testing.T, ctx context.Context) (x, head int64) {
	t.Helper()
	x = s.head(t, ctx)
	s.setCursor(t, ctx, x)
	for _, typ := range []string{"log", "log", "log", "status_changed", "status_changed", "delivery_sent"} {
		s.event(t, ctx, typ)
	}
	return x, s.head(t, ctx)
}

func (s *ocSuite) advance(ctx context.Context, actor string, expect int64, reason string) (json.RawMessage, error) {
	args, _ := json.Marshal(map[string]any{"expect_last_event_id": expect, "reason": reason})
	res, err := s.ex.Execute(ctx, executor.Call{Tool: ocTool, Actor: actor, Args: args})
	return res.Output, err
}

func (s *ocSuite) mustAdvance(t *testing.T, ctx context.Context, expect int64) advanceOut {
	t.Helper()
	raw, err := s.advance(ctx, ocHuman, expect, "itest start from now")
	if err != nil {
		t.Fatalf("%s(expect=%d) as %s: %v", ocTool, expect, ocHuman, err)
	}
	var out advanceOut
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s output %s: %v", ocTool, raw, err)
	}
	return out
}

// holdEngineLock holds the orchestrator's SESSION lock on its own connection,
// as a running orchestratord does.
func (s *ocSuite) holdEngineLock(t *testing.T, ctx context.Context) (release func()) {
	t.Helper()
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire lock conn: %v", err)
	}
	var ok bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, int64(orch.AdvisoryLockKey)).Scan(&ok); err != nil || !ok {
		conn.Release()
		t.Fatalf("take the orchestrator lock (ok=%v, err=%v) — is an orchestratord running against :5433?", ok, err)
	}
	done := false
	release = func() {
		if done {
			return
		}
		done = true
		var unlocked bool
		_ = conn.QueryRow(context.Background(), `SELECT pg_advisory_unlock($1)`, int64(orch.AdvisoryLockKey)).Scan(&unlocked)
		conn.Release()
	}
	t.Cleanup(release)
	return release
}

// ---- criterion 2, bullet 1: CAS to head + an independent histogram ------------

func TestOrchestratorCursorAdvance_Integration_MovesToHeadWithIndependentHistogram(t *testing.T) {
	ctx := context.Background()
	s := newOCSuite(t, ctx)
	x, head := s.seedBacklog(t, ctx)

	out := s.mustAdvance(t, ctx, x)

	if got := s.cursor(t, ctx); got != head {
		t.Errorf("cursor = %d after the advance, want head = %d", got, head)
	}
	if out.From != x || out.To != head {
		t.Errorf("output from=%d to=%d, want from=%d to=%d", out.From, out.To, x, head)
	}

	// Independent GROUP BY over (X, head].
	rows, err := s.pool.Query(ctx,
		`SELECT event_type, count(*) FROM task_events WHERE id > $1 AND id <= $2 GROUP BY 1`, x, head)
	if err != nil {
		t.Fatalf("independent histogram: %v", err)
	}
	want := map[string]int64{}
	var total int64
	for rows.Next() {
		var typ string
		var n int64
		if err := rows.Scan(&typ, &n); err != nil {
			t.Fatalf("scan histogram: %v", err)
		}
		want[typ] = n
		total += n
	}
	rows.Close()
	if want["delivery_sent"] != 1 || want["log"] != 3 || want["status_changed"] != 2 {
		t.Fatalf("POSITIVE CONTROL: the independent histogram %v is not the seeded backlog (another suite "+
			"writing task_events concurrently? `make integration` runs -p 1)", want)
	}
	if !reflect.DeepEqual(out.SkippedByType, want) {
		t.Errorf("skipped_by_type = %v, want the independent GROUP BY over (%d, %d] = %v", out.SkippedByType, x, head, want)
	}
	if out.SkippedTotal != total {
		t.Errorf("skipped_total = %d, want %d", out.SkippedTotal, total)
	}
}

// ---- criterion 2, bullet 2: a stale expect is refused, naming the value -------

func TestOrchestratorCursorAdvance_Integration_StaleExpectRefusedNamingCurrent(t *testing.T) {
	ctx := context.Background()
	s := newOCSuite(t, ctx)
	x, head := s.seedBacklog(t, ctx)
	s.mustAdvance(t, ctx, x)

	// An event arrives after the first advance. A second (stale) run must never
	// skip it: that is the whole reason the tool is a compare-and-set.
	late := s.event(t, ctx, "done_local")

	_, err := s.advance(ctx, ocHuman, x, "second run")
	if err == nil {
		t.Fatalf("a second advance with expect=%d succeeded; the cursor is %d, so the CAS must refuse — "+
			"otherwise a re-run skips event %d, which arrived after the first advance", x, head, late)
	}
	if !strings.Contains(err.Error(), strconv.FormatInt(head, 10)) {
		t.Errorf("refusal %q does not name the current value %d (D1: 'a mismatch is refused, naming the "+
			"actual value')", err, head)
	}
	if got := s.cursor(t, ctx); got != head {
		t.Errorf("cursor = %d after the refused call, want unchanged %d (event %d must stay unprocessed-visible)",
			got, head, late)
	}
}

// ---- criterion 2, bullet 3: refused while the engine holds its lock -----------

func TestOrchestratorCursorAdvance_Integration_RefusedWhileOrchestratorLockHeld(t *testing.T) {
	ctx := context.Background()
	s := newOCSuite(t, ctx)
	x, head := s.seedBacklog(t, ctx)

	release := s.holdEngineLock(t, ctx)
	_, err := s.advance(ctx, ocHuman, x, "while running")
	if err == nil {
		t.Fatal("the advance succeeded while the orchestrator's advisory lock was held on another " +
			"connection. D1: inside its transaction it takes pg_try_advisory_xact_lock on the SAME key; " +
			"a running engine makes that false and the tool refuses")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "orchestratord") {
		t.Errorf("refusal %q does not tell the operator to stop orchestratord first", err)
	}
	if got := s.cursor(t, ctx); got != x {
		t.Errorf("cursor = %d after the refused call, want unchanged %d", got, x)
	}

	// Positive control: with the lock released the identical call succeeds, so
	// the lock — not the CAS or validation — was the reason for the refusal.
	release()
	s.mustAdvance(t, ctx, x)
	if got := s.cursor(t, ctx); got != head {
		t.Errorf("after releasing the lock the advance left cursor = %d, want %d", got, head)
	}
}

// ---- criterion 2, bullet 4: audit_events + policy_decisions (invariant 3) -----

func TestOrchestratorCursorAdvance_Integration_AuditAndPolicyRows(t *testing.T) {
	ctx := context.Background()
	s := newOCSuite(t, ctx)
	x, head := s.seedBacklog(t, ctx)

	s.mustAdvance(t, ctx, x)
	if _, err := s.advance(ctx, ocHuman, x, "stale"); err == nil {
		t.Fatal("stale second advance succeeded (see StaleExpect test)")
	}
	// A non-human caller through the REAL matrix: humanOnly denies it, and the
	// denial is audited too.
	if _, err := s.advance(ctx, ocWorker, head, "a worker trying"); err == nil {
		t.Errorf("%s as %s succeeded; the tool is humanOnly (criterion 3)", ocTool, ocWorker)
	}
	if got := s.cursor(t, ctx); got != head {
		t.Errorf("cursor = %d after the denied call, want %d", got, head)
	}

	type auditRow struct {
		id       int64
		actor    string
		status   string
		decision string
		rule     string
	}
	rows, err := s.pool.Query(ctx,
		`SELECT a.id, a.actor, a.status, COALESCE(p.decision,''), COALESCE(p.rule,'')
		   FROM audit_events a LEFT JOIN policy_decisions p ON p.audit_event_id = a.id
		  WHERE a.tool = $1 AND a.actor LIKE '%itest-orchcur%'
		  ORDER BY a.id`, ocTool)
	if err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	var got []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.id, &r.actor, &r.status, &r.decision, &r.rule); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		got = append(got, r)
	}
	rows.Close()

	if len(got) != 3 {
		t.Fatalf("audit_events rows for %s = %d (%+v), want 3: one per call, each with its policy "+
			"decision (validate -> policy -> audit start -> handler -> audit complete; invariant 3)",
			ocTool, len(got), got)
	}
	want := []struct{ actor, status, decision, rule string }{
		{ocHuman, "ok", "allow", ""},
		{ocHuman, "error", "allow", ""},
		{ocWorker, "denied", "deny", "human_only"},
	}
	for i, w := range want {
		r := got[i]
		if r.actor != w.actor || r.status != w.status || r.decision != w.decision || (w.rule != "" && r.rule != w.rule) {
			t.Errorf("audit row %d = {actor %s, status %s, decision %q, rule %q}, want {actor %s, status %s, "+
				"decision %q, rule %q}", i, r.actor, r.status, r.decision, r.rule, w.actor, w.status, w.decision, w.rule)
		}
	}
}

// ---- criterion 2, bullet 5: validation -----------------------------------------

func TestOrchestratorCursorAdvance_Integration_ValidationRefusals(t *testing.T) {
	ctx := context.Background()
	s := newOCSuite(t, ctx)
	x, _ := s.seedBacklog(t, ctx)

	cases := []struct {
		name   string
		expect int64
		reason string
	}{
		// expect == the real cursor, so ONLY the empty reason can refuse it: the
		// CAS alone would succeed.
		{"empty reason", x, ""},
		// A negative expect can never match a cursor, so the CAS would refuse it
		// too; the "validate" check below proves it is refused as INVALID ARGS.
		{"negative expect", -1, "itest"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.advance(ctx, ocHuman, tc.expect, tc.reason)
			if err == nil {
				t.Fatalf("%s(expect=%d, reason=%q) succeeded, want refused", ocTool, tc.expect, tc.reason)
			}
			// executor.Execute wraps a validator error as "validate <tool> args: …".
			if !strings.Contains(err.Error(), "validate") {
				t.Errorf("refusal %q did not come from the validator; D1's args contract is "+
					"expect_last_event_id >= 0 and a non-empty reason", err)
			}
			if got := s.cursor(t, ctx); got != x {
				t.Errorf("cursor = %d after a refused call, want unchanged %d", got, x)
			}
		})
	}
}
