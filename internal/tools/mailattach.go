package tools

// SWT-42: mail_list_attachments / mail_read_attachment — read-only access to the
// attachments ingestion already stored (raw_source_items.raw_json), for every
// Claude Code session, including the user-scope install (owner decision O1).
//
// Served from the stored raw bytes, never from a live mailbox (the mail.go
// rule). The content is someone else's text: descriptions and the MCP
// Instructions say so, and a hosted model only ever sees attachments of
// shareable mail — the SWT-21 locality rule, applied here in one place
// (mailClassJudge), for every caller and profile.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/provider"
)

const (
	// mailAttachmentTextCap bounds one inline read (D7): it covers the worked
	// example's 84,274-byte Response.json in one call.
	mailAttachmentTextCap = 100 << 10
	// mailAttachmentFileTTL is how long a saved attachment stays in the cache (D9).
	mailAttachmentFileTTL        = 7 * 24 * time.Hour
	mailAttachFinderDefaultLimit = 10
	mailAttachFinderMaxLimit     = 25
	// mailboxCleanMinFiled is O2's evidence floor: a mailbox's unfiled mail is
	// shareable only once at least this many of its messages are filed, none of
	// them under a local_only project.
	mailboxCleanMinFiled = 20
	// mailAttachFinderScanCap bounds how many candidate messages one finder call
	// examines, so a broad sender match cannot turn into a mailbox walk.
	mailAttachFinderScanCap = 2000
	// mailAttachFinderByteBudget bounds the stored mail one finder call loads and
	// MIME-walks (Codex review): candidates are fetched one raw row at a time and
	// the walk stops, reporting truncated, once this much has been read.
	mailAttachFinderByteBudget = 64 << 20
	mailAttachSniffWindow      = 8 << 10
)

// ---- args and validation ------------------------------------------------------

type mailListAttachmentsArgs struct {
	RawSourceItemID int64  `json:"raw_source_item_id,omitempty"`
	MessageID       string `json:"message_id,omitempty"`
	ThreadID        int64  `json:"thread_id,omitempty"`
	ThreadKey       string `json:"thread_key,omitempty"`
	From            string `json:"from,omitempty"`
	Subject         string `json:"subject,omitempty"`
	Since           string `json:"since,omitempty"`
	Until           string `json:"until,omitempty"`
	Limit           *int   `json:"limit,omitempty"`
	WorkerID        string `json:"worker_id,omitempty"`
}

func (a mailListAttachmentsArgs) idKinds() int {
	n := 0
	if a.RawSourceItemID != 0 {
		n++
	}
	if strings.TrimSpace(a.MessageID) != "" {
		n++
	}
	if a.ThreadID != 0 {
		n++
	}
	if strings.TrimSpace(a.ThreadKey) != "" {
		n++
	}
	return n
}

func (a mailListAttachmentsArgs) finderFields() bool {
	return a.From != "" || a.Subject != "" || a.Since != "" || a.Until != "" || a.Limit != nil
}

func parseMailListAttachments(args []byte) (mailListAttachmentsArgs, error) {
	var a mailListAttachmentsArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return a, fmt.Errorf("parse args: %w", err)
	}
	if a.Limit != nil && *a.Limit < 0 {
		return a, fmt.Errorf("limit %d is negative", *a.Limit)
	}
	switch n := a.idKinds(); {
	case n > 1:
		return a, errors.New("give exactly one of raw_source_item_id, message_id, thread_id, thread_key")
	case n == 1 && a.finderFields():
		return a, errors.New("an identifier and finder fields (from, subject, since, until, limit) are two ways to pick; use one")
	case n == 0 && strings.TrimSpace(a.From) == "" && strings.TrimSpace(a.Subject) == "":
		return a, errors.New("missing selector: give an identifier (raw_source_item_id, message_id, thread_id, thread_key) or from/subject")
	}
	return a, nil
}

func validateMailListAttachments(args []byte) error {
	_, err := parseMailListAttachments(args)
	return err
}

type mailReadAttachmentArgs struct {
	RawSourceItemID int64  `json:"raw_source_item_id,omitempty"`
	MessageID       string `json:"message_id,omitempty"`
	ThreadID        int64  `json:"thread_id,omitempty"`
	ThreadKey       string `json:"thread_key,omitempty"`
	Index           *int   `json:"index,omitempty"`
	Filename        string `json:"filename,omitempty"`
	PartID          string `json:"part_id,omitempty"`
	Offset          int    `json:"offset,omitempty"`
	ToFile          bool   `json:"to_file,omitempty"`
	WorkerID        string `json:"worker_id,omitempty"`
}

func parseMailReadAttachment(args []byte) (mailReadAttachmentArgs, error) {
	var a mailReadAttachmentArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return a, fmt.Errorf("parse args: %w", err)
	}
	if a.ThreadID != 0 || strings.TrimSpace(a.ThreadKey) != "" {
		return a, errors.New("a thread is not a read identifier: give raw_source_item_id or message_id (list the thread's attachments first)")
	}
	hasRaw, hasMID := a.RawSourceItemID != 0, strings.TrimSpace(a.MessageID) != ""
	switch {
	case hasRaw && hasMID:
		return a, errors.New("give exactly one of raw_source_item_id, message_id")
	case !hasRaw && !hasMID:
		return a, errors.New("missing identifier: raw_source_item_id or message_id is required")
	}
	selectors := 0
	if a.Index != nil {
		selectors++
		if *a.Index < 1 {
			return a, fmt.Errorf("index %d is out of range (attachments are numbered from 1)", *a.Index)
		}
	}
	if a.Filename != "" {
		selectors++
	}
	if a.PartID != "" {
		selectors++
	}
	switch {
	case selectors == 0:
		return a, errors.New("missing part selector: give index, filename or part_id")
	case selectors > 1:
		return a, errors.New("give exactly one of index, filename, part_id")
	}
	if a.Offset < 0 {
		return a, fmt.Errorf("offset %d is negative", a.Offset)
	}
	return a, nil
}

func validateMailReadAttachment(args []byte) error {
	_, err := parseMailReadAttachment(args)
	return err
}

// ---- the message and its class ----------------------------------------------

type mailAttachMsg struct {
	id, raw, account int64
	threadID         *int64
	messageID        string
	threadKey        string
	subject, sender  string
	sentAt           string
	direction        string
	rawJSON          json.RawMessage
}

const mailAttachMsgSelect = `
	SELECT m.id, m.raw_source_item_id, r.source_account_id, m.thread_id,
	       COALESCE(m.external_message_id,''), COALESCE(t.thread_key,''), COALESCE(m.subject,''),
	       COALESCE(m.sender,''), COALESCE(m.sent_at::text,''), m.direction, r.raw_json
	  FROM normalized_messages m
	  JOIN raw_source_items r ON r.id = m.raw_source_item_id
	  LEFT JOIN normalized_threads t ON t.id = m.thread_id
	 WHERE m.channel = 'gmail'`

// mailAttachHeaderSelect is mailAttachMsgSelect without the raw row: the
// finder scans up to mailAttachFinderScanCap candidates and loads raw_json per
// candidate, never all of them at once.
var mailAttachHeaderSelect = strings.Replace(mailAttachMsgSelect, "r.raw_json", "NULL::jsonb", 1)

// likeEscape makes s a literal substring for LIKE/ILIKE (default escape
// character backslash): the finder's from/subject are substrings, never
// patterns, so "%" or "_" cannot widen a search to the whole mailbox.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func scanMailAttachMsg(row pgx.Row) (mailAttachMsg, error) {
	var m mailAttachMsg
	err := row.Scan(&m.id, &m.raw, &m.account, &m.threadID, &m.messageID, &m.threadKey,
		&m.subject, &m.sender, &m.sentAt, &m.direction, &m.rawJSON)
	return m, err
}

// loadOneMailAttachMsg resolves an explicit identifier to its normalized message.
func loadOneMailAttachMsg(ctx context.Context, pool *pgxpool.Pool, rawID int64, messageID string) (mailAttachMsg, error) {
	var (
		m   mailAttachMsg
		err error
	)
	if rawID != 0 {
		m, err = scanMailAttachMsg(pool.QueryRow(ctx, mailAttachMsgSelect+` AND m.raw_source_item_id = $1`, rawID))
		if errors.Is(err, pgx.ErrNoRows) {
			// D6: a cross-account duplicate (or an un-normalized row) has no message
			// of its own; guessing its winner would be a second dedup spelling.
			return m, fmt.Errorf("raw_source_item_id %d has no normalized mail message (a cross-account duplicate, "+
				"or not normalized yet); use message_id", rawID)
		}
	} else {
		m, err = scanMailAttachMsg(pool.QueryRow(ctx, mailAttachMsgSelect+` AND m.external_message_id = $1`,
			strings.TrimSpace(messageID)))
		if errors.Is(err, pgx.ErrNoRows) {
			return m, fmt.Errorf("no ingested mail message has message_id %q", messageID)
		}
	}
	if err != nil {
		return m, fmt.Errorf("load mail message: %w", err)
	}
	return m, nil
}

// mailClassJudge computes the SWT-21 class of mail messages, caching each
// mailbox's O2 verdict for the call.
type mailClassJudge struct {
	ctx   context.Context
	pool  *pgxpool.Pool
	clean map[int64]bool
}

func newMailClassJudge(ctx context.Context, pool *pgxpool.Pool) *mailClassJudge {
	return &mailClassJudge{ctx: ctx, pool: pool, clean: map[int64]bool{}}
}

// mailboxClean is O2: at least mailboxCleanMinFiled of the account's inbound
// messages are filed under a project (latest decision per message, any mode),
// and none is filed under a local_only project. Derived from data at call
// time; fails closed.
func (j *mailClassJudge) mailboxClean(account int64) (bool, error) {
	if v, ok := j.clean[account]; ok {
		return v, nil
	}
	var filed, localOnly int
	err := j.pool.QueryRow(j.ctx, `
		WITH latest AS (
		  SELECT DISTINCT ON (cd.message_id) cd.message_id, cd.project_id
		    FROM capture_decisions cd
		    JOIN normalized_messages m ON m.id = cd.message_id AND m.direction = 'inbound'
		    JOIN raw_source_items r ON r.id = m.raw_source_item_id AND r.source_account_id = $1
		   ORDER BY cd.message_id, cd.id DESC)
		SELECT count(*) FILTER (WHERE l.project_id IS NOT NULL),
		       count(*) FILTER (WHERE p.ai_locality = 'local_only')
		  FROM latest l LEFT JOIN projects p ON p.id = l.project_id`, account).Scan(&filed, &localOnly)
	if err != nil {
		return false, fmt.Errorf("mailbox filing history: %w", err)
	}
	v := filed >= mailboxCleanMinFiled && localOnly == 0
	j.clean[account] = v
	return v, nil
}

// inboundClass is one inbound message's class and, when restricted, why.
func (j *mailClassJudge) inboundClass(messageID, account int64) (provider.Class, string, error) {
	var hasProject, localOnly bool
	err := j.pool.QueryRow(j.ctx, `
		SELECT cd.project_id IS NOT NULL, COALESCE(p.ai_locality = 'local_only', false)
		  FROM capture_decisions cd LEFT JOIN projects p ON p.id = cd.project_id
		 WHERE cd.message_id = $1 ORDER BY cd.id DESC LIMIT 1`, messageID).Scan(&hasProject, &localOnly)
	state := provider.AttrUnmatched
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		state = provider.AttrUnseen
	case err != nil:
		return provider.ClassRestricted, "", fmt.Errorf("latest capture decision: %w", err)
	case hasProject:
		state = provider.AttrProject
	}
	if state == provider.AttrProject {
		if c := provider.ClassOf(state, localOnly); c != provider.ClassGeneral {
			return c, "filed under a local-only project", nil
		}
		return provider.ClassGeneral, "", nil
	}
	clean, err := j.mailboxClean(account)
	if err != nil {
		return provider.ClassRestricted, "", err
	}
	if clean {
		return provider.ClassGeneral, "", nil
	}
	return provider.ClassOf(state, false), "not filed under a project (and its mailbox has local-only or too little filed mail)", nil
}

// class is the message's class: inbound by its own filing (plus O2), outbound
// by the most restrictive of its thread's inbound messages (the drafts rule:
// outbound never gets a capture decision).
func (j *mailClassJudge) class(m mailAttachMsg) (provider.Class, string, error) {
	if m.direction != "outbound" {
		return j.inboundClass(m.id, m.account)
	}
	if m.threadID == nil {
		return provider.ClassRestricted, "outbound mail with no thread to judge it by", nil
	}
	rows, err := j.pool.Query(j.ctx, `
		SELECT m.id, r.source_account_id FROM normalized_messages m
		  JOIN raw_source_items r ON r.id = m.raw_source_item_id
		 WHERE m.thread_id = $1 AND m.direction = 'inbound'`, *m.threadID)
	if err != nil {
		return provider.ClassRestricted, "", fmt.Errorf("thread inbound messages: %w", err)
	}
	type inbound struct{ id, account int64 }
	var ins []inbound
	for rows.Next() {
		var in inbound
		if err := rows.Scan(&in.id, &in.account); err != nil {
			rows.Close()
			return provider.ClassRestricted, "", fmt.Errorf("scan thread inbound: %w", err)
		}
		ins = append(ins, in)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return provider.ClassRestricted, "", fmt.Errorf("iterate thread inbound: %w", err)
	}
	if len(ins) == 0 {
		return provider.ClassRestricted, "outbound mail with no inbound message on its thread to judge it by", nil
	}
	classes := make([]provider.Class, 0, len(ins))
	for _, in := range ins {
		c, _, err := j.inboundClass(in.id, in.account)
		if err != nil {
			return provider.ClassRestricted, "", err
		}
		classes = append(classes, c)
	}
	if c := provider.MostRestrictive(classes...); c != provider.ClassGeneral {
		return c, "outbound mail on a thread that holds private mail", nil
	}
	return provider.ClassGeneral, "", nil
}

func privateMailError(m mailAttachMsg, reason string) error {
	return fmt.Errorf("message %s is private mail (%s): its attachments are never shown to a hosted model", m.messageID, reason)
}

// ---- mail_list_attachments ---------------------------------------------------

type mailAttachListed struct {
	MessageID         string              `json:"message_id"`
	RawSourceItemID   int64               `json:"raw_source_item_id"`
	ThreadID          *int64              `json:"thread_id"`
	ThreadKey         string              `json:"thread_key"`
	Subject           string              `json:"subject"`
	Sender            string              `json:"sender"`
	SentAt            string              `json:"sent_at"`
	Direction         string              `json:"direction"`
	Source            string              `json:"source"`
	Truncated         bool                `json:"truncated"`
	Attachments       []google.Attachment `json:"attachments"`
	UnavailableReason string              `json:"unavailable_reason"`
}

func listedFor(m mailAttachMsg) (mailAttachListed, error) {
	var env struct {
		Source string `json:"source"`
	}
	_ = json.Unmarshal(m.rawJSON, &env)
	atts, info, err := google.ListAttachments(m.rawJSON)
	if err != nil {
		return mailAttachListed{}, fmt.Errorf("list attachments of %s: %w", m.messageID, err)
	}
	if atts == nil {
		atts = []google.Attachment{}
	}
	return mailAttachListed{
		MessageID: m.messageID, RawSourceItemID: m.raw, ThreadID: m.threadID, ThreadKey: m.threadKey,
		Subject: m.subject, Sender: m.sender, SentAt: m.sentAt, Direction: m.direction, Source: env.Source,
		Truncated: info.Truncated, Attachments: atts, UnavailableReason: info.UnavailableReason,
	}, nil
}

func mailListAttachments(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	a, err := parseMailListAttachments(args)
	if err != nil {
		return nil, err
	}
	judge := newMailClassJudge(ctx, pool)

	// Explicit message: an error when private (criterion 15).
	if a.RawSourceItemID != 0 || strings.TrimSpace(a.MessageID) != "" {
		m, err := loadOneMailAttachMsg(ctx, pool, a.RawSourceItemID, a.MessageID)
		if err != nil {
			return nil, err
		}
		c, reason, err := judge.class(m)
		if err != nil {
			return nil, err
		}
		if c != provider.ClassGeneral {
			return nil, privateMailError(m, reason)
		}
		l, err := listedFor(m)
		if err != nil {
			return nil, err
		}
		return marshalResult(map[string]any{"messages": []mailAttachListed{l}, "withheld_private": 0, "truncated": false})
	}

	// Thread form: every shareable member, restricted ones counted, not shown.
	if a.ThreadID != 0 || strings.TrimSpace(a.ThreadKey) != "" {
		rows, err := pool.Query(ctx, mailAttachMsgSelect+`
		   AND (($1::bigint IS NOT NULL AND m.thread_id = $1) OR ($1 IS NULL AND t.thread_key = $2))
		 ORDER BY m.sent_at ASC NULLS LAST, m.id ASC LIMIT $3`,
			nullableID(a.ThreadID), strings.TrimSpace(a.ThreadKey), mailThreadMaxMessages)
		if err != nil {
			return nil, fmt.Errorf("load thread messages: %w", err)
		}
		msgs, err := collectMailAttachMsgs(rows)
		if err != nil {
			return nil, err
		}
		out := []mailAttachListed{}
		withheld := 0
		for _, m := range msgs {
			l, err := listedFor(m)
			if err != nil {
				return nil, err
			}
			c, _, err := judge.class(m)
			if err != nil {
				return nil, err
			}
			if c != provider.ClassGeneral {
				if len(l.Attachments) > 0 {
					withheld++
				}
				continue
			}
			out = append(out, l)
		}
		return marshalResult(map[string]any{"messages": out, "withheld_private": withheld, "truncated": false})
	}

	// Finder: headers only, newest first, messages with at least one listed part.
	limit := mailAttachFinderDefaultLimit
	if a.Limit != nil && *a.Limit > 0 {
		limit = *a.Limit
	}
	if limit > mailAttachFinderMaxLimit {
		limit = mailAttachFinderMaxLimit
	}
	// Headers only here; each candidate's raw row is loaded in the loop, so one
	// call never holds more than one stored message at a time.
	rows, err := pool.Query(ctx, mailAttachHeaderSelect+`
	   AND ($1 = '' OR m.sender ILIKE '%'||$1||'%')
	   AND ($2 = '' OR m.subject ILIKE '%'||$2||'%')
	   AND ($3 = '' OR m.sent_at >= $3::timestamptz)
	   AND ($4 = '' OR m.sent_at <= $4::timestamptz)
	 ORDER BY m.sent_at DESC NULLS LAST, m.id DESC LIMIT $5`,
		likeEscape(strings.TrimSpace(a.From)), likeEscape(strings.TrimSpace(a.Subject)),
		strings.TrimSpace(a.Since), strings.TrimSpace(a.Until), mailAttachFinderScanCap)
	if err != nil {
		return nil, fmt.Errorf("find mail: %w", err)
	}
	msgs, err := collectMailAttachMsgs(rows)
	if err != nil {
		return nil, err
	}
	out := []mailAttachListed{}
	withheld, truncated := 0, false
	budget := mailAttachFinderByteBudget
	for _, m := range msgs {
		if err := pool.QueryRow(ctx, `SELECT raw_json FROM raw_source_items WHERE id = $1`, m.raw).Scan(&m.rawJSON); err != nil {
			return nil, fmt.Errorf("load raw item %d: %w", m.raw, err)
		}
		if budget -= len(m.rawJSON); budget < 0 {
			truncated = true // out of budget: more may match; narrow the search
			break
		}
		l, err := listedFor(m)
		if err != nil {
			return nil, err
		}
		if len(l.Attachments) == 0 {
			continue // criterion 3: only messages with at least one listed part
		}
		c, _, err := judge.class(m)
		if err != nil {
			return nil, err
		}
		if c != provider.ClassGeneral {
			withheld++
			continue
		}
		if len(out) == limit {
			truncated = true // limit+1: a further qualifying hit exists
			break
		}
		out = append(out, l)
	}
	if len(msgs) == mailAttachFinderScanCap {
		truncated = true // the scan cap cut the candidates: older matches may exist
	}
	return marshalResult(map[string]any{"messages": out, "withheld_private": withheld, "truncated": truncated})
}

func collectMailAttachMsgs(rows pgx.Rows) ([]mailAttachMsg, error) {
	defer rows.Close()
	var out []mailAttachMsg
	for rows.Next() {
		m, err := scanMailAttachMsg(rows)
		if err != nil {
			return nil, fmt.Errorf("scan mail message: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate mail messages: %w", err)
	}
	return out, nil
}

// ---- mail_read_attachment ------------------------------------------------------

func mailReadAttachment(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	a, err := parseMailReadAttachment(args)
	if err != nil {
		return nil, err
	}
	m, err := loadOneMailAttachMsg(ctx, pool, a.RawSourceItemID, a.MessageID)
	if err != nil {
		return nil, err
	}
	c, reason, err := newMailClassJudge(ctx, pool).class(m)
	if err != nil {
		return nil, err
	}
	if c != provider.ClassGeneral {
		return nil, privateMailError(m, reason)
	}
	sel := google.AttachmentSelector{Filename: a.Filename, PartID: a.PartID}
	if a.Index != nil {
		sel.Index = *a.Index
	}
	att, data, err := google.ReadAttachment(m.rawJSON, sel)
	if err != nil {
		return nil, err
	}
	base := map[string]any{
		"message_id": m.messageID, "index": att.Index, "part_id": att.PartID, "filename": att.Filename,
		"content_type": att.ContentType, "size_bytes": len(data),
	}
	text, isText := attachmentText(att.ContentType, att.Charset, data)
	if a.ToFile || !isText {
		path, err := writeAttachmentFile(m.raw, att.Index, att.Filename, data)
		if err != nil {
			return nil, fmt.Errorf("save attachment: %w", err)
		}
		sum := sha256.Sum256(data)
		base["kind"] = "file"
		base["path"] = path
		base["sha256"] = hex.EncodeToString(sum[:])
		base["hint"] = "Open it with Claude Code's Read tool (it renders PDFs and images). The file is removed after 7 days."
		return marshalResult(base)
	}
	page, err := pageAttachmentText(text, a.Offset)
	if err != nil {
		return nil, err
	}
	base["kind"] = "text"
	base["text"] = page.Text
	base["offset"] = page.Offset
	base["returned_bytes"] = page.ReturnedBytes
	base["total_bytes"] = page.TotalBytes
	base["truncated"] = page.Truncated
	base["next_offset"] = page.NextOffset
	return marshalResult(base)
}

// ---- text vs file (criterion 6) ------------------------------------------------

var utf8BOM = []byte("\xef\xbb\xbf")

// attachmentText decides from the CONTENT whether a part is text: no NUL in the
// first 8 KiB, valid UTF-8 after a BOM, and net/http sniffing says text/*. A
// declared text/* part whose bytes are not UTF-8 (a charset Go cannot read) is
// repaired as latin-1 — toValidUTF8's rule.
func attachmentText(contentType, charset string, data []byte) (string, bool) {
	window := data
	if len(window) > mailAttachSniffWindow {
		window = window[:mailAttachSniffWindow]
	}
	if bytes.IndexByte(window, 0) >= 0 {
		return "", false
	}
	body := bytes.TrimPrefix(data, utf8BOM)
	if !strings.HasPrefix(http.DetectContentType(body), "text/") {
		return "", false
	}
	if utf8.Valid(body) {
		return string(body), true
	}
	if !strings.HasPrefix(strings.ToLower(contentType), "text/") {
		return "", false
	}
	return latin1Repair(body), true
}

func latin1Repair(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b) + len(b)/4)
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			sb.WriteRune(rune(b[i]))
			i++
			continue
		}
		sb.Write(b[i : i+size])
		i += size
	}
	return sb.String()
}

// ---- the inline cap (criterion 7) ---------------------------------------------

type attachmentPage struct {
	Text          string
	Offset        int
	ReturnedBytes int
	TotalBytes    int
	Truncated     bool
	NextOffset    int
}

func pageAttachmentText(text string, offset int) (attachmentPage, error) {
	total := len(text)
	if offset < 0 || offset > total {
		return attachmentPage{}, fmt.Errorf("offset %d is outside the attachment's %d bytes", offset, total)
	}
	if offset < total && !utf8.RuneStart(text[offset]) {
		return attachmentPage{}, fmt.Errorf("offset %d is inside a character; use a next_offset the tool returned", offset)
	}
	rest := text[offset:]
	if len(rest) <= mailAttachmentTextCap {
		return attachmentPage{Text: rest, Offset: offset, ReturnedBytes: len(rest), TotalBytes: total}, nil
	}
	end := offset + mailAttachmentTextCap
	for end > offset && !utf8.RuneStart(text[end]) {
		end-- // never half a rune (capBody's rule)
	}
	line := fmt.Sprintf("[attachment truncated: showing bytes %d–%d of %d; call mail_read_attachment again with offset=%d, or to_file=true]",
		offset, end, total, end)
	return attachmentPage{
		Text: text[offset:end] + "\n" + line, Offset: offset, ReturnedBytes: end - offset,
		TotalBytes: total, Truncated: true, NextOffset: end,
	}, nil
}

// ---- the file writer (criteria 8-10) ------------------------------------------

// writeAttachmentFile saves data to <os.UserCacheDir()>/switchboard/attachments/
// <rawID>/<index>-<sanitized name>, owner-only, through an os.Root on the base
// so a symlink planted inside it can never carry a write outside. The name is
// computed here from a sanitized filename; a caller never supplies a path.
func writeAttachmentFile(rawID int64, index int, filename string, data []byte) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate the cache directory: %w", err)
	}
	base := filepath.Join(cache, "switchboard", "attachments")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", base, err)
	}
	_ = os.Chmod(base, 0o700)
	sweepAttachmentCache(base, time.Now())

	root, err := os.OpenRoot(base)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", base, err)
	}
	defer root.Close()

	dir := strconv.FormatInt(rawID, 10)
	if err := root.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("create attachment directory: %w", err)
	}
	st, err := root.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("stat attachment directory: %w", err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("attachment directory %s is not a plain directory; refusing to write through it", dir)
	}
	name := fmt.Sprintf("%d-%s", index, sanitizeAttachmentName(filename))
	rel := filepath.Join(dir, name)
	// Replace, never follow: whatever sits at the name (a file, or a planted
	// symlink) is removed first, and the new file is created exclusively.
	if err := root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("replace %s: %w", rel, err)
	}
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", rel, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return "", fmt.Errorf("write %s: %w", rel, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close %s: %w", rel, err)
	}
	return filepath.Join(base, rel), nil
}

// sweepAttachmentCache removes entries under base older than the TTL:
// best-effort, errors ignored, never following a symlink out of the base.
func sweepAttachmentCache(base string, now time.Time) {
	dirs, err := os.ReadDir(base)
	if err != nil {
		return
	}
	cutoff := now.Add(-mailAttachmentFileTTL)
	for _, d := range dirs {
		p := filepath.Join(base, d.Name())
		info, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if !info.IsDir() { // a stray file or symlink directly under the base
			if info.ModTime().Before(cutoff) {
				_ = os.Remove(p)
			}
			continue
		}
		files, err := os.ReadDir(p)
		if err != nil {
			continue
		}
		left := 0
		for _, f := range files {
			fp := filepath.Join(p, f.Name())
			fi, err := os.Lstat(fp)
			if err != nil {
				left++
				continue
			}
			if fi.ModTime().Before(cutoff) && !fi.IsDir() {
				if os.Remove(fp) == nil {
					continue
				}
			}
			left++
		}
		if left == 0 && info.ModTime().Before(cutoff) {
			_ = os.Remove(p)
		}
	}
}

// sanitizeAttachmentName keeps only the last path element, maps anything
// outside [A-Za-z0-9._-] to '_', strips leading dots, and caps the result at 80
// bytes with the extension kept. An empty result is "part".
func sanitizeAttachmentName(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	var sb strings.Builder
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	s := strings.TrimLeft(sb.String(), ".")
	if len(s) > 80 {
		ext := filepath.Ext(s)
		if len(ext) > 16 {
			ext = ""
		}
		s = s[:80-len(ext)] + ext
	}
	if s == "" {
		return "part"
	}
	return s
}
