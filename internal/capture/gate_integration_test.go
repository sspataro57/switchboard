//go:build integration

package capture_test

// SWT-40 Part D against a real database (docs/tickets/inquiry-promote_SPEC.md):
// criteria D1 (held), D4 (the gate driver on a fake Jira), D5 (rate + the
// shared snapshot), D6 (the report), D-D2's shadow-overwrite guard, the
// migration's live shape, and V3's mutations.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run CaptureGate ./internal/capture/
//
// Build-tagged `integration` AND env-gated on DATABASE_URL, with a FATAL guard
// on 192.168.50.49. NO LLM, NO live Jira: the lookup half talks to the httptest
// server at the bottom of this file, injected the way cmd/pipelined injects the
// token-decrypting factory — the ticketstatus suite's pattern.
//
// TEST THE COLUMN, NOT THE FIXTURE (IK SWT-21 #6). Every `held` row here is
// written by the REAL live capture pass over a project whose
// projects.ticket_assignee_gate is set in the fixture INSERT; no test inserts a
// held row by hand. D1's mutation — read the gate from a constant instead of the
// column — is red in BOTH directions: constant true holds the gate-off
// collaboratory control; constant false lets the gated project create a task.
//
// ---- IMPOSED surface, beyond gate_test.go's ----------------------------------
//
//	type GateConfig struct {
//	    Limit  int                // bound the inbox; 0 = the driver's default
//	    TTL    time.Duration      // zero => ticketstatus.LookupTTL()
//	    Lookup jira.ClientFactory // nil = no credential: every unreadable hold stays held
//	}
//	type GateStats struct {
//	    TasksCreated, Appended, Reopened, Attributed, PendingLookup int
//	    Resolved int // gate rows written = TasksCreated + Appended + Attributed.
//	                 // What pipelined's PassFunc returns: a pass whose holds all
//	                 // stay pending reports 0, so the stage loop never re-runs
//	                 // it at once (D-D5: nothing retries faster than the sweep).
//	}
//	var ErrGateLockHeld error // RunGate's error (errors.Is) when 0x5157_0015 is held;
//	                          // pipelined maps it to pipeline.ErrLockHeld
//	func RunGate(ctx context.Context, pool *pgxpool.Pool, ex *executor.Executor,
//	             cfg GateConfig) (GateStats, error)
//
// Clock: expiry reads the HOLD's age — the held capture_decisions row's
// created_at, when capture wrote it (review fix 4) — never the message's
// sent_at: a late-captured old message still gets its full GateMaxAge of
// lookups. The inbox reaches past GateMaxAge for the expired holds too — they
// resolve `attributed (gate_unverified_expired)` and stop costing GETs.
//
// Freshness (review fix 1): a stored snapshot decides a hold only if it was
// verified at or after the message's first-seen time (normalized_messages
// .created_at). gate_freshness_integration_test.go owns those cases.
//
// Report format (D6), imposed loosely — a section whose header line starts with
// GATE, then one line per fact, the count as the LAST field:
//
//	GATE (capture-time assignee gate)
//	  held                        3
//	  pending_lookup              1
//	  gate task                   1
//	  gate attributed not_assigned 1
//
// GREENFIELD NOTE — EXPECTED RED: RunGate / GateConfig / GateStats /
// ErrGateLockHeld do not exist (compile failure first). Past that, migration
// 0029 is required on the compose db (cgRequireMigration says so in one line).
//
// CROSS-SUITE DISCIPLINE. The capture pass and the gate inbox are GLOBAL, so
// this suite deletes capture_decisions WHOLESALE at start and end — the
// rules_integration_test.go precedent, compose db only. It owns projects
// itest-capgate-%, accounts itest-capgate%, threads gmail:itest-capgate:% and
// jira:itest-capgate.jira.com:%, and audit rows by task plus actors capture:%
// (capture:gate included), ticketstatus:% and dashboard:itest-capgate.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/connector/jira"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/ticketstatus"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	cgGatedSlug = "itest-capgate-reengine" // ticket_assignee_gate = true  (reengine's shape)
	cgPlainSlug = "itest-capgate-collab"   // ticket_assignee_gate = false (collaboratory's shape)

	cgMailAcct   = "itest-capgate@mail.example.test"
	cgLookupAcct = "itest-capgate-lookup@example.test"

	cgThreadPrefix = "gmail:itest-capgate:"
	cgWebPrefix    = "jira:itest-capgate.jira.com:WEB-" // rule 10's shape
	cgJiraSender   = "jira@itest-capgate.example.test"  // rule 2's shape

	cgOwnID   = "acc-itest-capgate-own"
	cgOtherID = "acc-itest-capgate-other"

	cgCaptureActor = "capture:itest-capgate"
	cgSetupActor   = "capture:itest-capgate-setup"
	cgHuman        = "dashboard:itest-capgate"
	cgGateActor    = "capture:gate"

	cgLockKey = int64(0x5157_0015)
)

// ---- harness --------------------------------------------------------------------

type cgSuite struct {
	pool *pgxpool.Pool
	ex   *executor.Executor
	fake *cgFakeJira

	gated, plain                 int64
	ruleKey, ruleSender, ruleWeb int64
	mailAcct, lookupAcct         int64
	threads                      map[string]int64
}

func newCGSuite(t *testing.T, ctx context.Context) *cgSuite {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db (cleanup deletes capture_decisions " +
			"wholesale); use the compose db on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	cgRequireMigration(t, ctx, pool)
	cgCleanup(t, ctx, pool)
	t.Cleanup(func() { cgCleanup(t, ctx, pool) })

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	s := &cgSuite{
		pool: pool, ex: executor.New(reg, checker, audit.NewPGStore(pool)),
		fake: newCGFakeJira(), threads: map[string]int64{},
	}
	t.Cleanup(s.fake.close)
	s.seed(t, ctx)
	return s
}

// cgRequireMigration turns "violates check constraint capture_decisions_action_check"
// — which otherwise surfaces from inside the first live pass — into the one
// sentence that is true before this ticket's migration is applied.
func cgRequireMigration(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var present bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'capture_decisions_gate_uniq')`).Scan(&present); err != nil {
		t.Fatalf("probe for capture_decisions_gate_uniq: %v", err)
	}
	if !present {
		t.Fatalf("capture_decisions_gate_uniq does not exist in this database: migrations/0029_capture_ticket_gate.sql " +
			"(held, mode='gate', the gate shape CHECKs, the partial unique index) is not applied. `make integration` " +
			"applies migrations to the compose db before running")
	}
}

func cgCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE account_email LIKE 'itest-capgate%')`
	const projs = `(SELECT id FROM projects WHERE slug LIKE 'itest-capgate-%')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	const audited = `(SELECT id FROM audit_events WHERE task_id IN ` + tasksOf +
		` OR actor LIKE 'capture:%' OR actor LIKE 'ticketstatus:%' OR actor = '` + cgHuman + `')`
	for _, q := range []string{
		`DELETE FROM capture_decisions`, // wholesale: see the header
		`DELETE FROM ticket_status_syncs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM external_refs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM deliveries WHERE task_id IN ` + tasksOf,
		`DELETE FROM policy_decisions WHERE audit_event_id IN ` + audited,
		`DELETE FROM audit_events WHERE id IN ` + audited,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM capture_rules WHERE project_id IN ` + projs,
		`DELETE FROM projects WHERE slug LIKE 'itest-capgate-%'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN
		   (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'gmail:itest-capgate:%'
		    OR thread_key LIKE 'jira:itest-capgate.jira.com:%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE account_email LIKE 'itest-capgate%'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func (s *cgSuite) insID(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", q, err)
	}
	return id
}

func (s *cgSuite) n(t *testing.T, ctx context.Context, q string, args ...any) int {
	t.Helper()
	var v int
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return v
}

func (s *cgSuite) seed(t *testing.T, ctx context.Context) {
	t.Helper()
	// ticket_assignee_gate named EXPLICITLY on both: 0023 defaults it false, and
	// a fixture leaning on the default would test the plain shape twice.
	project := func(slug string, gate bool) int64 {
		return s.insID(t, ctx,
			`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality, ticket_assignee_gate)
			 VALUES ($1,$1,'itest-capgate-client','manual','dashboard','/tmp/itest-capgate','any',$2) RETURNING id`,
			slug, gate)
	}
	s.gated = project(cgGatedSlug, true)
	s.plain = project(cgPlainSlug, false)

	// Rule 1's shape (body_regex, jira, priority 100), rule 2's (the Jira sender,
	// key_regex over subject+body), and rule 10's on the gate-OFF project.
	s.ruleKey = s.insID(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, url_template, priority, enabled, note)
		 VALUES ($1,'body_regex','GTE-[0-9]+','jira',NULL,'https://itest-capgate.example.test/browse/{key}',100,true,'itest-capgate rule1')
		 RETURNING id`, s.gated)
	s.ruleSender = s.insID(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, url_template, priority, enabled, note)
		 VALUES ($1,'sender',$2,'jira','(GTE-[0-9]+)','https://itest-capgate.example.test/browse/{key}',100,true,'itest-capgate rule2')
		 RETURNING id`, s.gated, cgJiraSender)
	s.ruleWeb = s.insID(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, url_template, priority, enabled, note)
		 VALUES ($1,'thread_key_prefix',$2,'jira','[A-Z]+-[0-9]+$','https://itest-capgate.jira.com/browse/{key}',90,true,'itest-capgate rule10')
		 RETURNING id`, s.plain, cgWebPrefix)

	s.mailAcct = s.insID(t, ctx,
		`INSERT INTO source_accounts (provider, account_email, scopes, send_enabled, calendar_in_availability)
		 VALUES ('google',$1,'{}',false,false) RETURNING id`, cgMailAcct)
	// The jira_lookup account (prod: 8309, scopes {LHH,LHHSF}); no own id yet —
	// the gate's lookup fills it through /myself.
	s.lookupAcct = s.insID(t, ctx,
		`INSERT INTO source_accounts (provider, account_email, domain_default, scopes, send_enabled, sync_cursor)
		 VALUES ('jira_lookup',$1,$2,'{GTE}',false,'{}'::jsonb) RETURNING id`, cgLookupAcct, s.fake.url())
}

func (s *cgSuite) thread(t *testing.T, ctx context.Context, key string) int64 {
	t.Helper()
	if id, ok := s.threads[key]; ok {
		return id
	}
	id := s.insID(t, ctx,
		`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,'itest capgate','[]') RETURNING id`, key)
	s.threads[key] = id
	return id
}

// msg seeds one INBOUND message the capture pass has never decided.
func (s *cgSuite) msg(t *testing.T, ctx context.Context, label, threadKey, sender, subject, body string, age time.Duration) int64 {
	t.Helper()
	thread := s.thread(t, ctx, threadKey)
	raw := s.insID(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,'{}',$3, now()) RETURNING id`,
		s.mailAcct, "itest-capgate-"+label, "itest-capgate-h-"+label)
	return s.insID(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1,$2,'inbound',$3, now() - make_interval(secs => $4), $5,$6,$7,'gmail') RETURNING id`,
		raw, thread, "<itest-capgate-"+label+"@mail.example.test>", age.Seconds(), body, subject, sender)
}

// mention is a colleague's mail naming a ticket: rule 1 matches it.
func (s *cgSuite) mention(t *testing.T, ctx context.Context, label, key string, age time.Duration) int64 {
	t.Helper()
	return s.msg(t, ctx, label, cgThreadPrefix+label, "Colleague <colleague@itest-capgate.example.test>",
		"about "+key, "can you look at "+key+"?", age)
}

func (s *cgSuite) threadOf(t *testing.T, ctx context.Context, msgID int64) int64 {
	t.Helper()
	return int64(s.n(t, ctx, `SELECT thread_id FROM normalized_messages WHERE id=$1`, msgID))
}

func (s *cgSuite) live(t *testing.T, ctx context.Context) capture.RulesStats {
	t.Helper()
	st, err := capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{Mode: capture.RulesModeLive, Actor: cgCaptureActor})
	if err != nil {
		t.Fatalf("EvaluateRules(live): %v", err)
	}
	return st
}

func (s *cgSuite) shadow(t *testing.T, ctx context.Context, all bool) {
	t.Helper()
	if _, err := capture.EvaluateRules(ctx, s.pool, s.ex,
		capture.RulesConfig{Mode: capture.RulesModeShadow, Actor: cgCaptureActor, All: all}); err != nil {
		t.Fatalf("EvaluateRules(shadow, all=%v): %v", all, err)
	}
}

func (s *cgSuite) gate(t *testing.T, ctx context.Context) capture.GateStats {
	t.Helper()
	st, err := capture.RunGate(ctx, s.pool, s.ex, capture.GateConfig{Limit: 200, Lookup: s.fake.factory()})
	if err != nil {
		t.Fatalf("RunGate: %v", err)
	}
	if st.Resolved != st.TasksCreated+st.Appended+st.Attributed {
		t.Errorf("GateStats.Resolved = %d, want TasksCreated+Appended+Attributed = %d+%d+%d (it is what the "+
			"pipelined pass reports as processed)", st.Resolved, st.TasksCreated, st.Appended, st.Attributed)
	}
	return st
}

type cgDecision struct {
	action                    string
	projectID, ruleID, taskID *int64
	extSystem, extKey, reason *string
}

func (s *cgSuite) decision(t *testing.T, ctx context.Context, msgID int64, mode string) (cgDecision, bool) {
	t.Helper()
	var d cgDecision
	err := s.pool.QueryRow(ctx,
		`SELECT action, project_id, matched_rule_id, task_id, external_system, external_key, reason
		   FROM capture_decisions WHERE message_id=$1 AND mode=$2 ORDER BY id DESC LIMIT 1`, msgID, mode).
		Scan(&d.action, &d.projectID, &d.ruleID, &d.taskID, &d.extSystem, &d.extKey, &d.reason)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return d, false
		}
		t.Fatalf("read %s decision for message %d: %v", mode, msgID, err)
	}
	return d, true
}

func (s *cgSuite) latest(t *testing.T, ctx context.Context, msgID int64) (string, string) {
	t.Helper()
	var mode, action string
	if err := s.pool.QueryRow(ctx,
		`SELECT mode, action FROM capture_decisions WHERE message_id=$1 ORDER BY id DESC LIMIT 1`, msgID).
		Scan(&mode, &action); err != nil {
		t.Fatalf("latest decision for message %d: %v", msgID, err)
	}
	return mode, action
}

func (s *cgSuite) refTask(t *testing.T, ctx context.Context, key string) (int64, bool) {
	t.Helper()
	var id int64
	err := s.pool.QueryRow(ctx, `SELECT task_id FROM external_refs WHERE system='jira' AND external_key=$1`, key).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return 0, false
		}
		t.Fatalf("read ref for %s: %v", key, err)
	}
	return id, true
}

// taskWithRef creates a task and links a key to it through the EXECUTOR — the
// ref is written by link_external_ref, not the harness.
func (s *cgSuite) taskWithRef(t *testing.T, ctx context.Context, key string) int64 {
	t.Helper()
	args := fmt.Sprintf(`{"project":%q,"title":%q,"body":"itest-capgate setup","assignee_type":"human","priority":0}`,
		cgGatedSlug, "itest-capgate setup "+key)
	res, err := s.ex.Execute(ctx, executor.Call{Tool: "create_task", Actor: cgSetupActor, Args: []byte(args)})
	if err != nil {
		t.Fatalf("setup create_task: %v", err)
	}
	var out struct {
		TaskID int64 `json:"task_id"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil || out.TaskID == 0 {
		t.Fatalf("setup create_task output %s: %v", res.Output, err)
	}
	link := fmt.Sprintf(`{"task_id":%d,"system":"jira","external_key":%q}`, out.TaskID, key)
	if _, err := s.ex.Execute(ctx, executor.Call{Tool: "link_external_ref", Actor: cgSetupActor,
		Args: []byte(link), TaskID: &out.TaskID}); err != nil {
		t.Fatalf("setup link_external_ref: %v", err)
	}
	return out.TaskID
}

func (s *cgSuite) gateRows(t *testing.T, ctx context.Context) int {
	t.Helper()
	return s.n(t, ctx, `SELECT count(*) FROM capture_decisions WHERE mode='gate'`)
}

func (s *cgSuite) pendingHolds(t *testing.T, ctx context.Context) int {
	t.Helper()
	return s.n(t, ctx,
		`SELECT count(*) FROM capture_decisions h
		  WHERE h.mode='live' AND h.action='held'
		    AND NOT EXISTS (SELECT 1 FROM capture_decisions g WHERE g.message_id=h.message_id AND g.mode='gate')`)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ---- migration shape, against the live schema -----------------------------------

func TestCaptureGate_Integration_MigrationShape(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)

	def := func(name string) string {
		var d string
		if err := s.pool.QueryRow(ctx,
			`SELECT pg_get_constraintdef(oid) FROM pg_constraint
			  WHERE conname=$1 AND conrelid='capture_decisions'::regclass`, name).Scan(&d); err != nil {
			t.Errorf("constraint %s on capture_decisions: %v (confirm the inline names against prod, as 0015 did)", name, err)
		}
		return d
	}
	if d := def("capture_decisions_action_check"); !strings.Contains(d, "'held'") {
		t.Errorf("capture_decisions_action_check = %q, want it to admit 'held'", d)
	}
	if d := def("capture_decisions_mode_check"); !strings.Contains(d, "'gate'") {
		t.Errorf("capture_decisions_mode_check = %q, want it to admit 'gate'", d)
	}
	def("capture_decisions_gate_shape")
	def("capture_decisions_held_is_not_gate")
	var idx string
	if err := s.pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname='capture_decisions_gate_uniq'`).Scan(&idx); err != nil {
		t.Fatalf("capture_decisions_gate_uniq: %v", err)
	}
	if !strings.Contains(idx, "UNIQUE") || !strings.Contains(idx, "(message_id)") || !strings.Contains(idx, "mode = 'gate'") {
		t.Errorf("capture_decisions_gate_uniq = %q, want UNIQUE (message_id) WHERE mode = 'gate'", idx)
	}

	msg := s.mention(t, ctx, "shape", "GTE-1", time.Minute)
	const ins = `INSERT INTO capture_decisions (message_id, mode, matched_rule_id, project_id, action, external_system, external_key)
	             VALUES ($1,$2,$3,$4,$5,$6,$7)`
	try := func(label, wantErr string, queries ...[]any) {
		t.Helper()
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		var last error
		for _, q := range queries {
			_, last = tx.Exec(ctx, q[0].(string), q[1:]...)
			if last != nil {
				break
			}
		}
		switch {
		case wantErr == "" && last != nil:
			t.Errorf("%s: unexpected error %v", label, last)
		case wantErr != "" && (last == nil || !strings.Contains(last.Error(), wantErr)):
			t.Errorf("%s: error = %v, want one containing %q", label, last, wantErr)
		}
	}
	row := func(mode, action string, key any) []any {
		return []any{ins, msg, mode, s.ruleKey, s.gated, action, "jira", key}
	}
	try("a live held row", "", row("live", "held", "GTE-1"))
	try("a shadow held row", "", row("shadow", "held", "GTE-1"))
	try("a gate task row", "", row("gate", "task", "GTE-1"))
	try("a gate row cannot be held", "check constraint", row("gate", "held", "GTE-1"))
	try("a gate row names its key", "check constraint", row("gate", "attributed", nil))
	try("a gate row is never unmatched", "check constraint",
		[]any{ins, msg, "gate", s.ruleKey, nil, "unmatched", "jira", "GTE-1"})
	try("one resolution per message, forever", "duplicate key", row("gate", "task", "GTE-1"), row("gate", "attributed", "GTE-1"))
	try("the restated predicate infers the partial index", "", row("gate", "task", "GTE-1"),
		[]any{ins + ` ON CONFLICT (message_id) WHERE mode = 'gate' DO NOTHING`, msg, "gate", s.ruleKey, s.gated, "attributed", "jira", "GTE-1"})
	try("an UNrestated ON CONFLICT cannot infer it", "no unique or exclusion constraint",
		[]any{ins + ` ON CONFLICT (message_id) DO NOTHING`, msg, "gate", s.ruleKey, s.gated, "task", "jira", "GTE-1"})
	try("the live claim and the gate claim coexist on one message", "", row("live", "held", "GTE-1"), row("gate", "task", "GTE-1"))

	// capture_decisions_gate_task_pin: attributed names no task. task AND
	// task_log are claimed BEFORE the executor acts (claim-before-act) with
	// task_id NULL, and recordDecisionTask completes them afterwards (round-2
	// fix 1), so either may be NULL — in flight, or the report's crash line.
	def("capture_decisions_gate_task_pin")
	task := s.taskWithRef(t, ctx, "GTE-2")
	const insT = `INSERT INTO capture_decisions (message_id, mode, matched_rule_id, project_id, action, external_system, external_key, task_id)
	              VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`
	rowT := func(mode, action string, taskID any) []any {
		return []any{insT, msg, mode, s.ruleKey, s.gated, action, "jira", "GTE-1", taskID}
	}
	try("a gate attributed row names no task", "check constraint", rowT("gate", "attributed", task))
	try("a gate task_log row is claimed before its log is written", "", rowT("gate", "task_log", nil))
	try("a gate task_log row completed with its task", "", rowT("gate", "task_log", task))
	try("a gate task row is claimed before its task exists", "", rowT("gate", "task", nil))
	try("a gate task row completed with its task", "", rowT("gate", "task", task))
	try("the pin binds only gate rows (a live attributed row keeps whatever it had)", "", rowT("live", "attributed", nil))
}

// ---- D1: capture records `held` for gated jira-keyed matches --------------------

// MUTATIONS that turn this red:
//   - gate read from a constant TRUE  → the collaboratory control is held, no task;
//   - gate read from a constant FALSE → the gated key creates a task at capture;
//   - `held` only for would-be `task` → the would-be task_log appends a log;
//   - an unkeyed match held → the sender-rule message is held instead of attributed.
func TestCaptureGate_Integration_CaptureHoldsGatedKeyedMatches(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)

	// A ticket that ALREADY has a task (written through the executor): today's
	// capture would append a log to it. Gated, it must hold instead.
	existing := s.taskWithRef(t, ctx, "GTE-9")
	logsBefore := s.n(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='log'`, existing)

	newKey := s.mention(t, ctx, "d1-new", "GTE-8", 50*time.Minute)
	knownKey := s.mention(t, ctx, "d1-known", "GTE-9", 49*time.Minute)
	unkeyed := s.msg(t, ctx, "d1-unkeyed", cgThreadPrefix+"d1-unkeyed", "Jira <"+cgJiraSender+">",
		"[JIRA] weekly digest", "nothing keyed in here", 48*time.Minute)
	web1 := s.msg(t, ctx, "d1-web1", cgWebPrefix+"77", "Jira <jira@itest-capgate.jira.com>", "WEB-77",
		"first comment", 47*time.Minute)
	web2 := s.msg(t, ctx, "d1-web2", cgWebPrefix+"77", "Jira <jira@itest-capgate.jira.com>", "WEB-77",
		"second comment", 46*time.Minute)

	gatedTasksBefore := s.n(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, s.gated)

	// Shadow decides everything, acts on nothing — and writes held too (D-D1).
	s.shadow(t, ctx, false)
	for label, id := range map[string]int64{"new key": newKey, "known key": knownKey} {
		if d, ok := s.decision(t, ctx, id, "shadow"); !ok || d.action != "held" {
			t.Errorf("shadow decision for the %s = %+v (found %v), want held (D-D1: shadow writes held too)", label, d, ok)
		}
	}

	st := s.live(t, ctx)

	for _, c := range []struct {
		label string
		id    int64
		key   string
	}{{"would-be task", newKey, "GTE-8"}, {"would-be task_log", knownKey, "GTE-9"}} {
		d, ok := s.decision(t, ctx, c.id, "live")
		if !ok || d.action != "held" {
			t.Errorf("live decision for the %s (%s on a gated project) = %q (found %v), want held — capture does "+
				"not call Jira; it records held and the gate stage resolves it (D-D1)", c.label, c.key, d.action, ok)
			continue
		}
		if d.projectID == nil || *d.projectID != s.gated {
			t.Errorf("%s: held project_id = %v, want the gated project %d (held NAMES a project: the "+
				"(action='unmatched') = (project_id IS NULL) CHECK still holds)", c.label, d.projectID, s.gated)
		}
		if d.ruleID == nil || *d.ruleID != s.ruleKey {
			t.Errorf("%s: held matched_rule_id = %v, want rule %d", c.label, d.ruleID, s.ruleKey)
		}
		if deref(d.extSystem) != "jira" || deref(d.extKey) != c.key {
			t.Errorf("%s: held external = (%q, %q), want (jira, %s) — the gate resolves from these",
				c.label, deref(d.extSystem), deref(d.extKey), c.key)
		}
		if d.taskID != nil {
			t.Errorf("%s: held row carries task_id %d; held acted on nothing", c.label, *d.taskID)
		}
	}
	if got := s.n(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, s.gated); got != gatedTasksBefore {
		t.Errorf("tasks on the GATED project went %d -> %d during a live capture pass; O5: no task exists before "+
			"the Jira check", gatedTasksBefore, got)
	}
	if got := s.n(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='log'`, existing); got != logsBefore {
		t.Errorf("the existing GTE-9 task gained %d log event(s) at capture; a gated would-be task_log holds too "+
			"(O5: not assigned → no task AND no task_log)", got-logsBefore)
	}
	if st.TasksCreated != 1 || st.Appended != 1 {
		t.Errorf("live RulesStats = %+v, want TasksCreated 1 and Appended 1 — the gate-OFF control's task and "+
			"log only", st)
	}

	// Unkeyed on a jira-system gated rule: today's attribution, unchanged.
	if d, ok := s.decision(t, ctx, unkeyed, "live"); !ok || d.action != "attributed" || d.projectID == nil || *d.projectID != s.gated {
		t.Errorf("the unkeyed rule-2 message = %+v (found %v), want attributed to the gated project — no key, "+
			"nothing to look up, today's behaviour", d, ok)
	}

	// The gate-OFF control (collaboratory rule 10): byte-identical to today.
	d1, ok1 := s.decision(t, ctx, web1, "live")
	d2, ok2 := s.decision(t, ctx, web2, "live")
	if !ok1 || d1.action != "task" || d1.taskID == nil {
		t.Fatalf("gate-OFF control, first WEB-77 message = %+v (found %v), want task with a task id — a project "+
			"whose ticket_assignee_gate is false must behave exactly as before", d1, ok1)
	}
	if !ok2 || d2.action != "task_log" || d2.taskID == nil || *d2.taskID != *d1.taskID {
		t.Errorf("gate-OFF control, second WEB-77 message = %+v, want task_log on task %d", d2, *d1.taskID)
	}
	if got := s.n(t, ctx, `SELECT count(*) FROM capture_decisions WHERE action='held' AND project_id=$1`, s.plain); got != 0 {
		t.Errorf("%d held decision(s) on the gate-OFF project; the gate column is false there", got)
	}
}

// ---- D4: the gate driver ----------------------------------------------------------

func TestCaptureGate_Integration_AssignedKeyCreatesOneTaskWithRefAndProvenance(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	s.fake.put(cgIssue{"GTE-101", "indeterminate", "In Progress", cgOwnID})
	m := s.mention(t, ctx, "assigned", "GTE-101", 10*time.Minute)
	s.live(t, ctx)

	st := s.gate(t, ctx)

	task, ok := s.refTask(t, ctx, "GTE-101")
	if !ok {
		t.Fatalf("no external_refs row for GTE-101 after the gate resolved an ASSIGNED ticket (stats %+v)", st)
	}
	if got := s.n(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, s.gated); got != 1 {
		t.Errorf("tasks on the gated project = %d, want 1", got)
	}
	var status, assignee string
	var srcThread *int64
	if err := s.pool.QueryRow(ctx, `SELECT status, assignee_type, source_thread_id FROM tasks WHERE id=$1`, task).
		Scan(&status, &assignee, &srcThread); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if status != "ready" || assignee != "human" {
		t.Errorf("gate-created task = (%s, %s), want (ready, human) — exactly what capture creates today", status, assignee)
	}
	if srcThread == nil || *srcThread != s.threadOf(t, ctx, m) {
		t.Errorf("source_thread_id = %v, want the held message's thread %d (provenance, capture's exact helper)",
			srcThread, s.threadOf(t, ctx, m))
	}
	var url *string
	if err := s.pool.QueryRow(ctx, `SELECT external_url FROM external_refs WHERE task_id=$1`, task).Scan(&url); err != nil {
		t.Fatalf("read ref url: %v", err)
	}
	if deref(url) != "https://itest-capgate.example.test/browse/GTE-101" {
		t.Errorf("external_url = %q, want the rule's url_template applied", deref(url))
	}

	g, ok := s.decision(t, ctx, m, "gate")
	if !ok || g.action != "task" || g.taskID == nil || *g.taskID != task {
		t.Errorf("gate decision = %+v (found %v), want mode=gate action=task task_id=%d (claim-before-act, then "+
			"completed with task_id — recordDecisionTask's shape)", g, ok, task)
	}
	if g.ruleID == nil || *g.ruleID != s.ruleKey || deref(g.extKey) != "GTE-101" || g.projectID == nil || *g.projectID != s.gated {
		t.Errorf("gate decision carries (rule %v, key %q, project %v), want (%d, GTE-101, %d)",
			g.ruleID, deref(g.extKey), g.projectID, s.ruleKey, s.gated)
	}
	if h, _ := s.decision(t, ctx, m, "live"); h.action != "held" || h.taskID != nil {
		t.Errorf("the live held row was rewritten (%+v); the resolution is a SECOND row, the held row stays", h)
	}
	if mode, action := s.latest(t, ctx, m); mode != "gate" || action != "task" {
		t.Errorf("latest decision = (%s, %s), want (gate, task): every latest-decision reader follows it", mode, action)
	}

	// Invariant 3: every write went through the executor as capture:gate.
	if got := s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE actor=$1 AND tool='create_task'`, cgGateActor); got != 1 {
		t.Errorf("create_task audit rows by %s = %d, want 1", cgGateActor, got)
	}
	for _, tool := range []string{"link_external_ref", "task_set_source_thread"} {
		if got := s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE actor=$1 AND tool=$2 AND task_id=$3`,
			cgGateActor, tool, task); got != 1 {
			t.Errorf("%s audit rows by %s on task %d = %d, want 1", tool, cgGateActor, task, got)
		}
	}
	if st.TasksCreated != 1 || st.Resolved != 1 || st.PendingLookup != 0 {
		t.Errorf("GateStats = %+v, want TasksCreated 1, Resolved 1, PendingLookup 0", st)
	}
}

// V3 MUTATION: drop the per-message ref re-query under the lock (e.g. resolve
// refs once before the loop) → the second mention creates a SECOND task (or
// fails on external_refs' unique key) and this goes red.
func TestCaptureGate_Integration_TwoHeldMentionsOfANewKeyMakeOneTaskAndOneLog(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	s.fake.put(cgIssue{"GTE-102", "indeterminate", "In Progress", cgOwnID})
	m1 := s.mention(t, ctx, "twice-1", "GTE-102", 20*time.Minute)
	m2 := s.mention(t, ctx, "twice-2", "GTE-102", 10*time.Minute)
	s.live(t, ctx)
	for _, m := range []int64{m1, m2} {
		if d, _ := s.decision(t, ctx, m, "live"); d.action != "held" {
			t.Fatalf("setup: message %d live decision = %q, want held (both mentions predate any task)", m, d.action)
		}
	}

	st := s.gate(t, ctx)

	if got := s.n(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, s.gated); got != 1 {
		t.Errorf("tasks = %d for two held mentions of ONE ticket, want 1 — each held message's ref is re-queried "+
			"under the lock, so the second logs onto the first's task (D-D4)", got)
	}
	task, ok := s.refTask(t, ctx, "GTE-102")
	if !ok {
		t.Fatalf("no ref for GTE-102")
	}
	g1, _ := s.decision(t, ctx, m1, "gate")
	g2, _ := s.decision(t, ctx, m2, "gate")
	if g1.action != "task" || g2.action != "task_log" || g2.taskID == nil || *g2.taskID != task {
		t.Errorf("gate decisions = (%s, %s on %v), want (task, task_log on %d): oldest first", g1.action, g2.action, g2.taskID, task)
	}
	if got := s.n(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='log'`, task); got != 1 {
		t.Errorf("log events on the task = %d, want 1 (the second mention)", got)
	}
	if got := s.fake.getsFor("GTE-102"); got != 1 {
		t.Errorf("GETs for GTE-102 = %d, want 1: two mentions of one ticket cost ONE GET (D-D5)", got)
	}
	if st.TasksCreated != 1 || st.Appended != 1 {
		t.Errorf("GateStats = %+v, want TasksCreated 1, Appended 1", st)
	}
}

func TestCaptureGate_Integration_UnassignedKeyIsAttributedWithNoTask(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	s.fake.put(cgIssue{"GTE-103", "indeterminate", "In Progress", cgOtherID})
	m := s.mention(t, ctx, "notmine", "GTE-103", 10*time.Minute)
	s.live(t, ctx)
	eventsBefore := s.n(t, ctx, `SELECT count(*) FROM task_events`)

	st := s.gate(t, ctx)

	g, ok := s.decision(t, ctx, m, "gate")
	if !ok || g.action != "attributed" {
		t.Fatalf("gate decision for a ticket assigned to someone else = %+v (found %v), want attributed (O5: not "+
			"his → no task, no log)", g, ok)
	}
	if !strings.Contains(deref(g.reason), "not_assigned") {
		t.Errorf("gate reason = %q, want it to name the drop (not_assigned)", deref(g.reason))
	}
	if g.taskID != nil || g.projectID == nil || *g.projectID != s.gated || deref(g.extKey) != "GTE-103" {
		t.Errorf("attributed gate row = %+v, want no task, the gated project and key GTE-103", g)
	}
	if _, ok := s.refTask(t, ctx, "GTE-103"); ok {
		t.Errorf("an external_refs row exists for an unassigned ticket")
	}
	if got := s.n(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, s.gated); got != 0 {
		t.Errorf("tasks on the gated project = %d, want 0", got)
	}
	if got := s.n(t, ctx, `SELECT count(*) FROM task_events`); got != eventsBefore {
		t.Errorf("task_events moved %d -> %d on an attributed resolution (no executor call, SPEC API section)", eventsBefore, got)
	}
	if got := s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE actor=$1`, cgGateActor); got != 0 {
		t.Errorf("%d audit rows by %s; an attributed resolution makes no executor call", got, cgGateActor)
	}
	if st.Attributed != 1 || st.TasksCreated != 0 {
		t.Errorf("GateStats = %+v, want Attributed 1, TasksCreated 0", st)
	}
}

func TestCaptureGate_Integration_JiraDownStaysHeld(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	s.fake.put(cgIssue{"GTE-104", "indeterminate", "In Progress", cgOwnID})
	s.fake.errAll(true)
	m := s.mention(t, ctx, "down", "GTE-104", 10*time.Minute)
	s.live(t, ctx)

	st := s.gate(t, ctx)

	if _, ok := s.decision(t, ctx, m, "gate"); ok {
		t.Errorf("a gate row was written while Jira was unreachable; an unreadable hold STAYS held (fail closed, " +
			"retried on the next sweep)")
	}
	if mode, action := s.latest(t, ctx, m); mode != "live" || action != "held" {
		t.Errorf("latest decision = (%s, %s), want (live, held)", mode, action)
	}
	if got := s.n(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, s.gated); got != 0 {
		t.Errorf("tasks = %d with Jira down, want 0", got)
	}
	if st.PendingLookup != 1 || st.Resolved != 0 {
		t.Errorf("GateStats = %+v, want PendingLookup 1 and Resolved 0 — a pass whose holds all stay pending "+
			"reports nothing processed, so the stage loop waits for the sweep", st)
	}
}

// Review fix 4: GateMaxAge runs from when the HOLD was written, not from the
// message's sent_at. The control is an OLD message (sent 73h ago, captured just
// now) whose hold is fresh: it must stay pending, not expire on arrival.
func TestCaptureGate_Integration_ExpiredHoldIsAttributedUnverified(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	s.fake.errAll(true)
	expired := s.mention(t, ctx, "expired", "GTE-105", time.Hour)
	lateOld := s.mention(t, ctx, "late-old", "GTE-115", capture.GateMaxAge+time.Hour)
	s.live(t, ctx)
	for _, m := range []int64{expired, lateOld} {
		if d, _ := s.decision(t, ctx, m, "live"); d.action != "held" {
			t.Fatalf("setup: message %d live decision = %q, want held (inside the 720h live horizon)", m, d.action)
		}
	}
	// Age the HOLD (the row the real live pass wrote), not the message.
	if _, err := s.pool.Exec(ctx,
		`UPDATE capture_decisions SET created_at = now() - make_interval(secs => $2)
		  WHERE message_id = $1 AND mode = 'live'`, expired, (capture.GateMaxAge + time.Hour).Seconds()); err != nil {
		t.Fatalf("age the hold: %v", err)
	}

	st := s.gate(t, ctx)

	g, ok := s.decision(t, ctx, expired, "gate")
	if !ok || g.action != "attributed" || !strings.Contains(deref(g.reason), "gate_unverified_expired") {
		t.Errorf("gate decision for a hold written more than GateMaxAge ago and still unreadable = %+v (found %v), "+
			"want attributed with reason gate_unverified_expired — fail closed, no task (D-D5)", g, ok)
	}
	if _, ok := s.decision(t, ctx, lateOld, "gate"); ok {
		t.Errorf("a hold written just now on a message SENT %s ago was resolved; GateMaxAge is measured from "+
			"the hold's created_at, so a late-captured message still gets its lookups", capture.GateMaxAge+time.Hour)
	}
	if got := s.n(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, s.gated); got != 0 {
		t.Errorf("tasks = %d, want 0", got)
	}
	if st.Attributed != 1 || st.PendingLookup != 1 {
		t.Errorf("GateStats = %+v, want Attributed 1 (the aged hold) and PendingLookup 1 (the fresh hold)", st)
	}
}

// SWT-36 on the gate path: a warranted mention of a ticket whose task a human
// DISMISSED logs onto it, then calls the GUARDED task_reopen as capture:gate.
func TestCaptureGate_Integration_ExistingRefOnADismissedTaskLogsAndReopens(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	s.fake.put(cgIssue{"GTE-106", "indeterminate", "In Progress", cgOwnID})
	s.mention(t, ctx, "dis-1", "GTE-106", 30*time.Minute)
	s.live(t, ctx)
	s.gate(t, ctx)
	task, ok := s.refTask(t, ctx, "GTE-106")
	if !ok {
		t.Fatalf("setup: the first mention did not create a task through the gate")
	}
	if _, err := s.ex.Execute(ctx, executor.Call{Tool: "task_dismiss", Actor: cgHuman, TaskID: &task,
		Args: []byte(fmt.Sprintf(`{"task_id":%d,"reason_code":"handled_elsewhere"}`, task))}); err != nil {
		t.Fatalf("setup task_dismiss: %v", err)
	}
	var dismissal int64
	if err := s.pool.QueryRow(ctx, `SELECT id FROM task_dismissals WHERE task_id=$1 ORDER BY id DESC LIMIT 1`, task).Scan(&dismissal); err != nil {
		t.Fatalf("read dismissal: %v", err)
	}
	logsBefore := s.n(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='log'`, task)

	m2 := s.mention(t, ctx, "dis-2", "GTE-106", time.Minute)
	s.live(t, ctx)
	if d, _ := s.decision(t, ctx, m2, "live"); d.action != "held" {
		t.Fatalf("setup: follow-up live decision = %q, want held (a would-be task_log on a gated project)", d.action)
	}
	st := s.gate(t, ctx)

	g, _ := s.decision(t, ctx, m2, "gate")
	if g.action != "task_log" || g.taskID == nil || *g.taskID != task {
		t.Errorf("gate decision = %+v, want task_log on the dismissed task %d", g, task)
	}
	if got := s.n(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='log'`, task); got != logsBefore+1 {
		t.Errorf("log events %d -> %d, want +1", logsBefore, got)
	}
	var status string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, task).Scan(&status); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if status != "ready" {
		t.Errorf("dismissed task status after a warranted follow-up = %q, want ready (SWT-36's guarded reopen, "+
			"as capture does today)", status)
	}
	var by *string
	var byMsg *int64
	if err := s.pool.QueryRow(ctx, `SELECT reopened_by, reopened_by_message_id FROM task_dismissals WHERE id=$1`, dismissal).
		Scan(&by, &byMsg); err != nil {
		t.Fatalf("read dismissal stamp: %v", err)
	}
	if deref(by) != cgGateActor || byMsg == nil || *byMsg != m2 {
		t.Errorf("dismissal stamp = (%q, %v), want (%s, %d)", deref(by), byMsg, cgGateActor, m2)
	}
	if got := s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE actor=$1 AND tool='task_reopen' AND task_id=$2`,
		cgGateActor, task); got != 1 {
		t.Errorf("task_reopen audit rows by %s = %d, want 1 (invariant 3)", cgGateActor, got)
	}
	var lastLog, lastStatus int64
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE((SELECT max(id) FROM task_events WHERE task_id=$1 AND event_type='log'),0),
		        COALESCE((SELECT max(id) FROM task_events WHERE task_id=$1 AND event_type='status_changed'),0)`,
		task).Scan(&lastLog, &lastStatus); err != nil {
		t.Fatalf("read event order: %v", err)
	}
	if !(lastLog < lastStatus) {
		t.Errorf("event order: last log %d, last status_changed %d — SWT-36 D10: log first, then reopen", lastLog, lastStatus)
	}
	if st.Appended != 1 || st.Reopened != 1 {
		t.Errorf("GateStats = %+v, want Appended 1, Reopened 1", st)
	}
}

func TestCaptureGate_Integration_RunTwiceWritesNothingNew(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	s.fake.put(cgIssue{"GTE-107", "indeterminate", "In Progress", cgOwnID})
	s.fake.put(cgIssue{"GTE-117", "indeterminate", "In Progress", cgOtherID})
	s.mention(t, ctx, "twice-a", "GTE-107", 10*time.Minute)
	s.mention(t, ctx, "twice-b", "GTE-117", 9*time.Minute)
	s.live(t, ctx)
	s.gate(t, ctx)
	if got := s.gateRows(t, ctx); got != 2 {
		t.Fatalf("setup: the first gate pass wrote %d gate rows, want 2 (one task, one attributed) — a rerun "+
			"check over a pass that did nothing proves nothing", got)
	}

	type snap struct{ tasks, refs, events, decisions, audits, syncRuns, requests int }
	take := func() snap {
		return snap{
			tasks:     s.n(t, ctx, `SELECT count(*) FROM tasks`),
			refs:      s.n(t, ctx, `SELECT count(*) FROM external_refs`),
			events:    s.n(t, ctx, `SELECT count(*) FROM task_events`),
			decisions: s.n(t, ctx, `SELECT count(*) FROM capture_decisions`),
			audits:    s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE actor=$1`, cgGateActor),
			syncRuns:  s.n(t, ctx, `SELECT count(*) FROM sync_runs WHERE source_account_id=$1`, s.lookupAcct),
			requests:  s.fake.requests(),
		}
	}
	before := take()
	st := s.gate(t, ctx)
	if after := take(); after != before {
		t.Errorf("a second gate pass over an already-resolved inbox changed the world: %+v -> %+v (one "+
			"resolution per message, forever; an empty inbox costs no lookup)", before, after)
	}
	if !reflect.DeepEqual(st, capture.GateStats{}) {
		t.Errorf("second pass stats = %+v, want the zero value", st)
	}
}

// ---- D5: rate and the shared snapshot -----------------------------------------

func TestCaptureGate_Integration_FetchesAtMostGateMaxKeysPerPass(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	const total = 60
	for i := 0; i < total; i++ {
		key := fmt.Sprintf("GTE-%d", 2001+i)
		s.fake.put(cgIssue{key, "indeterminate", "In Progress", cgOwnID})
		s.mention(t, ctx, fmt.Sprintf("rate-%02d", i), key, time.Duration(total-i)*time.Minute)
	}
	s.live(t, ctx)
	if got := s.pendingHolds(t, ctx); got != total {
		t.Fatalf("setup: %d pending holds, want %d", got, total)
	}

	myselfBefore := s.fake.myselfCalls()
	st1 := s.gate(t, ctx)
	first := s.fake.issueGets()
	if len(first) == 0 || len(first) > capture.GateMaxKeysPerPass {
		t.Errorf("issue GETs in one pass over %d distinct held keys = %d, want 1..%d (D-D5: the rest stay held "+
			"for the next pass or sweep)", total, len(first), capture.GateMaxKeysPerPass)
	}
	if got := s.fake.myselfCalls() - myselfBefore; got > 1 {
		t.Errorf("/myself calls in one pass = %d, want at most 1 per account per pass (D-D5)", got)
	}
	if got := s.pendingHolds(t, ctx); got != total-len(first) {
		t.Errorf("pending holds after pass 1 = %d, want %d (every fetched key resolved, every unfetched one still held)",
			got, total-len(first))
	}
	if st1.TasksCreated != len(first) {
		t.Errorf("pass 1 created %d tasks for %d fetched (all assigned) keys", st1.TasksCreated, len(first))
	}
	if st1.BudgetSkipped != total-len(first) {
		t.Errorf("pass 1 BudgetSkipped = %d, want %d: the holds left over the %d-key budget are counted, so "+
			"the pass log says why they are still held (review fix 8)", st1.BudgetSkipped, total-len(first),
			capture.GateMaxKeysPerPass)
	}

	s.gate(t, ctx)
	all := s.fake.issueGets()
	if s.pendingHolds(t, ctx) != 0 {
		t.Errorf("pending holds after pass 2 = %d, want 0", s.pendingHolds(t, ctx))
	}
	seen := map[string]int{}
	for _, k := range all {
		seen[k]++
	}
	for k, n := range seen {
		if n > 1 {
			t.Errorf("%s fetched %d times across two passes; a key fetched within the TTL is not re-fetched", k, n)
		}
	}
	if len(seen) != total {
		t.Errorf("distinct keys fetched over two passes = %d, want %d", len(seen), total)
	}
}

// D-D5: the cache IS the reconciler's stored snapshot. A key the gate fetched
// is not re-fetched by the next reconciler pass, and a key the reconciler
// fetched is not re-fetched by the gate.
func TestCaptureGate_Integration_SnapshotIsSharedWithTheReconciler(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	cfg := ticketstatus.Config{Lookup: s.fake.factory()}

	// Gate first, reconciler second.
	s.fake.put(cgIssue{"GTE-301", "indeterminate", "In Progress", cgOwnID})
	s.mention(t, ctx, "shared-1", "GTE-301", 10*time.Minute)
	s.live(t, ctx)
	s.gate(t, ctx)
	if got := s.fake.getsFor("GTE-301"); got != 1 {
		t.Fatalf("setup: GETs for GTE-301 after the gate = %d, want 1", got)
	}
	ts, err := ticketstatus.Run(ctx, s.pool, s.ex, cfg)
	if err != nil {
		t.Fatalf("ticketstatus.Run: %v", err)
	}
	if got := s.fake.getsFor("GTE-301"); got != 1 {
		t.Errorf("the reconciler re-fetched GTE-301 (%d GETs) inside the TTL of the gate's fetch; they share the "+
			"stored snapshot under IssueRawID (D-D5)", got)
	}
	if ts.FetchSkippedTTL < 1 {
		t.Errorf("reconciler FetchSkippedTTL = %d, want >= 1", ts.FetchSkippedTTL)
	}

	// Reconciler first, gate second. The mention is first seen BEFORE the
	// reconciler's fetch: a snapshot older than the message may not decide it
	// (review fix 1; gate_freshness_integration_test.go), so the shared-cache
	// property is the one for a message the snapshot post-dates.
	existing := s.taskWithRef(t, ctx, "GTE-302")
	s.fake.put(cgIssue{"GTE-302", "indeterminate", "In Progress", cgOwnID})
	m := s.mention(t, ctx, "shared-2", "GTE-302", time.Minute)
	if _, err := ticketstatus.Run(ctx, s.pool, s.ex, cfg); err != nil {
		t.Fatalf("ticketstatus.Run: %v", err)
	}
	if got := s.fake.getsFor("GTE-302"); got != 1 {
		t.Fatalf("setup: the reconciler fetched GTE-302 %d times, want 1", got)
	}
	s.live(t, ctx)
	s.gate(t, ctx)
	if got := s.fake.getsFor("GTE-302"); got != 1 {
		t.Errorf("the gate re-fetched GTE-302 (%d GETs) inside the TTL of the reconciler's fetch", got)
	}
	if g, _ := s.decision(t, ctx, m, "gate"); g.action != "task_log" || g.taskID == nil || *g.taskID != existing {
		t.Errorf("gate decision = %+v, want task_log on %d decided from the reconciler's stored snapshot", g, existing)
	}
}

// ---- D-D2: the shadow-overwrite guard -----------------------------------------

// MUTATION: drop the gate/route exclusion from pendingMessages → a shadow
// `--all` pass writes a NEWER shadow row that every latest-decision reader
// follows, burying the gate's resolution, and this goes red.
func TestCaptureGate_Integration_ShadowPassNeverOverwritesAGateResolution(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	s.fake.put(cgIssue{"GTE-501", "indeterminate", "In Progress", cgOwnID})
	m := s.mention(t, ctx, "guard", "GTE-501", 10*time.Minute)
	s.live(t, ctx)
	s.gate(t, ctx)
	if g, ok := s.decision(t, ctx, m, "gate"); !ok || g.action != "task" {
		t.Fatalf("setup: no gate task row for the guard message (%+v, found %v); the guard has nothing to guard", g, ok)
	}
	before := s.n(t, ctx, `SELECT count(*) FROM capture_decisions WHERE message_id=$1`, m)

	s.shadow(t, ctx, false)
	s.shadow(t, ctx, true) // --all: the documented post-rule-change re-pointing pass (A-D6)
	s.live(t, ctx)

	if got := s.n(t, ctx, `SELECT count(*) FROM capture_decisions WHERE message_id=$1`, m); got != before {
		t.Errorf("decision rows for a gate-resolved message went %d -> %d across shadow, shadow --all and live "+
			"passes; pendingMessages must exclude messages carrying a gate row in EVERY mode (D-D2)", before, got)
	}
	if mode, action := s.latest(t, ctx, m); mode != "gate" || action != "task" {
		t.Errorf("latest decision = (%s, %s), want (gate, task) still", mode, action)
	}
}

// ---- D6: the report ---------------------------------------------------------------

func TestCaptureGate_Integration_ReportRendersHeldGateAndPendingLookup(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	s.fake.put(cgIssue{"GTE-401", "indeterminate", "In Progress", cgOwnID})
	s.fake.put(cgIssue{"GTE-402", "indeterminate", "In Progress", cgOtherID})
	// GTE-403 is on nobody's server: a per-key 404, no snapshot, stays held.
	s.mention(t, ctx, "rep-1", "GTE-401", 12*time.Minute)
	s.mention(t, ctx, "rep-2", "GTE-402", 11*time.Minute)
	s.mention(t, ctx, "rep-3", "GTE-403", 10*time.Minute)
	s.live(t, ctx)
	s.gate(t, ctx)

	report, err := capture.Report(ctx, s.pool, time.Time{}, "")
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	t.Logf("report:\n%s", report)
	lines := gateSection(report)
	if len(lines) == 0 {
		t.Fatalf("the report has no GATE section (a header line starting with GATE); D6: held, gate resolutions " +
			"and pending_lookup render on their own lines")
	}
	want := []struct {
		label string
		match func(f []string) bool
		count string
	}{
		{"held", func(f []string) bool { return f[0] == "held" }, "3"},
		{"pending_lookup", func(f []string) bool { return f[0] == "pending_lookup" }, "1"},
		{"gate task", func(f []string) bool { return hasField(f, "gate") && hasField(f, "task") }, "1"},
		{"gate attributed not_assigned", func(f []string) bool {
			return hasField(f, "gate") && hasField(f, "attributed") && hasField(f, "not_assigned")
		}, "1"},
	}
	for _, w := range want {
		found := false
		for _, line := range lines {
			f := strings.Fields(line)
			if len(f) < 2 || !w.match(f) {
				continue
			}
			found = true
			if f[len(f)-1] != w.count {
				t.Errorf("GATE line %q: count %s, want %s", line, f[len(f)-1], w.count)
			}
		}
		if !found {
			t.Errorf("GATE section has no %q line; lines:\n%s", w.label, strings.Join(lines, "\n"))
		}
	}
}

func gateSection(report string) []string {
	var out []string
	in := false
	for _, line := range strings.Split(report, "\n") {
		switch {
		case strings.HasPrefix(line, "GATE"):
			in = true
			continue
		case line != "" && !strings.HasPrefix(line, " "):
			in = false
		}
		if in && strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func hasField(fields []string, want string) bool {
	for _, f := range fields {
		if f == want {
			return true
		}
	}
	return false
}

// ---- the lock (E-D4 / D-D4) -------------------------------------------------------

func TestCaptureGate_Integration_LockHeldIsASkipNotAWrite(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	s.fake.put(cgIssue{"GTE-601", "indeterminate", "In Progress", cgOwnID})
	s.mention(t, ctx, "lock", "GTE-601", 10*time.Minute)
	s.live(t, ctx)

	holder, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer holder.Release()
	var taken bool
	if err := holder.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, cgLockKey).Scan(&taken); err != nil || !taken {
		t.Fatalf("take 0x%X: taken=%v err=%v", cgLockKey, taken, err)
	}
	defer func() {
		var ok bool
		_ = holder.QueryRow(context.Background(), `SELECT pg_advisory_unlock($1)`, cgLockKey).Scan(&ok)
	}()

	st, err := capture.RunGate(ctx, s.pool, s.ex, capture.GateConfig{Limit: 200, Lookup: s.fake.factory()})
	if !errors.Is(err, capture.ErrGateLockHeld) {
		t.Errorf("RunGate with capture's lock held elsewhere: err = %v, want errors.Is(err, capture.ErrGateLockHeld) "+
			"(pipelined maps it to pipeline.ErrLockHeld: retry in 30s, never an error)", err)
	}
	if !reflect.DeepEqual(st, capture.GateStats{}) {
		t.Errorf("stats = %+v, want zero", st)
	}
	if s.gateRows(t, ctx) != 0 || s.fake.requests() != 0 {
		t.Errorf("the pass acted while 0x%X was held: %d gate rows, %d Jira requests", cgLockKey, s.gateRows(t, ctx), s.fake.requests())
	}
}

// ---- the fake Jira server -----------------------------------------------------------

type cgIssue struct {
	key, category, statusName, assignee string // assignee "" = null
}

func cgIssueJSON(iss cgIssue) string {
	assignee := `null`
	if iss.assignee != "" {
		assignee = `{"accountId":"` + iss.assignee + `"}`
	}
	return `{"id":"1","key":"` + iss.key + `","fields":{"summary":"itest ` + iss.key + `",` +
		`"description":"body","created":"2026-08-01T10:00:00.000+0000","updated":"2026-09-01T10:00:00.000+0000",` +
		`"reporter":{"accountId":"` + cgOtherID + `"},` +
		`"status":{"name":"` + iss.statusName + `","id":"3","statusCategory":{"id":3,"key":"` + iss.category +
		`","colorName":"blue-gray","name":"a category name"}},"assignee":` + assignee + `}}`
}

type cgFakeJira struct {
	mu     sync.Mutex
	srv    *httptest.Server
	issues map[string]cgIssue
	gets   []string
	myself int
	reqs   int
	fail   bool
	// onIssueGet runs inside every issue GET, before the response (with f.mu
	// held: it must not call f's methods). The no-lock-across-HTTP test uses it.
	onIssueGet func()
}

func (f *cgFakeJira) setOnIssueGet(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onIssueGet = fn
}

func newCGFakeJira() *cgFakeJira {
	f := &cgFakeJira{issues: map[string]cgIssue{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *cgFakeJira) close()      { f.srv.Close() }
func (f *cgFakeJira) url() string { return f.srv.URL }

func (f *cgFakeJira) put(iss cgIssue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues[iss.key] = iss
}

func (f *cgFakeJira) errAll(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = on
}

func (f *cgFakeJira) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs
}

func (f *cgFakeJira) myselfCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.myself
}

// issueGets is every successful issue GET, in order.
func (f *cgFakeJira) issueGets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.gets...)
	sort.Strings(out)
	return out
}

func (f *cgFakeJira) getsFor(key string) int {
	n := 0
	for _, k := range f.issueGets() {
		if k == key {
			n++
		}
	}
	return n
}

// factory is shaped exactly like the token-decrypting closure pipelined holds,
// and aimed at acct.SiteBaseURL.
func (f *cgFakeJira) factory() jira.ClientFactory {
	return func(_ context.Context, acct jira.Account) (*jira.Client, error) {
		return jira.NewClient(http.DefaultClient, acct.SiteBaseURL, acct.Email, "itest-token"), nil
	}
}

func (f *cgFakeJira) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs++
	if f.fail {
		http.Error(w, `{"errorMessages":["itest forced failure"]}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/rest/api/2/myself":
		f.myself++
		_ = json.NewEncoder(w).Encode(map[string]any{"accountId": cgOwnID})
	case strings.HasPrefix(r.URL.Path, "/rest/api/2/issue/"):
		if f.onIssueGet != nil {
			f.onIssueGet()
		}
		key := strings.TrimPrefix(r.URL.Path, "/rest/api/2/issue/")
		iss, ok := f.issues[key]
		if !ok {
			http.Error(w, `{"errorMessages":["no issue"]}`, http.StatusNotFound)
			return
		}
		f.gets = append(f.gets, key)
		_, _ = io.WriteString(w, cgIssueJSON(iss))
	default:
		http.Error(w, `{"errorMessages":["no route: `+r.URL.Path+`"]}`, http.StatusNotFound)
	}
}
