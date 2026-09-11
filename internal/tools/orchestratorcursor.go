package tools

// SWT-41 D1: orchestrator_cursor_advance — the one audited way a human moves
// the orchestrator's cursor forward, i.e. decides to DISCARD lifecycle events.
// humanOnly (policy.humanOnly) and deliberately NOT in
// internal/mcpserver/schemas.go: run once by hand at the first switch-on
// (docs/runbooks/orchestrator.md), never as a routine restart step — downtime
// catch-up is the engine's default and correct behaviour.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/lockkeys"
)

// OrchestratorAdvisoryLockKey is orchestratord's single-instance lock. The one
// literal lives in the import-free internal/lockkeys, which both this package
// and internal/orchestrator alias (neither may import the other).
const OrchestratorAdvisoryLockKey = lockkeys.Orchestrator

type cursorAdvanceArgs struct {
	ExpectLastEventID *int64 `json:"expect_last_event_id"`
	Reason            string `json:"reason"`
}

func parseCursorAdvance(args []byte) (cursorAdvanceArgs, error) {
	var a cursorAdvanceArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return a, fmt.Errorf("parse args: %w", err)
	}
	if a.ExpectLastEventID == nil {
		return a, errors.New("missing expect_last_event_id (the cursor value you read and are advancing from)")
	}
	if *a.ExpectLastEventID < 0 {
		return a, fmt.Errorf("expect_last_event_id %d: must be >= 0", *a.ExpectLastEventID)
	}
	if strings.TrimSpace(a.Reason) == "" {
		return a, errors.New("missing reason: discarding lifecycle events is a decision, and the audit row carries why")
	}
	return a, nil
}

func validateCursorAdvance(args []byte) error {
	_, err := parseCursorAdvance(args)
	return err
}

// orchestratorCursorAdvance sets orchestrator_cursor.last_event_id to the
// current head of task_events, iff it still equals expect_last_event_id
// (compare-and-set: a stale or second run can never skip events that arrived
// after the first). One transaction: the engine's own advisory key is taken
// as a TRANSACTION lock first — session and transaction advisory locks share
// one key space, so a running orchestratord makes it fail (refuse), and while
// this transaction holds it no engine can start mid-advance.
func orchestratorCursorAdvance(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	a, err := parseCursorAdvance(args)
	if err != nil {
		return nil, err
	}
	expect := *a.ExpectLastEventID

	out := struct {
		From          int64            `json:"from"`
		To            int64            `json:"to"`
		SkippedTotal  int64            `json:"skipped_total"`
		SkippedByType map[string]int64 `json:"skipped_by_type"`
	}{SkippedByType: map[string]int64{}}

	err = inTx(ctx, pool, func(tx pgx.Tx) error {
		var locked bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, OrchestratorAdvisoryLockKey).Scan(&locked); err != nil {
			return fmt.Errorf("take the orchestrator lock: %w", err)
		}
		if !locked {
			return errors.New("an orchestratord is running (it holds the orchestrator advisory lock); " +
				"stop orchestratord first — the cursor only moves while no engine drains")
		}

		// Codex (SWT-41 review): BIGSERIAL allocation and commit order are
		// independent. A writer holding id 100 uncommitted while id 101 commits
		// would leave max(id)=101, and the cursor set there would skip 100 forever
		// once it commits. SHARE mode conflicts with every inserting transaction's
		// ROW EXCLUSIVE lock: it waits for in-flight inserts to finish and blocks
		// new ones until this transaction commits, so max(id) below is a watermark
		// with nothing uncommitted beneath it. Bounded, so a stuck writer refuses
		// the advance instead of hanging it.
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '10s'`); err != nil {
			return fmt.Errorf("set lock timeout: %w", err)
		}
		if _, err := tx.Exec(ctx, `LOCK TABLE task_events IN SHARE MODE`); err != nil {
			return fmt.Errorf("wait for in-flight task_events writers (retry when they finish): %w", err)
		}

		var cur int64
		if err := tx.QueryRow(ctx,
			`SELECT last_event_id FROM orchestrator_cursor WHERE name='orchestrator' FOR UPDATE`).Scan(&cur); err != nil {
			return fmt.Errorf("read orchestrator cursor: %w", err)
		}
		if cur != expect {
			return fmt.Errorf("orchestrator cursor is at %d, not the expected %d; refusing — read it again and "+
				"decide against the current value", cur, expect)
		}

		var head int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(id), 0) FROM task_events`).Scan(&head); err != nil {
			return fmt.Errorf("read task_events head: %w", err)
		}
		out.From, out.To = cur, cur
		if head <= cur {
			return nil // nothing to skip; the cursor stays
		}

		rows, err := tx.Query(ctx,
			`SELECT event_type, count(*) FROM task_events WHERE id > $1 AND id <= $2 GROUP BY 1`, cur, head)
		if err != nil {
			return fmt.Errorf("histogram of skipped events: %w", err)
		}
		for rows.Next() {
			var typ string
			var n int64
			if err := rows.Scan(&typ, &n); err != nil {
				rows.Close()
				return fmt.Errorf("scan skipped-event histogram: %w", err)
			}
			out.SkippedByType[typ] = n
			out.SkippedTotal += n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate skipped-event histogram: %w", err)
		}

		if _, err := tx.Exec(ctx,
			`UPDATE orchestrator_cursor SET last_event_id=$1, updated_at=now() WHERE name='orchestrator'`, head); err != nil {
			return fmt.Errorf("advance orchestrator cursor: %w", err)
		}
		out.To = head
		return nil
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}
