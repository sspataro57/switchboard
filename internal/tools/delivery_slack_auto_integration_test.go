//go:build integration

package tools_test

// slack-auto-tier (SWT-77, docs/tickets/slack-auto-tier_SPEC.md) — the verb
// end to end: send_slack_reply {task_id, target_ref, text} drafts, approves and
// sends a Slack reply in ONE executor call. Acceptance criteria 4, 5, 6, 7, 8,
// 9, 11, 12, 15, 16 and 19, plus D6 (a worker console may post).
//
// Build-tagged `integration` AND env-gated on DATABASE_URL. Every call goes
// through executor.Execute with the REAL policy Matrix and the REAL pg snapshot
// loader (deliveryExecutor, delivery_lifecycle_integration_test.go); the bridge
// is an injected fake tools.SlackSender — NEVER a live bridge, never a browser,
// never the Mac mini. Run it in a private database (IK: the compose Postgres is
// shared by every worktree):
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_slackauto?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run SlackAuto ./internal/tools/
//
// GREENFIELD NOTE — EXPECTED RED: send_slack_reply is not registered, so every
// call fails with `unknown tool "send_slack_reply"`. Once registered, the policy
// half needs matrix.go + pgloader.go (sendShaped/freezeGated and the channel
// pin, D4/D5) or every call is denied channel_not_live / allowed with no
// brakes; the handler half needs delivery_slack.go (D2 + the D10 guard).
//
// Cross-suite discipline: this suite owns the slug 'itest-ssr-proj', actors
// containing 'itest-ssr', and the synthetic Slack accounts
// tssrtest@slack-web.local (send_enabled) and tssroff@slack-web.local
// (send_enabled=false). Workspace TSSRNOACCT deliberately has NO account. It
// cleans its own corpus in FK order at the start AND end of every test, and
// leaves ops_flags.sending_frozen false.
//
// MUTATION MAP (Verification Step 1: "break the predicate and watch it go red"):
//	Drop the D10 guard                              -> TestSlackAuto_DuplicateGuard
//	D10 on raw target/body instead of canonical/scrubbed -> ..._DuplicateGuard/"same words, other spelling"
//	D10 on target only (ignores body)               -> ..._DuplicateGuard/"different words"
//	D10 still refusing after sent/failed            -> ..._DuplicateGuard/"resolved"
//	approval_source anything but 'switchboard'      -> sendSlackReply refuses; ..._SendsAndRecords
//	Skip the approvals INSERT                       -> ..._SendsAndRecords
//	Store the caller's spelling / unscrubbed text   -> ..._StoredRowMatchesDraftDelivery
//	Drop the loader pin / sendShaped entry          -> ..._KillSwitch, ..._RateLimit

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/connector/slackweb"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	ssrSession     = "mcp:manual:itest-ssr"            // an interactive session
	ssrWorker      = "mcp:itest-ssr-worker"            // a worker console (D6)
	ssrHuman       = "dashboard:itest-ssr@example.com" // the human two-step, for comparisons
	ssrSlug        = "itest-ssr-proj"
	ssrClient      = "itest-ssr-client"
	ssrAcct        = "tssrtest@slack-web.local"
	ssrOffAcct     = "tssroff@slack-web.local"
	ssrTarget      = "https://app.slack.com/client/TSSRTEST/CSSRTEST"
	ssrThread      = "https://app.slack.com/client/TSSRTEST/CSSRTEST/p1726000000000100"
	ssrOffTarget   = "https://app.slack.com/client/TSSROFF/CSSROFF"
	ssrNoAcctTgt   = "https://app.slack.com/client/TSSRNOACCT/CSSRNOACCT"
	ssrText        = "on it — pushing the fix tonight"
	ssrTrailer     = "Co-Authored-By: Claude <noreply@anthropic.com>"
	ssrQueuedJobID = "send-ssr-7f3a"
)

// ---- the injected fake leaf ---------------------------------------------------

type ssrSender struct {
	mu         sync.Mutex // the concurrency tests call Send from several goroutines
	delay      time.Duration
	pool       *pgxpool.Pool
	calls      int
	lastTarget string
	lastText   string
	// atDispatchStatus is the newest row for the target as it stood when the
	// "bridge" was called: invariant 4 says the delivery row is 'sending' BEFORE
	// anything leaves the process.
	atDispatchStatus string
	outcome          slackweb.SendOutcome
	err              error
}

func (f *ssrSender) Send(ctx context.Context, targetURL, text string, _ time.Duration) (slackweb.SendOutcome, error) {
	// Outside the mutex: concurrent sends must overlap in the bridge. A
	// deadline ends the wait the way a real HTTP call would.
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		f.mu.Lock()
		f.calls++
		f.mu.Unlock()
		return slackweb.SendOutcome{}, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastTarget, f.lastText = targetURL, text
	f.atDispatchStatus = ""
	_ = f.pool.QueryRow(ctx,
		`SELECT status FROM deliveries WHERE channel='slack_reply' AND target_ref=$1 ORDER BY id DESC LIMIT 1`,
		targetURL).Scan(&f.atDispatchStatus)
	return f.outcome, f.err
}

func (f *ssrSender) succeed() { f.outcome, f.err = slackweb.SendOutcome{}, nil }
func (f *ssrSender) queue() {
	f.outcome = slackweb.SendOutcome{Queued: true, JobID: ssrQueuedJobID,
		QueuedAt: time.Now().UTC().Truncate(time.Second), ExpiresIn: 10 * time.Minute}
	f.err = nil
}
func (f *ssrSender) ambiguous() {
	f.outcome, f.err = slackweb.SendOutcome{}, errors.New("bridge: context deadline exceeded after click dispatch")
}
func (f *ssrSender) reject() {
	f.outcome, f.err = slackweb.SendOutcome{}, &slackweb.SendRejectedError{Status: 403, Body: "unattended send disabled"}
}

// ---- scaffolding ---------------------------------------------------------------

type ssrSuite struct {
	pool   *pgxpool.Pool
	ex     *executor.Executor
	fake   *ssrSender
	taskID int64
}

func newSsrSuite(t *testing.T, ctx context.Context, taskStatus string) ssrSuite {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db")
	}
	// Generous by default so the send-heavy tests are never throttled by each
	// other; TestSlackAuto_RateLimit sets its own.
	t.Setenv("OPS_SEND_HOURLY_LIMIT", "1000")
	pool := newToolsPool(t, ctx)
	t.Cleanup(pool.Close)
	cleanupSsr(t, ctx, pool)
	t.Cleanup(func() { cleanupSsr(t, context.Background(), pool) })

	for _, a := range []struct {
		email, ws string
		enabled   bool
	}{{ssrAcct, "TSSRTEST", true}, {ssrOffAcct, "TSSROFF", false}} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO source_accounts (provider, account_email, domain_default, scopes, send_enabled, calendar_in_availability)
			 VALUES ('slack_web', $1, 'https://app.slack.com/client/'||$2, ARRAY[]::text[], $3, false)`,
			a.email, a.ws, a.enabled); err != nil {
			t.Fatalf("seed slack_web source_account %s: %v", a.email, err)
		}
	}
	projID := seedProject(t, ctx, pool, ssrSlug, ssrClient)
	var taskID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'itest-ssr work','human',$2) RETURNING id`, projID, taskStatus).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	fake := &ssrSender{pool: pool}
	tools.SetSlackSender(fake)
	t.Cleanup(func() { tools.SetSlackSender(nil) })
	return ssrSuite{pool: pool, ex: deliveryExecutor(pool), fake: fake, taskID: taskID}
}

func cleanupSsr(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const ourTasks = `(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + ssrSlug + `'))`
	for _, st := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO ops_flags (name, value) VALUES ('sending_frozen', '{"frozen": false}')
		  ON CONFLICT (name) DO UPDATE SET value='{"frozen": false}'`, nil},
		{`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE actor LIKE '%itest-ssr%')`, nil},
		{`DELETE FROM audit_events WHERE actor LIKE '%itest-ssr%'`, nil},
		{`DELETE FROM approvals WHERE subject_type='delivery' AND subject_id IN
			(SELECT id FROM deliveries WHERE task_id IN ` + ourTasks + `)`, nil},
		{`DELETE FROM approvals WHERE decided_by LIKE '%itest-ssr%'`, nil},
		{`DELETE FROM task_events WHERE task_id IN ` + ourTasks, nil},
		{`DELETE FROM deliveries WHERE task_id IN ` + ourTasks, nil},
		{`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, []any{ssrSlug}},
		{`DELETE FROM decisions WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, []any{ssrSlug}},
		{`DELETE FROM projects WHERE slug=$1`, []any{ssrSlug}},
		{`DELETE FROM source_accounts WHERE provider='slack_web' AND account_email IN ($1,$2)`, []any{ssrAcct, ssrOffAcct}},
	} {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

func (s ssrSuite) args(target, text string) string {
	return `{"task_id":` + itoa(s.taskID) + `,"target_ref":` + ssrJSON(target) + `,"text":` + ssrJSON(text) + `}`
}

func (s ssrSuite) call(ctx context.Context, actor, target, text string) (json.RawMessage, error) {
	res, err := s.ex.Execute(ctx, executor.Call{Tool: "send_slack_reply", Actor: actor, Args: []byte(s.args(target, text))})
	return res.Output, err
}

func (s ssrSuite) mustCall(t *testing.T, ctx context.Context, actor, target, text string) map[string]any {
	t.Helper()
	out, err := s.call(ctx, actor, target, text)
	if err != nil {
		t.Fatalf("send_slack_reply(%s, %q): %v", target, text, err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("send_slack_reply result %s is not a JSON object: %v", out, err)
	}
	return m
}

func (s ssrSuite) count(t *testing.T, ctx context.Context, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

func (s ssrSuite) deliveryCount(t *testing.T, ctx context.Context) int {
	return s.count(t, ctx, `SELECT count(*) FROM deliveries WHERE task_id=$1`, s.taskID)
}

func resultID(t *testing.T, m map[string]any) int64 {
	t.Helper()
	f, ok := m["delivery_id"].(float64)
	if !ok || f == 0 {
		t.Fatalf("result %v carries no delivery_id", m)
	}
	return int64(f)
}

func ssrKeys(m map[string]any) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

// assertSsrDenied: refused by the POLICY matrix with the given rule, a deny
// policy_decisions row naming the tool, and an audit row closed 'denied'
// (criterion 19's deny half).
func (s ssrSuite) assertDenied(t *testing.T, ctx context.Context, err error, actor, wantRule string) {
	t.Helper()
	if err == nil {
		t.Fatalf("send_slack_reply was ALLOWED; want a policy denial (%s)", wantRule)
	}
	if !strings.Contains(err.Error(), "denied by policy") || !strings.Contains(err.Error(), "("+wantRule+")") {
		t.Fatalf("send_slack_reply error = %q, want a policy denial with rule %s", err, wantRule)
	}
	if n := s.count(t, ctx,
		`SELECT count(*) FROM policy_decisions p JOIN audit_events a ON a.id=p.audit_event_id
		  WHERE a.actor=$1 AND a.tool='send_slack_reply' AND a.status='denied'
		    AND p.tool='send_slack_reply' AND p.decision='deny' AND p.rule=$2`, actor, wantRule); n != 1 {
		t.Errorf("deny policy_decisions/audit rows for send_slack_reply rule=%s = %d, want 1 (criterion 19)", wantRule, n)
	}
}

// ---- criteria 5, 7, 19: one call, one row, one approval, sent -----------------

func TestSlackAuto_SendsAndRecords(t *testing.T) {
	ctx := context.Background()
	s := newSsrSuite(t, ctx, "in_progress")
	s.fake.succeed()

	res := s.mustCall(t, ctx, ssrSession, ssrTarget, ssrText)
	id := resultID(t, res)

	// Criterion 7: the result is send_delivery's slack branch, byte for byte in shape.
	if got := ssrKeys(res); got != "delivery_id,status" || res["status"] != "sent" {
		t.Errorf("result = %v, want exactly {delivery_id, status:\"sent\"} (criterion 7)", res)
	}

	// Criterion 5: exactly one row, with the verb's columns.
	if n := s.deliveryCount(t, ctx); n != 1 {
		t.Fatalf("deliveries for the task = %d, want exactly 1 (criterion 5)", n)
	}
	var channel, target, body, status, createdBy string
	var approvalSource *string
	var sentAt, attemptedAt, settledAt *time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT channel, target_ref, body, status, created_by, approval_source, sent_at, send_attempted_at, send_settled_at
		   FROM deliveries WHERE id=$1`, id).
		Scan(&channel, &target, &body, &status, &createdBy, &approvalSource, &sentAt, &attemptedAt, &settledAt); err != nil {
		t.Fatalf("read delivery %d: %v", id, err)
	}
	if channel != "slack_reply" {
		t.Errorf("channel = %q, want slack_reply", channel)
	}
	if target != ssrTarget || body != ssrText {
		t.Errorf("stored target/body = %q / %q, want %q / %q", target, body, ssrTarget, ssrText)
	}
	if status != "sent" || sentAt == nil || attemptedAt == nil || settledAt == nil {
		t.Errorf("row = status %q sent_at %v attempted %v settled %v, want sent with all three stamps (criterion 7)",
			status, sentAt, attemptedAt, settledAt)
	}
	if createdBy != ssrSession {
		t.Errorf("created_by = %q, want the calling identity %q (executor.ActorFrom)", createdBy, ssrSession)
	}
	if approvalSource == nil || *approvalSource != "switchboard" {
		t.Errorf("approval_source = %v, want 'switchboard' (D3: policy gated it; sendSlackReply requires it)", approvalSource)
	}

	// Criterion 5: exactly one approvals row naming the actor.
	var apprN int
	var apprStatus, decidedBy string
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(max(status),''), COALESCE(max(decided_by),'') FROM approvals
		  WHERE subject_type='delivery' AND subject_id=$1`, id).Scan(&apprN, &apprStatus, &decidedBy); err != nil {
		t.Fatalf("read approvals: %v", err)
	}
	if apprN != 1 || apprStatus != "approved" || decidedBy != ssrSession {
		t.Errorf("approvals = %d row(s), status %q, decided_by %q; want exactly 1 approved by %q (criterion 5: "+
			"WHICH session auto-approved is on record)", apprN, apprStatus, decidedBy, ssrSession)
	}

	// Criterion 7: delivery_sent, once.
	if n := s.count(t, ctx,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_sent'
		   AND payload->>'delivery_id'=$2 AND payload->>'channel'='slack_reply'`, s.taskID, itoa(id)); n != 1 {
		t.Errorf("delivery_sent events = %d, want 1 (criterion 7)", n)
	}

	// The bridge saw the row already 'sending' (invariant 4), once, with the stored words.
	if s.fake.calls != 1 {
		t.Errorf("bridge called %d times, want 1", s.fake.calls)
	}
	if s.fake.atDispatchStatus != "sending" {
		t.Errorf("row status at dispatch = %q, want sending: nothing leaves before the row says so (invariant 4)",
			s.fake.atDispatchStatus)
	}
	if s.fake.lastTarget != ssrTarget || s.fake.lastText != ssrText {
		t.Errorf("bridge got %q / %q, want %q / %q", s.fake.lastTarget, s.fake.lastText, ssrTarget, ssrText)
	}

	// Criterion 19: one audit row for the verb, completed ok, with the actor;
	// and its policy_decisions row: allow / matrix-send / send_slack_reply.
	var auditID int64
	var auditStatus string
	var completed *time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT id, status, completed_at FROM audit_events WHERE actor=$1 AND tool='send_slack_reply'
		  ORDER BY id DESC LIMIT 1`, ssrSession).Scan(&auditID, &auditStatus, &completed); err != nil {
		t.Fatalf("no audit_events row for send_slack_reply by %s: %v (criterion 19)", ssrSession, err)
	}
	if auditStatus != "ok" || completed == nil {
		t.Errorf("audit row = status %q completed %v, want ok + completed (the start/complete pair)", auditStatus, completed)
	}
	var pTool, pDecision, pRule string
	if err := s.pool.QueryRow(ctx,
		`SELECT tool, decision, rule FROM policy_decisions WHERE audit_event_id=$1`, auditID).
		Scan(&pTool, &pDecision, &pRule); err != nil {
		t.Fatalf("no policy_decisions row for audit %d: %v (criterion 19)", auditID, err)
	}
	if pTool != "send_slack_reply" || pDecision != "allow" || pRule != "matrix-send" {
		t.Errorf("policy_decisions = %s/%s/%s, want send_slack_reply/allow/matrix-send. Anything else means the "+
			"call never reached the slack_reply branch (D4/D5)", pTool, pDecision, pRule)
	}
}

// D6: the full profile is also the worker consoles' surface, and the verb is
// not actor-gated — a worker console can post, and the approvals row names it.
func TestSlackAuto_WorkerConsoleCanPost(t *testing.T) {
	ctx := context.Background()
	s := newSsrSuite(t, ctx, "in_progress")
	s.fake.succeed()

	res := s.mustCall(t, ctx, ssrWorker, ssrThread, "PR is up: tests green")
	id := resultID(t, res)
	var decidedBy string
	if err := s.pool.QueryRow(ctx,
		`SELECT decided_by FROM approvals WHERE subject_type='delivery' AND subject_id=$1`, id).Scan(&decidedBy); err != nil {
		t.Fatalf("read approvals: %v", err)
	}
	if decidedBy != ssrWorker {
		t.Errorf("approvals.decided_by = %q, want the worker console %q", decidedBy, ssrWorker)
	}
}

// ---- criterion 4: the stored row is byte-identical to draft_delivery's --------

func TestSlackAuto_StoredRowMatchesDraftDelivery(t *testing.T) {
	ctx := context.Background()
	s := newSsrSuite(t, ctx, "in_progress")
	s.fake.succeed()

	// A spelling ParseTargetURL accepts but the matcher never will (trailing
	// slash), and text carrying an attribution trailer the scrub removes.
	rawTarget := ssrTarget + "/"
	rawText := ssrText + "\n" + ssrTrailer

	id := resultID(t, s.mustCall(t, ctx, ssrSession, rawTarget, rawText))
	out := callOK(t, ctx, s.ex, ssrHuman, "draft_delivery",
		`{"task_id":`+itoa(s.taskID)+`,"channel":"slack_reply","target_ref":`+ssrJSON(rawTarget)+`,"body":`+ssrJSON(rawText)+`}`)
	var d struct {
		DeliveryID int64 `json:"delivery_id"`
	}
	mustUnmarshal(t, out, &d)

	var aTarget, aBody, bTarget, bBody string
	if err := s.pool.QueryRow(ctx, `SELECT target_ref, body FROM deliveries WHERE id=$1`, id).Scan(&aTarget, &aBody); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT target_ref, body FROM deliveries WHERE id=$1`, d.DeliveryID).Scan(&bTarget, &bBody); err != nil {
		t.Fatal(err)
	}
	if aTarget != bTarget || aBody != bBody {
		t.Errorf("send_slack_reply stored (%q, %q); draft_delivery stores (%q, %q) for the same input. Criterion 4: "+
			"byte-identical — the export's exact-target_ref + body-prefix matcher confirms our own message only if "+
			"the verb stores what draft_delivery stores (invariant 5)", aTarget, aBody, bTarget, bBody)
	}
	if aTarget != ssrTarget {
		t.Errorf("stored target_ref = %q, want the canonical %q (SWT-13 landmine)", aTarget, ssrTarget)
	}
	if aBody != ssrText {
		t.Errorf("stored body = %q, want the scrubbed %q (invariant 6)", aBody, ssrText)
	}
	if s.fake.lastTarget != ssrTarget || s.fake.lastText != ssrText {
		t.Errorf("bridge got %q / %q, want the canonical target and scrubbed text", s.fake.lastTarget, s.fake.lastText)
	}
}

// ---- criterion 8: 202 / queued ------------------------------------------------

func TestSlackAuto_Queued202(t *testing.T) {
	ctx := context.Background()
	s := newSsrSuite(t, ctx, "in_progress")
	s.fake.queue()

	res := s.mustCall(t, ctx, ssrSession, ssrTarget, ssrText)
	id := resultID(t, res)
	if got := ssrKeys(res); got != "delivery_id,job_id,queued,queued_at,status" {
		t.Errorf("queued result keys = %s, want delivery_id,job_id,queued,queued_at,status (criterion 8): %v", got, res)
	}
	if res["status"] != "sending" || res["queued"] != true || res["job_id"] != ssrQueuedJobID {
		t.Errorf("queued result = %v, want status sending, queued true, job_id %q", res, ssrQueuedJobID)
	}

	var status string
	var queuedAt, settled *time.Time
	var jobID, errMsg *string
	if err := s.pool.QueryRow(ctx,
		`SELECT status, send_queued_at, send_queue_job_id, error, send_settled_at FROM deliveries WHERE id=$1`, id).
		Scan(&status, &queuedAt, &jobID, &errMsg, &settled); err != nil {
		t.Fatal(err)
	}
	if status != "sending" || queuedAt == nil || jobID == nil || *jobID != ssrQueuedJobID || errMsg != nil || settled != nil {
		t.Errorf("queued row = status %q queued_at %v job %v error %v settled %v; want sending, queued_at set, "+
			"job %q, error NULL, settled NULL (criterion 8 / SWT-76 D4)", status, queuedAt, jobID, errMsg, settled, ssrQueuedJobID)
	}
	if n := s.count(t, ctx,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='log' AND payload->>'kind'='delivery_queued'`,
		s.taskID); n != 1 {
		t.Errorf("delivery_queued log events = %d, want 1 (criterion 8)", n)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_sent'`, s.taskID); n != 0 {
		t.Errorf("delivery_sent fired at enqueue (%d); nothing has left yet and delivery_sent drives R8", n)
	}
}

// ---- criterion 9: the SWT-76 failure model, reached through the new verb -------

func TestSlackAuto_RejectedLandsFailed(t *testing.T) {
	ctx := context.Background()
	s := newSsrSuite(t, ctx, "in_progress")
	s.fake.reject()

	if _, err := s.call(ctx, ssrSession, ssrTarget, ssrText); err == nil {
		t.Fatal("a SendRejectedError from the bridge returned success")
	}
	var status string
	var settled *time.Time
	var errMsg *string
	if err := s.pool.QueryRow(ctx,
		`SELECT status, send_settled_at, error FROM deliveries WHERE task_id=$1 AND channel='slack_reply'`, s.taskID).
		Scan(&status, &settled, &errMsg); err != nil {
		t.Fatalf("read the verb's row: %v (a rejected send must still leave its delivery row — invariant 4)", err)
	}
	if status != "failed" || settled == nil || errMsg == nil {
		t.Errorf("rejected row = status %q settled %v error %v, want failed + settled + error (criterion 9)", status, settled, errMsg)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_sent'`, s.taskID); n != 0 {
		t.Errorf("delivery_sent fired for a rejected send (%d)", n)
	}
}

func TestSlackAuto_AmbiguousStaysSending(t *testing.T) {
	ctx := context.Background()
	s := newSsrSuite(t, ctx, "in_progress")
	s.fake.ambiguous()

	if _, err := s.call(ctx, ssrSession, ssrTarget, ssrText); err == nil {
		t.Fatal("an ambiguous bridge error returned success")
	}
	var status string
	var settled *time.Time
	var errMsg *string
	if err := s.pool.QueryRow(ctx,
		`SELECT status, send_settled_at, error FROM deliveries WHERE task_id=$1 AND channel='slack_reply'`, s.taskID).
		Scan(&status, &settled, &errMsg); err != nil {
		t.Fatal(err)
	}
	if status != "sending" || settled == nil || errMsg == nil || !strings.Contains(*errMsg, "deadline exceeded") {
		t.Errorf("ambiguous row = status %q settled %v error %v, want sending + settled + the diagnostic (criterion 9: "+
			"the click MAY have landed, so nothing retries it)", status, settled, errMsg)
	}
}

// ---- criterion 6: closed task refused before any row --------------------------

func TestSlackAuto_ClosedTaskRefusedBeforeAnyRow(t *testing.T) {
	ctx := context.Background()
	s := newSsrSuite(t, ctx, "closed")
	s.fake.succeed()

	_, err := s.call(ctx, ssrSession, ssrTarget, ssrText)
	if err == nil {
		t.Fatal("send_slack_reply on a CLOSED task succeeded")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("refusal = %q, want it to say the task is closed", err)
	}
	if n := s.deliveryCount(t, ctx); n != 0 {
		t.Errorf("deliveries after the refusal = %d, want 0 (criterion 6: refused before any insert)", n)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM approvals WHERE decided_by=$1`, ssrSession); n != 0 {
		t.Errorf("approvals after the refusal = %d, want 0 (criterion 6)", n)
	}
	if s.fake.calls != 0 {
		t.Errorf("the bridge was called %d times for a closed task", s.fake.calls)
	}
}

// ---- criterion 11: kill switch, nothing written -------------------------------

func TestSlackAuto_KillSwitchWritesNothing(t *testing.T) {
	ctx := context.Background()
	s := newSsrSuite(t, ctx, "in_progress")
	s.fake.succeed()
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO ops_flags (name, value) VALUES ('sending_frozen', '{"frozen": true}')
		 ON CONFLICT (name) DO UPDATE SET value='{"frozen": true}'`); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	_, err := s.call(ctx, ssrSession, ssrTarget, ssrText)
	if _, uerr := s.pool.Exec(ctx, `UPDATE ops_flags SET value='{"frozen": false}' WHERE name='sending_frozen'`); uerr != nil {
		t.Fatalf("unfreeze: %v", uerr)
	}
	s.assertDenied(t, ctx, err, ssrSession, "kill_switch")
	if n := s.deliveryCount(t, ctx); n != 0 {
		t.Errorf("deliveries after a kill_switch deny = %d, want 0 (criterion 11: the deny happens before the handler)", n)
	}
	if s.fake.calls != 0 {
		t.Errorf("the bridge was called %d times with sending frozen", s.fake.calls)
	}
}

// ---- criterion 12: hourly limit, and the verb's own row counts ----------------

func TestSlackAuto_RateLimitCountsTheVerbsOwnRow(t *testing.T) {
	ctx := context.Background()
	s := newSsrSuite(t, ctx, "in_progress")
	s.fake.succeed()

	// Whatever else is in this database this hour, leave exactly ONE send of headroom.
	existing := s.count(t, ctx,
		`SELECT count(*) FROM deliveries WHERE channel='slack_reply' AND status IN ('sent','sending')
		   AND COALESCE(sent_at, send_attempted_at) >= now() - interval '1 hour'`)
	t.Setenv("OPS_SEND_HOURLY_LIMIT", strconv.Itoa(existing+1))

	s.mustCall(t, ctx, ssrSession, ssrTarget, "first message")
	_, err := s.call(ctx, ssrSession, ssrTarget, "second message")
	s.assertDenied(t, ctx, err, ssrSession, "rate_limit")
	if n := s.deliveryCount(t, ctx); n != 1 {
		t.Errorf("deliveries after the rate_limit deny = %d, want 1 (only the first call's row)", n)
	}
	if s.fake.calls != 1 {
		t.Errorf("bridge calls = %d, want 1", s.fake.calls)
	}
}

// ---- criterion 15: the per-workspace gate, reached through the new verb ---------

func TestSlackAuto_WorkspaceGatesRefuseByName(t *testing.T) {
	ctx := context.Background()
	s := newSsrSuite(t, ctx, "in_progress")
	s.fake.succeed()

	for _, tc := range []struct {
		name, target, ws, want string
	}{
		{"send_enabled=false", ssrOffTarget, "TSSROFF", "send-enabled"},
		{"no ingested account", ssrNoAcctTgt, "TSSRNOACCT", "no ingested account"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.call(ctx, ssrSession, tc.target, ssrText)
			if err == nil {
				t.Fatalf("send_slack_reply to workspace %s succeeded", tc.ws)
			}
			if !strings.Contains(err.Error(), tc.ws) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal = %q, want it to name workspace %s and say %q (criterion 15)", err, tc.ws, tc.want)
			}
			// D2a: the gate runs BEFORE the draft, so a refused workspace leaves
			// no row at all — not even an approved one a human could Send.
			if n := s.count(t, ctx,
				`SELECT count(*) FROM deliveries WHERE task_id=$1 AND target_ref=$2`,
				s.taskID, tc.target); n != 0 {
				t.Errorf("%d row(s) for %s exist after the refusal, want none (D2a)", n, tc.ws)
			}
		})
	}
	if s.fake.calls != 0 {
		t.Errorf("the bridge was called %d times for gated workspaces", s.fake.calls)
	}
}

// An unwired sender (an MCP binary without SLACK_WEB_BRIDGE_URL) is refused by
// name BEFORE the draft: no deliveries row, no approvals row. go-reviewer
// finding, SWT-77 — without the up-front check every such call left an
// approved row behind.
func TestSlackAuto_NoSenderRefusedBeforeAnyRow(t *testing.T) {
	ctx := context.Background()
	s := newSsrSuite(t, ctx, "in_progress")
	tools.SetSlackSender(nil)

	_, err := s.call(ctx, ssrSession, ssrTarget, ssrText)
	if err == nil || !strings.Contains(err.Error(), "no Slack send adapter wired") {
		t.Fatalf("send_slack_reply with no sender = %v, want the no-adapter refusal", err)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM deliveries WHERE task_id=$1`, s.taskID); n != 0 {
		t.Errorf("%d deliveries row(s) after the no-sender refusal, want none", n)
	}
	if n := s.count(t, ctx,
		`SELECT count(*) FROM approvals a JOIN deliveries d ON d.id = a.subject_id
		 WHERE a.subject_type='delivery' AND d.task_id=$1`, s.taskID); n != 0 {
		t.Errorf("%d approvals row(s) after the no-sender refusal, want none", n)
	}
}

// ---- criterion 16 / D10: the duplicate guard ----------------------------------

func TestSlackAuto_DuplicateGuard(t *testing.T) {
	ctx := context.Background()
	s := newSsrSuite(t, ctx, "in_progress")

	// A: the first attempt comes back ambiguous — the click MAY have landed —
	// and the row stays 'sending'. This is the retry case D10 exists for.
	s.fake.ambiguous()
	if _, err := s.call(ctx, ssrSession, ssrTarget, ssrText); err == nil {
		t.Fatal("setup: the ambiguous first attempt returned success")
	}
	var aID int64
	if err := s.pool.QueryRow(ctx,
		`SELECT id FROM deliveries WHERE task_id=$1 AND status='sending'`, s.taskID).Scan(&aID); err != nil {
		t.Fatalf("setup: no 'sending' row after the ambiguous attempt: %v", err)
	}
	s.fake.succeed()
	callsBefore := s.fake.calls

	assertDup := func(t *testing.T, target, text string) {
		t.Helper()
		before := s.deliveryCount(t, ctx)
		_, err := s.call(ctx, ssrSession, target, text)
		if err == nil {
			t.Fatalf("a second send of the same words to the same conversation succeeded while delivery %d is "+
				"unresolved — a double post into a client conversation (D10)", aID)
		}
		msg := err.Error()
		if strings.Contains(msg, "denied by policy") {
			t.Fatalf("the duplicate was refused by POLICY (%v); D10 is the handler's guard", err)
		}
		if !strings.Contains(msg, itoa(aID)) {
			t.Errorf("refusal %q does not name the earlier delivery %d (criterion 16)", msg, aID)
		}
		if !strings.Contains(msg, "mark_delivery_sent") || !strings.Contains(msg, "mark_delivery_failed") {
			t.Errorf("refusal %q does not point at mark_delivery_sent / mark_delivery_failed (criterion 16)", msg)
		}
		if n := s.deliveryCount(t, ctx); n != before {
			t.Errorf("deliveries %d -> %d: the guard must refuse BEFORE inserting", before, n)
		}
		if s.fake.calls != callsBefore {
			t.Errorf("the bridge was called for a refused duplicate")
		}
	}

	t.Run("same words, same conversation -> refused", func(t *testing.T) {
		assertDup(t, ssrTarget, ssrText)
	})
	t.Run("same words, other spelling (trailing slash, attribution trailer) -> refused", func(t *testing.T) {
		// SAME canonical target_ref and SAME scrubbed body: a guard comparing the
		// caller's raw strings would let this through.
		assertDup(t, ssrTarget+"/", ssrText+"\n"+ssrTrailer)
	})
	t.Run("different words, same conversation -> allowed", func(t *testing.T) {
		// D10 refuses IDENTICAL words only; a guard on the target alone would
		// freeze the conversation until a human resolved the first row.
		s.mustCall(t, ctx, ssrSession, ssrTarget, "and a follow-up line")
		callsBefore = s.fake.calls
	})
	t.Run("same words, other conversation -> allowed", func(t *testing.T) {
		s.mustCall(t, ctx, ssrSession, ssrThread, ssrText)
		callsBefore = s.fake.calls
	})

	t.Run("resolved as sent -> the same words are legal again", func(t *testing.T) {
		callOK(t, ctx, s.ex, ssrHuman, "mark_delivery_sent", `{"delivery_id":`+itoa(aID)+`}`)
		if st := deliveryStatus(t, ctx, s.pool, aID); st != "sent" {
			t.Fatalf("setup: delivery %d is %q after mark_delivery_sent", aID, st)
		}
		res := s.mustCall(t, ctx, ssrSession, ssrTarget, ssrText)
		if res["status"] != "sent" {
			t.Errorf("same words after the first row was sent = %v, want sent (saying \"ok\" twice is legal)", res)
		}
	})

	t.Run("queued (unsettled) counts as unresolved; resolved as failed -> legal again", func(t *testing.T) {
		const text = "second thought: shipping tomorrow instead"
		s.fake.queue()
		q := s.mustCall(t, ctx, ssrSession, ssrTarget, text)
		qID := resultID(t, q)
		s.fake.succeed()
		callsBefore = s.fake.calls
		before := s.deliveryCount(t, ctx)
		_, err := s.call(ctx, ssrSession, ssrTarget, text)
		if err == nil || !strings.Contains(err.Error(), itoa(qID)) {
			t.Fatalf("same words while delivery %d is QUEUED = %v, want a refusal naming it (queued is unresolved)", qID, err)
		}
		if n := s.deliveryCount(t, ctx); n != before {
			t.Errorf("a refused duplicate inserted a row")
		}
		// Resolve it as failed. The queued attempt is unsettled and inside the
		// lease, so mark_delivery_failed would (rightly) refuse; settle and age it
		// the way the lease expiring would.
		if _, err := s.pool.Exec(ctx,
			`UPDATE deliveries SET send_settled_at=now(), send_attempted_at=now()-interval '1 hour' WHERE id=$1`, qID); err != nil {
			t.Fatal(err)
		}
		callOK(t, ctx, s.ex, ssrHuman, "mark_delivery_failed", `{"delivery_id":`+itoa(qID)+`}`)
		if st := deliveryStatus(t, ctx, s.pool, qID); st != "failed" {
			t.Fatalf("setup: delivery %d is %q after mark_delivery_failed", qID, st)
		}
		res := s.mustCall(t, ctx, ssrSession, ssrTarget, text)
		if res["status"] != "sent" {
			t.Errorf("same words after the earlier row FAILED = %v, want sent (criterion 16)", res)
		}
	})
}

// ---- codex review: the kill switch at SEND time, and approved rows as duplicates ----

// staticExecutor is the executor with the policy matrix replaced by a plain
// allow-list: it models "the policy snapshot was taken BEFORE the freeze" —
// the window between the loader's read and the handler.
func (s ssrSuite) staticExecutor() *executor.Executor {
	reg := executor.NewRegistry()
	tools.Register(reg, s.pool)
	return executor.New(reg, policy.NewStatic(reg.Names()...), audit.NewPGStore(s.pool))
}

func (s ssrSuite) freeze(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO ops_flags (name, value) VALUES ('sending_frozen', '{"frozen": true}')
		 ON CONFLICT (name) DO UPDATE SET value='{"frozen": true}'`); err != nil {
		t.Fatalf("freeze: %v", err)
	}
}

// A freeze pressed after policy's snapshot still stops send_slack_reply, before
// any row is written.
func TestSlackAuto_FreezeAfterPolicySnapshotWritesNothing(t *testing.T) {
	ctx := context.Background()
	s := newSsrSuite(t, ctx, "in_progress")
	s.fake.succeed()
	s.freeze(t, ctx)

	_, err := s.staticExecutor().Execute(ctx, executor.Call{Tool: "send_slack_reply", Actor: ssrSession,
		Args: []byte(s.args(ssrTarget, ssrText))})
	if err == nil || !strings.Contains(err.Error(), "kill switch") {
		t.Fatalf("send_slack_reply during a freeze (policy bypassed) = %v, want the handler's kill-switch refusal", err)
	}
	if n := s.deliveryCount(t, ctx); n != 0 {
		t.Errorf("%d deliveries row(s) after the freeze refusal, want none", n)
	}
	if s.fake.calls != 0 {
		t.Errorf("the bridge was called %d time(s) during a freeze", s.fake.calls)
	}
}

// The shared send half re-checks the switch in the transaction that commits
// 'sending', so the human two-step (send_delivery) is stopped too, and the row
// stays approved for after the freeze.
func TestSlackAuto_SendHalfRechecksFreeze(t *testing.T) {
	ctx := context.Background()
	s := newSsrSuite(t, ctx, "in_progress")
	s.fake.succeed()
	var id int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, approval_source, created_by)
		 VALUES ($1, 'slack_reply', $2, 'freeze race', 'approved', 'switchboard', $3) RETURNING id`,
		s.taskID, ssrTarget, ssrHuman).Scan(&id); err != nil {
		t.Fatalf("seed approved row: %v", err)
	}
	s.freeze(t, ctx)

	_, err := s.staticExecutor().Execute(ctx, executor.Call{Tool: "send_delivery", Actor: ssrHuman,
		Args: []byte(`{"delivery_id":` + itoa(id) + `}`)})
	if err == nil || !strings.Contains(err.Error(), "kill switch") {
		t.Fatalf("send_delivery during a freeze (policy bypassed) = %v, want the send half's kill-switch refusal", err)
	}
	var status string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM deliveries WHERE id=$1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "approved" {
		t.Errorf("row is %s after the freeze refusal, want approved (nothing moved)", status)
	}
	if s.fake.calls != 0 {
		t.Errorf("the bridge was called %d time(s) during a freeze", s.fake.calls)
	}
}

// An APPROVED row with the same words to the same conversation is one Send
// away — an orphan of a crash between approve and dispatch, or the human
// two-step in progress — so a second send_slack_reply is refused, naming it.
func TestSlackAuto_DuplicateGuardCountsApprovedRows(t *testing.T) {
	ctx := context.Background()
	s := newSsrSuite(t, ctx, "in_progress")
	s.fake.succeed()
	var id int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, approval_source, created_by)
		 VALUES ($1, 'slack_reply', $2, $3, 'approved', 'switchboard', $4) RETURNING id`,
		s.taskID, ssrTarget, ssrText, ssrSession).Scan(&id); err != nil {
		t.Fatalf("seed orphan approved row: %v", err)
	}
	before := s.deliveryCount(t, ctx)

	_, err := s.call(ctx, ssrSession, ssrTarget+"/", ssrText+"\n"+ssrTrailer)
	if err == nil {
		t.Fatal("send_slack_reply beside an approved row with the same words SUCCEEDED; want a duplicate refusal")
	}
	if !strings.Contains(err.Error(), itoa(id)) || !strings.Contains(err.Error(), "approved but not sent") {
		t.Errorf("refusal = %q, want it to name delivery %d as approved but not sent", err, id)
	}
	if n := s.deliveryCount(t, ctx); n != before {
		t.Errorf("deliveries went %d -> %d; the refusal must write nothing", before, n)
	}
	if s.fake.calls != 0 {
		t.Errorf("the bridge was called %d time(s)", s.fake.calls)
	}
}

// ---- codex re-review: admission is atomic under concurrency ------------------

// fanOut runs n concurrent calls and returns how many succeeded plus the
// refusals.
func (s ssrSuite) fanOut(ctx context.Context, n int, text func(i int) string) (ok int, errs []error) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.call(ctx, ssrSession, ssrTarget, text(i))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else {
				errs = append(errs, err)
			}
		}(i)
	}
	wg.Wait()
	return ok, errs
}

// N identical concurrent calls post ONCE: the admission lock makes the
// duplicate guard see the first call's row before the next call checks.
func TestSlackAuto_ConcurrentIdenticalCallsPostOnce(t *testing.T) {
	ctx := context.Background()
	s := newSsrSuite(t, ctx, "in_progress")
	s.fake.ambiguous() // the first row stays 'sending': every later identical call must see it

	// The one admitted call reports the ambiguous outcome (an error, row left
	// 'sending'); every other must be the duplicate guard's refusal. Three
	// times the pool's size: without the admission lock two calls reliably pass
	// the guard, and a lock whose waiters held pooled connections would
	// starve the holder into a deadlock (the go test -timeout catches that).
	n := 3 * int(s.pool.Config().MaxConns)
	if n < 16 {
		n = 16
	}
	fctx, cancel := context.WithTimeout(ctx, 60*time.Second) // a starved pool fails here, not at go test -timeout
	defer cancel()
	ok, errs := s.fanOut(fctx, n, func(int) string { return ssrText })
	ambiguous, dup := 0, 0
	for _, err := range errs {
		switch {
		case strings.Contains(err.Error(), "outcome unknown"):
			ambiguous++
		case strings.Contains(err.Error(), "already carries these words"):
			dup++
		default:
			t.Errorf("unexpected refusal %q", err)
		}
	}
	if ok != 0 || ambiguous != 1 || dup != n-1 {
		t.Fatalf("%d identical concurrent calls: %d ok, %d ambiguous, %d duplicate refusals; want 0/1/%d (%v)",
			n, ok, ambiguous, dup, n-1, errs)
	}
	if s.fake.calls != 1 {
		t.Errorf("the bridge was called %d times for %d identical calls, want 1", s.fake.calls, n)
	}
	if n := s.deliveryCount(t, ctx); n != 1 {
		t.Errorf("%d deliveries rows after the fan-out, want 1 (the refusals write nothing)", n)
	}
}

// N concurrent calls with ONE slot left admit exactly one.
func TestSlackAuto_ConcurrentCallsRespectHourlyLimit(t *testing.T) {
	ctx := context.Background()
	s := newSsrSuite(t, ctx, "in_progress")
	t.Setenv("OPS_SEND_HOURLY_LIMIT", "3")
	s.fake.succeed()
	for i := 0; i < 2; i++ {
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO deliveries (task_id, channel, target_ref, body, status, approval_source, created_by, sent_at, send_attempted_at)
			 VALUES ($1, 'slack_reply', $2, $3, 'sent', 'switchboard', $4, now(), now())`,
			s.taskID, ssrTarget, "earlier "+itoa(int64(i)), ssrHuman); err != nil {
			t.Fatalf("seed sent row: %v", err)
		}
	}

	ok, errs := s.fanOut(ctx, 6, func(i int) string { return "fan-out " + itoa(int64(i)) })
	if ok != 1 {
		t.Fatalf("%d of 6 concurrent calls succeeded with one slot left, want exactly 1 (errors: %v)", ok, errs)
	}
	for _, err := range errs {
		if !strings.Contains(err.Error(), "hourly send limit") {
			t.Errorf("refusal = %q, want the hourly limit (policy's or the handler's)", err)
		}
	}
	if s.fake.calls != 1 {
		t.Errorf("the bridge was called %d times, want 1", s.fake.calls)
	}
}

// The shared send half re-counts under the channel lock, so the human two-step
// is held to the limit too even when policy's snapshot was taken earlier.
func TestSlackAuto_SendHalfRechecksHourlyLimit(t *testing.T) {
	ctx := context.Background()
	s := newSsrSuite(t, ctx, "in_progress")
	t.Setenv("OPS_SEND_HOURLY_LIMIT", "1")
	s.fake.succeed()
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, approval_source, created_by, sent_at, send_attempted_at)
		 VALUES ($1, 'slack_reply', $2, 'already sent', 'sent', 'switchboard', $3, now(), now())`,
		s.taskID, ssrTarget, ssrHuman); err != nil {
		t.Fatalf("seed sent row: %v", err)
	}
	var id int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, approval_source, created_by)
		 VALUES ($1, 'slack_reply', $2, 'over the limit', 'approved', 'switchboard', $3) RETURNING id`,
		s.taskID, ssrTarget, ssrHuman).Scan(&id); err != nil {
		t.Fatalf("seed approved row: %v", err)
	}

	_, err := s.staticExecutor().Execute(ctx, executor.Call{Tool: "send_delivery", Actor: ssrHuman,
		Args: []byte(`{"delivery_id":` + itoa(id) + `}`)})
	if err == nil || !strings.Contains(err.Error(), "hourly send limit") {
		t.Fatalf("send_delivery over the limit (policy bypassed) = %v, want the send half's rate refusal", err)
	}
	var status string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM deliveries WHERE id=$1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "approved" || s.fake.calls != 0 {
		t.Errorf("row is %s and the bridge was called %d time(s); want approved and 0", status, s.fake.calls)
	}
}

// ---- codex third review: the kill switch with NO flag row yet ------------------

// On a db where sending_frozen was never written, a freeze whose upsert is in
// flight when the send half reaches phase 1 must still stop the click: the send
// waits for the freeze to commit, then refuses. Before the ensure-row insert,
// FOR SHARE on the absent row locked nothing and the send went out.
func TestSlackAuto_FreezeWithNoFlagRowIsOrdered(t *testing.T) {
	ctx := context.Background()
	s := newSsrSuite(t, ctx, "in_progress")
	s.fake.succeed()
	var id int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, approval_source, created_by)
		 VALUES ($1, 'slack_reply', $2, 'absent flag race', 'approved', 'switchboard', $3) RETURNING id`,
		s.taskID, ssrTarget, ssrHuman).Scan(&id); err != nil {
		t.Fatalf("seed approved row: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM ops_flags WHERE name='sending_frozen'`); err != nil {
		t.Fatal(err)
	}

	freezer, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = freezer.Rollback(context.Background()) }()
	if _, err := freezer.Exec(ctx,
		`INSERT INTO ops_flags (name, value) VALUES ('sending_frozen', '{"frozen": true}')
		 ON CONFLICT (name) DO UPDATE SET value=EXCLUDED.value`); err != nil {
		t.Fatalf("in-flight freeze: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.staticExecutor().Execute(ctx, executor.Call{Tool: "send_delivery", Actor: ssrHuman,
			Args: []byte(`{"delivery_id":` + itoa(id) + `}`)})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("send_delivery returned (%v) while a freeze of the absent flag row was in flight; "+
			"it must wait for it", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := freezer.Commit(ctx); err != nil {
		t.Fatalf("commit freeze: %v", err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "kill switch") {
			t.Fatalf("send_delivery after the freeze committed = %v, want the kill-switch refusal", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("send_delivery still blocked 10s after the freeze committed")
	}
	if s.fake.calls != 0 {
		t.Errorf("the bridge was called %d time(s) across a freeze", s.fake.calls)
	}
}

// With no flag row and no freeze, the send half creates the row as not-frozen
// and sends: the ensure-row insert changes no outcome.
func TestSlackAuto_NoFlagRowSendsAndSeedsIt(t *testing.T) {
	ctx := context.Background()
	s := newSsrSuite(t, ctx, "in_progress")
	s.fake.succeed()
	if _, err := s.pool.Exec(ctx, `DELETE FROM ops_flags WHERE name='sending_frozen'`); err != nil {
		t.Fatal(err)
	}
	s.mustCall(t, ctx, ssrSession, ssrTarget, ssrText)
	var frozen bool
	if err := s.pool.QueryRow(ctx,
		`SELECT (value->>'frozen')::boolean FROM ops_flags WHERE name='sending_frozen'`).Scan(&frozen); err != nil {
		t.Fatalf("sending_frozen row after a send on a flagless db: %v", err)
	}
	if frozen {
		t.Error("the ensured row says frozen; want false")
	}
}

// The channel lock in the shared send half (0x5157_0078) is what holds the
// human two-step to the limit under concurrency: N concurrent send_delivery
// calls on N approved rows with ONE slot left, policy's snapshot bypassed,
// admit exactly one. The fake bridge is slow so the calls overlap.
func TestSlackAuto_ConcurrentSendDeliveryRespectsHourlyLimit(t *testing.T) {
	ctx := context.Background()
	s := newSsrSuite(t, ctx, "in_progress")
	t.Setenv("OPS_SEND_HOURLY_LIMIT", "2")
	s.fake.succeed()
	s.fake.delay = 200 * time.Millisecond
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, approval_source, created_by, sent_at, send_attempted_at)
		 VALUES ($1, 'slack_reply', $2, 'already sent', 'sent', 'switchboard', $3, now(), now())`,
		s.taskID, ssrTarget, ssrHuman); err != nil {
		t.Fatalf("seed sent row: %v", err)
	}
	// One task per row: phase 1 locks the delivery's TASK row, so rows on one
	// task serialize there and would never exercise the channel lock.
	const n = 8
	ids := make([]int64, n)
	for i := range ids {
		var taskID int64
		if err := s.pool.QueryRow(ctx,
			`INSERT INTO tasks (project_id, title, assignee_type, status)
			 SELECT project_id, 'itest-ssr fan-out', 'human', 'in_progress' FROM tasks WHERE id=$1
			 RETURNING id`, s.taskID).Scan(&taskID); err != nil {
			t.Fatalf("seed task: %v", err)
		}
		if err := s.pool.QueryRow(ctx,
			`INSERT INTO deliveries (task_id, channel, target_ref, body, status, approval_source, created_by)
			 VALUES ($1, 'slack_reply', $2, $3, 'approved', 'switchboard', $4) RETURNING id`,
			taskID, ssrTarget, "human two-step "+itoa(int64(i)), ssrHuman).Scan(&ids[i]); err != nil {
			t.Fatalf("seed approved row: %v", err)
		}
	}
	// A phase-1 transaction lasts milliseconds, so N goroutines rarely overlap
	// there on their own. Hold the flag row FOR UPDATE: every send parks at its
	// FOR SHARE read, AFTER its row locks and BEFORE the channel lock and the
	// count; releasing it lets all of them reach the count together.
	gate, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gate.Rollback(context.Background()) }()
	if _, err := gate.Exec(ctx, `SELECT 1 FROM ops_flags WHERE name='sending_frozen' FOR UPDATE`); err != nil {
		t.Fatalf("hold the flag row: %v", err)
	}
	ex := s.staticExecutor()
	var mu sync.Mutex
	var wg sync.WaitGroup
	ok := 0
	var errs []error
	defer func() {
		if t.Failed() {
			t.Logf("outcomes: %d ok, errors %v", ok, errs)
		}
	}()
	for _, id := range ids {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			_, err := ex.Execute(ctx, executor.Call{Tool: "send_delivery", Actor: ssrHuman,
				Args: []byte(`{"delivery_id":` + itoa(id) + `}`)})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else {
				errs = append(errs, err)
			}
		}(id)
	}
	time.Sleep(500 * time.Millisecond) // let every call reach the gate
	if err := gate.Commit(ctx); err != nil {
		t.Fatalf("release the gate: %v", err)
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("%d of %d concurrent send_delivery calls succeeded with one slot left, want exactly 1 (%v)", ok, n, errs)
	}
	for _, err := range errs {
		if !strings.Contains(err.Error(), "hourly send limit") {
			t.Errorf("refusal = %q, want the send half's hourly limit", err)
		}
	}
}

// ---- codex fourth review: a hung bridge, and the human path's duplicates ----

// A /send that never answers is cut off by the dispatch bound: the row takes
// the ambiguous outcome (sending, UNSETTLED, diagnostic), the admission lock is
// released, and the next send_slack_reply goes through promptly.
func TestSlackAuto_HungBridgeIsBoundedAndReleasesAdmission(t *testing.T) {
	ctx := context.Background()
	s := newSsrSuite(t, ctx, "in_progress")
	t.Setenv("SLACK_SEND_DISPATCH_TIMEOUT", "300ms")
	s.fake.succeed()
	s.fake.delay = time.Hour // hung

	start := time.Now()
	_, err := s.call(ctx, ssrSession, ssrTarget, "the hung one")
	if err == nil || !strings.Contains(err.Error(), "outcome unknown") {
		t.Fatalf("send through a hung bridge = %v, want the ambiguous outcome", err)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("the hung send returned after %s; the dispatch bound is 300ms", el)
	}
	// The row stays 'sending' and UNSETTLED, with the diagnostic: the leaf may
	// still click after we stop waiting, so the lease must keep protecting it.
	var hungID int64
	var status string
	var settled *time.Time
	var diag *string
	if err := s.pool.QueryRow(ctx,
		`SELECT id, status, send_settled_at, error FROM deliveries WHERE task_id=$1 AND body='the hung one'`,
		s.taskID).Scan(&hungID, &status, &settled, &diag); err != nil {
		t.Fatal(err)
	}
	if status != "sending" || settled != nil || diag == nil || *diag == "" {
		t.Errorf("hung row is %s (settled %v, error %v), want sending, UNSETTLED, with the diagnostic",
			status, settled, diag)
	}
	// ...so mark_delivery_failed still refuses inside the lease.
	if _, err := s.ex.Execute(ctx, executor.Call{Tool: "mark_delivery_failed", Actor: ssrHuman,
		Args: []byte(`{"delivery_id":` + itoa(hungID) + `}`)}); err == nil {
		t.Error("mark_delivery_failed succeeded right after a dispatch timeout; the lease must still hold " +
			"(the leaf may yet click it, and a resend would double-post)")
	}

	s.fake.delay = 0
	start = time.Now()
	s.mustCall(t, ctx, ssrSession, ssrTarget, "the next one")
	if el := time.Since(start); el > 10*time.Second {
		t.Errorf("the next send waited %s: the admission lock was not released after the bound", el)
	}
}

// The send-time duplicate check holds on the HUMAN path too: an approved row
// whose words are already 'sending' to the same conversation is refused at
// phase 1, naming the earlier row, and stays approved.
func TestSlackAuto_SendHalfRefusesDuplicateAcrossPaths(t *testing.T) {
	ctx := context.Background()
	s := newSsrSuite(t, ctx, "in_progress")
	s.fake.succeed()
	var auto, human int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, approval_source, created_by, send_attempted_at)
		 VALUES ($1, 'slack_reply', $2, $3, 'sending', 'switchboard', $4, now()) RETURNING id`,
		s.taskID, ssrTarget, ssrText, ssrSession).Scan(&auto); err != nil {
		t.Fatalf("seed in-flight auto row: %v", err)
	}
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, approval_source, created_by)
		 VALUES ($1, 'slack_reply', $2, $3, 'approved', 'switchboard', $4) RETURNING id`,
		s.taskID, ssrTarget, ssrText, ssrHuman).Scan(&human); err != nil {
		t.Fatalf("seed approved human row: %v", err)
	}

	_, err := s.staticExecutor().Execute(ctx, executor.Call{Tool: "send_delivery", Actor: ssrHuman,
		Args: []byte(`{"delivery_id":` + itoa(human) + `}`)})
	if err == nil || !strings.Contains(err.Error(), itoa(auto)) || !strings.Contains(err.Error(), "still unresolved") {
		t.Fatalf("human send of words already in flight = %v, want a refusal naming delivery %d", err, auto)
	}
	var status string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM deliveries WHERE id=$1`, human).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "approved" || s.fake.calls != 0 {
		t.Errorf("human row is %s and the bridge was called %d time(s); want approved and 0", status, s.fake.calls)
	}
}

// ---- codex fifth review: the pool the admission holder depends on ------------

// A one-connection pool can never finish a call (the lock holds the only
// connection), so it is refused by name at once, before any row, instead of
// parking the holder forever.
func TestSlackAuto_OneConnectionPoolRefusedByName(t *testing.T) {
	ctx := context.Background()
	s := newSsrSuite(t, ctx, "in_progress")
	s.fake.succeed()
	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	one, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	reg := executor.NewRegistry()
	tools.Register(reg, one)
	ex := executor.New(reg, policy.NewStatic(reg.Names()...), audit.NewPGStore(s.pool))

	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err = ex.Execute(cctx, executor.Call{Tool: "send_slack_reply", Actor: ssrSession,
		Args: []byte(s.args(ssrTarget, ssrText))})
	if err == nil || !strings.Contains(err.Error(), "at least 2 connections") {
		t.Fatalf("send_slack_reply on a one-connection pool = %v, want the refusal by name", err)
	}
	if n := s.deliveryCount(t, ctx); n != 0 || s.fake.calls != 0 {
		t.Errorf("%d rows and %d bridge calls after the refusal, want none", n, s.fake.calls)
	}
}

// ---- codex sixth review: a caller-side cancel is not proof of anything --------

// The caller cancelling mid-send (an MCP client disconnecting, a dashboard
// request dropping) leaves the row unsettled too: the leaf may still click, so
// mark_delivery_failed must keep refusing within the lease.
func TestSlackAuto_CallerCancelDuringSendLeavesLeaseHeld(t *testing.T) {
	ctx := context.Background()
	s := newSsrSuite(t, ctx, "in_progress")
	s.fake.succeed()
	s.fake.delay = time.Hour // the bridge never answers

	cctx, cancel := context.WithCancel(ctx)
	time.AfterFunc(300*time.Millisecond, cancel)
	// The error text is not asserted: with the caller's ctx cancelled the
	// executor's audit-complete write fails too and its error wins (existing
	// executor behaviour, outside this ticket). The row is what matters.
	if _, err := s.call(cctx, ssrSession, ssrTarget, "the cancelled one"); err == nil {
		t.Fatal("send cancelled mid-dispatch succeeded")
	}
	var id int64
	var status string
	var settled *time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT id, status, send_settled_at FROM deliveries WHERE task_id=$1 AND body='the cancelled one'`,
		s.taskID).Scan(&id, &status, &settled); err != nil {
		t.Fatal(err)
	}
	if status != "sending" || settled != nil {
		t.Errorf("cancelled row is %s (settled %v), want sending and UNSETTLED", status, settled)
	}
	if _, err := s.ex.Execute(ctx, executor.Call{Tool: "mark_delivery_failed", Actor: ssrHuman,
		Args: []byte(`{"delivery_id":` + itoa(id) + `}`)}); err == nil {
		t.Error("mark_delivery_failed succeeded right after a caller-side cancel; the lease must still hold")
	}
}
