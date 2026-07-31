package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/textmatch"
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
		deliveryID, taskID, err = s.matchByBodyPrefix(ctx, msg)
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

// matchByBodyPrefix claims an unconfirmed delivery on this issue whose body opens
// the same way the ingested comment does, returning pgx.ErrNoRows when none does.
//
// The comparison is whitespace-normalized (SWT-16). It used to be `left(body,120)`
// in SQL, comparing raw bytes: the text we stored against the text Jira handed back
// after re-serializing it. Jira is entitled to alter line endings, trailing spaces,
// and blank-line runs without altering the message, and any such change made the
// match fail — permanently, because nothing retries a comparison that is already
// exact. An unclaimable row then has a second cost since SWT-16: capture sees an
// outbound comment no delivery claims and logs it as sent by hand, which is a false
// statement about a comment switchboard posted itself.
//
// Note this is NOT the scrub mismatch the SWT-16 review hypothesized — draft_delivery
// and update_delivery both store ScrubAIAttribution(body) and the scrub is
// idempotent, so the stored body already equals the sent body. The exposure is the
// provider round trip, not our own rewriting.
//
// Multi-match behavior is deliberately unchanged: newest candidate wins. Slack's
// matcher refuses to guess instead, which is stronger, but changing that here would
// alter which deliveries get confirmed and belongs with its own reasoning.
//
// Selection and stamping run in ONE transaction with the candidates locked FOR
// UPDATE, and the attempt-time floor below is what actually keeps a stale comment
// off a live retry. Both close a window the previous single-statement form also
// had — its outer clause was a bare `WHERE id = (subquery)` with no guards at all,
// so this is strictly tighter, not a new exposure.
func (s *PGSink) matchByBodyPrefix(ctx context.Context, msg NormalizedMessage) (int64, int64, error) {
	want := textmatch.NormalizedPrefix(msg.BodyText, jiraMatchPrefixLen)
	if want == "" {
		// An empty or whitespace-only comment would match every candidate whose
		// body normalizes to empty, claiming a delivery on no evidence at all.
		// slackweb's matcher refuses the same case up front.
		return 0, 0, pgx.ErrNoRows
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("begin jira delivery match: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The attempt-time floor is SWT-12's, ported from slackweb (0012's rationale
	// applies verbatim): a delivery whose send was attempted AFTER this comment
	// existed cannot be the thing that produced it. Without it, a row re-approved
	// and re-sent while this match is in flight could be stamped with the OLD
	// comment's id — recording a send that did happen against the wrong external
	// object, and losing the new comment's id. The two-minute allowance absorbs
	// clock skew between Postgres (send_attempted_at) and Jira (the comment's
	// created), which a strict comparison would turn into a PERMANENT refusal.
	//
	// COALESCE on body because it is nullable (0001) and scanning NULL into a
	// string ERRORS, failing the whole normalize run rather than just this match.
	//
	// FOR UPDATE holds the candidates until the stamp below commits. It cannot
	// block on an in-flight send: send_delivery commits its 'sending' transition
	// and releases the row BEFORE the HTTP call.
	rows, err := tx.Query(ctx,
		`SELECT id, task_id, COALESCE(body,'') FROM deliveries
		  WHERE channel='jira_comment' AND status IN ('sending','sent','failed')
		    AND sent_external_id IS NULL AND confirmed_at IS NULL
		    AND target_ref=$1
		    AND (send_attempted_at IS NULL OR send_attempted_at - interval '2 minutes' <= $2)
		  ORDER BY id DESC
		  FOR UPDATE`, msg.ThreadKey, msg.SentAt)
	if err != nil {
		return 0, 0, fmt.Errorf("select jira deliveries to confirm: %w", err)
	}
	var deliveryID, taskID int64
	for rows.Next() {
		var id, task int64
		var body string
		if err := rows.Scan(&id, &task, &body); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("scan jira delivery candidate: %w", err)
		}
		if deliveryID == 0 && textmatch.NormalizedPrefix(body, jiraMatchPrefixLen) == want {
			deliveryID, taskID = id, task
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterate jira delivery candidates: %w", err)
	}
	if deliveryID == 0 {
		return 0, 0, pgx.ErrNoRows
	}
	// The IS NULL guards make a --all replay idempotent: an id is never overwritten,
	// so no second delivery_confirmed event is emitted for the same claim. Under
	// FOR UPDATE they are belt-and-braces rather than the whole defense.
	tag, err := tx.Exec(ctx,
		`UPDATE deliveries SET sent_external_id=$2, confirmed_at=now(), status='sent',
		        sent_at=COALESCE(sent_at, now()), updated_at=now()
		  WHERE id=$1 AND sent_external_id IS NULL AND confirmed_at IS NULL`,
		deliveryID, msg.ExternalMessageID)
	if err != nil {
		return 0, 0, fmt.Errorf("confirm jira delivery %d: %w", deliveryID, err)
	}
	if tag.RowsAffected() == 0 {
		return 0, 0, pgx.ErrNoRows
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("commit jira delivery match %d: %w", deliveryID, err)
	}
	return deliveryID, taskID, nil
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
