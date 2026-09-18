//go:build integration

package promote_test

// chat-on-closed-task (SWT-53, docs/tickets/chat-on-closed-task_SPEC.md)
// against a real database, the PROMOTE side: criteria 13 (inquiryInbox, the
// same four clauses as classify's), 14 (a real capture pass → a seeded
// classify_inquiry verdict → promote.Run on the inquiry lane after the grace
// → a Holding task on the message's own conversation; the closed task stays
// closed), and 15 (T5: a second ask on the same DM attaches; T14 claude_task;
// `answered` and `pending` gate exactly as for an attributed message).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_isochat?sslmode=disable \
//	  TZ=UTC go test -tags integration -p 1 -count=1 -run PromoteInquiryResurface ./internal/promote/
//
// Reuses inquiry_integration_test.go's harness (iqpSuite: the armed / unarmed
// / nocut projects, the verdict writer in classify's inquiryFields shape, the
// cleanup pact). Build-tagged `integration`, env-gated, FATAL on 192.168.50.49,
// NO LLM (verdicts are hand-written rows), NO network.
//
// Criterion 14 runs capture.EvaluateRules LIVE, whose pending set is GLOBAL:
// use an isolated database. Its rule carries note 'itest-inqp' and every row it
// touches is on this suite's projects, so iqpCleanup clears it.
//
// ---- IMPOSED SURFACE ------------------------------------------------------------
//
//	promote.Verdict.LoggedOnTaskID (the latest LIVE decision's task_id for a resurface-branch
//	decision); the inquiry body's LAST line `logged_on_closed_task: N`; the
//	promotion reason part "capture logged the message onto closed task N;
//	resurfaced (chat-on-closed-task)". InquiryGate, Decide, threadTask and
//	inquiryCreateStatus unchanged.
//
// RED TODAY: capture_decisions has no resurface column; every test FATALs in
// rsRequire0034 until 0034 is applied.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/promote"
)

const rsCaptureActor = "capture:itest-inqp"

func rsRequire0034(t *testing.T, ctx context.Context, s *iqpSuite) {
	t.Helper()
	if n := s.count(t, ctx, `SELECT count(*) FROM information_schema.columns
	                          WHERE table_name='capture_decisions' AND column_name='resurface'`); n != 1 {
		t.Fatalf("capture_decisions.resurface does not exist; apply migrations/0034_chat_on_closed_task.sql " +
			"(make migrate LOCAL_DB_URL=...)")
	}
}

func rsPart(task int64) string {
	return fmt.Sprintf("capture logged the message onto closed task %d; resurfaced (chat-on-closed-task)", task)
}

// rsTask creates a task through the executor (the ref task capture logs onto).
func (s *iqpSuite) rsTask(t *testing.T, ctx context.Context, slug, label string) int64 {
	t.Helper()
	args := fmt.Sprintf(`{"project":%q,"title":%q,"body":"itest-inqp ref task","assignee_type":"human","priority":0}`,
		slug, "itest-inqp ref "+label)
	res, err := s.ex.Execute(ctx, executor.Call{Tool: "create_task", Actor: iqpCloser, Args: []byte(args)})
	if err != nil {
		t.Fatalf("setup create_task: %v", err)
	}
	var out struct {
		TaskID int64 `json:"task_id"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil || out.TaskID == 0 {
		t.Fatalf("setup create_task output %s: %v", res.Output, err)
	}
	return out.TaskID
}

func (s *iqpSuite) rsClose(t *testing.T, ctx context.Context, task int64) {
	t.Helper()
	s.execute(t, ctx, "task_close", iqpCloser, task, fmt.Sprintf(`{"task_id":%d,"reason":"itest-inqp done"}`, task))
	if st := s.status(t, ctx, task); st != "closed" {
		t.Fatalf("setup: task %d is %q after task_close", task, st)
	}
}

// rsMessage is iqpSuite.message with a chosen body (capture's rule matches it).
func (s *iqpSuite) rsMessage(t *testing.T, ctx context.Context, label, key, channel, sender, body string,
	sentAt time.Time) (msg, raw int64) {
	t.Helper()
	raw = s.id(t, ctx, `INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
	                    VALUES ($1,$2,'{}',$3, now()) RETURNING id`, s.account, "itest-inqp-"+label, "itest-inqp-h-"+label)
	msg = s.id(t, ctx, `INSERT INTO normalized_messages
	                      (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
	                       body_text, subject, sender, channel)
	                    VALUES ($1,$2,'inbound',$3,$4,$5,'',$6,$7) RETURNING id`,
		raw, s.thread(t, ctx, key), "itest-inqp-ext-"+label, sentAt, body, sender, channel)
	return msg, raw
}

// rsDecision writes capture's row shape for a message logged onto task.
func (s *iqpSuite) rsDecision(t *testing.T, ctx context.Context, msg, project, task int64, resurface bool) {
	t.Helper()
	s.exec(t, ctx, `INSERT INTO capture_decisions (message_id, mode, action, project_id, external_system, external_key,
	                                               task_id, resurface, reason)
	                VALUES ($1,'live','task_log',$2,'jira','IQW',$3,$4,'itest-inqp resurface')`, msg, project, task, resurface)
}

// rsAsk seeds an ask logged onto task (resurface as given) with a stored verdict.
func (s *iqpSuite) rsAsk(t *testing.T, ctx context.Context, label, key, channel, scope string, sentAt time.Time,
	project, task int64, resurface bool) int64 {
	t.Helper()
	m, r := s.message(t, ctx, iqpMsg{label: label, key: key, channel: channel, sentAt: sentAt})
	s.rsDecision(t, ctx, m, project, task, resurface)
	s.verdict(t, ctx, m, r, iqpV{scope: scope, project: project})
	return m
}

func (s *iqpSuite) rsTaskRow(t *testing.T, ctx context.Context, task int64) (status, body string, srcThread *int64) {
	t.Helper()
	if err := s.pool.QueryRow(ctx, `SELECT status, COALESCE(body,''), source_thread_id FROM tasks WHERE id=$1`, task).
		Scan(&status, &body, &srcThread); err != nil {
		t.Fatalf("read task %d: %v", task, err)
	}
	return status, body, srcThread
}

// ---- criterion 13: inquiryInbox, one fixture per clause -----------------------------

// MUTATIONS: drop the `EXISTS … lt.status = 'closed'` → reopened promotes;
// drop `live.resurface` → noflag and ontheopen promote; drop p.ai_inquiry →
// unarmed promotes; leave `ON latest.action = 'attributed'` → admitted does
// not promote.
func TestPromoteInquiryResurface_Integration_InboxOneFixturePerClause(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	rsRequire0034(t, ctx, s)

	closed := s.rsTask(t, ctx, iqpArmed, "closed")
	s.rsClose(t, ctx, closed)
	reopened := s.rsTask(t, ctx, iqpArmed, "reopened")
	s.rsClose(t, ctx, reopened)
	open := s.rsTask(t, ctx, iqpArmed, "open")
	unarmedClosed := s.rsTask(t, ctx, iqpUnarmed, "unarmed")
	s.rsClose(t, ctx, unarmedClosed)
	tasksBefore := s.armedTasks(t, ctx)

	admitted := s.rsAsk(t, ctx, "rs-admitted", gmailKey("rs-admitted"), "gmail", "thread", s.ago(2*time.Hour),
		s.armed, closed, true)
	excluded := map[string]int64{
		"resurface=false onto a closed task": s.rsAsk(t, ctx, "rs-noflag", gmailKey("rs-noflag"), "gmail", "thread",
			s.ago(2*time.Hour), s.armed, closed, false),
		"resurface=true whose task has since been reopened (T7)": s.rsAsk(t, ctx, "rs-reopened", gmailKey("rs-reopened"),
			"gmail", "thread", s.ago(2*time.Hour), s.armed, reopened, true),
		"resurface=true in a project without ai_inquiry (T12)": s.rsAsk(t, ctx, "rs-unarmed", gmailKey("rs-unarmed"),
			"gmail", "thread", s.ago(2*time.Hour), s.unarmed, unarmedClosed, true),
		"a task_log onto an open task": s.rsAsk(t, ctx, "rs-ontheopen", gmailKey("rs-ontheopen"), "gmail", "thread",
			s.ago(2*time.Hour), s.armed, open, false),
	}
	s.exec(t, ctx, `UPDATE tasks SET status='ready' WHERE id=$1`, reopened) // reopened after capture decided

	st := s.run(t, ctx, promote.Config{})

	p, ok := s.promotion(t, ctx, admitted)
	if !ok || p.action != "task" || p.taskID == nil || *p.taskID == closed {
		t.Fatalf("a resurfaced task_log onto a closed task: promotion %+v (found=%v), want a NEW review (holding) "+
			"task — CC5: the inbox admits it exactly like an attributed message; CC2: never the closed task", p, ok)
	}
	status, body, _ := s.rsTaskRow(t, ctx, *p.taskID)
	if status != "ready" {
		t.Errorf("the resurfaced ask's task is %q, want ready (O7 unchanged)", status)
	}
	if want := "logged_on_closed_task: " + strconv.FormatInt(closed, 10) + "\n"; !strings.HasSuffix(body, want) {
		t.Errorf("task body does not END with %q (CC6: one line, LAST):\n%s", want, body)
	}
	if !strings.Contains(p.reason, rsPart(closed)) {
		t.Errorf("promotion reason %q does not contain %q (CC6)", p.reason, rsPart(closed))
	}
	for why, msg := range excluded {
		if p, ok := s.promotion(t, ctx, msg); ok {
			t.Errorf("%s: promoted (%+v); criterion 13 excludes it", why, p)
		}
	}
	if n := gatedTotal(st); n != 0 {
		t.Errorf("Gated = %v; every exclusion above is an INBOX clause, never a gate reason", st.Gated)
	}
	if st.Created != 1 || s.armedTasks(t, ctx) != tasksBefore+1 {
		t.Errorf("stats %+v, %d armed tasks (was %d); want exactly one ready task", st, s.armedTasks(t, ctx), tasksBefore)
	}
	if got := s.status(t, ctx, closed); got != "closed" {
		t.Errorf("the closed task is %q after promotion; CC2: it stays closed", got)
	}
}

// CC5b: a SHADOW decision never promotes through the resurface branch. The
// live row is task_log with resurface=false; a newer shadow re-evaluation writes
// task_log with resurface=true and the message carries a needs_reply verdict.
// Nothing may promote it, so no live Holding task comes from a shadow row. The
// live resurfaced ask beside it is the positive control.
// MUTATION: drop `AND lcd.mode = 'live'` from InquiryLiveDecisionJoinSQL →
// the shadow row becomes the `live` row, the shadowed ask promotes, red.
func TestPromoteInquiryResurface_Integration_AShadowResurfacePromotesNothing(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	rsRequire0034(t, ctx, s)
	closed := s.rsTask(t, ctx, iqpArmed, "closed shadow")
	s.rsClose(t, ctx, closed)
	tasksBefore := s.armedTasks(t, ctx)

	control := s.rsAsk(t, ctx, "rs-live-control", gmailKey("rs-live-control"), "gmail", "thread", s.ago(2*time.Hour),
		s.armed, closed, true)

	m, r := s.message(t, ctx, iqpMsg{label: "rs-shadowed", key: gmailKey("rs-shadowed"), channel: "gmail",
		sentAt: s.ago(2 * time.Hour)})
	s.rsDecision(t, ctx, m, s.armed, closed, false)
	s.exec(t, ctx, `INSERT INTO capture_decisions (message_id, mode, action, project_id, external_system, external_key,
	                                               task_id, resurface, reason)
	                VALUES ($1,'shadow','task_log',$2,'jira','IQW',$3,true,'itest-inqp shadow re-evaluation')`,
		m, s.armed, closed)
	s.verdict(t, ctx, m, r, iqpV{scope: "thread", project: s.armed})

	st := s.run(t, ctx, promote.Config{})

	if p, ok := s.promotion(t, ctx, control); !ok || p.action != "task" {
		t.Fatalf("POSITIVE CONTROL: the LIVE resurfaced ask: promotion %+v (found=%v), want a ready task", p, ok)
	}
	if p, ok := s.promotion(t, ctx, m); ok {
		t.Errorf("a message whose live decision is task_log resurface=false, under a NEWER shadow task_log "+
			"resurface=true, promoted (%+v). CC5b: only a LIVE decision admits through the resurface branch; a "+
			"shadow re-evaluation must never create a live Holding task", p)
	}
	if st.Created != 1 || s.armedTasks(t, ctx) != tasksBefore+1 {
		t.Errorf("stats %+v, %d armed tasks (was %d); want exactly one ready task, the control's",
			st, s.armedTasks(t, ctx), tasksBefore)
	}
	if n := gatedTotal(st); n != 0 {
		t.Errorf("Gated = %v; the shadowed message is kept out by the INBOX, never gated", st.Gated)
	}
}

// CC5b, the reverse: shadow decisions never REMOVE a resurfaced message. The
// live decision is task_log resurface=true onto a closed task; a newer shadow
// `unmatched` row (no project) sits on top. The ask still promotes, under the
// live row's project, and the body and reason still name the closed task.
// MUTATIONS: read the resurface fact from the any-mode `latest` row, or join
// projects on latest.project_id → nothing promotes, red.
func TestPromoteInquiryResurface_Integration_ANewerShadowRowRemovesNothing(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	rsRequire0034(t, ctx, s)
	closed := s.rsTask(t, ctx, iqpArmed, "closed reverse")
	s.rsClose(t, ctx, closed)

	m := s.rsAsk(t, ctx, "rs-reverse", gmailKey("rs-reverse"), "gmail", "thread", s.ago(2*time.Hour),
		s.armed, closed, true)
	s.exec(t, ctx, `INSERT INTO capture_decisions (message_id, mode, action, reason)
	                VALUES ($1,'shadow','unmatched','itest-inqp shadow unmatched')`, m)

	st := s.run(t, ctx, promote.Config{})

	p, ok := s.promotion(t, ctx, m)
	if !ok || p.action != "task" || p.taskID == nil || *p.taskID == closed {
		t.Fatalf("a live resurfaced ask under a NEWER shadow `unmatched` row: promotion %+v (found=%v), want a NEW "+
			"review task. CC5b: the resurface branch reads only live decisions, so a non-attributed shadow row "+
			"must not remove it", p, ok)
	}
	if p.project != s.armed {
		t.Errorf("promotion project = %v, want the live row's %d", p.project, s.armed)
	}
	_, body, _ := s.rsTaskRow(t, ctx, *p.taskID)
	if want := "logged_on_closed_task: " + strconv.FormatInt(closed, 10) + "\n"; !strings.HasSuffix(body, want) {
		t.Errorf("task body does not END with %q (LoggedOnTaskID comes from the LIVE row):\n%s", want, body)
	}
	if !strings.Contains(p.reason, rsPart(closed)) {
		t.Errorf("promotion reason %q does not contain %q", p.reason, rsPart(closed))
	}
	if st.Created != 1 {
		t.Errorf("stats %+v, want Review 1", st)
	}
}

// CC5b's ACCEPTED corner (owner-session decision 2026-09-14): a NEWER shadow
// `attributed` row takes precedence under the Part A re-point contract. The
// live decision is task_log resurface=true onto a closed task in the armed
// project; a newer shadow row attributes the message to a SECOND armed
// project. The attributed branch admits it under the SHADOW row's project,
// with LoggedOnTaskID = 0: no logged_on_closed_task line, no resurface reason.
func TestPromoteInquiryResurface_Integration_ANewerShadowAttributedRowTakesPrecedence(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	rsRequire0034(t, ctx, s)
	armed2 := s.id(t, ctx, `INSERT INTO projects (name, slug, client, execution, delivery, ai_locality, ai_classify,
	                                              ai_inquiry, inquiry_promote_after)
	                        VALUES ($1,$1,'LlamaSite','manual','dashboard','any',false,true, now() - interval '1 hour')
	                        RETURNING id`, iqpArmed+"2")
	closed := s.rsTask(t, ctx, iqpArmed, "closed shadow-attributed")
	s.rsClose(t, ctx, closed)

	m := s.rsAsk(t, ctx, "rs-shadow-attr", gmailKey("rs-shadow-attr"), "gmail", "thread", s.ago(2*time.Hour),
		s.armed, closed, true)
	s.decision(t, ctx, m, "shadow", "attributed", armed2)

	st := s.run(t, ctx, promote.Config{})

	p, ok := s.promotion(t, ctx, m)
	if !ok || p.action != "task" || p.taskID == nil || *p.taskID == closed {
		t.Fatalf("live resurfaced task_log under a NEWER shadow `attributed` row: promotion %+v (found=%v), want a "+
			"NEW review task (the attributed branch admits it; Part A: attribution moves)", p, ok)
	}
	if p.project != armed2 {
		t.Errorf("promotion project = %d, want the SHADOW row's %d (the newer attributed row takes precedence), "+
			"not the live row's %d", p.project, armed2, s.armed)
	}
	var taskProject int64
	if err := s.pool.QueryRow(ctx, `SELECT project_id FROM tasks WHERE id=$1`, *p.taskID).Scan(&taskProject); err != nil {
		t.Fatalf("read the new task's project: %v", err)
	}
	if taskProject != armed2 {
		t.Errorf("the new task is in project %d, want the shadow row's %d", taskProject, armed2)
	}
	_, body, _ := s.rsTaskRow(t, ctx, *p.taskID)
	if strings.Contains(body, "logged_on_closed_task:") {
		t.Errorf("the body carries a logged_on_closed_task line; an attributed admission has LoggedOnTaskID = 0:\n%s", body)
	}
	if strings.Contains(p.reason, rsPart(closed)) {
		t.Errorf("promotion reason %q names the closed task; an attributed admission has LoggedOnTaskID = 0", p.reason)
	}
	if st.Created != 1 {
		t.Errorf("stats %+v, want Review 1", st)
	}
	if got := s.status(t, ctx, closed); got != "closed" {
		t.Errorf("the closed task is %q; it stays closed", got)
	}
}

// ---- criterion 14: through real rows, capture pass first ------------------------------

// T1 end to end. MUTATIONS: capture writes resurface=false → setup FATAL;
// the inbox left at attributed-only → no promotion; inquiryBody without the
// line → the suffix assertion; a reopen of the closed task anywhere → the
// status / task_reopen assertions.
func TestPromoteInquiryResurface_Integration_ACapturePassBecomesAReadyTaskOnTheDM(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	rsRequire0034(t, ctx, s)

	// Rule 10's SHAPE on the armed project: a prefix group, jira, no key_regex.
	s.id(t, ctx, `INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, priority, enabled, note)
	              VALUES ($1,'body_regex','(IQW|IQA|IQO)-[0-9]+','jira',NULL,90,true,'itest-inqp') RETURNING id`, s.armed)
	bucket := s.rsTask(t, ctx, iqpArmed, "bucket IQW")
	s.execute(t, ctx, "link_external_ref", iqpCloser, bucket,
		fmt.Sprintf(`{"task_id":%d,"system":"jira","external_key":"IQW"}`, bucket))
	s.rsClose(t, ctx, bucket)
	logsBefore := s.count(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='log'`, bucket)

	dm := slackKey("D0IQPRS1", "")
	msg, raw := s.rsMessage(t, ctx, "rs-capture", dm, "slack", "asunda45", "can you check IQW-10355?", s.ago(2*time.Hour))
	if _, err := capture.EvaluateRules(ctx, s.pool, s.ex,
		capture.RulesConfig{Mode: capture.RulesModeLive, Actor: rsCaptureActor}); err != nil {
		t.Fatalf("capture.EvaluateRules(live): %v", err)
	}
	var (
		action    string
		logged    *int64
		resurface bool
	)
	if err := s.pool.QueryRow(ctx, `SELECT action, task_id, resurface FROM capture_decisions
	                                  WHERE message_id=$1 AND mode='live'`, msg).Scan(&action, &logged, &resurface); err != nil {
		t.Fatalf("setup: read capture's live decision: %v", err)
	}
	if action != "task_log" || logged == nil || *logged != bucket || !resurface {
		t.Fatalf("setup: capture's live decision = {%s, task %v, resurface %v}, want {task_log, %d, true}: criterion 14 "+
			"starts from capture's OWN row", action, logged, resurface, bucket)
	}

	s.verdict(t, ctx, msg, raw, iqpV{scope: "conversation", asker: "asunda45", ask: "can you check IQW-10355?"})
	before := s.armedTasks(t, ctx)
	st := s.run(t, ctx, promote.Config{})

	p, ok := s.promotion(t, ctx, msg)
	if !ok || p.action != "task" || p.taskID == nil || *p.taskID == bucket {
		t.Fatalf("the resurfaced DM: promotion %+v (found=%v), want a NEW ready task (not the bucket %d)", p, ok, bucket)
	}
	task := *p.taskID
	status, body, src := s.rsTaskRow(t, ctx, task)
	if status != "ready" {
		t.Errorf("the new task is %q, want ready", status)
	}
	if src == nil || *src != s.threads[dm] {
		t.Errorf("the new task's source_thread_id = %v, want the DM's thread %d (the message's OWN conversation)", src, s.threads[dm])
	}
	want := "logged_on_closed_task: " + strconv.FormatInt(bucket, 10) + "\n"
	if !strings.HasSuffix(body, want) || strings.Count(body, "logged_on_closed_task:") != 1 {
		t.Errorf("the task body does not end with exactly one %q:\n%s", want, body)
	}
	if !strings.Contains(p.reason, rsPart(bucket)) {
		t.Errorf("promotion reason %q does not name the closed task (%q)", p.reason, rsPart(bucket))
	}
	if st.Created != 1 {
		t.Errorf("stats %+v, want Review 1", st)
	}

	if got := s.status(t, ctx, bucket); got != "closed" {
		t.Errorf("the closed bucket is %q; CC2: it stays closed", got)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool='task_reopen'`, bucket); n != 0 {
		t.Errorf("%d task_reopen audit row(s) for the closed bucket; no tool reopens the closed ref task", n)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='log'`, bucket); n != logsBefore+1 {
		t.Errorf("log events on the bucket went %d -> %d, want exactly one new one (capture's)", logsBefore, n)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE actor='promote:inquiry' AND task_id=$1`, bucket); n != 0 {
		t.Errorf("%d promote:inquiry call(s) on the closed bucket; promote acts on the conversation's task only", n)
	}

	// Run twice: nothing new.
	st2 := s.run(t, ctx, promote.Config{})
	if n := s.armedTasks(t, ctx); n != before+1 {
		t.Errorf("%d armed tasks after a second run, want %d (no second task)", n, before+1)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM classify_promotions WHERE normalized_message_id=$1`, msg); n != 1 {
		t.Errorf("%d promotion rows for the message, want 1", n)
	}
	if st2.Review+st2.Created+st2.Attached != 0 {
		t.Errorf("the second run acted: %+v", st2)
	}
}

// ---- criterion 15: T5, a second ask on the same DM attaches -----------------------------

func TestPromoteInquiryResurface_Integration_ASecondAskOnTheSameDMAttaches(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	rsRequire0034(t, ctx, s)
	bucket := s.rsTask(t, ctx, iqpArmed, "bucket T5")
	s.rsClose(t, ctx, bucket)
	dm := slackKey("D0IQPRS2", "")

	a := s.rsAsk(t, ctx, "rs-first", dm, "slack", "conversation", s.ago(3*time.Hour), s.armed, bucket, true)
	s.run(t, ctx, promote.Config{})
	holding := s.taskOf(t, ctx, a)
	if st := s.status(t, ctx, holding); st != "ready" {
		t.Fatalf("setup: the first resurfaced ask's task is %q, want ready", st)
	}

	b := s.rsAsk(t, ctx, "rs-second", dm, "slack", "conversation", s.ago(2*time.Hour), s.armed, bucket, true)
	st := s.run(t, ctx, promote.Config{})

	p, ok := s.promotion(t, ctx, b)
	if !ok || p.action != "attached" || p.taskID == nil || *p.taskID != holding {
		t.Fatalf("second resurfaced ask on the same DM: %+v (found=%v), want attached to the Holding task %d (T5: "+
			"Decide, unchanged)", p, ok, holding)
	}
	if st.Attached != 1 || s.armedTasks(t, ctx) != 2 {
		t.Errorf("stats %+v, %d armed tasks; want one attach and exactly two tasks (the bucket and the Holding one)",
			st, s.armedTasks(t, ctx))
	}
	if !strings.Contains(p.reason, rsPart(bucket)) {
		t.Errorf("attach reason %q does not name the closed task", p.reason)
	}
	if got := s.status(t, ctx, bucket); got != "closed" {
		t.Errorf("the bucket is %q; it stays closed", got)
	}
}

// ---- criterion 15: T14 and the grace / answered gates, twin for twin ----------------------

// Each gate reason is exercised twice: once on an `attributed` message and
// once on a resurfaced one. The counts must be equal — the gate reads stored
// facts only and cannot tell the two admissions apart (InquiryGate unchanged).
func TestPromoteInquiryResurface_Integration_GatesExactlyAsForAnAttributedMessage(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	rsRequire0034(t, ctx, s)
	bucket := s.rsTask(t, ctx, iqpArmed, "bucket gates")
	s.rsClose(t, ctx, bucket)

	type mk func(label, key, channel, scope string, sentAt time.Time) int64
	twins := []struct {
		name string
		ask  mk
	}{
		{"A", func(label, key, channel, scope string, sentAt time.Time) int64 { // attributed
			m, r := s.message(t, ctx, iqpMsg{label: label, key: key, channel: channel, sentAt: sentAt})
			s.decision(t, ctx, m, "live", "attributed", s.armed)
			s.verdict(t, ctx, m, r, iqpV{scope: scope})
			return m
		}},
		{"R", func(label, key, channel, scope string, sentAt time.Time) int64 { // resurfaced
			return s.rsAsk(t, ctx, label, key, channel, scope, sentAt, s.armed, bucket, true)
		}},
	}
	var asks []int64
	for _, tw := range twins {
		// pending: a DM ask inside the 1h grace.
		asks = append(asks, tw.ask("rs-pending-"+tw.name, slackKey("D0IQPRSP"+tw.name, ""), "slack", "conversation",
			s.ago(10*time.Minute)))
		// answered: he spoke in the DM after the ask.
		ka := slackKey("D0IQPRSA"+tw.name, "")
		asks = append(asks, tw.ask("rs-answered-"+tw.name, ka, "slack", "conversation", s.ago(2*time.Hour)))
		s.message(t, ctx, iqpMsg{label: "rs-answered-ours-" + tw.name, key: ka, channel: "slack", direction: "outbound",
			sentAt: s.ago(time.Hour)})
		// claude_task (T14): the thread's open task is a claude task.
		kc := gmailKey("rs-claude-" + tw.name)
		s.id(t, ctx, `INSERT INTO tasks (project_id, title, body, assignee_type, status, priority, source_thread_id)
		              VALUES ($1,$2,'','claude','ready',0,$3) RETURNING id`,
			s.armed, "itest-inqp rs claude "+tw.name, s.thread(t, ctx, kc))
		asks = append(asks, tw.ask("rs-claude-"+tw.name, kc, "gmail", "thread", s.ago(2*time.Hour)))
	}

	auditBefore := s.inquiryAudit(t, ctx)
	st := s.run(t, ctx, promote.Config{})

	for _, reason := range []string{"pending", "answered", promote.GateClaudeTask} {
		if st.Gated[reason] != 2 {
			t.Errorf("Gated[%s] = %d, want 2 (one attributed, one resurfaced — the gate is unchanged); Gated = %v",
				reason, st.Gated[reason], st.Gated)
		}
	}
	for _, m := range asks {
		if p, ok := s.promotion(t, ctx, m); ok {
			t.Errorf("gated message %d wrote a promotion row %+v", m, p)
		}
	}
	if n := s.inquiryAudit(t, ctx); n != auditBefore {
		t.Errorf("%d promote:inquiry call(s); a gated verdict calls no tool", n-auditBefore)
	}
	if st.Review+st.Created+st.Attached != 0 {
		t.Errorf("stats %+v; nothing may be acted on", st)
	}
}
