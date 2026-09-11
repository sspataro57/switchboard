//go:build integration

package dashboard_test

// SWT-36 (docs/tickets/dismiss-reopen-on-activity_SPEC.md) criterion 17
// against a real database: the board row's "reopened after dismissal
// (<reason_code>)" marker (D12), and the exports it must not touch.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run BoardReopen ./internal/dashboard/
//
// The REAL dashboard.Server under httptest with dev-login auth (newDashServer),
// the REAL executor and policy matrix. Dismissals are POSTed through the board's
// own form (bdDismiss, board_dismiss_integration_test.go) — criterion 26: never
// by INSERT. Activity reopens are the guarded task_reopen a pass would make.
//
// D12's predicate: the marker shows when the task is NOT closed and its NEWEST
// dismissal row has reopened_by_message_id IS NOT NULL. MUTATIONS that turn
// this red:
//   - key the marker on reopened_at instead of reopened_by_message_id -> task B
//     (a human's plain reopen) shows it;
//   - key it on ANY row rather than the newest -> task C (re-dismissed, then
//     plainly reopened) shows it;
//   - add the dismissal read to boardQuery -> the export assertions go red.
//
// IMPOSED SURFACE: the rendered text `reopened after dismissal (<reason_code>)`
// on the board row; nothing else by name.
//
// GREENFIELD NOTE — EXPECTED RED: 0026 is not applied, the guarded reopen does
// not exist, and the board renders no marker.
//
// CLEANUP PACT: owns project itest-boardreopen-proj, provider
// itest-boardreopen-src, threads itest-boardreopen:%, audit rows by task and by
// this file's two actors. FK-ordered, start AND end.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	brrSlug     = "itest-boardreopen-proj"
	brrProvider = "itest-boardreopen-src"
	brrAccount  = "itest-boardreopen@local"
	brrSpine    = "capture:itest-boardreopen"
	brrHuman    = "opsctl:itest-boardreopen"
	brrMarker   = "reopened after dismissal"
)

func brrCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const projs = `(SELECT id FROM projects WHERE slug='` + brrSlug + `')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	const accts = `(SELECT id FROM source_accounts WHERE provider='` + brrProvider + `')`
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`
	const actors = `('` + brrSpine + `','` + brrHuman + `')`
	for _, q := range []string{
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor IN ` + actors + `)`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor IN ` + actors,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM projects WHERE slug='` + brrSlug + `'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'itest-boardreopen:%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider='` + brrProvider + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

type brrSuite struct {
	pool    *pgxpool.Pool
	ex      *executor.Executor
	project int64
	account int64
}

func newBRRSuite(t *testing.T, ctx context.Context) *brrSuite {
	t.Helper()
	dashGuard(t)
	pool := dashPool(t, ctx)
	t.Cleanup(pool.Close)
	brrCleanup(t, ctx, pool)
	t.Cleanup(func() { brrCleanup(t, ctx, pool) })

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name='task_dismissals' AND column_name IN ('reopened_at','reopened_by_message_id')`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("task_dismissals lacks migration 0026's columns (n=%d, err=%v)", n, err)
	}

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	s := &brrSuite{pool: pool,
		ex: executor.New(reg, policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...)),
			audit.NewPGStore(pool))}
	s.project = s.insID(t, ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,'itest-boardreopen-client','manual','dashboard','/tmp/itest-boardreopen','any') RETURNING id`, brrSlug)
	s.account = s.insID(t, ctx,
		`INSERT INTO source_accounts (provider, account_email, send_enabled) VALUES ($1,$2,false) RETURNING id`,
		brrProvider, brrAccount)
	return s
}

func (s *brrSuite) insID(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", q, err)
	}
	return id
}

type brrTask struct {
	id, thread int64
	title      string
}

// task seeds a ready task on its own thread with BOTH an inbound and an
// outbound message on it (criterion 26).
func (s *brrSuite) task(t *testing.T, ctx context.Context, label string) brrTask {
	t.Helper()
	bt := brrTask{title: "BRR " + label}
	bt.thread = s.insID(t, ctx,
		`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,'','[]') RETURNING id`,
		"itest-boardreopen:"+label)
	bt.id = s.insID(t, ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status, source_thread_id)
		 VALUES ($1,$2,'human','ready',$3) RETURNING id`, s.project, bt.title, bt.thread)
	var now time.Time
	if err := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatalf("db clock: %v", err)
	}
	s.message(t, ctx, label+"-in0", bt.thread, "inbound", now.Add(-time.Hour))
	s.message(t, ctx, label+"-out0", bt.thread, "outbound", now.Add(-50*time.Minute))
	return bt
}

func (s *brrSuite) message(t *testing.T, ctx context.Context, label string, thread int64, direction string, at time.Time) int64 {
	t.Helper()
	raw := s.insID(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,'{}',$3, now()) RETURNING id`,
		s.account, "itest-boardreopen-"+label, "itest-boardreopen-h-"+label)
	return s.insID(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		    body_text, subject, sender, channel, created_at)
		 VALUES ($1,$2,$3,$4,$5,'itest-boardreopen body','','Mario Cruz','gmail',$5) RETURNING id`,
		raw, thread, direction, "<itest-boardreopen-"+label+"@mail.example>", at)
}

func (s *brrSuite) latestDismissal(t *testing.T, ctx context.Context, taskID int64) (int64, time.Time) {
	t.Helper()
	var id int64
	var at time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT id, created_at FROM task_dismissals WHERE task_id=$1 ORDER BY id DESC LIMIT 1`, taskID).
		Scan(&id, &at); err != nil {
		t.Fatalf("read dismissal of task %d: %v", taskID, err)
	}
	return id, at
}

func (s *brrSuite) execute(t *testing.T, ctx context.Context, actor string, taskID int64, args string) {
	t.Helper()
	if _, err := s.ex.Execute(ctx, executor.Call{Tool: "task_reopen", Actor: actor, Args: []byte(args), TaskID: &taskID}); err != nil {
		t.Fatalf("task_reopen(%s) as %s: %v", args, actor, err)
	}
}

// activityReopen is what the capture or promote pass does after a new inbound
// message lands on the dismissed task's thread.
func (s *brrSuite) activityReopen(t *testing.T, ctx context.Context, bt brrTask, label string) {
	t.Helper()
	d, at := s.latestDismissal(t, ctx, bt.id)
	m := s.message(t, ctx, label, bt.thread, "inbound", at.Add(time.Minute))
	s.execute(t, ctx, brrSpine, bt.id,
		fmt.Sprintf(`{"task_id":%d,"dismissal_id":%d,"message_id":%d,"reason":"itest: new inbound"}`, bt.id, d, m))
}

func (s *brrSuite) plainReopen(t *testing.T, ctx context.Context, bt brrTask) {
	t.Helper()
	s.execute(t, ctx, brrHuman, bt.id, fmt.Sprintf(`{"task_id":%d,"reason":"mis-click"}`, bt.id))
}

// brrRow returns the <tr>...</tr> that renders a given title ("" if absent).
func brrRow(body, title string) string {
	i := strings.Index(body, ">"+title+"<")
	if i < 0 {
		return ""
	}
	start := strings.LastIndex(body[:i], "<tr")
	if start < 0 {
		start = 0
	}
	rest := body[start:]
	if j := strings.Index(rest, "</tr>"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func TestBoardReopen_Integration_MarkerFollowsTheNewestDismissal(t *testing.T) {
	ctx := context.Background()
	s := newBRRSuite(t, ctx)
	ts, client := newDashServer(t, ctx, s.pool)
	defer ts.Close()

	dismiss := func(bt brrTask, code string) {
		t.Helper()
		if f := bdDismiss(t, client, ts.URL, bt.id, code, ""); !strings.Contains(f, "task_dismiss ok") {
			t.Fatalf("dismissing %q flashed %q", bt.title, f)
		}
	}

	a := s.task(t, ctx, "A activity reopened")
	dismiss(a, "handled_elsewhere")
	s.activityReopen(t, ctx, a, "A-in1")

	b := s.task(t, ctx, "B human reopened")
	dismiss(b, "duplicate")
	s.plainReopen(t, ctx, b)

	c := s.task(t, ctx, "C re-dismissed")
	dismiss(c, "wrong_kind")
	s.activityReopen(t, ctx, c, "C-in1")
	dismiss(c, "not_actionable")

	_, board := get(t, client, ts.URL+"/tasks?project="+brrSlug)

	rowA := brrRow(board, a.title)
	if rowA == "" {
		t.Fatalf("the activity-reopened task is not on the default board\n%s", snippet(board))
	}
	if !strings.Contains(rowA, brrMarker+" (handled_elsewhere)") {
		t.Errorf("the activity-reopened row does not show %q. The SPEC's 'usable alone': a dismissed task "+
			"that comes back says so on the board\n%s", brrMarker+" (handled_elsewhere)", rowA)
	}
	rowB := brrRow(board, b.title)
	if rowB == "" {
		t.Fatalf("the plainly reopened task is not on the board")
	}
	if strings.Contains(rowB, brrMarker) {
		t.Errorf("a HUMAN's plain reopen shows the marker. D12 keys on reopened_by_message_id, not reopened_at: "+
			"undoing a mis-click is not 'reopened after dismissal' by activity\n%s", rowB)
	}

	_, closedBoard := get(t, client, ts.URL+"/tasks?project="+brrSlug+"&status=closed")
	rowC := brrRow(closedBoard, c.title)
	if rowC == "" {
		t.Fatalf("the re-dismissed task is not under ?status=closed")
	}
	if strings.Contains(rowC, brrMarker) {
		t.Errorf("the re-dismissed (closed) task shows the marker (criterion 17: absent once re-dismissed)\n%s", rowC)
	}

	// Newest row wins: C's first row is activity-stamped, its second is now
	// human-stamped. No marker.
	s.plainReopen(t, ctx, c)
	_, board2 := get(t, client, ts.URL+"/tasks?project="+brrSlug)
	rowC2 := brrRow(board2, c.title)
	if rowC2 == "" {
		t.Fatalf("task C is not on the board after its plain reopen")
	}
	if strings.Contains(rowC2, brrMarker) {
		t.Errorf("task C shows the marker although its NEWEST dismissal was undone by a human. D12 reads the "+
			"newest row only\n%s", rowC2)
	}
}

// "The CSV/JSON export header and columns are unchanged." pinnedHeader is
// export_test.go's constant (same package, no build tag).
func TestBoardReopen_Integration_ExportsKeepTheirHeaderAndColumns(t *testing.T) {
	ctx := context.Background()
	s := newBRRSuite(t, ctx)
	ts, client := newDashServer(t, ctx, s.pool)
	defer ts.Close()

	a := s.task(t, ctx, "export")
	if f := bdDismiss(t, client, ts.URL, a.id, "handled_elsewhere", ""); !strings.Contains(f, "ok") {
		t.Fatalf("dismiss flashed %q", f)
	}
	s.activityReopen(t, ctx, a, "export-in1")

	_, csv := get(t, client, ts.URL+"/export/tasks.csv?project="+brrSlug)
	first := strings.SplitN(strings.ReplaceAll(csv, "\r\n", "\n"), "\n", 2)[0]
	if first != pinnedHeader {
		t.Errorf("CSV header = %q, want %q. D12: the marker is a separate read, NOT a boardQuery column",
			first, pinnedHeader)
	}
	if strings.Contains(csv, brrMarker) {
		t.Errorf("the CSV export carries the board marker")
	}

	_, js := get(t, client, ts.URL+"/export/tasks.json?project="+brrSlug)
	var rows []map[string]any
	if err := json.Unmarshal([]byte(js), &rows); err != nil {
		t.Fatalf("decode the JSON export: %v\n%s", err, snippet(js))
	}
	want := strings.Split(pinnedHeader, ",")
	for _, r := range rows {
		if r["id"] != float64(a.id) {
			continue
		}
		if len(r) != len(want) {
			t.Errorf("JSON export row has %d keys, want %d (%v): %v", len(r), len(want), want, r)
		}
		for _, k := range want {
			if _, ok := r[k]; !ok {
				t.Errorf("JSON export row lacks %q", k)
			}
		}
		return
	}
	t.Errorf("the JSON export has no row for task %d", a.id)
}
