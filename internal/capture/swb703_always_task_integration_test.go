//go:build integration

package capture_test

// swb 703 / SWT-93 always-task: a keyless capture rule with always_task makes
// every message it attributes a task on its thread's open task, in INCOMING,
// before any classifier (docs/tickets/always-task_SPEC.md). Reuses the SWT-78
// direct suite (dmt): its project, account and cleanup pact.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run AlwaysTask ./internal/capture/

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/capture"
)

func (s *dmtSuite) mail(t *testing.T, ctx context.Context, key, sender, subject, body string) int64 {
	t.Helper()
	s.seq++
	label := fmt.Sprintf("%s-mail-%d", dmtSubject, s.seq)
	rawID := s.id(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,'{}',$3, now()) RETURNING id`, s.account, label, "h-"+label)
	return s.id(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1,$2,'inbound',$3,$4,$5,$6,$7,'gmail') RETURNING id`,
		rawID, s.thread(t, ctx, key), "<"+label+"@mail.test>", s.sentClock.Add(time.Duration(s.seq)*time.Minute),
		body, subject, sender)
}

func TestAlwaysTask_Integration_MailBecomesAnIncomingTaskPerThread(t *testing.T) {
	ctx := context.Background()
	s := newDMTSuite(t, ctx)
	rule := s.id(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, priority, enabled, note, always_task)
		 VALUES ($1,'sender','@pines.example',NULL,6,true,'itest-dmtasks always_task',true) RETURNING id`, s.project)
	s.id(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, priority, enabled, note)
		 VALUES ($1,'sender','@plain.example',NULL,6,true,'itest-dmtasks plain keyless') RETURNING id`, s.project)

	const thread = "gmail:itest-dmtasks@example.test:thr-pines-1"
	m1 := s.mail(t, ctx, thread, "Pines Property Management Inc <support@pines.example>", "[#XN1] Notice",
		"Your annual meeting is on Oct 9.")
	plain := s.mail(t, ctx, "gmail:itest-dmtasks@example.test:thr-plain", "Plain <news@plain.example>", "hello",
		"nothing to do")
	s.pass(t, ctx, capture.RulesModeLive)

	task := s.created(t, ctx, m1, "an always_task rule's mail is always a task")
	got := s.task(t, ctx, task)
	if got.assignee != "human" || got.status != "ready" || got.refs != 0 || got.sourceThread == nil {
		t.Errorf("task %d = %+v, want a ready human task with its source thread and no external ref", task, got)
	}
	if got.activityBy == nil || *got.activityBy != m1 {
		t.Errorf("task %d activity_by_message_id = %v, want %d: the activity mark puts it in INCOMING", task,
			got.activityBy, m1)
	}
	var body string
	if err := s.pool.QueryRow(ctx, `SELECT body FROM tasks WHERE id=$1`, task).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "always actionable (swb 703)") || !strings.Contains(body, fmt.Sprintf("capture rule %d", rule)) {
		t.Errorf("task body does not carry the always-task marker and its rule:\n%s", body)
	}
	if d, ok := s.decision(t, ctx, plain, capture.RulesModeLive); !ok || d.action != "attributed" {
		t.Errorf("an ordinary keyless rule's mail = %v (found=%v), want attributed only", d, ok)
	}

	// A second message on the same thread attaches; a new thread makes a new task.
	m2 := s.mail(t, ctx, thread, "Pines Property Management Inc <support@pines.example>", "Re: [#XN1] Notice",
		"Reminder: annual meeting.")
	m3 := s.mail(t, ctx, "gmail:itest-dmtasks@example.test:thr-pines-2",
		"Pines Property Management Inc <support@pines.example>", "[#XN2] Violation", "Please trim the hedge.")
	s.pass(t, ctx, capture.RulesModeLive)
	s.attached(t, ctx, m2, task, "the same thread's open task")
	if other := s.created(t, ctx, m3, "a new thread is a new notice"); other == task {
		t.Errorf("a second thread attached to task %d; want its own task", task)
	}
}

func TestAlwaysTask_Integration_TheColumnRefusesAKeyedRule(t *testing.T) {
	ctx := context.Background()
	s := newDMTSuite(t, ctx)
	_, err := s.pool.Exec(ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, priority, enabled, always_task)
		 VALUES ($1,'body_regex','PINES-[0-9]+','jira','(PINES-[0-9]+)',5,true,true)`, s.project)
	if err == nil || !strings.Contains(err.Error(), "capture_rules_always_task_keyless") {
		t.Errorf("a keyed always_task rule inserted (err=%v); want the keyless CHECK to refuse it", err)
	}
}

// D4's recovery path: a mail already decided `attributed` (the live decision is
// forever) is put on a task by DirectBackfill once an always_task rule matches
// it, and a rerun finds it done.
func TestAlwaysTask_Integration_BackfillPutsAnAttributedMailOnATask(t *testing.T) {
	ctx := context.Background()
	s := newDMTSuite(t, ctx)
	old := s.id(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, priority, enabled, note)
		 VALUES ($1,'sender','pines.example',NULL,5,true,'itest-dmtasks rule19 shape') RETURNING id`, s.project)
	m := s.mail(t, ctx, "gmail:itest-dmtasks@example.test:thr-pines-bf", "Pines <support@pines.example>",
		"[#XN3] Message", "Message from your association.")
	s.pass(t, ctx, capture.RulesModeLive)
	if d, ok := s.decision(t, ctx, m, capture.RulesModeLive); !ok || d.action != "attributed" {
		t.Fatalf("setup: decision %v (found=%v), want attributed", d, ok)
	}
	s.id(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, priority, enabled, note, always_task)
		 VALUES ($1,'sender','@pines.example',NULL,6,true,'itest-dmtasks always_task',true) RETURNING id`, s.project)
	if _, err := s.pool.Exec(ctx, `UPDATE capture_rules SET enabled=false WHERE id=$1`, old); err != nil {
		t.Fatal(err)
	}

	st, err := capture.DirectBackfill(ctx, s.pool, s.ex, []int64{m}, false, io.Discard)
	if err != nil {
		t.Fatalf("DirectBackfill: %v", err)
	}
	if st.Created != 1 {
		t.Fatalf("backfill stats %+v, want Created 1", st)
	}
	n := s.count(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1 AND body LIKE '%always actionable (swb 703)%'
	                       AND activity_by_message_id=$2`, s.project, m)
	if n != 1 {
		t.Errorf("%d always-task tasks marked by message %d, want 1", n, m)
	}
	st, err = capture.DirectBackfill(ctx, s.pool, s.ex, []int64{m}, false, io.Discard)
	if err != nil || st.AlreadyDone != 1 || st.Created != 0 {
		t.Errorf("rerun: %+v, %v; want AlreadyDone 1 and nothing created", st, err)
	}
}
