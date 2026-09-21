//go:build integration

package tools_test

// SWT-71 (gmail-sending-stuck). A send whose process died between the network
// call and the finalize leaves the row `sending` with its sent_external_id set.
// The message may well have left: when its copy re-enters ingestion the gmail
// sink stamps confirmed_at — deliberately WITHOUT promoting the status, because
// a promotion there would emit no delivery_sent and R8 would never fire. So the
// row sat in `sending` forever and no verb could move it (mark_delivery_sent is
// the assisted tier's; send_delivery refused "never resend").
//
// The transition belongs to send_delivery: on a gmail row that is `sending`
// AND confirmed, it FINISHES the send — no transport call, status sent, sent_at
// from the confirmation, and the delivery_sent event R8 needs. An UNCONFIRMED
// `sending` row still refuses: nothing proves that message left.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/tools"
)

func TestSendDelivery_Integration_FinishesAConfirmedSendingRow(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()
	cleanupDeliveryData(t, ctx, pool)
	defer cleanupDeliveryData(t, ctx, pool)

	fx := seedDeliveryFixture(t, ctx, pool)
	fake := &fakeGmailSender{pool: pool}
	tools.SetGmailSender(fake)
	ex := deliveryExecutor(pool)

	id, err := draftGmailCc(t, ctx, ex, fx.parentID, fx.threadID, `[]`)
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	approve(t, ctx, ex, id)
	// The state a died send leaves behind: sending + the reserved Message-ID.
	if _, err := pool.Exec(ctx,
		`UPDATE deliveries SET status='sending', sent_external_id='<sb-itest-stuck@example.com>',
		        send_attempted_at=now() - interval '3 minutes' WHERE id=$1`, id); err != nil {
		t.Fatalf("seed the stuck row: %v", err)
	}
	send := func() error {
		_, err := ex.Execute(ctx, executor.Call{Tool: "send_delivery", Actor: delActor,
			Args: []byte(`{"delivery_id":` + itoa(id) + `}`)})
		return err
	}

	// UNCONFIRMED: nothing proves it left, and it must never be sent twice.
	if err := send(); err == nil || !strings.Contains(err.Error(), "invariant 4") {
		t.Fatalf("send_delivery on an unconfirmed sending row = %v, want the invariant-4 refusal", err)
	}

	// CONFIRMED by loop closure, status untouched — exactly what the sink does.
	if _, err := pool.Exec(ctx,
		`UPDATE deliveries SET confirmed_at = now() - interval '1 minute' WHERE id=$1`, id); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	// The work was closed in the meantime ("reply sent" is a common reason to
	// close it). Finishing is record-keeping for a message that already left,
	// so the closed-task refusal must not strand the row.
	if _, err := pool.Exec(ctx, `UPDATE tasks SET status='closed', closed_at=now() WHERE id=$1`, fx.parentID); err != nil {
		t.Fatalf("close the task: %v", err)
	}
	if err := send(); err != nil {
		t.Fatalf("send_delivery on a CONFIRMED sending row = %v, want it to finish the send", err)
	}
	if fake.calls != 0 {
		t.Errorf("transport calls = %d, want 0: finishing a confirmed send must never touch the network", fake.calls)
	}
	var status string
	var sentAtIsConfirmedAt bool
	if err := pool.QueryRow(ctx,
		`SELECT status, sent_at = confirmed_at FROM deliveries WHERE id=$1`, id).Scan(&status, &sentAtIsConfirmedAt); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if status != "sent" || !sentAtIsConfirmedAt {
		t.Errorf("row = status %q, sent_at==confirmed_at %v; want sent, with sent_at taken from the confirmation",
			status, sentAtIsConfirmedAt)
	}
	var payload []byte
	if err := pool.QueryRow(ctx,
		`SELECT payload FROM task_events WHERE task_id=$1 AND event_type='delivery_sent' ORDER BY id DESC LIMIT 1`,
		fx.parentID).Scan(&payload); err != nil {
		t.Fatalf("no delivery_sent event: %v — without it R8 never fires and the work task sits at done_locally", err)
	}
	var ev struct {
		DeliveryID int64  `json:"delivery_id"`
		Channel    string `json:"channel"`
		ExternalID string `json:"sent_external_id"`
		Recovered  bool   `json:"recovered"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if ev.DeliveryID != id || ev.Channel != "gmail" || ev.ExternalID != "<sb-itest-stuck@example.com>" || !ev.Recovered {
		t.Errorf("delivery_sent payload = %s; want this delivery, channel gmail, the reserved id, recovered=true", payload)
	}

	// And once finished it is an ordinary sent row: refused forever, one event.
	// (The task is closed by now, so the refusal that answers is the closed-task
	// one; either way nothing is sent and no second event is written.)
	if err := send(); err == nil {
		t.Errorf("a second finish was accepted; a sent row refuses forever")
	}
	if fake.calls != 0 {
		t.Errorf("transport calls = %d after the second attempt, want 0", fake.calls)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_sent'`, fx.parentID).Scan(&n)
	if n != 1 {
		t.Errorf("delivery_sent events = %d, want exactly 1", n)
	}
}
