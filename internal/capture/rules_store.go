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

	"github.com/sspataro57/switchboard/internal/connector/jira"
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

	pending, err := pendingMessages(ctx, pool, cfg)
	if err != nil {
		return RulesStats{}, err
	}

	var stats RulesStats
	for _, pm := range pending {
		decision, winner, err := decideMessage(ctx, pool, cfg.Mode, pm, pure, byID)
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
		if decision.action == actionUnmatched {
			stats.Unmatched++
		} else {
			stats.Matched++
		}
		if cfg.Mode != RulesModeLive {
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
			// SWT-36 D10 / SWT-45 J9: log first, THEN the reopen — a crash
			// between the two leaves exactly today's behaviour (logged, still
			// closed). The revive handles an open dismissal itself, so it takes
			// precedence over SWT-36's guarded reopen.
			if decision.revive {
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
		        COALESCE(r.url_template,''), p.ticket_assignee_gate, r.revive, r.addressed
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
			&s.revive, &s.addressed); err != nil {
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

	q := `SELECT m.id, m.raw_source_item_id, m.thread_id, COALESCE(sa.account_email,''),
	             COALESCE(nt.thread_key,''), COALESCE(m.sender,''), COALESCE(m.subject,''),
	             COALESCE(m.body_text,''), COALESCE(m.external_message_id,''),
	             COALESCE(nt.participants,'[]'::jsonb), COALESCE(m.channel,''),
	             COALESCE(m.sent_at, m.created_at)
	        FROM normalized_messages m
	        LEFT JOIN raw_source_items ri ON ri.id = m.raw_source_item_id
	        LEFT JOIN source_accounts sa ON sa.id = ri.source_account_id
	        LEFT JOIN normalized_threads nt ON nt.id = m.thread_id
	       WHERE m.direction = 'inbound'
	         AND ($1::timestamptz IS NULL OR COALESCE(m.sent_at, m.created_at) >= $1)
	         AND NOT EXISTS (
	           SELECT 1 FROM capture_decisions g
	            WHERE g.message_id = m.id AND g.mode = 'gate')`
	// SWT-40 D-D2, the shadow-overwrite guard: a message the gate resolved is
	// excluded in EVERY mode, --all included. Every latest-decision reader
	// follows ORDER BY id DESC, so a newer shadow row would bury the gate's
	// resolution — the message's one action — for all of them.
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
		var pm pendingMessage
		var participants []byte
		if err := rows.Scan(&pm.msg.ID, &pm.rawItemID, &pm.threadID, &pm.msg.Source, &pm.msg.ThreadKey,
			&pm.msg.Sender, &pm.msg.Subject, &pm.msg.BodyText, &pm.msg.ExternalMessageID,
			&participants, &pm.channel, &pm.sentAt); err != nil {
			return nil, fmt.Errorf("scan pending message: %w", err)
		}
		pm.msg.Participants = parseThreadParticipants(participants)
		out = append(out, pm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending messages: %w", err)
	}
	return out, nil
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
func decideMessage(ctx context.Context, pool *pgxpool.Pool, mode string, pm pendingMessage,
	rules []Rule, byID map[int64]storedRule) (ruleDecision, storedRule, error) {
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
		return d, winner, nil
	case outcome.externalKey == "":
		// An empty key must never become an external_refs row: external_key='' would
		// collide every keyless message of that system onto ONE task, forever.
		d.reason = fmt.Sprintf("rule %d (%s) attributes to %s; external_system %s but no key could be derived",
			winner.rule.ID, winner.rule.Kind, winner.rule.Project, winner.extSystem)
		return d, winner, nil
	}

	system, key := winner.extSystem, outcome.externalKey
	d.extSystem, d.extKey = &system, &key

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

	existing, found, err := taskForExternalRef(ctx, pool, system, key)
	if err != nil {
		return d, winner, err
	}
	if found {
		taskID := existing.taskID
		d.action = actionTaskLog
		d.taskID = &taskID
		d.reason = fmt.Sprintf("rule %d (%s): %s %s already linked to task %d; append a log",
			winner.rule.ID, winner.rule.Kind, system, key, taskID)
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
		case existing.dismissalID != 0:
			d.dismissalID = existing.dismissalID
			d.reason += fmt.Sprintf("; task %d was dismissed (%s); reopen requested against dismissal %d",
				taskID, existing.dismissalCode, existing.dismissalID)
		}
		return d, winner, nil
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
		    task_id, reason)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		 ON CONFLICT (message_id) WHERE mode = 'live' DO NOTHING
		 RETURNING id`,
		pm.msg.ID, pm.rawItemID, mode, d.matchedRuleID, d.projectID,
		d.matchedRuleIDs, d.ambiguous, d.action, d.extSystem, d.extKey,
		d.taskID, ruleNullIfEmpty(d.reason)).Scan(&id)
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
	args, err := json.Marshal(map[string]any{
		"project":       winner.rule.Project,
		"subproject":    winner.subproject,
		"title":         ruleTaskTitle(key, pm.msg, winner),
		"body":          ruleTaskBody(pm, winner, system, key),
		"assignee_type": "human",
		"priority":      0,
	})
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
		"external_url": externalURL(winner.urlTemplate, key),
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
// message, which thread, which rule. Nothing generated.
func ruleTaskBody(pm pendingMessage, winner storedRule, system, key string) string {
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
	fmt.Fprintf(&b, "\n%s", textmatch.NormalizedPrefix(rulesPreview(pm.msg), rulesPreviewLen))
	return b.String()
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
