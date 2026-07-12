//go:build integration

package tools_test

// Integration test for the live jira_comment delivery channel (SPEC
// 09-jira-github-connectors, criteria 6-8 + invariants 4/5). Build-tagged
// `integration` AND env-gated on DATABASE_URL. Every mutation goes through
// executor.Execute with the REAL policy Matrix (deliveryExecutor, defined in
// delivery_lifecycle_integration_test.go) over the compose db; the Jira network
// call is replaced by an injected fake JiraSender — NEVER a live Jira call.
//
//   DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//     go test -tags integration -run Jira ./internal/tools/
//
// GREENFIELD NOTE: the jira_comment draft validation, the send_delivery jira
// branch, and the tools.JiraSender seam do not exist yet, so under
// `-tags integration` this fails at the first jira_comment draft_delivery until
// they are implemented — the expected failure mode.
//
// Imposed seams (documented for the implementer; SPEC criterion 7 leaves the
// exact injection shape open):
//
//   // Beside GmailSender in internal/tools/delivery.go. Jira assigns the comment
//   // id AFTER the POST, so Send returns it (unlike gmail's self-chosen id).
//   // cmd/* wire the real connector/jira.Sender (email + pgp_sym_decrypted token);
//   // tests inject a fake. Its ONLY caller is the send_delivery jira branch (inv 4).
//   package tools
//   type JiraSender interface {
//       Send(ctx context.Context, siteHost, issueKey, body string) (commentID string, err error)
//   }
//   func SetJiraSender(s JiraSender)
//
// Key formats pinned by the SPEC: draft target_ref = jira:{site_host}:{issueKey}
// (thread_key form, criterion 6); sent_external_id = jira:{site_host}:comment:{id}
// (criterion 7). from_account is resolved SERVER-SIDE by matching the target_ref
// site_host against provider='jira' accounts' domain_default — never caller-chosen.
//
// Cross-suite discipline: this suite owns everything under 'itest-jdl-%' and the
// jira account itest-jdl@example.com; it cleans its OWN corpus in FK order,
// rerunnably. It creates no inbound normalized_messages, so triage's global
// pending filter is untouched (no pact obligation).

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	jdlActor    = "dashboard:itest-jdl@example.com"
	jdlSlug     = "itest-jdl-proj"
	jdlClient   = "itest-jdl-client"
	jdlAcct     = "itest-jdl@example.com"
	jdlSite     = "sspataro.atlassian.net"
	jdlBaseURL  = "https://sspataro.atlassian.net"
	jdlIssueKey = "CRM-1"
	jdlCommID   = "990011" // the id the fake Jira assigns AFTER the POST
)

func jdlTargetRef() string { return "jira:" + jdlSite + ":" + jdlIssueKey }
func jdlSentExtID() string { return "jira:" + jdlSite + ":comment:" + jdlCommID }

// fakeJiraSender records every send and reads the deliveries row AT send time to
// prove `sending` was committed with sent_external_id STILL NULL before the
// network call (invariant 4: Jira id is only knowable post-call).
type fakeJiraSender struct {
	pool           *pgxpool.Pool
	calls          int
	lastSite       string
	lastKey        string
	lastBody       string
	preSendStatus  string
	preSendExtNull bool
}

func (f *fakeJiraSender) Send(ctx context.Context, siteHost, issueKey, body string) (string, error) {
	f.calls++
	f.lastSite, f.lastKey, f.lastBody = siteHost, issueKey, body
	// The delivery this call belongs to must be 'sending' with a NULL
	// sent_external_id at this instant.
	_ = f.pool.QueryRow(ctx,
		`SELECT status, sent_external_id IS NULL
		   FROM deliveries
		  WHERE channel='jira_comment' AND target_ref=$1 AND status='sending'`,
		jdlTargetRef()).Scan(&f.preSendStatus, &f.preSendExtNull)
	return jdlCommID, nil
}

func seedJiraDeliveryFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sendEnabled bool) (accountID, taskID int64) {
	t.Helper()
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, refresh_token_encrypted, scopes, domain_default, send_enabled)
		 VALUES ('jira', $1, pgp_sym_encrypt('dummy-token','k'), ARRAY['CRM'], $2, $3) RETURNING id`,
		jdlAcct, jdlBaseURL, sendEnabled).Scan(&accountID); err != nil {
		t.Fatalf("seed jira source_account: %v", err)
	}
	projID := seedProject(t, ctx, pool, jdlSlug, jdlClient)
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1, 'itest-jdl work', 'claude', 'done_locally') RETURNING id`, projID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return accountID, taskID
}

func cleanupJiraDelivery(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE actor=$1)`, []any{jdlActor}},
		{`DELETE FROM audit_events WHERE actor=$1`, []any{jdlActor}},
		{`DELETE FROM approvals WHERE subject_type='delivery' AND subject_id IN
			(SELECT id FROM deliveries WHERE task_id IN
				(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)))`, []any{jdlSlug}},
		{`DELETE FROM task_events WHERE task_id IN
			(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{jdlSlug}},
		{`DELETE FROM deliveries WHERE task_id IN
			(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{jdlSlug}},
		{`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, []any{jdlSlug}},
		{`DELETE FROM decisions WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, []any{jdlSlug}},
		{`DELETE FROM projects WHERE slug=$1`, []any{jdlSlug}},
		{`DELETE FROM source_accounts WHERE provider='jira' AND account_email=$1`, []any{jdlAcct}},
	}
	for _, st := range stmts {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

func TestJiraDelivery_Integration_DraftApproveSend(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()

	cleanupJiraDelivery(t, ctx, pool)
	defer cleanupJiraDelivery(t, ctx, pool)

	acctID, taskID := seedJiraDeliveryFixture(t, ctx, pool, true)

	fake := &fakeJiraSender{pool: pool}
	tools.SetJiraSender(fake)
	ex := deliveryExecutor(pool)

	// 1. draft_delivery (jira_comment): drafted; from_account resolved server-side
	//    from the target_ref site_host (never caller-chosen).
	out := callOK(t, ctx, ex, jdlActor, "draft_delivery",
		`{"task_id":`+itoa(taskID)+`,"channel":"jira_comment","body":"pushed the fix, please retest","target_ref":"`+jdlTargetRef()+`"}`)
	var r struct {
		DeliveryID int64 `json:"delivery_id"`
	}
	mustUnmarshal(t, out, &r)
	if r.DeliveryID == 0 {
		t.Fatal("draft_delivery returned delivery_id 0")
	}
	var fromAcct *int64
	if err := pool.QueryRow(ctx, `SELECT from_account_id FROM deliveries WHERE id=$1`, r.DeliveryID).Scan(&fromAcct); err != nil {
		t.Fatalf("read draft: %v", err)
	}
	if fromAcct == nil || *fromAcct != acctID {
		t.Errorf("from_account_id = %v, want the jira account %d resolved by site_host (server-side)", fromAcct, acctID)
	}

	// 2. approve.
	callOK(t, ctx, ex, jdlActor, "approve_delivery", `{"delivery_id":`+itoa(r.DeliveryID)+`}`)

	// 3. send_delivery via fake JiraSender: sending committed pre-network, then
	//    sent_external_id = jira:{site}:comment:{id} + status sent + delivery_sent.
	before := eventCount(t, ctx, pool, taskID, "delivery_sent")
	callOK(t, ctx, ex, jdlActor, "send_delivery", `{"delivery_id":`+itoa(r.DeliveryID)+`}`)

	if fake.calls != 1 {
		t.Fatalf("jira sends = %d, want exactly 1", fake.calls)
	}
	if fake.preSendStatus != "sending" || !fake.preSendExtNull {
		t.Errorf("at send time delivery was status=%q sent_external_id_null=%v, want sending + NULL (id assigned POST-call, invariant 4)",
			fake.preSendStatus, fake.preSendExtNull)
	}
	if fake.lastSite != jdlSite || fake.lastKey != jdlIssueKey {
		t.Errorf("posted to site=%q key=%q, want %q / %q (parsed from target_ref)", fake.lastSite, fake.lastKey, jdlSite, jdlIssueKey)
	}
	if strings.TrimSpace(fake.lastBody) == "" {
		t.Errorf("posted an empty comment body")
	}

	var status string
	var sentExtID *string
	if err := pool.QueryRow(ctx,
		`SELECT status, sent_external_id FROM deliveries WHERE id=$1`, r.DeliveryID).Scan(&status, &sentExtID); err != nil {
		t.Fatalf("read sent delivery: %v", err)
	}
	if status != "sent" {
		t.Errorf("status = %q, want sent", status)
	}
	if sentExtID == nil || *sentExtID != jdlSentExtID() {
		t.Errorf("sent_external_id = %v, want %q (post-call comment id, criterion 7)", sentExtID, jdlSentExtID())
	}
	if eventCount(t, ctx, pool, taskID, "delivery_sent") != before+1 {
		t.Errorf("send_delivery did not emit a delivery_sent task_event")
	}

	// 4. resend refused idempotently: no second network call.
	if _, err := ex.Execute(ctx, executor.Call{Tool: "send_delivery", Actor: jdlActor,
		Args: []byte(`{"delivery_id":` + itoa(r.DeliveryID) + `}`)}); err == nil {
		t.Errorf("resend of an already-sent jira delivery must fail")
	}
	if fake.calls != 1 {
		t.Errorf("jira sends after resend = %d, want still 1 (never resend while sent_external_id present)", fake.calls)
	}
}

// The operator go-live gate: jira-auth registers accounts send_enabled=false;
// an approved jira_comment must NOT send until the operator flips the flag
// (SPEC criterion 6, same gate gmail enforces). The refusal happens inside the
// pre-network tx, so the delivery stays approved — not failed, no side effects.
func TestJiraDelivery_Integration_SendEnabledGate(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()

	cleanupJiraDelivery(t, ctx, pool)
	defer cleanupJiraDelivery(t, ctx, pool)

	_, taskID := seedJiraDeliveryFixture(t, ctx, pool, false)

	fake := &fakeJiraSender{pool: pool}
	tools.SetJiraSender(fake)
	ex := deliveryExecutor(pool)

	out := callOK(t, ctx, ex, jdlActor, "draft_delivery",
		`{"task_id":`+itoa(taskID)+`,"channel":"jira_comment","body":"gate check","target_ref":"`+jdlTargetRef()+`"}`)
	var r struct {
		DeliveryID int64 `json:"delivery_id"`
	}
	mustUnmarshal(t, out, &r)
	callOK(t, ctx, ex, jdlActor, "approve_delivery", `{"delivery_id":`+itoa(r.DeliveryID)+`}`)

	_, err := ex.Execute(ctx, executor.Call{Tool: "send_delivery", Actor: jdlActor,
		Args: []byte(`{"delivery_id":` + itoa(r.DeliveryID) + `}`)})
	if err == nil {
		t.Fatal("send_delivery succeeded on a send_enabled=false jira account")
	}
	if !strings.Contains(err.Error(), "not send-enabled") {
		t.Errorf("refusal error = %v, want the not-send-enabled gate", err)
	}
	if fake.calls != 0 {
		t.Errorf("jira sends = %d, want 0 (gate is pre-network)", fake.calls)
	}
	var status string
	var extNull bool
	if err := pool.QueryRow(ctx,
		`SELECT status, sent_external_id IS NULL FROM deliveries WHERE id=$1`, r.DeliveryID).Scan(&status, &extNull); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if status != "approved" || !extNull {
		t.Errorf("delivery after refused send = %s ext_null=%v, want approved + NULL (tx rolled back)", status, extNull)
	}
}
