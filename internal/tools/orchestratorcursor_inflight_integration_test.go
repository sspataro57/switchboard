//go:build integration

package tools_test

// SWT-41 review (Codex, high): BIGSERIAL allocation and commit order are
// independent. A writer that took a LOWER task_events id and has not committed
// yet, while a higher id commits, leaves max(id) above an event that is not
// visible. An advance to that max would skip the lower event forever once it
// commits. orchestrator_cursor_advance takes LOCK TABLE task_events IN SHARE
// MODE, which waits for in-flight inserts, so its head is a real watermark.
//
// MUTATION THAT MUST TURN THIS RED: drop the LOCK TABLE statement in
// orchestratorcursor.go -> the advance returns while tx A is still open, and
// its histogram misses A's event.

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestOrchestratorCursorAdvance_Integration_WaitsForInFlightEventWriters(t *testing.T) {
	ctx := context.Background()
	s := newOCSuite(t, ctx)
	x := s.head(t, ctx)
	s.setCursor(t, ctx, x)

	// Writer A takes the LOWER id and stays open.
	txA, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx A: %v", err)
	}
	defer func() { _ = txA.Rollback(context.Background()) }()
	var idA int64
	if err := txA.QueryRow(ctx,
		`INSERT INTO task_events (task_id, event_type, payload) VALUES ($1,'log','{}') RETURNING id`,
		s.task).Scan(&idA); err != nil {
		t.Fatalf("tx A insert: %v", err)
	}
	// Writer B commits a HIGHER id.
	idB := s.event(t, ctx, "status_changed")
	if idB <= idA {
		t.Fatalf("PREMISE: B's id %d is not above A's %d", idB, idA)
	}

	type result struct {
		out advanceOut
		err error
	}
	done := make(chan result, 1)
	go func() {
		raw, err := s.advance(ctx, ocHuman, x, "itest in-flight writer")
		var out advanceOut
		if err == nil {
			err = json.Unmarshal(raw, &out)
		}
		done <- result{out, err}
	}()

	select {
	case r := <-done:
		t.Fatalf("the advance returned (%+v, err=%v) while event %d was still uncommitted below head %d: "+
			"the cursor would skip it forever once it commits", r.out, r.err, idA, idB)
	case <-time.After(500 * time.Millisecond):
		// Expected: blocked on the SHARE lock behind tx A.
	}
	if err := txA.Commit(ctx); err != nil {
		t.Fatalf("commit tx A: %v", err)
	}

	var r result
	select {
	case r = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the advance never finished after tx A committed")
	}
	if r.err != nil {
		t.Fatalf("advance after tx A committed: %v", r.err)
	}
	if r.out.To < idB {
		t.Errorf("advance to=%d, want at least %d", r.out.To, idB)
	}
	var committed int64
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM task_events WHERE id > $1 AND id <= $2`, x, r.out.To).Scan(&committed); err != nil {
		t.Fatalf("independent count: %v", err)
	}
	if r.out.SkippedTotal != committed {
		t.Errorf("skipped_total = %d, but %d events now sit in (%d, %d]: the advance skipped event %d without "+
			"counting it", r.out.SkippedTotal, committed, x, r.out.To, idA)
	}
}
