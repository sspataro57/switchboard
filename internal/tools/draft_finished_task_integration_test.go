//go:build integration

package tools_test

// SWT-37 criterion 28 (Codex review, medium): draft_delivery refuses finished
// work UNDER THE TASK ROW LOCK, for every caller. drafts.DeliverTasks filters on
// parent.status = 'done_locally' (Q1 = b), but that is a read taken before a
// model call; a hand close ("swb close", opsctl) landing in that window would
// otherwise still get a drafted delivery that could later be approved and sent.
// The write-time check is what closes the race, so it is what this pins.
//
// MUTATION THAT MUST TURN THIS RED: drop the `status == "closed" || status ==
// "delivered"` refusal in draftDelivery — the closed/delivered drafts are then
// written and both subtests fail on the row count.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
)

func TestDraftDelivery_Integration_RefusesFinishedTask(t *testing.T) {
	ctx := context.Background()
	s := newSlackSuite(t, ctx, false)

	// POSITIVE CONTROL: the fixture's task, as seeded, accepts a draft — so a
	// refusal below is the status check and nothing else.
	s.draft(t, ctx, "itest positive control")

	args := json.RawMessage(`{"task_id":` + itoa(s.taskID) +
		`,"channel":"slack_reply","body":"itest finished","target_ref":"` + sdsTarget + `"}`)
	for _, status := range []string{"closed", "delivered"} {
		status := status
		t.Run(status, func(t *testing.T) {
			if _, err := s.pool.Exec(ctx, `UPDATE tasks SET status=$1 WHERE id=$2`, status, s.taskID); err != nil {
				t.Fatalf("set task status %s: %v", status, err)
			}
			var before, after int
			if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE task_id=$1`, s.taskID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			_, err := s.ex.Execute(ctx, executor.Call{Tool: "draft_delivery", Actor: sdsActor, Args: args, TaskID: &s.taskID})
			if err == nil || !strings.Contains(err.Error(), "finished work") {
				t.Errorf("draft_delivery on a %s task = %v, want a refusal naming finished work", status, err)
			}
			if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE task_id=$1`, s.taskID).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Errorf("draft_delivery on a %s task wrote %d delivery row(s); a finished task must get none", status, after-before)
			}
		})
	}
}
