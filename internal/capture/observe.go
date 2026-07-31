// Package capture closes the half of invariant 5 that the delivery matchers
// cannot: a message sent entirely OUTSIDE switchboard.
//
// Such a message already lands raw, normalizes with direction='outbound', never
// becomes a spurious task (triage filters to inbound), and already reaches triage
// and the draft worker as thread history. What was missing is task-log linkage —
// "attaches as task log" fires only through a delivery match, so a reply Salvador
// sent from the Gmail app appended nothing to the task it answered, and a worker
// could redo work already done.
//
// This is a post-normalize deterministic pass in the mold of
// slackweb.ReconcileUnconfirmed: SQL and Go, no LLM, no provider adapters, run by
// each connector's main after Normalize so the matchers have already claimed what
// is theirs. It appends one informational task_event and touches nothing else. In
// particular it creates NO deliveries row: these were never switchboard
// deliveries, and inventing rows would record a gate that never happened — the
// same reasoning as SWT-12's approval_source.
package capture

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
	"github.com/sspataro57/switchboard/internal/textmatch"
)

// DefaultObserveHorizon bounds how far back a pass looks. Long enough that a
// connector resuming after weeks suspended still captures the backlog that
// matters; short enough that a first deploy does not flood closed tasks with
// months of stale observations.
const DefaultObserveHorizon = 720 * time.Hour

// ObserveHorizon reads the OUTBOUND_OBSERVE_HORIZON override (a Go duration).
//
// Anything unparseable or non-positive falls back to the default. "720" is the
// realistic typo — a bare number is not a Go duration, and reading it as 720ns
// would make the scan window empty, so capture would log nothing forever with no
// error anywhere. Same defensive shape as slackweb.UnconfirmedFlagPasses.
func ObserveHorizon() time.Duration {
	raw := os.Getenv("OUTBOUND_OBSERVE_HORIZON")
	if raw == "" {
		return DefaultObserveHorizon
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return DefaultObserveHorizon
	}
	return d
}

// bodyPreviewLen matches slackweb's matcher width, so a preview in the log reads
// the same as the prefix the confirmation path compares.
const bodyPreviewLen = 120

// Channel binds a message channel to the delivery channel that would claim its
// sends, plus how a thread reaches a task on that channel.
//
// The join is the crux of this package. There is exactly ONE stored, deterministic
// thread→task association in the schema: `deliveries`. `tasks` has no thread
// column, triage is still shadow so no task was ever created from a message, and
// external_refs keys are agent-chosen free text that cannot be matched against a
// thread_key. Every heuristic association the draft worker makes is materialized
// into the delivery row, so this join is complete with respect to links that
// exist — and a task with no delivery on the thread has no machine-readable link
// at all, which this package refuses to guess about.
type Channel struct {
	// Message is normalized_messages.channel.
	Message string
	// Delivery is the deliveries.channel whose sends would claim these ids.
	Delivery string
	// joinByThreadID keys on deliveries.thread_id rather than target_ref. Gmail
	// drafts always carry thread_id (draft_delivery requires it); the other
	// channels store a target_ref whose format equals the thread_key.
	joinByThreadID bool
	// targetRefs maps a thread_key to the target_ref spellings that correspond to
	// it. Nil means "use the thread_key verbatim".
	targetRefs func(threadKey string) []string
}

var (
	// Gmail joins on thread_id: the delivery row carries it directly.
	Gmail = Channel{Message: "gmail", Delivery: "gmail", joinByThreadID: true}

	// Jira's thread_key and a jira_comment target_ref are the same string,
	// jira:{site_host}:{KEY}.
	Jira = Channel{Message: "jira", Delivery: "jira_comment"}

	// Slack stores a canonical app.slack.com URL, so the thread_key has to be
	// translated — and both the thread and conversation forms correspond, because
	// a delivery aimed at the channel corresponds on the whole conversation.
	Slack = Channel{Message: "slack", Delivery: "slack_reply", targetRefs: SlackTargetRefs}
)

// SlackTargetRefs turns a slack thread_key into every target_ref spelling that
// corresponds to it.
//
// Exported so a test can pin equality with slackweb.Target.CanonicalURL's output.
// That is not ceremony: the join matches target_ref by EXACT string, and SWT-13's
// landmine was precisely a non-canonical spelling that silently never matched and
// left a delivery unconfirmable forever. A second URL builder in a second package
// would reproduce it with no error anywhere, so this one is built from slackweb's
// own type and checked against it.
//
// A key that is not a slack thread key yields nothing: refusing to guess beats
// producing a URL that can never match.
func SlackTargetRefs(threadKey string) []string {
	parts := strings.Split(threadKey, ":")
	if len(parts) < 3 || len(parts) > 4 || parts[0] != "slack" {
		return nil
	}
	workspace, conversation := parts[1], parts[2]
	if workspace == "" || conversation == "" {
		return nil
	}
	conv := slackweb.Target{WorkspaceID: workspace, ConversationID: conversation}.CanonicalURL()
	if len(parts) == 3 {
		return []string{conv}
	}
	if parts[3] == "" {
		return nil
	}
	thread := slackweb.Target{
		WorkspaceID: workspace, ConversationID: conversation, MessageID: parts[3],
	}.CanonicalURL()
	return []string{thread, conv}
}

// candidate is one outbound message no delivery claimed.
type candidate struct {
	messageID  int64
	externalID string
	threadID   int64
	threadKey  string
	sentAt     time.Time
	sender     string
	body       string
}

// ObserveOutbound appends an `outbound_observed` event to every task linked to the
// thread of an outbound message that switchboard did not send. It returns the
// number of EVENTS appended, so one message on a thread with two linked tasks
// counts twice.
//
// Purely informational: no task status changes, no delivery is created or mutated,
// no sent_external_id is ever invented, and the orchestrator has no rule for the
// event (the drain ignores unknown types).
func ObserveOutbound(ctx context.Context, pool *pgxpool.Pool, ch Channel) (int, error) {
	if ch.Message == "" || ch.Delivery == "" {
		return 0, fmt.Errorf("capture: channel is not configured")
	}
	horizon := ObserveHorizon()
	since := time.Now().Add(-horizon)

	// Unclaimed means no delivery on this channel carries the id as its
	// sent_external_id. Switchboard's own sends always end up claimed: gmail
	// reserves its Message-ID before sending, and the slack/jira matchers stamp
	// theirs on a later pass. A NULL or empty external id cannot be claimed by
	// that test, and counts as unclaimed — switchboard sends always carry one.
	rows, err := pool.Query(ctx,
		`SELECT m.id, COALESCE(m.external_message_id,''), m.thread_id,
		        COALESCE(t.thread_key,''), m.sent_at, COALESCE(m.sender,''),
		        COALESCE(m.body_text,'')
		   FROM normalized_messages m
		   JOIN normalized_threads t ON t.id = m.thread_id
		  WHERE m.channel = $1
		    AND m.direction = 'outbound'
		    AND m.sent_at >= $2
		    AND NOT EXISTS (
		      SELECT 1 FROM deliveries d
		       WHERE d.channel = $3
		         AND d.sent_external_id IS NOT NULL
		         AND d.sent_external_id = m.external_message_id)
		  ORDER BY m.id`,
		ch.Message, since, ch.Delivery)
	if err != nil {
		return 0, fmt.Errorf("select unclaimed outbound %s messages: %w", ch.Message, err)
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.messageID, &c.externalID, &c.threadID, &c.threadKey,
			&c.sentAt, &c.sender, &c.body); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan outbound %s message: %w", ch.Message, err)
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate outbound %s messages: %w", ch.Message, err)
	}

	appended := 0
	for _, c := range candidates {
		var refList []string
		switch {
		case ch.joinByThreadID:
			// The join keys on thread_id, which is NOT NULL; refList is unused.
		case ch.targetRefs != nil:
			refList = ch.targetRefs(c.threadKey)
			if len(refList) == 0 {
				// Not a key this channel can translate; no join candidate.
				continue
			}
		case c.threadKey == "":
			// An empty key would join every delivery whose target_ref is '',
			// attributing the message to unrelated tasks. Unreachable today
			// (draft_delivery requires a target_ref) — refuse anyway, exactly as
			// SlackTargetRefs refuses a key it cannot translate.
			continue
		default:
			refList = []string{c.threadKey}
		}

		deferred, err := hasUnconfirmedClaimant(ctx, pool, ch, c, refList, since)
		if err != nil {
			return appended, err
		}
		if deferred {
			// A switchboard send to the same destination is still unresolved, so
			// this message may BE that send with its stamp not yet applied (or
			// refused — the matcher declines to guess on a multi-match or outside
			// the skew floor). Claiming "sent outside switchboard" here would be a
			// lie. Skip; the next pass retries once the row settles or the horizon
			// moves past it.
			continue
		}

		taskIDs, err := linkedTasks(ctx, pool, ch, c, refList)
		if err != nil {
			return appended, err
		}
		payload, err := json.Marshal(map[string]any{
			"message_id":          c.messageID,
			"external_message_id": c.externalID,
			"channel":             ch.Message,
			"thread_key":          c.threadKey,
			"sent_at":             c.sentAt.UTC().Format(time.RFC3339),
			"sender":              c.sender,
			"body_preview":        normalizedPrefix(c.body, bodyPreviewLen),
		})
		if err != nil {
			return appended, fmt.Errorf("marshal outbound_observed payload: %w", err)
		}
		for _, taskID := range taskIDs {
			// Dedup on normalized_messages.id, which is stable across
			// re-normalization — external_message_id can be absent. The guard is
			// task_events_outbound_observed_uniq (0013) rather than a preceding
			// read, so a --all replay adds nothing and two passes racing on the
			// same channel cannot both win. Compared as text, not ::bigint: the
			// cast would make this query ERROR (failing the connector run) if any
			// future event type ever wrote a non-numeric message_id, since nothing
			// guarantees Postgres evaluates the event_type qual first.
			tag, err := pool.Exec(ctx,
				`INSERT INTO task_events (task_id, event_type, payload)
				 VALUES ($1, 'outbound_observed', $2::jsonb)
				 ON CONFLICT (task_id, (payload->>'message_id'))
				   WHERE event_type = 'outbound_observed'
				   DO NOTHING`,
				taskID, payload)
			if err != nil {
				return appended, fmt.Errorf("append outbound_observed for task %d: %w", taskID, err)
			}
			appended += int(tag.RowsAffected())
		}
	}
	return appended, nil
}

// hasUnconfirmedClaimant reports whether a switchboard send to the same
// destination is still in flight or unresolved, in which case this message must
// not be claimed as external.
//
// Horizon-bounded on purpose: an unresolvable claimant must not blind capture on a
// conversation forever. COALESCE order matters — sent_at, then the attempt instant
// (0012), and only then updated_at, because updated_at is bumped by later error
// writes and reading it first would defer indefinitely.
func hasUnconfirmedClaimant(ctx context.Context, pool *pgxpool.Pool, ch Channel,
	c candidate, refList []string, since time.Time) (bool, error) {
	const pending = `status IN ('sending','sent') AND sent_external_id IS NULL
	                 AND confirmed_at IS NULL
	                 AND COALESCE(sent_at, send_attempted_at, updated_at) >= $2`
	var exists bool
	var err error
	if ch.joinByThreadID {
		err = pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM deliveries
			   WHERE channel=$1 AND thread_id=$3 AND `+pending+`)`,
			ch.Delivery, since, c.threadID).Scan(&exists)
	} else {
		err = pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM deliveries
			   WHERE channel=$1 AND target_ref = ANY($3) AND `+pending+`)`,
			ch.Delivery, since, refList).Scan(&exists)
	}
	if err != nil {
		return false, fmt.Errorf("check unconfirmed claimant for message %d: %w", c.messageID, err)
	}
	return exists, nil
}

// linkedTasks returns every DISTINCT task with a delivery on this thread, any
// status including drafted.
//
// No status filter and no "most recent delivery wins": the event is informational
// history, so picking one would be a guess, and filtering would hide the
// observation from exactly the task a human is about to inspect. A pending draft
// whose thread just received a direct reply is the most useful case of all.
func linkedTasks(ctx context.Context, pool *pgxpool.Pool, ch Channel,
	c candidate, refList []string) ([]int64, error) {
	var rows interface {
		Next() bool
		Scan(...any) error
		Err() error
		Close()
	}
	var err error
	if ch.joinByThreadID {
		rows, err = pool.Query(ctx,
			`SELECT DISTINCT task_id FROM deliveries
			  WHERE channel=$1 AND thread_id=$2 ORDER BY task_id`,
			ch.Delivery, c.threadID)
	} else {
		rows, err = pool.Query(ctx,
			`SELECT DISTINCT task_id FROM deliveries
			  WHERE channel=$1 AND target_ref = ANY($2) ORDER BY task_id`,
			ch.Delivery, refList)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve linked tasks for message %d: %w", c.messageID, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan linked task: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate linked tasks: %w", err)
	}
	return out, nil
}

// normalizedPrefix delegates to the shared spelling (SWT-16), so a preview in the
// log reads exactly as the prefix the confirmation matchers compare.
func normalizedPrefix(value string, limit int) string {
	return textmatch.NormalizedPrefix(value, limit)
}
