package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGSink is the ops-db side of the jira connector.
type PGSink struct {
	pool *pgxpool.Pool
}

func NewPGSink(pool *pgxpool.Pool) *PGSink {
	return &PGSink{pool: pool}
}

// NewSink is the constructor name the integration surface pins.
func NewSink(pool *pgxpool.Pool) *PGSink { return NewPGSink(pool) }

// ListAccounts returns every provider='jira' account.
func (s *PGSink) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, account_email, COALESCE(domain_default,''), scopes
		 FROM source_accounts WHERE provider='jira' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("select jira accounts: %w", err)
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.Email, &a.SiteBaseURL, &a.Projects); err != nil {
			return nil, fmt.Errorf("scan jira account: %w", err)
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
		`UPDATE source_accounts SET sync_cursor = sync_cursor || $2::jsonb WHERE id=$1`, accountID, raw); err != nil {
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
		 VALUES ($1,$2,$3,$4)`, accountID, externalID, raw, hash); err != nil {
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
	accountID  int64
	externalID string
	raw        json.RawMessage
}

type accountMeta struct {
	siteHost     string
	ownAccountID string
}

func (s *PGSink) accountMeta(ctx context.Context) (map[int64]accountMeta, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, COALESCE(domain_default,''), COALESCE(sync_cursor->>'own_account_id','')
		 FROM source_accounts WHERE provider='jira'`)
	if err != nil {
		return nil, fmt.Errorf("select jira account meta: %w", err)
	}
	defer rows.Close()
	out := map[int64]accountMeta{}
	for rows.Next() {
		var id int64
		var base, own string
		if err := rows.Scan(&id, &base, &own); err != nil {
			return nil, fmt.Errorf("scan account meta: %w", err)
		}
		out[id] = accountMeta{siteHost: SiteHost(base), ownAccountID: own}
	}
	return out, rows.Err()
}

func (s *PGSink) pendingRaw(ctx context.Context, all bool) ([]rawItem, error) {
	q := `SELECT r.id, r.source_account_id, r.external_id, r.raw_json
	      FROM raw_source_items r
	      JOIN source_accounts a ON a.id = r.source_account_id
	      WHERE a.provider = 'jira'`
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
		if err := rows.Scan(&it.id, &it.accountID, &it.externalID, &it.raw); err != nil {
			return nil, fmt.Errorf("scan raw item: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// upsertThreadMessage writes the thread + message and runs loop closure for
// outbound comments (invariant 5): id equality against sent deliveries, plus
// the post-hoc prefix matcher for ambiguous send failures.
func (s *PGSink) upsertThreadMessage(ctx context.Context, rawItemID int64, th NormalizedThread, msg NormalizedMessage) error {
	var threadID int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO normalized_threads (thread_key, subject, participants)
		 VALUES ($1, NULLIF($2,''), '[]')
		 ON CONFLICT (thread_key) WHERE thread_key IS NOT NULL
		 DO UPDATE SET subject = COALESCE(EXCLUDED.subject, normalized_threads.subject)
		 RETURNING id`,
		msg.ThreadKey, th.Subject).Scan(&threadID)
	if err != nil {
		return fmt.Errorf("upsert thread %s: %w", msg.ThreadKey, err)
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
		rawItemID, threadID, msg.Direction, msg.ExternalMessageID, msg.SentAt,
		msg.BodyText, msg.Subject, msg.Sender, msg.Channel); err != nil {
		return fmt.Errorf("upsert message for raw item %d: %w", rawItemID, err)
	}

	if msg.Direction == "outbound" {
		if err := s.confirmDelivery(ctx, msg); err != nil {
			return err
		}
	}
	return nil
}

const jiraMatchPrefixLen = 120

// confirmDelivery closes the loop: exact sent_external_id equality first, then
// the post-hoc prefix match (an ambiguous send failure leaves a sent comment
// on Jira with no id recorded — the poller re-ingests it and claims the
// delivery here, closing the duplicate window).
func (s *PGSink) confirmDelivery(ctx context.Context, msg NormalizedMessage) error {
	var deliveryID, taskID int64
	err := s.pool.QueryRow(ctx,
		`UPDATE deliveries SET confirmed_at=now(), updated_at=now()
		 WHERE sent_external_id=$1 AND confirmed_at IS NULL
		 RETURNING id, task_id`, msg.ExternalMessageID).Scan(&deliveryID, &taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		// post-hoc: body-prefix match against unconfirmed jira deliveries on
		// this thread (target_ref = thread_key).
		err = s.pool.QueryRow(ctx,
			`UPDATE deliveries SET sent_external_id=$1, confirmed_at=now(), status='sent',
			        sent_at=COALESCE(sent_at, now()), updated_at=now()
			 WHERE id = (
			   SELECT id FROM deliveries
			   WHERE channel='jira_comment' AND status IN ('sending','sent','failed')
			     AND sent_external_id IS NULL AND confirmed_at IS NULL
			     AND target_ref=$2
			     AND left(body, $3) = left($4, $3)
			   ORDER BY id DESC LIMIT 1)
			 RETURNING id, task_id`,
			msg.ExternalMessageID, msg.ThreadKey, jiraMatchPrefixLen, msg.BodyText).Scan(&deliveryID, &taskID)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("confirm jira delivery for %s: %w", msg.ExternalMessageID, err)
	}
	payload, _ := json.Marshal(map[string]any{"delivery_id": deliveryID, "matched_message_id": msg.ExternalMessageID})
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

// ClientFactory builds a per-account REST client.
type ClientFactory func(ctx context.Context, acct Account) (*Client, error)

// Run ingests every provider='jira' account.
func Run(ctx context.Context, sink *PGSink, factory ClientFactory, cfg Config) (Stats, error) {
	var total Stats
	accounts, err := sink.ListAccounts(ctx)
	if err != nil {
		return total, fmt.Errorf("list jira accounts: %w", err)
	}
	if len(accounts) == 0 {
		return total, fmt.Errorf("no provider='jira' accounts exist; run jira-auth add first")
	}
	for _, acct := range accounts {
		c, err := factory(ctx, acct)
		if err != nil {
			return total, fmt.Errorf("build client for %s: %w", acct.Email, err)
		}
		st, err := Ingest(ctx, c, sink, acct, cfg)
		total.IssuesListed += st.IssuesListed
		total.IssuesFetched += st.IssuesFetched
		total.CommentsFetched += st.CommentsFetched
		total.RawInserted += st.RawInserted
		total.RawUpdated += st.RawUpdated
		total.RawUnchanged += st.RawUnchanged
		if err != nil {
			return total, fmt.Errorf("jira ingest for %s: %w", acct.Email, err)
		}
	}
	return total, nil
}
