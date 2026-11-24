package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/provider"
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
	// MailThreadMaxMessages and MailThreadBodyCap are exported because the task
	// detail page renders the same conversation this tool returns, at the same
	// caps (SWT-65 D8). Two spellings of "how much of a thread is enough" would
	// drift apart the first time either moved; there is exactly one.
	MailThreadMaxMessages = 50
	MailThreadBodyCap     = 8 * 1024
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
	// RequireGeneralMail is the user profile's pin (user-mail-read, swb 4683):
	// "true" withholds every message the SWT-21 locality rule does not judge
	// general — the same mailClassJudge the attachment tools use — and counts
	// it as withheld_private. Omitted (the full profile, the drafts worker, the
	// dashboard) reads as before. A model never chooses it: the adapter
	// overwrites the key on the user profile.
	RequireGeneralMail string `json:"require_general_mail,omitempty"`
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
	if a.RequireGeneralMail != "" && a.RequireGeneralMail != "true" {
		return fmt.Errorf("require_general_mail %q: must be \"true\" or omitted", a.RequireGeneralMail)
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
	gated := a.RequireGeneralMail == "true"
	limit := a.Limit
	// Gated, the caller's limit is ignored (the attachment tools' thread form
	// takes none): the SQL LIMIT runs before the judge, so stepping limit up
	// while watching withheld_private would locate each private message.
	if gated || limit <= 0 || limit > MailThreadMaxMessages {
		limit = MailThreadMaxMessages
	}

	// Ascending: a thread is read forwards. The cap takes the OLDEST messages
	// because the opening of a conversation carries the ask; a tail would show
	// the replies without what they answer.
	rows, err := pool.Query(ctx, `
		SELECT m.id, r.source_account_id, m.thread_id,
		       COALESCE(t.thread_key,''), COALESCE(t.subject,''), COALESCE(m.subject,''),
		       COALESCE(m.external_message_id,''), COALESCE(m.sender,''), COALESCE(m.sent_at::text,''), m.direction,
		       left(COALESCE(m.body_text,''), $3)
		  FROM normalized_messages m
		  JOIN raw_source_items r ON r.id = m.raw_source_item_id
		  LEFT JOIN normalized_threads t ON t.id = m.thread_id
		 WHERE m.channel = 'gmail'
		   AND (($1::bigint IS NOT NULL AND m.thread_id = $1) OR ($1 IS NULL AND t.thread_key = $2))
		 ORDER BY m.sent_at ASC NULLS LAST, m.id ASC
		 LIMIT $4`,
		nullableID(a.ThreadID), strings.TrimSpace(a.ThreadKey), MailThreadBodyCap, limit)
	if err != nil {
		return nil, fmt.Errorf("read mail thread: %w", err)
	}
	type threadRow struct {
		msg                mailThreadMessage
		id, account        int64
		threadID           *int64
		threadKey, subject string
		ownSubject         string
	}
	var all []threadRow
	for rows.Next() {
		var r threadRow
		if err := rows.Scan(&r.id, &r.account, &r.threadID, &r.threadKey, &r.subject, &r.ownSubject,
			&r.msg.MessageID, &r.msg.Sender, &r.msg.SentAt, &r.msg.Direction, &r.msg.BodyText); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan thread message: %w", err)
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate thread messages: %w", err)
	}

	if a.RequireGeneralMail != "" && !gated {
		// The validator refuses this; refuse here too so a skipped validator
		// fails closed rather than ungated.
		return nil, fmt.Errorf("require_general_mail %q: must be \"true\" or omitted", a.RequireGeneralMail)
	}
	var judge *mailClassJudge
	if gated {
		judge = newMailClassJudge(ctx, pool)
	}
	threadKey, subject := "", ""
	withheld := 0
	messages := make([]mailThreadMessage, 0, len(all))
	for _, r := range all {
		if gated {
			// Fail closed: a judge error refuses the whole read rather than
			// show a message whose class is unknown.
			c, _, err := judge.class(mailAttachMsg{id: r.id, account: r.account, threadID: r.threadID, direction: r.msg.Direction})
			if err != nil {
				return nil, fmt.Errorf("judge mail locality: %w", err)
			}
			if c != provider.ClassGeneral {
				// A withheld message's body never leaves. The thread subject is
				// the FIRST ingested message's (the sink keeps it), which may be
				// a withheld one, so gated reads return the first SHOWN
				// message's own subject instead. A fully withheld thread returns
				// an empty key and subject; on a mixed thread the key is shown,
				// as mail_list_attachments shows it for the general messages.
				withheld++
				continue
			}
			if subject == "" {
				subject = r.ownSubject
			}
			threadKey = r.threadKey
			messages = append(messages, r.msg)
			continue
		}
		threadKey, subject = r.threadKey, r.subject
		messages = append(messages, r.msg)
	}
	out := map[string]any{"thread_key": threadKey, "subject": subject, "messages": messages}
	if gated {
		out["withheld_private"] = withheld
	}
	return marshalResult(out)
}

// nullableID renders 0 as SQL NULL so the query can branch on which identifier
// the caller supplied without building the SQL by concatenation.
func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}
