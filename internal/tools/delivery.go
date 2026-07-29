package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/connector/slackweb"
	"github.com/sspataro57/switchboard/internal/executor"
)

// The SWT-8 delivery lifecycle tools (invariant 4: nothing external without a
// delivery row; sent_external_id set once, never resend while present).
// draft_delivery is agent-facing; the rest are spine-facing.

// GmailSender is the send_delivery handler's adapter seam — cmd/* wire the
// real google.GmailSender; tests inject a fake. Package-level because tool
// handlers close over the pool only.
type GmailSender interface {
	Send(ctx context.Context, fromUserID string, rawMIME []byte, threadID string) (string, error)
}

var gmailSender GmailSender

// SetGmailSender wires the send adapter (the ONLY caller is the send_delivery
// handler — invariant 4's single gate).
func SetGmailSender(s GmailSender) { gmailSender = s }

// JiraSender is the jira_comment send seam (SWT-9). Jira assigns the comment
// id post-call, so the idempotency shape differs from gmail: `sending` commits
// pre-network, the id lands post-call, and the connector's post-hoc prefix
// matcher closes the ambiguous-failure window.
type JiraSender interface {
	Send(ctx context.Context, siteHost, issueKey, body string) (commentID string, err error)
}

var jiraSender JiraSender

// SetJiraSender wires the jira comment adapter.
func SetJiraSender(s JiraSender) { jiraSender = s }

// SlackDrafter is the assisted Slack delivery seam. Its implementation may
// populate a browser composer, but it must never send the message.
type SlackDrafter interface {
	Draft(ctx context.Context, targetURL, text string) error
}

var slackDrafter SlackDrafter

// SetSlackDrafter wires the local Slack Web bridge used by prefill_delivery.
func SetSlackDrafter(d SlackDrafter) { slackDrafter = d }

// SlackSender is the promoted Slack delivery seam (SWT-12). Unlike SlackDrafter
// it DOES send: the connector clicks Send through its bridge after switchboard
// approval, because the assisted tier required remote-desktopping into the Mac
// mini to press the button.
//
// A browser click reserves no external id, so Send returns nothing to record.
// The delivery's sent_external_id stays NULL and the next connector export
// stamps it by matching the body prefix — see slackweb.PGSink.confirmDelivery.
type SlackSender interface {
	Send(ctx context.Context, targetURL, text string) error
}

var slackSender SlackSender

// SetSlackSender wires the Slack send adapter used by send_delivery.
func SetSlackSender(s SlackSender) { slackSender = s }

// ---- draft_delivery (agent-facing) ------------------------------------------

type draftDeliveryArgs struct {
	TaskID    int64  `json:"task_id"`
	Channel   string `json:"channel"`
	Body      string `json:"body"`
	Subject   string `json:"subject,omitempty"`
	ThreadID  *int64 `json:"thread_id,omitempty"`
	TargetRef string `json:"target_ref,omitempty"`
}

func validateDraftDelivery(args []byte) error {
	var a draftDeliveryArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID == 0 {
		return errors.New("missing task_id")
	}
	switch a.Channel {
	case "gmail", "upwork_chat", "jira_comment", "slack_reply":
	default:
		return fmt.Errorf("channel %q: must be gmail, upwork_chat, jira_comment, or slack_reply", a.Channel)
	}
	if a.Body == "" {
		return errors.New("missing body")
	}
	if a.Channel == "gmail" && a.ThreadID == nil {
		return errors.New("gmail drafts require thread_id (From is resolved from the thread)")
	}
	if (a.Channel == "upwork_chat" || a.Channel == "jira_comment" || a.Channel == "slack_reply") && a.TargetRef == "" {
		return errors.New("upwork_chat/jira_comment/slack_reply drafts require target_ref")
	}
	if a.Channel == "slack_reply" {
		if _, err := slackweb.ParseTargetURL(a.TargetRef); err != nil {
			return fmt.Errorf("invalid slack_reply target_ref: %w", err)
		}
	}
	return nil
}

// ---- prefill_delivery (assisted Slack tier) -----------------------------------

func prefillDelivery(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a deliveryIDOnlyArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	if slackDrafter == nil {
		return nil, fmt.Errorf("no Slack draft adapter wired (SetSlackDrafter)")
	}

	if err := inTx(ctx, pool, func(tx pgx.Tx) error {
		var status, channel, targetRef, body string
		if err := tx.QueryRow(ctx,
			`SELECT status, channel, COALESCE(target_ref,''), body
			 FROM deliveries WHERE id=$1 FOR UPDATE`, a.DeliveryID).
			Scan(&status, &channel, &targetRef, &body); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("delivery %d not found", a.DeliveryID)
			}
			return fmt.Errorf("load Slack delivery %d: %w", a.DeliveryID, err)
		}
		if channel != "slack_reply" {
			return fmt.Errorf("prefill_delivery only supports slack_reply; delivery %d is %s", a.DeliveryID, channel)
		}
		if status != "approved" {
			return fmt.Errorf("delivery %d is %s; only approved Slack replies can be prefilled", a.DeliveryID, status)
		}
		if targetRef == "" || body == "" {
			return fmt.Errorf("delivery %d is missing its Slack target or body", a.DeliveryID)
		}
		// Keep the row lock while the local composer operation runs so a
		// concurrent mark_delivery_sent cannot race this approved-only check.
		if err := slackDrafter.Draft(ctx, targetRef, body); err != nil {
			return fmt.Errorf("prefill Slack delivery %d: %w", a.DeliveryID, err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{
		"delivery_id": a.DeliveryID,
		"drafted":     true,
		"sent":        false,
	})
}

func draftDelivery(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a draftDeliveryArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	var fromAccountID *int64
	if a.Channel == "jira_comment" {
		// From is resolved server-side: the target_ref's site_host must match a
		// provider='jira' account's domain_default — never caller-chosen.
		siteHost, _, err := splitJiraTargetRef(a.TargetRef)
		if err != nil {
			return nil, err
		}
		var acctID int64
		err = pool.QueryRow(ctx,
			`SELECT id FROM source_accounts WHERE provider='jira'
			 AND domain_default LIKE '%'||$1||'%' ORDER BY id LIMIT 1`, siteHost).Scan(&acctID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("no jira account for site %s", siteHost)
		}
		if err != nil {
			return nil, fmt.Errorf("resolve jira account: %w", err)
		}
		fromAccountID = &acctID
	}
	if a.Channel == "slack_reply" {
		// Store the canonical spelling, not the caller's. Loop closure matches
		// target_ref by exact string, so an accepted-but-noncanonical variant
		// (trailing slash) would leave the delivery unconfirmable forever.
		target, err := slackweb.ParseTargetURL(a.TargetRef)
		if err != nil {
			return nil, fmt.Errorf("invalid slack_reply target_ref: %w", err)
		}
		a.TargetRef = target.CanonicalURL()
	}
	if a.Channel == "gmail" {
		// From is resolved server-side from the thread's mailbox segment
		// (gmail:{account_email}:{threadId}) — the caller cannot choose it.
		var threadKey *string
		err := pool.QueryRow(ctx,
			`SELECT thread_key FROM normalized_threads WHERE id=$1`, *a.ThreadID).Scan(&threadKey)
		if errors.Is(err, pgx.ErrNoRows) || threadKey == nil {
			return nil, fmt.Errorf("thread %d not found", *a.ThreadID)
		}
		if err != nil {
			return nil, fmt.Errorf("resolve thread %d: %w", *a.ThreadID, err)
		}
		email, _, err := splitGmailThreadKey(*threadKey)
		if err != nil {
			return nil, err
		}
		var acctID int64
		err = pool.QueryRow(ctx,
			`SELECT id FROM source_accounts WHERE provider='google' AND lower(account_email)=lower($1)`,
			email).Scan(&acctID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("no google account for mailbox %s", email)
		}
		if err != nil {
			return nil, fmt.Errorf("resolve mailbox account: %w", err)
		}
		fromAccountID = &acctID
	}

	var deliveryID int64
	err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, subject, status,
		                         from_account_id, thread_id, created_by)
		 VALUES ($1, $2, NULLIF($3,''), $4, NULLIF($5,''), 'drafted', $6, $7, $8)
		 RETURNING id`,
		a.TaskID, a.Channel, a.TargetRef,
		google.ScrubAIAttribution(a.Body), google.ScrubAIAttribution(a.Subject),
		fromAccountID, a.ThreadID, executor.ActorFrom(ctx)).Scan(&deliveryID)
	if err != nil {
		return nil, fmt.Errorf("insert delivery: %w", err)
	}
	return marshalResult(map[string]any{"delivery_id": deliveryID})
}

func splitGmailThreadKey(key string) (email, gmailThreadID string, err error) {
	parts := strings.SplitN(key, ":", 3)
	if len(parts) != 3 || parts[0] != "gmail" {
		return "", "", fmt.Errorf("thread key %q is not a gmail thread", key)
	}
	return parts[1], parts[2], nil
}

// ---- update_delivery ---------------------------------------------------------

type updateDeliveryArgs struct {
	DeliveryID int64   `json:"delivery_id"`
	Subject    *string `json:"subject,omitempty"`
	Body       *string `json:"body,omitempty"`
}

func validateUpdateDelivery(args []byte) error {
	var a updateDeliveryArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.DeliveryID == 0 {
		return errors.New("missing delivery_id")
	}
	if a.Subject == nil && a.Body == nil {
		return errors.New("nothing to update (subject or body required)")
	}
	return nil
}

func updateDelivery(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a updateDeliveryArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	subject, body := "", ""
	if a.Subject != nil {
		subject = google.ScrubAIAttribution(*a.Subject)
	}
	if a.Body != nil {
		body = google.ScrubAIAttribution(*a.Body)
	}
	tag, err := pool.Exec(ctx,
		`UPDATE deliveries SET
		   subject = CASE WHEN $2 THEN NULLIF($3,'') ELSE subject END,
		   body    = CASE WHEN $4 THEN $5 ELSE body END,
		   updated_at = now()
		 WHERE id=$1 AND status='drafted'`,
		a.DeliveryID, a.Subject != nil, subject, a.Body != nil, body)
	if err != nil {
		return nil, fmt.Errorf("update delivery %d: %w", a.DeliveryID, err)
	}
	if tag.RowsAffected() == 0 {
		return nil, fmt.Errorf("delivery %d is not drafted (editing an approved draft would bypass approval)", a.DeliveryID)
	}
	return marshalResult(map[string]any{"delivery_id": a.DeliveryID})
}

// ---- approve_delivery ----------------------------------------------------------

type deliveryIDOnlyArgs struct {
	DeliveryID int64 `json:"delivery_id"`
	// LeafGated is only read by mark_delivery_sent, and only for a drafted
	// slack_reply row: the caller states this message was already sent through
	// the Slack connector's own approval token, so there is no switchboard
	// approval to look for. Ignored everywhere else.
	LeafGated bool `json:"leaf_gated,omitempty"`
}

func validateDeliveryIDOnly(args []byte) error {
	var a deliveryIDOnlyArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.DeliveryID == 0 {
		return errors.New("missing delivery_id")
	}
	return nil
}

func approveDelivery(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a deliveryIDOnlyArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		var status string
		var extID *string
		if err := tx.QueryRow(ctx,
			`SELECT status, sent_external_id FROM deliveries WHERE id=$1 FOR UPDATE`,
			a.DeliveryID).Scan(&status, &extID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("delivery %d not found", a.DeliveryID)
			}
			return fmt.Errorf("lock delivery %d: %w", a.DeliveryID, err)
		}
		switch {
		case status == "drafted":
		case status == "failed" && extID == nil:
		default:
			return fmt.Errorf("delivery %d is %s; only drafted (or failed without a sent id) can be approved", a.DeliveryID, status)
		}
		// approval_source records WHICH authority let this row out (SWT-12).
		// Written in the same statement as the status transition: a crash must
		// never leave a row whose gate is unknown, which is the one thing the
		// column exists to prevent.
		if _, err := tx.Exec(ctx,
			`UPDATE deliveries SET status='approved', approval_source='switchboard', updated_at=now()
			 WHERE id=$1`, a.DeliveryID); err != nil {
			return fmt.Errorf("approve delivery %d: %w", a.DeliveryID, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO approvals (subject_type, subject_id, status, decided_by, decided_at)
			 VALUES ('delivery', $1, 'approved', $2, now())`,
			a.DeliveryID, executor.ActorFrom(ctx)); err != nil {
			return fmt.Errorf("insert approval: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"delivery_id": a.DeliveryID, "status": "approved"})
}

// ---- send_delivery -------------------------------------------------------------

func sendDelivery(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a deliveryIDOnlyArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	// Channel routing: jira_comment has its own send shape (id post-call).
	var channel string
	if err := pool.QueryRow(ctx, `SELECT channel FROM deliveries WHERE id=$1`, a.DeliveryID).Scan(&channel); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("delivery %d not found", a.DeliveryID)
		}
		return nil, fmt.Errorf("resolve delivery channel: %w", err)
	}
	if channel == "jira_comment" {
		return sendJiraComment(ctx, pool, a.DeliveryID)
	}
	if channel == "slack_reply" {
		return sendSlackReply(ctx, pool, a.DeliveryID)
	}
	if gmailSender == nil {
		return nil, fmt.Errorf("no gmail send adapter wired (SetGmailSender)")
	}

	// Phase 1 (tx): lock, verify, resolve headers, commit sending +
	// sent_external_id BEFORE any network call (invariant 4 idempotency).
	var (
		d struct {
			taskID    int64
			channel   string
			body      string
			subject   *string
			threadID  *int64
			fromEmail string
		}
		msg     google.OutboundMessage
		gThread string
		msgID   string
	)
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		var status string
		var extID *string
		var fromAcct *int64
		var sendEnabled *bool
		err := tx.QueryRow(ctx,
			`SELECT d.task_id, d.channel, d.body, d.subject, d.thread_id, d.status,
			        d.sent_external_id, d.from_account_id, a.send_enabled, COALESCE(a.account_email,'')
			 FROM deliveries d LEFT JOIN source_accounts a ON a.id = d.from_account_id
			 WHERE d.id=$1 FOR UPDATE OF d`,
			a.DeliveryID).Scan(&d.taskID, &d.channel, &d.body, &d.subject, &d.threadID,
			&status, &extID, &fromAcct, &sendEnabled, &d.fromEmail)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("delivery %d not found", a.DeliveryID)
		}
		if err != nil {
			return fmt.Errorf("lock delivery %d: %w", a.DeliveryID, err)
		}
		if extID != nil {
			return fmt.Errorf("delivery %d already carries sent_external_id; never resend (invariant 4)", a.DeliveryID)
		}
		if status != "approved" {
			return fmt.Errorf("delivery %d is %s; only approved deliveries send", a.DeliveryID, status)
		}
		if d.channel != "gmail" {
			return fmt.Errorf("channel %s has no direct send path", d.channel)
		}
		if fromAcct == nil || d.fromEmail == "" {
			return fmt.Errorf("delivery %d has no from account", a.DeliveryID)
		}
		if sendEnabled == nil || !*sendEnabled {
			return fmt.Errorf("account %s is not send-enabled", d.fromEmail)
		}
		if d.threadID == nil {
			return fmt.Errorf("delivery %d has no thread", a.DeliveryID)
		}

		// Resolve threading material.
		var threadKey string
		if err := tx.QueryRow(ctx,
			`SELECT thread_key FROM normalized_threads WHERE id=$1`, *d.threadID).Scan(&threadKey); err != nil {
			return fmt.Errorf("resolve thread %d: %w", *d.threadID, err)
		}
		_, gt, err := splitGmailThreadKey(threadKey)
		if err != nil {
			return err
		}
		gThread = gt

		var to, inReplyTo string
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(sender,''), COALESCE(external_message_id,'')
			 FROM normalized_messages
			 WHERE thread_id=$1 AND direction='inbound'
			 ORDER BY sent_at DESC, id DESC LIMIT 1`, *d.threadID).Scan(&to, &inReplyTo); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("thread %d has no inbound message to reply to", *d.threadID)
			}
			return fmt.Errorf("resolve reply target: %w", err)
		}

		var refs []string
		rows, err := tx.Query(ctx,
			`SELECT external_message_id FROM normalized_messages
			 WHERE thread_id=$1 AND external_message_id LIKE '<%'
			 ORDER BY sent_at, id`, *d.threadID)
		if err != nil {
			return fmt.Errorf("resolve references: %w", err)
		}
		for rows.Next() {
			var mid string
			if err := rows.Scan(&mid); err != nil {
				rows.Close()
				return fmt.Errorf("scan reference: %w", err)
			}
			refs = append(refs, mid)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate references: %w", err)
		}

		domain := d.fromEmail[strings.LastIndexByte(d.fromEmail, '@')+1:]
		msgID = fmt.Sprintf("<sb-%d-%d@%s>", a.DeliveryID, time.Now().UnixNano(), domain)

		subject := ""
		if d.subject != nil {
			subject = *d.subject
		}
		msg = google.OutboundMessage{
			From: d.fromEmail, To: to, Subject: subject, Body: d.body,
			MessageID: msgID, InReplyTo: inReplyTo, References: refs, Date: time.Now(),
		}

		if _, err := tx.Exec(ctx,
			`UPDATE deliveries SET status='sending', sent_external_id=$2, updated_at=now()
			 WHERE id=$1`, a.DeliveryID, msgID); err != nil {
			return fmt.Errorf("mark sending: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Phase 2: the network call, then finalize sent | failed.
	raw, err := google.BuildOutboundMIME(msg)
	if err != nil {
		_, _ = pool.Exec(ctx, `UPDATE deliveries SET status='failed', error=$2, updated_at=now() WHERE id=$1`,
			a.DeliveryID, err.Error())
		return nil, fmt.Errorf("build outbound message: %w", err)
	}
	if _, err := gmailSender.Send(ctx, d.fromEmail, raw, gThread); err != nil {
		var rejected *google.SendRejectedError
		if errors.As(err, &rejected) {
			// Definite rejection: clear the reserved Message-ID so
			// approve_delivery's failed->approved retry path is reachable.
			_, _ = pool.Exec(ctx,
				`UPDATE deliveries SET status='failed', sent_external_id=NULL, error=$2, updated_at=now() WHERE id=$1`,
				a.DeliveryID, err.Error())
		} else {
			// Ambiguous transport error: the send MAY have gone through —
			// keep the id; never risk a double send (invariant 4).
			_, _ = pool.Exec(ctx,
				`UPDATE deliveries SET status='failed', error=$2, updated_at=now() WHERE id=$1`,
				a.DeliveryID, err.Error())
		}
		return nil, fmt.Errorf("gmail send: %w", err)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE deliveries SET status='sent', sent_at=now(), error=NULL, updated_at=now() WHERE id=$1`,
		a.DeliveryID); err != nil {
		return nil, fmt.Errorf("finalize sent: %w", err)
	}
	if _, err := insertTaskEvent(ctx, pool, d.taskID, "delivery_sent",
		map[string]any{"delivery_id": a.DeliveryID, "channel": d.channel, "sent_external_id": msgID}); err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"delivery_id": a.DeliveryID, "status": "sent", "sent_external_id": msgID})
}

// ---- mark_delivery_sent (assisted tier) -----------------------------------------

func markDeliverySent(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a deliveryIDOnlyArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	var taskID int64
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		var status, channel string
		var approvalSource *string
		if err := tx.QueryRow(ctx,
			`SELECT status, channel, task_id, approval_source FROM deliveries WHERE id=$1 FOR UPDATE`,
			a.DeliveryID).Scan(&status, &channel, &taskID, &approvalSource); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("delivery %d not found", a.DeliveryID)
			}
			return fmt.Errorf("lock delivery %d: %w", a.DeliveryID, err)
		}
		if channel != "upwork_chat" && channel != "slack_reply" {
			return fmt.Errorf("mark_delivery_sent is the assisted tier's verb (upwork_chat/slack_reply); delivery %d is %s", a.DeliveryID, channel)
		}
		// The MCP surface can only RESOLVE an attempt switchboard itself made.
		//
		// Recording is safe from a prompt-injectable session only insofar as it
		// cannot invent a delivery from nothing. Resolving a 'sending' slack_reply
		// row qualifies: switchboard dispatched that click, so the worst an
		// injected call can do is claim it landed when it did not. Every other
		// transition does invent one — an 'approved' row, or a 'drafted' one via
		// leaf_gated, becomes 'sent' with no send having occurred, and R8 then
		// closes the work task as delivered. Those stay on the dashboard and
		// opsctl, which an interactive session can still reach through Bash.
		//
		// Checked here rather than in policy because policy decides on
		// (tool, actor, snapshot) and never sees a row's status; executor.ViaMCP
		// keeps the transport-prefix knowledge in one place.
		if executor.ViaMCP(ctx) && !(channel == "slack_reply" && status == "sending") {
			return fmt.Errorf("over MCP, mark_delivery_sent only resolves a slack_reply delivery already "+
				"in 'sending'; delivery %d is %s/%s — record it via the dashboard or `opsctl call`",
				a.DeliveryID, channel, status)
		}
		switch {
		case status == "approved":
		case status == "sending" && channel == "slack_reply":
			// SWT-12: a human looked in Slack and the message is there, so this
			// resolves the click-may-have-landed window that no automatic path
			// is allowed to retry.
		case status == "drafted" && channel == "slack_reply" && leafGated(approvalSource, a.LeafGated):
			// SWT-12 manual path: the connector's own token gated this send and
			// the message is already in the channel. There is no switchboard
			// approval to record, so drafted -> sent skips one, rather than
			// writing an approvals row for a gate that never ran.
			//
			// The caller may assert the gate here (leaf_gated) because nothing
			// else can: draft_delivery is agent-facing and unchanged, so a row
			// starts with approval_source NULL. Stamping it from an agent-facing
			// tool would let a worker pre-mark a row that later skips approval;
			// this tool is human-only, and "I sent this through the connector"
			// is the same kind of assertion it already exists to record.
			if approvalSource == nil {
				if _, err := tx.Exec(ctx,
					`UPDATE deliveries SET approval_source='leaf_token' WHERE id=$1`, a.DeliveryID); err != nil {
					return fmt.Errorf("stamp approval_source: %w", err)
				}
				leaf := "leaf_token"
				approvalSource = &leaf
			}
		default:
			return fmt.Errorf("delivery %d is %s; only approved deliveries can be marked sent "+
				"(slack_reply also accepts sending, or drafted when approval_source='leaf_token')",
				a.DeliveryID, status)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE deliveries SET status='sent', sent_at=now(), updated_at=now() WHERE id=$1`, a.DeliveryID); err != nil {
			return fmt.Errorf("mark sent: %w", err)
		}
		if _, err := insertTaskEvent(ctx, tx, taskID, "delivery_sent",
			map[string]any{"delivery_id": a.DeliveryID, "channel": channel, "manual": true}); err != nil {
			return err
		}
		// The kill switch does not gate recording (policy: only send_delivery is
		// freeze-gated), because a send made elsewhere was never switchboard's
		// to prevent. But "frozen" reads as "nothing moves", so every record
		// written during a freeze is logged rather than left to be inferred from
		// timestamps.
		var frozen *bool
		if err := tx.QueryRow(ctx,
			`SELECT (value->>'frozen')::boolean FROM ops_flags WHERE name='sending_frozen'`).Scan(&frozen); err != nil &&
			!errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read sending_frozen: %w", err)
		}
		if frozen != nil && *frozen {
			source := ""
			if approvalSource != nil {
				source = *approvalSource
			}
			if _, err := insertTaskEvent(ctx, tx, taskID, "delivery_recorded_during_freeze",
				map[string]any{"delivery_id": a.DeliveryID, "channel": channel, "approval_source": source}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"delivery_id": a.DeliveryID, "status": "sent"})
}

// ---- task_mark_delivered ---------------------------------------------------------

type markDeliveredArgs struct {
	TaskID int64  `json:"task_id"`
	Reason string `json:"reason,omitempty"`
}

func validateMarkDelivered(args []byte) error {
	var a markDeliveredArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID == 0 {
		return errors.New("missing task_id")
	}
	return nil
}

func taskMarkDelivered(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a markDeliveredArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	status := ""
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT status FROM tasks WHERE id=$1 FOR UPDATE`, a.TaskID).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("task %d not found", a.TaskID)
			}
			return fmt.Errorf("lock task %d: %w", a.TaskID, err)
		}
		switch status {
		case "delivered", "closed":
			return nil // idempotent replay (orchestrator discipline)
		case "done_locally":
		default:
			return fmt.Errorf("task %d is %s; only done_locally transitions to delivered", a.TaskID, status)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE tasks SET status='delivered', updated_at=now() WHERE id=$1`, a.TaskID); err != nil {
			return fmt.Errorf("mark delivered: %w", err)
		}
		if _, err := insertTaskEvent(ctx, tx, a.TaskID, "status_changed",
			map[string]any{"from": "done_locally", "to": "delivered", "reason": a.Reason}); err != nil {
			return err
		}
		status = "delivered"
		return nil
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"task_id": a.TaskID, "status": status})
}

// ---- set_sending_frozen (kill switch) --------------------------------------------

type frozenArgs struct {
	Frozen *bool `json:"frozen"`
}

func validateSetFrozen(args []byte) error {
	var a frozenArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.Frozen == nil {
		return errors.New("missing frozen (explicit true/false required)")
	}
	return nil
}

func setSendingFrozen(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a frozenArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	raw, err := json.Marshal(map[string]bool{"frozen": *a.Frozen})
	if err != nil {
		return nil, fmt.Errorf("marshal flag: %w", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO ops_flags (name, value) VALUES ('sending_frozen', $1)
		 ON CONFLICT (name) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, raw); err != nil {
		return nil, fmt.Errorf("upsert sending_frozen: %w", err)
	}
	return marshalResult(map[string]any{"frozen": *a.Frozen})
}

// splitJiraTargetRef parses jira:{site_host}:{issueKey}.
func splitJiraTargetRef(ref string) (siteHost, issueKey string, err error) {
	parts := strings.SplitN(ref, ":", 3)
	if len(parts) != 3 || parts[0] != "jira" || parts[1] == "" || parts[2] == "" {
		return "", "", fmt.Errorf("target_ref %q is not jira:{site_host}:{issueKey}", ref)
	}
	return parts[1], parts[2], nil
}

// sendJiraComment is the jira branch of send_delivery: sending committed
// pre-network; Jira assigns the id post-call; definite failures leave the row
// failed with sent_external_id NULL (the poller's post-hoc matcher recovers
// the ambiguous window).
func sendJiraComment(ctx context.Context, pool *pgxpool.Pool, deliveryID int64) ([]byte, error) {
	if jiraSender == nil {
		return nil, fmt.Errorf("no jira send adapter wired (SetJiraSender)")
	}

	var taskID int64
	var body, targetRef string
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		var status string
		var extID *string
		var target *string
		var fromAcct *int64
		var sendEnabled *bool
		var fromEmail string
		err := tx.QueryRow(ctx,
			`SELECT d.task_id, d.body, d.status, d.sent_external_id, d.target_ref,
			        d.from_account_id, a.send_enabled, COALESCE(a.account_email,'')
			 FROM deliveries d LEFT JOIN source_accounts a ON a.id = d.from_account_id
			 WHERE d.id=$1 FOR UPDATE OF d`, deliveryID).
			Scan(&taskID, &body, &status, &extID, &target, &fromAcct, &sendEnabled, &fromEmail)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("delivery %d not found", deliveryID)
		}
		if err != nil {
			return fmt.Errorf("lock delivery %d: %w", deliveryID, err)
		}
		if extID != nil {
			return fmt.Errorf("delivery %d already carries sent_external_id; never resend (invariant 4)", deliveryID)
		}
		if status != "approved" {
			return fmt.Errorf("delivery %d is %s; only approved deliveries send", deliveryID, status)
		}
		if fromAcct == nil || fromEmail == "" {
			return fmt.Errorf("delivery %d has no from account", deliveryID)
		}
		if sendEnabled == nil || !*sendEnabled {
			return fmt.Errorf("account %s is not send-enabled", fromEmail)
		}
		if target == nil {
			return fmt.Errorf("delivery %d has no target_ref", deliveryID)
		}
		targetRef = *target
		if _, err := tx.Exec(ctx,
			`UPDATE deliveries SET status='sending', updated_at=now() WHERE id=$1`, deliveryID); err != nil {
			return fmt.Errorf("mark sending: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	siteHost, issueKey, err := splitJiraTargetRef(targetRef)
	if err != nil {
		_, _ = pool.Exec(ctx, `UPDATE deliveries SET status='failed', error=$2, updated_at=now() WHERE id=$1`,
			deliveryID, err.Error())
		return nil, err
	}

	commentID, sendErr := jiraSender.Send(ctx, siteHost, issueKey, google.ScrubAIAttribution(body))
	if sendErr != nil {
		// id unknown — leave sent_external_id NULL; the poller's post-hoc
		// matcher recovers if the comment actually landed.
		_, _ = pool.Exec(ctx, `UPDATE deliveries SET status='failed', error=$2, updated_at=now() WHERE id=$1`,
			deliveryID, sendErr.Error())
		return nil, fmt.Errorf("jira send: %w", sendErr)
	}

	extID := "jira:" + siteHost + ":comment:" + commentID
	if _, err := pool.Exec(ctx,
		`UPDATE deliveries SET status='sent', sent_external_id=$2, sent_at=now(), error=NULL, updated_at=now()
		 WHERE id=$1`, deliveryID, extID); err != nil {
		return nil, fmt.Errorf("finalize sent: %w", err)
	}
	if _, err := insertTaskEvent(ctx, pool, taskID, "delivery_sent",
		map[string]any{"delivery_id": deliveryID, "channel": "jira_comment", "sent_external_id": extID}); err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"delivery_id": deliveryID, "status": "sent", "sent_external_id": extID})
}

// ---- slack_reply send (SWT-12) --------------------------------------------------

// sendSlackReply clicks Send through the Slack Web connector's bridge.
//
// Unlike gmail there is no reservable external id: a browser click exposes no
// message id, so sent_external_id stays NULL and the connector's next export
// stamps it by matching the body prefix. That shapes the whole failure model —
// 'sending' is committed BEFORE the click, and an ambiguous failure LEAVES the
// row in 'sending' rather than marking it failed, because a retry of a click
// that may have landed is a double-post into a client channel.
func sendSlackReply(ctx context.Context, pool *pgxpool.Pool, deliveryID int64) ([]byte, error) {
	if slackSender == nil {
		return nil, fmt.Errorf("no Slack send adapter wired (SetSlackSender)")
	}

	var taskID int64
	var body, targetRef string
	var attemptedAt time.Time
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		var status string
		var extID, target *string
		var approvalSource *string
		err := tx.QueryRow(ctx,
			`SELECT task_id, body, status, sent_external_id, target_ref, approval_source
			 FROM deliveries WHERE id=$1 FOR UPDATE`, deliveryID).
			Scan(&taskID, &body, &status, &extID, &target, &approvalSource)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("delivery %d not found", deliveryID)
		}
		if err != nil {
			return fmt.Errorf("lock delivery %d: %w", deliveryID, err)
		}
		if extID != nil {
			return fmt.Errorf("delivery %d already carries sent_external_id; never resend (invariant 4)", deliveryID)
		}
		if status != "approved" {
			return fmt.Errorf("delivery %d is %s; only approved deliveries send", deliveryID, status)
		}
		// The automated path REQUIRES switchboard to be the authority of record.
		// A NULL here would mean the row reached 'approved' without
		// approve_delivery, which no code path does — migration 0012 backfills
		// historical rows from the approvals table.
		if approvalSource == nil || *approvalSource != "switchboard" {
			got := "NULL"
			if approvalSource != nil {
				got = *approvalSource
			}
			return fmt.Errorf("delivery %d has approval_source=%s; send_delivery requires 'switchboard' "+
				"(a leaf-gated row is recorded with mark_delivery_sent, never sent again from here)", deliveryID, got)
		}
		if target == nil || *target == "" {
			return fmt.Errorf("delivery %d has no target_ref", deliveryID)
		}
		targetRef = *target

		// The workspace's synthetic account carries the per-workspace go-live
		// gate, mirroring gmail's send_enabled convention. EnsureAccount inserts
		// it false and never updates it, so a new workspace is off by default.
		parsed, err := slackweb.ParseTargetURL(targetRef)
		if err != nil {
			return fmt.Errorf("invalid slack_reply target_ref: %w", err)
		}
		accountEmail := strings.ToLower(parsed.WorkspaceID) + "@slack-web.local"
		var sendEnabled bool
		err = tx.QueryRow(ctx,
			`SELECT send_enabled FROM source_accounts WHERE provider=$1 AND account_email=$2`,
			slackweb.Provider, accountEmail).Scan(&sendEnabled)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("Slack workspace %s has no ingested account; run the connector first", parsed.WorkspaceID)
		}
		if err != nil {
			return fmt.Errorf("resolve Slack workspace %s account: %w", parsed.WorkspaceID, err)
		}
		if !sendEnabled {
			return fmt.Errorf("Slack workspace %s is not send-enabled", parsed.WorkspaceID)
		}

		// send_attempted_at marks this attempt as IN FLIGHT: send_settled_at stays
		// NULL until the bridge call returns. Without that distinction 'sending'
		// would mean both "executing now" and "returned ambiguously", and
		// mark_delivery_failed could reopen a live call for a second send.
		//
		// The timestamp is read back and every phase-2 write is fenced on it, so a
		// late-returning attempt cannot overwrite the outcome of a newer one.
		if err := tx.QueryRow(ctx,
			`UPDATE deliveries
			    SET status='sending', send_attempted_at=now(), send_settled_at=NULL, updated_at=now()
			  WHERE id=$1
			 RETURNING send_attempted_at`, deliveryID).Scan(&attemptedAt); err != nil {
			return fmt.Errorf("mark sending: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sendErr := slackSender.Send(ctx, targetRef, google.ScrubAIAttribution(body))

	// Constructed AFTER the call, never before: WithTimeout fixes an absolute
	// deadline at creation, so a window opened up-front would already be spent by
	// the time a real browser click returned — and the post-call write is exactly
	// what a slow send needs. WithoutCancel because the caller's deadline blowing
	// is the correlated event that most needs a marker and a diagnostic recorded.
	settleCtx, cancelSettle := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancelSettle()

	// Fence: only the attempt that wrote this timestamp may record its outcome,
	// and only while the row is still the one it dispatched. An attempt that
	// returns after a human resolved it — or after a newer attempt started —
	// affects nothing.
	const fenceArg3 = ` AND status='sending' AND send_attempted_at=$3`
	const fenceArg2 = ` AND status='sending' AND send_attempted_at=$2`

	if sendErr != nil {
		var rejected *slackweb.SendRejectedError
		if errors.As(sendErr, &rejected) {
			// Definite: the click never happened, so reopen failed->approved.
			if _, err := pool.Exec(settleCtx,
				`UPDATE deliveries SET status='failed', send_settled_at=now(), error=$2, updated_at=now()
				 WHERE id=$1`+fenceArg3, deliveryID, sendErr.Error(), attemptedAt); err != nil {
				// Report both: the send is known-not-sent, but the row still says
				// 'sending' and a human has to resolve it.
				return nil, fmt.Errorf("slack send rejected (%v) AND recording the failure failed (%v); "+
					"delivery %d is stuck in sending", sendErr, err, deliveryID)
			}
			return nil, fmt.Errorf("slack send: %w", sendErr)
		}
		// Ambiguous: the click MAY have landed. Leave the row in 'sending' — it
		// is not re-approvable and nothing retries it. Only the export matcher
		// or a human (mark_delivery_sent / mark_delivery_failed) resolves it.
		// send_settled_at closes the in-flight window so a human MAY resolve it.
		if _, err := pool.Exec(settleCtx,
			`UPDATE deliveries SET send_settled_at=now(), error=$2, updated_at=now() WHERE id=$1`+fenceArg3,
			deliveryID, sendErr.Error(), attemptedAt); err != nil {
			return nil, fmt.Errorf("slack send outcome unknown (%v) AND recording it failed (%v); "+
				"delivery %d is stuck in sending with no diagnostic", sendErr, err, deliveryID)
		}
		return nil, fmt.Errorf("slack send (outcome unknown, delivery %d left sending): %w", deliveryID, sendErr)
	}

	tag, err := pool.Exec(settleCtx,
		`UPDATE deliveries SET status='sent', sent_at=now(), send_settled_at=now(), error=NULL, updated_at=now()
		 WHERE id=$1`+fenceArg2, deliveryID, attemptedAt)
	if err != nil {
		return nil, fmt.Errorf("finalize sent: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// The click landed, but this attempt no longer owns the row: something
		// else already resolved it. Say so rather than reporting a clean send.
		return nil, fmt.Errorf("slack send landed but delivery %d was resolved by another actor first; "+
			"verify the channel before acting on this row", deliveryID)
	}
	if _, err := insertTaskEvent(settleCtx, pool, taskID, "delivery_sent",
		map[string]any{"delivery_id": deliveryID, "channel": "slack_reply"}); err != nil {
		return nil, err
	}
	// sent_external_id is deliberately absent: the export stamps it.
	return marshalResult(map[string]any{"delivery_id": deliveryID, "status": "sent"})
}

// ---- mark_delivery_failed (SWT-12) -----------------------------------------------

// sendAttemptLease bounds how long an unsettled send attempt blocks manual
// resolution. Longer than any sender context in the repo (opsctl 30s, the
// connector 15m) so a live attempt is never overridden, but finite so a sender
// that crashed mid-click does not wedge the row forever.
const sendAttemptLease = 15 * time.Minute

// markDeliveryFailed resolves the other side of the click-may-have-landed
// window: a human looked in Slack and the message is verifiably NOT there.
//
// Without this verb a stuck 'sending' row is unrecoverable except by raw SQL,
// which would be a side door around the executor (invariant 3). It is human-only
// but deliberately not send-shaped — it moves a row AWAY from the world, so
// neither the kill switch nor the rate limit has any claim on it.
func markDeliveryFailed(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a deliveryIDOnlyArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	var taskID int64
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		var status, channel string
		var extID *string
		var confirmedAt, attemptedAt, settledAt *time.Time
		if err := tx.QueryRow(ctx,
			`SELECT status, channel, task_id, sent_external_id, confirmed_at,
			        send_attempted_at, send_settled_at
			 FROM deliveries WHERE id=$1 FOR UPDATE`,
			a.DeliveryID).Scan(&status, &channel, &taskID, &extID, &confirmedAt,
			&attemptedAt, &settledAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("delivery %d not found", a.DeliveryID)
			}
			return fmt.Errorf("lock delivery %d: %w", a.DeliveryID, err)
		}
		if channel != "slack_reply" {
			return fmt.Errorf("mark_delivery_failed only supports slack_reply; delivery %d is %s", a.DeliveryID, channel)
		}
		if status != "sending" {
			return fmt.Errorf("delivery %d is %s; only a stuck sending row can be marked failed", a.DeliveryID, status)
		}
		// Both of these mean the message IS in Slack. Refuse rather than
		// contradict the evidence — invariant 4 never walks back a send.
		if extID != nil {
			return fmt.Errorf("delivery %d carries sent_external_id; it was sent", a.DeliveryID)
		}
		if confirmedAt != nil {
			return fmt.Errorf("delivery %d is confirmed; it was sent", a.DeliveryID)
		}
		// A send attempt that has NOT settled is still executing. Marking it
		// failed here would let the row be re-approved and sent a second time
		// while the first click is in progress — two client-visible posts. The
		// lease bounds that refusal: a crashed sender never writes
		// send_settled_at, so after sendAttemptLease the attempt is treated as
		// abandoned and a human may resolve it.
		if attemptedAt != nil && settledAt == nil && time.Since(*attemptedAt) < sendAttemptLease {
			return fmt.Errorf("delivery %d has a send attempt in flight since %s; wait for it to settle "+
				"(or %s from then) before resolving it by hand",
				a.DeliveryID, attemptedAt.Format(time.RFC3339), sendAttemptLease)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE deliveries SET status='failed', error=COALESCE(error,'') ||
			   CASE WHEN COALESCE(error,'') = '' THEN '' ELSE ' | ' END ||
			   'marked failed by ' || $2, updated_at=now()
			 WHERE id=$1`, a.DeliveryID, executor.ActorFrom(ctx)); err != nil {
			return fmt.Errorf("mark failed: %w", err)
		}
		_, err := insertTaskEvent(ctx, tx, taskID, "delivery_failed",
			map[string]any{"delivery_id": a.DeliveryID, "channel": channel, "manual": true})
		return err
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"delivery_id": a.DeliveryID, "status": "failed"})
}

// leafGated reports whether a drafted slack_reply row may go straight to sent:
// either the row already records the leaf as its gate, or the (human-only)
// caller asserts it now. Anything else — including approval_source='switchboard'
// — must go through approve_delivery, so this edge never becomes a general
// bypass of approval.
func leafGated(approvalSource *string, asserted bool) bool {
	if approvalSource != nil {
		return *approvalSource == "leaf_token"
	}
	return asserted
}
