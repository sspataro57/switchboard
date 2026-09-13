//go:build integration

package upworkcrm_test

// SWT-43 (docs/tickets/delivery-deny_SPEC.md) criteria 15 and 16, upwork half:
// confirmUpworkDelivery never stamps a REJECTED upwork_chat row, and
// ReconcileUnconfirmed never flags one.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run UpworkRejected ./internal/connector/upworkcrm/
//
// Matcher: the row sits in the matcher's own scope (same client and
// conversation, the post-0019 identity columns set) and its body has the SAME
// normalized prefix as the observed outbound communication while differing in
// raw whitespace — matcherhardening's umhStoredBody / umhObservedBody pair,
// whose validity that suite already checks. The control seeds the identical
// fixture at 'sent' (the matcher's only status) and must be stamped.
//
// Reconciler: a 'sent' control beside the rejected row must be flagged, or the
// rejected row's clean state proves nothing.
//
// GREENFIELD NOTE — EXPECTED RED: before 0028 no rejected fixture can be seeded.
//
// Cross-suite discipline: the matcher owns client dddddddd-…-e1 and project
// itest-udeny-proj, cleaned in FK order (person_identities by value, orphan
// people) at start and end of each subtest; the reconciler reuses
// reconcile_integration_test.go's itest-rc corpus and cleanup.

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/upworkcrm"
)

const (
	udnClient = "dddddddd-0000-0000-0000-0000000000e1"
	udnComm   = "dddddddd-0000-0000-0000-0000000000f1"
	udnExtID  = "upwork-room-msg-itest-udeny-901"
	udnSlug   = "itest-udeny-proj"
)

func cleanupUpworkDeny(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	key := umhThreadKey(udnClient, umhChannel)
	stmts := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM task_events WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{udnSlug}},
		{`DELETE FROM deliveries WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{udnSlug}},
		{`DELETE FROM normalized_messages WHERE external_message_id=$1`, []any{udnExtID}},
		{`DELETE FROM normalized_messages WHERE thread_id IN (SELECT id FROM normalized_threads WHERE thread_key=$1)`, []any{key}},
		{`DELETE FROM normalized_threads WHERE thread_key=$1`, []any{key}},
		{`DELETE FROM raw_source_items WHERE external_id=$1 AND source_account_id IN
			(SELECT id FROM source_accounts WHERE provider=$2 AND account_email=$3)`,
			[]any{"communications:" + udnComm, upworkcrm.Provider, upworkcrm.AccountEmail}},
		{`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, []any{udnSlug}},
		{`DELETE FROM projects WHERE slug=$1`, []any{udnSlug}},
		{`DELETE FROM person_identities WHERE provider=$1 AND value=$2`, []any{upworkcrm.Provider, udnClient}},
		{`DELETE FROM people WHERE id NOT IN (SELECT person_id FROM person_identities)`, nil},
	}
	for _, st := range stmts {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

func TestUpworkRejected_Integration_MatcherNeverStampsARejectedRow(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		wantStamped  bool
	}{
		{"control: a sent row IS stamped", "sent", true},
		{"a rejected row is never stamped", "rejected", false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx, pool, acctID := umhOpen(t)
			defer pool.Close()
			cleanupUpworkDeny(t, ctx, pool)
			defer cleanupUpworkDeny(t, ctx, pool)

			taskID := umhSeedProjectTask(t, ctx, pool, udnSlug, "itest-udeny-client")
			key := umhThreadKey(udnClient, umhChannel)
			sentAt := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)

			if _, err := pool.Exec(ctx,
				`INSERT INTO normalized_threads (thread_key, participants) VALUES ($1,'[]')
				 ON CONFLICT (thread_key) WHERE thread_key IS NOT NULL DO NOTHING`, key); err != nil {
				t.Fatalf("seed thread: %v", err)
			}
			var threadID, deliveryID int64
			if err := pool.QueryRow(ctx, `SELECT id FROM normalized_threads WHERE thread_key=$1`, key).Scan(&threadID); err != nil {
				t.Fatalf("read thread id: %v", err)
			}
			if tc.status == "sent" {
				err := pool.QueryRow(ctx,
					`INSERT INTO deliveries (task_id, channel, target_ref, target_client_ref, thread_id, body, status, sent_at)
					 VALUES ($1,'upwork_chat',$2,$3,$4,$5,'sent',$6) RETURNING id`,
					taskID, key, udnClient, threadID, umhStoredBody, sentAt).Scan(&deliveryID)
				if err != nil {
					t.Fatalf("seed sent delivery: %v", err)
				}
			} else {
				err := pool.QueryRow(ctx,
					`INSERT INTO deliveries (task_id, channel, target_ref, target_client_ref, thread_id, body, status, rejection_note)
					 VALUES ($1,'upwork_chat',$2,$3,$4,$5,'rejected','denied: I will write it myself') RETURNING id`,
					taskID, key, udnClient, threadID, umhStoredBody).Scan(&deliveryID)
				if err != nil {
					t.Fatalf("seed rejected delivery: %v (needs migration 0028)", err)
				}
			}

			umhSeedRaw(t, ctx, pool, acctID, udnComm, udnClient, umhChannel, umhObservedBody, udnExtID,
				"itest-udeny-hash-901", sentAt)
			umhNormalize(t, ctx, pool)

			extID, confirmedAt := umhReadDelivery(t, ctx, pool, deliveryID)
			var status string
			if err := pool.QueryRow(ctx, `SELECT status FROM deliveries WHERE id=$1`, deliveryID).Scan(&status); err != nil {
				t.Fatalf("read status: %v", err)
			}
			events := scanInt(t, ctx, pool,
				`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_confirmed'`, taskID)
			if tc.wantStamped {
				if extID == nil || *extID != udnExtID || confirmedAt == nil {
					t.Fatalf("CONTROL FAILED: the 'sent' row was not stamped (sent_external_id=%s); the fixture never "+
						"reaches confirmUpworkDelivery", umhStr(extID))
				}
				return
			}
			if status != "rejected" || extID != nil || confirmedAt != nil {
				t.Errorf("the upwork matcher touched a REJECTED row: status=%q sent_external_id=%s confirmed_at=%s "+
					"(criterion 15)", status, umhStr(extID), umhStr(confirmedAt))
			}
			if events != 0 {
				t.Errorf("delivery_confirmed events = %d for a rejected row, want 0", events)
			}
		})
	}
}

// Criterion 16: upworkcrm/reconcile.go selects status='sent' only.
func TestUpworkRejected_Integration_ReconcilerLeavesARejectedRowAlone(t *testing.T) {
	ctx, pool, acctID := rcOpen(t)
	defer pool.Close()
	cleanupUpworkReconcile(t, ctx, pool)
	defer cleanupUpworkReconcile(t, ctx, pool)

	var projID, taskID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,'itest-rc-client','manual','dashboard','/tmp/itest','any') RETURNING id`, rcSlug).Scan(&projID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status) VALUES ($1,'itest-rc deny work','claude','done_locally') RETURNING id`,
		projID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	// Same high-water-mark discipline as the sibling test: only this suite's
	// runs may count toward the threshold.
	var floor time.Time
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(max(started_at), now() - interval '1 hour') FROM sync_runs WHERE source_account_id=$1`,
		acctID).Scan(&floor); err != nil {
		t.Fatalf("read the sync_runs high-water mark: %v", err)
	}
	sentAt := floor.Add(time.Minute)
	if _, err := pool.Exec(ctx,
		`INSERT INTO normalized_threads (thread_key, participants) VALUES ($1,'[]')
		 ON CONFLICT (thread_key) WHERE thread_key IS NOT NULL DO NOTHING`, rcRoomedKey()); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	var threadID, control, rejected int64
	if err := pool.QueryRow(ctx, `SELECT id FROM normalized_threads WHERE thread_key=$1`, rcRoomedKey()).Scan(&threadID); err != nil {
		t.Fatalf("read thread id: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, target_client_ref, thread_id, body, status, sent_at)
		 VALUES ($1,'upwork_chat',$2,$3,$4,$5,'sent',$6) RETURNING id`,
		taskID, rcRoomedKey(), rcClient, threadID, rcBody, sentAt).Scan(&control); err != nil {
		t.Fatalf("seed control delivery: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, target_client_ref, thread_id, body, status,
		                         rejection_note, updated_at)
		 VALUES ($1,'upwork_chat',$2,$3,$4,$5,'rejected','denied',$6) RETURNING id`,
		taskID, rcRoomedKey(), rcClient, threadID, rcBody+" (rejected copy)", sentAt).Scan(&rejected); err != nil {
		t.Fatalf("seed rejected delivery: %v (needs migration 0028)", err)
	}
	rcSeedRuns(t, ctx, pool, acctID, 8, sentAt)

	if _, err := upworkcrm.ReconcileUnconfirmed(ctx, upworkcrm.NewSink(pool), 6); err != nil {
		t.Fatalf("ReconcileUnconfirmed: %v", err)
	}
	if r := rcRead(t, ctx, pool, control); r.errText == nil {
		t.Fatalf("CONTROL FAILED: the 'sent' row was not flagged after 8 eligible passes")
	}
	r := rcRead(t, ctx, pool, rejected)
	if r.errText != nil {
		t.Errorf("the upwork reconciler wrote %q onto a REJECTED row; it selects status='sent' only (criterion 16)", *r.errText)
	}
	if r.status != "rejected" {
		t.Errorf("status = %q, want rejected", r.status)
	}
	if n := scanInt(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE payload->>'delivery_id' = $1`, strconv.FormatInt(rejected, 10)); n != 0 {
		t.Errorf("task events naming the rejected row = %d, want 0", n)
	}
}
