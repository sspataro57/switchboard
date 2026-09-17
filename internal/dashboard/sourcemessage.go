package dashboard

// The task detail page's source-message section (SWT-65).
//
// A mail-derived task's body is the promoter's extract card: a pointer, by
// design (invariant 1 — the mail lives in raw_source_items / normalized_messages
// and the task points at it). This section follows the pointer for the one
// reader who should not have to: Salvador, on his own dashboard.
//
// PRIVACY, and say it here so nobody "fixes" it later: the SWT-21 locality gate
// that governs the MCP mail tools (mailClassJudge, the withheld_private
// counting) is a boundary on where text may TRAVEL — it keeps client mail away
// from a HOSTED MODEL. It is not a rule about who may read. This page renders
// his own mailboxes to his own eyes, and what keeps anyone else out is NETWORK
// REACH: the dashboard has no Ingress and is reached by port-forward. Do NOT
// read s.auth.Require as the barrier — with OIDC_ISSUER unset, auth.Routes
// mounts GET /dev/login and setSession hands a session to any user= value
// (auth.go:63-72). Anyone exposing this service must configure OIDC first, and
// that is true with or without this section.
// Applying the gate here would blank out `personal`
// and every local_only project — precisely the tasks that prompted the request.
// The absence of a locality check on this page is the DECISION, not an oversight.

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sspataro57/switchboard/internal/textmatch"
	"github.com/sspataro57/switchboard/internal/tools"
)

// sourceBodyCap bounds the source body the page renders, in characters.
//
// Deliberately NOT tools.MailThreadBodyCap, and do not "unify" them: that cap
// bounds what a MODEL reads (a context window), this one bounds what one human
// scrolls (a page). Same shape as SWT-64's two mail caps. 256 KiB is a safety
// stop, not an editorial choice — the largest body that resolves for any open
// task today is 33,465 characters, so in practice nothing is cut, and when
// something is the page says so.
const sourceBodyCap = 262144

// summaryPrefix is how much of a collapsed sibling's body the summary line
// carries, in runes.
const summaryPrefix = 120

// sourceMessage is the resolved message plus its conversation, ready to render.
type sourceMessage struct {
	Heading   string
	Channel   string
	Direction string
	Sender    string
	Subject   string
	SentAt    string
	Body      string
	// BodyNote is set only when the stored body was longer than sourceBodyCap.
	BodyNote string

	Attachments []sourceAttachment
	// AttachNote carries the capture's own truncation reason plus the repair.
	AttachNote string

	Thread []sourceThreadMessage
	// ThreadMore is the TRUE number of other messages on the thread, which may
	// exceed len(Thread) when the cap bites.
	ThreadMore int
	ThreadNote string
}

// sourceThreadMessage is one other message on the resolved message's thread.
type sourceThreadMessage struct {
	Summary  string
	Body     string
	BodyNote string
}

// sourceAttachment is one manifest entry. Names and sizes only: no content, no
// download link, no byte is served from this page (D6).
type sourceAttachment struct {
	Filename    string
	ContentType string
	Size        string
	Stored      string
}

// sourceMessageHeading names the section. Pure, and channel-aware in GO — the
// template never branches on the channel (the SWT-52 discipline: the light is
// Go, not template).
//
// The viaThread wording is not cosmetic. Branch 3 picks the thread's latest
// inbound message, which the database never claimed raised the task, so the
// heading must not overclaim.
func sourceMessageHeading(channel string, viaThread bool) string {
	var noun string
	switch channel {
	case "gmail":
		noun = "email"
	case "slack":
		noun = "Slack message"
	case "upwork":
		noun = "Upwork message"
	case "jira":
		noun = "Jira comment"
	default:
		noun = "message"
	}
	if viaThread {
		return "Latest " + noun + " on the source thread"
	}
	// The noun carries its own capitalisation: "Slack"/"Upwork"/"Jira" are proper
	// nouns, "email" and "message" are not.
	return "Source " + noun
}

// resolveSourceSQL is the whole precedence in ONE statement (criterion 22).
//
// Branch 1 (classify_promotions, action task|review) is the most precise claim
// in the database: the row exists BECAUSE that message made that task. Branch 2
// (capture_decisions, action task) is capture's equivalent. Both action filters
// are load-bearing — `attached` and `task_log` rows also carry task_id and name
// a LATER message that joined an existing task. Branch 3 is the thread's latest
// inbound, which is a statement about a conversation, not about causation.
//
// The order fragment is BOUND from tools.LatestInboundOrder rather than retyped:
// latestInboundMessage and this subquery must pick the same message by
// construction (D8, the tools.TaskQueueOrder precedent).
var resolveSourceSQL = fmt.Sprintf(`
	WITH cand AS (
	  SELECT 1 AS pri, (SELECT cp.normalized_message_id FROM classify_promotions cp
	                     WHERE cp.task_id = $1 AND cp.action IN ('task','review')
	                     ORDER BY cp.id LIMIT 1) AS mid
	  UNION ALL
	  SELECT 2, (SELECT cd.message_id FROM capture_decisions cd
	              WHERE cd.task_id = $1 AND cd.action = 'task'
	              ORDER BY cd.id LIMIT 1)
	  UNION ALL
	  SELECT 3, (SELECT nm.id FROM normalized_messages nm
	              WHERE nm.thread_id = $2 AND nm.direction = 'inbound'
	              ORDER BY %s LIMIT 1)
	)
	SELECT c.pri, nm.id, COALESCE(nm.thread_id, 0), COALESCE(nm.channel,''),
	       nm.direction, COALESCE(nm.sender,''), COALESCE(nm.subject,''),
	       COALESCE(nm.sent_at::text,''), left(COALESCE(nm.body_text,''), $3),
	       length(COALESCE(nm.body_text,'')), COALESCE(nm.raw_source_item_id, 0)
	  FROM cand c JOIN normalized_messages nm ON nm.id = c.mid
	 WHERE c.mid IS NOT NULL
	 ORDER BY c.pri
	 LIMIT 1`, tools.LatestInboundOrder)

// threadSQL reads the OTHER messages on the resolved message's own thread, in
// mailReadThread's order and at its caps, plus the true sibling count in the
// same statement (criterion 22 budgets one statement here).
//
// It reads each body ONCE, capped. The summary prefix is taken from that same
// capped text rather than from a second full-body column: 8,192 characters is
// ~68x the 120 runes a summary needs, and the second column would have put a
// 50-message thread's entire text on the wire (~1.6 MB for a thread of 33 KB
// messages) to produce one line each. The degenerate case is a body whose first
// 8 KiB collapses to fewer than 120 runes — over 98% whitespace — and then the
// summary is SHORTER, never wrong.
const threadSQL = `
	SELECT nm.direction, COALESCE(nm.sender,''), COALESCE(nm.sent_at::text,''),
	       left(COALESCE(nm.body_text,''), $3), length(COALESCE(nm.body_text,'')),
	       count(*) OVER ()
	  FROM normalized_messages nm
	 WHERE nm.thread_id = $1 AND nm.id <> $2
	 ORDER BY nm.sent_at ASC NULLS LAST, nm.id ASC
	 LIMIT $4`

// loadSourceMessage resolves the message a task came from and its conversation.
//
// It returns (nil, nil) when nothing resolves — the MAJORITY case (33 of the 51
// open tasks on 2026-09-17 are hand-made or plan-derived). The page then renders
// exactly as it did before this ticket: no heading, no empty block, no marker.
// That is criterion 15, and it carries the same weight as the render itself.
//
// It never reads tasks.surfaced_by_message_id: that is migration 0030's REVIVE
// marker — the message that resurfaced a closed task — which answers a different
// question than "what is this task about".
func (s *Server) loadSourceMessage(ctx context.Context, taskID int64, sourceThreadID *int64) (*sourceMessage, error) {
	var (
		pri        int
		id, thread int64
		rawItemID  int64
		storedLen  int
		m          sourceMessage
	)
	err := s.pool.QueryRow(ctx, resolveSourceSQL, taskID, sourceThreadID, sourceBodyCap).
		Scan(&pri, &id, &thread, &m.Channel, &m.Direction, &m.Sender, &m.Subject,
			&m.SentAt, &m.Body, &storedLen, &rawItemID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No row is the common answer, not a failure: the task has no
			// message. 33 of the 51 open tasks look exactly like this.
			return nil, nil
		}
		// Anything else means the read itself is broken — a renamed column, a
		// dead pool. The caller still swallows it, but absent-because-none and
		// absent-because-impossible must not be the same value here: a schema
		// drift would otherwise empty the section on EVERY task page and look
		// precisely like the majority render.
		return nil, fmt.Errorf("resolve source message for task %d: %w", taskID, err)
	}
	m.Heading = sourceMessageHeading(m.Channel, pri == 3)
	// storedLen comes from SQL length(), and left() cut by CHARACTERS — so the
	// comparison has to be in characters too. len(m.Body) is BYTES, and for a
	// Cyrillic or CJK body it exceeds the character count, which would silence
	// this marker on exactly the messages that were cut.
	if shown := utf8.RuneCountInString(m.Body); storedLen > shown {
		m.BodyNote = fmt.Sprintf("Showing the first %d of %d characters; the rest is in the mailbox.",
			shown, storedLen)
	}

	if thread != 0 {
		s.loadSourceThread(ctx, &m, thread, id)
	}
	// Gmail only: google.ListAttachments walks the IMAP rfc822_b64 envelope, and
	// a slack / upwork / jira raw_json has no such envelope (D6).
	if m.Channel == "gmail" && rawItemID != 0 {
		s.loadSourceAttachments(ctx, &m, rawItemID)
	}
	return &m, nil
}

// loadSourceThread fills the collapsed block. A failure here leaves the section
// standing with its source message: the conversation is context, and losing it
// must not cost him the message he came for.
func (s *Server) loadSourceThread(ctx context.Context, m *sourceMessage, threadID, sourceID int64) {
	rows, err := s.pool.Query(ctx, threadSQL, threadID, sourceID,
		tools.MailThreadBodyCap, tools.MailThreadMaxMessages)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var (
			direction, sender, sentAt, body string
			storedLen, total                int
		)
		if err := rows.Scan(&direction, &sender, &sentAt, &body, &storedLen, &total); err != nil {
			m.dropThread()
			return
		}
		m.ThreadMore = total
		t := sourceThreadMessage{
			// NormalizedPrefix is the ONE truncation spelling (SWT-16): a
			// left(body_text, 120) in SQL would cut mid-rune and keep the
			// newlines that make a summary line unreadable.
			Summary: fmt.Sprintf("%s · %s · %s — %s", direction, sender, sentAt,
				textmatch.NormalizedPrefix(body, summaryPrefix)),
			Body: body,
		}
		if shown := utf8.RuneCountInString(body); storedLen > shown {
			t.BodyNote = fmt.Sprintf("Showing the first %d of %d characters.", shown, storedLen)
		}
		m.Thread = append(m.Thread, t)
	}
	// finding 3: a half-read thread under a "(7 more)" heading is a partial
	// result presented as a complete one. Drop it; the source message stands.
	if rows.Err() != nil {
		m.dropThread()
		return
	}
	if m.ThreadMore > len(m.Thread) {
		m.ThreadNote = fmt.Sprintf("Showing the oldest %d; the rest are in the mailbox.", len(m.Thread))
	}
}

// dropThread removes a conversation that was only partly read, so the page
// never shows a count it did not render.
func (m *sourceMessage) dropThread() {
	m.Thread, m.ThreadMore, m.ThreadNote = nil, 0, ""
}

// loadSourceAttachments fills the manifest: names, types, sizes, whether the
// bytes are stored. NEVER content — the page serves no byte and adds no route.
// Reading an attachment stays mail_read_attachment over MCP.
func (s *Server) loadSourceAttachments(ctx context.Context, m *sourceMessage, rawItemID int64) {
	atts, info, err := tools.MailAttachmentsForRawItem(ctx, s.pool, rawItemID)
	if err != nil {
		return
	}
	for _, a := range atts {
		size := fmt.Sprintf("%d bytes", a.SizeBytes)
		if a.SizeIsEncoded {
			size += " (encoded)"
		}
		stored := "yes"
		if !a.Available {
			// Verbatim: the capture's own reason explains itself, and inventing
			// a friendlier string would hide which cap actually bit.
			stored = a.UnavailableReason
		}
		m.Attachments = append(m.Attachments, sourceAttachment{
			Filename: a.Filename, ContentType: a.ContentType, Size: size, Stored: stored,
		})
	}
	if reason := info.UnavailableReason; info.Truncated || reason != "" {
		if reason == "" {
			reason = "the capture was truncated"
		}
		// Name the repair at the point of frustration: since SWT-64 these rows
		// can be recovered rather than merely explained.
		m.AttachNote = reason + " — recover the bytes with: opsctl mail refetch --raw-id " +
			fmt.Sprint(rawItemID) + " --limit 1"
	}
}
