package capture

// The I/O half of the SWT-78 DM conversation-task path (direct.go is the pure
// predicate). decideDirect finds the conversation's open task; actDirect makes
// the executor calls a live pass owes the decision. Reads only here: every
// write is a tool call (rules_structure_test.go criterion 16).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/textmatch"
)

// directSimSystem keys DryRunRules' simulated conversation tasks in
// simulatedRefs, beside the external refs a dry run "creates". Not an
// external_refs system: the DM path writes no ref.
const directSimSystem = "slack-dm"

// directTaskBodyMarker opens every DM task's body. The backfill's recovery
// reads it (with the body's message_id line) to find a task an interrupted run
// created, so the body is the durable claim.
const directTaskBodyMarker = "Captured deterministically: a Slack DM is always actionable (SWT-78)"

// directFacts builds directConversationTask's input from the message and the
// winning rule, through the one spelling of each fact.
func directFacts(pm pendingMessage, winner storedRule) directInput {
	return directInput{
		slack:       pm.channel == slackweb.Channel,
		upwork:      pm.channel == upworkChannel,
		dm:          slackweb.IsDirectMessageKey(pm.msg.ThreadKey),
		groupDM:     pm.rawConvType == "group_dm",
		blankSender: blankSender(pm.msg.Sender),
		notifier:    notifierSender(pm.msg.Sender, winner.notifiers),
	}
}

// directConversationSQL finds the DM conversation's OPEN task (SWT-78
// decisions 1, 2 and 4): a HUMAN task (C-D13: untrusted Slack text never logs
// onto a claude task, whose log feeds a worker prompt) with NO external_refs
// row (a ticket task belongs to its ticket, not to the chat it was raised
// from), in the rule's project, whose source thread is ANY thread of the
// conversation — the conversation key itself or one of its rooted threads.
// "Open" is status NOT IN ('closed','delivered'), the one spelling
// (promote.open, threadTask): a dismissed task is closed, so the next DM makes
// a new one. Oldest first, threadTask's ordering.
//
// The second arm (codex review) finds a DM task whose provenance was never
// recorded — a pass that died between create_task and task_set_source_thread.
// $4 (exact) turns off the rooted-thread fold for an Upwork conversation: its
// key is used whole, and a room id may contain ':', so "A:B" must never fold
// into "A".
//
// Its BODY names the conversation (directTaskArgs' `conversation:` line under
// directTaskBodyMarker), so it is still this conversation's task: the next DM
// attaches to it (and actDirect records the missing provenance) instead of
// opening a second one.
const directConversationSQL = `
	SELECT t.id, t.source_thread_id IS NULL
	  FROM tasks t
	  LEFT JOIN normalized_threads nt ON nt.id = t.source_thread_id
	 WHERE ((nt.thread_key = $1 OR (NOT $4 AND left(nt.thread_key, length($1) + 1) = $1 || ':'))
	        OR (t.source_thread_id IS NULL AND left(t.body, length($3)) = $3
	            AND strpos(t.body, E'\nconversation: ' || $1 || E'\n') > 0))
	   AND t.project_id = $2
	   AND t.status NOT IN ('closed','delivered')
	   AND t.assignee_type = 'human'
	   AND NOT EXISTS (SELECT 1 FROM external_refs r WHERE r.task_id = t.id)
	 ORDER BY t.created_at, t.id
	 LIMIT 1`

// directConversationKey is the unit "one task per conversation" counts in. A
// Slack DM's conversation is its channel key with any rooted thread folded in
// (slackweb.ConversationThreadKey). An Upwork message's thread IS its
// conversation (one thread per room): its thread key, used whole and never
// parsed, so the Upwork key format keeps its one spelling in the connector.
func directConversationKey(pm pendingMessage) (string, bool) {
	if pm.channel == upworkChannel {
		return pm.msg.ThreadKey, strings.TrimSpace(pm.msg.ThreadKey) != ""
	}
	return slackweb.ConversationThreadKey(pm.msg.ThreadKey)
}

// decideDirect turns base (the attribution: project, rule, matched ids) into
// the DM decision: task_log onto the conversation's open task, else task. The
// reason keeps base's text and adds why, then what a live pass requests or a
// shadow pass would do (SWT-45 criterion 25's wording rule).
func decideDirect(ctx context.Context, pool *pgxpool.Pool, mode string, pm pendingMessage,
	base ruleDecision, winner storedRule, sim simulatedRefs, why string) (ruleDecision, storedRule, error) {
	conv, ok := directConversationKey(pm)
	if !ok {
		// A DM whose thread key does not parse cannot be counted into a
		// conversation; keep the attribution rather than guess one.
		base.reason += "; DM task skipped: thread key does not name a Slack conversation"
		return base, winner, nil
	}
	d := base
	d.direct, d.directConv = true, conv
	d.extSystem, d.extKey, d.taskID = nil, nil, nil
	d.resurface, d.comm, d.revive, d.surface, d.dismissalID = false, false, false, false, 0

	var taskID int64
	found := false
	if sim != nil {
		if rt, hit := sim.lookup(directSimSystem, conv); hit {
			taskID, found = rt.taskID, true
		}
	}
	if !found {
		err := pool.QueryRow(ctx, directConversationSQL, conv, winner.projectID, directTaskBodyMarker,
			pm.channel == upworkChannel).
			Scan(&taskID, &d.directNoThread)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return d, winner, fmt.Errorf("resolve open task for DM conversation %s: %w", conv, err)
		default:
			found = true
		}
	}
	verb := func(live, shadow string) string {
		if mode == RulesModeLive {
			return live
		}
		return shadow
	}
	if found {
		d.action = actionTaskLog
		d.taskID = &taskID
		d.reason += fmt.Sprintf("; %s; conversation %s has open task %d; %s", why, conv, taskID,
			verb("append requested", "would append"))
		return d, winner, nil
	}
	d.action = actionTask
	d.reason += fmt.Sprintf("; %s; conversation %s has no open task; %s", why, conv,
		verb("create requested", "would create one"))
	return d, winner, nil
}

// actDirect makes the executor calls a live DM decision owes, in the rule
// create branch's order: create → record the task on the decision (the claim
// is spent; a later failure must not lose the pointer) → provenance → the
// activity mark that puts it in INCOMING. An attach logs, then marks. No
// external_refs row, ever: the conversation is found by its thread, and a ref
// would make "closed → a new task" impossible (one key, one task, forever).
func actDirect(ctx context.Context, pool *pgxpool.Pool, ex *executor.Executor, actor string,
	decisionID int64, pm pendingMessage, winner storedRule, d ruleDecision, stats *RulesStats) error {
	switch d.action {
	case actionTask:
		taskID, err := createDirectTask(ctx, ex, actor, pm, winner, d.directConv)
		if err != nil {
			return err
		}
		if err := recordDecisionTask(ctx, pool, decisionID, taskID); err != nil {
			return err
		}
		if err := setRuleProvenance(ctx, ex, actor, taskID, pm); err != nil {
			return err
		}
		stats.TasksCreated++
		marked, err := markRuleActivity(ctx, ex, actor, pm, taskID, directSimSystem, d.directConv)
		if err != nil {
			return err
		}
		if marked {
			stats.Activity++
		}
	case actionTaskLog:
		if d.directNoThread {
			// Heal an interrupted create (see directConversationSQL).
			if err := setRuleProvenance(ctx, ex, actor, *d.taskID, pm); err != nil {
				return err
			}
		}
		if err := appendRuleLog(ctx, ex, actor, pm, *d.taskID, directSimSystem, d.directConv); err != nil {
			return err
		}
		stats.Appended++
		marked, err := markRuleActivity(ctx, ex, actor, pm, *d.taskID, directSimSystem, d.directConv)
		if err != nil {
			return err
		}
		if marked {
			stats.Activity++
		}
	default:
		return fmt.Errorf("DM decision for message %d has action %q; want task or task_log", pm.msg.ID, d.action)
	}
	return nil
}

// directTaskArgs is create_task's argument object for a DM conversation task:
// `{sender}: {first line}` (commTaskTitle's shape) and an ids-only body — no
// generated text, the message's own words only as the bounded preview.
func directTaskArgs(pm pendingMessage, winner storedRule, conv string) map[string]any {
	head := ruleFirstLine(pm.msg.BodyText)
	label := strings.TrimSpace(pm.msg.Sender)
	if label == "" {
		label = winner.rule.Project
	}
	title := label
	if head != "" {
		title = label + ": " + head
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s, via capture rule %d (%s %q).\n\n", directTaskBodyMarker,
		winner.rule.ID, winner.rule.Kind, winner.rule.Pattern)
	fmt.Fprintf(&b, "conversation: %s\n", conv)
	fmt.Fprintf(&b, "channel: %s\n", ruleOrNone(pm.channel))
	fmt.Fprintf(&b, "thread_key: %s\n", ruleOrNone(pm.msg.ThreadKey))
	fmt.Fprintf(&b, "sender: %s\n", ruleOrNone(pm.msg.Sender))
	fmt.Fprintf(&b, "sent_at: %s\n", pm.sentAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "message_id: %d\n", pm.msg.ID)
	if pm.msg.ExternalMessageID != "" {
		fmt.Fprintf(&b, "external_message_id: %s\n", pm.msg.ExternalMessageID)
	}
	fmt.Fprintf(&b, "\n%s", textmatch.NormalizedPrefix(rulesPreview(pm.msg), rulesPreviewLen))
	return map[string]any{
		"project":       winner.rule.Project,
		"subproject":    winner.subproject,
		"title":         textmatch.NormalizedPrefix(title, rulesTitleLen),
		"body":          b.String(),
		"assignee_type": "human",
		"priority":      0,
	}
}

func createDirectTask(ctx context.Context, ex *executor.Executor, actor string,
	pm pendingMessage, winner storedRule, conv string) (int64, error) {
	args, err := json.Marshal(directTaskArgs(pm, winner, conv))
	if err != nil {
		return 0, fmt.Errorf("marshal create_task args for DM message %d: %w", pm.msg.ID, err)
	}
	res, err := ex.Execute(ctx, executor.Call{Tool: "create_task", Actor: actor, Args: args})
	if err != nil {
		return 0, fmt.Errorf("create DM task for %s (message %d): %w", conv, pm.msg.ID, err)
	}
	var out struct {
		TaskID int64 `json:"task_id"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		return 0, fmt.Errorf("parse create_task result for DM %s: %w", conv, err)
	}
	if out.TaskID == 0 {
		return 0, fmt.Errorf("create_task returned no task id for DM %s", conv)
	}
	return out.TaskID, nil
}
