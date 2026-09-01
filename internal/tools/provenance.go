package tools

// task_set_source_thread (SWT-20): records which conversation raised a task.
//
// SPINE-FACING, deliberately NOT in internal/mcpserver/schemas.go — the same
// shape as capture_rule_add. This tool writes the fact that DECIDES where a
// task's deliveries may be aimed; an agent with a transport to it could aim
// them itself, which is exactly the exposure the old upwork_chat closure was
// written for (and why external_refs — agent-facing free text — was rejected
// as the provenance store, SPEC D1). The protection is structural: no agent
// has a transport. It is NOT humanOnly either: the capture engine
// (capture:{connector}) is its main caller, and a human-only gate would make
// the funnel's own observer illegal.
//
// Semantics (SPEC §1, D7): replaying the SAME thread id is an idempotent
// success — capture passes re-run (--all, overlapping CronJobs) and an error
// on the second write would fail the whole pass. A DIFFERENT thread id on a
// task that already carries one is REFUSED for everyone, naming the current
// value: provenance is a recorded observation, not a mutable pointer, and an
// overwrite silently re-aims every future delivery of the task. A genuine
// correction is a psql statement plus a note, not a verb.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type setSourceThreadArgs struct {
	TaskID   int64 `json:"task_id"`
	ThreadID int64 `json:"thread_id"`
}

// validateSetSourceThread refuses a half-specified write before the handler:
// zero is spelled out rather than left to the FK because {"task_id":0} is what
// a caller that forgot to unmarshal a result sends, and the FK would reject it
// only after a policy check and two audit rows, with an error about a foreign
// key rather than about the argument.
func validateSetSourceThread(args []byte) error {
	var a setSourceThreadArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID <= 0 {
		return errors.New("missing or zero task_id")
	}
	if a.ThreadID <= 0 {
		return errors.New("missing or zero thread_id")
	}
	return nil
}

func taskSetSourceThread(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a setSourceThreadArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	var cur *int64
	err := pool.QueryRow(ctx, `SELECT source_thread_id FROM tasks WHERE id=$1`, a.TaskID).Scan(&cur)
	if errors.Is(err, pgx.ErrNoRows) {
		// A silent success here would tell a capture pass that provenance was
		// recorded; the missing fact would then surface only as a
		// draft_delivery refusal much later, somewhere else.
		return nil, fmt.Errorf("task %d does not exist", a.TaskID)
	}
	if err != nil {
		return nil, fmt.Errorf("read task %d provenance: %w", a.TaskID, err)
	}
	if cur != nil {
		if *cur == a.ThreadID {
			// Idempotent replay.
			return marshalResult(map[string]any{"task_id": a.TaskID, "thread_id": a.ThreadID})
		}
		return nil, fmt.Errorf("task %d already records source thread %d; provenance is a recorded "+
			"observation, not a mutable pointer — re-aiming it would silently re-point every future "+
			"delivery of this task (SWT-20 D7). A genuine correction is a psql UPDATE plus a note",
			a.TaskID, *cur)
	}

	// The FK to normalized_threads does the thread-existence work: an unknown
	// thread id fails here, loudly, and the task is left untouched.
	tag, err := pool.Exec(ctx,
		`UPDATE tasks SET source_thread_id=$2, updated_at=now()
		  WHERE id=$1 AND source_thread_id IS NULL`, a.TaskID, a.ThreadID)
	if err != nil {
		return nil, fmt.Errorf("set source thread %d on task %d: %w", a.ThreadID, a.TaskID, err)
	}
	if tag.RowsAffected() != 1 {
		return nil, fmt.Errorf("task %d's provenance was set concurrently; re-read before retrying", a.TaskID)
	}
	return marshalResult(map[string]any{"task_id": a.TaskID, "thread_id": a.ThreadID})
}
