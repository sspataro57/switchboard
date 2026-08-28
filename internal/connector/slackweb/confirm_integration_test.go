//go:build integration

package slackweb_test

// Integration tests for SWT-12's post-hoc confirmation hardening (SPEC
// slack-send-promotion, criteria 10 + 11). Build-tagged `integration` AND
// env-gated on DATABASE_URL. No Slack, no browser: the export is a fixture
// source, exactly like integration_test.go.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -run SlackConfirm ./internal/connector/slackweb/
//
// GREENFIELD NOTE: confirmDelivery still matches status='sent' only, and the
// reconciler does not exist, so under `-tags integration` this compile-FAILs
// (ReconcileUnconfirmed undefined) and then fails on the sending-row promotion —
// the expected red state.
//
// Why this matters (named consequence 2): unlike SMTP's pre-reserved Message-ID,
// a browser click has NO reservable external id. A crash between the click and
// the 'sent' write leaves a 'sending' row switchboard cannot classify. The design
// answer is: commit 'sending' pre-click, let the next export confirm via the
// 120-char prefix matcher, and after N passes with no match flag for a human.
// NEVER auto-retry — a retry is a possible double-post into a client channel.
//
// IMPOSED surface (criterion 11 describes the behavior, not the function; this
// mirrors Normalize's free-function-over-*PGSink shape):
//
//	func ReconcileUnconfirmed(ctx context.Context, sink *PGSink, passes int) (flagged int, err error)
//
// It is connector-side and deterministic SQL — no LLM, orchestrator untouched
// (invariant 7). cmd/connectors/slackweb calls it after Normalize with
// UnconfirmedFlagPasses().
//
// Cross-suite discipline: this suite owns 'itest-slack-confirm-%' and the
// synthetic accounts tsdconf@slack-web.local / tsdrecon@slack-web.local, and
// cleans its OWN corpus in FK order, rerunnably, at start and end.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	cfSlug = "itest-slack-confirm-proj"

	// Confirmation corpus.
	cfWorkspace  = "TSDCONF"
	cfChannel    = "CSDCONF"
	cfOwnUser    = "USDCONFOWN"
	cfAccount    = "tsdconf@slack-web.local"
	cfThreadKey  = "slack:TSDCONF:CSDCONF"
	cfTarget     = "https://app.slack.com/client/TSDCONF/CSDCONF"
	cfMessageID  = "p1784900000000001"
	cfExternalID = "slack:TSDCONF:CSDCONF:p1784900000000001"
	cfBody       = "the promoted Slack send that the next export must confirm"

	// Reconciler corpus (no export at all — the point is that nothing matched).
	rcWorkspace = "TSDRECON"
	rcAccount   = "tsdrecon@slack-web.local"
	rcTarget    = "https://app.slack.com/client/TSDRECON/CSDRECON"
	rcBody      = "a Slack send that no export ever confirmed"
)

// ---- fixture export ------------------------------------------------------------

// confirmSource is one workspace, one channel, one OWN (outbound) message whose
// text is the delivery body — the loop-closure input.
type confirmSource struct{}

func (confirmSource) Export(context.Context) (slackweb.Export, error) {
	return slackweb.Export{
		SchemaVersion: slackweb.SchemaVersion,
		Workspaces: []slackweb.Workspace{{
			ID: cfWorkspace, Name: "Confirm Slack", URL: "https://app.slack.com/client/" + cfWorkspace,
			OwnUserID: cfOwnUser,
			Conversations: []slackweb.Conversation{{
				ID: cfChannel, Name: "confirm", Type: "public_channel", URL: cfTarget,
				Messages: []slackweb.Message{{
					ID: cfMessageID, Timestamp: "2026-07-28T09:00:00Z",
					Author: "Salvo", AuthorID: cfOwnUser, Text: cfBody,
				}},
			}},
		}},
	}, nil
}

// ---- scaffolding ---------------------------------------------------------------

func newConfirmPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must never run against the real ops database")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	cleanupSlackConfirm(t, ctx, pool)
	t.Cleanup(func() { cleanupSlackConfirm(t, ctx, pool) })
	return pool
}

func cleanupSlackConfirm(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE provider='slack_web' AND account_email IN ('` +
		cfAccount + `','` + rcAccount + `'))`
	queries := []string{
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`,
		`DELETE FROM task_events WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + cfSlug + `'))`,
		`DELETE FROM deliveries WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + cfSlug + `'))`,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + cfSlug + `')`,
		`DELETE FROM projects WHERE slug='` + cfSlug + `'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE '` + cfThreadKey + `%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider='slack_web' AND account_email IN ('` + cfAccount + `','` + rcAccount + `')`,
	}
	for _, query := range queries {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatalf("cleanup %q: %v", query, err)
		}
	}
}

func seedConfirmTask(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var projectID, taskID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,'itest-slack-confirm','manual','dashboard','/tmp/itest', 'any') RETURNING id`, cfSlug).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'Slack confirmation work','claude','delivered') RETURNING id`, projectID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return taskID
}

type confirmRow struct {
	status         string
	sentExternalID *string
	sentAt         *string
	confirmedAt    *string
	errText        *string
}

func readConfirmRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) confirmRow {
	t.Helper()
	var r confirmRow
	if err := pool.QueryRow(ctx,
		`SELECT status, sent_external_id, sent_at::text, confirmed_at::text, error FROM deliveries WHERE id=$1`, id).
		Scan(&r.status, &r.sentExternalID, &r.sentAt, &r.confirmedAt, &r.errText); err != nil {
		t.Fatalf("read delivery %d: %v", id, err)
	}
	return r
}

func confirmEventCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64, eventType string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type=$2`, taskID, eventType).Scan(&n); err != nil {
		t.Fatalf("count %s events: %v", eventType, err)
	}
	return n
}

// ---- criterion 10: the matcher promotes a 'sending' row ------------------------

// A crash between the click and the 'sent' write leaves status='sending' with
// sent_at NULL (send_delivery stamps sent_at only on success). The next export
// must self-heal it: stamp sent_external_id + confirmed_at, promote to 'sent',
// SET sent_at because it is null, and emit delivery_confirmed exactly once.
func TestSlackConfirm_Integration_PromotesSendingRow(t *testing.T) {
	ctx := context.Background()
	pool := newConfirmPool(t, ctx)
	taskID := seedConfirmTask(t, ctx, pool)

	var deliveryID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, approval_source)
		 VALUES ($1,'slack_reply',$2,$3,'sending','switchboard') RETURNING id`,
		taskID, cfTarget, cfBody).Scan(&deliveryID); err != nil {
		t.Fatalf("seed sending slack_reply delivery (apply migration 0011): %v", err)
	}

	sink := slackweb.NewSink(pool)
	if _, err := slackweb.Ingest(ctx, confirmSource{}, sink); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if _, err := slackweb.Normalize(ctx, sink, slackweb.Config{}); err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	r := readConfirmRow(t, ctx, pool, deliveryID)
	if r.status != "sent" {
		t.Errorf("status after the export matched = %q, want sent — confirmDelivery must match "+
			"status IN ('sending','sent') now, not 'sent' alone (criterion 10), so the crash window self-heals", r.status)
	}
	if r.sentExternalID == nil || *r.sentExternalID != cfExternalID {
		t.Errorf("sent_external_id = %v, want %q", r.sentExternalID, cfExternalID)
	}
	if r.confirmedAt == nil {
		t.Errorf("confirmed_at not stamped")
	}
	if r.sentAt == nil {
		t.Errorf("sent_at is still NULL on a promoted row; criterion 10 sets it if null")
	}
	if n := confirmEventCount(t, ctx, pool, taskID, "delivery_confirmed"); n != 1 {
		t.Fatalf("delivery_confirmed events = %d, want exactly 1", n)
	}

	// Re-running the whole export is idempotent: no second stamp, no second event.
	// (--all re-normalizes every Slack raw row, which is the real rerun shape.)
	if _, err := slackweb.Ingest(ctx, confirmSource{}, sink); err != nil {
		t.Fatalf("second Ingest: %v", err)
	}
	if _, err := slackweb.Normalize(ctx, sink, slackweb.Config{All: true}); err != nil {
		t.Fatalf("second Normalize(--all): %v", err)
	}
	again := readConfirmRow(t, ctx, pool, deliveryID)
	if again.sentExternalID == nil || *again.sentExternalID != cfExternalID {
		t.Errorf("sent_external_id after rerun = %v, want it unchanged", again.sentExternalID)
	}
	if again.confirmedAt == nil || *again.confirmedAt != *r.confirmedAt {
		t.Errorf("confirmed_at moved on rerun (%v -> %v); the WHERE ... IS NULL guard must hold", r.confirmedAt, again.confirmedAt)
	}
	if n := confirmEventCount(t, ctx, pool, taskID, "delivery_confirmed"); n != 1 {
		t.Errorf("delivery_confirmed events after rerun = %d, want still exactly 1", n)
	}
}

// The guard that must survive the widening: an existing sent_external_id is
// NEVER overwritten, whatever the export says (invariant 4).
func TestSlackConfirm_Integration_NeverOverwritesAnExistingSentExternalID(t *testing.T) {
	ctx := context.Background()
	pool := newConfirmPool(t, ctx)
	taskID := seedConfirmTask(t, ctx, pool)

	const foreignID = "slack:TSDCONF:CSDCONF:p1700000000000009"
	var deliveryID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, sent_at, sent_external_id, confirmed_at)
		 VALUES ($1,'slack_reply',$2,$3,'sent',now(),$4,now()) RETURNING id`,
		taskID, cfTarget, cfBody, foreignID).Scan(&deliveryID); err != nil {
		t.Fatalf("seed confirmed slack_reply delivery: %v", err)
	}

	sink := slackweb.NewSink(pool)
	if _, err := slackweb.Ingest(ctx, confirmSource{}, sink); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if _, err := slackweb.Normalize(ctx, sink, slackweb.Config{}); err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	r := readConfirmRow(t, ctx, pool, deliveryID)
	if r.sentExternalID == nil || *r.sentExternalID != foreignID {
		t.Errorf("sent_external_id = %v, want the pre-existing %q untouched", r.sentExternalID, foreignID)
	}
	if n := confirmEventCount(t, ctx, pool, taskID, "delivery_confirmed"); n != 0 {
		t.Errorf("delivery_confirmed events = %d, want 0 for an already-confirmed row", n)
	}
}

// ---- criterion 11: the reconciler flags, never retries -------------------------

// seedOKRun inserts one COMPLETED successful export pass for the account, with
// started_at / finished_at controlled relative to now.
func seedRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, accountID int64, startedAgo, finishedAgo string, status string) {
	t.Helper()
	finished := "now() - interval '" + finishedAgo + "'"
	if finishedAgo == "" {
		finished = "NULL"
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO sync_runs (source_account_id, started_at, finished_at, status, stats)
		 VALUES ($1, now() - interval '`+startedAgo+`', `+finished+`, $2, '{"phase":"slack_web"}')`,
		accountID, status); err != nil {
		t.Fatalf("seed sync_run (started %s ago, status %s): %v", startedAgo, status, err)
	}
}

// NOTE (gap reported to the implementer, deliberately not asserted here):
// criterion 11 counts runs that STARTED after `sent_at`, but an ambiguous-failure
// row sits in 'sending' with sent_at NULL — send_delivery stamps sent_at only on
// success. The SPEC does not say what the reference timestamp is for that state.
// Every row below therefore has sent_at set, which is the 'sent'-but-unconfirmed
// case criterion 11 describes literally.
func TestSlackConfirm_Integration_ReconcilerFlagsAfterThreePasses(t *testing.T) {
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

	// The unconfirmed send: 'sent' 30 minutes ago, no id, never confirmed.
	var deliveryID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, sent_at, approval_source)
		 VALUES ($1,'slack_reply',$2,$3,'sent', now() - interval '30 minutes', 'switchboard') RETURNING id`,
		taskID, rcTarget, rcBody).Scan(&deliveryID); err != nil {
		t.Fatalf("seed unconfirmed slack_reply delivery: %v", err)
	}

	// Runs that must NOT count.
	//   - completed BEFORE the send: nothing to do with it.
	//   - IN FLIGHT at send time (started before sent_at, finished after): the
	//     open sub-detail from Q2 — count runs that STARTED after sent_at.
	//   - a failed pass: not a successful pass.
	//   - a pass still running: not completed.
	seedRun(t, ctx, pool, accountID, "60 minutes", "50 minutes", "ok")
	seedRun(t, ctx, pool, accountID, "35 minutes", "20 minutes", "ok")
	seedRun(t, ctx, pool, accountID, "25 minutes", "24 minutes", "error")
	seedRun(t, ctx, pool, accountID, "5 minutes", "", "running")

	flagged, err := slackweb.ReconcileUnconfirmed(ctx, slackweb.NewSink(pool), 3)
	if err != nil {
		t.Fatalf("ReconcileUnconfirmed: %v", err)
	}
	if flagged != 0 {
		t.Fatalf("flagged = %d with ZERO eligible passes, want 0. Ineligible runs seeded: one completed before "+
			"the send, one IN FLIGHT at send time (started before sent_at), one status='error', one still "+
			"'running'. A run already in flight when the send happened cannot have observed it (Q2 sub-detail).", flagged)
	}

	// Two eligible passes: still under the threshold.
	seedRun(t, ctx, pool, accountID, "20 minutes", "18 minutes", "ok")
	seedRun(t, ctx, pool, accountID, "15 minutes", "13 minutes", "ok")
	flagged, err = slackweb.ReconcileUnconfirmed(ctx, slackweb.NewSink(pool), 3)
	if err != nil {
		t.Fatalf("ReconcileUnconfirmed: %v", err)
	}
	if flagged != 0 {
		t.Errorf("flagged = %d after 2 eligible passes, want 0 (N = 3)", flagged)
	}
	if r := readConfirmRow(t, ctx, pool, deliveryID); r.errText != nil {
		t.Errorf("error note = %q after 2 passes, want NULL", *r.errText)
	}
	if n := confirmEventCount(t, ctx, pool, taskID, "delivery_unconfirmed"); n != 0 {
		t.Errorf("delivery_unconfirmed events after 2 passes = %d, want 0", n)
	}

	// The third eligible pass crosses it.
	seedRun(t, ctx, pool, accountID, "10 minutes", "8 minutes", "ok")
	flagged, err = slackweb.ReconcileUnconfirmed(ctx, slackweb.NewSink(pool), 3)
	if err != nil {
		t.Fatalf("ReconcileUnconfirmed: %v", err)
	}
	if flagged != 1 {
		t.Fatalf("flagged = %d after 3 eligible passes, want 1", flagged)
	}

	r := readConfirmRow(t, ctx, pool, deliveryID)
	if r.errText == nil || *r.errText == "" {
		t.Errorf("error note = %v, want a note a human can read in the dashboard (criterion 16)", r.errText)
	}
	if r.status != "sent" {
		t.Errorf("status after flagging = %q, want it UNCHANGED — flagging is a note plus an event, never a "+
			"retry and never a status move (criterion 11, named consequence 2)", r.status)
	}
	if r.sentExternalID != nil {
		t.Errorf("sent_external_id = %v, want still NULL (flagging invents nothing)", r.sentExternalID)
	}
	if n := confirmEventCount(t, ctx, pool, taskID, "delivery_unconfirmed"); n != 1 {
		t.Fatalf("delivery_unconfirmed events = %d, want exactly 1", n)
	}
	var payload string
	if err := pool.QueryRow(ctx,
		`SELECT payload::text FROM task_events WHERE task_id=$1 AND event_type='delivery_unconfirmed'`,
		taskID).Scan(&payload); err != nil {
		t.Fatalf("read delivery_unconfirmed payload: %v", err)
	}
	for _, want := range []string{`"delivery_id"`, `"slack_reply"`, `"passes"`} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload %s lacks %s; the SPEC pins {delivery_id, channel, passes}", payload, want)
		}
	}
	noteAfterFirstFlag := *r.errText

	// Fires ONCE, not once per pass. More passes, more reconcile runs, same
	// single event and the same single note.
	seedRun(t, ctx, pool, accountID, "3 minutes", "2 minutes", "ok")
	for i := 0; i < 2; i++ {
		flagged, err = slackweb.ReconcileUnconfirmed(ctx, slackweb.NewSink(pool), 3)
		if err != nil {
			t.Fatalf("ReconcileUnconfirmed (repeat %d): %v", i, err)
		}
		if flagged != 0 {
			t.Errorf("flagged = %d on a repeat pass, want 0 (the guard must make it fire once)", flagged)
		}
	}
	after := readConfirmRow(t, ctx, pool, deliveryID)
	if after.errText == nil || *after.errText != noteAfterFirstFlag {
		t.Errorf("error note changed on repeat passes (%q -> %v); the flag must be guarded to fire once",
			noteAfterFirstFlag, after.errText)
	}
	if n := confirmEventCount(t, ctx, pool, taskID, "delivery_unconfirmed"); n != 1 {
		t.Errorf("delivery_unconfirmed events after repeat passes = %d, want still exactly 1 — otherwise the "+
			"dashboard fills with one alarm per poll", n)
	}
}

// A CONFIRMED row is never flagged, however many passes go by: the export
// already stamped it. Same for a row that is not slack_reply.
func TestSlackConfirm_Integration_ReconcilerIgnoresConfirmedRows(t *testing.T) {
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
	for i := 0; i < 5; i++ {
		seedRun(t, ctx, pool, accountID, "10 minutes", "9 minutes", "ok")
	}

	var confirmed, upwork int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, sent_at, sent_external_id, confirmed_at)
		 VALUES ($1,'slack_reply',$2,$3,'sent', now() - interval '30 minutes',
		         'slack:TSDRECON:CSDRECON:p1780000000000003', now()) RETURNING id`,
		taskID, rcTarget, rcBody).Scan(&confirmed); err != nil {
		t.Fatalf("seed confirmed delivery: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, sent_at)
		 VALUES ($1,'upwork_chat','upwork_crm:itest-slack-confirm:upwork','x','sent', now() - interval '30 minutes')
		 RETURNING id`, taskID).Scan(&upwork); err != nil {
		t.Fatalf("seed upwork delivery: %v", err)
	}

	flagged, err := slackweb.ReconcileUnconfirmed(ctx, slackweb.NewSink(pool), 3)
	if err != nil {
		t.Fatalf("ReconcileUnconfirmed: %v", err)
	}
	if flagged != 0 {
		t.Errorf("flagged = %d, want 0 (a confirmed slack_reply row and an unrelated upwork_chat row are "+
			"both out of scope)", flagged)
	}
	if r := readConfirmRow(t, ctx, pool, confirmed); r.errText != nil {
		t.Errorf("confirmed row picked up an error note: %q", *r.errText)
	}
	if r := readConfirmRow(t, ctx, pool, upwork); r.errText != nil {
		t.Errorf("upwork_chat row picked up an error note: %q", *r.errText)
	}
	if n := confirmEventCount(t, ctx, pool, taskID, "delivery_unconfirmed"); n != 0 {
		t.Errorf("delivery_unconfirmed events = %d, want 0", n)
	}
}
