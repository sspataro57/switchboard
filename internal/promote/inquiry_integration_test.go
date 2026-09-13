//go:build integration

package promote_test

// SWT-40 Part C against a real database (docs/tickets/inquiry-promote_SPEC.md,
// C-D2..C-D12; criteria C2, C4, C7, C8, C9, C10, C11, C12, C14).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_isoc?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run PromoteInquiry ./internal/promote/
//
// USE AN ISOLATED DATABASE (IK "Test infrastructure", 2026-09-12): the compose
// Postgres is shared by every worktree and agent. Build-tagged `integration`,
// env-gated on DATABASE_URL, FATAL on 192.168.50.49.
//
// "TEST THE COLUMN, NOT THE FIXTURE" (IK, SWT-21's 6th landmine): every clause
// here turns on a value Postgres produces — worker_type, ai_runs.status,
// fields->>'needs_reply', the LATEST capture_decisions row in ANY mode,
// p.ai_inquiry, p.inquiry_promote_after vs r.created_at, nm.sent_at,
// nm.direction on the thread (replyfold), nm.thread_id vs the stored one. So
// every assertion is against rows the driver read itself. Mutations are named
// inline (V3).
//
// NO LLM, NO NETWORK: verdicts are ai_runs + ai_extractions rows written by
// hand in exactly the shape classify's inquiryFields writes.
//
// CROSS-POLLUTION PACT: promotion of the inquiry lane requires
// projects.inquiry_promote_after, which NO other suite sets (0031 adds it NULL,
// no default) — criterion C-D2 doing double duty, as SWT-30's cutover does.
// This suite owns and clears, at start and end: projects itest-inqp-%, provider
// itest-inqp-src, threads with subject 'itest-inqp', ai_runs model
// itest-inqp-model, capture_rules note 'itest-inqp', audit rows by task plus
// actors promote:% and this file's two human actors.
//
// ANTI-DATE-ROT: every instant is the DB clock (clock_timestamp()) minus a
// duration.
//
// ---- IMPOSED SURFACE (beyond inquiry_test.go's) ------------------------------
//
//	type Config struct { DryRun bool; Limit int; Lane Lane; MaxAge time.Duration }
//	   // MaxAge > 0 is REFUSED unless DryRun (C11); it widens the inbox's
//	   // sent_at fence AND the gate's stale fence for a dry-run read (V5).
//	   // Limit bounds the verdicts ACTED ON (claimed), not the rows read: a
//	   // gated verdict writes no row and stays in the inbox, so a limit over
//	   // rows read would let old gated verdicts starve newer passing ones.
//	type Stats struct { ...existing...; Gated map[string]int } // gated verdicts by reason
//	var ErrLockHeld error  // Run wraps it when 0x5157_0021 is held (both lanes);
//	                       // pipelined maps it to pipeline.ErrLockHeld
//	func InquiryOutcomes(ctx context.Context, pool *pgxpool.Pool, since time.Duration) (OutcomeCounts, error)
//	   // since 0 = all; the window is on the promotion row's created_at.
//
//	Inquiry task (C-D9), exact text:
//	  title = textmatch.NormalizedPrefix(<asker, else sender> + ": " + <ask>, 120)
//	  body  = one "key: value\n" line each, in this order, "(none)" for empty:
//	          ask_kind, asker, sender, channel, subject,
//	          sent_at (normalized_messages.sent_at, UTC, time.RFC3339),
//	          normalized_message_id, ai_extraction_id,
//	          thread_id, thread_key, thread_scope (the stored thread identity),
//	          external_message_id, verdict (the verdict's reason)
//	  project = the current attribution; assignee_type human; priority 0;
//	  status holding (O7); promotion action review; kind = ask_kind.
//	  Order (C8): claim -> create_task -> recordTask -> task_set_source_thread,
//	  all as promote:inquiry.
//
// GREENFIELD NOTE — EXPECTED RED: the surface above does not exist, so this
// file compile-FAILS the integration build; after it compiles, the suite fails
// at setup until migrations/0031_inquiry_promotion.sql is applied (iqpRequire0031).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/promote"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/textmatch"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	iqpProvider = "itest-inqp-src"
	iqpAccount  = "itest-inqp@pg-main"
	iqpModel    = "itest-inqp-model"
	iqpSubject  = "itest-inqp"
	iqpArmed    = "itest-inqp-armed"
	iqpUnarmed  = "itest-inqp-unarmed"
	iqpNoCut    = "itest-inqp-nocut"
	iqpPersonal = "itest-inqp-personal"
	iqpHuman    = "dashboard:itest-inqp"
	iqpCloser   = "opsctl:itest-inqp"
	iqpSender   = "Dana Ruiz <dana@collab.example.test>"
	iqpOurs     = "Salvador <salvador@handsonconnect.org>"
	iqpWS       = "T0ITESTIQP"
	iqpLockKey  = int64(0x5157_0021)
)

type iqpSuite struct {
	pool    *pgxpool.Pool
	ex      *executor.Executor
	now     time.Time
	armed   int64
	unarmed int64
	nocut   int64
	account int64
	rule    int64
	threads map[string]int64
}

func newIQPSuite(t *testing.T, ctx context.Context) *iqpSuite {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use an isolated compose db on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	iqpRequire0031(t, ctx, pool)
	iqpCleanup(t, ctx, pool)
	t.Cleanup(func() { iqpCleanup(t, context.Background(), pool) })

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	s := &iqpSuite{pool: pool, ex: executor.New(reg, checker, audit.NewPGStore(pool)), threads: map[string]int64{}}
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&s.now); err != nil {
		t.Fatalf("db clock: %v", err)
	}

	// THE ARMED PROJECT in collaboratory's real shape: ai_locality='any', a
	// client, ai_classify false, ai_inquiry true, and the Part C cutover one hour
	// ago. classify_promote_after stays NULL: the inquiry lane arms on ITS OWN
	// column (C-D2), and a lane that read the personal cutover would promote
	// nothing here.
	s.armed = s.id(t, ctx, `INSERT INTO projects (name, slug, client, execution, delivery, ai_locality, ai_classify,
	                                              ai_inquiry, inquiry_promote_after)
	                        VALUES ($1,$1,'LlamaSite','manual','dashboard','any',false,true, now() - interval '1 hour')
	                        RETURNING id`, iqpArmed)
	// Identical but ai_inquiry=false: dropping `AND p.ai_inquiry` makes its message promote.
	s.unarmed = s.id(t, ctx, `INSERT INTO projects (name, slug, client, execution, delivery, ai_locality, ai_classify,
	                                                ai_inquiry, inquiry_promote_after)
	                          VALUES ($1,$1,'LlamaSite','manual','dashboard','any',false,false, now() - interval '1 hour')
	                          RETURNING id`, iqpUnarmed)
	// Identical but inquiry_promote_after NULL: dropping the cutover clause makes it promote.
	s.nocut = s.id(t, ctx, `INSERT INTO projects (name, slug, client, execution, delivery, ai_locality, ai_classify,
	                                              ai_inquiry, inquiry_promote_after)
	                        VALUES ($1,$1,'LlamaSite','manual','dashboard','any',false,true, NULL)
	                        RETURNING id`, iqpNoCut)
	s.account = s.id(t, ctx, `INSERT INTO source_accounts (provider, account_email, send_enabled)
	                          VALUES ($1,$2,false) RETURNING id`, iqpProvider, iqpAccount)
	// A DISABLED jira rule, only so gate/held fixtures can name matched_rule_id
	// (0029's gate shape). Disabled: capture suites sharing the db never match it.
	s.rule = s.id(t, ctx, `INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, priority, enabled, note)
	                       VALUES ($1,'body_regex','IQP-[0-9]+','jira',1,false,'itest-inqp') RETURNING id`, s.armed)
	return s
}

func iqpRequire0031(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
	                               WHERE table_name='projects' AND column_name='inquiry_promote_after'`).Scan(&n); err != nil {
		t.Fatalf("probe projects.inquiry_promote_after: %v", err)
	}
	if n != 1 {
		t.Fatalf("projects.inquiry_promote_after does not exist; apply migrations/0031_inquiry_promotion.sql " +
			"(make migrate LOCAL_DB_URL=...)")
	}
}

func iqpCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE provider='` + iqpProvider + `')`
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`
	const msgs = `(SELECT id FROM normalized_messages WHERE raw_source_item_id IN ` + raws + `)`
	const projs = `(SELECT id FROM projects WHERE slug LIKE 'itest-inqp-%')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	const actors = `(actor LIKE 'promote:%' OR actor IN ('` + iqpHuman + `','` + iqpCloser + `'))`
	for _, q := range []string{
		`DELETE FROM classify_promotions WHERE normalized_message_id IN ` + msgs +
			` OR project_id IN ` + projs + ` OR task_id IN ` + tasksOf,
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM external_refs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM deliveries WHERE task_id IN ` + tasksOf,
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + ` OR ` + actors + `)`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf + ` OR ` + actors,
		`DELETE FROM capture_decisions WHERE message_id IN ` + msgs + ` OR project_id IN ` + projs,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM capture_rules WHERE note = 'itest-inqp' OR project_id IN ` + projs,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE subject = '` + iqpSubject + `'`,
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM ai_extractions WHERE ai_run_id IN (SELECT id FROM ai_runs WHERE model='` + iqpModel + `')`,
		`DELETE FROM ai_runs WHERE model='` + iqpModel + `'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider='` + iqpProvider + `'`,
		`DELETE FROM projects WHERE slug LIKE 'itest-inqp-%'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

// ---- small helpers ---------------------------------------------------------------

func (s *iqpSuite) id(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var v int64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return v
}

func (s *iqpSuite) count(t *testing.T, ctx context.Context, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

func (s *iqpSuite) exec(t *testing.T, ctx context.Context, q string, args ...any) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func (s *iqpSuite) ago(d time.Duration) time.Time { return s.now.Add(-d) }

func gmailKey(label string) string { return "gmail:itest-inqp:" + label }

// slackKey builds a key in the normalizer's shape (slack:{ws}:{conv}[:{root}]).
func slackKey(conv, root string) string {
	k := "slack:" + iqpWS + ":" + conv
	if root != "" {
		k += ":" + root
	}
	return k
}

type iqpMsg struct {
	label     string
	key       string // thread key; "" = no thread
	channel   string // default gmail
	direction string // default inbound
	sentAt    time.Time
	sender    string
	subject   string
}

func (s *iqpSuite) thread(t *testing.T, ctx context.Context, key string) int64 {
	t.Helper()
	if id, ok := s.threads[key]; ok {
		return id
	}
	id := s.id(t, ctx, `INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,$2,'[]') RETURNING id`,
		key, iqpSubject)
	s.threads[key] = id
	return id
}

func (s *iqpSuite) message(t *testing.T, ctx context.Context, m iqpMsg) (msg, raw int64) {
	t.Helper()
	if m.channel == "" {
		m.channel = "gmail"
	}
	if m.direction == "" {
		m.direction = "inbound"
	}
	if m.sender == "" {
		m.sender = iqpSender
		if m.direction == "outbound" {
			m.sender = iqpOurs
		}
	}
	if m.subject == "" {
		m.subject = "itest-inqp subject " + m.label
	}
	raw = s.id(t, ctx, `INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
	                    VALUES ($1,$2,'{}',$3, now()) RETURNING id`,
		s.account, "itest-inqp-"+m.label, "itest-inqp-h-"+m.label)
	var thread *int64
	if m.key != "" {
		th := s.thread(t, ctx, m.key)
		thread = &th
	}
	msg = s.id(t, ctx, `INSERT INTO normalized_messages
	                      (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
	                       body_text, subject, sender, channel)
	                    VALUES ($1,$2,$3,$4,$5,'itest-inqp body',$6,$7,$8) RETURNING id`,
		raw, thread, m.direction, "<itest-inqp-"+m.label+"@mail.example>", m.sentAt, m.subject, m.sender, m.channel)
	return msg, raw
}

// decision writes one capture_decisions row; project 0 = NULL (unmatched).
func (s *iqpSuite) decision(t *testing.T, ctx context.Context, msg int64, mode, action string, project int64) {
	t.Helper()
	var p *int64
	if project != 0 {
		p = &project
	}
	s.exec(t, ctx, `INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
	                VALUES ($1,$2,$3,$4,'itest-inqp')`, msg, mode, action, p)
}

// heldThenGate is Part D's shape: a live `held` (the live claim) resolved by a
// `gate` row that attributes (not his / expired), the LATEST decision.
func (s *iqpSuite) heldThenGate(t *testing.T, ctx context.Context, msg int64, resolve bool) {
	t.Helper()
	s.exec(t, ctx, `INSERT INTO capture_decisions (message_id, mode, matched_rule_id, project_id, action,
	                                               external_system, external_key, reason)
	                VALUES ($1,'live',$2,$3,'held','jira','IQP-1','itest-inqp held')`, msg, s.rule, s.armed)
	if resolve {
		s.exec(t, ctx, `INSERT INTO capture_decisions (message_id, mode, matched_rule_id, project_id, action,
		                                               external_system, external_key, reason)
		                VALUES ($1,'gate',$2,$3,'attributed','jira','IQP-1','itest-inqp gate: not_assigned')`,
			msg, s.rule, s.armed)
	}
}

type iqpV struct {
	worker  string // default classify_inquiry
	status  string // default ok
	noReply bool
	kind    string // default question
	asker   string // default Dana Ruiz (unless noAsker)
	noAsker bool
	ask     string
	reason  string
	scope   string // thread | conversation | none (as classify's inquiryFields records it)
	project int64  // default armed
	runAgo  time.Duration
}

// verdict writes the ai_runs + ai_extractions pair in inquiryFields' shape,
// reading the thread identity back from the message's own columns.
func (s *iqpSuite) verdict(t *testing.T, ctx context.Context, msg, raw int64, v iqpV) int64 {
	t.Helper()
	if v.worker == "" {
		v.worker = "classify_inquiry"
	}
	if v.status == "" {
		v.status = "ok"
	}
	if v.kind == "" {
		v.kind = "question"
	}
	if v.asker == "" && !v.noAsker {
		v.asker = "Dana Ruiz"
	}
	if v.ask == "" {
		v.ask = "can you confirm the rotation date?"
	}
	if v.reason == "" {
		v.reason = "asks the recipient directly"
	}
	if v.project == 0 {
		v.project = s.armed
	}
	if v.runAgo == 0 {
		v.runAgo = 10 * time.Minute
	}
	var (
		sender, subject, channel, extID, key, slug string
		threadID                                   int64
	)
	if err := s.pool.QueryRow(ctx, `
		SELECT nm.sender, nm.subject, nm.channel, nm.external_message_id, COALESCE(nm.thread_id, 0),
		       COALESCE((SELECT thread_key FROM normalized_threads WHERE id = nm.thread_id), ''),
		       (SELECT slug FROM projects WHERE id = $2)
		  FROM normalized_messages nm WHERE nm.id = $1`, msg, v.project).
		Scan(&sender, &subject, &channel, &extID, &threadID, &key, &slug); err != nil {
		t.Fatalf("read message %d for its verdict: %v", msg, err)
	}
	if v.scope == "" {
		t.Fatalf("fixture bug: verdict for message %d has no thread_scope", msg)
	}
	fields, err := json.Marshal(map[string]any{
		"needs_reply": !v.noReply, "ask_kind": v.kind, "asker": v.asker, "ask": v.ask, "reason": v.reason,
		"sender": sender, "subject": subject, "channel": channel,
		"project_id": v.project, "project_slug": slug, "normalized_message_id": msg,
		"thread_id": threadID, "thread_key": key, "thread_scope": v.scope,
		"external_message_id": extID, "context_messages": 0,
	})
	if err != nil {
		t.Fatalf("marshal fields: %v", err)
	}
	run := s.id(t, ctx, `INSERT INTO ai_runs (worker_type, provider, model, input, output, status, created_at)
	                     VALUES ($1,'itest-inqp',$2, jsonb_build_object('itest','itest-inqp'), '{}', $3, $4)
	                     RETURNING id`, v.worker, iqpModel, v.status, s.ago(v.runAgo))
	return s.id(t, ctx, `INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields) VALUES ($1,$2,$3::jsonb)
	                     RETURNING id`, run, raw, string(fields))
}

// eligible seeds a gmail question two hours old, attributed (live) to the
// armed project, with a stored verdict recorded after the cutover: the shape
// every clause test starts from.
func (s *iqpSuite) eligible(t *testing.T, ctx context.Context, label string, v iqpV) (msg, raw, extr int64) {
	t.Helper()
	msg, raw = s.message(t, ctx, iqpMsg{label: label, key: gmailKey(label), sentAt: s.ago(2 * time.Hour)})
	s.decision(t, ctx, msg, "live", "attributed", s.armed)
	if v.scope == "" {
		v.scope = "thread"
	}
	return msg, raw, s.verdict(t, ctx, msg, raw, v)
}

func (s *iqpSuite) run(t *testing.T, ctx context.Context, cfg promote.Config) promote.Stats {
	t.Helper()
	if cfg.Lane == "" {
		cfg.Lane = promote.LaneInquiry
	}
	st, err := promote.Run(ctx, s.pool, s.ex, cfg)
	if err != nil {
		t.Fatalf("promote.Run(%+v): %v", cfg, err)
	}
	return st
}

type iqpPromotion struct {
	action, kind, reason string
	project, extr        int64
	taskID, raw          *int64
}

func (s *iqpSuite) promotion(t *testing.T, ctx context.Context, msg int64) (iqpPromotion, bool) {
	t.Helper()
	var p iqpPromotion
	var reason *string
	err := s.pool.QueryRow(ctx, `SELECT action, kind, project_id, ai_extraction_id, task_id, raw_source_item_id, reason
	                               FROM classify_promotions WHERE normalized_message_id=$1`, msg).
		Scan(&p.action, &p.kind, &p.project, &p.extr, &p.taskID, &p.raw, &reason)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return p, false
		}
		t.Fatalf("read promotion for message %d: %v", msg, err)
	}
	if reason != nil {
		p.reason = *reason
	}
	return p, true
}

func (s *iqpSuite) taskOf(t *testing.T, ctx context.Context, msg int64) int64 {
	t.Helper()
	p, ok := s.promotion(t, ctx, msg)
	if !ok || p.taskID == nil {
		t.Fatalf("message %d has no promoted task (found=%v, %+v)", msg, ok, p)
	}
	return *p.taskID
}

func (s *iqpSuite) status(t *testing.T, ctx context.Context, task int64) string {
	t.Helper()
	var st string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, task).Scan(&st); err != nil {
		t.Fatalf("read task %d: %v", task, err)
	}
	return st
}

func (s *iqpSuite) execute(t *testing.T, ctx context.Context, tool, actor string, task int64, args string) {
	t.Helper()
	if _, err := s.ex.Execute(ctx, executor.Call{Tool: tool, Actor: actor, Args: []byte(args), TaskID: &task}); err != nil {
		t.Fatalf("%s(%s) as %s: %v", tool, args, actor, err)
	}
}

func (s *iqpSuite) armedTasks(t *testing.T, ctx context.Context) int {
	return s.count(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, s.armed)
}

func (s *iqpSuite) inquiryAudit(t *testing.T, ctx context.Context) int {
	return s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE actor='promote:inquiry'`)
}

func gatedTotal(st promote.Stats) int {
	n := 0
	for _, v := range st.Gated {
		n += v
	}
	return n
}

// ---- C2: the inbox, one fixture per clause ----------------------------------------

// MUTATIONS (each turns exactly one fixture's assertion red):
//   - worker_type 'classify_inquiry' → any lane: personal/residue promote;
//   - drop `r.status='ok'`: errrun promotes;
//   - drop needs_reply: noreply promotes;
//   - drop `nm.direction='inbound'`: outbound promotes;
//   - read `latest.action <> 'unmatched'` instead of `= 'attributed'`: task,
//     task_log and held promote; filter the latest row on mode='live' (or read
//     the live row): shadowlatest and gatelatest vanish;
//   - drop `p.ai_inquiry`: unarmed promotes; drop the cutover: nocut and precut promote;
//   - drop `sent_at >= now() - InquiryMaxAge`: old reaches the gate as `stale`
//     (Gated["stale"] goes non-zero — a fence the INBOX owns, C-D6);
//   - drop the NOT EXISTS on classify_promotions: claimed gets a second attempt.
func TestPromoteInquiry_Integration_InboxOneFixturePerClause(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)

	eligible := map[string]int64{}
	excluded := map[string]int64{}

	m, _, _ := s.eligible(t, ctx, "base", iqpV{})
	eligible["base: live attributed"] = m

	// Latest attributed in ANY mode.
	m, r := s.message(t, ctx, iqpMsg{label: "shadowlatest", key: gmailKey("shadowlatest"), sentAt: s.ago(2 * time.Hour)})
	s.decision(t, ctx, m, "shadow", "attributed", s.armed)
	s.verdict(t, ctx, m, r, iqpV{scope: "thread"})
	eligible["shadowlatest: the latest row is a shadow attribution"] = m

	m, r = s.message(t, ctx, iqpMsg{label: "gatelatest", key: gmailKey("gatelatest"), sentAt: s.ago(2 * time.Hour)})
	s.heldThenGate(t, ctx, m, true)
	s.verdict(t, ctx, m, r, iqpV{scope: "thread"})
	eligible["gatelatest: a gate resolution (mode='gate') attributed it"] = m

	// worker_type / status / needs_reply.
	m, _, _ = s.eligible(t, ctx, "personalworker", iqpV{worker: "classify"})
	excluded["personalworker: a personal-lane (worker_type classify) verdict"] = m
	m, _, _ = s.eligible(t, ctx, "residueworker", iqpV{worker: "classify_residue"})
	excluded["residueworker: a residue-lane verdict"] = m
	m, _, _ = s.eligible(t, ctx, "errrun", iqpV{status: "error"})
	excluded["errrun: ai_runs.status='error'"] = m
	m, _, _ = s.eligible(t, ctx, "noreply", iqpV{noReply: true})
	excluded["noreply: needs_reply=false"] = m

	// inbound.
	m, r = s.message(t, ctx, iqpMsg{label: "outbound", key: gmailKey("outbound"), direction: "outbound", sentAt: s.ago(2 * time.Hour)})
	s.decision(t, ctx, m, "live", "attributed", s.armed)
	s.verdict(t, ctx, m, r, iqpV{scope: "thread"})
	excluded["outbound: our own send"] = m

	// The latest decision must be `attributed`.
	for _, a := range []string{"task", "task_log"} {
		m, r = s.message(t, ctx, iqpMsg{label: "latest" + a, key: gmailKey("latest" + a), sentAt: s.ago(2 * time.Hour)})
		s.decision(t, ctx, m, "shadow", "attributed", s.armed)
		s.decision(t, ctx, m, "shadow", a, s.armed)
		s.verdict(t, ctx, m, r, iqpV{scope: "thread"})
		excluded["latest"+a+": the latest decision already produced a task"] = m
	}
	m, r = s.message(t, ctx, iqpMsg{label: "latestheld", key: gmailKey("latestheld"), sentAt: s.ago(2 * time.Hour)})
	s.heldThenGate(t, ctx, m, false)
	s.verdict(t, ctx, m, r, iqpV{scope: "thread"})
	excluded["latestheld: an unresolved hold (its fate is pending)"] = m
	m, r = s.message(t, ctx, iqpMsg{label: "latestunmatched", key: gmailKey("latestunmatched"), sentAt: s.ago(2 * time.Hour)})
	s.decision(t, ctx, m, "shadow", "attributed", s.armed)
	s.decision(t, ctx, m, "shadow", "unmatched", 0)
	s.verdict(t, ctx, m, r, iqpV{scope: "thread"})
	excluded["latestunmatched: attributed once, unmatched LATEST"] = m

	// The project: ai_inquiry, the cutover, and the verdict clock.
	m, r = s.message(t, ctx, iqpMsg{label: "unarmed", key: gmailKey("unarmed"), sentAt: s.ago(2 * time.Hour)})
	s.decision(t, ctx, m, "live", "attributed", s.unarmed)
	s.verdict(t, ctx, m, r, iqpV{scope: "thread", project: s.unarmed})
	excluded["unarmed: ai_inquiry=false"] = m
	m, r = s.message(t, ctx, iqpMsg{label: "nocut", key: gmailKey("nocut"), sentAt: s.ago(2 * time.Hour)})
	s.decision(t, ctx, m, "live", "attributed", s.nocut)
	s.verdict(t, ctx, m, r, iqpV{scope: "thread", project: s.nocut})
	excluded["nocut: inquiry_promote_after IS NULL"] = m
	m, _, _ = s.eligible(t, ctx, "precut", iqpV{runAgo: 2 * time.Hour})
	excluded["precut: the verdict was recorded BEFORE inquiry_promote_after (forward-only on the verdict clock)"] = m

	// sent_at >= now() - InquiryMaxAge.
	m, r = s.message(t, ctx, iqpMsg{label: "old", key: gmailKey("old"), sentAt: s.ago(73 * time.Hour)})
	s.decision(t, ctx, m, "live", "attributed", s.armed)
	s.verdict(t, ctx, m, r, iqpV{scope: "thread"})
	excluded["old: sent 73h ago, past InquiryMaxAge"] = m

	// No promotion row.
	claimed, claimedRaw, claimedExtr := s.eligible(t, ctx, "claimed", iqpV{})
	s.exec(t, ctx, `INSERT INTO classify_promotions (normalized_message_id, raw_source_item_id, ai_extraction_id,
	                                                 project_id, kind, action, task_id, reason)
	                VALUES ($1,$2,$3,$4,'question','review',NULL,'itest-inqp: crashed between claim and create_task')`,
		claimed, claimedRaw, claimedExtr, s.armed)

	st := s.run(t, ctx, promote.Config{})

	for why, msg := range eligible {
		p, ok := s.promotion(t, ctx, msg)
		if !ok || p.taskID == nil || p.action != "review" {
			t.Errorf("%s: want a review promotion with a task, got found=%v %+v (C2)", why, ok, p)
		}
	}
	for why, msg := range excluded {
		if p, ok := s.promotion(t, ctx, msg); ok {
			t.Errorf("%s: promoted (%+v); C2's inbox must exclude it", why, p)
		}
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM classify_promotions WHERE normalized_message_id=$1`, claimed); n != 1 {
		t.Errorf("the crash artifact's message has %d promotion rows, want exactly the seeded 1", n)
	}
	if p, _ := s.promotion(t, ctx, claimed); p.taskID != nil {
		t.Errorf("the crash artifact was completed with task %d; a later pass never completes a claim it did not make", *p.taskID)
	}
	if st.Review != 3 || st.Created != 0 {
		t.Errorf("stats = %+v, want Review 3 (base, shadowlatest, gatelatest), Created 0 (O7: holding)", st)
	}
	if n := gatedTotal(st); n != 0 {
		t.Errorf("Gated = %v; every exclusion above is an INBOX clause, never a gate reason — in particular the "+
			"73h-old message is fenced by the inbox (C-D6's second fence), not counted `stale`", st.Gated)
	}
	if n := s.armedTasks(t, ctx); n != 3 {
		t.Errorf("%d tasks in the armed project, want 3", n)
	}
}

// C2: "latest action='attributed' in any mode, including route and gate (a
// fixture each)". Part C shipped this fixture skipping until Part B's
// migration existed. SWT-40 Part B made the `route` mode real
// (migrations/0032_route_tier.sql, the SPEC's 0029), so the skip is now a
// FAILURE: a database without 0032 is a database this suite cannot vouch for.
// B6: after a route row, the inquiry promotion inbox sees the message.
func TestPromoteInquiry_Integration_RouteAttributionIsFollowed(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	var def string
	if err := s.pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
	                                 WHERE conname='capture_decisions_mode_check'`).Scan(&def); err != nil {
		t.Fatalf("read capture_decisions_mode_check: %v", err)
	}
	if !strings.Contains(def, "route") {
		t.Fatalf("capture_decisions_mode_check does not admit 'route' (%s): migrations/0032_route_tier.sql (SWT-40 "+
			"Part B) is not applied to this database. `make migrate` against it first", def)
	}
	m, r := s.message(t, ctx, iqpMsg{label: "routed", key: gmailKey("routed"), sentAt: s.ago(2 * time.Hour)})
	s.decision(t, ctx, m, "live", "unmatched", 0)
	s.exec(t, ctx, `INSERT INTO capture_decisions (message_id, mode, action, project_id, route_step, reason)
	                VALUES ($1,'route','attributed',$2,'default','itest-inqp routed')`, m, s.armed)
	s.verdict(t, ctx, m, r, iqpV{scope: "thread"})
	s.run(t, ctx, promote.Config{})
	if p, ok := s.promotion(t, ctx, m); !ok || p.action != "review" {
		t.Errorf("a message attributed by the route tier (mode='route', latest) was not promoted (found=%v %+v)", ok, p)
	}
}

// "Oldest first": the older ask on a thread creates the task, the newer one
// attaches — so the task names the FIRST ask.
func TestPromoteInquiry_Integration_OldestFirst(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	key := gmailKey("oldest")
	a, ar := s.message(t, ctx, iqpMsg{label: "oldest-a", key: key, sentAt: s.ago(3 * time.Hour)})
	s.decision(t, ctx, a, "live", "attributed", s.armed)
	s.verdict(t, ctx, a, ar, iqpV{scope: "thread", asker: "Ann", ask: "first ask", runAgo: 20 * time.Minute})
	b, br := s.message(t, ctx, iqpMsg{label: "oldest-b", key: key, sentAt: s.ago(2 * time.Hour)})
	s.decision(t, ctx, b, "live", "attributed", s.armed)
	s.verdict(t, ctx, b, br, iqpV{scope: "thread", asker: "Bob", ask: "second ask", runAgo: 10 * time.Minute})

	s.run(t, ctx, promote.Config{})
	pa, _ := s.promotion(t, ctx, a)
	pb, _ := s.promotion(t, ctx, b)
	if pa.action != "review" || pb.action != "attached" || pa.taskID == nil || pb.taskID == nil || *pa.taskID != *pb.taskID {
		t.Fatalf("oldest-first: a=%+v b=%+v, want a creates (review) and b attaches to the same task", pa, pb)
	}
	var title string
	s.pool.QueryRow(ctx, `SELECT title FROM tasks WHERE id=$1`, *pa.taskID).Scan(&title)
	if title != "Ann: first ask" {
		t.Errorf("task title = %q, want %q — the OLDEST verdict creates", title, "Ann: first ask")
	}
}

// Cross-lane controls (C1, C2): each lane promotes only its own verdicts, as its
// own actor.
func TestPromoteInquiry_Integration_CrossLaneControls(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	personal := s.id(t, ctx, `INSERT INTO projects (name, slug, client, execution, delivery, ai_locality, ai_classify,
	                                                 ai_inquiry, classify_promote_after)
	                           VALUES ($1,$1,NULL,'manual','dashboard','local_only',true,false, now() - interval '1 hour')
	                           RETURNING id`, iqpPersonal)
	pm, pr := s.message(t, ctx, iqpMsg{label: "personal", key: gmailKey("personal"), sentAt: s.ago(2 * time.Hour)})
	s.decision(t, ctx, pm, "live", "attributed", personal)
	run := s.id(t, ctx, `INSERT INTO ai_runs (worker_type, provider, model, input, output, status, created_at)
	                     VALUES ('classify','itest-inqp',$1, jsonb_build_object('itest','itest-inqp'), '{}','ok', now() - interval '10 minutes')
	                     RETURNING id`, iqpModel)
	s.exec(t, ctx, `INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields)
	                VALUES ($1,$2, jsonb_build_object('actionable',true,'kind','payment_due','title','Pay the bill',
	                  'reason','itest','sender','x','subject','y','project_id',$3::bigint,'normalized_message_id',$4::bigint,
	                  'link_candidates',0))`, run, pr, personal, pm)
	im, _, _ := s.eligible(t, ctx, "inquiry", iqpV{})

	s.run(t, ctx, promote.Config{Lane: promote.LaneInquiry})
	if _, ok := s.promotion(t, ctx, pm); ok {
		t.Errorf("the INQUIRY lane promoted a personal-lane verdict")
	}
	if p, ok := s.promotion(t, ctx, im); !ok || p.action != "review" {
		t.Fatalf("the inquiry lane did not promote its own verdict (found=%v %+v)", ok, p)
	}

	if _, err := promote.Run(ctx, s.pool, s.ex, promote.Config{}); err != nil {
		t.Fatalf("personal promote.Run: %v", err)
	}
	if p, ok := s.promotion(t, ctx, pm); !ok || p.action != "task" {
		t.Errorf("positive control: the PERSONAL lane (Config{} — the CronJob's default) did not promote its "+
			"whitelisted verdict (found=%v %+v)", ok, p)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM classify_promotions WHERE normalized_message_id=$1`, im); n != 1 {
		t.Errorf("the personal lane touched the inquiry message (%d rows)", n)
	}
	task := s.taskOf(t, ctx, im)
	if n := s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND actor <> 'promote:inquiry'
	                          AND actor LIKE 'promote:%'`, task); n != 0 {
		t.Errorf("the inquiry task carries %d audit rows from another promote actor", n)
	}
	ptask := s.taskOf(t, ctx, pm)
	if n := s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND actor='promote:inquiry'`, ptask); n != 0 {
		t.Errorf("the personal task carries %d promote:inquiry audit rows", n)
	}
	s.exec(t, ctx, `DELETE FROM capture_decisions WHERE project_id=$1`, personal)
}

// ---- C3/C4: addressed to Salvador, and the replied-since block -------------------

// MUTATIONS: count ANY direction as prior participation → rootedinboundprior
// promotes; `<=` instead of `<` → rootedtie promotes; drop the DM clause → dm
// is not_addressed; accept `G…` → groupdm promotes; read the prior post in
// conversation scope → nothing here changes but the pure test's
// "not addressed EVEN after he spoke there" goes red.
func TestPromoteInquiry_Integration_AddressedToSalvador(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	ask := s.ago(2 * time.Hour)

	slackAsk := func(label, key, scope string) int64 {
		m, r := s.message(t, ctx, iqpMsg{label: label, key: key, channel: "slack", sentAt: ask})
		s.decision(t, ctx, m, "live", "attributed", s.armed)
		s.verdict(t, ctx, m, r, iqpV{scope: scope})
		return m
	}
	post := func(label, key, direction string, at time.Time) {
		s.message(t, ctx, iqpMsg{label: label, key: key, channel: "slack", direction: direction, sentAt: at})
	}

	dm := slackAsk("dm", slackKey("D0IQPDM1", ""), "conversation")
	channel := slackAsk("channel", slackKey("C0IQPCH1", ""), "conversation")
	rp := slackKey("C0IQPCH2", "p100")
	post("rootedprior-ours", rp, "outbound", ask.Add(-time.Hour))
	rootedPrior := slackAsk("rootedprior", rp, "thread")
	ra := slackKey("C0IQPCH3", "p200")
	rootedAfter := slackAsk("rootedafter", ra, "thread")
	post("rootedafter-ours", ra, "outbound", ask.Add(time.Hour))
	groupDM := slackAsk("groupdm", slackKey("G0IQPGD1", ""), "conversation")
	gm, _, _ := s.eligible(t, ctx, "gmail", iqpV{})
	ri := slackKey("C0IQPCH4", "p300")
	post("rootedinbound-theirs", ri, "inbound", ask.Add(-time.Hour))
	rootedInbound := slackAsk("rootedinbound", ri, "thread")
	rt := slackKey("C0IQPCH5", "p400")
	rootedTie := slackAsk("rootedtie", rt, "thread")
	post("rootedtie-ours", rt, "outbound", ask)
	dr := slackKey("D0IQPDM2", "")
	dmReplied := slackAsk("dmreplied", dr, "conversation")
	post("dmreplied-ours", dr, "outbound", ask.Add(time.Hour))

	st := s.run(t, ctx, promote.Config{})

	for name, m := range map[string]int64{"1:1 DM": dm, "rooted thread he posted in BEFORE": rootedPrior, "gmail": gm} {
		if p, ok := s.promotion(t, ctx, m); !ok || p.action != "review" {
			t.Errorf("%s: not promoted (found=%v %+v); C-D3 addresses it to Salvador", name, ok, p)
		}
	}
	for name, m := range map[string]int64{
		"top-level channel (conversation scope)":                           channel,
		"group DM (C-D4 excludes it)":                                      groupDM,
		"rooted thread where only SOMEONE ELSE posted before (direction)":  rootedInbound,
		"rooted thread where his post is at the SAME instant (strictness)": rootedTie,
		"rooted thread he answered AFTER the ask":                          rootedAfter,
		"DM he spoke in after the ask (spoke in conversation since)":       dmReplied,
	} {
		if p, ok := s.promotion(t, ctx, m); ok {
			t.Errorf("%s: promoted (%+v); a gated verdict writes no row", name, p)
		}
	}
	if st.Gated["not_addressed"] != 4 {
		t.Errorf("Gated[not_addressed] = %d, want 4 (channel, group DM, inbound-only prior, tie); Gated = %v",
			st.Gated["not_addressed"], st.Gated)
	}
	if st.Gated["answered"] != 2 {
		t.Errorf("Gated[answered] = %d, want 2 (a thread reply and a DM spoken in since — C-D7: ANY replied-since "+
			"state blocks); Gated = %v", st.Gated["answered"], st.Gated)
	}
}

// C-D10: the stored thread no longer matches the message's current one.
func TestPromoteInquiry_Integration_RethreadedIsGated(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	m, _, _ := s.eligible(t, ctx, "rethreaded", iqpV{})
	other := s.thread(t, ctx, gmailKey("rethreaded-elsewhere"))
	s.exec(t, ctx, `UPDATE normalized_messages SET thread_id=$2 WHERE id=$1`, m, other)
	st := s.run(t, ctx, promote.Config{})
	if _, ok := s.promotion(t, ctx, m); ok {
		t.Errorf("a verdict whose stored thread_id no longer matches normalized_messages.thread_id was promoted")
	}
	if st.Gated["rethreaded"] != 1 {
		t.Errorf("Gated = %v, want rethreaded=1 (C-D10)", st.Gated)
	}
}

// C-D5: fyi asks nothing (and drops SWT-33's fyi+needs_reply contradiction).
func TestPromoteInquiry_Integration_AskKindWhitelist(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	fyi, _, _ := s.eligible(t, ctx, "fyi", iqpV{kind: "fyi"})
	sched, _, _ := s.eligible(t, ctx, "sched", iqpV{kind: "scheduling"})
	st := s.run(t, ctx, promote.Config{})
	if _, ok := s.promotion(t, ctx, fyi); ok {
		t.Errorf("an fyi+needs_reply verdict was promoted")
	}
	if p, ok := s.promotion(t, ctx, sched); !ok || p.kind != "scheduling" {
		t.Errorf("a scheduling ask was not promoted with kind=scheduling (found=%v %+v)", ok, p)
	}
	if st.Gated["kind"] != 1 {
		t.Errorf("Gated = %v, want kind=1", st.Gated)
	}
}

// ---- C8: create order, task shape, exact text ---------------------------------------

func TestPromoteInquiry_Integration_CreateShapeOrderAndExactText(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	const (
		sender  = "Katie Byeluri <katie@rochester.example.test>"
		subject = "Activities Integration – Request and Response Validation"
		reason  = "asks Salvador to confirm the request shape before Friday"
	)
	ask := "Can you confirm whether the Activities integration should validate both the request and the response " +
		"payloads, or only the response, before we ship on Friday?"
	key := gmailKey("create")
	m, r := s.message(t, ctx, iqpMsg{label: "create", key: key, sentAt: s.ago(2 * time.Hour), sender: sender, subject: subject})
	s.decision(t, ctx, m, "live", "attributed", s.armed)
	extr := s.verdict(t, ctx, m, r, iqpV{scope: "thread", asker: "Katie Byeluri", ask: ask, reason: reason})
	// The sender fallback.
	m2, r2 := s.message(t, ctx, iqpMsg{label: "noasker", key: gmailKey("noasker"), sentAt: s.ago(2 * time.Hour), sender: sender, subject: subject})
	s.decision(t, ctx, m2, "live", "attributed", s.armed)
	s.verdict(t, ctx, m2, r2, iqpV{scope: "thread", noAsker: true, ask: "short ask", reason: reason})

	s.run(t, ctx, promote.Config{})

	p, ok := s.promotion(t, ctx, m)
	if !ok || p.taskID == nil {
		t.Fatalf("no promoted task for the create fixture (found=%v %+v)", ok, p)
	}
	if p.action != "review" || p.kind != "question" || p.project != s.armed || p.extr != extr || p.raw == nil || *p.raw != r {
		t.Errorf("promotion row = %+v, want action review (O7), kind question (ask_kind), project %d, extraction %d, raw %d",
			p, s.armed, extr, r)
	}
	task := *p.taskID

	var (
		title, body, status, assignee string
		project                       int64
		priority                      int
		srcThread                     *int64
	)
	if err := s.pool.QueryRow(ctx, `SELECT title, body, status, assignee_type, project_id, priority, source_thread_id
	                                  FROM tasks WHERE id=$1`, task).
		Scan(&title, &body, &status, &assignee, &project, &priority, &srcThread); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if status != "holding" {
		t.Errorf("task status = %q, want holding (O7: inquiryCreateStatus)", status)
	}
	if assignee != "human" || priority != 0 || project != s.armed {
		t.Errorf("task = {assignee %q, priority %d, project %d}, want {human, 0, %d} (C-D9)", assignee, priority, project, s.armed)
	}
	if srcThread == nil || *srcThread != s.threads[key] {
		t.Errorf("task source_thread_id = %v, want %d (task_set_source_thread, C-D9)", srcThread, s.threads[key])
	}

	wantTitle := textmatch.NormalizedPrefix("Katie Byeluri: "+ask, 120)
	if title != wantTitle {
		t.Errorf("title = %q\nwant    %q\n(C-D9: {asker, else sender}: {ask} via textmatch.NormalizedPrefix(…,120))", title, wantTitle)
	}
	var sentAt time.Time
	if err := s.pool.QueryRow(ctx, `SELECT sent_at FROM normalized_messages WHERE id=$1`, m).Scan(&sentAt); err != nil {
		t.Fatalf("read sent_at: %v", err)
	}
	wantBody := "ask_kind: question\n" +
		"asker: Katie Byeluri\n" +
		"sender: " + sender + "\n" +
		"channel: gmail\n" +
		"subject: " + subject + "\n" +
		"sent_at: " + sentAt.UTC().Format(time.RFC3339) + "\n" +
		"normalized_message_id: " + strconv.FormatInt(m, 10) + "\n" +
		"ai_extraction_id: " + strconv.FormatInt(extr, 10) + "\n" +
		"thread_id: " + strconv.FormatInt(s.threads[key], 10) + "\n" +
		"thread_key: " + key + "\n" +
		"thread_scope: thread\n" +
		"external_message_id: <itest-inqp-create@mail.example>\n" +
		"verdict: " + reason + "\n"
	if body != wantBody {
		t.Errorf("body =\n%s\nwant\n%s(C-D9's deterministic body, exact text)", body, wantBody)
	}

	t2 := s.taskOf(t, ctx, m2)
	var title2, body2 string
	s.pool.QueryRow(ctx, `SELECT title, body FROM tasks WHERE id=$1`, t2).Scan(&title2, &body2)
	if want := textmatch.NormalizedPrefix(sender+": short ask", 120); title2 != want {
		t.Errorf("no-asker title = %q, want %q (the sender fallback)", title2, want)
	}
	if !strings.Contains(body2, "\nasker: (none)\n") {
		t.Errorf("no-asker body does not carry `asker: (none)`:\n%s", body2)
	}

	// C8: create_task -> task_set_source_thread, both as promote:inquiry, both ok,
	// and the executor's audit rows are the evidence (invariant 3).
	rows, err := s.pool.Query(ctx, `SELECT tool, status, args FROM audit_events
	                                 WHERE actor='promote:inquiry'
	                                   AND ((tool='create_task' AND args->>'title' = $1)
	                                     OR (tool<>'create_task' AND task_id = $2))
	                                 ORDER BY id`, title, task)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	var seq []string
	for rows.Next() {
		var tool, st string
		var args []byte
		if err := rows.Scan(&tool, &st, &args); err != nil {
			t.Fatalf("scan audit: %v", err)
		}
		if st != "ok" {
			t.Errorf("audit %s status = %q, want ok", tool, st)
		}
		if tool == "create_task" && !strings.Contains(string(args), `"status": "holding"`) &&
			!strings.Contains(string(args), `"status":"holding"`) {
			t.Errorf("create_task args %s do not carry status holding", args)
		}
		seq = append(seq, tool)
	}
	rows.Close()
	if strings.Join(seq, ",") != "create_task,task_set_source_thread" {
		t.Errorf("promote:inquiry audit sequence for the task = %v, want [create_task task_set_source_thread] (C8)", seq)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE actor='promote:classify'`); n != 0 {
		t.Errorf("%d promote:classify audit rows from an inquiry pass; the lane's actor is promote:inquiry", n)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM deliveries WHERE task_id=$1`, task); n != 0 {
		t.Errorf("an inquiry task has %d deliveries; nothing sends (invariant 4)", n)
	}
}

// ---- C7: Decide's branches, each against the database -------------------------------

func TestPromoteInquiry_Integration_AttachesToTheOpenTask(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	key := gmailKey("attach")
	a, ar := s.message(t, ctx, iqpMsg{label: "attach-a", key: key, sentAt: s.ago(3 * time.Hour)})
	s.decision(t, ctx, a, "live", "attributed", s.armed)
	s.verdict(t, ctx, a, ar, iqpV{scope: "thread"})
	s.run(t, ctx, promote.Config{})
	task := s.taskOf(t, ctx, a)

	b, br := s.message(t, ctx, iqpMsg{label: "attach-b", key: key, sentAt: s.ago(2 * time.Hour)})
	s.decision(t, ctx, b, "live", "attributed", s.armed)
	s.verdict(t, ctx, b, br, iqpV{scope: "thread", ask: "and the second question?"})
	st := s.run(t, ctx, promote.Config{})

	p, ok := s.promotion(t, ctx, b)
	if !ok || p.action != "attached" || p.taskID == nil || *p.taskID != task {
		t.Fatalf("second ask on the thread: %+v (found=%v), want attached to task %d", p, ok, task)
	}
	if st.Attached != 1 || s.armedTasks(t, ctx) != 1 {
		t.Errorf("stats %+v / %d tasks, want one attach and no second task", st, s.armedTasks(t, ctx))
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE actor='promote:inquiry' AND tool='task_append_log'
	                          AND task_id=$1`, task); n != 1 {
		t.Errorf("%d task_append_log calls as promote:inquiry on task %d, want 1", n, task)
	}
	if st := s.status(t, ctx, task); st != "holding" {
		t.Errorf("an attach changed the task's status to %q", st)
	}
}

func TestPromoteInquiry_Integration_ReopensADismissedTask(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	key := gmailKey("reopen")
	a, ar := s.message(t, ctx, iqpMsg{label: "reopen-a", key: key, sentAt: s.ago(3 * time.Hour)})
	s.decision(t, ctx, a, "live", "attributed", s.armed)
	s.verdict(t, ctx, a, ar, iqpV{scope: "thread"})
	s.run(t, ctx, promote.Config{})
	task := s.taskOf(t, ctx, a)
	s.execute(t, ctx, "task_dismiss", iqpHuman, task, fmt.Sprintf(`{"task_id":%d,"reason_code":"not_actionable"}`, task))
	var dismissal int64
	s.pool.QueryRow(ctx, `SELECT id FROM task_dismissals WHERE task_id=$1`, task).Scan(&dismissal)

	// Ingested AFTER the dismissal (created_at = now()), sent 90 minutes ago (past grace).
	b, br := s.message(t, ctx, iqpMsg{label: "reopen-b", key: key, sentAt: s.ago(90 * time.Minute)})
	s.decision(t, ctx, b, "live", "attributed", s.armed)
	s.verdict(t, ctx, b, br, iqpV{scope: "thread", ask: "still waiting on this"})
	st := s.run(t, ctx, promote.Config{})

	p, ok := s.promotion(t, ctx, b)
	if !ok || p.action != "attached" || p.taskID == nil || *p.taskID != task {
		t.Fatalf("new ask on a dismissed task's thread: %+v (found=%v), want attached to %d (SWT-36 D8)", p, ok, task)
	}
	if st.Reopened != 1 {
		t.Errorf("Stats.Reopened = %d, want 1", st.Reopened)
	}
	if got := s.status(t, ctx, task); got != "holding" {
		t.Errorf("reopened task status = %q, want holding (closed_from_status)", got)
	}
	var by string
	var byMsg *int64
	s.pool.QueryRow(ctx, `SELECT COALESCE(reopened_by,''), reopened_by_message_id FROM task_dismissals WHERE id=$1`, dismissal).
		Scan(&by, &byMsg)
	if by != "promote:inquiry" || byMsg == nil || *byMsg != b {
		t.Errorf("dismissal %d reopened_by=%q by_message=%v, want promote:inquiry / %d (the guarded task_reopen)", dismissal, by, byMsg, b)
	}
	if s.armedTasks(t, ctx) != 1 {
		t.Errorf("a second task was created on a dismissed thread")
	}
}

func TestPromoteInquiry_Integration_ClosedTaskYieldsANewHoldingTask(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	key := gmailKey("q3")
	a, ar := s.message(t, ctx, iqpMsg{label: "q3-a", key: key, sentAt: s.ago(3 * time.Hour)})
	s.decision(t, ctx, a, "live", "attributed", s.armed)
	s.verdict(t, ctx, a, ar, iqpV{scope: "thread"})
	s.run(t, ctx, promote.Config{})
	first := s.taskOf(t, ctx, a)
	s.execute(t, ctx, "task_close", iqpCloser, first, fmt.Sprintf(`{"task_id":%d,"reason":"itest-inqp done"}`, first))

	b, br := s.message(t, ctx, iqpMsg{label: "q3-b", key: key, sentAt: s.ago(2 * time.Hour)})
	s.decision(t, ctx, b, "live", "attributed", s.armed)
	s.verdict(t, ctx, b, br, iqpV{scope: "thread", ask: "a new question"})
	s.run(t, ctx, promote.Config{})

	p, ok := s.promotion(t, ctx, b)
	if !ok || p.action != "review" || p.taskID == nil || *p.taskID == first {
		t.Fatalf("an ask past a plain-closed task: %+v (found=%v), want a NEW review task (Q3)", p, ok)
	}
	if s.status(t, ctx, *p.taskID) != "holding" {
		t.Errorf("the Q3 task is %q, want holding", s.status(t, ctx, *p.taskID))
	}
	if !strings.Contains(p.reason, strconv.FormatInt(first, 10)) {
		t.Errorf("promotion reason %q does not name the closed task %d", p.reason, first)
	}
}

// ---- C9: claim-before-act, run-twice, Lost ----------------------------------------

// The claim is inserted BEFORE the executor call: an executor that cannot
// create a task leaves the claim (task_id NULL) and no task, and a later pass
// with a working executor never completes it.
func TestPromoteInquiry_Integration_ClaimBeforeAct(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	m, _, _ := s.eligible(t, ctx, "claim", iqpV{})
	broken := executor.New(executor.NewRegistry(),
		policy.NewMatrix(policy.NewPGSnapshotLoader(s.pool), policy.NewStatic()), audit.NewPGStore(s.pool))
	if _, err := promote.Run(ctx, s.pool, broken, promote.Config{Lane: promote.LaneInquiry}); err == nil {
		t.Fatalf("a pass whose executor has no create_task returned nil")
	}
	p, ok := s.promotion(t, ctx, m)
	if !ok || p.taskID != nil || p.action != "review" {
		t.Errorf("after a failed create_task: promotion %+v (found=%v), want the CLAIM (action review, task_id NULL) — "+
			"it is written before the executor call (C9)", p, ok)
	}
	if s.armedTasks(t, ctx) != 0 {
		t.Errorf("a task exists although create_task failed")
	}
	s.run(t, ctx, promote.Config{})
	if p2, _ := s.promotion(t, ctx, m); p2.taskID != nil {
		t.Errorf("a later pass completed the crash artifact with task %d", *p2.taskID)
	}
	if s.armedTasks(t, ctx) != 0 {
		t.Errorf("a later pass created the task the crashed pass may or may not have created")
	}
}

func TestPromoteInquiry_Integration_RunTwiceWritesNothing(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	key := gmailKey("twice")
	a, ar := s.message(t, ctx, iqpMsg{label: "twice-a", key: key, sentAt: s.ago(3 * time.Hour)})
	s.decision(t, ctx, a, "live", "attributed", s.armed)
	s.verdict(t, ctx, a, ar, iqpV{scope: "thread"})
	b, br := s.message(t, ctx, iqpMsg{label: "twice-b", key: key, sentAt: s.ago(2 * time.Hour)})
	s.decision(t, ctx, b, "live", "attributed", s.armed)
	s.verdict(t, ctx, b, br, iqpV{scope: "thread"})
	s.eligible(t, ctx, "twice-fyi", iqpV{kind: "fyi"})

	s.run(t, ctx, promote.Config{})
	snap := func() [4]int {
		return [4]int{
			s.count(t, ctx, `SELECT count(*) FROM classify_promotions WHERE project_id=$1`, s.armed),
			s.armedTasks(t, ctx),
			s.count(t, ctx, `SELECT count(*) FROM task_events WHERE task_id IN (SELECT id FROM tasks WHERE project_id=$1)`, s.armed),
			s.inquiryAudit(t, ctx),
		}
	}
	before := snap()
	st := s.run(t, ctx, promote.Config{})
	if after := snap(); after != before {
		t.Errorf("second pass changed [promotions tasks task_events audit] %v -> %v; run-twice writes nothing (C9)", before, after)
	}
	if st.Review+st.Created+st.Attached != 0 {
		t.Errorf("second pass stats %+v, want nothing acted", st)
	}
}

// Lost: two verdicts for ONE message in one inbox read — the first claims, the
// second loses the claim and acts on nothing. A message a PERSONAL-lane
// promotion already claimed never reaches the inquiry claim at all: both lanes
// take 0x5157_0021, so they cannot race, and the inbox's `no promotion row`
// clause excludes it (SPEC ambiguity resolved, see the report).
func TestPromoteInquiry_Integration_LostClaimsAndThePersonalClaim(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	dup, dupRaw, _ := s.eligible(t, ctx, "dup", iqpV{runAgo: 20 * time.Minute})
	s.verdict(t, ctx, dup, dupRaw, iqpV{scope: "thread", runAgo: 10 * time.Minute, ask: "re-classified"})

	pm, pr, _ := s.eligible(t, ctx, "personalclaimed", iqpV{})
	pextr := s.verdict(t, ctx, pm, pr, iqpV{worker: "classify", scope: "thread"})
	s.exec(t, ctx, `INSERT INTO classify_promotions (normalized_message_id, raw_source_item_id, ai_extraction_id,
	                                                 project_id, kind, action, reason)
	                VALUES ($1,$2,$3,$4,'payment_due','review','itest-inqp: the personal lane claimed it')`,
		pm, pr, pextr, s.armed)

	st := s.run(t, ctx, promote.Config{})
	if st.Lost != 1 || st.Review != 1 {
		t.Errorf("stats %+v, want Lost 1 (the second verdict for `dup`) and Review 1", st)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM classify_promotions WHERE normalized_message_id=$1`, dup); n != 1 {
		t.Errorf("`dup` has %d promotion rows, want 1 (one row per message, forever)", n)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM classify_promotions WHERE normalized_message_id=$1`, pm); n != 1 {
		t.Errorf("the personal-claimed message has %d promotion rows, want the personal 1", n)
	}
	if s.armedTasks(t, ctx) != 1 {
		t.Errorf("%d tasks, want 1 (dup's)", s.armedTasks(t, ctx))
	}
}

// ---- C10: gated verdicts write no row and call no tool; the grace release -------------

func TestPromoteInquiry_Integration_GatedVerdictsWriteNoRowAndCallNoTool(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	gated := map[string]int64{}

	m, r := s.message(t, ctx, iqpMsg{label: "g-pending", key: gmailKey("g-pending"), sentAt: s.ago(10 * time.Minute)})
	s.decision(t, ctx, m, "live", "attributed", s.armed)
	s.verdict(t, ctx, m, r, iqpV{scope: "thread"})
	gated["pending"] = m

	m, _, _ = s.eligible(t, ctx, "g-answered", iqpV{})
	s.message(t, ctx, iqpMsg{label: "g-answered-ours", key: gmailKey("g-answered"), direction: "outbound", sentAt: s.ago(time.Hour)})
	gated["answered"] = m

	m, r = s.message(t, ctx, iqpMsg{label: "g-channel", key: slackKey("C0IQPGATE", ""), channel: "slack", sentAt: s.ago(2 * time.Hour)})
	s.decision(t, ctx, m, "live", "attributed", s.armed)
	s.verdict(t, ctx, m, r, iqpV{scope: "conversation"})
	gated["not_addressed"] = m

	m, _, _ = s.eligible(t, ctx, "g-kind", iqpV{kind: "fyi"})
	gated["kind"] = m

	m, _, _ = s.eligible(t, ctx, "g-rethreaded", iqpV{})
	s.exec(t, ctx, `UPDATE normalized_messages SET thread_id=$2 WHERE id=$1`, m, s.thread(t, ctx, gmailKey("g-elsewhere")))
	gated["rethreaded"] = m

	auditBefore := s.inquiryAudit(t, ctx)
	st := s.run(t, ctx, promote.Config{})

	for reason, msg := range gated {
		if p, ok := s.promotion(t, ctx, msg); ok {
			t.Errorf("gated (%s) verdict wrote a promotion row %+v; a gated verdict writes NO row (C10)", reason, p)
		}
		if st.Gated[reason] != 1 {
			t.Errorf("Gated[%s] = %d, want 1; Gated = %v", reason, st.Gated[reason], st.Gated)
		}
	}
	if n := s.inquiryAudit(t, ctx); n != auditBefore {
		t.Errorf("a pass whose every verdict was gated made %d executor call(s); a gated verdict calls no tool", n-auditBefore)
	}
	if st.Review+st.Created+st.Attached+st.Lost != 0 || s.armedTasks(t, ctx) != 0 {
		t.Errorf("stats %+v / %d tasks; nothing may be acted on", st, s.armedTasks(t, ctx))
	}
}

// C10's second half: a pending verdict promotes on the first pass after its
// grace. The clock is the fixture's sent_at (C13's method).
func TestPromoteInquiry_Integration_PendingReleasesAfterGrace(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	m, r := s.message(t, ctx, iqpMsg{label: "grace", key: gmailKey("grace"), sentAt: s.ago(10 * time.Minute)})
	s.decision(t, ctx, m, "live", "attributed", s.armed)
	s.verdict(t, ctx, m, r, iqpV{scope: "thread"})

	st := s.run(t, ctx, promote.Config{})
	if _, ok := s.promotion(t, ctx, m); ok || st.Gated["pending"] != 1 {
		t.Fatalf("inside the grace: promoted=%v Gated=%v, want no row and pending=1", ok, st.Gated)
	}
	s.exec(t, ctx, `UPDATE normalized_messages SET sent_at = now() - interval '2 hours' WHERE id=$1`, m)
	s.run(t, ctx, promote.Config{})
	p, ok := s.promotion(t, ctx, m)
	if !ok || p.action != "review" || p.taskID == nil || s.status(t, ctx, *p.taskID) != "holding" {
		t.Errorf("after the grace: %+v (found=%v), want a holding review task", p, ok)
	}
}

// ---- C11: stats, --dry-run, --max-age ----------------------------------------------

func TestPromoteInquiry_Integration_DryRunWritesNothingAndPlansTheSame(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	create, _, _ := s.eligible(t, ctx, "dry-create", iqpV{})
	m, r := s.message(t, ctx, iqpMsg{label: "dry-pending", key: gmailKey("dry-pending"), sentAt: s.ago(10 * time.Minute)})
	s.decision(t, ctx, m, "live", "attributed", s.armed)
	s.verdict(t, ctx, m, r, iqpV{scope: "thread"})

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	dry := s.run(t, ctx, promote.Config{DryRun: true})
	slog.SetDefault(old)

	if n := s.count(t, ctx, `SELECT count(*) FROM classify_promotions WHERE project_id=$1`, s.armed); n != 0 {
		t.Errorf("--dry-run wrote %d promotion rows", n)
	}
	if s.armedTasks(t, ctx) != 0 || s.inquiryAudit(t, ctx) != 0 {
		t.Errorf("--dry-run created tasks or made executor calls")
	}
	if dry.Review != 1 || dry.Gated["pending"] != 1 {
		t.Errorf("dry-run stats %+v, want Review 1 and Gated[pending] 1 (it counts gated verdicts by reason, C11)", dry)
	}
	if !strings.Contains(buf.String(), "status=holding") {
		t.Errorf("the dry-run plan does not show status=holding for the would-create line (V5: every would-create "+
			"line shows status=holding, O7):\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), strconv.FormatInt(create, 10)) {
		t.Errorf("the dry-run plan never names message %d", create)
	}

	live := s.run(t, ctx, promote.Config{})
	if live.Review != dry.Review || live.Gated["pending"] != dry.Gated["pending"] {
		t.Errorf("live %+v differs from the dry-run's plan %+v", live, dry)
	}
}

func TestPromoteInquiry_Integration_MaxAgeIsDryRunOnly(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	m, r := s.message(t, ctx, iqpMsg{label: "backfill", key: gmailKey("backfill"), sentAt: s.ago(100 * time.Hour)})
	s.decision(t, ctx, m, "live", "attributed", s.armed)
	s.verdict(t, ctx, m, r, iqpV{scope: "thread"})

	if _, err := promote.Run(ctx, s.pool, s.ex, promote.Config{Lane: promote.LaneInquiry, MaxAge: 720 * time.Hour}); err == nil {
		t.Errorf("a LIVE pass with MaxAge set returned nil; --max-age is refused unless --dry-run (C11): the " +
			"72h fence is what keeps historical asks from becoming tasks")
	}
	if _, ok := s.promotion(t, ctx, m); ok || s.armedTasks(t, ctx) != 0 {
		t.Errorf("the refused pass wrote something")
	}
	if st := s.run(t, ctx, promote.Config{DryRun: true}); st.Review != 0 {
		t.Errorf("dry-run without --max-age planned %d creates for a 100h-old ask, want 0", st.Review)
	}
	st := s.run(t, ctx, promote.Config{DryRun: true, MaxAge: 720 * time.Hour})
	if st.Review != 1 || st.Gated["stale"] != 0 {
		t.Errorf("dry-run --max-age 720h: %+v, want Review 1 and no stale (the widened fence reaches the gate too, V5)", st)
	}
	if _, ok := s.promotion(t, ctx, m); ok {
		t.Errorf("the dry-run wrote a promotion row")
	}
}

// Limit bounds what a pass ACTS on. A gated verdict stays in the inbox (it
// writes no row), so a limit over rows READ would let older gated verdicts fill
// every pass and starve newer passing ones until they age out at 72h.
func TestPromoteInquiry_Integration_GatedVerdictsDoNotStarveTheLimit(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	m, r := s.message(t, ctx, iqpMsg{label: "starve-old", key: slackKey("C0IQPSTARVE", ""), channel: "slack", sentAt: s.ago(3 * time.Hour)})
	s.decision(t, ctx, m, "live", "attributed", s.armed)
	s.verdict(t, ctx, m, r, iqpV{scope: "conversation", runAgo: 20 * time.Minute})
	pass, _, _ := s.eligible(t, ctx, "starve-new", iqpV{runAgo: 10 * time.Minute})

	s.run(t, ctx, promote.Config{Limit: 1})
	if _, ok := s.promotion(t, ctx, pass); !ok {
		t.Errorf("with Limit 1, an older not_addressed verdict starved the newer passing one")
	}
}

// ---- C12: CountersByLane under review; the lock -----------------------------------

func TestPromoteInquiry_Integration_CountersByLaneReportsInquiryUnderReview(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	lane := func() promote.LaneCounters {
		cs, err := promote.CountersByLane(ctx, s.pool, 7)
		if err != nil {
			t.Fatalf("CountersByLane: %v", err)
		}
		for _, c := range cs {
			if c.Lane == "classify_inquiry" {
				return c
			}
		}
		return promote.LaneCounters{Lane: "classify_inquiry"}
	}
	before := lane()
	s.eligible(t, ctx, "counters", iqpV{})
	s.run(t, ctx, promote.Config{})
	after := lane()
	if after.Review-before.Review != 1 || after.Created != before.Created {
		t.Errorf("CountersByLane(classify_inquiry) %+v -> %+v, want +1 review and no created (O7's holding phase)", before, after)
	}
}

func TestPromoteInquiry_Integration_LockHeldIsErrLockHeld(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	s.eligible(t, ctx, "locked", iqpV{})
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	var held bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, iqpLockKey).Scan(&held); err != nil || !held {
		conn.Release()
		t.Fatalf("take 0x%X: held=%v err=%v", iqpLockKey, held, err)
	}
	defer func() {
		conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, iqpLockKey)
		conn.Release()
	}()
	_, err = promote.Run(ctx, s.pool, s.ex, promote.Config{Lane: promote.LaneInquiry})
	if !errors.Is(err, promote.ErrLockHeld) {
		t.Errorf("Run with 0x%X held: err = %v, want errors.Is(err, promote.ErrLockHeld) — pipelined maps it to "+
			"pipeline.ErrLockHeld (retry, then the sweep); the CLI still exits non-zero", iqpLockKey, err)
	}
	if s.armedTasks(t, ctx) != 0 {
		t.Errorf("a pass that lost the lock created tasks")
	}
}

// ---- C14: the dismissal readout, folded on the FIRST dismissal --------------------------

// MUTATION: fold on the LATEST dismissal → `fpthen` (not_actionable, activity-
// reopened, then handled_elsewhere) reads true_positive: FalsePositive drops to
// 1 and TruePositive rises to 3.
func TestPromoteInquiry_Integration_OutcomesFoldOnTheFirstDismissal(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	before, err := promote.InquiryOutcomes(ctx, s.pool, 0)
	if err != nil {
		t.Fatalf("InquiryOutcomes: %v", err)
	}

	labels := []string{"fp", "fpthen", "tpclosed", "tphandled", "misclick", "dup", "open"}
	first := map[string]int64{}
	for _, l := range labels {
		m, r := s.message(t, ctx, iqpMsg{label: l, key: gmailKey("out-" + l), sentAt: s.ago(3 * time.Hour)})
		s.decision(t, ctx, m, "live", "attributed", s.armed)
		s.verdict(t, ctx, m, r, iqpV{scope: "thread"})
		first[l] = m
	}
	s.run(t, ctx, promote.Config{})
	task := map[string]int64{}
	for _, l := range labels {
		task[l] = s.taskOf(t, ctx, first[l])
	}
	dismiss := func(l, code string) {
		s.execute(t, ctx, "task_dismiss", iqpHuman, task[l], fmt.Sprintf(`{"task_id":%d,"reason_code":%q}`, task[l], code))
	}
	followUp := func(l string) {
		m, r := s.message(t, ctx, iqpMsg{label: l + "-2", key: gmailKey("out-" + l), sentAt: s.ago(90 * time.Minute)})
		s.decision(t, ctx, m, "live", "attributed", s.armed)
		s.verdict(t, ctx, m, r, iqpV{scope: "thread", ask: "follow-up"})
	}

	dismiss("fp", "not_actionable")
	dismiss("fpthen", "not_actionable")
	followUp("fpthen") // an ACTIVITY reopen (promote:inquiry), which does not undo the label
	followUp("open")   // an `attached` promotion: excluded
	s.run(t, ctx, promote.Config{})
	if got := s.status(t, ctx, task["fpthen"]); got == "closed" {
		t.Fatalf("setup: the follow-up did not activity-reopen fpthen's task")
	}
	dismiss("fpthen", "handled_elsewhere")
	s.execute(t, ctx, "task_close", iqpCloser, task["tpclosed"], fmt.Sprintf(`{"task_id":%d,"reason":"done"}`, task["tpclosed"]))
	dismiss("tphandled", "handled_elsewhere")
	dismiss("misclick", "not_actionable")
	s.execute(t, ctx, "task_reopen", iqpHuman, task["misclick"], fmt.Sprintf(`{"task_id":%d,"reason":"mis-click"}`, task["misclick"]))
	dismiss("dup", "duplicate")

	// Control: a PERSONAL-lane promotion of a dismissed task is not a unit here.
	pm, pr := s.message(t, ctx, iqpMsg{label: "out-personal", key: gmailKey("out-personal"), sentAt: s.ago(3 * time.Hour)})
	pextr := s.verdict(t, ctx, pm, pr, iqpV{worker: "classify", scope: "thread"})
	ptask := s.id(t, ctx, `INSERT INTO tasks (project_id, title, body, assignee_type, status, priority)
	                       VALUES ($1,'itest-inqp personal','', 'human','closed',0) RETURNING id`, s.armed)
	s.exec(t, ctx, `INSERT INTO task_dismissals (task_id, reason_code, dismissed_by) VALUES ($1,'not_actionable',$2)`, ptask, iqpHuman)
	s.exec(t, ctx, `INSERT INTO classify_promotions (normalized_message_id, raw_source_item_id, ai_extraction_id,
	                                                 project_id, kind, action, task_id)
	                VALUES ($1,$2,$3,$4,'informational','review',$5)`, pm, pr, pextr, s.armed, ptask)

	after, err := promote.InquiryOutcomes(ctx, s.pool, 0)
	if err != nil {
		t.Fatalf("InquiryOutcomes: %v", err)
	}
	d := promote.OutcomeCounts{
		FalsePositive: after.FalsePositive - before.FalsePositive,
		TruePositive:  after.TruePositive - before.TruePositive,
		MisClick:      after.MisClick - before.MisClick,
		Excluded:      after.Excluded - before.Excluded,
	}
	want := promote.OutcomeCounts{FalsePositive: 2, TruePositive: 2, MisClick: 1, Excluded: 4}
	if d != want {
		t.Errorf("InquiryOutcomes delta = %+v, want %+v:\n"+
			"  false_positive: fp, fpthen (FIRST dismissal not_actionable; the activity reopen does not undo it)\n"+
			"  true_positive:  tpclosed (closed, no dismissal), tphandled (handled_elsewhere)\n"+
			"  mis_click:      misclick (plain-reopened by a HUMAN)\n"+
			"  excluded:       dup (duplicate), open (still open), and the two `attached` promotions\n"+
			"  and the personal-lane control counts nowhere", d, want)
	}
	if got, err := promote.InquiryOutcomes(ctx, s.pool, time.Hour); err != nil || got.Decided() < d.Decided() {
		t.Errorf("InquiryOutcomes(since 1h) = %+v (err %v); the window is on the promotion's created_at and must "+
			"include this test's rows", got, err)
	}
}

// ---- C-D13: never attach to or reopen a non-human task ------------------------------

// claudeThreadTask seeds a thread in the armed project whose only task is a
// CLAUDE task (open, or closed with an open dismissal), then an eligible ask
// on that thread. It returns the task, its dismissal (0 when open) and the
// ask's message.
func (s *iqpSuite) claudeThreadTask(t *testing.T, ctx context.Context, label string, dismissed bool) (task, dismissal, msg int64) {
	t.Helper()
	key := gmailKey(label)
	th := s.thread(t, ctx, key)
	status := "ready"
	if dismissed {
		status = "closed"
	}
	task = s.id(t, ctx, `INSERT INTO tasks (project_id, title, body, assignee_type, status, priority, source_thread_id)
	                     VALUES ($1,$2,'','claude',$3,0,$4) RETURNING id`, s.armed, "itest-inqp "+label, status, th)
	if dismissed {
		dismissal = s.id(t, ctx, `INSERT INTO task_dismissals (task_id, reason_code, dismissed_by)
		                          VALUES ($1,'not_actionable',$2) RETURNING id`, task, iqpHuman)
	}
	m, r := s.message(t, ctx, iqpMsg{label: label, key: key, sentAt: s.ago(2 * time.Hour)})
	s.decision(t, ctx, m, "live", "attributed", s.armed)
	s.verdict(t, ctx, m, r, iqpV{scope: "thread", ask: "can you look at this?"})
	return task, dismissal, m
}

// assertClaudeGated: gated claude_task, no promotion row, no tool call, no
// event on the claude task, no second task — and the verdict stays in the
// inbox (a second pass gates it again).
func (s *iqpSuite) assertClaudeGated(t *testing.T, ctx context.Context, task, msg int64, wantStatus string) {
	t.Helper()
	for pass := 1; pass <= 2; pass++ {
		st := s.run(t, ctx, promote.Config{})
		if st.Gated[promote.GateClaudeTask] != 1 || st.Created+st.Review+st.Attached+st.Reopened != 0 {
			t.Errorf("pass %d: stats %+v, want exactly one gated %q and nothing acted", pass, st, promote.GateClaudeTask)
		}
	}
	if p, ok := s.promotion(t, ctx, msg); ok {
		t.Errorf("a gated verdict wrote a promotion row %+v", p)
	}
	if n := s.inquiryAudit(t, ctx); n != 0 {
		t.Errorf("%d audit rows as promote:inquiry; a gated verdict calls no tool (no log, no reopen)", n)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1`, task); n != 0 {
		t.Errorf("%d task_events on the claude task %d; nothing may be logged onto it", n, task)
	}
	if got := s.status(t, ctx, task); got != wantStatus {
		t.Errorf("claude task %d status = %q, want %q unchanged", task, got, wantStatus)
	}
	if n := s.armedTasks(t, ctx); n != 1 {
		t.Errorf("%d tasks on the armed project, want 1: never a second task on the thread (Q3)", n)
	}
}

// MUTATIONS: drop InquiryGate's C-D13 clause → both tests red (an attach log on
// the open claude task; a reopen of the dismissed one). Drop assignee_type from
// threadTask's SELECTs → AttachesToTheOpenTask and ReopensADismissedTask red
// (an empty assignee reads as not human), and so does this file's positive
// control. The positive control flips the SAME task to human: the gate is the
// only thing standing between the verdict and the attach.
func TestPromoteInquiry_Integration_NeverAttachesToAnOpenClaudeTask(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	task, _, m := s.claudeThreadTask(t, ctx, "claude-open", false)
	s.assertClaudeGated(t, ctx, task, m, "ready")

	s.exec(t, ctx, `UPDATE tasks SET assignee_type='human' WHERE id=$1`, task)
	st := s.run(t, ctx, promote.Config{})
	if p, ok := s.promotion(t, ctx, m); !ok || p.action != "attached" || p.taskID == nil || *p.taskID != task || st.Attached != 1 {
		t.Errorf("POSITIVE CONTROL: the same task as human: promotion %+v (found=%v), stats %+v; want attached to %d",
			p, ok, st, task)
	}
}

func TestPromoteInquiry_Integration_NeverReopensADismissedClaudeTask(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	task, dismissal, m := s.claudeThreadTask(t, ctx, "claude-dismissed", true)
	s.assertClaudeGated(t, ctx, task, m, "closed")
	if n := s.count(t, ctx, `SELECT count(*) FROM task_dismissals WHERE id=$1 AND reopened_at IS NULL`, dismissal); n != 1 {
		t.Errorf("dismissal %d was reopened; a claude task must never go back to a console queue", dismissal)
	}

	s.exec(t, ctx, `UPDATE tasks SET assignee_type='human' WHERE id=$1`, task)
	st := s.run(t, ctx, promote.Config{})
	if p, ok := s.promotion(t, ctx, m); !ok || p.action != "attached" || p.taskID == nil || *p.taskID != task || st.Reopened != 1 {
		t.Errorf("POSITIVE CONTROL: the same task as human: promotion %+v (found=%v), stats %+v; want attached to %d "+
			"and reopened", p, ok, st, task)
	}
}

// ---- stuck claims: the criterion-12 crash artifact, made visible ------------------

// A claim whose executor call never completed keeps task_id NULL and excludes
// its message from the inbox for good (at most once, never a duplicate task;
// ClaimBeforeAct pins that). StuckClaims reports them per lane with the oldest
// claim, all time. MUTATIONS: drop `WHERE cp.task_id IS NULL` → the completed
// control counts and the inquiry delta is 2; join ai_runs on 'classify_inquiry'
// only → the personal claim vanishes.
func TestPromoteInquiry_Integration_StuckClaimsAreReported(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	lane := func(sc []promote.StuckClaim, l string) promote.StuckClaim {
		for _, c := range sc {
			if c.Lane == l {
				return c
			}
		}
		t.Fatalf("StuckClaims has no %q line: %+v (personal and inquiry always print)", l, sc)
		return promote.StuckClaim{}
	}
	before, err := promote.StuckClaims(ctx, s.pool)
	if err != nil {
		t.Fatalf("StuckClaims: %v", err)
	}
	if len(before) < 2 || before[0].Lane != "personal" || before[1].Lane != "inquiry" {
		t.Fatalf("StuckClaims = %+v, want personal then inquiry first, zeros included", before)
	}

	claimAt := func(msg, raw, extr, task int64, at time.Time) {
		var tp *int64
		if task != 0 {
			tp = &task
		}
		s.exec(t, ctx, `INSERT INTO classify_promotions (normalized_message_id, raw_source_item_id, ai_extraction_id,
		                                                 project_id, kind, action, task_id, created_at)
		                VALUES ($1,$2,$3,$4,'question','review',$5,$6)`, msg, raw, extr, s.armed, tp, at)
	}
	im, ir, iextr := s.eligible(t, ctx, "stuck-inquiry", iqpV{})
	claimAt(im, ir, iextr, 0, s.ago(120*time.Hour))
	pm, pr := s.message(t, ctx, iqpMsg{label: "stuck-personal", key: gmailKey("stuck-personal"), sentAt: s.ago(3 * time.Hour)})
	pextr := s.verdict(t, ctx, pm, pr, iqpV{worker: "classify", scope: "thread"})
	claimAt(pm, pr, pextr, 0, s.ago(48*time.Hour))
	// Control: a COMPLETED inquiry claim is not stuck.
	cm, cr, cextr := s.eligible(t, ctx, "stuck-control", iqpV{})
	done := s.id(t, ctx, `INSERT INTO tasks (project_id, title, body, assignee_type, status, priority)
	                      VALUES ($1,'itest-inqp done','','human','holding',0) RETURNING id`, s.armed)
	claimAt(cm, cr, cextr, done, s.ago(200*time.Hour))

	after, err := promote.StuckClaims(ctx, s.pool)
	if err != nil {
		t.Fatalf("StuckClaims: %v", err)
	}
	for _, tc := range []struct {
		lane string
		at   time.Time
	}{{"inquiry", s.ago(120 * time.Hour)}, {"personal", s.ago(48 * time.Hour)}} {
		b, a := lane(before, tc.lane), lane(after, tc.lane)
		if a.Count-b.Count != 1 {
			t.Errorf("%s stuck claims %d -> %d, want +1 (the seeded NULL-task claim; the completed control never counts)",
				tc.lane, b.Count, a.Count)
		}
		if a.Oldest.IsZero() || a.Oldest.After(tc.at) {
			t.Errorf("%s oldest stuck claim = %v, want at or before the seeded %v", tc.lane, a.Oldest, tc.at)
		}
	}

	// The contract is unchanged: a pass never completes a stuck claim.
	s.run(t, ctx, promote.Config{})
	if p, _ := s.promotion(t, ctx, im); p.taskID != nil {
		t.Errorf("a pass completed the stuck claim with task %d; the at-most-once contract is not this change's", *p.taskID)
	}
}
