//go:build integration

package main

// swb 767: the "made by a console" fact lives in queueTasks' SQL, not in
// swbpush. This runs the real SELECT against the compose db:
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_<branch>?sslmode=disable \
//	  go test -tags integration -count=1 -run QueueTasks ./cmd/swb-push/

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/store"
)

func TestQueueTasks_ByConsoleComesFromTheCreateAudit(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use the compose db on :5433")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	const slug = "itest-swbpush"
	cleanup := func() {
		pool.Exec(ctx, `DELETE FROM audit_events WHERE args->>'project' = $1`, slug)
		pool.Exec(ctx, `DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, slug)
		pool.Exec(ctx, `DELETE FROM projects WHERE slug=$1`, slug)
	}
	cleanup()
	t.Cleanup(cleanup)
	var pid int64
	if err := pool.QueryRow(ctx, `INSERT INTO projects (name, slug) VALUES ($1,$1) RETURNING id`, slug).Scan(&pid); err != nil {
		t.Fatalf("project: %v", err)
	}
	task := func(title string) int64 {
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO tasks (project_id, title, status, assignee_type) VALUES ($1,$2,'ready','human') RETURNING id`,
			pid, title).Scan(&id); err != nil {
			t.Fatalf("task %q: %v", title, err)
		}
		return id
	}
	audit := func(actor, title, status string, offset time.Duration) {
		if _, err := pool.Exec(ctx, `INSERT INTO audit_events (actor, tool, args, status, started_at)
			VALUES ($1, 'create_task', jsonb_build_object('project', $2::text, 'title', $3::text), $4, now() + $5::interval)`,
			actor, slug, title, status, offset.String()); err != nil {
			t.Fatalf("audit: %v", err)
		}
	}
	console := task("console made this")
	audit("mcp:manual:salvo", "console made this", "ok", 0)
	opsctl := task("opsctl made this")
	audit("opsctl:salvo", "opsctl made this", "ok", 0)
	far := task("same title long ago")
	audit("mcp:manual:salvo", "same title long ago", "ok", -5*time.Minute)
	failed := task("create call not ok")
	audit("mcp:manual:salvo", "create call not ok", "started", 0)
	outside := task("arrived by mail")

	_, tasks, err := queueTasks(ctx, pool)
	if err != nil {
		t.Fatalf("queueTasks: %v", err)
	}
	want := map[int64]bool{console: true, opsctl: false, far: false, failed: false, outside: false}
	got := map[int64]bool{}
	for _, tk := range tasks {
		if _, ok := want[tk.ID]; ok {
			got[tk.ID] = tk.ByConsole
		}
	}
	for id, w := range want {
		g, ok := got[id]
		if !ok {
			t.Errorf("task %d not in the queue read", id)
			continue
		}
		if g != w {
			t.Errorf("task %d ByConsole = %v, want %v", id, g, w)
		}
	}
}
