//go:build integration

package slackweb_test

// SWT-43 (docs/tickets/delivery-deny_SPEC.md) criteria 15 and 16, slackweb
// half: confirmDelivery never stamps a REJECTED slack_reply row, and
// ReconcileUnconfirmed never flags one.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run SlackRejected ./internal/connector/slackweb/
//
// Matcher: same target_ref, an OWN exported message whose text has the SAME
// normalized 120-char prefix as the row's body, differing in raw whitespace.
// The control seeds the identical fixture at 'sending' (a click whose outcome
// was ambiguous — the matcher's self-heal case) and must be promoted.
// Mutation: add 'rejected' to slackweb/sink.go's status list -> red.
//
// Reconciler: a rejected slack row realistically HAS a send attempt (a
// definite slackweb.SendRejectedError leaves failed+attempted, which D4 lets
// Salvador reject), so this fixture seeds send_attempted_at 30 minutes back —
// otherwise a reconciler that wrongly selected 'rejected' would still count
// zero passes and the test would prove nothing.
//
// GREENFIELD NOTE — EXPECTED RED: before 0028 no rejected fixture can be seeded.
//
// Cross-suite discipline: the matcher owns workspace TSDDENYW (account
// tsddenyw@slack-web.local, project itest-slack-deny-proj); the reconciler test
// reuses confirm_integration_test.go's itest-slack-confirm corpus and its
// cleanup (newConfirmPool). Both clean in FK order at start and end.

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/textmatch"
)

const (
	sdnSlug      = "itest-slack-deny-proj"
	sdnWorkspace = "TSDDENYW"
	sdnChannel   = "CSDDENYW"
	sdnOwnUser   = "USDDENYOWN"
	sdnAccount   = "tsddenyw@slack-web.local"
	sdnThreadKey = "slack:TSDDENYW:CSDDENYW"
	sdnTarget    = "https://app.slack.com/client/TSDDENYW/CSDDENYW"
	sdnMessageID = "p1784900000000777"
	sdnStored    = "the  denied Slack draft,\n\nwhich Salvador then   typed by hand"
	sdnObserved  = "the denied Slack draft, which Salvador then typed by hand"
)

type denySource struct{}

func (denySource) Export(context.Context, slackweb.ExportRequest) (slackweb.Export, error) {
	return slackweb.Export{
		SchemaVersion: slackweb.SchemaVersion,
		Workspaces: []slackweb.Workspace{{
			ID: sdnWorkspace, Name: "Deny Slack", URL: "https://app.slack.com/client/" + sdnWorkspace,
			OwnUserID: sdnOwnUser,
			Conversations: []slackweb.Conversation{{
				ID: sdnChannel, Name: "deny", Type: "public_channel", URL: sdnTarget,
				Messages: []slackweb.Message{{
					ID: sdnMessageID, Timestamp: "2026-07-28T09:00:00Z",
					Author: "Salvo", AuthorID: sdnOwnUser, Text: sdnObserved,
				}},
			}},
		}},
	}, nil
}

func cleanupSlackDeny(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE provider='slack_web' AND account_email='` + sdnAccount + `')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + sdnSlug + `'))`
	for _, q := range []string{
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM deliveries WHERE task_id IN ` + tasksOf,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + sdnSlug + `')`,
		`DELETE FROM projects WHERE slug='` + sdnSlug + `'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE '` + sdnThreadKey + `%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider='slack_web' AND account_email='` + sdnAccount + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func TestSlackRejected_Integration_MatcherNeverStampsARejectedRow(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must never run against the real ops database")
	}
	if sdnStored == sdnObserved ||
		textmatch.NormalizedPrefix(sdnStored, 120) != textmatch.NormalizedPrefix(sdnObserved, 120) {
		t.Fatalf("fixture invalid: the bodies must differ raw and agree normalized")
	}

	for _, tc := range []struct {
		name, status string
		wantStamped  bool
	}{
		{"control: an ambiguous 'sending' row IS promoted by the export", "sending", true},
		{"a rejected row is never stamped", "rejected", false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool, err := store.NewPool(ctx)
			if err != nil {
				t.Fatalf("store.NewPool: %v", err)
			}
			defer pool.Close()
			cleanupSlackDeny(t, ctx, pool)
			defer cleanupSlackDeny(t, ctx, pool)

			var projID, taskID, deliveryID int64
			if err := pool.QueryRow(ctx,
				`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
				 VALUES ($1,$1,'itest-slack-deny','manual','dashboard','/tmp/itest','any') RETURNING id`, sdnSlug).Scan(&projID); err != nil {
				t.Fatalf("seed project: %v", err)
			}
			if err := pool.QueryRow(ctx,
				`INSERT INTO tasks (project_id, title, assignee_type, status) VALUES ($1,'slack deny work','claude','done_locally') RETURNING id`,
				projID).Scan(&taskID); err != nil {
				t.Fatalf("seed task: %v", err)
			}
			q := `INSERT INTO deliveries (task_id, channel, target_ref, body, status, approval_source)
			      VALUES ($1,'slack_reply',$2,$3,$4,'switchboard') RETURNING id`
			if tc.status == "rejected" {
				q = `INSERT INTO deliveries (task_id, channel, target_ref, body, status, approval_source, rejection_note)
				     VALUES ($1,'slack_reply',$2,$3,$4,'switchboard','denied: typed it myself') RETURNING id`
			}
			if err := pool.QueryRow(ctx, q, taskID, sdnTarget, sdnStored, tc.status).Scan(&deliveryID); err != nil {
				t.Fatalf("seed %s slack_reply delivery: %v (a 'rejected' row needs migration 0028)", tc.status, err)
			}

			sink := slackweb.NewSink(pool)
			if _, err := slackweb.Ingest(ctx, denySource{}, sink); err != nil {
				t.Fatalf("Ingest: %v", err)
			}
			if _, err := slackweb.Normalize(ctx, sink, slackweb.Config{}); err != nil {
				t.Fatalf("Normalize: %v", err)
			}

			r := readConfirmRow(t, ctx, pool, deliveryID)
			events := confirmEventCount(t, ctx, pool, taskID, "delivery_confirmed")
			if tc.wantStamped {
				if r.sentExternalID == nil || r.status != "sent" {
					t.Fatalf("CONTROL FAILED: the export did not promote the 'sending' row (status=%q, id=%v); the "+
						"fixture never reaches confirmDelivery", r.status, r.sentExternalID)
				}
				return
			}
			if r.status != "rejected" || r.sentExternalID != nil || r.confirmedAt != nil {
				t.Errorf("the slack matcher touched a REJECTED row: status=%q sent_external_id=%v confirmed_at=%v "+
					"(criterion 15)", r.status, r.sentExternalID, r.confirmedAt)
			}
			if events != 0 {
				t.Errorf("delivery_confirmed events = %d for a rejected row, want 0", events)
			}
		})
	}
}

// Criterion 16: the reconciler selects sending/sent only. A rejected row gets
// no `error` marker and no delivery_unconfirmed event, however many passes go
// by; the control 'sent' row beside it IS flagged, proving the passes count.
func TestSlackRejected_Integration_ReconcilerLeavesARejectedRowAlone(t *testing.T) {
	ctx := context.Background()
	pool := newConfirmPool(t, ctx)
	taskID := seedConfirmTask(t, ctx, pool)

	var accountID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, domain_default, scopes, send_enabled, calendar_in_availability)
		 VALUES ('slack_web', $1, 'https://app.slack.com/client/TSDRECON', ARRAY['CSDRECON'], true, false)
		 RETURNING id`, rcAccount).Scan(&accountID); err != nil {
		t.Fatalf("seed slack_web account: %v", err)
	}
	var control, rejected int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, sent_at, approval_source)
		 VALUES ($1,'slack_reply',$2,$3,'sent', now() - interval '30 minutes', 'switchboard') RETURNING id`,
		taskID, rcTarget, rcBody).Scan(&control); err != nil {
		t.Fatalf("seed control delivery: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, approval_source, rejection_note,
		                         send_attempted_at, send_settled_at, updated_at)
		 VALUES ($1,'slack_reply',$2,$3,'rejected','switchboard','denied after a definite failure',
		         now() - interval '30 minutes', now() - interval '29 minutes', now() - interval '30 minutes') RETURNING id`,
		taskID, rcTarget, rcBody+" (rejected copy)").Scan(&rejected); err != nil {
		t.Fatalf("seed rejected delivery: %v (needs migration 0028)", err)
	}
	for _, ago := range []string{"20 minutes", "15 minutes", "10 minutes", "5 minutes"} {
		seedRun(t, ctx, pool, accountID, ago, ago, "ok")
	}

	if _, err := slackweb.ReconcileUnconfirmed(ctx, slackweb.NewSink(pool), 3); err != nil {
		t.Fatalf("ReconcileUnconfirmed: %v", err)
	}
	if r := readConfirmRow(t, ctx, pool, control); r.errText == nil {
		t.Fatalf("CONTROL FAILED: the 'sent' row was not flagged after 4 eligible passes; the rejected case " +
			"below proves nothing")
	}
	r := readConfirmRow(t, ctx, pool, rejected)
	if r.errText != nil {
		t.Errorf("the reconciler wrote %q onto a REJECTED row; it selects sending/sent only (criterion 16)", *r.errText)
	}
	if r.status != "rejected" {
		t.Errorf("status = %q, want rejected", r.status)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_events WHERE event_type='delivery_unconfirmed' AND payload->>'delivery_id'=$1`,
		strconv.FormatInt(rejected, 10)).Scan(&n); err != nil {
		t.Fatalf("count delivery_unconfirmed: %v", err)
	}
	if n != 0 {
		t.Errorf("delivery_unconfirmed events for the rejected row = %d, want 0", n)
	}
}
