package store

// SWT-20 acceptance criterion 4, the half that can be proved with no database:
// `store.TaskSourceThread` is ONE reader of provenance, behind a querier seam,
// and "no provenance anywhere" is a `found=false`, NOT an error.
//
// ZERO I/O. The Postgres half — the parent walk, and POSTGRES rather than a
// fixture supplying the column — lives in provenance_integration_test.go,
// because IK is explicit that a predicate fed by a column needs an integration
// test ("test the column, not the fixture"; the SWT-21 landmine where a guard
// passed for months while the SELECT never read its column).
//
// WHY ONE READER: `drafts/store.go` and `tools/delivery.go` both need "which
// conversation raised this task", and two spellings of one query is the drift
// this repo has paid for four times (SWT-13, SWT-16, SWT-18, and the
// UnconfirmedNoteMarker consolidation in this very package). SPEC §2 puts the
// single definition here for the same reason `UnconfirmedNoteMarker` is here.
//
// GREENFIELD NOTE: none of `Querier`, `SourceThread` or `TaskSourceThread`
// exists yet, so this file is a COMPILE failure for the whole `store` package's
// tests. Expected red state.
//
// IMPOSED CONTRACT — argue with it here, not in a later ticket. SPEC §2 gives:
//
//	type SourceThread struct{ TaskID, ThreadID int64; ThreadKey string }
//	func TaskSourceThread(ctx context.Context, q Querier, taskID int64) (SourceThread, bool, error)
//
// but leaves `Querier` ("the querier seam") unspecified. This file pins it to
// the pgx-shaped minimum that BOTH callers already hold:
//
//	type Querier interface {
//	    QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
//	}
//
// so a `*pgxpool.Pool` and a `pgx.Tx` both satisfy it with no adapter — the
// delivery handler runs inside a transaction, the drafts resolver does not, and
// neither should have to wrap itself to ask one question.
//
// Deliberately NOT asserted here: the number of queries, or which task id is
// passed first. Those are the implementer's to choose (SPEC §2 says "resolves
// on the named task, then one level up through parent_id" — a two-step walk and
// a single self-join are both faithful), and a fake that has to know the SQL is
// a test of the fake.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The seam must be satisfied by the two things the callers actually hold, with
// no adapter. If either of these stops compiling, the seam has been narrowed to
// something only one caller can pass.
var (
	_ Querier = (*pgxpool.Pool)(nil)
	_ Querier = (pgx.Tx)(nil)
)

// emptyRow answers every query with pgx.ErrNoRows, whatever the SQL and however
// many times it is asked. That is what makes this test independent of the query
// shape: a task with no provenance, whose parent has none either, is the state
// EVERY task in production is in today (SPEC: "Backfill: none, and none is
// possible" — nothing recorded which message raised the existing tasks).
type emptyRow struct{}

func (emptyRow) Scan(_ ...any) error { return pgx.ErrNoRows }

type emptyQuerier struct{ calls int }

func (q *emptyQuerier) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	q.calls++
	return emptyRow{}
}

// Criterion 4's third clause: absent provenance is `found=false`, not an error.
//
// This distinction is load-bearing in both callers and in opposite directions.
// In `drafts` it must fall through to `Channel == ""` and the existing
// "unresolvable — tell the human" log (criterion 8) rather than failing the
// whole DeliverTasks scan for every other task in the queue. In
// `draft_delivery` it must produce the SPEC's specific refusal naming
// `task_set_source_thread` (criterion 10) rather than an opaque "no rows in
// result set" that reads like a database fault.
func TestTaskSourceThread_AbsentProvenanceIsNotAnError(t *testing.T) {
	q := &emptyQuerier{}

	got, found, err := TaskSourceThread(context.Background(), q, 4242)
	if err != nil {
		t.Fatalf("TaskSourceThread with no provenance anywhere returned an error: %v. Criterion 4: it returns "+
			"found=false. pgx.ErrNoRows must be absorbed here — every task in production is in this state "+
			"today (no backfill is possible), so surfacing it as an error fails the drafts scan wholesale", err)
	}
	if found {
		t.Errorf("found = true from a querier that returns no rows for anything; the fixture supplies no "+
			"provenance, so the only honest answer is false (got %+v)", got)
	}
	if got != (SourceThread{}) {
		t.Errorf("SourceThread = %+v, want the zero value when found is false — a caller that ignores `found` "+
			"must not receive a thread id it can act on", got)
	}
	if q.calls == 0 {
		t.Errorf("TaskSourceThread asked the querier NOTHING. A resolver that short-circuits before reading " +
			"tasks.source_thread_id would report 'no provenance' for every task forever, which is the safe " +
			"answer and a completely inert function")
	}
}

// The struct is part of the contract: `drafts` needs the KEY (to parse with
// upworkcrm.ParseThreadKey), `draft_delivery` needs the ID (to store in
// deliveries.thread_id, whose FK subsumes the old EXISTS check — SPEC §4 step 5
// and D5), and both want the task the value was found ON, which is not
// necessarily the task that was asked about (the parent walk).
func TestSourceThread_CarriesIDKeyAndOwningTask(t *testing.T) {
	st := SourceThread{TaskID: 7, ThreadID: 91, ThreadKey: "upwork_crm:aaaa-bbbb:room:room_1a2b"}
	if st.TaskID != 7 || st.ThreadID != 91 || st.ThreadKey == "" {
		t.Fatalf("SourceThread lost a field: %+v", st)
	}
}
