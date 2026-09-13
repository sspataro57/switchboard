//go:build integration

package tools_test

// SWT-43 review fixes against the compose db, reusing rjFixture
// (reject_delivery_integration_test.go) and its cleanup pact:
//
//   - fix 1 (go-reviewer): the D4 hole. A failed jira_comment approved (the
//     retry path) and then rejected slipped past D4, which keyed on
//     status='failed'. sendJiraComment writes `error` on every failure and
//     approve does not clear it, so an approved jira_comment with an error is
//     the same may-have-landed row.
//   - fix 2 (Codex): the Redo race. Two drafts workers reading one Redo could
//     each insert a draft. draft_delivery on the drafts-worker path
//     (expect_task_status) now re-checks, under the task lock, that no blocking
//     delivery (tools.BlockingDeliverySQL) exists.
//   - fix 5 (go-reviewer): Deny/Redo bound to the words shown, SWT-44's
//     expect_content_hash, compared under the delivery row lock.
//   - fix 7: the D7 refusal no longer says "Deny it instead" to an already
//     denied row.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run 'ApprovedJira|DraftsPath|ConcurrentRedo|RejectDelivery_Integration_ContentBound|RedoRefusalWording' ./internal/tools/
//
// MUTATIONS (run by hand; each turns the named test red):
//   - drop the approved+error jira_comment clause in rejectDelivery →
//     ApprovedJiraAfterAFailedSendRefuses at "succeeded";
//   - drop the refuseBlockingDelivery call from draftDelivery →
//     ConcurrentRedoDraftsOnce (2 rows) and DraftsPathRefusesBesideABlockingDelivery;
//   - drop the expect_content_hash comparison in rejectDelivery →
//     RejectDelivery_Integration_ContentBound at "REJECTED";
//   - make the D7 refusal say "Deny it instead" unconditionally →
//     RedoRefusalWordingFitsTheRow.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/tools"
)

// ---- fix 1: the D4 hole ------------------------------------------------------

func TestRejectDelivery_Integration_ApprovedJiraAfterAFailedSendRefuses(t *testing.T) {
	ctx := context.Background()
	f := newRJFixture(t, ctx)

	// What sendJiraComment leaves after ANY send error (delivery.go, both
	// failure writes): failed, NULL sent_external_id, error set.
	id := f.row(t, ctx, rjSpec{channel: "jira_comment", status: "failed", attempted: true})
	if _, err := f.pool.Exec(ctx,
		`UPDATE deliveries SET error='jira send: context deadline exceeded' WHERE id=$1`, id); err != nil {
		t.Fatalf("seed the failed send's error: %v", err)
	}
	// approve_delivery accepts failed-without-id: the retry path.
	if _, err := f.ex.Execute(ctx, executor.Call{Tool: "approve_delivery", Actor: rjActor,
		Args: []byte(`{"delivery_id":` + itoa(id) + `}`)}); err != nil {
		t.Fatalf("approve the failed jira_comment (the retry path): %v", err)
	}
	var status string
	var errText *string
	if err := f.pool.QueryRow(ctx, `SELECT status, error FROM deliveries WHERE id=$1`, id).Scan(&status, &errText); err != nil {
		t.Fatalf("read delivery %d: %v", id, err)
	}
	if status != "approved" || errText == nil {
		t.Fatalf("after approve: status=%q error=%v, want approved with the send error KEPT — approve does not "+
			"clear it, and that surviving error is the evidence the refusal keys on", status, errText)
	}

	before := f.fingerprint(t, ctx, id)
	for _, redraft := range []bool{false, true} {
		_, err := f.reject(ctx, rjActor, id, redraft, rjStr("should not land"))
		if err == nil {
			t.Fatalf("reject_delivery (redraft=%v) on an approved jira_comment whose earlier send failed succeeded. "+
				"D4: the failed send may have landed, so 'switchboard did not send this' could be false", redraft)
		}
		for _, want := range []string{itoa(id), "approved", "jira_comment", "may have been sent", "D4"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal %q does not name %q (same D4 wording as the failed row)", err, want)
			}
		}
	}
	if after := f.fingerprint(t, ctx, id); after != before {
		t.Errorf("the refused reject changed the row")
	}
	if n := f.approvals(t, ctx, id); n != 0 {
		t.Errorf("a refused reject wrote %d rejected approvals row(s)", n)
	}
	if n := len(f.events(t, ctx, id)); n != 0 {
		t.Errorf("a refused reject wrote %d delivery_rejected event(s)", n)
	}

	// Control: an approved jira_comment that never failed carries no error and
	// stays rejectable. The refusal keys on the failure evidence, not on the
	// channel.
	ctl := f.row(t, ctx, rjSpec{channel: "jira_comment", status: "approved", approvalSource: "switchboard"})
	if _, err := f.reject(ctx, rjActor, ctl, false, rjStr("not needed")); err != nil {
		t.Fatalf("reject_delivery on an approved jira_comment with no failed send: %v", err)
	}
	if st := f.state(t, ctx, ctl); st.status != "rejected" {
		t.Errorf("control row status = %q, want rejected", st.status)
	}
}

// ---- fix 2: the Redo race ----------------------------------------------------

func rvDraftArgs(f *rjFixture, taskID int64, expect bool) []byte {
	s := `{"task_id":` + itoa(taskID) + `,"channel":"gmail","thread_id":` + itoa(f.threadID) +
		`,"subject":"Re: login broken","body":"itest-deny redraft body"`
	if expect {
		s += `,"expect_task_status":"done_locally"`
	}
	return []byte(s + `}`)
}

func rvDraft(ctx context.Context, f *rjFixture, taskID int64, expect bool) error {
	_, err := f.ex.Execute(ctx, executor.Call{Tool: "draft_delivery", Actor: "drafts:gpt",
		Args: rvDraftArgs(f, taskID, expect), TaskID: &taskID})
	return err
}

func rvCount(t *testing.T, ctx context.Context, f *rjFixture, taskID int64, status string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM deliveries WHERE task_id=$1 AND status=$2`, taskID, status).Scan(&n); err != nil {
		t.Fatalf("count %s deliveries: %v", status, err)
	}
	return n
}

// Two drafts workers, one Redo. Both read the Deliver task from DeliverTasks
// (the Redo row does not block), both call the model, both call draft_delivery.
// The test HOLDS the task row so both calls are waiting on draftDelivery's task
// lock before either inserts: the window a sequential test cannot reach.
func TestDraftDelivery_Integration_ConcurrentRedoDraftsOnce(t *testing.T) {
	ctx := context.Background()
	f := newRJFixture(t, ctx)
	f.row(t, ctx, rjSpec{channel: "gmail", status: "rejected", note: rjStr("shorter"), redraft: true})

	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	// FOR NO KEY UPDATE conflicts with draftDelivery's FOR UPDATE, but not with
	// the FOR KEY SHARE the executor's audit insert (task_id FK) takes, so both
	// calls get past the audit start and queue on the handler's own lock.
	if _, err := holder.Exec(ctx, `SELECT 1 FROM tasks WHERE id=$1 FOR NO KEY UPDATE`, f.taskID); err != nil {
		t.Fatalf("hold the task row: %v", err)
	}

	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { errs <- rvDraft(ctx, f, f.taskID, true) }()
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		var waiting int
		if err := f.pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity
			  WHERE pid <> pg_backend_pid() AND wait_event_type = 'Lock'
			    AND query LIKE '%FROM tasks WHERE id=$1 FOR UPDATE%'`).Scan(&waiting); err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if waiting >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d draft_delivery call(s) reached the task lock in 20s", waiting)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("release the task row: %v", err)
	}

	var ok, refused int
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			switch {
			case err == nil:
				ok++
			case errors.Is(err, tools.ErrDeliveryBlocksDraft):
				refused++
			default:
				t.Errorf("draft_delivery failed with %v, want success or tools.ErrDeliveryBlocksDraft", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("a draft_delivery call did not return within 20s of releasing the task row")
		}
	}
	if n := rvCount(t, ctx, f, f.taskID, "drafted"); n != 1 {
		t.Errorf("two concurrent drafts for one Redo wrote %d drafted row(s), want exactly 1: the Redo bound is one "+
			"human click per re-draft", n)
	}
	if ok != 1 || refused != 1 {
		t.Errorf("outcomes: %d ok, %d refused with ErrDeliveryBlocksDraft; want 1 and 1", ok, refused)
	}
}

// The same re-check, sequentially: it is the drafts path's rule, not only the
// Redo's. A plain caller (no expect_task_status) keeps drafting siblings.
func TestDraftDelivery_Integration_DraftsPathRefusesBesideABlockingDelivery(t *testing.T) {
	ctx := context.Background()
	f := newRJFixture(t, ctx)

	for _, tc := range []struct {
		name    string
		seed    *rjSpec
		wantErr bool
	}{
		{"no delivery yet: the first draft", nil, false},
		{"a drafted row blocks", &rjSpec{channel: "gmail", status: "drafted"}, true},
		{"an approved row blocks", &rjSpec{channel: "gmail", status: "approved", approvalSource: "switchboard"}, true},
		{"a plain Deny blocks", &rjSpec{channel: "gmail", status: "rejected", note: rjStr("no")}, true},
		{"a Redo row alone does not block", &rjSpec{channel: "gmail", status: "rejected", note: rjStr("redo"), redraft: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			taskID := f.task(t, ctx, "done_locally")
			if tc.seed != nil {
				s := *tc.seed
				s.taskID = taskID
				f.row(t, ctx, s)
			}
			before := rvCount(t, ctx, f, taskID, "drafted")
			err := rvDraft(ctx, f, taskID, true)
			if tc.wantErr {
				if !errors.Is(err, tools.ErrDeliveryBlocksDraft) {
					t.Fatalf("drafts-path draft_delivery = %v, want tools.ErrDeliveryBlocksDraft", err)
				}
				if !strings.Contains(err.Error(), itoa(taskID)) {
					t.Errorf("refusal %q does not name task %d", err, taskID)
				}
				if after := rvCount(t, ctx, f, taskID, "drafted"); after != before {
					t.Errorf("a refused draft wrote %d row(s)", after-before)
				}
				return
			}
			if err != nil {
				t.Fatalf("drafts-path draft_delivery = %v, want allowed", err)
			}
			// The new draft blocks again: a second drafts-path call is refused.
			if err := rvDraft(ctx, f, taskID, true); !errors.Is(err, tools.ErrDeliveryBlocksDraft) {
				t.Errorf("second drafts-path draft_delivery = %v, want ErrDeliveryBlocksDraft (the new draft blocks)", err)
			}
			// A plain caller (a session, the full profile) still drafts a sibling.
			if err := rvDraft(ctx, f, taskID, false); err != nil {
				t.Errorf("draft_delivery without expect_task_status beside a draft = %v, want allowed (siblings)", err)
			}
		})
	}
}

// ---- fix 5: Deny / Redo bound to the words shown -----------------------------

func TestRejectDelivery_Integration_ContentBound(t *testing.T) {
	ctx := context.Background()
	f := newRJFixture(t, ctx)

	const subject = "itest-deny subject" // rjFixture.row's subject
	id := f.row(t, ctx, rjSpec{channel: "gmail", status: "drafted", body: "itest-deny bound body"})
	shown := tools.DeliveryContentHash(subject, "itest-deny bound body")

	// An edit lands after the page rendered (a session's update_delivery).
	if _, err := f.ex.Execute(ctx, executor.Call{Tool: "update_delivery", Actor: rjActor,
		Args: []byte(`{"delivery_id":` + itoa(id) + `,"body":"planted words"}`)}); err != nil {
		t.Fatalf("update_delivery: %v", err)
	}
	current := tools.DeliveryContentHash(subject, "planted words")

	before := f.fingerprint(t, ctx, id)
	_, err := f.rejectRaw(ctx, rjActor, map[string]any{
		"delivery_id": id, "redraft": true, "note": "shorter", "expect_content_hash": shown})
	if err == nil {
		t.Fatalf("reject_delivery with the hash of the words Salvador SAW was REJECTED after the row changed; " +
			"his verdict (and the note fed to the redraft) must be about what he was shown")
	}
	for _, want := range []string{itoa(id), "changed since it was shown to you", "reload"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("stale reject refused with %q, want it to name %q", err, want)
		}
	}
	if after := f.fingerprint(t, ctx, id); after != before {
		t.Errorf("the refused stale reject changed the row")
	}
	if n := f.approvals(t, ctx, id); n != 0 {
		t.Errorf("a refused stale reject wrote %d approvals row(s)", n)
	}
	if n := len(f.events(t, ctx, id)); n != 0 {
		t.Errorf("a refused stale reject wrote %d delivery_rejected event(s)", n)
	}

	// The hash of the CURRENT words rejects.
	res, err := f.rejectRaw(ctx, rjActor, map[string]any{"delivery_id": id, "expect_content_hash": current})
	if err != nil {
		t.Fatalf("reject_delivery with the current hash: %v", err)
	}
	if !res.Changed || f.state(t, ctx, id).status != "rejected" {
		t.Errorf("reject with the current hash: result %+v, status %q; want a real transition to rejected",
			res, f.state(t, ctx, id).status)
	}

	// The D6 upgrade is bound the same way.
	if _, err := f.rejectRaw(ctx, rjActor, map[string]any{
		"delivery_id": id, "redraft": true, "expect_content_hash": shown}); err == nil ||
		!strings.Contains(err.Error(), "changed since it was shown to you") {
		t.Errorf("a Deny->Redo upgrade with a stale hash = %v, want the changed-since-shown refusal", err)
	}
	if f.state(t, ctx, id).redraft {
		t.Errorf("the refused stale upgrade set redraft_requested_at")
	}
	if _, err := f.rejectRaw(ctx, rjActor, map[string]any{
		"delivery_id": id, "redraft": true, "expect_content_hash": current}); err != nil {
		t.Errorf("a Deny->Redo upgrade with the current hash: %v", err)
	}

	// A malformed hash is refused by the validator, not as "changed".
	id2 := f.row(t, ctx, rjSpec{channel: "gmail", status: "drafted"})
	if _, err := f.rejectRaw(ctx, rjActor, map[string]any{"delivery_id": id2, "expect_content_hash": "abc"}); err == nil ||
		!strings.Contains(err.Error(), "expect_content_hash") {
		t.Errorf("reject with a malformed hash = %v, want a validation refusal naming expect_content_hash", err)
	}
	if st := f.state(t, ctx, id2); st.status != "drafted" {
		t.Errorf("a malformed-hash reject left status %q, want drafted", st.status)
	}
}

// ---- fix 7: the D7 wording ---------------------------------------------------

// "Deny it instead" is the right advice for a drafted row on work that moved
// on, and wrong for a row that is already denied: there it stays denied.
func TestRejectDelivery_Integration_RedoRefusalWordingFitsTheRow(t *testing.T) {
	ctx := context.Background()
	f := newRJFixture(t, ctx)

	for _, status := range []string{"delivered", "closed"} {
		t.Run(status, func(t *testing.T) {
			taskID := f.task(t, ctx, status)

			drafted := f.row(t, ctx, rjSpec{taskID: taskID, channel: "gmail", status: "drafted"})
			_, err := f.reject(ctx, rjActor, drafted, true, rjStr("redo"))
			if err == nil || !strings.Contains(err.Error(), "done_locally") || !strings.Contains(err.Error(), "Deny it instead") {
				t.Errorf("Redo on a drafted row of %s work = %v, want the done_locally refusal advising Deny", status, err)
			}

			denied := f.row(t, ctx, rjSpec{taskID: taskID, channel: "gmail", status: "rejected", note: rjStr("stale")})
			_, err = f.reject(ctx, rjActor, denied, true, rjStr("redo"))
			if err == nil {
				t.Fatalf("the Deny->Redo upgrade on %s work succeeded; D7 refuses it", status)
			}
			msg := err.Error()
			if strings.Contains(msg, "Deny it instead") {
				t.Errorf("refusal %q tells him to Deny a row that is already denied", msg)
			}
			if !strings.Contains(msg, "done_locally") || !strings.Contains(msg, "already denied") {
				t.Errorf("refusal %q should say why (done_locally) and that the row is already denied", msg)
			}
		})
	}
}
