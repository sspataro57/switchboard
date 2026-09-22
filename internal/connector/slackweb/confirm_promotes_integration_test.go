//go:build integration

package slackweb_test

// slack-send-queue (SWT-76) Part 5 — D7: the confirmation path must emit
// `delivery_sent` when it PROMOTES a `sending` row, or the task never leaves
// `done_locally`. Criteria 24, 25, 26, 27 (the transaction half) and 28.
//
// Build-tagged `integration` AND env-gated on DATABASE_URL. No Slack, no
// browser, no mini: the export is a fixture source, exactly as
// confirm_integration_test.go does it.
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_sendqueue?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run SlackPromote ./internal/connector/slackweb/
//
// ---------------------------------------------------------------------------
// WHY THIS IS IN THIS TICKET AT ALL — the one scope expansion, and it is not
// optional. `confirmDelivery` promotes `sending -> sent` and emits ONLY
// `delivery_confirmed` (sink.go:465-486). Orchestrator R8 keys on
// `delivery_sent` alone (rules.go:114, :271-309) and there is no rule for
// `delivery_confirmed` anywhere (`grep delivery_confirmed internal/orchestrator`
// is empty). Today the promotion path is rare — a crashed sender, an ambiguous
// 500 — because the happy path emits `delivery_sent` at the click. EVERY QUEUED
// SEND TAKES THE PROMOTION PATH. Without D7 this feature ships a work task
// stuck at `done_locally` with its Deliver task open forever, silently, for
// every Slack reply approved while the browser was busy. gmail's sink names
// exactly this hazard as its reason not to promote at all
// (internal/connector/google/sink.go:732-738).
//
// GREENFIELD NOTE — no new symbol is required by this file, so it COMPILES
// today and FAILS on assertions: confirmDelivery emits `delivery_confirmed`
// only, so criteria 24, 26 and 28 are red from the first run. Criterion 27's
// atomicity test is red because the promotion UPDATE and the event INSERT are
// two unfenced pool.Exec calls (sink.go:465, :482). Criterion 25 should be
// GREEN today and must stay green — it is the control that stops D7 being
// implemented as "always emit delivery_sent".
//
// SWT-71's four rules, which D7 copies verbatim (IK, "A gmail send that died
// mid-flight"):
//  1. the candidate SELECT reads `status` alongside id/task_id/body and the
//     promotion UPDATE is guarded `AND status=$n` with THAT value — validate
//     the value that LANDS, never a branch that can disagree with the row it
//     writes;
//  2. the promotion AND its events go in ONE transaction;
//  3. lock order delivery -> task, and the task status is read WITHOUT a row
//     lock (a `FOR SHARE OF t` here would close the cycle refuseClosedTask's
//     comment at delivery.go:705-711 deliberately leaves open) — pinned
//     structurally in confirm_promotes_structure_test.go;
//  4. status was `sending` -> `delivery_confirmed` AND
//     `delivery_sent {recovered:true}`; already `sent` -> `delivery_confirmed`
//     only; task CLOSED since -> `delivery_confirmed` plus a `log`
//     {kind:"delivery_finished"}, NEVER `delivery_sent`.
//
// This suite owns 'itest-slack-promote-%', the synthetic account
// tsdprom@slack-web.local and the workspace TSDPROM. Rerunnable; cleans its own
// corpus in FK order at start and end; refuses 192.168.50.49.
//
// MUTATION MAP:
//	Emit only delivery_confirmed on a sending -> sent promotion -> criteria 24, 28
//	Emit delivery_sent on an already-`sent` confirmation          -> criterion 25
//	Emit delivery_sent when the task is closed                    -> criterion 26
//	Split the promotion and its events into two pool.Exec calls   -> criterion 27

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/connector/slackweb"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/fleet"
	orch "github.com/sspataro57/switchboard/internal/orchestrator"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	prSlug      = "itest-slack-promote-proj"
	prWorkspace = "TSDPROM"
	prConv      = "CSDPROM"
	prOwnUser   = "USDPROMOWN"
	prAccount   = "tsdprom@slack-web.local"
	prTarget    = "https://app.slack.com/client/TSDPROM/CSDPROM"
)

// ---- fixture export ------------------------------------------------------------

// promoSource is one workspace, one conversation, and whatever OWN (outbound)
// messages the test staged — the loop-closure input, with no Slack anywhere.
type promoSource struct{ msgs []slackweb.Message }

func (s promoSource) Export(context.Context, slackweb.ExportRequest) (slackweb.Export, error) {
	return slackweb.Export{
		SchemaVersion: slackweb.SchemaVersion,
		Workspaces: []slackweb.Workspace{{
			ID: prWorkspace, Name: "Promote Slack", URL: "https://app.slack.com/client/" + prWorkspace,
			OwnUserID: prOwnUser,
			Conversations: []slackweb.Conversation{{
				ID: prConv, Name: "promote", Type: "public_channel", URL: prTarget,
				Messages: s.msgs,
			}},
		}},
	}, nil
}

func ownMessage(id, text string) slackweb.Message {
	return slackweb.Message{
		ID: id, Timestamp: time.Now().UTC().Format(time.RFC3339),
		Author: "Salvo", AuthorID: prOwnUser, Text: text,
	}
}

// ---- scaffolding ---------------------------------------------------------------

func newPromoPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
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
	cleanupPromo(t, ctx, pool)
	t.Cleanup(func() { cleanupPromo(t, context.Background(), pool) })
	return pool
}

func cleanupPromo(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE provider='slack_web' AND account_email='` + prAccount + `')`
	const tasks = `(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + prSlug + `'))`
	for _, q := range []string{
		`DROP TRIGGER IF EXISTS itest_promote_block ON task_events`,
		`DROP FUNCTION IF EXISTS itest_promote_block()`,
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE actor LIKE 'itest-slack-promote%')`,
		`DELETE FROM audit_events WHERE actor LIKE 'itest-slack-promote%'`,
		`DELETE FROM task_events WHERE task_id IN ` + tasks,
		`DELETE FROM deliveries WHERE task_id IN ` + tasks,
		// children before parents (tasks.parent_id self-FK): R3's Deliver task.
		`DELETE FROM tasks WHERE parent_id IS NOT NULL AND project_id IN
		   (SELECT id FROM projects WHERE slug='` + prSlug + `')`,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + prSlug + `')`,
		`DELETE FROM projects WHERE slug='` + prSlug + `'`,
		// One statement, and NO LIKE on the thread key: its format has ONE
		// spelling (threadscope_test.go) and the repo refuses a second one in SQL.
		`WITH mine AS (SELECT id, thread_id FROM normalized_messages
		                WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)),
		      gone AS (DELETE FROM normalized_messages WHERE id IN (SELECT id FROM mine))
		 DELETE FROM normalized_threads t WHERE t.id IN (SELECT thread_id FROM mine)
		   AND NOT EXISTS (SELECT 1 FROM normalized_messages m WHERE m.thread_id = t.id AND m.id NOT IN (SELECT id FROM mine))`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider='slack_web' AND account_email='` + prAccount + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func seedPromoTask(t *testing.T, ctx context.Context, pool *pgxpool.Pool, status string) int64 {
	t.Helper()
	var projectID, taskID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,'itest-slack-promote','manual','dashboard','/tmp/itest','any')
		 ON CONFLICT (slug) DO UPDATE SET client=EXCLUDED.client RETURNING id`, prSlug).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'Slack promotion work','claude',$2) RETURNING id`, projectID, status).Scan(&taskID); err != nil {
		t.Fatalf("seed task (%s): %v", status, err)
	}
	return taskID
}

// seedQueuedSend is the row a queued send leaves behind: `sending`, attempt
// unsettled, send_queued_at set, no click result. It is the ONLY shape a queued
// send can be confirmed from.
func seedQueuedSend(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64, body string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, approval_source,
		                         send_attempted_at, send_settled_at, send_queued_at, send_queue_job_id)
		 VALUES ($1,'slack_reply',$2,$3,'sending','switchboard',
		         now() - interval '5 minutes', NULL, now() - interval '5 minutes', 'send-itest-1')
		 RETURNING id`, taskID, prTarget, body).Scan(&id); err != nil {
		t.Fatalf("seed queued slack_reply delivery (apply migration 0042): %v", err)
	}
	return id
}

func runExport(t *testing.T, ctx context.Context, pool *pgxpool.Pool, msgs []slackweb.Message, cfg slackweb.Config) error {
	t.Helper()
	sink := slackweb.NewSink(pool)
	if _, err := slackweb.Ingest(ctx, promoSource{msgs: msgs}, sink); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	_, err := slackweb.Normalize(ctx, sink, cfg)
	return err
}

func promoEventCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64, eventType string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type=$2`, taskID, eventType).Scan(&n); err != nil {
		t.Fatalf("count %s events: %v", eventType, err)
	}
	return n
}

func promoEventPayloads(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64, eventType string) []map[string]any {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT payload FROM task_events WHERE task_id=$1 AND event_type=$2 ORDER BY id`, taskID, eventType)
	if err != nil {
		t.Fatalf("select %s payloads: %v", eventType, err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan payload: %v", err)
		}
		var p map[string]any
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		out = append(out, p)
	}
	return out
}

type promoDelivery struct {
	status         string
	sentExternalID *string
	confirmedAt    *time.Time
	sentAt         *time.Time
}

func readPromo(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) promoDelivery {
	t.Helper()
	var d promoDelivery
	if err := pool.QueryRow(ctx,
		`SELECT status, sent_external_id, confirmed_at, sent_at FROM deliveries WHERE id=$1`, id).
		Scan(&d.status, &d.sentExternalID, &d.confirmedAt, &d.sentAt); err != nil {
		t.Fatalf("read delivery %d: %v", id, err)
	}
	return d
}

// ---- criterion 24: a queued send's confirmation emits BOTH events -------------

func TestSlackPromote_Integration_SendingRowEmitsConfirmedAndSent(t *testing.T) {
	ctx := context.Background()
	pool := newPromoPool(t, ctx)
	taskID := seedPromoTask(t, ctx, pool, "done_locally")
	const body = "the queued reply the leaf clicked in the gap after the rotation"
	deliveryID := seedQueuedSend(t, ctx, pool, taskID, body)

	const msgID = "p1790100000000001"
	if err := runExport(t, ctx, pool, []slackweb.Message{ownMessage(msgID, body)}, slackweb.Config{}); err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	d := readPromo(t, ctx, pool, deliveryID)
	if d.status != "sent" {
		t.Fatalf("status after the export matched = %q, want sent (the SWT-12 promotion, unchanged)", d.status)
	}
	if d.sentExternalID == nil || d.confirmedAt == nil || d.sentAt == nil {
		t.Errorf("promotion left sent_external_id=%v confirmed_at=%v sent_at=%v; all three are stamped",
			d.sentExternalID, d.confirmedAt, d.sentAt)
	}

	if n := promoEventCount(t, ctx, pool, taskID, "delivery_confirmed"); n != 1 {
		t.Errorf("delivery_confirmed events = %d, want exactly 1 (unchanged)", n)
	}
	sent := promoEventPayloads(t, ctx, pool, taskID, "delivery_sent")
	if len(sent) != 1 {
		t.Fatalf("delivery_sent events after promoting a `sending` row = %d, want exactly 1. D7: R8 keys on "+
			"delivery_sent ALONE (rules.go:114) and nothing anywhere reads delivery_confirmed, so without this "+
			"every queued send leaves its work task at done_locally with the Deliver task open FOREVER, "+
			"silently (criterion 24)", len(sent))
	}
	p := sent[0]
	if got, ok := p["delivery_id"].(float64); !ok || int64(got) != deliveryID {
		t.Errorf("delivery_sent payload delivery_id = %v, want %d", p["delivery_id"], deliveryID)
	}
	if p["recovered"] != true {
		t.Errorf("delivery_sent payload recovered = %v, want true. The send was never reported by the sender; "+
			"this event is reconstructed from the message's own re-entry, and the payload must say so "+
			"(SWT-71's spelling, delivery.go:820)", p["recovered"])
	}
}

// ---- criterion 25: the control — an already-`sent` row emits nothing new ------

// The half that must NOT change, and the reason D7 cannot be "always emit
// delivery_sent": a row already `sent` had its delivery_sent at the click, R8
// already ran and recorded its delivery_lifecycle key. A second one is a
// duplicate lifecycle event on a task that has moved on.
func TestSlackPromote_Integration_AlreadySentRowEmitsConfirmedOnly(t *testing.T) {
	ctx := context.Background()
	pool := newPromoPool(t, ctx)
	taskID := seedPromoTask(t, ctx, pool, "delivered")
	const body = "a reply that was clicked synchronously and is only now being confirmed"

	var deliveryID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, approval_source,
		                         send_attempted_at, send_settled_at, sent_at)
		 VALUES ($1,'slack_reply',$2,$3,'sent','switchboard',
		         now() - interval '5 minutes', now() - interval '5 minutes', now() - interval '5 minutes')
		 RETURNING id`, taskID, prTarget, body).Scan(&deliveryID); err != nil {
		t.Fatalf("seed sent slack_reply delivery: %v", err)
	}

	const msgID = "p1790200000000002"
	msgs := []slackweb.Message{ownMessage(msgID, body)}
	if err := runExport(t, ctx, pool, msgs, slackweb.Config{}); err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	if n := promoEventCount(t, ctx, pool, taskID, "delivery_confirmed"); n != 1 {
		t.Errorf("delivery_confirmed events = %d, want 1", n)
	}
	if n := promoEventCount(t, ctx, pool, taskID, "delivery_sent"); n != 0 {
		t.Fatalf("delivery_sent events on confirming an ALREADY-sent row = %d, want 0. Its delivery_sent fired "+
			"at the click; a second one is a duplicate R8 trigger. This is why the promotion UPDATE must be "+
			"guarded on the status the SELECT read and the events must branch on THAT value — never on a "+
			"branch that can disagree with the row it writes (SWT-71 rule 1, criterion 25)", n)
	}

	// The `--all` replay: re-normalizing every Slack raw row emits nothing new.
	// The sent_external_id IS NULL guard plus the RowsAffected check own this.
	if err := runExport(t, ctx, pool, msgs, slackweb.Config{All: true}); err != nil {
		t.Fatalf("Normalize(--all): %v", err)
	}
	if n := promoEventCount(t, ctx, pool, taskID, "delivery_confirmed"); n != 1 {
		t.Errorf("delivery_confirmed events after a --all replay = %d, want still 1", n)
	}
	if n := promoEventCount(t, ctx, pool, taskID, "delivery_sent"); n != 0 {
		t.Errorf("a --all replay produced %d delivery_sent events, want 0", n)
	}
}

// ---- criterion 26: the closed task ---------------------------------------------

// The SWT-28 calendar trap, restated at delivery.go:769-771: R8 would "succeed"
// through task_mark_delivered's closed no-op, record its `delivery_lifecycle`
// key against the task id, and then MUTE a later real delivery if the task is
// reopened. A closed task therefore gets the record and never the lifecycle
// event.
func TestSlackPromote_Integration_ClosedTaskGetsALogAndNeverDeliverySent(t *testing.T) {
	ctx := context.Background()
	pool := newPromoPool(t, ctx)
	taskID := seedPromoTask(t, ctx, pool, "closed")
	const body = "a queued reply whose task was closed while the leaf still held the job"
	deliveryID := seedQueuedSend(t, ctx, pool, taskID, body)

	const msgID = "p1790300000000003"
	if err := runExport(t, ctx, pool, []slackweb.Message{ownMessage(msgID, body)}, slackweb.Config{}); err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	if d := readPromo(t, ctx, pool, deliveryID); d.status != "sent" || d.sentExternalID == nil {
		t.Errorf("a closed task must still get its delivery RECORDED (status=%q id=%v); only the lifecycle "+
			"event is withheld", d.status, d.sentExternalID)
	}
	if n := promoEventCount(t, ctx, pool, taskID, "delivery_confirmed"); n != 1 {
		t.Errorf("delivery_confirmed events = %d, want 1", n)
	}
	if n := promoEventCount(t, ctx, pool, taskID, "delivery_sent"); n != 0 {
		t.Fatalf("delivery_sent was emitted for a CLOSED task (%d events). R8 would succeed through "+
			"task_mark_delivered's closed no-op, record its delivery_lifecycle key keyed on task id ALONE, and "+
			"mute a later REAL delivery after a reopen — the SWT-28 calendar trap (criterion 26)", n)
	}

	logs := promoEventPayloads(t, ctx, pool, taskID, "log")
	var found map[string]any
	for _, p := range logs {
		if p["kind"] == "delivery_finished" {
			found = p
		}
	}
	if found == nil {
		t.Fatalf("no log {kind:\"delivery_finished\"} event on the closed task; a finished row with no event "+
			"has no verb left and nothing records that the message did arrive. Payloads seen: %v", logs)
	}
	if got, ok := found["delivery_id"].(float64); !ok || int64(got) != deliveryID {
		t.Errorf("delivery_finished payload delivery_id = %v, want %d", found["delivery_id"], deliveryID)
	}
}

// ---- criterion 27: promotion and events commit together ------------------------

// Two unfenced pool.Exec calls (sink.go:465, :482) already lose the event on a
// crash between them. A queued send makes that event LOAD-BEARING: the row
// would read `sent` while the task sat at done_locally with nothing left to
// trigger R8, and no verb would fix it.
//
// The forced failure is a temporary BEFORE INSERT trigger on task_events scoped
// to this delivery id — a deterministic way to break the second statement
// without touching the code under test. The scratch database is this suite's
// own (see the header); the trigger is dropped by cleanupPromo either way.
func TestSlackPromote_Integration_PromotionAndEventsAreOneTransaction(t *testing.T) {
	ctx := context.Background()
	pool := newPromoPool(t, ctx)
	taskID := seedPromoTask(t, ctx, pool, "done_locally")
	const body = "a queued reply whose event insert is about to fail"
	deliveryID := seedQueuedSend(t, ctx, pool, taskID, body)

	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION itest_promote_block() RETURNS trigger AS $fn$
		BEGIN
		  IF NEW.payload->>'delivery_id' = '%d' THEN
		    RAISE EXCEPTION 'itest: forced failure on the delivery event insert';
		  END IF;
		  RETURN NEW;
		END; $fn$ LANGUAGE plpgsql;`, deliveryID)); err != nil {
		t.Fatalf("create the forced-failure function: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`CREATE TRIGGER itest_promote_block BEFORE INSERT ON task_events
		  FOR EACH ROW EXECUTE FUNCTION itest_promote_block()`); err != nil {
		t.Fatalf("create the forced-failure trigger: %v", err)
	}
	defer func() {
		if _, err := pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS itest_promote_block ON task_events`); err != nil {
			t.Errorf("drop the forced-failure trigger: %v", err)
		}
	}()

	const msgID = "p1790400000000004"
	err := runExport(t, ctx, pool, []slackweb.Message{ownMessage(msgID, body)}, slackweb.Config{})
	if err == nil {
		t.Fatal("Normalize swallowed a failed event insert; the failure must surface, or a lost lifecycle event " +
			"is invisible (criterion 27)")
	}

	d := readPromo(t, ctx, pool, deliveryID)
	if d.status != "sending" || d.sentExternalID != nil || d.confirmedAt != nil {
		t.Fatalf("the promotion COMMITTED while its event failed: status=%q sent_external_id=%v "+
			"confirmed_at=%v. One transaction, or a queued send ends as a `sent` row whose task is stranded at "+
			"done_locally with no verb left to move it (criterion 27, SWT-71 rule 2)",
			d.status, d.sentExternalID, d.confirmedAt)
	}
}

// ---- criterion 28: end to end, the task actually moves -------------------------

type noopPublisher struct{}

func (noopPublisher) PublishCommand(string, fleet.Cmd) error { return nil }

// The whole point of the ticket, from the leaf's acceptance to the board: a
// queued send is confirmed by the next export and the WORK TASK reads
// `delivered` with its Deliver task closed. Dropping the delivery_sent emission
// leaves the task at done_locally and turns this red.
//
// The orchestrator is driven one drain at a time (the `--once` shape), exactly
// as internal/orchestrator's own integration tests do. R8 itself is untouched:
// D7 writes a task_events row from a connector sink — established practice
// (sink.go:482, jira/sink.go:276, google/sink.go:756) — and imports nothing from
// internal/orchestrator, so invariant 7 holds.
func TestSlackPromote_Integration_QueuedSendConfirmedAdvancesTheTask(t *testing.T) {
	ctx := context.Background()
	pool := newPromoPool(t, ctx)
	taskID := seedPromoTask(t, ctx, pool, "done_locally")
	const body = "the end-to-end queued reply: accepted by the leaf, clicked later, confirmed by the export"
	deliveryID := seedQueuedSend(t, ctx, pool, taskID, body)

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	ex := executor.New(reg, policy.NewStatic(reg.Names()...), audit.NewPGStore(pool))
	engine := orch.NewEngine(pool, ex, noopPublisher{}, orch.Config{})

	// Cursor at the current max so the drain sees only this test's events.
	var maxEvent int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(max(id),0) FROM task_events`).Scan(&maxEvent); err != nil {
		t.Fatalf("max event id: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE orchestrator_cursor SET last_event_id=$1, updated_at=now() WHERE name='orchestrator'`, maxEvent); err != nil {
		t.Fatalf("set cursor: %v", err)
	}

	// R3: the work finished locally, so a Deliver task exists (project delivery
	// mode is 'dashboard'). The event is the fixture; the rule under test is R8.
	if _, err := pool.Exec(ctx,
		`INSERT INTO task_events (task_id, event_type, payload)
		 VALUES ($1,'done_local','{"summary":"slack reply drafted and approved"}'::jsonb)`, taskID); err != nil {
		t.Fatalf("seed done_local: %v", err)
	}
	if _, err := engine.DrainOnce(ctx); err != nil {
		t.Fatalf("DrainOnce (R3): %v", err)
	}
	var deliverTaskID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM tasks WHERE parent_id=$1 AND title LIKE 'Deliver #%'`, taskID).Scan(&deliverTaskID); err != nil {
		t.Fatalf("R3 created no Deliver task: %v", err)
	}

	// The export brings our own message back and closes the loop.
	const msgID = "p1790500000000005"
	if err := runExport(t, ctx, pool, []slackweb.Message{ownMessage(msgID, body)}, slackweb.Config{}); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if d := readPromo(t, ctx, pool, deliveryID); d.status != "sent" {
		t.Fatalf("the export did not promote the queued row (status %q); the rest of this test is moot", d.status)
	}
	if _, err := engine.DrainOnce(ctx); err != nil {
		t.Fatalf("DrainOnce (R8): %v", err)
	}

	var workStatus, deliverStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, taskID).Scan(&workStatus); err != nil {
		t.Fatalf("read work task: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, deliverTaskID).Scan(&deliverStatus); err != nil {
		t.Fatalf("read Deliver task: %v", err)
	}
	if workStatus != "delivered" {
		t.Errorf("the work task reads %q after its queued send was confirmed, want delivered. This is the "+
			"defect D7 exists to prevent: confirmDelivery emits delivery_confirmed, R8 keys on delivery_sent, "+
			"and nothing reads delivery_confirmed — so the task is stranded at done_locally with no error "+
			"anywhere (criterion 28)", workStatus)
	}
	if deliverStatus != "closed" {
		t.Errorf("R3's Deliver task reads %q, want closed: R8 retires it when the delivery lands (criterion 28)",
			deliverStatus)
	}
}
