package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/textmatch"
)

// PGSink is the ops-db side of the google connector: the ingest Sink plus the
// normalize/list store.
type PGSink struct {
	pool *pgxpool.Pool
}

func NewPGSink(pool *pgxpool.Pool) *PGSink {
	return &PGSink{pool: pool}
}

// EnsureBridgeAccount creates the token-free source account discovered from
// the sibling local connector. It deliberately leaves any existing encrypted
// refresh token, scopes, send flag, and calendar policy unchanged.
func (s *PGSink) EnsureBridgeAccount(ctx context.Context, discovered BridgeAccount) (Account, error) {
	email := strings.ToLower(strings.TrimSpace(discovered.Email))
	if email == "" {
		return Account{}, fmt.Errorf("Gmail bridge account %q has no email", discovered.Alias)
	}
	var account Account
	err := s.pool.QueryRow(ctx,
		`SELECT id, account_email, calendar_in_availability
		 FROM source_accounts
		 WHERE provider='google' AND lower(account_email)=lower($1)
		 ORDER BY id LIMIT 1`, email).
		Scan(&account.ID, &account.Email, &account.CalendarInAvailability)
	if err == nil {
		return account, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Account{}, fmt.Errorf("find bridge source account %s: %w", email, err)
	}
	err = s.pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email)
		 VALUES ('google', $1)
		 ON CONFLICT (provider, account_email) DO UPDATE SET account_email=EXCLUDED.account_email
		 RETURNING id, account_email, calendar_in_availability`, email).
		Scan(&account.ID, &account.Email, &account.CalendarInAvailability)
	if err != nil {
		return Account{}, fmt.Errorf("ensure bridge source account %s: %w", email, err)
	}
	return account, nil
}

// ListAccounts returns every provider='google' account, including its auth type
// and per-account endpoints (SWT-11) — callers choosing between the Gmail API
// and IMAP/SMTP read those off the row rather than inferring them.
func (s *PGSink) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.pool.Query(ctx, accountSelect+` ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("select google accounts: %w", err)
	}
	return scanAccounts(rows)
}

func (s *PGSink) Cursor(ctx context.Context, accountID int64) (Cursor, error) {
	var raw []byte
	if err := s.pool.QueryRow(ctx,
		`SELECT sync_cursor FROM source_accounts WHERE id=$1`, accountID).Scan(&raw); err != nil {
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

func (s *PGSink) StartRun(ctx context.Context, accountID int64, phase string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO sync_runs (source_account_id, status, stats)
		 VALUES ($1, 'running', jsonb_build_object('phase', $2::text)) RETURNING id`,
		accountID, phase).Scan(&id)
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
		`UPDATE sync_runs SET finished_at=now(), status=$2,
		        stats = stats || $3::jsonb, error=NULLIF($4,'')
		 WHERE id=$1`,
		runID, status, rawStats, errMsg); err != nil {
		return fmt.Errorf("finish sync run %d: %w", runID, err)
	}
	return nil
}

// RawHash reports the stored hash for a LIVE row. A superseded row answers
// "absent" on purpose: an event deleted and later re-created carries the same
// id, and if we reported its old hash the observation would either be skipped
// as unchanged or updated while still flagged superseded — either way the
// revived event would never normalize again. Absent routes it to InsertRaw,
// which upserts and clears the flag.
func (s *PGSink) RawHash(ctx context.Context, accountID int64, externalID string) (string, bool, error) {
	var h string
	err := s.pool.QueryRow(ctx,
		`SELECT content_hash FROM raw_source_items
		  WHERE source_account_id=$1 AND external_id=$2 AND superseded_at IS NULL`,
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
	// Upsert, not a bare insert: the row may exist but be superseded (see
	// RawHash). Re-observing it revives it — new bytes, cleared flag, and
	// normalized_at reset so the next pass rebuilds the canonical row.
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1,$2,$3,$4)
		 ON CONFLICT (source_account_id, external_id) DO UPDATE
		   SET raw_json=EXCLUDED.raw_json, content_hash=EXCLUDED.content_hash,
		       ingested_at=now(), normalized_at=NULL, superseded_at=NULL`,
		accountID, externalID, raw, hash); err != nil {
		return fmt.Errorf("insert raw item: %w", err)
	}
	return nil
}

func (s *PGSink) UpdateRaw(ctx context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE raw_source_items
		 SET raw_json=$3, content_hash=$4, ingested_at=now(), normalized_at=NULL,
		     superseded_at=NULL
		 WHERE source_account_id=$1 AND external_id=$2`,
		accountID, externalID, raw, hash); err != nil {
		return fmt.Errorf("update raw item: %w", err)
	}
	return nil
}

// ---- normalize store --------------------------------------------------------

type rawItem struct {
	id           int64
	externalID   string
	accountEmail string
	raw          json.RawMessage
}

// pendingRaw lists this connector's raw rows to normalize (pending, or all).
func (s *PGSink) pendingRaw(ctx context.Context, all bool) ([]rawItem, error) {
	// superseded_at filters BOTH paths, --all included: a Calendar reset marks
	// observations the replacement snapshot no longer carries, and replaying
	// them would resurrect events that no longer exist (0010).
	q := `SELECT r.id, r.external_id, a.account_email, r.raw_json
	      FROM raw_source_items r
	      JOIN source_accounts a ON a.id = r.source_account_id
	      WHERE a.provider = 'google' AND r.superseded_at IS NULL`
	if !all {
		q += ` AND r.normalized_at IS NULL`
	}
	q += ` ORDER BY r.external_id, r.id`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("select raw items: %w", err)
	}
	defer rows.Close()
	var out []rawItem
	for rows.Next() {
		var it rawItem
		if err := rows.Scan(&it.id, &it.externalID, &it.accountEmail, &it.raw); err != nil {
			return nil, fmt.Errorf("scan raw item: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ownEmailSet is the lowercase set of all google account emails (direction rule).
func (s *PGSink) ownEmailSet(ctx context.Context) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT lower(account_email) FROM source_accounts WHERE provider='google'`)
	if err != nil {
		return nil, fmt.Errorf("select own emails: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, fmt.Errorf("scan own email: %w", err)
		}
		out[e] = true
	}
	return out, rows.Err()
}

// upsertMessage writes the thread + message; cross-account Message-ID dedup:
// if another raw item already normalized this Message-ID, the copy is skipped
// (deduped=true) — the caller still stamps its raw item normalized_at.
func (s *PGSink) upsertMessage(ctx context.Context, rawItemID int64, nm NormalizedMessage) (deduped bool, err error) {
	// SELECT-first belt (the partial unique index is the suspenders).
	var existingRaw int64
	err = s.pool.QueryRow(ctx,
		`SELECT raw_source_item_id FROM normalized_messages
		 WHERE channel='gmail' AND external_message_id=$1`, nm.ExternalMessageID).Scan(&existingRaw)
	if err == nil && existingRaw != rawItemID {
		return true, nil // another copy won
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("dedup check %s: %w", nm.ExternalMessageID, err)
	}

	var threadID int64
	err = s.pool.QueryRow(ctx,
		`INSERT INTO normalized_threads (thread_key, subject, participants)
		 VALUES ($1, $2, '[]')
		 ON CONFLICT (thread_key) WHERE thread_key IS NOT NULL
		 DO UPDATE SET subject = COALESCE(normalized_threads.subject, EXCLUDED.subject)
		 RETURNING id`,
		nm.ThreadKey, nm.Subject).Scan(&threadID)
	if err != nil {
		return false, fmt.Errorf("upsert thread %s: %w", nm.ThreadKey, err)
	}

	// links travels in the SAME statement as the body (SWT-25 criterion 11), in
	// both the INSERT and the DO UPDATE list: one statement, so a re-normalize
	// can never leave a row with a fresh body and stale links, and the
	// `--normalize-only --all` backfill refreshes links with no new code path.
	linksJSON := []byte("[]")
	if len(nm.Links) > 0 {
		b, err := json.Marshal(nm.Links)
		if err != nil {
			return false, fmt.Errorf("marshal links for raw item %d: %w", rawItemID, err)
		}
		linksJSON = b
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		    body_text, subject, sender, channel, links)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		 ON CONFLICT (raw_source_item_id) DO UPDATE SET
		   thread_id=EXCLUDED.thread_id, direction=EXCLUDED.direction,
		   external_message_id=EXCLUDED.external_message_id, sent_at=EXCLUDED.sent_at,
		   body_text=EXCLUDED.body_text, subject=EXCLUDED.subject,
		   sender=EXCLUDED.sender, channel=EXCLUDED.channel, links=EXCLUDED.links`,
		rawItemID, threadID, nm.Direction, nm.ExternalMessageID, nm.SentAt,
		nm.BodyText, nm.Subject, nm.Sender, nm.Channel, linksJSON); err != nil {
		return false, fmt.Errorf("upsert message for raw item %d: %w", rawItemID, err)
	}

	// Loop closure (invariant 5): our own send re-entering via ingestion
	// confirms its delivery row by Message-ID — first match only, and it
	// attaches to the task as a delivery_confirmed event, never re-triaged
	// (it is outbound by the direction rule anyway).
	if nm.Direction == "outbound" {
		matched, err := s.confirmDelivery(ctx, nm.ExternalMessageID)
		if err != nil {
			return false, err
		}
		// Belt (SWT-11 criterion 15): a submission service may rewrite the
		// Message-ID we reserved. When that happens the exact match above finds
		// nothing and the delivery stays unconfirmed forever — which since SWT-16
		// also makes capture report our own send as sent by hand.
		//
		// ONLY when the primary matcher missed. Running it regardless would let
		// one real send confirm two rows: the exact match claims delivery A, A
		// then drops out of the candidate set, and the belt goes on to claim a
		// different delivery B that happens to share A's opening 120 characters.
		if !matched {
			if err := s.confirmDeliveryByBodyPrefix(ctx, rawItemID, nm); err != nil {
				return false, err
			}
		}
	}
	return false, nil
}

// confirmDelivery closes the loop for a sent delivery whose Message-ID just
// re-entered via ingestion.
// confirmDelivery reports whether this Message-ID claimed a delivery, so the
// caller can tell "already closed by the exact match" from "still open".
func (s *PGSink) confirmDelivery(ctx context.Context, messageID string) (bool, error) {
	var deliveryID, taskID int64
	err := s.pool.QueryRow(ctx,
		`UPDATE deliveries SET confirmed_at=now(), updated_at=now()
		 WHERE sent_external_id=$1 AND confirmed_at IS NULL
		 RETURNING id, task_id`, messageID).Scan(&deliveryID, &taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Either not ours, or ours and already confirmed. Both mean the belt has
		// nothing to add: a replay must not re-open a closed loop.
		var claimed bool
		if e := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM deliveries WHERE sent_external_id=$1)`,
			messageID).Scan(&claimed); e != nil {
			return false, fmt.Errorf("check delivery claim for %s: %w", messageID, e)
		}
		return claimed, nil
	}
	if err != nil {
		return false, fmt.Errorf("confirm delivery for %s: %w", messageID, err)
	}
	payload, _ := json.Marshal(map[string]any{"delivery_id": deliveryID, "matched_message_id": messageID})
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO task_events (task_id, event_type, payload) VALUES ($1, 'delivery_confirmed', $2)`,
		taskID, payload); err != nil {
		return true, fmt.Errorf("insert delivery_confirmed event: %w", err)
	}
	return true, nil
}

// upsertEvent writes one normalized_events row (upsert on raw_source_item_id).
func (s *PGSink) upsertEvent(ctx context.Context, rawItemID int64, ne NormalizedEvent) error {
	attendees, err := json.Marshal(ne.Attendees)
	if err != nil {
		return fmt.Errorf("marshal attendees: %w", err)
	}
	if ne.Attendees == nil {
		attendees = []byte(`[]`)
	}
	var startsAt, endsAt any = ne.StartsAt, ne.EndsAt
	if ne.StartsAt.IsZero() {
		startsAt = nil
	}
	if ne.EndsAt.IsZero() {
		endsAt = nil
	}
	if _, err := s.pool.Exec(ctx,
		// SELECT ... WHERE superseded_at IS NULL rather than VALUES: normalize
		// snapshots live raw rows and upserts them later, so a reset running in
		// between could be undone by the stale normalizer writing the event
		// back to confirmed — while the raw row stays superseded, making it
		// unreachable by any future pass. Re-checking here closes that window.
		`INSERT INTO normalized_events
		   (raw_source_item_id, starts_at, ends_at, attendees, title, status, transparency, all_day)
		 SELECT $1,$2,$3,$4,$5,$6,$7,$8
		  WHERE EXISTS (SELECT 1 FROM raw_source_items r
		                 WHERE r.id = $1 AND r.superseded_at IS NULL)
		 ON CONFLICT (raw_source_item_id) DO UPDATE SET
		   starts_at=EXCLUDED.starts_at, ends_at=EXCLUDED.ends_at,
		   attendees=EXCLUDED.attendees, title=EXCLUDED.title,
		   status=EXCLUDED.status, transparency=EXCLUDED.transparency,
		   all_day=EXCLUDED.all_day`,
		rawItemID, startsAt, endsAt, attendees, ne.Title, ne.Status,
		ne.Transparency, ne.AllDay); err != nil {
		return fmt.Errorf("upsert event for raw item %d: %w", rawItemID, err)
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

// bridgeAccountLockNS namespaces the per-account advisory lock (siblings:
// orchestrator 0x51570005, triage 0x51570006).
const bridgeAccountLockNS = 0x51570007

// LockAccount serializes one account's whole Gmail+Calendar pass. Without it
// two runs interleave: each reads the cursor, spends a long time in the leaf,
// and writes back — so an older export can overwrite newer raw JSON while the
// newer run commits its later sync token, leaving stale observations paired
// with a current cursor that no future delta will ever repair.
//
// Returns ok=false when another run holds the account; the caller skips it
// rather than failing, since the holder is doing the same work.
func (s *PGSink) LockAccount(ctx context.Context, accountID int64) (release func(), ok bool, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire lock connection: %w", err)
	}
	// Session-scoped, not xact-scoped: the lock must outlive the many separate
	// statements of a pass, and it is held on this one pinned connection.
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`,
		bridgeAccountLockNS, accountID).Scan(&ok); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("try account lock %d: %w", accountID, err)
	}
	if !ok {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		// Fresh bounded context: a cancelled ctx must still release, but an
		// unbounded one can hang cleanup on a degraded server.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		var unlocked bool
		err := conn.QueryRow(cleanupCtx,
			`SELECT pg_advisory_unlock($1, $2)`, bridgeAccountLockNS, accountID).Scan(&unlocked)
		if err != nil || !unlocked {
			// Returning a still-locked session to the pool would make every
			// later pass skip this account as busy, forever, exiting 0. Destroy
			// the connection instead so the lock dies with it, and say so.
			slog.Error("advisory unlock failed; destroying the connection",
				"account_id", accountID, "unlocked", unlocked, "err", err)
			hijacked := conn.Hijack()
			_ = hijacked.Close(cleanupCtx)
			return
		}
		conn.Release()
	}, true, nil
}

// SaveCursorField writes ONE cursor key. SaveCursor replaces the whole JSON
// blob, so a Gmail save could clobber a Calendar token written by a concurrent
// writer (direct mode, a manual run) between its read and its write.
func (s *PGSink) SaveCursorField(ctx context.Context, accountID int64, field string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal cursor field %s: %w", field, err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE source_accounts
		    SET sync_cursor = jsonb_set(COALESCE(sync_cursor, '{}'::jsonb), ARRAY[$2], $3::jsonb, true)
		  WHERE id = $1`, accountID, field, encoded); err != nil {
		return fmt.Errorf("save cursor field %s: %w", field, err)
	}
	return nil
}

// SupersedeAbsentCalendar applies a Calendar reset as a REPLACEMENT: every
// calendar observation INSIDE the replacement window that the snapshot no
// longer carries is stamped superseded and its normalized event cancelled, in
// one transaction, BEFORE the new sync token is saved.
//
// windowFrom/windowTo bound it to what the export actually asked Google for.
// Without that bound an event from last year — outside the queried window and
// therefore absent for a reason that has nothing to do with deletion — would be
// cancelled by every reset.
func (s *PGSink) SupersedeAbsentCalendar(ctx context.Context, accountID int64, keep []string, windowFrom, windowTo time.Time) (int, error) {
	if len(keep) == 0 {
		// An empty replacement is indistinguishable from a broken leaf, and
		// acting on it would cancel the whole window. Absence of evidence.
		return 0, fmt.Errorf("refusing to apply an empty Calendar reset snapshot for account %d", accountID)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin calendar reset: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// startsAt reads the event's own start out of the exact provider JSON:
	// dateTime for timed events, date for all-day ones. Rows with neither are
	// tombstones already and are left alone.
	const startsAt = `COALESCE(NULLIF(raw_json->'start'->>'dateTime',''), NULLIF(raw_json->'start'->>'date',''))`
	tag, err := tx.Exec(ctx,
		`UPDATE raw_source_items
		    SET superseded_at = now()
		  WHERE source_account_id = $1
		    AND external_id LIKE 'calendar:%'
		    AND superseded_at IS NULL
		    AND NOT (external_id = ANY($2::text[]))
		    AND `+startsAt+` IS NOT NULL
		    AND (`+startsAt+`)::timestamptz >= $3
		    AND (`+startsAt+`)::timestamptz < $4`, accountID, keep, windowFrom, windowTo)
	if err != nil {
		return 0, fmt.Errorf("supersede absent calendar raw: %w", err)
	}
	superseded := int(tag.RowsAffected())

	// The event stays as a cancelled row rather than being deleted: the funnel
	// keeps its history, and availability ignores cancelled spans.
	if _, err := tx.Exec(ctx,
		`UPDATE normalized_events e
		    SET status = 'cancelled'
		   FROM raw_source_items r
		  WHERE e.raw_source_item_id = r.id
		    AND r.source_account_id = $1
		    AND r.superseded_at IS NOT NULL
		    AND e.status IS DISTINCT FROM 'cancelled'`, accountID); err != nil {
		return 0, fmt.Errorf("cancel superseded calendar events: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit calendar reset: %w", err)
	}
	return superseded, nil
}

// mailMatchPrefixLen is the comparison width, matching the slack and jira
// matchers so one idiom governs every post-hoc confirmation.
const mailMatchPrefixLen = 120

// confirmDeliveryByBodyPrefix claims an unconfirmed gmail delivery whose body
// opens the way this outbound message does.
//
// Runs only after the exact Message-ID match failed. It exists because SMTP
// submission is ALLOWED to replace the Message-ID we reserved: that id is our
// idempotency token, not a promise the relay keeps. Whether Gmail's submission
// service actually rewrites it is exactly what the SPEC's live smoke is meant to
// determine — this belt is insurance, not a statement that it does.
//
// Note the asymmetry with the slack and jira matchers. Those look for rows with
// sent_external_id IS NULL, because a browser click and a REST post learn the id
// only afterwards. Gmail RESERVES its Message-ID before sending, so a belt
// candidate here already HAS one — what is missing is confirmation that it
// landed. Filtering on a NULL id would therefore match nothing, ever.
//
// Three guards, each load-bearing:
//
//   - sent_external_id is NEVER overwritten. It is what stops a resend, and
//     replacing it with an observed id would break the one thing invariant 4
//     rests on. The observed id goes into the event payload instead, as evidence.
//   - Scoped to the same from_account_id: another mailbox's pending reply is not
//     a candidate for this mailbox's send, however similar the text.
//   - An attempt-time floor. INSTITUTIONAL_KNOWLEDGE makes this unconditional
//     after SWT-16: any post-hoc matcher identifying our own message by CONTENT
//     needs a lower time bound, because content alone cannot tell two identical
//     sends apart. Without it a --full re-run or a UIDVALIDITY resync replays 90
//     days of Sent mail, and a historical message whose opening matches a
//     currently-pending delivery would confirm it. The two-minute allowance
//     absorbs clock skew between Postgres and the provider.
//   - Multi-match refuses rather than guesses. Two pending replies sharing 120
//     characters is realistic for short mail, and stamping the wrong row would
//     record a real send against the wrong task and leave the other
//     unconfirmable forever.
func (s *PGSink) confirmDeliveryByBodyPrefix(ctx context.Context, rawItemID int64, nm NormalizedMessage) error {
	want := textmatch.NormalizedPrefix(nm.BodyText, mailMatchPrefixLen)
	if want == "" {
		// An empty body would match any candidate that also normalizes to empty,
		// claiming a delivery on no evidence at all.
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin gmail delivery match: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx,
		`SELECT d.id, d.task_id, COALESCE(d.body,'')
		   FROM deliveries d
		  WHERE d.channel='gmail' AND d.status IN ('sending','sent','failed')
		    AND d.confirmed_at IS NULL
		    AND d.from_account_id = (SELECT source_account_id FROM raw_source_items WHERE id=$1)
		    AND (COALESCE(d.send_attempted_at, d.sent_at) IS NULL
		         OR COALESCE(d.send_attempted_at, d.sent_at) - interval '2 minutes' <= $2)
		  ORDER BY d.id DESC
		  FOR UPDATE OF d`, rawItemID, nm.SentAt)
	if err != nil {
		return fmt.Errorf("select gmail deliveries to confirm: %w", err)
	}
	var deliveryID, taskID int64
	matches := 0
	for rows.Next() {
		var id, task int64
		var body string
		if err := rows.Scan(&id, &task, &body); err != nil {
			rows.Close()
			return fmt.Errorf("scan gmail delivery candidate: %w", err)
		}
		if textmatch.NormalizedPrefix(body, mailMatchPrefixLen) == want {
			matches++
			if deliveryID == 0 {
				deliveryID, taskID = id, task
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate gmail delivery candidates: %w", err)
	}
	if deliveryID == 0 || matches > 1 {
		return nil
	}

	// confirmed_at IS NULL is the idempotence guard: a --all replay re-runs this
	// match, finds the row already confirmed, and emits no second event.
	// confirmed_at ONLY — deliberately not a status promotion. Flipping
	// 'sending' to 'sent' here would emit no delivery_sent event, so the
	// orchestrator's R8 never fires and the parent task sits at done_locally
	// forever. Confirmation is evidence the message landed; the lifecycle
	// transition belongs to the path that owns it.
	tag, err := tx.Exec(ctx,
		`UPDATE deliveries SET confirmed_at=now(), updated_at=now()
		  WHERE id=$1 AND confirmed_at IS NULL`, deliveryID)
	if err != nil {
		return fmt.Errorf("confirm gmail delivery %d: %w", deliveryID, err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	// The OBSERVED id is recorded here and nowhere else: evidence of what actually
	// landed, never a replacement for the token we reserved.
	payload, _ := json.Marshal(map[string]any{
		"delivery_id":        deliveryID,
		"matched_message_id": nm.ExternalMessageID,
		"matched_by":         "body_prefix",
	})
	if _, err := tx.Exec(ctx,
		`INSERT INTO task_events (task_id, event_type, payload) VALUES ($1,'delivery_confirmed',$2)`,
		taskID, payload); err != nil {
		return fmt.Errorf("insert delivery_confirmed event: %w", err)
	}
	return tx.Commit(ctx)
}
