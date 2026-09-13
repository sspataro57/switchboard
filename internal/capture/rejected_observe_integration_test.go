//go:build integration

package capture_test

// SWT-43 (docs/tickets/delivery-deny_SPEC.md) criterion 18: loop closure for a
// hand-sent copy of a rejected draft.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run CaptureRejected ./internal/capture/
//
// 'rejected' means "switchboard did not and will not send this row". If
// Salvador then sends the words by hand, capture must:
//   - NOT treat the rejected row as a pending claimant (hasUnconfirmedClaimant,
//     observe.go) — the observation is not deferred;
//   - attach exactly ONE outbound_observed event to the task (linkedTasks has
//     no status filter);
//   - leave the rejected row byte-unchanged.
//
// The slack case is the one that bites: a rejected slack row realistically
// carries a recent send_attempted_at (a definite failure, then a reject), which
// is exactly what a claimant looks like. Mutation: add 'rejected' to
// hasUnconfirmedClaimant's status list -> the observation is deferred -> red.
//
// GREENFIELD NOTE — EXPECTED RED: before 0028 no rejected row can be seeded.
//
// Reuses observe_integration_test.go's captureSuite (its project, accounts,
// thread prefixes and FK-ordered cleanup pact).

import (
	"context"
	"testing"

	"github.com/sspataro57/switchboard/internal/capture"
)

func (s *captureSuite) rejectedDelivery(t *testing.T, ctx context.Context, taskID int64, channel string,
	targetRef, threadID any, body string, attempted bool) int64 {
	t.Helper()
	attemptedAt := "NULL"
	if attempted {
		attemptedAt = "now() - interval '2 minutes'"
	}
	var id int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, thread_id, body, status, approval_source,
		                         rejection_note, send_attempted_at, send_settled_at)
		 VALUES ($1,$2,$3,$4,$5,'rejected','switchboard','denied: I will send it myself',
		         `+attemptedAt+`, `+attemptedAt+`) RETURNING id`,
		taskID, channel, targetRef, threadID, body).Scan(&id); err != nil {
		t.Fatalf("seed rejected %s delivery: %v (needs migration 0028)", channel, err)
	}
	return id
}

func (s *captureSuite) rowFingerprint(t *testing.T, ctx context.Context, id int64) string {
	t.Helper()
	var fp string
	if err := s.pool.QueryRow(ctx, `SELECT md5(d::text) FROM deliveries d WHERE id=$1`, id).Scan(&fp); err != nil {
		t.Fatalf("fingerprint delivery %d: %v", id, err)
	}
	return fp
}

func TestCaptureRejected_Integration_GmailHandSendIsObservedOnce(t *testing.T) {
	ctx := context.Background()
	s := newCaptureSuite(t, ctx)
	taskID := s.task(t, ctx, "gmail thread whose draft was rejected")

	const threadKey = capGmailThreadPrefix + "t-rejected"
	const words = "Pushed the fix to staging; the queue is draining and I will confirm once the backlog clears."
	_, threadID := s.message(t, ctx, msgFixture{
		provider: "google", channel: "gmail", direction: "outbound", threadKey: threadKey,
		extID: "<hand-sent-copy-itest-capture@mail.example.test>", sentAgo: "10 minutes",
		body: words, sender: "salvo@example.test", rawID: "gmail:rejected:1",
	})
	id := s.rejectedDelivery(t, ctx, taskID, "gmail", nil, threadID, words, false)
	before := s.rowFingerprint(t, ctx, id)

	if got := s.observe(t, ctx, capture.Gmail); got != 1 {
		t.Fatalf("ObserveOutbound(Gmail) = %d, want 1: a hand-sent copy of rejected words is an outbound "+
			"observation on the task (criterion 18)", got)
	}
	if n := s.eventCount(t, ctx, taskID, "outbound_observed"); n != 1 {
		t.Errorf("outbound_observed events = %d, want exactly 1", n)
	}
	if after := s.rowFingerprint(t, ctx, id); after != before {
		t.Errorf("observing the hand-send changed the rejected row; it is never stamped (criterion 15/18)")
	}
	if got := s.observe(t, ctx, capture.Gmail); got != 0 {
		t.Errorf("a second pass observed %d again, want 0 (one observation per task and message)", got)
	}
}

func TestCaptureRejected_Integration_SlackRejectedRowIsNotAClaimant(t *testing.T) {
	ctx := context.Background()
	s := newCaptureSuite(t, ctx)
	taskID := s.task(t, ctx, "slack conversation whose draft was rejected")

	const threadKey = capSlackThreadPrefix + "CCAPREJ"
	const conversationRef = "https://app.slack.com/client/TCAPTURE/CCAPREJ"
	const words = "the rejected Slack words, typed by hand a minute later"
	s.message(t, ctx, msgFixture{
		provider: "slack_web", channel: "slack", direction: "outbound", threadKey: threadKey,
		extID: "slack:TCAPTURE:CCAPREJ:p1780000000000031", sentAgo: "1 minute",
		body: words, sender: "Salvo", rawID: "message:CCAPREJ:p1780000000000031",
	})
	// Attempted two minutes ago (a definite failure, then Salvador rejected it):
	// inside the horizon, no id, not confirmed — a claimant in every respect
	// but its status.
	id := s.rejectedDelivery(t, ctx, taskID, "slack_reply", conversationRef, nil, words, true)
	before := s.rowFingerprint(t, ctx, id)

	if got := s.observe(t, ctx, capture.Slack); got != 1 {
		t.Fatalf("ObserveOutbound(Slack) = %d, want 1. A REJECTED row is not a pending claimant: switchboard will "+
			"never send it, so the message cannot be its own click (criterion 18)", got)
	}
	if n := s.eventCount(t, ctx, taskID, "outbound_observed"); n != 1 {
		t.Errorf("outbound_observed events = %d, want exactly 1", n)
	}
	if after := s.rowFingerprint(t, ctx, id); after != before {
		t.Errorf("observing the hand-send changed the rejected row")
	}
}
