//go:build integration

package tools_test

// Integration test for the storage half of SWT-19 criterion 7: draft_delivery
// persists the CANONICAL spelling of an upwork_chat target_ref, not the
// caller's.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run UpworkTarget ./internal/tools/
//
// This is the SWT-13 fix's shape, transplanted. There, slackweb.ParseTargetURL
// accepted a trailing slash while confirmDelivery matched target_ref by EXACT
// string, so a delivery drafted with the caller's spelling could never be
// confirmed — silently, with no error anywhere. draft_delivery now stores
// Target.CanonicalURL() (delivery.go), and the same rule must hold here: since
// SWT-18 the upwork matcher compares target_ref exactly, and since SWT-19 it
// parses it, so a stored spelling that differs from the normalizer's is a
// delivery that never closes its loop.
//
// SUPERSEDED IN PART, 2026-08-27. The canonicalisation and existence checks this
// file was written to exercise are still IN the code and still correct, but they
// are no longer reachable through draft_delivery: the fourth adversarial pass
// showed the channel's target cannot be bound to the task's client until SWT-20,
// so upwork_chat drafting is closed at the door. Their unit coverage lives in
// delivery_upwork_target_test.go and threadkey_test.go, which do not need the
// handler. What remains here is the one thing only an integration test can
// prove: that the closure actually holds through the executor, for every actor.
//
// Cross-suite discipline: everything is scoped to the itest-utg-% slug and
// cleaned in FK order, before and after, so the suite is rerunnable against the
// persistent compose db.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	utgActor  = "dashboard:itest-utg@example.com"
	utgSlug   = "itest-utg-proj"
	utgClient = "e2ef9b65-9813-4d79-ac10-0e1813f788ff" // a real client uuid shape
	utgRoom   = "room_1a2b3c4d5e"
)

func cleanupUpworkTarget(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	stmts := []string{
		// policy_decisions before audit_events: the FK points that way
		// (lifecycle_integration_test.go:80-82's order).
		`DELETE FROM policy_decisions WHERE audit_event_id IN
			(SELECT id FROM audit_events WHERE actor='` + utgActor + `')`,
		`DELETE FROM audit_events WHERE actor='` + utgActor + `'`,
		`DELETE FROM task_events WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + utgSlug + `'))`,
		`DELETE FROM deliveries WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + utgSlug + `'))`,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + utgSlug + `')`,
		`DELETE FROM projects WHERE slug='` + utgSlug + `'`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
}

func TestUpworkTarget_Integration_ChannelIsClosedUntilProvenance(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	defer pool.Close()

	cleanupUpworkTarget(t, ctx, pool)
	defer cleanupUpworkTarget(t, ctx, pool)

	projID := seedProject(t, ctx, pool, utgSlug, "itest-utg-client")
	var taskID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'itest-utg closed','claude','done_locally') RETURNING id`, projID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	ex := newExecutor(pool)

	// A real, ingested, canonically-spelled target — the most favourable case
	// there is. If even this is refused, nothing gets through.
	target := "upwork_crm:" + utgClient + ":upwork"
	if _, err := pool.Exec(ctx,
		`INSERT INTO normalized_threads (thread_key, participants) VALUES ($1,'[]')
		 ON CONFLICT (thread_key) WHERE thread_key IS NOT NULL DO NOTHING`, target); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	args := []byte(`{"task_id":` + itoa(taskID) + `,"channel":"upwork_chat","body":"thanks, will do","target_ref":"` + target + `"}`)

	// EVERY actor shape, because the previous gate's whole defect was that it
	// keyed on the actor prefix and so missed the drafts worker entirely. An
	// actor-prefix check describes a transport, not a trust level.
	for _, actor := range []string{
		utgActor,           // dashboard — a deliberate human
		"opsctl:salvo",     // the CLI
		"mcp:worker:itest", // an agent over MCP
		"mcp:manual:salvo", // an interactive session over MCP
		"drafts:gpt",       // the model-backed draft worker — the case the old gate missed
		"worker:itest-utg", // a direct executor caller
	} {
		t.Run(actor, func(t *testing.T) {
			_, err := ex.Execute(ctx, executor.Call{Tool: "draft_delivery", Actor: actor, Args: args})
			if err == nil {
				t.Fatalf("draft_delivery accepted an upwork_chat draft from actor %q. The target cannot be "+
					"bound to the task's client until SWT-20, so a supplied target_ref could name another "+
					"client's thread. The closure must not depend on who is calling — that was exactly the "+
					"defect in the previous ViaMCP-only gate, which %q walks straight past", actor, "drafts:gpt")
			}
			if !strings.Contains(err.Error(), "upwork_chat drafts are disabled") {
				t.Errorf("actor %q refused with %q; the error should name the channel closure and SWT-20, "+
					"not read as a problem with this particular target", actor, err.Error())
			}
		})
	}
}
