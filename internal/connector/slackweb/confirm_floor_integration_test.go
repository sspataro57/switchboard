//go:build integration

package slackweb_test

// The confirmation time floor (SWT-12 criterion 23), in its PRODUCTION shape.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -run SlackFloor ./internal/connector/slackweb/
//
// Why a separate file from confirm_integration_test.go: that suite seeds
// send_attempted_at NULL, so its rows take the assisted-tier carve-out and never
// reach the timestamp comparison. A real switchboard-dispatched row always has the
// column set, so without these two cases the floor — the whole defence against a
// replay binding a months-old message to a newly stuck row — is untested.
//
// Reuses the 'itest-slack-confirm-%' corpus and cleanup pact.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sspataro57/switchboard/internal/connector/slackweb"
)

// cfMessagePostedAt must match confirmSource's exported Message.Timestamp — that
// value, not the epoch inside cfMessageID, is what normalization stores as
// sent_at and therefore what the floor compares against.
const cfMessagePostedAt = "2026-07-28T09:00:00Z"

func seedFloorRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64, attemptOffset string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, approval_source,
		                         send_attempted_at, send_settled_at)
		 VALUES ($1,'slack_reply',$2,$3,'sending','switchboard',
		         $4::timestamptz `+attemptOffset+`, now())
		 RETURNING id`,
		taskID, cfTarget, cfBody, cfMessagePostedAt).Scan(&id); err != nil {
		t.Fatalf("seed slack_reply delivery (apply migrations 0011 and 0012): %v", err)
	}
	return id
}

func runFloorExport(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	sink := slackweb.NewSink(pool)
	if _, err := slackweb.Ingest(ctx, confirmSource{}, sink); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if _, err := slackweb.Normalize(ctx, sink, slackweb.Config{}); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
}

// The ordinary case: we clicked, then the message appeared. This is the self-heal
// path the whole no-external-id design rests on, and it must still work with the
// column populated.
func TestSlackFloor_Integration_ConfirmsAMessagePostdatingTheAttempt(t *testing.T) {
	ctx := context.Background()
	pool := newConfirmPool(t, ctx)
	taskID := seedConfirmTask(t, ctx, pool)
	id := seedFloorRow(t, ctx, pool, taskID, `- interval '1 hour'`)

	runFloorExport(t, ctx, pool)

	r := readConfirmRow(t, ctx, pool, id)
	if r.status != "sent" {
		t.Errorf("status = %q, want sent: the message postdates the click, so this is our send", r.status)
	}
	if r.sentExternalID == nil || *r.sentExternalID != cfExternalID {
		t.Errorf("sent_external_id = %v, want %q", r.sentExternalID, cfExternalID)
	}
	if n := confirmEventCount(t, ctx, pool, taskID, "delivery_confirmed"); n != 1 {
		t.Errorf("delivery_confirmed events = %d, want exactly 1", n)
	}
}

// The defence: a message that PREDATES the click cannot be that click's message.
// Without this, `--all` could bind an old — or hand-typed — identical message to a
// newly stuck row and promote a send that never happened.
func TestSlackFloor_Integration_RefusesAMessagePredatingTheAttempt(t *testing.T) {
	ctx := context.Background()
	pool := newConfirmPool(t, ctx)
	taskID := seedConfirmTask(t, ctx, pool)
	id := seedFloorRow(t, ctx, pool, taskID, `+ interval '1 hour'`)

	runFloorExport(t, ctx, pool)

	r := readConfirmRow(t, ctx, pool, id)
	if r.sentExternalID != nil {
		t.Errorf("sent_external_id = %v on a row whose click happened an hour AFTER the exported message; "+
			"that message cannot be this send", r.sentExternalID)
	}
	if r.confirmedAt != nil {
		t.Error("confirmed_at stamped from a message predating the send attempt")
	}
	if r.status != "sending" {
		t.Errorf("status = %q, want still sending — the row stays for the reconciler to flag", r.status)
	}
	if n := confirmEventCount(t, ctx, pool, taskID, "delivery_confirmed"); n != 0 {
		t.Errorf("delivery_confirmed events = %d, want 0", n)
	}
}

// The skew allowance must not be so tight that a database a few seconds ahead of
// Slack's clock refuses a legitimate confirmation permanently.
func TestSlackFloor_Integration_ToleratesSmallClockSkew(t *testing.T) {
	ctx := context.Background()
	pool := newConfirmPool(t, ctx)
	taskID := seedConfirmTask(t, ctx, pool)
	id := seedFloorRow(t, ctx, pool, taskID, `+ interval '30 seconds'`)

	runFloorExport(t, ctx, pool)

	if r := readConfirmRow(t, ctx, pool, id); r.sentExternalID == nil {
		t.Error("a click stamped 30s ahead of the message was refused; pg and Slack keep different clocks, " +
			"and an unforgiving floor turns skew into a permanently unconfirmable row")
	}
}
