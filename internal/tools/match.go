package tools

// task_match (SWT-74 D6): "which task does this belong to?" — capture's OWN
// decision, run read-only, over a stored message, a comm task (its activity
// message) or a pasted line. It proposes; task_attach acts. Not humanOnly: a
// worker console may ask, it cannot act on the answer.
//
// Two evidence sources, in rank order, deduped by task id:
//   - rule_ref (0): the matching rule's own external-ref resolution — the task
//     a capture pass would file the message onto;
//   - source_thread (1): open tasks whose source_thread_id is the message's
//     thread, oldest first (threadTask's shape, deliberately NOT project-scoped:
//     a thread is a conversation and the human decides).
//
// No recency source: "the ten most recent open tasks" is a list that looks
// like matches, and task_list already returns it. Titles only, never bodies.
//
// {task_id} resolves ONLY through tasks.activity_by_message_id — the message
// capture marked the comm with. A task without one is refused by name, never
// guessed from its thread: a guess answers a different question.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/capture"
)

const (
	matchLimitDefault = 5
	matchLimitMax     = 20
)

type matchArgs struct {
	MessageID int64  `json:"message_id,omitempty"`
	TaskID    int64  `json:"task_id,omitempty"`
	Text      string `json:"text,omitempty"`
	Project   string `json:"project,omitempty"`
	Limit     *int   `json:"limit,omitempty"`
}

// hasText reports a non-blank text input; a blank one is the pasted-line case
// with nothing pasted, and "no rule matched" for it would be a lie.
func (a matchArgs) hasText() bool { return strings.TrimSpace(a.Text) != "" }

func validateMatch(args []byte) error {
	var a matchArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	given := 0
	if a.MessageID != 0 {
		given++
	}
	if a.TaskID != 0 {
		given++
	}
	if a.Text != "" {
		given++
	}
	if given != 1 {
		return fmt.Errorf("pass exactly one of message_id, task_id or text (got %d)", given)
	}
	if a.MessageID < 0 || a.TaskID < 0 {
		return errors.New("message_id / task_id must be > 0 (and text non-empty): pass exactly one of message_id, task_id, text")
	}
	if a.Text != "" && !a.hasText() {
		return errors.New("text is blank: pass a non-empty text (or message_id / task_id instead)")
	}
	if a.Limit != nil && (*a.Limit < 1 || *a.Limit > matchLimitMax) {
		return fmt.Errorf("limit %d: must be 1..%d", *a.Limit, matchLimitMax)
	}
	return nil
}

// matchProposal is one candidate task, titles only.
type matchProposal struct {
	TaskID         int64  `json:"task_id"`
	Title          string `json:"title"`
	Status         string `json:"status"`
	AssigneeType   string `json:"assignee_type"`
	Project        string `json:"project"`
	Source         string `json:"source"`
	RuleID         int64  `json:"rule_id"`
	RuleKind       string `json:"rule_kind"`
	ExternalSystem string `json:"external_system"`
	ExternalKey    string `json:"external_key"`
	Partial        bool   `json:"partial"`
	Why            string `json:"why"`
}

func matchTask(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a matchArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	limit := matchLimitDefault
	if a.Limit != nil {
		limit = *a.Limit
	}

	var (
		ex       capture.Explanation
		err      error
		input    string
		partial  bool
		threadID *int64
	)
	switch {
	case a.hasText():
		input = "text"
		partial = true
		ex, err = capture.ExplainText(ctx, pool, capture.Message{BodyText: a.Text, Subject: firstLine(a.Text)})
	default:
		messageID := a.MessageID
		input = "message_id"
		if a.TaskID != 0 {
			input = "task_id"
			var by *int64
			if err := pool.QueryRow(ctx, `SELECT activity_by_message_id FROM tasks WHERE id = $1`, a.TaskID).Scan(&by); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil, fmt.Errorf("task %d not found", a.TaskID)
				}
				return nil, fmt.Errorf("read task %d: %w", a.TaskID, err)
			}
			if by == nil {
				return nil, fmt.Errorf("task %d carries no activity message; pass message_id or text instead", a.TaskID)
			}
			messageID = *by
		}
		ex, err = capture.ExplainMessage(ctx, pool, messageID)
		if err == nil {
			// ExplainMessage just proved the row exists; a failure here is a
			// database error, not a missing message.
			if terr := pool.QueryRow(ctx, `SELECT thread_id FROM normalized_messages WHERE id = $1`, messageID).
				Scan(&threadID); terr != nil {
				err = fmt.Errorf("read the thread of message %d: %w", messageID, terr)
			}
		}
	}
	if err != nil {
		return nil, err
	}

	var proposals []matchProposal
	seen := map[int64]bool{}
	add := func(id int64, source, why string) error {
		if seen[id] {
			return nil
		}
		var p matchProposal
		if err := pool.QueryRow(ctx,
			`SELECT t.id, t.title, t.status, t.assignee_type, COALESCE(pr.slug,'')
			   FROM tasks t LEFT JOIN projects pr ON pr.id = t.project_id WHERE t.id = $1`, id).
			Scan(&p.TaskID, &p.Title, &p.Status, &p.AssigneeType, &p.Project); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("read task %d: %w", id, err)
		}
		if a.Project != "" && p.Project != a.Project {
			return nil
		}
		p.Source, p.Why, p.Partial = source, why, partial
		// The rule fields belong to the rule_ref proposal only: a source_thread
		// row stamped with rule 75 would read as "rule 75 pointed here".
		if source == "rule_ref" {
			p.RuleID, p.RuleKind, p.ExternalSystem, p.ExternalKey = ex.RuleID, ex.RuleKind, ex.System, ex.Key
		}
		seen[id] = true
		proposals = append(proposals, p)
		return nil
	}
	// rule_ref ranks 0: the rule's own external-ref resolution.
	if ex.TaskID != 0 {
		if err := add(ex.TaskID, "rule_ref", ex.Reason); err != nil {
			return nil, err
		}
	}
	// source_thread ranks 1: open tasks on the message's thread, oldest first.
	if threadID != nil {
		rows, err := pool.Query(ctx,
			`SELECT id FROM tasks WHERE source_thread_id = $1 AND status <> 'closed' ORDER BY id`, *threadID)
		if err != nil {
			return nil, fmt.Errorf("read the thread's open tasks: %w", err)
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan thread task: %w", err)
			}
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			if len(proposals) >= limit {
				break
			}
			if err := add(id, "source_thread",
				fmt.Sprintf("open task on the message's thread (source_thread_id %d)", *threadID)); err != nil {
				return nil, err
			}
		}
	}
	if len(proposals) > limit {
		proposals = proposals[:limit]
	}
	if proposals == nil {
		proposals = []matchProposal{}
	}
	return marshalResult(map[string]any{
		"input": input, "matched": len(proposals) > 0, "reason": ex.Reason, "proposals": proposals,
	})
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
