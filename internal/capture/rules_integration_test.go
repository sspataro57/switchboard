//go:build integration

package capture_test

// Integration tests for the capture-rules driver (SWT-17,
// docs/tickets/capture-rules_SPEC.md §4-§7; acceptance criteria 3, 6, 7, 8, 9,
// 11, 12, 13, 19). Build-tagged `integration` AND env-gated on DATABASE_URL. No
// LLM, no provider, no network: the pass reads normalized tables and writes
// capture_decisions plus executor tool calls.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run CaptureRules ./internal/capture/
//
// GREENFIELD NOTE: fails today three ways — migration 0015 does not exist (no
// capture_rules / capture_decisions tables), and capture.EvaluateRules /
// capture.RulesConfig / capture.RulesStats do not exist. Expected red state.
//
// IMPOSED surface (declared by the implementer; SPEC §5-§6 fix the behaviour):
//
//	func EvaluateRules(ctx context.Context, pool *pgxpool.Pool, ex *executor.Executor,
//	                   cfg RulesConfig) (RulesStats, error)
//	type RulesConfig struct { Mode string; Horizon time.Duration; Limit int }
//
// RulesStats' FIELDS are deliberately not asserted anywhere except as a zero value
// (criterion 13), because the SPEC names a JSON stats line but not Go field names.
// Everything else is asserted against the database, which is the contract.
//
// Cross-suite discipline (the serialized cleanup pact). This suite is different
// from every other in one way that must be stated: EvaluateRules' pending filter
// is GLOBAL (like triage's), so a run here evaluates other suites' leftover
// inbound messages too and writes a capture_decisions row for each. Those rows
// carry an FK to normalized_messages, so leaving them behind would make ANOTHER
// suite's cleanup fail with a foreign-key violation that reads like a bug in that
// suite. This suite therefore deletes capture_decisions WHOLESALE (compose db
// only — the guard below refuses the production DSN), and owns:
//   - projects itest-caprules-reengine / itest-caprules-collab
//   - source_accounts (slack_web, tcaprules@slack-web.local) and
//     (jira, itest-caprules-jira@example.test) and (google, itest-caprules@gmail.example.test)
//   - thread_key prefixes slack:TCAPRULES:%, jira:caprules.jira.com:%, gmail:itest-caprules:%

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	crReengineSlug = "itest-caprules-reengine"
	crCollabSlug   = "itest-caprules-collab"

	// The slack workspace id as SLACK spells it, and the synthetic account as the
	// CONNECTOR writes it — `strings.ToLower(workspace.ID) + "@slack-web.local"`
	// (slackweb/sink.go:28). The two spellings differing is the point; see
	// TestCaptureRules_Integration_LiveCreatesOneTaskPerTicket's rule seeding.
	crWorkspace    = "TCAPRULES"
	crSlackAccount = "tcaprules@slack-web.local"
	crJiraAccount  = "itest-caprules-jira@example.test"
	crMailAccount  = "itest-caprules@gmail.example.test"

	crSlackThread = "slack:TCAPRULES:C0CAPRULES"
	crJiraThread  = "jira:caprules.jira.com:WEB-1204"
	crMailThread  = "gmail:itest-caprules:18f0caprules"

	crTicketKey = "LHH-23637"
	crTicketURL = "https://avviato.atlassian.net/browse/LHH-23637"

	// SPEC §5: "pg_try_advisory_lock with key 0x5157_0015 (the established
	// convention: orchestrator 0x51570005, triage 0x51570006)".
	crAdvisoryLockKey = int64(0x5157_0015)
)

// ---- harness --------------------------------------------------------------------

type crSuite struct {
	pool      *pgxpool.Pool
	ex        *executor.Executor
	reengine  int64
	collab    int64
	ruleLHH   int64
	ruleWS    int64
	ruleJira  int64
	messages  map[string]int64 // label -> normalized_messages.id
	rawItems  map[string]int64 // label -> raw_source_items.id
	threadIDs map[string]int64
}

func newCRSuite(t *testing.T, ctx context.Context) *crSuite {
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
	cleanupCaptureRules(t, ctx, pool)
	t.Cleanup(func() { cleanupCaptureRules(t, ctx, pool) })

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))

	s := &crSuite{
		pool: pool, ex: executor.New(reg, checker, audit.NewPGStore(pool)),
		messages: map[string]int64{}, rawItems: map[string]int64{}, threadIDs: map[string]int64{},
	}
	s.seed(t, ctx)
	return s
}

func cleanupCaptureRules(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE (provider, account_email) IN (
	                 ('slack_web','` + crSlackAccount + `'),
	                 ('jira','` + crJiraAccount + `'),
	                 ('google','` + crMailAccount + `')))`
	const projs = `(SELECT id FROM projects WHERE slug IN ('` + crReengineSlug + `','` + crCollabSlug + `'))`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	stmts := []string{
		// capture_decisions first: it FKs both normalized_messages and tasks, and
		// this suite's pass writes rows for foreign corpora too (see the header).
		`DELETE FROM capture_decisions`,
		`DELETE FROM external_refs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM deliveries WHERE task_id IN ` + tasksOf,
		// The pass reaches tasks/external_refs/task_events through the executor,
		// which writes an audit_events row per call — and audit_events.task_id FKs
		// tasks, so the delete below fails without this. policy_decisions first:
		// the FK points that way. Also swept by actor namespace, because the audit
		// row for a create_task call is written BEFORE the task exists and so
		// carries no task_id to scope by.
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor LIKE 'capture:%')`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor LIKE 'capture:%'`,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM capture_rules WHERE project_id IN ` + projs,
		`DELETE FROM projects WHERE slug IN ('` + crReengineSlug + `','` + crCollabSlug + `')`,
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`,
		`DELETE FROM normalized_threads WHERE thread_key IN ('` + crSlackThread + `','` + crJiraThread + `','` + crMailThread + `')`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE (provider, account_email) IN (
		   ('slack_web','` + crSlackAccount + `'),
		   ('jira','` + crJiraAccount + `'),
		   ('google','` + crMailAccount + `'))`,
	}
	for _, q := range stmts {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func (s *crSuite) scanInt(t *testing.T, ctx context.Context, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n
}

func (s *crSuite) insID(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", q, err)
	}
	return id
}

func (s *crSuite) account(t *testing.T, ctx context.Context, provider, email string) int64 {
	t.Helper()
	return s.insID(t, ctx,
		`INSERT INTO source_accounts (provider, account_email, scopes, send_enabled, calendar_in_availability)
		 VALUES ($1,$2,'{}',false,false)
		 ON CONFLICT (provider, account_email) DO UPDATE SET account_email=EXCLUDED.account_email
		 RETURNING id`, provider, email)
}

// seed builds the corpus and the rule set. Everything is real-shaped: the slack
// account is the lowercased synthetic identity, the sender is a raw From header,
// the jira thread_key is `jira:{site_host}:{KEY}`.
func (s *crSuite) seed(t *testing.T, ctx context.Context) {
	t.Helper()

	s.reengine = s.insID(t, ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,'','manual','dashboard','/tmp/itest-caprules', 'any') RETURNING id`, crReengineSlug)
	s.collab = s.insID(t, ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,'','manual','dashboard','/tmp/itest-caprules', 'any') RETURNING id`, crCollabSlug)

	// The acceptance rule set, in miniature. Rule ids are assigned by the
	// sequence, so priority is what must separate them — not insertion order.
	s.ruleWS = s.insID(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, priority, enabled, note)
		 VALUES ($1,'source_slack_workspace',$2,NULL,1,true,'itest-caprules catch-all') RETURNING id`,
		s.collab, crWorkspace)
	s.ruleJira = s.insID(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, url_template, priority, enabled, note)
		 VALUES ($1,'thread_key_prefix','jira:caprules.jira.com:WEB-','jira','[A-Z]+-[0-9]+$',
		         'https://caprules.jira.com/browse/{key}',50,true,'itest-caprules WEB') RETURNING id`, s.collab)
	s.ruleLHH = s.insID(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, url_template, priority, enabled, note)
		 VALUES ($1,'body_regex','LHH-[0-9]+','jira',NULL,$2,100,true,'itest-caprules LHH') RETURNING id`,
		s.reengine, "https://avviato.atlassian.net/browse/{key}")

	// The catch-all must have been created FIRST (lowest id) and carry the LOWEST
	// priority, so "priority DESC" is the only thing that can put the LHH rule in
	// front of it. If ids alone decided, the workspace rule would win.
	if !(s.ruleWS < s.ruleLHH) {
		t.Fatalf("fixture invalid: the priority-1 catch-all (id %d) must have a LOWER id than the priority-100 "+
			"LHH rule (id %d), or ordering by id would coincide with ordering by priority and prove nothing",
			s.ruleWS, s.ruleLHH)
	}

	slackAcct := s.account(t, ctx, "slack_web", crSlackAccount)
	jiraAcct := s.account(t, ctx, "jira", crJiraAccount)
	mailAcct := s.account(t, ctx, "google", crMailAccount)

	s.threadIDs["slack"] = s.insID(t, ctx,
		`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,'itest caprules','[]') RETURNING id`, crSlackThread)
	s.threadIDs["jira"] = s.insID(t, ctx,
		`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,'WEB-1204','[]') RETURNING id`, crJiraThread)
	s.threadIDs["mail"] = s.insID(t, ctx,
		`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,'unrelated','[]') RETURNING id`, crMailThread)

	// Six inbound Slack notifications about ONE ticket, oldest first — the
	// criterion-8 corpus. Every one of them also matches the workspace catch-all,
	// which is what makes them ambiguous (criteria 3 and 19).
	for i := 1; i <= 6; i++ {
		label := fmt.Sprintf("lhh%d", i)
		s.message(t, ctx, msgSpec{
			label: label, account: slackAcct, threadID: s.threadIDs["slack"], channel: "slack",
			direction: "inbound", sender: "Jira APP",
			subject:  "",
			body:     fmt.Sprintf("Salvador commented on %s: pass %d of the ranking fix", crTicketKey, i),
			minsAgo:  60 - i,
			external: fmt.Sprintf("slack:TCAPRULES:C0CAPRULES:p17800000000%02d", i),
		})
	}
	// Same workspace, no ticket key: the catch-all's own case (criteria 12, 19).
	s.message(t, ctx, msgSpec{
		label: "chatter", account: slackAcct, threadID: s.threadIDs["slack"], channel: "slack",
		direction: "inbound", sender: "Marco", body: "can someone look at the staging deploy?",
		minsAgo: 50, external: "slack:TCAPRULES:C0CAPRULES:p1780000000099",
	})
	// OUTBOUND, and it names the ticket: invariant 5 / criterion 11. Without the
	// direction filter, a comment switchboard itself posted would spawn a task
	// about itself.
	s.message(t, ctx, msgSpec{
		label: "ours", account: slackAcct, threadID: s.threadIDs["slack"], channel: "slack",
		direction: "outbound", sender: "Salvador",
		body:    fmt.Sprintf("shipped the fix for %s", crTicketKey),
		minsAgo: 45, external: "slack:TCAPRULES:C0CAPRULES:p1780000000100",
	})
	// A jira thread matched by prefix, whose key comes from the THREAD KEY.
	s.message(t, ctx, msgSpec{
		label: "web", account: jiraAcct, threadID: s.threadIDs["jira"], channel: "jira",
		direction: "inbound", sender: "Jira <jira@caprules.jira.com>", subject: "WEB-1204",
		body: "rate limiting is live", minsAgo: 40, external: "jira:caprules.jira.com:comment:5501",
	})
	// Covered by nothing: the row triage's inbox keys on (criterion 6, §8b).
	s.message(t, ctx, msgSpec{
		label: "unmatched", account: mailAcct, threadID: s.threadIDs["mail"], channel: "gmail",
		direction: "inbound", sender: "Stranger <stranger@example.test>", subject: "quick question",
		body: "are you available next month?", minsAgo: 35, external: "<itest-caprules-1@mail.example.test>",
	})
}

type msgSpec struct {
	label     string
	account   int64
	threadID  int64
	channel   string
	direction string
	sender    string
	subject   string
	body      string
	minsAgo   int
	external  string
}

func (s *crSuite) message(t *testing.T, ctx context.Context, m msgSpec) {
	t.Helper()
	raw := s.insID(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,'{}',$3, now()) RETURNING id`,
		m.account, "itest-caprules-"+m.label, "itest-caprules-hash-"+m.label)
	id := s.insID(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		    body_text, subject, sender, channel)
		 VALUES ($1,$2,$3,$4, now() - make_interval(mins => $5), $6,$7,$8,$9) RETURNING id`,
		raw, m.threadID, m.direction, m.external, m.minsAgo, m.body, m.subject, m.sender, m.channel)
	s.messages[m.label] = id
	s.rawItems[m.label] = raw
}

// decision is the latest capture_decisions row for a labelled message.
type decision struct {
	mode          string
	action        string
	projectID     *int64
	matchedRuleID *int64
	matchedRules  []int64
	ambiguous     bool
	externalSys   *string
	externalKey   *string
	taskID        *int64
	rawItemID     *int64
	count         int
}

func (s *crSuite) decision(t *testing.T, ctx context.Context, label string) decision {
	t.Helper()
	msgID, ok := s.messages[label]
	if !ok {
		t.Fatalf("no fixture message labelled %q", label)
	}
	var d decision
	d.count = s.scanInt(t, ctx, `SELECT count(*) FROM capture_decisions WHERE message_id=$1`, msgID)
	if d.count == 0 {
		return d
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT mode, action, project_id, matched_rule_id, matched_rule_ids, ambiguous,
		        external_system, external_key, task_id, raw_source_item_id
		   FROM capture_decisions WHERE message_id=$1 ORDER BY id DESC LIMIT 1`, msgID).
		Scan(&d.mode, &d.action, &d.projectID, &d.matchedRuleID, &d.matchedRules, &d.ambiguous,
			&d.externalSys, &d.externalKey, &d.taskID, &d.rawItemID); err != nil {
		t.Fatalf("read decision for %q: %v", label, err)
	}
	return d
}

func containsID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// ---- criteria 6, 7, 3, 19: the shadow pass -------------------------------------

func TestCaptureRules_Integration_ShadowDecidesAndCreatesNothing(t *testing.T) {
	ctx := context.Background()
	s := newCRSuite(t, ctx)

	tasksBefore := s.scanInt(t, ctx, `SELECT count(*) FROM tasks`)
	refsBefore := s.scanInt(t, ctx, `SELECT count(*) FROM external_refs`)
	eventsBefore := s.scanInt(t, ctx, `SELECT count(*) FROM task_events`)
	deliveriesBefore := s.scanInt(t, ctx, `SELECT count(*) FROM deliveries`)

	if _, err := capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{Mode: "shadow"}); err != nil {
		t.Fatalf("EvaluateRules(shadow): %v", err)
	}

	// Criterion 7: shadow writes nothing outside capture_decisions.
	for _, c := range []struct {
		table  string
		before int
	}{
		{"tasks", tasksBefore}, {"external_refs", refsBefore},
		{"task_events", eventsBefore}, {"deliveries", deliveriesBefore},
	} {
		if got := s.scanInt(t, ctx, `SELECT count(*) FROM `+c.table); got != c.before {
			t.Errorf("%s changed across a SHADOW run: before=%d after=%d (criterion 7: shadow creates zero rows "+
				"in tasks, external_refs, task_events and deliveries)", c.table, c.before, got)
		}
	}

	// Criterion 6: exactly one decision row per evaluated message, unmatched
	// included.
	for _, label := range []string{"lhh1", "lhh6", "chatter", "web", "unmatched"} {
		if d := s.decision(t, ctx, label); d.count != 1 {
			t.Errorf("message %q has %d capture_decisions rows after one run, want exactly 1", label, d.count)
		}
	}

	// Criterion 11 / invariant 5: the outbound message was never evaluated.
	if d := s.decision(t, ctx, "ours"); d.count != 0 {
		t.Errorf("the OUTBOUND message got %d decision rows; the pending filter must be direction='inbound' "+
			"(invariant 5 — otherwise a comment switchboard posted spawns a task about itself)", d.count)
	}

	// Criteria 3 + 19: the LHH message inside the catch-all workspace resolves to
	// ReEngine, records BOTH rules, and is flagged ambiguous.
	d := s.decision(t, ctx, "lhh1")
	if d.mode != "shadow" {
		t.Errorf("mode = %q, want shadow (the default)", d.mode)
	}
	if d.projectID == nil || *d.projectID != s.reengine {
		t.Errorf("project_id = %v, want the ReEngine project %d — priority 100 must claim the ticket before "+
			"the priority-1 workspace catch-all (criterion 19)", d.projectID, s.reengine)
	}
	if d.matchedRuleID == nil || *d.matchedRuleID != s.ruleLHH {
		t.Errorf("matched_rule_id = %v, want the LHH rule %d", d.matchedRuleID, s.ruleLHH)
	}
	if !containsID(d.matchedRules, s.ruleLHH) || !containsID(d.matchedRules, s.ruleWS) {
		t.Errorf("matched_rule_ids = %v, want BOTH the LHH rule %d and the workspace rule %d (criterion 3)",
			d.matchedRules, s.ruleLHH, s.ruleWS)
	}
	if !d.ambiguous {
		t.Errorf("ambiguous = false; two matched rules naming DIFFERENT projects (%d reengine, %d collaboratory) "+
			"must set it — that flag is the only report of a routing collision", s.ruleLHH, s.ruleWS)
	}
	if d.externalKey == nil || *d.externalKey != crTicketKey {
		t.Errorf("external_key = %v, want %q (§3: key_regex NULL on a body_regex rule reuses the pattern)", d.externalKey, crTicketKey)
	}
	if d.taskID != nil {
		t.Errorf("task_id = %v on a SHADOW decision; shadow records the DECISION, never the effect", *d.taskID)
	}
	// Invariant 1 traceability: a decision must lead back to the provider JSON.
	if d.rawItemID == nil || *d.rawItemID != s.rawItems["lhh1"] {
		t.Errorf("raw_source_item_id = %v, want %d — every decision must be re-derivable after a "+
			"re-normalization (SPEC invariant 1)", d.rawItemID, s.rawItems["lhh1"])
	}

	// Criterion 19's other half + criterion 12: same workspace, no ticket key →
	// attribution only.
	c := s.decision(t, ctx, "chatter")
	if c.action != "attributed" {
		t.Errorf("workspace catch-all action = %q, want attributed (external_system NULL ⇒ project attribution "+
			"only, no task — with 59%% of the corpus behind that rule this is load-bearing)", c.action)
	}
	if c.projectID == nil || *c.projectID != s.collab {
		t.Errorf("workspace catch-all project_id = %v, want the Collaboratory project %d", c.projectID, s.collab)
	}
	if c.ambiguous {
		t.Errorf("ambiguous = true for a message only ONE rule matched; ambiguity is two rules naming different projects")
	}

	// The unmatched row: the only action with project_id NULL, and §8(b)'s inbox.
	u := s.decision(t, ctx, "unmatched")
	if u.action != "unmatched" || u.projectID != nil {
		t.Errorf("uncovered message: action=%q project_id=%v, want action='unmatched' with a NULL project "+
			"(§8b keys triage's inbox on exactly that)", u.action, u.projectID)
	}

	// Shadow proposes one task per MESSAGE, not per ticket: there are no
	// external_refs rows in shadow, so nothing dedups. Criterion 14 exists because
	// of this — the report DISTINCTs (external_system, external_key) so "15
	// messages over 5 tickets reads as 5 tasks, 15 messages". If you dedup within
	// a shadow run instead, that DISTINCT is redundant and one of the two is wrong.
	proposed := s.scanInt(t, ctx,
		`SELECT count(*) FROM capture_decisions WHERE mode='shadow' AND action='task' AND external_key=$1`, crTicketKey)
	if proposed != 6 {
		t.Errorf("shadow decisions with action='task' for %s = %d, want 6 (one per message; the report is what "+
			"collapses them to one ticket — criterion 14)", crTicketKey, proposed)
	}
}

// ---- criteria 8, 9, 12: the live pass ------------------------------------------

func TestCaptureRules_Integration_LiveCreatesOneTaskPerTicket(t *testing.T) {
	ctx := context.Background()
	s := newCRSuite(t, ctx)

	tasksBefore := s.scanInt(t, ctx, `SELECT count(*) FROM tasks`)
	refsBefore := s.scanInt(t, ctx, `SELECT count(*) FROM external_refs`)

	if _, err := capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{Mode: "live"}); err != nil {
		t.Fatalf("EvaluateRules(live): %v", err)
	}

	// Criterion 8: ONE task on ReEngine for the six notifications.
	taskID := int64(0)
	if err := s.pool.QueryRow(ctx,
		`SELECT t.id FROM tasks t JOIN external_refs er ON er.task_id = t.id
		  WHERE er.system='jira' AND er.external_key=$1`, crTicketKey).Scan(&taskID); err != nil {
		t.Fatalf("no task linked to %s after a live run: %v", crTicketKey, err)
	}
	if got := s.scanInt(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, s.reengine); got != 1 {
		t.Errorf("tasks on the ReEngine project = %d, want exactly 1 for six messages about one ticket "+
			"(criterion 8; the dedup key is the external_refs lookup)", got)
	}
	var project, assignee, status string
	if err := s.pool.QueryRow(ctx,
		`SELECT p.slug, t.assignee_type, t.status FROM tasks t JOIN projects p ON p.id=t.project_id WHERE t.id=$1`,
		taskID).Scan(&project, &assignee, &status); err != nil {
		t.Fatalf("read created task: %v", err)
	}
	if project != crReengineSlug || assignee != "human" || status != "ready" {
		t.Errorf("created task = (project %s, assignee_type %s, status %s), want (%s, human, ready) — ReEngine is "+
			"execution=manual and human-resolved by design (criterion 8)", project, assignee, status, crReengineSlug)
	}

	// Criterion 8: exactly one external_refs row, with the url_template applied.
	var refs int
	var url *string
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*), max(external_url) FROM external_refs WHERE system='jira' AND external_key=$1`,
		crTicketKey).Scan(&refs, &url); err != nil {
		t.Fatalf("read external_refs: %v", err)
	}
	if refs != 1 {
		t.Errorf("external_refs rows for %s = %d, want 1", crTicketKey, refs)
	}
	if url == nil || *url != crTicketURL {
		t.Errorf("external_url = %v, want %q ({key} substituted once from url_template)", url, crTicketURL)
	}

	// Criterion 8: FIVE log events — the five follow-ups, on the same task.
	if got := s.scanInt(t, ctx,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='log'`, taskID); got != 5 {
		t.Errorf("log events on task %d = %d, want 5 (six notifications = one task + five appended logs; that "+
			"IS the 'usable alone' promise)", taskID, got)
	}

	// The decisions record which message did which.
	if got := s.scanInt(t, ctx,
		`SELECT count(*) FROM capture_decisions WHERE mode='live' AND action='task' AND external_key=$1`, crTicketKey); got != 1 {
		t.Errorf("live decisions with action='task' for %s = %d, want 1", crTicketKey, got)
	}
	if got := s.scanInt(t, ctx,
		`SELECT count(*) FROM capture_decisions WHERE mode='live' AND action='task_log' AND external_key=$1`, crTicketKey); got != 5 {
		t.Errorf("live decisions with action='task_log' for %s = %d, want 5", crTicketKey, got)
	}
	first := s.decision(t, ctx, "lhh1")
	if first.action != "task" || first.taskID == nil || *first.taskID != taskID {
		t.Errorf("the OLDEST message's decision = (action %q, task %v), want ('task', %d): processing is "+
			"ORDER BY sent_at, id, so the first notification creates and the rest append",
			first.action, first.taskID, taskID)
	}
	last := s.decision(t, ctx, "lhh6")
	if last.action != "task_log" || last.taskID == nil || *last.taskID != taskID {
		t.Errorf("the NEWEST message's decision = (action %q, task %v), want ('task_log', %d)",
			last.action, last.taskID, taskID)
	}

	// Criterion 12: a matched rule with external_system NULL creates NO task even
	// in live mode.
	c := s.decision(t, ctx, "chatter")
	if c.action != "attributed" || c.taskID != nil {
		t.Errorf("live catch-all decision = (action %q, task %v), want ('attributed', nil): a task per Slack "+
			"thread in a 40k-message workspace manufactures exactly the backlog this ticket exists to avoid",
			c.action, c.taskID)
	}
	if got := s.scanInt(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, s.collab); got != 1 {
		t.Errorf("tasks on the Collaboratory project = %d, want 1 (only the WEB-1204 jira rule creates one; the "+
			"workspace catch-all attributes and stops)", got)
	}

	// The jira-prefix rule keyed its ref from the THREAD KEY, not the body.
	var webKey string
	if err := s.pool.QueryRow(ctx,
		`SELECT er.external_key FROM external_refs er JOIN tasks t ON t.id=er.task_id
		  WHERE t.project_id=$1`, s.collab).Scan(&webKey); err != nil {
		t.Fatalf("read the WEB external ref: %v", err)
	}
	if webKey != "WEB-1204" {
		t.Errorf("external_key = %q, want WEB-1204 (key_regex applied to the thread_key for thread-key criteria)", webKey)
	}

	// Criterion 11 in live mode: the outbound message still produced nothing.
	if d := s.decision(t, ctx, "ours"); d.count != 0 {
		t.Errorf("the outbound message got %d decision rows in live mode", d.count)
	}

	// Nothing beyond the two expected tasks/refs was created anywhere.
	if got, want := s.scanInt(t, ctx, `SELECT count(*) FROM tasks`), tasksBefore+2; got != want {
		t.Errorf("total tasks = %d, want %d (two: one ReEngine ticket, one Collaboratory ticket)", got, want)
	}
	if got, want := s.scanInt(t, ctx, `SELECT count(*) FROM external_refs`), refsBefore+2; got != want {
		t.Errorf("total external_refs = %d, want %d", got, want)
	}

	// ---- criterion 9: the live pass is idempotent -------------------------------
	tasksAfter := s.scanInt(t, ctx, `SELECT count(*) FROM tasks`)
	refsAfter := s.scanInt(t, ctx, `SELECT count(*) FROM external_refs`)
	eventsAfter := s.scanInt(t, ctx, `SELECT count(*) FROM task_events`)
	decisionsAfter := s.scanInt(t, ctx, `SELECT count(*) FROM capture_decisions WHERE mode='live'`)

	if _, err := capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{Mode: "live"}); err != nil {
		t.Fatalf("EvaluateRules(live, rerun): %v", err)
	}
	if got := s.scanInt(t, ctx, `SELECT count(*) FROM tasks`); got != tasksAfter {
		t.Errorf("rerun created tasks: %d -> %d (criterion 9)", tasksAfter, got)
	}
	if got := s.scanInt(t, ctx, `SELECT count(*) FROM external_refs`); got != refsAfter {
		t.Errorf("rerun created external_refs: %d -> %d", refsAfter, got)
	}
	if got := s.scanInt(t, ctx, `SELECT count(*) FROM task_events`); got != eventsAfter {
		t.Errorf("rerun appended task_events: %d -> %d — task_append_log has no dedup of its own, so the live "+
			"pending filter (no mode='live' decision row) is the ONLY thing stopping a double-append",
			eventsAfter, got)
	}
	if got := s.scanInt(t, ctx, `SELECT count(*) FROM capture_decisions WHERE mode='live'`); got != decisionsAfter {
		t.Errorf("rerun wrote live decision rows: %d -> %d (capture_decisions_live_uniq is one live action per "+
			"message, FOREVER)", decisionsAfter, got)
	}
}

// ---- criterion 13: four connectors overlap, the lock serializes them -----------

// "The advisory lock is taken and a pass that cannot acquire it returns (0, nil)
// and prints a one-line skip, never an error exit." A connector run must not FAIL
// because another connector happened to be running — that is the difference from
// triage, which errors.
func TestCaptureRules_Integration_AdvisoryLockSkipsCleanly(t *testing.T) {
	ctx := context.Background()
	s := newCRSuite(t, ctx)

	// Hold the lock on a dedicated connection, exactly as a concurrent pass would.
	holder, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire holder connection: %v", err)
	}
	defer holder.Release()
	var taken bool
	if err := holder.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, crAdvisoryLockKey).Scan(&taken); err != nil {
		t.Fatalf("take the advisory lock: %v", err)
	}
	if !taken {
		t.Fatalf("could not take advisory lock 0x%X in the test itself; something else holds it", crAdvisoryLockKey)
	}
	defer func() {
		var released bool
		_ = holder.QueryRow(context.Background(), `SELECT pg_advisory_unlock($1)`, crAdvisoryLockKey).Scan(&released)
	}()

	before := s.scanInt(t, ctx, `SELECT count(*) FROM capture_decisions`)

	stats, err := capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{Mode: "shadow"})
	if err != nil {
		t.Fatalf("EvaluateRules while the lock is held returned an error (%v); it must be a clean no-op — a "+
			"connector run must not fail because another connector happened to be running (SPEC §5)", err)
	}
	if !reflect.DeepEqual(stats, capture.RulesStats{}) {
		t.Errorf("stats = %+v, want the zero value: the pass did no work", stats)
	}
	if got := s.scanInt(t, ctx, `SELECT count(*) FROM capture_decisions`); got != before {
		t.Errorf("capture_decisions moved from %d to %d while the advisory lock (0x%X) was held by another "+
			"session — the pass ran anyway, so four overlapping CronJobs are not serialized", before, got, crAdvisoryLockKey)
	}
}

// ---- §6: the horizon, and the "720" typo ---------------------------------------

// The horizon bounds sent_at. Its defensive shape is spelled out in the SPEC
// ("unparseable or non-positive falls back to the default; '720' is not a Go
// duration and must not become 720ns") and lives in the env accessor; what this
// asserts is the part the driver owns — that a horizon actually EXCLUDES older
// messages, so a live go-live cannot sweep three years of history into tasks.
func TestCaptureRules_Integration_HorizonBoundsTheCorpus(t *testing.T) {
	ctx := context.Background()
	s := newCRSuite(t, ctx)

	// Every fixture message is under an hour old; a 10-minute horizon must
	// exclude all of them.
	if _, err := capture.EvaluateRules(ctx, s.pool, s.ex,
		capture.RulesConfig{Mode: "shadow", Horizon: 10 * time.Minute}); err != nil {
		t.Fatalf("EvaluateRules(shadow, 10m): %v", err)
	}
	for _, label := range []string{"lhh1", "chatter", "web", "unmatched"} {
		if d := s.decision(t, ctx, label); d.count != 0 {
			t.Errorf("message %q (older than the 10m horizon) got %d decision rows; the horizon is what keeps "+
				"a live flip from acting on three years of history", label, d.count)
		}
	}

	// Unbounded (the shadow default) sees them.
	if _, err := capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{Mode: "shadow"}); err != nil {
		t.Fatalf("EvaluateRules(shadow, unbounded): %v", err)
	}
	if d := s.decision(t, ctx, "lhh1"); d.count != 1 {
		t.Errorf("with no horizon the message got %d decision rows, want 1 (shadow's default is 0 = unbounded, "+
			"because the whole corpus is what you want to diff)", d.count)
	}
}

// ---- criterion 14: the shadow report -------------------------------------------

// "opsctl capture-rules report [--since] prints, from capture_decisions only:
// per-project matched counts; DISTINCT (external_system, external_key)
// proposed-task counts (so 15 messages over 5 tickets reads as '5 tasks, 15
// messages'); the ambiguous list; and the top unmatched senders and thread_key
// prefixes by volume."
//
// Assertions are containment-scoped to this suite's own fixtures: the report is
// global by design (it is the diff surface for the whole corpus), so other suites'
// leftovers legitimately appear in the leaderboards and an exact total would be a
// flake, not a check.
func TestCaptureRules_Integration_ReportSections(t *testing.T) {
	ctx := context.Background()
	s := newCRSuite(t, ctx)

	if _, err := capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{Mode: "shadow"}); err != nil {
		t.Fatalf("EvaluateRules(shadow): %v", err)
	}
	report, err := capture.Report(ctx, s.pool, time.Time{})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	t.Logf("report:\n%s", report)

	for _, section := range []string{
		"DECISIONS", "BY PROJECT", "PROPOSED TASKS", "AMBIGUOUS",
		"TOP UNMATCHED SENDERS", "TOP UNMATCHED THREAD-KEY PREFIXES",
	} {
		if !strings.Contains(report, section) {
			t.Errorf("report is missing the %q section (criterion 14 names four; the totals and the "+
				"per-project split are what make them readable)", section)
		}
	}

	// Per-project counts: six LHH notifications on ReEngine, two messages on
	// Collaboratory (one attributed catch-all, one WEB ticket).
	if got := reportFields(t, report, crReengineSlug); len(got) >= 2 && got[1] != "6" {
		t.Errorf("BY PROJECT line for %s = %v, want 6 messages", crReengineSlug, got)
	}
	if got := reportFields(t, report, crCollabSlug); len(got) >= 2 && got[1] != "2" {
		t.Errorf("BY PROJECT line for %s = %v, want 2 messages", crCollabSlug, got)
	}

	// The headline of criterion 14: DISTINCT keys vs messages. Six messages over
	// ONE ticket must read as 1 and 6 — and the two numbers must DIFFER, or the
	// line proves nothing about distinctness.
	proposed := reportSectionLine(t, report, "PROPOSED TASKS", crReengineSlug)
	fields := strings.Fields(proposed)
	if len(fields) < 4 {
		t.Fatalf("PROPOSED TASKS line for %s is %q; want slug, system, keys, messages", crReengineSlug, proposed)
	}
	keys, messages := fields[len(fields)-2], fields[len(fields)-1]
	if keys != "1" || messages != "6" {
		t.Errorf("PROPOSED TASKS for %s = %s keys over %s messages, want 1 over 6 — '15 messages over 5 "+
			"tickets reads as 5 tasks, 15 messages' is the whole point of the DISTINCT", crReengineSlug, keys, messages)
	}
	if keys == messages {
		t.Errorf("PROPOSED TASKS printed the same number twice (%s); a report that cannot tell tickets from "+
			"messages cannot answer the question the go-live decision is made on", keys)
	}

	// The ambiguous list names the collision, not just its count.
	if !strings.Contains(report, fmt.Sprintf("message %d", s.messages["lhh1"])) {
		t.Errorf("AMBIGUOUS section does not name message %d, which two rules claimed for different projects",
			s.messages["lhh1"])
	}

	// Unmatched leaderboards: the sender and the thread-key prefix of the one
	// message no rule covered. The prefix is the granularity a NEW rule is
	// written at, which is what makes the section actionable.
	if !strings.Contains(report, "stranger@example.test") {
		t.Errorf("TOP UNMATCHED SENDERS does not list the uncovered message's sender; that leaderboard is how a " +
			"missing rule is discovered")
	}
	// The prefix is the first two colon-separated segments of the thread_key —
	// provider and account — which is the granularity a new rule is written at.
	wantPrefix := strings.Join(strings.SplitN(crMailThread, ":", 3)[:2], ":")
	if !strings.Contains(report, wantPrefix) {
		t.Errorf("TOP UNMATCHED THREAD-KEY PREFIXES does not list %q (from thread_key %q)", wantPrefix, crMailThread)
	}

	// Re-running shadow must not inflate the numbers. The pending filter is "no
	// LIVE decision row", so a second shadow pass writes a second row per message;
	// counting rows instead of messages would make every number grow with the
	// number of times someone ran the report's input.
	if _, err := capture.EvaluateRules(ctx, s.pool, s.ex,
		capture.RulesConfig{Mode: "shadow", All: true}); err != nil {
		t.Fatalf("EvaluateRules(shadow, --all): %v", err)
	}
	if got := s.scanInt(t, ctx,
		`SELECT count(*) FROM capture_decisions WHERE message_id=$1`, s.messages["lhh1"]); got < 2 {
		t.Fatalf("--all in shadow did not re-evaluate (message has %d decision rows); the de-duplication "+
			"assertion below would then prove nothing", got)
	}
	again, err := capture.Report(ctx, s.pool, time.Time{})
	if err != nil {
		t.Fatalf("Report (after re-run): %v", err)
	}
	if got := reportFields(t, again, crReengineSlug); len(got) >= 2 && got[1] != "6" {
		t.Errorf("BY PROJECT line for %s after a second shadow pass = %v, want 6 messages still — the report is "+
			"ONE ROW PER MESSAGE (latest decision), not one per decision row", crReengineSlug, got)
	}

	// --since bounds the window: a report from the future contains none of it.
	future, err := capture.Report(ctx, s.pool, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Report (future window): %v", err)
	}
	if strings.Contains(future, crReengineSlug) || strings.Contains(future, "stranger@example.test") {
		t.Errorf("a report windowed to the future still shows this suite's decisions; --since is what makes " +
			"'what changed after I added that rule' answerable")
	}
}

// reportFields returns the whitespace-separated fields of the FIRST line
// mentioning key, or nil (with a failure) if there is none.
func reportFields(t *testing.T, report, key string) []string {
	t.Helper()
	for _, line := range strings.Split(report, "\n") {
		if strings.Contains(line, key) {
			return strings.Fields(line)
		}
	}
	t.Errorf("report has no line mentioning %q", key)
	return nil
}

// reportSectionLine returns the first line mentioning key AFTER the named section
// header — the same string can appear in several sections.
func reportSectionLine(t *testing.T, report, section, key string) string {
	t.Helper()
	inSection := false
	for _, line := range strings.Split(report, "\n") {
		switch {
		case strings.HasPrefix(line, section):
			inSection = true
		case line != "" && !strings.HasPrefix(line, " "):
			inSection = false
		}
		if inSection && strings.Contains(line, key) {
			return line
		}
	}
	t.Fatalf("section %q has no line mentioning %q; report:\n%s", section, key, report)
	return ""
}

// ---- criterion 10: --all is shadow-only ----------------------------------------

// "--all in live mode exits with an error and performs no writes."
//
// The refusal lives in EvaluateRules, not in opsctl: `cmd/opsctl` passes the flag
// straight through (main.go's capture-rules run), so a test at the CLI would only
// pin the wiring while the guarantee sits one layer down — and the CronJobs reach
// the same function without opsctl at all.
//
// Why it matters: task_append_log has no dedup of its own. The live dedup IS
// capture_decisions_live_uniq, and re-evaluating messages that already have live
// decisions would append every log a second time — silently, on real client tasks.
func TestCaptureRules_Integration_AllIsRefusedInLiveMode(t *testing.T) {
	ctx := context.Background()
	s := newCRSuite(t, ctx)

	// A live pass first, so there is something a replay could double-append to.
	if _, err := capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{Mode: "live"}); err != nil {
		t.Fatalf("EvaluateRules(live): %v", err)
	}
	events := s.scanInt(t, ctx, `SELECT count(*) FROM task_events`)
	tasks := s.scanInt(t, ctx, `SELECT count(*) FROM tasks`)
	decisions := s.scanInt(t, ctx, `SELECT count(*) FROM capture_decisions`)

	stats, err := capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{Mode: "live", All: true})
	if err == nil {
		t.Fatalf("EvaluateRules(live, --all) succeeded; criterion 10 requires an error — a live replay would " +
			"double-append every task log, because task_append_log has no dedup and --all is precisely the flag " +
			"that bypasses the one that does")
	}
	if !reflect.DeepEqual(stats, capture.RulesStats{}) {
		t.Errorf("stats = %+v on a refused run, want the zero value", stats)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "all") {
		t.Errorf("error %q does not name --all; the operator typed one flag and needs to know which one was "+
			"refused", err)
	}

	// "performs no writes" — asserted, not inferred from the error.
	if got := s.scanInt(t, ctx, `SELECT count(*) FROM task_events`); got != events {
		t.Errorf("task_events moved %d -> %d on a refused run", events, got)
	}
	if got := s.scanInt(t, ctx, `SELECT count(*) FROM tasks`); got != tasks {
		t.Errorf("tasks moved %d -> %d on a refused run", tasks, got)
	}
	if got := s.scanInt(t, ctx, `SELECT count(*) FROM capture_decisions`); got != decisions {
		t.Errorf("capture_decisions moved %d -> %d on a refused run; the refusal must come BEFORE any "+
			"evaluation, not after the first write", decisions, got)
	}

	// The mirror: --all in SHADOW is the supported way to re-evaluate a corpus
	// after editing a rule. If it did nothing, the live refusal would be guarding
	// a flag that has no effect anywhere.
	before := s.scanInt(t, ctx,
		`SELECT count(*) FROM capture_decisions WHERE message_id=$1`, s.messages["lhh1"])
	if _, err := capture.EvaluateRules(ctx, s.pool, s.ex,
		capture.RulesConfig{Mode: "shadow", All: true}); err != nil {
		t.Fatalf("EvaluateRules(shadow, --all): %v", err)
	}
	if got := s.scanInt(t, ctx,
		`SELECT count(*) FROM capture_decisions WHERE message_id=$1`, s.messages["lhh1"]); got <= before {
		t.Errorf("--all in shadow re-evaluated nothing (%d -> %d decision rows for the same message); it is the "+
			"documented way to diff a rule change over the whole corpus", before, got)
	}
}
