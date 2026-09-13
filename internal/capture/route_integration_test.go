//go:build integration

package capture_test

// SWT-40 Part B against a real database (docs/tickets/inquiry-promote_SPEC.md):
// criteria B5 (one route row per applied message, no task, no executor call,
// run-twice), B6 (the inboxes follow the route row; a later shadow --all
// capture pass writes nothing for it), B7 (arming is the column; forward-only
// on the verdict clock), B9's rulesreport line, B-D1 (only accounts with
// candidate rows), E-D4's lock, and the migration's live shape.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_isob?sslmode=disable \
//	  TZ=UTC go test -tags integration -p 1 -count=1 -run CaptureRoute ./internal/capture/
//
// Build-tagged `integration`, env-gated on DATABASE_URL, FATAL on 192.168.50.49.
// NO LLM: verdicts are ai_runs + ai_extractions rows written by hand in exactly
// the shape the route lane records (internal/classify/route_test.go):
// fields.project_id (the resolved candidate or null) and fields.grounded.
//
// TEST THE COLUMN, NOT THE FIXTURE (IK SWT-21 #6): arming reads
// source_accounts.route_after, the verdict clock reads ai_runs.created_at, the
// candidate set reads source_account_projects through the message's
// raw_source_items.source_account_id, and the thread step reads the OTHER
// messages' latest decisions. Each is mutated inline in a comment.
//
// ---- IMPOSED SURFACE: see route_test.go (RunRouteApply, RouteApplyConfig,
// RouteStats, ErrRouteLockHeld). The route_apply INBOX:
//   - inbound; EXISTS a live 'unmatched' decision; the LATEST decision (any
//     mode) is 'unmatched' (so no route row yet);
//   - the receiving account has candidate rows AND route_after IS NOT NULL;
//   - sent within cfg.Since.
// Its verdict is the newest ok classify_route extraction on the message's raw
// item. Report format (B9), imposed loosely like Part D's GATE section: a
// section whose header line starts with ROUTE, then one line per step,
// `route <step>`, the count as the LAST field.
//
// GREENFIELD NOTE — EXPECTED RED: RunRouteApply and friends do not exist
// (compile failure first); past that, migration 0032 is required
// (raRequire0032 says so in one line).
//
// CROSS-SUITE DISCIPLINE. The shadow --all capture pass is GLOBAL, so this suite
// deletes capture_decisions WHOLESALE at start and end — the gate suite's
// precedent. Isolated database only. It owns projects itest-caproute-%, accounts
// by provider itest-caproute-src, threads gmail:itest-caproute:% and ai_runs
// model itest-caproute-model.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
	"github.com/sspataro57/switchboard/internal/triage"
)

const (
	raProvider  = "itest-caproute-src"
	raModel     = "itest-caproute-model"
	raThreadPfx = "gmail:itest-caproute:"
	raLockKey   = int64(0x5157_0015)
	raWindow    = 720 * time.Hour
)

type raSuite struct {
	pool                                      *pgxpool.Pool
	collab, reeng, other                      int64
	hoc, solo, nodef, unarmed, personal, late int64
	threads                                   map[string]int64
	seq                                       int
}

func raRequire0032(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var idx, tbl bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'capture_decisions_route_uniq'),
	                                     to_regclass('source_account_projects') IS NOT NULL`).Scan(&idx, &tbl); err != nil {
		t.Fatalf("probe for migration 0032: %v", err)
	}
	if !idx || !tbl {
		t.Fatalf("migrations/0032_route_tier.sql is not applied (capture_decisions_route_uniq %v, source_account_projects %v). "+
			"`make migrate` against the isolated database first", idx, tbl)
	}
}

func raCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE provider = '` + raProvider + `')`
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`
	const projs = `(SELECT id FROM projects WHERE slug LIKE 'itest-caproute-%')`
	for _, q := range []string{
		`DELETE FROM capture_decisions`, // wholesale: see the header
		`DELETE FROM ai_extractions WHERE ai_run_id IN (SELECT id FROM ai_runs WHERE model = '` + raModel + `')`,
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM ai_runs WHERE model = '` + raModel + `'`,
		`DELETE FROM source_account_projects WHERE source_account_id IN ` + accts + ` OR project_id IN ` + projs,
		`DELETE FROM capture_rules WHERE project_id IN ` + projs,
		`DELETE FROM projects WHERE slug LIKE 'itest-caproute-%'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE thread_key LIKE '` + raThreadPfx + `%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider = '` + raProvider + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func newRASuite(t *testing.T, ctx context.Context) *raSuite {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db (cleanup deletes capture_decisions " +
			"wholesale); use an isolated compose db on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	raRequire0032(t, ctx, pool)
	raCleanup(t, ctx, pool)
	t.Cleanup(func() { raCleanup(t, context.Background(), pool) })
	s := &raSuite{pool: pool, threads: map[string]int64{}}
	s.seed(t, ctx)
	return s
}

func (s *raSuite) id(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", q, err)
	}
	return id
}

func (s *raSuite) count(t *testing.T, ctx context.Context, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n
}

func (s *raSuite) exec(t *testing.T, ctx context.Context, q string, args ...any) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func (s *raSuite) seed(t *testing.T, ctx context.Context) {
	t.Helper()
	project := func(slug string, inquiry bool) int64 {
		return s.id(t, ctx, `INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality, ai_classify, ai_inquiry)
		                     VALUES ($1,$1,'itest-caproute-client','manual','dashboard','/tmp/itest-caproute','any',false,$2)
		                     RETURNING id`, slug, inquiry)
	}
	// collab carries ai_inquiry (collaboratory's shape), so B6 can show the
	// inquiry inbox following a route row.
	s.collab = project("itest-caproute-collab", true)
	s.reeng = project("itest-caproute-reeng", false)
	s.other = project("itest-caproute-other", false)

	account := func(email string, armedAgo time.Duration) int64 {
		id := s.id(t, ctx, `INSERT INTO source_accounts (provider, account_email, send_enabled) VALUES ($1,$2,false) RETURNING id`,
			raProvider, email)
		if armedAgo > 0 {
			s.exec(t, ctx, `UPDATE source_accounts SET route_after = now() - make_interval(secs => $2) WHERE id = $1`,
				id, armedAgo.Seconds())
		}
		return id
	}
	cand := func(acct, project int64, isDefault bool) {
		s.exec(t, ctx, `INSERT INTO source_account_projects (source_account_id, project_id, is_default, description)
		                VALUES ($1,$2,$3,$4)`, acct, project, isDefault, fmt.Sprintf("itest-caproute candidate %d", project))
	}
	month := 30 * 24 * time.Hour
	s.hoc = account("itest-caproute-hoc@example.test", month) // handsonconnect's shape: O2/O3
	cand(s.hoc, s.collab, true)
	cand(s.hoc, s.reeng, false)
	s.solo = account("itest-caproute-solo@example.test", month)
	cand(s.solo, s.collab, false)
	s.nodef = account("itest-caproute-nodef@example.test", month)
	cand(s.nodef, s.collab, false)
	cand(s.nodef, s.reeng, false)
	s.unarmed = account("itest-caproute-unarmed@example.test", 0) // route_after NULL: shadow
	cand(s.unarmed, s.collab, true)
	cand(s.unarmed, s.reeng, false)
	s.personal = account("itest-caproute-personal@example.test", month) // NO candidate rows: B-D1's control
	s.late = account("itest-caproute-late@example.test", time.Hour)     // armed an hour ago
	cand(s.late, s.collab, true)
	cand(s.late, s.reeng, false)
}

// msg seeds one message on the named thread (threads are shared by name).
func (s *raSuite) msg(t *testing.T, ctx context.Context, acct int64, thread, direction string, sentAgo time.Duration) (int64, int64) {
	t.Helper()
	s.seq++
	label := fmt.Sprintf("%s-%d", thread, s.seq)
	raw := s.id(t, ctx, `INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
	                     VALUES ($1,$2,'{}',$3,now()) RETURNING id`, acct, "itest-caproute-"+label, "itest-caproute-h-"+label)
	th, ok := s.threads[thread]
	if !ok {
		th = s.id(t, ctx, `INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,$2,'[]') RETURNING id`,
			raThreadPfx+thread, "itest-caproute "+thread)
		s.threads[thread] = th
	}
	m := s.id(t, ctx, `INSERT INTO normalized_messages
	                     (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
	                   VALUES ($1,$2,$3,$4, now() - make_interval(secs => $5), $6, $7, 'Pat Doe <pat@univ.example.test>', 'gmail')
	                   RETURNING id`,
		raw, th, direction, "<itest-caproute-"+label+"@example.test>", sentAgo.Seconds(),
		"itest-caproute body "+label, "itest-caproute subject "+thread)
	return m, raw
}

func (s *raSuite) dec(t *testing.T, ctx context.Context, msg, raw int64, mode, action string, project int64) {
	t.Helper()
	var p any
	if project != 0 {
		p = project
	}
	s.exec(t, ctx, `INSERT INTO capture_decisions (message_id, raw_source_item_id, mode, action, project_id, reason)
	                VALUES ($1,$2,$3,$4,$5,'itest-caproute')`, msg, raw, mode, action, p)
}

// verdict writes one ok classify_route verdict for the message, recorded
// recordedAgo before now; project 0 = the model chose nothing.
func (s *raSuite) verdict(t *testing.T, ctx context.Context, msg, raw, acct, project int64, grounded bool, recordedAgo time.Duration) int64 {
	t.Helper()
	run := s.id(t, ctx, `INSERT INTO ai_runs (worker_type, provider, model, input, output, status, created_at)
	                     VALUES ('classify_route','itest',$1,'{}','{}','ok', now() - make_interval(secs => $2)) RETURNING id`,
		raModel, recordedAgo.Seconds())
	var pid, idx any
	if project != 0 {
		pid, idx = project, 1
	}
	fields, _ := json.Marshal(map[string]any{
		"project_index": idx, "project_id": pid, "grounded": grounded,
		"evidence": "itest-caproute evidence", "reason": "itest-caproute",
		"normalized_message_id": msg, "source_account_id": acct,
	})
	return s.id(t, ctx, `INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields) VALUES ($1,$2,$3) RETURNING id`,
		run, raw, string(fields))
}

type raRoute struct {
	project    int64
	step       string
	extraction int64
	action     string
	rule, task *int64
}

func (s *raSuite) route(t *testing.T, ctx context.Context, msg int64) (raRoute, bool) {
	t.Helper()
	rows, err := s.pool.Query(ctx, `SELECT COALESCE(project_id,0), COALESCE(route_step,''), COALESCE(ai_extraction_id,0),
	                                       action, matched_rule_id, task_id
	                                  FROM capture_decisions WHERE message_id = $1 AND mode = 'route'`, msg)
	if err != nil {
		t.Fatalf("read route rows: %v", err)
	}
	defer rows.Close()
	var out []raRoute
	for rows.Next() {
		var r raRoute
		if err := rows.Scan(&r.project, &r.step, &r.extraction, &r.action, &r.rule, &r.task); err != nil {
			t.Fatalf("scan route row: %v", err)
		}
		out = append(out, r)
	}
	if len(out) > 1 {
		t.Errorf("message %d carries %d route rows; B5: one per message, forever", msg, len(out))
	}
	if len(out) == 0 {
		return raRoute{}, false
	}
	return out[0], true
}

func (s *raSuite) apply(t *testing.T, ctx context.Context) capture.RouteStats {
	t.Helper()
	st, err := capture.RunRouteApply(ctx, s.pool, capture.RouteApplyConfig{Since: raWindow})
	if err != nil {
		t.Fatalf("RunRouteApply: %v", err)
	}
	return st
}

// sideTables counts every table a route pass must NOT touch (B5, invariants 3/4).
func (s *raSuite) sideTables(t *testing.T, ctx context.Context) [4]int {
	t.Helper()
	var c [4]int
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tasks), (SELECT count(*) FROM audit_events),
	                                       (SELECT count(*) FROM deliveries), (SELECT count(*) FROM task_events)`).
		Scan(&c[0], &c[1], &c[2], &c[3]); err != nil {
		t.Fatalf("count side tables: %v", err)
	}
	return c
}

// reportCount finds the report line starting with label and returns its last field.
func reportCount(rep, label string) (int, bool) {
	for _, ln := range strings.Split(rep, "\n") {
		trimmed := strings.TrimSpace(ln)
		if !strings.HasPrefix(trimmed, label) {
			continue
		}
		f := strings.Fields(trimmed)
		if n, err := strconv.Atoi(f[len(f)-1]); err == nil {
			return n, true
		}
	}
	return 0, false
}

func TestCaptureRoute_Integration_EachStepWritesOneRouteRow(t *testing.T) {
	ctx := context.Background()
	s := newRASuite(t, ctx)
	h := time.Hour

	// step 1 — thread: the thread's other message is attributed to ONE candidate.
	a1, a1r := s.msg(t, ctx, s.hoc, "t-one", "inbound", 5*h)
	s.dec(t, ctx, a1, a1r, "live", "attributed", s.reeng)
	a2, a2r := s.msg(t, ctx, s.hoc, "t-one", "inbound", 4*h)
	s.dec(t, ctx, a2, a2r, "live", "unmatched", 0)
	// two-project thread, no verdict: pending (never the default).
	b1, b1r := s.msg(t, ctx, s.hoc, "t-split", "inbound", 6*h)
	s.dec(t, ctx, b1, b1r, "live", "attributed", s.collab)
	b2, b2r := s.msg(t, ctx, s.hoc, "t-split", "inbound", 5*h)
	s.dec(t, ctx, b2, b2r, "live", "attributed", s.reeng)
	b3, b3r := s.msg(t, ctx, s.hoc, "t-split", "inbound", 4*h)
	s.dec(t, ctx, b3, b3r, "live", "unmatched", 0)
	// step 3 — model: a grounded verdict.
	c1, c1r := s.msg(t, ctx, s.hoc, "t-model", "inbound", 4*h)
	s.dec(t, ctx, c1, c1r, "live", "unmatched", 0)
	c1v := s.verdict(t, ctx, c1, c1r, s.hoc, s.reeng, true, 0)
	// step 4 — default: an ungrounded verdict (O3).
	d1, d1r := s.msg(t, ctx, s.hoc, "t-ungrounded", "inbound", 4*h)
	s.dec(t, ctx, d1, d1r, "live", "unmatched", 0)
	s.verdict(t, ctx, d1, d1r, s.hoc, s.reeng, false, 0)
	// no verdict yet: pending_verdict.
	e1, e1r := s.msg(t, ctx, s.hoc, "t-pending", "inbound", 4*h)
	s.dec(t, ctx, e1, e1r, "live", "unmatched", 0)
	// step 2 — single.
	f1, f1r := s.msg(t, ctx, s.solo, "t-single", "inbound", 4*h)
	s.dec(t, ctx, f1, f1r, "live", "unmatched", 0)
	// no default: stays unmatched.
	g1, g1r := s.msg(t, ctx, s.nodef, "t-nodef", "inbound", 4*h)
	s.dec(t, ctx, g1, g1r, "live", "unmatched", 0)
	s.verdict(t, ctx, g1, g1r, s.nodef, s.reeng, false, 0)

	// ---- never routed ----
	// B-D1: an armed account with NO candidate rows. Mutation: drop the
	// candidate-rows clause → this gets a row.
	h1, h1r := s.msg(t, ctx, s.personal, "t-personal", "inbound", 4*h)
	s.dec(t, ctx, h1, h1r, "live", "unmatched", 0)
	s.verdict(t, ctx, h1, h1r, s.personal, s.reeng, true, 0)
	// Latest decision attributed (a later shadow rule re-pointed it). Mutation:
	// drop the latest-is-unmatched clause → this gets a row.
	i1, i1r := s.msg(t, ctx, s.hoc, "t-repointed", "inbound", 4*h)
	s.dec(t, ctx, i1, i1r, "live", "unmatched", 0)
	s.dec(t, ctx, i1, i1r, "shadow", "attributed", s.collab)
	s.verdict(t, ctx, i1, i1r, s.hoc, s.reeng, true, 0)
	// No live decision. Mutation: drop EXISTS live unmatched → this gets a row.
	j1, j1r := s.msg(t, ctx, s.hoc, "t-shadowonly", "inbound", 4*h)
	s.dec(t, ctx, j1, j1r, "shadow", "unmatched", 0)
	s.verdict(t, ctx, j1, j1r, s.hoc, s.reeng, true, 0)
	// Outbound (synthetic live row isolates the clause; invariant 5).
	k1, k1r := s.msg(t, ctx, s.hoc, "t-outbound", "outbound", 4*h)
	s.dec(t, ctx, k1, k1r, "live", "unmatched", 0)
	s.verdict(t, ctx, k1, k1r, s.hoc, s.reeng, true, 0)

	before := s.sideTables(t, ctx)
	st := s.apply(t, ctx)
	after := s.sideTables(t, ctx)

	for name, w := range map[string]struct {
		msg, project int64
		step         string
		ext          int64
	}{
		"thread":  {a2, s.reeng, "thread", 0},
		"model":   {c1, s.reeng, "model", c1v},
		"default": {d1, s.collab, "default", 0},
		"single":  {f1, s.collab, "single", 0},
	} {
		r, ok := s.route(t, ctx, w.msg)
		if !ok {
			t.Errorf("%s: message %d has no mode='route' row", name, w.msg)
			continue
		}
		if r.project != w.project || r.step != w.step || r.extraction != w.ext {
			t.Errorf("%s: route row = %+v, want project %d step %q extraction %d", name, r, w.project, w.step, w.ext)
		}
		if r.action != "attributed" || r.rule != nil || r.task != nil {
			t.Errorf("%s: route row action %q rule %v task %v; B-D5: attributed, no rule, no task", name, r.action, r.rule, r.task)
		}
	}
	for name, m := range map[string]int64{
		"two-project thread, no verdict": b3, "no verdict": e1, "no default": g1,
		"no candidate rows (personal)": h1, "re-pointed (latest attributed)": i1,
		"no live decision": j1, "outbound": k1,
		"the thread's attributed neighbours": a1,
	} {
		if r, ok := s.route(t, ctx, m); ok {
			t.Errorf("%s: message %d was routed (%+v); it must stay as it is", name, m, r)
		}
	}
	_ = b1
	_ = b2

	if st.Written != 4 {
		t.Errorf("RouteStats.Written = %d, want 4 (thread, model, default, single)", st.Written)
	}
	for step, n := range map[string]int{"thread": 1, "model": 1, "default": 1, "single": 1} {
		if st.ByStep[step] != n {
			t.Errorf("RouteStats.ByStep[%q] = %d, want %d (%v)", step, st.ByStep[step], n, st.ByStep)
		}
	}
	if st.Unrouted["pending_verdict"] != 2 || st.Unrouted["no_default"] != 1 {
		t.Errorf("RouteStats.Unrouted = %v, want pending_verdict 2 (split thread, no verdict) and no_default 1", st.Unrouted)
	}
	if after != before {
		t.Errorf("tasks/audit_events/deliveries/task_events went %v → %v. B5: a route pass creates no task and makes NO "+
			"executor call (no audit row); invariant 4: nothing sends", before, after)
	}

	// B9: rulesreport gives route rows their own lines.
	rep, err := capture.Report(ctx, s.pool, time.Now().Add(-24*time.Hour), "")
	if err != nil {
		t.Fatalf("capture.Report: %v", err)
	}
	if !strings.Contains(rep, "\nROUTE") && !strings.HasPrefix(rep, "ROUTE") {
		t.Errorf("the capture rules report has no ROUTE section (B9):\n%s", rep)
	}
	for _, step := range []string{"thread", "single", "model", "default"} {
		if n, ok := reportCount(rep, "route "+step); !ok || n != 1 {
			t.Errorf("report line `route %s` = %d (found %v), want 1:\n%s", step, n, ok, rep)
		}
	}
}

func TestCaptureRoute_Integration_RunTwiceWritesNothing(t *testing.T) {
	ctx := context.Background()
	s := newRASuite(t, ctx)
	a1, a1r := s.msg(t, ctx, s.hoc, "t-twice", "inbound", 3*time.Hour)
	s.dec(t, ctx, a1, a1r, "live", "attributed", s.reeng)
	a2, a2r := s.msg(t, ctx, s.hoc, "t-twice", "inbound", 2*time.Hour)
	s.dec(t, ctx, a2, a2r, "live", "unmatched", 0)
	c1, c1r := s.msg(t, ctx, s.hoc, "t-twice-model", "inbound", 2*time.Hour)
	s.dec(t, ctx, c1, c1r, "live", "unmatched", 0)
	s.verdict(t, ctx, c1, c1r, s.hoc, s.reeng, true, 0)

	if st := s.apply(t, ctx); st.Written != 2 {
		t.Fatalf("first pass Written = %d, want 2", st.Written)
	}
	if st := s.apply(t, ctx); st.Written != 0 {
		t.Errorf("second pass Written = %d, want 0 (B5: run-twice writes nothing)", st.Written)
	}
	for _, m := range []int64{a2, c1} {
		if n := s.count(t, ctx, `SELECT count(*) FROM capture_decisions WHERE message_id=$1 AND mode='route'`, m); n != 1 {
			t.Errorf("message %d has %d route rows after two passes, want 1", m, n)
		}
	}
}

// B7, first half: route_after NULL → NOTHING is written, steps 1-2 included.
// Mutation: drop `route_after IS NOT NULL` from the apply inbox → the first
// pass writes both rows.
func TestCaptureRoute_Integration_ArmingIsTheColumn(t *testing.T) {
	ctx := context.Background()
	s := newRASuite(t, ctx)
	a1, a1r := s.msg(t, ctx, s.unarmed, "t-unarmed-thread", "inbound", 3*time.Hour)
	s.dec(t, ctx, a1, a1r, "live", "attributed", s.reeng)
	a2, a2r := s.msg(t, ctx, s.unarmed, "t-unarmed-thread", "inbound", 2*time.Hour)
	s.dec(t, ctx, a2, a2r, "live", "unmatched", 0)
	c1, c1r := s.msg(t, ctx, s.unarmed, "t-unarmed-model", "inbound", 2*time.Hour)
	s.dec(t, ctx, c1, c1r, "live", "unmatched", 0)
	s.verdict(t, ctx, c1, c1r, s.unarmed, s.reeng, true, 0)

	if st := s.apply(t, ctx); st.Written != 0 {
		t.Errorf("an unarmed account's pass wrote %d route row(s); B7: route_after NULL → nothing is written", st.Written)
	}
	for _, m := range []int64{a2, c1} {
		if r, ok := s.route(t, ctx, m); ok {
			t.Errorf("message %d on an unarmed account was routed: %+v", m, r)
		}
	}

	// The verdict above is recorded now; arming a day in the past puts it after
	// the cutover.
	s.exec(t, ctx, `UPDATE source_accounts SET route_after = now() - interval '1 day' WHERE id = $1`, s.unarmed)
	st := s.apply(t, ctx)
	if st.Written != 2 {
		t.Errorf("after arming, Written = %d, want 2 (the thread and the model row)", st.Written)
	}
	if r, ok := s.route(t, ctx, a2); !ok || r.step != "thread" {
		t.Errorf("after arming, the thread message = %+v (found %v), want a thread row", r, ok)
	}
	if r, ok := s.route(t, ctx, c1); !ok || r.step != "model" {
		t.Errorf("after arming, the model message = %+v (found %v), want a model row", r, ok)
	}
}

// B7, second half: a step-3 verdict recorded before route_after is not applied
// (and, the conservative reading, not defaulted); steps 1-2 apply inside the
// pass window whenever the message was sent. Mutation: compare the verdict's
// ai_runs.created_at against nothing → m1 gets a model row.
func TestCaptureRoute_Integration_AVerdictBeforeArmingIsNotApplied(t *testing.T) {
	ctx := context.Background()
	s := newRASuite(t, ctx) // s.late was armed an hour ago
	h := time.Hour
	m1, m1r := s.msg(t, ctx, s.late, "t-late-before", "inbound", 3*h)
	s.dec(t, ctx, m1, m1r, "live", "unmatched", 0)
	s.verdict(t, ctx, m1, m1r, s.late, s.reeng, true, 2*h)
	m4, m4r := s.msg(t, ctx, s.late, "t-late-before-ungrounded", "inbound", 3*h)
	s.dec(t, ctx, m4, m4r, "live", "unmatched", 0)
	s.verdict(t, ctx, m4, m4r, s.late, s.reeng, false, 2*h)
	m2, m2r := s.msg(t, ctx, s.late, "t-late-after", "inbound", 3*h)
	s.dec(t, ctx, m2, m2r, "live", "unmatched", 0)
	s.verdict(t, ctx, m2, m2r, s.late, s.reeng, true, 0)
	n1, n1r := s.msg(t, ctx, s.late, "t-late-thread", "inbound", 80*h)
	s.dec(t, ctx, n1, n1r, "live", "attributed", s.reeng)
	m3, m3r := s.msg(t, ctx, s.late, "t-late-thread", "inbound", 72*h) // sent three days before arming
	s.dec(t, ctx, m3, m3r, "live", "unmatched", 0)

	st := s.apply(t, ctx)
	for _, m := range []int64{m1, m4} {
		if r, ok := s.route(t, ctx, m); ok {
			t.Errorf("message %d's verdict predates route_after and was applied: %+v. B7: forward-only on the verdict clock", m, r)
		}
	}
	if st.Unrouted["verdict_before_arming"] != 2 {
		t.Errorf("Unrouted = %v, want verdict_before_arming 2", st.Unrouted)
	}
	if r, ok := s.route(t, ctx, m2); !ok || r.step != "model" || r.project != s.reeng {
		t.Errorf("the verdict recorded after arming = %+v (found %v), want a model row for reengine — the positive control", r, ok)
	}
	if r, ok := s.route(t, ctx, m3); !ok || r.step != "thread" || r.project != s.reeng {
		t.Errorf("a message sent before arming on a one-project thread = %+v (found %v), want a thread row: steps 1-2 "+
			"apply inside the pass window (B-D7)", r, ok)
	}
}

// B6 / D-D2: a later shadow --all capture pass writes nothing for a routed
// message. Mutation: drop 'route' from pendingMessages' exclusion → the routed
// message gets a newer shadow row and the route stops being its latest decision.
func TestCaptureRoute_Integration_ALaterShadowAllPassWritesNothingForARoutedMessage(t *testing.T) {
	ctx := context.Background()
	s := newRASuite(t, ctx)
	a1, a1r := s.msg(t, ctx, s.hoc, "t-shadowall", "inbound", 3*time.Hour)
	s.dec(t, ctx, a1, a1r, "live", "attributed", s.reeng)
	routed, routedRaw := s.msg(t, ctx, s.hoc, "t-shadowall", "inbound", 2*time.Hour)
	s.dec(t, ctx, routed, routedRaw, "live", "unmatched", 0)
	control, controlRaw := s.msg(t, ctx, s.hoc, "t-shadowall-control", "inbound", 2*time.Hour)
	s.dec(t, ctx, control, controlRaw, "live", "unmatched", 0) // no verdict: stays pending, unrouted

	s.apply(t, ctx)
	if _, ok := s.route(t, ctx, routed); !ok {
		t.Fatalf("fixture: the thread message was not routed; the rest of this test needs a route row")
	}
	rowsRouted := s.count(t, ctx, `SELECT count(*) FROM capture_decisions WHERE message_id=$1`, routed)
	rowsControl := s.count(t, ctx, `SELECT count(*) FROM capture_decisions WHERE message_id=$1`, control)

	reg := executor.NewRegistry()
	tools.Register(reg, s.pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(s.pool), policy.NewStatic(reg.Names()...))
	ex := executor.New(reg, checker, audit.NewPGStore(s.pool))
	if _, err := capture.EvaluateRules(ctx, s.pool, ex, capture.RulesConfig{Mode: "shadow", All: true, Horizon: raWindow}); err != nil {
		t.Fatalf("shadow --all capture pass: %v", err)
	}

	if n := s.count(t, ctx, `SELECT count(*) FROM capture_decisions WHERE message_id=$1`, routed); n != rowsRouted {
		t.Errorf("the shadow --all pass wrote %d new row(s) for a routed message; D-D2/B6: pendingMessages excludes "+
			"route rows in every mode, or the route is buried for every latest-decision reader", n-rowsRouted)
	}
	var latestMode string
	if err := s.pool.QueryRow(ctx, `SELECT mode FROM capture_decisions WHERE message_id=$1 ORDER BY id DESC LIMIT 1`,
		routed).Scan(&latestMode); err != nil {
		t.Fatalf("read latest: %v", err)
	}
	if latestMode != "route" {
		t.Errorf("the routed message's latest decision is mode %q, want route", latestMode)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM capture_decisions WHERE message_id=$1`, control); n != rowsControl+1 {
		t.Errorf("POSITIVE CONTROL: the unrouted message got %d new shadow row(s), want 1 — without it this test "+
			"passes on a capture pass that never saw these messages", n-rowsControl)
	}
}

// B6: after a route row the inquiry inbox sees the message; the residue and
// triage inboxes do not. (The inquiry PROMOTION inbox is
// internal/promote's TestPromoteInquiry_Integration_RouteAttributionIsFollowed.)
func TestCaptureRoute_Integration_TheInboxesFollowTheRouteRow(t *testing.T) {
	ctx := context.Background()
	s := newRASuite(t, ctx)
	routed, routedRaw := s.msg(t, ctx, s.hoc, "t-inboxes", "inbound", 2*time.Hour)
	s.dec(t, ctx, routed, routedRaw, "live", "unmatched", 0)
	s.verdict(t, ctx, routed, routedRaw, s.hoc, s.reeng, false, 0) // ungrounded → default collab (ai_inquiry)
	control, controlRaw := s.msg(t, ctx, s.hoc, "t-inboxes-control", "inbound", 2*time.Hour)
	s.dec(t, ctx, control, controlRaw, "live", "unmatched", 0)

	s.apply(t, ctx)
	if r, ok := s.route(t, ctx, routed); !ok || r.project != s.collab {
		t.Fatalf("fixture: routed = %+v (found %v), want a default row for collab", r, ok)
	}

	ids := func(lane classify.Lane) map[int64]bool {
		got, err := classify.NewStore(s.pool).PendingMessages(ctx, classify.Config{Lane: lane, Since: raWindow})
		if err != nil {
			t.Fatalf("%s inbox: %v", lane.Name, err)
		}
		out := map[int64]bool{}
		for _, m := range got {
			out[m.MessageID] = true
		}
		return out
	}
	inquiry, residue := ids(classify.LaneInquiry), ids(classify.LaneResidue)
	if !inquiry[routed] {
		t.Errorf("the inquiry inbox does not see the routed message; B6/C2: the latest attributed decision in ANY mode, route included")
	}
	if residue[routed] {
		t.Errorf("the residue inbox still sees the routed message; its latest decision is no longer unmatched")
	}
	if !residue[control] {
		t.Errorf("POSITIVE CONTROL: the residue inbox does not see the unrouted message")
	}
	tri, err := triage.NewStore(s.pool).PendingMessages(ctx, triage.Config{Since: raWindow})
	if err != nil {
		t.Fatalf("triage inbox: %v", err)
	}
	seen := map[int64]bool{}
	for _, m := range tri {
		seen[m.MessageID] = true
	}
	if seen[routed] {
		t.Errorf("the triage inbox still sees the routed message (B6)")
	}
	if !seen[control] {
		t.Errorf("POSITIVE CONTROL: the triage inbox does not see the unrouted message")
	}
}

// E-D4 / B-D6: route_apply takes capture's lock 0x5157_0015; held elsewhere →
// ErrRouteLockHeld and nothing written.
func TestCaptureRoute_Integration_TheCaptureLockSerializesIt(t *testing.T) {
	ctx := context.Background()
	s := newRASuite(t, ctx)
	f1, f1r := s.msg(t, ctx, s.solo, "t-lock", "inbound", 2*time.Hour)
	s.dec(t, ctx, f1, f1r, "live", "unmatched", 0)

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, raLockKey); err != nil {
		t.Fatalf("take the capture lock: %v", err)
	}
	_, err = capture.RunRouteApply(ctx, s.pool, capture.RouteApplyConfig{Since: raWindow})
	if !errors.Is(err, capture.ErrRouteLockHeld) {
		t.Errorf("RunRouteApply with 0x5157_0015 held elsewhere = %v, want ErrRouteLockHeld", err)
	}
	if _, ok := s.route(t, ctx, f1); ok {
		t.Errorf("a route row was written while capture's lock was held elsewhere")
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, raLockKey); err != nil {
		t.Fatalf("release: %v", err)
	}
	if st := s.apply(t, ctx); st.Written != 1 {
		t.Errorf("after the lock was released, Written = %d, want 1", st.Written)
	}
}

// The migration's LIVE shape: every CHECK and both partial indexes bite.
func TestCaptureRoute_Integration_TheSchemaPinsTheRouteShape(t *testing.T) {
	ctx := context.Background()
	s := newRASuite(t, ctx)
	m, mr := s.msg(t, ctx, s.hoc, "t-schema", "inbound", time.Hour)
	s.dec(t, ctx, m, mr, "live", "unmatched", 0)
	ext := s.verdict(t, ctx, m, mr, s.hoc, s.reeng, true, 0)
	other, _ := s.msg(t, ctx, s.hoc, "t-schema-other", "inbound", time.Hour)
	rule := s.id(t, ctx, `INSERT INTO capture_rules (project_id, criteria_type, pattern, priority, enabled, note)
	                      VALUES ($1,'sender','itest-caproute-nomatch@nowhere.example.test',1,false,'itest-caproute') RETURNING id`, s.collab)

	for name, stmt := range map[string]struct {
		q    string
		args []any
	}{
		"a route row with no step": {`INSERT INTO capture_decisions (message_id, mode, action, project_id) VALUES ($1,'route','attributed',$2)`,
			[]any{m, s.collab}},
		"an unknown step": {`INSERT INTO capture_decisions (message_id, mode, action, project_id, route_step) VALUES ($1,'route','attributed',$2,'bogus')`,
			[]any{m, s.collab}},
		"a model row with no extraction": {`INSERT INTO capture_decisions (message_id, mode, action, project_id, route_step) VALUES ($1,'route','attributed',$2,'model')`,
			[]any{m, s.collab}},
		"a default row naming an extraction": {`INSERT INTO capture_decisions (message_id, mode, action, project_id, route_step, ai_extraction_id) VALUES ($1,'route','attributed',$2,'default',$3)`,
			[]any{m, s.collab, ext}},
		"a route row that is not attributed": {`INSERT INTO capture_decisions (message_id, mode, action, project_id, route_step) VALUES ($1,'route','unmatched',NULL,'default')`,
			[]any{m}},
		"a route row naming a rule": {`INSERT INTO capture_decisions (message_id, mode, action, project_id, route_step, matched_rule_id) VALUES ($1,'route','attributed',$2,'default',$3)`,
			[]any{m, s.collab, rule}},
		"a step on a non-route row": {`INSERT INTO capture_decisions (message_id, mode, action, project_id, route_step) VALUES ($1,'shadow','unmatched',NULL,'thread')`,
			[]any{other}},
		"a second default for one account": {`INSERT INTO source_account_projects (source_account_id, project_id, is_default, description) VALUES ($1,$2,true,'x')`,
			[]any{s.hoc, s.other}},
		"a project listed twice for one account": {`INSERT INTO source_account_projects (source_account_id, project_id, description) VALUES ($1,$2,'x')`,
			[]any{s.hoc, s.reeng}},
	} {
		if _, err := s.pool.Exec(ctx, stmt.q, stmt.args...); err == nil {
			t.Errorf("%s was accepted; 0032's CHECKs/indexes must refuse it", name)
		}
	}

	s.exec(t, ctx, `INSERT INTO capture_decisions (message_id, mode, action, project_id, route_step, ai_extraction_id)
	                VALUES ($1,'route','attributed',$2,'model',$3)`, m, s.reeng, ext)
	if _, err := s.pool.Exec(ctx, `INSERT INTO capture_decisions (message_id, mode, action, project_id, route_step)
	                               VALUES ($1,'route','attributed',$2,'default')`, m, s.collab); err == nil {
		t.Errorf("a second route row for one message was accepted; capture_decisions_route_uniq is one per message, forever")
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO capture_decisions (message_id, mode, action, project_id, route_step)
	                              VALUES ($1,'route','attributed',$2,'default')
	                              ON CONFLICT (message_id) WHERE mode = 'route' DO NOTHING`, m, s.collab)
	if err != nil {
		t.Errorf("ON CONFLICT (message_id) WHERE mode = 'route' DO NOTHING errored (%v); the restated predicate must "+
			"infer the partial index", err)
	} else if tag.RowsAffected() != 0 {
		t.Errorf("the conflicting insert wrote %d row(s)", tag.RowsAffected())
	}
}

// SPEC amendment (2026-09-13), end to end at the capture level: the V6.5
// backfill re-classifies a message whose only verdict predates arming, and
// route_apply then applies the FRESH verdict — never the stale one (B7 stands).
// Mutations: apply the newest verdict regardless of route_after → the first
// pass writes a row; apply the OLDEST verdict → the second pass routes to the
// stale verdict's project with its extraction id.
func TestCaptureRoute_Integration_AFreshVerdictAfterArmingIsApplied(t *testing.T) {
	ctx := context.Background()
	s := newRASuite(t, ctx) // s.late was armed an hour ago
	m, mr := s.msg(t, ctx, s.late, "t-late-refresh", "inbound", 3*time.Hour)
	s.dec(t, ctx, m, mr, "live", "unmatched", 0)
	stale := s.verdict(t, ctx, m, mr, s.late, s.collab, true, 2*time.Hour) // grounded, but pre-arming

	st := s.apply(t, ctx)
	if r, ok := s.route(t, ctx, m); ok {
		t.Fatalf("a pre-arming verdict was applied: %+v (B7: never)", r)
	}
	if st.Unrouted["verdict_before_arming"] != 1 {
		t.Errorf("first pass Unrouted = %v, want verdict_before_arming 1", st.Unrouted)
	}

	fresh := s.verdict(t, ctx, m, mr, s.late, s.reeng, true, 0) // the backfill's post-arming verdict
	st = s.apply(t, ctx)
	r, ok := s.route(t, ctx, m)
	if !ok {
		t.Fatalf("after a fresh post-arming verdict, route_apply wrote no row (Written %d, Unrouted %v)", st.Written, st.Unrouted)
	}
	if r.step != "model" || r.project != s.reeng || r.extraction != fresh {
		t.Errorf("route row = %+v, want step model, project %d (the fresh verdict's) and ai_extraction_id %d (the NEW "+
			"extraction; the stale one is %d)", r, s.reeng, fresh, stale)
	}
	if st.Written != 1 {
		t.Errorf("second pass Written = %d, want 1", st.Written)
	}
}
