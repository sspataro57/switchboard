package upworkcrm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"
)

// DefaultUnconfirmedFlagPasses is how many completed successful sync runs an
// upwork_chat delivery may go unconfirmed before it is flagged for a human.
//
// SIX, where slackweb's equivalent is three, and the difference is not a
// judgement call — it is arithmetic. ONE upworkcrm invocation writes TWO
// sync_runs rows: Ingest calls StartRun (ingest.go) and Normalize calls it again
// (normalize.go), and both finish 'ok'. Verified in production, where the rows
// arrive in pairs on every */15 tick. A threshold copied from slackweb would
// therefore fire after 1.5 CronJob invocations — about 22 minutes — instead of
// three. Six passes == three invocations == roughly 45 minutes at */15.
//
// Do NOT "fix" this by telling the two run kinds apart through their stats
// jsonb. Both marshal the same Stats struct, so the discriminating keys are
// present-and-zero in the run that did not populate them rather than absent —
// a predicate whose discriminating column is a constant, which is the exact
// mistake SWT-18's review caught and SWT-19 was opened to repair. If the two
// kinds ever must be distinguished, add a column that says which.
//
// Passes rather than wall time, for slackweb's reason: a suspended CronJob
// accumulates no passes and therefore cannot false-flag, whereas wall time
// would raise "the send may have failed" when the fact is "the connector didn't
// run". Those are different facts and must not share an alarm.
const DefaultUnconfirmedFlagPasses = 6

// UnconfirmedFlagPasses reads the UPWORK_UNCONFIRMED_FLAG_PASSES override.
// Anything unparseable or non-positive falls back to the default: a
// misconfigured 0 would mean "flag every unconfirmed row immediately", turning
// an operational typo into a stream of false alarms about rows that are merely
// waiting for the next run.
func UnconfirmedFlagPasses() int {
	raw := os.Getenv("UPWORK_UNCONFIRMED_FLAG_PASSES")
	if raw == "" {
		return DefaultUnconfirmedFlagPasses
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return DefaultUnconfirmedFlagPasses
	}
	return n
}

// unconfirmedNote prefixes the note ReconcileUnconfirmed writes, and doubles as
// the fire-once guard. Matching the marker rather than requiring error IS NULL
// lets the note append to a row that already carries an error.
const unconfirmedNote = "unconfirmed after"

// ReconcileUnconfirmed flags — never retries, never invents a sent_external_id —
// upwork_chat deliveries that no run has confirmed after `passes` completed
// successful sync runs.
//
// Why this ships with SWT-19 rather than later: every failure mode around this
// matcher has one signature — a row at status='sent' with sent_external_id NULL,
// forever, and nothing anywhere saying so. That covers a target_ref that no
// longer parses, a refusal on an ambiguous body prefix, and the pre-existing
// one-shot gap (confirmUpworkDelivery runs only from upsertMessage, only for
// pending raw items, so a message normalized BEFORE its delivery reaches 'sent'
// is never re-examined — and on the assisted tier a human clicks "mark sent"
// minutes or hours after pasting). Changing the matcher's scoping without
// shipping the detector would be shipping a silent failure mode on purpose.
//
// Retrying is the one thing that must not happen: there is no automated upwork
// send to retry, and a human re-sending a message that did land is a duplicate
// in a client's chat. So this raises a signal and moves nothing.
//
// Deterministic SQL, connector-side, no LLM — the orchestrator stays pure
// (invariant 7) and has no rule for delivery_unconfirmed.
func ReconcileUnconfirmed(ctx context.Context, sink *PGSink, passes int) (int, error) {
	if passes <= 0 {
		passes = DefaultUnconfirmedFlagPasses
	}
	// sent_at is the only instant available on this tier: send_attempted_at is
	// never written for upwork_chat (send_delivery is policy-denied for the
	// channel, and mark_delivery_sent writes status and sent_at only), which is
	// the same fact that made a time floor impossible in the matcher itself.
	// Here it is sound rather than inert: sent_at is when the human said the
	// message went out, and counting runs that STARTED after that is exactly the
	// question being asked.
	rows, err := sink.pool.Query(ctx,
		`SELECT id, task_id, COALESCE(sent_at, updated_at)
		   FROM deliveries
		  WHERE channel='upwork_chat'
		    AND status='sent'
		    AND sent_external_id IS NULL
		    AND confirmed_at IS NULL
		    AND (error IS NULL OR position($1 in error) = 0)
		  ORDER BY id`, unconfirmedNote)
	if err != nil {
		return 0, fmt.Errorf("select unconfirmed upwork deliveries: %w", err)
	}
	type candidate struct {
		deliveryID int64
		taskID     int64
		since      time.Time
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.deliveryID, &c.taskID, &c.since); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan unconfirmed upwork delivery: %w", err)
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate unconfirmed upwork deliveries: %w", err)
	}

	flagged := 0
	for _, c := range candidates {
		// Only runs that STARTED after the send can have observed the message. A
		// run already in flight at send time may have read the source before the
		// message existed, so counting it would flag early.
		var observed int
		if err := sink.pool.QueryRow(ctx,
			`SELECT count(*) FROM sync_runs r
			   JOIN source_accounts a ON a.id = r.source_account_id
			  WHERE a.provider=$1 AND a.account_email=$2
			    AND r.status='ok' AND r.started_at > $3`,
			Provider, AccountEmail, c.since).Scan(&observed); err != nil {
			return flagged, fmt.Errorf("count sync passes for delivery %d: %w", c.deliveryID, err)
		}
		if observed < passes {
			continue
		}

		// Name only actions that EXIST and do not corrupt state. This note has
		// been wrong twice, in opposite directions, and the history is worth
		// keeping: it first named mark_delivery_sent and mark_delivery_failed,
		// both of which reject this row; the fix extended mark_delivery_failed to
		// upwork, which was then reverted because failing a row that R8 had
		// already processed leaves the work task permanently 'delivered' while
		// the delivery says failed.
		//
		// So today the honest instruction is: if the message IS in Upwork,
		// nothing needs doing — the row is 'sent', which is true, and only the
		// external-id link is missing. If it is NOT, the delivery must be redone,
		// and that needs a new task rather than a status flip, because the work
		// task is already closed out as delivered. A one-verb recovery arrives
		// with SWT-20's compensating lifecycle transition.
		note := fmt.Sprintf("%s %d sync passes with no matching Upwork message. "+
			"Check the Upwork thread. If the message IS there, no action — only the external-id "+
			"link is missing. If it is NOT there, the reply was never delivered: raise a new task "+
			"to redo it (the work task is already closed as delivered, and no status flip fixes "+
			"that until SWT-20)", unconfirmedNote, observed)
		// The candidate list was read before the pass counting above, so a
		// concurrent normalize run may have CONFIRMED this row in the meantime.
		// Guarding only on the marker would then append "unconfirmed after N
		// passes" to a row that is demonstrably confirmed, and emit the event to
		// match — a contradictory alarm that outlives the race in the task
		// history and on the dashboard, where a human reads it as evidence.
		//
		// So restate the full candidate predicate on the UPDATE. RowsAffected
		// then reports whether the row was still unconfirmed at write time, and
		// the event below is only written when it was.
		// The marker and the event go in ONE transaction, holding the delivery row
		// locked. Two separate statements autocommit between, which left two real
		// holes: a normalize run could confirm the row in the gap, so the event
		// announced "unconfirmed" about a delivery that was by then confirmed;
		// and if the event insert failed, the marker was already committed, so
		// the fire-once guard suppressed every future attempt and the signal was
		// lost permanently. An alarm that can silently lose itself is worse than
		// no alarm — the same lesson this file has now learned twice.
		//
		// SELECT ... FOR UPDATE first so the guard is evaluated against a row no
		// concurrent matcher can change until this commits.
		wrote, err := func() (bool, error) {
			tx, err := sink.pool.Begin(ctx)
			if err != nil {
				return false, fmt.Errorf("begin flag for delivery %d: %w", c.deliveryID, err)
			}
			defer func() { _ = tx.Rollback(ctx) }()

			tag, err := tx.Exec(ctx,
				`UPDATE deliveries
				    SET error = CASE WHEN COALESCE(error,'') = '' THEN $2 ELSE error || ' | ' || $2 END,
				        updated_at = now()
				  WHERE id=$1
				    AND status='sent'
				    AND sent_external_id IS NULL
				    AND confirmed_at IS NULL
				    AND (error IS NULL OR position($3 in error) = 0)`,
				c.deliveryID, note, unconfirmedNote)
			if err != nil {
				return false, fmt.Errorf("flag unconfirmed upwork delivery %d: %w", c.deliveryID, err)
			}
			if tag.RowsAffected() == 0 {
				// Another pass won the race and owns the event, or the matcher
				// confirmed the row while this one was counting. Both mean: say
				// nothing, and roll back rather than leave a marker behind.
				return false, nil
			}
			payload, _ := json.Marshal(map[string]any{
				"delivery_id": c.deliveryID, "channel": "upwork_chat", "passes": observed,
			})
			if _, err := tx.Exec(ctx,
				`INSERT INTO task_events (task_id, event_type, payload)
				 VALUES ($1,'delivery_unconfirmed',$2)`, c.taskID, payload); err != nil {
				return false, fmt.Errorf("insert delivery_unconfirmed event: %w", err)
			}
			if err := tx.Commit(ctx); err != nil {
				return false, fmt.Errorf("commit flag for delivery %d: %w", c.deliveryID, err)
			}
			return true, nil
		}()
		if err != nil {
			return flagged, err
		}
		if !wrote {
			continue
		}
		flagged++
	}
	return flagged, nil
}
