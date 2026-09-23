package capture

// DryRunRules — `opsctl capture-rules try` (SWT-54 D9, D10, criterion 17).
//
// Prod capture is LIVE: a rule added there acts on the next connector tick, and
// capture_rules cannot be edited or re-added with the same pattern (IK F8). So a
// candidate rule is proven HERE first, in memory, over the stored corpus:
//
//   - the enabled rules plus the candidate (id max(id)+1, the id it would get)
//     are loaded, and every inbound message in the window is decided by the
//     SAME decideMessage the pass uses (Evaluate, the D1 authorship reads,
//     canonicalization, the D5 notice check) — the decideGateHolds precedent;
//   - each message's CURRENT latest decision is printed beside the proposed
//     one, then a per-PR rollup and the D9 backfill payloads.
//
// It writes NOTHING: no capture_decisions row, no executor call, no lock. Every
// statement it runs is a SELECT.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/github"
)

// CandidateRule is a capture_rule_add argument set, not yet stored.
type CandidateRule struct {
	Project, CriteriaType, Pattern, Subproject, ExternalSystem, KeyRegex, URLTemplate string
	Priority                                                                          int
	PRReview                                                                          bool
	ExcludePRAuthors                                                                  []string
	// CommTask (SWT-74 D1): simulate the rule ARMED for comms, so
	// `opsctl capture-rules try --comm-task --show all` shows what arming would
	// do before a row exists.
	CommTask bool
}

// DryRunConfig configures one dry run.
type DryRunConfig struct {
	Candidate CandidateRule
	// Since bounds sent_at to the last Since; 0 = unbounded.
	Since time.Duration
	// Show: "all" prints every message in the window; "wins" (the default)
	// prints only the messages the candidate matches — won, skipped (fell
	// through on his own PR) or lost to a higher rule.
	Show string
	Out  io.Writer
}

// DryRunSummary is one dry run's counts.
type DryRunSummary struct {
	CandidateID      int64
	Considered       int
	CandidateMatched int
	CandidateWon     int
	CandidateSkipped int
	CandidateLost    int
	WouldCreate      int
	WouldLog         int
	WouldClose       int
	PRs              int
	Backfill         int
}

// DryRunBackfillWindow is D9's "mail in the last 30 days".
const DryRunBackfillWindow = 30 * 24 * time.Hour

const (
	dryRunShowAll  = "all"
	dryRunShowWins = "wins"
)

// dryRunPR is one PR's rollup.
type dryRunPR struct {
	key      string
	verdict  *prAuthorVerdict
	title    string
	messages int
	creates  int
	logs     int
	closes   int
	skipped  int
	lastMail time.Time
	notice   string
	task     int64           // an existing task the mail logs onto (0 = none, or simulated)
	create   *pendingMessage // the message a live pass would create the task from
}

// DryRunRules decides the candidate over the stored corpus and prints the
// comparison. See the file comment: it writes nothing.
func DryRunRules(ctx context.Context, pool *pgxpool.Pool, cfg DryRunConfig) (DryRunSummary, error) {
	var sum DryRunSummary
	if pool == nil {
		return sum, errors.New("capture try: nil database pool")
	}
	show := cfg.Show
	if show == "" {
		show = dryRunShowWins
	}
	if show != dryRunShowAll && show != dryRunShowWins {
		return sum, fmt.Errorf("capture try: --show %q: must be %q or %q", show, dryRunShowAll, dryRunShowWins)
	}
	out := cfg.Out
	if out == nil {
		out = io.Discard
	}
	c := cfg.Candidate
	if c.Project == "" || c.Pattern == "" || !ValidKind(c.CriteriaType) {
		return sum, errors.New("capture try: the candidate needs a project, a valid criteria type and a pattern")
	}

	rules, err := loadRules(ctx, pool)
	if err != nil {
		return sum, err
	}
	// Finding A (SWT-54 review): once the rule is stored, the candidate (a
	// higher id at the same priority) never wins a match, the rollup stays
	// empty and the D9 backfill section reads "(none)" — a silent, wrong
	// answer. Refuse by name instead.
	for _, r := range rules {
		if r.rule.Project == c.Project && r.rule.Kind == c.CriteriaType && r.rule.Pattern == c.Pattern {
			return sum, fmt.Errorf("capture try: enabled rule %d already stores this rule (project %s, type %s, "+
				"pattern %q). A candidate can never win against it, so this run would roll up nothing and print an "+
				"EMPTY backfill. Use the saved output of the try run made BEFORE the add (HANDOFF step 3a)",
				r.rule.ID, c.Project, c.CriteriaType, c.Pattern)
		}
	}
	cand, err := dryRunCandidate(ctx, pool, c)
	if err != nil {
		return sum, err
	}
	sum.CandidateID = cand.rule.ID
	rules = append(rules, cand)
	byID := make(map[int64]storedRule, len(rules))
	pure := make([]Rule, 0, len(rules))
	for _, r := range rules {
		byID[r.rule.ID] = r
		pure = append(pure, r.rule)
	}

	pending, err := pendingMessages(ctx, pool, RulesConfig{All: true, Horizon: cfg.Since, everything: true})
	if err != nil {
		return sum, err
	}
	ids := make([]int64, 0, len(pending))
	for _, pm := range pending {
		ids = append(ids, pm.msg.ID)
	}
	current, err := dryRunLatestDecisions(ctx, pool, ids)
	if err != nil {
		return sum, err
	}

	window := "all time"
	if cfg.Since > 0 {
		window = "the last " + cfg.Since.String()
	}
	fmt.Fprintf(out, "try: candidate rule %d (max(id)+1): %s %s %q priority %d%s — %d inbound message(s) in %s; "+
		"writes nothing\n", cand.rule.ID, c.Project, c.CriteriaType, c.Pattern, c.Priority, dryRunRuleFlags(cand),
		len(pending), window)

	sim := simulatedRefs{}
	prs := map[string]*dryRunPR{}
	for i := range pending {
		pm := pending[i]
		d, winner, err := decideMessage(ctx, pool, RulesModeShadow, pm, pure, byID, sim)
		if err != nil {
			return sum, err
		}
		sum.Considered++

		role := ""
		switch {
		case d.prSkipped && d.prRuleID == cand.rule.ID:
			role = "skipped (his own PR, fell through)"
			if d.prVerdict != nil && d.prVerdict.verdict == prAuthorExcluded {
				role = "skipped (excluded author, fell through)"
			}
			sum.CandidateSkipped++
		case d.prUntrusted && d.prRuleID == cand.rule.ID:
			role = "skipped (untrusted GitHub mail, fell through)"
			sum.CandidateSkipped++
		case d.matchedRuleID != nil && *d.matchedRuleID == cand.rule.ID:
			role = "won"
			sum.CandidateWon++
		case dryRunHas(d.matchedRuleIDs, cand.rule.ID):
			role = fmt.Sprintf("lost to rule %d", *d.matchedRuleID)
			sum.CandidateLost++
		}
		if role != "" {
			sum.CandidateMatched++
		}

		// Simulate what a live pass would leave behind, so the next mail on the
		// same key reads as it would live.
		// SWT-78 item F: a DM decision simulates its CONVERSATION task, so a
		// second DM in the window reads task_log as the live pass would log it.
		if !d.deferred && d.direct {
			switch d.action {
			case actionTask:
				sum.WouldCreate++
				sim[directSimSystem+" "+d.directConv] = refTask{status: "ready"}
			case actionTaskLog:
				sum.WouldLog++
			}
		}
		if !d.deferred && d.extSystem != nil && d.extKey != nil {
			switch {
			case d.action == actionTask:
				sum.WouldCreate++
				sim[*d.extSystem+" "+*d.extKey] = refTask{status: "ready"}
			case d.action == actionTaskLog:
				sum.WouldLog++
				if d.prClose {
					sum.WouldClose++
					rt, _ := sim.lookup(*d.extSystem, *d.extKey)
					rt.taskID, rt.status = *d.taskID, "closed"
					sim[*d.extSystem+" "+*d.extKey] = rt
				}
			}
		}

		if d.prRuleID == cand.rule.ID && d.prKey != "" {
			p := prs[d.prKey]
			if p == nil {
				p = &dryRunPR{key: d.prKey}
				prs[d.prKey] = p
			}
			p.messages++
			if pm.sentAt.After(p.lastMail) {
				p.lastMail = pm.sentAt
			}
			if d.prNotice != "" {
				p.notice = d.prNotice // pending is sent_at order: the LAST notice is the PR's state
			}
			if d.prVerdict != nil && p.verdict == nil {
				p.verdict = d.prVerdict
			}
			if d.prSkipped || d.prUntrusted {
				p.skipped++
			}
			switch {
			case d.action == actionTask && winner.rule.ID == cand.rule.ID:
				p.creates++
				if p.create == nil {
					p.create = &pending[i]
					p.title, _ = ruleCreateTaskArgs(pm, cand, *d.extSystem, d.prKey)["title"].(string)
				}
			case d.action == actionTaskLog && winner.rule.ID == cand.rule.ID:
				p.logs++
				if p.task == 0 && d.taskID != nil {
					p.task = *d.taskID
				}
			}
			if d.prClose {
				p.closes++
			}
		}

		if show == dryRunShowAll || role != "" {
			fmt.Fprintf(out, "msg %d  %s  %s\n  current:  %s\n  proposed: %s", pm.msg.ID,
				pm.sentAt.UTC().Format(time.RFC3339), ruleOrNone(pm.msg.ThreadKey), current[pm.msg.ID], dryRunProposed(d))
			if role != "" {
				fmt.Fprintf(out, " — candidate %s", role)
			}
			fmt.Fprintf(out, "\n  reason:   %s\n", d.reason)
		}
	}

	keys := make([]string, 0, len(prs))
	for k := range prs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sum.PRs = len(keys)

	fmt.Fprintf(out, "\nper-PR rollup (%d PR(s) the candidate decides):\n", len(keys))
	for _, k := range keys {
		p := prs[k]
		verdict, author, evidence := "-", "-", "existing task; authorship is read only before a task exists"
		if p.verdict != nil {
			verdict, evidence = p.verdict.verdict, p.verdict.evidence
			if p.verdict.author != "" {
				author = p.verdict.author
			}
		}
		notice := "none"
		if p.notice != "" {
			notice = p.notice
		}
		task := "-"
		if p.task != 0 {
			task = fmt.Sprintf("%d", p.task)
		}
		fmt.Fprintf(out, "PR %s: verdict %s, author %s (%s); title %q; would create %d, log %d, close %d, "+
			"fall through %d; last mail %s; merge/close notice %s; existing task %s\n",
			k, verdict, author, evidence, p.title, p.creates, p.logs, p.closes, p.skipped,
			p.lastMail.UTC().Format(time.RFC3339), notice, task)
	}

	fmt.Fprintf(out, "\nD9 backfill payloads (verdict other/undetermined, no merge/close notice, mail in the last %s, "+
		"no task yet). Run the three calls of a PR in order; put create_task's task_id where TASK_ID stands:\n",
		DryRunBackfillWindow)
	cutoff := time.Now().Add(-DryRunBackfillWindow)
	for _, k := range keys {
		p := prs[k]
		if p.create == nil || p.verdict == nil || github.PRStateEndsPR(p.notice) || p.lastMail.Before(cutoff) {
			continue
		}
		if p.verdict.verdict != prAuthorOther && p.verdict.verdict != prAuthorUndetermined {
			continue
		}
		lines, err := dryRunBackfill(*p.create, cand, p.key)
		if err != nil {
			return sum, err
		}
		sum.Backfill++
		fmt.Fprintf(out, "# %s\n%s", p.key, lines)
	}
	if sum.Backfill == 0 {
		fmt.Fprintln(out, "(none)")
	}

	fmt.Fprintf(out, "\ntry: considered %d; candidate matched %d (won %d, skipped %d, lost %d); would create %d, "+
		"log %d, close %d; %d PR(s), %d to backfill. Nothing was written.\n",
		sum.Considered, sum.CandidateMatched, sum.CandidateWon, sum.CandidateSkipped, sum.CandidateLost,
		sum.WouldCreate, sum.WouldLog, sum.WouldClose, sum.PRs, sum.Backfill)
	return sum, nil
}

// dryRunCandidate builds the candidate's storedRule exactly as loadRules would
// read it back, with the id the insert would get (max(id)+1; a sequence gap can
// make the real one higher).
func dryRunCandidate(ctx context.Context, pool *pgxpool.Pool, c CandidateRule) (storedRule, error) {
	var s storedRule
	if err := pool.QueryRow(ctx, `SELECT id, name, ticket_assignee_gate FROM projects WHERE slug = $1`, c.Project).
		Scan(&s.projectID, &s.projectName, &s.gateOn); err != nil {
		return s, fmt.Errorf("capture try: project %q: %w", c.Project, err)
	}
	var maxID int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(max(id), 0) FROM capture_rules`).Scan(&maxID); err != nil {
		return s, fmt.Errorf("capture try: read max rule id: %w", err)
	}
	s.rule = Rule{ID: maxID + 1, Project: c.Project, Kind: c.CriteriaType, Pattern: c.Pattern,
		Priority: c.Priority, Enabled: true}
	if c.KeyRegex != "" {
		re := c.KeyRegex
		s.rule.ExternalKeyRegex = &re
	}
	if c.ExternalSystem != "" {
		system := c.ExternalSystem
		s.rule.Source = &system
	}
	s.subproject, s.extSystem, s.urlTemplate = c.Subproject, c.ExternalSystem, c.URLTemplate
	s.prReview = c.PRReview
	s.commTask = c.CommTask
	s.excludePRAuthors = append([]string{}, c.ExcludePRAuthors...)
	return s, nil
}

// dryRunLatestDecisions reads each message's latest capture decision in ANY
// mode — what every latest-decision reader follows today.
func dryRunLatestDecisions(ctx context.Context, pool *pgxpool.Pool, ids []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	for _, id := range ids {
		out[id] = "undecided"
	}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := pool.Query(ctx,
		`SELECT DISTINCT ON (message_id) message_id, mode, action, matched_rule_id, task_id
		   FROM capture_decisions WHERE message_id = ANY($1)
		  ORDER BY message_id, id DESC`, ids)
	if err != nil {
		return nil, fmt.Errorf("capture try: read current decisions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var mode, action string
		var rule, task *int64
		if err := rows.Scan(&id, &mode, &action, &rule, &task); err != nil {
			return nil, fmt.Errorf("capture try: scan current decision: %w", err)
		}
		s := action
		if rule != nil {
			s += fmt.Sprintf(" (rule %d", *rule)
			if task != nil {
				s += fmt.Sprintf(", task %d", *task)
			}
			s += ")"
		}
		out[id] = s + " [" + mode + "]"
	}
	return out, rows.Err()
}

func dryRunProposed(d ruleDecision) string {
	if d.deferred {
		return "deferred (own-action guard; a live pass would retry)"
	}
	s := d.action
	var parts []string
	if d.matchedRuleID != nil {
		parts = append(parts, fmt.Sprintf("rule %d", *d.matchedRuleID))
	}
	if d.extSystem != nil && d.extKey != nil {
		parts = append(parts, *d.extSystem+" "+*d.extKey)
	}
	if d.taskID != nil {
		if *d.taskID == 0 {
			parts = append(parts, "the task this dry run would create")
		} else {
			parts = append(parts, fmt.Sprintf("task %d", *d.taskID))
		}
	}
	if d.prClose {
		parts = append(parts, "then close it")
	}
	if d.comm {
		parts = append(parts, "and create a comm task for it")
	}
	if len(parts) > 0 {
		s += " (" + strings.Join(parts, ", ") + ")"
	}
	return s
}

func dryRunRuleFlags(s storedRule) string {
	var b strings.Builder
	if s.extSystem == "" {
		b.WriteString(", attribution only")
	} else {
		fmt.Fprintf(&b, ", %s key", s.extSystem)
		if s.rule.ExternalKeyRegex != nil {
			fmt.Fprintf(&b, " %s", *s.rule.ExternalKeyRegex)
		}
	}
	if s.prReview {
		fmt.Fprintf(&b, ", pr_review, exclude_pr_authors [%s]", strings.Join(s.excludePRAuthors, ","))
	}
	if s.commTask {
		b.WriteString(", comm-task (armed)")
	}
	return b.String()
}

func dryRunHas(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// dryRunBackfill renders D9's three calls for one PR, one line each, ready to
// paste. The args are the ones a live pass would have sent (ruleCreateTaskArgs,
// ruleExternalURL). TASK_ID is left bare on purpose: a payload pasted without
// substituting it is not valid JSON, so opsctl refuses it instead of linking
// the wrong task.
func dryRunBackfill(pm pendingMessage, cand storedRule, key string) (string, error) {
	var b strings.Builder
	create, err := json.Marshal(ruleCreateTaskArgs(pm, cand, cand.extSystem, key))
	if err != nil {
		return "", fmt.Errorf("capture try: marshal create_task payload for %s: %w", key, err)
	}
	fmt.Fprintf(&b, "opsctl call --tool create_task --args %s\n", dryRunShellQuote(string(create)))
	link, err := json.Marshal(map[string]any{"task_id": 0, "system": cand.extSystem, "external_key": key,
		"external_url": ruleExternalURL(cand, cand.extSystem, key)})
	if err != nil {
		return "", fmt.Errorf("capture try: marshal link_external_ref payload for %s: %w", key, err)
	}
	fmt.Fprintf(&b, "opsctl call --tool link_external_ref --args %s\n", dryRunShellQuote(dryRunTaskIDPlaceholder(link)))
	if pm.threadID != nil {
		thread, err := json.Marshal(map[string]any{"task_id": 0, "thread_id": *pm.threadID})
		if err != nil {
			return "", fmt.Errorf("capture try: marshal task_set_source_thread payload for %s: %w", key, err)
		}
		fmt.Fprintf(&b, "opsctl call --tool task_set_source_thread --args %s\n",
			dryRunShellQuote(dryRunTaskIDPlaceholder(thread)))
	}
	return b.String(), nil
}

func dryRunTaskIDPlaceholder(raw []byte) string {
	return strings.Replace(string(raw), `"task_id":0`, `"task_id":TASK_ID`, 1)
}

func dryRunShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
