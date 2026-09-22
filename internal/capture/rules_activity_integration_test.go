//go:build integration

package capture_test

// activity-resurfaces (SWT-72, docs/tickets/activity-resurfaces_SPEC.md) D7 and
// criteria 8, 10 and 11 against a real database: EVERY task_log attach a RULE
// makes onto an OPEN task marks activity on it, whatever the channel, whatever
// the rule kind — and a CLOSED target is left alone.
//
// This is the whole point of the ticket. The trigger measured four shapes in
// the ops db on 2026-09-21/22, and they all funnel through ONE branch:
//
//   - shape 1: Katie's six Jira comments, a thread_key_prefix rule;
//   - shape 2: José's direct mail from jose.g@avviato.com, a body_regex rule on
//     a WEB-NNNNN key, with no Jira in the path at all;
//   - shape 4: the same rule set over Slack and Upwork.
//
// So the hook cannot key on the channel or the rule kind (D7), and criterion 10
// drives a body_regex gmail rule and a sender Slack rule onto the SAME task.
//
// Reuses rules_revive_integration_test.go's crvSuite wholesale (its wholesale
// capture_decisions cleanup, its three accounts, its rule helpers).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_swt72?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run CaptureActivity ./internal/capture/
//
// RED TODAY: migration 0039 is not applied (craRequire0039), then RulesStats
// has no Activity counter, so this file does not compile.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - key the capture hook on the channel or the rule kind -> BlindToChannelAndRule
//     (the José case).
//   - drop the closed-task skip in task_mark_activity -> ClosedTargetsAreUntouched.
//   - move the mark before appendRuleLog -> TheLogComesFirst (the audit order).
//   - skip the mark in shadow mode... is REQUIRED: ShadowMarksNothing.
//
// CROSS-SUITE DISCIPLINE: EvaluateRules' pending set is GLOBAL, so crvCleanup
// deletes capture_decisions WHOLESALE at start and end. Run against an ISOLATED
// database (IK 2026-09-12: the compose `ops` db is shared by every worktree).

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/executor"
)

func craRequire0039(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
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

// craDismiss closes a task the board's way: task_dismiss through the executor
// as a human (crvDismisser), which writes the typed task_dismissals row
// SWT-36's guarded reopen reads.
func craDismiss(t *testing.T, ctx context.Context, s *crvSuite, task int64) {
	t.Helper()
	if _, err := s.ex.Execute(ctx, executor.Call{Tool: "task_dismiss", Actor: crvDismisser, TaskID: &task,
		Args: []byte(fmt.Sprintf(`{"task_id":%d,"reason_code":"not_actionable"}`, task))}); err != nil {
		t.Fatalf("task_dismiss: %v", err)
	}
}

func craActivity(t *testing.T, ctx context.Context, s *crvSuite, task int64) (*time.Time, *int64) {
	t.Helper()
	var at *time.Time
	var by *int64
	if err := s.pool.QueryRow(ctx,
		`SELECT activity_at, activity_by_message_id FROM tasks WHERE id=$1`, task).Scan(&at, &by); err != nil {
		t.Fatalf("read activity of task %d: %v", task, err)
	}
	return at, by
}

// craSeedTask runs one live pass over a Jira comment so the ticket's task
// EXISTS the way production made it (create_task + link_external_ref +
// task_set_source_thread through the executor), then clears the activity the
// creation path may have left, so each test starts from "open task, no activity".
func craSeedTask(t *testing.T, ctx context.Context, s *crvSuite, key string) int64 {
	t.Helper()
	now := s.dbNow(t, ctx)
	s.jiraMsg(t, ctx, key, now.Add(-2*time.Hour), now.Add(-2*time.Hour))
	s.pass(t, ctx, "live")
	task := s.mustTask(t, ctx, key)
	s.exec(t, ctx, `UPDATE tasks SET activity_at = NULL, activity_by_message_id = NULL WHERE id=$1`, task)
	// 2026-09-22 (swb #491): the create path marks activity too, so the seed
	// pass leaves one task_mark_activity audit row behind. The tests below count
	// what THEIR pass adds, so that row is removed here — the seed is fixture,
	// not the thing under test.
	s.exec(t, ctx, `DELETE FROM policy_decisions WHERE audit_event_id IN
	                  (SELECT id FROM audit_events WHERE task_id=$1 AND tool='task_mark_activity')`, task)
	s.exec(t, ctx, `DELETE FROM audit_events WHERE task_id=$1 AND tool='task_mark_activity'`, task)
	return task
}

// ---- criteria 8 and 10: blind to the channel and to the rule kind ----------------

func TestCaptureActivity_Integration_BlindToChannelAndRule(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	craRequire0039(t, ctx, s.pool)

	task := craSeedTask(t, ctx, s, "CRV-7001")
	if at, _ := craActivity(t, ctx, s, task); at != nil {
		t.Fatalf("CONTROL: the seeded task already carries activity_at=%v", at)
	}
	if got := s.status(t, ctx, task); got == "closed" {
		t.Fatalf("CONTROL: the seeded task is closed; every assertion below would be a skip")
	}

	// Shape 2, the JOSÉ case: a DIRECT gmail message, no Jira in the path,
	// claimed by a body_regex rule on the key.
	s.mentionRule(t, ctx, false)
	now := s.dbNow(t, ctx)
	mail := s.mailMsg(t, ctx, "jose.g@avviato.example", "inbound",
		"PR #3247 — required External ID locks out the importer",
		"CRV-7001 blocks the import; can you look?", now, now)
	st := s.pass(t, ctx, "live")

	at1, by1 := craActivity(t, ctx, s, task)
	if at1 == nil || by1 == nil || *by1 != mail {
		t.Fatalf("a body_regex gmail attach left activity_at=%v by=%v, want set / %d. Criterion 10, D7: the hook "+
			"sits in the ONE actionTaskLog branch every rule kind and every connector funnels through — shape 2 "+
			"(José writes directly, capture rule 75 files it onto the ticket's task) is the proof it cannot key "+
			"on the channel", at1, by1, mail)
	}
	if st.Activity != 1 {
		t.Errorf("RulesStats.Activity = %d, want 1 (criterion 8: counted from marked:true)", st.Activity)
	}
	if st.Appended != 1 {
		t.Errorf("RulesStats.Appended = %d, want 1; the mark rides on the existing attach, it does not replace it", st.Appended)
	}
	if n := s.audits(t, ctx, task, "task_mark_activity", crvActor); n != 1 {
		t.Errorf("%d task_mark_activity audit rows as %s, want 1 (criterion 8, invariant 3: through the executor "+
			"as cfg.Actor)", n, crvActor)
	}
	if n := s.audits(t, ctx, task, "task_append_log", crvActor); n != 1 {
		t.Errorf("%d task_append_log audit rows, want 1 — the log line is unchanged", n)
	}

	// Shape 1, the SAME task through a DIFFERENT RULE KIND: Katie's Jira comment
	// arrives on the connector thread and the thread_key_prefix rule files it.
	mid := s.dbNow(t, ctx)
	comment := s.jiraMsg(t, ctx, "CRV-7001", mid, mid)
	stJira := s.pass(t, ctx, "live")
	atJira, byJira := craActivity(t, ctx, s, task)
	if byJira == nil || *byJira != comment || atJira == nil || !atJira.After(*at1) {
		t.Errorf("a thread_key_prefix jira attach left activity_by=%v activity_at=%v, want %d and later than %v. "+
			"Criterion 10: a sender rule, a body_regex rule and a thread_key_prefix rule all converge on the "+
			"same task_log branch", byJira, atJira, comment, at1)
	}
	if stJira.Activity != 1 {
		t.Errorf("jira pass RulesStats.Activity = %d, want 1", stJira.Activity)
	}

	// Shape 4, the SAME task over SLACK: the activity moves again.
	later := s.dbNow(t, ctx)
	slack := s.slackMsg(t, ctx, "any chance of a look at CRV-7001 today?", later, later)
	st2 := s.pass(t, ctx, "live")
	at2, by2 := craActivity(t, ctx, s, task)
	if by2 == nil || *by2 != slack || at2 == nil || !at2.After(*atJira) {
		t.Errorf("a Slack attach left activity_by=%v activity_at=%v, want %d and later than %v. Criterion 10: "+
			"\"slack from jose and katie are critical. messages from yersterday went to black hole\"",
			by2, at2, slack, atJira)
	}
	if st2.Activity != 1 {
		t.Errorf("second pass RulesStats.Activity = %d, want 1", st2.Activity)
	}

	// The SWT-45 column is NOT touched (D1 / criterion 26's other half).
	if sat, sby := s.surfaced(t, ctx, task); sat != nil || sby != nil {
		t.Errorf("the attaches wrote surfaced_at=%v by=%v; D1: surfaced_at keeps its SWT-45 meaning and the "+
			"reconciler still closes a done ticket's task", sat, sby)
	}
	// And the mark writes NO event: the log line is the only one.
	if n := s.events(t, ctx, task, "log"); n != 3 {
		t.Errorf("%d log events on the task, want 3 (one per attach); the mark adds none (D3)", n)
	}
}

// Criterion 8 / D3: the ORDER is log first, then the mark — a crash between the
// two degrades to today's behaviour (logged, unsurfaced), which is the
// appendRuleLog -> reviveRuleTask discipline SWT-45 established.
func TestCaptureActivity_Integration_TheLogComesFirst(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	craRequire0039(t, ctx, s.pool)

	task := craSeedTask(t, ctx, s, "CRV-7002")
	now := s.dbNow(t, ctx)
	s.jiraMsg(t, ctx, "CRV-7002", now, now)
	s.pass(t, ctx, "live")

	rows, err := s.pool.Query(ctx,
		`SELECT tool FROM audit_events WHERE task_id=$1 AND actor=$2
		   AND tool IN ('task_append_log','task_mark_activity') ORDER BY id`, task, crvActor)
	if err != nil {
		t.Fatalf("read audit order: %v", err)
	}
	defer rows.Close()
	var seq []string
	for rows.Next() {
		var tool string
		if err := rows.Scan(&tool); err != nil {
			t.Fatalf("scan audit: %v", err)
		}
		seq = append(seq, tool)
	}
	if len(seq) != 2 || seq[0] != "task_append_log" || seq[1] != "task_mark_activity" {
		t.Errorf("audit sequence = %v, want [task_append_log task_mark_activity] (criterion 8: after "+
			"appendRuleLog succeeds and before the prClose/revive/reopen branch)", seq)
	}
}

// Criterion 11: a capture attach onto a CLOSED task leaves activity_at NULL —
// and the SWT-45 revive still happens. That is what keeps closed-task work
// (SWT-45's revive, SWT-53's chat-on-closed-task) out of this ticket for free:
// both branches run AFTER the mark, so the task is still closed when the mark
// is attempted and the mark SELF-EXCLUDES (D3).
func TestCaptureActivity_Integration_ClosedTargetsAreUntouched(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	craRequire0039(t, ctx, s.pool)

	// (a) SWT-45's revive shape: a reviving rule, a task closed by hand.
	revived := craSeedTask(t, ctx, s, "CRV-7003")
	s.humanClose(t, ctx, revived)
	s.mentionRule(t, ctx, true) // revive = true
	now := s.dbNow(t, ctx)
	s.mailMsg(t, ctx, "jose.g@avviato.example", "inbound", "CRV-7003 is back",
		"CRV-7003 needs another look", now, now)
	st := s.pass(t, ctx, "live")

	if at, _ := craActivity(t, ctx, s, revived); at != nil {
		t.Errorf("an attach onto a CLOSED task set activity_at=%v. Criterion 11 / D3: a closed task is a SKIP "+
			"(task_closed), which is what keeps SWT-45 and SWT-53 out of this ticket by construction", at)
	}
	if st.Revived != 1 {
		t.Errorf("RulesStats.Revived = %d, want 1 — the revive still happens; the mark runs BEFORE it and "+
			"self-excludes (criterion 11)", st.Revived)
	}
	if got := s.status(t, ctx, revived); got == "closed" {
		t.Errorf("the revived task is still closed (%q); the mark must not have swallowed the revive", got)
	}
	// The revive DOES surface it, through SWT-45's own column — unchanged.
	if sat, _ := s.surfaced(t, ctx, revived); sat == nil {
		t.Errorf("the revive did not set surfaced_at; SWT-45's path is untouched by this ticket")
	}
	if st.Activity != 0 {
		t.Errorf("RulesStats.Activity = %d on a closed-target pass, want 0 (criterion 11)", st.Activity)
	}

	// (b) SWT-36's dismissal shape: a dismissed task, reopened by the attach.
	dismissed := craSeedTask(t, ctx, s, "CRV-7004")
	craDismiss(t, ctx, s, dismissed)
	later := s.dbNow(t, ctx)
	s.jiraMsg(t, ctx, "CRV-7004", later, later)
	st2 := s.pass(t, ctx, "live")

	if at, _ := craActivity(t, ctx, s, dismissed); at != nil {
		t.Errorf("an attach onto a DISMISSED task set activity_at=%v; criterion 11", at)
	}
	if st2.Reopened != 1 {
		t.Errorf("RulesStats.Reopened = %d, want 1 — SWT-36's guarded reopen still fires (criterion 11)", st2.Reopened)
	}
	if st2.Activity != 0 {
		t.Errorf("RulesStats.Activity = %d, want 0 (criterion 11)", st2.Activity)
	}
}

// Criterion 8 says "live mode only". A shadow pass records what it WOULD do and
// calls no tool — the fail-safe the whole rules engine is built on.
func TestCaptureActivity_Integration_ShadowMarksNothing(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	craRequire0039(t, ctx, s.pool)

	task := craSeedTask(t, ctx, s, "CRV-7005")
	now := s.dbNow(t, ctx)
	s.jiraMsg(t, ctx, "CRV-7005", now, now)
	st := s.pass(t, ctx, capture.RulesModeShadow)

	if st.Activity != 0 {
		t.Errorf("a SHADOW pass reported Activity = %d, want 0 (criterion 8: live mode only)", st.Activity)
	}
	if at, _ := craActivity(t, ctx, s, task); at != nil {
		t.Errorf("a SHADOW pass set activity_at=%v; shadow calls no tool at all", at)
	}
	if n := s.audits(t, ctx, task, "task_mark_activity", crvActor); n != 0 {
		t.Errorf("%d task_mark_activity audit rows from a shadow pass, want 0", n)
	}
}
