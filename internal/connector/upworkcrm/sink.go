package upworkcrm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
	return nil
}

func (s *PGSink) markNormalized(ctx context.Context, rawItemID int64) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE raw_source_items SET normalized_at=now() WHERE id=$1`, rawItemID); err != nil {
		return fmt.Errorf("mark normalized: %w", err)
	}
	return nil
}
