package promote

// The inquiry lane's promotion (SWT-40 Part C, docs/tickets/inquiry-promote_SPEC.md,
// C-D2..C-D10): stored classify_inquiry verdicts that pass a deterministic gate
// become Holding tasks, through the same claim-before-act driver as the
// personal lane. The gate is pure (invariant 7); the inbox is one query; the
// replied-since fold is internal/replyfold's, the DM rule slackweb's. Nothing
// here calls a model, creates a delivery or reaches a send path.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/replyfold"
	"github.com/sspataro57/switchboard/internal/textmatch"
)

// Lane names the classify lane a promotion pass reads (the `--lane` spellings,
// E5). Config's zero value means personal, so the classify-promote CronJob and
// every existing caller are unchanged (C1).
type Lane string

const (
	LanePersonal Lane = "personal"
	LaneInquiry  Lane = "inquiry"
)

// InquiryActor is the identity on every inquiry-lane executor call (C-D1).
const InquiryActor = "promote:inquiry"

const (
	// InquiryMaxAge is C-D6's second fence behind the verdict clock: an ask
	// sent longer ago than this never becomes a task (SWT-30's backfill
	// residual). The inbox applies it on sent_at and the gate again.
	InquiryMaxAge = 72 * time.Hour
	// InquiryGrace is C-D6's grace on sent_at: an ask younger than this is
	// `pending`, so the replied-since fold can fire first. The pipelined sweep
	// (5 min) bounds how late a pending verdict releases.
	//
	// ZERO since 2026-09-18 (Salvador: "remove the 30 min ... so comms become
	// tasks as soon as they land and we can check for replies and remove them
	// from the board if so"). An hour, then 30 minutes, now no wait at all: an
	// ask becomes a task on the first pass that sees it.
	//
	// What this does NOT remove is the replied-since fold. GateAnswered still
	// runs, so an ask he has already answered is still refused — it simply
	// almost never fires now, because nothing waits long enough for a reply to
	// land first. The board is meant to be corrected AFTER the fact instead:
	// tasks whose thread has since been answered get closed, rather than never
	// being created.
	//
	// Zero still holds the clock-skew case. A future sent_at gives a NEGATIVE
	// age, and negative is less than zero, so such a message stays pending
	// rather than promoting on a timestamp that has not happened yet.
	InquiryGrace = 0
)

// inquiryCreateStatus is the status a NEW inquiry task is created with — the
// inquiry lane's autonomy argument, O7 (2026-09-11, "A is ok"): Holding first
// (promotion action 'review'), then ready once the rate was seen.
//
// FLIPPED to "ready" (action 'task') by Salvador on 2026-09-18, "remove the
// holding. the cadence is enought". The readout at the moment of the flip was
// 14 true positives, 2 false positives, 16 decided — a seventh of the 120 the
// design asked for, so this is his judgement on an INDICATIVE rate, not a
// measured one. Both false positives share one cause, recorded as swb 386:
// addressed() treats every gmail message as addressed to him, so a thread he is
// merely copied on can promote. Expect those to land in the working queue now
// rather than in holding. A Go constant, not a column or an env var, for
// SWT-30 D2's reason: a typo cannot widen it unreviewed.
//
// THE FLIP to "ready" (action 'task') is a deliberate one-line change that
// Salvador makes after about two weeks of `classify promote --outcomes`, and
// it edits inquiry_internal_test.go's pin and inquiry_test.go's assertion in
// the same diff. It changes only tasks promoted after the deploy.
const inquiryCreateStatus = "ready"

// actionForStatus maps a create status to its classify_promotions action: the
// action follows the status, never the other way round.
func actionForStatus(status string) string {
	if status == "ready" {
		return "task"
	}
	return "review"
}

// actorFor is the executor identity for a lane's calls.
func actorFor(l Lane) string {
	if l == LaneInquiry {
		return InquiryActor
	}
	return Actor
}

// inquiryAskKinds is C-D5's whitelist. `fyi` asks nothing, and excluding it
// also drops SWT-33's fyi+needs_reply contradictions.
var inquiryAskKinds = map[string]bool{
	"question":   true,
	"request":    true,
	"decision":   true,
	"scheduling": true,
}

// gmailChannel is the channel the google normalizer writes; C-D3 addresses a
// mail to Salvador by its channel alone.
const gmailChannel = "gmail"

// The gate reasons, in C3's order: the FIRST failing clause is the one
// reported, so stats and the dry-run count every gated verdict exactly once.
const (
	GateRethreaded   = "rethreaded"
	GateKind         = "kind"
	GateStale        = "stale"
	GatePending      = "pending"
	GateAnswered     = "answered"
	GateNotAddressed = "not_addressed"
	// GateClaudeTask is C-D13: the thread's open or dismissed task is not a
	// human's. Last in the order because it needs the thread task, which is
	// read only for a verdict that passed the other six.
	GateClaudeTask = "claude_task"
)

// InquiryGateReasons lists every gate reason in C3's order, then C-D13's.
func InquiryGateReasons() []string {
	return []string{GateRethreaded, GateKind, GateStale, GatePending, GateAnswered, GateNotAddressed, GateClaudeTask}
}

// InquiryCandidate is what the gate reads: stored verdict facts, the two
// replyfold columns and the message's current thread.
type InquiryCandidate struct {
	AskKind         string    // fields.ask_kind
	Channel         string    // fields.channel
	ThreadKey       string    // fields.thread_key, stored verbatim
	ThreadScope     string    // fields.thread_scope
	StoredThreadID  int64     // fields.thread_id (0 = none)
	CurrentThreadID int64     // normalized_messages.thread_id now (0 = none)
	SentAt          time.Time // normalized_messages.sent_at
	RepliedSince    bool      // replyfold.RepliedSinceCol
	PriorPost       bool      // replyfold.PriorParticipationCol
	// Mentioned (SWT-79 D4): slackweb.MentionsOwner over the message body,
	// computed at the scan. Read only by addressed(); the body never reaches a
	// title, a body or a log.
	Mentioned  bool
	MaxAge     time.Duration // 0 = InquiryMaxAge; only a dry-run may widen it
	ThreadTask *ExistingTask // threadTask's open-or-dismissed result (nil = none or not yet read)
}

// InquiryGate returns "" when the verdict may promote, else the FIRST failing
// reason in C3's order: rethreaded, kind, stale, pending, answered,
// not_addressed, then C-D13's claude_task. Pure: a function of its inputs, no
// I/O (invariant 7).
func InquiryGate(c InquiryCandidate, now time.Time) string {
	// C-D10: the thread the verdict was classified on is no longer the
	// message's thread; its replied-since and addressing facts describe the
	// wrong conversation.
	if c.StoredThreadID != c.CurrentThreadID {
		return GateRethreaded
	}
	if !inquiryAskKinds[c.AskKind] {
		return GateKind
	}
	maxAge := c.MaxAge
	if maxAge <= 0 {
		maxAge = InquiryMaxAge
	}
	age := now.Sub(c.SentAt)
	if age > maxAge {
		return GateStale
	}
	// A future sent_at (clock skew) is pending too: negative age is inside the
	// grace, never past it.
	if age < InquiryGrace {
		return GatePending
	}
	// C-D7: ANY replied-since state blocks promotion (answered in thread, or
	// spoke in the conversation since).
	if c.RepliedSince {
		return GateAnswered
	}
	if !addressed(c) {
		return GateNotAddressed
	}
	// C-D13, narrowed by SWT-72 D11: never attach to or reopen a task that is
	// not a human's. An attach would log a summary of untrusted mail or Slack
	// text onto a claude task, whose log feeds a worker prompt (SWT-38 pinned
	// the log verb to human tasks for this reason); a reopen would put it back
	// in a console queue. Since D11 an ask with an OPEN thread task is its own
	// task (ids-only pointer on the old one), so the gate bites only where
	// Decide would still ATTACH — rule 2's dismissed path — spelled as one call
	// to the pure Decide so "would attach" has one spelling. An empty assignee
	// reads as not human: fail closed.
	if c.ThreadTask != nil && c.ThreadTask.AssigneeType != "human" &&
		Decide(Verdict{Lane: LaneInquiry, Kind: c.AskKind}, c.ThreadTask).Action == "attached" {
		return GateClaudeTask
	}
	return ""
}

// addressed is C-D3, a deterministic spine rule and never a model field:
// gmail, OR a 1:1 Slack DM, OR a thread-scoped key he posted on strictly
// before the ask. A top-level channel message is NOT addressed even after he
// spoke there (conversation scope): the accepted recall cost.
func addressed(c InquiryCandidate) bool {
	switch {
	case c.Channel == gmailChannel:
		return true
	case slackweb.IsDirectMessageKey(c.ThreadKey):
		return true
	case c.Channel == slackweb.Channel && c.Mentioned:
		// SWT-79 D4: "we only respond to mentions on those channels" — a
		// channel message that @-mentions him is addressed to him, top-level
		// or in a thread he has not posted in. The thread rule below stays for
		// the paths this ticket leaves alone (resurface, gate/route rows).
		return true
	default:
		return c.ThreadScope == replyfold.ScopeThread && c.PriorPost
	}
}

// inquiryTitle is "{sender}: {ask}" (swb 384 / SWT-87). The name is the
// message's STORED sender, never the model's asker: on José's email (message
// 341859) the model named Katie, who was only quoted inside it, and the board
// sent attention to the wrong colleague. The asker stays in the body as the
// model's reading. The sender is shortened to its display name (a mail From
// "José Garcia <jose.g@avviato.com>" reads "José Garcia"). An empty ask falls
// back to the subject; with neither, the title is the sender alone, never a
// dangling "Name: ".
func inquiryTitle(sender, subject, ask string) string {
	who := senderLabel(sender)
	what := strings.TrimSpace(ask)
	if what == "" {
		what = strings.TrimSpace(subject)
	}
	if what == "" {
		return textmatch.NormalizedPrefix(who, titleLimit)
	}
	return textmatch.NormalizedPrefix(who+": "+what, titleLimit)
}

// senderLabel is a stored sender as a person's name: a mail From's display
// name, else its address; any other sender (a Slack display name) as it is;
// "(unknown sender)" when blank.
func senderLabel(sender string) string {
	s := strings.TrimSpace(sender)
	if s == "" {
		return "(unknown sender)"
	}
	if a, err := mail.ParseAddress(s); err == nil {
		if n := strings.TrimSpace(a.Name); n != "" {
			return n
		}
		return a.Address
	}
	return s
}

// inquiryBody is C-D9's deterministic body: one `key: value` line each, in a
// fixed order, "(none)" for an empty value. Every value is a copy of a stored
// verdict field or a column, never generated. No permalink: it exists only in
// raw_json. relatedTaskID (SWT-72 D11) is the thread's open task the ask
// would have been attached to, the LAST line of the fixed list.
func inquiryBody(v Verdict, relatedTaskID int64) string {
	sentAt := ""
	if v.SentAt != nil {
		sentAt = v.SentAt.UTC().Format(time.RFC3339)
	}
	storedThread := ""
	if v.StoredThreadID != 0 {
		storedThread = strconv.FormatInt(v.StoredThreadID, 10)
	}
	related := ""
	if relatedTaskID != 0 {
		related = strconv.FormatInt(relatedTaskID, 10)
	}
	var b strings.Builder
	for _, kv := range [][2]string{
		{"ask_kind", v.Kind},
		{"asker", v.Asker},
		{"sender", v.Sender},
		{"channel", v.Channel},
		{"subject", v.Subject},
		{"sent_at", sentAt},
		{"normalized_message_id", strconv.FormatInt(v.MessageID, 10)},
		{"ai_extraction_id", strconv.FormatInt(v.ExtractionID, 10)},
		{"thread_id", storedThread},
		{"thread_key", v.ThreadKey},
		{"thread_scope", v.ThreadScope},
		{"external_message_id", v.ExternalMessageID},
		{"verdict", v.Reason},
		{"related_task", related},
	} {
		fmt.Fprintf(&b, "%s: %s\n", kv[0], orNone(kv[1]))
	}
	// chat-on-closed-task CC6: one line, LAST, only when set, so every other
	// body stays byte-identical.
	if v.LoggedOnTaskID != 0 {
		fmt.Fprintf(&b, "logged_on_closed_task: %d\n", v.LoggedOnTaskID)
	}
	return b.String()
}

// bodyFor is the lane's task body. relatedTaskID is the inquiry lane's
// (Decision.RelatedTaskID); the personal lane ignores it.
func bodyFor(v Verdict, relatedTaskID int64) string {
	if v.Lane == LaneInquiry {
		return inquiryBody(v, relatedTaskID)
	}
	return taskBody(v)
}

// inquiryRow is one inbox row: the verdict the executor calls copy from, and
// the candidate the gate reads.
type inquiryRow struct {
	v Verdict
	c InquiryCandidate
}

// runInquiry is one inquiry-lane pass, under the lock Run already holds.
//
// LIMIT BOUNDS THE VERDICTS ACTED ON, never the rows read: a gated verdict
// writes no row and stays in the inbox, so a limit over rows read would let
// older gated verdicts fill every pass and starve newer passing ones until
// they aged out at 72h. The inbox is bounded anyway (armed projects, 72h).
func runInquiry(ctx context.Context, pool *pgxpool.Pool, ex *executor.Executor, cfg Config) (Stats, error) {
	stats := Stats{Gated: map[string]int{}}
	for _, r := range InquiryGateReasons() {
		stats.Gated[r] = 0
	}

	// C-D2: OFF until a human sets the lane's own cutover. Checked explicitly
	// so the answer is a sentence rather than a silent empty inbox.
	var armed int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM projects WHERE inquiry_promote_after IS NOT NULL`).Scan(&armed); err != nil {
		return stats, fmt.Errorf("promote: count inquiry-armed projects: %w", err)
	}
	if armed == 0 {
		slog.Info("promote: no project has an inquiry cutover set; inquiry promotion is off everywhere " +
			"(UPDATE projects SET inquiry_promote_after = ... to arm one — see docs/runbooks/local-classifier.md)")
		return stats, nil
	}

	maxAge := cfg.MaxAge
	if maxAge <= 0 {
		maxAge = InquiryMaxAge
	}
	// The DB clock, the one the inbox's age fence and every sent_at share.
	var now time.Time
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return stats, fmt.Errorf("promote: read the db clock: %w", err)
	}
	rows, err := inquiryInbox(ctx, pool, maxAge)
	if err != nil {
		return stats, err
	}

	acted := 0
	for _, row := range rows {
		if cfg.Limit > 0 && acted >= cfg.Limit {
			break
		}
		v, c := row.v, row.c
		c.MaxAge = cfg.MaxAge
		// C-D8: gate first. A gated verdict writes no row and calls no tool;
		// it is counted by reason and stays in the inbox (a pending one
		// releases on the first pass or sweep after its grace). The thread
		// task is read only for a verdict that passes the six C3 clauses,
		// then the gate runs again with it for C-D13 (claude_task, last).
		gated := InquiryGate(c, now)
		var existing, finished *ExistingTask
		if gated == "" {
			var err error
			existing, finished, err = threadTask(ctx, pool, v.ThreadID, v.ProjectID)
			if err != nil {
				return stats, err
			}
			c.ThreadTask = existing
			gated = InquiryGate(c, now)
		}
		if gated != "" {
			stats.Gated[gated]++
			if cfg.DryRun {
				slog.Info("promote dry-run", "lane", string(LaneInquiry), "message", v.MessageID,
					"kind", v.Kind, "gated", gated, "project", v.ProjectSlug)
			}
			continue
		}
		acted++

		d := Decide(v, existing)
		reason := decisionReason(v, d, existing, finished)

		if cfg.DryRun {
			stats.Considered++
			slog.Info("promote dry-run", "lane", string(LaneInquiry), "message", v.MessageID, "kind", v.Kind,
				"action", d.Action, "status", d.Status, "attach_task", d.TaskID,
				"reopen_dismissal", d.ReopenDismissalID,
				"project", v.ProjectSlug, "title", v.Title, "reason", reason)
			count(&stats, d)
			continue
		}
		if err := act(ctx, pool, ex, v, d, reason, &stats); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// inquiryInbox is C2, one query, oldest verdict first:
//
//   - an ok classify_inquiry verdict whose fields say needs_reply;
//   - on an INBOUND message;
//   - whose LATEST capture decision, in ANY mode (live, shadow, gate, route),
//     passes replyfold.InquiryEligibleLatestSQL: `attributed`, or the latest
//     LIVE decision is a task_log capture recorded with resurface=true onto a
//     task that is STILL closed (chat-on-closed-task CC5; that branch reads
//     only live decisions, CC5b; the project then comes from that live
//     row, replyfold.InquiryProjectIDSQL). Any other
//     task/task_log already produced a task or logs on an open one, held is
//     still pending its gate, unmatched names no project. No mode predicate on
//     WHICH row is latest: a gate resolution or a route row IS the message's
//     current attribution;
//   - to a project with ai_inquiry AND inquiry_promote_after set AND the
//     verdict recorded at or after it (forward-only on the verdict clock);
//   - sent within the age fence (InquiryMaxAge, or a dry-run's --max-age);
//   - with no promotion row (either lane's) for the message.
//
// The two replyfold columns ride along from the one set-based join.
func inquiryInbox(ctx context.Context, pool *pgxpool.Pool, maxAge time.Duration) ([]inquiryRow, error) {
	q := `
	SELECT e.id, e.raw_source_item_id, e.fields,
	       nm.id, nm.thread_id, nm.sent_at, r.created_at,
	       p.id, p.slug,
	       ` + replyfold.RepliedSinceCol + `, ` + replyfold.PriorParticipationCol + `,
	       ` + replyfold.InquiryLoggedOnTaskSQL + `,
	       COALESCE(nm.body_text, '')
	  FROM ai_extractions e
	  JOIN ai_runs r ON r.id = e.ai_run_id
	       AND r.worker_type = 'classify_inquiry' AND r.status = 'ok'
	  JOIN normalized_messages nm ON nm.raw_source_item_id = e.raw_source_item_id
	       AND nm.direction = 'inbound'
	  JOIN LATERAL (SELECT cd.action, cd.project_id, cd.channel_unmentioned FROM capture_decisions cd
	                 WHERE cd.message_id = nm.id
	                 ORDER BY cd.id DESC LIMIT 1) latest ON true` + replyfold.InquiryLiveDecisionJoinSQL + `
	  JOIN projects p ON p.id = ` + replyfold.InquiryProjectIDSQL + `
	       AND p.ai_inquiry
	       AND p.inquiry_promote_after IS NOT NULL
	       AND r.created_at >= p.inquiry_promote_after` + replyfold.JoinSQL + `
	 WHERE e.fields->>'needs_reply' = 'true'
	   AND ` + replyfold.InquiryEligibleLatestSQL + `
	   AND nm.sent_at >= now() - make_interval(secs => $1)
	   AND NOT EXISTS (SELECT 1 FROM classify_promotions cp
	                    WHERE cp.normalized_message_id = nm.id)
	 ORDER BY r.created_at ASC, e.id ASC`

	rows, err := pool.Query(ctx, q, maxAge.Seconds())
	if err != nil {
		return nil, fmt.Errorf("promote: select inquiry inbox: %w", err)
	}
	defer rows.Close()

	var out []inquiryRow
	for rows.Next() {
		var (
			v             Verdict
			raw           []byte
			rawItem       *int64
			currentThread *int64
			sentAt        *time.Time
			replied       bool
			prior         bool
			body          string
		)
		if err := rows.Scan(&v.ExtractionID, &rawItem, &raw,
			&v.MessageID, &currentThread, &sentAt, &v.RunAt,
			&v.ProjectID, &v.ProjectSlug, &replied, &prior, &v.LoggedOnTaskID, &body); err != nil {
			return nil, fmt.Errorf("promote: scan inquiry inbox row: %w", err)
		}
		var f struct {
			AskKind           string `json:"ask_kind"`
			Asker             string `json:"asker"`
			Ask               string `json:"ask"`
			Reason            string `json:"reason"`
			Sender            string `json:"sender"`
			Subject           string `json:"subject"`
			Channel           string `json:"channel"`
			ProjectID         int64  `json:"project_id"`
			StoredThreadID    int64  `json:"thread_id"`
			StoredKey         string `json:"thread_key"`
			ThreadScope       string `json:"thread_scope"`
			ExternalMessageID string `json:"external_message_id"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			// The personal lane's policy: a verdict this pass cannot read stalls
			// the lane visibly rather than being skipped forever.
			return nil, fmt.Errorf("promote: parse inquiry verdict fields for extraction %d: %w", v.ExtractionID, err)
		}
		v.Lane = LaneInquiry
		v.RawItemID = rawItem
		v.ThreadID = currentThread
		v.SentAt = sentAt
		v.Kind, v.Reason = f.AskKind, f.Reason
		v.Sender, v.Subject = f.Sender, f.Subject
		v.StoredProjectID = f.ProjectID
		v.Asker, v.Channel = f.Asker, f.Channel
		v.ThreadKey, v.ThreadScope = f.StoredKey, f.ThreadScope
		v.StoredThreadID, v.ExternalMessageID = f.StoredThreadID, f.ExternalMessageID
		v.Title = inquiryTitle(f.Sender, f.Subject, f.Ask)

		c := InquiryCandidate{
			AskKind: f.AskKind, Channel: f.Channel, ThreadKey: f.StoredKey, ThreadScope: f.ThreadScope,
			StoredThreadID: f.StoredThreadID, RepliedSince: replied, PriorPost: prior,
			Mentioned: slackweb.MentionsOwner(body),
		}
		if currentThread != nil {
			c.CurrentThreadID = *currentThread
		}
		if sentAt != nil {
			c.SentAt = *sentAt
		}
		out = append(out, inquiryRow{v: v, c: c})
	}
	return out, rows.Err()
}
