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
