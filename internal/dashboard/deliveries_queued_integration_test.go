//go:build integration

package dashboard_test

// slack-send-queue (SWT-76) Part 6 — the dashboard: criteria 30, 31 and 32.
//
// Against the REAL deliveries page (dev auth, the production policy matrix, a
// real Postgres), because both facts the page needs are COLUMNS: dropping
// send_queued_at from listDeliveries' SELECT must turn this red, and only a
// database can prove that (IK: test the column, not the fixture). The Slack
// bridge is an injected fake — never the mini, never a browser.
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_sendqueue?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run DashboardQueued ./internal/dashboard/
//
// ---------------------------------------------------------------------------
// GREENFIELD NOTE — compile-FAILs today: fakeQueuedSlackSender implements the
// NEW tools.SlackSender seam (four arguments, returning slackweb.SendOutcome),
// which does not exist yet. Once it compiles, every assertion below is red: the
// page has no "queued on the bridge" label, renders "Not in Slack" on every
// `sending` slack_reply row, and the flash reads "send_delivery ok".
//
// IMPOSED (behaviour, not field names — the assertions read RENDERED HTML so
// the implementer picks the spelling): listDeliveries' SELECT and deliveryRow
// gain send_queued_at and send_queue_job_id, plus whatever derived flag the
// template needs for "is the lease still holding" — a Go template cannot do
// time arithmetic, so the 15-minute comparison belongs in server.go beside the
// query, not in the HTML.
//
// D5's asymmetry, which is the whole point of criterion 30 and is easy to get
// backwards: RECORDING a send that happened is always safe, so "It's in Slack"
// stays. DECLARING that one did not happen while a click may still be pending
// is not, so "Not in Slack" is not offered while the lease holds — the verb
// would refuse anyway (delivery.go:2195-2205), and a button that errors teaches
// Salvador to distrust the page.
//
// This suite owns 'itest-dash-sq-%' and tdashq1@slack-web.local. Rerunnable;
// refuses 192.168.50.49 (dashGuard).
//
// MUTATION MAP:
//	Render "Not in Slack" on a queued row inside the lease -> criterion 30
//	Drop send_queued_at from listDeliveries' SELECT        -> criteria 30, 31
//	Report the queued send as an error in the flash        -> criterion 32

import (
	"context"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	dsqSlug   = "itest-dash-sq-proj"
	dsqAcct   = "tdashq1@slack-web.local"
	dsqTarget = "https://app.slack.com/client/TDASHQ1/CDASHQ1"
	dsqJobID  = "send-dash-91af"

	queuedLabel  = "queued on the bridge"
	inSlackBtn   = "It's in Slack"
	notInSlackBt = "Not in Slack"
)

// fakeQueuedSlackSender answers every send with the leaf's 202.
type fakeQueuedSlackSender struct{ calls int }

func (f *fakeQueuedSlackSender) Send(ctx context.Context, targetURL, text string, maxQueue time.Duration) (slackweb.SendOutcome, error) {
	f.calls++
	return slackweb.SendOutcome{
		Queued: true, JobID: dsqJobID, QueuedAt: time.Now().UTC(), ExpiresIn: 10 * time.Minute,
	}, nil
}

func cleanupDsq(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const tasks = `(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + dsqSlug + `'))`
	for _, q := range []string{
		`UPDATE ops_flags SET value='{"frozen": false}' WHERE name='sending_frozen'`,
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE actor LIKE 'dashboard:%' AND tool='send_delivery'
		     AND args->>'delivery_id' IN (SELECT id::text FROM deliveries WHERE task_id IN ` + tasks + `))`,
		`DELETE FROM audit_events WHERE tool='send_delivery'
		   AND args->>'delivery_id' IN (SELECT id::text FROM deliveries WHERE task_id IN ` + tasks + `)`,
		`DELETE FROM approvals WHERE subject_type='delivery'
		   AND subject_id IN (SELECT id FROM deliveries WHERE task_id IN ` + tasks + `)`,
		`DELETE FROM task_events WHERE task_id IN ` + tasks,
		`DELETE FROM deliveries WHERE task_id IN ` + tasks,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + dsqSlug + `')`,
		`DELETE FROM projects WHERE slug='` + dsqSlug + `'`,
		`DELETE FROM source_accounts WHERE provider='slack_web' AND account_email='` + dsqAcct + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func seedDsqTask(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var projID, taskID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,'itest-dash-sq-client','manual','dashboard','/tmp/itest','any') RETURNING id`,
		dsqSlug).Scan(&projID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'SQ dashboard work','human','done_locally') RETURNING id`, projID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return taskID
}

// ---- criteria 30 + 31: what the row renders -----------------------------------

func TestDashboardQueued_Integration_QueuedRowLabelAndButtons(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupDsq(t, ctx, pool)
	defer cleanupDsq(t, ctx, pool)
	taskID := seedDsqTask(t, ctx, pool)

	// Two `sending` slack_reply rows that differ in ONE column: the queued one
	// and today's ordinary stuck one.
	var queuedID, plainID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, approval_source,
		                         send_attempted_at, send_settled_at, send_queued_at, send_queue_job_id)
		 VALUES ($1,'slack_reply',$2,'queued while the rotation ran','sending','switchboard',
		         now() - interval '2 minutes', NULL, now() - interval '2 minutes', $3) RETURNING id`,
		taskID, dsqTarget, dsqJobID).Scan(&queuedID); err != nil {
		t.Fatalf("seed queued delivery (apply migration 0042): %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, approval_source,
		                         send_attempted_at, send_settled_at)
		 VALUES ($1,'slack_reply',$2,'an ordinary ambiguous send','sending','switchboard',
		         now() - interval '2 minutes', now() - interval '2 minutes') RETURNING id`,
		taskID, dsqTarget).Scan(&plainID); err != nil {
		t.Fatalf("seed plain sending delivery: %v", err)
	}

	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	code, page := get(t, client, ts.URL+"/deliveries")
	if code != 200 {
		t.Fatalf("GET /deliveries = %d\n%s", code, snippet(page))
	}

	// Criterion 31, first half: the page still renders with the new columns in
	// play, including for the row where both are NULL.
	q := rowOf(t, page, queuedID)
	p := rowOf(t, page, plainID)

	if !strings.Contains(strings.ToLower(q), queuedLabel) {
		t.Errorf("the queued row does not say %q. Salvador's whole complaint was that an approved reply "+
			"vanished into `failed`; the row has to SAY what happened to it (criterion 30). Row: %s",
			queuedLabel, q)
	}
	if !strings.Contains(q, dsqJobID) {
		t.Errorf("the queued row does not show its job id %q — it is the only handle tying this row to the "+
			"mini's log line. Row: %s", dsqJobID, q)
	}
	if !strings.Contains(q, inSlackBtn) {
		t.Errorf("the queued row lost %q; RECORDING a send that happened is always safe and must stay "+
			"available throughout (D5's asymmetry). Row: %s", inSlackBtn, q)
	}
	if strings.Contains(q, notInSlackBt) {
		t.Errorf("the queued row offers %q INSIDE the lease. mark_delivery_failed refuses an unsettled attempt "+
			"younger than 15 minutes (delivery.go:2195-2205), so the button can only error — and if it ever "+
			"stopped erroring, it would make a pending click re-approvable and double-post (criterion 30). "+
			"Row: %s", notInSlackBt, q)
	}

	// The control: a `sending` row with send_queued_at NULL renders exactly as
	// today. Without this the test above passes for a page that suppressed the
	// button everywhere.
	if strings.Contains(strings.ToLower(p), queuedLabel) {
		t.Errorf("a NON-queued sending row is labelled %q. Row: %s", queuedLabel, p)
	}
	for _, want := range []string{inSlackBtn, notInSlackBt} {
		if !strings.Contains(p, want) {
			t.Errorf("a sending row with send_queued_at NULL lost %q; D11 says nothing else about the page "+
				"changes. Row: %s", want, p)
		}
	}

	// After the lease both buttons render again: the leaf can no longer click
	// (enqueue + 10 min TTL + ~30 s < 15 min), so a human who looked in Slack
	// must be able to resolve it — the SECOND of D6's two horizons.
	if _, err := pool.Exec(ctx,
		`UPDATE deliveries SET send_attempted_at = now() - interval '16 minutes',
		        send_queued_at = now() - interval '16 minutes' WHERE id=$1`, queuedID); err != nil {
		t.Fatalf("age the queued row past the lease: %v", err)
	}
	_, page = get(t, client, ts.URL+"/deliveries")
	aged := rowOf(t, page, queuedID)
	if !strings.Contains(aged, notInSlackBt) {
		t.Errorf("after the lease the queued row still hides %q; the lease is the FIRST horizon and a human "+
			"resolves it there (D6). Row: %s", notInSlackBt, aged)
	}
	if !strings.Contains(strings.ToLower(aged), queuedLabel) {
		t.Errorf("the queued row lost its label once the lease expired; it is still a queued row and the "+
			"history matters to whoever resolves it. Row: %s", aged)
	}
}

// Criterion 31, second half: the page is one query, and it must render with the
// new columns NULL for EVERY row — the state of production the moment 0042 is
// applied and before any send is queued. A widened failure mode here takes the
// whole page down, not one row.
func TestDashboardQueued_Integration_PageRendersWithBothColumnsNull(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupDsq(t, ctx, pool)
	defer cleanupDsq(t, ctx, pool)
	taskID := seedDsqTask(t, ctx, pool)

	var ids []int64
	for _, row := range []struct{ status, channel string }{
		{"drafted", "slack_reply"}, {"approved", "slack_reply"}, {"sending", "slack_reply"},
		{"sent", "slack_reply"}, {"failed", "slack_reply"},
	} {
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO deliveries (task_id, channel, target_ref, body, status, approval_source)
			 VALUES ($1,$2,$3,'nothing was ever queued',$4,'switchboard') RETURNING id`,
			taskID, row.channel, dsqTarget, row.status).Scan(&id); err != nil {
			t.Fatalf("seed %s delivery: %v", row.status, err)
		}
		ids = append(ids, id)
	}

	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	code, page := get(t, client, ts.URL+"/deliveries")
	if code != 200 {
		t.Fatalf("GET /deliveries = %d with both new columns NULL everywhere: a nullable column scanned into "+
			"a non-pointer is how one NULL takes the whole page down\n%s", code, snippet(page))
	}
	for _, id := range ids {
		row := rowOf(t, page, id) // fails the test if the row is missing
		if strings.Contains(strings.ToLower(row), queuedLabel) {
			t.Errorf("delivery %d has both columns NULL and is still labelled %q. Row: %s", id, queuedLabel, row)
		}
	}
}

// ---- criterion 32: the flash ---------------------------------------------------

// The line Salvador reads the moment he presses Send during a rotation. Today
// it would say "slack send rejected (503)"; it must say the send was QUEUED and
// name the job id, because executeTo's default ("<tool> ok") tells him nothing
// about why the row is still sitting in `sending`.
func TestDashboardQueued_Integration_FlashSaysQueuedAndNamesTheJob(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupDsq(t, ctx, pool)
	defer cleanupDsq(t, ctx, pool)
	taskID := seedDsqTask(t, ctx, pool)

	if _, err := pool.Exec(ctx,
		`INSERT INTO source_accounts (provider, account_email, domain_default, scopes, send_enabled, calendar_in_availability)
		 VALUES ('slack_web',$1,'https://app.slack.com/client/TDASHQ1',ARRAY['CDASHQ1'],true,false)`,
		dsqAcct); err != nil {
		t.Fatalf("seed slack_web account: %v", err)
	}
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, approval_source)
		 VALUES ($1,'slack_reply',$2,'the reply he approves mid-rotation','approved','switchboard')
		 RETURNING id`, taskID, dsqTarget).Scan(&id); err != nil {
		t.Fatalf("seed approved delivery: %v", err)
	}

	fake := &fakeQueuedSlackSender{}
	tools.SetSlackSender(fake)
	defer tools.SetSlackSender(nil)

	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	resp, err := client.PostForm(ts.URL+"/deliveries/"+strconv.FormatInt(id, 10)+"/send", url.Values{})
	if err != nil {
		t.Fatalf("POST send: %v", err)
	}
	// The client follows the redirect, so this body IS the reloaded page.
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(raw)
	if fake.calls != 1 {
		t.Fatalf("bridge sends = %d, want 1", fake.calls)
	}

	// Read the flash element itself, not the whole page: the page also lists
	// rows and status filters, and "failed" appears there legitimately.
	m := regexp.MustCompile(`(?s)<div class="flash">(.*?)</div>`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no flash rendered after the send:\n%s", snippet(page))
	}
	flash := m[1]
	low := strings.ToLower(flash)
	if !strings.Contains(low, "queued") {
		t.Errorf("the flash after a queued send does not say it was queued: %q. Today it would read "+
			"\"slack send rejected (503)\"; executeTo's default reads \"send_delivery ok\", and neither "+
			"explains why the row is still sitting in `sending` (criterion 32)", flash)
	}
	if !strings.Contains(flash, dsqJobID) {
		t.Errorf("the flash does not name the job id %q; without it there is nothing to grep the mini's log "+
			"for when the message does not appear (criterion 32). Flash: %q", dsqJobID, flash)
	}
	for _, bad := range []string{"rejected", "error", "failed"} {
		if strings.Contains(low, bad) {
			t.Errorf("the flash after a SUCCESSFUL queued send reads like a failure (contains %q): %q. It is "+
				"not a failure — the leaf accepted the send and will click it in the next gap", bad, flash)
		}
	}

	var status string
	var queuedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT status, send_queued_at FROM deliveries WHERE id=$1`, id).
		Scan(&status, &queuedAt); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if status != "sending" || queuedAt == nil {
		t.Errorf("after the dashboard send the row is status=%q send_queued_at=%v, want sending + stamped",
			status, queuedAt)
	}
}
