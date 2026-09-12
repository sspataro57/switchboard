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
// An unreadable hold (no snapshot yet: Jira down, no credential, over this
// pass's key budget) writes nothing and stays held for the next wake or sweep.
//
// Invariant 3: tasks, external_refs, task_events and task_dismissals are reached
// only through the executor — create_task, link_external_ref,
// task_set_source_thread, task_append_log, the guarded task_reopen — via
// rules_store.go's helpers. The only direct write is capture's own log.

import (
	"context"
	"errors"
	"fmt"
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
	// the held MESSAGE's sent_at, strictly after) the hold resolves attributed,
	// gate_unverified_expired: fail closed, no task (D-D5).
	GateMaxAge = 72 * time.Hour
	// GateMaxKeysPerPass bounds the distinct tickets one pass looks up. Holds on
	// the rest stay held, untouched, for the next pass or sweep (D-D5).
	GateMaxKeysPerPass = 50
	// GateDefaultLimit bounds one pass's inbox (held rows) when GateConfig.Limit
	// is zero; pipelined's gate stage uses it as its --limit.
	GateDefaultLimit = 500
)

// The gate's reason codes. The drop reasons (not_assigned, ticket_delivered and
// the done one) are ticketstatus.Warranted's, passed through, never re-spelled.
const (
	gateReasonPending   = "pending_lookup"
	gateReasonExpired   = "gate_unverified_expired"
	gateReasonWarranted = "warranted"
)

// ErrGateLockHeld is RunGate's error when capture's advisory lock is held
// elsewhere; pipelined maps it to pipeline.ErrLockHeld (retry later, never a
// failure).
var ErrGateLockHeld = errors.New("capture gate: the capture-rules advisory lock is held elsewhere")

// GateObservation is everything DecideGate may know about one hold.
type GateObservation struct {
	Ticket  ticketstatus.Observation // facts from the STORED snapshot; GateOn from the column
	HeldFor time.Duration            // now minus the held message's sent_at
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

// GateStats is one pass's counters. Resolved = TasksCreated + Appended +
// Attributed: the gate rows written, and what pipelined reports as processed —
// a hold that stays pending never counts, so the stage loop does not re-run a
// pass at once over the same unreadable rows.
type GateStats struct {
	TasksCreated, Appended, Reopened, Attributed, PendingLookup int
	Resolved                                                    int
}

// gateHold is one inbox row: the live held decision, its message and its rule.
type gateHold struct {
	pm        pendingMessage
	rule      storedRule
	ruleIDs   []int64
	ambiguous bool
	system    string
	key       string
}

// RunGate is one gate pass: take capture's lock, load the inbox (live held rows
// with no gate row, oldest message first — expired holds included, so they
// resolve), ensure snapshots for at most GateMaxKeysPerPass distinct keys,
// decide each hold with DecideGate, and act.
func RunGate(ctx context.Context, pool *pgxpool.Pool, ex *executor.Executor, cfg GateConfig) (GateStats, error) {
	var stats GateStats
	if pool == nil {
		return stats, errors.New("capture gate: nil database pool")
	}
	if ex == nil {
		// Invariant 3: tasks are reachable only through the executor.
		return stats, errors.New("capture gate: requires an executor")
	}
	limit := cfg.Limit
	if limit <= 0 {
		limit = GateDefaultLimit
	}

	// Capture's own lock: the gate writes capture_decisions and must serialize
	// with every connector's capture pass (E-D4).
	release, held, err := tryRulesLock(ctx, pool)
	if err != nil {
		return stats, err
	}
	if !held {
		return stats, ErrGateLockHeld
	}
	defer release()

	holds, err := gateInbox(ctx, pool, limit)
	if err != nil || len(holds) == 0 {
		return stats, err
	}

	// D-D5: at most GateMaxKeysPerPass distinct tickets per pass, oldest holds
	// first. A ticket mentioned many times costs one lookup.
	inPass := map[string]bool{}
	keys := []string{}
	projects := []int64{}
	seenProject := map[int64]bool{}
	for _, h := range holds {
		if !inPass[h.key] && len(keys) < GateMaxKeysPerPass {
			inPass[h.key] = true
			keys = append(keys, h.key)
		}
		if !seenProject[h.rule.projectID] {
			seenProject[h.rule.projectID] = true
			projects = append(projects, h.rule.projectID)
		}
	}
	snaps, _, err := ticketstatus.EnsureSnapshots(ctx, pool, keys, ticketstatus.Config{TTL: cfg.TTL, Lookup: cfg.Lookup})
	if err != nil {
		return stats, fmt.Errorf("capture gate: %w", err)
	}
	delivered, err := ticketstatus.DeliveredStatusesByProject(ctx, pool, projects)
	if err != nil {
		return stats, fmt.Errorf("capture gate: %w", err)
	}

	for _, h := range holds {
		if !inPass[h.key] {
			continue // over this pass's key budget: stays held, untouched
		}
		snap, have := snaps[h.key]
		obs, readable := gateObservation(h, snap, have, delivered[h.rule.projectID])

		// The ref is re-queried per message, under the lock: two held mentions
		// of one new ticket create ONE task, and the second logs onto it
		// (external_refs' unique key is the backstop).
		var ref *GateRef
		rt, found, err := taskForExternalRef(ctx, pool, h.system, h.key)
		if err != nil {
			return stats, err
		}
		if found {
			ref = &GateRef{TaskID: rt.taskID, DismissalID: rt.dismissalID}
		}

		d := DecideGate(obs, readable, ref)
		if d.Action == actionHeld {
			stats.PendingLookup++
			continue
		}
		if err := applyGate(ctx, pool, ex, h, obs, d, &stats); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// applyGate claims the message's one resolution (the gate row, BEFORE any
// executor call) and then acts on it.
func applyGate(ctx context.Context, pool *pgxpool.Pool, ex *executor.Executor,
	h gateHold, obs GateObservation, d GateDecision, stats *GateStats) error {
	var taskID *int64
	if d.Action == actionTaskLog {
		taskID = &d.TaskID
	}
	id, inserted, err := insertGateDecision(ctx, pool, h, d.Action, taskID, gateReasonText(h, obs, d))
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
// driver's half of readable: exactly one stored row that parses.
func gateObservation(h gateHold, snap ticketstatus.Snapshot, have bool, delivered []string) (GateObservation, bool) {
	obs := GateObservation{
		HeldFor: time.Since(h.pm.sentAt),
		Ticket: ticketstatus.Observation{
			TicketKey:         h.key,
			GateOn:            h.rule.gateOn,
			DeliveredStatuses: delivered,
		},
	}
	if !have || snap.Count != 1 {
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

// gateInbox is the gate's queue-as-filter: live held rows with no gate row,
// oldest message first, limit-bounded — expired holds included (they resolve
// attributed and stop costing lookups). The message columns are
// pendingMessages', so capture's task helpers see exactly what capture would.
func gateInbox(ctx context.Context, pool *pgxpool.Pool, limit int) ([]gateHold, error) {
	rows, err := pool.Query(ctx, `
		SELECT m.id, m.raw_source_item_id, m.thread_id, COALESCE(nt.thread_key,''),
		       COALESCE(m.sender,''), COALESCE(m.subject,''), COALESCE(m.body_text,''),
		       COALESCE(m.external_message_id,''), COALESCE(m.channel,''),
		       COALESCE(m.sent_at, m.created_at),
		       h.matched_rule_ids, h.ambiguous, h.external_system, h.external_key,
		       r.id, p.slug, p.name, r.criteria_type, r.pattern, r.key_regex, r.priority, r.enabled,
		       r.project_id, COALESCE(r.subproject,''), COALESCE(r.url_template,''),
		       p.ticket_assignee_gate
		  FROM capture_decisions h
		  JOIN normalized_messages m ON m.id = h.message_id
		  JOIN capture_rules r ON r.id = h.matched_rule_id
		  JOIN projects p ON p.id = r.project_id
		  LEFT JOIN normalized_threads nt ON nt.id = m.thread_id
		 WHERE h.mode = 'live' AND h.action = 'held'
		   AND h.external_system IS NOT NULL AND h.external_key IS NOT NULL
		   AND NOT EXISTS (SELECT 1 FROM capture_decisions g
		                    WHERE g.message_id = h.message_id AND g.mode = 'gate')
		 ORDER BY COALESCE(m.sent_at, m.created_at), m.id
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("select held capture decisions: %w", err)
	}
	defer rows.Close()

	var out []gateHold
	for rows.Next() {
		var h gateHold
		if err := rows.Scan(&h.pm.msg.ID, &h.pm.rawItemID, &h.pm.threadID, &h.pm.msg.ThreadKey,
			&h.pm.msg.Sender, &h.pm.msg.Subject, &h.pm.msg.BodyText,
			&h.pm.msg.ExternalMessageID, &h.pm.channel, &h.pm.sentAt,
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

// insertGateDecision writes the message's one resolution. The partial index
// capture_decisions_gate_uniq is the claim, so the predicate is restated
// (arbiter inference matches a partial index only when it is repeated).
func insertGateDecision(ctx context.Context, pool *pgxpool.Pool, h gateHold,
	action string, taskID *int64, reason string) (int64, bool, error) {
	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO capture_decisions
		   (message_id, raw_source_item_id, mode, matched_rule_id, project_id,
		    matched_rule_ids, ambiguous, action, external_system, external_key,
		    task_id, reason)
		 VALUES ($1,$2,'gate',$3,$4,$5,$6,$7,$8,$9,$10,$11)
		 ON CONFLICT (message_id) WHERE mode = 'gate' DO NOTHING
		 RETURNING id`,
		h.pm.msg.ID, h.pm.rawItemID, h.rule.rule.ID, h.rule.projectID,
		h.ruleIDs, h.ambiguous, action, h.system, h.key,
		taskID, reason).Scan(&id)
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
			return fmt.Sprintf("%s: gate: %s %s still unreadable after %s; fail closed, no task",
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
