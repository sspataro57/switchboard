//go:build integration

package capture_test

// SWT-82 (docs/bugs/qa-comment-holds-task.md): a person's Jira comment that
// lands on an OPEN task whose ticket sits in a delivered status (TT-In QA)
// surfaces the task, so the ticket-status reconciler holds it instead of
// closing it seconds later. Production: Jahnvi's QA comments on API-4323 and
// API-4324, 2026-09-23, tasks #592 / #598 closed 2s after the comment logged.
//
// Reuses the SWT-45 crvSuite (rules_revive_integration_test.go); the same
// compose-db-only rule applies.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/capture"
)

// commentMsg is the connector's copy with the connector's REAL external id
// spelling (jira:{site}:comment:{id} or jira:{site}:issue:{KEY}), which the
// suite's jiraMsg does not use.
func (s *crvSuite) commentMsg(t *testing.T, ctx context.Context, key, extKind, sender string, at time.Time) int64 {
	t.Helper()
	m := s.msg(t, ctx, s.jiraAcct, crvJiraPrefix+key, "jira", "inbound", sender, "", "QA: works on integration", at, at)
	ext := fmt.Sprintf("%scomment:%d", crvJiraPrefix, m)
	if extKind == "issue" {
		ext = crvJiraPrefix + "issue:" + key
	}
	s.exec(t, ctx, `UPDATE normalized_messages SET external_message_id=$2 WHERE id=$1`, m, ext)
	return m
}

// inQATask creates KEY's task through the suite's NON-reviving connector rule
// (as rule 75's email created #592), lets the reconciler see it while the
// ticket is In Progress, then moves the ticket to the project's delivered
// status and arms the reviving connector rule (rule 72's shape).
func (s *crvSuite) inQATask(t *testing.T, ctx context.Context, key string) int64 {
	t.Helper()
	s.exec(t, ctx, `UPDATE projects SET ticket_delivered_statuses='{"TT-In QA"}' WHERE id=$1`, s.project)
	s.snapshot(t, ctx, key, "indeterminate", "In Progress")
	start := s.dbNow(t, ctx).Add(-30 * time.Minute)
	s.jiraMsg(t, ctx, key, start, start)
	s.pass(t, ctx, capture.RulesModeLive)
	task := s.mustTask(t, ctx, key)
	s.reconcile(t, ctx)
	if got := s.status(t, ctx, task); got != "ready" {
		t.Fatalf("setup: %s's task is %q before the move to QA, want ready", key, got)
	}
	s.snapshot(t, ctx, key, "indeterminate", "TT-In QA")
	return task
}

// MUTATIONS that turn this red:
//   - commentHolds returns false for every input (J10 as it was);
//   - the apply branch drops the markRuleSurfacedWhy call;
//   - the ":comment:" test in commentHolds is inverted.
func TestCommentHolds_Integration_APersonsQACommentKeepsTheTaskOpen(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)

	t.Run("a person's comment while the task is open holds it", func(t *testing.T) {
		task := s.inQATask(t, ctx, "CRV-82")
		s.rule(t, ctx, s.project, "thread_key_contains", crvJiraPrefix+"CRV-82", `[A-Z]+-[0-9]+$`, 99, true, false)
		m := s.commentMsg(t, ctx, "CRV-82", "comment", "Jahnvi Seth", s.dbNow(t, ctx))

		stats := s.pass(t, ctx, capture.RulesModeLive)
		if stats.SurfacedOpen != 1 {
			t.Errorf("SurfacedOpen = %d, want 1", stats.SurfacedOpen)
		}
		if _, by := s.surfaced(t, ctx, task); by == nil || *by != m {
			t.Fatalf("task not surfaced by comment %d (surfaced_by=%v)", m, by)
		}
		if r, _ := s.decisionReason(t, ctx, m, capture.RulesModeLive); !strings.Contains(r, "SWT-82") {
			t.Errorf("decision reason %q does not name the hold", r)
		}
		s.reconcile(t, ctx)
		if got := s.status(t, ctx, task); got != "ready" {
			t.Errorf("the reconciler closed the task a QA comment surfaced (%q), want it held ready", got)
		}
		for i := 0; i < 3; i++ {
			s.reconcile(t, ctx)
		}
		if got := s.status(t, ctx, task); got != "ready" {
			t.Errorf("the held task closed on a quiet pass (%q)", got)
		}
		// The hold ends the way every SWT-45 hold ends: the ticket moves on.
		s.snapshot(t, ctx, "CRV-82", "done", "Closed-ish")
		s.reconcile(t, ctx)
		if got := s.status(t, ctx, task); got != "closed" {
			t.Errorf("the held task did not close when the ticket went done (%q)", got)
		}
	})

	for _, tc := range []struct {
		key, name, kind, sender, notifier string
	}{
		{"CRV-83", "the ticket-description copy (re-emitted on any update)", "issue", "Jahnvi Seth", ""},
		{"CRV-84", "a comment from a notifier", "comment", "Automation for Jira", "Automation for Jira"},
		{"CRV-85", "a comment with no sender", "comment", " ", ""},
	} {
		t.Run(tc.name+" does not hold (J10)", func(t *testing.T) {
			task := s.inQATask(t, ctx, tc.key)
			s.exec(t, ctx, `UPDATE projects SET notifier_senders=$2 WHERE id=$1`, s.project, []string{tc.notifier})
			t.Cleanup(func() {
				s.exec(t, ctx, `UPDATE projects SET notifier_senders='{}' WHERE id=$1`, s.project)
			})
			s.rule(t, ctx, s.project, "thread_key_contains", crvJiraPrefix+tc.key, `[A-Z]+-[0-9]+$`, 99, true, false)
			s.commentMsg(t, ctx, tc.key, tc.kind, tc.sender, s.dbNow(t, ctx))
			if st := s.pass(t, ctx, capture.RulesModeLive); st.SurfacedOpen != 0 {
				t.Errorf("SurfacedOpen = %d, want 0", st.SurfacedOpen)
			}
			if at, _ := s.surfaced(t, ctx, task); at != nil {
				t.Errorf("task surfaced (%v); want log only", at)
			}
			s.reconcile(t, ctx)
			if got := s.status(t, ctx, task); got != "closed" {
				t.Errorf("task is %q; the reconciler should close it for the QA ticket", got)
			}
		})
	}

	t.Run("shadow says would surface and calls nothing", func(t *testing.T) {
		task := s.inQATask(t, ctx, "CRV-86")
		s.rule(t, ctx, s.project, "thread_key_contains", crvJiraPrefix+"CRV-86", `[A-Z]+-[0-9]+$`, 99, true, false)
		m := s.commentMsg(t, ctx, "CRV-86", "comment", "Jahnvi Seth", s.dbNow(t, ctx))
		st := s.pass(t, ctx, capture.RulesModeShadow)
		if st.SurfacedOpen != 0 || s.audits(t, ctx, task, "task_mark_surfaced", crvActor) != 0 {
			t.Errorf("shadow surfaced something (SurfacedOpen=%d)", st.SurfacedOpen)
		}
		if r, _ := s.decisionReason(t, ctx, m, capture.RulesModeShadow); !strings.Contains(r, "would surface") {
			t.Errorf("shadow reason %q, want it to say would surface", r)
		}
	})
}
