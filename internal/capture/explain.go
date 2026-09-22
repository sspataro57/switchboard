package capture

// comms-inbox (SWT-74 D6): "which task does this belong to?" answered by
// capture's OWN decision, read-only. task_match calls these; nothing else
// does. ExplainMessage loads a stored message with the SAME projection and
// scan the pass uses and runs decideMessage in shadow mode — the mode only
// words the reason, and shadow is the one that acts on nothing. It applies no
// direction filter and no horizon: the caller is asking a question, not
// running a pass, and an outbound message can be explained even though it can
// never become a comm (nothing is written, so nothing can be re-triaged).
//
// ExplainText is the "match a particular line" case: the pure matcher over a
// caller-built Message, its key resolved through taskForExternalRef. It cannot
// run the own-action guard or the PR-trust check (both need a stored message),
// and it SAYS so rather than pretending.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Explanation is one decision, as a value.
type Explanation struct {
	MessageID int64
	Action    string // unmatched | attributed | task | task_log | held
	Project   string
	RuleID    int64
	RuleKind  string
	System    string
	Key       string
	TaskID    int64  // the task the rule would file onto (action task_log)
	Reason    string // decideMessage's own reason string
	Deferred  bool
	// Direction is the stored message's direction ("" for ExplainText):
	// reported, never filtered, so a caller can see that an outbound message is
	// not a comm.
	Direction string
}

// ExplainMessage runs capture's decision over a stored message.
func ExplainMessage(ctx context.Context, pool *pgxpool.Pool, messageID int64) (Explanation, error) {
	// The pass's own projection and scan (criterion 21), with no direction
	// filter and no horizon: a question, not a pass.
	rows, err := pool.Query(ctx, `SELECT `+pendingMessageCols+pendingMessageFrom+`
	       WHERE m.id = $1`, messageID)
	if err != nil {
		return Explanation{}, fmt.Errorf("load message %d: %w", messageID, err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Explanation{}, fmt.Errorf("load message %d: %w", messageID, err)
		}
		return Explanation{}, fmt.Errorf("message %d: no such normalized message", messageID)
	}
	pm, err := scanPendingMessage(rows)
	if err != nil {
		return Explanation{}, err
	}
	rows.Close()
	var direction string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(direction,'') FROM normalized_messages WHERE id = $1`, messageID).
		Scan(&direction); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Explanation{}, fmt.Errorf("read direction of message %d: %w", messageID, err)
	}

	stored, err := loadRules(ctx, pool)
	if err != nil {
		return Explanation{}, err
	}
	rules, byID := rulesForEvaluate(stored)
	d, winner, err := decideMessage(ctx, pool, RulesModeShadow, pm, rules, byID, nil)
	if err != nil {
		return Explanation{}, err
	}
	out := explanationOf(pm.msg.ID, d, winner)
	out.Direction = direction
	if direction != "" && direction != "inbound" {
		out.Reason = fmt.Sprintf("message is %s (our own send re-entering; never a comm); ", direction) + out.Reason
	}
	return out, nil
}

// ExplainText evaluates the pure matcher over a caller-built Message and
// resolves the derived key through taskForExternalRef — the rule_ref proposal.
// Partial by construction: no own-action guard, no PR-trust check.
func ExplainText(ctx context.Context, pool *pgxpool.Pool, msg Message) (Explanation, error) {
	stored, err := loadRules(ctx, pool)
	if err != nil {
		return Explanation{}, err
	}
	rules, byID := rulesForEvaluate(stored)
	m := Evaluate(msg, rules)
	// A pasted line carries a subject/body and nothing else: sender,
	// thread_key_prefix/contains, source_slack_workspace and person rules can
	// never match it. Say so, or "unmatched" reads as a fact about the corpus.
	const pastedCaveat = "; partial: only subject/body criteria can match pasted text — sender, thread-key, " +
		"workspace and person rules need a stored message (pass message_id)"
	out := Explanation{MessageID: msg.ID, Action: actionUnmatched, Reason: "no enabled rule matched" + pastedCaveat}
	if m.Rule == nil {
		return out, nil
	}
	winner, ok := byID[m.Rule.ID]
	if !ok {
		return out, fmt.Errorf("matched rule %d is not in the loaded set", m.Rule.ID)
	}
	out.RuleID, out.RuleKind, out.Project = winner.rule.ID, winner.rule.Kind, winner.rule.Project
	out.Action = actionAttributed
	out.Reason = fmt.Sprintf("rule %d (%s) attributes to %s", winner.rule.ID, winner.rule.Kind, winner.rule.Project)
	if winner.extSystem == "" || m.ExternalKey == "" {
		out.Reason += "; no external key, so attribution only"
		return out, nil
	}
	out.System, out.Key = winner.extSystem, m.ExternalKey
	existing, found, err := taskForExternalRef(ctx, pool, winner.extSystem, m.ExternalKey)
	if err != nil {
		return out, err
	}
	if !found {
		out.Action = actionTask
		out.Reason += fmt.Sprintf("; %s %s has no task yet (a pass would create one)", winner.extSystem, m.ExternalKey)
	} else {
		out.Action = actionTaskLog
		out.TaskID = existing.taskID
		out.Reason += fmt.Sprintf("; %s %s already linked to task %d", winner.extSystem, m.ExternalKey, existing.taskID)
	}
	out.Reason += "; partial: the own-action guard and the PR-trust check need a stored message and did not run"
	return out, nil
}

// rulesForEvaluate is the loaded rules in Evaluate's shape plus the id index
// decideMessage takes.
func rulesForEvaluate(stored []storedRule) ([]Rule, map[int64]storedRule) {
	rules := make([]Rule, 0, len(stored))
	byID := make(map[int64]storedRule, len(stored))
	for _, s := range stored {
		rules = append(rules, s.rule)
		byID[s.rule.ID] = s
	}
	return rules, byID
}

func explanationOf(messageID int64, d ruleDecision, winner storedRule) Explanation {
	out := Explanation{MessageID: messageID, Action: d.action, Reason: d.reason, Deferred: d.deferred}
	if d.matchedRuleID != nil {
		out.RuleID = *d.matchedRuleID
		out.RuleKind = winner.rule.Kind
		out.Project = winner.rule.Project
	}
	if d.extSystem != nil {
		out.System = *d.extSystem
	}
	if d.extKey != nil {
		out.Key = *d.extKey
	}
	if d.taskID != nil {
		out.TaskID = *d.taskID
	}
	return out
}
