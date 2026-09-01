//go:build integration

package tools_test

// SWT-20 acceptance criterion 3, and decision D7: `task_set_source_thread`
// records a task's source conversation, is an idempotent SUCCESS on replay, and
// REFUSES to re-aim a task that already carries one.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run SetSourceThread ./internal/tools/
//
// WHY REPLAY MUST SUCCEED AND RE-AIM MUST NOT — the two halves are not symmetric
// and neither is arbitrary:
//
//   - Replays are NORMAL. The capture driver may re-run a pass (`--all`, a
//     re-normalize, a CronJob overlap), and a tool that errored on the second
//     write would fail the whole pass and leave later messages unprocessed.
//   - A CHANGE silently re-aims every future delivery of that task. Provenance
//     is a recorded observation, not a mutable pointer; correcting a genuinely
//     wrong value is a psql statement plus a note, not a verb (D7). The refusal
//     applies to EVERYONE — there is no actor that may overwrite it, because
//     "who is calling" is not what makes an overwrite dangerous.
//
// The policy checker here is the REAL matrix, not the static allow-list, so
// these tests also pin that the tool is deliberately NOT humanOnly: its main
// caller is the capture engine as `capture:{connector}`, and a human-only gate
// would make the funnel's own observer illegal.
//
// GREENFIELD NOTE: red twice over today — the tool is not registered ("unknown
// tool"), and `tasks.source_thread_id` does not exist until migration 0019 is
// applied to the compose db.
//
// Mutual-cleanup pact: this suite owns projects slugged `itest-sst-%`, the
// actors `capture:itest-sst` / `opsctl:itest-sst`, and the two thread keys
// below. Cleaned in FK order before and after. No global count assertions.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	sstSlug        = "itest-sst-proj"
	sstCaptureActr = "capture:itest-sst"
	sstHumanActr   = "opsctl:itest-sst"
	sstClient      = "eeee1111-0000-0000-0000-00000000sst1"
	sstRoom        = "room_sst1a2b3c4d"
)

// Assembled in Go from the parts — the key format has one spelling and no SQL
// may build or dissect it (keyspelling_test.go).
func sstRoomedKey() string { return "upwork_crm:" + sstClient + ":room:" + sstRoom }
func sstLegacyKey() string { return "upwork_crm:" + sstClient + ":upwork" }

func sstOpen(t *testing.T, ctx context.Context) (*pgxpool.Pool, *executor.Executor) {
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
	sstCleanup(t, ctx, pool)
	t.Cleanup(func() { sstCleanup(t, ctx, pool) })

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	// The REAL matrix, on purpose: see the file header.
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	return pool, executor.New(reg, checker, audit.NewPGStore(pool))
}

func sstCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const projs = `(SELECT id FROM projects WHERE slug LIKE 'itest-sst-%')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	threads := []string{sstRoomedKey(), sstLegacyKey()}
	stmts := []struct {
		sql  string
		args []any
	}{
		// policy_decisions before audit_events: the FK points that way.
		{`DELETE FROM policy_decisions WHERE audit_event_id IN
		    (SELECT id FROM audit_events WHERE actor LIKE 'capture:itest-sst%' OR actor LIKE 'opsctl:itest-sst%'
		       OR task_id IN ` + tasksOf + `)`, nil},
		{`DELETE FROM audit_events WHERE actor LIKE 'capture:itest-sst%' OR actor LIKE 'opsctl:itest-sst%'
		    OR task_id IN ` + tasksOf, nil},
		{`DELETE FROM task_events WHERE task_id IN ` + tasksOf, nil},
		{`DELETE FROM deliveries WHERE task_id IN ` + tasksOf, nil},
		{`DELETE FROM external_refs WHERE task_id IN ` + tasksOf, nil},
		{`DELETE FROM tasks WHERE parent_id IS NOT NULL AND project_id IN ` + projs, nil},
		{`DELETE FROM tasks WHERE project_id IN ` + projs, nil},
		{`DELETE FROM projects WHERE slug LIKE 'itest-sst-%'`, nil},
		{`DELETE FROM normalized_threads WHERE thread_key = ANY($1)`, []any{threads}},
	}
	for _, st := range stmts {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

type sstFixture struct {
	task        int64
	roomed      int64
	legacy      int64
	roomedKey   string
	legacyKeyer string
}

func sstSeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool) sstFixture {
	t.Helper()
	ins := func(q string, args ...any) int64 {
		var id int64
		if err := pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
		return id
	}
	f := sstFixture{roomedKey: sstRoomedKey(), legacyKeyer: sstLegacyKey()}
	f.roomed = ins(`INSERT INTO normalized_threads (thread_key, subject, participants)
	                VALUES ($1,'itest-sst roomed','[]') RETURNING id`, f.roomedKey)
	f.legacy = ins(`INSERT INTO normalized_threads (thread_key, subject, participants)
	                VALUES ($1,'itest-sst legacy','[]') RETURNING id`, f.legacyKeyer)
	proj := ins(`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
	             VALUES ($1,$1,'itest-sst client','manual','dashboard','/tmp/itest-sst','any') RETURNING id`, sstSlug)
	f.task = ins(`INSERT INTO tasks (project_id, title, assignee_type, status)
	              VALUES ($1,'itest-sst work','claude','ready') RETURNING id`, proj)
	if f.roomed == f.legacy {
		t.Fatalf("fixture invalid: both thread keys resolved to one row")
	}
	return f
}

func sstCall(t *testing.T, ctx context.Context, ex *executor.Executor, actor string, taskID, threadID int64) (json.RawMessage, error) {
	t.Helper()
	args, err := json.Marshal(map[string]any{"task_id": taskID, "thread_id": threadID})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := ex.Execute(ctx, executor.Call{
		Tool: "task_set_source_thread", Actor: actor, Args: args, TaskID: &taskID,
	})
	return res.Output, err
}

func sstProvenance(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64) *int64 {
	t.Helper()
	var got *int64
	if err := pool.QueryRow(ctx, `SELECT source_thread_id FROM tasks WHERE id=$1`, taskID).Scan(&got); err != nil {
		t.Fatalf("read provenance for task %d: %v", taskID, err)
	}
	return got
}

// Criterion 3, clause 1: it sets the column, and the write went through the
// executor (invariant 3 — validate → policy → audit start → handler → audit
// complete).
func TestSetSourceThread_Integration_SetsTheColumnAndAudits(t *testing.T) {
	ctx := context.Background()
	pool, ex := sstOpen(t, ctx)
	f := sstSeed(t, ctx, pool)

	out, err := sstCall(t, ctx, ex, sstCaptureActr, f.task, f.roomed)
	if err != nil {
		t.Fatalf("task_set_source_thread: %v. The tool is deliberately NOT humanOnly — a human-only gate would "+
			"make the capture engine, its main caller, illegal (SPEC 'API / MCP tool changes')", err)
	}
	var res struct {
		TaskID   int64 `json:"task_id"`
		ThreadID int64 `json:"thread_id"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("parse result %s: %v", out, err)
	}
	if res.TaskID != f.task || res.ThreadID != f.roomed {
		t.Errorf("result = %s, want {\"task_id\":%d,\"thread_id\":%d}", out, f.task, f.roomed)
	}

	got := sstProvenance(t, ctx, pool, f.task)
	if got == nil || *got != f.roomed {
		t.Fatalf("tasks.source_thread_id = %v, want %d", got, f.roomed)
	}

	// Invariant 3: the decision is answerable from audit_events.
	var audits int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE tool='task_set_source_thread' AND actor=$1 AND task_id=$2`,
		sstCaptureActr, f.task).Scan(&audits); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if audits == 0 {
		t.Errorf("no audit_events row for task_set_source_thread on task %d by %q. Provenance is the fact that "+
			"decides where this task's deliveries may be aimed; 'who recorded it, and when' must be answerable",
			f.task, sstCaptureActr)
	}
}

// Criterion 3, clause 2: re-writing the SAME thread id is an idempotent success.
func TestSetSourceThread_Integration_SameThreadIsAnIdempotentSuccess(t *testing.T) {
	ctx := context.Background()
	pool, ex := sstOpen(t, ctx)
	f := sstSeed(t, ctx, pool)

	if _, err := sstCall(t, ctx, ex, sstCaptureActr, f.task, f.roomed); err != nil {
		t.Fatalf("first task_set_source_thread: %v", err)
	}
	if _, err := sstCall(t, ctx, ex, sstCaptureActr, f.task, f.roomed); err != nil {
		t.Fatalf("replaying task_set_source_thread with the SAME thread id failed: %v. Replays are normal — the "+
			"capture driver re-runs passes (--all, a re-normalize, an overlapping CronJob tick) — and an error "+
			"here fails the whole pass, leaving every later message in it unprocessed", err)
	}
	got := sstProvenance(t, ctx, pool, f.task)
	if got == nil || *got != f.roomed {
		t.Errorf("after a replay, tasks.source_thread_id = %v, want %d unchanged", got, f.roomed)
	}
}

// Criterion 3, clause 3, and D7: a DIFFERENT thread id on a task that already
// has one is refused, with an error NAMING the current value — and refused for
// every actor, human included.
//
// Naming the current value is not decoration. The person who hits this refusal
// is about to decide whether the recorded value or the new one is right, and
// they cannot do that from "provenance already set". The SPEC's remedy is a psql
// statement plus a note, which needs the number.
func TestSetSourceThread_Integration_RefusesToReAimAnExistingProvenance(t *testing.T) {
	ctx := context.Background()
	pool, ex := sstOpen(t, ctx)
	f := sstSeed(t, ctx, pool)

	if _, err := sstCall(t, ctx, ex, sstCaptureActr, f.task, f.roomed); err != nil {
		t.Fatalf("first task_set_source_thread: %v", err)
	}

	// Both an observer and a HUMAN. D7 is not actor-keyed: an overwrite silently
	// re-aims every future delivery of the task, and that is equally true when a
	// person does it by hand at 2am.
	for _, actor := range []string{sstCaptureActr, sstHumanActr} {
		actor := actor
		t.Run(actor, func(t *testing.T) {
			_, err := sstCall(t, ctx, ex, actor, f.task, f.legacy)
			if err == nil {
				t.Fatalf("task_set_source_thread re-aimed task %d from thread %d to %d for actor %q. D7: an "+
					"overwrite silently re-points every future delivery of this task at another conversation. "+
					"A correction is a psql statement plus a note, not a verb", f.task, f.roomed, f.legacy, actor)
			}
			msg := err.Error()
			if strings.Contains(msg, "unknown tool") {
				t.Fatalf("task_set_source_thread is not registered: %q", msg)
			}
			if !strings.Contains(msg, itoa(f.roomed)) {
				t.Errorf("refusal %q does not name the CURRENT value (thread %d). The reader is about to decide "+
					"which of the two is right and cannot do it without the number", msg, f.roomed)
			}
		})
	}

	if got := sstProvenance(t, ctx, pool, f.task); got == nil || *got != f.roomed {
		t.Errorf("after two refused re-aims, tasks.source_thread_id = %v, want %d unchanged", got, f.roomed)
	}
}

// Criterion 3's last clause: a non-existent task or thread is a clean refusal,
// not a panic and not a silent no-op.
//
// A silent no-op is the dangerous one: the capture engine treats a successful
// call as "provenance recorded" and moves on, and the task then reaches
// draft_delivery with nothing recorded — where it is refused, much later,
// somewhere else entirely.
func TestSetSourceThread_Integration_RefusesUnknownTaskOrThread(t *testing.T) {
	ctx := context.Background()
	pool, ex := sstOpen(t, ctx)
	f := sstSeed(t, ctx, pool)

	const missing = int64(2147480000) // far past any sequence in a test db

	t.Run("unknown task", func(t *testing.T) {
		if _, err := sstCall(t, ctx, ex, sstCaptureActr, missing, f.roomed); err == nil {
			t.Errorf("task_set_source_thread accepted task %d, which does not exist. A silent success tells the "+
				"capture pass that provenance was recorded, and the missing fact only surfaces as a "+
				"draft_delivery refusal much later, somewhere else", missing)
		}
	})
	t.Run("unknown thread", func(t *testing.T) {
		if _, err := sstCall(t, ctx, ex, sstCaptureActr, f.task, missing); err == nil {
			t.Errorf("task_set_source_thread accepted thread %d, which does not exist. The FK to "+
				"normalized_threads is what does this work — do not replace it with a lookup that can go stale",
				missing)
		}
		if got := sstProvenance(t, ctx, pool, f.task); got != nil {
			t.Errorf("tasks.source_thread_id = %d after a refused write; the task must be left untouched", *got)
		}
	})
}
