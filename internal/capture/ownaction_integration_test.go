//go:build integration

package capture_test

// SWT-45 J17, the own-action guard (internal/capture/ownaction.go and
// rules_store.go ownActionFacts), against a real database through a real
// capture pass. Reuses the crvSuite harness from rules_revive_integration_test.go
// (same package, same cleanup, same private-database rule):
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_iso45?sslmode=disable \
//	  TZ=UTC go test -tags integration -p 1 -count=1 -run CaptureOwnAction ./internal/capture/
//
// The crvSuite's jira account has scopes '{}' — it covers no key, so every
// other revive test runs with the guard "not applicable". These tests arm it
// with ownActionPoller: domain_default gives the connector's thread key
// (jira:caprev.jira.com:{KEY}, the same spelling crvJiraPrefix uses), and scopes
// {CRV} make it the key's poller.
//
// The window tests send their email as "Anonymous (JIRA)" (anonMailMsg), the
// shape Jira gives his own changes. The suite's default mailMsg sender is a
// NAMED actor ("Katie Evans (JIRA)"), which the named-actor exemption lets
// through before the window is consulted — so a window test on mailMsg would
// prove nothing about the window.
//
// TEST THE COLUMN, NOT THE FIXTURE. Each predicate the guard reads is proven by
// a fixture that only that predicate excludes. MUTATIONS, each run by hand once
// and seen red:
//   - drop `o.direction = 'outbound'`  -> ...OutsideTheWindowStillRevives: the
//     INBOUND comment inside the window counts, and the revive is skipped;
//   - drop `o.sent_at >= $2 AND o.sent_at <= $3` -> the same test: the OUTBOUND
//     comment 11 minutes before the email counts;
//   - drop `r.started_at > $2` -> ...AStaleThreadDefers: a poller run that
//     started BEFORE the window's far edge makes the thread fresh;
//   - drop the unnormalized-raw clause -> ...AnUnnormalizedCommentDefers;
//   - drop the named-actor exemption (the `case o.namedOther()` in
//     ownActionSettled) -> ...ANamedOtherActorRevivesAndCreates: his comment
//     inside the window skips Katie's mention.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/capture"
)

// ownActionPoller makes the suite's jira account the poller of CRV-* keys.
func (s *crvSuite) ownActionPoller(t *testing.T, ctx context.Context) {
	t.Helper()
	s.exec(t, ctx, `UPDATE source_accounts SET domain_default = 'https://caprev.jira.com', scopes = '{CRV}' WHERE id = $1`,
		s.jiraAcct)
}

// pollerRan records one finished 'ok' connector-jira run that started at `at`.
func (s *crvSuite) pollerRan(t *testing.T, ctx context.Context, at time.Time) {
	t.Helper()
	s.exec(t, ctx, `INSERT INTO sync_runs (source_account_id, started_at, finished_at, status) VALUES ($1, $2, $2, 'ok')`,
		s.jiraAcct, at)
}

// hisComment is the connector copy of HIS OWN comment: outbound, on the
// ticket's jira thread, stamped with Jira's time, sender his Jira display name.
func (s *crvSuite) hisComment(t *testing.T, ctx context.Context, key string, sent time.Time) int64 {
	return s.msg(t, ctx, s.jiraAcct, crvJiraPrefix+key, "jira", "outbound", "Salvador Spataro", "",
		"his own comment on "+key, sent, sent)
}

// notifyMsg is a Jira notification email with an exact stored sender (the
// decoded raw From header, as the IMAP normalizer stores it).
func (s *crvSuite) notifyMsg(t *testing.T, ctx context.Context, sender, subject string, created, sent time.Time) int64 {
	s.seq++
	return s.msg(t, ctx, s.mail, fmt.Sprintf("%s<a%d@caprev>", crvMailPfx, s.seq), "gmail", "inbound",
		sender, subject, "", created, sent)
}

// anonMailMsg is prod's own-change shape, QUOTED as prod stores it:
// `"Anonymous (JIRA)" <jira@…>`.
func (s *crvSuite) anonMailMsg(t *testing.T, ctx context.Context, subject string, created, sent time.Time) int64 {
	return s.notifyMsg(t, ctx, `"Anonymous (JIRA)" <`+crvMailFrom+`>`, subject, created, sent)
}

// (1) an outbound message inside the window, Anonymous email -> no reopen,
// log only, own_action. This is also the "Anonymous inside the window is still
// skipped" half of the named-actor exemption.
func TestCaptureOwnAction_Integration_HisOwnCommentDoesNotRevive(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	s.mailRule(t, ctx, true)
	s.ownActionPoller(t, ctx)

	task, closedAt := s.closedTask(t, ctx, "CRV-70")
	emailSent := closedAt.Add(-10 * time.Minute)
	s.hisComment(t, ctx, "CRV-70", emailSent.Add(-5*time.Minute))
	s.pollerRan(t, ctx, s.dbNow(t, ctx))
	logs := s.events(t, ctx, task, "log")
	m := s.anonMailMsg(t, ctx, "[JIRA] (CRV-70) Fix the export", closedAt.Add(time.Second), emailSent)

	stats := s.pass(t, ctx, capture.RulesModeLive)

	if got := s.status(t, ctx, task); got != "closed" {
		t.Fatalf("CRV-70's task is %q, want closed: the email is Jira telling him about HIS OWN comment (an outbound "+
			"message on the ticket's thread 5 minutes before it), which must not revive (J17)", got)
	}
	reason, ok := s.decisionReason(t, ctx, m, capture.RulesModeLive)
	if !ok || !strings.Contains(reason, "own_action") || !strings.Contains(reason, "revive skipped") {
		t.Errorf("live decision reason = %q (found=%v), want it to record own_action and \"revive skipped\"", reason, ok)
	}
	if n := s.audits(t, ctx, task, "task_reopen", crvActor); n != 0 || stats.Revived != 0 {
		t.Errorf("task_reopen called %d time(s), Revived=%d — want 0 / 0: the skip is capture's, before any call", n, stats.Revived)
	}
	if n := s.events(t, ctx, task, "log"); n != logs+1 {
		t.Errorf("log events went %d -> %d, want +1: the notification is still logged on its ticket's task", logs, n)
	}
	if at, _ := s.surfaced(t, ctx, task); at != nil {
		t.Errorf("the task was surfaced (%v) by his own action", at)
	}
}

// (2) an outbound message OUTSIDE the window, and an INBOUND one inside it ->
// neither is his action on this email, so the revive proceeds. This is the test
// the two SQL predicates are proven by (see the header's mutations). The email
// is Anonymous so the named-actor exemption cannot be what lets it through.
func TestCaptureOwnAction_Integration_HisCommentOutsideTheWindowStillRevives(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	s.mailRule(t, ctx, true)
	s.ownActionPoller(t, ctx)

	task, closedAt := s.closedTask(t, ctx, "CRV-71")
	emailSent := closedAt.Add(-10 * time.Minute)
	s.hisComment(t, ctx, "CRV-71", emailSent.Add(-capture.OwnActionLead-time.Minute)) // 11 min before: an older comment of his
	s.msg(t, ctx, s.jiraAcct, crvJiraPrefix+"CRV-71", "jira", "inbound", "Katie Evans", "",
		"Katie's comment", closedAt.Add(time.Second), emailSent.Add(-time.Minute)) // inside the window, but not his
	s.pollerRan(t, ctx, s.dbNow(t, ctx))
	m := s.anonMailMsg(t, ctx, "[JIRA] (CRV-71) Fix the export", closedAt.Add(2*time.Second), emailSent)

	stats := s.pass(t, ctx, capture.RulesModeLive)

	if got := s.status(t, ctx, task); got != "ready" {
		t.Fatalf("CRV-71's task is %q, want ready: his only outbound message is 11 minutes before the email (outside "+
			"[-%s, +%s]) and the in-window comment is INBOUND. A guard that skipped here has lost its direction or its "+
			"time predicate", got, capture.OwnActionLead, capture.OwnActionLag)
	}
	reason, _ := s.decisionReason(t, ctx, m, capture.RulesModeLive)
	if strings.Contains(reason, "own_action") || !strings.Contains(reason, "revive requested") {
		t.Errorf("live decision reason = %q, want \"revive requested\" and no own_action", reason)
	}
	if stats.Revived != 1 || stats.Deferred != 0 || stats.Blind != 0 {
		t.Errorf("RulesStats revived=%d deferred=%d blind=%d, want 1 / 0 / 0", stats.Revived, stats.Deferred, stats.Blind)
	}
}

// The creation half: his own comment on a ticket with no task creates nothing
// (attributed, so no lower rule takes it), while an untouched ticket beside it
// is created and surfaced as before.
func TestCaptureOwnAction_Integration_HisOwnActionCreatesNothing(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	s.mailRule(t, ctx, true)
	s.ownActionPoller(t, ctx)

	now := s.dbNow(t, ctx)
	emailSent := now.Add(-10 * time.Minute)
	s.hisComment(t, ctx, "CRV-72", emailSent.Add(time.Minute)) // inside: +1 min
	s.pollerRan(t, ctx, now)
	mine := s.anonMailMsg(t, ctx, "[JIRA] (CRV-72) A ticket he just commented on", now, emailSent)
	other := s.anonMailMsg(t, ctx, "[JIRA] (CRV-73) A ticket someone else touched", now, emailSent)

	stats := s.pass(t, ctx, capture.RulesModeLive)

	if _, found := s.taskOf(t, ctx, "CRV-72"); found {
		t.Errorf("a task was created for CRV-72 from Jira's mail about HIS OWN comment (J17)")
	}
	if action, _ := s.decisionAction(t, ctx, mine, capture.RulesModeLive); action != "attributed" {
		t.Errorf("CRV-72's decision action = %q, want attributed (the match is spent; nothing falls through)", action)
	}
	if reason, _ := s.decisionReason(t, ctx, mine, capture.RulesModeLive); !strings.Contains(reason, "own_action") ||
		!strings.Contains(reason, "no task created") {
		t.Errorf("CRV-72's reason = %q, want own_action and \"no task created\"", reason)
	}
	task, found := s.taskOf(t, ctx, "CRV-73")
	if !found {
		t.Fatalf("no task for CRV-73: the guard must not block a ticket with nothing of his on its thread")
	}
	if at, by := s.surfaced(t, ctx, task); at == nil || by == nil || *by != other {
		t.Errorf("CRV-73's task surfaced_at=%v by=%v, want set / %d", at, by, other)
	}
	if stats.TasksCreated != 1 || stats.SurfacedCreated != 1 {
		t.Errorf("RulesStats tasks_created=%d surfaced_created=%d, want 1 / 1", stats.TasksCreated, stats.SurfacedCreated)
	}
}

// (3) the thread is not yet known synced past the email -> deferred (no
// decision row, the claim unspent), every pass, until OwnActionMaxWait from the
// message's ingest; then the revive proceeds, the reason says it ran blind, and
// RulesStats.Blind counts it.
func TestCaptureOwnAction_Integration_AStaleThreadDefersThenProceedsBlind(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	s.mailRule(t, ctx, true)
	s.ownActionPoller(t, ctx)

	task, closedAt := s.closedTask(t, ctx, "CRV-74")
	// Fixture: the close happened two hours ago, so a message whose ingest is
	// later moved back past the bound is still ingested after the close.
	s.exec(t, ctx, `UPDATE tasks SET closed_at = closed_at - interval '2 hours', updated_at = updated_at - interval '2 hours'
	                 WHERE id = $1`, task)
	emailSent := closedAt.Add(-10 * time.Minute)
	// A poller run that started INSIDE the window: it may have missed a comment
	// written at the window's far edge, so it does not make the thread fresh.
	s.pollerRan(t, ctx, emailSent.Add(capture.OwnActionLag))
	m := s.anonMailMsg(t, ctx, "[JIRA] (CRV-74) Fix the export", closedAt.Add(time.Second), emailSent)

	for pass := 1; pass <= 2; pass++ {
		stats := s.pass(t, ctx, capture.RulesModeLive)
		if stats.Deferred != 1 || stats.Considered != 0 || stats.Blind != 0 {
			t.Errorf("pass %d: deferred=%d considered=%d blind=%d, want 1 / 0 / 0", pass, stats.Deferred, stats.Considered, stats.Blind)
		}
		if _, ok := s.decisionReason(t, ctx, m, capture.RulesModeLive); ok {
			t.Fatalf("pass %d wrote a live decision for a message on a thread not known synced past it: the claim "+
				"is spent and the guard can never run with the facts (J17's race)", pass)
		}
		if got := s.status(t, ctx, task); got != "closed" {
			t.Fatalf("pass %d: the task is %q, want closed while the guard waits", pass, got)
		}
	}

	// The bound elapses: move the message's INGEST back past OwnActionMaxWait.
	s.exec(t, ctx, `UPDATE normalized_messages SET created_at = created_at - $2::interval WHERE id = $1`,
		m, (capture.OwnActionMaxWait + time.Minute).String())
	stats := s.pass(t, ctx, capture.RulesModeLive)

	if got := s.status(t, ctx, task); got != "ready" {
		t.Fatalf("after %s the task is %q, want ready: the guard must not hold a revive forever", capture.OwnActionMaxWait, got)
	}
	if reason, _ := s.decisionReason(t, ctx, m, capture.RulesModeLive); !strings.Contains(reason, "BLIND") {
		t.Errorf("reason = %q, want it to say the guard ran BLIND (it never got the fact it wanted)", reason)
	}
	if stats.Deferred != 0 || stats.Revived != 1 || stats.Blind != 1 {
		t.Errorf("RulesStats deferred=%d revived=%d blind=%d, want 0 / 1 / 1", stats.Deferred, stats.Revived, stats.Blind)
	}
}

// Freshness also needs the poller's rows NORMALIZED: a comment fetched but not
// yet a message could be his. A run after the window with K's comment raw row
// still pending defers; once it is normalized the guard decides (clear).
func TestCaptureOwnAction_Integration_AnUnnormalizedCommentDefers(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	s.mailRule(t, ctx, true)
	s.ownActionPoller(t, ctx)

	task, closedAt := s.closedTask(t, ctx, "CRV-75")
	emailSent := closedAt.Add(-10 * time.Minute)
	s.pollerRan(t, ctx, s.dbNow(t, ctx))
	s.exec(t, ctx, `INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
	                VALUES ($1, 'comment:CRV-75:9001', '{}', 'itest-caprev-pending-9001')`, s.jiraAcct)
	m := s.anonMailMsg(t, ctx, "[JIRA] (CRV-75) Fix the import", closedAt.Add(time.Second), emailSent)

	if stats := s.pass(t, ctx, capture.RulesModeLive); stats.Deferred != 1 {
		t.Fatalf("deferred=%d, want 1: CRV-75 has a comment raw row awaiting normalization", stats.Deferred)
	}
	s.exec(t, ctx, `UPDATE raw_source_items SET normalized_at = now() WHERE source_account_id = $1 AND external_id = 'comment:CRV-75:9001'`,
		s.jiraAcct)
	stats := s.pass(t, ctx, capture.RulesModeLive)

	if got := s.status(t, ctx, task); got != "ready" || stats.Revived != 1 {
		t.Errorf("after normalization: task %q revived=%d, want ready / 1", got, stats.Revived)
	}
	if reason, _ := s.decisionReason(t, ctx, m, capture.RulesModeLive); strings.Contains(reason, "BLIND") ||
		strings.Contains(reason, "own-action guard") {
		t.Errorf("reason = %q, want a clear guard (no note): the thread was synced and nothing of his is in the window", reason)
	}
}

// ---- the named-actor exemption (second review batch) --------------------------

// (a) WEB-10355's shape: Katie's mention arrives 8m38s after his own comment on
// the same ticket — inside the window — on a thread NOT known synced (no poller
// run at all). The email names Katie, so it is her action: the closed task
// revives and the task-less ticket is created and surfaced, in ONE pass, with
// no deferral and no blind decision.
func TestCaptureOwnAction_Integration_ANamedOtherActorRevivesAndCreates(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	s.mailRule(t, ctx, true)
	s.ownActionPoller(t, ctx)

	task, closedAt := s.closedTask(t, ctx, "CRV-80")
	emailSent := closedAt.Add(-10 * time.Minute)
	gap := 8*time.Minute + 38*time.Second // prod: his comment 20:18:45Z, her mention 20:27:23Z
	s.hisComment(t, ctx, "CRV-80", emailSent.Add(-gap))
	s.hisComment(t, ctx, "CRV-81", emailSent.Add(-gap))
	// Deliberately no pollerRan: a named-other email must not wait for one.
	katie := `"Katie Evans (JIRA)" <` + crvMailFrom + `>`
	revive := s.notifyMsg(t, ctx, katie, "[JIRA] Katie Evans mentioned you on CRV-80", closedAt.Add(time.Second), emailSent)
	create := s.notifyMsg(t, ctx, katie, "[JIRA] Katie Evans mentioned you on CRV-81", closedAt.Add(time.Second), emailSent)

	stats := s.pass(t, ctx, capture.RulesModeLive)

	if got := s.status(t, ctx, task); got != "ready" {
		t.Fatalf("CRV-80's task is %q, want ready: the email NAMES Katie as the actor, so his own comment 8m38s "+
			"earlier does not make her mention his action (the WEB-10355 case)", got)
	}
	if at, by := s.surfaced(t, ctx, task); at == nil || by == nil || *by != revive {
		t.Errorf("CRV-80's task surfaced_at=%v by=%v, want set / %d", at, by, revive)
	}
	for _, m := range []int64{revive, create} {
		reason, ok := s.decisionReason(t, ctx, m, capture.RulesModeLive)
		if !ok || !strings.Contains(reason, "actor named") || strings.Contains(reason, "own_action") ||
			strings.Contains(reason, "BLIND") {
			t.Errorf("message %d reason = %q (found=%v), want \"actor named\", no own_action, no BLIND", m, reason, ok)
		}
	}
	created, found := s.taskOf(t, ctx, "CRV-81")
	if !found {
		t.Fatalf("no task for CRV-81: Katie's mention must create one despite his comment in the window")
	}
	if at, by := s.surfaced(t, ctx, created); at == nil || by == nil || *by != create {
		t.Errorf("CRV-81's task surfaced_at=%v by=%v, want set / %d", at, by, create)
	}
	if stats.Deferred != 0 || stats.Blind != 0 || stats.Revived != 1 || stats.SurfacedCreated != 1 {
		t.Errorf("RulesStats deferred=%d blind=%d revived=%d surfaced_created=%d, want 0 / 0 / 1 / 1",
			stats.Deferred, stats.Blind, stats.Revived, stats.SurfacedCreated)
	}
}

// (b) The exemption is for OTHERS: an email naming HIM (the connector's stored
// sender of the outbound comment in the window) is still his own action.
// Never observed on prod; pinned so the comparison is to his stored name, not
// merely "any name".
func TestCaptureOwnAction_Integration_ANamedActorWhoIsHimIsStillSkipped(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	s.mailRule(t, ctx, true)
	s.ownActionPoller(t, ctx)

	task, closedAt := s.closedTask(t, ctx, "CRV-82")
	emailSent := closedAt.Add(-10 * time.Minute)
	s.hisComment(t, ctx, "CRV-82", emailSent.Add(-time.Minute))
	m := s.notifyMsg(t, ctx, `Salvador Spataro (JIRA) <`+crvMailFrom+`>`, "[JIRA] (CRV-82) Fix the export",
		closedAt.Add(time.Second), emailSent)

	s.pass(t, ctx, capture.RulesModeLive)

	if got := s.status(t, ctx, task); got != "closed" {
		t.Fatalf("CRV-82's task is %q, want closed: the email names HIM, and his outbound comment is in the window", got)
	}
	if reason, _ := s.decisionReason(t, ctx, m, capture.RulesModeLive); !strings.Contains(reason, "own_action") {
		t.Errorf("reason = %q, want own_action", reason)
	}
}
