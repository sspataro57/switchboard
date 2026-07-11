package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGSink is the ops-db side of the google connector: the ingest Sink plus the
// normalize/list store.
type PGSink struct {
	pool *pgxpool.Pool
}

func NewPGSink(pool *pgxpool.Pool) *PGSink {
	return &PGSink{pool: pool}
}

// ListAccounts returns every provider='google' account.
func (s *PGSink) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, account_email, calendar_in_availability
		 FROM source_accounts WHERE provider='google' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("select google accounts: %w", err)
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.Email, &a.CalendarInAvailability); err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
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
		 VALUES ($1,$2,$3,$4)`,
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
	id           int64
	externalID   string
	accountEmail string
	raw          json.RawMessage
}

// pendingRaw lists this connector's raw rows to normalize (pending, or all).
func (s *PGSink) pendingRaw(ctx context.Context, all bool) ([]rawItem, error) {
	q := `SELECT r.id, r.external_id, a.account_email, r.raw_json
	      FROM raw_source_items r
	      JOIN source_accounts a ON a.id = r.source_account_id
	      WHERE a.provider = 'google'`
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

	if _, err := s.pool.Exec(ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		    body_text, subject, sender, channel)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		 ON CONFLICT (raw_source_item_id) DO UPDATE SET
		   thread_id=EXCLUDED.thread_id, direction=EXCLUDED.direction,
		   external_message_id=EXCLUDED.external_message_id, sent_at=EXCLUDED.sent_at,
		   body_text=EXCLUDED.body_text, subject=EXCLUDED.subject,
		   sender=EXCLUDED.sender, channel=EXCLUDED.channel`,
		rawItemID, threadID, nm.Direction, nm.ExternalMessageID, nm.SentAt,
		nm.BodyText, nm.Subject, nm.Sender, nm.Channel); err != nil {
		return false, fmt.Errorf("upsert message for raw item %d: %w", rawItemID, err)
	}

	// Loop closure (invariant 5): our own send re-entering via ingestion
	// confirms its delivery row by Message-ID — first match only, and it
	// attaches to the task as a delivery_confirmed event, never re-triaged
	// (it is outbound by the direction rule anyway).
	if nm.Direction == "outbound" {
		if err := s.confirmDelivery(ctx, nm.ExternalMessageID); err != nil {
			return false, err
		}
	}
	return false, nil
}

// confirmDelivery closes the loop for a sent delivery whose Message-ID just
// re-entered via ingestion.
func (s *PGSink) confirmDelivery(ctx context.Context, messageID string) error {
	var deliveryID, taskID int64
	err := s.pool.QueryRow(ctx,
		`UPDATE deliveries SET confirmed_at=now(), updated_at=now()
		 WHERE sent_external_id=$1 AND confirmed_at IS NULL
		 RETURNING id, task_id`, messageID).Scan(&deliveryID, &taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // not one of ours, or already confirmed
	}
	if err != nil {
		return fmt.Errorf("confirm delivery for %s: %w", messageID, err)
	}
	payload, _ := json.Marshal(map[string]any{"delivery_id": deliveryID, "matched_message_id": messageID})
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO task_events (task_id, event_type, payload) VALUES ($1, 'delivery_confirmed', $2)`,
		taskID, payload); err != nil {
		return fmt.Errorf("insert delivery_confirmed event: %w", err)
	}
	return nil
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
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO normalized_events
		   (raw_source_item_id, starts_at, ends_at, attendees, title, status, transparency, all_day)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		 ON CONFLICT (raw_source_item_id) DO UPDATE SET
		   starts_at=EXCLUDED.starts_at, ends_at=EXCLUDED.ends_at,
		   attendees=EXCLUDED.attendees, title=EXCLUDED.title,
		   status=EXCLUDED.status, transparency=EXCLUDED.transparency,
		   all_day=EXCLUDED.all_day`,
		rawItemID, ne.StartsAt, ne.EndsAt, attendees, ne.Title, ne.Status,
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
