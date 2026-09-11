//go:build integration

package drafts_test

// SWT-37 (docs/tickets/mcp-task-verbs_SPEC.md), Q1 answered (b) on 2026-09-10:
// drafts.DeliverTasks no longer queues a Deliver task whose PARENT is no longer
// done_locally. Hand-marking work delivered ("swb delivered 412") or closing it
// ("swb close 412") moves only the parent; R8 fires on delivery_sent, never on
// a status change, so R3's `Deliver #N` child stays open. Before this clause,
// the draft worker drafted a delivery for work already delivered or closed. The
// draft never sends, but it lands in the approval queue as noise, and every
// path reaches it: MCP, opsctl, and any future dashboard verb.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run DeliverTaskOfA ./internal/drafts/
//
// IMPOSED SURFACE (SPEC "Data model changes", Q1 (b)): a READ predicate in
// internal/drafts/store.go's DeliverTasks WHERE clause,
//
//	AND parent.status = 'done_locally'
//
// with no schema change. The query already joins `tasks parent`.
//
// WHY INTEGRATION (IK "test the column, not the fixture"): the guard is fed
// by a DB column, parent.status, so the test sits where Postgres evaluates it.
// MUTATION: delete the clause from the SELECT and this test goes red on the
// delivered and closed rows. The done_locally row is the positive control: it
// proves the fixture reaches the queue at all, so a red on the other two cannot
// come from a fixture that never qualified.
//
// Fixture: store_integration_test.go's dsFixture (projects under
// itest-dstore-%, cleaned in FK order at start and end). The parent's status is
// moved by a plain UPDATE, exactly the column task_mark_delivered / task_close
// write. Never run against 192.168.50.49; newDSFixture refuses it.
//
// EXPECTED RED today: DeliverTasks returns the delivered-parent and
// closed-parent Deliver tasks.

import (
	"context"
	"testing"

	"github.com/sspataro57/switchboard/internal/drafts"
)

func TestDraftsStore_Integration_DeliverTaskOfAHandDeliveredOrClosedParentIsNotQueued(t *testing.T) {
	ctx := context.Background()
	f := newDSFixture(t, ctx)

	openDeliver, _, _ := f.project(t, ctx, projectSpec{name: "q1-open", client: "Q1 Open"})
	deliveredDeliver, deliveredParent, _ := f.project(t, ctx, projectSpec{name: "q1-delivered", client: "Q1 Delivered"})
	closedDeliver, closedParent, _ := f.project(t, ctx, projectSpec{name: "q1-closed", client: "Q1 Closed"})

	for id, status := range map[int64]string{deliveredParent: "delivered", closedParent: "closed"} {
		if _, err := f.pool.Exec(ctx, `UPDATE tasks SET status=$2, updated_at=now() WHERE id=$1`, id, status); err != nil {
			t.Fatalf("move parent %d to %s: %v", id, status, err)
		}
	}

	queued, err := drafts.NewStore(f.pool).DeliverTasks(ctx, drafts.Config{})
	if err != nil {
		t.Fatalf("DeliverTasks: %v", err)
	}
	got := map[int64]bool{}
	for _, dt := range queued {
		got[dt.DeliverTaskID] = true
	}

	if !got[openDeliver] {
		t.Fatalf("POSITIVE CONTROL FAILED: the Deliver task %d of a done_locally parent is not queued; the "+
			"fixture never reaches DeliverTasks, so the two assertions below would pass for the wrong reason", openDeliver)
	}
	if got[deliveredDeliver] {
		t.Errorf("DeliverTasks queued Deliver task %d although its parent %d is 'delivered' (marked by hand): "+
			"the draft worker would draft a delivery for work already delivered. Q1 (b): "+
			"AND parent.status = 'done_locally'", deliveredDeliver, deliveredParent)
	}
	if got[closedDeliver] {
		t.Errorf("DeliverTasks queued Deliver task %d although its parent %d is 'closed': the draft worker would "+
			"draft a delivery for closed work. Q1 (b): AND parent.status = 'done_locally'", closedDeliver, closedParent)
	}

	// The predicate is a READ filter: the Deliver tasks themselves are not
	// touched and stay on the board until someone closes them (Q1 (b)).
	for _, id := range []int64{deliveredDeliver, closedDeliver} {
		var s string
		if err := f.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, id).Scan(&s); err != nil {
			t.Fatalf("read Deliver task %d: %v", id, err)
		}
		if s != "ready" {
			t.Errorf("Deliver task %d status = %q, want ready: DeliverTasks must only filter, never write", id, s)
		}
	}
}
