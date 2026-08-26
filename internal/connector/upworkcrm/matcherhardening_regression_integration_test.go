//go:build integration

package upworkcrm_test

// REGRESSION suite for bug `upwork-matcher-hardening` (SWT-18).
// Converted from matcherhardening_repro_integration_test.go, which this file
// REPLACES (the repro's defect-2 test asserted an attempt-time floor that the
// settled fix explicitly rejects — see the diagnosis, "Superseded: the era
// assumption behind this matcher"; keeping both would enshrine two contradictory
// contracts in one package).
//
// Subject: confirmUpworkDelivery (internal/connector/upworkcrm/sink.go:285-322),
// the assisted-tier post-hoc matcher that binds an observed outbound Upwork
// message to the deliveries row that produced it (invariant 5, loop closure;
// invariant 4 collaterally — an id stamped on the wrong row is lost forever
// under the partial unique index deliveries_sent_external_idx).
//
// The contract these tests encode, both halves shipping together:
//
//  1. NORMALIZED comparison — textmatch.NormalizedPrefix on BOTH sides, computed
//     in Go. Never re-spelled in SQL: Postgres POSIX \s does not cover the
//     unicode spaces Go's strings.Fields does (SWT-16 landmine), so an NBSP alone
//     makes the two disagree with no error anywhere. Today's matcher compares
//     left(body,120) RAW, which the SPEC it shipped against (08-draft-deliveries,
//     criterion 8) already said should be whitespace-normalized.
//  2. EXACT ROOM matching — target_ref = {the message's thread_key}, replacing
//     the client-wide target_ref LIKE 'upwork_crm:{client}:%'. **NO time bound of
//     any kind.** A clock bound is a verified no-op on this tier: nothing writes
//     send_attempted_at for upwork_chat (send_delivery is policy-denied,
//     matrix.go:120-125; mark_delivery_sent writes status/sent_at only,
//     delivery.go:633), and sent_at is the instant a HUMAN clicked "mark sent" —
//     legitimately hours after the message. Hence no fixture below seeds
//     send_attempted_at, and TestRegression_SWT18_UpworkMatcher_MarkedSentLate...
//     pins the case a floor would have turned into a permanent refusal.
//  3. The empty-prefix refusal the other three matchers carry
//     (`if want == "" { return nil }`). Today's `nm.BodyText == ""` guard misses
//     a whitespace-only body, which after normalization matches any candidate
//     that also normalizes to empty — claiming a delivery on no evidence.
//  4. Idempotence: an already-stamped row is never re-stamped and emits no second
//     delivery_confirmed event, including across a re-normalize of the same raw
//     item. The fix moves the guards from a subquery into select-then-stamp, so
//     they must be restated on the UPDATE and RowsAffected checked before the
//     event is written (the SWT-16 jira shape, jira/sink.go:284-360).
//
// Everything runs through the real path: seed deliveries + a raw OUTBOUND
// upwork_crm communication, then call upworkcrm.Normalize, which reads
// raw_source_items only. No Upwork, no upwork_crm source db, no network.
//
// Run:
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run 'SWT18' ./internal/connector/upworkcrm/
//
// Cross-suite discipline (IK "integration suites cross-pollute"): every seeded
// row is under an itest-umh-* / dddddddd-* key, cleaned in FK order before and
// after each test, including this suite's person_identities (scoped BY VALUE,
// never provider-wide — other suites own upwork_crm identities too) and the
// orphan people they leave behind, the pattern at integration_test.go:83-86.
// The shared upwork_crm source account is never deleted.

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/upworkcrm"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/textmatch"
)

// The matcher's prefix length (sink.go's upworkMatchPrefixLen).
const umhPrefixLen = 120

// One client / project / message per test, so no test can mask another.
const (
	umhWSClient = "dddddddd-0000-0000-0000-0000000000d1"
	umhWSComm   = "dddddddd-0000-0000-0000-0000000000c1"
	umhWSExtID  = "upwork-room-msg-itest-umh-101"
	umhWSSlug   = "itest-umh-nbsp"

	umhRoomAClient = "dddddddd-0000-0000-0000-0000000000d2"
	umhRoomAComm   = "dddddddd-0000-0000-0000-0000000000c2"
	umhRoomAExtID  = "upwork-room-msg-itest-umh-202"
	umhRoomASlug   = "itest-umh-rooms-older"

	umhRoomBClient = "dddddddd-0000-0000-0000-0000000000d3"
	umhRoomBComm   = "dddddddd-0000-0000-0000-0000000000c3"
	umhRoomBExtID  = "upwork-room-msg-itest-umh-203"
	umhRoomBSlug   = "itest-umh-rooms-newer"

	umhLateClient = "dddddddd-0000-0000-0000-0000000000d4"
	umhLateComm   = "dddddddd-0000-0000-0000-0000000000c4"
	umhLateExtID  = "upwork-room-msg-itest-umh-404"
	umhLateSlug   = "itest-umh-marked-sent-late"

	umhBlankClient = "dddddddd-0000-0000-0000-0000000000d5"
	umhBlankComm   = "dddddddd-0000-0000-0000-0000000000c5"
	umhBlankExtID  = "upwork-room-msg-itest-umh-505"
	umhBlankSlug   = "itest-umh-blank-body"

	umhIdemClient  = "dddddddd-0000-0000-0000-0000000000d6"
	umhIdemComm    = "dddddddd-0000-0000-0000-0000000000c6"
	umhIdemExtID   = "upwork-room-msg-itest-umh-606"
	umhIdemPriorID = "upwork-room-msg-itest-umh-606-prior"
	umhIdemSlug    = "itest-umh-idempotent"

	// Added 2026-08-26 after the go-reviewer pass: the two decisions SWT-18
	// made deliberately had nothing pinning them.
	umhAmbigClient = "dddddddd-0000-0000-0000-0000000000d7"
	umhAmbigComm   = "dddddddd-0000-0000-0000-0000000000c7"
	umhAmbigExtID  = "upwork-room-msg-itest-umh-707"
	umhAmbigSlug   = "itest-umh-ambiguous"

	umhClaimedClient = "dddddddd-0000-0000-0000-0000000000d8"
	umhClaimedComm   = "dddddddd-0000-0000-0000-0000000000c8"
	umhClaimedExtID  = "upwork-room-msg-itest-umh-808"
	umhClaimedSlug   = "itest-umh-already-claimed"
)

// Whitespace fixture. The two bodies differ ONLY in whitespace, and the first
// difference (an NBSP, U+00A0, at character 81) is well inside the 120-character
// window:
//
//   - stored: what draft_delivery/update_delivery persisted — an NBSP, a trailing
//     double space, a three-newline blank-line run.
//   - observed: what the provider round trip (browser UI + human copy/paste)
//     handed back — plain space, no trailing spaces, one blank line.
//
// NBSP does not change the rune count, so the two 120-rune windows stay aligned:
// the windows are correctly *aligned* and simply not equal, byte for byte.
const (
	umhStoredBody   = "Thanks for the update. I pushed the staging fix and re-ran the migration, so the queue is draining now.  \n\n\nWill confirm once the backlog clears, around 18:00."
	umhObservedBody = "Thanks for the update. I pushed the staging fix and re-ran the migration, so the queue is draining now.\n\nWill confirm once the backlog clears, around 18:00."
)

// A status line reused verbatim across rooms — what a template or a re-approval
// produces, and the reason content matching alone cannot discriminate.
const umhSharedBody = "Quick status before EOD: the importer is deployed to staging, the backfill is running, and I will post the reconciliation numbers tomorrow morning."

// Whitespace-only on both sides: byte-identical (so today's raw comparison
// MATCHES it) and normalizing to "" (so the fixed matcher must refuse it).
const umhBlankBody = "  \n\t \n\n "

func umhThreadKey(client, channel string) string {
	return upworkcrm.Provider + ":" + client + ":" + channel
}

// umhRawComm builds a raw upwork_crm communications row: OUTBOUND, so triage's
// inbound-only filter ignores it and the other suites are undisturbed.
func umhRawComm(t *testing.T, commUUID, client, channel, body, extID string, sentAt time.Time) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"id":              commUUID,
		"client_id":       client,
		"direction":       "outbound",
		"channel":         channel,
		"subject":         nil,
		"body":            body,
		"communicated_at": sentAt.UTC().Format(time.RFC3339),
		"sender":          "me",
		"external_id":     extID,
	})
	if err != nil {
		t.Fatalf("marshal raw communication: %v", err)
	}
	return string(raw)
}

func cleanupUpworkMatcher(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	slugs := []string{umhWSSlug, umhRoomASlug, umhRoomBSlug, umhLateSlug, umhBlankSlug, umhIdemSlug,
		umhAmbigSlug, umhClaimedSlug}
	clients := []string{umhWSClient, umhRoomAClient, umhRoomBClient, umhLateClient, umhBlankClient, umhIdemClient,
		umhAmbigClient, umhClaimedClient}
	comms := []string{
		"communications:" + umhWSComm, "communications:" + umhRoomAComm,
		"communications:" + umhRoomBComm, "communications:" + umhLateComm,
		"communications:" + umhBlankComm, "communications:" + umhIdemComm,
		"communications:" + umhAmbigComm, "communications:" + umhClaimedComm,
	}
	extIDs := []string{umhWSExtID, umhRoomAExtID, umhRoomBExtID, umhLateExtID, umhBlankExtID, umhIdemExtID,
		umhAmbigExtID, umhClaimedExtID}
	var threads []string
	for _, c := range clients {
		threads = append(threads, umhThreadKey(c, "chat"), umhThreadKey(c, "room-b"))
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
		// This suite's identities only — never provider-wide.
		{`DELETE FROM person_identities WHERE provider=$1 AND value = ANY($2)`, []any{upworkcrm.Provider, clients}},
		// Orphan people left behind, minus any a project maps as its client
		// (integration_test.go:83-86's pattern).
		{`DELETE FROM people WHERE id NOT IN (SELECT person_id FROM person_identities)
		    AND id NOT IN (SELECT client_person_id FROM projects WHERE client_person_id IS NOT NULL)`, nil},
	}
	for _, st := range stmts {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

func umhOpen(t *testing.T) (context.Context, *pgxpool.Pool, int64) {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	sink := upworkcrm.NewSink(pool)
	acctID, err := sink.EnsureAccount(ctx)
	if err != nil {
		pool.Close()
		t.Fatalf("EnsureAccount: %v", err)
	}
	return ctx, pool, acctID
}

func umhSeedProjectTask(t *testing.T, ctx context.Context, pool *pgxpool.Pool, slug, client string) int64 {
	t.Helper()
	var projID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path)
		 VALUES ($1,$1,$2,'manual','dashboard','/tmp/itest') RETURNING id`, slug, client).Scan(&projID); err != nil {
		t.Fatalf("seed project %s: %v", slug, err)
	}
	var taskID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'umh work','claude','delivered') RETURNING id`, projID).Scan(&taskID); err != nil {
		t.Fatalf("seed task for %s: %v", slug, err)
	}
	return taskID
}

// umhSeedDelivery inserts an assisted-tier upwork delivery exactly as production
// leaves one: marked sent by a human via mark_delivery_sent, so sent_at is set
// and send_attempted_at is NULL (delivery.go:633 never writes it, and
// send_delivery is policy-denied for upwork_chat). Deliberately NOT seeding
// send_attempted_at — the repro did, and IK "The attempt-time floor is INERT on
// the assisted tier" records that seeding it is what makes a no-op clause look
// like a fix.
func umhSeedDelivery(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64, targetRef, body string, sentAt time.Time) int64 {
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

func umhSeedRaw(t *testing.T, ctx context.Context, pool *pgxpool.Pool, acctID int64, commUUID, client, channel, body, extID, hash string, sentAt time.Time) {
	t.Helper()
	raw := umhRawComm(t, commUUID, client, channel, body, extID, sentAt)
	if _, err := pool.Exec(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1,$2,$3,$4)`, acctID, "communications:"+commUUID, raw, hash); err != nil {
		t.Fatalf("seed raw communication %s: %v", commUUID, err)
	}
}

func umhNormalize(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := upworkcrm.Normalize(ctx, upworkcrm.NewSink(pool), upworkcrm.Config{}); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
}

func umhReadDelivery(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) (sentExtID, confirmedAt *string) {
	t.Helper()
	if err := pool.QueryRow(ctx,
		`SELECT sent_external_id, confirmed_at::text FROM deliveries WHERE id=$1`, id).Scan(&sentExtID, &confirmedAt); err != nil {
		t.Fatalf("read delivery %d: %v", id, err)
	}
	return sentExtID, confirmedAt
}

func umhStr(p *string) string {
	if p == nil {
		return "<NULL>"
	}
	return *p
}

// ---------------------------------------------------------------------------
// 1. A whitespace-only difference across the provider round trip must confirm.
// ---------------------------------------------------------------------------

func TestRegression_SWT18_UpworkMatcher_WhitespaceNormalizedMatch(t *testing.T) {
	ctx, pool, acctID := umhOpen(t)
	defer pool.Close()

	cleanupUpworkMatcher(t, ctx, pool)
	defer cleanupUpworkMatcher(t, ctx, pool)

	// Fixture validity: the two bodies must disagree RAW and agree NORMALIZED.
	// Hard failures on purpose — if either half stops holding, the fixture no
	// longer isolates the rule and the run below proves nothing.
	var rawEqual bool
	if err := pool.QueryRow(ctx,
		`SELECT left($1::text, $3) = left($2::text, $3)`, umhStoredBody, umhObservedBody, umhPrefixLen).Scan(&rawEqual); err != nil {
		t.Fatalf("raw prefix comparison: %v", err)
	}
	if rawEqual {
		t.Fatalf("fixture invalid: the two bodies compare EQUAL under left(body,%d); they must differ", umhPrefixLen)
	}
	storedNorm := textmatch.NormalizedPrefix(umhStoredBody, umhPrefixLen)
	observedNorm := textmatch.NormalizedPrefix(umhObservedBody, umhPrefixLen)
	if storedNorm != observedNorm {
		t.Fatalf("fixture invalid: textmatch.NormalizedPrefix disagrees too:\n stored:   %q\n observed: %q", storedNorm, observedNorm)
	}
	t.Logf("asymmetry: left(body,%d) says DIFFERENT, textmatch.NormalizedPrefix says SAME (%q)", umhPrefixLen, storedNorm)

	taskID := umhSeedProjectTask(t, ctx, pool, umhWSSlug, "itest-umh-client-ws")
	sentAt := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	deliveryID := umhSeedDelivery(t, ctx, pool, taskID, umhThreadKey(umhWSClient, "chat"), umhStoredBody, sentAt)

	umhSeedRaw(t, ctx, pool, acctID, umhWSComm, umhWSClient, "chat", umhObservedBody, umhWSExtID, "itest-umh-hash-101", sentAt)
	umhNormalize(t, ctx, pool)

	sentExtID, confirmedAt := umhReadDelivery(t, ctx, pool, deliveryID)
	if sentExtID == nil {
		t.Errorf("sent_external_id is NULL: the matcher missed a body that differs only in whitespace "+
			"(NBSP at char 81, trailing spaces, blank-line run) — textmatch.NormalizedPrefix on both sides matches. "+
			"want %q. Nothing ever retries an exact comparison, so this row is unclaimable forever", umhWSExtID)
	} else if *sentExtID != umhWSExtID {
		t.Errorf("sent_external_id = %q, want %q", *sentExtID, umhWSExtID)
	}
	if confirmedAt == nil {
		t.Errorf("confirmed_at is NULL; the normalized body-prefix match must confirm the send")
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_confirmed'`, taskID); got != 1 {
		t.Errorf("delivery_confirmed task_events = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// 2. Exact-room matching: the delivery whose target_ref IS the message's
//    thread_key confirms; a same-client, same-body delivery in another room is
//    untouched — in BOTH sent_at orderings, so no case can pass because
//    `ORDER BY sent_at DESC` happened to pick right.
// ---------------------------------------------------------------------------

func TestRegression_SWT18_UpworkMatcher_RoomDiscrimination(t *testing.T) {
	ctx, pool, acctID := umhOpen(t)
	defer pool.Close()

	cleanupUpworkMatcher(t, ctx, pool)
	defer cleanupUpworkMatcher(t, ctx, pool)

	msgSentAt := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		// the room the message is actually in; the other room is "room-b"
		client, comm, extID, slug, hash string
		// sent_at of the CORRECT-room delivery relative to the wrong-room one
		correctSentAt, otherSentAt time.Time
		why                        string
	}{
		{
			name: "correct room is the OLDER delivery", client: umhRoomAClient, comm: umhRoomAComm,
			extID: umhRoomAExtID, slug: umhRoomASlug, hash: "itest-umh-hash-202",
			correctSentAt: msgSentAt, otherSentAt: msgSentAt.Add(3 * time.Hour),
			why: "the wrong-room row was sent 3h AFTER the message existed, so it cannot have produced it — " +
				"but it is the newest, which is what `ORDER BY sent_at DESC LIMIT 1` picks",
		},
		{
			name: "correct room is the NEWER delivery", client: umhRoomBClient, comm: umhRoomBComm,
			extID: umhRoomBExtID, slug: umhRoomBSlug, hash: "itest-umh-hash-203",
			correctSentAt: msgSentAt, otherSentAt: msgSentAt.Add(-3 * time.Hour),
			why: "the mirror ordering: room identity, not recency, must be what decides",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			taskID := umhSeedProjectTask(t, ctx, pool, tc.slug, "itest-umh-client-"+tc.slug)
			correctRef := umhThreadKey(tc.client, "chat") // == the message's thread_key
			otherRef := umhThreadKey(tc.client, "room-b")
			correctID := umhSeedDelivery(t, ctx, pool, taskID, correctRef, umhSharedBody, tc.correctSentAt)
			otherID := umhSeedDelivery(t, ctx, pool, taskID, otherRef, umhSharedBody, tc.otherSentAt)

			umhSeedRaw(t, ctx, pool, acctID, tc.comm, tc.client, "chat", umhSharedBody, tc.extID, tc.hash, msgSentAt)
			umhNormalize(t, ctx, pool)

			correctExt, correctConfirmed := umhReadDelivery(t, ctx, pool, correctID)
			otherExt, otherConfirmed := umhReadDelivery(t, ctx, pool, otherID)

			if correctExt == nil || *correctExt != tc.extID {
				t.Errorf("delivery %d (target_ref=%q, THE room the message is in) has sent_external_id=%s, want %q — %s",
					correctID, correctRef, umhStr(correctExt), tc.extID, tc.why)
			}
			if correctConfirmed == nil {
				t.Errorf("delivery %d (target_ref=%q) has confirmed_at NULL; the matching room's send must be confirmed",
					correctID, correctRef)
			}
			if otherExt != nil {
				t.Errorf("delivery %d (target_ref=%q, a DIFFERENT room of the same client) was stamped sent_external_id=%q: "+
					"a real send recorded against a message from another room, and the correct row is locked out of that id "+
					"by deliveries_sent_external_idx", otherID, otherRef, *otherExt)
			}
			if otherConfirmed != nil {
				t.Errorf("delivery %d (target_ref=%q) has confirmed_at=%q; the other room's delivery must be untouched",
					otherID, otherRef, *otherConfirmed)
			}
			if got := scanInt(t, ctx, pool,
				`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_confirmed'`, taskID); got != 1 {
				t.Errorf("delivery_confirmed task_events = %d, want exactly 1", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 3. The realistic assisted-tier case: send_attempted_at NULL and sent_at hours
//    AFTER the message, because "mark sent" is a human clicking later. It must
//    still confirm. Under exact-room matching this passes by construction —
//    the test exists so a future change cannot quietly reintroduce a time
//    predicate (which IK records would be inert AND would create permanent
//    refusals here).
// ---------------------------------------------------------------------------

func TestRegression_SWT18_UpworkMatcher_MarkedSentLongAfterTheMessageStillConfirms(t *testing.T) {
	ctx, pool, acctID := umhOpen(t)
	defer pool.Close()

	cleanupUpworkMatcher(t, ctx, pool)
	defer cleanupUpworkMatcher(t, ctx, pool)

	msgSentAt := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	markedSentAt := msgSentAt.Add(5 * time.Hour)

	taskID := umhSeedProjectTask(t, ctx, pool, umhLateSlug, "itest-umh-client-late")
	deliveryID := umhSeedDelivery(t, ctx, pool, taskID, umhThreadKey(umhLateClient, "chat"), umhSharedBody, markedSentAt)

	// Guard the fixture's whole point: production upwork rows have no attempt
	// instant, so a floor built on send_attempted_at is a clause that is always
	// true (IK: "The attempt-time floor is INERT on the assisted tier").
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM deliveries WHERE id=$1 AND send_attempted_at IS NULL`, deliveryID); got != 1 {
		t.Fatalf("fixture invalid: send_attempted_at is set on delivery %d; no upwork_chat code path writes it", deliveryID)
	}

	umhSeedRaw(t, ctx, pool, acctID, umhLateComm, umhLateClient, "chat", umhSharedBody, umhLateExtID, "itest-umh-hash-404", msgSentAt)
	umhNormalize(t, ctx, pool)

	sentExtID, confirmedAt := umhReadDelivery(t, ctx, pool, deliveryID)
	if sentExtID == nil || *sentExtID != umhLateExtID {
		t.Errorf("sent_external_id = %s, want %q: the human marked this delivery sent %s AFTER the message went out "+
			"(sent_at=%s, message=%s). Refusing it would convert a silent miss into a PERMANENT refusal — "+
			"the failure mode a clock bound introduces on this tier",
			umhStr(sentExtID), umhLateExtID, markedSentAt.Sub(msgSentAt), markedSentAt.Format(time.RFC3339), msgSentAt.Format(time.RFC3339))
	}
	if confirmedAt == nil {
		t.Errorf("confirmed_at is NULL; a mark-sent-later delivery must still close the loop")
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_confirmed'`, taskID); got != 1 {
		t.Errorf("delivery_confirmed task_events = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// 4. A body that normalizes to empty is no evidence at all. The two bodies here
//    are byte-identical whitespace, so the RAW comparison matches them today;
//    after the fix, NormalizedPrefix makes both "" and the empty-prefix refusal
//    (`if want == "" { return nil }`, jira/sink.go:286, google/sink.go:544) must
//    stop the claim.
// ---------------------------------------------------------------------------

func TestRegression_SWT18_UpworkMatcher_WhitespaceOnlyBodyClaimsNothing(t *testing.T) {
	ctx, pool, acctID := umhOpen(t)
	defer pool.Close()

	cleanupUpworkMatcher(t, ctx, pool)
	defer cleanupUpworkMatcher(t, ctx, pool)

	if got := textmatch.NormalizedPrefix(umhBlankBody, umhPrefixLen); got != "" {
		t.Fatalf("fixture invalid: the whitespace-only body normalizes to %q, want the empty string", got)
	}

	sentAt := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	taskID := umhSeedProjectTask(t, ctx, pool, umhBlankSlug, "itest-umh-client-blank")
	deliveryID := umhSeedDelivery(t, ctx, pool, taskID, umhThreadKey(umhBlankClient, "chat"), umhBlankBody, sentAt)

	umhSeedRaw(t, ctx, pool, acctID, umhBlankComm, umhBlankClient, "chat", umhBlankBody, umhBlankExtID, "itest-umh-hash-505", sentAt)
	umhNormalize(t, ctx, pool)

	sentExtID, confirmedAt := umhReadDelivery(t, ctx, pool, deliveryID)
	if sentExtID != nil {
		t.Errorf("delivery %d was stamped sent_external_id=%q on a body that normalizes to the empty string: "+
			"an empty prefix matches every candidate that also normalizes to empty, so this claims a delivery on NO evidence. "+
			"The other three matchers refuse up front with `if want == \"\" { return nil }`", deliveryID, *sentExtID)
	}
	if confirmedAt != nil {
		t.Errorf("delivery %d has confirmed_at=%q; an empty normalized prefix must confirm nothing", deliveryID, *confirmedAt)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_confirmed'`, taskID); got != 0 {
		t.Errorf("delivery_confirmed task_events = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// 5. Idempotence (invariant 4): a delivery already carrying sent_external_id is
//    never re-stamped and never emits a second delivery_confirmed. The fix moves
//    these guards out of the subquery into a select-then-stamp transaction, so
//    they must be RESTATED on the UPDATE with RowsAffected checked before the
//    event is written.
// ---------------------------------------------------------------------------

func TestRegression_SWT18_UpworkMatcher_AlreadyStampedIsNotRestamped(t *testing.T) {
	ctx, pool, acctID := umhOpen(t)
	defer pool.Close()

	cleanupUpworkMatcher(t, ctx, pool)
	defer cleanupUpworkMatcher(t, ctx, pool)

	msgSentAt := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	taskID := umhSeedProjectTask(t, ctx, pool, umhIdemSlug, "itest-umh-client-idem")
	room := umhThreadKey(umhIdemClient, "chat")

	// A previous send in the SAME room with the SAME body, already confirmed
	// against its own message.
	priorID := umhSeedDelivery(t, ctx, pool, taskID, room, umhSharedBody, msgSentAt.Add(-24*time.Hour))
	if _, err := pool.Exec(ctx,
		`UPDATE deliveries SET sent_external_id=$2, confirmed_at=$3 WHERE id=$1`,
		priorID, umhIdemPriorID, msgSentAt.Add(-24*time.Hour)); err != nil {
		t.Fatalf("stamp prior delivery: %v", err)
	}
	_, priorConfirmedBefore := umhReadDelivery(t, ctx, pool, priorID)

	// The open delivery this message actually closes.
	openID := umhSeedDelivery(t, ctx, pool, taskID, room, umhSharedBody, msgSentAt)

	umhSeedRaw(t, ctx, pool, acctID, umhIdemComm, umhIdemClient, "chat", umhSharedBody, umhIdemExtID, "itest-umh-hash-606", msgSentAt)
	umhNormalize(t, ctx, pool)

	priorExt, priorConfirmed := umhReadDelivery(t, ctx, pool, priorID)
	if priorExt == nil || *priorExt != umhIdemPriorID {
		t.Errorf("delivery %d (already confirmed) has sent_external_id=%s, want its original %q — an id already recorded "+
			"is evidence of a send that happened and must never be overwritten (invariant 4)",
			priorID, umhStr(priorExt), umhIdemPriorID)
	}
	if priorConfirmed == nil || *priorConfirmed != *priorConfirmedBefore {
		t.Errorf("delivery %d confirmed_at moved from %s to %s", priorID, umhStr(priorConfirmedBefore), umhStr(priorConfirmed))
	}
	openExt, _ := umhReadDelivery(t, ctx, pool, openID)
	if openExt == nil || *openExt != umhIdemExtID {
		t.Errorf("delivery %d (the open one) has sent_external_id=%s, want %q", openID, umhStr(openExt), umhIdemExtID)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_confirmed'`, taskID); got != 1 {
		t.Errorf("delivery_confirmed task_events after the first pass = %d, want 1", got)
	}

	// Re-normalize the SAME raw item (the --all replay / a re-poll): no second
	// stamp, no second event. Scoped to this suite's row so no foreign fixture
	// is replayed.
	if _, err := pool.Exec(ctx,
		`UPDATE raw_source_items SET normalized_at=NULL WHERE external_id=$1 AND source_account_id=$2`,
		"communications:"+umhIdemComm, acctID); err != nil {
		t.Fatalf("reset normalized_at for replay: %v", err)
	}
	umhNormalize(t, ctx, pool)

	replayExt, _ := umhReadDelivery(t, ctx, pool, openID)
	if replayExt == nil || *replayExt != umhIdemExtID {
		t.Errorf("after replay delivery %d has sent_external_id=%s, want %q unchanged", openID, umhStr(replayExt), umhIdemExtID)
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_confirmed'`, taskID); got != 1 {
		t.Errorf("delivery_confirmed task_events after replay = %d, want 1: re-normalizing the same raw item must not "+
			"emit a second confirmation (the IS NULL guards must be restated on the UPDATE and RowsAffected checked)", got)
	}
}

// ---------------------------------------------------------------------------
// 6. Multi-match REFUSES. Two unconfirmed deliveries in the same thread whose
//    bodies share the normalized 120-char prefix: neither is stamped and no
//    event fires. Added after review — the refusal was a decided policy
//    (google and slackweb refuse; jira keeps newest-wins) with nothing pinning
//    it, so reverting `matches > 1` to newest-wins passed the whole suite.
//
//    It is not a nicety. On production data this refusal, NOT the target_ref
//    equality, is what prevents the wrong-row bind: thread_key's third segment
//    is communications.channel, which is the constant 'upwork' for every row in
//    the source db, so one thread == one client and the equality selects the
//    same candidate set the old client-wide LIKE did.
//
//    Refusing is the reversible half of the trade: two unconfirmed rows can
//    still be confirmed by a later distinct message or resolved by a human,
//    whereas one wrong stamp burns the external id under
//    deliveries_sent_external_idx and locks the correct row out permanently.
// ---------------------------------------------------------------------------

func TestRegression_SWT18_UpworkMatcher_AmbiguousPrefixConfirmsNothing(t *testing.T) {
	ctx, pool, acctID := umhOpen(t)
	defer pool.Close()

	cleanupUpworkMatcher(t, ctx, pool)
	defer cleanupUpworkMatcher(t, ctx, pool)

	msgSentAt := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	taskID := umhSeedProjectTask(t, ctx, pool, umhAmbigSlug, "itest-umh-client-ambig")
	room := umhThreadKey(umhAmbigClient, "chat")

	// Same opening 120 characters, different tails — realistic for a reusable
	// status-line template. Guard that the fixture is actually ambiguous and
	// not two identical strings, the failure mode this suite exists to avoid.
	bodyA := umhSharedBody + " Numbers attached."
	bodyB := umhSharedBody + " Will follow up on the invoice separately."
	if bodyA == bodyB {
		t.Fatalf("fixture invalid: the two bodies are the same string")
	}
	if textmatch.NormalizedPrefix(bodyA, umhPrefixLen) != textmatch.NormalizedPrefix(bodyB, umhPrefixLen) {
		t.Fatalf("fixture invalid: the bodies do not collide at %d characters, so nothing is ambiguous", umhPrefixLen)
	}

	olderID := umhSeedDelivery(t, ctx, pool, taskID, room, bodyA, msgSentAt.Add(-time.Hour))
	newerID := umhSeedDelivery(t, ctx, pool, taskID, room, bodyB, msgSentAt)

	umhSeedRaw(t, ctx, pool, acctID, umhAmbigComm, umhAmbigClient, "chat", bodyA, umhAmbigExtID, "itest-umh-hash-707", msgSentAt)
	umhNormalize(t, ctx, pool)

	for _, d := range []struct {
		id   int64
		name string
	}{{olderID, "older"}, {newerID, "newer"}} {
		ext, confirmed := umhReadDelivery(t, ctx, pool, d.id)
		if ext != nil {
			t.Errorf("delivery %d (%s) was stamped sent_external_id=%q: two pending deliveries in this thread share the "+
				"normalized %d-char prefix, so the matcher cannot tell which produced the message. Guessing stamps a real "+
				"send onto the wrong row and locks the other out of that id forever; the matcher must refuse",
				d.id, d.name, *ext, umhPrefixLen)
		}
		if confirmed != nil {
			t.Errorf("delivery %d (%s) has confirmed_at=%q; an ambiguous match must confirm nothing", d.id, d.name, *confirmed)
		}
	}
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='delivery_confirmed'`, taskID); got != 0 {
		t.Errorf("delivery_confirmed task_events = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// 7. A message whose external id another delivery already claims is SKIPPED,
//    and the normalize run must not fail. Added after review: this is the one
//    case the other guards cannot catch, and its cost is not a missed
//    confirmation but a crashed pass.
//
//    Shape: delivery A already holds THIS message's id. Delivery B is open with
//    the same prefix in the same thread. The candidate query excludes A (its
//    sent_external_id is not NULL), so exactly one candidate remains and the
//    multi-match refusal does not fire — without the pre-check the matcher
//    stamps B with an id deliveries_sent_external_idx already holds, and the
//    unique violation propagates out of upsertMessage and fails the WHOLE
//    normalize run rather than skipping one confirmation.
// ---------------------------------------------------------------------------

func TestRegression_SWT18_UpworkMatcher_ClaimedExternalIDSkipsWithoutFailingTheRun(t *testing.T) {
	ctx, pool, acctID := umhOpen(t)
	defer pool.Close()

	cleanupUpworkMatcher(t, ctx, pool)
	defer cleanupUpworkMatcher(t, ctx, pool)

	msgSentAt := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	taskID := umhSeedProjectTask(t, ctx, pool, umhClaimedSlug, "itest-umh-client-claimed")
	room := umhThreadKey(umhClaimedClient, "chat")

	// A: already confirmed against the very message we are about to ingest.
	claimedID := umhSeedDelivery(t, ctx, pool, taskID, room, umhSharedBody, msgSentAt.Add(-2*time.Hour))
	if _, err := pool.Exec(ctx,
		`UPDATE deliveries SET sent_external_id=$2, confirmed_at=$3 WHERE id=$1`,
		claimedID, umhClaimedExtID, msgSentAt); err != nil {
		t.Fatalf("stamp the claiming delivery: %v", err)
	}
	// B: open, same thread, same prefix — the only remaining candidate.
	openID := umhSeedDelivery(t, ctx, pool, taskID, room, umhSharedBody, msgSentAt)

	umhSeedRaw(t, ctx, pool, acctID, umhClaimedComm, umhClaimedClient, "chat", umhSharedBody, umhClaimedExtID, "itest-umh-hash-808", msgSentAt)

	// umhNormalize fails the test on error, which is exactly the assertion:
	// without the pre-check this run dies on deliveries_sent_external_idx.
	umhNormalize(t, ctx, pool)

	// The message still normalized — the run did its real work.
	if got := scanInt(t, ctx, pool,
		`SELECT count(*) FROM normalized_messages WHERE external_message_id=$1`, umhClaimedExtID); got != 1 {
		t.Errorf("normalized_messages for %q = %d, want 1: the message must be normalized even though its id is "+
			"already claimed — a confirmation that cannot be made must not cost the ingest", umhClaimedExtID, got)
	}
	openExt, openConfirmed := umhReadDelivery(t, ctx, pool, openID)
	if openExt != nil {
		t.Errorf("delivery %d was stamped sent_external_id=%q, an id delivery %d already holds: the partial unique index "+
			"makes this a constraint violation, not a duplicate row", openID, *openExt, claimedID)
	}
	if openConfirmed != nil {
		t.Errorf("delivery %d has confirmed_at=%q; an already-claimed id is no evidence about this row", openID, *openConfirmed)
	}
	claimedExt, _ := umhReadDelivery(t, ctx, pool, claimedID)
	if claimedExt == nil || *claimedExt != umhClaimedExtID {
		t.Errorf("delivery %d (the claimant) has sent_external_id=%s, want %q unchanged", claimedID, umhStr(claimedExt), umhClaimedExtID)
	}
}
