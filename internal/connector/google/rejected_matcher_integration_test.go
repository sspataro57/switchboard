//go:build integration

package google_test

// SWT-43 (docs/tickets/delivery-deny_SPEC.md) criterion 15, google half:
// confirmDeliveryByBodyPrefix (the gmail belt) never stamps a REJECTED row.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run GmailBeltRejected ./internal/connector/google/
//
// The belt runs when an outbound message's Message-ID matches no delivery (a
// relay rewrote it — or Salvador sent the words himself). A rejected row
// shares the belt's scope (same from_account_id) and, here, the SAME
// normalized 120-char prefix as the observed message; the two bodies differ in
// raw whitespace (IK: "a matcher test whose two bodies are the same string
// tests nothing").
//
// The CONTROL runs the identical fixture with the row at 'failed' (the belt's
// own candidate set) and must confirm — proving the fixture reaches the belt,
// so the rejected case's "untouched" is the status set's doing.
//
// Mutation: add 'rejected' to the belt's status list -> the stamp is attempted;
// deliveries_rejected_unsent_check refuses it and Normalize errors -> red.
//
// GREENFIELD NOTE — EXPECTED RED: before 0028 the rejected fixture cannot be
// seeded (deliveries_status_check).
//
// Cross-suite discipline: owns account itest-gdeny-a@example.com, project
// itest-gdeny-proj, thread keys gmail:itest-gdeny-%; cleaned in FK order at
// start and end of each subtest. The message is OUTBOUND, so triage's
// inbound-only filter never sees it.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/textmatch"
)

const (
	gdnEmail   = "itest-gdeny-a@example.com"
	gdnSlug    = "itest-gdeny-proj"
	gdnGThread = "itest-gdeny-thread-1"
	gdnRawExt  = "gmail:itest-gdeny-msg-1"
	gdnMsgID   = "<hand-sent-copy-itest-gdeny@mail.example.test>"
	// Stored: what draft_delivery persisted. Observed: what came back through
	// ingestion. Same words; different runs of spaces and blank lines.
	gdnStored   = "Pushed the staging fix and re-ran the migration,  so the queue   is draining now.\n\n\nWill confirm tonight once the backlog clears."
	gdnObserved = "Pushed the staging fix and re-ran the migration, so the queue is draining now.\nWill confirm tonight once the backlog clears."
)

func cleanupGmailDeny(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + gdnSlug + `'))`
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE account_email='` + gdnEmail + `'))`
	for _, q := range []string{
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM deliveries WHERE task_id IN ` + tasksOf,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'gmail:itest-gdeny-%'`,
		`DELETE FROM raw_source_items WHERE id IN ` + raws,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + gdnSlug + `')`,
		`DELETE FROM projects WHERE slug='` + gdnSlug + `'`,
		`DELETE FROM source_accounts WHERE account_email='` + gdnEmail + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func TestGmailBeltRejected_Integration_NeverConfirmsARejectedRow(t *testing.T) {
	requireCompose(t)
	if gdnStored == gdnObserved ||
		textmatch.NormalizedPrefix(gdnStored, 120) != textmatch.NormalizedPrefix(gdnObserved, 120) {
		t.Fatalf("fixture invalid: the bodies must differ raw and agree normalized")
	}

	for _, tc := range []struct {
		name, status  string
		wantConfirmed bool
	}{
		{"control: a definitely-failed row IS confirmed by the belt", "failed", true},
		{"a rejected row is never confirmed", "rejected", false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool, err := store.NewPool(ctx)
			if err != nil {
				t.Fatalf("store.NewPool: %v", err)
			}
			defer pool.Close()
			cleanupGmailDeny(t, ctx, pool)
			defer cleanupGmailDeny(t, ctx, pool)

			var acctID, projID, taskID, deliveryID int64
			if err := pool.QueryRow(ctx,
				`INSERT INTO source_accounts (provider, account_email, send_enabled) VALUES ('google',$1,true) RETURNING id`,
				gdnEmail).Scan(&acctID); err != nil {
				t.Fatalf("seed account: %v", err)
			}
			if err := pool.QueryRow(ctx,
				`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
				 VALUES ($1,$1,'itest-gdeny-client','manual','dashboard','/tmp/itest','any') RETURNING id`, gdnSlug).Scan(&projID); err != nil {
				t.Fatalf("seed project: %v", err)
			}
			if err := pool.QueryRow(ctx,
				`INSERT INTO tasks (project_id, title, assignee_type, status) VALUES ($1,'gdeny work','claude','done_locally') RETURNING id`,
				projID).Scan(&taskID); err != nil {
				t.Fatalf("seed task: %v", err)
			}
			q := `INSERT INTO deliveries (task_id, channel, body, status, from_account_id) VALUES ($1,'gmail',$2,$3,$4) RETURNING id`
			args := []any{taskID, gdnStored, tc.status, acctID}
			if tc.status == "rejected" {
				q = `INSERT INTO deliveries (task_id, channel, body, status, from_account_id, rejection_note)
				     VALUES ($1,'gmail',$2,$3,$4,'denied: wrong tone') RETURNING id`
			}
			if err := pool.QueryRow(ctx, q, args...).Scan(&deliveryID); err != nil {
				t.Fatalf("seed %s gmail delivery: %v (a 'rejected' row needs migration 0028)", tc.status, err)
			}

			full := gmailFull("gdeny-msg", gdnGThread, gdnMsgID, gdnEmail, "client@itest-gdeny.example",
				time.Now().Add(-time.Minute).UnixMilli(), gdnObserved)
			if _, err := pool.Exec(ctx,
				`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
				 VALUES ($1,$2,$3,'itest-gdeny-hash-1')`, acctID, gdnRawExt, full); err != nil {
				t.Fatalf("seed raw item: %v", err)
			}
			if _, err := google.Normalize(ctx, google.NewPGSink(pool), google.Config{}); err != nil {
				t.Fatalf("Normalize: %v", err)
			}

			var status string
			var extID, confirmedAt *string
			if err := pool.QueryRow(ctx,
				`SELECT status, sent_external_id, confirmed_at::text FROM deliveries WHERE id=$1`, deliveryID).
				Scan(&status, &extID, &confirmedAt); err != nil {
				t.Fatalf("read delivery: %v", err)
			}
			events := scanInt(t, ctx, pool,
				`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_confirmed'`, taskID)

			if tc.wantConfirmed {
				if confirmedAt == nil || events != 1 {
					t.Fatalf("CONTROL FAILED: the belt did not confirm a 'failed' row (confirmed_at=%v, events=%d); "+
						"the fixture never reaches confirmDeliveryByBodyPrefix, so the rejected case proves nothing",
						confirmedAt, events)
				}
				return
			}
			if status != "rejected" || extID != nil || confirmedAt != nil {
				t.Errorf("the belt touched a REJECTED row: status=%q sent_external_id=%v confirmed_at=%v. Criterion 15: "+
					"'rejected' means switchboard did not and will not send this row", status, extID, confirmedAt)
			}
			if events != 0 {
				t.Errorf("delivery_confirmed events = %d for a rejected row, want 0", events)
			}
		})
	}
}
