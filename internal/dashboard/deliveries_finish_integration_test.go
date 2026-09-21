//go:build integration

package dashboard_test

// SWT-71: a gmail row stuck in `sending` shows "Finish: it was sent" only when
// the proof send_delivery accepts is there — confirmed AND the composed message,
// by its own Message-ID, is in the ingested mailbox. Both facts come from
// columns, so this runs against a real database: dropping either from the
// page's SELECT must turn it red.

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

func TestDashboard_Integration_FinishButtonNeedsTheIngestedCopy(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	const mid = "<sb-itest-dash-cc-stuck@example.com>"
	dropCopy := func() {
		if _, err := pool.Exec(ctx, `DELETE FROM normalized_messages WHERE external_message_id=$1`, mid); err != nil {
			t.Fatalf("cleanup the own copy: %v", err)
		}
	}
	dropCopy()
	cleanupDlvCc(t, ctx, pool)
	defer cleanupDlvCc(t, ctx, pool)
	defer dropCopy() // LIFO: before cleanupDlvCc deletes the thread it points at
	sd := seedDlvCc(t, ctx, pool)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	if _, err := pool.Exec(ctx,
		`UPDATE deliveries SET status='sending', sent_external_id=$2, send_attempted_at=now(), confirmed_at=now()
		  WHERE id=$1`, sd.draftedID, mid); err != nil {
		t.Fatalf("make the row stuck: %v", err)
	}
	const button = "Finish: it was sent"

	_, page := get(t, client, ts.URL+"/deliveries")
	if row := rowOf(t, page, sd.draftedID); strings.Contains(row, button) {
		t.Errorf("a sending row confirmed WITHOUT its own copy ingested shows %q; a body-prefix confirmation is "+
			"not proof the composed message left", button)
	}

	// The own copy arrives: same Message-ID, outbound.
	// (One normalized message per raw item, so the copy gets its own; the raw row
	// goes with the account in cleanupDlvCc.)
	if _, err := pool.Exec(ctx,
		`WITH raw AS (
		   INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		   SELECT id, 'itest-dash-cc-raw-stuck', '{}', 'itest-dash-cc-hash-stuck' FROM source_accounts WHERE account_email=$2
		   RETURNING id)
		 INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 SELECT raw.id, m.thread_id, 'outbound', $1, now(), 'Thursday works.', 'Re: Rochester schedule', $2, 'gmail'
		   FROM raw, normalized_messages m WHERE m.external_message_id = $3`, mid, ccAcct, ccInboundID); err != nil {
		t.Fatalf("ingest the own copy: %v", err)
	}
	_, page = get(t, client, ts.URL+"/deliveries")
	row := rowOf(t, page, sd.draftedID)
	if !strings.Contains(row, button) {
		t.Fatalf("a sending row that is confirmed and whose own copy is ingested shows no %q button. Row: %s", button, row)
	}
	if !strings.Contains(row, `/deliveries/`+strconv.FormatInt(sd.draftedID, 10)+`/send`) {
		t.Errorf("the button does not post to the send route (send_delivery owns the transition). Row: %s", row)
	}

	// Unconfirmed: no button, even with the copy present.
	if _, err := pool.Exec(ctx, `UPDATE deliveries SET confirmed_at=NULL WHERE id=$1`, sd.draftedID); err != nil {
		t.Fatalf("unconfirm: %v", err)
	}
	_, page = get(t, client, ts.URL+"/deliveries")
	if strings.Contains(rowOf(t, page, sd.draftedID), button) {
		t.Errorf("an UNCONFIRMED sending row shows %q", button)
	}
}
