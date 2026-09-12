//go:build integration

package tools_test

// SWT-44 review fixes (user-profile-drafts) against a real database:
//
//   - fix 1, content-bound approval: approve_delivery with expect_content_hash
//     refuses, under the delivery row lock, when the row's subject/body is no
//     longer what the approver was shown, and leaves the row drafted.
//   - fix 4, the gmail route: tools.ResolveGmailRoute is the ONE spelling of
//     where a gmail send goes (From, To, threading), shared by send_delivery's
//     phase 1 and the dashboard.
//   - second round: draft_delivery's require_thread_in_task_project pin (owner
//     decision "Same project", 2026-09-12) and update_delivery's
//     require_channel pin (gmail-only edits).
//
// Reuses the SWT-8 lifecycle fixture (seedDeliveryFixture / cleanupDeliveryData
// / deliveryExecutor / draftGmail in delivery_lifecycle_integration_test.go).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -run 'ContentBound|GmailRoute' ./internal/tools/
//
// MUTATION (run by hand, SWT-44): delete the expect_content_hash comparison in
// approveDelivery → the stale approve succeeds and
// TestApproveDelivery_Integration_ContentBound goes red at "was APPROVED".

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/tools"
)

func TestApproveDelivery_Integration_ContentBound(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()
	cleanupDeliveryData(t, ctx, pool)
	defer cleanupDeliveryData(t, ctx, pool)

	fx := seedDeliveryFixture(t, ctx, pool)
	ex := deliveryExecutor(pool)

	// draftGmail writes subject "Re: login broken", body "draft body".
	id := draftGmail(t, ctx, ex, fx.parentID, fx.threadID)
	shown := tools.DeliveryContentHash("Re: login broken", "draft body")

	// An edit lands between the render and the click (a session's
	// update_delivery, say).
	callOK(t, ctx, ex, delActor, "update_delivery",
		`{"delivery_id":`+itoa(id)+`,"body":"planted words"}`)

	_, err := ex.Execute(ctx, executor.Call{Tool: "approve_delivery", Actor: delActor,
		Args: []byte(`{"delivery_id":` + itoa(id) + `,"expect_content_hash":"` + shown + `"}`)})
	if err == nil {
		t.Fatalf("approve_delivery with the hash of the words Salvador SAW was APPROVED after the row changed; " +
			"the human gate must be tied to what he was shown")
	}
	if !strings.Contains(err.Error(), "changed since it was shown to you") {
		t.Errorf("stale approve refused with %q, want the reload-and-review message", err)
	}
	if s := deliveryStatus(t, ctx, pool, id); s != "drafted" {
		t.Errorf("after a refused stale approve status = %q, want drafted", s)
	}
	var approvals int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM approvals WHERE subject_type='delivery' AND subject_id=$1`, id).Scan(&approvals); err != nil {
		t.Fatalf("count approvals: %v", err)
	}
	if approvals != 0 {
		t.Errorf("a refused approve left %d approvals row(s)", approvals)
	}

	// The hash of the CURRENT words approves.
	callOK(t, ctx, ex, delActor, "approve_delivery",
		`{"delivery_id":`+itoa(id)+`,"expect_content_hash":"`+tools.DeliveryContentHash("Re: login broken", "planted words")+`"}`)
	if s := deliveryStatus(t, ctx, pool, id); s != "approved" {
		t.Errorf("approve with the current hash left status %q, want approved", s)
	}

	// A cleared subject (NULL in the row) hashes as "", the dashboard's COALESCE.
	id2 := draftGmail(t, ctx, ex, fx.parentID, fx.threadID)
	callOK(t, ctx, ex, delActor, "update_delivery", `{"delivery_id":`+itoa(id2)+`,"subject":""}`)
	callOK(t, ctx, ex, delActor, "approve_delivery",
		`{"delivery_id":`+itoa(id2)+`,"expect_content_hash":"`+tools.DeliveryContentHash("", "draft body")+`"}`)
	if s := deliveryStatus(t, ctx, pool, id2); s != "approved" {
		t.Errorf("approve of a subject-less draft with hash(\"\", body) left status %q, want approved", s)
	}

	// Omitted hash: today's behaviour (opsctl, full-profile MCP callers).
	id3 := draftGmail(t, ctx, ex, fx.parentID, fx.threadID)
	approve(t, ctx, ex, id3)
	if s := deliveryStatus(t, ctx, pool, id3); s != "approved" {
		t.Errorf("approve without a hash left status %q, want approved", s)
	}
}

// ---- second review round: same-project threads, gmail-only edits -----------

const (
	tpOtherSlug = "itest-del-thrproj-other"
	tpOutMID    = "<itest-del-thrproj-out@example.com>"
	tpOutRaw    = "itest-del-thrproj-raw-out"
)

// thrProjCleanup removes what the thread-project test adds on top of the SWT-8
// fixture. It runs BEFORE cleanupDeliveryData: that one deletes
// normalized_threads before tasks, so a task still pointing at the thread
// through source_thread_id would fail its FK.
func thrProjCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, st := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE tasks SET source_thread_id=NULL WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, []any{delSlug}},
		{`DELETE FROM capture_decisions WHERE message_id IN
			(SELECT id FROM normalized_messages WHERE external_message_id = ANY($1))`, []any{[]string{delInboundMID, tpOutMID}}},
		{`DELETE FROM capture_decisions WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, []any{tpOtherSlug}},
		{`DELETE FROM normalized_messages WHERE external_message_id=$1`, []any{tpOutMID}},
		{`DELETE FROM raw_source_items WHERE external_id=$1`, []any{tpOutRaw}},
		{`DELETE FROM projects WHERE slug=$1`, []any{tpOtherSlug}},
	} {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

// Owner decision (Salvador, 2026-09-12: "Same project"): with the user
// profile's pin require_thread_in_task_project:"true", draft_delivery drafts
// only on a thread already filed under the task's project — the task's own
// source thread, or a thread with an INBOUND message whose LATEST
// capture_decisions row (any mode) names the task's project. Checked inside
// the transaction, before the insert; a refusal writes no row.
//
// MUTATION (run by hand): drop the require_thread_in_task_project check in
// draftDelivery → the unfiled, other-project, outbound-only and re-pointed
// refusals are all drafted, and this test goes red at "was DRAFTED".
func TestDraftDelivery_Integration_ThreadMustBeFiledUnderTheTaskProject(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()
	thrProjCleanup(t, ctx, pool)
	cleanupDeliveryData(t, ctx, pool)
	defer cleanupDeliveryData(t, ctx, pool)
	defer thrProjCleanup(t, ctx, pool) // LIFO: before cleanupDeliveryData

	fx := seedDeliveryFixture(t, ctx, pool)
	ex := deliveryExecutor(pool)

	var projID, inboundID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM projects WHERE slug=$1`, delSlug).Scan(&projID); err != nil {
		t.Fatalf("read project: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM normalized_messages WHERE external_message_id=$1`, delInboundMID).
		Scan(&inboundID); err != nil {
		t.Fatalf("read inbound: %v", err)
	}
	otherID := seedProject(t, ctx, pool, tpOtherSlug, delClient)

	decide := func(msgID int64, projectID *int64, mode string) {
		t.Helper()
		action := "attributed"
		if projectID == nil {
			action = "unmatched"
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
			 VALUES ($1, $2, $3, $4, 'itest thread-project')`, msgID, mode, action, projectID); err != nil {
			t.Fatalf("insert decision: %v", err)
		}
	}
	rows := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE task_id=$1`, fx.parentID).Scan(&n); err != nil {
			t.Fatalf("count deliveries: %v", err)
		}
		return n
	}
	draft := func(pin string) error {
		extra := ""
		if pin != "" {
			extra = `,"require_thread_in_task_project":"` + pin + `"`
		}
		_, err := ex.Execute(ctx, executor.Call{Tool: "draft_delivery", Actor: delActor,
			Args: []byte(`{"task_id":` + itoa(fx.parentID) + `,"channel":"gmail","subject":"Re: login broken",` +
				`"body":"on it","thread_id":` + itoa(fx.threadID) + extra + `}`)})
		return err
	}
	refused := func(stage string) {
		t.Helper()
		before := rows()
		err := draft("true")
		if err == nil {
			t.Errorf("%s: a pinned gmail draft was DRAFTED on a thread not filed under the task's project", stage)
			return
		}
		want := "thread " + itoa(fx.threadID) + " is not filed under this task's project (" + delSlug + ")"
		if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "file it first") {
			t.Errorf("%s: refusal = %q, want it to contain %q and say how to file it", stage, err, want)
		}
		if after := rows(); after != before {
			t.Errorf("%s: a refused draft changed the delivery count %d → %d", stage, before, after)
		}
	}
	allowed := func(stage string) {
		t.Helper()
		if err := draft("true"); err != nil {
			t.Errorf("%s: pinned draft refused: %v", stage, err)
		}
	}

	// 1. Unfiled: no decision at all.
	refused("unfiled thread")
	// Positive controls: no pin (the full profile, the drafts worker) and a pin
	// the caller set to false both draft as before — the handler checks only
	// "true"; the user profile's adapter overwrites whatever the model sent.
	if err := draft(""); err != nil {
		t.Errorf("POSITIVE CONTROL: an unpinned gmail draft on an unfiled thread was refused: %v", err)
	}
	if err := draft("false"); err != nil {
		t.Errorf("POSITIVE CONTROL: require_thread_in_task_project=false was treated as the pin: %v", err)
	}

	// 2. An OUTBOUND message filed under the project does not file the thread:
	// our own send is not evidence the conversation belongs to the project.
	var outRaw, outID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1, $2, '{}', 'itest-del-thrproj-hash-out') RETURNING id`, fx.accountID, tpOutRaw).Scan(&outRaw); err != nil {
		t.Fatalf("seed outbound raw: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1, $2, 'outbound', $3, now(), 'ours', 'login broken', $4, 'gmail') RETURNING id`,
		outRaw, fx.threadID, tpOutMID, delAcctEmail).Scan(&outID); err != nil {
		t.Fatalf("seed outbound: %v", err)
	}
	decide(outID, &projID, "live")
	refused("only an outbound message filed under the project")

	// 3. Filed under ANOTHER project. (capture_decisions_live_uniq allows one
	// live row per message; the later ones are shadow, which also proves the
	// latest decision counts in ANY mode, not only live.)
	decide(inboundID, &otherID, "live")
	refused("thread filed under another project")

	// 4. A later (shadow) decision re-points it here: allowed.
	decide(inboundID, &projID, "shadow")
	allowed("latest decision names the task's project")

	// 5. A later one re-points it away again: refused. Latest wins, not "any".
	decide(inboundID, &otherID, "shadow")
	refused("re-pointed to another project by a later decision")

	// 6. Latest decision unmatched (no project): unfiled again.
	decide(inboundID, nil, "shadow")
	refused("latest decision unmatched")

	// 7. The task's own source thread is allowed even while unfiled.
	if _, err := pool.Exec(ctx, `UPDATE tasks SET source_thread_id=$1 WHERE id=$2`, fx.threadID, fx.parentID); err != nil {
		t.Fatalf("set source thread: %v", err)
	}
	allowed("the task's own source thread")
}

// With the user profile's second update pin require_channel:"gmail", the
// update_delivery handler refuses — under the row lock — a non-gmail draft,
// even one the caller's own actor created (this repo's full-profile session
// shares mcp:manual:salvo with the user-scope install).
//
// MUTATION (run by hand): drop the require_channel comparison in
// updateDelivery → the slack_reply edit lands and this test goes red at
// "edited a slack_reply".
func TestUpdateDelivery_Integration_RequireChannel(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()
	cleanupDeliveryData(t, ctx, pool)
	defer cleanupDeliveryData(t, ctx, pool)

	fx := seedDeliveryFixture(t, ctx, pool)
	ex := deliveryExecutor(pool)

	var slackID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, created_by)
		 VALUES ($1, 'slack_reply', 'https://app.slack.com/client/TITEST/CITEST/p1750000000000000',
		         'their words', 'drafted', $2) RETURNING id`, fx.parentID, delActor).Scan(&slackID); err != nil {
		t.Fatalf("seed slack draft: %v", err)
	}
	body := func(id int64) string {
		t.Helper()
		var b string
		if err := pool.QueryRow(ctx, `SELECT body FROM deliveries WHERE id=$1`, id).Scan(&b); err != nil {
			t.Fatalf("read body: %v", err)
		}
		return b
	}

	_, err := ex.Execute(ctx, executor.Call{Tool: "update_delivery", Actor: delActor,
		Args: []byte(`{"delivery_id":` + itoa(slackID) + `,"body":"planted","require_own_draft":"true","require_channel":"gmail"}`)})
	if err == nil {
		t.Error("update_delivery under require_channel gmail edited a slack_reply draft (its own actor's)")
	} else if !strings.Contains(err.Error(), "gmail") || !strings.Contains(err.Error(), "slack_reply") {
		t.Errorf("refusal = %q, want it to name the draft's channel and the allowed one (gmail)", err)
	}
	if b := body(slackID); b != "their words" {
		t.Errorf("slack draft body = %q after a refused edit, want it untouched", b)
	}

	// Positive controls: the same own-draft edit with no channel pin passes
	// (the refusal above is the channel, not ownership), and a gmail draft
	// passes the pin.
	callOK(t, ctx, ex, delActor, "update_delivery",
		`{"delivery_id":`+itoa(slackID)+`,"body":"fixed here","require_own_draft":"true"}`)
	gid := draftGmail(t, ctx, ex, fx.parentID, fx.threadID)
	callOK(t, ctx, ex, delActor, "update_delivery",
		`{"delivery_id":`+itoa(gid)+`,"body":"fixed words","require_own_draft":"true","require_channel":"gmail"}`)
	if b := body(gid); b != "fixed words" {
		t.Errorf("gmail draft body = %q, want the edit", b)
	}
}

func TestResolveGmailRoute_Integration(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()
	cleanupDeliveryData(t, ctx, pool)
	defer cleanupDeliveryData(t, ctx, pool)

	fx := seedDeliveryFixture(t, ctx, pool)

	r, err := tools.ResolveGmailRoute(ctx, pool, fx.accountID, fx.threadID)
	if err != nil {
		t.Fatalf("ResolveGmailRoute: %v", err)
	}
	for _, c := range []struct{ name, got, want string }{
		{"From", r.From, delAcctEmail},
		{"To", r.To, delInboundFrom},
		{"InReplyTo", r.InReplyTo, delInboundMID},
		{"GmailThread", r.GmailThread, delGThreadID},
		{"Subject", r.Subject, "login broken"},
	} {
		if c.got != c.want {
			t.Errorf("route.%s = %q, want %q", c.name, c.got, c.want)
		}
	}

	// A thread with nothing inbound has no one to reply to: the send refuses it,
	// so the route must too (the dashboard then shows "(unresolved)").
	if _, err := pool.Exec(ctx,
		`UPDATE normalized_messages SET direction='outbound' WHERE external_message_id=$1`, delInboundMID); err != nil {
		t.Fatalf("flip inbound: %v", err)
	}
	if _, err := tools.ResolveGmailRoute(ctx, pool, fx.accountID, fx.threadID); err == nil ||
		!strings.Contains(err.Error(), "no inbound message to reply to") {
		t.Errorf("route of a thread with no inbound message = %v, want the send path's refusal", err)
	}
}
