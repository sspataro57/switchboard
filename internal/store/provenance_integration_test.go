//go:build integration

package store_test

// SWT-20 acceptance criterion 4, the half only Postgres can prove:
// `store.TaskSourceThread` reads `tasks.source_thread_id` — the COLUMN, written
// by the database, not a struct field a fixture set in Go.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run Provenance ./internal/store/
//
// IK, "the landmine's 6th instance": `drafts.Run`'s locality guard passed from
// the day it was written while being completely inert, because `DeliverTasks`
// never SELECTed the column that fed it — and "the unit test could not have
// caught it, by construction, because the unit test is the thing supplying the
// value". The rule that came out of it is the reason this file exists: for any
// predicate whose input comes from a column, the regression test belongs in the
// integration suite, and it must go red when the column stops being read.
// TestProvenance_Integration_ReadsTheColumnNotAFixture is that mutation check,
// written as an assertion instead of a manual step.
//
// GREENFIELD NOTE: red twice over today — `store.TaskSourceThread` does not
// exist (compile), and `tasks.source_thread_id` does not exist until migration
// 0019 is applied to the compose db (`make db-up && make migrate`). "column
// source_thread_id does not exist" is the expected first failure once the
// function compiles.
//
// Mutual-cleanup pact: this suite owns projects slugged `itest-prov-%` and the
// thread keys listed in provCleanup, and deletes them in FK order before and
// after. It makes no global count assertion.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/store"
)

const (
	provSlug   = "itest-prov-store"
	provClient = "cccc1111-0000-0000-0000-0000000prov1"
	provRoom   = "room_prov1a2b3c"
)

// Built in Go from the pieces, never in SQL: the key format has exactly one
// spelling (upworkcrm.ThreadKey/ParseThreadKey) and keyspelling_test.go fails
// any SQL that builds or dissects it.
func provRoomedKey() string { return "upwork_crm:" + provClient + ":room:" + provRoom }
func provOtherKey() string  { return "upwork_crm:" + provClient + ":upwork" }

func provOpen(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use the compose db on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	provCleanup(t, ctx, pool)
	t.Cleanup(func() { provCleanup(t, ctx, pool) })
	return pool
}

func provCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	threads := []string{provRoomedKey(), provOtherKey()}
	const projs = `(SELECT id FROM projects WHERE slug LIKE 'itest-prov-%')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	stmts := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM task_events WHERE task_id IN ` + tasksOf, nil},
		{`DELETE FROM deliveries WHERE task_id IN ` + tasksOf, nil},
		{`DELETE FROM external_refs WHERE task_id IN ` + tasksOf, nil},
		// children before parents (tasks.parent_id self-FK)
		{`DELETE FROM tasks WHERE parent_id IS NOT NULL AND project_id IN ` + projs, nil},
		{`DELETE FROM tasks WHERE project_id IN ` + projs, nil},
		{`DELETE FROM projects WHERE slug LIKE 'itest-prov-%'`, nil},
		{`DELETE FROM normalized_threads WHERE thread_key = ANY($1)`, []any{threads}},
	}
	for _, st := range stmts {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

type provFixture struct {
	pool         *pgxpool.Pool
	threadID     int64
	otherThread  int64
	parent       int64 // carries provenance
	child        int64 // carries none; its parent does
	orphan       int64 // no provenance, no parent
	orphanParent int64 // parent of orphan-with-parentless-provenance case
}

func provSeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool) provFixture {
	t.Helper()
	f := provFixture{pool: pool}
	ins := func(q string, args ...any) int64 {
		var id int64
		if err := pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
		return id
	}
	f.threadID = ins(`INSERT INTO normalized_threads (thread_key, subject, participants)
	                  VALUES ($1,'itest-prov','[]') RETURNING id`, provRoomedKey())
	f.otherThread = ins(`INSERT INTO normalized_threads (thread_key, subject, participants)
	                     VALUES ($1,'itest-prov other','[]') RETURNING id`, provOtherKey())
	projID := ins(`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
	               VALUES ($1,$1,'itest-prov client','manual','dashboard','/tmp/itest-prov','any') RETURNING id`, provSlug)

	// POSTGRES supplies the value: the column is written by an INSERT, and every
	// assertion below reads it back through TaskSourceThread's own query.
	f.parent = ins(`INSERT INTO tasks (project_id, title, assignee_type, status, source_thread_id)
	                VALUES ($1,'itest-prov work','claude','done_locally',$2) RETURNING id`, projID, f.threadID)
	f.child = ins(`INSERT INTO tasks (project_id, parent_id, title, assignee_type, status)
	               VALUES ($1,$2,'Deliver #itest-prov','claude','ready') RETURNING id`, projID, f.parent)
	f.orphanParent = ins(`INSERT INTO tasks (project_id, title, assignee_type, status)
	                      VALUES ($1,'itest-prov unprovenanced work','claude','done_locally') RETURNING id`, projID)
	f.orphan = ins(`INSERT INTO tasks (project_id, parent_id, title, assignee_type, status)
	                VALUES ($1,$2,'Deliver #itest-prov unprovenanced','claude','ready') RETURNING id`, projID, f.orphanParent)
	return f
}

// Criterion 4, clause 1: resolved on the NAMED task.
func TestProvenance_Integration_ResolvesOnTheNamedTask(t *testing.T) {
	ctx := context.Background()
	pool := provOpen(t, ctx)
	f := provSeed(t, ctx, pool)

	got, found, err := store.TaskSourceThread(ctx, pool, f.parent)
	if err != nil {
		t.Fatalf("TaskSourceThread(parent): %v", err)
	}
	if !found {
		t.Fatalf("found = false for task %d, whose tasks.source_thread_id is %d in Postgres right now. "+
			"The resolver is not reading the column (IK: 'test the column, not the fixture')", f.parent, f.threadID)
	}
	if got.ThreadID != f.threadID {
		t.Errorf("ThreadID = %d, want %d", got.ThreadID, f.threadID)
	}
	if got.TaskID != f.parent {
		t.Errorf("TaskID = %d, want %d — the task the value was found ON", got.TaskID, f.parent)
	}
	if got.ThreadKey != provRoomedKey() {
		t.Errorf("ThreadKey = %q, want %q. The key is what `drafts` parses and what `draft_delivery` compares "+
			"the caller's target_ref against; resolving the id without the key leaves both callers re-querying",
			got.ThreadKey, provRoomedKey())
	}
}

// Criterion 4, clause 2: resolved from the PARENT when the named task has none.
//
// Premise 8: `drafts` calls draft_delivery with the WORK task id and the
// delivery row hangs off the work task, but a human may call it on the R3
// Deliver child. The walk is one level, which is exactly the depth R3 creates.
func TestProvenance_Integration_WalksOneLevelToTheParent(t *testing.T) {
	ctx := context.Background()
	pool := provOpen(t, ctx)
	f := provSeed(t, ctx, pool)

	// Fixture guard: the child must genuinely carry NULL, or this test passes
	// for the reason the previous one did.
	var childHas bool
	if err := pool.QueryRow(ctx,
		`SELECT source_thread_id IS NOT NULL FROM tasks WHERE id=$1`, f.child).Scan(&childHas); err != nil {
		t.Fatalf("read child provenance: %v", err)
	}
	if childHas {
		t.Fatalf("fixture invalid: the Deliver child %d carries its own source_thread_id, so the parent walk "+
			"is not being exercised", f.child)
	}

	got, found, err := store.TaskSourceThread(ctx, pool, f.child)
	if err != nil {
		t.Fatalf("TaskSourceThread(child): %v", err)
	}
	if !found {
		t.Fatalf("found = false for the Deliver child %d whose PARENT %d records thread %d. A human calling "+
			"draft_delivery on the Deliver task would then be refused for a task that does record its "+
			"conversation (premise 8)", f.child, f.parent, f.threadID)
	}
	if got.ThreadID != f.threadID {
		t.Errorf("ThreadID = %d, want %d (the parent's)", got.ThreadID, f.threadID)
	}
	if got.TaskID != f.parent {
		t.Errorf("TaskID = %d, want the PARENT %d — the SourceThread must say which task the observation is "+
			"recorded on, or a caller cannot tell an inherited provenance from an own one", got.TaskID, f.parent)
	}
}

// Criterion 4, clause 3: neither has one -> found=false, no error.
func TestProvenance_Integration_NeitherTaskNorParentHasOne(t *testing.T) {
	ctx := context.Background()
	pool := provOpen(t, ctx)
	f := provSeed(t, ctx, pool)

	got, found, err := store.TaskSourceThread(ctx, pool, f.orphan)
	if err != nil {
		t.Fatalf("TaskSourceThread on a task with no provenance returned an error: %v. Criterion 4: found=false. "+
			"Every task that exists today is in this state — no backfill is possible — so an error here fails "+
			"the whole drafts scan on the first unprovenanced task it meets", err)
	}
	if found {
		t.Errorf("found = true (%+v) for task %d and its parent %d, neither of which has source_thread_id set",
			got, f.orphan, f.orphanParent)
	}
}

// The mutation check, as an assertion rather than a manual step (IK: "mutate the
// SELECT to a literal and watch it go red; if it stays green you have tested
// your fixture").
//
// Same task, same call, one UPDATE in between. If the resolver is reading
// anything other than `tasks.source_thread_id` — a join through external_refs,
// a cached value, a fixture field — this test cannot tell, and it goes red.
func TestProvenance_Integration_ReadsTheColumnNotAFixture(t *testing.T) {
	ctx := context.Background()
	pool := provOpen(t, ctx)
	f := provSeed(t, ctx, pool)

	if _, _, err := store.TaskSourceThread(ctx, pool, f.parent); err != nil {
		t.Fatalf("TaskSourceThread before the mutation: %v", err)
	}

	// Repoint the column at the OTHER thread, in Postgres.
	if _, err := pool.Exec(ctx, `UPDATE tasks SET source_thread_id=$2 WHERE id=$1`, f.parent, f.otherThread); err != nil {
		t.Fatalf("repoint source_thread_id: %v", err)
	}
	got, found, err := store.TaskSourceThread(ctx, pool, f.parent)
	if err != nil {
		t.Fatalf("TaskSourceThread after the mutation: %v", err)
	}
	if !found || got.ThreadID != f.otherThread || got.ThreadKey != provOtherKey() {
		t.Errorf("after repointing tasks.source_thread_id to %d (%q), TaskSourceThread still reports "+
			"(found=%v, thread %d, key %q). The resolver is not reading the column",
			f.otherThread, provOtherKey(), found, got.ThreadID, got.ThreadKey)
	}

	// And clearing it must be visible too — the state every existing task is in.
	if _, err := pool.Exec(ctx, `UPDATE tasks SET source_thread_id=NULL WHERE id=$1`, f.parent); err != nil {
		t.Fatalf("clear source_thread_id: %v", err)
	}
	if _, found, err := store.TaskSourceThread(ctx, pool, f.parent); err != nil || found {
		t.Errorf("after clearing the column, TaskSourceThread reports found=%v err=%v; want (false, nil)", found, err)
	}
}
