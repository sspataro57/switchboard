//go:build integration

package promote_test

// activity-resurfaces (SWT-72, docs/tickets/activity-resurfaces_SPEC.md) D11
// against a real database: criteria 14, 16's positive half, 17, 18 and 42 —
// an inquiry ask on a thread that ALREADY has an open task becomes its OWN
// task, with a light pointer both ways, and the old task is not surfaced.
//
// Reuses inquiry_integration_test.go's iqpSuite wholesale (its cleanup pact,
// its armed project, its message/verdict/decision fixtures, its FATAL guard on
// the production host). NO LLM, NO network: verdicts are ai_runs +
// ai_extractions rows in exactly the shape classify's inquiryFields writes.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_swt72?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run OwnAsk ./internal/promote/
//
// RED TODAY: Decision has no RelatedTaskID, so package promote_test does not
// compile; after that, the pass ATTACHES (the behaviour this ticket removes)
// and migration 0039 is not applied (oaRequire0039).
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - Decide returns `attached` for an inquiry verdict with an open thread task
//     -> AnAskOnAnOpenThreadBecomesItsOwnTask.
//   - the pointer log carries the message's title or sender text -> the log-text assertion.
//   - the pointer log also marks activity on the old task -> the activity_at assertion.
//   - drop related_task from the body -> the body assertion.
//   - keep C-D13 gating an OPEN claude thread task -> AClaudeThreadTaskNoLongerGates.
//   - loosen GateAnswered -> AnAnsweredAskStillCreatesNothing.

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/promote"
)

func oaRequire0039(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name='tasks' AND column_name IN ('activity_at','activity_by_message_id','reviewed_at')`).Scan(&n); err != nil {
		t.Fatalf("probe 0039's columns: %v", err)
	}
	if n != 3 {
		t.Fatalf("found %d of the 3 columns migration 0039 adds. Criterion 1: apply "+
			"migrations/0039_task_activity_review.sql (`make migrate LOCAL_DB_URL=...`)", n)
	}
}

// oaThreadTask seeds a thread in the armed project whose only task is OPEN and
// belongs to `assignee`, then an eligible ask on that thread. Returns the old
// task and the ask's message. This is the trigger's exact shape: #452, ready,
// human, with José's "Listo para arrancar WEB-10469" arriving on its thread.
func oaThreadTask(t *testing.T, ctx context.Context, s *iqpSuite, label, assignee string) (task, msg int64) {
	t.Helper()
	key := gmailKey(label)
	th := s.thread(t, ctx, key)
	task = s.id(t, ctx, `INSERT INTO tasks (project_id, title, body, assignee_type, status, priority, source_thread_id)
	                     VALUES ($1,$2,'',$3,'ready',0,$4) RETURNING id`, s.armed, "itest-inqp "+label, assignee, th)
	m, r := s.message(t, ctx, iqpMsg{label: label, key: key, sentAt: s.ago(2 * time.Hour)})
	s.decision(t, ctx, m, "live", "attributed", s.armed)
	s.verdict(t, ctx, m, r, iqpV{scope: "thread", ask: "Listo para arrancar WEB-10469?"})
	return task, m
}

func oaTaskActivity(t *testing.T, ctx context.Context, s *iqpSuite, task int64) *time.Time {
	t.Helper()
	var at *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT activity_at FROM tasks WHERE id=$1`, task).Scan(&at); err != nil {
		t.Fatalf("read activity_at of task %d: %v", task, err)
	}
	return at
}

// oaLogEvents returns the `log` event messages on a task, in id order.
func oaLogEvents(t *testing.T, ctx context.Context, s *iqpSuite, task int64) []string {
	t.Helper()
	rows, err := s.pool.Query(ctx,
		`SELECT COALESCE(payload->>'message','') FROM task_events
		  WHERE task_id=$1 AND event_type='log' ORDER BY id`, task)
	if err != nil {
		t.Fatalf("read log events of task %d: %v", task, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			t.Fatalf("scan log event: %v", err)
		}
		out = append(out, m)
	}
	return out
}

// ---- criteria 14 and 42 -----------------------------------------------------------

func TestPromoteOwnAsk_Integration_AnAskOnAnOpenThreadBecomesItsOwnTask(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	oaRequire0039(t, ctx, s.pool)

	old, msg := oaThreadTask(t, ctx, s, "ownask", "human")
	st := s.run(t, ctx, promote.Config{})

	// A SECOND task exists on the same thread. tasks.source_thread_id is
	// deliberately not unique (0019: "allows many tasks per conversation"), and
	// Q3 now reads "…for the personal lane" (D11's consequence, accepted).
	if st.Attached != 0 {
		t.Errorf("stats %+v: the pass ATTACHED. D11 (owner, 2026-09-22): \"those messages to create it's own "+
			"tasks even is they require action on other tasks\"", st)
	}
	if st.Related != 1 {
		t.Errorf("Stats.Related = %d, want 1 (criterion 14)", st.Related)
	}
	p, ok := s.promotion(t, ctx, msg)
	if !ok || p.taskID == nil {
		t.Fatalf("no promotion row with a task for the ask (found=%v, %+v)", ok, p)
	}
	newTask := *p.taskID
	if newTask == old {
		t.Fatalf("the promotion names the OLD task %d; D11: the ask is its own task", old)
	}
	if p.action != "review" && p.action != "task" {
		t.Errorf("promotion action = %q, want the inquiry lane's create action, never `attached` (criterion 14)", p.action)
	}
	if !strings.Contains(p.reason, strconv.FormatInt(old, 10)) ||
		!strings.Contains(p.reason, "an ask is always its own task") {
		t.Errorf("promotion reason = %q, want it to name task %d and the owner decision (D11: decisionReason "+
			"gains a part so classify_promotions.reason records it)", p.reason, old)
	}
	if n := s.armedTasks(t, ctx); n != 2 {
		t.Errorf("%d tasks on the armed project, want 2: the ticket task plus the ask's own task", n)
	}

	// The new task's body ends with the pointer (criterion 15).
	var body, status string
	if err := s.pool.QueryRow(ctx, `SELECT body, status FROM tasks WHERE id=$1`, newTask).Scan(&body, &status); err != nil {
		t.Fatalf("read the new task: %v", err)
	}
	if !strings.HasSuffix(body, "related_task: "+strconv.FormatInt(old, 10)+"\n") {
		t.Errorf("the new task's body does not end `related_task: %d`:\n%s\n(criteria 15, 42)", old, body)
	}

	// The OLD task gets ONE id-only pointer log, and NO activity mark.
	logs := oaLogEvents(t, ctx, s, old)
	want := "promote: ask #" + strconv.FormatInt(newTask, 10) + " created from this thread (message " +
		strconv.FormatInt(msg, 10) + ")"
	if len(logs) != 1 || logs[0] != want {
		t.Errorf("the old task's log events = %v, want exactly one %q (criterion 14: ids only, no message text — "+
			"which is why the pointer is safe on a claude task)", logs, want)
	}
	if at := oaTaskActivity(t, ctx, s, old); at != nil {
		t.Errorf("the pointer log also marked activity on the old task (activity_at=%v). D11, an EXPLICIT "+
			"exclusion: the NEW task is the thing to look at, and surfacing the old one too would double the "+
			"rows (criterion 14)", at)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE actor='promote:inquiry'
	                          AND tool='task_append_log' AND task_id=$1`, old); n != 1 {
		t.Errorf("%d task_append_log calls as promote:inquiry on the old task, want exactly 1 (invariant 3)", n)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE tool='task_mark_activity' AND task_id=$1`, old); n != 0 {
		t.Errorf("%d task_mark_activity audit rows on the old task, want 0 (criterion 14)", n)
	}

	// Re-running creates nothing (the claim is one row per message, forever).
	st2 := s.run(t, ctx, promote.Config{})
	if st2.Considered != 0 || s.armedTasks(t, ctx) != 2 {
		t.Errorf("a second pass: stats %+v, %d tasks; want nothing (criterion 18)", st2, s.armedTasks(t, ctx))
	}
	if logs := oaLogEvents(t, ctx, s, old); len(logs) != 1 {
		t.Errorf("a second pass wrote another pointer log (%d in all)", len(logs))
	}
}

// Criterion 18: two asks on the same thread in one pass create TWO tasks, each
// with its own promotion row and its own pointer log; the pointers both name
// threadTask's result — the thread's OLDEST open task in the project, which is
// the task the ask would have been piled onto today.
func TestPromoteOwnAsk_Integration_TwoAsksOnOneThreadCreateTwoTasks(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	oaRequire0039(t, ctx, s.pool)

	old, first := oaThreadTask(t, ctx, s, "ownask-two", "human")
	key := gmailKey("ownask-two")
	b, br := s.message(t, ctx, iqpMsg{label: "ownask-two-b", key: key, sentAt: s.ago(90 * time.Minute)})
	s.decision(t, ctx, b, "live", "attributed", s.armed)
	s.verdict(t, ctx, b, br, iqpV{scope: "thread", ask: "Pa cuando tienes planeado el release?"})

	s.run(t, ctx, promote.Config{})

	t1, t2 := s.taskOf(t, ctx, first), s.taskOf(t, ctx, b)
	if t1 == t2 || t1 == old || t2 == old {
		t.Fatalf("two asks yielded tasks %d and %d (old %d); want two NEW, distinct tasks (criterion 18)", t1, t2, old)
	}
	if n := s.armedTasks(t, ctx); n != 3 {
		t.Errorf("%d tasks on the armed project, want 3 (the ticket task plus one per ask)", n)
	}
	logs := oaLogEvents(t, ctx, s, old)
	if len(logs) != 2 {
		t.Errorf("the old task has %d pointer logs, want 2 — one per ask (criterion 18)", len(logs))
	}
	for _, l := range logs {
		if !strings.HasPrefix(l, "promote: ask #") {
			t.Errorf("pointer log %q is not D11's text", l)
		}
	}
	if at := oaTaskActivity(t, ctx, s, old); at != nil {
		t.Errorf("two pointer logs surfaced the old task (activity_at=%v); neither marks activity", at)
	}
}

// Criterion 16's positive half: an inquiry verdict whose thread's OPEN task is
// a CLAUDE task is NOT gated any more, and creates its own HUMAN task. C-D13's
// justification was the attach and the reopen; with no attach, gating it would
// keep the black hole open for exactly the messages this ticket is about.
func TestPromoteOwnAsk_Integration_AClaudeThreadTaskNoLongerGates(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	oaRequire0039(t, ctx, s.pool)

	old, msg := oaThreadTask(t, ctx, s, "ownask-claude", "claude")
	st := s.run(t, ctx, promote.Config{})

	if st.Gated[promote.GateClaudeTask] != 0 {
		t.Errorf("the verdict was gated %q. Criterion 16: the gate now refuses only when the thread task is not "+
			"a human's AND Decide would ATTACH to it (rule 2's dismissed path)", promote.GateClaudeTask)
	}
	p, ok := s.promotion(t, ctx, msg)
	if !ok || p.taskID == nil || *p.taskID == old {
		t.Fatalf("no new task for an ask on a claude task's thread (found=%v, %+v); D11 + criterion 16", ok, p)
	}
	var assignee, body string
	if err := s.pool.QueryRow(ctx, `SELECT assignee_type, body FROM tasks WHERE id=$1`, *p.taskID).
		Scan(&assignee, &body); err != nil {
		t.Fatalf("read the new task: %v", err)
	}
	if assignee != "human" {
		t.Errorf("the ask's own task is assignee_type %q, want human (C-D9: an ask needs a reply from HIM)", assignee)
	}
	if !strings.HasSuffix(body, "related_task: "+strconv.FormatInt(old, 10)+"\n") {
		t.Errorf("the new task's body does not end `related_task: %d`:\n%s", old, body)
	}
	// The pointer log is ids only, which is exactly what makes it safe here: the
	// claude task's log feeds a worker prompt.
	logs := oaLogEvents(t, ctx, s, old)
	if len(logs) != 1 || strings.Contains(logs[0], "Listo para arrancar") {
		t.Errorf("the claude task's log events = %v, want exactly one ids-only pointer with NO message text "+
			"(criterion 14, the C-D13 worry)", logs)
	}
	if at := oaTaskActivity(t, ctx, s, old); at != nil {
		t.Errorf("the claude task was surfaced into INCOMING by the pointer (activity_at=%v); the pointer is not "+
			"an activity mark", at)
	}

	// The DISMISSED claude shape is STILL gated (criterion 16's other half). A
	// fresh suite: assertClaudeGated counts audit rows and tasks suite-wide.
	s = newIQPSuite(t, ctx)
	task, dismissal, m := s.claudeThreadTask(t, ctx, "ownask-claude-dismissed", true)
	s.assertClaudeGated(t, ctx, task, m, "closed")
	if n := s.count(t, ctx, `SELECT count(*) FROM task_dismissals WHERE id=$1 AND reopened_at IS NULL`, dismissal); n != 1 {
		t.Errorf("dismissal %d was reopened; a dismissed claude task must never go back to a console queue", dismissal)
	}
}

// Criterion 17: the replied-since fold STAYS. An ask he answered on the thread
// is still gated `answered` and creates NOTHING — D11 widens what a passing
// verdict BECOMES, never what passes. Column-fed: the outbound reply is a real
// normalized_messages row that replyfold's join reads.
func TestPromoteOwnAsk_Integration_AnAnsweredAskStillCreatesNothing(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	oaRequire0039(t, ctx, s.pool)

	old, msg := oaThreadTask(t, ctx, s, "ownask-answered", "human")
	// Our own reply, AFTER the ask, on the same thread.
	s.message(t, ctx, iqpMsg{label: "ownask-answered-reply", key: gmailKey("ownask-answered"),
		direction: "outbound", sentAt: s.ago(time.Hour)})

	st := s.run(t, ctx, promote.Config{})

	if st.Gated[promote.GateAnswered] != 1 {
		t.Errorf("stats %+v, want exactly one gated %q (criterion 17: GateAnswered is unchanged and still first "+
			"among the clauses that matter here)", st, promote.GateAnswered)
	}
	if st.Created+st.Review+st.Attached+st.Related != 0 {
		t.Errorf("a gated verdict acted: %+v", st)
	}
	if p, ok := s.promotion(t, ctx, msg); ok {
		t.Errorf("a gated verdict wrote a promotion row %+v (criterion 17)", p)
	}
	if n := s.armedTasks(t, ctx); n != 1 {
		t.Errorf("%d tasks on the armed project, want 1 (only the seeded ticket task): an answered ask becomes "+
			"nothing at all", n)
	}
	if logs := oaLogEvents(t, ctx, s, old); len(logs) != 0 {
		t.Errorf("the old task got %v; a gated verdict calls no tool", logs)
	}
}
