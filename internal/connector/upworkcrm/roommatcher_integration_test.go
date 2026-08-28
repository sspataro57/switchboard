//go:build integration

package upworkcrm_test

// Integration tests for the SWT-19 matcher rule (SPEC §4), acceptance criteria
// 9, 10, 12, 13 and 14.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run RoomMatcher ./internal/connector/upworkcrm/
//
// THE RULE, and it is not "room matching": a room MISMATCH is the only thing
// that excludes; an unknown room excludes nothing.
//
//	                 delivery roomed            delivery unroomed
//	message roomed   candidate iff same room    candidate
//	message unroomed candidate                  candidate
//
// plus, always, same client. The honest description — the one the commit
// message, the runbook and the IK entry must also use — is ROOM-SCOPED FOR
// API-ERA TRAFFIC IN BOTH DIRECTIONS, CLIENT-WIDE FOR PRE-2026-07-21 HISTORY.
// SWT-18 called its change "exact room matching" and was wrong on production
// data; the scope here is still conditional on the source having supplied a room.
//
// Everything runs through the real path: seed deliveries + a raw OUTBOUND
// communication, call upworkcrm.Normalize (raw only), read the deliveries row.
// No Upwork, no source db, no network.
//
// GREENFIELD NOTE: fails until normalize.go keys on the room and
// confirmUpworkDelivery scopes in Go. Expected red state.
//
// Cross-suite discipline: one client / project / message per case, all under
// itest-rm-* / bbbb*-…-rm* keys, cleaned in FK order before AND after by
// explicit lists. person_identities are not created here (no clients: raw rows),
// and the shared upwork_crm source account is never deleted.

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
	"github.com/sspataro57/switchboard/internal/textmatch"
)

const rmPrefixLen = 120 // sink.go's upworkMatchPrefixLen

// Real-shaped ids throughout: `room_<hex>` rooms, uuid-shaped clients, and
// `story_<hex>` external ids (the source's shape since the 2026-07-14 backfill).
const (
	rmRoomOne = "room_a1b2c3d4e5"
	rmRoomTwo = "room_f6e5d4c3b2"
	rmChannel = "upwork"
)

// One body, reused across cases: what a template or a re-approval produces, and
// the reason content matching alone cannot discriminate between rooms.
const rmBody = "Quick status before EOD: the importer is deployed to staging, the backfill is running, and I will post the reconciliation numbers tomorrow morning."

// rmCase is one isolated fixture: its own client, project, task and message.
type rmCase struct {
	slug   string
	client string
	comm   string
	extID  string
}

// rmNewCase derives its client uuid from the case's INDEX in rmCases, not from
// the name's length: two names of equal length would otherwise share a client
// uuid, and "a different client's delivery" would silently become "the same
// client's delivery" — a fixture whose two sides are the same string.
func rmNewCase(name string) rmCase {
	idx := -1
	for i, n := range rmCases {
		if n == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		panic("rmNewCase: " + name + " is not registered in rmCases, so cleanup would not cover it")
	}
	return rmCase{
		slug:   "itest-rm-" + name,
		client: fmt.Sprintf("bbbb%04d-0000-0000-0000-00000000rm%02d", idx, idx),
		comm:   "comm-itest-rm-" + name,
		extID:  "story_itest_rm_" + name,
	}
}

func rmLegacyKey(client string) string { return "upwork_crm:" + client + ":" + rmChannel }
func rmRoomedKey(client, room string) string {
	return "upwork_crm:" + client + ":room:" + room
}

var rmCases = []string{
	"roomedtoroomed", "roomedtolegacy", "unroomedtoroomed",
	"otherclient", "badtarget", "ambiguous", "dupinroom",
}

func rmAllCases() []rmCase {
	out := make([]rmCase, 0, len(rmCases))
	for _, n := range rmCases {
		out = append(out, rmNewCase(n))
	}
	return out
}

func cleanupRoomMatcher(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var slugs, comms, extIDs, threads []string
	for _, c := range rmAllCases() {
		slugs = append(slugs, c.slug)
		comms = append(comms, "communications:"+c.comm, "communications:"+c.comm+"-b")
		extIDs = append(extIDs, c.extID, c.extID+"-b")
		threads = append(threads,
			rmLegacyKey(c.client),
			rmRoomedKey(c.client, rmRoomOne), rmRoomedKey(c.client, rmRoomTwo))
	}
	stmts := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM task_events WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug = ANY($1)))`, []any{slugs}},
		{`DELETE FROM deliveries WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug = ANY($1)))`, []any{slugs}},
		{`DELETE FROM normalized_messages WHERE external_message_id = ANY($1)`, []any{extIDs}},
		{`DELETE FROM normalized_messages WHERE thread_id IN (SELECT id FROM normalized_threads WHERE thread_key = ANY($1))`, []any{threads}},
		{`DELETE FROM normalized_threads WHERE thread_key = ANY($1)`, []any{threads}},
		{`DELETE FROM raw_source_items WHERE external_id = ANY($1) AND source_account_id IN
			(SELECT id FROM source_accounts WHERE provider=$2 AND account_email=$3)`,
			[]any{comms, upworkcrm.Provider, upworkcrm.AccountEmail}},
		{`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug = ANY($1))`, []any{slugs}},
		{`DELETE FROM projects WHERE slug = ANY($1)`, []any{slugs}},
		// sync_runs this suite inserted for the reconciler case, tagged in stats.
		{`DELETE FROM sync_runs WHERE stats->>'itest_rm' IS NOT NULL`, nil},
	}
	for _, st := range stmts {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

func rmOpen(t *testing.T) (context.Context, *pgxpool.Pool, int64) {
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

func rmSeedTask(t *testing.T, ctx context.Context, pool *pgxpool.Pool, slug string) int64 {
	t.Helper()
	var projID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,$2,'manual','dashboard','/tmp/itest', 'any') RETURNING id`, slug, slug+"-client").Scan(&projID); err != nil {
		t.Fatalf("seed project %s: %v", slug, err)
	}
	var taskID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'itest-rm work','claude','delivered') RETURNING id`, projID).Scan(&taskID); err != nil {
		t.Fatalf("seed task for %s: %v", slug, err)
	}
	return taskID
}

// rmSeedDelivery inserts an assisted-tier upwork delivery exactly as production
// leaves one: marked sent by a human, so sent_at is set and send_attempted_at is
// NULL. Deliberately NOT seeding send_attempted_at — IK records that seeding it
// is what makes an inert time-floor clause look like a working fix.
func rmSeedDelivery(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64, targetRef, body string, sentAt time.Time) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, sent_external_id, sent_at)
		 VALUES ($1,'upwork_chat',$2,$3,'sent',NULL,$4) RETURNING id`,
		taskID, targetRef, body, sentAt).Scan(&id); err != nil {
		t.Fatalf("seed delivery %s: %v", targetRef, err)
	}
	return id
}

// rmSeedRaw seeds one OUTBOUND raw communication. room=="" means the source
// named no room; column chooses which of the two columns carries it.
func rmSeedRaw(t *testing.T, ctx context.Context, pool *pgxpool.Pool, acctID int64,
	comm, client, room, column, body, extID string, sentAt time.Time) {
	t.Helper()
	row := map[string]any{
		"id":              comm,
		"client_id":       client,
		"direction":       "outbound",
		"channel":         rmChannel,
		"subject":         nil,
		"body":            body,
		"communicated_at": sentAt.UTC().Format(time.RFC3339),
		"sender":          "me",
		"external_id":     extID,
	}
	if room != "" {
		row[column] = room
	}
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal raw communication: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1,$2,$3,$4)`, acctID, "communications:"+comm, raw, "itest-rm-hash-"+comm); err != nil {
		t.Fatalf("seed raw communication %s: %v", comm, err)
	}
}

func rmNormalize(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := upworkcrm.Normalize(ctx, upworkcrm.NewSink(pool), upworkcrm.Config{}); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
}

func rmRead(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) (sentExtID, confirmedAt *string) {
	t.Helper()
	if err := pool.QueryRow(ctx,
		`SELECT sent_external_id, confirmed_at::text FROM deliveries WHERE id=$1`, id).Scan(&sentExtID, &confirmedAt); err != nil {
		t.Fatalf("read delivery %d: %v", id, err)
	}
	return sentExtID, confirmedAt
}

func rmStr(p *string) string {
	if p == nil {
		return "<NULL>"
	}
	return *p
}

func rmConfirmedEvents(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64) int {
	t.Helper()
	return scanInt(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_confirmed'`, taskID)
}

// ---------------------------------------------------------------------------
// Criterion 9: a ROOMED outbound message confirms (a) the delivery targeting its
// own room and (b) the same client's UNROOMED delivery. Separate cases, separate
// assertions — they are different cells of the rule's table, and one passing
// says nothing about the other.
//
// Case (a) carries its room in send_room_id ON PURPOSE: that is the column the
// MAJORITY of roomed outbound rows actually use (136 of 220 as of 2026-08-26),
// because our own sends record the room they were dispatched to. Exercising this
// cell with upwork_room_id instead would leave the one-column bug undetected in
// the very test written for the roomed path.
// ---------------------------------------------------------------------------

func TestRoomMatcher_Integration_RoomedMessageConfirmsItsOwnRoom(t *testing.T) {
	ctx, pool, acctID := rmOpen(t)
	defer pool.Close()

	cleanupRoomMatcher(t, ctx, pool)
	defer cleanupRoomMatcher(t, ctx, pool)

	c := rmNewCase("roomedtoroomed")
	sentAt := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	taskID := rmSeedTask(t, ctx, pool, c.slug)
	target := rmRoomedKey(c.client, rmRoomOne)
	deliveryID := rmSeedDelivery(t, ctx, pool, taskID, target, rmBody, sentAt)

	rmSeedRaw(t, ctx, pool, acctID, c.comm, c.client, rmRoomOne, "send_room_id", rmBody, c.extID, sentAt)
	rmNormalize(t, ctx, pool)

	ext, confirmed := rmRead(t, ctx, pool, deliveryID)
	if ext == nil || *ext != c.extID {
		t.Errorf("sent_external_id = %s, want %q. The message's room arrived in send_room_id — the column that "+
			"carries the room for most roomed OUTBOUND rows — so a normalizer reading upwork_room_id alone keys "+
			"this message onto the LEGACY thread and the roomed target is never matched", rmStr(ext), c.extID)
	}
	if confirmed == nil {
		t.Errorf("confirmed_at is NULL; a same-room match must confirm the send")
	}
	if got := rmConfirmedEvents(t, ctx, pool, taskID); got != 1 {
		t.Errorf("delivery_confirmed task_events = %d, want 1", got)
	}
}

func TestRoomMatcher_Integration_RoomedMessageConfirmsUnroomedDelivery(t *testing.T) {
	ctx, pool, acctID := rmOpen(t)
	defer pool.Close()

	cleanupRoomMatcher(t, ctx, pool)
	defer cleanupRoomMatcher(t, ctx, pool)

	// The top-right cell. A delivery drafted before the client's thread was
	// re-keyed — or drafted against a client with no roomed thread yet — targets
	// the unroomed key. The message that answers it is roomed. Excluding this
	// would strand every delivery written before the re-key, silently.
	c := rmNewCase("roomedtolegacy")
	sentAt := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	taskID := rmSeedTask(t, ctx, pool, c.slug)
	target := rmLegacyKey(c.client)
	deliveryID := rmSeedDelivery(t, ctx, pool, taskID, target, rmBody, sentAt)

	// Fixture guard: the message's key and the delivery's target must differ, or
	// this is the same-key case wearing a different name.
	msgKey := rmRoomedKey(c.client, rmRoomOne)
	if msgKey == target {
		t.Fatalf("fixture invalid: the message's thread key and the delivery's target_ref are the same string (%q)", target)
	}

	rmSeedRaw(t, ctx, pool, acctID, c.comm, c.client, rmRoomOne, "upwork_room_id", rmBody, c.extID, sentAt)
	rmNormalize(t, ctx, pool)

	ext, confirmed := rmRead(t, ctx, pool, deliveryID)
	if ext == nil || *ext != c.extID {
		t.Errorf("sent_external_id = %s, want %q: a roomed message must still claim its client's UNROOMED "+
			"delivery. An unknown room excludes nothing — only a MISMATCH does", rmStr(ext), c.extID)
	}
	if confirmed == nil {
		t.Errorf("confirmed_at is NULL")
	}
}

// ---------------------------------------------------------------------------
// Criterion 10: an UNROOMED outbound message DOES confirm a delivery whose
// target_ref is a ROOMED key of the same client, when it is the only prefix
// match.
//
// WHY THIS TOLERANCE EXISTS — read this before "tightening" the cell, because a
// future reader who does not will:
//
// The outbound path is HEALTHY. API-era outbound traffic is 98.9% ROOMED (186 of
// 188 rows as of 2026-08-26) and records its room in `send_room_id`, written by
// the CRM's send path and nothing else — 136 rows, in perfect agreement with
// `send_requested_at`, zero disagreements. Nothing in the CRM is broken and no
// ticket should be opened against it. An earlier draft of this SPEC measured
// outbound roomed-ness at 44.7% and inverted the rule on that basis; that number
// came from reading `upwork_room_id` alone, which is the one-column bug this
// ticket exists to prevent, and it is WRONG.
//
// What this cell actually covers is HISTORY: 576 outbound rows are unroomed
// pre-2026-07-21 traffic, 2 API-era rows carry no room at all, and `--all`
// replays that entire history back through this matcher. Refusing the cell would
// make those permanently unconfirmable, silently — a worse bug than the one
// being fixed, and an invisible one.
//
// So: LEGACY TOLERANCE, not an accommodation of a broken send path.
// ---------------------------------------------------------------------------

func TestRoomMatcher_Integration_UnroomedMessageConfirmsRoomedDelivery(t *testing.T) {
	ctx, pool, acctID := rmOpen(t)
	defer pool.Close()

	cleanupRoomMatcher(t, ctx, pool)
	defer cleanupRoomMatcher(t, ctx, pool)

	c := rmNewCase("unroomedtoroomed")
	sentAt := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC) // pre-2026-07-21: legacy era
	taskID := rmSeedTask(t, ctx, pool, c.slug)
	target := rmRoomedKey(c.client, rmRoomOne)
	deliveryID := rmSeedDelivery(t, ctx, pool, taskID, target, rmBody, sentAt)

	// No room in EITHER column — the pre-2026-07-21 shape.
	rmSeedRaw(t, ctx, pool, acctID, c.comm, c.client, "", "upwork_room_id", rmBody, c.extID, sentAt)
	rmNormalize(t, ctx, pool)

	ext, confirmed := rmRead(t, ctx, pool, deliveryID)
	if ext == nil || *ext != c.extID {
		t.Errorf("sent_external_id = %s, want %q. This is the bottom-left cell: an unroomed message claiming a "+
			"roomed delivery of the same client, as the only prefix match. It covers pre-2026-07-21 history and "+
			"`--all` replays of it, NOT a broken send path — API-era outbound is 98.9%% roomed and records its "+
			"room in send_room_id", rmStr(ext), c.extID)
	}
	if confirmed == nil {
		t.Errorf("confirmed_at is NULL")
	}
	if got := rmConfirmedEvents(t, ctx, pool, taskID); got != 1 {
		t.Errorf("delivery_confirmed task_events = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// Criterion 12: two things that are NEVER candidates, in either message shape.
//
//  1. A delivery belonging to a DIFFERENT client, even with an identical body.
//     Client scoping is the one predicate that was always doing real work.
//  2. A delivery whose target_ref does not parse. Since SWT-18 the match is by
//     exact target and since SWT-19 by parsed identity, so a legacy or
//     hand-written target is now permanently unconfirmable — which is why the
//     reconciler ships in the same ticket: this row must SURFACE, not rot.
// ---------------------------------------------------------------------------

func TestRoomMatcher_Integration_NeverCandidates(t *testing.T) {
	ctx, pool, acctID := rmOpen(t)
	defer pool.Close()

	cleanupRoomMatcher(t, ctx, pool)
	defer cleanupRoomMatcher(t, ctx, pool)

	sentAt := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)

	shapes := []struct {
		name, room, column string
	}{
		{"roomed message", rmRoomOne, "send_room_id"},
		{"unroomed message", "", "upwork_room_id"},
	}

	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			t.Run("a different client's delivery", func(t *testing.T) {
				cleanupRoomMatcher(t, ctx, pool)
				c := rmNewCase("otherclient")
				other := rmNewCase("badtarget") // borrow a second, distinct client uuid
				if c.client == other.client {
					t.Fatalf("fixture invalid: the two clients are the same uuid (%q), so nothing here is "+
						"cross-client", c.client)
				}
				taskID := rmSeedTask(t, ctx, pool, c.slug)
				foreign := rmSeedDelivery(t, ctx, pool, taskID, rmRoomedKey(other.client, rmRoomOne), rmBody, sentAt)

				rmSeedRaw(t, ctx, pool, acctID, c.comm, c.client, sh.room, sh.column, rmBody, c.extID, sentAt)
				rmNormalize(t, ctx, pool)

				ext, confirmed := rmRead(t, ctx, pool, foreign)
				if ext != nil {
					t.Errorf("delivery %d (another CLIENT's) was stamped %q from an identical body. A real send is "+
						"now recorded against a message from a different conversation, and the correct row is locked "+
						"out of that id forever by deliveries_sent_external_idx", foreign, *ext)
				}
				if confirmed != nil {
					t.Errorf("delivery %d has confirmed_at=%q; a different client's row must be untouched", foreign, *confirmed)
				}
				cleanupRoomMatcher(t, ctx, pool)
			})

			t.Run("a target_ref that does not parse", func(t *testing.T) {
				cleanupRoomMatcher(t, ctx, pool)
				c := rmNewCase("badtarget")
				taskID := rmSeedTask(t, ctx, pool, c.slug)
				// Written directly, bypassing draft_delivery's validator — which
				// is exactly how such a row exists at all: it predates the
				// validation, or a human wrote it.
				bad := rmSeedDelivery(t, ctx, pool, taskID, "upwork_crm:"+c.client, rmBody, sentAt)

				rmSeedRaw(t, ctx, pool, acctID, c.comm, c.client, sh.room, sh.column, rmBody, c.extID, sentAt)
				rmNormalize(t, ctx, pool)

				ext, _ := rmRead(t, ctx, pool, bad)
				if ext != nil {
					t.Errorf("delivery %d with an UNPARSEABLE target_ref was stamped %q: the matcher must not guess "+
						"at identity it cannot read", bad, *ext)
				}
				// And the normalize run itself survived — a target nobody can
				// parse must cost a confirmation, never the ingest.
				if got := scanInt(t, ctx, pool,
					`SELECT count(*) FROM normalized_messages WHERE external_message_id=$1`, c.extID); got != 1 {
					t.Errorf("normalized_messages for %q = %d, want 1: an unreadable delivery must not stop the "+
						"message being normalized", c.extID, got)
				}
				cleanupRoomMatcher(t, ctx, pool)
			})
		})
	}
}

// ---------------------------------------------------------------------------
// Criterion 13: two deliveries reachable from ONE message (one roomed, one
// unroomed, same client) whose bodies share the normalized 120-char prefix.
// Neither is stamped — and the reconciler then flags both, so the refusal stops
// being silent.
//
// The multi-match REFUSAL is a decided policy, not an inherited one, and the
// two-column finding is a fresh argument for it: the columns mean different
// things (dispatched-to vs observed-in), so if the CRM ever stores a dispatched
// message AND its later observation as two rows, both key to the same room
// CORRECTLY and the ambiguity arrives through a door room identity does not
// close. The trade stays asymmetric — two unconfirmed rows can be confirmed
// later or by a human, whereas one wrong stamp burns the external id under
// deliveries_sent_external_idx and locks the correct row out permanently.
// ---------------------------------------------------------------------------

func TestRoomMatcher_Integration_AmbiguousAcrossKeyShapesRefusesThenFlags(t *testing.T) {
	ctx, pool, acctID := rmOpen(t)
	defer pool.Close()

	cleanupRoomMatcher(t, ctx, pool)
	defer cleanupRoomMatcher(t, ctx, pool)

	c := rmNewCase("ambiguous")
	sentAt := time.Now().UTC().Add(-2 * time.Hour)
	taskID := rmSeedTask(t, ctx, pool, c.slug)

	// Same opening 120 characters, different tails — a reusable status line.
	bodyA := rmBody + " Numbers attached."
	bodyB := rmBody + " Will follow up on the invoice separately."
	if bodyA == bodyB {
		t.Fatalf("fixture invalid: the two bodies are the same string")
	}
	if textmatch.NormalizedPrefix(bodyA, rmPrefixLen) != textmatch.NormalizedPrefix(bodyB, rmPrefixLen) {
		t.Fatalf("fixture invalid: the bodies do not collide at %d characters, so nothing is ambiguous", rmPrefixLen)
	}

	roomedID := rmSeedDelivery(t, ctx, pool, taskID, rmRoomedKey(c.client, rmRoomOne), bodyA, sentAt)
	legacyID := rmSeedDelivery(t, ctx, pool, taskID, rmLegacyKey(c.client), bodyB, sentAt)

	rmSeedRaw(t, ctx, pool, acctID, c.comm, c.client, rmRoomOne, "send_room_id", bodyA, c.extID, sentAt)
	rmNormalize(t, ctx, pool)

	for _, d := range []struct {
		id   int64
		name string
	}{{roomedID, "roomed target"}, {legacyID, "unroomed target"}} {
		ext, confirmed := rmRead(t, ctx, pool, d.id)
		if ext != nil {
			t.Errorf("delivery %d (%s) was stamped %q: both are reachable from this message under the rule "+
				"(same room, and unknown-room-excludes-nothing) and both share the normalized %d-char prefix, so "+
				"the matcher cannot tell which produced it. Guessing burns the id on the wrong row",
				d.id, d.name, *ext, rmPrefixLen)
		}
		if confirmed != nil {
			t.Errorf("delivery %d (%s) has confirmed_at=%q; an ambiguous match must confirm nothing", d.id, d.name, *confirmed)
		}
	}
	if got := rmConfirmedEvents(t, ctx, pool, taskID); got != 0 {
		t.Errorf("delivery_confirmed task_events = %d, want 0", got)
	}

	// ...and the refusal SURFACES. Without the reconciler an upwork refusal is
	// silent — two rows sit unconfirmed forever and nothing anywhere says so,
	// which is precisely the state IK records slackweb already defends against
	// and upworkcrm did not.
	rmSeedOKRuns(t, ctx, pool, upworkcrm.DefaultUnconfirmedFlagPasses, sentAt.Add(time.Minute))
	flagged, err := upworkcrm.ReconcileUnconfirmed(ctx, upworkcrm.NewSink(pool), upworkcrm.DefaultUnconfirmedFlagPasses)
	if err != nil {
		t.Fatalf("ReconcileUnconfirmed: %v", err)
	}
	if flagged < 2 {
		t.Errorf("reconciler flagged %d rows, want both refused deliveries (2). A refusal nobody can see is the "+
			"same operational state as a silent miss", flagged)
	}
	for _, id := range []int64{roomedID, legacyID} {
		var errText *string
		if err := pool.QueryRow(ctx, `SELECT error FROM deliveries WHERE id=$1`, id).Scan(&errText); err != nil {
			t.Fatalf("read delivery %d error: %v", id, err)
		}
		if errText == nil || *errText == "" {
			t.Errorf("delivery %d carries no note after the reconciler ran", id)
		}
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_unconfirmed'`, taskID); got != 2 {
		t.Errorf("delivery_unconfirmed task_events = %d, want 2 (one per flagged row)", got)
	}
}

// rmSeedOKRuns inserts n completed 'ok' sync_runs for the upwork account,
// started after `after`. Tagged in stats so cleanup can find them.
//
// n is the FULL threshold on purpose: one upworkcrm invocation writes TWO
// sync_runs rows, so the default of 6 means three real CronJob invocations.
func rmSeedOKRuns(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int, after time.Time) {
	t.Helper()
	var acctID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM source_accounts WHERE provider=$1 AND account_email=$2`,
		upworkcrm.Provider, upworkcrm.AccountEmail).Scan(&acctID); err != nil {
		t.Fatalf("read upwork source account: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO sync_runs (source_account_id, status, started_at, finished_at, stats)
			 VALUES ($1,'ok',$2,$2,'{"itest_rm":true}'::jsonb)`,
			acctID, after.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("seed sync run %d: %v", i, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Criterion 14: same-body duplicates in ONE room refuse a second stamp — the
// shape the CRM would produce if it ever stored a dispatched row AND its later
// observation separately. The first message confirms; the second is refused by
// the already-claimed pre-check WITHOUT failing the normalize run.
//
// The cost of getting this wrong is not a missed confirmation but a crashed
// pass: without the pre-check the matcher stamps an id that
// deliveries_sent_external_idx already holds, and the unique violation
// propagates out of upsertMessage and kills the whole run.
// ---------------------------------------------------------------------------

func TestRoomMatcher_Integration_DuplicateBodiesInOneRoomDoNotDoubleStamp(t *testing.T) {
	ctx, pool, acctID := rmOpen(t)
	defer pool.Close()

	cleanupRoomMatcher(t, ctx, pool)
	defer cleanupRoomMatcher(t, ctx, pool)

	c := rmNewCase("dupinroom")
	sentAt := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	taskID := rmSeedTask(t, ctx, pool, c.slug)
	room := rmRoomedKey(c.client, rmRoomOne)

	// One open delivery; TWO outbound messages in the same room with the same
	// body — one recorded at dispatch, one observed afterwards.
	deliveryID := rmSeedDelivery(t, ctx, pool, taskID, room, rmBody, sentAt)

	// The second message's id is derived below as c.extID+"-b". Guard the
	// property this test needs — two DISTINCT ids in one room — against the
	// derivation changing, rather than comparing a value to itself, which can
	// never fire. (A fixture guard that cannot fail is the same class of mistake
	// as a matcher fixture whose two bodies are the same string: it reads as
	// proof and is not.)
	secondExtID := c.extID + "-b"
	if secondExtID == c.extID {
		t.Fatalf("fixture invalid: the two messages share the external id %q, so this test "+
			"would exercise the already-claimed pre-check on one message rather than two", c.extID)
	}
	rmSeedRaw(t, ctx, pool, acctID, c.comm, c.client, rmRoomOne, "send_room_id", rmBody, c.extID, sentAt)
	rmNormalize(t, ctx, pool)

	ext, _ := rmRead(t, ctx, pool, deliveryID)
	if ext == nil || *ext != c.extID {
		t.Fatalf("precondition: the first message did not confirm the delivery (sent_external_id=%s, want %q)",
			rmStr(ext), c.extID)
	}

	// The second, later observation of the same text in the same room. The run
	// must SURVIVE it — rmNormalize fails the test on error, which is the
	// assertion.
	rmSeedRaw(t, ctx, pool, acctID, c.comm+"-b", c.client, rmRoomOne, "upwork_room_id", rmBody, secondExtID,
		sentAt.Add(time.Minute))
	rmNormalize(t, ctx, pool)

	after, _ := rmRead(t, ctx, pool, deliveryID)
	if after == nil || *after != c.extID {
		t.Errorf("sent_external_id = %s after the duplicate, want the FIRST message's %q unchanged — an id already "+
			"recorded is evidence of a send that happened and must never be overwritten (invariant 4)",
			rmStr(after), c.extID)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_messages WHERE external_message_id=$1`, secondExtID); got != 1 {
		t.Errorf("the duplicate message was not normalized (%d rows): a confirmation that cannot be made must "+
			"not cost the ingest", got)
	}
	if got := rmConfirmedEvents(t, ctx, pool, taskID); got != 1 {
		t.Errorf("delivery_confirmed task_events = %d, want exactly 1", got)
	}
}
