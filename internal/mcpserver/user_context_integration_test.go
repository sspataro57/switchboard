//go:build integration

package mcpserver_test

// SWT-56 (docs/tickets/signal-session-name_SPEC.md) criteria 30, 31 and 36
// against a real database: task_context through a ProfileUser server over the
// PRODUCTION executor wiring (udExecutor: policy.NewMatrix in front of the
// static allow-list), the user_drafts_integration_test.go harness.
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_isosess?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run UserContext ./internal/mcpserver/
//
// Criterion 36 is Q1 = (a) (owner, 2026-09-15: "Show everything"): NO mail-text
// filter — a local_only project's task returns its capture body and log lines
// unredacted through the user profile. Pinned so a later gate is deliberate.
//
// Placement note: the SPEC's file list puts criteria 31 and 36 in internal/tools;
// both say "through the user profile", which is this package's adapter, so they
// live here beside criterion 30.
//
// The worker id is test-owned and HUMAN-shaped (manual:itest-mcp-ctx), exactly as
// manual:salvo is for policy; MCP audit rows carry a NULL task_id, so cleanup
// deletes them by actor.
//
// GREENFIELD NOTE — EXPECTED RED: the user profile does not list task_context
// ("not available over MCP"), and the document has no marker keys.
//
// MUTATIONS: delete userProfilePins["task_context"] → ReadOnlyThroughTheProfile;
// drop working_session (or its CASE gate) from taskContext's SELECT → TheDocument.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/mcpserver"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	ucSlug        = "itest-mcp-ctx-proj"
	ucPrivateSlug = "itest-mcp-ctx-private"
	ucWorker      = "manual:itest-mcp-ctx"
	ucActor       = "mcp:" + ucWorker
	ucCapture     = "capture:itest-mcp-ctx"
)

func ucPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use an isolated db on the compose :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	ucCleanup(t, ctx, pool)
	t.Cleanup(func() {
		ucCleanup(t, ctx, pool)
		pool.Close()
	})
	return pool
}

func ucCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const projs = `(SELECT id FROM projects WHERE slug IN ('` + ucSlug + `','` + ucPrivateSlug + `'))`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	const audits = `(SELECT id FROM audit_events WHERE actor LIKE '%itest-mcp-ctx%' OR task_id IN ` + tasksOf + `)`
	for _, q := range []string{
		`DELETE FROM policy_decisions WHERE audit_event_id IN ` + audits,
		`DELETE FROM audit_events WHERE id IN ` + audits,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM feedback_requests WHERE task_id IN ` + tasksOf,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM decisions WHERE project_id IN ` + projs,
		`DELETE FROM projects WHERE slug IN ('` + ucSlug + `','` + ucPrivateSlug + `')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func ucProject(t *testing.T, ctx context.Context, pool *pgxpool.Pool, slug, locality string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		VALUES ($1,$1,'itest-mcp-ctx-client','manual','dashboard','/tmp/itest',$2) RETURNING id`, slug, locality).Scan(&id); err != nil {
		t.Fatalf("seed project %s: %v", slug, err)
	}
	return id
}

func ucCall(t *testing.T, ctx context.Context, srv *mcpserver.Server, tool, args string) json.RawMessage {
	t.Helper()
	out, err := srv.CallTool(ctx, tool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("CallTool(%s, %s): %v", tool, args, err)
	}
	return out
}

type ucDoc struct {
	Task      map[string]any      `json:"task"`
	Decisions []map[string]string `json:"decisions"`
	Feedback  []map[string]string `json:"feedback"`
	Events    []map[string]string `json:"events"`
}

func ucContext(t *testing.T, ctx context.Context, srv *mcpserver.Server, id int64) (ucDoc, string) {
	t.Helper()
	out := ucCall(t, ctx, srv, "task_context", fmt.Sprintf(`{"task_id":%d}`, id))
	var d ucDoc
	if err := json.Unmarshal(out, &d); err != nil {
		t.Fatalf("task_context output %s: %v", out, err)
	}
	return d, string(out)
}

func ucStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) (status string, openClaims, changes int) {
	t.Helper()
	if err := pool.QueryRow(ctx, `SELECT status,
		(SELECT count(*) FROM task_claims WHERE task_id=$1 AND released_at IS NULL),
		(SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='status_changed')
		FROM tasks WHERE id=$1`, id).Scan(&status, &openClaims, &changes); err != nil {
		t.Fatalf("status of %d: %v", id, err)
	}
	return
}

// Criterion 30: a user-profile task_context is a pure read even when the model
// names the claim holder; the full profile is the control.
func TestUserContext_Integration_ReadOnlyThroughTheProfile(t *testing.T) {
	ctx := context.Background()
	pool := ucPool(t, ctx)
	ex := udExecutor(pool)
	p := ucProject(t, ctx, pool, ucSlug, "any")
	var a, b int64
	for _, dst := range []*int64{&a, &b} {
		if err := pool.QueryRow(ctx, `INSERT INTO tasks (project_id, title, assignee_type, status)
			VALUES ($1,'itest ctx claimed','human','ready') RETURNING id`, p).Scan(dst); err != nil {
			t.Fatalf("seed task: %v", err)
		}
	}
	full := mcpserver.New(ex, ucWorker)
	for _, id := range []int64{a, b} {
		ucCall(t, ctx, full, "task_claim", fmt.Sprintf(`{"task_id":%d}`, id))
		if s, open, _ := ucStatus(t, ctx, pool, id); s != "claimed" || open != 1 {
			t.Fatalf("CONTROL: after the claim task %d = (%s, %d open claims), want (claimed, 1)", id, s, open)
		}
	}
	_, _, changesBefore := ucStatus(t, ctx, pool, a)

	user := mcpserver.NewWithProfile(ex, ucWorker, mcpserver.ProfileUser)
	ucCall(t, ctx, user, "task_context", fmt.Sprintf(`{"task_id":%d,"worker_id":%q}`, a, ucWorker))
	if s, open, changes := ucStatus(t, ctx, pool, a); s != "claimed" || open != 1 || changes != changesBefore {
		t.Errorf("after a user-profile task_context naming the holder: (%s, %d open claims, %d status events), want "+
			"(claimed, 1, %d) — criterion 30: the read-only pin makes the flip unreachable from this profile",
			s, open, changes, changesBefore)
	}
	var ro, wid string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(args->>'require_read_only','<absent>'), COALESCE(args->>'worker_id','<absent>')
		FROM audit_events WHERE tool='task_context' AND actor=$1 AND args->>'task_id' = $2 ORDER BY id DESC LIMIT 1`,
		ucActor, fmt.Sprint(a)).Scan(&ro, &wid); err != nil {
		t.Fatalf("audit row for the user-profile task_context: %v", err)
	}
	if ro != "true" || wid != "" {
		t.Errorf("audit_events.args = require_read_only %q, worker_id %q; want \"true\" and \"\" (criterion 30: the "+
			"user-scope marker)", ro, wid)
	}

	// Control: the full profile (no pin) flips the holder's task, as today.
	ucCall(t, ctx, full, "task_context", fmt.Sprintf(`{"task_id":%d}`, b))
	if s, _, _ := ucStatus(t, ctx, pool, b); s != "in_progress" {
		t.Errorf("CONTROL: the full profile's holder fetch left task %d %s, want in_progress — without this the "+
			"test above would pass for the wrong reason", b, s)
	}
}

// Criterion 31: the document a session gets — body, log lines, the signal event,
// feedback, decisions and the current marker — built through the real tools.
func TestUserContext_Integration_TheDocument(t *testing.T) {
	ctx := context.Background()
	pool := ucPool(t, ctx)
	ex := udExecutor(pool)
	p := ucProject(t, ctx, pool, ucSlug, "any")
	user := mcpserver.NewWithProfile(ex, ucWorker, mcpserver.ProfileUser)

	const body = "Fix the flaky ingress check.\nFrom a Claude Code session in /home/salvo/projects/personal/kube"
	var created struct {
		TaskID int64 `json:"task_id"`
	}
	out := ucCall(t, ctx, user, "create_task", fmt.Sprintf(`{"project":%q,"title":"itest ctx document","body":%q}`, ucSlug, body))
	if err := json.Unmarshal(out, &created); err != nil || created.TaskID == 0 {
		t.Fatalf("create_task output %s: %v", out, err)
	}
	id := created.TaskID
	for _, m := range []string{"ctx log line one", "ctx log line two"} {
		ucCall(t, ctx, user, "task_append_log", fmt.Sprintf(`{"task_id":%d,"message":%q}`, id, m))
	}
	ucCall(t, ctx, user, "task_signal", fmt.Sprintf(`{"task_id":%d,"state":"needs_input","session":"kube-c7"}`, id))
	// The reads ARE the columns under test, so these two are fixtures.
	if _, err := pool.Exec(ctx, `INSERT INTO feedback_requests (task_id, question, answer, status)
		VALUES ($1,'itest: which approach?','approach B','answered')`, id); err != nil {
		t.Fatalf("seed feedback: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO decisions (project_id, title, body, created_by)
		VALUES ($1,'itest ctx decision','use the canary','itest')`, p); err != nil {
		t.Fatalf("seed decision: %v", err)
	}

	d, raw := ucContext(t, ctx, user, id)
	if d.Task["body"] != body {
		t.Errorf("task.body = %v, want the body verbatim", d.Task["body"])
	}
	logs, signals := 0, 0
	for _, e := range d.Events {
		switch e["event_type"] {
		case "log":
			if strings.Contains(e["payload"], "ctx log line one") || strings.Contains(e["payload"], "ctx log line two") {
				logs++
			}
		case "working_state_changed":
			signals++
		}
	}
	if logs != 2 || signals != 1 {
		t.Errorf("events: %d log lines carrying the messages and %d working_state_changed, want 2 and 1 (S13)", logs, signals)
	}
	if len(d.Feedback) != 1 || d.Feedback[0]["question"] != "itest: which approach?" {
		t.Errorf("feedback = %v, want the one feedback_requests row", d.Feedback)
	}
	if len(d.Decisions) != 1 || d.Decisions[0]["title"] != "itest ctx decision" {
		t.Errorf("decisions = %v, want the project's decision", d.Decisions)
	}
	if d.Task["working_state"] != "needs_input" || d.Task["working_session"] != "kube-c7" {
		t.Errorf("task marker = (%v, %v), want (needs_input, kube-c7) (S13)", d.Task["working_state"], d.Task["working_session"])
	}
	if s, _ := d.Task["working_state_at"].(string); s == "" {
		t.Errorf("task.working_state_at is empty or missing; want the signal time (S13). Document: %s", raw)
	}

	// An old binary's clear: state and time NULL, the name left dangling → "".
	if _, err := pool.Exec(ctx, `UPDATE tasks SET working_state = NULL, working_state_at = NULL WHERE id=$1`, id); err != nil {
		t.Fatalf("old-binary clear: %v", err)
	}
	d, _ = ucContext(t, ctx, user, id)
	if d.Task["working_session"] != "" || d.Task["working_state"] != "" || d.Task["working_state_at"] != "" {
		t.Errorf("after an old binary's clear the marker = (%v, %v, %v), want all \"\" (S4 read gating)",
			d.Task["working_state"], d.Task["working_state_at"], d.Task["working_session"])
	}
}

// Criterion 36, Q1 = (a): no mail-text filter. A task in a local_only project,
// built the way capture builds it (ruleTaskBody's layout with a 400-rune
// preview, and appendRuleLog's log line), is returned in full to a session in
// another repo.
func TestUserContext_Integration_LocalOnlyProjectIsReadInFull(t *testing.T) {
	ctx := context.Background()
	pool := ucPool(t, ctx)
	ex := udExecutor(pool)
	ucProject(t, ctx, pool, ucPrivateSlug, "local_only")

	preview := []rune("Your statement is ready — " + strings.Repeat("private personal mail text ", 30))[:400]
	pv := string(preview)
	body := "Captured deterministically by capture rule 7 (sender \"bank@example.com\").\n\n" +
		"external: gmail itest-ctx-1\nchannel: gmail\nthread_key: gmail:itest-ctx:1\nsender: bank@example.com\n" +
		"sent_at: 2026-09-15T12:00:00Z\nmessage_id: 1\n\n" + pv
	logMsg := "capture: gmail itest-ctx-1 — gmail message 1 from bank@example.com: " + pv

	args, _ := json.Marshal(map[string]any{"project": ucPrivateSlug, "title": "Statement ready", "body": body})
	res, err := ex.Execute(ctx, executor.Call{Tool: "create_task", Actor: ucCapture, Args: args})
	if err != nil {
		t.Fatalf("create_task as capture: %v", err)
	}
	var created struct {
		TaskID int64 `json:"task_id"`
	}
	if err := json.Unmarshal(res.Output, &created); err != nil || created.TaskID == 0 {
		t.Fatalf("create_task output %s: %v", res.Output, err)
	}
	id := created.TaskID
	args, _ = json.Marshal(map[string]any{"task_id": id, "kind": "log", "message": logMsg})
	if _, err := ex.Execute(ctx, executor.Call{Tool: "task_append_log", Actor: ucCapture, Args: args, TaskID: &id}); err != nil {
		t.Fatalf("task_append_log as capture: %v", err)
	}

	user := mcpserver.NewWithProfile(ex, ucWorker, mcpserver.ProfileUser)
	d, raw := ucContext(t, ctx, user, id)
	if d.Task["body"] != body {
		t.Errorf("task.body through the user profile = %q, want capture's body in full (Q1 = a: no filter)", d.Task["body"])
	}
	found := false
	for _, e := range d.Events {
		if e["event_type"] == "log" && strings.Contains(e["payload"], pv) {
			found = true
		}
	}
	if !found {
		t.Errorf("no log event carries the 400-rune capture preview unredacted (Q1 = a). Events: %v", d.Events)
	}
	if strings.Contains(raw, "withheld") {
		t.Errorf("the document says `withheld`: Q1 chose (a), no filter — a gate must be a deliberate later change")
	}
	if w, ok := d.Task["withheld"]; ok {
		t.Errorf("task.withheld = %v is present; variant (b) was not chosen", w)
	}
}
