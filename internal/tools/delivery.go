package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/connector/slackweb"
	"github.com/sspataro57/switchboard/internal/connector/upworkcrm"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
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
	// Start/End are the calendar channel's interval (SWT-28 criterion 10),
	// RFC3339. Calendar-only: they add no rule to any other channel.
	Start string `json:"start,omitempty"`
	End   string `json:"end,omitempty"`
	// ExpectTaskStatus (SWT-37, Q1 = b) is the task status the caller READ
	// before composing the draft. The handler re-checks it under the task row
	// lock and refuses on a mismatch, closing the read-then-write window. The
	// drafts worker sets "done_locally"; it can only narrow, never widen, so it
	// is harmless on the MCP surface and deliberately absent from its schema.
	ExpectTaskStatus string `json:"expect_task_status,omitempty"`
	// RequireChannel (SWT-44 review) is pinned to "gmail" by the user-scope
	// MCP profile (mcpserver.userProfilePins, after injectWorkerID, by
	// overwrite): a session in any repo drafts email replies — From resolved
	// from the thread, To shown on the dashboard before approval — and never a
	// Slack, Upwork, Jira or calendar delivery. It only narrows, so it is absent
	// from every schema, and the validator REFUSES a differing channel rather
	// than rewriting it, so the model is told the truth.
	RequireChannel string `json:"require_channel,omitempty"`
	// RequireThreadInTaskProject (SWT-44, owner decision "Same project",
	// Salvador 2026-09-12) is pinned to "true" by the user-scope MCP profile:
	// a session drafts only on a thread already filed under the task's project
	// — the task's own source_thread_id, or a thread whose LATEST INBOUND
	// message (the reply's recipient, latestInboundMessage) has a LATEST
	// capture_decisions row (any mode) naming the task's project.
	// Checked in draftDelivery's transaction, under the task lock, before the
	// insert. Same pattern as RequireChannel: only narrows, hidden from every
	// schema; the full profile and the drafts worker send none.
	RequireThreadInTaskProject string `json:"require_thread_in_task_project,omitempty"`
}

func validateDraftDelivery(args []byte) error {
	var a draftDeliveryArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID == 0 {
		return errors.New("missing task_id")
	}
	// First, so the refusal names the real reason rather than a channel rule
	// the caller was never going to be allowed to satisfy.
	if a.RequireChannel != "" && a.Channel != a.RequireChannel {
		return fmt.Errorf("channel %q is refused here: this caller drafts %s deliveries only", a.Channel, a.RequireChannel)
	}
	if a.ExpectTaskStatus != "" && !slices.Contains(taskStatuses, a.ExpectTaskStatus) {
		return fmt.Errorf("expect_task_status %q is not a task status", a.ExpectTaskStatus)
	}
	switch a.Channel {
	case "gmail", "upwork_chat", "jira_comment", "slack_reply", "calendar":
	default:
		return fmt.Errorf("channel %q: must be gmail, upwork_chat, jira_comment, slack_reply, or calendar", a.Channel)
	}
	if a.Channel == "calendar" {
		// SWT-28 criterion 10: a calendar row's interval IS its identity —
		// migration 0020's CHECK refuses the row without it, and the send path
		// books exactly [starts_at, ends_at).
		if a.TargetRef == "" {
			return errors.New("calendar drafts require target_ref (the account email of the calendar to book)")
		}
		if a.Subject == "" {
			return errors.New("calendar drafts require subject (it becomes the event summary)")
		}
		if a.Start == "" {
			return errors.New("calendar drafts require start (RFC3339)")
		}
		if a.End == "" {
			return errors.New("calendar drafts require end (RFC3339)")
		}
		s, err := time.Parse(time.RFC3339, a.Start)
		if err != nil {
			return fmt.Errorf("start: %w", err)
		}
		e, err := time.Parse(time.RFC3339, a.End)
		if err != nil {
			return fmt.Errorf("end: %w", err)
		}
		if !e.After(s) {
			return errors.New("end must be after start")
		}
		// The fat-finger guard (<= 12h; exactly 12 is legal): a typo'd end
		// DATE would blanket the calendar and make propose_slots refuse
		// everything downstream.
		if e.Sub(s) > 12*time.Hour {
			return fmt.Errorf("block is %s long; a calendar block is capped at 12 hours (a typo'd end date would blanket the calendar)", e.Sub(s))
		}
	}
	if a.Body == "" {
		return errors.New("missing body")
	}
	if a.Channel == "gmail" && a.ThreadID == nil {
		return errors.New("gmail drafts require thread_id (From is resolved from the thread)")
	}
	// The same-project pin is a check ON a thread: without one it would check
	// nothing, so a pinned call must name it.
	if a.RequireThreadInTaskProject == "true" && a.ThreadID == nil {
		return errors.New("this caller drafts only on a thread filed under the task's project: thread_id is required")
	}
	if (a.Channel == "upwork_chat" || a.Channel == "jira_comment" || a.Channel == "slack_reply") && a.TargetRef == "" {
		return errors.New("upwork_chat/jira_comment/slack_reply drafts require target_ref")
	}
	if a.Channel == "slack_reply" {
		if _, err := slackweb.ParseTargetURL(a.TargetRef); err != nil {
			return fmt.Errorf("invalid slack_reply target_ref: %w", err)
		}
	}
	if a.Channel == "upwork_chat" {
		// Parallel to slack_reply above, and overdue: this was the SWT-13
		// canonicalization landmine's fourth instance, validating only
		// non-emptiness while slack_reply parsed. SWT-19 made it urgent — the
		// matcher now compares target_ref by parsing it, so a target it cannot
		// read is PERMANENTLY unconfirmable where the pre-SWT-18 LIKE was
		// forgiving. Production has zero upwork_chat deliveries, so closing it
		// costs nothing today and cannot be closed cheaply later.
		//
		// Both key shapes are accepted: the legacy corpus is real, and the
		// matcher's tolerance of unknown rooms is what keeps an unroomed target
		// confirmable.
		if _, err := upworkcrm.ParseThreadKey(strings.TrimSpace(a.TargetRef)); err != nil {
			return fmt.Errorf("invalid upwork_chat target_ref: %w", err)
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
		// SWT-37 (Codex pass 3): prefilling puts the words in a real Slack
		// composer one click from a send, so closed work is refused here too —
		// task row first, like every approve and send path.
		if err := refuseClosedTask(ctx, tx, a.DeliveryID); err != nil {
			return err
		}
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
	var targetClientRef *string
	var startsAt, endsAt *time.Time
	if a.Channel == "calendar" {
		// SWT-28 criteria 11-12: the account is resolved SERVER-SIDE from
		// target_ref (never caller-chosen), matched case-insensitively, and
		// target_ref is stored as the account_email READ BACK from the row —
		// the database's spelling (the SWT-13 canonicalization rule). The
		// three refusal causes are distinguished by name. Requiring
		// calendar_in_availability is load-bearing: LoadBusy only reads
		// in-scope calendars, so booking onto an out-of-scope one is booking
		// blind.
		var acctID int64
		var canonical string
		var inScope, writeEnabled bool
		err := pool.QueryRow(ctx,
			`SELECT id, account_email, calendar_in_availability, calendar_write_enabled
			   FROM source_accounts
			  WHERE provider='google' AND lower(account_email)=lower($1)
			  ORDER BY id LIMIT 1`, strings.TrimSpace(a.TargetRef)).
			Scan(&acctID, &canonical, &inScope, &writeEnabled)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("no google account for calendar %q", a.TargetRef)
		}
		if err != nil {
			return nil, fmt.Errorf("resolve calendar account: %w", err)
		}
		if !inScope {
			return nil, fmt.Errorf("account %s is not in availability scope (calendar_in_availability=false); "+
				"LoadBusy only reads in-scope calendars, so booking onto it would be booking blind", canonical)
		}
		if !writeEnabled {
			return nil, fmt.Errorf("account %s is not calendar_write_enabled; the per-account go-live gate "+
				"is flipped by hand (see the calendar-availability runbook)", canonical)
		}
		fromAccountID = &acctID
		a.TargetRef = canonical
		s, _ := time.Parse(time.RFC3339, a.Start) // validated above
		e, _ := time.Parse(time.RFC3339, a.End)
		startsAt, endsAt = &s, &e
	}
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
	if a.Channel == "upwork_chat" {
		// Same reason as slack_reply above, and now load-bearing: since SWT-19
		// the matcher parses target_ref rather than pattern-matching it, so a
		// stored variant the parser rejects is unconfirmable forever with nothing
		// surfacing it but the reconciler.
		ref, err := upworkcrm.ParseThreadKey(strings.TrimSpace(a.TargetRef))
		if err != nil {
			return nil, fmt.Errorf("invalid upwork_chat target_ref: %w", err)
		}
		a.TargetRef = upworkcrm.ThreadKey(ref.ClientID, ref.RoomID, ref.Channel)
		// SWT-20: the pass-four closure is replaced by a SERVER-SIDE BINDING.
		// The target must belong to the conversation partner the task's recorded
		// source thread names, and that binding is UNCONDITIONAL — every actor,
		// drafts:gpt included (Q1 answered (a)). The one actor-keyed decision
		// below is who may pick a DIFFERENT ROOM inside the already-bound
		// client: it restricts nothing the binding has not already restricted,
		// it only decides who may exercise a choice among conversations that
		// are provably the same partner (finding 3's "explicit human choice").
		// It uses policy.HumanActor — the same predicate Decide's human gate
		// uses, so the two definitions cannot drift — and NOT executor.ViaMCP,
		// which drafts:gpt correctly passes and which is therefore useless as a
		// trust signal (the IK entry on actor prefixes; the counter-example is
		// this very worker).
		prov, found, err := store.TaskSourceThread(ctx, pool, a.TaskID)
		if err != nil {
			return nil, fmt.Errorf("resolve task %d provenance: %w", a.TaskID, err)
		}
		if !found {
			return nil, fmt.Errorf("task %d (and its parent) record no source conversation, so an "+
				"upwork_chat target cannot be bound — any supplied target_ref could name another client's "+
				"thread, which is the exposure the old closure existed for. Record which conversation "+
				"raised the task with task_set_source_thread (spine-only), then retry", a.TaskID)
		}
		provRef, err := upworkcrm.ParseThreadKey(prov.ThreadKey)
		if err != nil {
			return nil, fmt.Errorf("task %d's recorded source conversation %q is not an upwork thread; a "+
				"task raised elsewhere cannot be delivered into upwork by naming a target", a.TaskID, prov.ThreadKey)
		}

		upworkThreadID := prov.ThreadID
		if a.TargetRef != prov.ThreadKey {
			// The caller chose a target other than the recorded conversation.
			// It must (i) name an ingested thread — resolved to its id, whose
			// FK subsumes the old EXISTS probe (D5) — (ii) belong to the SAME
			// client as the provenance, and (iii) come from a human.
			var candID int64
			err := pool.QueryRow(ctx,
				`SELECT id FROM normalized_threads WHERE thread_key=$1`, a.TargetRef).Scan(&candID)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("upwork_chat target_ref %q names no ingested thread; "+
					"an unrecognized target would be confirmable by any message from that client", a.TargetRef)
			}
			if err != nil {
				return nil, fmt.Errorf("check upwork_chat target_ref: %w", err)
			}
			if ref.ClientID != provRef.ClientID {
				return nil, fmt.Errorf("target %q names a thread of client %s, but task %d's recorded "+
					"conversation belongs to client %s. The client binding is unconditional for every "+
					"actor (SWT-20 D8): nobody may draft into a different conversation partner",
					a.TargetRef, ref.ClientID, a.TaskID, provRef.ClientID)
			}
			if !policy.HumanActor(executor.ActorFrom(ctx)) {
				return nil, fmt.Errorf("target %q is a different room of the bound client than the "+
					"recorded %q; choosing among a client's rooms is an explicit human decision "+
					"(dashboard:/opsctl:/manual:), and the drafts worker's own resolution always "+
					"produces the recorded key", a.TargetRef, prov.ThreadKey)
			}
			upworkThreadID = candID
		}
		// The delivery's identity, extracted by the SAME parse that produced
		// the stored target_ref: what the shortlist selects on, and what the
		// CHECK constraint forces every upwork_chat row to carry.
		targetClientRef = &ref.ClientID
		a.ThreadID = &upworkThreadID
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

	// SWT-37 (Q1 = b, Codex review): no caller may draft for CLOSED work, and a
	// caller that read the task's status first (the drafts worker, before its
	// model call) gets a refusal if it moved on. Both checks run HERE, under the
	// task row lock closeTransition also takes: drafts.DeliverTasks' filter is a
	// read, so a hand close or "delivered" in the window would otherwise still
	// get a draft. `delivered` alone is not refused for every caller — a sibling
	// delivery (a Jira final comment after the email) is legitimate.
	var deliveryID int64
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx,
			`SELECT status FROM tasks WHERE id=$1 FOR UPDATE`, a.TaskID).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("task %d not found", a.TaskID)
			}
			return fmt.Errorf("lock task %d: %w", a.TaskID, err)
		}
		if status == "closed" {
			return fmt.Errorf("task %d is closed: switchboard never drafts a delivery for closed work", a.TaskID)
		}
		if a.ExpectTaskStatus != "" && status != a.ExpectTaskStatus {
			return fmt.Errorf("task %d is %s, not %s as the caller read it: the work moved on, so no draft",
				a.TaskID, status, a.ExpectTaskStatus)
		}
		if a.RequireThreadInTaskProject == "true" {
			if err := refuseThreadOutsideTaskProject(ctx, tx, a.TaskID, *a.ThreadID); err != nil {
				return err
			}
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO deliveries (task_id, channel, target_ref, body, subject, status,
			                         from_account_id, thread_id, target_client_ref, created_by,
			                         starts_at, ends_at)
			 VALUES ($1, $2, NULLIF($3,''), $4, NULLIF($5,''), 'drafted', $6, $7, $8, $9, $10, $11)
			 RETURNING id`,
			a.TaskID, a.Channel, a.TargetRef,
			google.ScrubAIAttribution(a.Body), google.ScrubAIAttribution(a.Subject),
			fromAccountID, a.ThreadID, targetClientRef, executor.ActorFrom(ctx),
			startsAt, endsAt).Scan(&deliveryID); err != nil {
			return fmt.Errorf("insert delivery: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"delivery_id": deliveryID})
}

// refuseThreadOutsideTaskProject is the user profile's same-project rule
// (owner decision "Same project", Salvador 2026-09-12). It follows the
// RECIPIENT: the thread is filed under the task's project when it is (a) the
// task's own source_thread_id (SWT-20 provenance), or (b) its LATEST INBOUND
// message — the very message the reply goes to, picked by
// latestInboundMessage, the helper ResolveGmailRoute uses, so the rule and
// the send cannot disagree about which message that is — has a LATEST
// capture_decisions row (`ORDER BY id DESC LIMIT 1`, any mode, the repo's
// latest-decision convention: classify/store.go, mailattach.go) naming the
// task's project. An older message filed here does not qualify a thread whose
// newest inbound mail is filed elsewhere, and outbound messages never count:
// our own send is not evidence of where the conversation belongs. Runs in
// draftDelivery's transaction, after the task row lock, before the insert.
//
// What it guarantees: the draft's thread is filed under the task's project.
// It does NOT limit which project a session drafts into — the same session
// can create_task in any project.
func refuseThreadOutsideTaskProject(ctx context.Context, tx pgx.Tx, taskID, threadID int64) error {
	var slug string
	var taskProject int64
	var sourceThread bool
	err := tx.QueryRow(ctx, `
		SELECT p.slug, t.project_id, COALESCE(t.source_thread_id = $2, false)
		  FROM tasks t JOIN projects p ON p.id = t.project_id
		 WHERE t.id = $1`, taskID, threadID).Scan(&slug, &taskProject, &sourceThread)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("task %d not found", taskID)
	}
	if err != nil {
		return fmt.Errorf("check thread %d against task %d's project: %w", threadID, taskID, err)
	}
	if sourceThread {
		return nil
	}
	refused := fmt.Errorf("thread %d is not filed under this task's project (%s): its latest inbound message is "+
		"filed elsewhere or not at all; ask Salvador to file it, or draft from the switchboard session", threadID, slug)
	latest, err := latestInboundMessage(ctx, tx, threadID)
	if errors.Is(err, pgx.ErrNoRows) {
		return refused // nothing inbound: nothing filed, and no one to reply to
	}
	if err != nil {
		return fmt.Errorf("check thread %d against task %d's project: %w", threadID, taskID, err)
	}
	var decided *int64
	err = tx.QueryRow(ctx,
		`SELECT project_id FROM capture_decisions WHERE message_id = $1 ORDER BY id DESC LIMIT 1`,
		latest.id).Scan(&decided)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("check thread %d against task %d's project: %w", threadID, taskID, err)
	}
	if decided == nil || *decided != taskProject {
		return refused
	}
	return nil
}

// refuseClosedTask SHARE-locks the delivery's TASK row and refuses a closed
// task (SWT-37, Codex re-review). It runs FIRST in every approve, send and
// prefill transaction. Both orderings of the draft/close race then end
// refused: a close that commits first makes the draft refuse, and a draft that
// commits first can no longer be approved or sent once the task is closed.
//
// FOR SHARE, not FOR UPDATE (go-reviewer): SHARE still conflicts with
// closeTransition's FOR UPDATE, so close-vs-send is serialised in both orders,
// but it does NOT conflict with the FOR KEY SHARE a task_events insert takes on
// its parent task. mark_delivery_sent/failed, the gmail loop-closure sink and
// the Upwork reconciler lock a delivery THEN insert a task event (delivery →
// task); a FOR UPDATE here (task → delivery) would close a deadlock cycle with
// them. Sibling sends on one task no longer queue behind each other either.
//
// `delivered` is deliberately NOT refused here: R8 marks a task delivered after
// its FIRST send, and a sibling delivery drafted beside it (an email plus a Jira
// final comment) must still go out. A stale draft on a task marked delivered by
// hand stays behind the human approval gate.
func refuseClosedTask(ctx context.Context, tx pgx.Tx, deliveryID int64) error {
	taskID, status, err := lockDeliveryTask(ctx, tx, deliveryID)
	if err != nil {
		return err
	}
	if status == "closed" {
		return fmt.Errorf("delivery %d's task %d is closed: switchboard never approves or sends a delivery for "+
			"closed work; reopen the task first", deliveryID, taskID)
	}
	return nil
}

// lockDeliveryTask SHARE-locks the delivery's task row and returns its id and
// status: the ONE spelling of the task → delivery lock order (refuseClosedTask's
// rationale above). refuseClosedTask and rejectDelivery both call it;
// rejectDelivery does not refuse closed work (SWT-43 criterion 8).
func lockDeliveryTask(ctx context.Context, tx pgx.Tx, deliveryID int64) (taskID int64, status string, err error) {
	err = tx.QueryRow(ctx,
		`SELECT t.id, t.status FROM deliveries d JOIN tasks t ON t.id = d.task_id
		  WHERE d.id=$1 FOR SHARE OF t`, deliveryID).Scan(&taskID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", fmt.Errorf("delivery %d not found", deliveryID)
	}
	if err != nil {
		return 0, "", fmt.Errorf("lock task of delivery %d: %w", deliveryID, err)
	}
	return taskID, status, nil
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
	// RequireOwnDraft (SWT-44 review) is pinned to "true" by the user-scope MCP
	// profile (mcpserver.userProfilePins, after injectWorkerID, by overwrite):
	// the caller edits only drafts whose created_by is its own actor. The actor
	// is "mcp:" + OPS_WORKER_ID, and every interactive install — the user-scope
	// one AND this repo's full-profile ops — runs as manual:salvo, so "own"
	// means created by the mcp:manual:salvo actor (any interactive session),
	// never the drafts worker's or the dashboard's. It cannot tell one session
	// from another; RequireChannel below keeps it to gmail. It only narrows, so
	// it is absent from every schema; the dashboard, opsctl and the full
	// profile send none and edit any draft, as before.
	RequireOwnDraft string `json:"require_own_draft,omitempty"`
	// RequireChannel (SWT-44, second review) is pinned to "gmail" by the same
	// profile, matching draft_delivery's pin: without it the shared actor would
	// let a user-scope session rewrite a slack_reply, jira_comment,
	// upwork_chat or calendar draft that this repo's full-profile session
	// wrote. Checked under the row lock, in updateDelivery.
	RequireChannel string `json:"require_channel,omitempty"`
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
	// SWT-44 review: a present body must say something — an empty one would
	// sit in the approval queue as a blank email. subject "" stays legal: it
	// clears the subject, as it always has.
	if a.Body != nil && strings.TrimSpace(*a.Body) == "" {
		return errors.New("body is empty: a delivery must say something (omit body to keep the current one)")
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
		// The validator saw the body before the scrub; a body that was nothing
		// but an attribution trailer is empty now.
		if strings.TrimSpace(body) == "" {
			return nil, fmt.Errorf("delivery %d: body is empty once attribution lines are removed; a delivery must say something", a.DeliveryID)
		}
	}
	// One transaction, the delivery row locked: the drafted and ownership checks
	// and the write see the same row, so neither an approve nor a re-draft can
	// slip between them.
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		var status, channel, createdBy string
		if err := tx.QueryRow(ctx,
			`SELECT status, channel, COALESCE(created_by,'') FROM deliveries WHERE id=$1 FOR UPDATE`,
			a.DeliveryID).Scan(&status, &channel, &createdBy); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("delivery %d not found", a.DeliveryID)
			}
			return fmt.Errorf("lock delivery %d: %w", a.DeliveryID, err)
		}
		if status != "drafted" {
			return fmt.Errorf("delivery %d is not drafted (editing an approved draft would bypass approval)", a.DeliveryID)
		}
		if a.RequireChannel != "" && channel != a.RequireChannel {
			return fmt.Errorf("delivery %d is a %s delivery: this caller edits %s drafts only; change it on the dashboard",
				a.DeliveryID, channel, a.RequireChannel)
		}
		// draft_delivery stores created_by = executor.ActorFrom(ctx), the same
		// string compared here: mcp:manual:salvo for every interactive session,
		// user-scope and full-profile alike.
		if a.RequireOwnDraft == "true" {
			if actor := executor.ActorFrom(ctx); createdBy != actor {
				return fmt.Errorf("delivery %d was drafted by %s, not by %s: this caller edits only its own drafts "+
					"(created by its actor, gmail only); change it on the dashboard", a.DeliveryID, createdBy, actor)
			}
		}
		if _, err := tx.Exec(ctx,
			`UPDATE deliveries SET
			   subject = CASE WHEN $2 THEN NULLIF($3,'') ELSE subject END,
			   body    = CASE WHEN $4 THEN $5 ELSE body END,
			   updated_at = now()
			 WHERE id=$1`,
			a.DeliveryID, a.Subject != nil, subject, a.Body != nil, body); err != nil {
			return fmt.Errorf("update delivery %d: %w", a.DeliveryID, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
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

// DeliveryContentHash is the ONE spelling of "the words of a delivery" for the
// content-bound approval (SWT-44 review): lowercase hex sha256 of the subject,
// a NUL, then the body. The NUL keeps words from moving across the
// subject/body boundary under the same hash. A NULL subject is "", the
// dashboard's COALESCE and approve_delivery's.
func DeliveryContentHash(subject, body string) string {
	sum := sha256.Sum256([]byte(subject + "\x00" + body))
	return hex.EncodeToString(sum[:])
}

// approveDeliveryArgs binds the human gate to what the approver saw (SWT-44
// review). The dashboard renders DeliveryContentHash(subject, body) into the
// Approve form and posts it back as ExpectContentHash; an edit landing between
// the render and the click (a session's update_delivery) then makes the
// approve refuse instead of passing words nobody read. Omitted = the old
// behaviour, for opsctl and full-profile MCP callers, which name a delivery
// id without being shown a page. It only narrows, so no MCP schema lists it.
//
// approve_delivery stays human-only (policy human_only) and is on no user
// profile: the dashboard is the review surface, and its route REQUIRES the
// hash (dashboard approveAction refuses a POST without one), so the optional
// form here serves only the human CLI and this repo's own session.
type approveDeliveryArgs struct {
	DeliveryID        int64  `json:"delivery_id"`
	ExpectContentHash string `json:"expect_content_hash,omitempty"`
}

func validateApproveDelivery(args []byte) error {
	var a approveDeliveryArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.DeliveryID == 0 {
		return errors.New("missing delivery_id")
	}
	// A malformed hash can never match, and refusing it as "changed since it
	// was shown" would send the approver to reload for the wrong reason.
	if h := a.ExpectContentHash; h != "" &&
		(len(h) != sha256.Size*2 || strings.Trim(h, "0123456789abcdef") != "") {
		return fmt.Errorf("expect_content_hash %q is not a lowercase hex sha256 (tools.DeliveryContentHash)", h)
	}
	return nil
}

func approveDelivery(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a approveDeliveryArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		if err := refuseClosedTask(ctx, tx, a.DeliveryID); err != nil {
			return err
		}
		var status, subject, body string
		var extID *string
		if err := tx.QueryRow(ctx,
			`SELECT status, sent_external_id, COALESCE(subject,''), COALESCE(body,'')
			   FROM deliveries WHERE id=$1 FOR UPDATE`,
			a.DeliveryID).Scan(&status, &extID, &subject, &body); err != nil {
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
		// Compared under the FOR UPDATE lock update_delivery also takes, so no
		// edit can land between this check and the status write below.
		if a.ExpectContentHash != "" && a.ExpectContentHash != DeliveryContentHash(subject, body) {
			return fmt.Errorf("delivery %d changed since it was shown to you; reload and review it again", a.DeliveryID)
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

// ---- reject_delivery (SWT-43) ----------------------------------------------------

// maxRejectNoteRunes caps the note: it travels into the drafts worker's prompt.
// Runes, not bytes, so an accented note is not refused at half length.
const maxRejectNoteRunes = 2000

type rejectDeliveryArgs struct {
	DeliveryID int64  `json:"delivery_id"`
	Note       string `json:"note,omitempty"`
	Redraft    bool   `json:"redraft,omitempty"`
}

func validateRejectDelivery(args []byte) error {
	var a rejectDeliveryArgs
	if err := json.Unmarshal(args, &a); err != nil {
		// A non-boolean redraft lands here: it is refused, never coerced.
		return fmt.Errorf("parse args (delivery_id int, note string, redraft bool): %w", err)
	}
	if a.DeliveryID == 0 {
		return errors.New("missing delivery_id")
	}
	if n := utf8.RuneCountInString(a.Note); n > maxRejectNoteRunes {
		return fmt.Errorf("note is %d runes; the limit is %d", n, maxRejectNoteRunes)
	}
	return nil
}

// rejectDelivery is the human negative verdict (Deny, or Redo with redraft).
// It moves an unsent row to the terminal status 'rejected' and records the
// verdict as labelled data (an approvals row plus a delivery_rejected event).
// With redraft it also stamps redraft_requested_at, the one thing that makes
// that row stop blocking the drafts worker. It is humanOnly and off MCP (D9).
//
// Lock order is task → delivery, through lockDeliveryTask. Unlike approve and
// send it does NOT refuse a closed task: cleaning up a stale draft is exactly
// what closed work needs. The task status it reads is Redo's precondition (D7).
func rejectDelivery(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a rejectDeliveryArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	var note *string
	if strings.TrimSpace(a.Note) != "" {
		note = &a.Note
	}

	changed := false
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		taskID, taskStatus, err := lockDeliveryTask(ctx, tx, a.DeliveryID)
		if err != nil {
			return err
		}
		var status, channel string
		var extID, stored *string
		var confirmed, redraftRequested bool
		if err := tx.QueryRow(ctx,
			`SELECT status, channel, sent_external_id, confirmed_at IS NOT NULL,
			        redraft_requested_at IS NOT NULL, rejection_note
			   FROM deliveries WHERE id=$1 FOR UPDATE`,
			a.DeliveryID).Scan(&status, &channel, &extID, &confirmed, &redraftRequested, &stored); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("delivery %d not found", a.DeliveryID)
			}
			return fmt.Errorf("lock delivery %d: %w", a.DeliveryID, err)
		}
		refuse := func(reason string) error {
			return fmt.Errorf("delivery %d (%s, %s) cannot be rejected: %s", a.DeliveryID, status, channel, reason)
		}
		// D7: a draft can only be written for done_locally work (draft_delivery's
		// expect_task_status), so a redraft flag anywhere else would never be honoured.
		needDoneLocally := func() error {
			if taskStatus != "done_locally" {
				return refuse(fmt.Sprintf("its task %d is %s, and a new draft can only be written for "+
					"done_locally work; Deny it instead", taskID, taskStatus))
			}
			return nil
		}

		upgrade := false
		switch status {
		case "drafted", "approved":
		case "failed":
			// D4: sendJiraComment writes failed with a NULL id for EVERY error, so the
			// comment may have landed and the jira matcher still claims failed rows.
			if channel == "jira_comment" {
				return refuse("a failed jira_comment may have been sent (the Jira send cannot tell a " +
					"refusal from a lost response), and its matcher can still claim it")
			}
			if extID != nil || confirmed {
				return refuse("it carries a sent id or a confirmation, so it may have been sent")
			}
		case "rejected":
			switch {
			case !redraftRequested && !a.Redraft, redraftRequested && a.Redraft:
				return nil // replay: no-op success, writes nothing
			case redraftRequested && !a.Redraft:
				return refuse("a redraft was already requested and the drafts worker may have written the " +
					"new draft; deny the new draft instead (D6)")
			}
			upgrade = true // D6: plain Deny → Redo
		default: // sending, sent
			return refuse("it is sending or sent; the words may already be on the wire")
		}
		if a.Redraft {
			if err := needDoneLocally(); err != nil {
				return err
			}
		}

		eventNote := note
		if upgrade {
			if _, err := tx.Exec(ctx,
				`UPDATE deliveries SET redraft_requested_at=now(), rejection_note=COALESCE($2, rejection_note),
				        updated_at=now()
				  WHERE id=$1`, a.DeliveryID, note); err != nil {
				return fmt.Errorf("request redraft of delivery %d: %w", a.DeliveryID, err)
			}
			if note == nil {
				eventNote = stored
			}
		} else {
			if _, err := tx.Exec(ctx,
				`UPDATE deliveries SET status='rejected', rejection_note=$2,
				        redraft_requested_at=CASE WHEN $3::boolean THEN now() END, updated_at=now()
				  WHERE id=$1`, a.DeliveryID, note, a.Redraft); err != nil {
				return fmt.Errorf("reject delivery %d: %w", a.DeliveryID, err)
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO approvals (subject_type, subject_id, status, decided_by, decided_at)
				 VALUES ('delivery', $1, 'rejected', $2, now())`,
				a.DeliveryID, executor.ActorFrom(ctx)); err != nil {
				return fmt.Errorf("insert approval: %w", err)
			}
		}
		if _, err := insertTaskEvent(ctx, tx, taskID, "delivery_rejected", map[string]any{
			"delivery_id": a.DeliveryID, "channel": channel, "redraft": a.Redraft, "note": eventNote,
		}); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{
		"delivery_id": a.DeliveryID, "status": "rejected", "redraft": a.Redraft, "changed": changed,
	})
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
	if channel == "calendar" {
		// SWT-28 criterion 17: the human two-step routes to the SAME send half
		// the auto verb uses — one code path to the write route.
		return sendCalendarBlock(ctx, pool, a.DeliveryID)
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
		if err := refuseClosedTask(ctx, tx, a.DeliveryID); err != nil {
			return err
		}
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

		// Where it goes — From, To and the threading material — comes from
		// ResolveGmailRoute, the ONE spelling the dashboard shows on the draft
		// before approval (SWT-44 review): two spellings could show Salvador one
		// recipient and send to another.
		route, err := ResolveGmailRoute(ctx, tx, *fromAcct, *d.threadID)
		if err != nil {
			return err
		}
		gThread = route.GmailThread
		d.fromEmail = route.From // the row's own account, the join above read it too
		to, inReplyTo := route.To, route.InReplyTo

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

		// send_attempted_at is the dispatch instant, recorded for the same reason
		// the slack path records it: it is the lower time bound a post-hoc content
		// matcher needs (SWT-16's rule). Without it the gmail belt's floor is inert
		// for exactly the 'sending' and ambiguous 'failed' rows the belt exists to
		// resolve, since sent_at is also NULL there. It also makes this attempt
		// visible to the rate-limit window rather than invisible until it settles.
		if _, err := tx.Exec(ctx,
			`UPDATE deliveries SET status='sending', sent_external_id=$2,
			        send_attempted_at=now(), updated_at=now()
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

// GmailRoute is where a gmail delivery goes: the mailbox it is sent From, the
// address it is sent To, and the threading material the send needs.
type GmailRoute struct {
	From        string // the delivery's from account (source_accounts.account_email)
	To          string // the sender of the thread's latest inbound message
	InReplyTo   string // that message's Message-ID
	GmailThread string // the provider thread id, from the thread key
	Subject     string // the thread's subject (normalized_threads.subject)
}

// ResolveGmailRoute is the ONE spelling of where a gmail send goes (SWT-44
// review): send_delivery's phase 1 builds its message from it, under the
// delivery lock, and the dashboard shows its From, To and Subject on a draft
// before Salvador approves it. It only reads; q is the send's pgx.Tx or the
// dashboard's pool. The errors are the send path's own words. On error the
// route carries what resolved before the failure (the dashboard shows those
// parts and "(unresolved)" for the rest); the send uses none of it.
func ResolveGmailRoute(ctx context.Context, q store.Querier, fromAccountID, threadID int64) (GmailRoute, error) {
	var r GmailRoute
	if err := q.QueryRow(ctx,
		`SELECT account_email FROM source_accounts WHERE id=$1`, fromAccountID).Scan(&r.From); err != nil {
		return r, fmt.Errorf("resolve from account %d: %w", fromAccountID, err)
	}
	var threadKey string
	if err := q.QueryRow(ctx,
		`SELECT thread_key, COALESCE(subject,'') FROM normalized_threads WHERE id=$1`, threadID).
		Scan(&threadKey, &r.Subject); err != nil {
		return r, fmt.Errorf("resolve thread %d: %w", threadID, err)
	}
	_, gt, err := splitGmailThreadKey(threadKey)
	if err != nil {
		return r, err
	}
	r.GmailThread = gt
	m, err := latestInboundMessage(ctx, q, threadID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return r, fmt.Errorf("thread %d has no inbound message to reply to", threadID)
		}
		return r, fmt.Errorf("resolve reply target: %w", err)
	}
	r.To, r.InReplyTo = m.sender, m.messageID
	return r, nil
}

// inboundMessage is the part of a thread's reply target the send and the
// same-project rule read.
type inboundMessage struct {
	id        int64
	sender    string
	messageID string
}

// latestInboundMessage is the ONE spelling of "the message a gmail reply on
// this thread answers": the thread's latest INBOUND message, `ORDER BY sent_at
// DESC, id DESC`. ResolveGmailRoute takes its To and In-Reply-To from it, and
// refuseThreadOutsideTaskProject checks ITS filing — so the same-project rule
// follows the recipient. pgx.ErrNoRows (unwrapped) when nothing is inbound.
func latestInboundMessage(ctx context.Context, q store.Querier, threadID int64) (inboundMessage, error) {
	var m inboundMessage
	err := q.QueryRow(ctx,
		`SELECT id, COALESCE(sender,''), COALESCE(external_message_id,'')
		 FROM normalized_messages
		 WHERE thread_id=$1 AND direction='inbound'
		 ORDER BY sent_at DESC, id DESC LIMIT 1`, threadID).Scan(&m.id, &m.sender, &m.messageID)
	return m, err
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
		var priorError string
		if err := tx.QueryRow(ctx,
			`SELECT status, channel, task_id, approval_source, COALESCE(error,'')
			   FROM deliveries WHERE id=$1 FOR UPDATE`,
			a.DeliveryID).Scan(&status, &channel, &taskID, &approvalSource, &priorError); err != nil {
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
			// Strip ONLY the reconciler's marker, keeping any other diagnostic.
			//
			// This re-arms the alarm: its fire-once guard is that marker string
			// inside deliveries.error, so leaving it would mean a row that was
			// flagged, failed, re-approved and sent again could never be flagged
			// again — permanently silent for exactly the delivery it had already
			// caught once.
			//
			// An earlier cut set error=NULL outright and justified it by claiming
			// the old text survived in the audit trail. It does not: the executor
			// audit stores the CALLER'S ARGUMENTS, not the row's prior state, and
			// the surrounding task events carry only delivery_id/channel. So a
			// real sender failure recorded before the flag would have been
			// destroyed by the re-arm, which is the opposite of what a diagnostic
			// column is for. Removing the marker alone keeps both properties.
			`UPDATE deliveries SET status='sent', sent_at=now(), updated_at=now(),
			        error = NULLIF($2,'')
			  WHERE id=$1`, a.DeliveryID, store.StripUnconfirmedNote(priorError)); err != nil {
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
		if err := refuseClosedTask(ctx, tx, deliveryID); err != nil {
			return err
		}
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
		if err := refuseClosedTask(ctx, tx, deliveryID); err != nil {
			return err
		}
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
			"the message IS in Slack — check the channel and do NOT re-approve, because approve_delivery "+
			"accepts a failed row and a resend would double-post", deliveryID)
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
		// slack_reply ONLY — and the upwork_chat extension that briefly lived here
		// was REVERTED, deliberately, because it was worse than the gap it filled.
		//
		// An upwork_chat row reaches 'sent' through mark_delivery_sent, which
		// emits delivery_sent, which drives orchestrator R8: the work task flips
		// to delivered, its Deliver task is CLOSED, and an orchestration row is
		// recorded so R8 will not run again. Flipping that delivery to 'failed'
		// emits delivery_failed, for which there is NO orchestrator rule. The
		// result is a real non-delivery permanently represented as delivered,
		// with its Deliver task closed and the draft worker never picking it up —
		// a database that disagrees with the world, silently, which is the exact
		// class of failure SWT-18 and SWT-19 exist to remove.
		//
		// slack_reply does not have that problem: it wedges at 'sending', which
		// means delivery_sent never fired and R8 never ran, so failing it
		// contradicts nothing.
		//
		// The honest recovery for a flagged upwork row needs a compensating
		// lifecycle transition — reopening the work and its Deliver task — and
		// that belongs with SWT-20's other go-live blockers, not bolted onto this
		// verb. Until then the reconciler's note names an action that exists and
		// does not corrupt state. Reachable consequence today: none, production
		// has never had an upwork_chat delivery.
		if channel != "slack_reply" {
			return fmt.Errorf("mark_delivery_failed only supports slack_reply; delivery %d is %s "+
				"(upwork_chat recovery needs a compensating lifecycle transition — SWT-20)", a.DeliveryID, channel)
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
