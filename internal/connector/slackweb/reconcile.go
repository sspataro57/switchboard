package slackweb

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultUnconfirmedFlagPasses is how many completed successful export passes a
// Slack delivery may go unconfirmed before it is flagged for a human (SWT-12,
// Q2). Passes rather than wall time: a paused poller, a suspended CronJob, or a
// mini that is simply off accumulates no passes and therefore cannot false-flag,
// whereas wall time would raise "the send may have failed" when the fact is "the
// poller didn't run". Those are different facts and must not share an alarm.
const DefaultUnconfirmedFlagPasses = 3

// UnconfirmedFlagPasses reads the SLACK_UNCONFIRMED_FLAG_PASSES override.
//
// Anything unparseable or non-positive falls back to the default: a
// misconfigured 0 would otherwise mean "flag every unconfirmed row on the first
// pass", turning an operational typo into a stream of false alarms about
// messages that are merely waiting for the next export.
func UnconfirmedFlagPasses() int {
	raw := os.Getenv("SLACK_UNCONFIRMED_FLAG_PASSES")
	if raw == "" {
		return DefaultUnconfirmedFlagPasses
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return DefaultUnconfirmedFlagPasses
	}
	return n
}

// unconfirmedNote prefixes the note ReconcileUnconfirmed writes. It doubles as
// the fire-once guard: an ambiguous-failure row already carries the send error in
// `error`, so "flag only when error IS NULL" would never flag the rows that most
// need it. Matching the marker instead lets the note append to a real error.
const unconfirmedNote = "unconfirmed after"

// ReconcileUnconfirmed flags — never retries — Slack replies that no export has
// confirmed after `passes` completed successful passes for their workspace.
//
// A browser click reserves no message id, so a delivery is only ever confirmed
// post hoc by the body-prefix matcher. When that never happens the row is
// genuinely ambiguous: the message may be in Slack, or the click may have been
// lost. Retrying is the one thing that must not happen, because a retry of a
// click that did land is a double-post into a client channel. So this raises a
// human signal and moves nothing: no status change, no invented
// sent_external_id.
//
// Deterministic SQL, connector-side, no LLM — the orchestrator stays pure
// (invariant 7) and has no rule for delivery_unconfirmed.
func ReconcileUnconfirmed(ctx context.Context, sink *PGSink, passes int) (int, error) {
	if passes <= 0 {
		passes = DefaultUnconfirmedFlagPasses
	}
	// send_attempted_at is the honest instant (migration 0012). updated_at is only
	// a fallback for rows predating it: for an ambiguous row it approximates the
	// attempt purely because the settle write bumps it, and that write may not
	// land at all if the caller's deadline blew.
	rows, err := sink.pool.Query(ctx,
		`SELECT id, task_id, COALESCE(target_ref,''), COALESCE(sent_at, send_attempted_at, updated_at)
		   FROM deliveries
		  WHERE channel='slack_reply'
		    AND status IN ('sending','sent')
		    AND sent_external_id IS NULL
		    AND confirmed_at IS NULL
		    AND (error IS NULL OR position($1 in error) = 0)
		  ORDER BY id`, unconfirmedNote)
	if err != nil {
		return 0, fmt.Errorf("select unconfirmed Slack deliveries: %w", err)
	}
	type candidate struct {
		deliveryID int64
		taskID     int64
		targetRef  string
		since      time.Time
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.deliveryID, &c.taskID, &c.targetRef, &c.since); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan unconfirmed Slack delivery: %w", err)
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate unconfirmed Slack deliveries: %w", err)
	}

	flagged := 0
	for _, c := range candidates {
		target, err := ParseTargetURL(c.targetRef)
		if err != nil {
			// Unparseable target: nothing to count passes against. Leave it be
			// rather than flag on a guess.
			continue
		}
		accountEmail := strings.ToLower(target.WorkspaceID) + "@slack-web.local"

		// Only runs that STARTED after the click can have observed the message.
		// A run already in flight at send time may have scraped the channel
		// before the message existed, so counting it would flag early.
		var observed int
		if err := sink.pool.QueryRow(ctx,
			`SELECT count(*) FROM sync_runs r
			   JOIN source_accounts a ON a.id = r.source_account_id
			  WHERE a.provider=$1 AND a.account_email=$2
			    AND r.status='ok' AND r.started_at > $3`,
			Provider, accountEmail, c.since).Scan(&observed); err != nil {
			return flagged, fmt.Errorf("count export passes for %s: %w", accountEmail, err)
		}
		if observed < passes {
			continue
		}

		note := fmt.Sprintf("%s %d export passes with no matching message in Slack; "+
			"verify manually then mark_delivery_sent or mark_delivery_failed", unconfirmedNote, observed)
		tag, err := sink.pool.Exec(ctx,
			`UPDATE deliveries
			    SET error = CASE WHEN COALESCE(error,'') = '' THEN $2 ELSE error || ' | ' || $2 END,
			        updated_at = now()
			  WHERE id=$1 AND (error IS NULL OR position($3 in error) = 0)`,
			c.deliveryID, note, unconfirmedNote)
		if err != nil {
			return flagged, fmt.Errorf("flag unconfirmed Slack delivery %d: %w", c.deliveryID, err)
		}
		if tag.RowsAffected() == 0 {
			// Another pass won the race; it owns the event.
			continue
		}
		payload, _ := json.Marshal(map[string]any{
			"delivery_id": c.deliveryID, "channel": DeliveryChannel, "passes": observed,
		})
		if _, err := sink.pool.Exec(ctx,
			`INSERT INTO task_events (task_id, event_type, payload)
			 VALUES ($1,'delivery_unconfirmed',$2)`, c.taskID, payload); err != nil {
			return flagged, fmt.Errorf("insert delivery_unconfirmed event: %w", err)
		}
		flagged++
	}
	return flagged, nil
}
