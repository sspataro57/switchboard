//go:build integration

package tools_test

// Integration tests for SWT-43 reject_delivery (docs/tickets/delivery-deny_SPEC.md),
// criteria 8-14, 17 and 35. Build-tagged `integration` AND env-gated on
// DATABASE_URL. Every verb goes through executor.Execute with the REAL policy
// Matrix (deliveryExecutor, delivery_lifecycle_integration_test.go) over the
// compose db; every network seam is a fake that counts calls — NEVER a live
// Gmail, Jira, Slack or Pipedream call.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run RejectDelivery ./internal/tools/
//
// GREENFIELD NOTE — EXPECTED RED. reject_delivery is not registered (every
// Execute returns "unknown tool"), and migration 0028 does not exist, so every
// fixture that seeds a 'rejected' row fails on deliveries_status_check (and on
// the missing rejection_note / redraft_requested_at columns). Both are the
// right reasons.
//
// IMPOSED CONTRACT (SPEC "API / MCP tool changes"):
//
//	reject_delivery {delivery_id: int, note?: string, redraft?: bool}
//	  -> {"delivery_id": N, "status": "rejected", "redraft": bool, "changed": bool}
//	task_events 'delivery_rejected' on deliveries.task_id, payload
//	  {delivery_id, channel, redraft, note}
//	approvals ('delivery', id, 'rejected', actor, now()) on a real transition only
//
// Fixtures are written directly (a fixture is not a production write); every
// assertion about invariant 3 is made against the VERB's effects.
//
// Cross-suite discipline: this suite owns project itest-deny-proj, the google
// account itest-deny-g@example.com, the thread keys below and the actors
// rjActor / rjWorkerActor; it cleans exactly those in FK order — audit rows
// (policy_decisions first) BEFORE tasks (the IK SWT-37 landmine), approvals
// explicitly (no FK to deliveries) — at start and end. It seeds ONE inbound
// normalized message (send_delivery needs a reply target), under its own
// account, deleted with it. It leaves ops_flags.sending_frozen false.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	rjActor       = "dashboard:itest-deny@example.com" // human (matrix human_only allows)
	rjWorkerActor = "mcp:itest-deny-worker"            // a worker console shape (denied)
	rjSlug        = "itest-deny-proj"
	rjClient      = "itest-deny-client"
	rjGAcct       = "itest-deny-g@example.com"
	rjGThreadKey  = "gmail:itest-deny-g@example.com:gthread-deny-1"
	rjInboundMID  = "<inbound-itest-deny-1@itest-deny.example>"
	rjJiraTarget  = "jira:itest-deny.atlassian.net:DENY-1"
	rjSlackTarget = "https://app.slack.com/client/TSDDENY/CSDDENY"
	rjUpClient    = "eeee0001-0000-0000-0000-00000000de01"
	rjUpKey       = "upwork_crm:eeee0001-0000-0000-0000-00000000de01:room:room_de00000001"
)

// ---- fixture -------------------------------------------------------------------

type rjFixture struct {
	pool      *pgxpool.Pool
	ex        *executor.Executor
	projectID int64
	taskID    int64 // the done_locally work task (Redo's precondition, D7)
	gAcct     int64
	threadID  int64 // gmail thread with one inbound message
	upThread  int64 // upwork thread (0019's identity CHECK needs a thread_id)
	seq       int
}

func cleanupRJ(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const projs = `(SELECT id FROM projects WHERE slug='` + rjSlug + `')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	const dels = `(SELECT id FROM deliveries WHERE task_id IN ` + tasksOf + `)`
	const accts = `(SELECT id FROM source_accounts WHERE account_email='` + rjGAcct + `')`
	const actors = `('` + rjActor + `','` + rjWorkerActor + `')`
	const threads = `('` + rjGThreadKey + `','` + rjUpKey + `')`
	for _, q := range []string{
		`UPDATE ops_flags SET value='{"frozen": false}' WHERE name='sending_frozen'`,
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE actor IN ` + actors + ` OR task_id IN ` + tasksOf + `)`,
		`DELETE FROM audit_events WHERE actor IN ` + actors + ` OR task_id IN ` + tasksOf,
		// approvals has NO FK to deliveries: delete it explicitly, while the
		// deliveries it names still exist to be selected.
		`DELETE FROM approvals WHERE subject_type='delivery' AND subject_id IN ` + dels,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM deliveries WHERE task_id IN ` + tasksOf,
		`DELETE FROM normalized_messages WHERE thread_id IN (SELECT id FROM normalized_threads WHERE thread_key IN ` + threads + `)`,
		`DELETE FROM normalized_threads WHERE thread_key IN ` + threads,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM tasks WHERE parent_id IS NOT NULL AND project_id IN ` + projs,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM projects WHERE slug='` + rjSlug + `'`,
		`DELETE FROM source_accounts WHERE account_email='` + rjGAcct + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func rjIns(t *testing.T, ctx context.Context, pool *pgxpool.Pool, q string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", q, err)
	}
	return id
}

func newRJFixture(t *testing.T, ctx context.Context) *rjFixture {
	t.Helper()
	pool := newToolsPool(t, ctx)
	t.Cleanup(pool.Close)
	cleanupRJ(t, ctx, pool)
	t.Cleanup(func() { cleanupRJ(t, ctx, pool) })

	f := &rjFixture{pool: pool, ex: deliveryExecutor(pool)}
	f.gAcct = rjIns(t, ctx, pool,
		`INSERT INTO source_accounts (provider, account_email, send_enabled) VALUES ('google',$1,true) RETURNING id`, rjGAcct)
	f.projectID = seedProject(t, ctx, pool, rjSlug, rjClient)
	f.taskID = f.task(t, ctx, "done_locally")

	raw := rjIns(t, ctx, pool,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1,'itest-deny-raw-1','{}','itest-deny-hash-1') RETURNING id`, f.gAcct)
	f.threadID = rjIns(t, ctx, pool,
		`INSERT INTO normalized_threads (thread_key, subject) VALUES ($1,'login broken') RETURNING id`, rjGThreadKey)
	rjIns(t, ctx, pool,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1,$2,'inbound',$3, now() - interval '1 hour','the login page is down','login broken',
		         'client@itest-deny.example','gmail') RETURNING id`, raw, f.threadID, rjInboundMID)
	f.upThread = rjIns(t, ctx, pool,
		`INSERT INTO normalized_threads (thread_key, participants) VALUES ($1,'[]') RETURNING id`, rjUpKey)
	return f
}

func (f *rjFixture) task(t *testing.T, ctx context.Context, status string) int64 {
	t.Helper()
	return rjIns(t, ctx, f.pool,
		`INSERT INTO tasks (project_id, title, assignee_type, status) VALUES ($1,$2,'claude',$3) RETURNING id`,
		f.projectID, "itest-deny work ("+status+")", status)
}

// rjSpec is one deliveries row. The per-channel identity columns (thread for
// gmail/upwork, target_ref, 0019's target_client_ref, 0020's interval and
// account) are filled by row() so every fixture is a row production could hold.
type rjSpec struct {
	taskID         int64 // 0 = the fixture's done_locally task
	channel        string
	status         string
	body           string
	extID          string // "" = NULL; made unique per row (deliveries_sent_external_idx)
	confirmed      bool
	attempted      bool   // send_attempted_at 20 minutes ago
	approvalSource string // "" = NULL
	note           *string
	redraft        bool // rejected rows only
}

func (f *rjFixture) row(t *testing.T, ctx context.Context, s rjSpec) int64 {
	t.Helper()
	f.seq++
	taskID := s.taskID
	if taskID == 0 {
		taskID = f.taskID
	}
	body := s.body
	if body == "" {
		body = fmt.Sprintf("itest-deny draft body %d", f.seq)
	}
	var targetRef, targetClient, threadID, fromAcct, startsAt, endsAt, ext, src any
	switch s.channel {
	case "gmail":
		threadID, fromAcct = f.threadID, f.gAcct
	case "jira_comment":
		targetRef = rjJiraTarget
	case "slack_reply":
		targetRef = rjSlackTarget
	case "upwork_chat":
		targetRef, targetClient, threadID = rjUpKey, rjUpClient, f.upThread
	case "calendar":
		st := time.Now().Add(72 * time.Hour).Truncate(time.Hour)
		targetRef, fromAcct, startsAt, endsAt = rjGAcct, f.gAcct, st, st.Add(time.Hour)
	default:
		t.Fatalf("rjSpec channel %q unknown", s.channel)
	}
	if s.extID != "" {
		ext = fmt.Sprintf("%s-%d-%d", s.extID, f.seq, time.Now().UnixNano())
	}
	if s.approvalSource != "" {
		src = s.approvalSource
	}
	cols := `task_id, channel, target_ref, target_client_ref, thread_id, from_account_id, body, subject, status,
	         sent_external_id, confirmed_at, approval_source, send_attempted_at, starts_at, ends_at, created_by`
	vals := `$1,$2,$3,$4,$5,$6,$7,'itest-deny subject',$8,$9,
	         CASE WHEN $10::boolean THEN now() END, $11,
	         CASE WHEN $12::boolean THEN now() - interval '20 minutes' END, $13, $14, 'itest-deny'`
	args := []any{taskID, s.channel, targetRef, targetClient, threadID, fromAcct, body, s.status,
		ext, s.confirmed, src, s.attempted, startsAt, endsAt}
	if s.status == "rejected" {
		cols += `, rejection_note, redraft_requested_at`
		vals += `, $15, CASE WHEN $16::boolean THEN now() END`
		args = append(args, s.note, s.redraft)
	}
	var id int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO deliveries (`+cols+`) VALUES (`+vals+`) RETURNING id`, args...).Scan(&id); err != nil {
		t.Fatalf("seed %s/%s delivery: %v (a 'rejected' row needs migration 0028: the widened "+
			"deliveries_status_check plus rejection_note / redraft_requested_at)", s.channel, s.status, err)
	}
	return id
}

// fingerprint is the WHOLE row — status, sent_external_id, send_attempted_at,
// confirmed_at, updated_at, the note, the redraft flag. "Byte-unchanged"
// (criterion 13) means every column, not the three the SPEC names.
func (f *rjFixture) fingerprint(t *testing.T, ctx context.Context, id int64) string {
	t.Helper()
	var s string
	if err := f.pool.QueryRow(ctx, `SELECT md5(d::text) FROM deliveries d WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatalf("fingerprint delivery %d: %v", id, err)
	}
	return s
}

type rjState struct {
	status    string
	note      *string
	redraft   bool
	extID     *string
	confirmed bool
}

func (f *rjFixture) state(t *testing.T, ctx context.Context, id int64) rjState {
	t.Helper()
	var s rjState
	if err := f.pool.QueryRow(ctx,
		`SELECT status, rejection_note, redraft_requested_at IS NOT NULL, sent_external_id, confirmed_at IS NOT NULL
		   FROM deliveries WHERE id=$1`, id).Scan(&s.status, &s.note, &s.redraft, &s.extID, &s.confirmed); err != nil {
		t.Fatalf("read delivery %d: %v", id, err)
	}
	return s
}

type rjResult struct {
	DeliveryID int64  `json:"delivery_id"`
	Status     string `json:"status"`
	Redraft    bool   `json:"redraft"`
	Changed    bool   `json:"changed"`
}

// rejectRaw sends exactly the given args object.
func (f *rjFixture) rejectRaw(ctx context.Context, actor string, args map[string]any) (rjResult, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return rjResult{}, err
	}
	res, err := f.ex.Execute(ctx, executor.Call{Tool: "reject_delivery", Actor: actor, Args: raw})
	if err != nil {
		return rjResult{}, err
	}
	var r rjResult
	if err := json.Unmarshal(res.Output, &r); err != nil {
		return rjResult{}, fmt.Errorf("reject_delivery output %s is not the SPEC's result object: %w", res.Output, err)
	}
	return r, nil
}

func (f *rjFixture) reject(ctx context.Context, actor string, id int64, redraft bool, note *string) (rjResult, error) {
	args := map[string]any{"delivery_id": id, "redraft": redraft}
	if note != nil {
		args["note"] = *note
	}
	return f.rejectRaw(ctx, actor, args)
}

func (f *rjFixture) approvals(t *testing.T, ctx context.Context, id int64) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM approvals WHERE subject_type='delivery' AND subject_id=$1 AND status='rejected'`,
		id).Scan(&n); err != nil {
		t.Fatalf("count rejected approvals for delivery %d: %v", id, err)
	}
	return n
}

type rjEvent struct {
	DeliveryID int64   `json:"delivery_id"`
	Channel    string  `json:"channel"`
	Redraft    *bool   `json:"redraft"`
	Note       *string `json:"note"`
}

func (f *rjFixture) events(t *testing.T, ctx context.Context, id int64) []rjEvent {
	t.Helper()
	rows, err := f.pool.Query(ctx,
		`SELECT payload::text FROM task_events
		  WHERE event_type='delivery_rejected' AND payload->>'delivery_id' = $1 ORDER BY id`, itoa(id))
	if err != nil {
		t.Fatalf("read delivery_rejected events: %v", err)
	}
	defer rows.Close()
	var out []rjEvent
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		var e rjEvent
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			t.Fatalf("delivery_rejected payload %s: %v", raw, err)
		}
		out = append(out, e)
	}
	return out
}

func rjStr(s string) *string { return &s }

func rjDeref(p *string) string {
	if p == nil {
		return "<NULL>"
	}
	return *p
}

// ---- criteria 9, 11, 12: the transitions that happen ---------------------------

// The table's first two rows, both columns. A drafted, approved, or definitely
// failed (id NULL, not confirmed, not jira_comment) row becomes 'rejected';
// Redo also stamps redraft_requested_at. All four writes land (criterion 11).
func TestRejectDelivery_Integration_AllowedTransitionsWriteAllFour(t *testing.T) {
	ctx := context.Background()
	f := newRJFixture(t, ctx)

	for _, from := range []rjSpec{
		{channel: "gmail", status: "drafted"},
		{channel: "gmail", status: "approved", approvalSource: "switchboard"},
		// gmail failed+NULL only arises from SendRejectedError — a DEFINITE refusal (D4).
		{channel: "gmail", status: "failed", attempted: true},
		// slack failed only arises from slackweb.SendRejectedError, also definite.
		{channel: "slack_reply", status: "failed", attempted: true, approvalSource: "switchboard"},
		{channel: "upwork_chat", status: "drafted"},
		{channel: "jira_comment", status: "drafted"}, // D4 excludes FAILED jira only
	} {
		for _, redraft := range []bool{false, true} {
			from, redraft := from, redraft
			name := from.channel + "/" + from.status + "/redraft=" + strconv.FormatBool(redraft)
			t.Run(name, func(t *testing.T) {
				id := f.row(t, ctx, from)
				note := "wrong tone: " + name
				res, err := f.reject(ctx, rjActor, id, redraft, &note)
				if err != nil {
					t.Fatalf("reject_delivery on a %s %s row: %v", from.status, from.channel, err)
				}
				want := rjResult{DeliveryID: id, Status: "rejected", Redraft: redraft, Changed: true}
				if res != want {
					t.Errorf("result = %+v, want %+v (criterion 12)", res, want)
				}

				st := f.state(t, ctx, id)
				if st.status != "rejected" {
					t.Errorf("status = %q, want rejected", st.status)
				}
				if rjDeref(st.note) != note {
					t.Errorf("rejection_note = %q, want %q", rjDeref(st.note), note)
				}
				if st.redraft != redraft {
					t.Errorf("redraft_requested_at set = %v, want %v", st.redraft, redraft)
				}
				if st.extID != nil || st.confirmed {
					t.Errorf("a rejected row carries sent_external_id=%s confirmed=%v", rjDeref(st.extID), st.confirmed)
				}

				// The approvals row, the approveDelivery idiom: ('delivery', id,
				// 'rejected', actor, now()). This is the labelled data (D8).
				var decided int
				if err := f.pool.QueryRow(ctx,
					`SELECT count(*) FROM approvals WHERE subject_type='delivery' AND subject_id=$1
					    AND status='rejected' AND decided_by=$2 AND decided_at IS NOT NULL`, id, rjActor).Scan(&decided); err != nil {
					t.Fatalf("read approvals: %v", err)
				}
				if decided != 1 {
					t.Errorf("approvals ('delivery', %d, 'rejected', %s) rows = %d, want exactly 1", id, rjActor, decided)
				}

				evs := f.events(t, ctx, id)
				if len(evs) != 1 {
					t.Fatalf("delivery_rejected events = %d, want exactly 1 (criterion 11)", len(evs))
				}
				e := evs[0]
				if e.Channel != from.channel {
					t.Errorf("event channel = %q, want %q", e.Channel, from.channel)
				}
				if e.Redraft == nil || *e.Redraft != redraft {
					t.Errorf("event redraft = %v, want %v — audit rows tell Deny from Redo only through this bit and args (D2)", e.Redraft, redraft)
				}
				if rjDeref(e.Note) != note {
					t.Errorf("event note = %q, want %q (the work task's log shows the verdict and the note, D10)", rjDeref(e.Note), note)
				}
				var onTask int
				if err := f.pool.QueryRow(ctx,
					`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_rejected'
					    AND payload->>'delivery_id'=$2`, f.taskID, itoa(id)).Scan(&onTask); err != nil {
					t.Fatalf("read task events: %v", err)
				}
				if onTask != 1 {
					t.Errorf("delivery_rejected on deliveries.task_id %d = %d, want 1", f.taskID, onTask)
				}
			})
		}
	}
}

// ---- criterion 9: the states that refuse ----------------------------------------

func TestRejectDelivery_Integration_RefusedStatesAreUntouched(t *testing.T) {
	ctx := context.Background()
	f := newRJFixture(t, ctx)

	for _, tc := range []struct {
		name   string
		spec   rjSpec
		wantIn string // the reason, beyond id/status/channel
	}{
		// D4: sendJiraComment writes failed+NULL for EVERY error; the comment may
		// have landed and the jira matcher still claims failed rows.
		{"failed jira_comment (D4)", rjSpec{channel: "jira_comment", status: "failed"}, ""},
		{"failed jira_comment even after an attempt (D4)", rjSpec{channel: "jira_comment", status: "failed", attempted: true}, ""},
		{"failed gmail carrying its reserved id", rjSpec{channel: "gmail", status: "failed", extID: "<sb-itest-deny@example.com>", attempted: true}, "may have been sent"},
		{"failed gmail already confirmed", rjSpec{channel: "gmail", status: "failed", confirmed: true, attempted: true}, "may have been sent"},
		{"sending gmail", rjSpec{channel: "gmail", status: "sending", extID: "<sb-itest-deny-s@example.com>", attempted: true, approvalSource: "switchboard"}, ""},
		{"sending slack_reply", rjSpec{channel: "slack_reply", status: "sending", attempted: true, approvalSource: "switchboard"}, ""},
		{"sent gmail", rjSpec{channel: "gmail", status: "sent", extID: "<sb-itest-deny-t@example.com>", approvalSource: "switchboard"}, ""},
		{"sent upwork_chat", rjSpec{channel: "upwork_chat", status: "sent"}, ""},
	} {
		for _, redraft := range []bool{false, true} {
			tc, redraft := tc, redraft
			t.Run(tc.name+"/redraft="+strconv.FormatBool(redraft), func(t *testing.T) {
				id := f.row(t, ctx, tc.spec)
				before := f.fingerprint(t, ctx, id)

				_, err := f.reject(ctx, rjActor, id, redraft, rjStr("should not land"))
				if err == nil {
					t.Fatalf("reject_delivery on a %s %s row succeeded; criterion 9 refuses it", tc.spec.status, tc.spec.channel)
				}
				msg := err.Error()
				if strings.Contains(msg, "unknown tool") {
					t.Fatalf("reject_delivery is not registered: %v", err)
				}
				if strings.Contains(msg, "denied by policy") {
					t.Fatalf("refused by POLICY (%v); a human caller must reach the handler and be refused by its "+
						"state table", err)
				}
				for _, want := range []string{itoa(id), tc.spec.status, tc.spec.channel, tc.wantIn} {
					if !strings.Contains(msg, want) {
						t.Errorf("refusal %q does not name %q. Criterion 9: every refusal names the delivery id, "+
							"its status and channel, and the reason", msg, want)
					}
				}
				if after := f.fingerprint(t, ctx, id); after != before {
					t.Errorf("the refused row changed (fingerprint %s -> %s)", before, after)
				}
				if n := f.approvals(t, ctx, id); n != 0 {
					t.Errorf("a refused reject wrote %d approvals row(s); a refusal writes nothing", n)
				}
				if n := len(f.events(t, ctx, id)); n != 0 {
					t.Errorf("a refused reject wrote %d delivery_rejected event(s)", n)
				}
			})
		}
	}
}

// ---- criteria 9 (last two rows), 11 (upgrade, no-op) and D6 --------------------

func TestRejectDelivery_Integration_AlreadyRejectedRows(t *testing.T) {
	ctx := context.Background()
	f := newRJFixture(t, ctx)

	deny := func(t *testing.T, redraft bool, note string) int64 {
		t.Helper()
		id := f.row(t, ctx, rjSpec{channel: "gmail", status: "drafted"})
		if _, err := f.reject(ctx, rjActor, id, redraft, rjStr(note)); err != nil {
			t.Fatalf("first reject_delivery: %v", err)
		}
		return id
	}

	t.Run("plain rejected + Deny is a no-op success that writes nothing", func(t *testing.T) {
		id := deny(t, false, "not needed")
		before := f.fingerprint(t, ctx, id)
		res, err := f.reject(ctx, rjActor, id, false, nil)
		if err != nil {
			t.Fatalf("repeat Deny: %v (a replay must be a no-op success)", err)
		}
		if want := (rjResult{DeliveryID: id, Status: "rejected", Redraft: false, Changed: false}); res != want {
			t.Errorf("result = %+v, want %+v", res, want)
		}
		if after := f.fingerprint(t, ctx, id); after != before {
			t.Errorf("a no-op reject changed the row (updated_at included)")
		}
		if n := f.approvals(t, ctx, id); n != 1 {
			t.Errorf("approvals rows = %d after a no-op, want still 1", n)
		}
		if n := len(f.events(t, ctx, id)); n != 1 {
			t.Errorf("delivery_rejected events = %d after a no-op, want still 1", n)
		}
	})

	t.Run("plain rejected + Redo upgrades and replaces the note (D6)", func(t *testing.T) {
		id := deny(t, false, "mis-clicked Deny")
		res, err := f.reject(ctx, rjActor, id, true, rjStr("shorter, no apology"))
		if err != nil {
			t.Fatalf("Redo on a plain-rejected row: %v. D6: a mis-clicked Deny must be recoverable without psql", err)
		}
		if want := (rjResult{DeliveryID: id, Status: "rejected", Redraft: true, Changed: true}); res != want {
			t.Errorf("result = %+v, want %+v", res, want)
		}
		st := f.state(t, ctx, id)
		if !st.redraft {
			t.Errorf("redraft_requested_at still NULL after the upgrade")
		}
		if rjDeref(st.note) != "shorter, no apology" {
			t.Errorf("rejection_note = %q, want the upgrade's note to REPLACE the old one", rjDeref(st.note))
		}
		if n := f.approvals(t, ctx, id); n != 1 {
			t.Errorf("approvals rows = %d, want still 1: the upgrade writes no second approvals row (criterion 11)", n)
		}
		evs := f.events(t, ctx, id)
		if len(evs) != 2 {
			t.Fatalf("delivery_rejected events = %d, want 2 (the Deny, then ONE upgrade event)", len(evs))
		}
		if evs[1].Redraft == nil || !*evs[1].Redraft {
			t.Errorf("the upgrade event's redraft = %v, want true", evs[1].Redraft)
		}
	})

	t.Run("plain rejected + Redo without a note keeps the note", func(t *testing.T) {
		id := deny(t, false, "keep this reason")
		if _, err := f.reject(ctx, rjActor, id, true, nil); err != nil {
			t.Fatalf("Redo without a note: %v", err)
		}
		if st := f.state(t, ctx, id); rjDeref(st.note) != "keep this reason" || !st.redraft {
			t.Errorf("after a note-less upgrade: note=%q redraft=%v, want the old note kept and redraft set",
				rjDeref(st.note), st.redraft)
		}
	})

	t.Run("redraft requested + Deny refuses (D6)", func(t *testing.T) {
		id := deny(t, true, "redo it")
		before := f.fingerprint(t, ctx, id)
		_, err := f.reject(ctx, rjActor, id, false, nil)
		if err == nil {
			t.Fatalf("Deny withdrew a Redo. D6: the drafts worker may already have written the new row, so " +
				"'withdraw' would be a lie — deny the new draft instead")
		}
		for _, want := range []string{itoa(id), "rejected", "gmail", "new draft"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("D6 refusal %q does not name %q", err, want)
			}
		}
		if after := f.fingerprint(t, ctx, id); after != before {
			t.Errorf("the refused withdraw changed the row")
		}
	})

	t.Run("redraft requested + Redo is a no-op success", func(t *testing.T) {
		id := deny(t, true, "redo it")
		before := f.fingerprint(t, ctx, id)
		res, err := f.reject(ctx, rjActor, id, true, rjStr("a second reason"))
		if err != nil {
			t.Fatalf("repeat Redo: %v", err)
		}
		if want := (rjResult{DeliveryID: id, Status: "rejected", Redraft: true, Changed: false}); res != want {
			t.Errorf("result = %+v, want %+v", res, want)
		}
		if after := f.fingerprint(t, ctx, id); after != before {
			t.Errorf("a no-op Redo changed the row (the note included: the table says no-op)")
		}
		if n := len(f.events(t, ctx, id)); n != 1 {
			t.Errorf("delivery_rejected events = %d, want still 1", n)
		}
	})
}

// ---- criteria 8 and 10: the task under the lock --------------------------------

// Redo needs the work task in done_locally (D7): draft_delivery refuses any
// other status when the drafts worker sends expect_task_status, so a redraft
// flag anywhere else is impossible, not pending. A plain Deny needs nothing —
// including on a CLOSED task (criterion 8: cleaning up a stale draft is exactly
// what closed work needs).
func TestRejectDelivery_Integration_RedoNeedsDoneLocallyWork(t *testing.T) {
	ctx := context.Background()
	f := newRJFixture(t, ctx)

	for _, status := range []string{"delivered", "closed", "in_progress", "ready"} {
		status := status
		t.Run(status, func(t *testing.T) {
			taskID := f.task(t, ctx, status)
			id := f.row(t, ctx, rjSpec{taskID: taskID, channel: "gmail", status: "drafted"})
			before := f.fingerprint(t, ctx, id)

			_, err := f.reject(ctx, rjActor, id, true, rjStr("redo it"))
			if err == nil {
				t.Fatalf("Redo on a delivery whose task is %s succeeded. Criterion 10: a draft can only be written "+
					"for done_locally work, so the flag would never be honoured", status)
			}
			if !strings.Contains(err.Error(), "done_locally") {
				t.Errorf("refusal %q does not say why (a draft can only be written for done_locally work)", err)
			}
			if after := f.fingerprint(t, ctx, id); after != before {
				t.Errorf("the refused Redo changed the row")
			}

			res, err := f.reject(ctx, rjActor, id, false, rjStr("stale"))
			if err != nil {
				t.Fatalf("plain Deny on a delivery whose task is %s: %v (criterion 8 allows it)", status, err)
			}
			if !res.Changed {
				t.Errorf("plain Deny result = %+v, want changed", res)
			}

			// ANY transition that sets redraft_requested_at — the D6 upgrade too.
			if _, err := f.reject(ctx, rjActor, id, true, nil); err == nil {
				t.Errorf("the Deny->Redo upgrade succeeded on a %s task; criterion 10 covers every transition "+
					"that sets redraft_requested_at", status)
			}
			if st := f.state(t, ctx, id); st.redraft {
				t.Errorf("redraft_requested_at set on a delivery whose task is %s", status)
			}
		})
	}
}

// ---- criterion 5 (the handler half): defaults and the blank note ---------------

func TestRejectDelivery_Integration_DefaultsAndBlankNote(t *testing.T) {
	ctx := context.Background()
	f := newRJFixture(t, ctx)

	id := f.row(t, ctx, rjSpec{channel: "gmail", status: "drafted"})
	res, err := f.rejectRaw(ctx, rjActor, map[string]any{"delivery_id": id, "note": "  \n\t "})
	if err != nil {
		t.Fatalf("reject_delivery with no redraft key and a blank note: %v", err)
	}
	if res.Redraft {
		t.Errorf("result redraft = true with no redraft key; criterion 5 defaults it to false")
	}
	st := f.state(t, ctx, id)
	if st.redraft {
		t.Errorf("redraft_requested_at set with no redraft key")
	}
	if st.note != nil {
		t.Errorf("rejection_note = %q, want NULL: a whitespace-only note is stored as NULL (criterion 5)", *st.note)
	}
}

// ---- criterion 6 through the real matrix ---------------------------------------

func TestRejectDelivery_Integration_WorkerIsDeniedByPolicy(t *testing.T) {
	ctx := context.Background()
	f := newRJFixture(t, ctx)

	id := f.row(t, ctx, rjSpec{channel: "gmail", status: "drafted"})
	before := f.fingerprint(t, ctx, id)
	_, err := f.reject(ctx, rjWorkerActor, id, true, rjStr("a worker judging its own words"))
	if err == nil || !strings.Contains(err.Error(), "denied by policy") {
		t.Fatalf("reject_delivery by %s = %v, want a policy denial (humanOnly, D9)", rjWorkerActor, err)
	}
	var n int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM policy_decisions p JOIN audit_events a ON a.id=p.audit_event_id
		  WHERE a.actor=$1 AND p.tool='reject_delivery' AND p.decision='deny' AND p.rule='human_only'`,
		rjWorkerActor).Scan(&n); err != nil {
		t.Fatalf("read policy_decisions: %v", err)
	}
	if n != 1 {
		t.Errorf("deny/human_only policy_decisions rows for %s = %d, want 1", rjWorkerActor, n)
	}
	if after := f.fingerprint(t, ctx, id); after != before {
		t.Errorf("a denied reject changed the row")
	}
}

// ---- criterion 13: every verb toward the world refuses a rejected row ----------

// No send code changes in this ticket: every one of these already refuses
// through its status allowlist. These tests are what make that allowlist
// load-bearing — mutation: add `case status == "rejected":` to approveDelivery's
// switch and the approve rows go red.
func TestRejectDelivery_Integration_EverySendPathRefusesARejectedRow(t *testing.T) {
	ctx := context.Background()
	f := newRJFixture(t, ctx)

	gm := &fakeGmailSender{pool: f.pool}
	jr := &fakeJiraSender{pool: f.pool}
	sl := &fakeSlackSender{pool: f.pool}
	dr := &fakeSlackDrafter{}
	cb := &fakeCalendarBooker{pool: f.pool}
	tools.SetGmailSender(gm)
	tools.SetJiraSender(jr)
	tools.SetSlackSender(sl)
	tools.SetSlackDrafter(dr)
	tools.SetCalendarBooker(cb)
	calls := func() int { return gm.calls + jr.calls + sl.calls + dr.calls + cb.calls }

	for _, tc := range []struct{ tool, channel, extra string }{
		{"approve_delivery", "gmail", ""},
		{"approve_delivery", "slack_reply", ""},
		{"update_delivery", "gmail", `,"body":"edited after the verdict"`},
		{"send_delivery", "gmail", ""},
		{"send_delivery", "jira_comment", ""},
		{"send_delivery", "slack_reply", ""},
		{"send_delivery", "calendar", ""},
		{"book_calendar_block", "calendar", ""},
		{"prefill_delivery", "slack_reply", ""},
		{"mark_delivery_sent", "upwork_chat", ""},
		{"mark_delivery_sent", "slack_reply", ""},
		{"mark_delivery_sent", "slack_reply", `,"leaf_gated":true`},
		{"mark_delivery_failed", "slack_reply", ""},
	} {
		for _, redraft := range []bool{false, true} {
			tc, redraft := tc, redraft
			t.Run(fmt.Sprintf("%s/%s%s/redraft=%v", tc.tool, tc.channel, tc.extra, redraft), func(t *testing.T) {
				spec := rjSpec{channel: tc.channel, status: "rejected", note: rjStr("denied"), redraft: redraft}
				if redraft {
					// A row approved, then rejected, keeps approval_source='switchboard':
					// the send paths must refuse on STATUS, not on a missing gate.
					spec.approvalSource = "switchboard"
				}
				id := f.row(t, ctx, spec)
				before := f.fingerprint(t, ctx, id)
				n0 := calls()

				_, err := f.ex.Execute(ctx, executor.Call{Tool: tc.tool, Actor: rjActor,
					Args: []byte(`{"delivery_id":` + itoa(id) + tc.extra + `}`)})
				if err == nil {
					t.Fatalf("%s on a REJECTED %s row succeeded. A rejected row can never be approved, sent, "+
						"booked, prefilled or marked sent (criterion 13, invariant 4)", tc.tool, tc.channel)
				}
				if strings.Contains(err.Error(), "denied by policy") {
					t.Fatalf("%s was refused by POLICY (%v), not by its status allowlist; the allowlist is what "+
						"this test makes load-bearing", tc.tool, err)
				}
				if after := f.fingerprint(t, ctx, id); after != before {
					t.Errorf("%s changed the rejected row even though it refused", tc.tool)
				}
				if n := calls(); n != n0 {
					t.Errorf("%s reached a network seam (%d call(s)) for a rejected row", tc.tool, n-n0)
				}
			})
		}
	}
}

// ---- criterion 14: sequential races --------------------------------------------

// rjHookGmailSender runs a callback DURING the network call — send phase 1 has
// committed 'sending' and released its locks by then, which is exactly the
// window a reject would race.
type rjHookGmailSender struct {
	calls   int
	hook    func(ctx context.Context) error
	hookErr error
}

func (h *rjHookGmailSender) Send(ctx context.Context, _ string, _ []byte, _ string) (string, error) {
	h.calls++
	if h.hook != nil {
		h.hookErr = h.hook(ctx)
	}
	return "gmail-api-id-itest-deny", nil
}

func TestRejectDelivery_Integration_SequentialRaces(t *testing.T) {
	ctx := context.Background()
	f := newRJFixture(t, ctx)
	sendTo := func(id int64) error {
		_, err := f.ex.Execute(ctx, executor.Call{Tool: "send_delivery", Actor: rjActor,
			Args: []byte(`{"delivery_id":` + itoa(id) + `}`)})
		return err
	}
	approveTo := func(id int64) error {
		_, err := f.ex.Execute(ctx, executor.Call{Tool: "approve_delivery", Actor: rjActor,
			Args: []byte(`{"delivery_id":` + itoa(id) + `}`)})
		return err
	}

	t.Run("reject then send: the send refuses", func(t *testing.T) {
		gm := &fakeGmailSender{pool: f.pool}
		tools.SetGmailSender(gm)
		id := f.row(t, ctx, rjSpec{channel: "gmail", status: "drafted"})
		if _, err := f.reject(ctx, rjActor, id, false, nil); err != nil {
			t.Fatalf("reject: %v", err)
		}
		if err := approveTo(id); err == nil {
			t.Errorf("approve_delivery after the reject succeeded")
		}
		if err := sendTo(id); err == nil {
			t.Errorf("send_delivery after the reject succeeded")
		}
		if gm.calls != 0 {
			t.Errorf("the gmail adapter was called %d time(s) for a rejected row", gm.calls)
		}
		if st := f.state(t, ctx, id); st.status != "rejected" {
			t.Errorf("status = %q, want rejected", st.status)
		}
	})

	t.Run("send phase 1 committed, then reject: the reject refuses", func(t *testing.T) {
		id := f.row(t, ctx, rjSpec{channel: "gmail", status: "approved", approvalSource: "switchboard"})
		hook := &rjHookGmailSender{}
		hook.hook = func(ctx context.Context) error {
			_, err := f.reject(ctx, rjActor, id, false, rjStr("too late"))
			return err
		}
		tools.SetGmailSender(hook)
		t.Cleanup(func() { tools.SetGmailSender(&fakeGmailSender{pool: f.pool}) })

		if err := sendTo(id); err != nil {
			t.Fatalf("send_delivery: %v", err)
		}
		if hook.calls != 1 {
			t.Fatalf("gmail adapter calls = %d, want 1 (the fixture must reach phase 2)", hook.calls)
		}
		if hook.hookErr == nil {
			t.Fatalf("reject_delivery succeeded while the row was 'sending' — the words were already on the " +
				"wire. Criterion 14: sending/sent are never rejectable")
		}
		if !strings.Contains(hook.hookErr.Error(), "sending") {
			t.Errorf("the in-flight refusal %q does not name the status 'sending'", hook.hookErr)
		}
		if st := f.state(t, ctx, id); st.status != "sent" {
			t.Errorf("status after the send = %q, want sent", st.status)
		}
		if n := f.approvals(t, ctx, id); n != 0 {
			t.Errorf("a rejected approvals row was written for a sent delivery")
		}
		if n := len(f.events(t, ctx, id)); n != 0 {
			t.Errorf("a delivery_rejected event was written for a sent delivery")
		}
	})

	t.Run("approve then reject: the reject succeeds and a following send refuses", func(t *testing.T) {
		gm := &fakeGmailSender{pool: f.pool}
		tools.SetGmailSender(gm)
		id := f.row(t, ctx, rjSpec{channel: "gmail", status: "drafted"})
		if err := approveTo(id); err != nil {
			t.Fatalf("approve_delivery: %v", err)
		}
		res, err := f.reject(ctx, rjActor, id, false, rjStr("changed my mind"))
		if err != nil {
			t.Fatalf("reject after approve: %v (approved is in the rejectable set)", err)
		}
		if !res.Changed {
			t.Errorf("result = %+v, want changed", res)
		}
		if err := sendTo(id); err == nil {
			t.Errorf("send_delivery after approve->reject succeeded")
		}
		if gm.calls != 0 {
			t.Errorf("the gmail adapter was called for a rejected row")
		}
	})
}

// ---- criterion 17: the schema backstop -----------------------------------------

func rjWantCheck(t *testing.T, err error, constraint, what string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Errorf("%s: err = %v, want a check violation on %s", what, err, constraint)
		return
	}
	if pgErr.Code != "23514" || pgErr.ConstraintName != constraint {
		t.Errorf("%s: SQLSTATE %s on %q, want 23514 (check_violation) on %s", what, pgErr.Code, pgErr.ConstraintName, constraint)
	}
}

func TestRejectDelivery_Integration_SchemaForbidsASendOnARejectedRow(t *testing.T) {
	ctx := context.Background()
	f := newRJFixture(t, ctx)

	rejected := f.row(t, ctx, rjSpec{channel: "gmail", status: "rejected", note: rjStr("no")})
	for _, stmt := range []string{
		`UPDATE deliveries SET confirmed_at=now() WHERE id=$1`,
		`UPDATE deliveries SET sent_external_id='<sb-itest-deny-backstop@example.com>' WHERE id=$1`,
	} {
		_, err := f.pool.Exec(ctx, stmt, rejected)
		rjWantCheck(t, err, "deliveries_rejected_unsent_check", stmt)
	}

	drafted := f.row(t, ctx, rjSpec{channel: "gmail", status: "drafted"})
	for _, stmt := range []string{
		`UPDATE deliveries SET rejection_note='stray' WHERE id=$1`,
		`UPDATE deliveries SET redraft_requested_at=now() WHERE id=$1`,
	} {
		_, err := f.pool.Exec(ctx, stmt, drafted)
		rjWantCheck(t, err, "deliveries_rejection_fields_check", stmt)
	}
}

// ---- criterion 35: the label query, verbatim -----------------------------------

const rjLabelQuery = `SELECT d.id, d.channel, d.created_by, d.body, d.rejection_note,
       (d.redraft_requested_at IS NOT NULL) AS redraft,
       a.decided_by, a.decided_at
  FROM deliveries d
  JOIN approvals a ON a.subject_type = 'delivery' AND a.subject_id = d.id
                  AND a.status = 'rejected'
 WHERE d.status = 'rejected'
 ORDER BY a.decided_at;`

func TestRejectDelivery_Integration_LabelQueryReturnsBothVerdicts(t *testing.T) {
	ctx := context.Background()
	f := newRJFixture(t, ctx)

	denied := f.row(t, ctx, rjSpec{channel: "gmail", status: "drafted", body: "the unwanted draft"})
	redone := f.row(t, ctx, rjSpec{channel: "gmail", status: "drafted", body: "the wrong-words draft"})
	if _, err := f.reject(ctx, rjActor, denied, false, rjStr("not needed")); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	if _, err := f.reject(ctx, rjActor, redone, true, rjStr("too long")); err != nil {
		t.Fatalf("Redo: %v", err)
	}

	rows, err := f.pool.Query(ctx, rjLabelQuery)
	if err != nil {
		t.Fatalf("the SPEC's label query does not run: %v", err)
	}
	defer rows.Close()
	type label struct {
		channel, decidedBy string
		createdBy, body    *string
		note               *string
		redraft            bool
	}
	got := map[int64]label{}
	for rows.Next() {
		var id int64
		var l label
		var decidedAt time.Time
		if err := rows.Scan(&id, &l.channel, &l.createdBy, &l.body, &l.note, &l.redraft, &l.decidedBy, &decidedAt); err != nil {
			t.Fatalf("scan label row: %v", err)
		}
		if id == denied || id == redone {
			got[id] = l
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate label rows: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("label query returned %d of the two verdicts; want both (criterion 35)", len(got))
	}
	for id, want := range map[int64]struct {
		redraft bool
		note    string
		body    string
	}{denied: {false, "not needed", "the unwanted draft"}, redone: {true, "too long", "the wrong-words draft"}} {
		l := got[id]
		if l.redraft != want.redraft {
			t.Errorf("delivery %d redraft = %v, want %v", id, l.redraft, want.redraft)
		}
		if rjDeref(l.note) != want.note || rjDeref(l.body) != want.body {
			t.Errorf("delivery %d note/body = %q/%q, want %q/%q", id, rjDeref(l.note), rjDeref(l.body), want.note, want.body)
		}
		if !strings.HasPrefix(l.decidedBy, "dashboard:") || l.decidedBy != rjActor {
			t.Errorf("delivery %d decided_by = %q, want %q", id, l.decidedBy, rjActor)
		}
		if rjDeref(l.createdBy) != "itest-deny" || l.channel != "gmail" {
			t.Errorf("delivery %d created_by/channel = %q/%q", id, rjDeref(l.createdBy), l.channel)
		}
	}
}
