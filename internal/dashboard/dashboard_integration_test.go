//go:build integration

package dashboard_test

// Integration tests for the SWT-10 dashboard full board / plans review / briefs
// / exports (SPEC 10-plan-import, criteria 6, 10, 11, 12). Build-tagged
// `integration` AND env-gated on DATABASE_URL. The REAL dashboard.Server runs
// under httptest with dev-login session auth (OIDC_ISSUER unset); reads are
// direct SQL, the approve action goes through the executor (invariant 3). NO
// LLM, NO live OIDC. Cleanup pact: FK-ordered, test-owned prefix
// 'itest-plan-dash-%'; rerunnable; real-db guard refuses 192.168.50.49.
//
// GREENFIELD NOTE: migration 0008, the /plans /tasks /briefs /export routes +
// their templates, and the export formatter (dashboard.WriteTasksCSV — imposed
// in export_test.go) do not exist yet. Under `-tags integration` this test
// binary compile-FAILs on the missing export symbols, and once it compiles the
// new routes 404 / the plan_imports seed fails — the expected spec-first
// failure. pinnedHeader is defined in export_test.go (same package).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/dashboard"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	dashSlug      = "itest-plan-dash-proj"
	dashOther     = "itest-plan-dash-other"
	dashClient    = "itest-plan-dash-client"
	dashAcctEmail = "itest-plan-dash-a@local"
	dashActor     = "dashboard:salvo"
)

const dashFields = `{"summary":"dash itest plan","tasks":[
  {"ref":"root-a","parent_ref":null,"title":"DASH Root A","body":"do a","assignee_type":"claude","subproject":null,"worker_type":null,"priority":2,"depends_on_refs":[],"confidence":0.91,"notes":"","plan_order":1},
  {"ref":"child-a1","parent_ref":"root-a","title":"DASH Child A1","body":"do a1","assignee_type":"claude","subproject":"sub","worker_type":null,"priority":0,"depends_on_refs":[],"confidence":0.6,"notes":"low conf","plan_order":1}
]}`

func dashGuard(t *testing.T) {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping dashboard integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use the compose db on :5433")
	}
}

func dashPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	return pool
}

// dashSeed holds the ids the assertions reference.
type dashSeed struct {
	projectID    int64
	rootTaskID   int64
	childTaskID  int64
	closedTaskID int64
	briefTaskID  int64
	planImportID int64
}

func cleanupDash(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE actor LIKE 'dashboard:%' AND tool='approve_plan_import' AND args::text LIKE '%itest-plan-dash%')`, nil},
		{`DELETE FROM approvals WHERE subject_type='plan_import' AND subject_id IN
			(SELECT id FROM plan_imports WHERE project_id IN (SELECT id FROM projects WHERE slug IN ($1,$2)))`, []any{dashSlug, dashOther}},
		{`DELETE FROM task_dependencies WHERE task_id IN
			(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug IN ($1,$2)))`, []any{dashSlug, dashOther}},
		{`DELETE FROM task_events WHERE task_id IN
			(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug IN ($1,$2)))`, []any{dashSlug, dashOther}},
		{`DELETE FROM tasks WHERE parent_id IS NOT NULL AND project_id IN (SELECT id FROM projects WHERE slug IN ($1,$2))`, []any{dashSlug, dashOther}},
		{`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug IN ($1,$2))`, []any{dashSlug, dashOther}},
		{`DELETE FROM plan_imports WHERE project_id IN (SELECT id FROM projects WHERE slug IN ($1,$2))`, []any{dashSlug, dashOther}},
		{`DELETE FROM ai_extractions WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE external_id LIKE 'plan:itest-plan-dash%')`, nil},
		{`DELETE FROM ai_runs WHERE worker_type='plan_import' AND input->>'itest'='plan-dash'`, nil},
		{`DELETE FROM raw_source_items WHERE external_id LIKE 'plan:itest-plan-dash%'`, nil},
		{`DELETE FROM projects WHERE slug IN ($1,$2)`, []any{dashSlug, dashOther}},
		{`DELETE FROM source_accounts WHERE account_email=$1`, []any{dashAcctEmail}},
	}
	for _, st := range stmts {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

func seedDash(t *testing.T, ctx context.Context, pool *pgxpool.Pool) dashSeed {
	t.Helper()
	var s dashSeed

	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path)
		 VALUES ($1,$1,$2,'manual','dashboard','/tmp/itest') RETURNING id`, dashSlug, dashClient).Scan(&s.projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	var otherID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path)
		 VALUES ($1,$1,$2,'manual','dashboard','/tmp/itest') RETURNING id`, dashOther, dashClient).Scan(&otherID); err != nil {
		t.Fatalf("seed other project: %v", err)
	}

	// Board tasks: a ready claude root, a human child (parent=root), a closed
	// task (hidden by default), and a morning-brief task.
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status, priority, plan_order)
		 VALUES ($1,'DASH ready root','claude','ready',2,1) RETURNING id`, s.projectID).Scan(&s.rootTaskID); err != nil {
		t.Fatalf("seed root task: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, parent_id, subproject, title, body, assignee_type, status, priority, plan_order)
		 VALUES ($1,$2,'sub','DASH human child','child body','human','blocked',0,1) RETURNING id`,
		s.projectID, s.rootTaskID).Scan(&s.childTaskID); err != nil {
		t.Fatalf("seed child task: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO task_dependencies (task_id, depends_on_task_id) VALUES ($1,$2)`, s.childTaskID, s.rootTaskID); err != nil {
		t.Fatalf("seed dep: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO task_events (task_id, event_type, payload) VALUES ($1,'child_created', $2::jsonb)`,
		s.rootTaskID, `{"child_task_id":`+strconv.FormatInt(s.childTaskID, 10)+`}`); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'DASH closed task','claude','closed') RETURNING id`, s.projectID).Scan(&s.closedTaskID); err != nil {
		t.Fatalf("seed closed task: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, body, assignee_type, status)
		 VALUES ($1,'Morning brief 2026-07-11','- item one\n- item two','human','ready') RETURNING id`, s.projectID).Scan(&s.briefTaskID); err != nil {
		t.Fatalf("seed brief task: %v", err)
	}
	// A task in the OTHER project so the project filter has something to exclude.
	if _, err := pool.Exec(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status) VALUES ($1,'DASH other-project task','claude','ready')`, otherID); err != nil {
		t.Fatalf("seed other task: %v", err)
	}

	// A proposed plan_import for the /plans review surface.
	var acctID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email) VALUES ('plan',$1)
		 ON CONFLICT DO NOTHING RETURNING id`, dashAcctEmail).Scan(&acctID); err != nil {
		if err := pool.QueryRow(ctx, `SELECT id FROM source_accounts WHERE account_email=$1`, dashAcctEmail).Scan(&acctID); err != nil {
			t.Fatalf("resolve plan account: %v", err)
		}
	}
	var rawID, runID, extID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,'plan:itest-plan-dash:hash1','{"path":"/p.md","content":"x"}'::jsonb,'itest-plan-dash-hash1',now()) RETURNING id`,
		acctID).Scan(&rawID); err != nil {
		t.Fatalf("seed raw: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO ai_runs (worker_type, provider, model, input, output, status)
		 VALUES ('plan_import','openai','gpt-5-mini','{"itest":"plan-dash"}'::jsonb,'{}'::jsonb,'ok') RETURNING id`).Scan(&runID); err != nil {
		t.Fatalf("seed ai_run: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields) VALUES ($1,$2,$3::jsonb) RETURNING id`,
		runID, rawID, dashFields).Scan(&extID); err != nil {
		t.Fatalf("seed ai_extraction: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO plan_imports (project_id, source_path, content_hash, raw_source_item_id, ai_run_id, ai_extraction_id, status)
		 VALUES ($1,'/p.md','itest-plan-dash-hash1',$2,$3,$4,'proposed') RETURNING id`,
		s.projectID, rawID, runID, extID).Scan(&s.planImportID); err != nil {
		t.Fatalf("seed plan_import: %v", err)
	}
	return s
}

// newDashServer wires the real Server (dev auth) + a logged-in client.
func newDashServer(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (*httptest.Server, *http.Client) {
	t.Helper()
	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	ex := executor.New(reg, policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...)), audit.NewPGStore(pool))

	auth, err := dashboard.NewAuth(ctx, "", "", "", "") // issuer "" -> dev mode
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	srv, err := dashboard.NewServer(pool, ex, auth)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	// dev login sets the session cookie.
	if _, err := client.Get(ts.URL + "/dev/login?user=salvo"); err != nil {
		t.Fatalf("dev login: %v", err)
	}
	return ts, client
}

func get(t *testing.T, client *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestDashboard_Integration_BoardPlansBriefsExports(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()

	cleanupDash(t, ctx, pool)
	defer cleanupDash(t, ctx, pool)
	sd := seedDash(t, ctx, pool)

	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	// ---- /plans list + detail (criterion 6) --------------------------------
	if code, body := get(t, client, ts.URL+"/plans"); code != 200 {
		t.Errorf("GET /plans = %d, want 200\n%s", code, snippet(body))
	} else if !strings.Contains(body, strconv.FormatInt(sd.planImportID, 10)) {
		t.Errorf("/plans missing the proposed plan_import id %d", sd.planImportID)
	}
	if code, body := get(t, client, ts.URL+"/plans/"+strconv.FormatInt(sd.planImportID, 10)); code != 200 {
		t.Errorf("GET /plans/{id} = %d, want 200\n%s", code, snippet(body))
	} else {
		for _, want := range []string{"DASH Root A", "DASH Child A1"} {
			if !strings.Contains(body, want) {
				t.Errorf("/plans/{id} missing tree node %q", want)
			}
		}
	}

	// ---- POST /plans/{id}/approve flips status (executor) -------------------
	postForm(t, client, ts.URL+"/plans/"+strconv.FormatInt(sd.planImportID, 10)+"/approve")
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM plan_imports WHERE id=$1`, sd.planImportID).Scan(&status); err != nil {
		t.Fatalf("read plan_import status: %v", err)
	}
	if status != "approved" {
		t.Errorf("after POST approve, plan_import status = %q, want approved", status)
	}

	// ---- /tasks board + filters (criterion 10) -----------------------------
	code, body := get(t, client, ts.URL+"/tasks?project="+dashSlug)
	if code != 200 {
		t.Fatalf("GET /tasks = %d, want 200\n%s", code, snippet(body))
	}
	if !strings.Contains(body, "DASH ready root") || !strings.Contains(body, "DASH human child") {
		t.Errorf("/tasks missing seeded tasks")
	}
	if strings.Contains(body, "DASH closed task") {
		t.Errorf("/tasks default view shows a closed task; closed must be hidden by default")
	}
	if strings.Contains(body, "DASH other-project task") {
		t.Errorf("/tasks?project=%s leaked a task from another project", dashSlug)
	}
	// ?status=closed reveals closed.
	if _, b := get(t, client, ts.URL+"/tasks?project="+dashSlug+"&status=closed"); !strings.Contains(b, "DASH closed task") {
		t.Errorf("/tasks?status=closed did not show the closed task")
	}
	// assignee_type filter.
	if _, b := get(t, client, ts.URL+"/tasks?project="+dashSlug+"&assignee_type=human"); !strings.Contains(b, "DASH human child") || strings.Contains(b, "DASH ready root") {
		t.Errorf("/tasks?assignee_type=human did not filter to human tasks")
	}

	// ---- /tasks/{id} detail (parent + children + deps) ---------------------
	if code, b := get(t, client, ts.URL+"/tasks/"+strconv.FormatInt(sd.childTaskID, 10)); code != 200 {
		t.Errorf("GET /tasks/{id} = %d, want 200\n%s", code, snippet(b))
	} else {
		// parent link (root id) and the dependency's presence.
		if !strings.Contains(b, strconv.FormatInt(sd.rootTaskID, 10)) {
			t.Errorf("/tasks/{child} detail missing parent id %d", sd.rootTaskID)
		}
		if !strings.Contains(b, "DASH ready root") {
			t.Errorf("/tasks/{child} detail missing the dependency/parent task title")
		}
	}

	// ---- /briefs (criterion 11) --------------------------------------------
	if code, b := get(t, client, ts.URL+"/briefs"); code != 200 {
		t.Errorf("GET /briefs = %d, want 200\n%s", code, snippet(b))
	} else {
		if !strings.Contains(b, "Morning brief 2026-07-11") {
			t.Errorf("/briefs missing the morning-brief task")
		}
		if strings.Contains(b, "DASH ready root") {
			t.Errorf("/briefs listed a non-brief task")
		}
	}

	// ---- exports (criterion 12) --------------------------------------------
	code, csv := get(t, client, ts.URL+"/export/tasks.csv?project="+dashSlug)
	if code != 200 {
		t.Fatalf("GET /export/tasks.csv = %d, want 200\n%s", code, snippet(csv))
	}
	firstLine, _, _ := strings.Cut(csv, "\n")
	firstLine = strings.TrimSuffix(firstLine, "\r")
	if firstLine != pinnedHeader {
		t.Errorf("CSV header = %q, want the pinned header", firstLine)
	}
	if !strings.Contains(csv, "DASH ready root") {
		t.Errorf("CSV export missing a seeded task")
	}
	if strings.Contains(csv, "DASH other-project task") {
		t.Errorf("CSV export ignored the project filter")
	}
	// id ASC ordering: root task id < child task id, so root's line precedes child's.
	if ri, ci := strings.Index(csv, "DASH ready root"), strings.Index(csv, "DASH human child"); ri < 0 || ci < 0 || ri > ci {
		t.Errorf("CSV rows not ordered id ASC (root at %d, child at %d)", ri, ci)
	}

	code, js := get(t, client, ts.URL+"/export/tasks.json?project="+dashSlug)
	if code != 200 {
		t.Fatalf("GET /export/tasks.json = %d, want 200\n%s", code, snippet(js))
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(js), &rows); err != nil {
		t.Fatalf("/export/tasks.json is not an array of objects: %v\n%s", err, snippet(js))
	}
	if len(rows) == 0 {
		t.Fatalf("/export/tasks.json returned no rows")
	}
	for _, k := range []string{"id", "project", "parent_id", "title", "status", "assignee_type", "priority", "plan_order"} {
		if _, ok := rows[0][k]; !ok {
			t.Errorf("/export/tasks.json row missing field %q", k)
		}
	}
}

func postForm(t *testing.T, client *http.Client, url string) {
	t.Helper()
	resp, err := client.PostForm(url, nil)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	resp.Body.Close()
}

func snippet(s string) string {
	if len(s) > 600 {
		return s[:600]
	}
	return s
}
