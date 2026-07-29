//go:build integration

package tools_test

// Coverage for the fixes that came out of SWT-12's adversarial review. Before
// this file every one of them was asserted by prose only, which the Go review
// called out: a pass that invented two columns, a lease, a time floor, a claim
// check and a transport restriction shipped without a single assertion.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -run SlackReview ./internal/tools/
//
// Reuses the slackSuite harness and the 'itest-sds-%' cleanup pact from
// delivery_slack_integration_test.go — same corpus, same teardown.

import (
	"context"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
)

// ---- the lease (criterion 22) ---------------------------------------------------

// An attempt that has not settled is still executing. Marking it failed there is
// what let a human reopen a live call and double-post into a client channel, so
// the refusal is the fix; the lease is the escape hatch for a sender that died
// mid-click and will therefore never settle.
func TestSlackReview_Integration_MarkFailedRefusesAnInFlightAttempt(t *testing.T) {
	ctx := context.Background()
	s := newSlackSuite(t, ctx, true)
	id := s.draft(t, ctx, "in flight attempt")
	s.call(t, ctx, "approve_delivery", id)

	// Simulate a dispatch that has not come back: attempted now, never settled.
	if _, err := s.pool.Exec(ctx,
		`UPDATE deliveries SET status='sending', send_attempted_at=now(), send_settled_at=NULL WHERE id=$1`,
		id); err != nil {
		t.Fatalf("seed in-flight attempt: %v", err)
	}

	err := s.tryCall(ctx, "mark_delivery_failed", id)
	if err == nil {
		t.Fatal("mark_delivery_failed accepted an IN-FLIGHT attempt; a human could then re-approve and " +
			"start a second click while the first was still running (two posts into a client channel)")
	}
	if !strings.Contains(err.Error(), "in flight") {
		t.Errorf("error should name the in-flight attempt, got: %v", err)
	}
	if r := s.row(t, ctx, id); r.status != "sending" {
		t.Errorf("status = %q after a refused mark-failed, want still sending", r.status)
	}

	// Past the lease the attempt is treated as abandoned, or a crashed sender
	// would wedge the row forever.
	if _, err := s.pool.Exec(ctx,
		`UPDATE deliveries SET send_attempted_at = now() - interval '16 minutes' WHERE id=$1`, id); err != nil {
		t.Fatalf("backdate attempt: %v", err)
	}
	if err := s.tryCall(ctx, "mark_delivery_failed", id); err != nil {
		t.Fatalf("mark_delivery_failed refused an attempt older than the lease: %v", err)
	}
	if r := s.row(t, ctx, id); r.status != "failed" {
		t.Errorf("status = %q after the lease expired, want failed", r.status)
	}
}

// A settled attempt is the ordinary ambiguous case and must stay resolvable
// immediately — the lease exists for unsettled attempts only.
func TestSlackReview_Integration_MarkFailedAllowsASettledAmbiguousAttempt(t *testing.T) {
	ctx := context.Background()
	s := newSlackSuite(t, ctx, true)
	id := s.draft(t, ctx, "settled ambiguous attempt")
	s.call(t, ctx, "approve_delivery", id)

	if _, err := s.pool.Exec(ctx,
		`UPDATE deliveries SET status='sending', send_attempted_at=now(), send_settled_at=now(),
		        error='bridge answered 502' WHERE id=$1`, id); err != nil {
		t.Fatalf("seed settled attempt: %v", err)
	}
	if err := s.tryCall(ctx, "mark_delivery_failed", id); err != nil {
		t.Fatalf("mark_delivery_failed refused a SETTLED ambiguous attempt: %v", err)
	}
}

// ---- attempt columns are actually written (criterion 22) -----------------------

func TestSlackReview_Integration_SendRecordsTheAttemptWindow(t *testing.T) {
	ctx := context.Background()
	s := newSlackSuite(t, ctx, true)
	id := s.draft(t, ctx, "attempt window")
	s.call(t, ctx, "approve_delivery", id)
	s.call(t, ctx, "send_delivery", id)

	var attempted, settled *string
	if err := s.pool.QueryRow(ctx,
		`SELECT send_attempted_at::text, send_settled_at::text FROM deliveries WHERE id=$1`,
		id).Scan(&attempted, &settled); err != nil {
		t.Fatalf("read attempt window: %v", err)
	}
	if attempted == nil {
		t.Error("send_attempted_at is NULL after a send; the lease and the confirmation floor both read it")
	}
	if settled == nil {
		t.Error("send_settled_at is NULL after the bridge call returned; the row would look in-flight forever " +
			"and mark_delivery_failed would refuse it for the whole lease")
	}
}

// ---- the MCP transport restriction (criterion 24) -------------------------------

// Over MCP the tool may resolve an attempt switchboard itself dispatched, and
// nothing else. Recording an approved or still-drafted row would assert a send
// that never happened, and delivery_sent drives R8, which closes the work task as
// delivered — so a prompt-injected session could fabricate completion.
func TestSlackReview_Integration_MCPMayOnlyResolveASendingRow(t *testing.T) {
	ctx := context.Background()
	s := newSlackSuite(t, ctx, true)
	const mcpActor = "mcp:manual:itest-sds"

	tryAsMCP := func(id int64) error {
		_, err := s.ex.Execute(ctx, executor.Call{Tool: "mark_delivery_sent", Actor: mcpActor,
			Args: []byte(`{"delivery_id":` + itoa(id) + `}`)})
		return err
	}

	t.Run("refused on approved", func(t *testing.T) {
		id := s.draft(t, ctx, "mcp on approved")
		s.call(t, ctx, "approve_delivery", id)
		if err := tryAsMCP(id); err == nil {
			t.Fatal("MCP recorded an APPROVED row as sent; switchboard never dispatched it, so this " +
				"fabricates a delivery and R8 then closes the task as delivered")
		}
	})

	t.Run("refused on drafted with leaf_gated", func(t *testing.T) {
		id := s.draft(t, ctx, "mcp on drafted leaf")
		if _, err := s.pool.Exec(ctx,
			`UPDATE deliveries SET approval_source='leaf_token' WHERE id=$1`, id); err != nil {
			t.Fatalf("stamp leaf_token: %v", err)
		}
		_, err := s.ex.Execute(ctx, executor.Call{Tool: "mark_delivery_sent", Actor: mcpActor,
			Args: []byte(`{"delivery_id":` + itoa(id) + `,"leaf_gated":true}`)})
		if err == nil {
			t.Fatal("MCP recorded a DRAFTED row as sent via leaf_gated; that is the forgeable edge — it must " +
				"go through the dashboard or opsctl")
		}
	})

	t.Run("allowed on sending", func(t *testing.T) {
		id := s.draft(t, ctx, "mcp on sending")
		s.call(t, ctx, "approve_delivery", id)
		if _, err := s.pool.Exec(ctx,
			`UPDATE deliveries SET status='sending', send_attempted_at=now(), send_settled_at=now()
			 WHERE id=$1`, id); err != nil {
			t.Fatalf("seed sending row: %v", err)
		}
		if err := tryAsMCP(id); err != nil {
			t.Fatalf("MCP could not resolve a 'sending' row, which is the one transition it exists for: %v", err)
		}
		if r := s.row(t, ctx, id); r.status != "sent" {
			t.Errorf("status = %q, want sent", r.status)
		}
	})

	// The non-MCP surfaces keep the full set: the restriction is about the
	// transport, not about what a human may assert.
	t.Run("dashboard actor keeps the leaf_gated edge", func(t *testing.T) {
		id := s.draft(t, ctx, "dashboard leaf gated")
		out, err := s.ex.Execute(ctx, executor.Call{Tool: "mark_delivery_sent", Actor: sdsActor,
			Args: []byte(`{"delivery_id":` + itoa(id) + `,"leaf_gated":true}`)})
		if err != nil {
			t.Fatalf("dashboard actor refused the leaf_gated edge: %v (out=%s)", err, out.Output)
		}
		var source *string
		if err := s.pool.QueryRow(ctx,
			`SELECT approval_source FROM deliveries WHERE id=$1`, id).Scan(&source); err != nil {
			t.Fatalf("read approval_source: %v", err)
		}
		if source == nil || *source != "leaf_token" {
			t.Errorf("approval_source = %v, want leaf_token stamped in the same tx as the transition", source)
		}
	})
}

// ---- send_delivery requires switchboard authority (criterion 26) ---------------

func TestSlackReview_Integration_SendRefusesALeafGatedRow(t *testing.T) {
	ctx := context.Background()
	s := newSlackSuite(t, ctx, true)
	id := s.draft(t, ctx, "leaf gated must not be sent")
	s.call(t, ctx, "approve_delivery", id)
	// Approval stamped 'switchboard'; forcing 'leaf_token' models a row whose
	// authority is not switchboard's to act on.
	if _, err := s.pool.Exec(ctx,
		`UPDATE deliveries SET approval_source='leaf_token' WHERE id=$1`, id); err != nil {
		t.Fatalf("force leaf_token: %v", err)
	}
	err := s.tryCall(ctx, "send_delivery", id)
	if err == nil {
		t.Fatal("send_delivery sent a row whose approval_source is leaf_token; that row's message is already " +
			"in the channel, so this is a double-post")
	}
	if !strings.Contains(err.Error(), "approval_source") {
		t.Errorf("error should name approval_source, got: %v", err)
	}
}

func TestSlackReview_Integration_ApproveStampsSwitchboardAuthority(t *testing.T) {
	ctx := context.Background()
	s := newSlackSuite(t, ctx, true)
	id := s.draft(t, ctx, "authority stamp")

	var before *string
	if err := s.pool.QueryRow(ctx, `SELECT approval_source FROM deliveries WHERE id=$1`, id).Scan(&before); err != nil {
		t.Fatalf("read approval_source: %v", err)
	}
	if before != nil {
		t.Errorf("a drafted row already claims approval_source=%q; a DEFAULT here would make every draft "+
			"assert a gate it never got, and would then refuse every manual-path record", *before)
	}

	s.call(t, ctx, "approve_delivery", id)
	var after *string
	if err := s.pool.QueryRow(ctx, `SELECT approval_source FROM deliveries WHERE id=$1`, id).Scan(&after); err != nil {
		t.Fatalf("read approval_source: %v", err)
	}
	if after == nil || *after != "switchboard" {
		t.Errorf("approval_source = %v after approve_delivery, want switchboard", after)
	}
}
