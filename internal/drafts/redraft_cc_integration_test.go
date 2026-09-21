//go:build integration

package drafts_test

// gmail-delivery-cc (SWT-69) criterion 21 / D9, the half that needs Postgres:
// DeliverTasks' LATERAL — which already carries the rejected row's body and
// rejection_note — also selects its `cc` into DeliverTask.RedraftCc, read from
// the COLUMN (IK landmine 6: a field fed by a column needs a test that makes
// POSTGRES produce the value; a fake store would only test the fake).
//
// The other half — drafts.Run passing that value as draft_delivery's `cc`, and
// only then — is the amended TestDrafts_Redraft_DraftsThroughTheSameCall plus
// TestDrafts_Redraft_NoCcOnTheRejectedRowPassesNoCcKey in worker_test.go.
//
// ISOLATED scratch database only:
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_swt69?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run DraftsRedraftCc ./internal/drafts/
//
// Reuses store_integration_test.go's dsFixture and redraft_integration_test.go's
// rdSeedRejected shape.
//
// EXPECTED RED: deliveries has no cc column and DeliverTask has no RedraftCc.
//
// MUTATION (SPEC mutation 11): drop `d.cc` from the LATERAL → RedraftCc comes
// back empty and this goes red; drop the pass-through in drafts.Run → the unit
// test goes red.

import (
	"context"
	"testing"
)

func TestDraftsRedraftCc_Integration_TheRejectedRowsCcIsCarried(t *testing.T) {
	ctx := context.Background()
	f := newDSFixture(t, ctx)

	withCc, withCcParent, _ := f.project(t, ctx, projectSpec{name: "rd-cc", client: "RD Cc"})
	noCc, noCcParent, _ := f.project(t, ctx, projectSpec{name: "rd-nocc", client: "RD NoCc"})

	const katie = "kevans@cecollaboratory.com"
	const billing = "billing@example.com"

	// A Redo on a row that had two carbon copies...
	var ccID int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, body, status, rejection_note, redraft_requested_at, cc, created_by)
		 VALUES ($1,'gmail','apologies for the delay','rejected','shorter', now(), $2, 'drafts:gpt') RETURNING id`,
		withCcParent, []string{katie, billing}).Scan(&ccID); err != nil {
		t.Fatalf("seed rejected delivery with a cc: %v (migration 0038 adds the column)", err)
	}
	// ...and a Redo on a row that had none: the control that proves the field
	// comes from the row rather than from the queue always filling it.
	rdSeedRejected(t, ctx, f, noCcParent, "no copies here", "shorter", true)

	q := rdQueue(t, ctx, f)

	got := q[withCc]
	if len(got) != 1 {
		t.Fatalf("the redraft-requested Deliver task is listed %d times, want 1", len(got))
	}
	if got[0].RedraftOf != ccID {
		t.Fatalf("RedraftOf = %d, want the rejected row %d", got[0].RedraftOf, ccID)
	}
	want := []string{katie, billing}
	if len(got[0].RedraftCc) != len(want) {
		t.Fatalf("RedraftCc = %v, want %v read from deliveries.cc — the Redo throws away the WORDS, not the "+
			"recipients (D9)", got[0].RedraftCc, want)
	}
	for i := range want {
		if got[0].RedraftCc[i] != want[i] {
			t.Errorf("RedraftCc[%d] = %q, want %q (order preserved)", i, got[0].RedraftCc[i], want[i])
		}
	}

	if none := q[noCc]; len(none) != 1 {
		t.Fatalf("the no-cc Deliver task is listed %d times, want 1", len(none))
	} else if len(none[0].RedraftCc) != 0 {
		t.Errorf("a rejected row with no cc produced RedraftCc = %v, want empty: switchboard never adds a "+
			"recipient on its own (D1)", none[0].RedraftCc)
	}
}
