//go:build integration

package main

// TestRegression_SWT78_DirectBackfill_* — bug slack-messages-not-becoming-tasks
// (Jira SWT-78, swb #521; docs/bugs/slack-messages-not-becoming-tasks_DIAGNOSIS.md,
// "Backfill (2026-09-22's 17 DMs)"). Salvador: "and backfill the ones from
// yesterday as tasks".
//
// The fixed capture path cannot re-decide the lost DMs: each already carries a
// live `attributed` decision, and the live claim is one row per message forever
// (capture_decisions_live_uniq). So the backfill is a one-off verb, wired like
// the other capture-rules subcommands that drive a pass (`run`, `gate`):
//
//	opsctl capture-rules direct-backfill --message <id[,id...]> [--dry-run]
//
// Contract encoded here (the diagnosis' recommendation, with decisions 1-4):
//   - it loads the NAMED messages through capture's one projection
//     (pendingMessageCols / scanPendingMessage) and runs the SAME predicate as
//     the live pass: a channel message and the Jira app's DM are skipped;
//   - oldest first (sent_at), whatever order the ids were given in;
//   - per conversation: no open conversation task → create ONE (human, no
//     external_refs, source thread in the conversation); an open one → attach;
//     every later message of the conversation attaches to it;
//   - every backfilled message is named by a `log` event on its conversation
//     task — the repro's "backfilled" PASS outcome reads that line, because the
//     live decision row stays `attributed`;
//   - NO capture_decisions write (the live claim is spent; the trace lives in
//     task_events and audit_events);
//   - everything through the executor (invariant 3): ok audit rows exist;
//   - --dry-run prints what it would do, naming each message, and writes nothing;
//   - idempotent: a second live run creates nothing and logs nothing new.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_dmtasks?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run SWT78 ./cmd/opsctl/
//
// EXPECTED RED: runCaptureRules has no "direct-backfill" verb, so every call
// returns `unknown capture-rules command "direct-backfill"`. The file compiles
// against today's runCaptureRules (main.go) and captureStdout
// (gate_integration_test.go). Cleanup deletes capture_decisions WHOLESALE
// (the capture suites' precedent): isolated database only.

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

	"github.com/sspataro57/switchboard/internal/store"
)

const (
	dbfSlug     = "itest-dmtbackfill"
	dbfProvider = "itest-dmtbackfill-src"
	dbfWS       = "TITESTDBF"
	dbfAccount  = "titestdbf@slack-web.local"
	dbfSubject  = "itest-dmtbackfill"
)

func dbfCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE provider='` + dbfProvider + `')`
	const projs = `(SELECT id FROM projects WHERE slug='` + dbfSlug + `')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	for _, q := range []string{
		`DELETE FROM capture_decisions`,
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM external_refs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + `)`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM capture_rules WHERE project_id IN ` + projs,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`,
		`DELETE FROM normalized_threads WHERE subject='` + dbfSubject + `'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider='` + dbfProvider + `'`,
		`DELETE FROM projects WHERE slug='` + dbfSlug + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

type dbfSuite struct {
	pool    *pgxpool.Pool
	project int64
	rule    int64
	account int64
	threads map[string]int64
	base    time.Time
	sentAt  map[int64]time.Time
}

func newDBFSuite(t *testing.T, ctx context.Context) *dbfSuite {
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
	dbfCleanup(t, ctx, pool)
	t.Cleanup(func() { dbfCleanup(t, context.Background(), pool) })

	s := &dbfSuite{pool: pool, threads: map[string]int64{}, sentAt: map[int64]time.Time{}}
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp() - interval '20 hours'`).Scan(&s.base); err != nil {
		t.Fatalf("db clock: %v", err)
	}
	s.project = s.id(t, ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, ai_locality, ai_inquiry, inquiry_promote_after,
		                       notifier_senders)
		 VALUES ($1,$1,'LlamaSite','manual','dashboard','any',true, now() - interval '10 days','{Jira}') RETURNING id`, dbfSlug)
	s.rule = s.id(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, priority, enabled, note)
		 VALUES ($1,'source_slack_workspace',$2,NULL,1,true,'itest-dmtbackfill') RETURNING id`, s.project, dbfWS)
	s.account = s.id(t, ctx,
		`INSERT INTO source_accounts (provider, account_email, scopes, send_enabled, calendar_in_availability)
		 VALUES ($1,$2,'{}',false,false) RETURNING id`, dbfProvider, dbfAccount)
	return s
}

func (s *dbfSuite) id(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var v int64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return v
}

func (s *dbfSuite) count(t *testing.T, ctx context.Context, q string, args ...any) int {
	t.Helper()
	return int(s.id(t, ctx, q, args...))
}

func (s *dbfSuite) thread(t *testing.T, ctx context.Context, conv string) int64 {
	t.Helper()
	key := "slack:" + dbfWS + ":" + conv
	if id, ok := s.threads[key]; ok {
		return id
	}
	id := s.id(t, ctx, `INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,$2,'[]') RETURNING id`,
		key, dbfSubject)
	s.threads[key] = id
	return id
}

// lost seeds one message in the 2026-09-22 state: inbound, normalized, and
// already carrying a LIVE `attributed` decision from the workspace catch-all
// (so neither `capture-rules run` nor the fixed live pass will ever re-decide it).
// minute orders sent_at independently of insert order.
func (s *dbfSuite) lost(t *testing.T, ctx context.Context, label, conv, convType, sender string, minute int) int64 {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{
		"kind":         "message",
		"workspace":    map[string]any{"id": dbfWS, "own_user_id": "U0DBFSELF"},
		"conversation": map[string]any{"id": conv, "name": conv, "type": convType},
		"message":      map[string]any{"id": label, "author": sender, "author_id": "U0DBF" + label, "text": label + " text"},
	})
	rawID := s.id(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,$3::jsonb,$4, now()) RETURNING id`, s.account, dbfSubject+"-"+label, string(raw), "h-dbf-"+label)
	sent := s.base.Add(time.Duration(minute) * time.Minute)
	msg := s.id(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1,$2,'inbound',$3,$4,$5,'',$6,'slack') RETURNING id`,
		rawID, s.thread(t, ctx, conv), dbfSubject+"-"+label, sent, label+" text", sender)
	s.id(t, ctx,
		`INSERT INTO capture_decisions (message_id, raw_source_item_id, mode, matched_rule_id, matched_rule_ids, project_id,
		                                action, reason)
		 VALUES ($1,$2,'live',$3,ARRAY[$3::bigint],$4,'attributed','rule (source_slack_workspace) attributes; attribution only')
		 RETURNING id`, msg, rawID, s.rule, s.project)
	s.sentAt[msg] = sent
	return msg
}

// state is every table the verb could touch, as one comparable string.
func (s *dbfSuite) state(t *testing.T, ctx context.Context) string {
	t.Helper()
	var v string
	if err := s.pool.QueryRow(ctx, `SELECT concat_ws(',',
	    (SELECT count(*) FROM tasks WHERE project_id=$1),
	    (SELECT count(*) FROM task_events WHERE task_id IN (SELECT id FROM tasks WHERE project_id=$1)),
	    (SELECT count(*) FROM external_refs),
	    (SELECT count(*) FROM capture_decisions),
	    (SELECT count(*) FROM audit_events))`, s.project).Scan(&v); err != nil {
		t.Fatalf("state: %v", err)
	}
	return v
}

func idList(ids ...int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprint(id)
	}
	return strings.Join(parts, ",")
}

// logOrder returns, in task_events id order, the message ids (from want) that
// the task's `log` events name.
func (s *dbfSuite) logOrder(t *testing.T, ctx context.Context, task int64, want []int64) []int64 {
	t.Helper()
	rows, err := s.pool.Query(ctx, `SELECT payload::text FROM task_events WHERE task_id=$1 AND event_type='log' ORDER BY id`, task)
	if err != nil {
		t.Fatalf("read log events: %v", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan: %v", err)
		}
		for _, m := range want {
			if regexp.MustCompile(`\b` + fmt.Sprint(m) + `\b`).MatchString(p) {
				out = append(out, m)
			}
		}
	}
	return out
}

// conversationTask returns the ONE task whose source thread is conv's, or fails.
func (s *dbfSuite) conversationTask(t *testing.T, ctx context.Context, conv string) int64 {
	t.Helper()
	rows, err := s.pool.Query(ctx, `SELECT id FROM tasks WHERE project_id=$1 AND source_thread_id=$2`, s.project,
		s.threads["slack:"+dbfWS+":"+conv])
	if err != nil {
		t.Fatalf("read conversation tasks: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	if len(ids) != 1 {
		t.Fatalf("conversation %s has tasks %v, want exactly one (one task per conversation)", conv, ids)
	}
	return ids[0]
}

func TestRegression_SWT78_DirectBackfill_DryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	s := newDBFSuite(t, ctx)
	j1 := s.lost(t, ctx, "j1", "D0DBFJOSE", "dm", "Jose Garcia", 1)
	j2 := s.lost(t, ctx, "j2", "D0DBFJOSE", "dm", "Jose Garcia", 5)
	k1 := s.lost(t, ctx, "k1", "D0DBFKATIE", "dm", "Katie", 3)

	before := s.state(t, ctx)
	out, err := captureStdout(t, func() error {
		return runCaptureRules("direct-backfill", []string{"--message", idList(j2, k1, j1), "--dry-run"})
	})
	if err != nil {
		t.Fatalf("opsctl capture-rules direct-backfill --dry-run: %v\n%s", err, out)
	}
	t.Logf("--dry-run:\n%s", out)
	for _, m := range []int64{j1, j2, k1} {
		if !regexp.MustCompile(`\b` + fmt.Sprint(m) + `\b`).MatchString(out) {
			t.Errorf("--dry-run output does not name message %d:\n%s", m, out)
		}
	}
	if after := s.state(t, ctx); after != before {
		t.Errorf("--dry-run wrote: tasks,task_events,external_refs,capture_decisions,audit_events %s -> %s", before, after)
	}
}

func TestRegression_SWT78_DirectBackfill_OneTaskPerConversationOldestFirstIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newDBFSuite(t, ctx)

	// José's DM (DSAV4HJ2F shape): three lost messages, seeded out of order.
	j3 := s.lost(t, ctx, "j3", "D0DBFJOSE", "dm", "Jose Garcia", 30)
	j1 := s.lost(t, ctx, "j1", "D0DBFJOSE", "dm", "Jose Garcia", 1)
	j2 := s.lost(t, ctx, "j2", "D0DBFJOSE", "dm", "Jose Garcia", 10)
	// Katie's DM (D04F7LXRB8B shape): two, incl. 403664's gated ask.
	k1 := s.lost(t, ctx, "k1", "D0DBFKATIE", "dm", "Katie", 2)
	k2 := s.lost(t, ctx, "k2", "D0DBFKATIE", "dm", "Katie", 40)
	// A conversation that already HAS an open human conversation task: attach, create nothing.
	open := s.id(t, ctx, `INSERT INTO tasks (project_id, title, body, status, assignee_type, source_thread_id)
	                      VALUES ($1,'Dana Ruiz: earlier','', 'ready','human',$2) RETURNING id`,
		s.project, s.thread(t, ctx, "D0DBFDANA"))
	d1 := s.lost(t, ctx, "d1", "D0DBFDANA", "dm", "Dana Ruiz", 4)
	// Named by mistake / by a broad query: the SAME predicate skips them.
	ch := s.lost(t, ctx, "ch", "C0DBFGENERAL", "public_channel", "Dana Ruiz", 6)
	jira := s.lost(t, ctx, "jira", "D0DBFJIRA", "dm", "Jira", 7)

	all := []int64{k2, ch, j3, d1, j1, jira, k1, j2} // deliberately not sent_at order
	decisionsBefore := s.count(t, ctx, `SELECT count(*) FROM capture_decisions`)
	tasksBefore := s.count(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, s.project)

	out, err := captureStdout(t, func() error {
		return runCaptureRules("direct-backfill", []string{"--message", idList(all...)})
	})
	if err != nil {
		t.Fatalf("opsctl capture-rules direct-backfill: %v\n%s", err, out)
	}
	t.Logf("live run:\n%s", out)

	if got := s.count(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, s.project); got != tasksBefore+2 {
		t.Errorf("tasks in the project went %d -> %d, want +2 (José's and Katie's conversations; Dana's has an open task)",
			tasksBefore, got)
	}
	jose := s.conversationTask(t, ctx, "D0DBFJOSE")
	katie := s.conversationTask(t, ctx, "D0DBFKATIE")
	if got := s.conversationTask(t, ctx, "D0DBFDANA"); got != open {
		t.Errorf("Dana's conversation task is %d, want the existing open task %d", got, open)
	}
	for _, task := range []int64{jose, katie} {
		var assignee string
		var refs int
		if err := s.pool.QueryRow(ctx, `SELECT assignee_type, (SELECT count(*) FROM external_refs WHERE task_id=$1)
		                                  FROM tasks WHERE id=$1`, task).Scan(&assignee, &refs); err != nil {
			t.Fatalf("read task %d: %v", task, err)
		}
		if assignee != "human" || refs != 0 {
			t.Errorf("backfilled task %d: assignee=%s refs=%d, want a human task with no external ref", task, assignee, refs)
		}
		if n := s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE tool='task_append_log' AND task_id=$1 AND status='ok'`,
			task); n == 0 {
			t.Errorf("no ok task_append_log audit rows on backfilled task %d (invariant 3)", task)
		}
		if n := s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE tool='task_set_source_thread' AND task_id=$1 AND status='ok'`,
			task); n == 0 {
			t.Errorf("no ok task_set_source_thread audit row on backfilled task %d", task)
		}
	}

	// Every message named by a log line on its conversation task, OLDEST FIRST.
	for _, c := range []struct {
		name string
		task int64
		want []int64
	}{
		{"José", jose, []int64{j1, j2, j3}},
		{"Katie", katie, []int64{k1, k2}},
		{"Dana (existing open task)", open, []int64{d1}},
	} {
		got := s.logOrder(t, ctx, c.task, c.want)
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: task %d's log events name messages %v, want %v (each backfilled message logged, oldest first "+
				"by sent_at, whatever order --message gave)", c.name, c.task, got, c.want)
		}
	}

	// The predicate: the channel message and the Jira app's DM got nothing.
	for name, m := range map[string]int64{"channel": ch, "Jira app DM": jira} {
		if n := s.count(t, ctx, `SELECT count(*) FROM task_events e JOIN tasks t ON t.id=e.task_id
		                          WHERE t.project_id=$1 AND e.payload::text ~ ('\m' || $2::text || '\M')`, s.project, fmt.Sprint(m)); n != 0 {
			t.Errorf("%s message %d is named by %d task events; the backfill runs the live pass's predicate and skips it", name, m, n)
		}
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM tasks t JOIN normalized_threads nt ON nt.id=t.source_thread_id
	                          WHERE t.project_id=$1 AND nt.thread_key IN ($2,$3)`, s.project,
		"slack:"+dbfWS+":C0DBFGENERAL", "slack:"+dbfWS+":D0DBFJIRA"); n != 0 {
		t.Errorf("%d tasks were created for the channel / Jira-app conversations", n)
	}

	// No capture_decisions write: every live row is still the original `attributed`.
	if got := s.count(t, ctx, `SELECT count(*) FROM capture_decisions`); got != decisionsBefore {
		t.Errorf("capture_decisions went %d -> %d; the backfill writes none (the live claim is spent forever)", decisionsBefore, got)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM capture_decisions WHERE message_id = ANY($1) AND action <> 'attributed'`,
		all); n != 0 {
		t.Errorf("%d decision rows changed action", n)
	}

	// Idempotent: a second live run creates nothing and logs nothing new.
	events := s.count(t, ctx, `SELECT count(*) FROM task_events WHERE task_id IN (SELECT id FROM tasks WHERE project_id=$1)`, s.project)
	out, err = captureStdout(t, func() error {
		return runCaptureRules("direct-backfill", []string{"--message", idList(all...)})
	})
	if err != nil {
		t.Fatalf("second direct-backfill run: %v\n%s", err, out)
	}
	t.Logf("second run:\n%s", out)
	if got := s.count(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, s.project); got != tasksBefore+2 {
		t.Errorf("the second run changed the task count to %d (want %d): the backfill is not idempotent", got, tasksBefore+2)
	}
	if got := s.count(t, ctx, `SELECT count(*) FROM task_events WHERE task_id IN (SELECT id FROM tasks WHERE project_id=$1)`,
		s.project); got != events {
		t.Errorf("the second run added %d task events: a re-run must log nothing new", got-events)
	}
}

// Codex review: a run that died between create_task and its log left a task
// the message created (its body names the message) with no provenance and no
// marker. A retry must FINISH that task, not create a second conversation task.
func TestRegression_SWT78_DirectBackfill_RecoversAnInterruptedCreate(t *testing.T) {
	ctx := context.Background()
	s := newDBFSuite(t, ctx)
	m := s.lost(t, ctx, "rec1", "D0DBFREC", "dm", "Rae Quinn", 1)
	// What the dead run left: create_task's row only — the DM body marker and
	// the message_id line, no source thread, no log.
	orphan := s.id(t, ctx,
		`INSERT INTO tasks (project_id, title, body, assignee_type, status)
		 VALUES ($1,'Rae Quinn: rec1 text',$2,'human','ready') RETURNING id`, s.project,
		"Captured deterministically: a Slack DM is always actionable (SWT-78), via capture rule 1 (x \"y\").\n\n"+
			"conversation: slack:"+dbfWS+":D0DBFREC\nmessage_id: "+fmt.Sprint(m)+"\n\nrec1 text")
	before := s.count(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, s.project)

	if _, err := captureStdout(t, func() error {
		return runCaptureRules("direct-backfill", []string{"--message", idList(m)})
	}); err != nil {
		t.Fatalf("direct-backfill: %v", err)
	}
	if got := s.count(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, s.project); got != before {
		t.Errorf("tasks went %d -> %d; the retry created a duplicate instead of finishing task %d", before, got, orphan)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM tasks WHERE id=$1 AND source_thread_id=$2`, orphan,
		s.thread(t, ctx, "D0DBFREC")); n != 1 {
		t.Errorf("the recovered task %d has no provenance; want source_thread_id = the DM's thread", orphan)
	}
	if got := s.logOrder(t, ctx, orphan, []int64{m}); len(got) != 1 {
		t.Errorf("the recovered task %d does not log message %d", orphan, m)
	}
	// And the recovery itself is idempotent.
	if _, err := captureStdout(t, func() error {
		return runCaptureRules("direct-backfill", []string{"--message", idList(m)})
	}); err != nil {
		t.Fatalf("second direct-backfill: %v", err)
	}
	if got := s.logOrder(t, ctx, orphan, []int64{m}); len(got) != 1 {
		t.Errorf("a second run logged message %d again (%d lines)", m, len(got))
	}
}

// go-reviewer: the backfill only repairs what the old path LOST — a message
// whose live decision is `attributed`. One already decided task/task_log
// (by the fixed pass) or still pending is skipped, so nothing is logged twice.
func TestRegression_SWT78_DirectBackfill_SkipsMessagesNotAttributed(t *testing.T) {
	ctx := context.Background()
	s := newDBFSuite(t, ctx)
	m := s.lost(t, ctx, "na1", "D0DBFNA", "dm", "Rae Quinn", 1)
	if _, err := s.pool.Exec(ctx, `UPDATE capture_decisions SET action='task_log' WHERE message_id=$1 AND mode='live'`, m); err != nil {
		t.Fatalf("re-shape the decision: %v", err)
	}
	before := s.state(t, ctx)
	out, err := captureStdout(t, func() error {
		return runCaptureRules("direct-backfill", []string{"--message", idList(m)})
	})
	if err != nil {
		t.Fatalf("direct-backfill: %v", err)
	}
	if !strings.Contains(out, "not attributed") {
		t.Errorf("output does not say message %d was skipped for its live decision:\n%s", m, out)
	}
	if after := s.state(t, ctx); after != before {
		t.Errorf("a message already decided task_log was backfilled: state %s -> %s", before, after)
	}
}
