//go:build integration

package dashboard_test

// gmail-delivery-cc (SWT-69) criteria 19 and 20 on the REAL deliveries page
// (dev auth, the production policy matrix, a scratch database):
//
//   - every gmail row's destination cell reads Cc beside From/To — INCLUDING a
//     `sent` row, because unlike From/To this is a STORED fact, not a
//     re-resolution (the "a sent row's route is not recomputed" rule,
//     server.go:186-190, does not apply to it);
//   - a drafted row's edit form carries a `cc` input PRE-FILLED with the
//     current list; saving it with content sets the list, saving it EMPTY
//     CLEARS it (D10), and an invalid address comes back as the handler's
//     flash rather than a silent no-op.
//
// D10 is the load-bearing half: actionEdit forwards body/subject only when
// non-empty (the SWT-61 residual, where clearing the subject box silently keeps
// the old subject). For a RECIPIENT list that behaviour is worse — Salvador
// deletes Katie from the box, sees the save succeed, and sends to her anyway —
// so cc is forwarded whenever the POST CONTAINS THE KEY.
//
// ISOLATED scratch database only (never the shared compose `ops`, never prod):
//
//	psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c 'CREATE DATABASE ops_swt69'
//	make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_swt69?sslmode=disable'
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_swt69?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run DeliveryCc ./internal/dashboard/
//
// Test-owned prefix 'itest-dash-cc-'; rerunnable; refuses 192.168.50.49.
//
// EXPECTED RED: deliveries.cc does not exist (the seed INSERT fails), the
// template renders no Cc and the form has no cc input.

import (
	"context"
	"io"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	ccSlug      = "itest-dash-cc-proj"
	ccAcct      = "itest-dash-cc-a@example.com"
	ccThreadKey = "gmail:itest-dash-cc-a@example.com:gt-1"
	ccInboundID = "<itest-dash-cc-in-1@example.com>"
	ccInboundTo = "client@itest-dash-cc.example"
	ccKatie     = "kevans@cecollaboratory.com"
	ccBilling   = "billing@itest-dash-cc.example"
)

func cleanupDlvCc(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const ours = `(SELECT id::text FROM deliveries WHERE task_id IN
	                (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + ccSlug + `')))`
	for _, st := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events
			WHERE actor LIKE 'dashboard:%' AND tool IN ('update_delivery','approve_delivery')
			  AND args->>'delivery_id' IN ` + ours + `)`, nil},
		{`DELETE FROM audit_events WHERE actor LIKE 'dashboard:%' AND tool IN ('update_delivery','approve_delivery')
			AND args->>'delivery_id' IN ` + ours, nil},
		{`DELETE FROM approvals WHERE subject_type='delivery' AND subject_id::text IN ` + ours, nil},
		{`DELETE FROM task_events WHERE task_id IN
			(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{ccSlug}},
		{`DELETE FROM deliveries WHERE task_id IN
			(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{ccSlug}},
		{`DELETE FROM normalized_messages WHERE external_message_id=$1`, []any{ccInboundID}},
		{`DELETE FROM normalized_threads WHERE thread_key=$1`, []any{ccThreadKey}},
		{`DELETE FROM raw_source_items WHERE source_account_id IN
			(SELECT id FROM source_accounts WHERE account_email=$1)`, []any{ccAcct}},
		{`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, []any{ccSlug}},
		{`DELETE FROM projects WHERE slug=$1`, []any{ccSlug}},
		{`DELETE FROM source_accounts WHERE account_email=$1`, []any{ccAcct}},
	} {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

type ccSeed struct{ draftedID, sentID int64 }

func seedDlvCc(t *testing.T, ctx context.Context, pool *pgxpool.Pool) ccSeed {
	t.Helper()
	var s ccSeed
	var acctID, projID, taskID, rawID, threadID int64
	must := func(err error, what string) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed %s: %v", what, err)
		}
	}
	must(pool.QueryRow(ctx, `INSERT INTO source_accounts (provider, account_email, send_enabled)
		VALUES ('google',$1,true) RETURNING id`, ccAcct).Scan(&acctID), "account")
	must(pool.QueryRow(ctx, `INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		VALUES ($1,$1,'itest-dash-cc-client','manual','dashboard','/tmp/itest','any') RETURNING id`,
		ccSlug).Scan(&projID), "project")
	must(pool.QueryRow(ctx, `INSERT INTO tasks (project_id, title, assignee_type, status)
		VALUES ($1,'CC Rochester reply','human','done_locally') RETURNING id`, projID).Scan(&taskID), "task")
	must(pool.QueryRow(ctx, `INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		VALUES ($1,'itest-dash-cc-raw-1','{}','itest-dash-cc-hash-1') RETURNING id`, acctID).Scan(&rawID), "raw")
	must(pool.QueryRow(ctx, `INSERT INTO normalized_threads (thread_key, subject) VALUES ($1,'Rochester schedule')
		RETURNING id`, ccThreadKey).Scan(&threadID), "thread")
	_, err := pool.Exec(ctx, `INSERT INTO normalized_messages
		(raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		VALUES ($1,$2,'inbound',$3,now(),'when are you on site?','Rochester schedule',$4,'gmail')`,
		rawID, threadID, ccInboundID, ccInboundTo)
	must(err, "inbound")

	must(pool.QueryRow(ctx, `INSERT INTO deliveries
		(task_id, channel, subject, body, status, from_account_id, thread_id, cc, created_by)
		VALUES ($1,'gmail','Re: Rochester schedule','Thursday works.','drafted',$2,$3,$4,'mcp:manual:salvo')
		RETURNING id`, taskID, acctID, threadID, []string{ccKatie}).Scan(&s.draftedID), "drafted gmail row")
	must(pool.QueryRow(ctx, `INSERT INTO deliveries
		(task_id, channel, subject, body, status, from_account_id, thread_id, cc, sent_external_id, sent_at, created_by)
		VALUES ($1,'gmail','Re: Rochester schedule (earlier)','Sent last week.','sent',$2,$3,$4,
		        '<sb-itest-dash-cc@example.com>', now(), 'mcp:manual:salvo')
		RETURNING id`, taskID, acctID, threadID, []string{ccKatie, ccBilling}).Scan(&s.sentID), "sent gmail row")
	return s
}

func TestDashboard_Integration_DeliveryCcShownAndEditable(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupDlvCc(t, ctx, pool)
	defer cleanupDlvCc(t, ctx, pool)
	sd := seedDlvCc(t, ctx, pool)

	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	readCc := func(id int64) []string {
		t.Helper()
		var cc []string
		if err := pool.QueryRow(ctx, `SELECT cc FROM deliveries WHERE id=$1`, id).Scan(&cc); err != nil {
			t.Fatalf("read delivery %d cc: %v", id, err)
		}
		return cc
	}

	code, page := get(t, client, ts.URL+"/deliveries")
	if code != 200 {
		t.Fatalf("GET /deliveries = %d\n%s", code, snippet(page))
	}

	// ---- criterion 19: Cc is visible before approval, and after the send ----
	drafted := rowOf(t, page, sd.draftedID)
	if !strings.Contains(drafted, "Cc:") || !strings.Contains(drafted, ccKatie) {
		t.Errorf("the drafted gmail row does not show \"Cc: %s\". Salvador approves what the page shows him; a "+
			"recipient the page hides is a recipient nobody reviewed (D2 rests on exactly this). Row: %s",
			ccKatie, drafted)
	}
	sent := rowOf(t, page, sd.sentID)
	for _, want := range []string{"Cc:", ccKatie, ccBilling} {
		if !strings.Contains(sent, want) {
			t.Errorf("the SENT gmail row does not show %q. Unlike From/To this is a STORED fact, not a "+
				"re-resolution, so the \"a sent row's route is not recomputed\" rule does not apply to it. Row: %s",
				want, sent)
		}
	}

	// ---- criterion 20: the edit box is pre-filled --------------------------
	if !strings.Contains(drafted, `name="cc"`) {
		t.Fatalf("the drafted row's edit form has no cc input (criterion 20). Row: %s", drafted)
	}
	i := strings.Index(drafted, `name="cc"`)
	formTail := drafted[i:]
	if j := strings.Index(formTail, ">"); j >= 0 {
		formTail = formTail[:j]
	}
	if !strings.Contains(formTail, ccKatie) {
		t.Errorf("the cc input is not pre-filled with the current list (%s): %s", ccKatie, formTail)
	}

	editURL := ts.URL + "/deliveries/" + strconv.FormatInt(sd.draftedID, 10) + "/edit"
	post := func(form url.Values) string {
		t.Helper()
		resp, err := client.PostForm(editURL, form)
		if err != nil {
			t.Fatalf("POST edit %v: %v", form, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	// (a) Saving it with content REPLACES the list (and normalizes it).
	post(url.Values{"cc": {"Billing <" + ccBilling[:strings.LastIndex(ccBilling, "@")] + strings.ToUpper(ccBilling[strings.LastIndex(ccBilling, "@"):]) + ">, " + ccKatie}})
	if got, want := readCc(sd.draftedID), []string{ccBilling, ccKatie}; !ccSame(got, want) {
		t.Errorf("after saving the cc box the row's cc = %q, want %q", got, want)
	}

	// (b) Saving it EMPTY clears it (D10). The POST contains the key with an
	// empty value: that is "no recipients", never "leave it as it was".
	post(url.Values{"cc": {""}})
	if got := readCc(sd.draftedID); len(got) != 0 {
		t.Errorf("clearing the cc box left %q on the row. This is the SWT-61 subject residual applied to a "+
			"RECIPIENT list: Salvador deletes Katie, sees the save succeed, and the mail still goes to her (D10)", got)
	}

	// (c) A POST with NO cc key leaves it alone (the "absent" half of D6):
	// another form on the page must not wipe the recipients.
	post(url.Values{"cc": {ccKatie}})
	post(url.Values{"body": {"Thursday works, 9am."}})
	if got, want := readCc(sd.draftedID), []string{ccKatie}; !ccSame(got, want) {
		t.Errorf("a POST that did not carry the cc key changed the cc to %q, want %q unchanged", got, want)
	}

	// (d) An invalid address comes back as the handler's flash, not a silent
	// no-op, and changes nothing.
	body := post(url.Values{"cc": {"not an address"}})
	if !strings.Contains(body, "cc") && !strings.Contains(body, "address") {
		t.Errorf("saving an invalid cc produced no flash naming the problem:\n%s", snippet(body))
	}
	if got, want := readCc(sd.draftedID), []string{ccKatie}; !ccSame(got, want) {
		t.Errorf("a refused cc edit changed the row to %q, want %q", got, want)
	}

	// (e) Criterion 9 on the review surface: a `sent` row has no edit form at
	// all, so its Cc cannot be changed from here.
	_, page = get(t, client, ts.URL+"/deliveries")
	if s := rowOf(t, page, sd.sentID); strings.Contains(s, `name="cc"`) {
		t.Errorf("the SENT row carries a cc edit input; only a drafted row is editable. Row: %s", s)
	}
}

func ccSame(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
