//go:build integration

package tools_test

// SWT-35 (docs/tickets/task-list-mcp_SPEC.md) acceptance criteria 8–16 against a
// real database: task_list and project_list through executor.Execute, the only
// route to a handler (invariant 3).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run 'TaskList|ProjectList' ./internal/tools/
//
// SPEC CHANGE APPLIED 2026-09-10 (Salvador, relayed by the coordinator):
// task_list does NOT refuse ai_locality='local_only' projects. Production has six
// (bulk, homelab, personal, foundry, saka, town-ai), four of them with clients,
// and their queues are meant to be listable from Claude. So:
//   - criterion 12 is INVERTED: a local_only project IS listed, for every actor
//     shape, byte-identically to the same fixture at ai_locality='any', and its
//     rows still carry no `body`;
//   - project_list rows carry NO local_only key (criterion 16);
//   - criterion 15's "refused call" is the unknown slug of criterion 13.
//
// WHY THESE ARE HERE AND NOT IN A UNIT TEST: every assertion turns on a value
// Postgres computes — the queue order, the in_play predicate, the project
// clause, the grouped counts. IK landmine 6 ("test the column, not the
// fixture"): a predicate fed by a column gets its regression test where Postgres
// produces the value, and it must fail when the clause is mutated. The mutations
// that must turn each test red are named inline; perform them by hand at
// verification step 2.
//
// CROSS-POLLUTION PACT (IK "integration suites cross-pollute"; `make
// integration` runs -p 1). Every project this file seeds is under the existing
// `itest-mcp-tools-` prefix, so lifecycle_integration_test.go's
// cleanupToolsData removes its tasks, claims, events and projects in FK order.
// project_list is a GLOBAL read (every other suite's leftover projects come
// back too), so every assertion is on this file's own marked rows, never on a
// global count or on the global order. Audit rows written under REAL actor
// shapes (criterion 12) are not caught by cleanupToolsData's actor LIKE, so
// queueCleanup also removes them by tool plus the itest slug in args,
// policy_decisions first. Cleanup runs at START and at END. The db-wide counts
// in criterion 15 are DELTAS within one test, which -p 1 makes safe.
//
// Refuses to run against production (192.168.50.49): it writes and deletes
// fixtures.
//
// GREENFIELD NOTE — EXPECTED RED. tasklist_export_test.go aliases the
// not-yet-declared taskStatuses, so `go vet -tags integration ./internal/tools/`
// compile-FAILS the package today. Once tasklist.go exists but before Register
// wires the tools, every call fails with `unknown tool "task_list"`.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

// qActor is this file's executor actor wherever the actor is not the subject.
// It is under the itest-mcp-tools- prefix, so cleanupToolsData removes its
// audit rows.
const qActor = "itest-mcp-tools-queue"

// queueStatusesIT is the tasks.status CHECK, hand-copied, used to SEED
// fixtures. Criterion 14 is the comparison with the live CHECK.
var queueStatusesIT = []string{
	"holding", "ready", "claimed", "in_progress", "needs_feedback",
	"pr_open", "awaiting_ci", "awaiting_merge", "done_locally",
	"delivered", "closed", "blocked",
}

// The nine actor shapes of criterion 12 (see internal/policy/matrix_tasklist_test.go
// for why each is there).
var queueActorShapesIT = []string{
	"dashboard:salvo", "opsctl:salvo", "mcp:manual:salvo",
	"mcp:acme", "mcp:acme.main", "mcp:worker:acme",
	"drafts:gpt", "worker:acme", "ticketstatus:jira",
}

// ---- scaffolding ---------------------------------------------------------------

func queuePool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("DATABASE_URL points at production (192.168.50.49); this suite writes and deletes " +
			"fixtures — use the compose db on :5433")
	}
	pool := newToolsPool(t, ctx) // skips when DATABASE_URL is unset
	queueCleanup(t, ctx, pool)
	t.Cleanup(func() {
		queueCleanup(t, ctx, pool)
		pool.Close()
	})
	return pool
}

func queueCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const ours = `(SELECT id FROM audit_events WHERE tool IN ('task_list','project_list')
	                 AND (actor LIKE 'itest-mcp-tools-%' OR args->>'project' LIKE 'itest-mcp-tools-%'))`
	for _, q := range []string{
		`DELETE FROM policy_decisions WHERE audit_event_id IN ` + ours,
		`DELETE FROM audit_events WHERE id IN ` + ours,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("queue cleanup %q: %v", q, err)
		}
	}
	cleanupToolsData(t, ctx, pool)
}

// seedQueueProject inserts a project with an explicit ai_locality; client ""
// means NULL (personal and bulk are client IS NULL in production).
func seedQueueProject(t *testing.T, ctx context.Context, pool *pgxpool.Pool, slug, client, locality string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1, $1, NULLIF($2,''), 'manual', 'dashboard', '/tmp/itest', $3) RETURNING id`,
		slug, client, locality).Scan(&id); err != nil {
		t.Fatalf("seed project %q (%s): %v", slug, locality, err)
	}
	return id
}

// queueMatrixExecutor is the production wiring — the policy Matrix in front of
// the static allow-list from the real registry — so an accidental humanOnly or
// snapshotGated entry shows up as a denial here, under the actor that hit it.
func queueMatrixExecutor(pool *pgxpool.Pool) *executor.Executor {
	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	return executor.New(reg, checker, audit.NewPGStore(pool))
}

type qTask struct {
	ID           int64  `json:"id"`
	Title        string `json:"title"`
	Status       string `json:"status"`
	Priority     int    `json:"priority"`
	AssigneeType string `json:"assignee_type"`
	Subproject   string `json:"subproject"`
	ParentID     *int64 `json:"parent_id"`
}

type taskListOut struct {
	Project   string                     `json:"project"`
	Filter    map[string]json.RawMessage `json:"filter"`
	Counts    map[string]int             `json:"counts"`
	Total     int                        `json:"total"`
	Truncated bool                       `json:"truncated"`
	Tasks     []qTask                    `json:"tasks"`
}

func listTasks(t *testing.T, ctx context.Context, ex *executor.Executor, actor, args string) (taskListOut, json.RawMessage) {
	t.Helper()
	raw := callOK(t, ctx, ex, actor, "task_list", args)
	var out taskListOut
	mustUnmarshal(t, raw, &out)
	return out, raw
}

func taskIDs(ts []qTask) []int64 {
	out := make([]int64, len(ts))
	for i, x := range ts {
		out[i] = x.ID
	}
	return out
}

func sortedIDs(in []int64) []int64 {
	out := append([]int64(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func idsEqual(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func countsEqual(got, want map[string]int) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func filterString(t *testing.T, out taskListOut, key string) string {
	t.Helper()
	raw, ok := out.Filter[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("filter.%s = %s, not a string", key, raw)
	}
	return s
}

func filterInt(t *testing.T, out taskListOut, key string) int {
	t.Helper()
	raw, ok := out.Filter[key]
	if !ok {
		t.Fatalf("filter has no %q key (filter: %v) — L8: limit is always echoed", key, out.Filter)
	}
	n, err := strconv.Atoi(string(raw))
	if err != nil {
		t.Fatalf("filter.%s = %s, not an integer", key, raw)
	}
	return n
}

func rawKeys(m map[string]json.RawMessage) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// ---- criterion 8: the order IS task_get_next's --------------------------------

func TestTaskList_Integration_OrderingMatchesGetNext(t *testing.T) {
	ctx := context.Background()
	pool := queuePool(t, ctx)

	const (
		slug   = "itest-mcp-tools-qord"
		client = "itest-mcp-tools-qordclient" // unique: task_get_next keys on CLIENT (fact 1)
		worker = "itest-mcp-tools-qordworker"
	)
	pid := seedProject(t, ctx, pool, slug, client)
	ex := newExecutor(pool)
	p := func(v int) *int { return &v }

	// In play but NOT ready/claude: they lead the DEFAULT set by priority, and
	// are absent from the ready/claude rounds. Distinct created_at offsets keep
	// the FIFO tie-break deterministic.
	xHuman := insertTask(t, ctx, pool, pid, "", "ready", "human", 100, nil, "40 seconds")
	xHolding := insertTask(t, ctx, pool, pid, "", "holding", "claude", 100, nil, "39 seconds")
	xBlocked := insertTask(t, ctx, pool, pid, "", "blocked", "claude", 100, nil, "38 seconds")
	xClaimed := insertTask(t, ctx, pool, pid, "", "claimed", "claude", 100, nil, "37 seconds")

	// The getnext_ordering_integration_test.go mix.
	tHigh := insertTask(t, ctx, pool, pid, "", "ready", "claude", 9, nil, "5 seconds")
	tMidA := insertTask(t, ctx, pool, pid, "", "ready", "claude", 5, p(1), "5 seconds")
	tMidB := insertTask(t, ctx, pool, pid, "", "ready", "claude", 5, p(2), "5 seconds")
	tMidNull := insertTask(t, ctx, pool, pid, "", "ready", "claude", 5, nil, "30 seconds") // earliest
	tMidNull2 := insertTask(t, ctx, pool, pid, "", "ready", "claude", 5, nil, "5 seconds")
	tLow := insertTask(t, ctx, pool, pid, "", "ready", "claude", 1, nil, "5 seconds")

	// The last tie-break, id ASC: two rows from ONE statement share now(), so
	// their created_at is identical and only the id can order them.
	rows, err := pool.Query(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status, priority, created_at)
		 SELECT $1, 'ord tie', 'claude', 'ready', 3, now() - interval '20 seconds'
		   FROM generate_series(1,2) RETURNING id`, pid)
	if err != nil {
		t.Fatalf("seed id tie pair: %v", err)
	}
	var tie []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan tie id: %v", err)
		}
		tie = append(tie, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(tie) != 2 {
		t.Fatalf("seed id tie pair: %d ids, err %v", len(tie), err)
	}
	tie = sortedIDs(tie)

	want := []int64{
		xHuman, xHolding, xBlocked, xClaimed, // priority 100, NULL plan_order, created_at ASC
		tHigh,                             // 9
		tMidA, tMidB, tMidNull, tMidNull2, // 5: plan_order 1, 2, then NULLs by created_at
		tie[0], tie[1], // 3: same created_at, id ASC
		tLow, // 1
	}
	out, _ := listTasks(t, ctx, ex, qActor, `{"project":"`+slug+`","limit":200}`)
	if got := taskIDs(out.Tasks); !idsEqual(got, want) {
		t.Fatalf("task_list default order = %v, want %v — priority DESC → plan_order ASC NULLS LAST → "+
			"created_at ASC → id ASC, task_get_next's ORDER BY (L7)", got, want)
	}

	// Pass after pass: the first ready/claude row IS what task_get_next hands a
	// worker. Claim it between rounds (through the executor, as a worker does)
	// until both are empty.
	peek := func() getNextOut {
		var n getNextOut
		mustUnmarshal(t, callOK(t, ctx, ex, qActor, "task_get_next", `{"client":"`+client+`"}`), &n)
		return n
	}
	wantRounds := []int64{tHigh, tMidA, tMidB, tMidNull, tMidNull2, tie[0], tie[1], tLow}
	var taken []int64
	for round := 0; ; round++ {
		if round > len(wantRounds)+1 {
			t.Fatalf("rounds did not drain after %d passes (taken %v)", round, taken)
		}
		list, raw := listTasks(t, ctx, ex, qActor,
			`{"project":"`+slug+`","status":"ready","assignee_type":"claude","limit":200}`)
		next := peek()
		if len(list.Tasks) == 0 {
			if next.Task != nil {
				t.Fatalf("round %d: task_list(ready, claude) is empty but task_get_next returned %+v", round, next.Task)
			}
			if !bytes.Contains(raw, []byte(`"tasks":[]`)) {
				t.Errorf("an empty task_list returned %s; want \"tasks\":[] — never null (SPEC, Response)", raw)
			}
			break
		}
		if next.Task == nil || next.Task.ID != list.Tasks[0].ID {
			t.Fatalf("round %d: task_list's first ready/claude row = %d, task_get_next = %+v — they must agree "+
				"(L7: ONE ordering)", round, list.Tasks[0].ID, next.Task)
		}
		taken = append(taken, next.Task.ID)
		callOK(t, ctx, ex, qActor, "task_claim",
			`{"task_id":`+strconv.FormatInt(next.Task.ID, 10)+`,"worker_id":"`+worker+`"}`)
	}
	if !idsEqual(taken, wantRounds) {
		t.Errorf("tasks taken in order %v, want %v", taken, wantRounds)
	}
}

// ---- criterion 9: the default is in_play, and explicit status still works -----

func TestTaskList_Integration_DefaultShowsOnlyWorkInPlay(t *testing.T) {
	ctx := context.Background()
	pool := queuePool(t, ctx)

	const slug = "itest-mcp-tools-qplay"
	pid := seedProject(t, ctx, pool, slug, "itest-mcp-tools-qplayclient")
	ex := newExecutor(pool)

	byStatus := map[string]int64{}
	for i, s := range queueStatusesIT {
		byStatus[s] = insertTask(t, ctx, pool, pid, "", s, "claude", 5, nil, strconv.Itoa(60-i)+" seconds")
	}

	out, _ := listTasks(t, ctx, ex, qActor, `{"project":"`+slug+`"}`)
	if len(out.Tasks) != 10 {
		t.Errorf("default task_list returned %d rows, want 10 — every status but closed and delivered (L4, Q2)", len(out.Tasks))
	}
	var wantIDs []int64
	for _, s := range queueStatusesIT {
		if s != "closed" && s != "delivered" {
			wantIDs = append(wantIDs, byStatus[s])
		}
	}
	if got := sortedIDs(taskIDs(out.Tasks)); !idsEqual(got, sortedIDs(wantIDs)) {
		t.Errorf("default task_list ids = %v, want the ten in-play ids %v", got, sortedIDs(wantIDs))
	}
	for _, x := range out.Tasks {
		if x.Status == "closed" || x.Status == "delivered" {
			t.Errorf("default task_list returned a %s task (#%d). Q2 = b: 'filter so no junk and wasted "+
				"tokens' — finished work is not in the answer unless asked for by status", x.Status, x.ID)
		}
	}
	for _, s := range []string{"closed", "delivered"} {
		if _, ok := out.Counts[s]; ok {
			t.Errorf("default counts carry a %q key (%v); the counts are over the SAME filter as the rows (L8)", s, out.Counts)
		}
	}
	for _, s := range []string{"holding", "done_locally"} {
		if out.Counts[s] != 1 {
			t.Errorf("default counts[%s] = %d, want 1. L4a: holding is a review decision owed by Salvador and "+
				"done_locally still owes its delivery — both are work IN PLAY", s, out.Counts[s])
		}
	}
	if out.Total != 10 {
		t.Errorf("default total = %d, want 10", out.Total)
	}
	if got := filterString(t, out, "status"); got != "in_play" {
		t.Errorf("default filter.status = %q, want \"in_play\" (L8)", got)
	}

	for _, s := range []string{"delivered", "closed"} {
		o, _ := listTasks(t, ctx, ex, qActor, `{"project":"`+slug+`","status":"`+s+`"}`)
		if got := taskIDs(o.Tasks); !idsEqual(got, []int64{byStatus[s]}) {
			t.Errorf("task_list(status=%s) = %v, want exactly [%d] — an explicit status still returns "+
				"exactly that set (L4)", s, got, byStatus[s])
		}
		if !countsEqual(o.Counts, map[string]int{s: 1}) || o.Total != 1 {
			t.Errorf("task_list(status=%s) counts = %v total %d, want {%s:1} total 1", s, o.Counts, o.Total, s)
		}
		if got := filterString(t, o, "status"); got != s {
			t.Errorf("task_list(status=%s) filter.status = %q, want %q", s, got, s)
		}
	}
}

// ---- criterion 10: the project clause, with a same-client sibling as control ---

// MUTATION THAT MUST TURN THIS RED: removing the project clause from the shared
// WHERE fragment. The same-client sibling B is what makes the control bite
// against a `p.client = …` substitution — task_get_next's clause, the natural
// thing to copy — which C alone would not catch.
func TestTaskList_Integration_ProjectFilterIsolates(t *testing.T) {
	ctx := context.Background()
	pool := queuePool(t, ctx)

	const (
		slugA  = "itest-mcp-tools-qisoa"
		slugB  = "itest-mcp-tools-qisob"
		slugC  = "itest-mcp-tools-qisoc"
		shared = "itest-mcp-tools-qisoclient"
	)
	pa := seedProject(t, ctx, pool, slugA, shared)
	pb := seedProject(t, ctx, pool, slugB, shared) // SAME client as A
	pc := seedProject(t, ctx, pool, slugC, "itest-mcp-tools-qisoother")
	ex := newExecutor(pool)

	seed := func(pid int64, n int) []int64 {
		var ids []int64
		for i := 0; i < n; i++ {
			ids = append(ids, insertTask(t, ctx, pool, pid, "", "ready", "claude", 5, nil, strconv.Itoa(30-i)+" seconds"))
		}
		return ids
	}
	aIDs := seed(pa, 2)
	bIDs := seed(pb, 3)
	seed(pc, 4)

	for _, c := range []struct {
		slug string
		ids  []int64
	}{{slugA, aIDs}, {slugB, bIDs}} {
		out, _ := listTasks(t, ctx, ex, qActor, `{"project":"`+c.slug+`","limit":200}`)
		if got := sortedIDs(taskIDs(out.Tasks)); !idsEqual(got, sortedIDs(c.ids)) {
			t.Errorf("task_list(%s) ids = %v, want only its own %v. A same-client sibling's tasks here "+
				"mean the clause is on p.client, not the project", c.slug, got, sortedIDs(c.ids))
		}
		if !countsEqual(out.Counts, map[string]int{"ready": len(c.ids)}) || out.Total != len(c.ids) {
			t.Errorf("task_list(%s) counts = %v total %d, want {ready:%d} total %d — counts count only "+
				"this project", c.slug, out.Counts, out.Total, len(c.ids), len(c.ids))
		}
		if out.Project != c.slug {
			t.Errorf("task_list(%s).project = %q, want the resolved slug echoed (L8)", c.slug, out.Project)
		}
	}
}

// ---- criterion 11: counts ignore limit and share the filter --------------------

// MUTATION THAT MUST TURN THIS RED: giving the counts query its own WHERE
// without the project clause (B's noise then inflates every count), or without
// the optional assignee_type / subproject clauses (the narrowed counts stop
// matching the rows).
func TestTaskList_Integration_CountsIgnoreLimitAndShareTheFilter(t *testing.T) {
	ctx := context.Background()
	pool := queuePool(t, ctx)

	const (
		slugA  = "itest-mcp-tools-qcnta"
		slugB  = "itest-mcp-tools-qcntb"
		slug30 = "itest-mcp-tools-qcnt30"
		client = "itest-mcp-tools-qcntclient"
	)
	pa := seedProject(t, ctx, pool, slugA, client)
	pb := seedProject(t, ctx, pool, slugB, client)
	ex := newExecutor(pool)

	// A: 5 ready + 3 blocked, mixed assignee and subproject.
	//   ready:   claude/main, claude/main, claude/-, human/-, human/main
	//   blocked: claude/main, claude/-,   human/-
	for i, r := range []struct{ sub, status, who string }{
		{"main", "ready", "claude"}, {"main", "ready", "claude"}, {"", "ready", "claude"},
		{"", "ready", "human"}, {"main", "ready", "human"},
		{"main", "blocked", "claude"}, {"", "blocked", "claude"}, {"", "blocked", "human"},
	} {
		insertTask(t, ctx, pool, pa, r.sub, r.status, r.who, 9-i, nil, strconv.Itoa(40-i)+" seconds")
	}
	// B: noise in the SAME statuses, under the same client and subproject, so a
	// leaked WHERE inflates every combination below.
	for i := 0; i < 4; i++ {
		insertTask(t, ctx, pool, pb, "main", "ready", "claude", 5, nil, "10 seconds")
	}
	for i := 0; i < 2; i++ {
		insertTask(t, ctx, pool, pb, "main", "blocked", "claude", 5, nil, "10 seconds")
	}

	// limit=2: two rows, but the counts are over the FULL filtered set.
	o, _ := listTasks(t, ctx, ex, qActor, `{"project":"`+slugA+`","limit":2}`)
	if len(o.Tasks) != 2 || !o.Truncated {
		t.Errorf("task_list(A, limit=2) = %d rows truncated=%v, want 2 rows truncated=true", len(o.Tasks), o.Truncated)
	}
	if !countsEqual(o.Counts, map[string]int{"blocked": 3, "ready": 5}) || o.Total != 8 {
		t.Errorf("task_list(A, limit=2) counts = %v total %d, want {blocked:3, ready:5} total 8 — L8: counts "+
			"are independent of limit ('12 ready, 3 blocked, showing 25' must be exact)", o.Counts, o.Total)
	}
	if got := filterInt(t, o, "limit"); got != 2 {
		t.Errorf("filter.limit = %d, want 2", got)
	}

	o, _ = listTasks(t, ctx, ex, qActor, `{"project":"`+slugA+`","limit":200}`)
	if o.Truncated || o.Total != len(o.Tasks) || o.Total != 8 {
		t.Errorf("task_list(A, limit=200) = %d rows, total %d, truncated=%v; want 8, 8, false", len(o.Tasks), o.Total, o.Truncated)
	}

	// The counts narrow EXACTLY as the rows do.
	for _, c := range []struct {
		args   string
		counts map[string]int
		who    string
		sub    string
	}{
		{`"assignee_type":"claude"`, map[string]int{"ready": 3, "blocked": 2}, "claude", ""},
		{`"assignee_type":"claude","subproject":"main"`, map[string]int{"ready": 2, "blocked": 1}, "claude", "main"},
		{`"subproject":"main"`, map[string]int{"ready": 3, "blocked": 1}, "", "main"},
	} {
		o, _ := listTasks(t, ctx, ex, qActor, `{"project":"`+slugA+`",`+c.args+`,"limit":200}`)
		if !countsEqual(o.Counts, c.counts) {
			t.Errorf("task_list(A, %s) counts = %v, want %v — the WHERE fragment is built ONCE and used by "+
				"both queries (L8)", c.args, o.Counts, c.counts)
		}
		tally := map[string]int{}
		for _, x := range o.Tasks {
			tally[x.Status]++
			if c.who != "" && x.AssigneeType != c.who {
				t.Errorf("task_list(A, %s) returned #%d assigned to %s", c.args, x.ID, x.AssigneeType)
			}
			if c.sub != "" && x.Subproject != c.sub {
				t.Errorf("task_list(A, %s) returned #%d in subproject %q", c.args, x.ID, x.Subproject)
			}
		}
		if !countsEqual(tally, o.Counts) || o.Total != len(o.Tasks) {
			t.Errorf("task_list(A, %s): rows tally %v (%d rows) but counts %v total %d — rows and counts "+
				"disagree about the filter", c.args, tally, len(o.Tasks), o.Counts, o.Total)
		}
		if c.who != "" && filterString(t, o, "assignee_type") != c.who {
			t.Errorf("task_list(A, %s) filter = %v, want assignee_type echoed", c.args, o.Filter)
		}
		if c.sub != "" && filterString(t, o, "subproject") != c.sub {
			t.Errorf("task_list(A, %s) filter = %v, want subproject echoed", c.args, o.Filter)
		}
	}
	// …and still independently of limit.
	o, _ = listTasks(t, ctx, ex, qActor, `{"project":"`+slugA+`","assignee_type":"claude","limit":1}`)
	if len(o.Tasks) != 1 || !o.Truncated || !countsEqual(o.Counts, map[string]int{"ready": 3, "blocked": 2}) || o.Total != 5 {
		t.Errorf("task_list(A, claude, limit=1) = %d rows truncated=%v counts %v total %d; want 1, true, "+
			"{ready:3, blocked:2}, 5", len(o.Tasks), o.Truncated, o.Counts, o.Total)
	}

	// L9: 30 ready in a fresh project, no limit → one screenful of 25.
	p30 := seedProject(t, ctx, pool, slug30, client)
	for i := 0; i < 30; i++ {
		insertTask(t, ctx, pool, p30, "", "ready", "claude", 5, nil, strconv.Itoa(100-i)+" seconds")
	}
	o, _ = listTasks(t, ctx, ex, qActor, `{"project":"`+slug30+`"}`)
	if len(o.Tasks) != 25 || !o.Truncated || o.Total != 30 || !countsEqual(o.Counts, map[string]int{"ready": 30}) {
		t.Errorf("task_list(30 ready, no limit) = %d rows truncated=%v total %d counts %v; want 25, true, 30, "+
			"{ready:30} (L9: default 25)", len(o.Tasks), o.Truncated, o.Total, o.Counts)
	}
	if got := filterInt(t, o, "limit"); got != 25 {
		t.Errorf("default filter.limit = %d, want 25 (the APPLIED value is echoed)", got)
	}
	if got := rawKeys(o.Filter); strings.Join(got, ",") != "limit,status" {
		t.Errorf("default filter keys = %v, want exactly [limit status] (L8)", got)
	}
	// Honest truncation: limit+1 is fetched, so a page that is exactly full is
	// NOT truncated, and one row short is.
	o, _ = listTasks(t, ctx, ex, qActor, `{"project":"`+slug30+`","limit":30}`)
	if len(o.Tasks) != 30 || o.Truncated {
		t.Errorf("task_list(30 ready, limit=30) = %d rows truncated=%v; want 30, false — mail_search's limit+1 rule", len(o.Tasks), o.Truncated)
	}
	o, _ = listTasks(t, ctx, ex, qActor, `{"project":"`+slug30+`","limit":29}`)
	if len(o.Tasks) != 29 || !o.Truncated {
		t.Errorf("task_list(30 ready, limit=29) = %d rows truncated=%v; want 29, true", len(o.Tasks), o.Truncated)
	}
	// Clamped, not refused; the applied value is echoed.
	o, _ = listTasks(t, ctx, ex, qActor, `{"project":"`+slug30+`","limit":10000}`)
	if got := filterInt(t, o, "limit"); got != 200 || len(o.Tasks) != 30 || o.Truncated {
		t.Errorf("task_list(limit=10000) filter.limit=%d rows=%d truncated=%v; want 200, 30, false (L9)", got, len(o.Tasks), o.Truncated)
	}
}

// ---- criterion 12 (INVERTED 2026-09-10): local_only projects ARE listed --------

// Salvador, 2026-09-10: task_list must NOT refuse ai_locality='local_only'
// projects. The fixture is local_only IN POSTGRES (asserted, not assumed —
// landmine 6: a locality test whose fixture never reached the column proves
// nothing), and every actor shape must get its tasks. The control is the SAME
// project flipped to 'any': the answers must be byte-identical, so the column
// changes nothing about the response.
//
// MUTATION THAT MUST TURN THIS RED: putting a locality clause back anywhere —
// `ai_locality` in the slug-resolving SELECT with a refusal on local_only, or
// `p.ai_locality = 'any'` in the WHERE fragment. Either way the local_only
// passes error or return no rows, and the byte comparison fails.
func TestTaskList_Integration_ListsLocalOnlyProjectsForEveryActor(t *testing.T) {
	ctx := context.Background()
	pool := queuePool(t, ctx)

	// Four of production's six local_only projects carry a client; so does this one.
	const (
		slug     = "itest-mcp-tools-qlocal"
		bodyText = "itest restricted personal text that must never reach a task_list row"
	)
	pid := seedQueueProject(t, ctx, pool, slug, "itest-mcp-tools-qlocalclient", "local_only")
	insertTask(t, ctx, pool, pid, "", "ready", "claude", 5, nil, "30 seconds")
	insertTask(t, ctx, pool, pid, "", "ready", "human", 3, nil, "20 seconds")
	insertTask(t, ctx, pool, pid, "", "blocked", "claude", 1, nil, "10 seconds")
	if _, err := pool.Exec(ctx, `UPDATE tasks SET body = $2 WHERE project_id = $1`, pid, bodyText); err != nil {
		t.Fatalf("seed bodies: %v", err)
	}
	var locality string
	if err := pool.QueryRow(ctx, `SELECT ai_locality FROM projects WHERE id = $1`, pid).Scan(&locality); err != nil || locality != "local_only" {
		t.Fatalf("fixture ai_locality = %q (err %v), want local_only — the test is about what the column does", locality, err)
	}

	ex := queueMatrixExecutor(pool)
	args := []byte(`{"project":"` + slug + `"}`)
	byActor := map[string]json.RawMessage{}
	for _, actor := range queueActorShapesIT {
		res, err := ex.Execute(ctx, executor.Call{Tool: "task_list", Actor: actor, Args: args})
		if err != nil {
			t.Errorf("task_list(local_only project) by %q = %v, want its tasks. Salvador, 2026-09-10: "+
				"local_only projects are listable from Claude — six in production, four with clients", actor, err)
			continue
		}
		byActor[actor] = res.Output
		var out taskListOut
		mustUnmarshal(t, res.Output, &out)
		if len(out.Tasks) != 3 || out.Total != 3 || !countsEqual(out.Counts, map[string]int{"ready": 2, "blocked": 1}) {
			t.Errorf("task_list(local_only) by %q = %d rows, total %d, counts %v; want 3, 3, {ready:2, blocked:1}",
				actor, len(out.Tasks), out.Total, out.Counts)
		}
		var rows struct {
			Tasks []map[string]json.RawMessage `json:"tasks"`
		}
		mustUnmarshal(t, res.Output, &rows)
		for _, r := range rows.Tasks {
			if _, ok := r["body"]; ok {
				t.Errorf("a task_list row by %q carries a body key (%v). L6: no body — the single largest "+
					"token cost, and task_context is the audited per-task read", actor, rawKeys(r))
			}
		}
		if bytes.Contains(res.Output, []byte(bodyText)) {
			t.Errorf("task_list output by %q contains the task body text", actor)
		}
	}

	// Control: the same fixture at 'any' — the column is the only difference.
	if _, err := pool.Exec(ctx, `UPDATE projects SET ai_locality = 'any' WHERE id = $1`, pid); err != nil {
		t.Fatalf("flip fixture to any: %v", err)
	}
	control, err := ex.Execute(ctx, executor.Call{Tool: "task_list", Actor: "mcp:acme", Args: args})
	if err != nil {
		t.Fatalf("control task_list at ai_locality='any': %v", err)
	}
	for actor, got := range byActor {
		if !bytes.Equal(got, control.Output) {
			t.Errorf("task_list by %q at local_only = %s\n  but the same fixture at 'any' = %s\n"+
				"The answer must not depend on ai_locality", actor, got, control.Output)
		}
	}
}

// ---- criterion 13: an unknown slug is an error that names project_list ---------

func TestTaskList_Integration_UnknownProjectIsAnError(t *testing.T) {
	ctx := context.Background()
	pool := queuePool(t, ctx)

	const (
		missing   = "itest-mcp-tools-qnope"
		neighbour = "itest-mcp-tools-qnopeneighbour"
	)
	seedProject(t, ctx, pool, neighbour, "itest-mcp-tools-qnopeclient")
	ex := newExecutor(pool)

	res, err := ex.Execute(ctx, executor.Call{Tool: "task_list", Actor: qActor, Args: []byte(`{"project":"` + missing + `"}`)})
	if err == nil {
		t.Fatalf("task_list(%q) = %s, want an error. L10: an empty {\"tasks\":[]} for a slug that does not "+
			"exist reads as 'nothing in my queue' — IK's 'a page that looks empty may be the wrong page'", missing, res.Output)
	}
	msg := err.Error()
	if !strings.Contains(msg, missing) {
		t.Errorf("task_list(%q) error = %q, want it to name the slug", missing, msg)
	}
	if !strings.Contains(msg, "project_list") {
		t.Errorf("task_list(%q) error = %q, want it to name project_list — 'call project_list for valid slugs' (L10)", missing, msg)
	}
	if strings.Contains(msg, neighbour) {
		t.Errorf("task_list(%q) error = %q enumerates slugs; L10: it points at project_list and does not list them", missing, msg)
	}
	if len(res.Output) != 0 {
		t.Errorf("a failed task_list returned output %s alongside its error", res.Output)
	}
}

// ---- criterion 14: taskStatuses IS the CHECK ------------------------------------

func TestTaskList_Integration_StatusVocabularyMatchesTheCheck(t *testing.T) {
	ctx := context.Background()
	pool := queuePool(t, ctx)

	rows, err := pool.Query(ctx,
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
		  WHERE conrelid = 'tasks'::regclass AND contype = 'c'`)
	if err != nil {
		t.Fatalf("read tasks CHECK constraints: %v", err)
	}
	mentionsStatus := regexp.MustCompile(`\bstatus\b`)
	var defs []string
	for rows.Next() {
		var def string
		if err := rows.Scan(&def); err != nil {
			t.Fatalf("scan constraint def: %v", err)
		}
		if mentionsStatus.MatchString(def) {
			defs = append(defs, def)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate constraints: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("found %d CHECK constraint(s) on tasks mentioning status (%v), want exactly 1 — otherwise the "+
			"parse below could pass vacuously or read the wrong list", len(defs), defs)
	}

	quoted := regexp.MustCompile(`'([a-z_]+)'`)
	check := map[string]bool{}
	for _, m := range quoted.FindAllStringSubmatch(defs[0], -1) {
		check[m[1]] = true
	}
	if len(check) == 0 {
		t.Fatalf("parsed no quoted values out of %q; the comparison below would be vacuous", defs[0])
	}
	goList := map[string]bool{}
	for _, s := range tools.TaskStatusesForTest {
		goList[s] = true
	}
	for s := range check {
		if !goList[s] {
			t.Errorf("the tasks.status CHECK allows %q but taskStatuses does not: validateTaskList would refuse a "+
				"status the database holds (L5)", s)
		}
	}
	for s := range goList {
		if !check[s] {
			t.Errorf("taskStatuses has %q but the tasks.status CHECK does not: a filter on it can only ever be "+
				"empty — the typo'd-status landmine L5 refuses by name", s)
		}
	}
}

// ---- criterion 15: reads write only their audit row -----------------------------

func TestTaskList_Integration_ReadsWriteOnlyTheAuditRow(t *testing.T) {
	ctx := context.Background()
	pool := queuePool(t, ctx)

	const (
		slug  = "itest-mcp-tools-qro"
		actor = "itest-mcp-tools-qro-actor"
	)
	pid := seedProject(t, ctx, pool, slug, "itest-mcp-tools-qroclient")
	insertTask(t, ctx, pool, pid, "", "ready", "claude", 5, nil, "50 seconds")
	insertTask(t, ctx, pool, pid, "", "ready", "human", 4, nil, "40 seconds")
	insertTask(t, ctx, pool, pid, "main", "ready", "claude", 3, nil, "30 seconds")
	insertTask(t, ctx, pool, pid, "", "blocked", "claude", 2, nil, "20 seconds")
	insertTask(t, ctx, pool, pid, "", "claimed", "claude", 1, nil, "10 seconds")
	ex := newExecutor(pool)

	type state struct{ status, updated string }
	snapshot := func() map[int64]state {
		rows, err := pool.Query(ctx, `SELECT id, status, updated_at::text FROM tasks WHERE project_id = $1`, pid)
		if err != nil {
			t.Fatalf("snapshot tasks: %v", err)
		}
		defer rows.Close()
		m := map[int64]state{}
		for rows.Next() {
			var id int64
			var s state
			if err := rows.Scan(&id, &s.status, &s.updated); err != nil {
				t.Fatalf("scan task: %v", err)
			}
			m[id] = s
		}
		return m
	}
	count := func(q string) int64 {
		var n int64
		if err := pool.QueryRow(ctx, q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	counters := []string{
		`SELECT count(*) FROM task_events`,
		`SELECT count(*) FROM task_claims`,
		`SELECT count(*) FROM tasks`,
		`SELECT count(*) FROM projects`,
	}
	before := snapshot()
	beforeCounts := map[string]int64{}
	for _, q := range counters {
		beforeCounts[q] = count(q)
	}
	auditBase := count(`SELECT COALESCE(max(id), 0) FROM audit_events`)

	callOK(t, ctx, ex, actor, "task_list", `{"project":"`+slug+`"}`)
	callOK(t, ctx, ex, actor, "task_list", `{"project":"`+slug+`","status":"ready","assignee_type":"claude"}`)
	callOK(t, ctx, ex, actor, "project_list", `{}`)
	// The refused call (criterion 13's unknown slug — since 2026-09-10 a
	// local_only project is not refused).
	if _, err := ex.Execute(ctx, executor.Call{Tool: "task_list", Actor: actor,
		Args: []byte(`{"project":"itest-mcp-tools-qro-nope"}`)}); err == nil {
		t.Fatalf("task_list on an unknown slug succeeded; criterion 13")
	}

	after := snapshot()
	if len(after) != len(before) {
		t.Errorf("fixture task count changed %d -> %d", len(before), len(after))
	}
	for id, b := range before {
		if a := after[id]; a != b {
			t.Errorf("task #%d changed across read-only calls: %+v -> %+v. task_list reads; it never claims "+
				"or touches (L0, L14)", id, b, a)
		}
	}
	for _, q := range counters {
		if got := count(q); got != beforeCounts[q] {
			t.Errorf("%s moved %d -> %d across read-only calls — nothing but audit_events may change "+
				"(invariant 7: no task_events write, no NOTIFY)", q, beforeCounts[q], got)
		}
	}

	rows, err := pool.Query(ctx,
		`SELECT tool, status, COALESCE(error,''), completed_at IS NOT NULL
		   FROM audit_events WHERE id > $1 AND actor = $2 ORDER BY id`, auditBase, actor)
	if err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	defer rows.Close()
	type auditRow struct {
		tool, status, errText string
		completed             bool
	}
	var got []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.tool, &r.status, &r.errText, &r.completed); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		got = append(got, r)
	}
	want := []struct{ tool, status string }{
		{"task_list", "ok"}, {"task_list", "ok"}, {"project_list", "ok"}, {"task_list", "error"},
	}
	if len(got) != len(want) {
		t.Fatalf("audit rows for %s = %+v, want %d rows %v — invariant 3 for reads: every call leaves exactly "+
			"one audit row", actor, got, len(want), want)
	}
	for i, w := range want {
		if got[i].tool != w.tool || got[i].status != w.status || !got[i].completed {
			t.Errorf("audit row %d = %+v, want tool %s status %s, completed", i, got[i], w.tool, w.status)
		}
	}
	if !strings.Contains(got[3].errText, "project_list") {
		t.Errorf("the refused call's audit error = %q, want the unknown-slug message naming project_list", got[3].errText)
	}
}

// ---- criterion 16: project_list is a directory with honest counts -------------

// MUTATION THAT MUST TURN THIS RED: counting without the in-play predicate
// (plain count(t.id), no FILTER). A would then read 5 — its closed and delivered
// tasks included — and A.in_play would stop equalling task_list(A).total.
func TestProjectList_Integration_DirectoryAndCounts(t *testing.T) {
	ctx := context.Background()
	pool := queuePool(t, ctx)

	const (
		slugA   = "itest-mcp-tools-qdira"
		slugE   = "itest-mcp-tools-qdire"
		slugL   = "itest-mcp-tools-qdirl"
		clientA = "itest-mcp-tools-qdirclient"
		clientL = "itest-mcp-tools-qdirlclient"
	)
	pa := seedQueueProject(t, ctx, pool, slugA, clientA, "any")
	seedQueueProject(t, ctx, pool, slugE, "", "any") // NULL client, zero tasks
	pl := seedQueueProject(t, ctx, pool, slugL, clientL, "local_only")
	for i, s := range []string{"ready", "blocked", "holding", "closed", "delivered"} {
		insertTask(t, ctx, pool, pa, "", s, "claude", 5, nil, strconv.Itoa(50-i)+" seconds")
	}
	insertTask(t, ctx, pool, pl, "", "ready", "claude", 5, nil, "20 seconds")
	insertTask(t, ctx, pool, pl, "", "ready", "human", 5, nil, "10 seconds")
	var locality string
	if err := pool.QueryRow(ctx, `SELECT ai_locality FROM projects WHERE id = $1`, pl).Scan(&locality); err != nil || locality != "local_only" {
		t.Fatalf("L's ai_locality = %q (err %v), want local_only", locality, err)
	}
	ex := newExecutor(pool)

	raw := callOK(t, ctx, ex, qActor, "project_list", `{}`)
	var dir struct {
		Projects []map[string]json.RawMessage `json:"projects"`
	}
	mustUnmarshal(t, raw, &dir)

	str := func(m map[string]json.RawMessage, k string) string {
		var s string
		_ = json.Unmarshal(m[k], &s)
		return s
	}
	num := func(m map[string]json.RawMessage, k string) int {
		n, err := strconv.Atoi(string(m[k]))
		if err != nil {
			t.Fatalf("%s = %s, not an integer", k, m[k])
		}
		return n
	}

	idx := map[string]int{}
	rowOf := map[string]map[string]json.RawMessage{}
	for i, r := range dir.Projects {
		if _, ok := r["local_only"]; ok {
			t.Errorf("project_list row %q carries a local_only key. SPEC change 2026-09-10: the key is removed "+
				"entirely, since task_list no longer refuses local_only projects", str(r, "slug"))
		}
		s := str(r, "slug")
		if s == slugA || s == slugE || s == slugL {
			idx[s] = i
			rowOf[s] = r
		}
	}
	for _, s := range []string{slugA, slugE, slugL} {
		if _, ok := rowOf[s]; !ok {
			t.Fatalf("project_list omits %q. L11: every project is listed, none hidden — a project with nothing "+
				"in play (E) and a local_only one (L) are still slugs a repo may memorise", s)
		}
	}
	if !(idx[slugA] < idx[slugE] && idx[slugE] < idx[slugL]) {
		t.Errorf("project_list order: %s@%d %s@%d %s@%d, want ORDER BY slug", slugA, idx[slugA], slugE, idx[slugE], slugL, idx[slugL])
	}

	a, e, l := rowOf[slugA], rowOf[slugE], rowOf[slugL]
	if got := rawKeys(a); strings.Join(got, ",") != "client,in_play,name,slug" {
		t.Errorf("A's keys = %v, want exactly [client in_play name slug]", got)
	}
	if str(a, "client") != clientA || str(a, "name") != slugA {
		t.Errorf("A row = client %q name %q, want %q / %q", str(a, "client"), str(a, "name"), clientA, slugA)
	}
	if got := rawKeys(e); strings.Join(got, ",") != "in_play,name,slug" {
		t.Errorf("E's keys = %v, want exactly [in_play name slug] — a NULL client is OMITTED, not null (L11)", got)
	}
	if got := rawKeys(l); strings.Join(got, ",") != "client,in_play,name,slug" {
		t.Errorf("L's keys = %v, want exactly [client in_play name slug] — no local_only key", got)
	}

	if num(a, "in_play") != 3 {
		t.Errorf("A.in_play = %d, want 3 (ready + blocked + holding; closed and delivered are not in play)", num(a, "in_play"))
	}
	if num(e, "in_play") != 0 {
		t.Errorf("E.in_play = %d, want 0", num(e, "in_play"))
	}
	if num(l, "in_play") != 2 {
		t.Errorf("L.in_play = %d, want 2", num(l, "in_play"))
	}

	// Parity (L11): in_play IS the total task_list reports under its default.
	for _, c := range []struct {
		slug string
		row  map[string]json.RawMessage
	}{{slugA, a}, {slugE, e}, {slugL, l}} {
		o, raw := listTasks(t, ctx, ex, qActor, `{"project":"`+c.slug+`"}`)
		if o.Total != num(c.row, "in_play") {
			t.Errorf("%s: project_list in_play = %d but task_list total = %d — the two must come from the SAME "+
				"predicate const (L11, criterion 6)", c.slug, num(c.row, "in_play"), o.Total)
		}
		if c.slug == slugE && !bytes.Contains(raw, []byte(`"tasks":[]`)) {
			t.Errorf("task_list(E) = %s; want \"tasks\":[] — never null when empty", raw)
		}
	}
}
