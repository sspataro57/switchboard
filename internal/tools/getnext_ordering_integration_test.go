//go:build integration

package tools_test

// Integration tests for task_get_next ordering and filtering (SPEC
// 04-mcp-task-tools, acceptance criterion 6). Same gate/prefix conventions as
// lifecycle_integration_test.go (shares its helpers: newToolsPool,
// cleanupToolsData, seedProject, newExecutor, callOK, getNextOut, itoa).
//
// Ordering pinned by the SPEC: priority DESC, then plan_order ASC NULLS LAST,
// then created_at ASC, then id ASC; filtered to projects.client=$client,
// status='ready', assignee_type='claude', optional subproject; never returns
// holding / blocked / claimed / human tasks.
//
// Tasks are seeded via direct INSERT (fixtures) with explicit created_at so the
// FIFO tie-break is deterministic; get_next itself always goes through the
// executor. Statuses are advanced to 'claimed' between peeks to expose the next
// task in priority order (peek does not remove anything).

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// insertTask seeds one tasks row directly (fixture). planOrder nil => NULL;
// createdAgo is a Postgres interval string (e.g. "5 seconds") subtracted from now().
func insertTask(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	projectID int64, subproject, status, assignee string, priority int, planOrder *int, createdAgo string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, subproject, title, assignee_type, status, priority, plan_order, created_at)
		 VALUES ($1, NULLIF($2,''), $3, $4, $5, $6, $7, now() - ($8::interval)) RETURNING id`,
		projectID, subproject, "ord "+status, assignee, status, priority, planOrder, createdAgo).Scan(&id)
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}
	return id
}

func setClaimed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE tasks SET status='claimed' WHERE id=$1`, id); err != nil {
		t.Fatalf("set task %d claimed: %v", id, err)
	}
}

func TestGetNext_Integration_OrderingAndFilters(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()
	cleanupToolsData(t, ctx, pool)

	const (
		actor  = "itest-mcp-tools-ord"
		slug   = "itest-mcp-tools-ord"
		client = "itest-mcp-tools-ordclient"
	)
	pid := seedProject(t, ctx, pool, slug, client)
	ex := newExecutor(pool)

	p := func(v int) *int { return &v }

	// getNext peeks and returns the top task (or nil).
	getNext := func(args string) getNextOut {
		var out getNextOut
		res := callOK(t, ctx, ex, actor, "task_get_next", args)
		mustUnmarshal(t, res, &out)
		return out
	}

	// --- filters: only non-ready / non-claude tasks present -> nil ---
	insertTask(t, ctx, pool, pid, "", "ready", "human", 100, nil, "5 seconds")    // wrong assignee
	insertTask(t, ctx, pool, pid, "", "holding", "claude", 100, nil, "5 seconds") // not ready
	insertTask(t, ctx, pool, pid, "", "blocked", "claude", 100, nil, "5 seconds") // not ready
	insertTask(t, ctx, pool, pid, "", "claimed", "claude", 100, nil, "5 seconds") // not ready
	if got := getNext(`{"client":"` + client + `"}`); got.Task != nil {
		t.Fatalf("get_next with only excluded tasks returned %+v, want nil", got.Task)
	}

	// --- ordering: seed a ready/claude set ---
	tHigh := insertTask(t, ctx, pool, pid, "", "ready", "claude", 9, nil, "5 seconds")
	tMidA := insertTask(t, ctx, pool, pid, "", "ready", "claude", 5, p(1), "5 seconds")
	tMidB := insertTask(t, ctx, pool, pid, "", "ready", "claude", 5, p(2), "5 seconds")
	tMidNull := insertTask(t, ctx, pool, pid, "", "ready", "claude", 5, nil, "30 seconds") // earliest
	tMidNull2 := insertTask(t, ctx, pool, pid, "", "ready", "claude", 5, nil, "5 seconds") // later
	tLow := insertTask(t, ctx, pool, pid, "", "ready", "claude", 1, nil, "5 seconds")

	step := func(want int64, why string) {
		got := getNext(`{"client":"` + client + `"}`)
		if got.Task == nil || got.Task.ID != want {
			t.Fatalf("%s: get_next = %+v, want task id %d", why, got.Task, want)
		}
		setClaimed(t, ctx, pool, want) // remove from ready set to expose the next
	}

	step(tHigh, "priority DESC wins (9 > 5 > 1)")
	step(tMidA, "within priority 5, plan_order 1 < 2 < NULL")
	step(tMidB, "plan_order 2 < NULL")
	step(tMidNull, "plan_order NULL tie -> earliest created_at (FIFO)")
	step(tMidNull2, "next FIFO among NULL plan_order")
	step(tLow, "priority 1 last")

	// all ready tasks consumed; only excluded remain -> nil
	if got := getNext(`{"client":"` + client + `"}`); got.Task != nil {
		t.Fatalf("get_next after draining ready set returned %+v, want nil", got.Task)
	}

	// --- subproject filter (isolated project so it can't disturb the above) ---
	const (
		subSlug   = "itest-mcp-tools-ordsub"
		subClient = "itest-mcp-tools-ordsubclient"
	)
	spid := seedProject(t, ctx, pool, subSlug, subClient)
	tAlpha := insertTask(t, ctx, pool, spid, "alpha", "ready", "claude", 5, nil, "5 seconds")
	tBeta := insertTask(t, ctx, pool, spid, "beta", "ready", "claude", 9, nil, "5 seconds") // higher priority

	if got := getNext(`{"client":"` + subClient + `","subproject":"alpha"}`); got.Task == nil || got.Task.ID != tAlpha {
		t.Fatalf("get_next(subproject=alpha) = %+v, want %d (beta filtered out despite higher priority)", got.Task, tAlpha)
	}
	if got := getNext(`{"client":"` + subClient + `","subproject":"beta"}`); got.Task == nil || got.Task.ID != tBeta {
		t.Fatalf("get_next(subproject=beta) = %+v, want %d", got.Task, tBeta)
	}
	// no subproject filter: highest priority across the project (beta) wins
	if got := getNext(`{"client":"` + subClient + `"}`); got.Task == nil || got.Task.ID != tBeta {
		t.Fatalf("get_next(no subproject) = %+v, want %d (priority 9)", got.Task, tBeta)
	}
	// unknown subproject: nil
	if got := getNext(`{"client":"` + subClient + `","subproject":"gamma"}`); got.Task != nil {
		t.Fatalf("get_next(subproject=gamma) = %+v, want nil", got.Task)
	}
}
