//go:build integration

package tools_test

// SWT-37 criteria 28 and 29 (Codex reviews): closed work never gets a draft, an
// approval or a send, and a caller that read the task's status before
// composing (the drafts worker) is refused if the work moved on.
//
// Both orderings of the draft/close race are pinned:
//   - close first → draft_delivery refuses (criterion 28);
//   - draft first → approve_delivery / send_delivery refuse once the task is
//     closed (criterion 29).
//
// `delivered` is deliberately NOT refused for every caller: R8 marks a task
// delivered after its first send, and a sibling delivery must still go out. The
// positive control at the end sends on a delivered task.
//
// MUTATIONS THAT MUST TURN THIS RED:
//   - drop the `status == "closed"` refusal in draftDelivery → RefusesClosed;
//   - drop the expect_task_status check in draftDelivery → ExpectTaskStatus;
//   - drop refuseClosedTask from approveDelivery → ApproveSendRefuseClosed/approve;
//   - drop refuseClosedTask from sendSlackReply → ApproveSendRefuseClosed/send.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/tools"
)

func setTaskStatus(t *testing.T, ctx context.Context, s slackSuite, status string) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET status=$1 WHERE id=$2`, status, s.taskID); err != nil {
		t.Fatalf("set task status %s: %v", status, err)
	}
}

func deliveryCount(t *testing.T, ctx context.Context, s slackSuite) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE task_id=$1`, s.taskID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func draftRowStatus(t *testing.T, ctx context.Context, s slackSuite, id int64) string {
	t.Helper()
	var st string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM deliveries WHERE id=$1`, id).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

func (s slackSuite) tryDraft(ctx context.Context, extra string) error {
	args := json.RawMessage(`{"task_id":` + itoa(s.taskID) +
		`,"channel":"slack_reply","body":"itest draft","target_ref":"` + sdsTarget + `"` + extra + `}`)
	_, err := s.ex.Execute(ctx, executor.Call{Tool: "draft_delivery", Actor: sdsActor, Args: args, TaskID: &s.taskID})
	return err
}

func TestDraftDelivery_Integration_RefusesClosedTask(t *testing.T) {
	ctx := context.Background()
	s := newSlackSuite(t, ctx, false)
	s.draft(t, ctx, "itest positive control") // the seeded task accepts a draft

	setTaskStatus(t, ctx, s, "closed")
	before := deliveryCount(t, ctx, s)
	if err := s.tryDraft(ctx, ""); err == nil || !strings.Contains(err.Error(), "closed work") {
		t.Errorf("draft_delivery on a closed task = %v, want a refusal naming closed work", err)
	}
	if after := deliveryCount(t, ctx, s); after != before {
		t.Errorf("draft_delivery on a closed task wrote %d row(s)", after-before)
	}
}

func TestDraftDelivery_Integration_ExpectTaskStatus(t *testing.T) {
	ctx := context.Background()
	s := newSlackSuite(t, ctx, false)
	setTaskStatus(t, ctx, s, "delivered")

	// A sibling delivery on a delivered task is legitimate for a plain caller.
	if err := s.tryDraft(ctx, ""); err != nil {
		t.Errorf("draft_delivery on a delivered task without expect_task_status = %v, want allowed "+
			"(a sibling delivery after R8's first send)", err)
	}
	// The drafts worker read done_locally before its model call; the work
	// moved on, so no draft.
	before := deliveryCount(t, ctx, s)
	err := s.tryDraft(ctx, `,"expect_task_status":"done_locally"`)
	if err == nil || !strings.Contains(err.Error(), "moved on") {
		t.Errorf("draft_delivery expecting done_locally on a delivered task = %v, want a refusal", err)
	}
	if after := deliveryCount(t, ctx, s); after != before {
		t.Errorf("a refused expect_task_status draft wrote %d row(s)", after-before)
	}
	// Positive control: the expectation holds → allowed.
	setTaskStatus(t, ctx, s, "done_locally")
	if err := s.tryDraft(ctx, `,"expect_task_status":"done_locally"`); err != nil {
		t.Errorf("draft_delivery expecting done_locally on a done_locally task = %v, want allowed", err)
	}
}

func TestApproveSend_Integration_RefuseClosedTask(t *testing.T) {
	ctx := context.Background()
	s := newSlackSuite(t, ctx, true)

	t.Run("approve", func(t *testing.T) {
		id := s.draft(t, ctx, "itest drafted while open")
		setTaskStatus(t, ctx, s, "closed")
		err := s.tryCall(ctx, "approve_delivery", id)
		if err == nil || !strings.Contains(err.Error(), "closed work") {
			t.Errorf("approve_delivery on a closed task's draft = %v, want a refusal naming closed work", err)
		}
		if st := draftRowStatus(t, ctx, s, id); st != "drafted" {
			t.Errorf("refused approval left the delivery %s, want drafted", st)
		}
		setTaskStatus(t, ctx, s, "done_locally")
	})

	t.Run("send", func(t *testing.T) {
		id := s.draft(t, ctx, "itest approved while open")
		s.call(t, ctx, "approve_delivery", id)
		setTaskStatus(t, ctx, s, "closed")
		calls := s.fake.calls
		err := s.tryCall(ctx, "send_delivery", id)
		if err == nil || !strings.Contains(err.Error(), "closed work") {
			t.Errorf("send_delivery on a closed task's approved delivery = %v, want a refusal", err)
		}
		if s.fake.calls != calls {
			t.Errorf("the Slack sender was called %d time(s) for a closed task", s.fake.calls-calls)
		}
		if st := draftRowStatus(t, ctx, s, id); st != "approved" {
			t.Errorf("refused send left the delivery %s, want approved", st)
		}
		setTaskStatus(t, ctx, s, "done_locally")
	})

	// Codex pass 3: prefill_delivery fills a REAL Slack composer, one click from
	// a send, so it runs the same guard. MUTATION: drop refuseClosedTask from
	// prefillDelivery → this subtest goes red (the drafter is called).
	t.Run("prefill", func(t *testing.T) {
		drafter := &fakeSlackDrafter{}
		tools.SetSlackDrafter(drafter)
		id := s.draft(t, ctx, "itest approved then closed before prefill")
		s.call(t, ctx, "approve_delivery", id)
		setTaskStatus(t, ctx, s, "closed")
		err := s.tryCall(ctx, "prefill_delivery", id)
		if err == nil || !strings.Contains(err.Error(), "closed work") {
			t.Errorf("prefill_delivery on a closed task's approved delivery = %v, want a refusal", err)
		}
		if drafter.calls != 0 {
			t.Errorf("the Slack composer was prefilled %d time(s) for a closed task", drafter.calls)
		}
		setTaskStatus(t, ctx, s, "done_locally")
	})

	// Codex pass 4, the other ordering: send phase 1 committed 'sending' and
	// dispatches after its transaction ends. A close landing in that gap would
	// let words reach a client for CLOSED work, so task_close refuses while a
	// delivery is in flight and succeeds once it settles. MUTATION: drop the
	// in-flight check from closeTransition → this subtest goes red.
	t.Run("close refuses an in-flight send", func(t *testing.T) {
		id := s.draft(t, ctx, "itest in flight")
		s.call(t, ctx, "approve_delivery", id)
		if _, err := s.pool.Exec(ctx, `UPDATE deliveries SET status='sending' WHERE id=$1`, id); err != nil {
			t.Fatalf("simulate send phase 1: %v", err)
		}
		_, err := s.ex.Execute(ctx, executor.Call{Tool: "task_close", Actor: sdsActor, TaskID: &s.taskID,
			Args: json.RawMessage(`{"task_id":` + itoa(s.taskID) + `,"reason":"itest close during send"}`)})
		if err == nil || !strings.Contains(err.Error(), "in flight") {
			t.Errorf("task_close with a delivery in 'sending' = %v, want a refusal naming the in-flight delivery", err)
		}
		// Codex pass 5: the Jira reconciler skips (non-fatal) only on this exact
		// phrase; any other wording would abort its whole pass.
		if err != nil && !strings.Contains(err.Error(), "refusing to close active work") {
			t.Errorf("the in-flight refusal %q lacks \"refusing to close active work\", the reconciler's "+
				"non-fatal marker (ticketstatus.activeWorkRefusal)", err)
		}
		var st string
		if err := s.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, s.taskID).Scan(&st); err != nil {
			t.Fatal(err)
		}
		if st == "closed" {
			t.Errorf("the task closed while its delivery was in flight")
		}
		// Once the delivery settles, the close goes through.
		if _, err := s.pool.Exec(ctx, `UPDATE deliveries SET status='failed' WHERE id=$1`, id); err != nil {
			t.Fatalf("settle the delivery: %v", err)
		}
		if _, err := s.ex.Execute(ctx, executor.Call{Tool: "task_close", Actor: sdsActor, TaskID: &s.taskID,
			Args: json.RawMessage(`{"task_id":` + itoa(s.taskID) + `,"reason":"itest close after settle"}`)}); err != nil {
			t.Errorf("task_close after the delivery settled = %v, want ok", err)
		}
		setTaskStatus(t, ctx, s, "done_locally")
	})

	// Codex pass 5: a phase-1 crash leaves 'sending' with no settle path for
	// gmail/Jira/calendar. Past the in-flight window the attempt is abandoned and
	// must not make the task uncloseable. MUTATION: drop the window clause from
	// the fence query → this subtest goes red.
	t.Run("close proceeds past a stale in-flight attempt", func(t *testing.T) {
		id := s.draft(t, ctx, "itest crashed send")
		s.call(t, ctx, "approve_delivery", id)
		if _, err := s.pool.Exec(ctx,
			`UPDATE deliveries SET status='sending', send_attempted_at=now()-interval '1 hour',
			        updated_at=now()-interval '1 hour' WHERE id=$1`, id); err != nil {
			t.Fatalf("simulate a crashed phase 1: %v", err)
		}
		if _, err := s.ex.Execute(ctx, executor.Call{Tool: "task_close", Actor: sdsActor, TaskID: &s.taskID,
			Args: json.RawMessage(`{"task_id":` + itoa(s.taskID) + `,"reason":"itest close past a stale attempt"}`)}); err != nil {
			t.Errorf("task_close past a 1h-old 'sending' attempt = %v, want ok (an abandoned attempt must not "+
				"block the close forever)", err)
		}
		if _, err := s.pool.Exec(ctx, `UPDATE deliveries SET status='failed' WHERE id=$1`, id); err != nil {
			t.Fatalf("settle: %v", err)
		}
		setTaskStatus(t, ctx, s, "done_locally")
	})

	// go-reviewer: the lease clock is send_attempted_at FIRST. A loop-closure
	// confirm that bumps updated_at on an old gmail attempt must not re-arm the
	// fence. MUTATION: swap the COALESCE order (or GREATEST it) → red.
	t.Run("a fresh updated_at does not re-arm an old attempt", func(t *testing.T) {
		id := s.draft(t, ctx, "itest old attempt, fresh confirm")
		s.call(t, ctx, "approve_delivery", id)
		if _, err := s.pool.Exec(ctx,
			`UPDATE deliveries SET status='sending', send_attempted_at=now()-interval '1 hour', updated_at=now()
			  WHERE id=$1`, id); err != nil {
			t.Fatalf("simulate: %v", err)
		}
		if _, err := s.ex.Execute(ctx, executor.Call{Tool: "task_close", Actor: sdsActor, TaskID: &s.taskID,
			Args: json.RawMessage(`{"task_id":` + itoa(s.taskID) + `,"reason":"itest"}`)}); err != nil {
			t.Errorf("task_close with an hour-old attempt but a fresh updated_at = %v, want ok", err)
		}
		if _, err := s.pool.Exec(ctx, `UPDATE deliveries SET status='failed' WHERE id=$1`, id); err != nil {
			t.Fatalf("settle: %v", err)
		}
		setTaskStatus(t, ctx, s, "done_locally")
	})

	// A settled Slack attempt (send_settled_at set, row left in 'sending' as
	// ambiguous) can put no new words anywhere, so it must not block a close
	// even inside the lease. MUTATION: drop `send_settled_at IS NULL` → red.
	t.Run("a settled attempt inside the lease does not block", func(t *testing.T) {
		id := s.draft(t, ctx, "itest settled ambiguous")
		s.call(t, ctx, "approve_delivery", id)
		if _, err := s.pool.Exec(ctx,
			`UPDATE deliveries SET status='sending', send_attempted_at=now(), send_settled_at=now() WHERE id=$1`,
			id); err != nil {
			t.Fatalf("simulate: %v", err)
		}
		if _, err := s.ex.Execute(ctx, executor.Call{Tool: "task_close", Actor: sdsActor, TaskID: &s.taskID,
			Args: json.RawMessage(`{"task_id":` + itoa(s.taskID) + `,"reason":"itest"}`)}); err != nil {
			t.Errorf("task_close with a settled attempt inside the lease = %v, want ok", err)
		}
		if _, err := s.pool.Exec(ctx, `UPDATE deliveries SET status='failed' WHERE id=$1`, id); err != nil {
			t.Fatalf("settle: %v", err)
		}
		setTaskStatus(t, ctx, s, "done_locally")
	})

	// POSITIVE CONTROL: a delivered task's sibling delivery still approves and
	// sends — `delivered` is not a refusal (R8 marks it after the first send).
	t.Run("delivered sibling still sends", func(t *testing.T) {
		id := s.draft(t, ctx, "itest sibling")
		setTaskStatus(t, ctx, s, "delivered")
		calls := s.fake.calls
		s.call(t, ctx, "approve_delivery", id)
		s.call(t, ctx, "send_delivery", id)
		if s.fake.calls != calls+1 {
			t.Errorf("sibling send on a delivered task called the sender %d time(s), want 1", s.fake.calls-calls)
		}
	})
}
