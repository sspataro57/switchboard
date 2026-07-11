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
	if a.Channel != "gmail" && a.Channel != "upwork_chat" {
		return fmt.Errorf("channel %q: must be gmail or upwork_chat", a.Channel)
	}
	if a.Body == "" {
		return errors.New("missing body")
	}
	if a.Channel == "gmail" && a.ThreadID == nil {
		return errors.New("gmail drafts require thread_id (From is resolved from the thread)")
	}
	if a.Channel == "upwork_chat" && a.TargetRef == "" {
		return errors.New("upwork_chat drafts require target_ref (the thread_key)")
	}
	return nil
}

func draftDelivery(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a draftDeliveryArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	var fromAccountID *int64
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
		if _, err := tx.Exec(ctx,
			`UPDATE deliveries SET status='approved', updated_at=now() WHERE id=$1`, a.DeliveryID); err != nil {
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
		if err := tx.QueryRow(ctx,
			`SELECT status, channel, task_id FROM deliveries WHERE id=$1 FOR UPDATE`,
			a.DeliveryID).Scan(&status, &channel, &taskID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("delivery %d not found", a.DeliveryID)
			}
			return fmt.Errorf("lock delivery %d: %w", a.DeliveryID, err)
		}
		if channel != "upwork_chat" {
			return fmt.Errorf("mark_delivery_sent is the assisted tier's verb (upwork_chat); delivery %d is %s", a.DeliveryID, channel)
		}
		if status != "approved" {
			return fmt.Errorf("delivery %d is %s; only approved deliveries can be marked sent", a.DeliveryID, status)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE deliveries SET status='sent', sent_at=now(), updated_at=now() WHERE id=$1`, a.DeliveryID); err != nil {
			return fmt.Errorf("mark sent: %w", err)
		}
		_, err := insertTaskEvent(ctx, tx, taskID, "delivery_sent",
			map[string]any{"delivery_id": a.DeliveryID, "channel": channel, "manual": true})
		return err
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
