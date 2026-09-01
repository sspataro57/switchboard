//go:build integration

package upworkcrm_test

// SWT-20 acceptance criterion 14: the matcher SHORTLISTS before it locks, so an
// outbound message for client A does not lock any delivery of client B.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run Shortlist ./internal/connector/upworkcrm/
//
// THE DEFECT THIS PINS (SWT-19's fourth adversarial finding, deferred here):
// `confirmUpworkDelivery` selects EVERY `upwork_chat` row at status='sent' with
// `sent_external_id IS NULL AND confirmed_at IS NULL`, across all clients,
// `ORDER BY id DESC FOR UPDATE`, and filters in Go. Two things make that worse
// than a slow query:
//
//   - the cost is O(outbound x unresolved) once the tier is live, and
//   - a row never LEAVES that set on its own. The reconciler ANNOTATES a stuck
//     delivery; it does not resolve it. So one client's permanently-stuck row is
//     locked by every other client's outbound message, forever.
//
// HOW THE TEST DETECTS IT, and why it is a lock test rather than a count: the
// only externally visible difference between "locked and then rejected in Go"
// and "never locked" is CONTENTION. So another transaction holds a row lock on
// client B's unconfirmed delivery for the whole run. Today's matcher tries to
// lock it and blocks; the shortlisted one never sees the row.
//
// The context deadline is what turns a HANG into a red test. Without it this
// case would sit until the package timeout and report as a panic in an unrelated
// test — the classic way a lock test becomes unmaintainable.
//
// MUTATION CHECK (verification protocol step 5; do it by hand once when
// implementing): delete the `target_client_ref = $1` clause from the shortlist
// and re-run. If this test still passes, it is testing nothing.
//
// GREENFIELD NOTE: red twice over today. `deliveries.target_client_ref` and
// `deliveries_upwork_identity_check` do not exist until migration 0019 is
// applied to the compose db (the seeds below insert the post-0019 fixture shape
// criterion 13 requires), and the matcher still locks every unresolved row.
//
// Mutual-cleanup pact: this suite owns projects slugged `itest-sl-%`, the two
// client uuids below and their thread keys, and the raw items prefixed
// `communications:comm-itest-sl`. Cleaned in FK order before and after. No
// global count assertions.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/upworkcrm"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	slClientA = "aaaa3030-0000-0000-0000-00000000sl01"
	slClientB = "bbbb3030-0000-0000-0000-00000000sl02"
	slRoomA   = "room_sl0a1b2c3d"
	slRoomB   = "room_sl9f8e7d6c"
	slChannel = "upwork"

	// Two DIFFERENT bodies. A matcher test whose two bodies are the same string
	// tests nothing (IK, the SWT-16 entry): with one shared body, client B's row
	// would be a body-match too and the test could not tell "not locked" from
	// "locked and rejected on the body".
	slBodyA = "Staging is green and the importer finished its first full pass; numbers in the morning."
	slBodyB = "Sorry for the delay — the invoice went out this afternoon, let me know if it needs splitting."

	// Long enough to be a real wait, short enough that a blocked run reports as a
	// failed assertion in seconds rather than as a package timeout.
	slDeadline = 8 * time.Second
)

func slRoomedKey(client, room string) string { return "upwork_crm:" + client + ":room:" + room }

func slOpen(t *testing.T, ctx context.Context) (*pgxpool.Pool, int64) {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use the compose db on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	slCleanup(t, ctx, pool)
	t.Cleanup(func() { slCleanup(t, ctx, pool) })

	acctID, err := upworkcrm.NewSink(pool).EnsureAccount(ctx)
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	return pool, acctID
}

func slCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	threads := []string{slRoomedKey(slClientA, slRoomA), slRoomedKey(slClientB, slRoomB)}
	raws := []string{"communications:comm-itest-sl-a", "communications:comm-itest-sl-b"}
	extIDs := []string{"story_itest_sl_a", "story_itest_sl_b"}
	const projs = `(SELECT id FROM projects WHERE slug LIKE 'itest-sl-%')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	stmts := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM task_events WHERE task_id IN ` + tasksOf, nil},
		{`DELETE FROM deliveries WHERE task_id IN ` + tasksOf, nil},
		{`DELETE FROM normalized_messages WHERE external_message_id = ANY($1)`, []any{extIDs}},
		{`DELETE FROM normalized_messages WHERE thread_id IN
		    (SELECT id FROM normalized_threads WHERE thread_key = ANY($1))`, []any{threads}},
		// tasks BEFORE normalized_threads: since migration 0019 a task points at
		// the conversation that raised it (tasks_source_thread_id_fkey), so the
		// pre-0019 order — threads first — now fails on a foreign key and takes
		// the whole suite's cleanup down with it. Children before parents, as
		// always; this ticket just added a new parent.
		{`DELETE FROM tasks WHERE project_id IN ` + projs, nil},
		{`DELETE FROM projects WHERE slug LIKE 'itest-sl-%'`, nil},
		{`DELETE FROM normalized_threads WHERE thread_key = ANY($1)`, []any{threads}},
		{`DELETE FROM raw_source_items WHERE external_id = ANY($1)`, []any{raws}},
	}
	for _, st := range stmts {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

// slSeedClient inserts one client's whole world: a thread, a project, a task and
// an assisted-tier delivery marked sent by a human.
//
// The delivery carries `target_client_ref` and `thread_id` because after
// migration 0019 `deliveries_upwork_identity_check` REQUIRES them for this
// channel (criterion 13). That is the post-0019 fixture shape every upwork
// fixture in the repo has to adopt, and it is the whole point of the constraint:
// a fixture that forgets the column is refused loudly at INSERT instead of
// silently dropping out of the shortlist.
//
// send_attempted_at is deliberately NOT seeded — IK records that seeding it is
// how an inert time-floor clause comes to look like a working fix. On the
// assisted tier nothing ever writes it.
func slSeedClient(t *testing.T, ctx context.Context, pool *pgxpool.Pool, slug, client, room, body string) (deliveryID, taskID int64) {
	t.Helper()
	ins := func(q string, args ...any) int64 {
		var id int64
		if err := pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
		return id
	}
	key := slRoomedKey(client, room)
	threadID := ins(`INSERT INTO normalized_threads (thread_key, subject, participants)
	                 VALUES ($1,'itest-sl','[]') RETURNING id`, key)
	projID := ins(`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
	               VALUES ($1,$1,$2,'manual','dashboard','/tmp/itest-sl','any') RETURNING id`, slug, slug+"-client")
	taskID = ins(`INSERT INTO tasks (project_id, title, assignee_type, status, source_thread_id)
	              VALUES ($1,'itest-sl work','claude','delivered',$2) RETURNING id`, projID, threadID)
	deliveryID = ins(`INSERT INTO deliveries
	                    (task_id, channel, target_ref, target_client_ref, thread_id, body, status, sent_external_id, sent_at)
	                  VALUES ($1,'upwork_chat',$2,$3,$4,$5,'sent',NULL, now() - interval '2 hours') RETURNING id`,
		taskID, key, client, threadID, body)
	return deliveryID, taskID
}

// slSeedOutbound seeds one OUTBOUND raw communication, roomed via send_room_id —
// the column most roomed outbound rows actually use, because our own sends
// record the room they were dispatched to.
func slSeedOutbound(t *testing.T, ctx context.Context, pool *pgxpool.Pool, acctID int64, comm, client, room, body, extID string) {
	t.Helper()
	row := map[string]any{
		"id":              comm,
		"client_id":       client,
		"direction":       "outbound",
		"channel":         slChannel,
		"subject":         nil,
		"body":            body,
		"communicated_at": time.Now().UTC().Add(-90 * time.Minute).Format(time.RFC3339),
		"sender":          "me",
		"external_id":     extID,
		"send_room_id":    room,
	}
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal raw communication: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1,$2,$3,$4)`, acctID, "communications:"+comm, raw, "itest-sl-hash-"+comm); err != nil {
		t.Fatalf("seed raw communication %s: %v", comm, err)
	}
}

func TestShortlist_Integration_ClientBsRowIsNeverLocked(t *testing.T) {
	ctx := context.Background()
	pool, acctID := slOpen(t, ctx)

	if slClientA == slClientB {
		t.Fatalf("fixture invalid: the two client ids are the same string")
	}
	if slBodyA == slBodyB {
		t.Fatalf("fixture invalid: the two bodies are the same string, so 'not locked' and 'locked but " +
			"rejected on the body' would be indistinguishable")
	}

	deliveryA, taskA := slSeedClient(t, ctx, pool, "itest-sl-a", slClientA, slRoomA, slBodyA)
	deliveryB, _ := slSeedClient(t, ctx, pool, "itest-sl-b", slClientB, slRoomB, slBodyB)

	// Client A's outbound message: the one that must confirm.
	slSeedOutbound(t, ctx, pool, acctID, "comm-itest-sl-a", slClientA, slRoomA, slBodyA, "story_itest_sl_a")

	// Hold a row lock on client B's unconfirmed delivery, on its own connection,
	// for the whole run. This is the state a permanently-stuck row is in whenever
	// a human is inspecting it, another connector run is mid-flight, or a
	// dashboard action has it open — and, once the tier is live, it is simply the
	// state of every busy moment.
	lockCtx, cancelLock := context.WithTimeout(ctx, 2*slDeadline)
	defer cancelLock()
	holder, err := pool.Begin(lockCtx)
	if err != nil {
		t.Fatalf("begin lock-holding transaction: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	var lockedID int64
	if err := holder.QueryRow(lockCtx,
		`SELECT id FROM deliveries WHERE id=$1 FOR UPDATE`, deliveryB).Scan(&lockedID); err != nil {
		t.Fatalf("lock client B's delivery %d: %v", deliveryB, err)
	}

	// Now normalize client A's message, under a deadline. A matcher that locks
	// every unresolved upwork row waits on client B's lock and dies here.
	runCtx, cancelRun := context.WithTimeout(ctx, slDeadline)
	defer cancelRun()
	if _, err := upworkcrm.Normalize(runCtx, upworkcrm.NewSink(pool), upworkcrm.Config{}); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "context deadline exceeded") {
			t.Fatalf("Normalize BLOCKED for %s on client B's locked delivery %d while confirming a message for "+
				"client %s.\nCriterion 14: the matcher must shortlist by target_client_ref BEFORE it locks "+
				"anything, so client B's row is never in the candidate set. Today it selects every unconfirmed "+
				"upwork row FOR UPDATE across all clients — and because the reconciler annotates rather than "+
				"resolves, one stuck row blocks every other client's connector run indefinitely.\n%v",
				slDeadline, deliveryB, slClientA, err)
		}
		t.Fatalf("Normalize: %v", err)
	}

	// Client A confirmed...
	var extID, confirmedAt *string
	if err := pool.QueryRow(ctx,
		`SELECT sent_external_id, confirmed_at::text FROM deliveries WHERE id=$1`, deliveryA).Scan(&extID, &confirmedAt); err != nil {
		t.Fatalf("read delivery A: %v", err)
	}
	if extID == nil || *extID != "story_itest_sl_a" {
		t.Errorf("client A's sent_external_id = %v, want %q. Shortlisting must not lose a confirmation: the "+
			"narrowing is candidate-EQUIVALENT (SameConversation excludes on a client mismatch anyway), so a "+
			"miss here means the shortlist dropped a row the rule would have accepted", extID, "story_itest_sl_a")
	}
	if confirmedAt == nil {
		t.Errorf("client A's confirmed_at is NULL after a same-room outbound message")
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_confirmed'`, taskA); got != 1 {
		t.Errorf("delivery_confirmed events on client A's task = %d, want 1", got)
	}

	// ...and client B untouched, still locked, still unconfirmed.
	var bExt, bConfirmed *string
	if err := pool.QueryRow(ctx,
		`SELECT sent_external_id, confirmed_at::text FROM deliveries WHERE id=$1`, deliveryB).Scan(&bExt, &bConfirmed); err != nil {
		t.Fatalf("read delivery B: %v", err)
	}
	if bExt != nil || bConfirmed != nil {
		t.Errorf("client B's delivery %d was stamped (sent_external_id=%v, confirmed_at=%v) by client A's "+
			"message. A shortlist that widens the candidate set is far worse than one that narrows it: a wrong "+
			"stamp burns the external id under deliveries_sent_external_idx and locks the correct row out "+
			"permanently", deliveryB, bExt, bConfirmed)
	}

	if err := holder.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatalf("release client B's lock: %v", err)
	}
}
