package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Read-only mail tools (SWT-11, criterion 16).
//
// Served from normalized_messages, NEVER from live IMAP. That is not a
// shortcut — a second provider connection from the tool layer would put a
// network dependency behind an agent call, bypass the one-funnel rule, and
// re-introduce exactly the raw_api surface invariant 3 forbids. Everything an
// agent can read here is something ingestion already captured and normalized.
//
// The consequence is stated in the tool descriptions rather than hidden: agents
// see only what has been ingested. Searching beyond the backfill window is an
// ingestion decision, not an MCP one.
//
// Both tools write nothing but their audit row, so they are not humanOnly and
// not snapshotGated — an ordinary worker may call them.

const (
	mailSearchDefaultLimit = 20
	mailSearchMaxLimit     = 50
	mailSnippetLen         = 300
	mailThreadMaxMessages  = 50
	mailThreadBodyCap      = 8 * 1024
)

// ---- mail_search --------------------------------------------------------------

type mailSearchArgs struct {
	Query     string `json:"query,omitempty"`
	From      string `json:"from,omitempty"`
	ThreadKey string `json:"thread_key,omitempty"`
	Since     string `json:"since,omitempty"`
	Until     string `json:"until,omitempty"`
	Direction string `json:"direction,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

func validateMailSearch(args []byte) error {
	var a mailSearchArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	// At least one selector. An unselective search over a six-figure corpus is
	// not a query, it is a dump — and a dump is how a mailbox ends up pasted
	// into a model context by accident.
	if strings.TrimSpace(a.Query) == "" &&
		strings.TrimSpace(a.From) == "" &&
		strings.TrimSpace(a.ThreadKey) == "" {
		return errors.New("missing selector: one of query, from or thread_key is required")
	}
	switch a.Direction {
	case "", "inbound", "outbound":
	default:
		return fmt.Errorf("direction %q is not inbound or outbound", a.Direction)
	}
	if a.Limit < 0 {
		return fmt.Errorf("limit %d is negative", a.Limit)
	}
	return nil
}

type mailSearchHit struct {
	// MessageID is the RFC 5322 Message-ID, not the row id: it is the identifier
	// that is stable across re-normalization and meaningful outside switchboard,
	// which is what an agent should be quoting back.
	MessageID string `json:"message_id"`
	// Nullable: thread_id is nullable in the schema, and reporting 0 for
	// "unthreaded" would be a lie an agent could act on.
	ThreadID  *int64 `json:"thread_id"`
	ThreadKey string `json:"thread_key"`
	Subject   string `json:"subject"`
	Sender    string `json:"sender"`
	SentAt    string `json:"sent_at"`
	Direction string `json:"direction"`
	Snippet   string `json:"snippet"`
}

func mailSearch(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a mailSearchArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	limit := a.Limit
	if limit <= 0 {
		limit = mailSearchDefaultLimit
	}
	if limit > mailSearchMaxLimit {
		limit = mailSearchMaxLimit
	}

	// Fetch one extra row to detect truncation honestly, rather than reporting
	// "truncated" whenever the page happens to be full.
	rows, err := pool.Query(ctx, `
		SELECT COALESCE(m.external_message_id,''), m.thread_id, COALESCE(t.thread_key,''), COALESCE(m.subject,''),
		       COALESCE(m.sender,''), COALESCE(m.sent_at::text,''), m.direction,
		       left(regexp_replace(COALESCE(m.body_text,''), '\s+', ' ', 'g'), $7)
		  FROM normalized_messages m
		  -- LEFT: thread_id is nullable, and a message that was never threaded is
		  -- still a message. An inner join would silently drop it from every
		  -- search, which is indistinguishable from it never having been ingested.
		  LEFT JOIN normalized_threads t ON t.id = m.thread_id
		 WHERE m.channel = 'gmail'
		   AND ($1 = '' OR m.subject ILIKE '%'||$1||'%' OR m.sender ILIKE '%'||$1||'%'
		                OR m.body_text ILIKE '%'||$1||'%')
		   AND ($2 = '' OR m.sender ILIKE '%'||$2||'%')
		   AND ($3 = '' OR t.thread_key = $3)
		   AND ($4 = '' OR m.sent_at >= $4::timestamptz)
		   AND ($5 = '' OR m.sent_at <= $5::timestamptz)
		   AND ($6 = '' OR m.direction = $6)
		 ORDER BY m.sent_at DESC NULLS LAST, m.id DESC
		 LIMIT $8`,
		strings.TrimSpace(a.Query), strings.TrimSpace(a.From), strings.TrimSpace(a.ThreadKey),
		strings.TrimSpace(a.Since), strings.TrimSpace(a.Until), a.Direction,
		mailSnippetLen, limit+1)
	if err != nil {
		return nil, fmt.Errorf("search mail: %w", err)
	}
	defer rows.Close()

	messages := make([]mailSearchHit, 0, limit)
	truncated := false
	for rows.Next() {
		if len(messages) == limit {
			truncated = true
			break
		}
		var h mailSearchHit
		if err := rows.Scan(&h.MessageID, &h.ThreadID, &h.ThreadKey, &h.Subject,
			&h.Sender, &h.SentAt, &h.Direction, &h.Snippet); err != nil {
			return nil, fmt.Errorf("scan mail hit: %w", err)
		}
		messages = append(messages, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate mail hits: %w", err)
	}
	return marshalResult(map[string]any{"messages": messages, "truncated": truncated})
}

// ---- mail_read_thread ----------------------------------------------------------

type mailReadThreadArgs struct {
	ThreadID  int64  `json:"thread_id,omitempty"`
	ThreadKey string `json:"thread_key,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

func validateMailReadThread(args []byte) error {
	var a mailReadThreadArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.ThreadID == 0 && strings.TrimSpace(a.ThreadKey) == "" {
		return errors.New("missing identifier: thread_id or thread_key is required")
	}
	if a.Limit < 0 {
		return fmt.Errorf("limit %d is negative", a.Limit)
	}
	return nil
}

type mailThreadMessage struct {
	MessageID string `json:"message_id"`
	Sender    string `json:"sender"`
	SentAt    string `json:"sent_at"`
	Direction string `json:"direction"`
	BodyText  string `json:"body_text"`
}

func mailReadThread(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a mailReadThreadArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	limit := a.Limit
	if limit <= 0 || limit > mailThreadMaxMessages {
		limit = mailThreadMaxMessages
	}

	// Ascending: a thread is read forwards. The cap takes the OLDEST messages
	// because the opening of a conversation carries the ask; a tail would show
	// the replies without what they answer.
	rows, err := pool.Query(ctx, `
		SELECT COALESCE(t.thread_key,''), COALESCE(t.subject,''),
		       COALESCE(m.external_message_id,''), COALESCE(m.sender,''), COALESCE(m.sent_at::text,''), m.direction,
		       left(COALESCE(m.body_text,''), $3)
		  FROM normalized_messages m
		  LEFT JOIN normalized_threads t ON t.id = m.thread_id
		 WHERE m.channel = 'gmail'
		   AND (($1::bigint IS NOT NULL AND m.thread_id = $1) OR ($1 IS NULL AND t.thread_key = $2))
		 ORDER BY m.sent_at ASC NULLS LAST, m.id ASC
		 LIMIT $4`,
		nullableID(a.ThreadID), strings.TrimSpace(a.ThreadKey), mailThreadBodyCap, limit)
	if err != nil {
		return nil, fmt.Errorf("read mail thread: %w", err)
	}
	defer rows.Close()

	threadKey, subject := "", ""
	messages := make([]mailThreadMessage, 0, limit)
	for rows.Next() {
		var m mailThreadMessage
		var k, s string
		if err := rows.Scan(&k, &s, &m.MessageID, &m.Sender, &m.SentAt, &m.Direction, &m.BodyText); err != nil {
			return nil, fmt.Errorf("scan thread message: %w", err)
		}
		threadKey, subject = k, s
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate thread messages: %w", err)
	}
	return marshalResult(map[string]any{
		"thread_key": threadKey, "subject": subject, "messages": messages,
	})
}

// nullableID renders 0 as SQL NULL so the query can branch on which identifier
// the caller supplied without building the SQL by concatenation.
func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}
