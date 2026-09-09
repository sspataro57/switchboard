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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/jira"
	"github.com/sspataro57/switchboard/internal/executor"
)

// Actor is the identity every executor call carries (criterion 40), in the
// capture:{connector} / promote:{lane} shape.
const Actor = "ticketstatus:jira"

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
}

// Stats is criterion 43's counter vocabulary, one field per printed name.
type Stats struct {
	Considered, ClosedTicketDone, ClosedNotAssigned, Reopened int
	RefusedActive, SuppressedDismissed, Converged             int
	Unpolled, Ambiguous, Unreadable                           int
	Fetched, FetchSkippedTTL, FetchFailed                     int
}

// candidate is one external_refs row joined to its task and project.
type candidate struct {
	refID     int64
	key       string
	taskID    int64
	taskStat  string
	gateOn    bool
	dismissed bool
	state     *State
}

// snapshot is one stored raw issue row, with the storing account's identity.
type snapshot struct {
	raw          []byte
	ingestedAt   time.Time
	ownAccountID string
	count        int // rows found for this key; >1 is criterion 31's ambiguity
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
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = LookupTTL()
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

	snaps, err := loadSnapshots(ctx, pool, cands)
	if err != nil {
		return stats, err
	}

	// The candidate-driven lookup (D16): route each key that needs a snapshot —
	// missing, or stale beyond the TTL — to the lookup account claiming its
	// prefix, fetch, and RE-READ the stored rows (D19: write-then-read-back).
	lookupAccts, err := loadLookupAccounts(ctx, pool)
	if err != nil {
		return stats, err
	}
	needFetch := map[int64][]string{} // account id -> keys
	acctByID := map[int64]jira.Account{}
	wouldFetch := map[string]bool{}
	seenKey := map[string]bool{}
	for _, c := range cands {
		if seenKey[c.key] {
			continue
		}
		seenKey[c.key] = true
		snap, have := snaps[c.key]
		fresh := have && snap.count == 1 && time.Since(snap.ingestedAt) < ttl
		if fresh && !cfg.Force {
			if _, outcome := RouteLookup(c.key, lookupAccts); outcome == "routed" {
				stats.FetchSkippedTTL++
			}
			continue
		}
		acct, outcome := RouteLookup(c.key, lookupAccts)
		if outcome != "routed" {
			// No claiming account (or an ambiguous claim): the stored snapshot,
			// if any, still decides; a key with neither is counted below.
			continue
		}
		if cfg.DryRun {
			// Criterion 42: a dry run cannot even mutate raw_source_items.
			wouldFetch[c.key] = true
			continue
		}
		if cfg.Lookup == nil {
			// D21: no credential. Loud, once per account, at the fetch site.
			continue
		}
		acctByID[acct.ID] = acct
		needFetch[acct.ID] = append(needFetch[acct.ID], c.key)
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
		for _, id := range ids {
			acct, keys := acctByID[id], needFetch[id]
			sort.Strings(keys)
			client, err := cfg.Lookup(ctx, acct)
			if err != nil {
				slog.Warn("ticketstatus: cannot build lookup client; refs stay on their stored snapshots",
					"account", acct.Email, "err", err)
				stats.FetchFailed += len(keys)
				continue
			}
			st, err := jira.LookupIssues(ctx, client, sink, acct, keys, jira.Config{})
			stats.Fetched += st.IssuesFetched
			if err != nil {
				// A whole-call failure (scope refusal, cursor write, /myself)
				// never becomes a verdict (criterion 32): the previous
				// snapshots, if any, still decide below. Per-key fetch misses
				// are handled inside LookupIssues and simply leave no
				// snapshot, which the loop below counts as unpolled.
				slog.Warn("ticketstatus: lookup pass failed; refs stay on their stored snapshots",
					"account", acct.Email, "err", err)
				stats.FetchFailed += len(keys) - st.IssuesFetched
			}
		}
		snaps, err = loadSnapshots(ctx, pool, cands)
		if err != nil {
			return stats, err
		}
	}

	for _, c := range cands {
		stats.Considered++
		snap, have := snaps[c.key]
		if !have {
			stats.Unpolled++
			continue
		}
		if snap.count > 1 {
			// Criterion 31: two stored snapshots for one key. Refusing is
			// reversible; a wrong close is a task that vanishes with no
			// explanation.
			stats.Ambiguous++
			continue
		}

		facts, err := jira.IssueFacts(snap.raw)
		if err != nil {
			// A stored row that will not parse is a corrupt snapshot; the
			// operator should hear about it rather than the pass shrugging.
			return stats, fmt.Errorf("ticketstatus: stored snapshot for %s is unreadable: %w", c.key, err)
		}
		obs := Observation{
			TicketKey:      c.key,
			StatusCategory: facts.StatusCategory,
			StatusName:     facts.StatusName,
			StatusKnown:    facts.StatusKnown,
			Assignee:       facts.Assignee,
			AssigneeKnown:  facts.AssigneeKnown,
			OwnAccountID:   snap.ownAccountID,
			GateOn:         c.gateOn,
			TaskStatus:     c.taskStat,
			Dismissed:      c.dismissed,
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
		if d.Act {
			if err := act(ctx, ex, c, obs, d, reason); err != nil {
				return stats, err
			}
		}
		if err := upsertState(ctx, pool, c, obs, d, reason); err != nil {
			return stats, err
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
		if d.DropReason == "not_assigned" {
			stats.ClosedNotAssigned++
		} else {
			stats.ClosedTicketDone++
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
	       EXISTS (SELECT 1 FROM task_dismissals d WHERE d.task_id = t.id) AS dismissed,
	       s.last_action, s.closed_from_status, s.status_category, s.assignee_account_id
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
		var lastAction, closedFrom, category, assignee *string
		if err := rows.Scan(&c.refID, &c.key, &c.taskID, &c.taskStat, &c.gateOn, &c.dismissed,
			&lastAction, &closedFrom, &category, &assignee); err != nil {
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
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// loadSnapshots reads the stored raw issues for every candidate key, under
// polled and lookup accounts alike, with the STORING account's own identity
// (D12). Ids are computed in Go via jira.IssueRawID and bound as one array
// parameter (criterion 34).
func loadSnapshots(ctx context.Context, pool *pgxpool.Pool, cands []candidate) (map[string]snapshot, error) {
	byID := map[string]string{} // raw external_id -> ticket key
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		id := jira.IssueRawID(c.key)
		if _, seen := byID[id]; !seen {
			byID[id] = c.key
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

	out := map[string]snapshot{}
	for rows.Next() {
		var id, own string
		var raw []byte
		var ingested time.Time
		if err := rows.Scan(&id, &raw, &ingested, &own); err != nil {
			return nil, fmt.Errorf("ticketstatus: scan snapshot: %w", err)
		}
		key := byID[id]
		s := out[key]
		s.count++
		s.raw, s.ingestedAt, s.ownAccountID = raw, ingested, own
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
