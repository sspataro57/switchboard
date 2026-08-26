//go:build integration

package drafts_test

// Integration test for SWT-19 acceptance criterion 15: the draft worker's store
// resolves an upwork_chat target through upworkcrm.ParseThreadKey, preferring a
// ROOMED thread for the client and falling back to the unroomed one —
// DELIBERATELY, not by `ORDER BY id DESC`.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run UpworkRoomTarget ./internal/drafts/
//
// Why "deliberately" is the whole criterion (SPEC fact 16): store.go today is
//
//	WHERE thread_key LIKE 'upwork_crm:'||$1||':%' ORDER BY id DESC LIMIT 1
//
// and after the re-key roomed threads happen to have HIGHER ids than the legacy
// client thread, so it happens to prefer a roomed thread. Accidental correctness
// is not correctness: it inverts the moment a client's roomed thread is created
// before their legacy one — which is exactly what happens for any client whose
// first ingest is API-era, and for any corpus re-normalized in a different
// order. Both insertion orders are therefore asserted, and a fixture guard
// hard-fails if the two threads are not actually ordered as intended.
//
// It also matters downstream: the matcher's room scoping only tightens anything
// if deliveries are TARGETED at roomed threads where one exists. A drafts store
// that picks the legacy thread would keep every new delivery client-wide, and
// SWT-19 would ship as a no-op that passes its own unit tests — the SWT-18
// mistake, one layer up.
//
// GREENFIELD NOTE: fails until store.go's upwork branch stops relying on
// `ORDER BY id DESC` (and stops spelling the key format in SQL — criterion 20).
// Expected red state.
//
// Cross-suite discipline: everything is scoped to itest-durt-% / this suite's
// two client uuids, cleaned in FK order before and after, and the upwork_crm
// person_identities are deleted BY VALUE, never provider-wide — other suites own
// upwork_crm identities too.

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/drafts"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	durtSlugRoomFirst   = "itest-durt-room-first"
	durtSlugLegacyFirst = "itest-durt-legacy-first"

	// Real-shaped ids. The room ids are `room_<hex>`, the identifier space both
	// communications room columns draw from — not "chat"/"room-b", the channel
	// values SWT-18 fabricated and the source has never emitted.
	durtClientRoomFirst   = "eeee1111-0000-0000-0000-0000000durt1"
	durtClientLegacyFirst = "eeee2222-0000-0000-0000-0000000durt2"
	durtRoomA             = "room_5c4b3a2918"
	durtRoomB             = "room_77aa66bb55"
)

func durtLegacyKey(client string) string { return "upwork_crm:" + client + ":upwork" }
func durtRoomedKey(client, room string) string {
	return "upwork_crm:" + client + ":room:" + room
}

func cleanupDraftsUpworkRoom(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	slugs := []string{durtSlugRoomFirst, durtSlugLegacyFirst}
	clients := []string{durtClientRoomFirst, durtClientLegacyFirst}
	threads := []string{
		durtLegacyKey(durtClientRoomFirst), durtRoomedKey(durtClientRoomFirst, durtRoomA),
		durtLegacyKey(durtClientLegacyFirst), durtRoomedKey(durtClientLegacyFirst, durtRoomB),
	}
	stmts := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM task_events WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug = ANY($1)))`, []any{slugs}},
		{`DELETE FROM deliveries WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug = ANY($1)))`, []any{slugs}},
		{`DELETE FROM normalized_messages WHERE thread_id IN (SELECT id FROM normalized_threads WHERE thread_key = ANY($1))`, []any{threads}},
		{`DELETE FROM normalized_threads WHERE thread_key = ANY($1)`, []any{threads}},
		{`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug = ANY($1))`, []any{slugs}},
		{`DELETE FROM projects WHERE slug = ANY($1)`, []any{slugs}},
		{`DELETE FROM person_identities WHERE provider='upwork_crm' AND value = ANY($1)`, []any{clients}},
		{`DELETE FROM people WHERE id NOT IN (SELECT person_id FROM person_identities)
		    AND id NOT IN (SELECT client_person_id FROM projects WHERE client_person_id IS NOT NULL)`, nil},
	}
	for _, st := range stmts {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

// durtSeed builds one client with BOTH thread shapes, inserted in the requested
// order, plus the Deliver task the store is supposed to resolve.
func durtSeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, slug, client, room string, roomedFirst bool) (deliverTaskID, roomedThreadID, legacyThreadID int64) {
	t.Helper()

	var personID int64
	if err := pool.QueryRow(ctx, `INSERT INTO people (display_name) VALUES ($1) RETURNING id`, slug+" client").Scan(&personID); err != nil {
		t.Fatalf("seed person: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO person_identities (person_id, provider, value) VALUES ($1,'upwork_crm',$2)`,
		personID, client); err != nil {
		t.Fatalf("seed upwork identity: %v", err)
	}

	var projID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, client_person_id, policies)
		 VALUES ($1,$1,$2,'manual','dashboard','/tmp/itest',$3,'{"delivery_channel":"upwork_chat"}'::jsonb)
		 RETURNING id`, slug, slug+"-client", personID).Scan(&projID); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	insert := func(key string) int64 {
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO normalized_threads (thread_key, subject, participants)
			 VALUES ($1,'',$2) RETURNING id`, key, []byte(`[]`)).Scan(&id); err != nil {
			t.Fatalf("seed thread %s: %v", key, err)
		}
		return id
	}
	if roomedFirst {
		roomedThreadID = insert(durtRoomedKey(client, room))
		legacyThreadID = insert(durtLegacyKey(client))
	} else {
		legacyThreadID = insert(durtLegacyKey(client))
		roomedThreadID = insert(durtRoomedKey(client, room))
	}

	var parentID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'itest-durt work','claude','done_locally') RETURNING id`, projID).Scan(&parentID); err != nil {
		t.Fatalf("seed parent task: %v", err)
	}
	// The title must literally start "Deliver #" — that prefix IS the store's
	// queue predicate. Built in Go rather than with `||$2::text` in SQL, which
	// makes Postgres deduce two types for one parameter.
	deliverTitle := fmt.Sprintf("Deliver #%d", parentID)
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, parent_id, title, assignee_type, status)
		 VALUES ($1,$2,$3,'claude','ready') RETURNING id`, projID, parentID, deliverTitle).Scan(&deliverTaskID); err != nil {
		t.Fatalf("seed deliver task: %v", err)
	}
	return deliverTaskID, roomedThreadID, legacyThreadID
}

func TestUpworkRoomTarget_Integration_DraftsPrefersTheRoomedThread(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	defer pool.Close()

	cleanupDraftsUpworkRoom(t, ctx, pool)
	defer cleanupDraftsUpworkRoom(t, ctx, pool)

	cases := []struct {
		name, slug, client, room string
		roomedFirst              bool
		why                      string
	}{
		{
			name: "roomed thread inserted FIRST (lower id)", slug: durtSlugRoomFirst,
			client: durtClientRoomFirst, room: durtRoomA, roomedFirst: true,
			why: "`ORDER BY id DESC` picks the LEGACY thread here, which is why fact 16 calls today's " +
				"correctness accidental — it holds only while roomed threads happen to be created later",
		},
		{
			name: "roomed thread inserted SECOND (higher id)", slug: durtSlugLegacyFirst,
			client: durtClientLegacyFirst, room: durtRoomB, roomedFirst: false,
			why: "the mirror ordering: roomed-ness, not insertion order, must be what decides",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deliverID, roomedThreadID, legacyThreadID := durtSeed(t, ctx, pool, tc.slug, tc.client, tc.room, tc.roomedFirst)

			// Fixture validity, hard-failed: the two threads must be DISTINCT
			// rows and ordered as the case name claims, or the case proves
			// nothing about preference.
			if roomedThreadID == legacyThreadID {
				t.Fatalf("fixture invalid: the roomed and legacy threads are the same row (%d)", roomedThreadID)
			}
			if tc.roomedFirst && !(roomedThreadID < legacyThreadID) {
				t.Fatalf("fixture invalid: roomed thread id %d is not lower than the legacy %d, so `ORDER BY id DESC` "+
					"would already pick the roomed one and this case cannot fail", roomedThreadID, legacyThreadID)
			}
			if !tc.roomedFirst && !(legacyThreadID < roomedThreadID) {
				t.Fatalf("fixture invalid: legacy thread id %d is not lower than the roomed %d", legacyThreadID, roomedThreadID)
			}

			tasks, err := drafts.NewStore(pool).DeliverTasks(ctx, drafts.Config{})
			if err != nil {
				t.Fatalf("DeliverTasks: %v", err)
			}
			var got *drafts.DeliverTask
			for i := range tasks {
				if tasks[i].DeliverTaskID == deliverID {
					got = &tasks[i]
					break
				}
			}
			if got == nil {
				t.Fatalf("Deliver task %d not returned by DeliverTasks; the fixture is not reaching the queue", deliverID)
			}
			if got.Channel != "upwork_chat" {
				t.Fatalf("channel = %q, want upwork_chat", got.Channel)
			}
			want := durtRoomedKey(tc.client, tc.room)
			if got.TargetRef != want {
				t.Errorf("target_ref = %q, want the ROOMED key %q — %s.\nThe matcher's room scoping only tightens "+
					"anything if deliveries are targeted at roomed threads where one exists; targeting the legacy "+
					"key keeps every new delivery client-wide and makes SWT-19 a no-op that passes its own unit tests",
					got.TargetRef, want, tc.why)
			}
		})
	}
}

// The fallback half: a client with ONLY the legacy thread still resolves — the
// 2,009 legacy messages are a real corpus, and §4's mismatch-only-excludes rule
// keeps an unroomed target confirmable. Reusing the legacy-first fixture's
// client with its roomed thread removed keeps this scoped to the suite.
func TestUpworkRoomTarget_Integration_FallsBackToTheLegacyThread(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	defer pool.Close()

	cleanupDraftsUpworkRoom(t, ctx, pool)
	defer cleanupDraftsUpworkRoom(t, ctx, pool)

	deliverID, roomedThreadID, _ := durtSeed(t, ctx, pool, durtSlugLegacyFirst, durtClientLegacyFirst, durtRoomB, false)
	if _, err := pool.Exec(ctx, `DELETE FROM normalized_threads WHERE id=$1`, roomedThreadID); err != nil {
		t.Fatalf("drop the roomed thread: %v", err)
	}

	tasks, err := drafts.NewStore(pool).DeliverTasks(ctx, drafts.Config{})
	if err != nil {
		t.Fatalf("DeliverTasks: %v", err)
	}
	for _, dt := range tasks {
		if dt.DeliverTaskID != deliverID {
			continue
		}
		if want := durtLegacyKey(durtClientLegacyFirst); dt.TargetRef != want {
			t.Errorf("target_ref = %q, want the legacy key %q: a client with no roomed thread must still be "+
				"deliverable — pre-2026-07-21 history is most of the corpus", dt.TargetRef, want)
		}
		return
	}
	t.Fatalf("Deliver task %d not returned by DeliverTasks", deliverID)
}
