//go:build integration

package capture_test

// swb #610 (Salvador, 2026-09-24): "the townai project they only message me
// using upwork … those messages landing there should create tasks". Town-ai's
// rule 56 (thread_key_prefix upwork_crm:{client}:, external_system upwork_crm)
// linked every message to task #80. Whenever #80 was closed (it was reopened
// by hand on 09-13, 09-15 and 09-17), the client's messages (Erica Rapa) were
// logged onto the closed task and resurfaced to an inquiry lane town-ai never
// armed, so nothing reached the board: everything after 09-18. An Upwork
// conversation now takes SWT-78's DM path: one open task per conversation,
// always actionable, no classifier.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/capture"
)

const upwRoom = "upwork_crm:itest-dmt-client:room:room_itest1"

func (s *dmtSuite) upworkMsg(t *testing.T, ctx context.Context, key, sender, body string) int64 {
	t.Helper()
	s.seq++
	label := fmt.Sprintf("%s-upw-%d", dmtSubject, s.seq)
	raw, _ := json.Marshal(map[string]any{"kind": "communication", "sender": sender, "body": body})
	rawID := s.id(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,$3::jsonb,$4, now()) RETURNING id`, s.account, label, string(raw), "h-"+label)
	sent := s.sentClock.Add(time.Duration(s.seq) * time.Minute)
	return s.id(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1,$2,'inbound',$3,$4,$5,'',$6,'upwork') RETURNING id`,
		rawID, s.thread(t, ctx, key), label, sent, body, sender)
}

// MUTATIONS that turn this red: directFacts drops the upwork fact (the second
// message resurfaces to the inquiry lane again); directConversationKey returns
// ok=false for upwork (attribution kept, no task).
func TestUpworkClientMessagesBecomeConversationTasks(t *testing.T) {
	ctx := context.Background()
	s := newDMTSuite(t, ctx)
	s.id(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, priority, enabled, note)
		 VALUES ($1,'thread_key_prefix','upwork_crm:itest-dmt-client:','upwork_crm',50,true,'itest-dmtasks rule56')
		 RETURNING id`, s.project)

	// The first message ever makes the ticket-shaped task (#80's history).
	first := s.upworkMsg(t, ctx, upwRoom, "Erica Rapa", "hi, starting the project")
	s.pass(t, ctx, capture.RulesModeLive)
	d0, _ := s.decision(t, ctx, first, capture.RulesModeLive)
	if d0.action != "task" || d0.taskID == nil {
		t.Fatalf("setup: first Upwork message decided %+v, want the rule's task", d0)
	}
	ticket := *d0.taskID
	s.closeTask(t, ctx, ticket)

	// Weeks later: she writes again. Today this logs onto closed #80.
	m1 := s.upworkMsg(t, ctx, upwRoom, "Erica Rapa", "can you look at the login bug?")
	s.pass(t, ctx, capture.RulesModeLive)
	d1, ok := s.decision(t, ctx, m1, capture.RulesModeLive)
	if !ok || d1.action != "task" || d1.taskID == nil || *d1.taskID == ticket {
		t.Fatalf("decision %+v, want a NEW conversation task, not a log onto closed task %d", d1, ticket)
	}
	conv := s.task(t, ctx, *d1.taskID)
	if conv.assignee != "human" || conv.refs != 0 || conv.status == "closed" {
		t.Errorf("conversation task %d = %+v, want an open human task with no external ref", *d1.taskID, conv)
	}
	if d1.resurface || s.inquiryInbox(t, ctx)[m1] {
		t.Errorf("message %d went to the inquiry lane; an Upwork client message skips it", m1)
	}
	if s.count(t, ctx, `SELECT count(*) FROM tasks WHERE id=$1 AND activity_by_message_id=$2`, *d1.taskID, m1) != 1 {
		t.Errorf("the new task is not marked with its message, so it is not in INCOMING")
	}

	// Her next message joins that conversation's task.
	m2 := s.upworkMsg(t, ctx, upwRoom, "Erica Rapa", "also the export")
	s.pass(t, ctx, capture.RulesModeLive)
	if d2, _ := s.decision(t, ctx, m2, capture.RulesModeLive); d2.action != "task_log" || d2.taskID == nil || *d2.taskID != *d1.taskID {
		t.Fatalf("second message decided %+v, want task_log onto %d (one task per conversation)", d2, *d1.taskID)
	}
	if st := s.task(t, ctx, ticket).status; st != "closed" {
		t.Errorf("closed task %d was reopened (%q); the conversation task carries the messages", ticket, st)
	}
}

// upworkClosedTicket makes rule 56's shape, lets the room's first message
// create the ref task, and closes it. Returns the rule id and the task.
func (s *dmtSuite) upworkClosedTicket(t *testing.T, ctx context.Context, room string) (int64, int64) {
	t.Helper()
	rule := s.id(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, priority, enabled, note)
		 VALUES ($1,'thread_key_prefix','upwork_crm:itest-dmt-client:','upwork_crm',50,true,'itest-dmtasks rule56')
		 RETURNING id`, s.project)
	first := s.upworkMsg(t, ctx, room, "Erica Rapa", "hi")
	s.pass(t, ctx, capture.RulesModeLive)
	d0, _ := s.decision(t, ctx, first, capture.RulesModeLive)
	if d0.taskID == nil {
		t.Fatalf("setup: no ref task (%+v)", d0)
	}
	s.closeTask(t, ctx, *d0.taskID)
	return rule, *d0.taskID
}

// Review finding: the conversation lookup's rooted-thread fold is Slack's: a
// conversation key K also finds tasks on threads "K:…". An Upwork room id may
// contain ':', so a task for room "A:x" must never be found for room "A".
// MUTATION: pass exact=false for upwork -> room A's message joins A:x's task, red.
func TestUpworkRoomsNeverFoldTogether(t *testing.T) {
	ctx := context.Background()
	s := newDMTSuite(t, ctx)
	roomAx := upwRoom + ":x"
	s.upworkClosedTicket(t, ctx, roomAx)
	ax := s.upworkMsg(t, ctx, roomAx, "Erica Rapa", "in room A:x")
	s.pass(t, ctx, capture.RulesModeLive)
	dax, _ := s.decision(t, ctx, ax, capture.RulesModeLive)
	if dax.action != "task" || dax.taskID == nil {
		t.Fatalf("room A:x decided %+v, want its conversation task", dax)
	}
	// Room A: its first message makes its own ref task; closed, so the next
	// message takes the DM path and looks up conversation "A".
	first := s.upworkMsg(t, ctx, upwRoom, "Erica Rapa", "room A opens")
	s.pass(t, ctx, capture.RulesModeLive)
	d0, _ := s.decision(t, ctx, first, capture.RulesModeLive)
	if d0.taskID == nil || *d0.taskID == *dax.taskID {
		t.Fatalf("setup: room A's first message decided %+v", d0)
	}
	s.closeTask(t, ctx, *d0.taskID)
	a := s.upworkMsg(t, ctx, upwRoom, "Erica Rapa", "in room A")
	s.pass(t, ctx, capture.RulesModeLive)
	da, _ := s.decision(t, ctx, a, capture.RulesModeLive)
	if da.taskID == nil {
		t.Fatalf("room A decided %+v, want a conversation task", da)
	}
	if *da.taskID == *dax.taskID {
		t.Errorf("room A's message joined room A:x's conversation task %d", *dax.taskID)
	}
}

// Review finding: the backfill's new clause (a live task_log with
// resurface=true is a lost message too) is what ran live for task 611.
// MUTATION: drop the clause -> "skipped", red. The control (resurface=false)
// stays skipped.
func TestDirectBackfillTakesAResurfacedUpworkLog(t *testing.T) {
	ctx := context.Background()
	s := newDMTSuite(t, ctx)
	rule, ticket := s.upworkClosedTicket(t, ctx, upwRoom)
	lost := s.upworkMsg(t, ctx, upwRoom, "Erica Rapa", "a brief that never reached the board")
	kept := s.upworkMsg(t, ctx, upwRoom, "Erica Rapa", "a plain log")
	// The pre-#610 decisions, as production has them.
	for _, m := range []struct {
		id        int64
		resurface bool
	}{{lost, true}, {kept, false}} {
		s.id(t, ctx,
			`INSERT INTO capture_decisions (message_id, mode, matched_rule_id, project_id, matched_rule_ids, ambiguous,
			                                action, external_system, external_key, task_id, reason, resurface)
			 VALUES ($1,'live',$2,$3,ARRAY[$2]::bigint[],false,'task_log','upwork_crm',$4,$5,'pre-#610',$6) RETURNING id`,
			m.id, rule, s.project, upwRoom, ticket, m.resurface)
	}
	st, err := capture.DirectBackfill(ctx, s.pool, s.ex, []int64{lost, kept}, false, io.Discard)
	if err != nil {
		t.Fatalf("DirectBackfill: %v", err)
	}
	if st.Created != 1 || st.Skipped != 1 {
		t.Fatalf("backfill stats %+v, want created=1 (the resurfaced log) skipped=1 (the plain log)", st)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1 AND activity_by_message_id=$2`, s.project, lost); n != 1 {
		t.Errorf("no task carries the lost message %d", lost)
	}
}
