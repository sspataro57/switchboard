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
