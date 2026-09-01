//go:build integration

package drafts_test

// SWT-20 acceptance criteria 5, 6, 7 and 8: `internal/drafts` resolves an upwork
// delivery target from the TASK'S RECORDED SOURCE THREAD and from nothing else.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run DraftsProvenance ./internal/drafts/
//
// WHAT CHANGES, in one sentence: SWT-19 had to CHOOSE a room among the client's
// threads and, when there were several, refused; SWT-20 records which
// conversation raised the task, so there is nothing left to choose.
//
// GREENFIELD NOTE: red twice over today. `tasks.source_thread_id` does not exist
// until migration 0019 is applied to the compose db ("column source_thread_id
// does not exist" is the expected first failure), and once it does,
// `PGStore.resolve` still ignores it — it resolves through `external_refs` and
// then runs the client-wide candidate scan in `upworkTarget`.
//
// TESTS THIS FILE REPLACES — read before "fixing" a failure in the sibling file.
// Two cases in store_integration_test.go assert the behaviour criterion 5
// DELETES, and they cannot both be right:
//
//   - TestDraftsStore_Integration_UpworkPrefersTheRoomedThread asserts that a
//     task whose ref names the LEGACY key resolves to the client's roomed
//     thread. Criterion 7 asserts the exact opposite for a task whose
//     PROVENANCE is legacy, and the SPEC deletes the roomed preference outright
//     (§3, store.go:292-362).
//   - TestDraftsStore_Integration_UpworkTargetIsTheThreadKey resolves a target
//     from an external_refs row with no provenance at all; criterion 8 makes
//     that state the REFUSAL.
//
// Both are SWT-19-era and are the implementer's to delete or re-point at
// provenance. They are deliberately left standing here so the reversal is a
// visible decision rather than a quiet edit.
//
// Cross-suite discipline: this file owns nothing of its own. It reuses the
// itest-dstore-% projects, client uuids and thread keys already in
// store_integration_test.go's cleanup pact (dsCleanup), which is why dsRoomC was
// added there rather than a fourth key introduced here.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// dsSetProvenance records the conversation that raised a task, the way
// `task_set_source_thread` will.
//
// Written with a direct UPDATE rather than through the executor ON PURPOSE:
// this file is about what the DRAFTS RESOLVER does with the column, not about
// how the column comes to be set. The tool's own contract — idempotent replay,
// refusal to re-aim, the FK — is criterion 3's, in
// internal/tools/provenance_integration_test.go. Keeping them apart means a
// failure here names the resolver and a failure there names the tool.
func dsSetProvenance(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID, threadID int64) {
	t.Helper()
	tag, err := pool.Exec(ctx, `UPDATE tasks SET source_thread_id=$2 WHERE id=$1`, taskID, threadID)
	if err != nil {
		t.Fatalf("set provenance on task %d: %v", taskID, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("set provenance on task %d touched %d rows, want 1", taskID, tag.RowsAffected())
	}
}

// ---- criterion 6: the mitigation's cost is removed ------------------------------

// SWT-19 shipped a MITIGATION here, and this test is its deliberate reversal.
//
// Quoting SWT-19's own comment (store.go:335-351): "this client has several
// Upwork rooms and the ref this task carries names only the client. Picking the
// most recent is a GUESS... So refuse to target it... Two production clients are
// in this state (3 rooms and 2 rooms)." Refusing was right while nothing on the
// task said which conversation it came from. It cost those two clients every
// automated Upwork delivery.
//
// The fixture is THE SAME ONE the mitigation refused — a client with several
// roomed threads and a task whose external_refs row names only the legacy key —
// with one fact added: `tasks.source_thread_id`. That fact is what turns a guess
// into a lookup, so the refusal is no longer the safe choice; it is a pointless
// one.
//
// Three rooms, and the recorded one is the OLDEST with the STALEST traffic, so
// none of the deleted heuristics ("prefer roomed", "newest max(sent_at)",
// "highest id") can produce the right answer by accident.
func TestDraftsProvenance_Integration_RoomedProvenanceBeatsTheMultiRoomRefusal(t *testing.T) {
	ctx := context.Background()
	f := newDSFixture(t, ctx)

	client := dsClientTwoRooms // three rooms below; the name is the sibling file's
	legacy := dsLegacyKey(client)
	roomA := dsRoomedKey(client, dsRoomA)
	roomB := dsRoomedKey(client, dsRoomB)
	roomC := dsRoomedKey(client, dsRoomC)
	for i, a := range []string{legacy, roomA, roomB, roomC} {
		for j, b := range []string{legacy, roomA, roomB, roomC} {
			if i != j && a == b {
				t.Fatalf("fixture invalid: two of the four thread keys are the same string (%q)", a)
			}
		}
	}

	f.thread(t, ctx, legacy, 5000)              // ancient
	roomAThread := f.thread(t, ctx, roomA, 900) // the recorded one: OLDEST roomed traffic
	f.thread(t, ctx, roomB, 60)
	f.thread(t, ctx, roomC, 5) // newest — what "most recent room" would pick

	deliverID, parentID, _ := f.project(t, ctx, projectSpec{
		name: "prov-multiroom", client: "Multi Room Client",
		refSystem: "upwork_crm", refKey: legacy,
	})
	dsSetProvenance(t, ctx, f.pool, parentID, roomAThread)

	dt := deliverTask(t, ctx, f.pool, deliverID)
	if dt.Channel != "upwork_chat" {
		t.Fatalf("Channel = %q, want upwork_chat. Under SWT-19's mitigation this fixture resolved to \"\" "+
			"(refused) because the client has several rooms; with provenance recorded there is nothing to "+
			"choose and refusing costs the two production multi-room clients every automated delivery",
			dt.Channel)
	}
	if dt.TargetRef != roomA {
		t.Errorf("TargetRef = %q, want the RECORDED room %q. Rooms B (%q) and C (%q) carry newer traffic and "+
			"higher thread ids, so any surviving 'prefer roomed / newest wins / highest id' heuristic lands on "+
			"one of them — and since SWT-19 a wrong-room delivery can NEVER confirm, surfacing only via the "+
			"reconciler ~45 minutes later", dt.TargetRef, roomA, roomB, roomC)
	}
}

// ---- criterion 5: provenance is the ONLY route -----------------------------------

// No external_refs row at all, and the target still resolves.
//
// This is the property criterion 5 actually asserts, and it is the one that
// makes the SPEC's D1 argument real: `external_refs` cannot be the provenance
// (agent-writable free text, a mutable join key, and UNIQUE on (system,
// external_key) so a conversation can back exactly one task, forever). If the
// resolver still needs a ref to work, the second task raised from one
// conversation is undeliverable and an agent can still aim a delivery by
// writing one.
func TestDraftsProvenance_Integration_ResolvesWithNoExternalRefAtAll(t *testing.T) {
	ctx := context.Background()
	f := newDSFixture(t, ctx)

	roomed := dsRoomedKey(dsClientOneRoom, dsRoomA)
	roomedThread := f.thread(t, ctx, roomed, 30)

	// refSystem deliberately empty: the sibling fixture's only route is omitted.
	deliverID, parentID, _ := f.project(t, ctx, projectSpec{
		name: "prov-noref", client: "Refless Client",
	})
	dsSetProvenance(t, ctx, f.pool, parentID, roomedThread)

	dt := deliverTask(t, ctx, f.pool, deliverID)
	if dt.Channel != "upwork_chat" {
		t.Fatalf("Channel = %q, want upwork_chat derived from the RECORDED thread's key. With no external_refs "+
			"row the pre-SWT-20 resolver finds no thread and yields \"\" — which is exactly why provenance "+
			"cannot live in external_refs (SPEC D1: one task per key, forever)", dt.Channel)
	}
	if dt.TargetRef != roomed {
		t.Errorf("TargetRef = %q, want %q", dt.TargetRef, roomed)
	}
}

// ---- criterion 7: LEGACY provenance resolves to the LEGACY key --------------------

// The truthful statement is "this client's conversation, room not recorded by
// the source" — not "whichever room looks most alive".
//
// D6: the legacy key IS the recorded conversation, SameConversation's legacy
// tolerance keeps it confirmable, and the send is a human paste on the assisted
// tier. Re-aiming it at a room the task was never raised in would be the
// wrong-room send this whole ticket exists to prevent, dressed up as a
// convenience.
//
// The roomed thread here carries NEWER messages and a HIGHER id than the legacy
// one, which is precisely the ordering SWT-19's deleted `max(m.sent_at) DESC
// NULLS LAST, t.id DESC` was built to exploit.
func TestDraftsProvenance_Integration_LegacyProvenanceStaysOnTheLegacyKey(t *testing.T) {
	ctx := context.Background()
	f := newDSFixture(t, ctx)

	legacy := dsLegacyKey(dsClientOneRoom)
	roomed := dsRoomedKey(dsClientOneRoom, dsRoomA)
	if legacy == roomed {
		t.Fatalf("fixture invalid: the two keys are the same string (%q)", legacy)
	}
	legacyThread := f.thread(t, ctx, legacy, 900) // old traffic, created first
	roomedThread := f.thread(t, ctx, roomed, 10)  // current traffic, higher id
	if !(legacyThread < roomedThread) {
		t.Fatalf("fixture invalid: the legacy thread (%d) must be created BEFORE the roomed one (%d), or "+
			"'stay on the recorded thread' coincides with 'lowest id wins'", legacyThread, roomedThread)
	}

	deliverID, parentID, _ := f.project(t, ctx, projectSpec{
		name: "prov-legacy", client: "Legacy Client",
		refSystem: "upwork_crm", refKey: legacy,
	})
	dsSetProvenance(t, ctx, f.pool, parentID, legacyThread)

	dt := deliverTask(t, ctx, f.pool, deliverID)
	if dt.Channel != "upwork_chat" {
		t.Fatalf("Channel = %q, want upwork_chat", dt.Channel)
	}
	if dt.TargetRef != legacy {
		t.Errorf("TargetRef = %q, want the recorded LEGACY key %q — NOT the roomed thread %q, which has newer "+
			"messages and a higher id. SWT-19's roomed preference is deleted by criterion 5; keeping it means "+
			"replying in a room the task was never raised in", dt.TargetRef, legacy, roomed)
	}
}

// ---- criterion 8: no provenance -> refuse, and SWT-19's fallback survives ----------

// A task with NO recorded source thread yields `Channel == ""` and reaches
// drafts.go's existing "unresolvable — tell the human" log. No delivery is
// created; nothing is guessed.
//
// This is the ONLY fallback after SWT-20, and it is strictly stronger than
// SWT-19's multi-room refusal: the refusal no longer depends on how many rooms
// the client happens to have. The second subtest keeps the SWT-19 case itself
// pinned — a multi-room client with no provenance must still refuse, and after
// this ticket it refuses for the more general reason.
//
// Every task that exists in production today is in this state and stays there:
// nothing recorded which message raised them, and the SPEC is explicit that no
// backfill is possible. Refusing is reversible; a wrong-room send is not.
func TestDraftsProvenance_Integration_NoProvenanceRefuses(t *testing.T) {
	ctx := context.Background()
	f := newDSFixture(t, ctx)

	t.Run("single-room client, ref present, provenance absent", func(t *testing.T) {
		// The sibling suite's TestDraftsStore_Integration_UpworkTargetIsTheThreadKey
		// resolves this exact shape to a target today. Criterion 8 makes it a
		// refusal: an external_refs row is an agent-writable claim, not a
		// recorded observation (D1).
		key := dsLegacyKey(dsClientLegacyOnly)
		f.thread(t, ctx, key, 30)
		deliverID, parentID, _ := f.project(t, ctx, projectSpec{
			name: "prov-none", client: "Unprovenanced Client",
			refSystem: "upwork_crm", refKey: key,
		})

		var has bool
		if err := f.pool.QueryRow(ctx,
			`SELECT source_thread_id IS NOT NULL FROM tasks WHERE id=$1`, parentID).Scan(&has); err != nil {
			t.Fatalf("read provenance: %v", err)
		}
		if has {
			t.Fatalf("fixture invalid: the work task %d already carries provenance", parentID)
		}

		dt := deliverTask(t, ctx, f.pool, deliverID)
		if dt.Channel != "" || dt.TargetRef != "" {
			t.Errorf("resolved (Channel %q, TargetRef %q) for a task that records NO source conversation; want "+
				"a refusal (Channel=\"\", TargetRef=\"\") routing to drafts.go's 'unresolvable — tell the "+
				"human' log. An external_refs row proves an agent wrote a key, not that the task came from "+
				"that conversation (SPEC D1)", dt.Channel, dt.TargetRef)
		}
	})

	t.Run("multi-room client, provenance absent (SWT-19's case, still refused)", func(t *testing.T) {
		legacy := dsLegacyKey(dsClientTwoRooms)
		f.thread(t, ctx, legacy, 900)
		f.thread(t, ctx, dsRoomedKey(dsClientTwoRooms, dsRoomA), 40)
		f.thread(t, ctx, dsRoomedKey(dsClientTwoRooms, dsRoomB), 10)

		deliverID, _, _ := f.project(t, ctx, projectSpec{
			name: "prov-none-multi", client: "Multi Room Client",
			refSystem: "upwork_crm", refKey: legacy,
		})

		dt := deliverTask(t, ctx, f.pool, deliverID)
		if dt.Channel != "" || dt.TargetRef != "" {
			t.Errorf("resolved (Channel %q, TargetRef %q) for a multi-room client with no provenance. SWT-19's "+
				"refusal must SURVIVE as the fallback — after this ticket it fires for the stronger reason "+
				"(no recorded conversation) rather than for the room count, but a wrong-room send is still "+
				"irreversible and still invisible until the reconciler runs", dt.Channel, dt.TargetRef)
		}
	})
}
