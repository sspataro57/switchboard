//go:build integration

package ticketstatus_test

// SWT-36 (docs/tickets/dismiss-reopen-on-activity_SPEC.md) criterion 16 and
// criterion 23's loadCandidates half, against a real database: the
// reconciler's D4 suppression reads only OPEN dismissals (D9).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run ActivityReopened ./internal/ticketstatus/
//
// Reuses store_integration_test.go's suite (newTSSuite: its fixtures, its
// cleanup pact over itest-tstatus-%, its fake Jira, its FATAL guard on
// 192.168.50.49) and adds ONE ticket key of its own. The raw message row lives
// under the suite's polled account, so tsCleanup already sweeps it.
//
// The task is dismissed through task_dismiss and activity-reopened through the
// guarded task_reopen, both on the executor — criterion 26's production shape —
// so the stamped dismissal row the pass reads was written by the real code.
//
// SWT-32 criteria 26 and 28 (an OPEN dismissal still suppresses) are unchanged
// and stay pinned by TestTicketStatus_ReopensOnlyWhatItClosedAndKeepsDismissalsDown.
//
// IMPOSED SURFACE: loadCandidates' dismissed flag becomes
// `EXISTS (SELECT 1 FROM task_dismissals d WHERE d.task_id = t.id AND d.reopened_at IS NULL)`;
// task_reopen's guarded args {task_id, dismissal_id, message_id, reason}.
//
// MUTATION: drop `AND d.reopened_at IS NULL` from loadCandidates -> the last
// pass answers suppressed_dismissed instead of reopened, and this goes red.
//
// GREENFIELD NOTE — EXPECTED RED: 0026's columns are missing, then the guarded
// reopen is ignored (validateReopen), then loadCandidates suppresses.

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/ticketstatus"
)

const (
	tsrHuman = "dashboard:itest-tstatus"
	tsrSpine = "capture:itest-tstatus"
)

func TestTicketStatus_AnActivityReopenedTaskIsOrdinaryAgain(t *testing.T) {
	ctx := context.Background()
	s := newTSSuite(t, ctx)

	var cols int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name='task_dismissals' AND column_name IN ('closed_from_status','reopened_at')`).Scan(&cols); err != nil || cols != 2 {
		t.Fatalf("task_dismissals lacks migration 0026's columns (n=%d, err=%v)", cols, err)
	}

	call := func(tool, actor string, taskID int64, args string) {
		t.Helper()
		if _, err := s.ex.Execute(ctx, executor.Call{Tool: tool, Actor: actor, Args: json.RawMessage(args), TaskID: &taskID}); err != nil {
			t.Fatalf("%s(%s) as %s: %v", tool, args, actor, err)
		}
	}

	const key = "ITS-ACTREOPEN"
	s.storeRaw(t, ctx, tsPolledAcct, tsIssue{key, "indeterminate", tsOwnID, true}, 0)
	task := s.insID(t, ctx,
		`INSERT INTO tasks (project_id, title, status) VALUES ($1,$2,'ready') RETURNING id`, s.plain, "itest-tstatus "+key)
	s.tasks[key] = task
	s.refs[key] = s.insID(t, ctx,
		`INSERT INTO external_refs (task_id, system, external_key) VALUES ($1,'jira',$2) RETURNING id`, task, key)
	id := strconv.FormatInt(task, 10)

	// Salvador dismisses it on the board.
	call("task_dismiss", tsrHuman, task, `{"task_id":`+id+`,"reason_code":"handled_elsewhere"}`)
	var dismissal int64
	var dismissedAt time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT id, created_at FROM task_dismissals WHERE task_id=$1 ORDER BY id DESC LIMIT 1`, task).
		Scan(&dismissal, &dismissedAt); err != nil {
		t.Fatalf("read the dismissal: %v", err)
	}

	// A new comment on the ticket is ingested after it, and capture's guarded
	// reopen brings the task back.
	raw := s.insID(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1,'itest-tsreopen-comment-1','{}','itest-tsreopen-h-1') RETURNING id`, s.accounts[tsPolledAcct])
	msg := s.insID(t, ctx,
		`INSERT INTO normalized_messages (raw_source_item_id, direction, sent_at, body_text, sender, channel, created_at)
		 VALUES ($1,'inbound',$2,'a follow-up comment','Marco','jira',$2) RETURNING id`, raw, dismissedAt.Add(time.Minute))
	call("task_reopen", tsrSpine, task, `{"task_id":`+id+`,"dismissal_id":`+strconv.FormatInt(dismissal, 10)+
		`,"message_id":`+strconv.FormatInt(msg, 10)+`,"reason":"itest: new comment on the ticket"}`)
	if got := s.status(t, ctx, key); got != "ready" {
		t.Fatalf("setup: the guarded reopen left the task %q, want ready (the tools-level criteria are red first)", got)
	}

	// D9: the ticket is Done, so the reconciler closes it in the same tick.
	s.storeRaw(t, ctx, tsPolledAcct, tsIssue{key, "done", tsOwnID, true}, 0)
	s.run(t, ctx, ticketstatus.Config{})
	if got := s.status(t, ctx, key); got != "closed" {
		t.Fatalf("after the ticket went Done the task is %q, want closed — 'the ticket's current state outranks "+
			"a comment' (D9)", got)
	}
	if st := s.state(t, ctx, key); st.lastAction != "closed" {
		t.Fatalf("state.last_action = %q, want closed: this close must be the reconciler's OWN", st.lastAction)
	}

	// The ticket is warranted again: that close was the reconciler's own, so it
	// reopens — the old, stamped dismissal must no longer suppress it.
	s.storeRaw(t, ctx, tsPolledAcct, tsIssue{key, "indeterminate", tsOwnID, true}, 0)
	s.run(t, ctx, ticketstatus.Config{})

	st := s.state(t, ctx, key)
	if st.lastAction != "reopened" {
		t.Errorf("state.last_action = %q, want \"reopened\". Criterion 16: a task that was activity-reopened, "+
			"then closed by the pass, is ORDINARY — loadCandidates must read only OPEN dismissals "+
			"(reopened_at IS NULL). suppressed_dismissed here means an overtaken label still outranks the ticket", st.lastAction)
	}
	if got := s.status(t, ctx, key); got != "ready" {
		t.Errorf("the task is %q, want ready (restored by the reconciler from its own closed_from_status)", got)
	}
}
