//go:build integration

package tools_test

// Integration tests for the SWT-10 plan-import executor path + migration 0008
// (SPEC 10-plan-import, criteria 1, 5, 7, 8, verification protocol step 2).
// Build-tagged `integration` AND env-gated on DATABASE_URL. Every plan mutation
// goes through executor.Execute — the ONLY route to a handler (invariant 3) —
// against the compose db. NO LLM anywhere: the proposal's ai_run/ai_extraction
// are seeded DIRECTLY (the propose flow's provider call is unit-tested with a
// fake in internal/planimport); this suite exercises the four executor tools
// (propose/approve/reject/apply) + the orchestrator drain (R4).
//
//   DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//     go test -tags integration -run PlanImport ./internal/tools/
//
// GREENFIELD NOTE: migration 0008, the four plan-import tools, and their
// registration in tools.Register do not exist yet, so under `-tags integration`
// these Execute calls return "unknown tool" and the tests FAIL at the first
// mutation (and the migration test fails: no plan_imports table). Expected.
//
// Imposed contract the implementer follows (test seeds the extraction, apply
// reads it): ai_extractions.fields for a plan carries
//   {"summary": s, "tasks": [ {ref, parent_ref, title, body, assignee_type,
//      subproject, worker_type, priority, depends_on_refs, ..., plan_order}, ...]}
// apply_plan_import reads tasks[] in that shape (plan_order Go-assigned upstream)
// and inserts tasks parents-first, writing child_created / dependency_added /
// plan_imported events; plan_imports.result = {"tasks": {ref: task_id, ...}}.
//
// Cleanup pact (SWT-6): serialized `-p 1`, FK-ordered, test-owned prefixes
// (project slug 'itest-plan-tools-%', plan account 'itest-plan-tools-a@local',
// actor 'manual:itest-plan-tools'); cleans only its OWN corpus, both before and
// after; rerunnable. The real-db guard refuses 192.168.50.49.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/fleet"
	orch "github.com/sspataro57/switchboard/internal/orchestrator"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	planActor     = "manual:itest-plan-tools" // human prefix (approve/apply are humanOnly)
	planSlug      = "itest-plan-tools-proj"
	planClient    = "itest-plan-tools-client"
	planAcctEmail = "itest-plan-tools-a@local"
)

// planFields is the validated plan tree the apply handler materializes. Two
// roots (root-b depends_on root-a → blocked after drain) and one child of
// root-a. plan_order is the Go-assigned sibling index.
const planFields = `{"summary":"itest plan","tasks":[
  {"ref":"root-a","parent_ref":null,"title":"itest-plan Root A","body":"do a","assignee_type":"claude","subproject":null,"worker_type":null,"priority":2,"depends_on_refs":[],"confidence":0.9,"notes":"","plan_order":1},
  {"ref":"root-b","parent_ref":null,"title":"itest-plan Root B","body":"do b","assignee_type":"human","subproject":null,"worker_type":null,"priority":0,"depends_on_refs":["root-a"],"confidence":0.8,"notes":"after a","plan_order":2},
  {"ref":"child-a1","parent_ref":"root-a","title":"itest-plan Child A1","body":"do a1","assignee_type":"claude","subproject":"sub","worker_type":null,"priority":0,"depends_on_refs":[],"confidence":0.7,"notes":"","plan_order":1}
]}`

// ---- guards / scaffolding -----------------------------------------------------

func guardRealDB(t *testing.T) {
	t.Helper()
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db (cleanup deletes corpus rows); use the compose db on :5433")
	}
}

type planNoopPublisher struct{}

func (planNoopPublisher) PublishCommand(string, fleet.Cmd) error { return nil }

// planExecutor uses the REAL policy Matrix so the humanOnly gate on
// approve/apply and the static fallthrough for propose actually run.
func planExecutor(pool *pgxpool.Pool) *executor.Executor {
	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	return executor.New(reg, checker, audit.NewPGStore(pool))
}

func cleanupPlanImport(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM policy_decisions WHERE audit_event_id IN
			(SELECT id FROM audit_events WHERE actor LIKE $1)`, []any{planActor + "%"}},
		{`DELETE FROM audit_events WHERE actor LIKE $1`, []any{planActor + "%"}},
		{`DELETE FROM approvals WHERE subject_type='plan_import' AND subject_id IN
			(SELECT id FROM plan_imports WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{planSlug}},
		{`DELETE FROM task_dependencies WHERE task_id IN
			(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{planSlug}},
		{`DELETE FROM task_events WHERE task_id IN
			(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{planSlug}},
		{`DELETE FROM tasks WHERE parent_id IS NOT NULL AND project_id IN
			(SELECT id FROM projects WHERE slug=$1)`, []any{planSlug}},
		{`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, []any{planSlug}},
		{`DELETE FROM plan_imports WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, []any{planSlug}},
		{`DELETE FROM ai_extractions WHERE raw_source_item_id IN
			(SELECT id FROM raw_source_items WHERE external_id LIKE 'plan:itest-plan-tools-%')`, nil},
		{`DELETE FROM ai_runs WHERE worker_type='plan_import' AND input->>'itest'='plan-tools'`, nil},
		{`DELETE FROM raw_source_items WHERE external_id LIKE 'plan:itest-plan-tools-%'`, nil},
		{`DELETE FROM projects WHERE slug=$1`, []any{planSlug}},
		{`DELETE FROM source_accounts WHERE account_email=$1`, []any{planAcctEmail}},
	}
	for _, st := range stmts {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

// planProposalIDs are the ids the propose executor tool consumes.
type planProposalIDs struct {
	projectID   int64
	rawID       int64
	aiRunID     int64
	extractID   int64
	contentHash string
}

// seedProposal writes the raw item + ai_run + ai_extraction directly (the
// pre-executor half the propose flow produces). contentHash is unique per call
// so the pending-uniqueness index can be exercised across proposals.
func seedProposal(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID int64, contentHash string) planProposalIDs {
	t.Helper()
	var acctID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email) VALUES ('plan', $1)
		 ON CONFLICT DO NOTHING RETURNING id`, planAcctEmail).Scan(&acctID); err != nil {
		// already exists — look it up
		if err := pool.QueryRow(ctx,
			`SELECT id FROM source_accounts WHERE account_email=$1`, planAcctEmail).Scan(&acctID); err != nil {
			t.Fatalf("resolve plan account: %v", err)
		}
	}

	ids := planProposalIDs{projectID: projectID, contentHash: contentHash}
	extID := "plan:" + planSlug + ":" + contentHash
	rawJSON := `{"path":"/home/salvo/plans/itest.md","content":"# itest plan\n- a\n- b (after a)\n"}`
	// Upsert: re-proposing the same content reuses the raw item (raw-first is
	// idempotent on (account, external_id) — criterion 2).
	if err := pool.QueryRow(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1, $2, $3::jsonb, $4, now())
		 ON CONFLICT (source_account_id, external_id) DO UPDATE SET content_hash = EXCLUDED.content_hash
		 RETURNING id`,
		acctID, extID, rawJSON, contentHash).Scan(&ids.rawID); err != nil {
		t.Fatalf("seed raw item: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO ai_runs (worker_type, provider, model, input, output, status)
		 VALUES ('plan_import','openai','gpt-5-mini', $1::jsonb, '{}'::jsonb, 'ok') RETURNING id`,
		`{"itest":"plan-tools","prompt_version":"plan-import-v1","raw_source_item_id":`+itoa(ids.rawID)+`}`).Scan(&ids.aiRunID); err != nil {
		t.Fatalf("seed ai_run: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields)
		 VALUES ($1, $2, $3::jsonb) RETURNING id`,
		ids.aiRunID, ids.rawID, planFields).Scan(&ids.extractID); err != nil {
		t.Fatalf("seed ai_extraction: %v", err)
	}
	return ids
}

func proposeArgs(ids planProposalIDs) string {
	return `{"project":"` + planSlug + `","source_path":"/home/salvo/plans/itest.md","content_hash":"` +
		ids.contentHash + `","raw_source_item_id":` + itoa(ids.rawID) +
		`,"ai_run_id":` + itoa(ids.aiRunID) + `,"ai_extraction_id":` + itoa(ids.extractID) + `}`
}

func planImportStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, `SELECT status FROM plan_imports WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatalf("read plan_import %d status: %v", id, err)
	}
	return s
}

func planTaskCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, projectID).Scan(&n); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	return n
}

// ---- migration 0008 -----------------------------------------------------------

func TestPlanImport_Integration_Migration0008(t *testing.T) {
	guardRealDB(t)
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()

	// Table present.
	var tbl int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_name='plan_imports'`).Scan(&tbl); err != nil {
		t.Fatalf("query tables: %v", err)
	}
	if tbl != 1 {
		t.Fatalf("plan_imports table missing — apply migration 0008_plan_import.sql (make migrate)")
	}

	// Partial unique index on (project_id, content_hash) WHERE status <> 'rejected'.
	var indexdef string
	if err := pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname='plan_imports_pending_uniq'`).Scan(&indexdef); err != nil {
		t.Fatalf("plan_imports_pending_uniq index missing (migration 0008): %v", err)
	}
	low := strings.ToLower(indexdef)
	if !strings.Contains(low, "unique") {
		t.Errorf("plan_imports_pending_uniq is not UNIQUE: %s", indexdef)
	}
	for _, want := range []string{"project_id", "content_hash", "rejected"} {
		if !strings.Contains(low, want) {
			t.Errorf("plan_imports_pending_uniq def missing %q: %s", want, indexdef)
		}
	}

	// CHECK constraint on status.
	var checkDef string
	if err := pool.QueryRow(ctx,
		`SELECT pg_get_constraintdef(c.oid) FROM pg_constraint c
		 WHERE c.conrelid='plan_imports'::regclass AND c.contype='c'
		   AND pg_get_constraintdef(c.oid) ILIKE '%status%' LIMIT 1`).Scan(&checkDef); err != nil {
		t.Fatalf("plan_imports status CHECK constraint missing (migration 0008): %v", err)
	}
	for _, v := range []string{"proposed", "approved", "rejected", "applied"} {
		if !strings.Contains(checkDef, v) {
			t.Errorf("status CHECK missing value %q: %s", v, checkDef)
		}
	}
}

// ---- the full walk ------------------------------------------------------------

func TestPlanImport_Integration_ProposeApproveApply(t *testing.T) {
	guardRealDB(t)
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()

	cleanupPlanImport(t, ctx, pool)
	defer cleanupPlanImport(t, ctx, pool)

	projectID := seedProject(t, ctx, pool, planSlug, planClient)
	ids := seedProposal(t, ctx, pool, projectID, "itest-plan-tools-hash-walk")
	ex := planExecutor(pool)

	// 1. propose: plan_imports row 'proposed'; ZERO tasks (shadow-lane).
	out := callOK(t, ctx, ex, planActor, "propose_plan_import", proposeArgs(ids))
	var pr struct {
		PlanImportID int64 `json:"plan_import_id"`
	}
	mustUnmarshal(t, out, &pr)
	if pr.PlanImportID == 0 {
		t.Fatal("propose_plan_import returned plan_import_id 0")
	}
	if s := planImportStatus(t, ctx, pool, pr.PlanImportID); s != "proposed" {
		t.Fatalf("after propose status = %q, want proposed", s)
	}
	if n := planTaskCount(t, ctx, pool, projectID); n != 0 {
		t.Fatalf("propose created %d tasks, want 0 (nothing disposed before approval)", n)
	}

	// 2. duplicate propose (same content_hash) refused by the partial unique index.
	ids2 := seedProposal(t, ctx, pool, projectID, "itest-plan-tools-hash-walk") // same hash
	if _, err := ex.Execute(ctx, executor.Call{Tool: "propose_plan_import", Actor: planActor, Args: []byte(proposeArgs(ids2))}); err == nil {
		t.Errorf("duplicate propose of the same content must be refused (one live proposal per content)")
	}

	// 3. approve: proposed -> approved + approvals row (subject_type=plan_import).
	callOK(t, ctx, ex, planActor, "approve_plan_import", `{"plan_import_id":`+itoa(pr.PlanImportID)+`}`)
	if s := planImportStatus(t, ctx, pool, pr.PlanImportID); s != "approved" {
		t.Fatalf("after approve status = %q, want approved", s)
	}
	var apprCount int
	var decidedBy string
	if err := pool.QueryRow(ctx,
		`SELECT count(*), coalesce(max(decided_by),'') FROM approvals
		 WHERE subject_type='plan_import' AND subject_id=$1`, pr.PlanImportID).Scan(&apprCount, &decidedBy); err != nil {
		t.Fatalf("read approvals: %v", err)
	}
	if apprCount != 1 {
		t.Errorf("approvals rows = %d, want 1 (subject_type=plan_import)", apprCount)
	}
	if decidedBy != planActor {
		t.Errorf("approvals.decided_by = %q, want %q", decidedBy, planActor)
	}

	// 4. apply: single-tx tree insert. Capture the max event id first so the
	//    orchestrator drain processes exactly apply's events.
	cursorBase := maxEventIDTools(t, ctx, pool)
	callOK(t, ctx, ex, planActor, "apply_plan_import", `{"plan_import_id":`+itoa(pr.PlanImportID)+`}`)
	if s := planImportStatus(t, ctx, pool, pr.PlanImportID); s != "applied" {
		t.Fatalf("after apply status = %q, want applied", s)
	}

	// result = {"tasks":{ref:task_id}}
	refToID := planResultMap(t, ctx, pool, pr.PlanImportID)
	for _, ref := range []string{"root-a", "root-b", "child-a1"} {
		if refToID[ref] == 0 {
			t.Fatalf("result.tasks missing ref %q: %v", ref, refToID)
		}
	}
	if n := planTaskCount(t, ctx, pool, projectID); n != 3 {
		t.Fatalf("apply created %d tasks, want 3", n)
	}

	// Parent + plan_order + status per node (all ready before the drain).
	rootA, rootB, childA1 := refToID["root-a"], refToID["root-b"], refToID["child-a1"]
	assertTask(t, ctx, pool, rootA, taskWant{status: "ready", parent: 0, planOrder: 1})
	assertTask(t, ctx, pool, rootB, taskWant{status: "ready", parent: 0, planOrder: 2})
	assertTask(t, ctx, pool, childA1, taskWant{status: "ready", parent: rootA, planOrder: 1})

	// Events: child_created on root-a; dependency_added on root-b; plan_imported
	// on each root.
	if got := eventCount(t, ctx, pool, rootA, "child_created"); got != 1 {
		t.Errorf("child_created on root-a = %d, want 1", got)
	}
	if got := eventCount(t, ctx, pool, rootB, "dependency_added"); got != 1 {
		t.Errorf("dependency_added on root-b = %d, want 1", got)
	}
	if got := eventCount(t, ctx, pool, rootA, "plan_imported") + eventCount(t, ctx, pool, rootB, "plan_imported"); got != 2 {
		t.Errorf("plan_imported on roots = %d, want 2 (one per root)", got)
	}

	// task_dependencies row (root-b depends_on root-a).
	var deps int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_dependencies WHERE task_id=$1 AND depends_on_task_id=$2`, rootB, rootA).Scan(&deps); err != nil {
		t.Fatalf("read deps: %v", err)
	}
	if deps != 1 {
		t.Errorf("task_dependencies(root-b -> root-a) = %d, want 1", deps)
	}

	// 5. orchestrator drain: R4 blocks root-b (unmet dep on ready root-a); the
	//    independent root-a and child-a1 stay ready.
	setCursorTools(t, ctx, pool, cursorBase)
	engine := orch.NewEngine(pool, ex, planNoopPublisher{}, orch.Config{})
	if _, err := engine.DrainOnce(ctx); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if s := taskStatus(t, ctx, pool, rootB); s != "blocked" {
		t.Errorf("root-b after drain = %q, want blocked (R4 on the emitted dependency_added)", s)
	}
	if s := taskStatus(t, ctx, pool, rootA); s != "ready" {
		t.Errorf("root-a after drain = %q, want ready (independent root)", s)
	}
	if s := taskStatus(t, ctx, pool, childA1); s != "ready" {
		t.Errorf("child-a1 after drain = %q, want ready (no deps)", s)
	}

	// 6. re-apply is an idempotent no-op returning the stored result: zero new
	//    tasks / task_events.
	tasksBefore := planTaskCount(t, ctx, pool, projectID)
	eventsBefore := projectEventCount(t, ctx, pool, projectID)
	callOK(t, ctx, ex, planActor, "apply_plan_import", `{"plan_import_id":`+itoa(pr.PlanImportID)+`}`)
	if got := planTaskCount(t, ctx, pool, projectID); got != tasksBefore {
		t.Errorf("re-apply created tasks (%d -> %d), want no-op", tasksBefore, got)
	}
	if got := projectEventCount(t, ctx, pool, projectID); got != eventsBefore {
		t.Errorf("re-apply wrote task_events (%d -> %d), want no-op", eventsBefore, got)
	}
	if refToID2 := planResultMap(t, ctx, pool, pr.PlanImportID); refToID2["root-a"] != rootA {
		t.Errorf("re-apply changed the stored result map (root-a %d -> %d)", rootA, refToID2["root-a"])
	}
}

// ---- gate errors (criteria 6, 7, 8) -------------------------------------------

func TestPlanImport_Integration_GateErrors(t *testing.T) {
	guardRealDB(t)
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()

	cleanupPlanImport(t, ctx, pool)
	defer cleanupPlanImport(t, ctx, pool)

	projectID := seedProject(t, ctx, pool, planSlug, planClient)
	ex := planExecutor(pool)

	// -- apply-before-approve: a proposed plan cannot be applied; deny/error is
	//    visible in audit_events (criterion 8).
	idsA := seedProposal(t, ctx, pool, projectID, "itest-plan-tools-hash-gate-a")
	outA := callOK(t, ctx, ex, planActor, "propose_plan_import", proposeArgs(idsA))
	var prA struct {
		PlanImportID int64 `json:"plan_import_id"`
	}
	mustUnmarshal(t, outA, &prA)
	if _, err := ex.Execute(ctx, executor.Call{Tool: "apply_plan_import", Actor: planActor,
		Args: []byte(`{"plan_import_id":` + itoa(prA.PlanImportID) + `}`)}); err == nil {
		t.Errorf("apply on a 'proposed' plan must error (approval required)")
	}
	if planTaskCount(t, ctx, pool, projectID) != 0 {
		t.Errorf("apply-before-approve created tasks, want 0")
	}
	var applyErrAudits int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE actor=$1 AND tool='apply_plan_import' AND status IN ('error','denied')`,
		planActor).Scan(&applyErrAudits); err != nil {
		t.Fatalf("count apply audit: %v", err)
	}
	if applyErrAudits < 1 {
		t.Errorf("no error/denied audit_events row for apply-before-approve (criterion 8)")
	}

	// -- reject idempotency: reject flips proposed->rejected (+ approvals row);
	//    a second reject is an idempotent no-op success.
	callOK(t, ctx, ex, planActor, "reject_plan_import", `{"plan_import_id":`+itoa(prA.PlanImportID)+`,"reason":"wrong parse"}`)
	if s := planImportStatus(t, ctx, pool, prA.PlanImportID); s != "rejected" {
		t.Fatalf("after reject status = %q, want rejected", s)
	}
	callOK(t, ctx, ex, planActor, "reject_plan_import", `{"plan_import_id":`+itoa(prA.PlanImportID)+`}`) // idempotent
	if s := planImportStatus(t, ctx, pool, prA.PlanImportID); s != "rejected" {
		t.Errorf("idempotent reject changed status to %q", s)
	}

	// -- approve-after-reject is an error (rejected is terminal for approval).
	if _, err := ex.Execute(ctx, executor.Call{Tool: "approve_plan_import", Actor: planActor,
		Args: []byte(`{"plan_import_id":` + itoa(prA.PlanImportID) + `}`)}); err == nil {
		t.Errorf("approve on a 'rejected' plan must error")
	}

	// -- re-propose is allowed only AFTER rejection (the pending-uniq index
	//    excludes rejected). Same content_hash now proposes cleanly.
	idsB := seedProposal(t, ctx, pool, projectID, "itest-plan-tools-hash-gate-a") // same hash as the rejected one
	if _, err := ex.Execute(ctx, executor.Call{Tool: "propose_plan_import", Actor: planActor, Args: []byte(proposeArgs(idsB))}); err != nil {
		t.Errorf("re-propose after rejection must succeed (partial unique excludes rejected): %v", err)
	}
}

// ---- small helpers (plan-import-owned; distinct names from the shared set) -----

type taskWant struct {
	status    string
	parent    int64 // 0 = expect NULL
	planOrder int
}

func assertTask(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64, want taskWant) {
	t.Helper()
	var status string
	var parent *int64
	var planOrder *int
	if err := pool.QueryRow(ctx,
		`SELECT status, parent_id, plan_order FROM tasks WHERE id=$1`, id).Scan(&status, &parent, &planOrder); err != nil {
		t.Fatalf("read task %d: %v", id, err)
	}
	if status != want.status {
		t.Errorf("task %d status = %q, want %q", id, status, want.status)
	}
	if want.parent == 0 {
		if parent != nil {
			t.Errorf("task %d parent_id = %v, want NULL (root)", id, *parent)
		}
	} else if parent == nil || *parent != want.parent {
		t.Errorf("task %d parent_id = %v, want %d", id, parent, want.parent)
	}
	if planOrder == nil || *planOrder != want.planOrder {
		t.Errorf("task %d plan_order = %v, want %d", id, planOrder, want.planOrder)
	}
}

func planResultMap(t *testing.T, ctx context.Context, pool *pgxpool.Pool, planImportID int64) map[string]int64 {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT result FROM plan_imports WHERE id=$1`, planImportID).Scan(&raw); err != nil {
		t.Fatalf("read plan_import result: %v", err)
	}
	var doc struct {
		Tasks map[string]int64 `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse plan_imports.result %s: %v", raw, err)
	}
	return doc.Tasks
}

func projectEventCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_events WHERE task_id IN (SELECT id FROM tasks WHERE project_id=$1)`, projectID).Scan(&n); err != nil {
		t.Fatalf("count project events: %v", err)
	}
	return n
}

func maxEventIDTools(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(max(id),0) FROM task_events`).Scan(&n); err != nil {
		t.Fatalf("max event id: %v", err)
	}
	return n
}

func setCursorTools(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int64) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`UPDATE orchestrator_cursor SET last_event_id=$1, updated_at=now() WHERE name='orchestrator'`, n); err != nil {
		t.Fatalf("set cursor: %v", err)
	}
}
