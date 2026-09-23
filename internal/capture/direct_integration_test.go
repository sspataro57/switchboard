//go:build integration

package capture_test

// TestRegression_SWT78_* — bug slack-messages-not-becoming-tasks (Jira SWT-78,
// swb #521). docs/bugs/slack-messages-not-becoming-tasks_DIAGNOSIS.md, items
// C, D, E and G.2-G.7, plus the "Decisions on the open questions" section,
// which OVERRIDES the fix scope where they differ:
//
//	1. a dismissed conversation task + a new DM → a NEW task;
//	2. the conversation's task = a HUMAN task with NO external_refs row whose
//	   source thread is in that DM conversation;
//	3. group DMs are included, from raw conversation.type='group_dm';
//	4. rooted threads inside a DM fold into the conversation's task;
//	5. item E is IN: a person's DM that would resurface onto a CLOSED ticket
//	   task takes the DM conversation-task path instead (no resurface, no qwen).
//
// What went wrong on 2026-09-22: 17 human DMs were decided `attributed` by the
// workspace catch-all rules (8/9), went to qwen3:8b's inquiry lane, and 16 were
// judged needs_reply=false (read by nothing) and 1 was gated `answered`. None
// became a task. After the fix a person's DM is decided in CAPTURE: `task`
// (first message of the conversation, or no open conversation task) or
// `task_log` onto the conversation's open task, with external_system NULL.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_dmtasks?sslmode=disable \
//	  TZ=UTC go test -tags integration -p 1 -count=1 -run SWT78 ./internal/capture/
//
// Build-tagged `integration`, env-gated on DATABASE_URL, FATAL on
// 192.168.50.49. NO LLM, NO network. USE AN ISOLATED DATABASE (IK "Test
// infrastructure", 2026-09-12): this suite deletes capture_decisions
// WHOLESALE (EvaluateRules' pending set is global; the rules suites' precedent).
//
// "TEST THE COLUMN, NOT THE FIXTURE": the group-DM fact is seeded ONLY in
// raw_source_items.raw_json->'conversation'->>'type' (the C… id cannot tell),
// the notifier list only in projects.notifier_senders, the ticket task is
// created by a live pass (so its external_refs row is link_external_ref's),
// and every closed/dismissed state is reached through task_close /
// task_dismiss. MUTATIONS (each turns a named test red):
//   - drop the raw conversation type from pendingMessageCols (select a literal
//     '') → GroupDMFromTheRawColumn;
//   - drop the notifier clause → JiraAppDMUnchanged;
//   - drop `status NOT IN ('closed','delivered')` from the conversation lookup
//     → ClosedOrDismissedConversationTaskGetsANewTask;
//   - drop the assignee_type / external_refs filters → ClaudeOrTicketTaskIsNotTheConversationTask;
//   - look up by thread id instead of by conversation → RootedThreadFoldsIntoTheConversationTask;
//   - leave resurfaces() without a DM disqualifier → DMOnAClosedTicketTaskTakesTheDMPath.
//
// SIGNATURES: every entry point here exists today (capture.EvaluateRules,
// capture.RulesConfig, capture.RulesStats, classify.Store.PendingMessages), so
// this file compiles and fails on BEHAVIOUR. (The package's unit file
// direct_test.go names the new pure predicate and does not compile until
// direct.go exists; that is the only compile failure.)

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
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
)

const (
	dmtSlug     = "itest-dmtasks"
	dmtProvider = "itest-dmtasks-src"
	dmtWS       = "TITESTDMT"
	dmtAccount  = "titestdmt@slack-web.local" // source_slack_workspace matches pattern+"@slack-web.local"
	dmtSubject  = "itest-dmtasks"
	dmtActor    = "capture:itest-dmtasks"
	dmtCloser   = "opsctl:itest-dmtasks"
	dmtHuman    = "dashboard:itest-dmtasks"
	// rule 75's shape with this suite's own letters: a body_regex jira rule.
	dmtTicketPattern = `DMT-[0-9]+`
)

type dmtSuite struct {
	pool      *pgxpool.Pool
	ex        *executor.Executor
	project   int64
	account   int64
	wsRule    int64
	jiraRule  int64
	threads   map[string]int64
	seq       int
	sentClock time.Time
}

func newDMTSuite(t *testing.T, ctx context.Context) *dmtSuite {
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
	dmtCleanup(t, ctx, pool)
	t.Cleanup(func() { dmtCleanup(t, context.Background(), pool) })

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	s := &dmtSuite{pool: pool, ex: executor.New(reg, checker, audit.NewPGStore(pool)), threads: map[string]int64{}}
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp() - interval '2 hours'`).Scan(&s.sentClock); err != nil {
		t.Fatalf("db clock: %v", err)
	}

	// collaboratory's real shape: inquiry-armed (so "skips qwen" is a real
	// claim), gate off, the Jira app on the notifier list (0034's column).
	s.project = s.id(t, ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality, ai_classify,
		                       ai_inquiry, inquiry_promote_after, ticket_assignee_gate, notifier_senders)
		 VALUES ($1,$1,'itest-dmtasks-client','manual','dashboard','/tmp/itest-dmtasks','any',false,
		         true, now() - interval '1 day', false, '{Jira}') RETURNING id`, dmtSlug)
	// Rules 8/9's shape: the workspace catch-all, NO external_system.
	s.wsRule = s.id(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, priority, enabled, note)
		 VALUES ($1,'source_slack_workspace',$2,NULL,1,true,'itest-dmtasks ws catch-all') RETURNING id`, s.project, dmtWS)
	// Rule 75's shape: a jira body_regex rule that outranks the catch-all.
	s.jiraRule = s.id(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, priority, enabled, note)
		 VALUES ($1,'body_regex',$2,'jira',NULL,90,true,'itest-dmtasks rule75') RETURNING id`, s.project, dmtTicketPattern)
	s.account = s.id(t, ctx,
		`INSERT INTO source_accounts (provider, account_email, scopes, send_enabled, calendar_in_availability)
		 VALUES ($1,$2,'{}',false,false) RETURNING id`, dmtProvider, dmtAccount)
	return s
}

func dmtCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE provider='` + dmtProvider + `')`
	const projs = `(SELECT id FROM projects WHERE slug='` + dmtSlug + `')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	const actors = `('` + dmtActor + `','` + dmtCloser + `','` + dmtHuman + `')`
	for _, q := range []string{
		`DELETE FROM capture_decisions`, // wholesale: see the header
		`DELETE FROM ticket_status_syncs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM external_refs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM deliveries WHERE task_id IN ` + tasksOf,
		`DELETE FROM classify_promotions WHERE task_id IN ` + tasksOf + ` OR project_id IN ` + projs,
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor IN ` + actors + `)`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor IN ` + actors,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM capture_rules WHERE project_id IN ` + projs,
		`DELETE FROM projects WHERE slug='` + dmtSlug + `'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN
		   (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`,
		`DELETE FROM normalized_threads WHERE subject = '` + dmtSubject + `'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider='` + dmtProvider + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

// ---- fixtures -----------------------------------------------------------------

func (s *dmtSuite) id(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var v int64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return v
}

func (s *dmtSuite) count(t *testing.T, ctx context.Context, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

func (s *dmtSuite) execute(t *testing.T, ctx context.Context, tool, actor string, taskID int64, args string) {
	t.Helper()
	if _, err := s.ex.Execute(ctx, executor.Call{Tool: tool, Actor: actor, Args: []byte(args), TaskID: &taskID}); err != nil {
		t.Fatalf("%s(%s) as %s: %v", tool, args, actor, err)
	}
}

// Conversation keys in the normalizer's shape: slack:{ws}:{conv}[:{root}].
func dmtKey(conv string) string          { return "slack:" + dmtWS + ":" + conv }
func dmtRooted(conv, root string) string { return "slack:" + dmtWS + ":" + conv + ":" + root }

func (s *dmtSuite) thread(t *testing.T, ctx context.Context, key string) int64 {
	t.Helper()
	if id, ok := s.threads[key]; ok {
		return id
	}
	id := s.id(t, ctx, `INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,$2,'[]') RETURNING id`,
		key, dmtSubject)
	s.threads[key] = id
	return id
}

// dmtMsg is one Slack message fixture. convType is what the LEAF wrote into
// the raw observation's conversation.type ('dm', 'group_dm', 'public_channel').
type dmtMsg struct {
	key       string // thread key
	conv      string // conversation id (the raw observation's)
	convType  string
	sender    string
	body      string
	direction string // default inbound
}

// message seeds one Slack message the capture pass has not decided yet. The
// raw_json is the slackweb leaf's shape; sent_at advances one minute per call
// so "oldest first" is deterministic.
func (s *dmtSuite) message(t *testing.T, ctx context.Context, m dmtMsg) int64 {
	t.Helper()
	if m.direction == "" {
		m.direction = "inbound"
	}
	s.seq++
	label := fmt.Sprintf("%s-%d", dmtSubject, s.seq)
	raw, err := json.Marshal(map[string]any{
		"kind":         "message",
		"workspace":    map[string]any{"id": dmtWS, "own_user_id": "U0DMTSELF"},
		"conversation": map[string]any{"id": m.conv, "name": m.conv, "type": m.convType},
		"message": map[string]any{"id": label, "author": m.sender, "author_id": "U0DMT" + fmt.Sprint(s.seq),
			"text": m.body},
	})
	if err != nil {
		t.Fatalf("marshal raw: %v", err)
	}
	rawID := s.id(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,$3::jsonb,$4, now()) RETURNING id`, s.account, label, string(raw), "h-"+label)
	sent := s.sentClock.Add(time.Duration(s.seq) * time.Minute)
	return s.id(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1,$2,$3,$4,$5,$6,'',$7,'slack') RETURNING id`,
		rawID, s.thread(t, ctx, m.key), m.direction, label, sent, m.body, m.sender)
}

// dm is a person's top-level 1:1 DM in conversation conv.
func (s *dmtSuite) dm(t *testing.T, ctx context.Context, conv, sender, body string) int64 {
	t.Helper()
	return s.message(t, ctx, dmtMsg{key: dmtKey(conv), conv: conv, convType: "dm", sender: sender, body: body})
}

func (s *dmtSuite) pass(t *testing.T, ctx context.Context, mode string) capture.RulesStats {
	t.Helper()
	st, err := capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{Mode: mode, Actor: dmtActor})
	if err != nil {
		t.Fatalf("EvaluateRules(%s): %v", mode, err)
	}
	return st
}

type dmtDecision struct {
	action    string
	taskID    *int64
	extSystem *string
	extKey    *string
	commTask  *int64
	reason    string
	resurface bool
}

func (s *dmtSuite) decision(t *testing.T, ctx context.Context, msg int64, mode string) (dmtDecision, bool) {
	t.Helper()
	var d dmtDecision
	err := s.pool.QueryRow(ctx,
		`SELECT action, task_id, external_system, external_key, comm_task_id, COALESCE(reason,''), resurface
		   FROM capture_decisions WHERE message_id=$1 AND mode=$2 ORDER BY id DESC LIMIT 1`, msg, mode).
		Scan(&d.action, &d.taskID, &d.extSystem, &d.extKey, &d.commTask, &d.reason, &d.resurface)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return d, false
		}
		t.Fatalf("read %s decision for message %d: %v", mode, msg, err)
	}
	return d, true
}

func (d dmtDecision) String() string {
	task := "nil"
	if d.taskID != nil {
		task = fmt.Sprint(*d.taskID)
	}
	return fmt.Sprintf("{action=%s task=%s resurface=%v reason=%q}", d.action, task, d.resurface, d.reason)
}

// created asserts msg's live decision is `task` on the DM path and returns the
// task: external_system/key NULL, resurface false, a task id recorded.
func (s *dmtSuite) created(t *testing.T, ctx context.Context, msg int64, why string) int64 {
	t.Helper()
	d, ok := s.decision(t, ctx, msg, capture.RulesModeLive)
	if !ok || d.action != "task" || d.taskID == nil {
		t.Fatalf("message %d: live decision = %v (found=%v), want action=task with a task_id — %s. Today a person's "+
			"DM ends at the attribution-only exit (`attributed`, no task) and waits for qwen", msg, d, ok, why)
	}
	if d.extSystem != nil || d.extKey != nil {
		t.Errorf("message %d: DM task decision carries external_system=%v external_key=%v; the DM path writes NULL "+
			"(no external_refs row, item C)", msg, d.extSystem, d.extKey)
	}
	if d.resurface {
		t.Errorf("message %d: resurface=true on a `task` decision", msg)
	}
	return *d.taskID
}

// attached asserts msg's live decision is `task_log` onto task.
func (s *dmtSuite) attached(t *testing.T, ctx context.Context, msg, task int64, why string) {
	t.Helper()
	d, ok := s.decision(t, ctx, msg, capture.RulesModeLive)
	if !ok || d.action != "task_log" || d.taskID == nil || *d.taskID != task {
		t.Fatalf("message %d: live decision = %v (found=%v), want task_log onto task %d — %s", msg, d, ok, task, why)
	}
	if d.resurface {
		t.Errorf("message %d: resurface=true; an attach onto an OPEN conversation task never resurfaces", msg)
	}
}

type dmtTask struct {
	status, assignee string
	sourceThread     *int64
	activityBy       *int64
	refs             int
}

func (s *dmtSuite) task(t *testing.T, ctx context.Context, id int64) dmtTask {
	t.Helper()
	var r dmtTask
	if err := s.pool.QueryRow(ctx,
		`SELECT status, assignee_type, source_thread_id, activity_by_message_id,
		        (SELECT count(*) FROM external_refs WHERE task_id = t.id)
		   FROM tasks t WHERE id=$1 AND project_id=$2`, id, s.project).
		Scan(&r.status, &r.assignee, &r.sourceThread, &r.activityBy, &r.refs); err != nil {
		t.Fatalf("read task %d: %v", id, err)
	}
	return r
}

func (s *dmtSuite) projectTasks(t *testing.T, ctx context.Context) int {
	t.Helper()
	return s.count(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, s.project)
}

// conversationTasks counts tasks whose source thread is ANY thread of conv.
func (s *dmtSuite) conversationTasks(t *testing.T, ctx context.Context, conv string) int {
	t.Helper()
	return s.count(t, ctx,
		`SELECT count(*) FROM tasks t JOIN normalized_threads nt ON nt.id = t.source_thread_id
		  WHERE t.project_id=$1 AND (nt.thread_key = $2 OR nt.thread_key LIKE $2 || ':%')`, s.project, dmtKey(conv))
}

// loggedMessage reports whether task has a `log` event naming message msg.
func (s *dmtSuite) loggedMessage(t *testing.T, ctx context.Context, task, msg int64) bool {
	t.Helper()
	rows, err := s.pool.Query(ctx, `SELECT payload::text FROM task_events WHERE task_id=$1 AND event_type='log'`, task)
	if err != nil {
		t.Fatalf("read log events of task %d: %v", task, err)
	}
	defer rows.Close()
	re := regexp.MustCompile(`\b` + fmt.Sprint(msg) + `\b`)
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if re.MatchString(p) {
			return true
		}
	}
	return false
}

// audited asserts invariant 3: the executor wrote an ok audit row for tool on task.
func (s *dmtSuite) audited(t *testing.T, ctx context.Context, tool string, task int64) {
	t.Helper()
	if n := s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE actor=$1 AND tool=$2 AND task_id=$3 AND status='ok'`,
		dmtActor, tool, task); n == 0 {
		t.Errorf("no ok audit_events row for %s on task %d as %s (invariant 3: everything through the executor)",
			tool, task, dmtActor)
	}
}

// inquiryInbox is classify's REAL inquiry inbox (inboxWhereInquiry), the query
// the qwen lane runs.
func (s *dmtSuite) inquiryInbox(t *testing.T, ctx context.Context) map[int64]bool {
	t.Helper()
	rows, err := classify.NewStore(s.pool).PendingMessages(ctx,
		classify.Config{Lane: classify.LaneInquiry, Since: 24 * time.Hour})
	if err != nil {
		t.Fatalf("classify PendingMessages(inquiry): %v", err)
	}
	out := map[int64]bool{}
	for _, r := range rows {
		out[r.MessageID] = true
	}
	return out
}

// ticketTask creates a ticket task the way production does (rule 75 from a
// person's message naming key, one live pass) and returns it; the message's
// conversation becomes its source thread (the #452 shape when conv is a DM).
func (s *dmtSuite) ticketTask(t *testing.T, ctx context.Context, conv, convType, key string) int64 {
	t.Helper()
	m := s.message(t, ctx, dmtMsg{key: dmtKey(conv), conv: conv, convType: convType, sender: "Setup Person",
		body: "please look at " + key})
	s.pass(t, ctx, capture.RulesModeLive)
	var task int64
	if err := s.pool.QueryRow(ctx,
		`SELECT r.task_id FROM external_refs r JOIN tasks t ON t.id = r.task_id
		  WHERE r.system='jira' AND r.external_key=$1 AND t.project_id=$2`, key, s.project).Scan(&task); err != nil {
		d, _ := s.decision(t, ctx, m, capture.RulesModeLive)
		t.Fatalf("setup: the rule-75 live pass created no ticket task for %s (decision %v): %v", key, d, err)
	}
	return task
}

func (s *dmtSuite) closeTask(t *testing.T, ctx context.Context, task int64) {
	t.Helper()
	s.execute(t, ctx, "task_close", dmtCloser, task, fmt.Sprintf(`{"task_id":%d,"reason":"itest-dmtasks done"}`, task))
	if st := s.task(t, ctx, task).status; st != "closed" {
		t.Fatalf("setup: task %d is %q after task_close", task, st)
	}
}

// ---- G.2: first DM creates, second attaches -------------------------------------

func TestRegression_SWT78_FirstDMCreatesTheConversationTask_SecondAttaches(t *testing.T) {
	ctx := context.Background()
	s := newDMTSuite(t, ctx)

	m1 := s.dm(t, ctx, "D0DMTJOSE", "Jose Garcia", "Jajajaja")
	st := s.pass(t, ctx, capture.RulesModeLive)
	task := s.created(t, ctx, m1, "a person's first DM in a conversation with no open task creates ONE task")

	got := s.task(t, ctx, task)
	if got.assignee != "human" || got.status != "ready" {
		t.Errorf("task %d is assignee=%s status=%s, want a human task in ready", task, got.assignee, got.status)
	}
	if got.sourceThread == nil || *got.sourceThread != s.threads[dmtKey("D0DMTJOSE")] {
		t.Errorf("task %d source_thread_id = %v, want the DM's thread %d (task_set_source_thread, setRuleProvenance)",
			task, got.sourceThread, s.threads[dmtKey("D0DMTJOSE")])
	}
	if got.activityBy == nil || *got.activityBy != m1 {
		t.Errorf("task %d activity_by_message_id = %v, want %d: the created task lands in INCOMING (task_mark_activity, swb #491)",
			task, got.activityBy, m1)
	}
	if got.refs != 0 {
		t.Errorf("task %d has %d external_refs rows; a DM conversation task has NONE (a ticket task belongs to its ticket)",
			task, got.refs)
	}
	s.audited(t, ctx, "task_set_source_thread", task)
	s.audited(t, ctx, "task_mark_activity", task)
	if n := s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE actor=$1 AND tool='create_task' AND status='ok'`,
		dmtActor); n != 1 {
		t.Errorf("%d ok create_task audit rows as %s, want 1", n, dmtActor)
	}
	if st.TasksCreated != 1 {
		t.Errorf("RulesStats.TasksCreated = %d, want 1 (%+v)", st.TasksCreated, st)
	}

	// The second DM in the SAME conversation attaches: still one task.
	m2 := s.dm(t, ctx, "D0DMTJOSE", "Jose Garcia", "Si")
	st = s.pass(t, ctx, capture.RulesModeLive)
	s.attached(t, ctx, m2, task, "one open task per conversation: later DMs attach to it")
	if n := s.projectTasks(t, ctx); n != 1 {
		t.Errorf("%d tasks in the project after two DMs in one conversation, want 1", n)
	}
	if !s.loggedMessage(t, ctx, task, m2) {
		t.Errorf("task %d has no log event naming message %d (task_append_log, the attach's trace)", task, m2)
	}
	if a := s.task(t, ctx, task).activityBy; a == nil || *a != m2 {
		t.Errorf("task %d activity_by_message_id = %v after the attach, want %d (SWT-72 D3: an attach is activity)",
			task, a, m2)
	}
	s.audited(t, ctx, "task_append_log", task)
	if st.Appended != 1 || st.TasksCreated != 0 {
		t.Errorf("RulesStats = %+v, want Appended 1, TasksCreated 0", st)
	}
}

// ---- G.2: closed (plain close AND dismissed) → a new task (decision 1) --------

func TestRegression_SWT78_ClosedOrDismissedConversationTaskGetsANewTask(t *testing.T) {
	ctx := context.Background()
	s := newDMTSuite(t, ctx)

	first := s.created(t, ctx, func() int64 {
		m := s.dm(t, ctx, "D0DMTKATIE", "Katie", "can we move the call?")
		s.pass(t, ctx, capture.RulesModeLive)
		return m
	}(), "setup")
	s.closeTask(t, ctx, first)

	m := s.dm(t, ctx, "D0DMTKATIE", "Katie", "and one more thing")
	s.pass(t, ctx, capture.RulesModeLive)
	second := s.created(t, ctx, m, "the conversation's only task is CLOSED, so a new DM creates a new task (open = "+
		"status NOT IN ('closed','delivered'))")
	if second == first {
		t.Fatalf("the new DM landed on closed task %d", first)
	}
	if st := s.task(t, ctx, first).status; st != "closed" {
		t.Errorf("the closed conversation task %d is %q; a DM never reopens it", first, st)
	}

	// Dismissed = closed (decision 1: a NEW task, not SWT-36's reopen).
	s.execute(t, ctx, "task_dismiss", dmtHuman, second,
		fmt.Sprintf(`{"task_id":%d,"reason_code":"handled_elsewhere"}`, second))
	m3 := s.dm(t, ctx, "D0DMTKATIE", "Katie", "ping")
	s.pass(t, ctx, capture.RulesModeLive)
	third := s.created(t, ctx, m3, "the conversation's task was DISMISSED: decision 1 says a new task")
	if third == second || third == first {
		t.Fatalf("the DM after a dismissal landed on task %d (first %d, dismissed %d)", third, first, second)
	}
	if st := s.task(t, ctx, second).status; st != "closed" {
		t.Errorf("the dismissed task %d is %q; decision 1: a new task, the dismissal stands", second, st)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM task_dismissals WHERE task_id=$1 AND reopened_by_message_id IS NOT NULL`,
		second); n != 0 {
		t.Errorf("the dismissal of task %d records a reopen by a DM; decision 1 overrides SWT-36 for DMs", second)
	}
	if n := s.conversationTasks(t, ctx, "D0DMTKATIE"); n != 3 {
		t.Errorf("%d tasks on the conversation, want 3 (created, recreated after close, recreated after dismissal)", n)
	}
}

// ---- G.2: a claude task or a ticket task is not the conversation's task --------

func TestRegression_SWT78_ClaudeOrTicketTaskIsNotTheConversationTask(t *testing.T) {
	ctx := context.Background()
	s := newDMTSuite(t, ctx)

	// A claude task whose source thread is the DM (C-D13: never log untrusted
	// text onto a claude task; its log feeds a worker prompt).
	claude := s.id(t, ctx,
		`INSERT INTO tasks (project_id, title, body, status, assignee_type, source_thread_id)
		 VALUES ($1,'itest-dmtasks claude task','', 'ready','claude',$2) RETURNING id`,
		s.project, s.thread(t, ctx, dmtKey("D0DMTCLAUDE")))
	m := s.dm(t, ctx, "D0DMTCLAUDE", "Dana Ruiz", "any news?")
	s.pass(t, ctx, capture.RulesModeLive)
	got := s.created(t, ctx, m, "the only open task on the conversation is a CLAUDE task: decision 2 counts human tasks only")
	if got == claude {
		t.Fatalf("the DM was logged onto claude task %d", claude)
	}
	if s.loggedMessage(t, ctx, claude, m) {
		t.Errorf("claude task %d carries a log line naming DM message %d", claude, m)
	}

	// A ticket task created by rule 75 from a DM (#452 WEB-10469 on José's DM):
	// open, human, source thread = the DM, but it has an external_refs row.
	ticket := s.ticketTask(t, ctx, "D0DMTTICKET", "dm", "DMT-452")
	if tk := s.task(t, ctx, ticket); tk.refs != 1 || tk.sourceThread == nil {
		t.Fatalf("setup: ticket task %d = %+v, want one external ref and a source thread", ticket, tk)
	}
	before := s.count(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1`, ticket)
	m2 := s.dm(t, ctx, "D0DMTTICKET", "Jose Garcia", "Jajajaja")
	s.pass(t, ctx, capture.RulesModeLive)
	conv := s.created(t, ctx, m2, "the only open task on the DM is a TICKET task (external_refs): decision 2 excludes it")
	if conv == ticket {
		t.Fatalf("José's chatter was piled onto ticket task %d", ticket)
	}
	if after := s.count(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1`, ticket); after != before {
		t.Errorf("ticket task %d gained %d events from a keyless DM", ticket, after-before)
	}
	// And the next DM attaches to the conversation task, not the ticket.
	m3 := s.dm(t, ctx, "D0DMTTICKET", "Jose Garcia", "Si")
	s.pass(t, ctx, capture.RulesModeLive)
	s.attached(t, ctx, m3, conv, "the conversation task, not the ticket task")
}

// ---- decision 4: rooted threads inside a DM fold into the conversation task -----

func TestRegression_SWT78_RootedThreadFoldsIntoTheConversationTask(t *testing.T) {
	ctx := context.Background()
	s := newDMTSuite(t, ctx)

	// (a) An open conversation task exists; a reply in a ROOTED thread of the
	// same DM (its own normalized_threads row) attaches to it.
	top := s.dm(t, ctx, "D0DMTFOLDA", "Katie", "quick question")
	s.pass(t, ctx, capture.RulesModeLive)
	task := s.created(t, ctx, top, "setup")
	rooted := s.message(t, ctx, dmtMsg{key: dmtRooted("D0DMTFOLDA", "p1758550000000100"), conv: "D0DMTFOLDA",
		convType: "dm", sender: "Katie", body: "in thread: also this"})
	s.pass(t, ctx, capture.RulesModeLive)
	s.attached(t, ctx, rooted, task, "a rooted thread inside the DM folds into the conversation's open task (decision 4); "+
		"a lookup by thread id alone misses it and creates a second task")
	if n := s.conversationTasks(t, ctx, "D0DMTFOLDA"); n != 1 {
		t.Errorf("%d tasks on conversation D0DMTFOLDA, want 1", n)
	}

	// (b) No open task: the rooted-thread message CREATES the conversation
	// task, and a later top-level DM attaches to it.
	first := s.message(t, ctx, dmtMsg{key: dmtRooted("D0DMTFOLDB", "p1758550000000200"), conv: "D0DMTFOLDB",
		convType: "dm", sender: "Dana Ruiz", body: "in thread: starting here"})
	s.pass(t, ctx, capture.RulesModeLive)
	convTask := s.created(t, ctx, first, "a rooted DM thread message with no open conversation task creates it")
	later := s.dm(t, ctx, "D0DMTFOLDB", "Dana Ruiz", "top level now")
	s.pass(t, ctx, capture.RulesModeLive)
	s.attached(t, ctx, later, convTask, "the top-level DM finds the task the rooted thread created (ONE conversation key)")
	if n := s.conversationTasks(t, ctx, "D0DMTFOLDB"); n != 1 {
		t.Errorf("%d tasks on conversation D0DMTFOLDB, want 1", n)
	}
}

// ---- G.3: own messages never create tasks -----------------------------------

func TestRegression_SWT78_OwnMessagesNeverCreateTasks(t *testing.T) {
	ctx := context.Background()
	s := newDMTSuite(t, ctx)

	// Control: a person's DM in the same pass DOES become a task, so the
	// outbound assertions below are not vacuous.
	control := s.dm(t, ctx, "D0DMTCTRL", "Katie", "hello")
	// His reply in a person's DM.
	reply := s.message(t, ctx, dmtMsg{key: dmtKey("D0DMTCTRL"), conv: "D0DMTCTRL", convType: "dm",
		sender: "Salvador Spataro", body: "on it", direction: "outbound"})
	// The self-DM shape (DSA806DHA): outbound only.
	self1 := s.message(t, ctx, dmtMsg{key: dmtKey("D0DMTSELF"), conv: "D0DMTSELF", convType: "dm",
		sender: "Salvador Spataro", body: "note to self", direction: "outbound"})
	self2 := s.message(t, ctx, dmtMsg{key: dmtKey("D0DMTSELF"), conv: "D0DMTSELF", convType: "dm",
		sender: "Salvador Spataro", body: "another note", direction: "outbound"})
	s.pass(t, ctx, capture.RulesModeLive)

	s.created(t, ctx, control, "CONTROL: a person's DM in the same pass")
	for _, m := range []int64{reply, self1, self2} {
		if d, ok := s.decision(t, ctx, m, capture.RulesModeLive); ok {
			t.Errorf("outbound message %d has a capture decision %v; capture decides inbound only (invariant 5)", m, d)
		}
	}
	if n := s.conversationTasks(t, ctx, "D0DMTSELF"); n != 0 {
		t.Errorf("the self-DM (outbound only) has %d tasks, want 0", n)
	}
	if n := s.projectTasks(t, ctx); n != 1 {
		t.Errorf("%d tasks in the project, want 1 (the control's)", n)
	}
}

// ---- G.4: the Jira app's DMs are unchanged ----------------------------------

func TestRegression_SWT78_JiraAppDMUnchanged(t *testing.T) {
	ctx := context.Background()
	s := newDMTSuite(t, ctx)

	// An open ticket task, raised by a person in a channel.
	ticket := s.ticketTask(t, ctx, "C0DMTENG", "public_channel", "DMT-7")
	before := s.projectTasks(t, ctx)

	// With a key: rule 75's task_log onto the ticket task, no comm task.
	withKey := s.dm(t, ctx, "D0DMTJIRAAPP", "Jira", "DMT-7 moved to In Review")
	// Without a key: the workspace catch-all; the notifier keeps it off the DM path.
	noKey := s.dm(t, ctx, "D0DMTJIRAAPP", "Jira", "Your daily digest is ready")
	s.pass(t, ctx, capture.RulesModeLive)

	d, ok := s.decision(t, ctx, withKey, capture.RulesModeLive)
	if !ok || d.action != "task_log" || d.taskID == nil || *d.taskID != ticket {
		t.Errorf("Jira-app DM naming DMT-7: decision %v (found=%v), want rule 75's task_log onto ticket task %d", d, ok, ticket)
	}
	if d.commTask != nil {
		t.Errorf("Jira-app DM made comm task %d; the notifier never does", *d.commTask)
	}
	d, ok = s.decision(t, ctx, noKey, capture.RulesModeLive)
	if !ok || d.action != "attributed" || d.taskID != nil {
		t.Errorf("keyless Jira-app DM: decision %v (found=%v), want attributed with no task: the sender is on the "+
			"project's notifier list (decision 7), so it is not a person's DM", d, ok)
	}
	if n := s.conversationTasks(t, ctx, "D0DMTJIRAAPP"); n != 0 {
		t.Errorf("the Jira app's DM conversation has %d tasks, want 0", n)
	}
	if after := s.projectTasks(t, ctx); after != before {
		t.Errorf("tasks in the project went %d -> %d; the Jira app's DMs create nothing", before, after)
	}
}

// ---- G.5 (capture half): channels still go to the classifier -----------------

func TestRegression_SWT78_ChannelMessageStaysAttributed(t *testing.T) {
	ctx := context.Background()
	s := newDMTSuite(t, ctx)

	// SWT-79 (slack-channel-mentions): a channel message reaches the classifier
	// only when it @-mentions Salvador, so the channel control carries the
	// mention; an unmentioned one is now correctly absent from the inbox.
	ch := s.message(t, ctx, dmtMsg{key: dmtKey("C0DMTGENERAL"), conv: "C0DMTGENERAL", convType: "public_channel",
		sender: "Dana Ruiz", body: "@Salvador morning all"})
	dm := s.dm(t, ctx, "D0DMTDANA", "Dana Ruiz", "morning")
	s.pass(t, ctx, capture.RulesModeLive)

	d, ok := s.decision(t, ctx, ch, capture.RulesModeLive)
	if !ok || d.action != "attributed" || d.taskID != nil {
		t.Errorf("public channel message: decision %v (found=%v), want attributed with no task (channels keep the "+
			"classifier: 'just messages on the general forum need decision')", d, ok)
	}
	s.created(t, ctx, dm, "the DM beside it")

	inbox := s.inquiryInbox(t, ctx)
	if !inbox[ch] {
		t.Errorf("the channel message %d is not in classify's inquiry inbox; channels still get a decision", ch)
	}
	if inbox[dm] {
		t.Errorf("DM message %d is in classify's inquiry inbox (inboxWhereInquiry): DMs skip qwen", dm)
	}
}

// ---- G.6: shadow writes the reason and creates nothing ------------------------

func TestRegression_SWT78_ShadowRecordsTheDMDecisionAndCreatesNothing(t *testing.T) {
	ctx := context.Background()
	s := newDMTSuite(t, ctx)

	m1 := s.dm(t, ctx, "D0DMTSHADOW", "Katie", "first")
	counts := func() string {
		var v string
		if err := s.pool.QueryRow(ctx, `SELECT concat_ws(',',
		    (SELECT count(*) FROM tasks WHERE project_id=$1), (SELECT count(*) FROM task_events),
		    (SELECT count(*) FROM external_refs), (SELECT count(*) FROM audit_events WHERE actor=$2))`,
			s.project, dmtActor).Scan(&v); err != nil {
			t.Fatalf("counts: %v", err)
		}
		return v
	}
	before := counts()
	st := s.pass(t, ctx, capture.RulesModeShadow)

	d, ok := s.decision(t, ctx, m1, capture.RulesModeShadow)
	if !ok || d.action != "task" {
		t.Errorf("shadow decision for a person's DM = %v (found=%v), want action=task (the decision is mode-free)", d, ok)
	}
	if d.taskID != nil {
		t.Errorf("shadow decision names task %d; shadow creates nothing", *d.taskID)
	}
	if !strings.Contains(strings.ToLower(d.reason), "would") {
		t.Errorf("shadow reason %q is not worded by mode (\"would …\", item C)", d.reason)
	}
	if after := counts(); after != before {
		t.Errorf("a shadow pass wrote: tasks,task_events,external_refs,audit %s -> %s", before, after)
	}
	if st.TasksCreated != 0 || st.Appended != 0 {
		t.Errorf("shadow RulesStats = %+v; TasksCreated/Appended are zero in shadow, always", st)
	}
}

// ---- G.7: group DMs, read from the raw column (decision 3) --------------------

// MUTATION: select ” (or drop) the raw conversation type in
// pendingMessageCols → the C… group DM reads as a channel and this goes red.
// The G… row alone would not catch it if someone "fixed" it by id prefix; the
// C… public channel is the guard against that fix.
func TestRegression_SWT78_GroupDMFromTheRawColumn(t *testing.T) {
	ctx := context.Background()
	s := newDMTSuite(t, ctx)

	cGroup := s.message(t, ctx, dmtMsg{key: dmtKey("C0DMTMPDM"), conv: "C0DMTMPDM", convType: "group_dm",
		sender: "Dana Ruiz", body: "hey both"})
	gGroup := s.message(t, ctx, dmtMsg{key: dmtKey("G0DMTMPDM"), conv: "G0DMTMPDM", convType: "group_dm",
		sender: "Esteban", body: "legacy group dm"})
	cChan := s.message(t, ctx, dmtMsg{key: dmtKey("C0DMTCHAN"), conv: "C0DMTCHAN", convType: "public_channel",
		sender: "Dana Ruiz", body: "hey all"})
	s.pass(t, ctx, capture.RulesModeLive)

	c := s.created(t, ctx, cGroup, "a C… conversation the leaf typed group_dm is a group DM (40 of 53 prod mpdms)")
	g := s.created(t, ctx, gGroup, "a G… conversation typed group_dm")
	if c == g {
		t.Errorf("two group DMs share task %d; one task per conversation", c)
	}
	d, ok := s.decision(t, ctx, cChan, capture.RulesModeLive)
	if !ok || d.action != "attributed" || d.taskID != nil {
		t.Errorf("C… public channel: decision %v (found=%v), want attributed: the id prefix is not the rule, the raw type is", d, ok)
	}

	// A second message in the C… group DM attaches.
	again := s.message(t, ctx, dmtMsg{key: dmtKey("C0DMTMPDM"), conv: "C0DMTMPDM", convType: "group_dm",
		sender: "Esteban", body: "agreed"})
	s.pass(t, ctx, capture.RulesModeLive)
	s.attached(t, ctx, again, c, "one task per group-DM conversation")
}

// ---- item E: a person's DM onto a CLOSED ticket task takes the DM path ---------

// Today (406195 → #497 on 09-22): rule 75 logs the DM onto the closed ticket
// task with resurface=true, which puts it in the inquiry lane — qwen again.
// Decision 5: it takes the DM conversation-task path instead.
// MUTATION: resurfaces() without a DM disqualifier → resurface=true, red.
func TestRegression_SWT78_DMOnAClosedTicketTaskTakesTheDMPath(t *testing.T) {
	ctx := context.Background()
	s := newDMTSuite(t, ctx)

	ticket := s.ticketTask(t, ctx, "D0DMTRESURF", "dm", "DMT-497")
	s.closeTask(t, ctx, ticket)
	ticketEvents := s.count(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1`, ticket)

	m := s.dm(t, ctx, "D0DMTRESURF", "Jose Garcia", "about DMT-497, one more change please")
	s.pass(t, ctx, capture.RulesModeLive)

	d, ok := s.decision(t, ctx, m, capture.RulesModeLive)
	if !ok {
		t.Fatalf("no live decision for message %d", m)
	}
	if d.resurface {
		t.Errorf("decision %v: resurface=true — the DM goes to the inquiry lane (qwen). Decision 5: a person's DM "+
			"that would resurface onto a closed ticket task takes the DM conversation-task path", d)
	}
	if d.taskID == nil || *d.taskID == ticket || (d.action != "task" && d.action != "task_log") {
		t.Fatalf("decision %v, want task/task_log onto the DM conversation's task, not closed ticket task %d", d, ticket)
	}
	conv := s.task(t, ctx, *d.taskID)
	if conv.assignee != "human" || conv.refs != 0 || conv.status == "closed" {
		t.Errorf("the DM's task %d = %+v, want an open human task with no external ref", *d.taskID, conv)
	}
	if conv.sourceThread == nil || *conv.sourceThread != s.threads[dmtKey("D0DMTRESURF")] {
		t.Errorf("the DM's task %d source_thread_id = %v, want the DM thread", *d.taskID, conv.sourceThread)
	}
	if st := s.task(t, ctx, ticket).status; st != "closed" {
		t.Errorf("the closed ticket task %d is %q; the DM path never reopens it", ticket, st)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1`, ticket); n != ticketEvents {
		t.Errorf("closed ticket task %d gained %d events; the DM went to its conversation task instead", ticket, n-ticketEvents)
	}
	if s.inquiryInbox(t, ctx)[m] {
		t.Errorf("DM message %d is in classify's inquiry inbox: item E exists so that NO DM reaches qwen", m)
	}
}

// ---- risk: the live pass over a mixed batch must not nil-deref ----------------

// The existing actionTask / actionTaskLog cases dereference *decision.extSystem
// and *decision.extKey (rules_store.go); a DM decision carries NULL for both.
// One live pass over every shape the fix touches must complete: a panic here
// would stall capture for every connector main.
func TestRegression_SWT78_MixedLivePassDoesNotPanic(t *testing.T) {
	ctx := context.Background()
	s := newDMTSuite(t, ctx)

	ticket := s.ticketTask(t, ctx, "C0DMTMIXENG", "public_channel", "DMT-60")
	person := s.dm(t, ctx, "D0DMTMIXP", "Katie", "can you call me")
	personAgain := s.dm(t, ctx, "D0DMTMIXP", "Katie", "whenever")
	rooted := s.message(t, ctx, dmtMsg{key: dmtRooted("D0DMTMIXP", "p1758550000000300"), conv: "D0DMTMIXP",
		convType: "dm", sender: "Katie", body: "in thread"})
	channel := s.message(t, ctx, dmtMsg{key: dmtKey("C0DMTMIXGEN"), conv: "C0DMTMIXGEN", convType: "public_channel",
		sender: "Dana Ruiz", body: "buenos dias"})
	jiraKey := s.dm(t, ctx, "D0DMTMIXJIRA", "Jira", "DMT-60 was updated")
	jiraNoKey := s.dm(t, ctx, "D0DMTMIXJIRA", "Jira", "digest")
	outbound := s.message(t, ctx, dmtMsg{key: dmtKey("D0DMTMIXP"), conv: "D0DMTMIXP", convType: "dm",
		sender: "Salvador Spataro", body: "sure", direction: "outbound"})
	group := s.message(t, ctx, dmtMsg{key: dmtKey("C0DMTMIXMPDM"), conv: "C0DMTMIXMPDM", convType: "group_dm",
		sender: "Esteban", body: "hi"})

	var st capture.RulesStats
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("EvaluateRules(live) PANICKED over the mixed batch: %v — a DM decision has NULL "+
					"external_system/external_key and the act switch dereferenced them", r)
			}
		}()
		st, err = capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{Mode: capture.RulesModeLive, Actor: dmtActor})
	}()
	if err != nil {
		t.Fatalf("EvaluateRules(live) over the mixed batch: %v", err)
	}

	task := s.created(t, ctx, person, "the person's first DM")
	s.attached(t, ctx, personAgain, task, "the person's second DM, same pass")
	s.attached(t, ctx, rooted, task, "the rooted thread in the same DM, same pass")
	s.created(t, ctx, group, "the group DM")
	for name, m := range map[string]int64{"channel": channel, "keyless Jira-app DM": jiraNoKey} {
		if d, ok := s.decision(t, ctx, m, capture.RulesModeLive); !ok || d.action != "attributed" || d.taskID != nil {
			t.Errorf("%s: decision %v (found=%v), want attributed", name, d, ok)
		}
	}
	if d, ok := s.decision(t, ctx, jiraKey, capture.RulesModeLive); !ok || d.action != "task_log" || d.taskID == nil ||
		*d.taskID != ticket {
		t.Errorf("Jira-app DM with a key: decision %v (found=%v), want task_log onto ticket %d", d, ok, ticket)
	}
	if _, ok := s.decision(t, ctx, outbound, capture.RulesModeLive); ok {
		t.Errorf("the outbound message has a decision")
	}
	if st.TasksCreated != 2 {
		t.Errorf("RulesStats.TasksCreated = %d, want 2 (the person's DM, the group DM): %+v", st.TasksCreated, st)
	}
}

// Codex review: EVERY exit that would leave a person's DM merely attributed
// takes the DM path — here a jira rule that matches the DM but derives no key
// (external_system set, key_regex misses). Before the fix this was
// `attributed` and went to qwen.
func TestRegression_SWT78_DMOnAKeylessExternalRuleTakesTheDMPath(t *testing.T) {
	ctx := context.Background()
	s := newDMTSuite(t, ctx)
	s.id(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, priority, enabled, note)
		 VALUES ($1,'body_regex','KEYLESS','jira','NEVERMATCHES-([0-9]+)',95,true,'itest-dmtasks keyless') RETURNING id`, s.project)
	msg := s.dm(t, ctx, "D0KEYLESS", "Dana Ruiz", "KEYLESS can you look at this")
	s.pass(t, ctx, capture.RulesModeLive)
	s.created(t, ctx, msg, "a DM matched by an external-system rule with no derivable key")
	if s.inquiryInbox(t, ctx)[msg] {
		t.Errorf("message %d is in the inquiry inbox; a person's DM never reaches qwen", msg)
	}
}

// go-reviewer: the conversation lookup is scoped to the RULE's project, and a
// conversation id that merely starts with another's (D0X vs D0X1) is a
// different conversation. MUTATIONS: drop `t.project_id = $2`, or match by a
// bare prefix instead of `conv || ':'` — each turns this red.
func TestRegression_SWT78_ConversationLookupIsProjectScopedAndExact(t *testing.T) {
	ctx := context.Background()
	s := newDMTSuite(t, ctx)

	// An open human task in ANOTHER project on the same DM thread.
	other := s.id(t, ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ('itest-dmtasks-other','itest-dmtasks-other','itest-dmtasks-client','manual','dashboard','/tmp/x','any')
		 RETURNING id`)
	// Registered after newDMTSuite's, so it runs FIRST: the foreign task points
	// at a suite thread, and the suite's cleanup deletes the threads.
	t.Cleanup(func() {
		const of = `(SELECT id FROM tasks WHERE project_id=$1)`
		for _, q := range []string{
			`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE task_id IN ` + of + `)`,
			`DELETE FROM audit_events WHERE task_id IN ` + of,
			`DELETE FROM task_events WHERE task_id IN ` + of,
			`DELETE FROM tasks WHERE project_id=$1`,
			`DELETE FROM projects WHERE id=$1`,
		} {
			if _, err := s.pool.Exec(context.Background(), q, other); err != nil {
				t.Errorf("cleanup %q: %v", q, err)
			}
		}
	})
	foreign := s.id(t, ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status, source_thread_id)
		 VALUES ($1,'other project task','human','ready',$2) RETURNING id`, other, s.thread(t, ctx, dmtKey("D0SCOPE")))

	first := s.dm(t, ctx, "D0SCOPE", "Dana Ruiz", "first in D0SCOPE")
	s.pass(t, ctx, capture.RulesModeLive)
	task := s.created(t, ctx, first, "an open task in ANOTHER project is not this conversation's task")
	if task == foreign {
		t.Fatalf("the DM attached to task %d of another project", foreign)
	}

	// D0SCOPE1 starts with D0SCOPE but is another conversation: its own task.
	lookalike := s.dm(t, ctx, "D0SCOPE1", "Dana Ruiz", "first in D0SCOPE1")
	s.pass(t, ctx, capture.RulesModeLive)
	if got := s.created(t, ctx, lookalike, "D0SCOPE1 is not D0SCOPE"); got == task {
		t.Errorf("a message in D0SCOPE1 attached to D0SCOPE's task %d", task)
	}
}

// Codex review: a live pass that died between create_task and
// task_set_source_thread left the conversation's task with no source thread.
// The next DM must find it by its body (marker + conversation line), attach,
// and record the missing provenance — never open a second conversation task.
// MUTATION: drop the body arm of the lookup → a second task, test red.
func TestRegression_SWT78_InterruptedCreateIsFoundAndHealed(t *testing.T) {
	ctx := context.Background()
	s := newDMTSuite(t, ctx)
	conv := dmtKey("D0HEAL")
	half := s.id(t, ctx,
		`INSERT INTO tasks (project_id, title, body, assignee_type, status)
		 VALUES ($1,'Dana Ruiz: first',$2,'human','ready') RETURNING id`, s.project,
		"Captured deterministically: a Slack DM is always actionable (SWT-78), via capture rule 1 (x \"y\").\n\n"+
			"conversation: "+conv+"\nchannel: slack\n")
	msg := s.dm(t, ctx, "D0HEAL", "Dana Ruiz", "second message")
	s.pass(t, ctx, capture.RulesModeLive)
	s.attached(t, ctx, msg, half, "the half-created conversation task is still this conversation's task")
	if n := s.count(t, ctx, `SELECT count(*) FROM tasks WHERE id=$1 AND source_thread_id=$2`, half,
		s.thread(t, ctx, conv)); n != 1 {
		t.Errorf("task %d's missing provenance was not recorded by the attach", half)
	}
	if n := s.conversationTasks(t, ctx, "D0HEAL"); n != 1 {
		t.Errorf("conversation D0HEAL has %d tasks by source thread, want 1", n)
	}
}
