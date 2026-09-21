//go:build integration

package tools_test

// SWT-71 (gmail-sending-stuck). A send whose process died between the network
// call and the finalize leaves the row `sending` with its sent_external_id set.
// When the message's own copy re-enters ingestion the gmail sink stamps
// confirmed_at — deliberately WITHOUT promoting the status, because a promotion
// there would emit no delivery_sent and R8 would never fire. The row then sat in
// `sending` forever: mark_delivery_sent is the assisted tier's verb and
// send_delivery refused "never resend".
//
// send_delivery now FINISHES such a row: no transport call, status sent, sent_at
// from the confirmation, and the task event in the same transaction. The proof
// it accepts is the strong one only — the composed message, by its own
// Message-ID, is in the ingested mailbox. confirmed_at alone (the body-prefix
// belt stamps it too) finishes nothing, and neither does an unconfirmed row.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	stuckMID = "<sb-itest-stuck@example.com>"
	stuckRaw = "itest-del-stuck-raw"
)

func stuckCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, q := range []string{
		`DELETE FROM normalized_messages WHERE external_message_id = '` + stuckMID + `'`,
		`DELETE FROM raw_source_items WHERE external_id = '` + stuckRaw + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

// seedStuck drafts and approves a gmail delivery, then puts it in the state a
// died send leaves behind: sending + the reserved Message-ID.
func seedStuck(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ex *executor.Executor, fx delFixture) int64 {
	t.Helper()
	id, err := draftGmailCc(t, ctx, ex, fx.parentID, fx.threadID, `[]`)
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	approve(t, ctx, ex, id)
	if _, err := pool.Exec(ctx,
		`UPDATE deliveries SET status='sending', sent_external_id=$2,
		        send_attempted_at=now() - interval '3 minutes' WHERE id=$1`, id, stuckMID); err != nil {
		t.Fatalf("seed the stuck row: %v", err)
	}
	return id
}

// ingestOwnCopy is what the mailbox sync does when the sent message comes back:
// the outbound copy lands under the delivery's own Message-ID.
func ingestOwnCopy(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fx delFixture) {
	t.Helper()
	var rawID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1,$2,'{}','itest-del-stuck-hash') RETURNING id`, fx.accountID, stuckRaw).Scan(&rawID); err != nil {
		t.Fatalf("seed raw: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1,$2,'outbound',$3, now(), 'draft body', 'Re: login broken', 'itest-del-a@example.com', 'gmail')`,
		rawID, fx.threadID, stuckMID); err != nil {
		t.Fatalf("seed the own copy: %v", err)
	}
}

func sendErr(ctx context.Context, ex *executor.Executor, id int64) error {
	_, err := ex.Execute(ctx, executor.Call{Tool: "send_delivery", Actor: delActor,
		Args: []byte(`{"delivery_id":` + itoa(id) + `}`)})
	return err
}

func countEvents(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64, typ string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type=$2`, taskID, typ).Scan(&n); err != nil {
		t.Fatalf("count %s events: %v", typ, err)
	}
	return n
}

func TestSendDelivery_Integration_FinishesAConfirmedSendingRow(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()
	stuckCleanup(t, ctx, pool)
	cleanupDeliveryData(t, ctx, pool)
	defer cleanupDeliveryData(t, ctx, pool)
	defer stuckCleanup(t, ctx, pool) // LIFO: before cleanupDeliveryData

	fx := seedDeliveryFixture(t, ctx, pool)
	fake := &fakeGmailSender{pool: pool}
	tools.SetGmailSender(fake)
	ex := deliveryExecutor(pool)
	id := seedStuck(t, ctx, pool, ex, fx)

	// 1. UNCONFIRMED: nothing proves it left, and it must never be sent twice.
	if err := sendErr(ctx, ex, id); err == nil || !strings.Contains(err.Error(), "invariant 4") {
		t.Fatalf("send_delivery on an unconfirmed sending row = %v, want the invariant-4 refusal", err)
	}

	// 2. confirmed_at ALONE is not proof: the sink's body-prefix belt stamps it
	// from any message of the same mailbox that opens with the same words.
	if _, err := pool.Exec(ctx, `UPDATE deliveries SET confirmed_at = now() - interval '1 minute' WHERE id=$1`, id); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := sendErr(ctx, ex, id); err == nil || !strings.Contains(err.Error(), "invariant 4") {
		t.Fatalf("send_delivery on a row confirmed WITHOUT its own copy ingested = %v, want the invariant-4 refusal: "+
			"a body-prefix confirmation must not finish a send", err)
	}

	// 3. The composed message, by its own Message-ID, is in the mailbox: finish.
	ingestOwnCopy(t, ctx, pool, fx)
	if err := sendErr(ctx, ex, id); err != nil {
		t.Fatalf("send_delivery on a confirmed sending row whose own copy is ingested = %v, want it to finish", err)
	}
	if fake.calls != 0 {
		t.Errorf("transport calls = %d, want 0: finishing a confirmed send must never touch the network", fake.calls)
	}
	var status string
	var sentAtIsConfirmedAt bool
	if err := pool.QueryRow(ctx,
		`SELECT d.status, d.sent_at = (SELECT nm.sent_at FROM normalized_messages nm WHERE nm.external_message_id = d.sent_external_id)
		   FROM deliveries d WHERE d.id=$1`, id).Scan(&status, &sentAtIsConfirmedAt); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if status != "sent" || !sentAtIsConfirmedAt {
		t.Errorf("row = status %q, sent_at==the copy's sent_at %v; want sent, with the TRUE send instant from the ingested copy", status, sentAtIsConfirmedAt)
	}
	var payload []byte
	if err := pool.QueryRow(ctx,
		`SELECT payload FROM task_events WHERE task_id=$1 AND event_type='delivery_sent' ORDER BY id DESC LIMIT 1`,
		fx.parentID).Scan(&payload); err != nil {
		t.Fatalf("no delivery_sent event: %v — on an OPEN task R8 needs it, or the work sits at done_locally", err)
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
	if ev.DeliveryID != id || ev.Channel != "gmail" || ev.ExternalID != stuckMID || !ev.Recovered {
		t.Errorf("delivery_sent payload = %s; want this delivery, channel gmail, the reserved id, recovered=true", payload)
	}

	// 4. Finished, it is an ordinary sent row: invariant 4 forever, one event.
	if err := sendErr(ctx, ex, id); err == nil || !strings.Contains(err.Error(), "invariant 4") {
		t.Errorf("a second finish = %v, want the invariant-4 refusal", err)
	}
	if n := countEvents(t, ctx, pool, fx.parentID, "delivery_sent"); n != 1 {
		t.Errorf("delivery_sent events = %d, want exactly 1", n)
	}
	if fake.calls != 0 {
		t.Errorf("transport calls = %d, want 0", fake.calls)
	}
}

// The task was closed since ("reply sent" is a common reason to close it). The
// finish must still run — it sends nothing — but it must NOT emit delivery_sent:
// R8 would change nothing on a closed task and would still record its
// delivery_lifecycle key, muting a later real delivery if the task is reopened.
func TestSendDelivery_Integration_FinishOnAClosedTaskWritesALogNotDeliverySent(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()
	stuckCleanup(t, ctx, pool)
	cleanupDeliveryData(t, ctx, pool)
	defer cleanupDeliveryData(t, ctx, pool)
	defer stuckCleanup(t, ctx, pool)

	fx := seedDeliveryFixture(t, ctx, pool)
	fake := &fakeGmailSender{pool: pool}
	tools.SetGmailSender(fake)
	ex := deliveryExecutor(pool)
	id := seedStuck(t, ctx, pool, ex, fx)
	if _, err := pool.Exec(ctx, `UPDATE deliveries SET confirmed_at = now() WHERE id=$1`, id); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	ingestOwnCopy(t, ctx, pool, fx)
	if _, err := pool.Exec(ctx, `UPDATE tasks SET status='closed', closed_at=now() WHERE id=$1`, fx.parentID); err != nil {
		t.Fatalf("close the task: %v", err)
	}

	if err := sendErr(ctx, ex, id); err != nil {
		t.Fatalf("finish on a closed task = %v, want it to finish: the message already left", err)
	}
	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM deliveries WHERE id=$1`, id).Scan(&status)
	if status != "sent" {
		t.Errorf("status = %q, want sent", status)
	}
	if n := countEvents(t, ctx, pool, fx.parentID, "delivery_sent"); n != 0 {
		t.Errorf("delivery_sent events on a CLOSED task = %d, want 0 (R8 would record a lifecycle key for a "+
			"transition that never happens)", n)
	}
	var kind string
	if err := pool.QueryRow(ctx,
		`SELECT payload->>'kind' FROM task_events WHERE task_id=$1 AND event_type='log'
		  AND payload->>'kind'='delivery_finished' ORDER BY id DESC LIMIT 1`, fx.parentID).Scan(&kind); err != nil {
		t.Errorf("no delivery_finished log event on the closed task: %v", err)
	}
	if fake.calls != 0 {
		t.Errorf("transport calls = %d, want 0", fake.calls)
	}
}

// finishingSender plays the race the review named: the send is in flight (row
// `sending`), the own copy is ingested and confirmed, a REAL Finish lands through
// the executor — and only then does the transport return. Whatever it returns,
// phase 2 must not overwrite the finished row: no `failed` over `sent`, no
// cleared Message-ID, and exactly one delivery_sent.
type finishingSender struct {
	t      *testing.T
	pool   *pgxpool.Pool
	ex     *executor.Executor
	fx     delFixture
	id     int64
	result error
}

func (f *finishingSender) Send(ctx context.Context, _ string, _ []byte, _ string) (string, error) {
	var mid string
	if err := f.pool.QueryRow(ctx, `SELECT sent_external_id FROM deliveries WHERE id=$1`, f.id).Scan(&mid); err != nil {
		f.t.Fatalf("read the reserved Message-ID mid-send: %v", err)
	}
	var rawID int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1,$2,'{}','itest-del-stuck-hash') RETURNING id`, f.fx.accountID, stuckRaw).Scan(&rawID); err != nil {
		f.t.Fatalf("seed raw mid-send: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1,$2,'outbound',$3, now(), 'draft body', 'Re: login broken', 'itest-del-a@example.com', 'gmail')`,
		rawID, f.fx.threadID, mid); err != nil {
		f.t.Fatalf("ingest the own copy mid-send: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE deliveries SET confirmed_at=now() WHERE id=$1`, f.id); err != nil {
		f.t.Fatalf("confirm mid-send: %v", err)
	}
	if err := sendErr(ctx, f.ex, f.id); err != nil {
		f.t.Fatalf("the Finish that lands mid-send = %v, want it to finish", err)
	}
	return "", f.result
}

func TestSendDelivery_Integration_AnInFlightSendNeverOverwritesAFinishedRow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result error
	}{
		{"ambiguous transport error", errors.New("dial tcp: i/o timeout")},
		{"definite rejection", &google.SendRejectedError{Status: 400, Body: "rejected"}},
		{"success", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newToolsPool(t, ctx)
			defer pool.Close()
			cleanMid := func() {
				for _, q := range []string{
					`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE external_id='` + stuckRaw + `')`,
					`DELETE FROM raw_source_items WHERE external_id='` + stuckRaw + `'`,
				} {
					if _, err := pool.Exec(ctx, q); err != nil {
						t.Fatalf("cleanup %q: %v", q, err)
					}
				}
			}
			cleanMid()
			cleanupDeliveryData(t, ctx, pool)
			defer cleanupDeliveryData(t, ctx, pool)
			defer cleanMid()

			fx := seedDeliveryFixture(t, ctx, pool)
			ex := deliveryExecutor(pool)
			id, err := draftGmailCc(t, ctx, ex, fx.parentID, fx.threadID, `[]`)
			if err != nil {
				t.Fatalf("draft: %v", err)
			}
			approve(t, ctx, ex, id)
			tools.SetGmailSender(&finishingSender{t: t, pool: pool, ex: ex, fx: fx, id: id, result: tc.result})
			_ = sendErr(ctx, ex, id)

			var status string
			var hasID bool
			if err := pool.QueryRow(ctx,
				`SELECT status, sent_external_id IS NOT NULL FROM deliveries WHERE id=$1`, id).Scan(&status, &hasID); err != nil {
				t.Fatalf("read row: %v", err)
			}
			if status != "sent" || !hasID {
				t.Errorf("after the in-flight send returned: status %q, has Message-ID %v; want the finished row "+
					"untouched (sent, id kept) — `failed` here undoes a delivery R8 processed, and a cleared id "+
					"re-opens a resend", status, hasID)
			}
			if n := countEvents(t, ctx, pool, fx.parentID, "delivery_sent"); n != 1 {
				t.Errorf("delivery_sent events = %d, want exactly 1 (the finish's; the in-flight send must add none)", n)
			}
		})
	}
}

// A definite rejection on a row that is still `sending` is ALWAYS recorded, even
// when the body-prefix belt has stamped confirmed_at in the meantime: gmail has no
// reconciler, so a skipped write would be a silent wedge. Only the Message-ID is
// kept in that case.
type beltThenRejectSender struct {
	pool *pgxpool.Pool
	id   int64
}

func (r *beltThenRejectSender) Send(ctx context.Context, _ string, _ []byte, _ string) (string, error) {
	_, _ = r.pool.Exec(ctx, `UPDATE deliveries SET confirmed_at=now() WHERE id=$1`, r.id) // the belt, mid-send
	return "", &google.SendRejectedError{Status: 400, Body: "rejected"}
}

func TestSendDelivery_Integration_ARejectionOnABeltConfirmedRowIsStillRecorded(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()
	cleanupDeliveryData(t, ctx, pool)
	defer cleanupDeliveryData(t, ctx, pool)

	fx := seedDeliveryFixture(t, ctx, pool)
	ex := deliveryExecutor(pool)
	id, err := draftGmailCc(t, ctx, ex, fx.parentID, fx.threadID, `[]`)
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	approve(t, ctx, ex, id)
	tools.SetGmailSender(&beltThenRejectSender{pool: pool, id: id})
	if err := sendErr(ctx, ex, id); err == nil {
		t.Fatalf("a rejected send returned no error")
	}
	var status, errText string
	var hasID bool
	if err := pool.QueryRow(ctx,
		`SELECT status, COALESCE(error,''), sent_external_id IS NOT NULL FROM deliveries WHERE id=$1`, id).
		Scan(&status, &errText, &hasID); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if status != "failed" || errText == "" {
		t.Errorf("row = status %q, error %q; want failed with the rejection recorded — a skipped write leaves it "+
			"silently `sending` on a channel with no reconciler", status, errText)
	}
	if !hasID {
		t.Errorf("the Message-ID was cleared on a CONFIRMED row; a confirmed row never loses its id")
	}
}
