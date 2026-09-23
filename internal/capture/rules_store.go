package capture

// The database half of the capture-rules engine (SWT-17,
// docs/tickets/capture-rules_SPEC.md §4-§7): rule loading, the pending-message
// query, the decision writes, the advisory lock, and the EvaluateRules driver.
//
// The split from rules.go is the point of §2 and is enforced structurally
// (rules_structure_test.go): matching is a pure function of (message, rules) with
// no context and no pool, and NOTHING here re-implements it. Every routing
// question — which rule wins, what the external key is — is answered by calling
// Evaluate. This file only decides what to DO with the answer.
//
// Invariant 3 is the other structural line: `tasks`, `external_refs` and
// `task_events` are reached ONLY through create_task / link_external_ref /
// task_append_log on the executor. Direct SQL here is reads plus the engine's own
// append-only log, `capture_decisions` — the ai_runs/ai_extractions precedent.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/github"
	"github.com/sspataro57/switchboard/internal/connector/jira"
	"github.com/sspataro57/switchboard/internal/connector/slackweb"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/textmatch"
	"github.com/sspataro57/switchboard/internal/ticketstatus"
)

// RulesAdvisoryLockKey serializes the pass across the four connector CronJobs,
// which can overlap. The established convention: orchestrator 0x51570005, triage
// 0x51570006, this pass 0x51570015 (the migration number).
//
// Unlike triage, losing the race is NOT an error — see EvaluateRules.
const RulesAdvisoryLockKey int64 = 0x5157_0015

// Modes. Shadow is the DEFAULT and is real: it extracts and records everything
// and creates nothing.
const (
	RulesModeShadow = "shadow"
	RulesModeLive   = "live"
)

// DefaultRulesHorizon bounds `sent_at` in LIVE mode, matching
// DefaultObserveHorizon. Shadow defaults to unbounded (0) instead, because the
// whole corpus is what you want to diff.
//
// The consequence is chosen rather than discovered (SPEC §6): once the mode flips
// to live, only messages inside the horizon are acted on. Three years of history
// stays un-tasked unless someone deliberately runs a bounded backfill
// (`opsctl capture-rules run --live --since 8760h`).
const DefaultRulesHorizon = DefaultObserveHorizon

// MinLiveRulesHorizon is the shortest LIVE horizon normalize accepts (SWT-45
// J17). The own-action guard's deferral writes NO decision row, so a deferred
// message is decided only while it is still inside the horizon. The horizon is
// measured on sent_at, the deferral from ingest, so a message must stay inside
// the window for: its sent→ingest lag (mail delivery plus a */15 connector tick),
// OwnActionMaxWait (30m), and one more */15 pass to make the post-bound
// decision. 2h covers those 45 minutes plus 75 minutes of margin — five missed
// */15 ticks, or a slow mail relay — before a deferred message could age out
// undecided. Shadow is not floored: it acts on nothing, and its short horizons
// are diffing tools.
const MinLiveRulesHorizon = 2 * time.Hour

// DefaultRulesActor is the actor every executor call this pass makes is attributed
// to when the caller names none. SPEC "API / MCP tool changes" spells the shape
// `capture:{connector}`; a connector main that knows which one it is should set
// RulesConfig.Actor to `capture:jira`, `capture:slackweb`, and so on.
const DefaultRulesActor = "capture:rules"

const (
	// rulesTitleLen is SPEC §7's 120 runes, truncated with textmatch.NormalizedPrefix
	// — the ONE spelling of whitespace-collapsed, rune-safe truncation (SWT-16).
	rulesTitleLen = 120
	// rulesPreviewLen is the body preview carried into the task body and the log
	// message. Wider than bodyPreviewLen because this text is read by a human
	// deciding what the task is, not compared against a provider round trip.
	rulesPreviewLen = 400
)

// RulesConfig is the pass's per-run configuration.
//
// Mode and Horizon are SPEC §6. Limit bounds one run (the narrow live smoke).
// It is smoke-only, and it has a head-of-line effect: the pending query reads
// oldest first (sent_at, id), and an own-action-deferred message stays pending,
// so up to OwnActionMaxWait a run with Limit N can spend its N on deferred rows
// and decide nothing new. No CronJob sets Limit; a hand-run `--limit` should
// expect that (docs/runbooks/capture-rules.md).
//
// Actor and All are additive on the shared contract and both default safely, so a
// caller that sets neither gets exactly the contracted behaviour:
//   - Actor names the connector for the audit trail (SPEC's `capture:{connector}`);
//     empty means DefaultRulesActor.
//   - All re-evaluates messages that already carry a live decision row. It is
//     SHADOW-ONLY and refused in live mode (criterion 10): task_append_log has no
//     dedup of its own, so a live replay would double-append, and
//     capture_decisions_live_uniq is the only thing standing between the pass and
//     that. The refusal is what keeps the index meaningful.
type RulesConfig struct {
	Mode    string
	Horizon time.Duration
	Limit   int
	Actor   string
	All     bool
	// everything is DryRunRules' reading of the corpus (SWT-54 D10): every
	// inbound message in the window, gate- and route-resolved ones included,
	// because the dry run writes nothing that could bury them. Never set by a
	// pass that writes.
	everything bool
}

// RulesStats is one run's counters. Considered == Matched + Unmatched;
// TasksCreated, Appended, Reopened, Revived and SurfacedCreated are zero in
// shadow mode, always. Reopened (SWT-36) counts dismissed tasks the guarded
// task_reopen answered reopened:true for; every one of them is also counted in
// Appended. Revived (SWT-45) counts closed tasks the revive form of task_reopen
// answered reopened:true for (also counted in Appended); SurfacedCreated counts
// tasks an overriding rule created and task_mark_surfaced surfaced (also
// counted in TasksCreated).
//
// Deferred (SWT-45 J17) counts messages the own-action guard left undecided
// because the ticket's Jira thread is not yet known synced past them: no
// decision row, not Considered, pending again next pass. Counted in both modes.
// Blind counts decisions the guard made BLIND: still not synced after
// OwnActionMaxWait, so the pass decided as if clear (fail-open, SPEC J17).
// Each is also Considered and its reason says BLIND. Counted in both modes.
//
// Resurfaced (chat-on-closed-task, CC7) counts decisions written with
// capture_decisions.resurface=true: a task_log onto a CLOSED task that the
// inquiry lanes will read. A recorded fact, not an action, so it is counted in
// both modes (the Deferred/Blind precedent).
//
// PRAuthorSkipped (SWT-54 D2) counts messages a pr_review rule would have
// created a task for but whose PR is his own or its author excluded, so the
// message was re-decided without that rule (fall-through). The decision is
// mode-free, so it is counted in both modes. PRClosed (SWT-54 D5) counts review
// tasks task_close closed on GitHub's merge or close notice; live only (also
// counted in Appended, whose log rides first).
type RulesStats struct {
	Considered      int
	Matched         int
	Unmatched       int
	TasksCreated    int
	Appended        int
	Reopened        int
	Revived         int
	SurfacedCreated int
	Deferred        int
	Blind           int
	Resurfaced      int
	PRAuthorSkipped int
	PRClosed        int
	Activity        int // SWT-72: task_log attaches that marked activity on an OPEN task
	CommTasks       int // SWT-74: comm tasks CREATED by armed rules' task_log attaches
}

// RulesMode reads CAPTURE_RULES_MODE. Anything that is not exactly "live" —
// unset, misspelled, "LIVE", "true" — is SHADOW.
//
// Fail-safe on purpose: the failure mode of guessing wrong in one direction is a
// pass that records and creates nothing, and in the other a pass that creates
// tasks nobody approved. A typo in a CronJob manifest must not be able to flip the
// funnel on.
func RulesMode() string {
	if os.Getenv("CAPTURE_RULES_MODE") == RulesModeLive {
		return RulesModeLive
	}
	return RulesModeShadow
}

// RulesHorizon reads CAPTURE_RULES_SINCE (a Go duration) for the given mode.
//
// Same defensive shape as ObserveHorizon: anything unparseable or non-positive
// falls back to the mode's default. "720" is the realistic typo — a bare number is
// not a Go duration, and reading it as 720ns would make the scan window empty, so
// the pass would decide nothing forever with no error anywhere.
func RulesHorizon(mode string) time.Duration {
	fallback := time.Duration(0) // shadow: unbounded, the whole corpus
	if mode == RulesModeLive {
		fallback = DefaultRulesHorizon
	}
	raw := os.Getenv("CAPTURE_RULES_SINCE")
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// normalize applies defaults and refuses configurations that must never run.
func (c RulesConfig) normalize() (RulesConfig, error) {
	switch c.Mode {
	case "":
		c.Mode = RulesModeShadow
	case RulesModeShadow, RulesModeLive:
	default:
		return c, fmt.Errorf("capture rules: mode %q: must be %q or %q", c.Mode, RulesModeShadow, RulesModeLive)
	}
	if c.All && c.Mode == RulesModeLive {
		return c, errors.New("capture rules: --all is shadow-only; a live replay would double-append " +
			"(task_append_log has no dedup, and capture_decisions_live_uniq is the live dedup)")
	}
	if c.Horizon <= 0 && c.Mode == RulesModeLive {
		// Structural, not caller-supplied: the anti-flood bound must not depend on
		// every caller remembering it.
		c.Horizon = DefaultRulesHorizon
	}
	if c.Mode == RulesModeLive && c.Horizon < MinLiveRulesHorizon {
		// A positive CAPTURE_RULES_SINCE / --since below the floor lands here: it
		// errors the pass rather than silently widening the window.
		return c, fmt.Errorf("capture rules: live horizon %s is below the %s floor: a message the own-action "+
			"guard defers (up to %s from ingest) writes no decision row and would age out of the window undecided; "+
			"set CAPTURE_RULES_SINCE / --since to at least %s", c.Horizon, MinLiveRulesHorizon, OwnActionMaxWait,
			MinLiveRulesHorizon)
	}
	if c.Actor == "" {
		c.Actor = DefaultRulesActor
	}
	return c, nil
}

// storedRule is one capture_rules row: the pure evaluator's Rule plus the columns
// only the driver needs (project id, subproject, external system, url template).
//
// The split mirrors the package split. Rule carries what MATCHING needs and
// nothing else, so a change to what a task looks like cannot reach the evaluator.
type storedRule struct {
	rule        Rule
	projectID   int64
	subproject  string
	extSystem   string
	urlTemplate string
	// projectName is projects.name — the title's fallback label when a
	// thread-keyed task's message carries no sender (SWT-31 criterion 3).
	projectName string
	// gateOn is projects.ticket_assignee_gate, read from the COLUMN with the
	// rules (SWT-40 D-D1): a jira-keyed match on a gated project is `held`, and
	// the pipelined gate stage looks the ticket up before any task exists.
	gateOn bool
	// revive and addressed are capture_rules.revive / .addressed (SWT-45 J1),
	// read from the COLUMNS with the rules. overrides(revive, addressed, gateOn)
	// decides whether a match is activity that revives or surfaces.
	revive    bool
	addressed bool
	// prReview and excludePRAuthors are capture_rules.pr_review /
	// .exclude_pr_authors (SWT-54, migration 0035), read from the COLUMNS with
	// the rules. A pr_review rule's create branch reads authorship from the
	// stored raw headers and falls through on his own or an excluded author's
	// PR; its task_log branch closes the task on a merge or close notice.
	prReview         bool
	excludePRAuthors []string
	// notifiers is projects.notifier_senders (chat-on-closed-task CC4, 0034),
	// read from the COLUMN with the rules. This is its one reader:
	// notifierSender matches a sender against it by equality.
	notifiers []string
	// commTask is capture_rules.comm_task (SWT-74 D1), read from the COLUMN
	// with the rules — the ONLY reader. An armed rule's task_log attach from a
	// person becomes its own INCOMING task (comm.go decides).
	commTask bool
}

// pendingMessage is one inbound message the pass must decide about.
type pendingMessage struct {
	msg       Message
	rawItemID *int64
	// threadID is the message's normalized_threads id — the provenance a
	// live `task` decision records on the task it creates (SWT-20). NULL when
	// the message has no thread; nothing is invented.
	threadID *int64
	channel  string
	sentAt   time.Time
	// rawConvType is the raw observation's conversation.type ('dm',
	// 'group_dm', 'public_channel', …; '' for non-Slack), read from
	// raw_source_items.raw_json in pendingMessageCols. SWT-78: a group DM is
	// only knowable here — 40 of 53 production group DMs have C… ids.
	rawConvType string
}

// ruleDecision is one capture_decisions row before it is written.
type ruleDecision struct {
	action         string
	projectID      *int64
	matchedRuleID  *int64
	matchedRuleIDs []int64
	ambiguous      bool
	extSystem      *string
	extKey         *string
	taskID         *int64
	reason         string
	// dismissalID is the linked task's OPEN task_dismissals row (SWT-36
	// criterion 13), 0 = none. Carried, never written to capture_decisions:
	// the typed outcome lives in task_dismissals.reopened_by_message_id (D7).
	dismissalID int64
	// comm (SWT-74 D3): this task_log makes its own comm task — carried on the
	// decision, never a column; the created task's id is what capture_decisions
	// records (comm_task_id), after the fact.
	comm bool
	// revive: a task_log on a CLOSED task by an overriding rule (SWT-45 J9) —
	// log, then the revive form of task_reopen. surface: a task created by an
	// overriding rule — create, link, provenance, then task_mark_surfaced.
	// Carried, never written to capture_decisions: the typed outcome lives in
	// tasks.surfaced_* (the SWT-36 D7 precedent).
	revive  bool
	surface bool
	// deferred: the own-action guard (J17) could not yet decide, so NO row is
	// written and the message stays pending — the live claim is not spent.
	deferred bool
	// blind: the guard decided past OwnActionMaxWait without the freshness it
	// wanted (the reason says BLIND). Carried for RulesStats.Blind only.
	blind bool

	// SWT-54, carried, never written as columns (the reason carries them):
	//   - prRuleID / prKey / prNumber: a pr_review rule was the top-level winner
	//     and derived this canonical PR key (also set after a fall-through, so
	//     the dry run can roll the message up under its PR);
	//   - prVerdict: the D1 verdict, computed on the create branch only;
	//   - prSkipped: the verdict was own/excluded and the message fell through;
	//   - prUntrusted: the mail is not a receiving-MX-authenticated GitHub
	//     notification (D1 amendment 2026-09-14), so it fell through before
	//     any pr_review action; not counted in PRAuthorSkipped;
	//   - prNotice: the message is GitHub's merge/close notice for the PR;
	//   - prClose: a task_log on a not-closed task that the live pass must
	//     then close through task_close (D5).
	prRuleID    int64
	prKey       string
	prNumber    int
	prVerdict   *prAuthorVerdict
	prSkipped   bool
	prUntrusted bool
	prNotice    string
	prClose     bool

	// resurface (chat-on-closed-task CC3): a task_log onto a CLOSED task that
	// no other path owns, decided by the pure resurfaces(). WRITTEN to
	// capture_decisions.resurface: the inquiry lanes read it from there.
	resurface bool

	// direct (SWT-78): a person's Slack DM decided onto its CONVERSATION task —
	// `task` (none open) or `task_log` (onto it) — with external_system and
	// external_key NULL. Carried, never a column: the NULL ref and the reason
	// are what the row records. directConv is the conversation's thread key
	// (slackweb.ConversationThreadKey), the unit "one task per conversation"
	// counts in.
	direct     bool
	directConv string

	// channelUnmentioned (SWT-79 D2): an `attributed` Slack CHANNEL decision
	// whose text does not @-mention him. WRITTEN to
	// capture_decisions.channel_unmentioned: both inquiry inboxes skip it, so
	// qwen never sees channel chatter. Set by decideMessage's one post-decision
	// step, never by the inner decision.
	channelUnmentioned bool
	// directNoThread: the conversation task found carries no source thread
	// (an interrupted create); the attach records it.
	directNoThread bool
}

// simulatedRefs is DryRunRules' stand-in for the external_refs rows its own
// proposed `task` decisions would have written, keyed system+" "+key, so a
// second mail on the same new PR reads as task_log, as the live pass would log
// it. nil on every pass that writes.
type simulatedRefs map[string]refTask

func (s simulatedRefs) lookup(system, key string) (refTask, bool) {
	rt, ok := s[system+" "+key]
	return rt, ok
}

// Actions, spelled exactly as capture_decisions.action's CHECK (SPEC §4).
const (
	actionUnmatched  = "unmatched"
	actionAttributed = "attributed"
	actionTask       = "task"
	actionTaskLog    = "task_log"
	// actionHeld (SWT-40 D-D1, migration 0029): a jira-keyed match on a gated
	// project. It names the project, rule and key and acts on nothing; the
	// pipelined gate stage resolves it into a mode='gate' row (gate.go).
	actionHeld = "held"
)

// EvaluateRules is the driver: load the rules, take the advisory lock, evaluate
// every pending inbound message, and record one capture_decisions row for each.
//
// In LIVE mode it additionally creates ONE task per external ticket through the
// executor and appends later notifications about the same ticket as task log
// events. In SHADOW mode — the default — it records the same decisions and creates
// nothing at all: no task, no external ref, no task event, no delivery.
//
// Losing the advisory lock is a clean no-op returning (RulesStats{}, nil), never
// an error. That is the difference from triage, which exits: this pass is a
// hitchhiker on a connector run, and a connector must not fail because another
// connector happened to be running.
func EvaluateRules(ctx context.Context, pool *pgxpool.Pool, ex *executor.Executor, cfg RulesConfig) (RulesStats, error) {
	cfg, err := cfg.normalize()
	if err != nil {
		return RulesStats{}, err
	}
	if pool == nil {
		return RulesStats{}, errors.New("capture rules: nil database pool")
	}
	if cfg.Mode == RulesModeLive && ex == nil {
		// Invariant 3: live mode reaches tasks/external_refs/task_events only
		// through the executor, so without one there is no legal way to act.
		return RulesStats{}, errors.New("capture rules: live mode requires an executor")
	}

	release, held, err := tryRulesLock(ctx, pool)
	if err != nil {
		return RulesStats{}, err
	}
	if !held {
		log.Printf("capture rules: another pass holds advisory lock 0x%X; skipping this run", RulesAdvisoryLockKey)
		return RulesStats{}, nil
	}
	defer release()

	rules, err := loadRules(ctx, pool)
	if err != nil {
		return RulesStats{}, err
	}
	byID := make(map[int64]storedRule, len(rules))
	pure := make([]Rule, 0, len(rules))
	for _, r := range rules {
		byID[r.rule.ID] = r
		pure = append(pure, r.rule)
	}

	if err := recheckEditedMentions(ctx, pool); err != nil {
		return RulesStats{}, err
	}

	pending, err := pendingMessages(ctx, pool, cfg)
	if err != nil {
		return RulesStats{}, err
	}

	var stats RulesStats
	for _, pm := range pending {
		decision, winner, err := decideMessage(ctx, pool, cfg.Mode, pm, pure, byID, nil)
		if err != nil {
			return stats, err
		}
		if decision.deferred {
			// SWT-45 J17: deciding now could revive a task on his own Jira
			// action the poller has not stored yet. Writing nothing leaves the
			// message pending; OwnActionMaxWait bounds how long.
			stats.Deferred++
			log.Printf("capture rules: message %d deferred: %s", pm.msg.ID, decision.reason)
			continue
		}

		decisionID, inserted, err := insertDecision(ctx, pool, cfg.Mode, pm, decision)
		if err != nil {
			return stats, err
		}
		if !inserted {
			// Only reachable in live mode, and only if something wrote a live
			// decision for this message between the pending query and here. The
			// partial unique index is the claim: whoever won it owns the action, so
			// this pass must NOT act, and must not count the message as considered.
			// One live action per message, forever.
			continue
		}
		stats.Considered++
		if decision.blind {
			stats.Blind++
		}
		if decision.resurface {
			stats.Resurfaced++
		}
		if decision.prSkipped {
			stats.PRAuthorSkipped++
		}
		if decision.action == actionUnmatched {
			stats.Unmatched++
		} else {
			stats.Matched++
		}
		if cfg.Mode != RulesModeLive {
			continue
		}
		// SWT-78: a DM decision carries NULL external_system/key, so it has its
		// own act — the switch below dereferences both on every branch.
		if decision.direct {
			if err := actDirect(ctx, pool, ex, cfg.Actor, decisionID, pm, winner, decision, &stats); err != nil {
				return stats, err
			}
			continue
		}

		switch decision.action {
		case actionTask:
			taskID, err := createRuleTask(ctx, ex, cfg.Actor, pm, winner, *decision.extSystem, *decision.extKey)
			if err != nil {
				return stats, err
			}
			// Record the task on its decision BEFORE linking the ref. The decision
			// row is already committed and can never be retried (the live claim is
			// one row per message, forever), so a link failure must not also lose
			// the pointer to the task that was created — the report's "action=task
			// with no task_id" line is how an operator finds these, and it must
			// mean "nothing was created", not "something was, somewhere".
			if err := recordDecisionTask(ctx, pool, decisionID, taskID); err != nil {
				return stats, err
			}
			if err := linkRuleRef(ctx, ex, cfg.Actor, taskID, winner, *decision.extSystem, *decision.extKey); err != nil {
				return stats, err
			}
			// SWT-20 criterion 19: record which conversation raised the task,
			// through the executor, in the linkRuleRef shape — the error fails
			// the pass rather than being logged. A version that logged and
			// continued would leave a provenance-less task behind exactly once,
			// silently: the live decision is spent, external_refs has taken the
			// key forever, and draft_delivery then refuses the task for a fact
			// nothing will ever write.
			if err := setRuleProvenance(ctx, ex, cfg.Actor, taskID, pm); err != nil {
				return stats, err
			}
			stats.TasksCreated++
			// SWT-72 follow-up (2026-09-22, Salvador: "lyle's emails are not
			// landing in incoming"): a task a rule CREATES from a person's first
			// message is activity by that message too, so it lands in INCOMING
			// with the sender and "new email / new comment / new slack" like an
			// attach does — rather than sitting silently in QUEUE. Same tool,
			// same skips (a non-inbound message errors; a replay is a no-op).
			// After provenance and before surfacing, so a crash leaves today's
			// behaviour (a queued task) and never a half-linked one. NOT for a
			// pr_review rule: a PR notice is not a person's comm, and the review
			// task reaches INCOMING on its own kind (SWT-59 pr_review) already.
			if !(winner.prReview && *decision.extSystem == "github") {
				marked, err := markRuleActivity(ctx, ex, cfg.Actor, pm, taskID, *decision.extSystem, *decision.extKey)
				if err != nil {
					return stats, err
				}
				if marked {
					stats.Activity++
				}
			}
			// SWT-45 J7/J9: surfacing LAST. A crash before it degrades to today:
			// the reconciler may close the new task, and the next overriding
			// message revives and surfaces it.
			if decision.surface {
				surfaced, err := markRuleSurfaced(ctx, ex, cfg.Actor, pm, taskID, *decision.extSystem, *decision.extKey)
				if err != nil {
					return stats, err
				}
				if surfaced {
					stats.SurfacedCreated++
				}
			}
		case actionTaskLog:
			if err := appendRuleLog(ctx, ex, cfg.Actor, pm, *decision.taskID, *decision.extSystem, *decision.extKey); err != nil {
				return stats, err
			}
			stats.Appended++
			// SWT-72 D3/D7: the attach is activity on the task — a comment, a
			// direct email, a Slack message — and the board puts it in INCOMING
			// for review. Log first, THEN the mark (a crash between the two
			// leaves today's behaviour); a closed target is skipped by the tool,
			// so the revive and reopen below keep their SWT-45/SWT-36 meaning.
			// Two exclusions: the merged/closed PR notice (the next call closes
			// the task, and surfacing a row to close it is noise) and a comm
			// (SWT-74 D3: the mark MOVES to the comm task — the new row is the
			// thing to look at, and surfacing the target too would double the rows).
			if !decision.prClose && !decision.comm {
				marked, err := markRuleActivity(ctx, ex, cfg.Actor, pm, *decision.taskID, *decision.extSystem, *decision.extKey)
				if err != nil {
					return stats, err
				}
				if marked {
					stats.Activity++
				}
			}
			// SWT-74 D3: an ARMED rule's attach from a person makes its OWN comm
			// task, in this order — create (through create_task), record the id
			// on the decision (the claim is spent; a later failure must not lose
			// the pointer), provenance (draft_delivery refuses a task without
			// it), the activity mark (the ONE thing that puts the comm in
			// INCOMING), and LAST the ids-only pointer on the target. No
			// external_refs row for the comm: taskForExternalRef takes the NEWEST
			// ref for a key, and a second row would hijack every future attach.
			// Crash window: a death between create_task and recordDecisionCommTask
			// leaves one orphan comm task — ready, in QUEUE, no thread, no pointer,
			// unreferenced by the spent claim — that nothing reconciles. Accepted:
			// one quiet extra row, strictly milder than the create branch's own
			// window, and the operator sees it as a plain human task to dismiss.
			if decision.comm {
				commID, err := createCommTask(ctx, ex, cfg.Actor, pm, winner, *decision.extSystem, *decision.extKey, *decision.taskID)
				if err != nil {
					return stats, err
				}
				if err := recordDecisionCommTask(ctx, pool, decisionID, commID); err != nil {
					return stats, err
				}
				if err := setRuleProvenance(ctx, ex, cfg.Actor, commID, pm); err != nil {
					return stats, err
				}
				marked, err := markRuleActivity(ctx, ex, cfg.Actor, pm, commID, *decision.extSystem, *decision.extKey)
				if err != nil {
					return stats, err
				}
				if !marked {
					// Cannot happen for a task created one statement ago — and if
					// it did, the comm would sit in QUEUE, the one failure this
					// ticket exists to prevent. Loud, not silent.
					return stats, fmt.Errorf("comm task %d for message %d was not marked with its activity (tool skipped)",
						commID, pm.msg.ID)
				}
				if err := appendCommPointer(ctx, ex, cfg.Actor, pm, *decision.taskID, commID); err != nil {
					return stats, err
				}
				stats.CommTasks++
			}
			// SWT-36 D10 / SWT-45 J9: log first, THEN the reopen — a crash
			// between the two leaves exactly today's behaviour (logged, still
			// closed). The revive handles an open dismissal itself, so it takes
			// precedence over SWT-36's guarded reopen.
			//
			// SWT-54 D5: a merge/close notice on a NOT-closed review task closes
			// it, log first, then task_close. Exclusive with the two reopens,
			// which only ever act on a closed task.
			if decision.prClose {
				closed, err := closeRuleTask(ctx, pool, ex, cfg.Actor, pm, decisionID, *decision.taskID,
					decision.prNumber, decision.prNotice)
				if err != nil {
					return stats, err
				}
				if closed {
					stats.PRClosed++
				}
			} else if decision.revive {
				revived, err := reviveRuleTask(ctx, ex, cfg.Actor, pm, *decision.taskID,
					*decision.extSystem, *decision.extKey)
				if err != nil {
					return stats, err
				}
				if revived {
					stats.Revived++
				}
			} else if decision.dismissalID != 0 {
				reopened, err := reopenRuleTask(ctx, ex, cfg.Actor, pm, *decision.taskID, decision.dismissalID,
					*decision.extSystem, *decision.extKey)
				if err != nil {
					return stats, err
				}
				if reopened {
					stats.Reopened++
				}
			}
		}
	}
	return stats, nil
}

// tryRulesLock takes the single-instance advisory lock on a dedicated connection.
//
// The returned release UNLOCKS explicitly before returning the connection to the
// pool. Releasing a pooled connection does not end the session, and a session-held
// advisory lock outlives the release — so a pass that only released would poison
// its own pool for every later pass in the same process.
func tryRulesLock(ctx context.Context, pool *pgxpool.Pool) (func(), bool, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire capture rules lock connection: %w", err)
	}
	var taken bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, RulesAdvisoryLockKey).Scan(&taken); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("pg_try_advisory_lock(0x%X): %w", RulesAdvisoryLockKey, err)
	}
	if !taken {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		// context.Background(): the unlock must happen even when the run was
		// cancelled, or the lock leaks for the life of the process.
		if _, err := conn.Exec(context.Background(),
			`SELECT pg_advisory_unlock($1)`, RulesAdvisoryLockKey); err != nil {
			log.Printf("capture rules: releasing advisory lock 0x%X: %v", RulesAdvisoryLockKey, err)
		}
		conn.Release()
	}, true, nil
}

// loadRules reads the enabled rules in evaluation order, `priority DESC, id ASC`
// (SPEC §2, and the order capture_rules_eval_idx is built for).
//
// Evaluate does not depend on this order — it sorts what it is given — but the
// query is spelled in it anyway so that a human reading `opsctl capture-rules
// list` and a human reading this file see the same sequence.
func loadRules(ctx context.Context, pool *pgxpool.Pool) ([]storedRule, error) {
	rows, err := pool.Query(ctx,
		`SELECT r.id, p.slug, p.name, r.criteria_type, r.pattern, r.key_regex, r.priority, r.enabled,
		        r.project_id, COALESCE(r.subproject,''), COALESCE(r.external_system,''),
		        COALESCE(r.url_template,''), p.ticket_assignee_gate, r.revive, r.addressed,
		        r.pr_review, r.exclude_pr_authors, p.notifier_senders, r.comm_task
		   FROM capture_rules r
		   JOIN projects p ON p.id = r.project_id
		  WHERE r.enabled
		  ORDER BY r.priority DESC, r.id ASC`)
	if err != nil {
		return nil, fmt.Errorf("select capture rules: %w", err)
	}
	defer rows.Close()

	var out []storedRule
	for rows.Next() {
		var s storedRule
		if err := rows.Scan(&s.rule.ID, &s.rule.Project, &s.projectName, &s.rule.Kind, &s.rule.Pattern,
			&s.rule.ExternalKeyRegex, &s.rule.Priority, &s.rule.Enabled,
			&s.projectID, &s.subproject, &s.extSystem, &s.urlTemplate, &s.gateOn,
			&s.revive, &s.addressed, &s.prReview, &s.excludePRAuthors, &s.notifiers, &s.commTask); err != nil {
			return nil, fmt.Errorf("scan capture rule: %w", err)
		}
		// Rule.Source is the evaluator's carrier for `external_system` (Evaluate
		// deliberately never reads it). Populated so a loaded Rule is faithful to
		// its row and the driver's copy cannot drift from it; the driver itself
		// reads storedRule.extSystem, which it needs alongside project id,
		// subproject and url_template anyway.
		if s.extSystem != "" {
			system := s.extSystem
			s.rule.Source = &system
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate capture rules: %w", err)
	}
	return out, nil
}

// pendingMessages is the queue-as-filter (SPEC §6): INBOUND messages with no
// mode='live' decision row, oldest first.
//
// `direction='inbound'` is invariant 5 and the single most important line in the
// pass — it mirrors internal/triage/store.go. Without it a Jira comment
// switchboard itself posted, quoting LHH-23637, would spawn a task about itself.
//
// The Source the evaluator matches `source_slack_workspace` against is
// `source_accounts.account_email`, resolved through the raw item. That is SPEC §1's
// deliberate choice over prefix-matching `slack:{ws}:` on the thread key: the
// account link survives a thread-key format change, and a rule that silently stops
// matching after a format change is the SWT-13 landmine in another costume.
func pendingMessages(ctx context.Context, pool *pgxpool.Pool, cfg RulesConfig) ([]pendingMessage, error) {
	var since *time.Time
	if cfg.Horizon > 0 {
		t := time.Now().Add(-cfg.Horizon)
		since = &t
	}

	q := `SELECT ` + pendingMessageCols + pendingMessageFrom + `
	       WHERE m.direction = 'inbound'
	         AND ($1::timestamptz IS NULL OR COALESCE(m.sent_at, m.created_at) >= $1)`
	if !cfg.everything {
		q += `
	         AND NOT EXISTS (
	           SELECT 1 FROM capture_decisions g
	            WHERE g.message_id = m.id AND g.mode IN ('gate', 'route'))`
	}
	// SWT-40 D-D2 / B6, the shadow-overwrite guard: a message the gate resolved
	// or the routing tier routed is excluded in EVERY mode, --all included.
	// Every latest-decision reader follows ORDER BY id DESC, so a newer shadow
	// row would bury the gate's resolution or the route — the message's current
	// attribution — for all of them. Accepted residual (B-D5): a rule added
	// after routing does not re-point a routed message.
	if !cfg.All {
		// Skip messages this MODE has already decided — not just live ones.
		//
		// The original spelling was `cd.mode = 'live'`, which is correct for a
		// live pass and catastrophic for a shadow one: in shadow no live rows are
		// ever written, so nothing was ever excluded and EVERY pass re-evaluated
		// the entire inbound corpus. Measured on production at the time of
		// writing: 49,415 inbound messages, ~65 MB of body_text loaded into one
		// slice per pass, one INSERT round-trip each — times four connector mains
		// on */15, plus google's watch loop firing on every IMAP IDLE wake. Order
		// of 10^7 capture_decisions rows a day before a single task exists, and
		// the report then scans that table seven times.
		//
		// Neither the unit suite nor `make integration` can see this: the fixture
		// corpus is ten messages. It is a production-only failure of the kind
		// that arrives as a disk alert.
		//
		// Keying on cfg.Mode rather than "any decision at all" is deliberate and
		// load-bearing: "any decision" would break the shadow -> live transition,
		// because after a shadow period every message carries a shadow row and a
		// first live pass would decide NOTHING. Per-mode, a live pass still sees
		// the whole corpus exactly once, which is what going live means.
		//
		// The cost is that a message decided under a rule set that has since
		// changed is not automatically re-decided. That is what --all is for, and
		// the runbook says to run it after changing rules.
		q += `
	         AND NOT EXISTS (
	           SELECT 1 FROM capture_decisions cd
	            WHERE cd.message_id = m.id AND cd.mode = $2)`
	}
	q += `
	       ORDER BY m.sent_at, m.id`
	// $1 is the horizon; $2 is the mode, present only when the skip filter is on
	// (--all drops it). LIMIT's placeholder therefore has to be computed rather
	// than hard-coded, or it silently collides with the mode parameter.
	args := []any{since}
	if !cfg.All {
		args = append(args, string(cfg.Mode))
	}
	if cfg.Limit > 0 {
		q += fmt.Sprintf(" LIMIT $%d", len(args)+1)
		args = append(args, cfg.Limit)
	}

	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("select pending messages for capture rules: %w", err)
	}
	defer rows.Close()

	var out []pendingMessage
	for rows.Next() {
		pm, err := scanPendingMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, pm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending messages: %w", err)
	}
	return out, nil
}

// pendingMessageCols and pendingMessageFrom are the ONE projection of the
// message capture evaluates (SWT-74 criterion 21): pendingMessages runs it for
// a pass and ExplainMessage for a question, so the explainer can never read a
// message differently from the pass that decides it. Aliases: m, ri, sa, nt.
const (
	pendingMessageCols = `m.id, m.raw_source_item_id, m.thread_id, COALESCE(sa.account_email,''),
	             COALESCE(nt.thread_key,''), COALESCE(m.sender,''), COALESCE(m.subject,''),
	             COALESCE(m.body_text,''), COALESCE(m.external_message_id,''),
	             COALESCE(nt.participants,'[]'::jsonb), COALESCE(m.channel,''),
	             COALESCE(m.sent_at, m.created_at),
	             COALESCE(ri.raw_json->'conversation'->>'type','')`
	pendingMessageFrom = `
	        FROM normalized_messages m
	        LEFT JOIN raw_source_items ri ON ri.id = m.raw_source_item_id
	        LEFT JOIN source_accounts sa ON sa.id = ri.source_account_id
	        LEFT JOIN normalized_threads nt ON nt.id = m.thread_id`
)

// scanPendingMessage is the ONE scan of pendingMessageCols: it is where the
// participants and the channel are folded in.
func scanPendingMessage(row interface{ Scan(dest ...any) error }) (pendingMessage, error) {
	var pm pendingMessage
	var participants []byte
	if err := row.Scan(&pm.msg.ID, &pm.rawItemID, &pm.threadID, &pm.msg.Source, &pm.msg.ThreadKey,
		&pm.msg.Sender, &pm.msg.Subject, &pm.msg.BodyText, &pm.msg.ExternalMessageID,
		&participants, &pm.channel, &pm.sentAt, &pm.rawConvType); err != nil {
		return pm, fmt.Errorf("scan pending message: %w", err)
	}
	pm.msg.Participants = parseThreadParticipants(participants)
	return pm, nil
}

// parseParticipants reads normalized_threads.participants, a JSON array of
// people.id (upworkcrm/sink.go is the one sink that populates it today).
//
// A shape this cannot read is an empty participant list, not an error: the column
// is '[]' for 16,959 of 16,985 threads and a future sink writing objects instead of
// ids must not take every connector run down.
func parseThreadParticipants(raw []byte) []int64 {
	if len(raw) == 0 {
		return nil
	}
	var ids []int64
	if err := json.Unmarshal(raw, &ids); err != nil {
		return nil
	}
	return ids
}

// decideMessage turns one message into one capture_decisions row, calling Evaluate for
// every routing question and answering none of them itself.
//
// It returns the decision and the winning rule (zero storedRule when unmatched).
// mode only words the reason: a live pass "requests" a revive, a shadow pass
// says it "would" (SWT-45 criterion 25); the decision itself is mode-free.
//
// sim is DryRunRules' simulated refs (nil on every pass that writes): a ref
// the database does not hold but the dry run "created" earlier in its window.
func decideMessage(ctx context.Context, pool *pgxpool.Pool, mode string, pm pendingMessage,
	rules []Rule, byID map[int64]storedRule, sim simulatedRefs) (ruleDecision, storedRule, error) {
	d, winner, err := decideMessageInner(ctx, pool, mode, pm, rules, byID, sim)
	if err != nil || d.deferred || d.action != actionAttributed {
		return d, winner, err
	}
	// SWT-79 D1: ONE post-decision step, on every `attributed` exit whatever
	// the project — outside the inner decision, so prFallThrough's recursion
	// never applies it twice.
	if flag, why := channelUnmentioned(channelMentionInput{
		slack:     pm.channel == slackweb.Channel,
		dm:        slackweb.IsDirectMessageKey(pm.msg.ThreadKey),
		groupDM:   pm.rawConvType == "group_dm",
		mentioned: slackweb.MentionsOwner(pm.msg.BodyText),
	}); flag {
		d.channelUnmentioned = true
		d.reason += why
	}
	return d, winner, nil
}

// decideMessageInner is the decision itself; decideMessage adds the SWT-79
// mention fact on top. prFallThrough recurses into THIS, not decideMessage.
func decideMessageInner(ctx context.Context, pool *pgxpool.Pool, mode string, pm pendingMessage,
	rules []Rule, byID map[int64]storedRule, sim simulatedRefs) (ruleDecision, storedRule, error) {
	outcome := evaluateAll(pm.msg, rules)
	d := ruleDecision{action: actionUnmatched, matchedRuleIDs: outcome.matchedIDs, ambiguous: outcome.ambiguous}
	if !outcome.matched {
		d.reason = "no enabled rule matched"
		return d, storedRule{}, nil
	}

	winner, ok := byID[outcome.winnerID]
	if !ok {
		// Unreachable: `rules` is built from `byID`. Refusing beats attributing a
		// message to a project no loaded rule names.
		return d, storedRule{}, fmt.Errorf("capture rules: matched rule %d is not in the loaded set", outcome.winnerID)
	}
	projectID := winner.projectID
	d.projectID = &projectID
	ruleID := winner.rule.ID
	d.matchedRuleID = &ruleID
	d.action = actionAttributed

	switch {
	case winner.extSystem == "":
		// SPEC §3's scope boundary, and load-bearing rather than an oversight: the
		// Treetop workspace catch-all covers 59% of the corpus, and a task per
		// thread there would manufacture exactly the backlog this engine exists to
		// avoid. Turning arbitrary chatter into tasks is triage's job.
		d.reason = fmt.Sprintf("rule %d (%s) attributes to %s; no external_system, so attribution only",
			winner.rule.ID, winner.rule.Kind, winner.rule.Project)
		// SWT-78: a person's DM does not stop at attribution — that sent it to
		// the inquiry lane, where qwen dropped 16 of 22 on 2026-09-22. It is
		// decided HERE onto its conversation's task, and a task/task_log live
		// decision keeps it out of both inquiry inboxes by construction.
		if ok, why := directConversationTask(directFacts(pm, winner)); ok {
			return decideDirect(ctx, pool, mode, pm, d, winner, sim, why)
		}
		return d, winner, nil
	case outcome.externalKey == "":
		// An empty key must never become an external_refs row: external_key='' would
		// collide every keyless message of that system onto ONE task, forever.
		d.reason = fmt.Sprintf("rule %d (%s) attributes to %s; external_system %s but no key could be derived",
			winner.rule.ID, winner.rule.Kind, winner.rule.Project, winner.extSystem)
		// SWT-78 (codex review): every exit that leaves a DM merely attributed
		// sends it to the inquiry lane, so each one takes the DM path.
		if ok, why := directConversationTask(directFacts(pm, winner)); ok {
			return decideDirect(ctx, pool, mode, pm, d, winner, sim, why)
		}
		return d, winner, nil
	}

	system, key := winner.extSystem, outcome.externalKey

	// SWT-54 D2 point 3: ONE spelling of a GitHub PR key. key_regex returns one
	// capture group, so a thread-root rule captures the PATH form
	// `{owner}/{repo}/pull/{N}`; every github-derived key is canonicalized to
	// the connector's `{owner}/{repo}#{N}`, and one that does not parse (an
	// issue, a commit) is attribution only, never a ref.
	var prRef github.PRRef
	if system == "github" {
		ref, ok := github.ParsePRRef(key)
		if !ok {
			d.reason = fmt.Sprintf("rule %d (%s) attributes to %s; github key %q is not a pull request, so "+
				"attribution only", winner.rule.ID, winner.rule.Kind, winner.rule.Project, key)
			if ok, why := directConversationTask(directFacts(pm, winner)); ok {
				return decideDirect(ctx, pool, mode, pm, d, winner, sim, why)
			}
			return d, winner, nil
		}
		prRef, key = ref, github.PRKey(ref)
	}
	d.extSystem, d.extKey = &system, &key
	if winner.prReview && system == "github" {
		// SWT-54 D1 amendment (2026-09-14): the origin check comes before ANY
		// pr_review action — create, log, close. A mail anyone could have sent
		// (GitHub-shaped Message-ID, made-up X-GitHub-* headers) falls through
		// to the next rule exactly like his own PR's mail, so it keeps today's
		// decision.
		trusted, why, err := prMailTrusted(ctx, pool, pm, prRef)
		if err != nil {
			return d, winner, err
		}
		if !trusted {
			d.prRuleID, d.prKey, d.prNumber = winner.rule.ID, key, prRef.PR
			fell, fellWinner, err := prFallThrough(ctx, pool, mode, pm, rules, byID, sim, outcome, d, winner,
				fmt.Sprintf("rule %d skipped: untrusted GitHub mail for PR %s (%s); ", winner.rule.ID, key, why))
			if err != nil {
				return fell, fellWinner, err
			}
			fell.prUntrusted = true
			return fell, fellWinner, nil
		}
		d.prRuleID, d.prKey, d.prNumber = winner.rule.ID, key, prRef.PR
		if state, ok := github.PRStateNotice(pm.msg.BodyText, prRef.PR); ok {
			d.prNotice = state
		}
	}

	// SWT-45 J1: is this match activity that revives a closed task or surfaces
	// a new one? The flags and the gate are all COLUMNS, loaded with the rules.
	// Only a jira-keyed match can be activity: the Part D hold below keys on
	// system == "jira", so a reviving rule on any other system would slip past
	// a gated project's assignee check. capture_rule_add refuses such a rule;
	// this keeps one stored some other way inert.
	activity := system == "jira" && overrides(winner.revive, winner.addressed, winner.gateOn)
	requested := "revive requested"
	surfacing := "surface requested"
	if mode != RulesModeLive {
		requested, surfacing = "would revive", "would surface"
	}

	if system == "jira" && winner.gateOn && !activity {
		// SWT-40 D-D1: the ticket is checked against Jira BEFORE a task exists
		// (O5), and capture never calls Jira itself — the lookup credential
		// lives only where the gate stage runs. So a would-be task or task_log
		// on a gated project is held: project, rule and key recorded, nothing
		// created. The pipelined gate stage resolves it (gate.go). The gate is
		// the COLUMN, loaded with the rules (the SWT-21 "test the column" rule).
		//
		// SWT-45 decision 3 (refined): a match by an ADDRESSED rule ("X
		// mentioned you on K", "X assigned K to you") overrides the gate and is
		// NOT held — on a gated project overrides() is exactly `addressed`. All
		// other activity on a gated ticket, revive-only rules included, still
		// follows the gate, and the gate's own resolution never revives or
		// surfaces (the SPEC's Part D section).
		d.action = actionHeld
		d.reason = fmt.Sprintf("rule %d (%s): %s %s on %s, whose ticket assignee gate is on; held for the "+
			"gate stage's Jira lookup", winner.rule.ID, winner.rule.Kind, system, key, winner.rule.Project)
		return d, winner, nil
	}

	// The dry run's simulated refs win over the database: a ref it "created",
	// or a task it "closed", earlier in its window.
	var existing refTask
	var found bool
	if sim != nil {
		existing, found = sim.lookup(system, key)
	}
	if !found {
		var err error
		existing, found, err = taskForExternalRef(ctx, pool, system, key)
		if err != nil {
			return d, winner, err
		}
	}
	if found {
		taskID := existing.taskID
		d.action = actionTaskLog
		d.taskID = &taskID
		d.reason = fmt.Sprintf("rule %d (%s): %s %s already linked to task %d; append a log",
			winner.rule.ID, winner.rule.Kind, system, key, taskID)
		if d.prNotice != "" {
			// SWT-54 D5 (OQ-2 = a): GitHub's merge/close notice closes the
			// review task, log first. A closed task (Done or dismissed) is not
			// closed again, and a PR state notice NEVER reopens a review task
			// (owner decision 2026-09-14): SWT-36's dismissal reopen below is
			// suppressed for it, so the notice is logged and nothing else changes.
			// A "Reopened #N." notice is a state notice too: logged only.
			d.reason += fmt.Sprintf("; PR #%d %s on GitHub", prRef.PR, d.prNotice)
			switch {
			case !github.PRStateEndsPR(d.prNotice):
				d.reason += "; logged only (a reopened notice never closes a task)"
			case existing.status == "closed":
				d.reason += fmt.Sprintf("; task %d is already closed", taskID)
			default:
				d.prClose = true
				if mode != RulesModeLive {
					d.reason += "; would close"
				} else {
					d.reason += "; close requested"
				}
			}
			if existing.dismissalID != 0 {
				d.reason += fmt.Sprintf("; a PR state notice never reopens a review task (dismissal %d stays open)",
					existing.dismissalID)
			}
		}
		switch {
		case activity && existing.status == "closed":
			// SWT-45 J9: activity on a CLOSED task revives it; the handler
			// decides (ingested after the close, an open dismissal included).
			// Activity on an OPEN task only logs (J10), or the ticket-closed
			// email would pin every done ticket's task open.
			verdict, note, err := ownActionGuard(ctx, pool, pm, key)
			if err != nil {
				return d, winner, err
			}
			switch verdict {
			case ownActionDefer:
				d.deferred = true
				d.reason += note
			case ownActionSkip:
				// J17: his own action. Log only — neither the revive nor
				// SWT-36's dismissal reopen: the mail is not new activity.
				d.reason += note + "; revive skipped"
			default:
				d.revive = true
				d.blind = verdict == ownActionBlind
				d.reason += fmt.Sprintf("; task %d is closed and rule %d is activity; %s", taskID, winner.rule.ID, requested) + note
			}
		case existing.dismissalID != 0 && d.prNotice == "":
			d.dismissalID = existing.dismissalID
			d.reason += fmt.Sprintf("; task %d was dismissed (%s); reopen requested against dismissal %d",
				taskID, existing.dismissalCode, existing.dismissalID)
		}
		// chat-on-closed-task CC3: does this log onto a CLOSED task resurface
		// the message through the inquiry lane? Decided here, where the status,
		// the activity flag and the dismissal are already known, and RECORDED on
		// the decision row. The reason fragment is added only for a closed task,
		// so every open-task reason reads exactly as before.
		resurface, why := resurfaces(resurfaceInput{
			status:        existing.status,
			activity:      activity,
			dismissed:     existing.dismissalID != 0,
			connectorCopy: pm.channel == jira.Channel,
			notifier:      notifierSender(pm.msg.Sender, winner.notifiers),
			blankSender:   blankSender(pm.msg.Sender),
			prNotice:      d.prNotice != "",
		})
		d.resurface = resurface
		// SWT-78 item E: a person's DM that would resurface onto a CLOSED
		// ticket task goes to its conversation task instead — resurfacing is
		// the inquiry lane, and no DM goes to qwen. The closed ticket gets
		// nothing (no log, no reopen): the DM is about the conversation.
		if resurface {
			if ok, why := directConversationTask(directFacts(pm, winner)); ok {
				base := ruleDecision{action: actionAttributed, projectID: d.projectID,
					matchedRuleID: d.matchedRuleID, matchedRuleIDs: d.matchedRuleIDs, ambiguous: d.ambiguous,
					reason: fmt.Sprintf("rule %d (%s): %s %s is linked to closed task %d; a DM does not resurface",
						winner.rule.ID, winner.rule.Kind, system, key, taskID)}
				return decideDirect(ctx, pool, mode, pm, base, winner, sim, why)
			}
		}
		// comms-inbox (SWT-74 D2): does this attach make its own comm task?
		// Decided from VALUES — the rule's column, the task's status, the three
		// sender facts — and worded by mode: a live pass REQUESTS, a shadow pass
		// says what it WOULD do (SWT-45 criterion 25's pattern). Every outcome
		// names its cause on the decision row, the un-armed one included, so a
		// smoke read of capture_decisions can tell them apart (criterion 44).
		comm, commWhy := commTask(commInput{
			armed:       winner.commTask,
			status:      existing.status,
			blankSender: blankSender(pm.msg.Sender),
			notifier:    notifierSender(pm.msg.Sender, winner.notifiers),
			ownJiraEdit: anonymousJiraActor(pm.msg.Sender),
		})
		// `&& d.prNotice == ""` is defence in depth only: 0040's CHECK and
		// capture_rule_add both refuse comm_task with pr_review, so a PR notice
		// can never reach an armed rule. Kept so the invariant is visible here.
		d.comm = comm && d.prNotice == ""
		switch {
		case !d.comm:
			d.reason += "; " + commWhy
		case mode != RulesModeLive:
			d.reason += "; " + commWhy + "; would create a comm task"
		default:
			d.reason += "; " + commWhy + "; comm task requested"
		}
		if existing.status == "closed" {
			d.reason += "; " + why
		}
		return d, winner, nil
	}
	if d.prKey != "" {
		return decidePRReviewCreate(ctx, pool, mode, pm, rules, byID, sim, outcome, d, winner, prRef)
	}
	d.action = actionTask
	d.reason = fmt.Sprintf("rule %d (%s): first message for %s %s on %s; create one task",
		winner.rule.ID, winner.rule.Kind, system, key, winner.rule.Project)
	if activity {
		verdict, note, err := ownActionGuard(ctx, pool, pm, key)
		if err != nil {
			return d, winner, err
		}
		switch verdict {
		case ownActionDefer:
			d.deferred = true
			d.reason += note
		case ownActionSkip:
			// J17: no task for his own action. Attribution only, so no other
			// rule can pick the message up: first match already won.
			d.action = actionAttributed
			d.reason += note + "; no task created"
		default:
			d.surface = true
			d.blind = verdict == ownActionBlind
			d.reason += fmt.Sprintf("; rule %d is activity; %s", winner.rule.ID, surfacing) + note
		}
	}
	return d, winner, nil
}

// decidePRReviewCreate is a pr_review rule's CREATE branch (SWT-54 D1, D2, D5):
// no ref exists for the PR yet.
//
//   - own / excluded: the message is re-decided WITHOUT that rule (the next rule
//     in the matched list, else unmatched), so his own PR's mail gets exactly
//     today's decision. The fallen-to decision keeps matched_rule_ids and
//     ambiguity from the FULL evaluation, and its reason is prefixed
//     "rule R skipped: PR {key} authored by him ({evidence}); ".
//   - a merge/close notice as the first mail seen: attributed, no task (a
//     review of merged work is not work).
//   - other / undetermined: one review task (undetermined is fail-open).
func decidePRReviewCreate(ctx context.Context, pool *pgxpool.Pool, mode string, pm pendingMessage,
	rules []Rule, byID map[int64]storedRule, sim simulatedRefs, outcome rulesEvaluation,
	d ruleDecision, winner storedRule, ref github.PRRef) (ruleDecision, storedRule, error) {
	facts, err := prReviewFacts(ctx, pool, pm, ref)
	if err != nil {
		return d, winner, err
	}
	v := decidePRAuthor(facts, winner.excludePRAuthors)
	d.prVerdict = &v

	switch v.verdict {
	case prAuthorOwn, prAuthorExcluded:
		prefix := fmt.Sprintf("rule %d skipped: PR %s authored by him (%s: %s); ", winner.rule.ID, d.prKey, v.verdict, v.evidence)
		if v.verdict == prAuthorExcluded {
			prefix = fmt.Sprintf("rule %d skipped: PR %s excluded author (%s): %s; ", winner.rule.ID, d.prKey, v.entry, v.evidence)
		}
		fell, fellWinner, err := prFallThrough(ctx, pool, mode, pm, rules, byID, sim, outcome, d, winner, prefix)
		if err != nil {
			return fell, fellWinner, err
		}
		fell.prSkipped = true
		fell.prVerdict = d.prVerdict
		if fell.prNotice == "" {
			fell.prNotice = d.prNotice
		}
		return fell, fellWinner, nil
	}

	var author string
	switch {
	case v.verdict == prAuthorUndetermined:
		author = fmt.Sprintf("author undetermined (%s); created fail-open", v.evidence)
	case v.author == "":
		author = fmt.Sprintf("author not named (%s)", v.evidence)
	default:
		author = fmt.Sprintf("author %s (%s)", v.author, v.evidence)
	}
	// Only a merged/closed notice makes the first mail seen a no-task: a
	// reopened PR is open again, so its review is work (created like any mail).
	if github.PRStateEndsPR(d.prNotice) {
		d.action = actionAttributed
		d.reason = fmt.Sprintf("rule %d (%s): %s %s on %s: PR #%d %s on GitHub before any review task existed; "+
			"PR already merged/closed; no review task; %s", winner.rule.ID, winner.rule.Kind, *d.extSystem, d.prKey,
			winner.rule.Project, ref.PR, d.prNotice, author)
		return d, winner, nil
	}
	d.action = actionTask
	d.reason = fmt.Sprintf("rule %d (%s): first message for %s %s on %s; create one review task; %s",
		winner.rule.ID, winner.rule.Kind, *d.extSystem, d.prKey, winner.rule.Project, author)
	return d, winner, nil
}

// prFallThrough re-decides pm WITHOUT the pr_review rule that won (SWT-54 D2's
// fall-through, shared by the own/excluded verdict and the untrusted-mail
// origin check). The fallen-to decision keeps matched_rule_ids and ambiguity
// from the FULL evaluation, its reason is prefix + the fallen-to reason, and it
// carries the PR rule's id, key and number (so the dry run can roll it up).
func prFallThrough(ctx context.Context, pool *pgxpool.Pool, mode string, pm pendingMessage,
	rules []Rule, byID map[int64]storedRule, sim simulatedRefs, outcome rulesEvaluation,
	d ruleDecision, winner storedRule, prefix string) (ruleDecision, storedRule, error) {
	fell, fellWinner, err := decideMessageInner(ctx, pool, mode, pm, withoutRule(rules, winner.rule.ID), byID, sim)
	if err != nil {
		return fell, fellWinner, err
	}
	fell.matchedRuleIDs = outcome.matchedIDs
	fell.ambiguous = outcome.ambiguous
	fell.reason = prefix + fell.reason
	fell.prRuleID, fell.prKey, fell.prNumber = d.prRuleID, d.prKey, d.prNumber
	return fell, fellWinner, nil
}

// ownActionGuard runs the own-action guard (SWT-45 J17, ownaction.go) for an
// activity match that would revive or create, and returns its verdict with the
// reason suffix the decision records ("" when clear). A blind verdict is also
// logged: the guard wanted a fact it never got.
func ownActionGuard(ctx context.Context, pool *pgxpool.Pool, pm pendingMessage, key string) (string, string, error) {
	obs, hit, err := ownActionFacts(ctx, pool, key, pm)
	if err != nil {
		return "", "", err
	}
	from, to := ownActionWindow(pm.sentAt)
	verdict := decideOwnAction(obs)
	switch verdict {
	case ownActionNotApplicable:
		return verdict, fmt.Sprintf("; own-action guard not applicable (no jira poller covers %s)", key), nil
	case ownActionNamed:
		note := fmt.Sprintf("; own-action guard: actor named (%q, not his), proceeding without waiting", obs.actor)
		if obs.found {
			note += fmt.Sprintf("; his outbound message %d (%q) is inside [%s, %s] but the email names another actor",
				hit.messageID, hit.sender, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
		}
		return verdict, note, nil
	case ownActionSkip:
		return verdict, fmt.Sprintf("; %s: outbound message %d on %s sent %s, inside [%s, %s] around this message",
			ownActionSkip, hit.messageID, hit.threadKey, hit.sentAt.UTC().Format(time.RFC3339),
			from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339)), nil
	case ownActionDefer:
		return verdict, fmt.Sprintf("; own-action guard deferred: %s not known synced past %s (waited %s of %s)",
			key, ownActionSyncedPast(pm.sentAt).UTC().Format(time.RFC3339), obs.waited.Round(time.Second), OwnActionMaxWait), nil
	case ownActionBlind:
		note := fmt.Sprintf("; own-action guard ran BLIND: %s still not known synced past %s after %s",
			key, ownActionSyncedPast(pm.sentAt).UTC().Format(time.RFC3339), OwnActionMaxWait)
		log.Printf("capture rules: message %d%s", pm.msg.ID, note)
		return verdict, note, nil
	}
	return verdict, "", nil
}

// ownActionHit is the outbound message that made a match his own action.
type ownActionHit struct {
	messageID int64
	threadKey string
	sentAt    time.Time
	sender    string // normalized_messages.sender: his Jira display name, as the connector stores it
}

// ownActionFacts reads the own-action guard's inputs for key K and message pm
// (SWT-45 J17). Reads only; every instant it binds comes from ownaction.go.
//
//   - pollers: provider='jira' accounts whose scopes claim K's prefix
//     (ticketstatus.KeyPrefix, RouteLookup's rule). Only a poller stores his
//     comments, on `jira:{site_host}:{K}` (jira.SiteHost of domain_default,
//     the connector's own thread-key spelling).
//   - found: an OUTBOUND message on one of those threads with sent_at inside
//     ownActionWindow. Both predicates are the guard: an inbound comment in
//     the window is someone else's, an outbound one outside it is old.
//   - fresh: EVERY such poller has an 'ok' sync_runs row that STARTED after
//     ownActionSyncedPast (its JQL then covered the comment), and no raw row of
//     K under it awaits normalization (a comment fetched but not yet a message).
//   - waited: now() minus the message's ingest, on the database clock.
func ownActionFacts(ctx context.Context, pool *pgxpool.Pool, key string, pm pendingMessage) (ownActionObservation, ownActionHit, error) {
	obs := ownActionObservation{actor: namedJiraActor(pm.msg.Sender)}
	var hit ownActionHit
	prefix, ok := ticketstatus.KeyPrefix(key)
	if !ok {
		return obs, hit, nil
	}
	rows, err := pool.Query(ctx,
		`SELECT id, COALESCE(domain_default,''), scopes FROM source_accounts WHERE provider = 'jira' ORDER BY id`)
	if err != nil {
		return obs, hit, fmt.Errorf("own-action guard: select jira pollers: %w", err)
	}
	var pollers []int64
	var threads []string
	for rows.Next() {
		var id int64
		var domain string
		var scopes []string
		if err := rows.Scan(&id, &domain, &scopes); err != nil {
			rows.Close()
			return obs, hit, fmt.Errorf("own-action guard: scan jira poller: %w", err)
		}
		for _, s := range scopes {
			if s == prefix {
				pollers = append(pollers, id)
				threads = append(threads, "jira:"+jira.SiteHost(domain)+":"+key)
				break
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return obs, hit, fmt.Errorf("own-action guard: iterate jira pollers: %w", err)
	}
	obs.pollers = len(pollers)
	if obs.pollers == 0 {
		return obs, hit, nil
	}

	from, to := ownActionWindow(pm.sentAt)
	err = pool.QueryRow(ctx,
		`SELECT o.id, nt.thread_key, o.sent_at, COALESCE(o.sender,'')
		   FROM normalized_messages o
		   JOIN normalized_threads nt ON nt.id = o.thread_id
		  WHERE nt.thread_key = ANY($1)
		    AND o.direction = 'outbound'
		    AND o.sent_at >= $2 AND o.sent_at <= $3
		  ORDER BY o.sent_at, o.id LIMIT 1`, threads, from, to).Scan(&hit.messageID, &hit.threadKey, &hit.sentAt, &hit.sender)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return obs, hit, fmt.Errorf("own-action guard: look for his action on %s: %w", key, err)
	default:
		obs.found = true
		obs.hisName = hit.sender
	}
	if _, settled := ownActionSettled(obs); settled {
		// A named-other actor, or his outbound message in the window: the
		// verdict is already fixed, so the freshness reads below are skipped.
		return obs, hit, nil
	}

	var stale int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM unnest($1::bigint[]) AS p(id)
		  WHERE NOT EXISTS (SELECT 1 FROM sync_runs r
		                     WHERE r.source_account_id = p.id AND r.status = 'ok' AND r.started_at > $2)
		     OR EXISTS (SELECT 1 FROM raw_source_items ri
		                 WHERE ri.source_account_id = p.id AND ri.normalized_at IS NULL
		                   AND (ri.external_id = $3 OR ri.external_id LIKE $4))`,
		pollers, ownActionSyncedPast(pm.sentAt), jira.IssueRawID(key), jira.CommentRawIDPrefix(key)+"%").Scan(&stale); err != nil {
		return obs, hit, fmt.Errorf("own-action guard: read poller freshness for %s: %w", key, err)
	}
	obs.fresh = stale == 0

	var secs float64
	if err := pool.QueryRow(ctx,
		`SELECT EXTRACT(EPOCH FROM (now() - created_at))::float8 FROM normalized_messages WHERE id = $1`,
		pm.msg.ID).Scan(&secs); err != nil {
		return obs, hit, fmt.Errorf("own-action guard: read ingest time of message %d: %w", pm.msg.ID, err)
	}
	obs.waited = time.Duration(secs * float64(time.Second))
	return obs, hit, nil
}

// evaluateAll records every rule that matched, not just the winner, by calling
// Evaluate repeatedly over the rules it has not yet claimed.
//
// Matching is NOT re-implemented here — that is the whole point of §2, and doing
// it a second way is how two spellings of one rule drift apart. Rules are removed
// by ID rather than by slice position because Evaluate sorts what it is given, so
// "everything after the winner" is not a statement about the caller's slice.
//
// Ambiguity is recorded and reported, never used to change the outcome: total and
// reproducible beats clever. `ambiguous` is true when two matched rules name
// DIFFERENT projects — it is the only report of a routing collision.
func evaluateAll(msg Message, rules []Rule) rulesEvaluation {
	remaining := make([]Rule, len(rules))
	copy(remaining, rules)

	out := rulesEvaluation{matchedIDs: []int64{}}
	winnerProject := ""
	for len(remaining) > 0 {
		m := Evaluate(msg, remaining)
		if m.Rule == nil {
			break
		}
		// Read the Match into VALUES immediately. Match.Rule is a pointer, and
		// nothing in the contract says whether it points into the caller's slice or
		// into a copy Evaluate made; holding it across the next call would be a bet
		// on that.
		id, project := m.Rule.ID, m.Project
		out.matchedIDs = append(out.matchedIDs, id)
		if !out.matched {
			out.matched = true
			out.winnerID = id
			out.externalKey = m.ExternalKey
			winnerProject = project
		} else if project != winnerProject {
			out.ambiguous = true
		}
		remaining = withoutRule(remaining, id)
	}
	return out
}

// rulesEvaluation is evaluateAll's answer in values, not pointers.
type rulesEvaluation struct {
	matched     bool
	winnerID    int64
	externalKey string
	matchedIDs  []int64
	ambiguous   bool
}

// withoutRule returns a NEW slice. Filtering in place would rewrite the backing
// array a previously returned Match may still point into.
func withoutRule(rules []Rule, id int64) []Rule {
	out := make([]Rule, 0, len(rules))
	for _, r := range rules {
		if r.ID != id {
			out = append(out, r)
		}
	}
	return out
}

// taskForExternalRef is the dedup lookup — the same query shape as
// github.PGTaskResolver.Resolve. It runs in BOTH modes: in shadow it finds nothing
// (shadow writes no external_refs), so shadow proposes one task per MESSAGE and
// the report is what collapses those to one per ticket. Deduping inside a shadow
// run instead would make the report's DISTINCT redundant and hide the volume the
// diff exists to show.
//
// SWT-36 criterion 13: it also returns the task's OPEN dismissal — present iff
// tasks.status='closed' AND a task_dismissals row with reopened_at IS NULL
// exists (D3). The partial unique index allows at most one open row per task,
// so the LEFT JOINs cannot multiply the ref row. A task whose dismissal was
// overtaken and which was then plain-closed carries none: plain-closed.
//
// SWT-45 criterion 19: it also returns the task's status, read from the
// column, so a revive is requested only for a CLOSED task.
func taskForExternalRef(ctx context.Context, pool *pgxpool.Pool, system, key string) (refTask, bool, error) {
	var rt refTask
	var dismissalID *int64
	var dismissalCode, status *string
	err := pool.QueryRow(ctx,
		`SELECT r.task_id, d.id, d.reason_code, t.status
		   FROM external_refs r
		   LEFT JOIN tasks t ON t.id = r.task_id
		   LEFT JOIN task_dismissals d ON d.task_id = t.id AND t.status = 'closed' AND d.reopened_at IS NULL
		  WHERE r.system = $1 AND r.external_key = $2
		  ORDER BY r.created_at DESC, r.id DESC LIMIT 1`, system, key).Scan(&rt.taskID, &dismissalID, &dismissalCode, &status)
	if status != nil {
		rt.status = *status
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return refTask{}, false, nil
	}
	if err != nil {
		return refTask{}, false, fmt.Errorf("resolve task for %s %s: %w", system, key, err)
	}
	if dismissalID != nil {
		rt.dismissalID = *dismissalID
	}
	if dismissalCode != nil {
		rt.dismissalCode = *dismissalCode
	}
	return rt, true, nil
}

// refTask is taskForExternalRef's answer: the linked task and, when it is
// dismissed, its open dismissal (0 / "" = none), and its status (SWT-45).
type refTask struct {
	taskID        int64
	dismissalID   int64
	dismissalCode string
	status        string
}

// insertDecision writes the capture_decisions row and reports whether it won the
// live claim.
//
// The ON CONFLICT predicate is RESTATED (`WHERE mode='live'`) because
// capture_decisions_live_uniq is a PARTIAL unique index: arbiter inference matches
// one only when the predicate is repeated, and omitting it raises "no unique or
// exclusion constraint matching the ON CONFLICT specification" at runtime, inside
// a CronJob. Same requirement as task_events_outbound_observed_uniq (0013).
//
// The row is written BEFORE the tool calls it describes, so the index is the claim
// rather than a record of one. A crash between the claim and the action leaves a
// decision with no task — visible in the report — where the reverse order would
// leave a task nothing remembers and a second run would append to it twice.
func insertDecision(ctx context.Context, pool *pgxpool.Pool, mode string,
	pm pendingMessage, d ruleDecision) (int64, bool, error) {
	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO capture_decisions
		   (message_id, raw_source_item_id, mode, matched_rule_id, project_id,
		    matched_rule_ids, ambiguous, action, external_system, external_key,
		    task_id, reason, resurface, channel_unmentioned)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		 ON CONFLICT (message_id) WHERE mode = 'live' DO NOTHING
		 RETURNING id`,
		pm.msg.ID, pm.rawItemID, mode, d.matchedRuleID, d.projectID,
		d.matchedRuleIDs, d.ambiguous, d.action, d.extSystem, d.extKey,
		d.taskID, ruleNullIfEmpty(d.reason), d.resurface, d.channelUnmentioned).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("record capture decision for message %d: %w", pm.msg.ID, err)
	}
	return id, true, nil
}

// recordDecisionTask fills in the task a live 'task' decision produced. Writing
// capture_decisions is this package's own log; tasks themselves are never touched
// here (invariant 3).
func recordDecisionTask(ctx context.Context, pool *pgxpool.Pool, decisionID, taskID int64) error {
	if _, err := pool.Exec(ctx,
		`UPDATE capture_decisions SET task_id = $1 WHERE id = $2`, taskID, decisionID); err != nil {
		return fmt.Errorf("record task %d on capture decision %d: %w", taskID, decisionID, err)
	}
	return nil
}

// createRuleTask creates the task through the executor (invariant 3): validate →
// policy check → audit start → handler → audit complete, so "which rule created
// this task, and when" is answerable from audit_events.
//
// Linking the external ref is a SEPARATE call, made by the driver after the task
// id is recorded on the decision row, so a failure to link can never leave a
// created task that no decision points at.
//
// status is not chosen here — create_task already inserts `ready`, which is what a
// human-resolved project wants.
func createRuleTask(ctx context.Context, ex *executor.Executor, actor string,
	pm pendingMessage, winner storedRule, system, key string) (int64, error) {
	args, err := json.Marshal(ruleCreateTaskArgs(pm, winner, system, key))
	if err != nil {
		return 0, fmt.Errorf("marshal create_task args for message %d: %w", pm.msg.ID, err)
	}
	res, err := ex.Execute(ctx, executor.Call{Tool: "create_task", Actor: actor, Args: args})
	if err != nil {
		return 0, fmt.Errorf("create task for %s %s (message %d): %w", system, key, pm.msg.ID, err)
	}
	var out struct {
		TaskID int64 `json:"task_id"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		return 0, fmt.Errorf("parse create_task result for %s %s: %w", system, key, err)
	}
	if out.TaskID == 0 {
		return 0, fmt.Errorf("create_task returned no task id for %s %s", system, key)
	}
	return out.TaskID, nil
}

// ruleCreateTaskArgs is create_task's argument object for a rule-created task,
// shared by createRuleTask and DryRunRules' D9 backfill payloads so the pasted
// call is byte-for-byte what a live pass would have made.
//
// A pr_review rule's title is prReviewTitle (SWT-54 D2 point 4); every other
// rule's is ruleTaskTitle, byte-identical to before.
func ruleCreateTaskArgs(pm pendingMessage, winner storedRule, system, key string) map[string]any {
	title := ruleTaskTitle(key, pm.msg, winner)
	if winner.prReview && system == "github" {
		if ref, ok := github.ParsePRRef(key); ok {
			title = prReviewTitle(ref, pm.msg.Subject, pm.msg.BodyText)
		}
	}
	return map[string]any{
		"project":       winner.rule.Project,
		"subproject":    winner.subproject,
		"title":         title,
		"body":          ruleTaskBody(pm, winner, system, key, 0),
		"assignee_type": "human",
		"priority":      0,
	}
}

// ruleExternalURL is the ref's URL. A github key's is github.PRURL (SWT-54:
// url_template is refused for github, because {key} in the canonical key
// contains '#'); every other system substitutes the rule's url_template.
func ruleExternalURL(winner storedRule, system, key string) string {
	if system == "github" {
		if ref, ok := github.ParsePRRef(key); ok {
			return github.PRURL(ref)
		}
		return ""
	}
	return externalURL(winner.urlTemplate, key)
}

// linkRuleRef writes the external_refs row that IS the dedup key.
//
// It fails loudly rather than continuing: without the ref, the next notification
// about the same ticket finds nothing and creates a SECOND task. The one live
// system this can hit today is a rule with external_system 'slack' or 'gmail' —
// migration 0015 widened external_refs.system's CHECK to accept them, but
// link_external_ref's own validator still enforces the pre-0015 three, so such a
// rule stores fine and refuses here on every message it matches.
func linkRuleRef(ctx context.Context, ex *executor.Executor, actor string,
	taskID int64, winner storedRule, system, key string) error {
	args, err := json.Marshal(map[string]any{
		"task_id":      taskID,
		"system":       system,
		"external_key": key,
		"external_url": ruleExternalURL(winner, system, key),
	})
	if err != nil {
		return fmt.Errorf("marshal link_external_ref args for task %d: %w", taskID, err)
	}
	if _, err := ex.Execute(ctx, executor.Call{
		Tool: "link_external_ref", Actor: actor, Args: args, TaskID: &taskID,
	}); err != nil {
		return fmt.Errorf("link %s %s to task %d: %w", system, key, taskID, err)
	}
	return nil
}

// setRuleProvenance records the conversation that raised the task
// (tasks.source_thread_id) via task_set_source_thread — the executor path, so
// rules_structure_test.go's ban on touching tasks directly holds and "who
// recorded this task's conversation" is answerable from audit_events.
//
// It fails loudly for the same reason linkRuleRef does: without the
// provenance the task can never be delivered (draft_delivery refuses it), and
// the failure is silent unless the pass stops. A message with no thread
// records nothing — provenance is an observation, never an invention.
func setRuleProvenance(ctx context.Context, ex *executor.Executor, actor string,
	taskID int64, pm pendingMessage) error {
	if pm.threadID == nil {
		return nil
	}
	args, err := json.Marshal(map[string]any{
		"task_id":   taskID,
		"thread_id": *pm.threadID,
	})
	if err != nil {
		return fmt.Errorf("marshal task_set_source_thread args for task %d: %w", taskID, err)
	}
	if _, err := ex.Execute(ctx, executor.Call{
		Tool: "task_set_source_thread", Actor: actor, Args: args, TaskID: &taskID,
	}); err != nil {
		return fmt.Errorf("record source thread %d on task %d: %w", *pm.threadID, taskID, err)
	}
	return nil
}

// appendRuleLog records a later notification about a ticket that already has a
// task — the five follow-ups that must read as five appended events rather than
// five more tasks.
func appendRuleLog(ctx context.Context, ex *executor.Executor, actor string,
	pm pendingMessage, taskID int64, system, key string) error {
	args, err := json.Marshal(map[string]any{
		"task_id": taskID,
		"kind":    "log",
		"message": fmt.Sprintf("capture: %s %s — %s message %d from %s: %s",
			system, key, ruleOrNone(pm.channel), pm.msg.ID, ruleOrNone(pm.msg.Sender),
			textmatch.NormalizedPrefix(rulesPreview(pm.msg), rulesPreviewLen)),
	})
	if err != nil {
		return fmt.Errorf("marshal task_append_log args for task %d: %w", taskID, err)
	}
	if _, err := ex.Execute(ctx, executor.Call{
		Tool: "task_append_log", Actor: actor, Args: args, TaskID: &taskID,
	}); err != nil {
		return fmt.Errorf("append capture log to task %d (message %d): %w", taskID, pm.msg.ID, err)
	}
	return nil
}

// reopenRuleTask is the guarded task_reopen (SWT-36 criterion 14) through the
// executor as the configured capture:{connector} actor: ids only — the handler
// reads both instants and the message direction from columns, under the row
// lock, and answers reopened:true or a skip. It fails the pass on error, the
// linkRuleRef policy: the live claim is spent, and a silently swallowed
// failure would leave the dismissed task down with nothing saying why.
func reopenRuleTask(ctx context.Context, ex *executor.Executor, actor string,
	pm pendingMessage, taskID, dismissalID int64, system, key string) (bool, error) {
	args, err := json.Marshal(map[string]any{
		"task_id":      taskID,
		"dismissal_id": dismissalID,
		"message_id":   pm.msg.ID,
		"reason": fmt.Sprintf("capture: new inbound %s message %d on %s %s",
			ruleOrNone(pm.channel), pm.msg.ID, system, key),
	})
	if err != nil {
		return false, fmt.Errorf("marshal task_reopen args for task %d: %w", taskID, err)
	}
	res, err := ex.Execute(ctx, executor.Call{
		Tool: "task_reopen", Actor: actor, Args: args, TaskID: &taskID,
	})
	if err != nil {
		return false, fmt.Errorf("reopen dismissed task %d (dismissal %d, message %d): %w",
			taskID, dismissalID, pm.msg.ID, err)
	}
	var out struct {
		Reopened bool `json:"reopened"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		return false, fmt.Errorf("parse task_reopen result for task %d: %w", taskID, err)
	}
	return out.Reopened, nil
}

// reviveRuleTask is the revive form of task_reopen (SWT-45 J6) through the
// executor as the configured capture:{connector} actor: ids only. The handler
// reads the message's direction and ingest time, the close record and any open
// dismissal from columns under the row lock, and answers reopened:true or a
// skip (not_closed, message_predates_close). An error fails the pass, the
// reopenRuleTask policy.
func reviveRuleTask(ctx context.Context, ex *executor.Executor, actor string,
	pm pendingMessage, taskID int64, system, key string) (bool, error) {
	args, err := json.Marshal(map[string]any{
		"task_id":    taskID,
		"message_id": pm.msg.ID,
		"revive":     true,
		"reason": fmt.Sprintf("capture: new inbound %s activity, message %d on %s %s",
			ruleOrNone(pm.channel), pm.msg.ID, system, key),
	})
	if err != nil {
		return false, fmt.Errorf("marshal task_reopen (revive) args for task %d: %w", taskID, err)
	}
	res, err := ex.Execute(ctx, executor.Call{
		Tool: "task_reopen", Actor: actor, Args: args, TaskID: &taskID,
	})
	if err != nil {
		return false, fmt.Errorf("revive closed task %d (message %d): %w", taskID, pm.msg.ID, err)
	}
	var out struct {
		Reopened bool `json:"reopened"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		return false, fmt.Errorf("parse task_reopen (revive) result for task %d: %w", taskID, err)
	}
	return out.Reopened, nil
}

// closeActiveWorkRefusal is internal/tools/close.go's ONE refusal phrase for
// active work (activeWorkRefusal there, pinned by ticketstatus's statusset
// test). A review task someone is working on is not closed out from under them
// by a merge notice: that refusal is a non-fatal skip, the ticketstatus
// precedent. Any other error fails the pass.
const closeActiveWorkRefusal = "refusing to close active work"

// closeRuleTask is SWT-54 D5's close: task_close through the executor as the
// configured capture:{connector} actor, AFTER the notice was logged, with the
// reason "PR #N merged on GitHub (message M)" (or closed). It writes no
// dismissal label, so it counts as Done. An active-work refusal is skipped: the
// pass logs it and appends it to the decision's reason (capture_decisions is
// this package's own log); the task keeps its status. A crash between the log
// and the close leaves today's behaviour (logged, still open).
func closeRuleTask(ctx context.Context, pool *pgxpool.Pool, ex *executor.Executor, actor string,
	pm pendingMessage, decisionID, taskID int64, prNumber int, state string) (bool, error) {
	args, err := json.Marshal(map[string]any{
		"task_id": taskID,
		"reason":  fmt.Sprintf("PR #%d %s on GitHub (message %d)", prNumber, state, pm.msg.ID),
	})
	if err != nil {
		return false, fmt.Errorf("marshal task_close args for task %d: %w", taskID, err)
	}
	if _, err := ex.Execute(ctx, executor.Call{
		Tool: "task_close", Actor: actor, Args: args, TaskID: &taskID,
	}); err != nil {
		if strings.Contains(err.Error(), closeActiveWorkRefusal) {
			log.Printf("capture rules: message %d: PR #%d %s on GitHub, but task %d is active work; close skipped: %v",
				pm.msg.ID, prNumber, state, taskID, err)
			note := fmt.Sprintf("; task_close skipped: task %d is active work (%s)", taskID, closeActiveWorkRefusal)
			if _, uerr := pool.Exec(ctx,
				`UPDATE capture_decisions SET reason = COALESCE(reason,'') || $1 WHERE id = $2`, note, decisionID); uerr != nil {
				return false, fmt.Errorf("record close refusal on capture decision %d: %w", decisionID, uerr)
			}
			return false, nil
		}
		return false, fmt.Errorf("close review task %d on PR #%d %s (message %d): %w", taskID, prNumber, state, pm.msg.ID, err)
	}
	return true, nil
}

// markRuleActivity is task_mark_activity (SWT-72 D3) through the executor as
// the configured capture:{connector} actor, after appendRuleLog on the same
// message: ids only — the handler reads the message direction and the task's
// status under the row lock, and answers marked:true or a skip (closed task,
// same message again). Channel-, rule- and assignee-blind on purpose
// (criterion 10): the hook sits in the one task_log branch every connector and
// every rule kind funnel through. Fails the pass on error, the linkRuleRef
// policy.
func markRuleActivity(ctx context.Context, ex *executor.Executor, actor string,
	pm pendingMessage, taskID int64, system, key string) (bool, error) {
	args, err := json.Marshal(map[string]any{
		"task_id":    taskID,
		"message_id": pm.msg.ID,
		"reason":     fmt.Sprintf("capture: activity on %s %s, message %d", system, key, pm.msg.ID),
	})
	if err != nil {
		return false, fmt.Errorf("marshal task_mark_activity args for task %d: %w", taskID, err)
	}
	res, err := ex.Execute(ctx, executor.Call{
		Tool: "task_mark_activity", Actor: actor, Args: args, TaskID: &taskID,
	})
	if err != nil {
		return false, fmt.Errorf("mark activity on task %d (message %d): %w", taskID, pm.msg.ID, err)
	}
	var out struct {
		Marked bool `json:"marked"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		return false, fmt.Errorf("parse task_mark_activity result for task %d: %w", taskID, err)
	}
	return out.Marked, nil
}

// markRuleSurfaced is task_mark_surfaced (SWT-45 J7) through the executor, for
// a task an overriding rule just created: the reconciler then holds it open
// against a done ticket instead of closing it in the same tick.
func markRuleSurfaced(ctx context.Context, ex *executor.Executor, actor string,
	pm pendingMessage, taskID int64, system, key string) (bool, error) {
	args, err := json.Marshal(map[string]any{
		"task_id":    taskID,
		"message_id": pm.msg.ID,
		"reason": fmt.Sprintf("capture: created by activity, message %d on %s %s",
			pm.msg.ID, system, key),
	})
	if err != nil {
		return false, fmt.Errorf("marshal task_mark_surfaced args for task %d: %w", taskID, err)
	}
	res, err := ex.Execute(ctx, executor.Call{
		Tool: "task_mark_surfaced", Actor: actor, Args: args, TaskID: &taskID,
	})
	if err != nil {
		return false, fmt.Errorf("surface task %d (message %d): %w", taskID, pm.msg.ID, err)
	}
	var out struct {
		Surfaced bool `json:"surfaced"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		return false, fmt.Errorf("parse task_mark_surfaced result for task %d: %w", taskID, err)
	}
	return out.Surfaced, nil
}

// ruleTaskTitle is SPEC §7's "{external_key} — {subject-or-first-line}", truncated
// to 120 runes with the ONE spelling of whitespace-collapsed, rune-safe truncation.
// No model, no invention: every character is copied from stored data.
// ruleTaskTitle composes a board title. For a key that is NOT the message's
// thread key (jira, body_regex, any key_regex rule) the shape is byte-identical
// to what always shipped: `{key} — {head}`. When the derived key IS the
// message's non-empty thread key — capture.externalKey returns it VERBATIM for
// a keyless non-body_regex rule, which is what put 128-character keys on the
// board — the key is a dedup key, not a title, so the label is the SENDER
// (SWT-31 Q1), falling back to the project name, the slug, then the key itself
// (criterion 4: never empty, never a dangling separator).
//
// The discriminator is the EQUALITY and nothing else (D1): never the provider
// name — a thread key's format is the owning connector's spelling, not this
// package's — and `key != ""` so two absent values do not read as a match.
func ruleTaskTitle(key string, msg Message, winner storedRule) string {
	head := strings.TrimSpace(msg.Subject)
	if head == "" {
		head = ruleFirstLine(msg.BodyText)
	}
	label := key
	if key != "" && key == msg.ThreadKey {
		switch {
		case strings.TrimSpace(msg.Sender) != "":
			label = strings.TrimSpace(msg.Sender)
		case winner.projectName != "":
			label = winner.projectName
		case winner.rule.Project != "":
			label = winner.rule.Project
		}
	}
	title := label
	if head != "" {
		title = label + " — " + head
	}
	return textmatch.NormalizedPrefix(title, rulesTitleLen)
}

// ruleTaskBody copies the provenance a human needs to judge the task: which
// message, which thread, which rule. Nothing generated. relatedTaskID (SWT-74
// D4) is the ticket task a comm belongs with: when non-zero it is the LAST
// key/value line, before the blank line and the preview (D11's key name, one
// vocabulary for both paths); 0 leaves the body byte-identical to before.
func ruleTaskBody(pm pendingMessage, winner storedRule, system, key string, relatedTaskID int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Captured deterministically by capture rule %d (%s %q).\n\n",
		winner.rule.ID, winner.rule.Kind, winner.rule.Pattern)
	fmt.Fprintf(&b, "external: %s %s\n", system, key)
	fmt.Fprintf(&b, "channel: %s\n", ruleOrNone(pm.channel))
	fmt.Fprintf(&b, "thread_key: %s\n", ruleOrNone(pm.msg.ThreadKey))
	fmt.Fprintf(&b, "sender: %s\n", ruleOrNone(pm.msg.Sender))
	fmt.Fprintf(&b, "sent_at: %s\n", pm.sentAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "message_id: %d\n", pm.msg.ID)
	if pm.msg.ExternalMessageID != "" {
		fmt.Fprintf(&b, "external_message_id: %s\n", pm.msg.ExternalMessageID)
	}
	if relatedTaskID != 0 {
		fmt.Fprintf(&b, "related_task: %d\n", relatedTaskID)
	}
	fmt.Fprintf(&b, "\n%s", textmatch.NormalizedPrefix(rulesPreview(pm.msg), rulesPreviewLen))
	return b.String()
}

// commTaskTitle is a comm task's title (SWT-74 D4, criterion 14):
// `{sender}: {subject else first line}`, through textmatch.NormalizedPrefix at
// rulesTitleLen — the ONE truncation spelling. The label falls back to the
// project NAME, then the slug, then the key; never empty, and the separator
// appears only when there is something after it (ruleTaskTitle's rule).
func commTaskTitle(pm pendingMessage, winner storedRule, key string) string {
	label := strings.TrimSpace(pm.msg.Sender)
	switch {
	case label != "":
	case winner.projectName != "":
		label = winner.projectName
	case winner.rule.Project != "":
		label = winner.rule.Project
	default:
		label = key
	}
	head := strings.TrimSpace(pm.msg.Subject)
	if head == "" {
		head = ruleFirstLine(pm.msg.BodyText)
	}
	title := label
	if head != "" {
		title = label + ": " + head
	}
	return textmatch.NormalizedPrefix(title, rulesTitleLen)
}

// commTaskArgs is create_task's argument object for a comm task (SWT-74 D4):
// the RULE's project and subproject, a human task at priority 0, the sender-led
// title and ruleTaskBody's body with `related_task: N` as its last key line.
// Pure, like ruleCreateTaskArgs, so the payload is testable without a database.
func commTaskArgs(pm pendingMessage, winner storedRule, system, key string, relatedTaskID int64) map[string]any {
	return map[string]any{
		"project":       winner.rule.Project,
		"subproject":    winner.subproject,
		"title":         commTaskTitle(pm, winner, key),
		"body":          ruleTaskBody(pm, winner, system, key, relatedTaskID),
		"assignee_type": "human",
		"priority":      0,
	}
}

// createCommTask creates the comm task through create_task on the executor.
// No link_external_ref (the key belongs to the ticket task; a second ref row
// would hijack every future attach) and no task_mark_surfaced (SWT-45's
// machinery for an overriding create).
func createCommTask(ctx context.Context, ex *executor.Executor, actor string,
	pm pendingMessage, winner storedRule, system, key string, relatedTaskID int64) (int64, error) {
	args, err := json.Marshal(commTaskArgs(pm, winner, system, key, relatedTaskID))
	if err != nil {
		return 0, fmt.Errorf("marshal create_task args for comm on message %d: %w", pm.msg.ID, err)
	}
	res, err := ex.Execute(ctx, executor.Call{Tool: "create_task", Actor: actor, Args: args})
	if err != nil {
		return 0, fmt.Errorf("create comm task for %s %s (message %d): %w", system, key, pm.msg.ID, err)
	}
	var out struct {
		TaskID int64 `json:"task_id"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		return 0, fmt.Errorf("parse create_task result for comm on message %d: %w", pm.msg.ID, err)
	}
	if out.TaskID == 0 {
		return 0, fmt.Errorf("create_task returned no task id for comm on message %d", pm.msg.ID)
	}
	return out.TaskID, nil
}

// recordDecisionCommTask completes the decision with the comm task it created
// (SWT-74 D3 step 3) — recordDecisionTask's twin: the claim is spent, so a
// later failure must not lose the pointer to what was created.
func recordDecisionCommTask(ctx context.Context, pool *pgxpool.Pool, decisionID, commTaskID int64) error {
	if _, err := pool.Exec(ctx,
		`UPDATE capture_decisions SET comm_task_id = $2 WHERE id = $1`, decisionID, commTaskID); err != nil {
		return fmt.Errorf("record comm task %d on capture decision %d: %w", commTaskID, decisionID, err)
	}
	return nil
}

// appendCommPointer is the ids-only pointer on the TARGET (SWT-74 D3 step 6):
// one task_append_log through the executor, no title, no sender, no preview —
// which is what makes it safe on a claude task, whose log feeds a worker
// prompt. Deliberately NOT an activity mark: the mark moved to the comm.
func appendCommPointer(ctx context.Context, ex *executor.Executor, actor string,
	pm pendingMessage, targetID, commID int64) error {
	args, err := json.Marshal(map[string]any{
		"task_id": targetID,
		"kind":    "log",
		"message": fmt.Sprintf("capture: comm #%d created from this message (message %d)", commID, pm.msg.ID),
	})
	if err != nil {
		return fmt.Errorf("marshal comm pointer args for task %d: %w", targetID, err)
	}
	if _, err := ex.Execute(ctx, executor.Call{
		Tool: "task_append_log", Actor: actor, Args: args, TaskID: &targetID,
	}); err != nil {
		return fmt.Errorf("append comm pointer to task %d (comm %d, message %d): %w", targetID, commID, pm.msg.ID, err)
	}
	return nil
}

// externalURL substitutes {key} once, per SPEC §3. An empty template yields an
// empty url; link_external_ref stores NULL for that.
func externalURL(template, key string) string {
	if template == "" {
		return ""
	}
	return strings.Replace(template, "{key}", key, 1)
}

// previewText prefers the subject, because a Jira notification's whole content is
// often its subject line, and falls back to the body.
func rulesPreview(msg Message) string {
	if s := strings.TrimSpace(msg.Subject); s != "" && strings.TrimSpace(msg.BodyText) != "" {
		return s + " — " + msg.BodyText
	}
	if s := strings.TrimSpace(msg.Subject); s != "" {
		return s
	}
	return msg.BodyText
}

func ruleFirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func ruleOrNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

func ruleNullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
