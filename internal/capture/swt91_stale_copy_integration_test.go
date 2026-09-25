//go:build integration

package capture_test

// SWT-91 stale-copy-reopens, capture's half: a message whose sender is on the
// project's notifier_senders list (read by loadRules, the column's one reader)
// asks task_reopen to treat it as a copy (notifier_copy). The Jira app's Slack
// DM on a dismissed task then leaves it closed; a person's Slack message on the
// same task still reopens it. The verb's own guards are pinned in
// internal/tools/stale_copy_reopen_integration_test.go.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run SWT91 ./internal/capture/
//
// Reuses the resurface (rsf) suite: collaboratory's shape, rule 10.
//
// MUTATIONS: drop the notifierCopy assignment in decideMessage (or stop
// passing it to reopenRuleTask) -> (a) reopens the task.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/capture"
)

func TestSWT91_Integration_ASlackNotifierCopyLeavesADismissedTaskClosed(t *testing.T) {
	ctx := context.Background()
	s := newRSFSuite(t, ctx)
	s.exec(t, ctx, `UPDATE projects SET notifier_senders = '{Jira}' WHERE id=$1`, s.project)
	b := s.bucket(t, ctx, "CCO")
	s.execute(t, ctx, "task_dismiss", rsfHuman, b, fmt.Sprintf(`{"task_id":%d,"reason_code":"handled_elsewhere"}`, b))

	// (a) the Jira app's DM: logged, never reopened.
	m := s.human(t, ctx, "jira-copy", "Jira", "Katie Evans transitioned CCO-7 from TT-In QA to TT-Verified")
	st := s.pass(t, ctx, capture.RulesModeLive)
	d := s.logged(t, ctx, m, capture.RulesModeLive, b, false, "a notifier copy on a dismissed task is logged")
	if !strings.Contains(d.reason, "notifier copy (SWT-91)") {
		t.Errorf("decision reason %q does not say the message is a notifier copy", d.reason)
	}
	if got := s.status(t, ctx, b); got != "closed" {
		t.Errorf("the dismissed task is %q after the Jira app's Slack DM, want closed", got)
	}
	if st.Reopened != 0 {
		t.Errorf("RulesStats.Reopened = %d, want 0", st.Reopened)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool='task_reopen'
	                          AND actor=$2 AND (args->>'notifier_copy')::boolean`, b, rsfActor); n != 1 {
		t.Errorf("task_reopen calls carrying notifier_copy = %d, want 1", n)
	}

	// (b) a person's Slack message on the same task still reopens it.
	p := s.human(t, ctx, "person", "Katie Evans", "CCO-7 is still broken for me")
	st = s.pass(t, ctx, capture.RulesModeLive)
	s.logged(t, ctx, p, capture.RulesModeLive, b, false, "a person's message on a dismissed task is logged")
	if got := s.status(t, ctx, b); got != "ready" {
		t.Errorf("the dismissed task is %q after a person's message, want ready", got)
	}
	if st.Reopened != 1 {
		t.Errorf("RulesStats.Reopened = %d, want 1", st.Reopened)
	}
}

// The revive half (reviewer finding 1): an ACTIVITY rule's closed task gets the
// same flag through reviveRuleTask. MUTATION: stop passing
// decision.notifierCopy to reviveRuleTask -> (a) and (b) revive the task.
func TestSWT91_Integration_ANotifierCopyNeverRevivesAClosedTaskLate(t *testing.T) {
	ctx := context.Background()
	s := newRSFSuite(t, ctx)
	s.exec(t, ctx, `UPDATE projects SET notifier_senders = '{Jira,builds@ci.example}' WHERE id=$1`, s.project)
	s.insID(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, priority, enabled,
		                            revive, note)
		 VALUES ($1,'body_regex','CCR-[0-9]+','jira','(CCR-[0-9]+)',95,true,true,'itest-capcc revive') RETURNING id`,
		s.project)
	task := s.createdBy(t, ctx, "CCR-9", func() int64 {
		return s.human(t, ctx, "swt91-setup", "Dana Ruiz", "please look at CCR-9")
	})
	s.closeTask(t, ctx, task)

	// (a) the Jira app's Slack DM: never revives.
	s.human(t, ctx, "swt91-slack", "Jira", "Katie Evans commented on CCR-9")
	if st := s.pass(t, ctx, capture.RulesModeLive); st.Revived != 0 || s.status(t, ctx, task) != "closed" {
		t.Errorf("a Slack notifier copy: Revived=%d status=%q, want 0 / closed", st.Revived, s.status(t, ctx, task))
	}

	// (b) a bot email SENT 30 min before the close, ingested after: never revives.
	m := s.message(t, ctx, "swt91-mail", "itest-capcc-mail-ccr9", "gmail", "CI <builds@ci.example>",
		"build failed on CCR-9", s.treetop)
	s.exec(t, ctx, `UPDATE normalized_messages SET sent_at = (SELECT closed_at FROM tasks WHERE id=$2) - interval '30 minutes'
	                 WHERE id=$1`, m, task)
	if st := s.pass(t, ctx, capture.RulesModeLive); st.Revived != 0 || s.status(t, ctx, task) != "closed" {
		t.Errorf("a stale bot email: Revived=%d status=%q, want 0 / closed", st.Revived, s.status(t, ctx, task))
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool='task_reopen'
	                          AND actor=$2 AND (args->>'revive')::boolean AND (args->>'notifier_copy')::boolean`,
		task, rsfActor); n != 2 {
		t.Errorf("revive calls carrying notifier_copy = %d, want 2", n)
	}

	// (c) a person's message still revives it.
	s.human(t, ctx, "swt91-person", "Katie Evans", "CCR-9 still broken")
	if st := s.pass(t, ctx, capture.RulesModeLive); st.Revived != 1 || s.status(t, ctx, task) == "closed" {
		t.Errorf("a person's message: Revived=%d status=%q, want 1 / open", st.Revived, s.status(t, ctx, task))
	}
}
