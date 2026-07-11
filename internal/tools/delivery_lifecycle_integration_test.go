//go:build integration

package tools_test

// Integration test for the SWT-8 delivery lifecycle (SPEC 08-draft-deliveries,
// criteria 2-6, 8, verification protocol step 2). Build-tagged `integration`
// AND env-gated on DATABASE_URL. Every delivery mutation goes through
// executor.Execute — the ONLY route to a handler (invariant 3) — with the REAL
// policy Matrix checker (kill switch / rate limit / channel tiers / human-only)
// over the compose db. The Gmail network call is replaced by an injected fake
// (the SPEC's adapter seam); NEVER a live Google call.
//
//   DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//     go test -tags integration -run Delivery ./internal/tools/
//
// GREENFIELD NOTE: migration 0006, the delivery tools, the policy Matrix + pg
// loader, and the tools.GmailSender seam do not exist yet, so under
// `-tags integration` this compile-FAILs / fails at the first Execute until they
// are implemented — the expected failure mode.
//
// Imposed seams (documented for the implementer; the SPEC leaves the exact
// injection shape open — "however the SPEC pins it"):
//
//   // The send_delivery handler's Gmail adapter is a package-level seam so the
//   // network call can be faked offline. cmd/* wire the real google.GmailSender
//   // (env base URL + TokenClient); tests inject a fake. Its ONLY caller is the
//   // send_delivery handler (invariant 4).
//   package tools
//   type GmailSender interface {
//       Send(ctx context.Context, fromUserID string, rawMIME []byte, threadID string) (externalID string, err error)
//   }
//   func SetGmailSender(s GmailSender)
//
//   // Policy Matrix + pg snapshot loader (SPEC criterion 4).
//   package policy
//   func NewPGSnapshotLoader(pool *pgxpool.Pool) SnapshotLoader
//   func NewMatrix(loader SnapshotLoader, fallback Checker) Checker
//
// Cross-suite discipline (SWT-6 mutual-cleanup pact): this suite seeds ONE
// inbound normalized_messages row (so send_delivery can resolve To / In-Reply-To)
// scoped 'itest-del-%'; that row is visible to triage's GLOBAL pending filter, so
// triage's cleanupTriage must gain matching 'itest-del-%' deletes (the pact-join
// obligation on the triage side — this file cleans only its OWN corpus, in FK
// order, rerunnably). All count assertions here are scoped to this corpus.

import (
	"context"
	"net/mail"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	delActor       = "dashboard:itest-del@example.com" // human-prefixed (matrix human_only)
	delSlug        = "itest-del-proj"
	delClient      = "itest-del-client"
	delAcctEmail   = "itest-del-a@example.com"
	delThreadKey   = "gmail:itest-del-a@example.com:gthread-1"
	delGThreadID   = "gthread-1"
	delInboundFrom = "client@itest-del.example"
	delInboundMID  = "<inbound-itest-del-1@itest-del.example>"
)

// ---- injected fake Gmail adapter ----------------------------------------------

// fakeGmailSender records every send and, crucially, reads the deliveries row at
// send time to PROVE sent_external_id was persisted BEFORE the network call
// (invariant 4 / criterion 5).
type fakeGmailSender struct {
	pool          *pgxpool.Pool
	calls         int
	lastRaw       []byte
	lastThread    string
	lastFromUser  string
	preSendStatus string // deliveries.status observed at send time (want 'sending')
}

func (f *fakeGmailSender) Send(ctx context.Context, fromUserID string, rawMIME []byte, threadID string) (string, error) {
	f.calls++
	f.lastRaw = rawMIME
	f.lastThread = threadID
	f.lastFromUser = fromUserID
	// The self-chosen Message-ID lives in the MIME; the committed row must
	// already carry it as sent_external_id (written BEFORE this call).
	if m, err := mail.ReadMessage(strings.NewReader(string(rawMIME))); err == nil {
		mid := m.Header.Get("Message-ID")
		_ = f.pool.QueryRow(ctx,
			`SELECT status FROM deliveries WHERE sent_external_id=$1`, mid).Scan(&f.preSendStatus)
	}
	return "gmail-api-id-xyz", nil
}

// ---- fixtures -----------------------------------------------------------------

type delFixture struct {
	accountID int64
	parentID  int64 // done_locally work task; the delivery attaches here
	threadID  int64
}

func seedDeliveryFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) delFixture {
	t.Helper()
	var fx delFixture

	// send-capable google mailbox account (thread_key mailbox segment == email).
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, send_enabled)
		 VALUES ('google', $1, true) RETURNING id`, delAcctEmail).Scan(&fx.accountID); err != nil {
		t.Fatalf("seed source_account: %v", err)
	}

	projID := seedProject(t, ctx, pool, delSlug, delClient)

	// The delivered work task, done_locally (task_mark_delivered target later).
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1, 'itest-del work task', 'claude', 'done_locally') RETURNING id`, projID).Scan(&fx.parentID); err != nil {
		t.Fatalf("seed parent task: %v", err)
	}

	// A raw item + thread + inbound message so send_delivery can resolve
	// To (last inbound sender) and In-Reply-To (last message-id).
	var rawID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1, 'itest-del-raw-1', '{}', 'itest-del-hash-1') RETURNING id`, fx.accountID).Scan(&rawID); err != nil {
		t.Fatalf("seed raw item: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO normalized_threads (thread_key, subject) VALUES ($1, 'login broken') RETURNING id`,
		delThreadKey).Scan(&fx.threadID); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1, $2, 'inbound', $3, now(), 'the login page is down', 'login broken', $4, 'gmail')`,
		rawID, fx.threadID, delInboundMID, delInboundFrom); err != nil {
		t.Fatalf("seed inbound message: %v", err)
	}
	return fx
}

func cleanupDeliveryData(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`UPDATE ops_flags SET value='{"frozen": false}' WHERE name='sending_frozen'`, nil},
		{`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE actor=$1)`, []any{delActor}},
		{`DELETE FROM audit_events WHERE actor=$1`, []any{delActor}},
		{`DELETE FROM approvals WHERE subject_type='delivery' AND subject_id IN
			(SELECT id FROM deliveries WHERE task_id IN
				(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)))`, []any{delSlug}},
		{`DELETE FROM task_events WHERE task_id IN
			(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{delSlug}},
		{`DELETE FROM deliveries WHERE task_id IN
			(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{delSlug}},
		{`DELETE FROM normalized_messages WHERE external_message_id=$1`, []any{delInboundMID}},
		{`DELETE FROM normalized_threads WHERE thread_key=$1`, []any{delThreadKey}},
		{`DELETE FROM raw_source_items WHERE source_account_id IN
			(SELECT id FROM source_accounts WHERE account_email=$1)`, []any{delAcctEmail}},
		{`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, []any{delSlug}},
		{`DELETE FROM decisions WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, []any{delSlug}},
		{`DELETE FROM projects WHERE slug=$1`, []any{delSlug}},
		{`DELETE FROM source_accounts WHERE account_email=$1`, []any{delAcctEmail}},
	}
	for _, st := range stmts {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

// deliveryExecutor builds the executor with the REAL policy Matrix (so kill
// switch / rate limit / channel tiers actually gate), falling back to the static
// allow-list for non-delivery tools.
func deliveryExecutor(pool *pgxpool.Pool) *executor.Executor {
	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	return executor.New(reg, checker, audit.NewPGStore(pool))
}

// ---- helpers ------------------------------------------------------------------

func deliveryStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, `SELECT status FROM deliveries WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatalf("read delivery %d status: %v", id, err)
	}
	return s
}

// callDenied asserts the tool call is refused by the policy matrix with the
// given rule and that a matching policy_decisions row landed (criterion 4).
func callDenied(t *testing.T, ctx context.Context, ex *executor.Executor, pool *pgxpool.Pool, tool, args, wantRule string) {
	t.Helper()
	_, err := ex.Execute(ctx, executor.Call{Tool: tool, Actor: delActor, Args: []byte(args)})
	if err == nil {
		t.Fatalf("%s expected a policy denial (%s), got nil error", tool, wantRule)
	}
	if !strings.Contains(err.Error(), "denied by policy") {
		t.Fatalf("%s error = %q, want a policy denial", tool, err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM policy_decisions p JOIN audit_events a ON a.id=p.audit_event_id
		 WHERE a.actor=$1 AND p.tool=$2 AND p.decision='deny' AND p.rule=$3`,
		delActor, tool, wantRule).Scan(&n); err != nil {
		t.Fatalf("count deny policy_decisions: %v", err)
	}
	if n < 1 {
		t.Errorf("no deny policy_decisions row for %s rule=%s (criterion 4)", tool, wantRule)
	}
}

func draftGmail(t *testing.T, ctx context.Context, ex *executor.Executor, parentID, threadID int64) int64 {
	t.Helper()
	out := callOK(t, ctx, ex, delActor, "draft_delivery",
		`{"task_id":`+itoa(parentID)+`,"channel":"gmail","subject":"Re: login broken","body":"draft body","thread_id":`+itoa(threadID)+`}`)
	var r struct {
		DeliveryID int64 `json:"delivery_id"`
	}
	mustUnmarshal(t, out, &r)
	if r.DeliveryID == 0 {
		t.Fatal("draft_delivery returned delivery_id 0")
	}
	return r.DeliveryID
}

func approve(t *testing.T, ctx context.Context, ex *executor.Executor, deliveryID int64) {
	t.Helper()
	callOK(t, ctx, ex, delActor, "approve_delivery", `{"delivery_id":`+itoa(deliveryID)+`}`)
}

// ---- the walk -----------------------------------------------------------------

func TestDelivery_Integration_FullLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()

	cleanupDeliveryData(t, ctx, pool)
	defer cleanupDeliveryData(t, ctx, pool)

	fx := seedDeliveryFixture(t, ctx, pool)

	fake := &fakeGmailSender{pool: pool}
	tools.SetGmailSender(fake)
	ex := deliveryExecutor(pool)

	// 1. draft_delivery (gmail): drafted; from_account_id resolved server-side
	//    from the thread's mailbox (never caller-chosen); created_by = actor.
	deliveryID := draftGmail(t, ctx, ex, fx.parentID, fx.threadID)
	if s := deliveryStatus(t, ctx, pool, deliveryID); s != "drafted" {
		t.Fatalf("after draft status = %q, want drafted", s)
	}
	var fromAcct *int64
	var createdBy string
	if err := pool.QueryRow(ctx,
		`SELECT from_account_id, created_by FROM deliveries WHERE id=$1`, deliveryID).Scan(&fromAcct, &createdBy); err != nil {
		t.Fatalf("read draft: %v", err)
	}
	if fromAcct == nil || *fromAcct != fx.accountID {
		t.Errorf("from_account_id = %v, want the thread's mailbox account %d (server-resolved)", fromAcct, fx.accountID)
	}
	if createdBy != delActor {
		t.Errorf("created_by = %q, want the actor %q", createdBy, delActor)
	}

	// 2. update_delivery: edits body while drafted.
	callOK(t, ctx, ex, delActor, "update_delivery",
		`{"delivery_id":`+itoa(deliveryID)+`,"body":"edited body — pushed the fix"}`)
	var body string
	if err := pool.QueryRow(ctx, `SELECT body FROM deliveries WHERE id=$1`, deliveryID).Scan(&body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(body, "edited body") {
		t.Errorf("update_delivery did not persist the edit; body=%q", body)
	}

	// 3. approve_delivery: drafted -> approved + approvals row.
	approve(t, ctx, ex, deliveryID)
	if s := deliveryStatus(t, ctx, pool, deliveryID); s != "approved" {
		t.Fatalf("after approve status = %q, want approved", s)
	}
	var apprCount int
	var decidedBy string
	if err := pool.QueryRow(ctx,
		`SELECT count(*), coalesce(max(decided_by),'') FROM approvals
		 WHERE subject_type='delivery' AND subject_id=$1`, deliveryID).Scan(&apprCount, &decidedBy); err != nil {
		t.Fatalf("read approvals: %v", err)
	}
	if apprCount != 1 {
		t.Errorf("approvals rows = %d, want 1 (subject_type=delivery)", apprCount)
	}
	if decidedBy != delActor {
		t.Errorf("approvals.decided_by = %q, want %q", decidedBy, delActor)
	}

	// 4. send_delivery (gmail via fake): sending -> sent; sent_external_id set
	//    (<sb-...>) BEFORE the network call; sent_at set; delivery_sent event.
	sentEventsBefore := eventCount(t, ctx, pool, fx.parentID, "delivery_sent")
	callOK(t, ctx, ex, delActor, "send_delivery", `{"delivery_id":`+itoa(deliveryID)+`}`)
	if s := deliveryStatus(t, ctx, pool, deliveryID); s != "sent" {
		t.Fatalf("after send status = %q, want sent", s)
	}
	var sentExtID *string
	var sentAt *string
	if err := pool.QueryRow(ctx,
		`SELECT sent_external_id, sent_at::text FROM deliveries WHERE id=$1`, deliveryID).Scan(&sentExtID, &sentAt); err != nil {
		t.Fatalf("read sent delivery: %v", err)
	}
	if sentExtID == nil || !strings.HasPrefix(*sentExtID, "<sb-") {
		t.Errorf("sent_external_id = %v, want a self-chosen <sb-...> Message-ID", sentExtID)
	}
	if sentAt == nil {
		t.Errorf("sent_at not stamped on a sent delivery")
	}
	if fake.calls != 1 {
		t.Fatalf("gmail sends = %d, want exactly 1", fake.calls)
	}
	// Ordering proof: at send time the committed row already carried the id.
	if fake.preSendStatus != "sending" {
		t.Errorf("delivery status at send time = %q, want 'sending' (sent_external_id + sending committed BEFORE the network call)", fake.preSendStatus)
	}
	if fake.lastThread != delGThreadID {
		t.Errorf("posted threadId = %q, want %q", fake.lastThread, delGThreadID)
	}
	// Threading headers assembled from the thread.
	m, err := mail.ReadMessage(strings.NewReader(string(fake.lastRaw)))
	if err != nil {
		t.Fatalf("sent raw is not a parseable message: %v", err)
	}
	if got := addrIn(m.Header.Get("From")); got != delAcctEmail {
		t.Errorf("From = %q, want the mailbox account %q (never model-chosen)", got, delAcctEmail)
	}
	if got := addrIn(m.Header.Get("To")); got != delInboundFrom {
		t.Errorf("To = %q, want the last inbound sender %q", got, delInboundFrom)
	}
	if got := m.Header.Get("In-Reply-To"); got != delInboundMID {
		t.Errorf("In-Reply-To = %q, want the last message-id %q", got, delInboundMID)
	}
	if got := m.Header.Get("Message-ID"); sentExtID == nil || got != *sentExtID {
		t.Errorf("Message-ID header = %q, want it to equal sent_external_id %v", got, sentExtID)
	}
	if eventCount(t, ctx, pool, fx.parentID, "delivery_sent") != sentEventsBefore+1 {
		t.Errorf("send_delivery did not emit a delivery_sent task_event")
	}

	// 5. resend refused idempotently (invariant 4): no second network call.
	_, err = ex.Execute(ctx, executor.Call{Tool: "send_delivery", Actor: delActor, Args: []byte(`{"delivery_id":` + itoa(deliveryID) + `}`)})
	if err == nil {
		t.Errorf("resend of an already-sent delivery must fail")
	}
	if fake.calls != 1 {
		t.Errorf("gmail sends after resend attempt = %d, want still 1 (never resend while sent_external_id present)", fake.calls)
	}

	// 6. rate limit: seed 10 gmail deliveries sent this hour, then a fresh
	//    approved gmail delivery is refused rate_limit.
	seedSentGmail(t, ctx, pool, fx.parentID, 10)
	rl := draftGmail(t, ctx, ex, fx.parentID, fx.threadID)
	approve(t, ctx, ex, rl)
	callDenied(t, ctx, ex, pool, "send_delivery", `{"delivery_id":`+itoa(rl)+`}`, "rate_limit")

	// 7. upwork_chat assisted tier: draft -> approve -> send DENIED (assisted) ->
	//    mark_delivery_sent confirms manually (sent_external_id NULL).
	outUp := callOK(t, ctx, ex, delActor, "draft_delivery",
		`{"task_id":`+itoa(fx.parentID)+`,"channel":"upwork_chat","body":"thanks, will do","target_ref":"upwork_crm:itest-del:chat"}`)
	var up struct {
		DeliveryID int64 `json:"delivery_id"`
	}
	mustUnmarshal(t, outUp, &up)
	approve(t, ctx, ex, up.DeliveryID)
	callDenied(t, ctx, ex, pool, "send_delivery", `{"delivery_id":`+itoa(up.DeliveryID)+`}`, "channel_assisted")

	upEventsBefore := eventCount(t, ctx, pool, fx.parentID, "delivery_sent")
	callOK(t, ctx, ex, delActor, "mark_delivery_sent", `{"delivery_id":`+itoa(up.DeliveryID)+`}`)
	if s := deliveryStatus(t, ctx, pool, up.DeliveryID); s != "sent" {
		t.Errorf("after mark_delivery_sent status = %q, want sent", s)
	}
	var upExtID *string
	if err := pool.QueryRow(ctx, `SELECT sent_external_id FROM deliveries WHERE id=$1`, up.DeliveryID).Scan(&upExtID); err != nil {
		t.Fatalf("read upwork delivery: %v", err)
	}
	if upExtID != nil {
		t.Errorf("mark_delivery_sent sent_external_id = %v, want NULL (post-hoc match fills it later)", *upExtID)
	}
	if eventCount(t, ctx, pool, fx.parentID, "delivery_sent") != upEventsBefore+1 {
		t.Errorf("mark_delivery_sent did not emit a delivery_sent event")
	}

	// 8. kill switch: freeze -> send of a fresh approved gmail delivery is denied
	//    kill_switch; then unfreeze.
	callOK(t, ctx, ex, delActor, "set_sending_frozen", `{"frozen":true}`)
	ks := draftGmail(t, ctx, ex, fx.parentID, fx.threadID)
	approve(t, ctx, ex, ks)
	callDenied(t, ctx, ex, pool, "send_delivery", `{"delivery_id":`+itoa(ks)+`}`, "kill_switch")
	callOK(t, ctx, ex, delActor, "set_sending_frozen", `{"frozen":false}`)

	// 9. task_mark_delivered: done_locally -> delivered; idempotent replay.
	callOK(t, ctx, ex, delActor, "task_mark_delivered", `{"task_id":`+itoa(fx.parentID)+`}`)
	if s := taskStatus(t, ctx, pool, fx.parentID); s != "delivered" {
		t.Fatalf("after task_mark_delivered status = %q, want delivered", s)
	}
	callOK(t, ctx, ex, delActor, "task_mark_delivered", `{"task_id":`+itoa(fx.parentID)+`}`) // idempotent no-op
	if s := taskStatus(t, ctx, pool, fx.parentID); s != "delivered" {
		t.Fatalf("idempotent task_mark_delivered changed status to %q", s)
	}

	// 10. audit trail: every ok call and every denial produced an audit row
	//     (invariant 3).
	for _, tool := range []string{"draft_delivery", "update_delivery", "approve_delivery", "send_delivery", "mark_delivery_sent", "task_mark_delivered", "set_sending_frozen"} {
		var ok int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM audit_events WHERE actor=$1 AND tool=$2 AND status='ok'`, delActor, tool).Scan(&ok); err != nil {
			t.Fatalf("count audit ok for %s: %v", tool, err)
		}
		if ok < 1 {
			t.Errorf("no ok audit_events row for %s (invariant 3)", tool)
		}
	}
	var denied int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE actor=$1 AND tool='send_delivery' AND status='denied'`, delActor).Scan(&denied); err != nil {
		t.Fatalf("count denied audit: %v", err)
	}
	if denied < 3 {
		t.Errorf("denied send_delivery audit rows = %d, want >= 3 (rate_limit, channel_assisted, kill_switch)", denied)
	}
}

// seedSentGmail inserts n gmail deliveries sent within the current hour (for the
// rate-limit path), scoped to the test's task so cleanup reaches them.
func seedSentGmail(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO deliveries (task_id, channel, status, sent_external_id, sent_at)
			 VALUES ($1, 'gmail', 'sent', $2, now())`,
			taskID, "<sb-itest-del-seed-"+itoa(int64(i))+"@example.com>"); err != nil {
			t.Fatalf("seed sent gmail %d: %v", i, err)
		}
	}
}

// addrIn extracts the bare address from a header value.
func addrIn(header string) string {
	a, err := mail.ParseAddress(header)
	if err != nil {
		return header
	}
	return a.Address
}
