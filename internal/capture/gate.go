package capture

// The capture-time Jira assignee gate (SWT-40 Part D,
// docs/tickets/inquiry-promote_SPEC.md D-D1..D-D6; runbook
// docs/runbooks/ticket-status-sync.md, "Capture-time assignee gate").
//
// Capture never calls Jira (D-D1). A jira-keyed match on a project whose
// projects.ticket_assignee_gate is on is recorded `held` — project, rule and key
// named, nothing created — and this file resolves those holds as the pipelined
// `gate` stage. It makes sure a stored snapshot of each held ticket exists
// (ticketstatus.EnsureSnapshots: the reconciler's own fetch, cache and TTL),
// asks the reconciler's own predicate (ticketstatus.Warranted), and writes the
// message's one ACTION as a second capture_decisions row, mode='gate':
//
//	task | task_log  the ticket is his and open: capture's exact helpers, as capture:gate
//	attributed       not his, done, delivered — or still unreadable after GateMaxAge
//
// FRESHNESS (review fix 1). A stored snapshot decides a hold only if it was
// verified at or after the held message's first-seen time
// (normalized_messages.created_at, which no upsert rewrites). A key whose
// snapshot predates any held message in the pass is fetched whatever the TTL
// (ticketstatus.Config.MinFresh); if that fetch fails, the hold stays pending
// and, if it never becomes fresh, expires fail-closed.
//
// FETCH, THEN LOCK (review fix 3). The fetch is idempotent raw-first ingestion,
// so it runs OUTSIDE capture's advisory lock 0x5157_0015: every connector's
// capture pass takes that lock, and none of them may wait on Jira. The pass then
// takes the lock, re-reads the inbox and decides from the stored snapshots.
//
// TENANT SCOPE (review round 2, fix 2). A stored snapshot counts only if it came
// from the account ticketstatus routes the key to (ticketstatus.Config
// .ScopeToRoute): the lookup account whose prefix scope claims it, or — for a
// key no lookup account claims — the one poller account storing it. Any other
// stored copy is another site's ticket and is ignored.
//
// CLAIM, THEN ACT, THEN COMPLETE (review round 2, fix 1). Every gate row that
// leads to an executor call — task and task_log alike — is claimed with task_id
// NULL and completed with recordDecisionTask only after the calls succeed, so a
// pass that dies in between leaves a row the report's "claimed with no task"
// line counts.
//
// An unreadable hold (no snapshot, stale, Jira down, no credential, over this
// pass's key budget) writes nothing and stays held for the next wake or sweep.
//
// DryRunGate is `opsctl capture-rules gate --dry-run`: the same decide step
// (decideGateHolds) over stored snapshots only, printing instead of acting.
//
// Invariant 3: tasks, external_refs, task_events and task_dismissals are reached
// only through the executor — create_task, link_external_ref,
// task_set_source_thread, task_append_log, the guarded task_reopen — via
// rules_store.go's helpers. The only direct write is capture's own log.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/jira"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/ticketstatus"
)

const (
	// GateActor is the executor actor for every task write the gate makes, in
	// the capture:{connector} shape.
	GateActor = "capture:gate"
	// GateMaxAge is how long a hold may stay unreadable. Past it (measured from
	// when the HOLD was written — the held row's created_at — strictly after)
	// the hold resolves attributed, gate_unverified_expired: fail closed, no
	// task (D-D5). Not the message's sent_at: a message captured late still
	// gets its full window of lookups.
	GateMaxAge = 72 * time.Hour
	// GateMaxKeysPerPass bounds the distinct tickets one pass looks up. Holds on
	// the rest stay held, untouched, for the next pass or sweep (D-D5), counted
	// GateStats.BudgetSkipped.
	GateMaxKeysPerPass = 50
	// GateDefaultLimit bounds one pass's inbox (held rows) when GateConfig.Limit
	// is zero; pipelined's gate stage uses it as its --limit.
	GateDefaultLimit = 500
)

// The gate's reason codes. The drop reasons (not_assigned, ticket_delivered and
// the done one) are ticketstatus.Warranted's, passed through, never re-spelled.
const (
	gateReasonPending   = "pending_lookup"
	gateReasonBudget    = "budget_skipped" // never written: a skipped hold writes nothing
	gateReasonExpired   = "gate_unverified_expired"
	gateReasonWarranted = "warranted"
)

// The inbox's two sources: live holds (the only ones the gate resolves) and,
// for the dry run only, the latest shadow decision per message when it is held.
const (
	gateInboxLive   = "live"
	gateInboxShadow = "shadow"
)

// ErrGateLockHeld is RunGate's error when capture's advisory lock is held
// elsewhere; pipelined maps it to pipeline.ErrLockHeld (retry later, never a
// failure).
var ErrGateLockHeld = errors.New("capture gate: the capture-rules advisory lock is held elsewhere")

// GateObservation is everything DecideGate may know about one hold.
type GateObservation struct {
	Ticket  ticketstatus.Observation // facts from the STORED snapshot; GateOn from the column
	HeldFor time.Duration            // now minus the held row's created_at: how long the hold has waited
}

// GateRef is the task external_refs links the held key to, re-read per message.
type GateRef struct {
	TaskID      int64 // the linked task
	DismissalID int64 // its OPEN task_dismissals row (SWT-36), 0 = none
}

// GateDecision is what one hold becomes.
type GateDecision struct {
	Action      string // held | task | task_log | attributed
	Reason      string // pending_lookup (held); the drop or gate_unverified_expired (attributed)
	TaskID      int64  // task_log: the ref's task
	DismissalID int64  // task_log on a dismissed task: the guarded reopen's target
}

// GateConfig drives one RunGate pass.
type GateConfig struct {
	// Limit bounds the inbox (held rows); 0 means GateDefaultLimit.
	Limit int
	// TTL is the snapshot freshness bound; zero means the reconciler's default.
	TTL time.Duration
	// Lookup is the token-decrypting client factory pipelined holds. NIL means
	// no credential: nothing is fetched, and unreadable holds stay held until
	// they expire.
	Lookup jira.ClientFactory
}

// GateDryRunConfig drives one DryRunGate.
type GateDryRunConfig struct {
	// Limit bounds the inbox; 0 means GateDefaultLimit.
	Limit int
	// Shadow reads the latest SHADOW decision per message when it is held,
	// instead of the live holds.
	Shadow bool
	// Out receives one line per hold; nil discards.
	Out io.Writer
}

// GateStats is one pass's counters. Resolved = TasksCreated + Appended +
// Attributed: the gate rows written, and what pipelined reports as processed —
// a hold that stays pending never counts, so the stage loop does not re-run a
// pass at once over the same unreadable rows. BudgetSkipped counts the holds
// left untouched because their key was over this pass's GateMaxKeysPerPass.
type GateStats struct {
	TasksCreated, Appended, Reopened, Attributed, PendingLookup, BudgetSkipped int
	Resolved                                                                   int
}

// gateHold is one inbox row: the held decision, its message and its rule.
type gateHold struct {
	pm        pendingMessage
	rule      storedRule
	ruleIDs   []int64
	ambiguous bool
	system    string
	key       string
	firstSeen time.Time // normalized_messages.created_at: the freshness floor
	heldAt    time.Time // the held row's created_at: GateMaxAge's clock
}

// RunGate is one gate pass: fetch (outside the lock) the snapshots the inbox's
// first GateMaxKeysPerPass distinct keys need, then take capture's lock,
// re-read the inbox (live held rows with no gate row, oldest message first —
// expired holds included, so they resolve), and decide and act on each hold.
func RunGate(ctx context.Context, pool *pgxpool.Pool, ex *executor.Executor, cfg GateConfig) (GateStats, error) {
	var stats GateStats
	if pool == nil {
		return stats, errors.New("capture gate: nil database pool")
	}
	if ex == nil {
		// Invariant 3: tasks are reachable only through the executor.
		return stats, errors.New("capture gate: requires an executor")
	}
	limit := gateLimit(cfg.Limit)

	// A busy lock means a capture pass (or another gate pass) is running: skip
	// before spending a single GET. pipelined retries in 30 s, then the sweep.
	release, held, err := tryRulesLock(ctx, pool)
	if err != nil {
		return stats, err
	}
	if !held {
		return stats, ErrGateLockHeld
	}
	release()

	// Fetch OUTSIDE the lock: raw-first ingestion is idempotent, and every
	// connector's capture pass takes 0x5157_0015 — none may wait on Jira.
	planned, err := gateInbox(ctx, pool, gateInboxLive, limit)
	if err != nil || len(planned) == 0 {
		return stats, err
	}
	keys, minFresh := gateKeys(planned)
	snaps, _, err := ticketstatus.EnsureSnapshots(ctx, pool, keys,
		ticketstatus.Config{TTL: cfg.TTL, Lookup: cfg.Lookup, MinFresh: minFresh, ScopeToRoute: true})
	if err != nil {
		return stats, fmt.Errorf("capture gate: %w", err)
	}

	// Capture's own lock for the decide-and-act half: the gate writes
	// capture_decisions and must serialize with every capture pass (E-D4).
	release, held, err = tryRulesLock(ctx, pool)
	if err != nil {
		return stats, err
	}
	if !held {
		return stats, ErrGateLockHeld
	}
	defer release()

	// Re-read under the lock: another pass may have resolved some holds, and
	// holds captured since are decided against what was fetched — a hold newer
	// than its key's snapshot stays pending for the next wake.
	holds, err := gateInbox(ctx, pool, gateInboxLive, limit)
	if err != nil || len(holds) == 0 {
		return stats, err
	}
	err = decideGateHolds(ctx, pool, holds, snaps, nil, &stats,
		func(h gateHold, obs GateObservation, d GateDecision) error {
			if d.Action == actionHeld {
				return nil // pending or over budget: stays held, untouched
			}
			return applyGate(ctx, pool, ex, h, obs, d, &stats)
		})
	return stats, err
}

// DryRunGate decides every hold exactly as RunGate would (decideGateHolds) but
// from the STORED snapshots only — no fetch, no lock, no writes of any kind —
// and prints one line per hold: message, key, and the would-be outcome
// (task / task_log / attributed:<reason> / pending_lookup / budget_skipped).
// A second hold of a key the dry run "created" a task for reads as task_log,
// as the live pass would log onto that task.
func DryRunGate(ctx context.Context, pool *pgxpool.Pool, cfg GateDryRunConfig) (GateStats, error) {
	var stats GateStats
	if pool == nil {
		return stats, errors.New("capture gate: nil database pool")
	}
	out := cfg.Out
	if out == nil {
		out = io.Discard
	}
	source := gateInboxLive
	if cfg.Shadow {
		source = gateInboxShadow
	}
	holds, err := gateInbox(ctx, pool, source, gateLimit(cfg.Limit))
	if err != nil {
		return stats, err
	}
	if len(holds) == 0 {
		fmt.Fprintf(out, "gate dry-run: no pending %s holds\n", source)
		return stats, nil
	}
	keys, _ := gateKeys(holds)
	// DryRun: EnsureSnapshots returns the stored rows and fetches nothing, so
	// raw_source_items, sync_runs and the lookup cursor stay untouched.
	snaps, _, err := ticketstatus.EnsureSnapshots(ctx, pool, keys, ticketstatus.Config{DryRun: true, ScopeToRoute: true})
	if err != nil {
		return stats, fmt.Errorf("capture gate: %w", err)
	}
	simulated := map[string]bool{}
	err = decideGateHolds(ctx, pool, holds, snaps, simulated, &stats,
		func(h gateHold, obs GateObservation, d GateDecision) error {
			fmt.Fprintf(out, "gate dry-run: message=%d key=%s outcome=%s", h.pm.msg.ID, h.key, gateOutcome(d))
			if d.Action != actionHeld {
				fmt.Fprintf(out, " (%s)", gateReasonText(h, obs, d))
			}
			fmt.Fprintln(out)
			switch d.Action {
			case actionTask:
				simulated[h.key] = true
				stats.TasksCreated++
			case actionTaskLog:
				stats.Appended++
				if d.DismissalID != 0 {
					stats.Reopened++
				}
			case actionAttributed:
				stats.Attributed++
			default:
				return nil
			}
			stats.Resolved++
			return nil
		})
	return stats, err
}

// gateOutcome is a decision's one-token dry-run spelling.
func gateOutcome(d GateDecision) string {
	switch d.Action {
	case actionHeld:
		return d.Reason
	case actionAttributed:
		return actionAttributed + ":" + d.Reason
	default:
		return d.Action
	}
}

func gateLimit(limit int) int {
	if limit <= 0 {
		return GateDefaultLimit
	}
	return limit
}

// gateKeys is D-D5's budget over one inbox, oldest hold first: the first
// GateMaxKeysPerPass distinct keys, and for each the first-seen time of its
// NEWEST hold — the freshness a stored snapshot needs before it may decide them
// all (so a burst of mentions costs one GET).
func gateKeys(holds []gateHold) ([]string, map[string]time.Time) {
	keys := []string{}
	minFresh := map[string]time.Time{}
	for _, h := range holds {
		if _, in := minFresh[h.key]; !in {
			if len(keys) >= GateMaxKeysPerPass {
				continue // over budget: not fetched, not decided this pass
			}
			keys = append(keys, h.key)
		}
		if h.firstSeen.After(minFresh[h.key]) {
			minFresh[h.key] = h.firstSeen
		}
	}
	return keys, minFresh
}

// decideGateHolds is the ONE decide step, shared by RunGate and DryRunGate: the
// key budget, the freshness rule (gateObservation), the per-message ref
// re-query and DecideGate. act sees every hold in inbox order — resolutions,
// pending holds and budget skips alike (Action held, Reason pending_lookup or
// budget_skipped) — and returns before the next hold's ref is re-queried, so a
// task it created is the next mention's ref: two held mentions of one new
// ticket make ONE task and one log (external_refs' unique key is the
// backstop). simulated is the dry run's stand-in for that (keys it "created" a
// task for); nil on the live path.
func decideGateHolds(ctx context.Context, pool *pgxpool.Pool, holds []gateHold,
	snaps map[string]ticketstatus.Snapshot, simulated map[string]bool, stats *GateStats,
	act func(gateHold, GateObservation, GateDecision) error) error {
	keys, _ := gateKeys(holds)
	inPass := make(map[string]bool, len(keys))
	for _, k := range keys {
		inPass[k] = true
	}
	projects := []int64{}
	seenProject := map[int64]bool{}
	for _, h := range holds {
		if !seenProject[h.rule.projectID] {
			seenProject[h.rule.projectID] = true
			projects = append(projects, h.rule.projectID)
		}
	}
	delivered, err := ticketstatus.DeliveredStatusesByProject(ctx, pool, projects)
	if err != nil {
		return fmt.Errorf("capture gate: %w", err)
	}

	now := time.Now()
	for _, h := range holds {
		if !inPass[h.key] {
			stats.BudgetSkipped++
			if err := act(h, GateObservation{}, GateDecision{Action: actionHeld, Reason: gateReasonBudget}); err != nil {
				return err
			}
			continue
		}
		snap, have := snaps[h.key]
		obs, readable := gateObservation(h, snap, have, delivered[h.rule.projectID], now)
		ref, err := gateRef(ctx, pool, h, simulated)
		if err != nil {
			return err
		}
		d := DecideGate(obs, readable, ref)
		if d.Action == actionHeld {
			stats.PendingLookup++
		}
		if err := act(h, obs, d); err != nil {
			return err
		}
	}
	return nil
}

// gateRef re-queries the key's ref for one message (under the lock, on the
// live path).
func gateRef(ctx context.Context, pool *pgxpool.Pool, h gateHold, simulated map[string]bool) (*GateRef, error) {
	rt, found, err := taskForExternalRef(ctx, pool, h.system, h.key)
	if err != nil {
		return nil, err
	}
	if found {
		return &GateRef{TaskID: rt.taskID, DismissalID: rt.dismissalID}, nil
	}
	if simulated[h.key] {
		return &GateRef{}, nil // dry run: the task an earlier hold would have created
	}
	return nil, nil
}

// applyGate claims the message's one resolution (the gate row, with task_id
// NULL, BEFORE any executor call), acts on it, and only then records the task
// on the row — for task and task_log alike, so a failure after the claim leaves
// a row the report's "claimed with no task" line counts (review round 2, fix 1).
func applyGate(ctx context.Context, pool *pgxpool.Pool, ex *executor.Executor,
	h gateHold, obs GateObservation, d GateDecision, stats *GateStats) error {
	id, inserted, err := insertGateDecision(ctx, pool, h, d.Action, gateReasonText(h, obs, d))
	if err != nil {
		return err
	}
	if !inserted {
		// Another pass resolved this message first: one resolution per
		// message, forever, and whoever won the claim owns the action.
		return nil
	}
	switch d.Action {
	case actionTask:
		created, err := createRuleTask(ctx, ex, GateActor, h.pm, h.rule, h.system, h.key)
		if err != nil {
			return err
		}
		// Recorded before the link, for insertDecision's reason: the claim is
		// spent and must keep pointing at what it created.
		if err := recordDecisionTask(ctx, pool, id, created); err != nil {
			return err
		}
		if err := linkRuleRef(ctx, ex, GateActor, created, h.rule, h.system, h.key); err != nil {
			return err
		}
		if err := setRuleProvenance(ctx, ex, GateActor, created, h.pm); err != nil {
			return err
		}
		stats.TasksCreated++
	case actionTaskLog:
		if err := appendRuleLog(ctx, ex, GateActor, h.pm, d.TaskID, h.system, h.key); err != nil {
			return err
		}
		stats.Appended++
		// SWT-36 D10: log first, THEN the guarded reopen.
		if d.DismissalID != 0 {
			reopened, err := reopenRuleTask(ctx, ex, GateActor, h.pm, d.TaskID, d.DismissalID, h.system, h.key)
			if err != nil {
				return err
			}
			if reopened {
				stats.Reopened++
			}
		}
		// Completed last: the log (and the guarded reopen) are done.
		if err := recordDecisionTask(ctx, pool, id, d.TaskID); err != nil {
			return err
		}
	case actionAttributed:
		// No executor call: attribution names the project and creates nothing.
		stats.Attributed++
	default:
		return fmt.Errorf("capture gate: no action for decision %q on message %d", d.Action, h.pm.msg.ID)
	}
	stats.Resolved++
	return nil
}

// gateObservation builds the observation from the STORED snapshot (D19). The
// driver's half of readable: exactly one stored row, verified no earlier than
// the message was first seen, that parses.
func gateObservation(h gateHold, snap ticketstatus.Snapshot, have bool, delivered []string, now time.Time) (GateObservation, bool) {
	obs := GateObservation{
		HeldFor: now.Sub(h.heldAt),
		Ticket: ticketstatus.Observation{
			TicketKey:         h.key,
			GateOn:            h.rule.gateOn,
			DeliveredStatuses: delivered,
		},
	}
	if !have || snap.Count != 1 {
		return obs, false
	}
	if snap.VerifiedAt.Before(h.firstSeen) {
		// Review fix 1, the freshness rule: this snapshot describes the ticket
		// as it was BEFORE the message existed — the ticket may have been
		// reassigned since (D-D6's assignment mail is exactly that case). It is
		// no verdict: pending, and a fetch failure keeps it so until expiry.
		return obs, false
	}
	facts, err := jira.IssueFacts(snap.Raw)
	if err != nil {
		// Unreadable, not fatal: the hold stays held and, if it never becomes
		// readable, expires fail-closed instead of wedging every pass.
		log.Printf("capture gate: stored snapshot for %s does not parse; hold stays pending: %v", h.key, err)
		return obs, false
	}
	obs.Ticket.StatusCategory = facts.StatusCategory
	obs.Ticket.StatusName = facts.StatusName
	obs.Ticket.StatusKnown = facts.StatusKnown
	obs.Ticket.Assignee = facts.Assignee
	obs.Ticket.AssigneeKnown = facts.AssigneeKnown
	obs.Ticket.OwnAccountID = snap.OwnAccountID
	return obs, true
}

// gateInbox is the gate's queue-as-filter: held rows with no gate row on
// INBOUND messages (re-checked: a re-normalization can rewrite direction),
// oldest message first, limit-bounded — expired holds included (they resolve
// attributed and stop costing lookups). source is gateInboxLive, or
// gateInboxShadow (the dry run's: the latest shadow decision per message, when
// it is held). The message columns are pendingMessages', so capture's task
// helpers see exactly what capture would.
func gateInbox(ctx context.Context, pool *pgxpool.Pool, source string, limit int) ([]gateHold, error) {
	from := `capture_decisions`
	switch source {
	case gateInboxLive:
	case gateInboxShadow:
		from = `(SELECT DISTINCT ON (message_id) * FROM capture_decisions
		          WHERE mode = 'shadow' ORDER BY message_id, id DESC)`
	default:
		return nil, fmt.Errorf("capture gate: unknown inbox source %q", source)
	}
	rows, err := pool.Query(ctx, `
		SELECT m.id, m.raw_source_item_id, m.thread_id, COALESCE(nt.thread_key,''),
		       COALESCE(m.sender,''), COALESCE(m.subject,''), COALESCE(m.body_text,''),
		       COALESCE(m.external_message_id,''), COALESCE(m.channel,''),
		       COALESCE(m.sent_at, m.created_at), m.created_at, h.created_at,
		       h.matched_rule_ids, h.ambiguous, h.external_system, h.external_key,
		       r.id, p.slug, p.name, r.criteria_type, r.pattern, r.key_regex, r.priority, r.enabled,
		       r.project_id, COALESCE(r.subproject,''), COALESCE(r.url_template,''),
		       p.ticket_assignee_gate
		  FROM `+from+` h
		  JOIN normalized_messages m ON m.id = h.message_id
		  JOIN capture_rules r ON r.id = h.matched_rule_id
		  JOIN projects p ON p.id = r.project_id
		  LEFT JOIN normalized_threads nt ON nt.id = m.thread_id
		 WHERE h.mode = $2 AND h.action = 'held'
		   AND m.direction = 'inbound'
		   AND h.external_system IS NOT NULL AND h.external_key IS NOT NULL
		   AND NOT EXISTS (SELECT 1 FROM capture_decisions g
		                    WHERE g.message_id = h.message_id AND g.mode = 'gate')
		 ORDER BY COALESCE(m.sent_at, m.created_at), m.id
		 LIMIT $1`, limit, source)
	if err != nil {
		return nil, fmt.Errorf("select held capture decisions: %w", err)
	}
	defer rows.Close()

	var out []gateHold
	for rows.Next() {
		var h gateHold
		if err := rows.Scan(&h.pm.msg.ID, &h.pm.rawItemID, &h.pm.threadID, &h.pm.msg.ThreadKey,
			&h.pm.msg.Sender, &h.pm.msg.Subject, &h.pm.msg.BodyText,
			&h.pm.msg.ExternalMessageID, &h.pm.channel, &h.pm.sentAt, &h.firstSeen, &h.heldAt,
			&h.ruleIDs, &h.ambiguous, &h.system, &h.key,
			&h.rule.rule.ID, &h.rule.rule.Project, &h.rule.projectName, &h.rule.rule.Kind, &h.rule.rule.Pattern,
			&h.rule.rule.ExternalKeyRegex, &h.rule.rule.Priority, &h.rule.rule.Enabled,
			&h.rule.projectID, &h.rule.subproject, &h.rule.urlTemplate, &h.rule.gateOn); err != nil {
			return nil, fmt.Errorf("scan held capture decision: %w", err)
		}
		h.rule.extSystem = h.system
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate held capture decisions: %w", err)
	}
	return out, nil
}

// insertGateDecision writes the message's one resolution, always with task_id
// NULL: the claim. applyGate completes a task / task_log row afterwards with
// recordDecisionTask. The partial index capture_decisions_gate_uniq is the
// claim, so the predicate is restated (arbiter inference matches a partial index
// only when it is repeated).
func insertGateDecision(ctx context.Context, pool *pgxpool.Pool, h gateHold,
	action string, reason string) (int64, bool, error) {
	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO capture_decisions
		   (message_id, raw_source_item_id, mode, matched_rule_id, project_id,
		    matched_rule_ids, ambiguous, action, external_system, external_key,
		    task_id, reason)
		 VALUES ($1,$2,'gate',$3,$4,$5,$6,$7,$8,$9,NULL,$10)
		 ON CONFLICT (message_id) WHERE mode = 'gate' DO NOTHING
		 RETURNING id`,
		h.pm.msg.ID, h.pm.rawItemID, h.rule.rule.ID, h.rule.projectID,
		h.ruleIDs, h.ambiguous, action, h.system, h.key,
		reason).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("record gate decision for message %d: %w", h.pm.msg.ID, err)
	}
	return id, true, nil
}

// gateReasonText is the gate row's reason: "<code>: <prose>". The code is the
// one field the report groups by (gateReasonCode); the prose is the trail.
func gateReasonText(h gateHold, obs GateObservation, d GateDecision) string {
	t := obs.Ticket
	facts := fmt.Sprintf("status %s/%s, assignee %s, own %s", ruleOrNone(t.StatusCategory),
		ruleOrNone(t.StatusName), ruleOrNone(t.Assignee), ruleOrNone(t.OwnAccountID))
	switch d.Action {
	case actionTask:
		return fmt.Sprintf("%s: gate: %s %s warrants a task (%s); create one task",
			gateReasonWarranted, h.system, h.key, facts)
	case actionTaskLog:
		s := fmt.Sprintf("%s: gate: %s %s warrants a task (%s); already linked to task %d, append a log",
			gateReasonWarranted, h.system, h.key, facts, d.TaskID)
		if d.DismissalID != 0 {
			s += fmt.Sprintf("; reopen requested against dismissal %d", d.DismissalID)
		}
		return s
	case actionAttributed:
		if d.Reason == gateReasonExpired {
			return fmt.Sprintf("%s: gate: %s %s still unreadable %s after the hold; fail closed, no task",
				d.Reason, h.system, h.key, GateMaxAge)
		}
		return fmt.Sprintf("%s: gate: %s %s does not warrant a task (%s); attribution only",
			d.Reason, h.system, h.key, facts)
	}
	return ""
}

// gateReasonCode reads back the code gateReasonText wrote.
func gateReasonCode(reason string) string {
	code, _, _ := strings.Cut(reason, ":")
	return strings.TrimSpace(code)
}

// DecideGate is the gate's whole rule set (D-D3). Pure: a function of the
// observation, the driver's half of readability and the key's existing ref,
// with the hold's age arriving as a value. Unreadable holds stay held until
// they are STRICTLY older than GateMaxAge, then fail closed; a readable hold
// decides on its facts at any age, with the reconciler's predicate.
func DecideGate(obs GateObservation, readable bool, ref *GateRef) GateDecision {
	warranted, drop, factsReadable := ticketstatus.Warranted(obs.Ticket)
	switch {
	case !readable || !factsReadable:
		if obs.HeldFor > GateMaxAge {
			return GateDecision{Action: actionAttributed, Reason: gateReasonExpired}
		}
		return GateDecision{Action: actionHeld, Reason: gateReasonPending}
	case !warranted:
		return GateDecision{Action: actionAttributed, Reason: drop}
	case ref != nil:
		return GateDecision{Action: actionTaskLog, TaskID: ref.TaskID, DismissalID: ref.DismissalID}
	default:
		return GateDecision{Action: actionTask}
	}
}
