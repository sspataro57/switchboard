//go:build integration

package tools_test

// slack-send-queue (SWT-76) Part 3 — the delivery row: criteria 15, 16, 17, 18,
// 19, 20, plus D3's "the bound reaches the leaf" and migration 0042's applied
// schema.
//
// Build-tagged `integration` AND env-gated on DATABASE_URL. Every mutation goes
// through executor.Execute with the REAL policy Matrix (deliveryExecutor, from
// delivery_lifecycle_integration_test.go); the bridge is an injected fake
// tools.SlackSender that returns a 202-shaped SendOutcome — NEVER a live
// bridge, never a browser, never the Mac mini.
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_sendqueue?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run SlackQueue ./internal/tools/
//
// ---------------------------------------------------------------------------
// GREENFIELD NOTE — compile-FAILs today under `-tags integration`:
// tools.SlackSender's method takes three arguments and returns `error`, and
// slackweb.SendOutcome does not exist. Then, once it compiles, it fails at the
// first send: there is no queued branch, no send_queued_at, no
// send_queue_job_id, and a 202 is not reachable at all. Expected red.
//
// IMPOSED SURFACE (see http_bridge_send_queue_test.go for the full argument and
// for the ONE deviation from the SPEC's file placement — SendOutcome must live
// in slackweb, not tools, or internal/tools <-> internal/connector/slackweb is
// an import cycle):
//
//	type SlackSender interface {
//		Send(ctx context.Context, targetURL, text string, maxQueue time.Duration) (slackweb.SendOutcome, error)
//	}
//
// D4 is the spine of every assertion here and is worth restating: a queued send
// gets NO new status. Phase 1 already committed exactly the right row —
// status='sending', send_attempted_at=now(), send_settled_at=NULL
// (delivery.go:2042-2048) — and the 202 branch LEAVES BOTH ALONE. That single
// fact is what makes the row not re-approvable (approve_delivery takes only
// drafted/failed, :1102), un-retried (nothing sends a `sending` row), and
// protected from a human's mark_delivery_failed for sendAttemptLease
// (:2195-2205).
//
// Cross-suite discipline: this suite owns 'itest-sdq-%' and the synthetic Slack
// account tsdqtest@slack-web.local. It cleans its OWN corpus in FK order,
// rerunnably, at the start AND end of every test, and deletes by id / by its
// own slug — never wholesale. It leaves ops_flags.sending_frozen false.
//
// MUTATION MAP:
//	Set send_settled_at=now() on the queued branch  -> criteria 15, 19
//	Drop error=NULL from the queued UPDATE          -> criterion 15
//	Drop the send_attempted_at fence                -> criterion 16
//	Emit delivery_sent at enqueue time              -> criterion 17

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	sdqActor     = "dashboard:itest-sdq@example.com"
	sdqSlug      = "itest-sdq-proj"
	sdqClient    = "itest-sdq-client"
	sdqWorkspace = "TSDQTEST"
	sdqAcct      = "tsdqtest@slack-web.local"
	sdqTarget    = "https://app.slack.com/client/TSDQTEST/CSDQTEST"
	sdqBody      = "queued while the rotation was running; the leaf clicks it in the next gap"
	sdqJobID     = "send-4b19c2"
)

// ---- the injected fake leaf ---------------------------------------------------

// fakeQueueSender is the leaf's 202 (and its 200) without a leaf. It records the
// maxQueue the tool derived, reads the row AT dispatch time to prove phase 1
// committed before anything left the process (invariant 4), and can run a hook
// mid-flight so a concurrent resolution can be staged deterministically
// (criterion 16).
type fakeQueueSender struct {
	pool     *pgxpool.Pool
	calls    int
	lastText string
	// lastMaxQueue is the bound the tool derived from the lease and put in the
	// request. D3: the CALLER owns it, because the caller owns the lease.
	lastMaxQueue time.Duration
	// atDispatch is the row as it stood when the "bridge" was called.
	atDispatchStatus   string
	atDispatchSettled  *time.Time
	atDispatchQueuedAt *time.Time

	outcome slackweb.SendOutcome
	err     error
	// midFlight runs after the dispatch read and before the outcome is returned.
	midFlight func(ctx context.Context)
}

func (f *fakeQueueSender) Send(ctx context.Context, targetURL, text string, maxQueue time.Duration) (slackweb.SendOutcome, error) {
	f.calls++
	f.lastText, f.lastMaxQueue = text, maxQueue
	_ = f.pool.QueryRow(ctx,
		`SELECT status, send_settled_at, send_queued_at FROM deliveries
		  WHERE channel='slack_reply' AND target_ref=$1 AND status='sending'
		  ORDER BY id DESC LIMIT 1`, targetURL).
		Scan(&f.atDispatchStatus, &f.atDispatchSettled, &f.atDispatchQueuedAt)
	if f.midFlight != nil {
		f.midFlight(ctx)
	}
	return f.outcome, f.err
}

// queuedOutcome is the 202 the leaf answers when the browser is busy.
func queuedOutcome() slackweb.SendOutcome {
	return slackweb.SendOutcome{
		Queued:    true,
		JobID:     sdqJobID,
		QueuedAt:  time.Now().UTC().Truncate(time.Second),
		ExpiresIn: 10 * time.Minute,
	}
}

// ---- scaffolding ---------------------------------------------------------------

type sdqSuite struct {
	pool   *pgxpool.Pool
	ex     *executor.Executor
	fake   *fakeQueueSender
	taskID int64
}

func newSdqSuite(t *testing.T, ctx context.Context) sdqSuite {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db")
	}
	pool := newToolsPool(t, ctx)
	t.Cleanup(pool.Close)
	cleanupSdq(t, ctx, pool)
	t.Cleanup(func() { cleanupSdq(t, context.Background(), pool) })

	if _, err := pool.Exec(ctx,
		`INSERT INTO source_accounts (provider, account_email, domain_default, scopes, send_enabled, calendar_in_availability)
		 VALUES ('slack_web', $1, 'https://app.slack.com/client/TSDQTEST', ARRAY['CSDQTEST'], true, false)`,
		sdqAcct); err != nil {
		t.Fatalf("seed slack_web source_account: %v", err)
	}
	projID := seedProject(t, ctx, pool, sdqSlug, sdqClient)
	var taskID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'itest-sdq work','claude','done_locally') RETURNING id`, projID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	fake := &fakeQueueSender{pool: pool, outcome: queuedOutcome()}
	tools.SetSlackSender(fake)
	t.Cleanup(func() { tools.SetSlackSender(nil) })
	return sdqSuite{pool: pool, ex: deliveryExecutor(pool), fake: fake, taskID: taskID}
}

func cleanupSdq(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const ourTasks = `(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + sdqSlug + `'))`
	for _, st := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE ops_flags SET value='{"frozen": false}' WHERE name='sending_frozen'`, nil},
		{`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE actor=$1)`, []any{sdqActor}},
		{`DELETE FROM audit_events WHERE actor=$1`, []any{sdqActor}},
		{`DELETE FROM approvals WHERE subject_type='delivery' AND subject_id IN
			(SELECT id FROM deliveries WHERE task_id IN ` + ourTasks + `)`, nil},
		{`DELETE FROM task_events WHERE task_id IN ` + ourTasks, nil},
		{`DELETE FROM deliveries WHERE task_id IN ` + ourTasks, nil},
		{`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, []any{sdqSlug}},
		{`DELETE FROM decisions WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, []any{sdqSlug}},
		{`DELETE FROM projects WHERE slug=$1`, []any{sdqSlug}},
		{`DELETE FROM source_accounts WHERE provider='slack_web' AND account_email=$1`, []any{sdqAcct}},
	} {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

// approved drafts and approves one slack_reply, returning its id.
func (s sdqSuite) approved(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	out := callOK(t, ctx, s.ex, sdqActor, "draft_delivery",
		`{"task_id":`+itoa(s.taskID)+`,"channel":"slack_reply","body":"`+sdqBody+`","target_ref":"`+sdqTarget+`"}`)
	var r struct {
		DeliveryID int64 `json:"delivery_id"`
	}
	mustUnmarshal(t, out, &r)
	if r.DeliveryID == 0 {
		t.Fatal("draft_delivery returned delivery_id 0")
	}
	callOK(t, ctx, s.ex, sdqActor, "approve_delivery", `{"delivery_id":`+itoa(r.DeliveryID)+`}`)
	return r.DeliveryID
}

func (s sdqSuite) try(ctx context.Context, tool string, id int64) error {
	_, err := s.ex.Execute(ctx, executor.Call{Tool: tool, Actor: sdqActor,
		Args: []byte(`{"delivery_id":` + itoa(id) + `}`)})
	return err
}

// sdqRow is every column the queued contract touches. Read as one SELECT so a
// column dropped from the production write is visible here and nowhere else.
type sdqRow struct {
	status         string
	attemptedAt    *time.Time
	settledAt      *time.Time
	queuedAt       *time.Time
	queueJobID     *string
	errText        *string
	sentAt         *time.Time
	sentExternalID *string
}

func (s sdqSuite) row(t *testing.T, ctx context.Context, id int64) sdqRow {
	t.Helper()
	var r sdqRow
	if err := s.pool.QueryRow(ctx,
		`SELECT status, send_attempted_at, send_settled_at, send_queued_at, send_queue_job_id,
		        error, sent_at, sent_external_id
		   FROM deliveries WHERE id=$1`, id).
		Scan(&r.status, &r.attemptedAt, &r.settledAt, &r.queuedAt, &r.queueJobID,
			&r.errText, &r.sentAt, &r.sentExternalID); err != nil {
		t.Fatalf("read delivery %d (apply migration 0042 for send_queued_at/send_queue_job_id): %v", id, err)
	}
	return r
}

// logEvents returns the payloads of `log` task events whose kind matches.
func logEvents(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64, kind string) []map[string]any {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT payload FROM task_events WHERE task_id=$1 AND event_type='log' AND payload->>'kind'=$2
		  ORDER BY id`, taskID, kind)
	if err != nil {
		t.Fatalf("select log events: %v", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan log payload: %v", err)
		}
		var p map[string]any
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("unmarshal log payload: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// ---- migration 0042, as Postgres applied it ------------------------------------

// The other half of sendqueue_migration_test.go's file-shape guard. The runner
// keys on schema_migrations.version with NO checksum, so a file edited after it
// was applied is skipped SILENTLY — only a probe of the live catalog catches it.
func TestMigration0042_Integration_SendQueueColumns(t *testing.T) {
	ctx := context.Background()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db")
	}
	pool := newToolsPool(t, ctx)
	defer pool.Close()

	for _, c := range []struct{ name, wantType string }{
		{"send_queued_at", "timestamp with time zone"},
		{"send_queue_job_id", "text"},
	} {
		var dataType, nullable string
		var dflt *string
		if err := pool.QueryRow(ctx,
			`SELECT data_type, is_nullable, column_default FROM information_schema.columns
			  WHERE table_name='deliveries' AND column_name=$1`, c.name).Scan(&dataType, &nullable, &dflt); err != nil {
			t.Fatalf("deliveries.%s is missing: %v (run cmd/tools/migrate; 0042 is this ticket's migration)", c.name, err)
		}
		if dataType != c.wantType {
			t.Errorf("deliveries.%s is %s, want %s", c.name, dataType, c.wantType)
		}
		if nullable != "YES" {
			t.Errorf("deliveries.%s is NOT NULL; NULL is 'this send was never queued', which is true of every "+
				"row that existed before 0042", c.name)
		}
		if dflt != nil {
			t.Errorf("deliveries.%s has DEFAULT %q; a default would make historical rows claim a queue state "+
				"they never had", c.name, *dflt)
		}
	}

	// D4: no new status value. The CHECK is untouched.
	var check string
	if err := pool.QueryRow(ctx,
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
		  WHERE conrelid='deliveries'::regclass AND contype='c' AND pg_get_constraintdef(oid) ILIKE '%status%'
		  LIMIT 1`).Scan(&check); err != nil {
		t.Fatalf("read deliveries' status CHECK: %v", err)
	}
	if strings.Contains(strings.ToLower(check), "queued") {
		t.Errorf("deliveries.status' CHECK gained a queued value (%s). D4: a queued send is a `sending` row "+
			"whose attempt has not settled — a new status would need a new rule in every reader", check)
	}
}

// ---- criterion 15 (+ D3): the 202 branch ---------------------------------------

func TestSlackQueue_Integration_QueuedSendLeavesTheRowSendingAndUnsettled(t *testing.T) {
	ctx := context.Background()
	s := newSdqSuite(t, ctx)
	id := s.approved(t, ctx)

	// The row carries a stale reconciler flag from an earlier life. The queued
	// UPDATE must CLEAR it: `error` is where ReconcileUnconfirmed's fire-once
	// marker lives, and it skips any row already carrying it (reconcile.go:43,
	// :73, :137). A queued send IS a new attempt, so the alarm must be re-armed
	// or it goes permanently silent for exactly the delivery it already caught
	// once (IK: "An alarm whose fire-once marker is never cleared goes
	// permanently silent").
	if _, err := s.pool.Exec(ctx,
		`UPDATE deliveries SET error='unconfirmed after 3 export passes with no matching message in Slack'
		  WHERE id=$1`, id); err != nil {
		t.Fatalf("seed the stale reconciler flag: %v", err)
	}

	out, err := s.ex.Execute(ctx, executor.Call{Tool: "send_delivery", Actor: sdqActor,
		Args: []byte(`{"delivery_id":` + itoa(id) + `}`)})
	if err != nil {
		t.Fatalf("send_delivery on a leaf 202 = %v, want SUCCESS. The whole ticket: the leaf ACCEPTED the "+
			"send, so this is not a failure and must not cost Salvador a second approval (criterion 15)", err)
	}

	// The result (criterion 15 / the API table).
	var result struct {
		DeliveryID int64  `json:"delivery_id"`
		Status     string `json:"status"`
		Queued     bool   `json:"queued"`
		JobID      string `json:"job_id"`
		QueuedAt   string `json:"queued_at"`
	}
	mustUnmarshal(t, out.Output, &result)
	if result.DeliveryID != id || result.Status != "sending" || !result.Queued || result.JobID != sdqJobID {
		t.Errorf("send_delivery result = %s, want {delivery_id:%d, status:\"sending\", queued:true, job_id:%q}. "+
			"The executor's audit row stores no payload (executor.go:101), so the RESULT is the only place the "+
			"caller learns the send is queued rather than clicked", out.Output, id, sdqJobID)
	}
	if result.QueuedAt == "" {
		t.Errorf("send_delivery result has no queued_at: %s", out.Output)
	}

	// D3: the bound the tool derived from the lease reached the leaf.
	if s.fake.calls != 1 {
		t.Fatalf("bridge sends = %d, want exactly 1", s.fake.calls)
	}
	if s.fake.lastMaxQueue != 10*time.Minute {
		t.Errorf("the bridge was called with maxQueue=%s, want 10m. D3: max_queue_ms is DERIVED FROM THE LEASE "+
			"(sendQueueMaxWait + sendQueueClickAllowance <= sendAttemptLease — see "+
			"delivery_slack_lease_test.go), never a free env value, because the caller owns the 15-minute "+
			"window that protects this row", s.fake.lastMaxQueue)
	}
	if s.fake.atDispatchStatus != "sending" || s.fake.atDispatchSettled != nil {
		t.Errorf("at dispatch the row was status=%q settled=%v, want sending + NULL (invariant 4: phase 1 "+
			"commits BEFORE anything leaves the process)", s.fake.atDispatchStatus, s.fake.atDispatchSettled)
	}
	if s.fake.atDispatchQueuedAt != nil {
		t.Errorf("send_queued_at was already set at dispatch (%v); nothing may claim a queue before the leaf "+
			"answered 202", s.fake.atDispatchQueuedAt)
	}

	r := s.row(t, ctx, id)
	if r.status != "sending" {
		t.Errorf("status after a queued send = %q, want sending. D4: NO new status — the 202 branch leaves "+
			"status alone, and `sending` is already not re-approvable and never retried", r.status)
	}
	if r.settledAt != nil {
		t.Errorf("send_settled_at = %v after a queued send, want NULL. The attempt is HONESTLY still open: the "+
			"click has not happened. Settling it would let mark_delivery_failed reopen a row the leaf is about "+
			"to click, and a `failed` row is re-approvable — a double post", r.settledAt)
	}
	if r.attemptedAt == nil {
		t.Errorf("send_attempted_at is NULL; it is the lease's reference point")
	}
	if r.queuedAt == nil {
		t.Errorf("send_queued_at is NULL after a queued send; it is what the dashboard's \"queued on the " +
			"bridge\" label and its elapsed time read (D5)")
	}
	if r.queueJobID == nil || *r.queueJobID != sdqJobID {
		t.Errorf("send_queue_job_id = %v, want %q — the one thing that ties this row to a line in the mini's log",
			r.queueJobID, sdqJobID)
	}
	if r.errText != nil {
		t.Errorf("error = %q after a queued send, want NULL. `error=NULL` is not tidiness: it RE-ARMS "+
			"ReconcileUnconfirmed's fire-once marker for the new attempt (D5)", *r.errText)
	}
	if r.sentAt != nil || r.sentExternalID != nil {
		t.Errorf("a queued send stamped sent_at=%v / sent_external_id=%v; nothing has been clicked yet, and "+
			"the export matcher is the only thing that may stamp an id", r.sentAt, r.sentExternalID)
	}
}

// ---- criterion 16: the fence ---------------------------------------------------

// Every phase-2 write is fenced on `status='sending' AND send_attempted_at=$n`
// (delivery.go:2069-2070). The queued write is a phase-2 write and gets the same
// fence: a row another actor resolved between dispatch and return must NOT be
// overwritten, and the call must SAY so rather than report a clean queue.
func TestSlackQueue_Integration_QueuedWriteIsFencedOnTheAttempt(t *testing.T) {
	ctx := context.Background()
	s := newSdqSuite(t, ctx)
	id := s.approved(t, ctx)

	// While the "bridge" call is in flight, a human sees the message in Slack and
	// runs mark_delivery_sent. (Staged with SQL rather than the tool so the race
	// is deterministic and the assertion is about the FENCE, not about locking.)
	s.fake.midFlight = func(ctx context.Context) {
		if _, err := s.pool.Exec(ctx,
			`UPDATE deliveries SET status='sent', sent_at=now(), send_settled_at=now(),
			        sent_external_id='slack:TSDQTEST:CSDQTEST:p1790000000000001', updated_at=now()
			  WHERE id=$1`, id); err != nil {
			t.Errorf("stage the concurrent resolution: %v", err)
		}
	}

	err := s.try(ctx, "send_delivery", id)
	if err == nil {
		t.Fatal("send_delivery reported a clean queue for a row another actor had already resolved; the caller " +
			"must be told, or a human reads \"queued\" about a row that is already sent (criterion 16)")
	}

	r := s.row(t, ctx, id)
	if r.status != "sent" {
		t.Errorf("status = %q after the fenced write, want the other actor's `sent` untouched", r.status)
	}
	if r.queuedAt != nil || r.queueJobID != nil {
		t.Errorf("the queued write landed on a resolved row (send_queued_at=%v job=%v). Dropping the "+
			"`send_attempted_at=$n` fence lets a late-returning attempt overwrite the outcome of a newer one",
			r.queuedAt, r.queueJobID)
	}
	if r.sentExternalID == nil {
		t.Errorf("the other actor's sent_external_id was cleared; invariant 4 never walks back a send")
	}
}

// ---- criterion 17: the event ---------------------------------------------------

func TestSlackQueue_Integration_WritesADeliveryQueuedLogAndNothingLifecycleBearing(t *testing.T) {
	ctx := context.Background()
	s := newSdqSuite(t, ctx)
	id := s.approved(t, ctx)

	if err := s.try(ctx, "send_delivery", id); err != nil {
		t.Fatalf("send_delivery on a 202 = %v, want success", err)
	}

	events := logEvents(t, ctx, s.pool, s.taskID, "delivery_queued")
	if len(events) != 1 {
		t.Fatalf("log events with kind=delivery_queued = %d, want exactly 1. D5: `task_events.event_type` is "+
			"free TEXT, and `log` with a `kind` is the shape SWT-71 used (delivery.go:811-813)", len(events))
	}
	p := events[0]
	if got, ok := p["delivery_id"].(float64); !ok || int64(got) != id {
		t.Errorf("delivery_queued payload delivery_id = %v, want %d", p["delivery_id"], id)
	}
	if p["job_id"] != sdqJobID {
		t.Errorf("delivery_queued payload job_id = %v, want %q", p["job_id"], sdqJobID)
	}
	for _, key := range []string{"queued_at", "max_queue_ms"} {
		if _, ok := p[key]; !ok {
			t.Errorf("delivery_queued payload has no %q; the event is the audit-visible record of WHEN the "+
				"leaf took it and HOW LONG it was allowed to hold it (payload: %v)", key, p)
		}
	}

	// The two events that must NOT exist. delivery_sent drives orchestrator R8
	// (rules.go:114, :271-309): emitting it at enqueue would mark the work task
	// `delivered` and CLOSE its Deliver task for a message nobody has sent yet,
	// and record the delivery_lifecycle dedup key so the real confirmation later
	// changes nothing. delivery_failed has no orchestrator rule at all, and the
	// send did not fail.
	if n := eventCount(t, ctx, s.pool, s.taskID, "delivery_sent"); n != 0 {
		t.Errorf("delivery_sent events after a QUEUED send = %d, want 0 — nothing has left yet (criterion 17)", n)
	}
	if n := eventCount(t, ctx, s.pool, s.taskID, "delivery_failed"); n != 0 {
		t.Errorf("delivery_failed events after a QUEUED send = %d, want 0 — it did not fail", n)
	}
	if st := taskStatus(t, ctx, s.pool, s.taskID); st != "done_locally" {
		t.Errorf("the work task moved to %q on a QUEUED send; it advances on confirmation (D7), not on "+
			"acceptance", st)
	}
}

// ---- criterion 20: the audit trail --------------------------------------------

// A queued send is an ALLOWED, AUDITED call, not an error (invariant 3). The
// audit row proves the call happened; it stores no payload (executor.go:101),
// which is precisely why the queue fact also lives on the row and in the event.
func TestSlackQueue_Integration_QueuedCallIsAuditedOK(t *testing.T) {
	ctx := context.Background()
	s := newSdqSuite(t, ctx)
	id := s.approved(t, ctx)
	if err := s.try(ctx, "send_delivery", id); err != nil {
		t.Fatalf("send_delivery on a 202 = %v, want success", err)
	}

	// audit.PGStore INSERTs 'started' and UPDATEs the same row to 'ok' with
	// completed_at — start and complete are one row's two states.
	var status string
	var completed *time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT status, completed_at FROM audit_events
		  WHERE actor=$1 AND tool='send_delivery' AND args->>'delivery_id'=$2
		  ORDER BY id DESC LIMIT 1`, sdqActor, itoa(id)).Scan(&status, &completed); err != nil {
		t.Fatalf("no audit_events row for the queued send_delivery: %v (invariant 3)", err)
	}
	if status != "ok" || completed == nil {
		t.Errorf("audit_events row = status %q completed_at %v, want ok + a completion stamp: a queued send is "+
			"an allowed call that SUCCEEDED, not an error (criterion 20)", status, completed)
	}

	var decision, rule string
	if err := s.pool.QueryRow(ctx,
		`SELECT p.decision, COALESCE(p.rule,'') FROM policy_decisions p
		   JOIN audit_events a ON a.id = p.audit_event_id
		  WHERE a.actor=$1 AND p.tool='send_delivery' ORDER BY p.id DESC LIMIT 1`, sdqActor).Scan(&decision, &rule); err != nil {
		t.Fatalf("no policy_decisions row for the queued send_delivery: %v (invariant 3)", err)
	}
	if decision != "allow" {
		t.Errorf("policy decision for the queued send = %q (rule %q), want allow — D11: send_delivery stays "+
			"sendShaped on the same matrix row", decision, rule)
	}
}

// ---- criterion 18: the queued row is not re-approvable and not re-sendable ----

// Pinned here because it is now THE guard (D4's table, rows 1 and 2): nothing
// else stops a second click while the leaf holds the job.
func TestSlackQueue_Integration_QueuedRowRefusesApproveAndSend(t *testing.T) {
	ctx := context.Background()
	s := newSdqSuite(t, ctx)
	id := s.approved(t, ctx)
	if err := s.try(ctx, "send_delivery", id); err != nil {
		t.Fatalf("send_delivery on a 202 = %v, want success", err)
	}
	callsAfterQueue := s.fake.calls

	if err := s.try(ctx, "approve_delivery", id); err == nil {
		t.Error("approve_delivery accepted a QUEUED row. approve_delivery takes only drafted/failed " +
			"(delivery.go:1102) and `sending` is neither; if it ever did, a queued send could be approved and " +
			"sent a second time while the first click is still pending (criterion 18)")
	}
	if err := s.try(ctx, "send_delivery", id); err == nil {
		t.Error("send_delivery accepted a QUEUED row; only `approved` deliveries send (delivery.go:1993)")
	}
	if s.fake.calls != callsAfterQueue {
		t.Errorf("bridge sends = %d after the refusals, want still %d: both guards are pre-network",
			s.fake.calls, callsAfterQueue)
	}
	if r := s.row(t, ctx, id); r.status != "sending" || r.settledAt != nil {
		t.Errorf("after the refusals the row is status=%q settled=%v, want sending + unsettled (untouched)",
			r.status, r.settledAt)
	}
}

// ---- criterion 19: the lease, both sides --------------------------------------

func TestSlackQueue_Integration_MarkFailedRefusedDuringTheLeaseAndPermittedAfter(t *testing.T) {
	ctx := context.Background()
	s := newSdqSuite(t, ctx)
	id := s.approved(t, ctx)
	if err := s.try(ctx, "send_delivery", id); err != nil {
		t.Fatalf("send_delivery on a 202 = %v, want success", err)
	}

	err := s.try(ctx, "mark_delivery_failed", id)
	if err == nil {
		t.Fatal("mark_delivery_failed resolved a QUEUED send inside the lease. That refusal IS the protection " +
			"(D4/D6): a `failed` row is re-approvable, so declaring a pending click failed is an automatic path " +
			"to a double post into a client conversation (criterion 19)")
	}
	if !strings.Contains(err.Error(), "in flight") {
		t.Errorf("the refusal = %v; it must NAME the in-flight attempt so the human knows what to wait for "+
			"(delivery.go:2195-2205)", err)
	}
	if r := s.row(t, ctx, id); r.status != "sending" {
		t.Errorf("status after the refused mark_delivery_failed = %q, want sending", r.status)
	}

	// The horizon, and there is no other (D6): T + sendAttemptLease. By then the
	// leaf can no longer click — enqueue + 10 min TTL + ~30 s click < 15 min.
	if _, err := s.pool.Exec(ctx,
		`UPDATE deliveries SET send_attempted_at = now() - interval '16 minutes',
		        send_queued_at = now() - interval '16 minutes' WHERE id=$1`, id); err != nil {
		t.Fatalf("age the attempt past the lease: %v", err)
	}
	if err := s.try(ctx, "mark_delivery_failed", id); err != nil {
		t.Fatalf("mark_delivery_failed after the lease = %v, want permitted: a sender that crashed mid-click "+
			"must not wedge the row forever (criterion 19)", err)
	}
	if r := s.row(t, ctx, id); r.status != "failed" {
		t.Errorf("status after the permitted mark_delivery_failed = %q, want failed", r.status)
	}
}

// The asymmetry D4 calls deliberate and D5 makes the dashboard reflect:
// RECORDING a send that happened is always safe, throughout the lease.
func TestSlackQueue_Integration_MarkSentIsPermittedThroughoutAndEmitsDeliverySent(t *testing.T) {
	ctx := context.Background()
	s := newSdqSuite(t, ctx)
	id := s.approved(t, ctx)
	if err := s.try(ctx, "send_delivery", id); err != nil {
		t.Fatalf("send_delivery on a 202 = %v, want success", err)
	}

	// Seconds into the lease: Salvador looked in Slack and the message is there
	// (the leaf clicked it in the first gap).
	before := eventCount(t, ctx, s.pool, s.taskID, "delivery_sent")
	if err := s.try(ctx, "mark_delivery_sent", id); err != nil {
		t.Fatalf("mark_delivery_sent on a queued row = %v, want permitted. It is safe BY CONSTRUCTION — it "+
			"records a message that is visibly in Slack — and it is the human's fast path out of the lease "+
			"(criterion 19)", err)
	}
	r := s.row(t, ctx, id)
	if r.status != "sent" {
		t.Errorf("status after mark_delivery_sent = %q, want sent", r.status)
	}
	if r.errText != nil {
		t.Errorf("error = %q after mark_delivery_sent; the re-arm rule applies to every path that resolves an "+
			"attempt (IK: the fire-once marker)", *r.errText)
	}
	if n := eventCount(t, ctx, s.pool, s.taskID, "delivery_sent"); n != before+1 {
		t.Errorf("delivery_sent events = %d, want %d: mark_delivery_sent emits it so R8 advances the work "+
			"(D4's table, row 4)", n, before+1)
	}
}
