//go:build integration

package tools_test

// SWT-8 review finding 1: send failure semantics. A DEFINITE gmail API
// rejection (google.SendRejectedError) clears the reserved sent_external_id so
// approve_delivery's failed->approved retry path is reachable; an AMBIGUOUS
// transport error keeps the id (the send may have gone through — invariant 4
// errs toward never-resend, leaving the row failed and un-retryable via tools).

import (
	"context"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/tools"
)

// rejectingSender always returns a definite API rejection.
type rejectingSender struct{ calls int }

func (s *rejectingSender) Send(_ context.Context, _ string, _ []byte, _ string) (string, error) {
	s.calls++
	return "", &google.SendRejectedError{Status: 400, Body: "Invalid To header"}
}

// flakySender always returns an ambiguous transport error.
type flakySender struct{ calls int }

func (s *flakySender) Send(_ context.Context, _ string, _ []byte, _ string) (string, error) {
	s.calls++
	return "", context.DeadlineExceeded
}

func TestDelivery_Integration_FailureRetrySemantics(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()

	cleanupDeliveryData(t, ctx, pool)
	defer cleanupDeliveryData(t, ctx, pool)
	fx := seedDeliveryFixture(t, ctx, pool)
	ex := deliveryExecutor(pool)

	// ---- definite rejection: sent_external_id cleared, retry reachable ------
	rej := &rejectingSender{}
	tools.SetGmailSender(rej)

	d1 := draftGmail(t, ctx, ex, fx.parentID, fx.threadID)
	approve(t, ctx, ex, d1)
	if _, err := ex.Execute(ctx, executor.Call{Tool: "send_delivery", Actor: delActor,
		Args: []byte(`{"delivery_id":` + itoa(d1) + `}`)}); err == nil {
		t.Fatal("send with a rejecting adapter must fail")
	}
	var status string
	var extID *string
	if err := pool.QueryRow(ctx,
		`SELECT status, sent_external_id FROM deliveries WHERE id=$1`, d1).Scan(&status, &extID); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if status != "failed" {
		t.Errorf("status after definite rejection = %q, want failed", status)
	}
	if extID != nil {
		t.Errorf("sent_external_id after definite rejection = %v, want NULL (retry must be reachable)", *extID)
	}
	// The failed->approved retry path is live.
	approve(t, ctx, ex, d1)
	if s := deliveryStatus(t, ctx, pool, d1); s != "approved" {
		t.Fatalf("failed->approved retry: status = %q, want approved", s)
	}

	// ---- ambiguous transport error: id kept, retry blocked ------------------
	fl := &flakySender{}
	tools.SetGmailSender(fl)

	d2 := draftGmail(t, ctx, ex, fx.parentID, fx.threadID)
	approve(t, ctx, ex, d2)
	if _, err := ex.Execute(ctx, executor.Call{Tool: "send_delivery", Actor: delActor,
		Args: []byte(`{"delivery_id":` + itoa(d2) + `}`)}); err == nil {
		t.Fatal("send with a flaky adapter must fail")
	}
	if err := pool.QueryRow(ctx,
		`SELECT status, sent_external_id FROM deliveries WHERE id=$1`, d2).Scan(&status, &extID); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if status != "failed" {
		t.Errorf("status after transport error = %q, want failed", status)
	}
	if extID == nil || !strings.HasPrefix(*extID, "<sb-") {
		t.Errorf("sent_external_id after transport error = %v, want the reserved id KEPT (send may have gone through)", extID)
	}
	// approve is refused: the id is present.
	if _, err := ex.Execute(ctx, executor.Call{Tool: "approve_delivery", Actor: delActor,
		Args: []byte(`{"delivery_id":` + itoa(d2) + `}`)}); err == nil {
		t.Error("re-approve with a reserved sent id must be refused (manual investigation path)")
	}
}
