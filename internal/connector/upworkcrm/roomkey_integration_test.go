//go:build integration

package upworkcrm_test

// Integration tests for the SWT-19 re-key: one thread per (client, room),
// whichever room column supplied it; one thread for the room-less rows under the
// UNCHANGED legacy key; and a re-normalize that moves the roomed messages while
// leaving the legacy thread row itself alone.
//
// Acceptance criteria 5, 6 and 23.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run RoomKey ./internal/connector/upworkcrm/
//
// Everything runs through the real path: raw_source_items in, upworkcrm.Normalize,
// canonical rows out. No upwork_crm source db, no network — Normalize reads raw
// only (criterion 4, invariant 1).
//
// ON CRITERION 23's NUMBERS. The SPEC once named 432 roomed / 2,009 legacy /
// 2,441 total as literals. Those were true on 2026-08-26 and are already false:
// connector-upworkcrm ingests every 15 minutes, so the corpus grows daily and a
// frozen literal would fail on any day a client sends a message. The property
// that made the equality worth having is preserved in a better form here:
//
//   - the fixture corpus below is OWNED by this test, so its expected counts are
//     exact literals and cannot drift; and
//   - the same assertion is ALSO made against a count computed independently in
//     SQL — COALESCE(NULLIF(upwork_room_id,''), NULLIF(send_room_id,'')) over the
//     very rows Go just normalized. Two independent computations of the same
//     number, so a one-column implementation still fails: it would produce the
//     upwork_room_id-only count against a coalesce-derived expectation.
//
// The NULLIFs are not decoration — Go treats an empty string as absent, and the
// SQL must ask the same question or the two sides drift by exactly the rows the
// question is about.
//
// GREENFIELD NOTE: fails until normalize.go reads BOTH room columns and keys the
// thread on them. Before that every message lands on the legacy key and the
// roomed counts are 0. Expected red state.
//
// Cross-suite discipline (IK "integration suites cross-pollute"): every row is
// under this suite's own client uuids / itest-rk-* external ids, cleaned in FK
// order before AND after, by explicit key lists rather than a provider-wide
// prefix. The shared upwork_crm source account is never deleted.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/upworkcrm"
	"github.com/sspataro57/switchboard/internal/store"
)

// Two multi-room clients, mirroring production: one with THREE rooms and one
// with two. Both exist on the real corpus (43431d4c-… has 3, e2ef9b65-… has 2),
// and the three-room client is the stronger demonstration — today it is one
// undifferentiated thread.
const (
	rkClient3 = "aaaa3333-0000-0000-0000-00000000rk03" // three rooms
	rkClient2 = "aaaa2222-0000-0000-0000-00000000rk02" // two rooms
	rkClient0 = "aaaa0000-0000-0000-0000-00000000rk00" // no room ever named

	// `room_<hex>`: the identifier space BOTH columns draw from. Not "chat" or
	// "room-b" — those are channel values the source has never emitted, and
	// building a room fixture out of them is how SWT-18 proved nothing.
	rkRoomA = "room_0a1b2c3d4e"
	rkRoomB = "room_5f6e7d8c9b"
	rkRoomC = "room_112233aabb"
	rkRoomD = "room_ccddee0011"
	rkRoomE = "room_2244668800"

	rkChannel = "upwork" // the only value communications.channel has ever held
)

// roomCol says which column carries the room for a fixture row. Both spellings
// occur in production and the point of the ticket is that they are equivalent.
type roomCol int

const (
	rkNoRoom roomCol = iota
	rkObserved
	rkDispatched
)

// The fixture corpus. Exact by construction: this test owns these rows.
//
//	client   room     column        rows
//	3-room   A        observed        2
//	3-room   B        dispatched      2
//	3-room   C        observed        1
//	3-room   -        -               2
//	2-room   D        dispatched      1
//	2-room   E        observed        1
//	2-room   -        -               1
//	0-room   -        -               2
//	                            total 12, roomed 7, legacy 5
//
// Reading `upwork_room_id` ALONE would see 4 roomed, not 7 — asserted below as a
// fixture-validity guard, because a corpus where the two numbers agree cannot
// distinguish a correct implementation from the one-column bug.
var rkCorpus = []struct {
	client, room string
	col          roomCol
	direction    string
}{
	{rkClient3, rkRoomA, rkObserved, "inbound"},
	{rkClient3, rkRoomA, rkObserved, "outbound"},
	{rkClient3, rkRoomB, rkDispatched, "outbound"},
	{rkClient3, rkRoomB, rkDispatched, "outbound"},
	{rkClient3, rkRoomC, rkObserved, "inbound"},
	{rkClient3, "", rkNoRoom, "inbound"},
	{rkClient3, "", rkNoRoom, "outbound"},
	{rkClient2, rkRoomD, rkDispatched, "outbound"},
	{rkClient2, rkRoomE, rkObserved, "inbound"},
	{rkClient2, "", rkNoRoom, "inbound"},
	{rkClient0, "", rkNoRoom, "inbound"},
	{rkClient0, "", rkNoRoom, "outbound"},
}

const (
	rkWantTotal            = 12
	rkWantRoomed           = 7
	rkWantLegacy           = 5
	rkWantRoomedThreads    = 5 // A,B,C for the 3-room client + D,E for the 2-room one
	rkWantLegacyThreads    = 3 // one per client that has a room-less row
	rkWantOneColumnRoomed  = 4 // what a upwork_room_id-only implementation would find
	rkWantThreadsOfClient3 = 3
)

func rkExternalID(i int) string {
	return fmt.Sprintf("communications:aaaa%04d-0000-0000-0000-00000000rk%02d", i, i)
}
func rkMessageID(i int) string { return fmt.Sprintf("story_itest_rk_%03d", i) }

func rkLegacyKey(client string) string { return "upwork_crm:" + client + ":" + rkChannel }
func rkRoomedKey(client, room string) string {
	return "upwork_crm:" + client + ":room:" + room
}

// rkAllThreadKeys enumerates every key this suite can produce, in both shapes,
// so cleanup never needs a provider-wide prefix (criterion 20 keeps LIKE on the
// key out of SQL; enumerating is also simply safer next to other suites).
func rkAllThreadKeys() []string {
	keys := []string{
		rkLegacyKey(rkClient3), rkLegacyKey(rkClient2), rkLegacyKey(rkClient0),
	}
	for _, r := range []string{rkRoomA, rkRoomB, rkRoomC} {
		keys = append(keys, rkRoomedKey(rkClient3, r))
	}
	for _, r := range []string{rkRoomD, rkRoomE} {
		keys = append(keys, rkRoomedKey(rkClient2, r))
	}
	return keys
}

func rkAllExternalIDs() (raw, msg []string) {
	for i := range rkCorpus {
		raw = append(raw, rkExternalID(i))
		msg = append(msg, rkMessageID(i))
	}
	return raw, msg
}

func cleanupRoomKey(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	rawIDs, msgIDs := rkAllExternalIDs()
	threads := rkAllThreadKeys()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM normalized_messages WHERE external_message_id = ANY($1)`, []any{msgIDs}},
		{`DELETE FROM normalized_messages WHERE thread_id IN (SELECT id FROM normalized_threads WHERE thread_key = ANY($1))`, []any{threads}},
		{`DELETE FROM normalized_threads WHERE thread_key = ANY($1)`, []any{threads}},
		{`DELETE FROM raw_source_items WHERE external_id = ANY($1) AND source_account_id IN
			(SELECT id FROM source_accounts WHERE provider=$2 AND account_email=$3)`,
			[]any{rawIDs, upworkcrm.Provider, upworkcrm.AccountEmail}},
	}
	for _, st := range stmts {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

// rkRawJSON builds one raw communications row. withRooms=false omits BOTH room
// keys entirely — the pre-2026-07-11 shape, and the "before" state of the
// re-key in criterion 6.
func rkRawJSON(t *testing.T, i int, withRooms bool) string {
	t.Helper()
	c := rkCorpus[i]
	row := map[string]any{
		"id":              fmt.Sprintf("aaaa%04d-0000-0000-0000-00000000rk%02d", i, i),
		"client_id":       c.client,
		"direction":       c.direction,
		"channel":         rkChannel,
		"subject":         nil,
		"body":            fmt.Sprintf("itest-rk message %d for %s", i, c.client),
		"communicated_at": time.Date(2026, 8, 1, 10, i, 0, 0, time.UTC).Format(time.RFC3339),
		"sender":          "itest-rk",
		"external_id":     rkMessageID(i),
	}
	if withRooms {
		switch c.col {
		case rkObserved:
			row["upwork_room_id"] = c.room
		case rkDispatched:
			row["send_room_id"] = c.room
		}
	}
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal raw row %d: %v", i, err)
	}
	return string(raw)
}

func rkSeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, acctID int64, withRooms bool) {
	t.Helper()
	for i := range rkCorpus {
		if _, err := pool.Exec(ctx,
			`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
			 VALUES ($1,$2,$3,$4)`,
			acctID, rkExternalID(i), rkRawJSON(t, i, withRooms), fmt.Sprintf("itest-rk-hash-%d-%v", i, withRooms)); err != nil {
			t.Fatalf("seed raw row %d: %v", i, err)
		}
	}
}

func rkOpen(t *testing.T) (context.Context, *pgxpool.Pool, int64) {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	acctID, err := upworkcrm.NewSink(pool).EnsureAccount(ctx)
	if err != nil {
		pool.Close()
		t.Fatalf("EnsureAccount: %v", err)
	}
	return ctx, pool, acctID
}

func rkNormalize(t *testing.T, ctx context.Context, pool *pgxpool.Pool, all bool) {
	t.Helper()
	if _, err := upworkcrm.Normalize(ctx, upworkcrm.NewSink(pool), upworkcrm.Config{All: all}); err != nil {
		t.Fatalf("Normalize(all=%v): %v", all, err)
	}
}

// rkCountMessagesOnKeys counts this suite's normalized messages sitting on the
// given thread keys. Scoped by external_message_id so a foreign fixture on the
// same key could never inflate it.
func rkCountMessagesOnKeys(t *testing.T, ctx context.Context, pool *pgxpool.Pool, keys []string) int {
	t.Helper()
	msgIDs := []string{}
	for i := range rkCorpus {
		msgIDs = append(msgIDs, rkMessageID(i))
	}
	return scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_messages m JOIN normalized_threads t ON t.id=m.thread_id
		  WHERE m.external_message_id = ANY($1) AND t.thread_key = ANY($2)`, msgIDs, keys)
}

func rkRoomedKeys() []string {
	return []string{
		rkRoomedKey(rkClient3, rkRoomA), rkRoomedKey(rkClient3, rkRoomB), rkRoomedKey(rkClient3, rkRoomC),
		rkRoomedKey(rkClient2, rkRoomD), rkRoomedKey(rkClient2, rkRoomE),
	}
}

func rkLegacyKeys() []string {
	return []string{rkLegacyKey(rkClient3), rkLegacyKey(rkClient2), rkLegacyKey(rkClient0)}
}

// ---------------------------------------------------------------------------
// Criterion 5 (+23): one thread per (client, room) regardless of which column
// supplied the room, ONE legacy thread per client for the room-less rows, every
// message on the right thread, and --all changes no counts.
// ---------------------------------------------------------------------------

func TestRoomKey_Integration_ThreadPerClientRoom(t *testing.T) {
	ctx, pool, acctID := rkOpen(t)
	defer pool.Close()

	cleanupRoomKey(t, ctx, pool)
	defer cleanupRoomKey(t, ctx, pool)

	rkSeed(t, ctx, pool, acctID, true)

	// Fixture validity, hard-failed. If the corpus's coalesce-roomed count and
	// its upwork_room_id-only count were equal, this whole file would pass
	// against a one-column normalizer — the exact class of vacuous fixture IK
	// records for this matcher.
	rawIDs, _ := rkAllExternalIDs()
	coalesceRoomed := scanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items
		  WHERE external_id = ANY($1) AND source_account_id=$2
		    AND COALESCE(NULLIF(raw_json->>'upwork_room_id',''), NULLIF(raw_json->>'send_room_id','')) IS NOT NULL`,
		rawIDs, acctID)
	oneColumnRoomed := scanInt(t, ctx, pool,
		`SELECT count(*) FROM raw_source_items
		  WHERE external_id = ANY($1) AND source_account_id=$2
		    AND NULLIF(raw_json->>'upwork_room_id','') IS NOT NULL`,
		rawIDs, acctID)
	if coalesceRoomed != rkWantRoomed {
		t.Fatalf("fixture invalid: %d raw rows are roomed under COALESCE, want %d", coalesceRoomed, rkWantRoomed)
	}
	if oneColumnRoomed != rkWantOneColumnRoomed {
		t.Fatalf("fixture invalid: %d raw rows carry upwork_room_id, want %d", oneColumnRoomed, rkWantOneColumnRoomed)
	}
	if coalesceRoomed == oneColumnRoomed {
		t.Fatalf("fixture invalid: reading one column and reading both give the same count (%d), so nothing here "+
			"can catch a one-column normalizer", coalesceRoomed)
	}

	rkNormalize(t, ctx, pool, false)

	// Criterion 23, in the form that survives a live corpus: Go's output equals
	// a count computed INDEPENDENTLY in SQL over the same raw rows.
	if got := rkCountMessagesOnKeys(t, ctx, pool, rkRoomedKeys()); got != coalesceRoomed {
		t.Errorf("messages on :room: keys = %d, want %d (= the rows whose "+
			"COALESCE(NULLIF(upwork_room_id,''), NULLIF(send_room_id,'')) is non-null). A one-column normalizer "+
			"lands on %d here: well-formed keys, no errors, and the send_room_id majority quietly filed under "+
			"the legacy thread", got, coalesceRoomed, oneColumnRoomed)
	}
	if got := rkCountMessagesOnKeys(t, ctx, pool, rkLegacyKeys()); got != rkWantTotal-coalesceRoomed {
		t.Errorf("messages on legacy keys = %d, want %d (total %d minus roomed %d)",
			got, rkWantTotal-coalesceRoomed, rkWantTotal, coalesceRoomed)
	}
	// And the fixture's own exact literals, which cannot drift because this test
	// owns the rows.
	if got := rkCountMessagesOnKeys(t, ctx, pool, rkRoomedKeys()); got != rkWantRoomed {
		t.Errorf("messages on :room: keys = %d, want exactly %d", got, rkWantRoomed)
	}
	if got := rkCountMessagesOnKeys(t, ctx, pool, rkLegacyKeys()); got != rkWantLegacy {
		t.Errorf("messages on legacy keys = %d, want exactly %d", got, rkWantLegacy)
	}
	if got := rkCountMessagesOnKeys(t, ctx, pool, append(rkRoomedKeys(), rkLegacyKeys()...)); got != rkWantTotal {
		t.Errorf("messages on this suite's keys = %d, want %d: every message must be on one of them, so a row "+
			"lost to a malformed key would show up here rather than nowhere", got, rkWantTotal)
	}

	// One thread per (client, room) — including the two rooms whose room arrived
	// via send_room_id. Threads, not just messages: a normalizer that read the
	// column but keyed on the client would pass a message-count check.
	roomedThreads := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_threads WHERE thread_key = ANY($1)`, rkRoomedKeys())
	if roomedThreads != rkWantRoomedThreads {
		t.Errorf("roomed threads = %d, want %d (one per (client,room) pair)", roomedThreads, rkWantRoomedThreads)
	}
	legacyThreads := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_threads WHERE thread_key = ANY($1)`, rkLegacyKeys())
	if legacyThreads != rkWantLegacyThreads {
		t.Errorf("legacy threads = %d, want %d (one per client with a room-less row)", legacyThreads, rkWantLegacyThreads)
	}

	// The headline of the "usable alone" check: a multi-room client stops being
	// one undifferentiated thread. Asserted for BOTH multi-room clients, and by
	// QUERY rather than by naming keys, so the same assertion reads the same way
	// against production.
	for _, tc := range []struct {
		client string
		want   int
	}{{rkClient3, rkWantThreadsOfClient3}, {rkClient2, 2}} {
		got := scanInt(t, ctx, pool,
			`SELECT count(DISTINCT t.thread_key) FROM normalized_threads t
			   JOIN normalized_messages m ON m.thread_id=t.id
			  WHERE t.thread_key = ANY($1) AND m.external_message_id = ANY($2)`,
			rkRoomedKeys(), rkClientMessageIDs(tc.client))
		if got != tc.want {
			t.Errorf("client %s occupies %d distinct roomed threads, want %d — this is the observable outcome of "+
				"the whole ticket: a client with several Upwork chat rooms stops being one thread", tc.client, got, tc.want)
		}
	}

	// Every message points at the thread its own room names.
	for i, c := range rkCorpus {
		want := rkLegacyKey(c.client)
		if c.col != rkNoRoom {
			want = rkRoomedKey(c.client, c.room)
		}
		var got string
		if err := pool.QueryRow(ctx,
			`SELECT t.thread_key FROM normalized_messages m JOIN normalized_threads t ON t.id=m.thread_id
			  WHERE m.external_message_id=$1`, rkMessageID(i)).Scan(&got); err != nil {
			t.Fatalf("read thread of message %d (%s): %v", i, rkMessageID(i), err)
		}
		if got != want {
			t.Errorf("message %d (room %q via column %v) is on thread %q, want %q", i, c.room, c.col, got, want)
		}
	}

	// --all changes no counts (idempotence of the re-key).
	rkNormalize(t, ctx, pool, true)
	if got := rkCountMessagesOnKeys(t, ctx, pool, rkRoomedKeys()); got != rkWantRoomed {
		t.Errorf("after --all, messages on :room: keys = %d, want %d unchanged", got, rkWantRoomed)
	}
	if got := rkCountMessagesOnKeys(t, ctx, pool, rkLegacyKeys()); got != rkWantLegacy {
		t.Errorf("after --all, messages on legacy keys = %d, want %d unchanged", got, rkWantLegacy)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_threads WHERE thread_key = ANY($1)`,
		append(rkRoomedKeys(), rkLegacyKeys()...)); got != rkWantRoomedThreads+rkWantLegacyThreads {
		t.Errorf("after --all, threads = %d, want %d unchanged", got, rkWantRoomedThreads+rkWantLegacyThreads)
	}
}

func rkClientMessageIDs(client string) []string {
	var out []string
	for i, c := range rkCorpus {
		if c.client == client {
			out = append(out, rkMessageID(i))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Criterion 6: re-keying a corpus that was FIRST normalized under the old key.
// The roomed messages move to new threads; the room-less ones stay on the
// ORIGINAL normalized_threads row (same id, same participants, same FK edges);
// nothing is orphaned and no thread is deleted.
//
// This is the production procedure in miniature: `--full` re-reads the source,
// updates raw_json where the content hash changed (which resets normalized_at),
// and `--all` re-normalizes. Here the "source change" is the room columns
// arriving on rows that were ingested before they existed.
// ---------------------------------------------------------------------------

func TestRoomKey_Integration_ReNormalizeMovesOnlyTheRoomedMessages(t *testing.T) {
	ctx, pool, acctID := rkOpen(t)
	defer pool.Close()

	cleanupRoomKey(t, ctx, pool)
	defer cleanupRoomKey(t, ctx, pool)

	// BEFORE: the corpus as it was ingested with no room columns at all.
	rkSeed(t, ctx, pool, acctID, false)
	rkNormalize(t, ctx, pool, false)

	legacyBefore := map[string]int64{}
	for _, key := range rkLegacyKeys() {
		var id int64
		if err := pool.QueryRow(ctx, `SELECT id FROM normalized_threads WHERE thread_key=$1`, key).Scan(&id); err != nil {
			t.Fatalf("read legacy thread %s before the re-key: %v", key, err)
		}
		legacyBefore[key] = id
	}
	if got := rkCountMessagesOnKeys(t, ctx, pool, rkLegacyKeys()); got != rkWantTotal {
		t.Fatalf("precondition: %d of %d messages are on legacy keys before the re-key, want all of them",
			got, rkWantTotal)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_threads WHERE thread_key = ANY($1)`, rkRoomedKeys()); got != 0 {
		t.Fatalf("precondition: %d roomed threads already exist before the re-key", got)
	}

	// The `--full` half: raw_json gains the room columns, content_hash changes,
	// normalized_at resets. Reproduced by hand because there is no source db here.
	for i := range rkCorpus {
		if _, err := pool.Exec(ctx,
			`UPDATE raw_source_items SET raw_json=$3, content_hash=$4, normalized_at=NULL
			  WHERE source_account_id=$1 AND external_id=$2`,
			acctID, rkExternalID(i), rkRawJSON(t, i, true), fmt.Sprintf("itest-rk-hash-%d-true", i)); err != nil {
			t.Fatalf("backfill rooms into raw row %d: %v", i, err)
		}
	}

	// The `--all` half.
	rkNormalize(t, ctx, pool, true)

	if got := rkCountMessagesOnKeys(t, ctx, pool, rkRoomedKeys()); got != rkWantRoomed {
		t.Errorf("after the re-key, %d messages moved to :room: keys, want %d", got, rkWantRoomed)
	}
	if got := rkCountMessagesOnKeys(t, ctx, pool, rkLegacyKeys()); got != rkWantLegacy {
		t.Errorf("after the re-key, %d messages remain on legacy keys, want %d. Most rows NOT moving is the "+
			"designed outcome, not a failed migration", got, rkWantLegacy)
	}

	// The legacy thread ROWS survive with their identity intact. This is what
	// makes the re-key need no migration: the unroomed key is byte-identical to
	// today's, so the existing thread rows keep their ids, participants and FK
	// edges. A client whose messages ALL carry rooms leaves an EMPTY legacy
	// thread behind — inert, and deleting it would need a migration for no
	// benefit (SPEC §5). Nothing here may delete one.
	for key, before := range legacyBefore {
		var after int64
		if err := pool.QueryRow(ctx, `SELECT id FROM normalized_threads WHERE thread_key=$1`, key).Scan(&after); err != nil {
			t.Fatalf("legacy thread %s no longer exists after the re-key (%v): re-keying must not delete threads", key, err)
		}
		if after != before {
			t.Errorf("legacy thread %s changed id %d -> %d: the unroomed key is byte-identical to today's, so its "+
				"row must be reused, not recreated — a new id breaks every FK edge pointing at the old one",
				key, before, after)
		}
	}

	// No orphans: every one of this suite's messages still points at a live
	// thread, and the total is unchanged.
	if got := rkCountMessagesOnKeys(t, ctx, pool, append(rkRoomedKeys(), rkLegacyKeys()...)); got != rkWantTotal {
		t.Errorf("after the re-key %d of %d messages are reachable through a thread; the rest are orphaned",
			got, rkWantTotal)
	}
	msgIDs := []string{}
	for i := range rkCorpus {
		msgIDs = append(msgIDs, rkMessageID(i))
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_messages WHERE external_message_id = ANY($1) AND thread_id IS NULL`, msgIDs); got != 0 {
		t.Errorf("%d messages have a NULL thread_id after the re-key", got)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_messages WHERE external_message_id = ANY($1)`, msgIDs); got != rkWantTotal {
		t.Errorf("normalized_messages for this corpus = %d, want %d: the re-key updates rows in place "+
			"(ON CONFLICT (raw_source_item_id) DO UPDATE SET thread_id=EXCLUDED.thread_id), never duplicates them", got, rkWantTotal)
	}

	// Idempotent: a second --all changes nothing.
	rkNormalize(t, ctx, pool, true)
	if got := rkCountMessagesOnKeys(t, ctx, pool, rkRoomedKeys()); got != rkWantRoomed {
		t.Errorf("after a second --all, roomed messages = %d, want %d", got, rkWantRoomed)
	}
	if got := rkCountMessagesOnKeys(t, ctx, pool, rkLegacyKeys()); got != rkWantLegacy {
		t.Errorf("after a second --all, legacy messages = %d, want %d", got, rkWantLegacy)
	}
}
