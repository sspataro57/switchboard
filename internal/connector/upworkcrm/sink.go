package upworkcrm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/textmatch"
)

// PGSink is the ops-db side of the connector: the ingest Sink and the
// normalize store in one, over the sink pool (DATABASE_URL).
type PGSink struct {
	pool *pgxpool.Pool
}

func NewSink(pool *pgxpool.Pool) *PGSink {
	return &PGSink{pool: pool}
}

func (s *PGSink) EnsureAccount(ctx context.Context) (int64, error) {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO source_accounts (provider, account_email, send_enabled)
		 VALUES ($1, $2, false)
		 ON CONFLICT (provider, account_email) DO NOTHING`,
		Provider, AccountEmail)
	if err != nil {
		return 0, fmt.Errorf("upsert source account: %w", err)
	}
	var id int64
	err = s.pool.QueryRow(ctx,
		`SELECT id FROM source_accounts WHERE provider=$1 AND account_email=$2`,
		Provider, AccountEmail).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("select source account: %w", err)
	}
	return id, nil
}

func (s *PGSink) Cursor(ctx context.Context, accountID int64) (Cursor, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT sync_cursor FROM source_accounts WHERE id=$1`, accountID).Scan(&raw)
	if err != nil {
		return Cursor{}, fmt.Errorf("select sync_cursor: %w", err)
	}
	var c Cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return Cursor{}, fmt.Errorf("parse sync_cursor: %w", err)
	}
	return c, nil
}

func (s *PGSink) SaveCursor(ctx context.Context, accountID int64, c Cursor) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal cursor: %w", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE source_accounts SET sync_cursor=$2 WHERE id=$1`, accountID, raw); err != nil {
		return fmt.Errorf("save cursor: %w", err)
	}
	return nil
}

func (s *PGSink) StartRun(ctx context.Context, accountID int64) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO sync_runs (source_account_id, status) VALUES ($1, 'running') RETURNING id`,
		accountID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert sync run: %w", err)
	}
	return id, nil
}

func (s *PGSink) FinishRun(ctx context.Context, runID int64, status string, stats Stats, errMsg string) error {
	rawStats, err := json.Marshal(stats)
	if err != nil {
		return fmt.Errorf("marshal stats: %w", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE sync_runs SET finished_at=now(), status=$2, stats=$3, error=NULLIF($4,'') WHERE id=$1`,
		runID, status, rawStats, errMsg); err != nil {
		return fmt.Errorf("finish sync run %d: %w", runID, err)
	}
	return nil
}

func (s *PGSink) RawHash(ctx context.Context, accountID int64, externalID string) (string, bool, error) {
	var h string
	err := s.pool.QueryRow(ctx,
		`SELECT content_hash FROM raw_source_items WHERE source_account_id=$1 AND external_id=$2`,
		accountID, externalID).Scan(&h)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("select content_hash: %w", err)
	}
	return h, true, nil
}

func (s *PGSink) InsertRaw(ctx context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1, $2, $3, $4)`,
		accountID, externalID, raw, hash); err != nil {
		return fmt.Errorf("insert raw item: %w", err)
	}
	return nil
}

func (s *PGSink) UpdateRaw(ctx context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE raw_source_items
		 SET raw_json=$3, content_hash=$4, ingested_at=now(), normalized_at=NULL
		 WHERE source_account_id=$1 AND external_id=$2`,
		accountID, externalID, raw, hash); err != nil {
		return fmt.Errorf("update raw item: %w", err)
	}
	return nil
}

// ---- normalize store --------------------------------------------------------

type rawItem struct {
	id         int64
	externalID string
	raw        json.RawMessage
}

// pendingRaw returns the raw rows to normalize — pending only, or every row
// with all=true. Ordered by external_id so clients: sorts before
// communications: (people exist before messages reference them).
func (s *PGSink) pendingRaw(ctx context.Context, accountID int64, all bool) ([]rawItem, error) {
	q := `SELECT id, external_id, raw_json FROM raw_source_items
	      WHERE source_account_id=$1`
	if !all {
		q += ` AND normalized_at IS NULL`
	}
	q += ` ORDER BY external_id`
	rows, err := s.pool.Query(ctx, q, accountID)
	if err != nil {
		return nil, fmt.Errorf("select raw items: %w", err)
	}
	defer rows.Close()

	var out []rawItem
	for rows.Next() {
		var it rawItem
		if err := rows.Scan(&it.id, &it.externalID, &it.raw); err != nil {
			return nil, fmt.Errorf("scan raw item: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate raw items: %w", err)
	}
	return out, nil
}

// OwnerOf implements IdentityResolver over person_identities.
func (s *PGSink) OwnerOf(ctx context.Context, provider, value string) (int64, bool, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`SELECT person_id FROM person_identities WHERE provider=$1 AND value=$2`,
		provider, value).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("select identity owner: %w", err)
	}
	return id, true, nil
}

// upsertClient finds-or-creates the person via the primary upwork_crm identity,
// refreshes the display name, and reconciles secondary identities with no
// auto-merge. Returns the number of suspected merges.
func (s *PGSink) upsertClient(ctx context.Context, nc NormalizedClient) (int, error) {
	personID, ok, err := s.OwnerOf(ctx, Provider, nc.ClientID)
	if err != nil {
		return 0, err
	}
	if !ok {
		err = s.pool.QueryRow(ctx,
			`INSERT INTO people (display_name) VALUES ($1) RETURNING id`, nc.DisplayName).Scan(&personID)
		if err != nil {
			return 0, fmt.Errorf("insert person: %w", err)
		}
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO person_identities (person_id, provider, value) VALUES ($1, $2, $3)`,
			personID, Provider, nc.ClientID); err != nil {
			return 0, fmt.Errorf("insert primary identity: %w", err)
		}
	} else {
		if _, err := s.pool.Exec(ctx,
			`UPDATE people SET display_name=$2 WHERE id=$1`, personID, nc.DisplayName); err != nil {
			return 0, fmt.Errorf("update person: %w", err)
		}
	}

	var secondary []Identity
	for _, id := range nc.Identities {
		if id.Provider != Provider {
			secondary = append(secondary, id)
		}
	}
	insert, suspected, err := ReconcileIdentities(ctx, personID, secondary, s)
	if err != nil {
		return 0, err
	}
	for _, id := range insert {
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO person_identities (person_id, provider, value) VALUES ($1, $2, $3)
			 ON CONFLICT (provider, value) DO NOTHING`,
			personID, id.Provider, id.Value); err != nil {
			return 0, fmt.Errorf("insert identity %s:%s: %w", id.Provider, id.Value, err)
		}
	}
	return len(suspected), nil
}

// upsertMessage upserts the thread (keyed upwork_crm:{client}:{channel}) and
// the message (one per raw item).
func (s *PGSink) upsertMessage(ctx context.Context, rawItemID int64, nm NormalizedMessage) error {
	participants := []byte(`[]`)
	if personID, ok, err := s.OwnerOf(ctx, Provider, nm.ClientID); err != nil {
		return err
	} else if ok {
		raw, err := json.Marshal([]int64{personID})
		if err != nil {
			return fmt.Errorf("marshal participants: %w", err)
		}
		participants = raw
	}

	var threadID int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO normalized_threads (thread_key, subject, participants)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (thread_key) WHERE thread_key IS NOT NULL
		 DO UPDATE SET participants = EXCLUDED.participants
		 RETURNING id`,
		nm.ThreadKey, nm.Subject, participants).Scan(&threadID)
	if err != nil {
		return fmt.Errorf("upsert thread %s: %w", nm.ThreadKey, err)
	}

	if _, err := s.pool.Exec(ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		    body_text, subject, sender, channel)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 ON CONFLICT (raw_source_item_id) DO UPDATE SET
		   thread_id=EXCLUDED.thread_id, direction=EXCLUDED.direction,
		   external_message_id=EXCLUDED.external_message_id, sent_at=EXCLUDED.sent_at,
		   body_text=EXCLUDED.body_text, subject=EXCLUDED.subject,
		   sender=EXCLUDED.sender, channel=EXCLUDED.channel`,
		rawItemID, threadID, nm.Direction, nm.ExternalMessageID, nm.SentAt,
		nm.BodyText, nm.Subject, nm.Sender, nm.Channel); err != nil {
		return fmt.Errorf("upsert message for raw item %d: %w", rawItemID, err)
	}

	// Loop closure, assisted tier (invariant 5 / SWT-8): a manually-sent
	// upwork_chat delivery has no Message-ID, so the confirmation is a
	// post-hoc match — an OUTBOUND communication IN THE SAME ROOM whose body
	// opens the same way (whitespace-normalized) claims the delivery, filling
	// sent_external_id (the partial unique index makes double-claims
	// impossible) + confirmed_at + a delivery_confirmed event. The scope was
	// client-wide and the comparison raw until SWT-18.
	if nm.Direction == "outbound" {
		if err := s.confirmUpworkDelivery(ctx, nm); err != nil {
			return err
		}
	}
	return nil
}

const upworkMatchPrefixLen = 120

// confirmUpworkDelivery runs the assisted-tier post-hoc matcher: an observed
// OUTBOUND Upwork message claims the deliveries row that produced it, filling
// sent_external_id + confirmed_at and emitting delivery_confirmed.
//
// It is the fourth of four post-hoc body matchers (google, jira, slackweb,
// here) and until SWT-18 it was the only one that had received neither of the
// two hardenings the others did. Both land here together, and they must:
// normalization strictly WIDENS the candidate set, so shipping it without
// narrowing the scope would make the wrong-row bind MORE likely, not less.
//
// 1. NORMALIZED comparison, computed in Go on both sides. The query used to
// compare left(body,120) raw — the text we stored against the text Upwork
// handed back after a browser round trip and a human copy/paste. A provider may
// change line endings, trailing spaces, or blank-line runs without changing the
// message, and any such change made the match fail PERMANENTLY, because nothing
// retries a comparison that is already exact. The row then stays unclaimable
// with sent_external_id NULL forever. This is not new policy: the SPEC this
// matcher shipped against (08-draft-deliveries, criterion 8) already said
// "whitespace-normalized"; the code never did it, and the test that shipped
// alongside seeded ONE constant on both sides of the comparison, so raw and
// normalized passed identically.
//
// Do NOT re-spell the rule in SQL. Postgres's POSIX \s does not cover the
// unicode spaces Go's strings.Fields does, so an NBSP alone makes the two
// disagree with no error anywhere (IK: the SWT-13 canonicalization landmine).
// textmatch.NormalizedPrefix is the single spelling.
//
// 2. CONVERSATION scoping, via SameConversation (threadkey.go) — and say what
// that is, precisely, because SWT-18 shipped calling its version "exact room
// matching" and was wrong on production data.
//
// What it is: room-scoped for API-era traffic in both directions, client-wide
// for pre-2026-07-21 history. A room MISMATCH excludes; an unknown room does
// not. Since SWT-19 the normalizer reads BOTH room columns
// (upwork_room_id, the room a message was observed in, and send_room_id, the
// room a send was dispatched to), so API-era traffic is 98.9% roomed outbound
// (186 of 188) and 99.5% inbound. The outbound send path is HEALTHY — it has
// been recording its room in send_room_id all along. The 44.7% that appears in
// early SWT-19 drafts was one column, measured confidently and wrongly.
//
// What it is NOT: a guarantee that any two messages of one client are told
// apart. 576 outbound rows are pre-2026-07-21 history with no room in either
// column, and they all share one legacy thread per client. For those, this is
// exactly the client-wide scope it has always been, and the multi-match refusal
// below is the only thing standing between an ambiguous body and a wrong bind.
//
// The scope was target_ref LIKE 'upwork_crm:{client}:%' before SWT-18 and an
// equality against a key whose room segment was a constant before SWT-19. It is
// now genuinely conditional on what the source supplied.
//
// Multi-match policy is REFUSE, chosen rather than inherited. google and
// slackweb refuse; jira keeps newest-wins as a documented carry-over. Refusing
// is the reversible half of the trade: two unconfirmed rows can still be
// confirmed by a later distinct message or resolved by a human, whereas one
// wrong stamp burns the external id under the unique index and locks the
// correct row out permanently (invariant 4).
func (s *PGSink) confirmUpworkDelivery(ctx context.Context, nm NormalizedMessage) error {
	if nm.ExternalMessageID == "" || nm.ThreadKey == "" {
		// Without a room there is nothing to match on: target_ref is never
		// empty on a real delivery (draft_delivery rejects it), so an empty
		// key would simply select nothing — but the guard says why, rather
		// than leaving it to the reader of the query. slackweb refuses the
		// same way.
		return nil
	}
	want := textmatch.NormalizedPrefix(nm.BodyText, upworkMatchPrefixLen)
	if want == "" {
		// The old guard was nm.BodyText == "", which a whitespace-only body
		// walks straight past. Normalized, such a body is "" and would match
		// every candidate that also normalizes to "" — claiming a delivery on
		// no evidence whatsoever. The other three matchers refuse it up front.
		return nil
	}

	messageRef, err := ParseThreadKey(nm.ThreadKey)
	if err != nil {
		// Our own normalizer built this key, so a parse failure is a bug here
		// rather than bad data — but confirmation is a best-effort side channel
		// and must not fail the normalize run over it.
		return nil
	}

	// Refuse a message some delivery already owns. Without this, a replay
	// (--all, or a re-poll after normalized_at is cleared) that finds a second
	// unconfirmed row with the same prefix would try to stamp an id the unique
	// index already holds, failing the whole normalize run rather than skipping
	// one confirmation. slackweb guards the same way.
	var claimed bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM deliveries
		   WHERE channel='upwork_chat' AND sent_external_id=$1)`,
		nm.ExternalMessageID).Scan(&claimed); err != nil {
		return fmt.Errorf("check upwork external id claim: %w", err)
	}
	if claimed {
		return nil
	}

	// Selection and stamping run in ONE transaction with the candidates locked
	// FOR UPDATE (the SWT-16 jira shape). The statement this replaces was a
	// bare `WHERE id = (subquery)` carrying no guards on the outer UPDATE at
	// all, so restating them below is strictly tighter, not a new exposure.
	// Nothing can block on an in-flight upwork send: there is no automated one.
	//
	// status='sent' only, unchanged. An upwork_chat row has no 'sending' phase
	// to self-heal from — switchboard never dispatches the click.
	//
	// COALESCE on body because it is nullable (0001) and scanning NULL into a
	// string ERRORS, which would fail the entire normalize run instead of this
	// one match.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin upwork delivery match: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// SWT-20: SHORTLIST, then lock, then revalidate. The shortlist narrows by
	// the persisted client identity (deliveries.target_client_ref, written in
	// Go by the same ParseThreadKey call that produced the stored target_ref;
	// the partial index deliveries_upwork_unconfirmed_idx serves it), so an
	// outbound message locks only its OWN client's unresolved rows and the
	// common case — a client with no open deliveries — locks nothing at all.
	//
	// Why the narrowing cannot change the outcome: SameConversation returns
	// false whenever the client ids differ, so the rows the shortlist excludes
	// are exactly the rows the Go decision below would have excluded anyway —
	// and deliveries_upwork_identity_check guarantees every upwork_chat row
	// carries the client value that parser produced. A stale or wrong column
	// can therefore only cause a MISS, never a wrong stamp, and a miss is what
	// the reconciler exists to surface. The client-equality predicate is the
	// ONLY one implied by SameConversation; the room clause is the rule itself
	// and stays in Go (D3 — no room column, no second spelling).
	//
	// The reconciler still ANNOTATES a stuck row rather than resolving it, so a
	// flagged row stays in its own client's set permanently — deliberately: it
	// can legitimately confirm later. What changed is that it no longer blocks
	// every other client's connector run.
	var candidateIDs []int64
	idRows, err := tx.Query(ctx,
		`SELECT id FROM deliveries
		  WHERE channel='upwork_chat' AND status='sent'
		    AND sent_external_id IS NULL AND confirmed_at IS NULL
		    AND target_client_ref = $1`, messageRef.ClientID)
	if err != nil {
		return fmt.Errorf("shortlist upwork deliveries to confirm: %w", err)
	}
	for idRows.Next() {
		var id int64
		if err := idRows.Scan(&id); err != nil {
			idRows.Close()
			return fmt.Errorf("scan upwork delivery shortlist: %w", err)
		}
		candidateIDs = append(candidateIDs, id)
	}
	idRows.Close()
	if err := idRows.Err(); err != nil {
		return fmt.Errorf("iterate upwork delivery shortlist: %w", err)
	}
	if len(candidateIDs) == 0 {
		// Nothing to confirm for this client; nothing was locked.
		return nil
	}

	// Lock ONLY the shortlisted ids, re-stating the unresolved guards so a row
	// that resolved between the two statements drops out. Two concurrent
	// connector runs still lock in the same id DESC order — which holds for
	// overlapping SUBSETS too — so they block rather than deadlock.
	rows, err := tx.Query(ctx,
		`SELECT id, task_id, COALESCE(target_ref,''), COALESCE(body,'') FROM deliveries
		  WHERE id = ANY($1)
		    AND status='sent' AND sent_external_id IS NULL AND confirmed_at IS NULL
		  ORDER BY id DESC
		  FOR UPDATE`, candidateIDs)
	if err != nil {
		return fmt.Errorf("select upwork deliveries to confirm: %w", err)
	}
	var deliveryID, taskID int64
	matches := 0
	for rows.Next() {
		var id, task int64
		var targetRef, body string
		if err := rows.Scan(&id, &task, &targetRef, &body); err != nil {
			rows.Close()
			return fmt.Errorf("scan upwork delivery candidate: %w", err)
		}
		// A target_ref that does not parse is never a candidate. It does not rot
		// silently: the reconciler flags it, which is how a legacy or hand-written
		// target surfaces instead of sitting unconfirmable forever.
		deliveryRef, err := ParseThreadKey(targetRef)
		if err != nil {
			continue
		}
		if !SameConversation(messageRef, deliveryRef) {
			continue
		}
		if textmatch.NormalizedPrefix(body, upworkMatchPrefixLen) == want {
			matches++
			if deliveryID == 0 {
				deliveryID, taskID = id, task
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate upwork delivery candidates: %w", err)
	}
	if deliveryID == 0 || matches > 1 {
		return nil
	}

	// The IS NULL guards restated here are what keep a replay idempotent: an id
	// is never overwritten, so no second delivery_confirmed is emitted for the
	// same claim. Under FOR UPDATE they are belt-and-braces; the RowsAffected
	// check is not, because the event below must not fire on a stamp that did
	// not happen.
	tag, err := tx.Exec(ctx,
		`UPDATE deliveries SET sent_external_id=$2, confirmed_at=now(), updated_at=now()
		  WHERE id=$1 AND sent_external_id IS NULL AND confirmed_at IS NULL`,
		deliveryID, nm.ExternalMessageID)
	if err != nil {
		return fmt.Errorf("confirm upwork delivery %d: %w", deliveryID, err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit upwork delivery match %d: %w", deliveryID, err)
	}

	payload, _ := json.Marshal(map[string]any{"delivery_id": deliveryID, "matched_external_id": nm.ExternalMessageID})
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO task_events (task_id, event_type, payload) VALUES ($1, 'delivery_confirmed', $2)`,
		taskID, payload); err != nil {
		return fmt.Errorf("insert delivery_confirmed event: %w", err)
	}
	return nil
}

func (s *PGSink) markNormalized(ctx context.Context, rawItemID int64) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE raw_source_items SET normalized_at=now() WHERE id=$1`, rawItemID); err != nil {
		return fmt.Errorf("mark normalized: %w", err)
	}
	return nil
}
