//go:build integration

package orchestrator_test

// SWT-41 (docs/tickets/orchestrator-deploy_SPEC.md) criterion 4: start-from-now,
// end to end. The engine plus the real orchestrator_cursor_advance tool, driven
// one drain at a time like the rest of this file's siblings.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run StartFromNow ./internal/orchestrator/
//
// The seeded backlog is the PROD shape (O2): a status_changed -> closed on a
// task with a blocked dependent (R5 would unblock it) and a done_local on a
// delivery='dashboard' task (R3 would create its Deliver task). After the
// advance, DrainOnce must process 0 events, the dependent must stay blocked and
// no Deliver task may appear. Then one NEW done_local must be handled normally:
// exactly one Deliver #N.
//
// MUTATION (criterion 4): skip the advance call and the first drain processes
// the seeded events — the dependent flips to ready and the old task gets a
// Deliver task — so the three post-advance assertions go red.
//
// NOTE ON "processes exactly 1" (SPEC criterion 4): R3 answers the done_local
// with create_task (writes no event) AND record_orchestration, which inserts an
// `orchestrated` task_event (tools/close.go). DrainOnce keeps looping until no
// event is past the cursor, so it also drains that event and returns 2, not 1.
// This test therefore asserts the count against an INDEPENDENT measure (every
// event past the pre-drain cursor, of which exactly one is done_local) rather
// than the literal 1. Flagged to the main thread.
//
// Every task mutation goes through executor.Execute (invariant 3); the only raw
// SQL is setCursor's fixture positioning before the scenario starts.
//
// CLEANUP: cleanupOrch (itest-orch-% slugs and actors) plus the advance's own
// audit rows, which carry a human actor (opsctl:itest-orch-sfn) and no task id
// and so are out of cleanupOrch's reach. Both at start and in t.Cleanup.
//
// GREENFIELD NOTE — EXPECTED RED: `orchestrator_cursor_advance` is not
// registered, so callOrch fails with `unknown tool`. (While health_test.go /
// health_integration_test.go reference missing symbols, this package's
// integration build compile-fails first.)

import (
	"context"
	"testing"

	orch "github.com/sspataro57/switchboard/internal/orchestrator"
)

const sfnHumanActor = "opsctl:itest-orch-sfn"

func TestOrchestrator_Integration_StartFromNow(t *testing.T) {
	ctx := context.Background()
	pool := newOrchPool(t, ctx)
	t.Cleanup(pool.Close) // registered first so it runs LAST, after the fixture cleanups and lock releases
	healthGuard(t)

	cleanAudit := func(ctx context.Context) {
		for _, q := range []string{
			`DELETE FROM policy_decisions WHERE audit_event_id IN
			   (SELECT id FROM audit_events WHERE actor LIKE 'opsctl:itest-orch-%')`,
			`DELETE FROM audit_events WHERE actor LIKE 'opsctl:itest-orch-%'`,
		} {
			if _, err := pool.Exec(ctx, q); err != nil {
				t.Fatalf("cleanup %q: %v", q, err)
			}
		}
	}
	cleanupOrch(t, ctx, pool)
	cleanAudit(ctx)
	t.Cleanup(func() {
		cleanupOrch(t, context.Background(), pool)
		cleanAudit(context.Background())
	})

	const (
		actor  = "itest-orch-sfn"
		slug   = "itest-orch-sfn"
		client = "itest-orch-sfn-client"
		worker = "itest-orch-sfn-w"
	)
	seedProjectDelivery(t, ctx, pool, slug, client, "dashboard")
	ex := newOrchExecutor(pool)
	engine := orch.NewEngine(pool, ex, &recordingPublisher{}, orch.Config{})
	setCursor(t, ctx, pool, maxEventID(t, ctx, pool))

	// Pre-state, drained normally: R4 blocks the dependent on its root.
	root := createReadyTask(t, ctx, ex, actor, slug, "sfn plan root")
	dependent := createReadyTask(t, ctx, ex, actor, slug, "sfn blocked dependent")
	addDep(t, ctx, ex, actor, dependent, root)
	drain(t, ctx, engine)
	if s := orchStatus(t, ctx, pool, dependent); s != "blocked" {
		t.Fatalf("POSITIVE CONTROL: dependent = %q after add_dependency + drain, want blocked", s)
	}

	// Prepared up to in_progress BEFORE the advance, so the only event it writes
	// afterwards is its done_local.
	fresh := createReadyTask(t, ctx, ex, actor, slug, "sfn fresh work")
	claimAndProgress(t, ctx, ex, actor, fresh, worker)

	// The backlog the advance skips.
	old := createReadyTask(t, ctx, ex, actor, slug, "sfn old work")
	claimAndProgress(t, ctx, ex, actor, old, worker)
	markDone(t, ctx, ex, actor, old, worker)
	callOrch(t, ctx, ex, actor, "task_close", `{"task_id":`+itoa(root)+`,"reason":"O2 shape: dead plan root"}`)

	x := cursorValue(t, ctx, pool)
	head := maxEventID(t, ctx, pool)
	if head <= x {
		t.Fatalf("POSITIVE CONTROL: no events past the cursor (cursor %d, head %d)", x, head)
	}

	out := callOrch(t, ctx, ex, sfnHumanActor, "orchestrator_cursor_advance",
		`{"expect_last_event_id":`+itoa(x)+`,"reason":"itest start from now"}`)
	var adv struct {
		From int64 `json:"from"`
		To   int64 `json:"to"`
	}
	mustJSON(t, out, &adv)
	if adv.From != x || adv.To != head {
		t.Errorf("advance output from=%d to=%d, want %d and %d", adv.From, adv.To, x, head)
	}

	if n := drain(t, ctx, engine); n != 0 {
		t.Errorf("DrainOnce after the advance processed %d events, want 0 — start-from-now must not "+
			"replay the skipped range", n)
	}
	if s := orchStatus(t, ctx, pool, dependent); s != "blocked" {
		t.Errorf("dependent = %q after advance + drain, want blocked: the root's status_changed->closed "+
			"was skipped, so R5 must not have fired", s)
	}
	if n := deliverTaskCount(t, ctx, pool, old); n != 0 {
		t.Errorf("old task has %d Deliver tasks after advance + drain, want 0: its done_local was skipped", n)
	}

	// A NEW done_local after the advance is handled normally.
	markDone(t, ctx, ex, actor, fresh, worker)
	before := cursorValue(t, ctx, pool)
	n := drain(t, ctx, engine)

	var past, doneLocals int
	if err := pool.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE event_type='done_local') FROM task_events WHERE id > $1`,
		before).Scan(&past, &doneLocals); err != nil {
		t.Fatalf("independent count of drained events: %v", err)
	}
	if doneLocals != 1 {
		t.Errorf("%d done_local events past the pre-drain cursor, want exactly the one new one", doneLocals)
	}
	if n != past {
		t.Errorf("DrainOnce processed %d events, want %d (every event past the pre-drain cursor: the new "+
			"done_local plus what R3's own actions wrote)", n, past)
	}
	t.Logf("post-advance drain processed %d event(s) (SPEC says 1; see header)", n)

	if c := deliverTaskCount(t, ctx, pool, fresh); c != 1 {
		t.Errorf("fresh task has %d Deliver tasks, want exactly 1 (R3 on the new done_local)", c)
	}
	if c := deliverTaskCount(t, ctx, pool, old); c != 0 {
		t.Errorf("old task has %d Deliver tasks after the second drain, want 0", c)
	}
	if s := orchStatus(t, ctx, pool, dependent); s != "blocked" {
		t.Errorf("dependent = %q after the second drain, want still blocked", s)
	}
}
