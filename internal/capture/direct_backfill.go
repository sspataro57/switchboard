package capture

// DirectBackfill (SWT-78, docs/bugs/slack-messages-not-becoming-tasks_DIAGNOSIS.md
// "Backfill"): puts NAMED Slack DMs that the inquiry lane dropped onto their
// conversation tasks. The fixed live pass cannot re-decide them — each already
// carries a live `attributed` decision, and the live claim is one row per
// message, forever — so this is a one-off verb that runs the SAME decision
// (decideMessage, hence directConversationTask and the conversation lookup)
// and the same executor calls, writing NO capture_decisions row. Its trace is
// the `log` event naming each message (the backfill marker below) and the
// audit rows.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/textmatch"
)

// DirectBackfillActor attributes every backfill tool call in audit_events.
const DirectBackfillActor = "capture:backfill"

// directBackfillMarker opens every backfill log line; the idempotency check
// reads it, so a second run finds the message already done.
const directBackfillMarker = "capture: DM backfill (SWT-78)"

// ErrDirectBackfillLockHeld: a capture pass holds the rules lock. The backfill
// decides against the same tasks a live pass does, so it never runs beside one.
var ErrDirectBackfillLockHeld = errors.New("capture rules lock is held by another pass")

// DirectBackfillStats counts one backfill run.
type DirectBackfillStats struct {
	Named, Created, Attached, AlreadyDone, Skipped int
}

// DirectBackfill processes the named messages oldest first. dryRun writes
// nothing and prints what a live run would do, simulating the conversation
// tasks it would create so a later message reads as an attach.
func DirectBackfill(ctx context.Context, pool *pgxpool.Pool, ex *executor.Executor,
	ids []int64, dryRun bool, out io.Writer) (DirectBackfillStats, error) {
	var st DirectBackfillStats
	if pool == nil {
		return st, errors.New("direct backfill: nil database pool")
	}
	if !dryRun && ex == nil {
		return st, errors.New("direct backfill: a live run requires an executor (invariant 3)")
	}
	release, held, err := tryRulesLock(ctx, pool)
	if err != nil {
		return st, err
	}
	if !held {
		return st, ErrDirectBackfillLockHeld
	}
	defer release()

	rules, err := loadRules(ctx, pool)
	if err != nil {
		return st, err
	}
	byID := make(map[int64]storedRule, len(rules))
	pure := make([]Rule, 0, len(rules))
	for _, r := range rules {
		byID[r.rule.ID] = r
		pure = append(pure, r.rule)
	}

	// The ONE projection capture decides from; inbound only (his own messages
	// never become tasks, invariant 5), oldest first whatever the id order.
	rows, err := pool.Query(ctx, `SELECT `+pendingMessageCols+pendingMessageFrom+`
	       WHERE m.id = ANY($1) AND m.direction = 'inbound'
	       ORDER BY COALESCE(m.sent_at, m.created_at), m.id`, ids)
	if err != nil {
		return st, fmt.Errorf("load backfill messages: %w", err)
	}
	var msgs []pendingMessage
	for rows.Next() {
		pm, err := scanPendingMessage(rows)
		if err != nil {
			rows.Close()
			return st, err
		}
		msgs = append(msgs, pm)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, fmt.Errorf("iterate backfill messages: %w", err)
	}
	st.Named = len(ids)
	found := map[int64]bool{}
	for _, pm := range msgs {
		found[pm.msg.ID] = true
	}
	var missing []int64
	for _, id := range ids {
		if !found[id] {
			missing = append(missing, id)
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
	for _, id := range missing {
		st.Skipped++
		fmt.Fprintf(out, "msg %d  skipped: not an inbound message\n", id)
	}

	var sim simulatedRefs
	mode := RulesModeLive
	if dryRun {
		sim, mode = simulatedRefs{}, RulesModeShadow
	}
	for _, pm := range msgs {
		done, err := directBackfillDone(ctx, pool, pm.msg.ID)
		if err != nil {
			return st, err
		}
		if done {
			st.AlreadyDone++
			fmt.Fprintf(out, "msg %d  already backfilled\n", pm.msg.ID)
			continue
		}
		// Only a message the old path LOST: its live decision is `attributed`
		// (go-reviewer). A pending message is the live pass's to decide, and one
		// already decided task/task_log (e.g. by the fixed pass) is done —
		// backfilling either would log and mark it twice.
		//
		// swb #610 adds the other lost shape: a task_log onto a CLOSED task that
		// resurfaced to the inquiry lane (resurface=true). The log changed
		// nothing on the board, and for a project whose inquiry lane is not
		// armed (town-ai) nothing ever read it. Erica Rapa's Upwork messages.
		live, resurfaced, err := directBackfillLiveAction(ctx, pool, pm.msg.ID)
		if err != nil {
			return st, err
		}
		if live != actionAttributed && !(live == actionTaskLog && resurfaced) {
			st.Skipped++
			if live == "" {
				live = "none yet (pending)"
			}
			fmt.Fprintf(out, "msg %d  skipped: live decision is %s, not attributed or a resurfaced task_log\n", pm.msg.ID, live)
			continue
		}
		// Recovery (codex review): a previous run that died after create_task
		// left a task this message created, without its provenance or log.
		// The task BODY is the durable claim — it names the message id from
		// the create call itself — so finish that task instead of creating a
		// second conversation task.
		orphan, hasThread, err := directBackfillOrphan(ctx, pool, pm.msg.ID)
		if err != nil {
			return st, err
		}
		if orphan != 0 {
			st.AlreadyDone++
			if dryRun {
				fmt.Fprintf(out, "msg %d  would finish task %d a previous run created\n", pm.msg.ID, orphan)
				continue
			}
			if !hasThread {
				if err := setRuleProvenance(ctx, ex, DirectBackfillActor, orphan, pm); err != nil {
					return st, err
				}
			}
			conv, _ := directConversationKey(pm)
			if err := directBackfillLog(ctx, ex, pm, orphan, conv); err != nil {
				return st, err
			}
			if _, err := markRuleActivity(ctx, ex, DirectBackfillActor, pm, orphan, directSimSystem, conv); err != nil {
				return st, err
			}
			fmt.Fprintf(out, "msg %d  finished task %d a previous run created\n", pm.msg.ID, orphan)
			continue
		}
		d, winner, err := decideMessage(ctx, pool, mode, pm, pure, byID, sim)
		if err != nil {
			return st, err
		}
		if !d.direct || d.deferred {
			st.Skipped++
			fmt.Fprintf(out, "msg %d  skipped: not a person's DM (%s)\n", pm.msg.ID, d.reason)
			continue
		}
		switch d.action {
		case actionTask:
			st.Created++
			if dryRun {
				sim[directSimSystem+" "+d.directConv] = refTask{status: "ready"}
				fmt.Fprintf(out, "msg %d  would create the task for %s\n", pm.msg.ID, d.directConv)
				continue
			}
			taskID, err := createDirectTask(ctx, ex, DirectBackfillActor, pm, winner, d.directConv)
			if err != nil {
				return st, err
			}
			if err := setRuleProvenance(ctx, ex, DirectBackfillActor, taskID, pm); err != nil {
				return st, err
			}
			if err := directBackfillLog(ctx, ex, pm, taskID, d.directConv); err != nil {
				return st, err
			}
			if _, err := markRuleActivity(ctx, ex, DirectBackfillActor, pm, taskID, directSimSystem, d.directConv); err != nil {
				return st, err
			}
			fmt.Fprintf(out, "msg %d  created task %d for %s\n", pm.msg.ID, taskID, d.directConv)
		case actionTaskLog:
			st.Attached++
			if dryRun {
				target := "the task this run would create"
				if *d.taskID != 0 {
					target = fmt.Sprintf("task %d", *d.taskID)
				}
				fmt.Fprintf(out, "msg %d  would attach to %s (%s)\n", pm.msg.ID, target, d.directConv)
				continue
			}
			if err := directBackfillLog(ctx, ex, pm, *d.taskID, d.directConv); err != nil {
				return st, err
			}
			if _, err := markRuleActivity(ctx, ex, DirectBackfillActor, pm, *d.taskID, directSimSystem, d.directConv); err != nil {
				return st, err
			}
			fmt.Fprintf(out, "msg %d  attached to task %d (%s)\n", pm.msg.ID, *d.taskID, d.directConv)
		}
	}
	return st, nil
}

// directBackfillDone reports whether a backfill log line already names the
// message: the idempotency check, read from the marker the line carries.
func directBackfillDone(ctx context.Context, pool *pgxpool.Pool, messageID int64) (bool, error) {
	var n int
	// conv and channel carry no spaces, so the message id is pinned to its own
	// slot: a preview that quotes "message N from" cannot match.
	pattern := "^" + regexp.QuoteMeta(directBackfillMarker) + ` \S+ — \S+ message ` + fmt.Sprint(messageID) + ` from `
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_events WHERE event_type='log' AND payload->>'message' ~ $1`,
		pattern).Scan(&n); err != nil {
		return false, fmt.Errorf("check backfill of message %d: %w", messageID, err)
	}
	return n > 0, nil
}

// directBackfillLiveAction is the message's live capture decision action, ""
// when it has none.
func directBackfillLiveAction(ctx context.Context, pool *pgxpool.Pool, messageID int64) (string, bool, error) {
	var action string
	var resurface bool
	err := pool.QueryRow(ctx,
		`SELECT action, resurface FROM capture_decisions WHERE message_id=$1 AND mode='live' ORDER BY id DESC LIMIT 1`,
		messageID).Scan(&action, &resurface)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read live decision of message %d: %w", messageID, err)
	}
	return action, resurface, nil
}

// directBackfillOrphan finds a task a DM create_task made FROM this message
// (its body's `message_id: N` line under the DM marker) that carries no
// backfill log for it — the trace of a run that died between create_task and
// the log. hasThread reports whether its provenance was recorded.
func directBackfillOrphan(ctx context.Context, pool *pgxpool.Pool, messageID int64) (int64, bool, error) {
	var id int64
	var thread *int64
	err := pool.QueryRow(ctx,
		`SELECT id, source_thread_id FROM tasks
		  WHERE body LIKE $1 AND body ~ $2
		  ORDER BY id LIMIT 1`,
		directTaskBodyMarker+"%", `(^|\n)message_id: `+fmt.Sprint(messageID)+`\n`).Scan(&id, &thread)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("look for an unfinished backfill task for message %d: %w", messageID, err)
	}
	return id, thread != nil, nil
}

func directBackfillLog(ctx context.Context, ex *executor.Executor, pm pendingMessage, taskID int64, conv string) error {
	args, err := json.Marshal(map[string]any{
		"task_id": taskID,
		"kind":    "log",
		"message": fmt.Sprintf("%s %s — %s message %d from %s: %s",
			directBackfillMarker, conv, ruleOrNone(pm.channel), pm.msg.ID, ruleOrNone(pm.msg.Sender),
			textmatch.NormalizedPrefix(rulesPreview(pm.msg), rulesPreviewLen)),
	})
	if err != nil {
		return fmt.Errorf("marshal backfill log for message %d: %w", pm.msg.ID, err)
	}
	if _, err := ex.Execute(ctx, executor.Call{
		Tool: "task_append_log", Actor: DirectBackfillActor, Args: args, TaskID: &taskID,
	}); err != nil {
		return fmt.Errorf("log backfilled message %d on task %d: %w", pm.msg.ID, taskID, err)
	}
	return nil
}
