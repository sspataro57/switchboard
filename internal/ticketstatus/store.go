package ticketstatus

// The driver (SWT-32 criteria 30-33, 40-44): advisory lock, candidate query,
// per-ref decision, executor calls, state UPSERT — capture.EvaluateRules and
// promote.Run's mould. The candidate set is external_refs WHERE system='jira'
// (D16: the task IS the candidate); the observation is read from STORED raw
// rows, never from an HTTP response in memory (D19); and every task write goes
// through the executor as ticketstatus:jira with Call.TaskID set (invariant 3).
// The only table this package writes directly is its own typed state,
// ticket_status_syncs (D3 — the capture_decisions / classify_promotions
// precedent).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/jira"
	"github.com/sspataro57/switchboard/internal/executor"
)

// Actor is the identity every executor call carries (criterion 40), in the
// capture:{connector} / promote:{lane} shape.
const Actor = "ticketstatus:jira"

// activeWorkRefusal is the substring of internal/tools/close.go's shared
// refusal this pass keys its mid-pass race handling on; statusset_test.go pins
// the two spellings together so the message cannot drift out from under the
// check silently.
const activeWorkRefusal = "refusing to close active work"

// advisoryLockKey serialises passes — it also covers the lookup, so two
// overlapping passes cannot both GET the same key. The low four hex digits are
// this ticket's migration number, the established convention; freeness is
// checked mechanically by the repo-wide collision scan, and the literal is
// spelled exactly ONCE, here.
const advisoryLockKey = int64(0x5157_0023)

// Config drives one pass.
type Config struct {
	// DryRun takes the same decisions over the same rows, performs NO writes of
	// any kind — no state rows, and no fetches, so a dry run cannot even mutate
	// raw_source_items — and prints one line per decision (criterion 42).
	DryRun bool
	// Force bypasses the lookup freshness TTL (D20) — the smoke's knob.
	Force bool
	// Limit bounds how many candidate refs one pass considers (0 = all).
	Limit int
	// TTL is the lookup freshness bound; zero means LookupTTL().
	TTL time.Duration
	// Lookup is the injected client factory (cmd/connectors/jira's
	// token-decrypting closure). NIL means no credential: the status half still
	// reconciles, and every lookup-routed ref without a stored snapshot counts
	// unpolled, LOUDLY (D21). This package never handles a token itself.
	Lookup jira.ClientFactory
	// MinFresh is the capture-time gate's per-key minimum freshness (SWT-40
	// review fix 1): a key whose stored snapshot was ingested before
	// MinFresh[key] is fetched whatever the TTL, because a snapshot older than a
	// held message may never decide it. A burst of mentions still costs one GET
	// (the gate passes the NEWEST first-seen time per key). Nil — the reconciler
	// — changes nothing: its TTL logic is untouched.
	MinFresh map[string]time.Time
}

// Stats is criterion 43's counter vocabulary, one field per printed name.
type Stats struct {
	Considered, ClosedTicketDone, ClosedTicketDelivered, ClosedNotAssigned, Reopened int
	RefusedActive, SuppressedDismissed, Converged                                    int
	Unpolled, Ambiguous, Unreadable                                                  int
	Fetched, FetchSkippedTTL, FetchFailed                                            int
}

// candidate is one external_refs row joined to its task and project.
type candidate struct {
	refID     int64
	key       string
	taskID    int64
	taskStat  string
	gateOn    bool
	delivered []string // projects.ticket_delivered_statuses (SWT-34), from the COLUMN
	dismissed bool
	state     *State
}

// Snapshot is one stored raw issue row, with the storing account's identity —
// what EnsureSnapshots hands the reconciler and the capture-time gate. Decisions
// read Raw, the STORED row (D19), never an HTTP response in memory.
type Snapshot struct {
	Raw        []byte
	IngestedAt time.Time
	// VerifiedAt is the latest time the ticket is KNOWN to have looked like Raw
	// (SWT-40 review fix 1): IngestedAt, or — when this call's GET of the key
	// succeeded — the database clock at the start of that GET. The two differ on
	// an unchanged refetch: upsertRaw's hash short-circuit leaves ingested_at
	// alone, so IngestedAt alone would call a just-verified ticket stale. The
	// capture-time gate compares it with a held message's first-seen time.
	VerifiedAt   time.Time
	OwnAccountID string // the storing account's sync_cursor->>'own_account_id' (D12)
	Count        int    // rows found for this key; >1 is criterion 31's ambiguity
}

// Run is one reconciliation pass.
//
// Lock contention is log-and-skip returning ZERO stats and no error (capture's
// policy — this is a hitchhiker on a connector run), and the skip prints a line
// so a silent no-op and a real empty pass never look the same.
func Run(ctx context.Context, pool *pgxpool.Pool, ex *executor.Executor, cfg Config) (Stats, error) {
	var stats Stats
	if pool == nil {
		return stats, errors.New("ticketstatus: nil database pool")
	}
	if !cfg.DryRun && ex == nil {
		// Invariant 3: the live path reaches tasks and task_events only through
		// the executor, so without one there is no legal way to act.
		return stats, errors.New("ticketstatus: live mode requires an executor")
	}

	release, held, err := tryLock(ctx, pool)
	if err != nil {
		return stats, err
	}
	if !held {
		fmt.Println("ticketstatus: another pass holds the advisory lock; skipping this run")
		return stats, nil
	}
	defer release()

	cands, err := loadCandidates(ctx, pool, cfg.Limit)
	if err != nil {
		return stats, err
	}
	if len(cands) == 0 {
		return stats, nil
	}

	// The snapshot half (SWT-40 D-D3): routing, TTL freshness, the fetch and
	// the read-back, shared with the capture-time gate.
	keys := make([]string, 0, len(cands))
	for _, c := range cands {
		keys = append(keys, c.key)
	}
	es, err := ensureSnapshots(ctx, pool, keys, cfg)
	stats.Fetched, stats.FetchSkippedTTL, stats.FetchFailed = es.stats.Fetched, es.stats.FetchSkippedTTL, es.stats.FetchFailed
	if err != nil {
		return stats, err
	}
	snaps, wouldFetch, routeAmbiguous := es.snaps, es.wouldFetch, es.routeAmbiguous

	for _, c := range cands {
		stats.Considered++
		snap, have := snaps[c.key]
		if !have {
			if routeAmbiguous[c.key] {
				stats.Ambiguous++
			} else {
				stats.Unpolled++
			}
			continue
		}
		if snap.Count > 1 {
			// Criterion 31: two stored snapshots for one key. Refusing is
			// reversible; a wrong close is a task that vanishes with no
			// explanation.
			stats.Ambiguous++
			continue
		}

		facts, err := jira.IssueFacts(snap.Raw)
		if err != nil {
			// A stored row that will not parse is a corrupt snapshot; the
			// operator should hear about it rather than the pass shrugging.
			return stats, fmt.Errorf("ticketstatus: stored snapshot for %s is unreadable: %w", c.key, err)
		}
		obs := Observation{
			TicketKey:         c.key,
			StatusCategory:    facts.StatusCategory,
			StatusName:        facts.StatusName,
			StatusKnown:       facts.StatusKnown,
			Assignee:          facts.Assignee,
			AssigneeKnown:     facts.AssigneeKnown,
			OwnAccountID:      snap.OwnAccountID,
			GateOn:            c.gateOn,
			TaskStatus:        c.taskStat,
			Dismissed:         c.dismissed,
			DeliveredStatuses: c.delivered,
		}
		d := Decide(obs, c.state)

		if cfg.DryRun {
			fmt.Printf("ticketstatus dry-run: %s task=%d action=%s warranted=%v drop_reason=%s "+
				"status=%s/%s assignee=%s own=%s would_fetch=%v\n",
				c.key, c.taskID, d.Action, d.Warranted, d.DropReason,
				obs.StatusCategory, obs.StatusName, orUnassigned(obs.Assignee), obs.OwnAccountID,
				wouldFetch[c.key])
			count(&stats, d)
			continue
		}
		if d.Action == "unreadable" {
			// Counted and left alone: status_category is NOT NULL with a
			// three-value CHECK, so an unreadable ref cannot even be recorded.
			stats.Unreadable++
			continue
		}

		reason := decisionReason(obs, d)
		// Ordering is per action, and each direction protects ITS return path
		// (go-reviewer F3). A close records state FIRST: if the close commits
		// and the record were lost, last_action would never say 'closed' and
		// the reopen path would be dead for that ref forever — while a state
		// row claiming a close that then failed just re-closes next pass (the
		// task is still open and restorable). A reopen acts FIRST: a
		// 'reopened' record for a reopen that failed would leave the task
		// closed with a state that never authorises another attempt.
		if d.Act && d.Action == "closed" {
			if err := upsertState(ctx, pool, c, obs, d, reason); err != nil {
				return stats, err
			}
		}
		if d.Act {
			if err := act(ctx, ex, c, obs, d, reason); err != nil {
				// Fact 11's race, handled rather than fatal (go-reviewer F6): a
				// worker can claim the task between the candidate read and the
				// close, and task_close then refuses active work. The claim-first
				// state row written above must NOT be left standing as
				// last_action='closed' — a close claim the pass did not earn is
				// exactly what D3's precondition trusts (go-reviewer delta,
				// 2026-09-09) — so the row is rewritten as an honest "observed,
				// did nothing"; the next pass re-evaluates against the fresh
				// status and logs the refusal properly. Anything else stays
				// loud and fatal.
				if strings.Contains(err.Error(), activeWorkRefusal) {
					slog.Warn("ticketstatus: task is active (claimed mid-pass, or a live send in flight); skipping",
						"key", c.key, "task", c.taskID, "err", err)
					none := Decision{Warranted: d.Warranted, Action: "none", DropReason: d.DropReason}
					if err := upsertState(ctx, pool, c, obs, none, reason); err != nil {
						return stats, err
					}
					stats.RefusedActive++
					continue
				}
				return stats, err
			}
		}
		if !(d.Act && d.Action == "closed") {
			if err := upsertState(ctx, pool, c, obs, d, reason); err != nil {
				return stats, err
			}
		}
		count(&stats, d)
	}
	return stats, nil
}

func count(stats *Stats, d Decision) {
	if d.Action == "unreadable" {
		stats.Unreadable++
		return
	}
	if !d.Act {
		stats.Converged++
		return
	}
	switch d.Action {
	case "closed":
		// A SWITCH on the reason, not an if/else (SWT-34 criterion 26): drop_reason
		// and its counter are one vocabulary, so a fourth value must be a visible
		// GAP rather than a wrong number silently folded into ticket_done. The
		// CHECK in migration 0025 is what stops such a row existing at all; this
		// switch's job is to make the day it happens legible.
		switch d.DropReason {
		case "ticket_done":
			stats.ClosedTicketDone++
		case "ticket_delivered":
			stats.ClosedTicketDelivered++
		case "not_assigned":
			stats.ClosedNotAssigned++
		}
	case "reopened":
		stats.Reopened++
	case "refused_active":
		stats.RefusedActive++
	case "suppressed_dismissed":
		stats.SuppressedDismissed++
	}
}

func orUnassigned(assignee string) string {
	if assignee == "" {
		return "(unassigned)"
	}
	return assignee
}

// decisionReason is the prose trail: the ticket, the fact that moved, and the
// assignee — what the log line and the state row both carry.
func decisionReason(obs Observation, d Decision) string {
	switch d.Action {
	case "closed":
		return fmt.Sprintf("ticketstatus: %s no longer warrants this task (%s; status %s/%s, assignee %s)",
			obs.TicketKey, d.DropReason, obs.StatusCategory, obs.StatusName, orUnassigned(obs.Assignee))
	case "reopened":
		return fmt.Sprintf("ticketstatus: %s warrants this task again (status %s/%s, assignee %s); restoring %s",
			obs.TicketKey, obs.StatusCategory, obs.StatusName, orUnassigned(obs.Assignee), d.RestoreStatus)
	case "refused_active":
		return fmt.Sprintf("ticketstatus: %s no longer warrants this task (%s; status %s/%s, assignee %s) "+
			"but the task is %s — refusing to close active work; close or release it and the next pass will drop it",
			obs.TicketKey, d.DropReason, obs.StatusCategory, obs.StatusName, orUnassigned(obs.Assignee), obs.TaskStatus)
	case "suppressed_dismissed":
		return fmt.Sprintf("ticketstatus: %s warrants a task again (status %s/%s, assignee %s) but this task "+
			"was DISMISSED by a human, which outranks the reconciler; it stays closed",
			obs.TicketKey, obs.StatusCategory, obs.StatusName, orUnassigned(obs.Assignee))
	default:
		return ""
	}
}

// act makes the ONE executor call a decision demands.
func act(ctx context.Context, ex *executor.Executor, c candidate, obs Observation, d Decision, reason string) error {
	var tool string
	args := map[string]any{"task_id": c.taskID}
	switch d.Action {
	case "closed":
		tool = "task_close"
		args["reason"] = reason
	case "reopened":
		tool = "task_reopen"
		args["status"] = d.RestoreStatus
		args["reason"] = reason
	case "refused_active", "suppressed_dismissed":
		tool = "task_append_log"
		args["kind"] = "log"
		args["message"] = reason
	default:
		return fmt.Errorf("ticketstatus: no executor call for action %q", d.Action)
	}
	raw, err := marshalArgs(args)
	if err != nil {
		return fmt.Errorf("ticketstatus: marshal %s args for task %d: %w", tool, c.taskID, err)
	}
	if _, err := ex.Execute(ctx, executor.Call{Tool: tool, Actor: Actor, Args: raw, TaskID: &c.taskID}); err != nil {
		return fmt.Errorf("ticketstatus: %s for %s (task %d): %w", tool, c.key, c.taskID, err)
	}
	return nil
}

// upsertState records what this pass observed and did — one row per ref,
// UPSERTed in place. closed_from_status is preserved on a convergent re-record
// (Decide keeps Action='closed' with Act=false exactly so this row keeps the
// one fact that authorises a later reopen), and acted_at survives no-op
// updates.
func upsertState(ctx context.Context, pool *pgxpool.Pool, c candidate, obs Observation, d Decision, reason string) error {
	var closedFrom *string
	if d.Action == "closed" {
		if d.Act {
			closedFrom = &obs.TaskStatus
		} else if c.state != nil && c.state.ClosedFromStatus != "" {
			closedFrom = &c.state.ClosedFromStatus
		}
	}
	var assignedToSelf *bool
	if obs.AssigneeKnown && obs.OwnAccountID != "" {
		v := obs.Assignee == obs.OwnAccountID
		assignedToSelf = &v
	}
	var drop, name, assignee *string
	if d.DropReason != "" {
		drop = &d.DropReason
	}
	if obs.StatusName != "" {
		name = &obs.StatusName
	}
	if obs.Assignee != "" {
		assignee = &obs.Assignee
	}
	var actedAt *time.Time
	if d.Act {
		now := time.Now()
		actedAt = &now
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO ticket_status_syncs
		   (external_ref_id, task_id, status_category, status_name, assignee_account_id,
		    assigned_to_self, last_action, drop_reason, closed_from_status, reason, observed_at, acted_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9, NULLIF($10,''), now(), $11)
		 ON CONFLICT (external_ref_id) DO UPDATE SET
		   task_id = EXCLUDED.task_id,
		   status_category = EXCLUDED.status_category,
		   status_name = EXCLUDED.status_name,
		   assignee_account_id = EXCLUDED.assignee_account_id,
		   assigned_to_self = EXCLUDED.assigned_to_self,
		   last_action = EXCLUDED.last_action,
		   drop_reason = EXCLUDED.drop_reason,
		   closed_from_status = EXCLUDED.closed_from_status,
		   reason = EXCLUDED.reason,
		   observed_at = EXCLUDED.observed_at,
		   acted_at = COALESCE(EXCLUDED.acted_at, ticket_status_syncs.acted_at)`,
		c.refID, c.taskID, obs.StatusCategory, name, assignee,
		assignedToSelf, d.Action, drop, closedFrom, reason, actedAt); err != nil {
		return fmt.Errorf("ticketstatus: record state for %s: %w", c.key, err)
	}
	return nil
}

// loadCandidates is the candidate query (criterion 30): every jira-keyed ref,
// its task, its project's gate, its dismissal, and this pass's recorded state.
func loadCandidates(ctx context.Context, pool *pgxpool.Pool, limit int) ([]candidate, error) {
	q := `
	SELECT r.id, r.external_key, t.id, t.status, p.ticket_assignee_gate,
	       p.ticket_delivered_statuses,
	       EXISTS (SELECT 1 FROM task_dismissals d
	                WHERE d.task_id = t.id AND d.reopened_at IS NULL) AS dismissed,
	       s.last_action, s.closed_from_status, s.status_category, s.assignee_account_id,
	       s.status_name
	  FROM external_refs r
	  JOIN tasks t ON t.id = r.task_id
	  JOIN projects p ON p.id = t.project_id
	  LEFT JOIN ticket_status_syncs s ON s.external_ref_id = r.id
	 WHERE r.system = 'jira'
	 ORDER BY r.external_key, r.id`
	args := []any{}
	if limit > 0 {
		q += ` LIMIT $1`
		args = append(args, limit)
	}
	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("ticketstatus: select candidates: %w", err)
	}
	defer rows.Close()

	var out []candidate
	for rows.Next() {
		var c candidate
		var lastAction, closedFrom, category, assignee, statusName *string
		if err := rows.Scan(&c.refID, &c.key, &c.taskID, &c.taskStat, &c.gateOn, &c.delivered, &c.dismissed,
			&lastAction, &closedFrom, &category, &assignee, &statusName); err != nil {
			return nil, fmt.Errorf("ticketstatus: scan candidate: %w", err)
		}
		if lastAction != nil {
			c.state = &State{LastAction: *lastAction}
			if closedFrom != nil {
				c.state.ClosedFromStatus = *closedFrom
			}
			if category != nil {
				c.state.StatusCategory = *category
			}
			if assignee != nil {
				c.state.Assignee = *assignee
			}
			// E6: the drop-triggering NAME is part of the "unchanged
			// observation" key once a configured name can cause the drop.
			if statusName != nil {
				c.state.StatusName = *statusName
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// EnsureSnapshots is the snapshot half of the reconciler (SWT-40 D-D3), shared
// by Run and the capture-time gate (capture.RunGate): it routes each key that
// needs a snapshot — missing, or stale beyond the TTL — to the lookup account
// claiming its prefix, fetches it raw-first through jira.LookupIssues, and
// RE-READS the stored rows (D19: write-then-read-back). The stored snapshot IS
// the cache, so a key either caller fetched inside the TTL is not fetched again
// by the other (D-D5).
//
// A key whose fetch failed, or that no account claims, keeps whatever it had
// stored (possibly nothing): an evidence gap for the caller, never a verdict.
// Stats carries Fetched, FetchSkippedTTL and FetchFailed only. cfg.Lookup nil
// means no credential: stored rows come back and nothing is fetched (D21);
// cfg.DryRun fetches nothing either.
func EnsureSnapshots(ctx context.Context, pool *pgxpool.Pool, keys []string, cfg Config) (map[string]Snapshot, Stats, error) {
	if pool == nil {
		return nil, Stats{}, errors.New("ticketstatus: nil database pool")
	}
	es, err := ensureSnapshots(ctx, pool, keys, cfg)
	return es.snaps, es.stats, err
}

// ensured is ensureSnapshots' answer: the snapshots, the fetch counters, and
// the two per-key facts only Run's decision loop reads.
type ensured struct {
	snaps          map[string]Snapshot
	stats          Stats
	wouldFetch     map[string]bool // dry run: routed keys a live pass would fetch
	routeAmbiguous map[string]bool // two lookup accounts claim the prefix
}

func ensureSnapshots(ctx context.Context, pool *pgxpool.Pool, keys []string, cfg Config) (ensured, error) {
	es := ensured{snaps: map[string]Snapshot{}, wouldFetch: map[string]bool{}, routeAmbiguous: map[string]bool{}}
	if len(keys) == 0 {
		return es, nil
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = LookupTTL()
	}

	snaps, err := loadSnapshots(ctx, pool, keys)
	if err != nil {
		return es, err
	}

	// The candidate-driven lookup (D16): route each key that needs a snapshot —
	// missing, or stale beyond the TTL — to the lookup account claiming its
	// prefix, fetch, and RE-READ the stored rows (D19: write-then-read-back).
	lookupAccts, err := loadLookupAccounts(ctx, pool)
	if err != nil {
		return es, err
	}
	needFetch := map[int64][]string{} // account id -> keys
	acctByID := map[int64]jira.Account{}
	seenKey := map[string]bool{}
	for _, key := range keys {
		if seenKey[key] {
			continue
		}
		seenKey[key] = true
		snap, have := snaps[key]
		fresh := have && snap.Count == 1 && time.Since(snap.IngestedAt) < ttl
		if min, ok := cfg.MinFresh[key]; ok && have && snap.IngestedAt.Before(min) {
			// The gate's freshness floor: stored before the newest held message
			// naming this key was first seen, so it cannot decide that message.
			fresh = false
		}
		if fresh && !cfg.Force {
			if _, outcome := RouteLookup(key, lookupAccts); outcome == "routed" {
				es.stats.FetchSkippedTTL++
			}
			continue
		}
		acct, outcome := RouteLookup(key, lookupAccts)
		if outcome != "routed" {
			// No claiming account, or an ambiguous claim: the stored snapshot,
			// if any, still decides; a key with neither is counted by the
			// caller — and an ambiguous route is remembered so its refusal is
			// VISIBLE (go-reviewer F4: an invisible refusal is
			// indistinguishable from an unpolled prefix).
			if outcome == "ambiguous" {
				es.routeAmbiguous[key] = true
				slog.Warn("ticketstatus: two lookup accounts claim this key's prefix; refusing to fetch",
					"key", key)
			}
			continue
		}
		if cfg.DryRun {
			// Criterion 42: a dry run cannot even mutate raw_source_items.
			es.wouldFetch[key] = true
			continue
		}
		if cfg.Lookup == nil {
			// D21: no credential. Loud, once per account, at the fetch site.
			continue
		}
		acctByID[acct.ID] = acct
		needFetch[acct.ID] = append(needFetch[acct.ID], key)
	}

	if cfg.Lookup == nil && !cfg.DryRun {
		for _, a := range lookupAccts {
			slog.Warn("ticketstatus: no lookup credential; skipping lookup account",
				"account", a.Email, "site", a.SiteBaseURL,
				"consequence", "its refs count unpolled until OPS_TOKEN_KEY is available")
		}
	}

	if len(needFetch) > 0 {
		sink := jira.NewSink(pool)
		ids := make([]int64, 0, len(needFetch))
		for id := range needFetch {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		verified := map[string]time.Time{} // key -> DB clock at the start of its successful GET
		for _, id := range ids {
			acct, keys := acctByID[id], needFetch[id]
			sort.Strings(keys)
			client, err := cfg.Lookup(ctx, acct)
			if err != nil {
				slog.Warn("ticketstatus: cannot build lookup client; refs stay on their stored snapshots",
					"account", acct.Email, "err", err)
				es.stats.FetchFailed += len(keys)
				continue
			}
			// The DATABASE clock, not this process's: VerifiedAt is compared
			// with normalized_messages.created_at, which the database stamps.
			var started time.Time
			if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&started); err != nil {
				return es, fmt.Errorf("ticketstatus: read the fetch start time: %w", err)
			}
			st, err := jira.LookupIssues(ctx, client, sink, acct, keys, jira.Config{})
			for _, k := range st.FetchedKeys {
				verified[k] = started
			}
			es.stats.Fetched += st.IssuesFetched
			es.stats.FetchFailed += st.FetchFailed
			if err != nil {
				// A whole-call failure (scope refusal, cursor write, /myself)
				// never becomes a verdict (criterion 32): the previous
				// snapshots, if any, still decide. Per-key fetch misses are
				// handled inside LookupIssues and simply leave no snapshot,
				// which the caller counts as unpolled.
				slog.Warn("ticketstatus: lookup pass failed; refs stay on their stored snapshots",
					"account", acct.Email, "err", err)
				es.stats.FetchFailed += len(keys) - st.IssuesFetched
			}
		}
		snaps, err = loadSnapshots(ctx, pool, keys)
		if err != nil {
			return es, err
		}
		for k, s := range snaps {
			if v, ok := verified[k]; ok && v.After(s.VerifiedAt) {
				s.VerifiedAt = v
				snaps[k] = s
			}
		}
	}
	es.snaps = snaps
	return es, nil
}

// DeliveredStatusesByProject reads projects.ticket_delivered_statuses for the
// given projects: the capture-time gate's copy of the value loadCandidates hands
// Observation.DeliveredStatuses, so the gate's Warranted sees the same delivered
// set as the reconciler's. It lives in this file so the column keeps its one
// reader file (SWT-34 criterion 22); one row per query, compared nowhere in SQL.
func DeliveredStatusesByProject(ctx context.Context, pool *pgxpool.Pool, projectIDs []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	for _, id := range projectIDs {
		var set []string
		if err := pool.QueryRow(ctx,
			`SELECT ticket_delivered_statuses FROM projects WHERE id = $1`, id).Scan(&set); err != nil {
			return nil, fmt.Errorf("ticketstatus: read delivered statuses for project %d: %w", id, err)
		}
		out[id] = set
	}
	return out, nil
}

// loadSnapshots reads the stored raw issues for every key, under polled and
// lookup accounts alike, with the STORING account's own identity (D12). Ids
// are computed in Go via jira.IssueRawID and bound as one array parameter
// (criterion 34).
func loadSnapshots(ctx context.Context, pool *pgxpool.Pool, keys []string) (map[string]Snapshot, error) {
	byID := map[string]string{} // raw external_id -> ticket key
	ids := make([]string, 0, len(keys))
	for _, key := range keys {
		id := jira.IssueRawID(key)
		if _, seen := byID[id]; !seen {
			byID[id] = key
			ids = append(ids, id)
		}
	}
	rows, err := pool.Query(ctx, `
		SELECT ri.external_id, ri.raw_json, ri.ingested_at,
		       COALESCE(sa.sync_cursor->>'own_account_id','')
		  FROM raw_source_items ri
		  JOIN source_accounts sa ON sa.id = ri.source_account_id
		       AND sa.provider IN ('jira','jira_lookup')
		 WHERE ri.external_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("ticketstatus: select snapshots: %w", err)
	}
	defer rows.Close()

	out := map[string]Snapshot{}
	for rows.Next() {
		var id, own string
		var raw []byte
		var ingested time.Time
		if err := rows.Scan(&id, &raw, &ingested, &own); err != nil {
			return nil, fmt.Errorf("ticketstatus: scan snapshot: %w", err)
		}
		key := byID[id]
		s := out[key]
		s.Count++
		s.Raw, s.IngestedAt, s.VerifiedAt, s.OwnAccountID = raw, ingested, ingested, own
		out[key] = s
	}
	return out, rows.Err()
}

func loadLookupAccounts(ctx context.Context, pool *pgxpool.Pool) ([]jira.Account, error) {
	return jira.NewSink(pool).ListLookupAccounts(ctx)
}

// tryLock is promote/capture's lock shape: a dedicated connection, an explicit
// unlock before release (a session-level advisory lock outlives a returned
// pooled connection), context.Background() on the unlock so cancellation cannot
// leak it.
func tryLock(ctx context.Context, pool *pgxpool.Pool) (func(), bool, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("ticketstatus: acquire lock conn: %w", err)
	}
	var ok bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, advisoryLockKey).Scan(&ok); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("ticketstatus: pg_try_advisory_lock: %w", err)
	}
	if !ok {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, advisoryLockKey); err != nil {
			slog.Warn("ticketstatus: releasing advisory lock", "err", err)
		}
		conn.Release()
	}, true, nil
}

func marshalArgs(args map[string]any) ([]byte, error) {
	return json.Marshal(args)
}
