//go:build integration

package dashboard_test

// demo-mode (SWT-99, docs/tickets/demo-mode_SPEC.md) against a REAL database and
// the REAL dashboard.Server (dev-login auth): criteria 2 (DB half), 3, 9-31, with
// the leak crawl (25), its demo-off CONTROL (26) and the banned words (27).
// Build-tagged `integration` AND env-gated on DATABASE_URL. NO LLM, NO network,
// NO broker. Every server here runs a RECORDING fake executor, so a verb that
// slips past the pre-check is counted, never executed.
//
// Run it in the branch-owned database (the IK compose landmine):
//
//	psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c 'CREATE DATABASE ops_demo_mode'
//	make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_demo_mode?sslmode=disable'
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_demo_mode?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run 'TestDemo' ./internal/dashboard/
//
// Reuses dashGuard / dashPool / get / snippet (dashboard_integration_test.go),
// rowOf (deliveries_integration_test.go), boardRow (board_lights_integration_test.go),
// layoutSections (board_layout_integration_test.go), countTracer
// (board_refresh_integration_test.go), pinnedHeader (export_test.go), and the
// test-only export DemoBareHandlers (demo_test.go).
//
// FIXTURE (scoped to the prefix itest-dm-; cleanup before AND in t.Cleanup, FK
// order, and the demo_mode ops_flags row deleted both times). The allowlist in
// the flag is the TEST's own: project itest-dm-vis, accounts ITEST-DM-VIS@local.test
// (mixed case on purpose, D1) and tdmvis1@slack-web.local. Every hidden row
// carries the sentinel ZZHIDDEN (matched case-insensitively). No visible string
// contains "demo" or "hidden", so criterion 27's word check means something.
//
// Two SPEC readings this file makes explicit:
//   - /tasks?project=<hidden slug> echoes the slug the viewer typed (the filter
//     box, the sign header, the kiosk link). The crawl removes that echo before
//     scanning that one URL; criterion 9's "renders exactly like a nonexistent
//     slug" is asserted separately with both slugs masked.
//   - "Location headers match byte for byte": the refusal flash names the
//     posted id ("task #N not found"), so the hidden and the nonexistent
//     Location are compared with that one id masked to N.
//
// GREENFIELD NOTE — EXPECTED RED. Today the package's test binary does not
// compile (demo_test.go's undefined symbols, incl. DemoBareHandlers' siblings).
// Once it does and before migration 0048 is applied, every test that marks a
// task fails at `column "demo_hidden" does not exist`; the rest fail where they
// assert (hidden rows render with demo on, verbs reach the executor, ...).
//
// MUTATIONS THAT MUST TURN THIS FILE RED: M1, M2, M4-M10, M12-M14 (see each
// test's comment), and the /tasks?project filter leaking through the <select>.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/dashboard"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/orchestrator"
)

const (
	dmVisSlug  = "itest-dm-vis"
	dmHidSlug  = "itest-dm-ZZHIDDEN"
	dmNoSlug   = "itest-dm-nonexistent"
	dmVisAcct  = "itest-dm-vis@local.test"
	dmHidAcct  = "itest-dm-ZZHIDDEN@local.test"
	dmCrmAcct  = "itest-dm-upworkcrm@local.test"
	dmVisWS    = "TDMVIS1"
	dmHidWS    = "TZZHIDDEN1"
	dmVisSlack = "tdmvis1@slack-web.local"
	dmHidSlack = "tzzhidden1@slack-web.local"
	dmKeyPfx   = "itest-dm:"
	dmMissing  = int64(999999999)

	dmOn  = `{"on":true,"projects":["itest-dm-vis"],"source_accounts":["ITEST-DM-VIS@local.test","tdmvis1@slack-web.local"]}`
	dmOff = `{"on":false,"projects":["itest-dm-vis"],"source_accounts":["ITEST-DM-VIS@local.test"]}`
)

var dmAllAccts = []string{dmVisAcct, dmHidAcct, dmCrmAcct, dmVisSlack, dmHidSlack}

// dmAllowLower is the flag's account list, lower-cased (D1).
var dmAllowLower = []string{strings.ToLower(dmVisAcct), dmVisSlack}

// ---- cleanup ------------------------------------------------------------------

func dmCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const projs = `(SELECT id FROM projects WHERE slug IN ('` + dmVisSlug + `','` + dmHidSlug + `'))`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	accts := `(SELECT id FROM source_accounts WHERE account_email IN ('` + strings.Join(dmAllAccts, "','") + `'))`
	rawOf := `(SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`
	for _, q := range []string{
		`DELETE FROM ops_flags WHERE name = 'demo_mode'`,
		`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + `)`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM approvals WHERE subject_type = 'delivery' AND subject_id IN (SELECT id FROM deliveries WHERE task_id IN ` + tasksOf + `)`,
		`DELETE FROM deliveries WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM feedback_requests WHERE task_id IN ` + tasksOf,
		`DELETE FROM external_refs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_dependencies WHERE task_id IN ` + tasksOf + ` OR depends_on_task_id IN ` + tasksOf,
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM capture_decisions WHERE message_id IN (SELECT id FROM normalized_messages WHERE raw_source_item_id IN ` + rawOf + `)`,
		`UPDATE tasks SET parent_id = NULL WHERE id IN ` + tasksOf + ` AND parent_id IS NOT NULL`,
		`DELETE FROM tasks WHERE id IN ` + tasksOf,
		`DELETE FROM plan_imports WHERE project_id IN ` + projs,
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN ` + rawOf,
		`DELETE FROM ai_runs WHERE input->>'itest' = 'dm'`,
		`DELETE FROM capture_rules WHERE project_id IN ` + projs,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + rawOf,
		`DELETE FROM normalized_threads WHERE thread_key LIKE '` + dmKeyPfx + `%'`,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM slack_watch WHERE workspace_id IN ('` + dmVisWS + `','` + dmHidWS + `')`,
		`DELETE FROM projects WHERE slug IN ('` + dmVisSlug + `','` + dmHidSlug + `')`,
		`DELETE FROM source_accounts WHERE account_email IN ('` + strings.Join(dmAllAccts, "','") + `')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

// dmStart is every test's opening: guard, pool, cleanup before and after.
func dmStart(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	dmCleanup(t, ctx, pool)
	t.Cleanup(func() {
		dmCleanup(t, ctx, pool)
		pool.Close()
	})
	return ctx, pool
}

// ---- seed ---------------------------------------------------------------------

type dmSeed struct {
	visProj, hidProj                      int64
	visAcct, hidAcct                      int64
	tv, th, tr                            int64 // threads: visible source, hidden source, route
	mV1, mH1, mH2, mHR, mU                int64
	v, vk, vc, vs, va, vr, vdt, vb        int64 // visible project
	d, c, g                               int64 // visible project, the demo_hidden chain
	hr, hi, hn, hf, hdt, hcl, hp, hc      int64 // hidden project
	dV, dU, dVR, dD, dC, dG, dH, dHI, dHN int64 // deliveries
	pv, ph                                int64 // plan imports
	visTitles                             map[int64]string
}

// hiddenTasks is every task id that must never appear with demo on.
func (s dmSeed) hiddenTasks() []int64 {
	return []int64{s.hr, s.hi, s.hn, s.hf, s.hdt, s.hcl, s.hp, s.hc, s.d, s.c, s.g, s.vb}
}

func (s dmSeed) hiddenDeliveries() []int64 { return []int64{s.dH, s.dU, s.dD, s.dC, s.dG} }

// boardVisible is what the default /tasks shows with demo on.
func (s dmSeed) boardVisible() []int64 { return []int64{s.v, s.vk, s.vc, s.vs, s.va, s.vr, s.vdt} }

func dmID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, q string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
		t.Fatalf("seed %s: %v", demoOneLineSQL(q), err)
	}
	return id
}

func dmExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, q string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, q, args...); err != nil {
		t.Fatalf("%s: %v", demoOneLineSQL(q), err)
	}
}

func demoOneLineSQL(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}

func dmSeedAll(t *testing.T, ctx context.Context, pool *pgxpool.Pool) dmSeed {
	t.Helper()
	var s dmSeed
	id := func(q string, args ...any) int64 { t.Helper(); return dmID(t, ctx, pool, q, args...) }
	ex := func(q string, args ...any) { t.Helper(); dmExec(t, ctx, pool, q, args...) }

	s.visProj = id(`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		VALUES ($1,$1,'itest-dm-client-vis','manual','dashboard','/tmp/itest','any') RETURNING id`, dmVisSlug)
	s.hidProj = id(`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		VALUES ($1,$1,'itest-dm-client-ZZHIDDEN','manual','dashboard','/tmp/itest','any') RETURNING id`, dmHidSlug)

	s.visAcct = id(`INSERT INTO source_accounts (provider, account_email) VALUES ('google',$1) RETURNING id`, dmVisAcct)
	s.hidAcct = id(`INSERT INTO source_accounts (provider, account_email) VALUES ('google',$1) RETURNING id`, dmHidAcct)
	crmAcct := id(`INSERT INTO source_accounts (provider, account_email) VALUES ('upwork_crm',$1) RETURNING id`, dmCrmAcct)
	id(`INSERT INTO source_accounts (provider, account_email) VALUES ('slack_web',$1) RETURNING id`, dmVisSlack)
	id(`INSERT INTO source_accounts (provider, account_email) VALUES ('slack_web',$1) RETURNING id`, dmHidSlack)
	for _, a := range []int64{s.visAcct, s.hidAcct, crmAcct} {
		ex(`INSERT INTO sync_runs (source_account_id, started_at, finished_at, status, stats)
			VALUES ($1, now() - interval '2 hours', now() - interval '1 hour', 'ok', '{}')`, a)
	}
	ex(`INSERT INTO slack_watch (workspace_id, conversation_id, label) VALUES ($1,'CDMVIS1','DMVIS watch')`, dmVisWS)
	ex(`INSERT INTO slack_watch (workspace_id, conversation_id, label) VALUES ($1,'CZZHIDDEN1','ZZHIDDEN watch')`, dmHidWS)

	// Raw items two days back, so the funnel's day buckets never straddle the
	// UTC/Eastern midnight (the SWT-48 flake class).
	raw := func(acct int64, ext string, normalized bool) int64 {
		t.Helper()
		na := "now() - interval '2 days'"
		if !normalized {
			na = "NULL"
		}
		return id(`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, ingested_at, normalized_at)
			VALUES ($1,$2,'{}',$2, now() - interval '2 days', `+na+`) RETURNING id`, acct, dmKeyPfx+ext)
	}
	thread := func(key, subject string) int64 {
		t.Helper()
		return id(`INSERT INTO normalized_threads (thread_key, subject) VALUES ($1,$2) RETURNING id`, dmKeyPfx+key, subject)
	}
	msg := func(rawID, threadID int64, channel, sender, subject, body string) int64 {
		t.Helper()
		var th any
		if threadID != 0 {
			th = threadID
		}
		return id(`INSERT INTO normalized_messages
			(raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel, created_at)
			VALUES ($1,$2,'inbound',$3, now() - interval '2 days', $4,$5,$6,$7, now() - interval '2 days') RETURNING id`,
			rawID, th, fmt.Sprintf("<itest-dm-%d@local.test>", rawID), body, subject, sender, channel)
	}
	s.tv = thread("tv", "DMVIS thread")
	s.th = thread("th", "ZZHIDDEN thread")
	s.tr = thread("tr", "DMVIS route thread")
	s.mV1 = msg(raw(s.visAcct, "v1", true), s.tv, "gmail", "client@itest-dm.example", "DMVIS subject", "DMVIS source body")
	s.mU = msg(raw(s.visAcct, "vu", true), 0, "upwork", "ZZHIDDEN upwork sender", "ZZHIDDEN upwork subject", "ZZHIDDEN upwork body")
	raw(s.visAcct, "vpending", false)
	s.mH1 = msg(raw(s.hidAcct, "h1", true), s.th, "gmail", "ZZHIDDEN sender <zz@itest-dm.example>", "ZZHIDDEN subject", "ZZHIDDEN body")
	s.mH2 = msg(raw(s.hidAcct, "h2", true), 0, "gmail", "ZZHIDDEN activity sender", "ZZHIDDEN activity subject", "ZZHIDDEN activity")
	s.mHR = msg(raw(s.hidAcct, "hr", true), s.tr, "gmail", "route-client@itest-dm.example", "DMVIS route subject", "route body")
	raw(s.hidAcct, "hpending", false)

	task := func(proj int64, parent *int64, title, assignee, status string, prio int) int64 {
		t.Helper()
		var p any
		if parent != nil {
			p = *parent
		}
		sub := any(nil)
		body := "DMVIS body"
		if proj == s.hidProj || strings.Contains(title, "ZZHIDDEN") {
			sub, body = "ZZHIDDEN-sub", "ZZHIDDEN body of "+title
		}
		closed := "NULL"
		if status == "closed" {
			closed = "now()"
		}
		return id(`INSERT INTO tasks (project_id, parent_id, subproject, title, body, assignee_type, status, priority, closed_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,`+closed+`) RETURNING id`, proj, p, sub, title, body, assignee, status, prio)
	}

	// Visible project.
	s.v = task(s.visProj, nil, "DMVIS main task", "human", "ready", 1)
	ex(`UPDATE tasks SET source_thread_id = $2 WHERE id = $1`, s.v, s.tv)
	s.vk = task(s.visProj, &s.v, "DMVIS kid", "human", "ready", 0)
	s.vdt = task(s.visProj, nil, "DMVIS done today", "human", "closed", 0)
	s.d = task(s.visProj, &s.v, "ZZHIDDEN flagged D", "human", "ready", 3)
	s.c = task(s.visProj, &s.d, "ZZHIDDEN child C", "human", "ready", 0)
	s.g = task(s.visProj, &s.c, "ZZHIDDEN grandchild G", "human", "in_progress", 0)
	s.vs = task(s.visProj, nil, "DMVIS foreign-source task", "human", "ready", 0)
	ex(`UPDATE tasks SET source_thread_id = $2 WHERE id = $1`, s.vs, s.th)
	s.va = task(s.visProj, nil, "DMVIS activity task", "human", "ready", 0)
	ex(`UPDATE tasks SET activity_at = now(), activity_by_message_id = $2 WHERE id = $1`, s.va, s.mH2)
	s.vr = task(s.visProj, nil, "DMVIS route task", "human", "done_locally", 0)
	s.vb = id(`INSERT INTO tasks (project_id, title, body, assignee_type, status)
		VALUES ($1,'Morning brief 2026-09-29','- ZZHIDDEN: 3 open\n- itest-dm-vis: 2 open','human','ready') RETURNING id`, s.visProj)

	// Hidden project.
	s.hr = task(s.hidProj, nil, "ZZHIDDEN ready", "claude", "ready", 2)
	s.hi = task(s.hidProj, nil, "ZZHIDDEN in flight", "claude", "in_progress", 0)
	ex(`UPDATE tasks SET working_state = 'working', working_state_at = now(), working_session = 'ZZHIDDEN-session' WHERE id = $1`, s.hi)
	s.hn = task(s.hidProj, nil, "ZZHIDDEN needs input", "human", "ready", 0)
	ex(`UPDATE tasks SET working_state = 'needs_input', working_state_at = now() WHERE id = $1`, s.hn)
	s.hf = task(s.hidProj, nil, "ZZHIDDEN needs feedback", "claude", "needs_feedback", 0)
	ex(`INSERT INTO feedback_requests (task_id, question) VALUES ($1,'ZZHIDDEN question')`, s.hf)
	s.hdt = task(s.hidProj, nil, "ZZHIDDEN done today", "human", "closed", 0)
	s.hcl = id(`INSERT INTO tasks (project_id, title, body, assignee_type, status, closed_at)
		VALUES ($1,'ZZHIDDEN closed long ago','ZZHIDDEN','human','closed', now() - interval '3 days') RETURNING id`, s.hidProj)
	s.hp = task(s.hidProj, nil, "ZZHIDDEN parent", "human", "ready", 0)
	s.hc = task(s.hidProj, &s.v, "ZZHIDDEN child of visible", "human", "ready", 0)
	// A visible-project task whose parent is a hidden-project task (criterion 15's ParentLink).
	s.vc = task(s.visProj, &s.hp, "DMVIS child of foreign parent", "human", "ready", 0)

	// Dependencies: V depends on a hidden-project task, on D, and on a visible one.
	for _, dep := range []int64{s.hr, s.d, s.vdt} {
		ex(`INSERT INTO task_dependencies (task_id, depends_on_task_id) VALUES ($1,$2)`, s.v, dep)
	}

	// Events: one visible, one on each of D, C, G, one hidden-project.
	ex(`INSERT INTO task_events (task_id, event_type, payload) VALUES ($1,'log','{"note":"DMVIS-EVENT"}')`, s.v)
	ex(`INSERT INTO feedback_requests (task_id, question) VALUES ($1,'DMVIS question')`, s.v)
	for _, tk := range []int64{s.d, s.c, s.g, s.hr} {
		ex(`INSERT INTO task_events (task_id, event_type, payload) VALUES ($1,'log','{"note":"ZZHIDDEN event"}')`, tk)
	}

	// Refs.
	ex(`INSERT INTO external_refs (task_id, system, external_key, external_url) VALUES ($1,'jira','DMVIS-1','https://itest-dm.example/DMVIS-1')`, s.v)
	ex(`INSERT INTO external_refs (task_id, system, external_key) VALUES ($1,'upwork_crm','itest-dm-crm-ZZHIDDEN')`, s.v)
	ex(`INSERT INTO external_refs (task_id, system, external_key) VALUES ($1,'jira','ZZHIDDEN-2')`, s.hr)

	// Deliveries.
	s.dV = id(`INSERT INTO deliveries (task_id, channel, subject, body, status, from_account_id, thread_id, created_by)
		VALUES ($1,'gmail','Re: DMVIS subject','DMVIS delivery','drafted',$2,$3,'mcp:manual:salvo') RETURNING id`, s.v, s.visAcct, s.tv)
	s.dU = id(`INSERT INTO deliveries (task_id, channel, body, status, target_client_ref, thread_id, created_by)
		VALUES ($1,'upwork_chat','ZZHIDDEN upwork chat body','drafted','itest-dm-client-ref',$2,'drafts:gpt') RETURNING id`, s.v, s.tv)
	s.dVR = id(`INSERT INTO deliveries (task_id, channel, subject, body, status, from_account_id, thread_id, created_by)
		VALUES ($1,'gmail','Re: DMVIS route subject','DMVIS route delivery','drafted',$2,$3,'mcp:manual:salvo') RETURNING id`, s.vr, s.hidAcct, s.tr)
	slack := func(task int64, status, body string) int64 {
		t.Helper()
		sent := "NULL"
		if status == "sent" {
			sent = "now()"
		}
		return id(`INSERT INTO deliveries (task_id, channel, body, status, target_ref, created_by, sent_at)
			VALUES ($1,'slack_reply',$2,$3,'https://app.slack.com/client/TDMVIS1/CDMVIS1/p1','drafts:gpt',`+sent+`) RETURNING id`,
			task, body, status)
	}
	s.dD = slack(s.d, "approved", "ZZHIDDEN D delivery")
	s.dC = slack(s.c, "failed", "ZZHIDDEN C delivery")
	s.dG = slack(s.g, "sent", "ZZHIDDEN G delivery")
	s.dH = slack(s.hr, "drafted", "ZZHIDDEN delivery body")
	s.dHN = slack(s.hn, "sending", "ZZHIDDEN sending body")
	s.dHI = id(`INSERT INTO deliveries (task_id, channel, body, status, target_ref, created_by, rejection_note)
		VALUES ($1,'slack_reply','ZZHIDDEN rejected body','rejected','https://app.slack.com/client/TDMVIS1/CDMVIS1/p2','drafts:gpt','ZZHIDDEN note') RETURNING id`, s.hi)

	// Capture rule in the hidden project (the SPEC's "capture rule name"; no page renders rule text today).
	ex(`INSERT INTO capture_rules (project_id, subproject, criteria_type, pattern, note)
		VALUES ($1,'ZZHIDDEN-sub','sender','zz@itest-dm.example','ZZHIDDEN rule')`, s.hidProj)

	// Plan imports: one visible, one hidden.
	plan := func(proj, acct int64, path, node string) int64 {
		t.Helper()
		rawID := raw(acct, "plan-"+path, true)
		run := id(`INSERT INTO ai_runs (worker_type, provider, model, input, output, status)
			VALUES ('plan_import','openai','gpt-5-mini','{"itest":"dm"}','{}','ok') RETURNING id`)
		fields := `{"summary":"` + node + ` summary","tasks":[{"ref":"a","parent_ref":null,"title":"` + node +
			`","body":"b","assignee_type":"human","subproject":null,"worker_type":null,"priority":0,"depends_on_refs":[],"confidence":0.9,"notes":"","plan_order":1}]}`
		ext := id(`INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields) VALUES ($1,$2,$3::jsonb) RETURNING id`, run, rawID, fields)
		return id(`INSERT INTO plan_imports (project_id, source_path, content_hash, raw_source_item_id, ai_run_id, ai_extraction_id, status)
			VALUES ($1,$2,$2,$3,$4,$5,'proposed') RETURNING id`, proj, path, rawID, run, ext)
	}
	s.pv = plan(s.visProj, s.visAcct, "/itest/dmvis-plan.md", "DMVIS plan node")
	s.ph = plan(s.hidProj, s.hidAcct, "/itest/ZZHIDDEN-plan.md", "ZZHIDDEN plan node")

	s.visTitles = map[int64]string{
		s.v: "DMVIS main task", s.vk: "DMVIS kid", s.vc: "DMVIS child of foreign parent", s.vs: "DMVIS foreign-source task",
		s.va: "DMVIS activity task", s.vr: "DMVIS route task", s.vdt: "DMVIS done today",
	}
	return s
}

// dmMarkHidden is the runbook's psql write (criterion 31: never the dashboard's).
func dmMarkHidden(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids ...int64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE tasks SET demo_hidden = true WHERE id = ANY($1)`, ids); err != nil {
		t.Fatalf("UPDATE tasks SET demo_hidden (migration 0048, criterion 28): %v", err)
	}
}

func dmFlag(t *testing.T, ctx context.Context, pool *pgxpool.Pool, value string) {
	t.Helper()
	dmExec(t, ctx, pool, `INSERT INTO ops_flags (name, value) VALUES ('demo_mode', $1::jsonb)
		ON CONFLICT (name) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, value)
}

func dmFlagAbsent(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	dmExec(t, ctx, pool, `DELETE FROM ops_flags WHERE name = 'demo_mode'`)
}

// ---- server -------------------------------------------------------------------

type dmRecExec struct {
	mu    sync.Mutex
	calls []executor.Call
}

func (r *dmRecExec) Execute(_ context.Context, call executor.Call) (executor.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call)
	return executor.Result{}, nil
}

func (r *dmRecExec) take() []executor.Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.calls
	r.calls = nil
	return out
}

type dmEnv struct {
	ts       *httptest.Server
	srv      *dashboard.Server
	follow   *http.Client
	noFollow *http.Client
	rec      *dmRecExec
}

func dmServer(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *dmEnv {
	t.Helper()
	auth, err := dashboard.NewAuth(ctx, "", "", "", "")
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	rec := &dmRecExec{}
	srv, err := dashboard.NewServer(pool, rec, auth)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	follow := &http.Client{Jar: jar}
	if _, err := follow.Get(ts.URL + "/dev/login?user=salvo"); err != nil {
		t.Fatalf("dev login: %v", err)
	}
	noFollow := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &dmEnv{ts: ts, srv: srv, follow: follow, noFollow: noFollow, rec: rec}
}

func (e *dmEnv) get(t *testing.T, path string) (int, string) {
	t.Helper()
	return get(t, e.follow, e.ts.URL+path)
}

// post sends a form without following the redirect; returns status and Location.
func (e *dmEnv) post(t *testing.T, path string, form url.Values) (int, string) {
	t.Helper()
	resp, err := e.noFollow.PostForm(e.ts.URL+path, form)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Location")
}

// ---- the crawl's checks ---------------------------------------------------------

func dmAround(s string, i int) string {
	lo, hi := i-80, i+80
	if lo < 0 {
		lo = 0
	}
	if hi > len(s) {
		hi = len(s)
	}
	return strings.Join(strings.Fields(s[lo:hi]), " ")
}

// dmLeaks reports every criterion-25 leak in body.
func dmLeaks(body string, sd dmSeed) []string {
	var out []string
	low := strings.ToLower(body)
	for _, w := range []string{"zzhidden", "upwork"} {
		if i := strings.Index(low, w); i >= 0 {
			out = append(out, fmt.Sprintf("%q at …%s…", w, dmAround(body, i)))
		}
	}
	for _, id := range sd.hiddenTasks() {
		ref := "/tasks/" + strconv.FormatInt(id, 10) + `"`
		if i := strings.Index(body, ref); i >= 0 {
			out = append(out, fmt.Sprintf("a link to hidden task %d at …%s…", id, dmAround(body, i)))
		}
	}
	if i := strings.Index(body, "/plans/"+strconv.FormatInt(sd.ph, 10)+`"`); i >= 0 {
		out = append(out, fmt.Sprintf("a link to hidden plan %d at …%s…", sd.ph, dmAround(body, i)))
	}
	return out
}

var (
	dmWordTok = regexp.MustCompile(`[^\s<>]*(?i:demo|hidden)[^\s<>]*`)
	dmDigits  = regexp.MustCompile(`\d+`)
)

// dmWordTokens is criterion 27's unit: every whitespace/tag-delimited token
// containing "demo" or "hidden" (any case), digits masked, the ZZHIDDEN
// sentinel's own tokens left to dmLeaks.
func dmWordTokens(body string) map[string]bool {
	out := map[string]bool{}
	for _, m := range dmWordTok.FindAllString(body, -1) {
		if strings.Contains(strings.ToLower(m), "zzhidden") {
			continue
		}
		out[dmDigits.ReplaceAllString(m, "0")] = true
	}
	return out
}

// dmNewWords is the demo-on tokens the demo-off render of the same page lacks.
func dmNewWords(on, off string) []string {
	base := dmWordTokens(off)
	var extra []string
	for tok := range dmWordTokens(on) {
		if !base[tok] {
			extra = append(extra, tok)
		}
	}
	sort.Strings(extra)
	return extra
}

func dmTaskPaths(sd dmSeed) []string {
	ids := append(sd.boardVisible(), sd.hiddenTasks()...)
	var out []string
	for _, id := range ids {
		out = append(out, "/tasks/"+strconv.FormatInt(id, 10))
	}
	return out
}

func dmCrawlPaths(sd dmSeed) []string {
	paths := []string{"/tasks", "/tasks?refresh=on", "/kiosk"}
	for _, st := range []string{"holding", "ready", "blocked", "claimed", "in_progress", "needs_feedback", "pr_open",
		"awaiting_ci", "awaiting_merge", "done_locally", "delivered", "closed"} {
		paths = append(paths, "/tasks?status="+st)
	}
	paths = append(paths, "/tasks?assignee_type=claude", "/tasks?assignee_type=human")
	paths = append(paths, dmTaskPaths(sd)...)
	paths = append(paths, "/deliveries")
	for _, st := range []string{"drafted", "approved", "sending", "sent", "failed", "rejected"} {
		paths = append(paths, "/deliveries?status="+st)
	}
	paths = append(paths,
		"/plans", "/plans?status=proposed",
		"/plans/"+strconv.FormatInt(sd.pv, 10), "/plans/"+strconv.FormatInt(sd.ph, 10),
		"/briefs", "/sources", "/funnel?days=90",
		"/export/tasks.csv", "/export/tasks.json", "/export/tasks.csv?status=closed", "/export/tasks.json?status=closed")
	return paths
}

// ---- criterion 3: the flag flips with no restart -----------------------------------

// M2 (the loader returns off regardless of the row) turns this red.
func TestDemo_Integration_FlipWithoutRestart(t *testing.T) {
	ctx, pool := dmStart(t)
	sd := dmSeedAll(t, ctx, pool)
	e := dmServer(t, ctx, pool)

	if _, b := e.get(t, "/tasks"); !strings.Contains(b, "ZZHIDDEN ready") {
		t.Fatalf("CONTROL: with no demo_mode row /tasks does not show the hidden-project sentinel task %d", sd.hr)
	}
	dmFlag(t, ctx, pool, dmOn)
	if _, b := e.get(t, "/tasks"); strings.Contains(b, "ZZHIDDEN ready") {
		t.Errorf("after INSERTing demo_mode on:true, the NEXT request on the same Server still shows the hidden task; " +
			"criterion 3: the flag is read on every request, no restart")
	} else if !strings.Contains(b, "DMVIS main task") {
		t.Errorf("demo on hid the allowlisted project's task too; the scope is not the flag's lists")
	}
	dmFlag(t, ctx, pool, dmOff)
	if _, b := e.get(t, "/tasks"); !strings.Contains(b, "ZZHIDDEN ready") {
		t.Errorf("after setting on:false, the next request still hides the task; criterion 3")
	}
	dmFlagAbsent(t, ctx, pool)
	if _, b := e.get(t, "/tasks"); !strings.Contains(b, "ZZHIDDEN ready") {
		t.Errorf("with the row deleted again the task is hidden; D1: no row is off")
	}
	if n := len(e.rec.take()); n != 0 {
		t.Errorf("GET requests made %d executor calls", n)
	}
}

// ---- criterion 2: once per request, and never without the wrapper -------------------

func TestDemo_Integration_ScopeLoadedOncePerRequest(t *testing.T) {
	ctx, plain := dmStart(t)
	dmSeedAll(t, ctx, plain)
	dmFlag(t, ctx, plain, dmOn)

	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	tr := &countTracer{}
	cfg.ConnConfig.Tracer = tr
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	defer pool.Close()
	e := dmServer(t, ctx, pool)

	for _, path := range []string{"/tasks", "/tasks/" + strconv.FormatInt(dmMissing, 10), "/sources", "/export/tasks.csv"} {
		tr.take()
		e.get(t, path)
		n := 0
		for _, q := range tr.take() {
			if strings.Contains(q, "ops_flags") && !strings.Contains(q, "sending_frozen") {
				n++
			}
		}
		if n != 1 {
			t.Errorf("GET %s read ops_flags %d times, want exactly once (criterion 2: one wrapper load per request, "+
				"handlers read the scope from the context)", path, n)
		}
	}
	for _, path := range []string{"/healthz", "/static/icon-192.png", "/dev/login?user=salvo"} {
		tr.take()
		// NOT followed: /dev/login redirects to the board, and the board's own
		// read of the flag is not /dev/login's (criterion 4 is about the route).
		if resp, err := e.noFollow.Get(e.ts.URL + path); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		for _, q := range tr.take() {
			if strings.Contains(q, "ops_flags") {
				t.Errorf("GET %s read ops_flags; criterion 4: /healthz, /static/* and /dev/login never read the flag", path)
			}
		}
	}
}

func TestDemo_Integration_UnwrappedHandlerShowsNothing(t *testing.T) {
	ctx, pool := dmStart(t)
	dmSeedAll(t, ctx, pool)
	dmFlagAbsent(t, ctx, pool) // the DATABASE says off
	e := dmServer(t, ctx, pool)
	if _, b := e.get(t, "/tasks"); !strings.Contains(b, "DMVIS main task") || !strings.Contains(b, "ZZHIDDEN ready") {
		t.Fatalf("CONTROL: the wrapped /tasks with demo off does not show both fixture tasks")
	}
	bare := dashboard.DemoBareHandlers(e.srv)
	for _, name := range []string{"tasks", "deliveries", "export.csv"} {
		path := map[string]string{"tasks": "/tasks", "deliveries": "/deliveries", "export.csv": "/export/tasks.csv"}[name]
		rec := httptest.NewRecorder()
		bare[name].ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
		body := rec.Body.String()
		for _, w := range []string{"DMVIS main task", "DMVIS delivery", "ZZHIDDEN"} {
			if strings.Contains(body, w) {
				t.Errorf("%s served WITHOUT the demo wrapper renders %q (status %d). Criterion 2: a handler with no scope "+
					"in its context treats demo as ON with empty lists, whatever the flag row says", name, w, rec.Code)
			}
		}
	}
}

// ---- criteria 25-27: the leak crawl, its control, and the banned words --------------

// M1, M6, M7, M8, M9, M10, M13, M14 each put a ZZHIDDEN, an "upwork" or a hidden
// link into some demo-on body here.
func TestDemo_Integration_LeakCrawl(t *testing.T) {
	ctx, pool := dmStart(t)
	sd := dmSeedAll(t, ctx, pool)
	dmMarkHidden(t, ctx, pool, sd.d)
	e := dmServer(t, ctx, pool)
	paths := dmCrawlPaths(sd)
	hiddenFilter := "/tasks?project=" + url.QueryEscape(dmHidSlug)
	paths = append(paths, hiddenFilter)

	// ---- demo OFF: the control (criterion 26) and the word baseline (27) ----
	dmFlagAbsent(t, ctx, pool)
	off := map[string]string{}
	for _, p := range paths {
		_, off[p] = e.get(t, p)
	}
	for _, p := range []string{"/tasks", "/deliveries", "/sources", "/funnel?days=90", "/plans"} {
		if !strings.Contains(strings.ToLower(off[p]), "zzhidden") {
			t.Errorf("CONTROL (criterion 26): with demo off %s does not contain ZZHIDDEN; the fixture does not reach "+
				"that page, so the demo-on crawl proves nothing there", p)
		}
	}
	for _, title := range []string{"ZZHIDDEN flagged D", "ZZHIDDEN child C", "ZZHIDDEN grandchild G",
		"ZZHIDDEN ready", "ZZHIDDEN in flight", "ZZHIDDEN needs input", "ZZHIDDEN done today"} {
		if !strings.Contains(off["/tasks"], title) {
			t.Errorf("CONTROL: with demo off /tasks lacks %q (demo_hidden must have NO effect when demo is off, "+
				"criterion 31; and the fixture must reach the board)", title)
		}
	}
	if !strings.Contains(off["/tasks?status=closed"], "ZZHIDDEN closed long ago") {
		t.Errorf("CONTROL: with demo off /tasks?status=closed lacks the old hidden close")
	}
	if !strings.Contains(strings.ToLower(off["/deliveries"]), "upwork") {
		t.Errorf("CONTROL: with demo off /deliveries has no \"upwork\" (the upwork_chat row's Copy for Upwork); the " +
			"upwork ban below would be vacuous")
	}
	if !strings.Contains(off["/briefs"], "Morning brief 2026-09-29") {
		t.Errorf("CONTROL: with demo off /briefs lacks the fixture brief")
	}
	if n := len(e.rec.take()); n != 0 {
		t.Fatalf("the crawl's GETs made %d executor calls", n)
	}

	// ---- demo ON: the crawl (criterion 25) and the words (27) ----
	dmFlag(t, ctx, pool, dmOn)
	for _, p := range paths {
		code, body := e.get(t, p)
		if code >= 500 {
			t.Errorf("demo on: GET %s = %d\n%s", p, code, snippet(body))
			continue
		}
		scan := body
		if p == hiddenFilter {
			scan = strings.ReplaceAll(scan, dmHidSlug, "") // the viewer's own typed filter, echoed back
		}
		for _, leak := range dmLeaks(scan, sd) {
			t.Errorf("demo on: GET %s leaks %s (criterion 25)", p, leak)
		}
		if extra := dmNewWords(scan, off[p]); len(extra) > 0 {
			t.Errorf("demo on: GET %s renders %v, which the demo-off render of the same page does not (criterion 27: "+
				"no page says \"demo\" or \"hidden\", nor shows that anything is filtered)", p, extra)
		}
	}
	if n := len(e.rec.take()); n != 0 {
		t.Errorf("the demo-on crawl made %d executor calls", n)
	}
}

// ---- criteria 9-11, 13: the board, its counts, the queue head, the exports ----------

func TestDemo_Integration_BoardCountsQueueHeadAndExports(t *testing.T) {
	ctx, pool := dmStart(t)
	sd := dmSeedAll(t, ctx, pool)
	dmMarkHidden(t, ctx, pool, sd.d)
	e := dmServer(t, ctx, pool)

	// Demo off: D (priority 3) heads the visible project's human lane (the control).
	_, offBody := e.get(t, "/tasks")
	if r := boardRow(offBody, sd.d); !strings.Contains(r, "s-next") {
		t.Fatalf("CONTROL: with demo off the demo_hidden task D does not head its lane; row: %s", r)
	}
	alertOff := strings.Contains(offBody, `id="orchestrator-health"`)

	dmFlag(t, ctx, pool, dmOn)
	_, body := e.get(t, "/tasks")

	// Criterion 10 / D5: queue heads are recomputed among visible tasks.
	if r := boardRow(body, sd.v); !strings.Contains(r, "s-next") {
		t.Errorf("demo on: V does not head the visible project's human lane once D is hidden (criterion 10, D5: "+
			"both boardLightFacts statements carry @demo.task). Row: %s", r)
	}

	// Criterion 9: every section, the counts footer.
	want := map[int64]bool{}
	for _, id := range sd.boardVisible() {
		want[id] = true
	}
	seen := map[int64]bool{}
	total := 0
	for _, sec := range layoutSections(body) {
		if sec.count != len(sec.ids) {
			t.Errorf("section %s says (%d) but lists %d rows", sec.key, sec.count, len(sec.ids))
		}
		total += sec.count
		for _, id := range sec.ids {
			seen[id] = true
			if !want[id] {
				t.Errorf("demo on: /tasks section %s shows task %d, which is not a visible fixture task", sec.key, id)
			}
		}
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("demo on: /tasks is missing visible task %d %q; the filter is hiding too much", id, sd.visTitles[id])
		}
	}
	if total != len(want) {
		t.Errorf("demo on: the section counts sum to %d, want %d visible tasks", total, len(want))
	}
	m := regexp.MustCompile(`Overall · (\d+) open · (\d+) done today`).FindStringSubmatch(body)
	if m == nil {
		t.Errorf("demo on: no counts footer on /tasks")
	} else if o, d := dmAtoi(m[1]), dmAtoi(m[2]); o+d != len(want) || d != 2 { // VDT closed today + VR done_locally
		t.Errorf("demo on: counts footer = %d open, %d done today; want %d in all with 2 done today (criterion 9: "+
			"the footer counts only what is visible)", o, d, len(want))
	}

	// Criterion 9: the project <select>.
	if strings.Contains(body, `value="`+dmHidSlug+`"`) || strings.Contains(strings.ToLower(body), "zzhidden") {
		t.Errorf("demo on: the hidden project slug is on /tasks (the project <select>?)")
	}
	if !strings.Contains(body, `value="`+dmVisSlug+`"`) {
		t.Errorf("demo on: the visible project is missing from the project <select>")
	}

	// Criterion 11: no orchestrator alert in demo.
	if !alertOff {
		t.Logf("note: the orchestrator alert does not render with demo off in this database, so criterion 11's " +
			"board check below has no control here")
	}
	if strings.Contains(body, `id="orchestrator-health"`) {
		t.Errorf("demo on: /tasks renders the orchestrator alert (criterion 11: global event volume is never shown in demo)")
	}

	// Criterion 13: the exports share boardRows; header and columns unchanged.
	_, csv := e.get(t, "/export/tasks.csv")
	first, _, _ := strings.Cut(csv, "\n")
	if strings.TrimSuffix(first, "\r") != pinnedHeader {
		t.Errorf("demo on: CSV header = %q, want the pinned header (criterion 13: no demo_hidden column)", first)
	}
	for _, id := range sd.boardVisible() {
		if !strings.Contains(csv, sd.visTitles[id]) {
			t.Errorf("demo on: the CSV export lacks visible task %q", sd.visTitles[id])
		}
	}
	_, js := e.get(t, "/export/tasks.json")
	var rows []map[string]any
	if err := json.Unmarshal([]byte(js), &rows); err != nil {
		t.Fatalf("demo on: /export/tasks.json is not JSON: %v\n%s", err, snippet(js))
	}
	if len(rows) != len(want) {
		t.Errorf("demo on: the JSON export has %d rows, want the %d visible tasks", len(rows), len(want))
	}
	for _, r := range rows {
		if _, ok := r["demo_hidden"]; ok {
			t.Errorf("the JSON export carries demo_hidden; criterion 13: it is not an export column")
		}
	}
}

func dmAtoi(s string) int { n, _ := strconv.Atoi(s); return n }

func TestDemo_Integration_HiddenProjectFilterLooksNonexistent(t *testing.T) {
	ctx, pool := dmStart(t)
	sd := dmSeedAll(t, ctx, pool)
	// The ZZHIDDEN-titled demo_hidden chain (D, C, G) sits in the VISIBLE
	// project: it is hidden only once D is marked, as the crawl does.
	dmMarkHidden(t, ctx, pool, sd.d)
	e := dmServer(t, ctx, pool)
	dmFlag(t, ctx, pool, dmOn)
	norm := func(body, slug string) string {
		return dmDigits.ReplaceAllString(strings.ReplaceAll(body, slug, "SLUG"), "0")
	}
	c1, hid := e.get(t, "/tasks?project="+url.QueryEscape(dmHidSlug))
	c2, none := e.get(t, "/tasks?project="+url.QueryEscape(dmNoSlug))
	if c1 != c2 || norm(hid, dmHidSlug) != norm(none, dmNoSlug) {
		t.Errorf("demo on: /tasks?project=<hidden slug> (%d) does not render exactly like /tasks?project=<nonexistent> "+
			"(%d), slugs and digits masked (criterion 9).\nhidden: %s\nnone:   %s", c1, c2, snippet(hid), snippet(none))
	}
	k1, kh := e.get(t, "/kiosk")
	if k1 != http.StatusOK || strings.Contains(strings.ToLower(kh), "zzhidden") {
		t.Errorf("demo on: /kiosk = %d or names the hidden project", k1)
	}
	// The kiosk frames /tasks?refresh=on: that render is the board, filtered.
	if _, b := e.get(t, "/tasks?refresh=on"); strings.Contains(strings.ToLower(b), "zzhidden") {
		t.Errorf("demo on: the kiosk's framed board (/tasks?refresh=on) shows a hidden row")
	}
}

// ---- criteria 14-16, 29: the task page ----------------------------------------------

// M10 (ParentLink from the raw parent_id) and M13/M14 turn this red.
func TestDemo_Integration_TaskPage(t *testing.T) {
	ctx, pool := dmStart(t)
	sd := dmSeedAll(t, ctx, pool)
	dmMarkHidden(t, ctx, pool, sd.d)
	e := dmServer(t, ctx, pool)
	path := func(id int64) string { return "/tasks/" + strconv.FormatInt(id, 10) }

	// Controls with demo off.
	if _, b := e.get(t, path(sd.vc)); !strings.Contains(b, path(sd.hp)+`"`) {
		t.Fatalf("CONTROL: with demo off VC's page does not link its hidden-project parent %d", sd.hp)
	}
	if _, b := e.get(t, path(sd.vs)); !strings.Contains(b, "ZZHIDDEN") {
		t.Fatalf("CONTROL: with demo off VS's page does not show its hidden-account source message")
	}
	if c, _ := e.get(t, path(sd.d)); c != http.StatusOK {
		t.Fatalf("CONTROL: with demo off the demo_hidden task's page = %d, want 200 (criterion 31: no effect when off)", c)
	}

	dmFlag(t, ctx, pool, dmOn)
	// Criterion 14 / 29: a hidden task's page IS the not-found page, byte for byte.
	mc, mb := e.get(t, path(dmMissing))
	for _, id := range sd.hiddenTasks() {
		c, b := e.get(t, path(id))
		if c != mc || b != mb {
			t.Errorf("demo on: GET %s = %d, want exactly GET %s's %d and body %q (criteria 14, 29)", path(id), c,
				path(dmMissing), mc, snippet(mb))
		}
	}
	// A child created AFTER the flag, never flagged itself (D9).
	late := dmID(t, ctx, pool, `INSERT INTO tasks (project_id, parent_id, title, assignee_type, status)
		VALUES ($1,$2,'ZZHIDDEN late child','human','ready') RETURNING id`, sd.visProj, sd.g)
	if c, b := e.get(t, path(late)); c != mc || b != mb {
		t.Errorf("demo on: a child of G created after the flag was set is shown (%d); D9: inherited at read time", c)
	}
	if _, b := e.get(t, "/tasks"); strings.Contains(b, "ZZHIDDEN late child") {
		t.Errorf("demo on: the late child is on the board (D9)")
	}

	// Criterion 15/16 on V.
	c, b := e.get(t, path(sd.v))
	if c != http.StatusOK || !strings.Contains(b, "DMVIS main task") {
		t.Fatalf("demo on: V's page = %d without its title\n%s", c, snippet(b))
	}
	for _, w := range []string{"DMVIS kid", "DMVIS done today", "Re: DMVIS subject", "DMVIS-1", "DMVIS-EVENT", "DMVIS question",
		"on the source thread"} {
		if !strings.Contains(b, w) {
			t.Errorf("demo on: V's page lacks %q; visible children, dependencies, deliveries, refs, events, feedback "+
				"and the visible source message render unchanged (criteria 15, 16)", w)
		}
	}
	for _, id := range []int64{sd.d, sd.hc, sd.hr} {
		if strings.Contains(b, path(id)+`"`) {
			t.Errorf("demo on: V's page links hidden task %d (children/dependencies, criterion 15)", id)
		}
	}
	// ParentLink on VC: its parent is a hidden-project task.
	_, b = e.get(t, path(sd.vc))
	if strings.Contains(b, path(sd.hp)+`"`) || strings.Contains(b, "#"+strconv.FormatInt(sd.hp, 10)+"<") {
		t.Errorf("demo on: VC's page links its hidden parent %d (criterion 15, M10: ParentLink only when the parent "+
			"passed @demo.task)", sd.hp)
	}
	if !strings.Contains(b, "<th>parent</th><td>—</td>") {
		t.Errorf("demo on: VC's parent cell is not the plain no-parent dash")
	}
	// A hidden account's message costs VS the section and nothing else.
	c, b = e.get(t, path(sd.vs))
	if c != http.StatusOK || !strings.Contains(b, "DMVIS foreign-source task") {
		t.Errorf("demo on: VS's page = %d or lost its title; a hidden source message costs only the section", c)
	}
	if strings.Contains(b, "on the source thread") || strings.Contains(b, "Source email") {
		t.Errorf("demo on: VS renders a source-message section from a hidden account (criterion 15, @demo.message)")
	}
}

// ---- criteria 17-19, 30, 31: verbs refused exactly like not-found ----------------

type dmVerb struct {
	name  string
	path  func(id int64) string
	form  url.Values
	back  string // the page the verb returns to
	noun  string // the flash's noun
	numID bool
}

func dmVerbs(v int64) (task, delivery, plan []dmVerb) {
	tp := func(verb string) func(int64) string {
		return func(id int64) string { return "/tasks/" + strconv.FormatInt(id, 10) + "/" + verb }
	}
	dp := func(verb string) func(int64) string {
		return func(id int64) string { return "/deliveries/" + strconv.FormatInt(id, 10) + "/" + verb }
	}
	pp := func(verb string) func(int64) string {
		return func(id int64) string { return "/plans/" + strconv.FormatInt(id, 10) + "/" + verb }
	}
	st := url.Values{"status": {"ready"}}
	with := func(kv ...string) url.Values {
		f := url.Values{"status": {"ready"}}
		for i := 0; i+1 < len(kv); i += 2 {
			f.Set(kv[i], kv[i+1])
		}
		return f
	}
	task = []dmVerb{
		{name: "dismiss", path: tp("dismiss"), form: with("reason_code", "not_actionable"), back: "/tasks", noun: "task"},
		{name: "close", path: tp("close"), form: with("note", "x"), back: "/tasks", noun: "task"},
		{name: "requeue", path: tp("requeue"), form: st, back: "/tasks", noun: "task"},
		{name: "attach (source)", path: tp("attach"), form: with("target_task_id", strconv.FormatInt(v, 10)), back: "/tasks", noun: "task"},
	}
	delivery = []dmVerb{
		{name: "edit", path: dp("edit"), form: url.Values{"body": {"x"}}, back: "/deliveries", noun: "delivery"},
		{name: "approve", path: dp("approve"), form: url.Values{"content_hash": {"x"}}, back: "/deliveries", noun: "delivery"},
		{name: "reject", path: dp("reject"), form: url.Values{"content_hash": {"x"}, "note": {"x"}}, back: "/deliveries", noun: "delivery"},
		{name: "send", path: dp("send"), form: url.Values{}, back: "/deliveries", noun: "delivery"},
		{name: "mark-sent", path: dp("mark-sent"), form: url.Values{}, back: "/deliveries", noun: "delivery"},
		{name: "mark-failed", path: dp("mark-failed"), form: url.Values{}, back: "/deliveries", noun: "delivery"},
	}
	plan = []dmVerb{
		{name: "plan approve", path: pp("approve"), form: url.Values{}, back: "/plans", noun: "plan import"},
		{name: "plan reject", path: pp("reject"), form: url.Values{}, back: "/plans", noun: "plan import"},
	}
	return
}

// dmMaskID replaces the encoded "#<id>" in a Location with "#N".
func dmMaskID(loc string, id int64) string {
	return regexp.MustCompile(`%23`+strconv.FormatInt(id, 10)+`\b`).ReplaceAllString(loc, "%23N")
}

// dmCheckRefusal posts v for id and for dmMissing and requires identical,
// not-found refusals that never reach the executor. flashID is the id the flash
// names (the target, for an attach target).
func dmCheckRefusal(t *testing.T, e *dmEnv, sd dmSeed, v dmVerb, pathID, flashID int64, form url.Values, offBase map[string]string) {
	t.Helper()
	e.rec.take()
	code, loc := e.post(t, v.path(pathID), form)
	calls := e.rec.take()
	missForm := form
	missPath := dmMissing
	if pathID != flashID { // the attach target case: same source, a nonexistent target
		missForm = url.Values{}
		for k, vs := range form {
			missForm[k] = vs
		}
		missForm.Set("target_task_id", strconv.FormatInt(dmMissing, 10))
		missPath = pathID
	}
	mcode, mloc := e.post(t, v.path(missPath), missForm)
	mcalls := e.rec.take()
	label := fmt.Sprintf("%s on %s", v.name, v.path(pathID))
	if pathID != flashID {
		label += fmt.Sprintf(" with target_task_id=%d", flashID)
	}
	if len(calls) != 0 {
		t.Errorf("demo on: %s reached the executor (%d call(s), first %s). Criteria 17/30: the pre-executor check "+
			"refuses a hidden id, so no audit row is ever written", label, len(calls), calls[0].Tool)
	}
	if len(mcalls) != 0 {
		t.Errorf("%s with a NONEXISTENT id reached the executor; criterion 18: the not-found flash comes from the "+
			"dashboard in both modes", v.name)
	}
	if code != mcode || dmMaskID(loc, flashID) != dmMaskID(mloc, dmMissing) {
		t.Errorf("demo on: %s answered %d %q; a nonexistent id answers %d %q. Criteria 17/30: refused EXACTLY like "+
			"not-found (Location byte for byte, the flash's id masked)", label, code, loc, mcode, mloc)
	}
	u, err := url.Parse(loc)
	if err != nil || u.Path != v.back {
		t.Errorf("demo on: %s redirects to %q, want %s (the page the verb returns to)", label, loc, v.back)
		return
	}
	wantFlash := fmt.Sprintf("%s #%d not found", v.noun, flashID)
	if got := u.Query().Get("flash"); got != wantFlash {
		t.Errorf("demo on: %s flash = %q, want %q (criterion 17)", label, got, wantFlash)
	}
	if v.back == "/tasks" && u.Query().Get("status") != "ready" {
		t.Errorf("demo on: %s dropped the board filter from its redirect (%q); it lands where the verb would have", label, loc)
	}
	// The crawl (25) follows the redirect.
	if _, body := e.get(t, loc); loc != "" && body != "" {
		for _, leak := range dmLeaks(body, sd) {
			t.Errorf("demo on: following %s's redirect leaks %s", label, leak)
		}
		if extra := dmNewWords(body, offBase[v.back]); len(extra) > 0 {
			t.Errorf("demo on: %s's landing page renders %v (criterion 27)", label, extra)
		}
	}
	for _, leak := range dmLeaks(loc, sd) {
		t.Errorf("demo on: %s's Location leaks %s", label, leak)
	}
}

// M4 (drop the pre-check in closeTaskAction) and M5 (drop the attach TARGET check)
// turn this red.
func TestDemo_Integration_VerbsRefusedLikeNotFound(t *testing.T) {
	ctx, pool := dmStart(t)
	sd := dmSeedAll(t, ctx, pool)
	dmMarkHidden(t, ctx, pool, sd.d)
	e := dmServer(t, ctx, pool)

	offBase := map[string]string{}
	dmFlagAbsent(t, ctx, pool)
	for _, p := range []string{"/tasks", "/deliveries", "/plans"} {
		_, offBase[p] = e.get(t, p+map[string]string{"/tasks": "?status=ready"}[p])
	}

	// ---- demo OFF (criteria 18, 31) ----
	e.rec.take()
	if c, loc := e.post(t, "/tasks/"+strconv.FormatInt(sd.d, 10)+"/close", url.Values{"note": {"x"}}); c != http.StatusSeeOther {
		t.Errorf("demo off: close on the demo_hidden task answered %d %q", c, loc)
	}
	if calls := e.rec.take(); len(calls) != 1 || calls[0].Tool != "task_close" {
		t.Errorf("demo off: close on the demo_hidden task made %v; criterion 31: with demo off it acts exactly as today "+
			"(one task_close through the executor)", calls)
	}
	for _, tc := range []struct{ path, back, flash string }{
		{"/tasks/" + strconv.FormatInt(dmMissing, 10) + "/close", "/tasks", fmt.Sprintf("task #%d not found", dmMissing)},
		{"/deliveries/" + strconv.FormatInt(dmMissing, 10) + "/send", "/deliveries", fmt.Sprintf("delivery #%d not found", dmMissing)},
		{"/plans/" + strconv.FormatInt(dmMissing, 10) + "/approve", "/plans", fmt.Sprintf("plan import #%d not found", dmMissing)},
	} {
		_, loc := e.post(t, tc.path, url.Values{"note": {"x"}})
		if calls := e.rec.take(); len(calls) != 0 {
			t.Errorf("demo off: POST %s (nonexistent) reached the executor; criterion 18: the dashboard's own not-found "+
				"flash in both modes", tc.path)
		}
		if u, err := url.Parse(loc); err != nil || u.Path != tc.back || u.Query().Get("flash") != tc.flash {
			t.Errorf("demo off: POST %s redirected to %q, want %s with flash %q (criterion 18)", tc.path, loc, tc.back, tc.flash)
		}
	}

	// ---- demo ON (criteria 17, 30) ----
	dmFlag(t, ctx, pool, dmOn)
	task, delivery, plan := dmVerbs(sd.v)
	hiddenTaskIDs := []int64{sd.hr, sd.d, sd.c, sd.g}
	for _, v := range task {
		for _, id := range hiddenTaskIDs {
			dmCheckRefusal(t, e, sd, v, id, id, v.form, offBase)
		}
	}
	attachTarget := dmVerb{name: "attach (target)", path: func(id int64) string {
		return "/tasks/" + strconv.FormatInt(id, 10) + "/attach"
	}, back: "/tasks", noun: "task"}
	for _, target := range hiddenTaskIDs {
		form := url.Values{"status": {"ready"}, "target_task_id": {strconv.FormatInt(target, 10)}}
		dmCheckRefusal(t, e, sd, attachTarget, sd.v, target, form, offBase)
	}
	for _, v := range delivery {
		for _, id := range sd.hiddenDeliveries() {
			dmCheckRefusal(t, e, sd, v, id, id, v.form, offBase)
		}
	}
	for _, v := range plan {
		dmCheckRefusal(t, e, sd, v, sd.ph, sd.ph, v.form, offBase)
	}

	// Non-numeric delivery and plan ids are a 400.
	for _, p := range []string{"/deliveries/abc/send", "/deliveries/abc/edit", "/deliveries/abc/mark-sent", "/plans/abc/approve", "/plans/abc/reject"} {
		if c, _ := e.post(t, p, url.Values{"content_hash": {"x"}}); c != http.StatusBadRequest {
			t.Errorf("demo on: POST %s = %d, want 400 (criterion 17: as board verbs already do)", p, c)
		}
	}
	if calls := e.rec.take(); len(calls) != 0 {
		t.Errorf("non-numeric ids reached the executor: %v", calls)
	}

	// A VISIBLE row's path is unchanged: one executor call.
	e.post(t, "/tasks/"+strconv.FormatInt(sd.v, 10)+"/close", url.Values{"note": {"x"}})
	if calls := e.rec.take(); len(calls) != 1 || calls[0].Tool != "task_close" {
		t.Errorf("demo on: close on a VISIBLE task made %v, want one task_close (the executor path for a visible row "+
			"is unchanged)", calls)
	}
	e.post(t, "/deliveries/"+strconv.FormatInt(sd.dV, 10)+"/send", url.Values{})
	if calls := e.rec.take(); len(calls) != 1 || calls[0].Tool != "send_delivery" {
		t.Errorf("demo on: send on a VISIBLE delivery made %v, want one send_delivery", calls)
	}
	// Criterion 19: the freeze names no row and is untouched.
	e.post(t, "/flags/sending-frozen", url.Values{"frozen": {"false"}})
	if calls := e.rec.take(); len(calls) != 1 || calls[0].Tool != "set_sending_frozen" {
		t.Errorf("demo on: POST /flags/sending-frozen made %v, want one set_sending_frozen (criterion 19)", calls)
	}
}

// ---- criterion 20: deliveries --------------------------------------------------------

// M6 and M12 (filter in Go after LIMIT) turn this red.
func TestDemo_Integration_DeliveriesFilterBeforeLimit(t *testing.T) {
	ctx, pool := dmStart(t)
	sd := dmSeedAll(t, ctx, pool)
	e := dmServer(t, ctx, pool)
	// The From control is read before the bulk rows push the route row off the page.
	_, offRoute := e.get(t, "/deliveries?status=drafted")
	if offRow := rowOf(t, offRoute, sd.dVR); !strings.Contains(strings.ToLower(offRow), "zzhidden") {
		t.Errorf("CONTROL: with demo off the route row does not show its hidden From account; row: %s", offRow)
	}
	for i := 1; i <= 3; i++ {
		dmExec(t, ctx, pool, `INSERT INTO deliveries (task_id, channel, body, status, target_ref, created_by)
			VALUES ($1,'slack_reply',$2,'drafted','https://app.slack.com/client/TDMVIS1/CDMVIS1/p3','drafts:gpt')`,
			sd.v, fmt.Sprintf("DMVIS-LIMIT-%d", i))
	}
	dmExec(t, ctx, pool, `INSERT INTO deliveries (task_id, channel, body, status, target_ref, created_by)
		SELECT $1,'slack_reply','ZZHIDDEN bulk '||g,'drafted','https://app.slack.com/client/TDMVIS1/CDMVIS1/p4','drafts:gpt'
		  FROM generate_series(1,150) g`, sd.hr)

	if _, b := e.get(t, "/deliveries"); strings.Contains(b, "DMVIS-LIMIT-1") {
		t.Fatalf("CONTROL: with demo off the 150 newer hidden rows do not push the 3 visible ones past LIMIT 100; " +
			"the test cannot tell SQL filtering from Go filtering")
	}
	dmFlag(t, ctx, pool, dmOn)
	for _, p := range []string{"/deliveries", "/deliveries?status=drafted"} {
		_, b := e.get(t, p)
		for i := 1; i <= 3; i++ {
			if w := fmt.Sprintf("DMVIS-LIMIT-%d", i); !strings.Contains(b, w) {
				t.Errorf("demo on: %s lacks %s behind 150 newer hidden rows; criterion 20: @demo.delivery is applied in "+
					"SQL BEFORE LIMIT 100", p, w)
			}
		}
		if strings.Contains(strings.ToLower(b), "upwork") {
			t.Errorf("demo on: %s shows the upwork_chat row (criterion 20: d.channel <> 'upwork_chat')", p)
		}
	}
	// From on a visible gmail row whose from-account is hidden.
	_, b := e.get(t, "/deliveries?status=drafted")
	row := rowOf(t, b, sd.dVR)
	if !strings.Contains(row, "(unresolved)") || strings.Contains(strings.ToLower(row), "zzhidden") {
		t.Errorf("demo on: the route row's From is not \"(unresolved)\" or names the hidden account (criterion 20: "+
			"sc.accountVisible on the resolved From). Row: %s", row)
	}
}

// ---- criteria 21-24: plans, briefs, sources, funnel ---------------------------------

func TestDemo_Integration_PlansBriefsSourcesFunnel(t *testing.T) {
	ctx, plain := dmStart(t)
	sd := dmSeedAll(t, ctx, plain)
	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	tr := &countTracer{}
	cfg.ConnConfig.Tracer = tr
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	defer pool.Close()
	e := dmServer(t, ctx, pool)

	// Demo-off controls.
	_, offFunnel := e.get(t, "/funnel?days=90")
	offSQL := strings.Join(tr.take(), "\n")
	for _, h := range []string{"<h2>Capture attribution</h2>", "<h2>Classify shadow summary</h2>", "<h2>Classify promotion</h2>",
		"<th>backlog</th>", "<th>cursor</th>", "<th>head</th>"} {
		if !strings.Contains(offFunnel, h) {
			t.Errorf("CONTROL: demo-off /funnel lacks %q", h)
		}
	}
	for _, tb := range []string{"capture_decisions", "classify_promotions", "ai_extractions"} {
		if !strings.Contains(offSQL, tb) {
			t.Errorf("CONTROL: demo-off /funnel ran no statement naming %s; the loader check below is vacuous", tb)
		}
	}

	dmFlag(t, ctx, plain, dmOn)

	// Criterion 21.
	mc, mb := e.get(t, "/plans/"+strconv.FormatInt(dmMissing, 10))
	if c, b := e.get(t, "/plans/"+strconv.FormatInt(sd.ph, 10)); c != mc || b != mb {
		t.Errorf("demo on: the hidden plan's page = %d, want exactly the nonexistent id's %d and body (criterion 21)", c, mc)
	}
	if c, b := e.get(t, "/plans/"+strconv.FormatInt(sd.pv, 10)); c != http.StatusOK || !strings.Contains(b, "DMVIS plan node") {
		t.Errorf("demo on: the visible plan's page = %d without its node", c)
	}
	if _, b := e.get(t, "/plans"); !strings.Contains(b, "/itest/dmvis-plan.md") {
		t.Errorf("demo on: /plans lacks the visible plan")
	}

	// Criterion 22 (M7).
	// The page's own heading reads "Morning briefs"; the fixture's brief is titled
	// "Morning brief 2026-09-29" (the demo-off control finds it).
	if _, b := e.get(t, "/briefs"); strings.Contains(b, "Morning brief 2026-09-29") || !strings.Contains(b, "No briefs yet") {
		t.Errorf("demo on: /briefs is not its normal empty state (criterion 22: @demo.task's brief clause hides every "+
			"'Morning brief %%' task)\n%s", snippet(b))
	}

	// Criterion 23.
	_, src := e.get(t, "/sources")
	var wantRaw, wantMsgs, wantPending, wantGmail int
	if err := plain.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM raw_source_items ri JOIN source_accounts a ON a.id = ri.source_account_id
		         WHERE lower(a.account_email) = ANY($1)),
		       (SELECT count(*) FROM normalized_messages nm JOIN raw_source_items ri ON ri.id = nm.raw_source_item_id
		          JOIN source_accounts a ON a.id = ri.source_account_id
		         WHERE lower(a.account_email) = ANY($1) AND nm.channel <> 'upwork'),
		       (SELECT count(*) FROM raw_source_items ri JOIN source_accounts a ON a.id = ri.source_account_id
		         WHERE lower(a.account_email) = ANY($1) AND ri.normalized_at IS NULL),
		       (SELECT count(*) FROM normalized_messages nm JOIN raw_source_items ri ON ri.id = nm.raw_source_item_id
		          JOIN source_accounts a ON a.id = ri.source_account_id
		         WHERE lower(a.account_email) = ANY($1) AND nm.channel = 'gmail')`, dmAllowLower).
		Scan(&wantRaw, &wantMsgs, &wantPending, &wantGmail); err != nil {
		t.Fatalf("expected totals: %v", err)
	}
	for _, h := range []struct {
		label string
		want  int
	}{{"raw items", wantRaw}, {"messages", wantMsgs}, {"awaiting normalize", wantPending}} {
		m := regexp.MustCompile(`<strong[^>]*>(\d+)</strong> ` + h.label).FindStringSubmatch(src)
		if m == nil || dmAtoi(m[1]) != h.want {
			t.Errorf("demo on: /sources headline %q = %v, want %d (criterion 23: sums of VISIBLE rows only)", h.label, m, h.want)
		}
	}
	if m := regexp.MustCompile(`<td>gmail</td>\s*<td class="num">(\d+)</td>`).FindStringSubmatch(src); m == nil || dmAtoi(m[1]) != wantGmail {
		t.Errorf("demo on: /sources channel gmail = %v, want %d visible messages (criterion 23)", m, wantGmail)
	}
	for _, w := range []string{dmVisAcct, "DMVIS watch"} {
		if !strings.Contains(src, w) {
			t.Errorf("demo on: /sources lacks the visible %q", w)
		}
	}

	// Criterion 24 (M8).
	tr.take()
	_, fun := e.get(t, "/funnel?days=90")
	onSQL := tr.take()
	for _, h := range []string{"Capture attribution", "Classify shadow", "Classify promotion", "lane</h3>", "personal",
		"<th>backlog</th>", "<th>cursor</th>", "<th>head</th>"} {
		if strings.Contains(fun, h) {
			t.Errorf("demo on: /funnel renders %q (criterion 24: capture/classify/promotion sections, their help text "+
				"and the orchestrator's backlog/cursor/head numbers do not render in demo)", h)
		}
	}
	for _, q := range onSQL {
		for _, tb := range []string{"capture_decisions", "classify_promotions", "ai_extractions"} {
			if strings.Contains(q, tb) {
				t.Errorf("demo on: /funnel ran a statement naming %s (criterion 24: those loaders DO NOT RUN in demo): %s",
					tb, demoOneLineSQL(q))
			}
		}
	}
	if !strings.Contains(fun, "<h2>Orchestrator</h2>") {
		t.Errorf("demo on: /funnel dropped the orchestrator section; it shows verdict and running state")
	}
	if h, err := orchestrator.Health(ctx, plain, time.Now()); err == nil && !strings.Contains(fun, h.Verdict) {
		t.Errorf("demo on: /funnel does not show the orchestrator verdict %q", h.Verdict)
	}
	// The intake trend counts only visible accounts: its columns and its sums.
	sec := fun
	if i := strings.Index(sec, "<h2>Intake trend</h2>"); i >= 0 {
		sec = sec[i:]
		if j := strings.Index(sec, "</table>"); j >= 0 {
			sec = sec[:j]
		}
		heads := regexp.MustCompile(`<th class="num">([^<]*)</th>`).FindAllStringSubmatch(sec, -1)
		var accts []string
		for _, h := range heads {
			if h[1] != "Raw total" && h[1] != "Messages" {
				accts = append(accts, strings.ToLower(h[1]))
			}
		}
		sort.Strings(accts)
		wantAccts := append([]string(nil), dmAllowLower...)
		sort.Strings(wantAccts)
		if strings.Join(accts, ",") != strings.Join(wantAccts, ",") {
			t.Errorf("demo on: the intake columns are %v, want exactly the allowlisted accounts %v (criterion 24)", accts, wantAccts)
		}
		var rawSum, msgSum int
		for _, row := range regexp.MustCompile(`(?s)<tr>\s*<td>\d{4}-\d\d-\d\d</td>(.*?)</tr>`).FindAllStringSubmatch(sec, -1) {
			nums := regexp.MustCompile(`<td class="num">(\d*)</td>`).FindAllStringSubmatch(row[1], -1)
			if len(nums) >= 2 {
				rawSum += dmAtoi(nums[len(nums)-2][1])
				msgSum += dmAtoi(nums[len(nums)-1][1])
			}
		}
		if rawSum != wantRaw || msgSum != wantMsgs {
			t.Errorf("demo on: the intake trend sums to %d raw / %d messages over 90 days, want %d / %d (criterion 24: no "+
				"count includes a hidden account's row)", rawSum, msgSum, wantRaw, wantMsgs)
		}
	} else {
		t.Errorf("demo on: /funnel has no intake trend")
	}
}

// ---- criteria 12, 28: the column and its trigger -------------------------------------

func TestDemo_Integration_DemoHiddenColumnShape(t *testing.T) {
	ctx, pool := dmStart(t)
	var dataType, nullable string
	var def *string
	err := pool.QueryRow(ctx, `SELECT data_type, is_nullable, column_default FROM information_schema.columns
		WHERE table_name = 'tasks' AND column_name = 'demo_hidden'`).Scan(&dataType, &nullable, &def)
	if err != nil {
		t.Fatalf("tasks.demo_hidden: %v (criterion 28: migration 0048 adds it)", err)
	}
	if dataType != "boolean" || nullable != "NO" || def == nil || *def != "false" {
		t.Errorf("tasks.demo_hidden = %s nullable=%s default=%v, want boolean NOT NULL DEFAULT false (criterion 28)",
			dataType, nullable, def)
	}
	var idx int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE tablename = 'tasks' AND indexdef LIKE '%demo_hidden%'`).
		Scan(&idx); err != nil {
		t.Fatalf("pg_indexes: %v", err)
	}
	if idx != 0 {
		t.Errorf("tasks has %d index(es) on demo_hidden; criterion 28: no index", idx)
	}
}

// Criterion 12: setting demo_hidden is a tasks UPDATE, so 0044's trigger wakes an
// open board within one broadcast.
func TestDemo_Integration_DemoHiddenUpdateWakesTheBoard(t *testing.T) {
	ctx, pool := dmStart(t)
	sd := dmSeedAll(t, ctx, pool)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN board_changed"); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}
	defer conn.Exec(context.Background(), "UNLISTEN board_changed")
	dmMarkHidden(t, ctx, pool, sd.vk)
	want := "tasks:" + strconv.FormatInt(sd.vk, 10)
	deadline := time.Now().Add(5 * time.Second)
	for {
		wctx, cancel := context.WithDeadline(ctx, deadline)
		n, err := conn.Conn().WaitForNotification(wctx)
		cancel()
		if err != nil {
			t.Fatalf("no board_changed %q within 5 s of setting demo_hidden (criterion 12): %v", want, err)
		}
		if n.Payload == want {
			return
		}
	}
}
