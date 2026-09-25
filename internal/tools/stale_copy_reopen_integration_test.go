//go:build integration

package tools_test

// SWT-91 stale-copy-reopens (docs/bugs/stale-copy-reopens.md): a notifier's
// late COPY of an event from before the put-down must not bring a dismissed or
// closed task back. The caller (capture, the one reader of
// projects.notifier_senders) flags the message with notifier_copy; the verb then
// applies two guards from COLUMNS, after D2's ingest clock:
//
//  1. a slack copy never reopens (the Jira app's DM repeats what the Jira
//     connector carries) — skipped: slack_notifier_copy;
//  2. any other copy SENT more than reopenSendWindow (20 min) before the
//     put-down skips — message_sent_before_dismissal / _close. A NULL sent_at
//     leaves the ingest clock alone.
//
// An unflagged message (a person's) keeps D2 exactly: Salvador, 2026-09-25,
// "only bot copies" — Slack's p50 ingest lag is ~30 min, so a window on people
// would swallow real late replies.
//
// Evidence (prod, 2026-09-25): slack 582535 "Katie transitioned WEB-10444 →
// TT-Verified", sent 17:15, ingested 18:13, reopened task 377 dismissed 17:30;
// its twin 583650 from the second workspace reopened it again at 18:43.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run StaleCopy ./internal/tools/
//
// Reuses the dismissal (dro) and revive (rv) suites' fixtures and cleanup pacts.

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// slackMessage seeds a slack message on thread with the given sender. The
// raw row keeps invariant 1's shape.
func (s *droSuite) slackMessage(t *testing.T, ctx context.Context, label string, thread int64,
	sender string, createdAt, sentAt time.Time) int64 {
	t.Helper()
	raw := s.insID(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,'{}',$3, now()) RETURNING id`,
		s.account, "itest-dreopen-"+label, "itest-dreopen-h-"+label)
	return s.insID(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		    body_text, subject, sender, channel, created_at)
		 VALUES ($1,$2,'inbound',$3,$4,'Katie Evans transitioned DRO-1','Jira',$5,'slack',$6) RETURNING id`,
		raw, thread, "slack:itest-dreopen:"+label, sentAt, sender, createdAt)
}

// guardedCopy is capture's call for a flagged notifier copy: ids plus the flag.
func (s *droSuite) guardedCopy(t *testing.T, ctx context.Context, taskID, dismissalID, messageID int64) map[string]any {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"task_id": taskID, "dismissal_id": dismissalID, "message_id": messageID,
		"notifier_copy": true, "reason": "itest-dreopen: notifier copy"})
	return s.call(t, ctx, "task_reopen", droSpine, taskID, string(b))
}

// MUTATIONS: drop the sent_at clause -> (a) reopens; shrink the window below
// 19 min -> (b) skips; apply the window without the flag -> (d) skips.
func TestStaleCopy_ANotifierCopySentLongBeforeTheDismissalSkips(t *testing.T) {
	ctx := context.Background()
	s := newDroSuite(t, ctx)

	t.Run("(a) a copy sent 30 min before the dismissal, ingested after: skips", func(t *testing.T) {
		c := s.seedDismissed(t, ctx, "stale", "ready", "handled_elsewhere")
		m := s.message(t, ctx, "stale-in1", c.thread, "inbound",
			c.dismissedAt.Add(time.Minute), c.dismissedAt.Add(-30*time.Minute))
		droWantSkipped(t, "a copy sent 30 min before the dismissal", s.guardedCopy(t, ctx, c.task, c.dismissal, m),
			"message_sent_before_dismissal")
		if got := s.status(t, ctx, c.task); got != "closed" {
			t.Errorf("task after a stale copy = %q, want closed", got)
		}
		if rows := s.dismissals(t, ctx, c.task); len(rows) != 1 || rows[0].reopenedAt != nil {
			t.Errorf("a stale copy stamped the dismissal: %+v", rows)
		}
	})

	t.Run("(b) a copy sent 19 min before, ingested after: the lag case still reopens", func(t *testing.T) {
		c := s.seedDismissed(t, ctx, "lag19", "ready", "handled_elsewhere")
		m := s.message(t, ctx, "lag19-in1", c.thread, "inbound",
			c.dismissedAt.Add(time.Minute), c.dismissedAt.Add(-19*time.Minute))
		if out := s.guardedCopy(t, ctx, c.task, c.dismissal, m); out["reopened"] != true {
			t.Errorf("a copy sent 19 min before the dismissal and ingested after = %v, want a reopen "+
				"(Jira's measured poll lag reaches ~15 min; he could not have seen it)", out)
		}
	})

	t.Run("(c) a copy with no sent_at: the ingest clock alone decides", func(t *testing.T) {
		c := s.seedDismissed(t, ctx, "nosent", "ready", "handled_elsewhere")
		m := s.message(t, ctx, "nosent-in1", c.thread, "inbound", c.dismissedAt.Add(time.Minute), time.Time{})
		if _, err := s.pool.Exec(ctx, `UPDATE normalized_messages SET sent_at=NULL WHERE id=$1`, m); err != nil {
			t.Fatal(err)
		}
		if out := s.guardedCopy(t, ctx, c.task, c.dismissal, m); out["reopened"] != true {
			t.Errorf("a copy with no sent_at ingested after the dismissal = %v, want a reopen", out)
		}
	})

	t.Run("(d) a person's message sent 2 h before, ingested after: D2 reopens", func(t *testing.T) {
		c := s.seedDismissed(t, ctx, "person", "ready", "handled_elsewhere")
		m := s.message(t, ctx, "person-in1", c.thread, "inbound",
			c.dismissedAt.Add(time.Minute), c.dismissedAt.Add(-2*time.Hour))
		if out := s.guarded(t, ctx, c.task, c.dismissal, m); out["reopened"] != true {
			t.Errorf("an unflagged late message = %v, want a reopen: the window is for bot copies only", out)
		}
	})
}

// MUTATIONS: drop the slack clause -> (a) reopens; skip slack without the flag
// -> (b) skips.
func TestStaleCopy_ASlackNotifierCopyNeverReopensADismissedTask(t *testing.T) {
	ctx := context.Background()
	s := newDroSuite(t, ctx)

	t.Run("(a) the Jira app's Slack DM, sent after the dismissal: skips", func(t *testing.T) {
		c := s.seedDismissed(t, ctx, "slackjira", "ready", "handled_elsewhere")
		m := s.slackMessage(t, ctx, "slackjira-in1", c.thread, "Jira",
			c.dismissedAt.Add(time.Minute), c.dismissedAt.Add(30*time.Second))
		droWantSkipped(t, "a Slack Jira-app copy", s.guardedCopy(t, ctx, c.task, c.dismissal, m), "slack_notifier_copy")
		if got := s.status(t, ctx, c.task); got != "closed" {
			t.Errorf("task after a Slack notifier copy = %q, want closed", got)
		}
	})

	t.Run("(b) a person on Slack (unflagged) reopens", func(t *testing.T) {
		c := s.seedDismissed(t, ctx, "slackperson", "ready", "handled_elsewhere")
		m := s.slackMessage(t, ctx, "slackperson-in1", c.thread, "Katie Evans",
			c.dismissedAt.Add(time.Minute), c.dismissedAt.Add(30*time.Second))
		if out := s.guarded(t, ctx, c.task, c.dismissal, m); out["reopened"] != true {
			t.Errorf("a person's Slack message = %v, want a reopen", out)
		}
	})
}

// A plain reopen carrying the flag is refused: the flag qualifies an activity
// call, and a plain reopen has no message to judge.
func TestStaleCopy_NotifierCopyOnAPlainReopenIsRefused(t *testing.T) {
	ctx := context.Background()
	s := newDroSuite(t, ctx)
	c := s.seedDismissed(t, ctx, "plainflag", "ready", "handled_elsewhere")
	if _, err := s.run(ctx, "task_reopen", droHuman, c.task,
		`{"task_id":`+itoa(c.task)+`,"reason":"x","notifier_copy":true}`); err == nil {
		t.Errorf("task_reopen with notifier_copy and no message_id validated; want a refusal")
	}
}

func (s *rvSuite) slackMessage(t *testing.T, ctx context.Context, label string, thread int64,
	sender string, createdAt, sentAt time.Time) int64 {
	t.Helper()
	s.seq++
	raw := s.id(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,'{}',$3, now()) RETURNING id`, s.account, "itest-revive-"+label, "itest-revive-h-"+label)
	return s.id(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		    body_text, subject, sender, channel, created_at)
		 VALUES ($1,$2,'inbound',$3,$4,'Katie Evans commented on RVV-1','Jira',$5,'slack',$6) RETURNING id`,
		raw, thread, "slack:itest-revive:"+label, sentAt, sender, createdAt)
}

// MUTATIONS: drop either clause from reviveGuarded, or its staleCopy call -> (a) or (b) revives.
func TestStaleCopy_TheReviveHasTheSameTwoGuards(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)
	reviveCopy := func(taskID, messageID int64) map[string]any {
		b, _ := json.Marshal(map[string]any{"task_id": taskID, "message_id": messageID, "revive": true,
			"notifier_copy": true, "reason": "itest-revive: notifier copy"})
		return s.call(t, ctx, "task_reopen", rvSpine, taskID, string(b))
	}
	closed := func(label string) (task, thread int64, closedAt time.Time) {
		task, thread = s.task(t, ctx, label, "ready")
		s.close(t, ctx, task)
		return task, thread, *s.row(t, ctx, task).closedAt
	}

	t.Run("(a) sent 30 min before the close, ingested after: skips", func(t *testing.T) {
		task, thread, at := closed("stale")
		m := s.message(t, ctx, "stale", thread, "inbound", at.Add(time.Minute), at.Add(-30*time.Minute))
		rvWantSkipped(t, "a copy sent 30 min before the close", reviveCopy(task, m), "message_sent_before_close")
	})
	t.Run("(b) the Jira app's Slack DM: skips", func(t *testing.T) {
		task, thread, at := closed("slackjira")
		m := s.slackMessage(t, ctx, "slackjira", thread, "Jira", at.Add(time.Minute), at.Add(30*time.Second))
		rvWantSkipped(t, "a Slack Jira-app copy", reviveCopy(task, m), "slack_notifier_copy")
		if s.row(t, ctx, task).status != "closed" {
			t.Errorf("a Slack notifier copy revived the task")
		}
	})
	t.Run("(c) a copy sent 10 min before the close, ingested after: revives", func(t *testing.T) {
		task, thread, at := closed("lag10")
		m := s.message(t, ctx, "lag10", thread, "inbound", at.Add(time.Minute), at.Add(-10*time.Minute))
		if out := reviveCopy(task, m); out["reopened"] != true {
			t.Errorf("a copy inside the window = %v, want a revive", out)
		}
	})
}
