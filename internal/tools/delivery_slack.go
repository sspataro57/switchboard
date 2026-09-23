package tools

// The slack_reply auto tier (SWT-77, slack-auto-tier): send_slack_reply
// drafts, approves and sends a Slack message in ONE executor call, from the
// caller's own text. It composes the two existing halves — draftDelivery for
// the row (canonical target_ref, ScrubAIAttribution, closed-task refusal) and
// sendSlackReply for the bridge — so slackSender.Send keeps exactly one call
// site (invariant 3).
//
// WHAT AN INJECTED send_slack_reply CALL CAN AND CANNOT DO (the
// book_calendar_block register): it can post words of its choosing into any
// conversation of a workspace a human send-enabled (send_enabled on the
// workspace's synthetic account, re-checked at send time), at most the
// channel's hourly limit, every call audited, stoppable with
// set_sending_frozen. It cannot send a row somebody else drafted — it takes
// text, never a delivery_id (D11) — and it cannot post the same words to the
// same conversation twice while the first attempt is unresolved (D10). The
// WHEN rule (only on Salvador's go-ahead) lives in the tool description and
// the MCP Instructions; nothing in code can enforce it.

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

// slackAutoAdmissionLock serializes send_slack_reply calls (session lock, see
// sendSlackReplyTool). A member of the 0x5157_00NN family; no test enforces
// uniqueness across the family, so grep for 0x5157 before adding a key.
const slackAutoAdmissionLock = int64(0x5157_0077)

// slackAdmissionPoll is how long a waiting send_slack_reply sleeps between
// tries for the admission lock; slackAdmissionMaxWait bounds the whole wait.
// The holder keeps the lock across the bridge call, and the bridge client has
// no timeout of its own, so without the bound one hung click would stall every
// later call indefinitely. A refusal after it names the reason.
const (
	slackAdmissionPoll    = 25 * time.Millisecond
	slackAdmissionMaxWait = 60 * time.Second
)

// acquireSlackAdmission takes slackAutoAdmissionLock as a session lock and
// returns its release. It TRIES rather than blocks, and hands the connection
// back between tries: a waiter parked in pg_advisory_lock would hold a pooled
// connection, and enough of them starve the holder — which needs the pool for
// its draft, approve and send — into a deadlock. Waits until ctx ends or
// slackAdmissionMaxWait passes.
// Released on a fresh context; if the unlock fails the connection is closed,
// which drops the lock with it.
func acquireSlackAdmission(ctx context.Context, pool *pgxpool.Pool) (func(), error) {
	deadline := time.Now().Add(slackAdmissionMaxWait)
	for {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			return nil, fmt.Errorf("acquire connection for slack admission: %w", err)
		}
		var got bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, slackAutoAdmissionLock).Scan(&got); err != nil {
			conn.Release()
			return nil, fmt.Errorf("lock slack admission: %w", err)
		}
		if got {
			return func() {
				uctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, slackAutoAdmissionLock); err != nil {
					_ = conn.Conn().Close(uctx)
				}
				conn.Release()
			}, nil
		}
		conn.Release()
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("another send_slack_reply has been in flight for over %s (a slow or hung Slack bridge); "+
				"nothing was drafted — check /deliveries and the mini, then try again", slackAdmissionMaxWait)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for another send_slack_reply to finish: %w", ctx.Err())
		case <-time.After(slackAdmissionPoll):
		}
	}
}

type sendSlackReplyArgs struct {
	TaskID    int64  `json:"task_id"`
	TargetRef string `json:"target_ref"`
	Text      string `json:"text"`
}

func validateSendSlackReply(args []byte) error {
	var a sendSlackReplyArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID <= 0 {
		return errors.New("missing task_id")
	}
	if a.TargetRef == "" {
		return errors.New("missing target_ref")
	}
	if _, err := slackweb.ParseTargetURL(a.TargetRef); err != nil {
		return fmt.Errorf("invalid target_ref: %w", err)
	}
	if strings.TrimSpace(a.Text) == "" {
		return errors.New("missing text")
	}
	// Validate the value that LANDS (SWT-61): words that scrub to nothing
	// would post an empty line.
	if strings.TrimSpace(google.ScrubAIAttribution(a.Text)) == "" {
		return errors.New("text is empty after the AI-attribution scrub; nothing would reach Slack")
	}
	return nil
}

// sendSlackReplyTool: admission lock → sender, workspace, kill-switch and
// hourly-limit gates → duplicate guard → draft → approve → send. The refusals
// ahead of the draft write nothing; a refusal from the draft (closed task)
// writes nothing either. The approve half is
// bookCalendarBlock's shape: approval_source='switchboard' in the same
// statement as the transition (sendSlackReply requires it; D3), plus an
// approvals row naming executor.ActorFrom(ctx), so which session posted is
// recorded.
func sendSlackReplyTool(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a sendSlackReplyArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	target, err := slackweb.ParseTargetURL(a.TargetRef)
	if err != nil {
		return nil, fmt.Errorf("invalid slack_reply target_ref: %w", err)
	}
	canonical := target.CanonicalURL()

	// Admission is serialized (SWT-77, codex review): a session lock on its
	// own connection, held from the gates through sendSlackReply's return, so
	// concurrent calls run one at a time and the duplicate guard and the rate
	// check see every earlier call's row already 'sending'. It spans the
	// bridge call too; the mini's one browser serializes clicks anyway, and a
	// queued send answers 202 at once.
	//
	// The holder keeps one pooled connection for the lock and needs at least
	// one more for its own work, so a one-connection pool can never finish a
	// call: refused by name. And the whole call gets a deadline — admission
	// wait, dispatch bound, and a minute for the database work — because an
	// MCP call carries none: an exhausted pool must end in an error that
	// releases the lock, never in a holder parked forever (codex review).
	if max := pool.Config().MaxConns; max < 2 {
		return nil, fmt.Errorf("send_slack_reply needs a database pool of at least 2 connections (this one has %d): "+
			"one holds the admission lock while the other drafts and sends", max)
	}
	ctx, cancel := context.WithTimeout(ctx, slackAdmissionMaxWait+slackSendDispatchTimeout()+time.Minute)
	defer cancel()
	unlock, err := acquireSlackAdmission(ctx, pool)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Every refusal the send half can make on its own is checked here first
	// (sendSlackReply re-checks each at send time), so a refused call leaves
	// no approved row behind: this verb takes text, so a retry mints a NEW
	// row, and an orphaned approved one would sit on /deliveries one Send
	// click away from posting the same words twice.
	if slackSender == nil {
		return nil, fmt.Errorf("no Slack send adapter wired (SetSlackSender): set SLACK_WEB_BRIDGE_URL for this process")
	}
	if err := refuseSlackWorkspaceNotSendable(ctx, pool, canonical); err != nil {
		return nil, err
	}
	if err := refuseSendingFrozen(ctx, pool, false); err != nil {
		return nil, err
	}
	if err := refuseSlackOverHourlyLimit(ctx, pool); err != nil {
		return nil, err
	}

	// D10: a call that timed out after the click leaves the caller free to
	// call again with the same words — a double post. Refuse while an attempt
	// with the same canonical target and the same scrubbed body is unresolved:
	// 'sending' (in flight, ambiguous or queued) or 'approved' (one Send away —
	// a row this verb approved and then lost to a crash or a freeze, or the
	// same words awaiting the human two-step; codex review).
	// Exact under the admission lock against other send_slack_reply calls; a
	// concurrent human two-step on the same words is caught by status.
	var dupID int64
	var dupStatus string
	err = pool.QueryRow(ctx,
		`SELECT id, status FROM deliveries
		 WHERE channel='slack_reply' AND status IN ('approved','sending') AND target_ref=$1 AND body=$2
		 ORDER BY id LIMIT 1`,
		canonical, google.ScrubAIAttribution(a.Text)).Scan(&dupID, &dupStatus)
	if err == nil && dupStatus == "approved" {
		return nil, fmt.Errorf("delivery %d already carries these words to this conversation and is approved but not sent; "+
			"send or reject it on /deliveries instead of sending again", dupID)
	}
	if err == nil {
		return nil, fmt.Errorf("delivery %d already carries these words to this conversation and is still unresolved (sending); "+
			"check Slack, then record it with mark_delivery_sent or mark_delivery_failed instead of sending again", dupID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("check for an unresolved duplicate: %w", err)
	}

	draftArgs, err := json.Marshal(draftDeliveryArgs{
		TaskID: a.TaskID, Channel: "slack_reply", Body: a.Text, TargetRef: a.TargetRef,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal draft args: %w", err)
	}
	out, err := draftDelivery(ctx, pool, draftArgs)
	if err != nil {
		return nil, err
	}
	var drafted struct {
		DeliveryID int64 `json:"delivery_id"`
	}
	if err := json.Unmarshal(out, &drafted); err != nil || drafted.DeliveryID == 0 {
		return nil, fmt.Errorf("draft returned no delivery id: %s", out)
	}
	id := drafted.DeliveryID

	err = inTx(ctx, pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE deliveries SET status='approved', approval_source='switchboard', updated_at=now()
			 WHERE id=$1 AND status='drafted'`, id)
		if err != nil {
			return fmt.Errorf("approve delivery %d: %w", id, err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("delivery %d left drafted before it could be approved", id)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO approvals (subject_type, subject_id, status, decided_by, decided_at)
			 VALUES ('delivery', $1, 'approved', $2, now())`,
			id, executor.ActorFrom(ctx)); err != nil {
			return fmt.Errorf("insert approval: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sendSlackReply(ctx, pool, id)
}
