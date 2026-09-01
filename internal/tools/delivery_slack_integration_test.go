//go:build integration

package tools_test

// Integration test for the SWT-12 slack_reply send promotion (SPEC
// slack-send-promotion, criteria 6-9, 12, 18-19, 21). Build-tagged `integration`
// AND env-gated on DATABASE_URL. Every mutation goes through executor.Execute
// with the REAL policy Matrix (deliveryExecutor, defined in
// delivery_lifecycle_integration_test.go) over the compose db; the Slack Web
// bridge is replaced by an injected fake tools.SlackSender — NEVER a live bridge,
// never a browser.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -run SlackDelivery ./internal/tools/
//
// GREENFIELD NOTE: the tools.SlackSender seam, the sendSlackReply branch, the
// markDeliverySent extension, mark_delivery_failed, and migration 0011's
// deliveries.approval_source column do not exist yet, so under
// `-tags integration` this compile-FAILs (SetSlackSender / SlackSender undefined)
// and then fails at the first send — the expected failure mode.
//
// Contract pinned by the SPEC (criterion 6): the seam is
//
//	package tools
//	type SlackSender interface { Send(ctx context.Context, targetURL, text string) error }
//	func SetSlackSender(s SlackSender)
//
// Definite-vs-ambiguous classification (criterion 9) uses
// *slackweb.SendRejectedError for a DEFINITE 4xx refusal and leaves 5xx /
// transport / timeout errors untyped, mirroring google.SendRejectedError. See
// internal/connector/slackweb/send_test.go for that side of the contract.
//
// UNRESOLVED SPEC DETAIL, deliberately not decided here: criterion 19 says
// mark_delivery_sent accepts a drafted row "when approval_source='leaf_token'",
// but no tool in this ticket takes a new argument that would put 'leaf_token'
// there (draft_delivery is explicitly unchanged). These tests therefore stamp
// approval_source directly on the fixture row and require mark_delivery_sent to
// read the ROW, not trust a caller-supplied gate name. If the implementer instead
// adds an argument, the row-reading assertions still hold.
//
// Cross-suite discipline (the SWT-6 mutual-cleanup pact): this suite owns
// everything under 'itest-sds-%' plus the synthetic Slack account
// tsdstest@slack-web.local; it cleans its OWN corpus in FK order, rerunnably, at
// the start AND end of every test. It creates no inbound normalized_messages, so
// triage's global pending filter is untouched (no pact obligation). It leaves
// ops_flags.sending_frozen false.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	sdsActor  = "dashboard:itest-sds@example.com"
	sdsSlug   = "itest-sds-proj"
	sdsClient = "itest-sds-client"
	// The synthetic per-workspace account. slackweb.PGSink.EnsureAccount builds
	// it as strings.ToLower(workspace.ID) + "@slack-web.local", while a target URL
	// always carries the workspace id UPPERCASE (url.go's ^T[A-Z0-9]+$). The
	// send branch must therefore resolve the account case-insensitively — this
	// fixture seeds only the real, lowercased spelling so a case-sensitive lookup
	// fails loudly instead of quietly reporting "no account".
	sdsWorkspace = "TSDSTEST"
	sdsAcct      = "tsdstest@slack-web.local"
	sdsTarget    = "https://app.slack.com/client/TSDSTEST/CSDSTEST"
	sdsBody      = "pushed the fix; ready for your retest"
)

// ---- injected fake Slack bridge -------------------------------------------------

// fakeSlackSender records every send and reads the deliveries row AT send time to
// prove `sending` was committed BEFORE the bridge call with sent_external_id
// still NULL (criterion 7-8: a browser click has no reservable external id, so
// nothing may fabricate one at send time).
type fakeSlackSender struct {
	pool           *pgxpool.Pool
	calls          int
	lastTarget     string
	lastText       string
	preSendStatus  string
	preSendExtNull bool
	err            error // what the bridge "returns"
}

func (f *fakeSlackSender) Send(ctx context.Context, targetURL, text string) error {
	f.calls++
	f.lastTarget, f.lastText = targetURL, text
	_ = f.pool.QueryRow(ctx,
		`SELECT status, sent_external_id IS NULL FROM deliveries
		  WHERE channel='slack_reply' AND target_ref=$1 AND status='sending'
		  ORDER BY id DESC LIMIT 1`, targetURL).Scan(&f.preSendStatus, &f.preSendExtNull)
	return f.err
}

// ---- fixtures ------------------------------------------------------------------

func seedSlackDeliveryFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sendEnabled bool) (accountID, taskID int64) {
	t.Helper()
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, domain_default, scopes, send_enabled, calendar_in_availability)
		 VALUES ('slack_web', $1, 'https://app.slack.com/client/TSDSTEST', ARRAY['CSDSTEST'], $2, false)
		 RETURNING id`, sdsAcct, sendEnabled).Scan(&accountID); err != nil {
		t.Fatalf("seed slack_web source_account: %v", err)
	}
	projID := seedProject(t, ctx, pool, sdsSlug, sdsClient)
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1, 'itest-sds work', 'claude', 'done_locally') RETURNING id`, projID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return accountID, taskID
}

func cleanupSlackDelivery(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`UPDATE ops_flags SET value='{"frozen": false}' WHERE name='sending_frozen'`, nil},
		{`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE actor=$1)`, []any{sdsActor}},
		{`DELETE FROM audit_events WHERE actor=$1`, []any{sdsActor}},
		{`DELETE FROM approvals WHERE subject_type='delivery' AND subject_id IN
			(SELECT id FROM deliveries WHERE task_id IN
				(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)))`, []any{sdsSlug}},
		{`DELETE FROM task_events WHERE task_id IN
			(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{sdsSlug}},
		{`DELETE FROM deliveries WHERE task_id IN
			(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{sdsSlug}},
		{`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, []any{sdsSlug}},
		{`DELETE FROM decisions WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, []any{sdsSlug}},
		{`DELETE FROM projects WHERE slug=$1`, []any{sdsSlug}},
		{`DELETE FROM source_accounts WHERE provider='slack_web' AND account_email=$1`, []any{sdsAcct}},
	}
	for _, st := range stmts {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

// ---- helpers -------------------------------------------------------------------

type slackSuite struct {
	pool   *pgxpool.Pool
	ex     *executor.Executor
	fake   *fakeSlackSender
	taskID int64
}

func newSlackSuite(t *testing.T, ctx context.Context, sendEnabled bool) slackSuite {
	t.Helper()
	pool := newToolsPool(t, ctx)
	t.Cleanup(pool.Close)
	cleanupSlackDelivery(t, ctx, pool)
	t.Cleanup(func() { cleanupSlackDelivery(t, ctx, pool) })

	_, taskID := seedSlackDeliveryFixture(t, ctx, pool, sendEnabled)
	fake := &fakeSlackSender{pool: pool}
	tools.SetSlackSender(fake)
	return slackSuite{pool: pool, ex: deliveryExecutor(pool), fake: fake, taskID: taskID}
}

func (s slackSuite) draft(t *testing.T, ctx context.Context, body string) int64 {
	t.Helper()
	out := callOK(t, ctx, s.ex, sdsActor, "draft_delivery",
		`{"task_id":`+itoa(s.taskID)+`,"channel":"slack_reply","body":"`+body+`","target_ref":"`+sdsTarget+`"}`)
	var r struct {
		DeliveryID int64 `json:"delivery_id"`
	}
	mustUnmarshal(t, out, &r)
	if r.DeliveryID == 0 {
		t.Fatal("draft_delivery returned delivery_id 0")
	}
	return r.DeliveryID
}

func (s slackSuite) call(t *testing.T, ctx context.Context, tool string, id int64) {
	t.Helper()
	callOK(t, ctx, s.ex, sdsActor, tool, `{"delivery_id":`+itoa(id)+`}`)
}

func (s slackSuite) tryCall(ctx context.Context, tool string, id int64) error {
	_, err := s.ex.Execute(ctx, executor.Call{Tool: tool, Actor: sdsActor,
		Args: []byte(`{"delivery_id":` + itoa(id) + `}`)})
	return err
}

// sdsRow is the delivery state the assertions care about.
type sdsRow struct {
	status         string
	sentExternalID *string
	sentAt         *string
	confirmedAt    *string
	errText        *string
	approvalSource *string
}

func (s slackSuite) row(t *testing.T, ctx context.Context, id int64) sdsRow {
	t.Helper()
	var r sdsRow
	if err := s.pool.QueryRow(ctx,
		`SELECT status, sent_external_id, sent_at::text, confirmed_at::text, error, approval_source
		   FROM deliveries WHERE id=$1`, id).
		Scan(&r.status, &r.sentExternalID, &r.sentAt, &r.confirmedAt, &r.errText, &r.approvalSource); err != nil {
		t.Fatalf("read delivery %d (apply migration 0011 for approval_source): %v", id, err)
	}
	return r
}

func (s slackSuite) approvalCount(t *testing.T, ctx context.Context, id int64) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM approvals WHERE subject_type='delivery' AND subject_id=$1`, id).Scan(&n); err != nil {
		t.Fatalf("count approvals for delivery %d: %v", id, err)
	}
	return n
}

// sdsDenied asserts a POLICY denial (not a handler refusal) with the given rule
// and that the matching policy_decisions row landed (invariant 3).
func sdsDenied(t *testing.T, ctx context.Context, s slackSuite, tool string, id int64, wantRule string) {
	t.Helper()
	err := s.tryCall(ctx, tool, id)
	if err == nil {
		t.Fatalf("%s expected a policy denial (%s), got nil error", tool, wantRule)
	}
	if !strings.Contains(err.Error(), "denied by policy") {
		t.Fatalf("%s error = %q, want a policy denial (%s)", tool, err, wantRule)
	}
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM policy_decisions p JOIN audit_events a ON a.id=p.audit_event_id
		  WHERE a.actor=$1 AND p.tool=$2 AND p.decision='deny' AND p.rule=$3`,
		sdsActor, tool, wantRule).Scan(&n); err != nil {
		t.Fatalf("count deny policy_decisions: %v", err)
	}
	if n < 1 {
		t.Errorf("no deny policy_decisions row for %s rule=%s", tool, wantRule)
	}
}

// setApprovalSource stamps the gate marker directly. Used to build manual-path
// (leaf_token) fixtures without pretending a tool wrote them.
func (s slackSuite) setApprovalSource(t *testing.T, ctx context.Context, id int64, source any) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, `UPDATE deliveries SET approval_source=$2 WHERE id=$1`, id, source); err != nil {
		t.Fatalf("stamp approval_source on delivery %d: %v", id, err)
	}
}

// ---- criterion 7 + 8 + 18: the two-phase send ----------------------------------

func TestSlackDelivery_Integration_TwoPhaseSend(t *testing.T) {
	ctx := context.Background()
	s := newSlackSuite(t, ctx, true)

	// 1. draft. A drafted row has NO gate yet: approval_source must be NULL, not
	//    defaulted to 'switchboard'. A column default would make every drafted row
	//    claim a switchboard gate it never got — and would then collide with
	//    criterion 19, which refuses exactly that combination on the manual path.
	id := s.draft(t, ctx, sdsBody)
	before := s.row(t, ctx, id)
	if before.status != "drafted" {
		t.Fatalf("after draft status = %q, want drafted", before.status)
	}
	if before.approvalSource != nil {
		t.Errorf("approval_source after draft = %q, want NULL (no gate has been applied yet; a "+
			"DEFAULT 'switchboard' would break the manual path's drafted+leaf_token edge)", *before.approvalSource)
	}
	// target_ref is stored canonically (SWT-13 rule, criterion 14).
	var targetRef string
	if err := s.pool.QueryRow(ctx, `SELECT target_ref FROM deliveries WHERE id=$1`, id).Scan(&targetRef); err != nil {
		t.Fatalf("read target_ref: %v", err)
	}
	want, err := slackweb.ParseTargetURL(sdsTarget)
	if err != nil {
		t.Fatalf("ParseTargetURL(%s): %v", sdsTarget, err)
	}
	if targetRef != want.CanonicalURL() {
		t.Errorf("target_ref = %q, want the canonical form %q", targetRef, want.CanonicalURL())
	}

	// 2. approve: the delivery row IS the approval of record, and the gate
	//    authority is recorded ON the row (criterion 18).
	s.call(t, ctx, "approve_delivery", id)
	approved := s.row(t, ctx, id)
	if approved.status != "approved" {
		t.Fatalf("after approve status = %q, want approved", approved.status)
	}
	if approved.approvalSource == nil || *approved.approvalSource != "switchboard" {
		t.Fatalf("approval_source after approve_delivery = %v, want 'switchboard' (criterion 18: written in "+
			"the SAME tx as the status transition — a crash must never leave a row whose gate is unknown)",
			approved.approvalSource)
	}
	if s.approvalCount(t, ctx, id) != 1 {
		t.Errorf("approvals rows after approve_delivery = %d, want 1", s.approvalCount(t, ctx, id))
	}

	// 3. poison the stored body with an AI attribution line, bypassing the tool
	//    (draft_delivery already scrubs at write time). Criterion 7 requires the
	//    send path to scrub AGAIN before the bridge call — invariant 6's belt.
	if _, err := s.pool.Exec(ctx,
		`UPDATE deliveries SET body = body || E'\n\nCo-Authored-By: Claude <noreply@anthropic.com>' WHERE id=$1`, id); err != nil {
		t.Fatalf("poison body: %v", err)
	}

	// 4. send: `sending` committed BEFORE the bridge call.
	eventsBefore := eventCount(t, ctx, s.pool, s.taskID, "delivery_sent")
	s.call(t, ctx, "send_delivery", id)

	if s.fake.calls != 1 {
		t.Fatalf("bridge sends = %d, want exactly 1", s.fake.calls)
	}
	if s.fake.preSendStatus != "sending" || !s.fake.preSendExtNull {
		t.Errorf("at bridge-call time the row was status=%q sent_external_id_null=%v, want sending + NULL "+
			"(criterion 7: 'sending' commits BEFORE the click; criterion 8: nothing fabricates a sent_external_id)",
			s.fake.preSendStatus, s.fake.preSendExtNull)
	}
	if s.fake.lastTarget != want.CanonicalURL() {
		t.Errorf("bridge target = %q, want the approved canonical destination %q", s.fake.lastTarget, want.CanonicalURL())
	}
	if !strings.Contains(s.fake.lastText, "ready for your retest") {
		t.Errorf("bridge text = %q, want the approved body", s.fake.lastText)
	}
	if strings.Contains(strings.ToLower(s.fake.lastText), "co-authored-by") {
		t.Errorf("bridge text carried an AI attribution line: %q — google.ScrubAIAttribution must run on the "+
			"body before the bridge call (criterion 7, invariant 6)", s.fake.lastText)
	}

	sent := s.row(t, ctx, id)
	if sent.status != "sent" {
		t.Errorf("status after send = %q, want sent", sent.status)
	}
	if sent.sentAt == nil {
		t.Errorf("sent_at not stamped on a sent Slack delivery")
	}
	if sent.sentExternalID != nil {
		t.Errorf("sent_external_id after send = %q, want NULL — Slack Web exposes no message id at click time; "+
			"the export stamps it later (criterion 8: no code path writes a fabricated id at send time)",
			*sent.sentExternalID)
	}
	if sent.errText != nil {
		t.Errorf("error after a successful send = %q, want NULL", *sent.errText)
	}
	if eventCount(t, ctx, s.pool, s.taskID, "delivery_sent") != eventsBefore+1 {
		t.Errorf("send_delivery did not emit a delivery_sent task_event")
	}

	// 5. resend of a 'sent' row is refused; no second click.
	if err := s.tryCall(ctx, "send_delivery", id); err == nil {
		t.Errorf("resend of a sent Slack delivery must fail")
	}
	if s.fake.calls != 1 {
		t.Errorf("bridge sends after resend attempt = %d, want still 1", s.fake.calls)
	}
}

// Invariant 4, the absolute rule: a present sent_external_id refuses resend
// forever. Reachable in practice after the export confirmed a 'sending' row.
func TestSlackDelivery_Integration_NeverResendsWithSentExternalID(t *testing.T) {
	ctx := context.Background()
	s := newSlackSuite(t, ctx, true)

	id := s.draft(t, ctx, sdsBody)
	s.call(t, ctx, "approve_delivery", id)
	// The export stamped an id (and the operator re-approved by hand, the worst
	// case this guard exists for).
	if _, err := s.pool.Exec(ctx,
		`UPDATE deliveries SET sent_external_id='slack:TSDSTEST:CSDSTEST:p1780000000000001', confirmed_at=now()
		  WHERE id=$1`, id); err != nil {
		t.Fatalf("stamp sent_external_id: %v", err)
	}

	err := s.tryCall(ctx, "send_delivery", id)
	if err == nil {
		t.Fatal("send_delivery on a row that already carries sent_external_id must fail (invariant 4)")
	}
	if !strings.Contains(err.Error(), "sent_external_id") {
		t.Errorf("refusal error = %v, want the never-resend guard to name sent_external_id", err)
	}
	if s.fake.calls != 0 {
		t.Errorf("bridge sends = %d, want 0 (the guard is pre-network)", s.fake.calls)
	}
}

// Criterion 7's two pre-network gates: status='approved' and the workspace
// account's send_enabled=true. Both refuse inside the tx, so the row is untouched
// and no click happens.
func TestSlackDelivery_Integration_RequiresApprovedStatus(t *testing.T) {
	ctx := context.Background()
	s := newSlackSuite(t, ctx, true)

	id := s.draft(t, ctx, sdsBody)
	err := s.tryCall(ctx, "send_delivery", id)
	if err == nil {
		t.Fatal("send_delivery on a drafted Slack row must fail (only approved deliveries send)")
	}
	if s.fake.calls != 0 {
		t.Errorf("bridge sends = %d, want 0", s.fake.calls)
	}
	if r := s.row(t, ctx, id); r.status != "drafted" {
		t.Errorf("status after refused send = %q, want drafted (tx rolled back)", r.status)
	}
}

func TestSlackDelivery_Integration_SendEnabledGate(t *testing.T) {
	ctx := context.Background()
	// The operator go-live gate: slackweb.EnsureAccount inserts send_enabled=false
	// and never updates it, so the default is safely off per workspace.
	s := newSlackSuite(t, ctx, false)

	id := s.draft(t, ctx, sdsBody)
	s.call(t, ctx, "approve_delivery", id)

	err := s.tryCall(ctx, "send_delivery", id)
	if err == nil {
		t.Fatal("send_delivery succeeded for a send_enabled=false Slack workspace")
	}
	if !strings.Contains(err.Error(), "not send-enabled") {
		t.Errorf("refusal error = %v, want the not-send-enabled gate. If this says the account was not FOUND, "+
			"the workspace lookup is case-sensitive: EnsureAccount writes lower(workspace_id)@slack-web.local "+
			"while a target URL always carries the id uppercase", err)
	}
	if s.fake.calls != 0 {
		t.Errorf("bridge sends = %d, want 0 (the gate is pre-network)", s.fake.calls)
	}
	if r := s.row(t, ctx, id); r.status != "approved" {
		t.Errorf("status after refused send = %q, want approved (tx rolled back)", r.status)
	}
}

// ---- criterion 9: the failure split -------------------------------------------

// A 4xx from the bridge is DEFINITE — the request never reached the click. The
// row goes failed with the error recorded and sent_external_id NULL, so the
// existing failed->approved retry path is reachable.
func TestSlackDelivery_Integration_DefiniteFailureIsReApprovable(t *testing.T) {
	ctx := context.Background()
	s := newSlackSuite(t, ctx, true)
	s.fake.err = &slackweb.SendRejectedError{Status: 403, Body: `{"error":"writes_disabled"}`}

	id := s.draft(t, ctx, sdsBody)
	s.call(t, ctx, "approve_delivery", id)
	if err := s.tryCall(ctx, "send_delivery", id); err == nil {
		t.Fatal("send_delivery with a rejecting bridge must fail")
	}

	r := s.row(t, ctx, id)
	if r.status != "failed" {
		t.Errorf("status after a definite 4xx = %q, want failed", r.status)
	}
	if r.errText == nil || !strings.Contains(*r.errText, "writes_disabled") {
		t.Errorf("error after a definite 4xx = %v, want the bridge's refusal recorded", r.errText)
	}
	if r.sentExternalID != nil {
		t.Errorf("sent_external_id after a definite 4xx = %q, want NULL", *r.sentExternalID)
	}

	// The retry path is live.
	s.call(t, ctx, "approve_delivery", id)
	again := s.row(t, ctx, id)
	if again.status != "approved" {
		t.Fatalf("failed->approved retry: status = %q, want approved", again.status)
	}
	if again.approvalSource == nil || *again.approvalSource != "switchboard" {
		t.Errorf("approval_source after re-approve = %v, want 'switchboard'", again.approvalSource)
	}
}

// A 5xx / transport error / timeout is AMBIGUOUS — the click MAY have landed.
// Named consequence 2: 'sending' is TERMINAL until the export matcher or a human
// moves it. Nothing retries it automatically, ever, because a retry is a possible
// double-post into a client channel.
func TestSlackDelivery_Integration_AmbiguousFailureStaysSending(t *testing.T) {
	ctx := context.Background()
	s := newSlackSuite(t, ctx, true)
	s.fake.err = context.DeadlineExceeded

	id := s.draft(t, ctx, sdsBody)
	s.call(t, ctx, "approve_delivery", id)
	if err := s.tryCall(ctx, "send_delivery", id); err == nil {
		t.Fatal("send_delivery with a timing-out bridge must fail")
	}

	r := s.row(t, ctx, id)
	if r.status != "sending" {
		t.Fatalf("status after an ambiguous bridge failure = %q, want it to STAY 'sending' — the click may "+
			"have landed, so the row must not become re-approvable (criterion 9)", r.status)
	}
	if r.errText == nil || *r.errText == "" {
		t.Errorf("error after an ambiguous failure = %v, want the transport error recorded", r.errText)
	}
	if r.sentExternalID != nil || r.confirmedAt != nil {
		t.Errorf("ambiguous failure left sent_external_id=%v confirmed_at=%v, want both NULL", r.sentExternalID, r.confirmedAt)
	}

	// Not re-approvable: approve_delivery only accepts drafted/failed.
	if err := s.tryCall(ctx, "approve_delivery", id); err == nil {
		t.Error("approve_delivery on a 'sending' row must be refused (a retry could double-post)")
	}
	// And nothing re-sends it: the approved-status gate blocks a second attempt.
	if err := s.tryCall(ctx, "send_delivery", id); err == nil {
		t.Error("send_delivery on a 'sending' row must be refused")
	}
	if s.fake.calls != 1 {
		t.Errorf("bridge sends = %d, want exactly 1 (never auto-retry an ambiguous Slack send)", s.fake.calls)
	}
}

// ---- criterion 12: the human resolution verbs ----------------------------------

// Salvador looked at Slack and the message IS there: mark_delivery_sent accepts
// 'sending' for slack_reply.
func TestSlackDelivery_Integration_MarkSentResolvesSendingRow(t *testing.T) {
	ctx := context.Background()
	s := newSlackSuite(t, ctx, true)
	s.fake.err = context.DeadlineExceeded

	id := s.draft(t, ctx, sdsBody)
	s.call(t, ctx, "approve_delivery", id)
	_ = s.tryCall(ctx, "send_delivery", id) // leaves the row 'sending'
	if r := s.row(t, ctx, id); r.status != "sending" {
		t.Fatalf("precondition: status = %q, want sending", r.status)
	}

	before := eventCount(t, ctx, s.pool, s.taskID, "delivery_sent")
	s.call(t, ctx, "mark_delivery_sent", id)

	r := s.row(t, ctx, id)
	if r.status != "sent" {
		t.Errorf("status after mark_delivery_sent on a sending row = %q, want sent (criterion 12)", r.status)
	}
	if r.sentAt == nil {
		t.Errorf("sent_at not stamped by mark_delivery_sent")
	}
	if r.sentExternalID != nil {
		t.Errorf("sent_external_id = %q, want NULL (the export stamps it)", *r.sentExternalID)
	}
	if r.approvalSource == nil || *r.approvalSource != "switchboard" {
		t.Errorf("approval_source = %v, want 'switchboard' preserved — this row WAS gated by switchboard; "+
			"resolving it must not relabel the gate as leaf_token", r.approvalSource)
	}
	if eventCount(t, ctx, s.pool, s.taskID, "delivery_sent") != before+1 {
		t.Errorf("mark_delivery_sent did not emit a delivery_sent task_event")
	}
}

// Criterion 19 / Q3b: the manual path skips the approval it never had —
// drafted -> sent, NO approvals row — and ONLY when the row says leaf_token. The
// new edge must not become a general bypass of approval.
func TestSlackDelivery_Integration_MarkSentAcceptsDraftedOnlyForLeafToken(t *testing.T) {
	ctx := context.Background()
	s := newSlackSuite(t, ctx, true)

	t.Run("leaf_token: drafted -> sent with no approval row", func(t *testing.T) {
		id := s.draft(t, ctx, sdsBody)
		s.setApprovalSource(t, ctx, id, "leaf_token")

		before := eventCount(t, ctx, s.pool, s.taskID, "delivery_sent")
		s.call(t, ctx, "mark_delivery_sent", id)

		r := s.row(t, ctx, id)
		if r.status != "sent" {
			t.Fatalf("status = %q, want sent (drafted -> sent directly, criterion 19)", r.status)
		}
		if r.sentAt == nil {
			t.Errorf("sent_at not stamped")
		}
		if r.approvalSource == nil || *r.approvalSource != "leaf_token" {
			t.Errorf("approval_source = %v, want 'leaf_token' preserved (named consequence 1: without it the "+
				"two kinds of row are indistinguishable)", r.approvalSource)
		}
		if n := s.approvalCount(t, ctx, id); n != 0 {
			t.Errorf("approvals rows = %d, want 0 — no switchboard approval happened; writing one puts an "+
				"approval that never occurred into the table an audit trusts (Q3b)", n)
		}
		if eventCount(t, ctx, s.pool, s.taskID, "delivery_sent") != before+1 {
			t.Errorf("the manual-path record did not emit a delivery_sent task_event (invariant 4 still wants a row)")
		}
	})

	t.Run("switchboard: drafted is still REFUSED", func(t *testing.T) {
		id := s.draft(t, ctx, sdsBody)
		s.setApprovalSource(t, ctx, id, "switchboard")
		if err := s.tryCall(ctx, "mark_delivery_sent", id); err == nil {
			t.Fatal("mark_delivery_sent accepted a drafted row marked 'switchboard'; the leaf_token edge must " +
				"not become a general bypass of approve_delivery (criterion 19)")
		}
		if r := s.row(t, ctx, id); r.status != "drafted" {
			t.Errorf("status after refusal = %q, want drafted", r.status)
		}
	})

	t.Run("NULL gate: drafted is still REFUSED", func(t *testing.T) {
		id := s.draft(t, ctx, sdsBody)
		if err := s.tryCall(ctx, "mark_delivery_sent", id); err == nil {
			t.Fatal("mark_delivery_sent accepted a drafted row with no approval_source; an unknown gate is not " +
				"a leaf-token gate")
		}
		if r := s.row(t, ctx, id); r.status != "drafted" {
			t.Errorf("status after refusal = %q, want drafted", r.status)
		}
	})

	t.Run("an unprovenanced upwork draft is refused, so leaf_token cannot reach it", func(t *testing.T) {
		// This subtest used to draft an upwork row and prove mark_delivery_sent
		// refused it in 'drafted' even with leaf_gated set — i.e. that the
		// leaf-token edge was slack-only. SWT-20 REOPENED the channel behind a
		// server-side provenance binding, and this fixture's task records no
		// source conversation (the state of every pre-SWT-20 task), so the
		// draft is still refused — now by the binding, not by a closed door.
		// The leaf-token edge stays slack-only for this fixture the same way:
		// no upwork row exists to try it on. The open-channel matrix lives in
		// delivery_upwork_binding_integration_test.go.
		if _, err := s.ex.Execute(ctx, executor.Call{Tool: "draft_delivery", Actor: sdsActor,
			Args: []byte(`{"task_id":` + itoa(s.taskID) + `,"channel":"upwork_chat","body":"thanks",` +
				`"target_ref":"upwork_crm:itest-sds:upwork"}`)}); err == nil {
			t.Fatal("draft_delivery accepted an upwork_chat draft for a task with no recorded source " +
				"conversation; the SWT-20 binding must refuse it")
		}
	})
}

// Criterion 21 / Q4: recording is exempt from the kill switch, and a record
// written during a freeze is logged distinctly — "a message went out by another
// path while the kill switch was on" must be a direct query, not a timestamp
// reconstruction.
func TestSlackDelivery_Integration_MarkSentDuringFreezeIsRecordedAndLogged(t *testing.T) {
	ctx := context.Background()
	s := newSlackSuite(t, ctx, true)

	manual := s.draft(t, ctx, sdsBody)
	s.setApprovalSource(t, ctx, manual, "leaf_token")
	automated := s.draft(t, ctx, "a second reply switchboard wants to send")
	s.call(t, ctx, "approve_delivery", automated)

	callOK(t, ctx, s.ex, sdsActor, "set_sending_frozen", `{"frozen":true}`)

	// The panic button genuinely stops switchboard's Slack sending (named
	// consequence 4 — that is new; the assisted tier's freeze never touched the
	// human's click).
	sdsDenied(t, ctx, s, "send_delivery", automated, "kill_switch")
	if s.fake.calls != 0 {
		t.Errorf("bridge sends while frozen = %d, want 0", s.fake.calls)
	}

	// ...and it stops nothing else: the leaf-token record still lands.
	s.call(t, ctx, "mark_delivery_sent", manual)
	if r := s.row(t, ctx, manual); r.status != "sent" {
		t.Fatalf("mark_delivery_sent while frozen left status %q, want sent — recording a send made through "+
			"the leaf's own token was never switchboard's to prevent (Q4)", r.status)
	}

	if n := eventCount(t, ctx, s.pool, s.taskID, "delivery_recorded_during_freeze"); n != 1 {
		t.Fatalf("delivery_recorded_during_freeze events = %d, want exactly 1 (criterion 21: the exemption "+
			"must be visible in review rather than inferred from timestamps)", n)
	}
	var payload string
	if err := s.pool.QueryRow(ctx,
		`SELECT payload::text FROM task_events
		  WHERE task_id=$1 AND event_type='delivery_recorded_during_freeze'`, s.taskID).Scan(&payload); err != nil {
		t.Fatalf("read delivery_recorded_during_freeze payload: %v", err)
	}
	for _, want := range []string{`"delivery_id"`, `"slack_reply"`, `"leaf_token"`} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload %s lacks %s; criterion 21 pins {delivery_id, channel, approval_source}", payload, want)
		}
	}

	// An unfrozen record must NOT emit the event.
	callOK(t, ctx, s.ex, sdsActor, "set_sending_frozen", `{"frozen":false}`)
	second := s.draft(t, ctx, "a third reply recorded after the thaw")
	s.setApprovalSource(t, ctx, second, "leaf_token")
	s.call(t, ctx, "mark_delivery_sent", second)
	if n := eventCount(t, ctx, s.pool, s.taskID, "delivery_recorded_during_freeze"); n != 1 {
		t.Errorf("delivery_recorded_during_freeze events after an UNFROZEN record = %d, want still 1", n)
	}
}

// Criterion 12's second verb: the message is verifiably NOT in Slack, so a human
// un-sticks the row and reopens the retry path. Without it a verified-unsent
// stuck row is unrecoverable except by raw SQL, which would be a side door
// (invariant 3).
func TestSlackDelivery_Integration_MarkDeliveryFailed(t *testing.T) {
	ctx := context.Background()
	s := newSlackSuite(t, ctx, true)
	s.fake.err = context.DeadlineExceeded

	// sending -> failed, then re-approvable.
	stuck := s.draft(t, ctx, sdsBody)
	s.call(t, ctx, "approve_delivery", stuck)
	_ = s.tryCall(ctx, "send_delivery", stuck)
	if r := s.row(t, ctx, stuck); r.status != "sending" {
		t.Fatalf("precondition: status = %q, want sending", r.status)
	}

	out := callOK(t, ctx, s.ex, sdsActor, "mark_delivery_failed", `{"delivery_id":`+itoa(stuck)+`}`)
	if !strings.Contains(string(out), `"failed"`) {
		t.Errorf("mark_delivery_failed result = %s, want {delivery_id, status:\"failed\"}", out)
	}
	r := s.row(t, ctx, stuck)
	if r.status != "failed" {
		t.Fatalf("status after mark_delivery_failed = %q, want failed", r.status)
	}
	if r.sentExternalID != nil {
		t.Errorf("sent_external_id = %v, want NULL so failed->approved is reachable", r.sentExternalID)
	}
	s.call(t, ctx, "approve_delivery", stuck)
	if got := s.row(t, ctx, stuck); got.status != "approved" {
		t.Errorf("failed->approved after mark_delivery_failed = %q, want approved", got.status)
	}

	t.Run("refused once the export confirmed the send", func(t *testing.T) {
		id := s.draft(t, ctx, sdsBody)
		s.call(t, ctx, "approve_delivery", id)
		_ = s.tryCall(ctx, "send_delivery", id)
		if _, err := s.pool.Exec(ctx,
			`UPDATE deliveries SET sent_external_id='slack:TSDSTEST:CSDSTEST:p1780000000000002' WHERE id=$1`, id); err != nil {
			t.Fatalf("stamp sent_external_id: %v", err)
		}
		if err := s.tryCall(ctx, "mark_delivery_failed", id); err == nil {
			t.Fatal("mark_delivery_failed accepted a row carrying sent_external_id; the message IS in Slack")
		}
	})

	t.Run("refused once confirmed_at is set", func(t *testing.T) {
		id := s.draft(t, ctx, sdsBody)
		s.call(t, ctx, "approve_delivery", id)
		_ = s.tryCall(ctx, "send_delivery", id)
		if _, err := s.pool.Exec(ctx, `UPDATE deliveries SET confirmed_at=now() WHERE id=$1`, id); err != nil {
			t.Fatalf("stamp confirmed_at: %v", err)
		}
		if err := s.tryCall(ctx, "mark_delivery_failed", id); err == nil {
			t.Fatal("mark_delivery_failed accepted a confirmed row")
		}
	})

	t.Run("refused on a non-sending status", func(t *testing.T) {
		id := s.draft(t, ctx, sdsBody)
		if err := s.tryCall(ctx, "mark_delivery_failed", id); err == nil {
			t.Fatal("mark_delivery_failed accepted a drafted row; it is the sending->failed verb only")
		}
		s.call(t, ctx, "approve_delivery", id)
		if err := s.tryCall(ctx, "mark_delivery_failed", id); err == nil {
			t.Fatal("mark_delivery_failed accepted an approved row")
		}
	})

	// This subtest flipped twice and is back where it started, which is worth
	// recording rather than quietly restoring.
	//
	// SWT-19 first extended mark_delivery_failed to upwork_chat, because the new
	// reconciler flags rows at status='sent' and every verb its alarm could name
	// refused them. The adversarial re-review then showed that extension was
	// worse than the gap: an upwork row reaches 'sent' via mark_delivery_sent,
	// which fires delivery_sent, which drives R8 to mark the work task delivered
	// and CLOSE its Deliver task. delivery_failed has no orchestrator rule, so
	// failing the delivery afterwards leaves a real non-delivery permanently
	// recorded as delivered. Reverted; the compensating transition remains
	// future work (SWT-20 deferred it).
	//
	// slack_reply is unaffected because it wedges at 'sending', where
	// delivery_sent never fired and R8 never ran.
	t.Run("refused on a non-slack channel", func(t *testing.T) {
		// Post-0019 (SWT-20 criterion 13): identity columns are mandatory.
		var upThread int64
		if err := s.pool.QueryRow(ctx,
			`INSERT INTO normalized_threads (thread_key, participants) VALUES ($1,'[]')
			 ON CONFLICT (thread_key) WHERE thread_key IS NOT NULL DO NOTHING
			 RETURNING id`, "upwork_crm:itest-sds:upwork").Scan(&upThread); err != nil {
			if err := s.pool.QueryRow(ctx,
				`SELECT id FROM normalized_threads WHERE thread_key=$1`,
				"upwork_crm:itest-sds:upwork").Scan(&upThread); err != nil {
				t.Fatalf("seed upwork thread: %v", err)
			}
		}
		var id int64
		if err := s.pool.QueryRow(ctx,
			`INSERT INTO deliveries (task_id, channel, target_ref, target_client_ref, thread_id, body, status, sent_at)
			 VALUES ($1,'upwork_chat','upwork_crm:itest-sds:upwork','itest-sds',$2,'stuck','sent',now()) RETURNING id`,
			s.taskID, upThread).Scan(&id); err != nil {
			t.Fatalf("seed upwork sent row: %v", err)
		}
		if err := s.tryCall(ctx, "mark_delivery_failed", id); err == nil {
			t.Fatal("mark_delivery_failed accepted an upwork_chat row. R8 has already marked the work task " +
				"delivered and closed its Deliver task off the back of delivery_sent, and delivery_failed has " +
				"no orchestrator rule — so this transition would leave a non-delivery recorded as delivered, " +
				"permanently and silently. Recovery needs a compensating transition, which remains future work")
		}
		if r := s.row(t, ctx, id); r.status != "sent" {
			t.Errorf("status after refusal = %q, want sent — the refusal must not move the row", r.status)
		}
	})
}

// The executor path is the only route (invariant 3): every one of the new verbs
// leaves an audit row.
func TestSlackDelivery_Integration_AuditTrail(t *testing.T) {
	ctx := context.Background()
	s := newSlackSuite(t, ctx, true)

	id := s.draft(t, ctx, sdsBody)
	s.call(t, ctx, "approve_delivery", id)
	s.call(t, ctx, "send_delivery", id)

	stuck := s.draft(t, ctx, "another reply")
	s.call(t, ctx, "approve_delivery", stuck)
	s.fake.err = context.DeadlineExceeded
	_ = s.tryCall(ctx, "send_delivery", stuck)
	s.call(t, ctx, "mark_delivery_failed", stuck)

	manual := s.draft(t, ctx, "recorded from the leaf")
	s.setApprovalSource(t, ctx, manual, "leaf_token")
	s.call(t, ctx, "mark_delivery_sent", manual)

	for _, tool := range []string{"draft_delivery", "approve_delivery", "send_delivery", "mark_delivery_sent", "mark_delivery_failed"} {
		var n int
		if err := s.pool.QueryRow(ctx,
			`SELECT count(*) FROM audit_events WHERE actor=$1 AND tool=$2 AND status='ok'`, sdsActor, tool).Scan(&n); err != nil {
			t.Fatalf("count audit ok for %s: %v", tool, err)
		}
		if n < 1 {
			t.Errorf("no ok audit_events row for %s (invariant 3)", tool)
		}
	}
}
