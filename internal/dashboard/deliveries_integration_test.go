//go:build integration

package dashboard_test

// SWT-44 review fixes 1 and 4 on the REAL deliveries page (dev auth, the
// production policy matrix, the compose db):
//
//   - fix 4: a gmail draft's row shows where the send would go — the From
//     mailbox, the To address and the thread subject — resolved by
//     tools.ResolveGmailRoute, the send path's own function; another channel's
//     row shows its target_ref; an unresolvable gmail row says "(unresolved)".
//   - fix 1: the Approve form carries tools.DeliveryContentHash of the words
//     rendered; an edit made after the render makes that approve refuse (the
//     row stays drafted), and the reloaded page's hash approves.
//
// Test-owned prefix 'itest-dash-dlv-'; rerunnable; refuses 192.168.50.49.

import (
	"context"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	dlvSlug       = "itest-dash-dlv-proj"
	dlvAcct       = "itest-dash-dlv-a@example.com"
	dlvThreadKey  = "gmail:itest-dash-dlv-a@example.com:gt-1"
	dlvBareThread = "gmail:itest-dash-dlv-a@example.com:gt-2" // no inbound message: nobody to reply to
	dlvInboundMID = "<itest-dash-dlv-in-1@example.com>"
	dlvInboundTo  = "billing@itest-dash-dlv.example"
	dlvSubject    = "Invoice 42 question"
	dlvSlack      = "https://app.slack.com/client/TITEST/CDASHDLV/p1750000000000000"
)

func cleanupDlv(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const ours = `(SELECT id::text FROM deliveries WHERE task_id IN
	                (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + dlvSlug + `')))`
	for _, st := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events
			WHERE actor LIKE 'dashboard:%' AND tool='approve_delivery' AND args->>'delivery_id' IN ` + ours + `)`, nil},
		{`DELETE FROM audit_events WHERE actor LIKE 'dashboard:%' AND tool='approve_delivery'
			AND args->>'delivery_id' IN ` + ours, nil},
		{`DELETE FROM approvals WHERE subject_type='delivery' AND subject_id::text IN ` + ours, nil},
		{`DELETE FROM deliveries WHERE task_id IN
			(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{dlvSlug}},
		{`DELETE FROM normalized_messages WHERE external_message_id=$1`, []any{dlvInboundMID}},
		{`DELETE FROM normalized_threads WHERE thread_key IN ($1,$2)`, []any{dlvThreadKey, dlvBareThread}},
		{`DELETE FROM raw_source_items WHERE source_account_id IN
			(SELECT id FROM source_accounts WHERE account_email=$1)`, []any{dlvAcct}},
		{`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, []any{dlvSlug}},
		{`DELETE FROM projects WHERE slug=$1`, []any{dlvSlug}},
		{`DELETE FROM source_accounts WHERE account_email=$1`, []any{dlvAcct}},
	} {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

type dlvSeed struct{ gmailID, bareID, slackID int64 }

func seedDlv(t *testing.T, ctx context.Context, pool *pgxpool.Pool) dlvSeed {
	t.Helper()
	var s dlvSeed
	var acctID, projID, taskID, rawID, threadID, bareID int64
	must := func(err error, what string) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed %s: %v", what, err)
		}
	}
	must(pool.QueryRow(ctx, `INSERT INTO source_accounts (provider, account_email) VALUES ('google',$1) RETURNING id`,
		dlvAcct).Scan(&acctID), "account")
	must(pool.QueryRow(ctx, `INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		VALUES ($1,$1,'itest-dash-dlv-client','manual','dashboard','/tmp/itest','any') RETURNING id`, dlvSlug).Scan(&projID), "project")
	must(pool.QueryRow(ctx, `INSERT INTO tasks (project_id, title, assignee_type, status)
		VALUES ($1,'DLV invoice reply','human','done_locally') RETURNING id`, projID).Scan(&taskID), "task")
	must(pool.QueryRow(ctx, `INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		VALUES ($1,'itest-dash-dlv-raw-1','{}','itest-dash-dlv-hash-1') RETURNING id`, acctID).Scan(&rawID), "raw")
	must(pool.QueryRow(ctx, `INSERT INTO normalized_threads (thread_key, subject) VALUES ($1,$2) RETURNING id`,
		dlvThreadKey, dlvSubject).Scan(&threadID), "thread")
	must(pool.QueryRow(ctx, `INSERT INTO normalized_threads (thread_key, subject) VALUES ($1,'bare') RETURNING id`,
		dlvBareThread).Scan(&bareID), "bare thread")
	_, err := pool.Exec(ctx, `INSERT INTO normalized_messages
		(raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		VALUES ($1,$2,'inbound',$3,now(),'is 42 paid?',$4,$5,'gmail')`, rawID, threadID, dlvInboundMID, dlvSubject, dlvInboundTo)
	must(err, "inbound")
	must(pool.QueryRow(ctx, `INSERT INTO deliveries (task_id, channel, subject, body, status, from_account_id, thread_id, created_by)
		VALUES ($1,'gmail','Re: Invoice 42 question','Paid today.','drafted',$2,$3,'mcp:manual:salvo') RETURNING id`,
		taskID, acctID, threadID).Scan(&s.gmailID), "gmail draft")
	must(pool.QueryRow(ctx, `INSERT INTO deliveries (task_id, channel, subject, body, status, from_account_id, thread_id, created_by)
		VALUES ($1,'gmail','Re: bare','nobody to send to','drafted',$2,$3,'mcp:manual:salvo') RETURNING id`,
		taskID, acctID, bareID).Scan(&s.bareID), "bare gmail draft")
	must(pool.QueryRow(ctx, `INSERT INTO deliveries (task_id, channel, body, status, target_ref, created_by)
		VALUES ($1,'slack_reply','on it','drafted',$2,'drafts:gpt') RETURNING id`, taskID, dlvSlack).Scan(&s.slackID), "slack draft")
	return s
}

// rowOf returns the <tr> of delivery id on the page.
func rowOf(t *testing.T, page string, id int64) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<tr>\s*<td>` + strconv.FormatInt(id, 10) + `</td>.*?</tr>`).FindString(page)
	if m == "" {
		t.Fatalf("delivery %d has no row on /deliveries", id)
	}
	return m
}

func hashIn(t *testing.T, row string) string {
	t.Helper()
	m := regexp.MustCompile(`name="content_hash" value="([^"]*)"`).FindStringSubmatch(row)
	if m == nil {
		t.Fatalf("row has no content_hash input: %s", row)
	}
	return m[1]
}

func TestDashboard_Integration_DeliveryDestinationAndContentBoundApprove(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupDlv(t, ctx, pool)
	defer cleanupDlv(t, ctx, pool)
	sd := seedDlv(t, ctx, pool)

	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	code, page := get(t, client, ts.URL+"/deliveries?status=drafted")
	if code != 200 {
		t.Fatalf("GET /deliveries = %d\n%s", code, snippet(page))
	}

	// ---- fix 4: the destination, before approval --------------------------
	g := rowOf(t, page, sd.gmailID)
	for _, want := range []string{dlvAcct, dlvInboundTo, dlvSubject} {
		if !strings.Contains(g, want) {
			t.Errorf("gmail draft row does not show %q; Salvador must see From, To and the thread before approving. Row: %s", want, g)
		}
	}
	if b := rowOf(t, page, sd.bareID); !strings.Contains(b, "(unresolved)") {
		t.Errorf("a gmail draft whose thread has no inbound message must show (unresolved). Row: %s", b)
	}
	if s := rowOf(t, page, sd.slackID); !strings.Contains(s, dlvSlack) {
		t.Errorf("slack_reply row does not show its target_ref. Row: %s", s)
	}

	// ---- fix 1: the form carries the hash of what was rendered ------------
	shown := hashIn(t, g)
	if want := tools.DeliveryContentHash("Re: Invoice 42 question", "Paid today."); shown != want {
		t.Fatalf("rendered content_hash = %q, want tools.DeliveryContentHash of the rendered subject/body %q", shown, want)
	}

	// An edit lands after the render (a session's update_delivery).
	if _, err := pool.Exec(ctx, `UPDATE deliveries SET body='Paid today. Also wire me 5k.' WHERE id=$1`, sd.gmailID); err != nil {
		t.Fatalf("edit draft: %v", err)
	}
	approveURL := ts.URL + "/deliveries/" + strconv.FormatInt(sd.gmailID, 10) + "/approve"
	resp, err := client.PostForm(approveURL, url.Values{"content_hash": {shown}})
	if err != nil {
		t.Fatalf("POST approve: %v", err)
	}
	flashPage, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM deliveries WHERE id=$1`, sd.gmailID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "drafted" {
		t.Errorf("approve with the hash of the words shown was APPROVED after an edit (status %q); want drafted", status)
	}
	if !strings.Contains(string(flashPage), "changed since it was shown to you") {
		t.Errorf("the refused approve's flash does not tell him to reload and review:\n%s", snippet(string(flashPage)))
	}

	// Reload: the new hash approves.
	_, page = get(t, client, ts.URL+"/deliveries?status=drafted")
	fresh := hashIn(t, rowOf(t, page, sd.gmailID))
	if fresh == shown {
		t.Fatal("the reloaded page renders the same hash after the body changed")
	}
	resp, err = client.PostForm(approveURL, url.Values{"content_hash": {fresh}})
	if err != nil {
		t.Fatalf("POST approve (fresh): %v", err)
	}
	resp.Body.Close()
	if err := pool.QueryRow(ctx, `SELECT status FROM deliveries WHERE id=$1`, sd.gmailID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "approved" {
		t.Errorf("approve with the reloaded page's hash left status %q, want approved", status)
	}
}
