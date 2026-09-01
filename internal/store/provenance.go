package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Querier is the pgx-shaped minimum both provenance readers already hold: a
// *pgxpool.Pool (the drafts resolver) and a pgx.Tx (the delivery handler runs
// inside a transaction) both satisfy it with no adapter.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// SourceThread is a task's recorded source conversation (SWT-20).
//
// TaskID is the task the observation was found ON — not necessarily the task
// that was asked about, because of the parent walk — so a caller can tell an
// inherited provenance from an own one. ThreadKey is what drafts parses and
// what draft_delivery compares the caller's target_ref against; ThreadID is
// what deliveries.thread_id stores (its FK subsumes the old EXISTS probe).
type SourceThread struct {
	TaskID    int64
	ThreadID  int64
	ThreadKey string
}

// TaskSourceThread is the ONE reader of tasks.source_thread_id. Both
// internal/drafts and internal/tools/delivery.go call it: two spellings of the
// provenance query is the drift this repo has paid for four times, and this
// package exists for exactly this consolidation (UnconfirmedNoteMarker).
//
// It resolves on the named task, then one level up through parent_id —
// draft_delivery is called with the WORK task id by the drafts worker, and a
// human may call it on the R3 Deliver child (SWT-20 premise 8). Two lookups,
// bounded, the same coverage drafts.taskThread has.
//
// Absent provenance ANYWHERE is (zero, false, nil) — never an error. Every
// task that predates SWT-20 is in that state forever (no backfill is
// possible), so an error here would fail the whole drafts scan on the first
// unprovenanced task it meets; the callers turn `false` into their own
// channel-appropriate refusal.
func TaskSourceThread(ctx context.Context, q Querier, taskID int64) (SourceThread, bool, error) {
	cur := taskID
	for depth := 0; depth < 2; depth++ {
		var parent, threadID *int64
		var key *string
		err := q.QueryRow(ctx,
			`SELECT t.parent_id, t.source_thread_id, nt.thread_key
			   FROM tasks t
			   LEFT JOIN normalized_threads nt ON nt.id = t.source_thread_id
			  WHERE t.id = $1`, cur).Scan(&parent, &threadID, &key)
		if errors.Is(err, pgx.ErrNoRows) {
			// The task does not exist (or, on the second pass, the parent id
			// dangles). Nothing recorded is the honest answer either way.
			return SourceThread{}, false, nil
		}
		if err != nil {
			return SourceThread{}, false, fmt.Errorf("resolve source thread for task %d: %w", cur, err)
		}
		if threadID != nil {
			k := ""
			if key != nil {
				k = *key
			}
			return SourceThread{TaskID: cur, ThreadID: *threadID, ThreadKey: k}, true, nil
		}
		if parent == nil {
			return SourceThread{}, false, nil
		}
		cur = *parent
	}
	return SourceThread{}, false, nil
}
