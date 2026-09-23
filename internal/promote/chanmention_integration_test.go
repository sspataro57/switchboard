//go:build integration

package promote_test

// TestSWT79_PromoteInquiry_* — slack-channel-mentions (Jira SWT-79,
// docs/tickets/slack-channel-mentions_SPEC.md) criterion 12, decision D4,
// through promote.Run on the real SQL (reuses inquiry_integration_test.go's
// iqpSuite: its armed project, its verdict writer, its cleanup).
//
//   - an armed project, a TOP-LEVEL channel verdict (ask_kind=question,
//     needs_reply=true, thread_scope=conversation), body "@Salvador can you
//     confirm the date?" → ONE `ready` human task;
//   - the same without the mention, on a LEGACY capture row (channel_unmentioned
//     defaults false, so the inbox admits it) → gated `not_addressed`;
//   - a lookalike "@Salvadora" → gated `not_addressed` (the ONE spelling,
//     slackweb.MentionsOwner, decides — not a substring);
//   - the body is used for the predicate ONLY: it never appears in the task's
//     title, body, events, or any log line the pass writes (C-D9: every value is
//     a copy of a stored verdict field).
//
// "TEST THE COLUMN": the mention is read from normalized_messages.body_text,
// written here as the column value; nothing hands the gate a Mentioned flag.
// MUTATION (V3): select a constant instead of nm.body_text in inquiryInbox →
// the mentioned verdict is gated not_addressed and this goes red.
//
// EXPECTED RED TODAY: addressed() has no mention clause, so the mentioned
// top-level channel verdict is gated not_addressed (C-D3's accepted recall
// cost, which D4 retires). Compiles today: every entry point exists.

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/promote"
)

// A marker only the message BODY carries; the verdict's ask/reason never do.
const chmBodyMarker = "zq-swt79-bodymarker"

func TestSWT79_PromoteInquiry_ATopLevelChannelMentionBecomesOneReadyTask(t *testing.T) {
	ctx := context.Background()
	s := newIQPSuite(t, ctx)
	ask := s.ago(2 * time.Hour)

	channelAsk := func(label, conv, body string) int64 {
		t.Helper()
		m, r := s.message(t, ctx, iqpMsg{label: label, key: slackKey(conv, ""), channel: "slack", sentAt: ask,
			sender: "Esteban", subject: "#general"})
		s.exec(t, ctx, `UPDATE normalized_messages SET body_text=$2 WHERE id=$1`, m, body)
		// A LEGACY live row: channel_unmentioned is not named, so it records the
		// default (false = eligible), exactly what a pre-deploy or old-binary row holds.
		s.decision(t, ctx, m, "live", "attributed", s.armed)
		s.verdict(t, ctx, m, r, iqpV{scope: "conversation", asker: "Esteban", ask: "confirm the rollout date",
			reason: "asks the recipient directly"})
		return m
	}
	mentioned := channelAsk("chm-mention", "C0IQPCHM1", "@Salvador can you confirm the date? "+chmBodyMarker)
	unmentioned := channelAsk("chm-plain", "C0IQPCHM2", "can you confirm the date? "+chmBodyMarker)
	lookalike := channelAsk("chm-lookalike", "C0IQPCHM3", "@Salvadora can you confirm the date? "+chmBodyMarker)

	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	st := s.run(t, ctx, promote.Config{})
	slog.SetDefault(old)

	p, ok := s.promotion(t, ctx, mentioned)
	if !ok || p.action != "task" || p.taskID == nil {
		t.Fatalf("the top-level channel verdict whose body mentions @Salvador was not promoted (found=%v %+v; "+
			"Gated=%v). D4: a Slack channel mention is ADDRESSED — without it the feature does nothing", ok, p, st.Gated)
	}
	var (
		title, body, status, assignee string
		events                        string
	)
	if err := s.pool.QueryRow(ctx, `SELECT title, COALESCE(body,''), status, assignee_type,
	                                       COALESCE((SELECT string_agg(payload::text, ' ') FROM task_events WHERE task_id=t.id),'')
	                                  FROM tasks t WHERE id=$1 AND project_id=$2`, *p.taskID, s.armed).
		Scan(&title, &body, &status, &assignee, &events); err != nil {
		t.Fatalf("read task %d: %v", *p.taskID, err)
	}
	if status != "ready" || assignee != "human" {
		t.Errorf("task %d is status=%s assignee=%s, want a ready human task", *p.taskID, status, assignee)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM classify_promotions WHERE normalized_message_id=$1`, mentioned); n != 1 {
		t.Errorf("%d promotion rows for the mentioned message, want ONE", n)
	}
	for where, text := range map[string]string{"title": title, "body": body, "task_events": events, "the pass's logs": logs.String()} {
		if strings.Contains(text, chmBodyMarker) || strings.Contains(text, "@Salvador") {
			t.Errorf("the message body leaked into the task's %s: %q. D4: nm.body_text is read ONLY for the mention "+
				"predicate (C-D9: every value is a copy of a stored verdict field)", where, text)
		}
	}

	for name, m := range map[string]int64{
		"the unmentioned top-level channel verdict (legacy false capture row)": unmentioned,
		"the @Salvadora lookalike": lookalike,
	} {
		if p, ok := s.promotion(t, ctx, m); ok {
			t.Errorf("%s was promoted (%+v); want gated not_addressed (unchanged)", name, p)
		}
	}
	if st.Gated["not_addressed"] != 2 {
		t.Errorf("Gated[not_addressed] = %d, want 2 (unmentioned + lookalike; the mention is addressed); Gated = %v",
			st.Gated["not_addressed"], st.Gated)
	}
}
