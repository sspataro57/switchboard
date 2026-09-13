package capture

// The routing tier's APPLICATION (SWT-40 Part B, docs/tickets/inquiry-promote_SPEC.md
// B-D2, B-D5, B-D6, B-D7): for an inbound message the rules left unmatched on an
// ARMED account with candidate rows, decide which candidate project it belongs
// to and record that as a capture_decisions row with mode='route'.
//
// It is capture's own log and nothing else (B5, invariant 3): an attribution,
// never a task, never a tool call, never a send. It never reaches a model: the
// LLM stage is the classify route LANE, a leaf consumer, and this driver reads
// the verdict that lane RECORDED (fields.project_id, the resolved candidate,
// and fields.grounded, decided at classify time). It runs as the pipelined
// `route_apply` stage, woken by `route_classified`, under capture's own lock
// 0x5157_0015 (E-D4), so it serializes with every connector's capture pass and
// with the gate.
//
// Why mode='route' and not a second live row, or a side table (B-D5): a second
// live row per message is impossible under capture_decisions_live_uniq, and
// every latest-decision reader already follows this table. The partial
// capture_decisions_route_uniq gives one route per message, forever; every
// ON CONFLICT here restates its predicate. The shadow-overwrite guard is
// pendingMessages' exclusion of route rows in every mode (D-D2).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The four steps of B-D2, as capture_decisions.route_step stores them.
const (
	RouteStepThread  = "thread"
	RouteStepSingle  = "single"
	RouteStepModel   = "model"
	RouteStepDefault = "default"
)

// Why a message was left unrouted this pass (RouteDecision.Reason when Step is
// empty). None writes a row: the message stays in the inbox.
const (
	// No classify_route verdict exists yet: a missing verdict NEVER falls to the
	// default (B-D2), or the default would win every race with the GPU.
	RouteReasonPendingVerdict = "pending_verdict"
	// The verdict chose nothing confidently-grounded and the account has no
	// default candidate: the message stays unmatched.
	RouteReasonNoDefault = "no_default"
	// The newest verdict was recorded before the account was armed (B-D7): it is
	// not applied at step 3 and does not fall to the default either. The
	// post-arming backfill re-classifies it (the classify inbox's amendment).
	RouteReasonBeforeArming = "verdict_before_arming"
	// The chosen project stopped being one of the account's candidates between
	// the pass's candidate read and the insert (a route_candidate_remove mid-
	// batch). Nothing is written, so the message retries on the next pass against
	// the current candidate set.
	RouteReasonCandidateRevoked = "candidate_revoked"
)

// RouteDefaultLimit bounds one route_apply pass when RouteApplyConfig.Limit is
// 0. Application is two small reads and at most one insert per message, so the
// bound is generous; a pass that fills it is repeated at once by the stage loop.
const RouteDefaultLimit = 500

// ErrRouteLockHeld is RunRouteApply's error when capture's advisory lock is
// held elsewhere; pipelined maps it to pipeline.ErrLockHeld (retry later, never
// a failure).
var ErrRouteLockHeld = errors.New("capture route: the capture-rules advisory lock is held elsewhere")

// RouteFacts is everything about a message's surroundings DecideRoute may know.
type RouteFacts struct {
	// ThreadProjects are the projects named by the latest NON-route decision of
	// each OTHER inbound message on the thread (attributed, task or task_log; a
	// held row's fate is pending and does not count). Rules/gate attribution
	// only: a route never begets a route (routeThreadProjects). Repeats are
	// allowed.
	ThreadProjects []int64
	// ArmedAt is the account's source_accounts.route_after. The driver never
	// calls DecideRoute for an unarmed account.
	ArmedAt time.Time
}

// RouteCandidate is one of the account's source_account_projects rows.
type RouteCandidate struct {
	ProjectID int64
	IsDefault bool
}

// RouteVerdict is the message's newest ok classify_route verdict, as recorded.
type RouteVerdict struct {
	ExtractionID int64     // the classify_route ai_extractions.id
	ProjectID    int64     // fields.project_id: the resolved candidate, 0 = none chosen
	Grounded     bool      // fields.grounded (classify.Grounded, decided at classify time)
	RecordedAt   time.Time // ai_runs.created_at — the verdict clock (B-D7)
}

// RouteDecision is what one message becomes.
type RouteDecision struct {
	Step         string // thread | single | model | default; "" = write no row
	ProjectID    int64  // the row's project; 0 iff Step == ""
	ExtractionID int64  // set iff Step == "model"
	Reason       string // Step == "": pending_verdict | no_default | verdict_before_arming
}

// RouteApplyConfig drives one RunRouteApply pass.
type RouteApplyConfig struct {
	// Limit bounds the inbox; 0 means RouteDefaultLimit.
	Limit int
	// Since is REQUIRED (> 0): the pass window, on the message's sent_at. Steps
	// 1-2 apply to any message inside it, whenever it was sent relative to
	// arming; step 3 is forward-only on the verdict clock (B-D7).
	Since time.Duration
}

// RouteStats is one pass's counters. Written is pipelined's processed count:
// rows inserted, each a message that left the inbox. An unrouted message never
// counts, so the stage loop never re-runs at once over the same pending rows.
type RouteStats struct {
	Written  int            // mode='route' rows inserted
	ByStep   map[string]int // thread | single | model | default
	Unrouted map[string]int // pending_verdict | no_default | verdict_before_arming | candidate_revoked
}

// routeRow is one route_apply inbox row.
type routeRow struct {
	messageID, rawItemID, threadID, accountID int64
	armedAt                                   time.Time
}

// RunRouteApply is one route_apply pass: under capture's lock, read the inbox
// (inbound; a live 'unmatched' decision exists; the latest decision in any mode
// is still 'unmatched'; the receiving account is armed and has candidate rows;
// sent within cfg.Since), decide each message with DecideRoute and write one
// mode='route' row per routed message. No executor, no task, no tool call.
func RunRouteApply(ctx context.Context, pool *pgxpool.Pool, cfg RouteApplyConfig) (RouteStats, error) {
	stats := RouteStats{ByStep: map[string]int{}, Unrouted: map[string]int{}}
	// Refused before any I/O: without a window a pass would walk every
	// unmatched message ever captured on the account.
	if cfg.Since <= 0 {
		return stats, errors.New("capture route: RouteApplyConfig.Since is required (> 0) — the pass window steps " +
			"1-2 apply inside (B-D7); an unbounded pass walks the account's whole history")
	}
	if pool == nil {
		return stats, errors.New("capture route: nil database pool")
	}
	limit := cfg.Limit
	if limit <= 0 {
		limit = RouteDefaultLimit
	}

	release, held, err := tryRulesLock(ctx, pool)
	if err != nil {
		return stats, err
	}
	if !held {
		return stats, ErrRouteLockHeld
	}
	defer release()

	inbox, err := routeInbox(ctx, pool, cfg.Since, limit)
	if err != nil {
		return stats, err
	}
	// The candidate set is cached per account for the pass; it may go stale
	// mid-batch (a route_candidate_remove), which is why the insert revalidates.
	candidates := map[int64][]RouteCandidate{}
	for _, r := range inbox {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		cands, ok := candidates[r.accountID]
		if !ok {
			if cands, err = routeCandidates(ctx, pool, r.accountID); err != nil {
				return stats, err
			}
			candidates[r.accountID] = cands
		}
		if err := applyRoute(ctx, pool, r, cands, &stats); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// applyRoute is one message's step of a pass: read its facts, decide with the
// (possibly stale) cached candidate set, and write at most one route row.
func applyRoute(ctx context.Context, pool *pgxpool.Pool, r routeRow, cands []RouteCandidate, stats *RouteStats) error {
	thread, err := routeThreadProjects(ctx, pool, r)
	if err != nil {
		return err
	}
	verdict, err := routeLatestVerdict(ctx, pool, r.rawItemID)
	if err != nil {
		return err
	}
	d := DecideRoute(RouteFacts{ThreadProjects: thread, ArmedAt: r.armedAt}, cands, verdict)
	if d.Step == "" {
		stats.Unrouted[d.Reason]++
		return nil
	}
	wrote, current, err := insertRouteDecision(ctx, pool, r, d)
	if err != nil {
		return err
	}
	switch {
	case !current:
		stats.Unrouted[RouteReasonCandidateRevoked]++
	case wrote:
		stats.Written++
		stats.ByStep[d.Step]++
	}
	return nil
}

// routeInbox is the route_apply queue-as-filter, oldest first. Each clause is
// pinned by a fixture in route_integration_test.go:
//
//   - `nm.direction = 'inbound'` (invariant 5; capture decides nothing else);
//   - EXISTS a live 'unmatched' decision: the rules tier ran on it live and left
//     it unmatched (B-D2), so a shadow-only message is not this tier's;
//   - the LATEST decision (any mode) is 'unmatched': a later re-pointing, a
//     gate resolution or this message's own route row takes it out — which is
//     also why run-twice writes nothing;
//   - the receiving account is ARMED (route_after set; B7, NULL writes
//     nothing) and has candidate rows (B-D1);
//   - sent within the pass window.
//
// The receiving account is read through the raw item's source_account_id, the
// same join pendingMessages makes; the provider payload is never read.
func routeInbox(ctx context.Context, pool *pgxpool.Pool, since time.Duration, limit int) ([]routeRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT nm.id, nm.raw_source_item_id, COALESCE(nm.thread_id, 0), sa.id, sa.route_after
		  FROM normalized_messages nm
		  JOIN raw_source_items ri ON ri.id = nm.raw_source_item_id
		  JOIN source_accounts sa ON sa.id = ri.source_account_id
		  JOIN LATERAL (SELECT cd.action FROM capture_decisions cd
		                 WHERE cd.message_id = nm.id ORDER BY cd.id DESC LIMIT 1) latest ON true
		 WHERE nm.direction = 'inbound'
		   AND latest.action = 'unmatched'
		   AND EXISTS (SELECT 1 FROM capture_decisions lv
		                WHERE lv.message_id = nm.id AND lv.mode = 'live' AND lv.action = 'unmatched')
		   AND sa.route_after IS NOT NULL
		   AND EXISTS (SELECT 1 FROM source_account_projects sap WHERE sap.source_account_id = sa.id)
		   AND COALESCE(nm.sent_at, nm.created_at) >= now() - $1::interval
		 ORDER BY COALESCE(nm.sent_at, nm.created_at), nm.id
		 LIMIT $2`, since.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("select route_apply inbox: %w", err)
	}
	defer rows.Close()
	var out []routeRow
	for rows.Next() {
		var r routeRow
		if err := rows.Scan(&r.messageID, &r.rawItemID, &r.threadID, &r.accountID, &r.armedAt); err != nil {
			return nil, fmt.Errorf("scan route_apply inbox row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate route_apply inbox: %w", err)
	}
	return out, nil
}

// routeCandidates reads the account's closed candidate set.
func routeCandidates(ctx context.Context, pool *pgxpool.Pool, accountID int64) ([]RouteCandidate, error) {
	rows, err := pool.Query(ctx,
		`SELECT project_id, is_default FROM source_account_projects WHERE source_account_id = $1 ORDER BY id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("select route candidates for account %d: %w", accountID, err)
	}
	defer rows.Close()
	var out []RouteCandidate
	for rows.Next() {
		var c RouteCandidate
		if err := rows.Scan(&c.ProjectID, &c.IsDefault); err != nil {
			return nil, fmt.Errorf("scan route candidate: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate route candidates for account %d: %w", accountID, err)
	}
	return out, nil
}

// routeThreadProjects reads step 1's facts: the project named by the latest
// NON-route decision of every OTHER inbound message on the thread. Inbound only,
// for the reason classify's neighbours() records: an outbound message never
// carries a decision. A thread-less message has no neighbours.
//
// The thread step reads rules/gate attribution only; a route never begets a
// route (SPEC B-D2 amendment, 2026-09-13). mode='route' rows are filtered INSIDE
// the LATERAL, so a neighbour counts by its latest shadow/live/gate decision and
// a neighbour whose only attribution is a route contributes nothing. Otherwise
// one weakly grounded, wrong model route (or a default) would be deterministic
// thread evidence, take precedence over every later message's fresh verdict,
// and route the whole thread after it.
func routeThreadProjects(ctx context.Context, pool *pgxpool.Pool, r routeRow) ([]int64, error) {
	if r.threadID == 0 {
		return nil, nil
	}
	rows, err := pool.Query(ctx, `
		SELECT latest.project_id
		  FROM normalized_messages o
		  JOIN LATERAL (SELECT cd.action, cd.project_id FROM capture_decisions cd
		                 WHERE cd.message_id = o.id AND cd.mode <> 'route'
		                 ORDER BY cd.id DESC LIMIT 1) latest ON true
		 WHERE o.thread_id = $1 AND o.id <> $2 AND o.direction = 'inbound'
		   AND latest.action IN ('attributed', 'task', 'task_log')
		   AND latest.project_id IS NOT NULL`, r.threadID, r.messageID)
	if err != nil {
		return nil, fmt.Errorf("select thread projects for message %d: %w", r.messageID, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan thread project: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate thread projects for message %d: %w", r.messageID, err)
	}
	return out, nil
}

// routeLatestVerdict reads the message's NEWEST ok classify_route verdict on
// its raw item, or nil. Newest, never oldest: after arming, the backfill writes
// a fresh verdict beside the shadow one, and only the fresh one may apply.
func routeLatestVerdict(ctx context.Context, pool *pgxpool.Pool, rawItemID int64) (*RouteVerdict, error) {
	rows, err := pool.Query(ctx, `
		SELECT e.id, e.fields, r.created_at
		  FROM ai_extractions e
		  JOIN ai_runs r ON r.id = e.ai_run_id AND r.worker_type = 'classify_route' AND r.status = 'ok'
		 WHERE e.raw_source_item_id = $1
		 ORDER BY r.created_at DESC, e.id DESC
		 LIMIT 1`, rawItemID)
	if err != nil {
		return nil, fmt.Errorf("select route verdict for raw item %d: %w", rawItemID, err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	var v RouteVerdict
	var raw []byte
	if err := rows.Scan(&v.ExtractionID, &raw, &v.RecordedAt); err != nil {
		return nil, fmt.Errorf("scan route verdict: %w", err)
	}
	// A verdict whose fields do not decode is a verdict that chose nothing: it
	// can still fall to the default, never to a model step.
	var f struct {
		ProjectID *int64 `json:"project_id"`
		Grounded  bool   `json:"grounded"`
	}
	if err := json.Unmarshal(raw, &f); err == nil {
		if f.ProjectID != nil {
			v.ProjectID = *f.ProjectID
		}
		v.Grounded = f.Grounded
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate route verdict for raw item %d: %w", rawItemID, err)
	}
	return &v, nil
}

// insertRouteDecision writes the one mode='route' row, if and only if the
// chosen project is STILL one of the message's receiving account's candidates.
// It returns (wrote, current): current=false means the source_account_projects
// row is gone (a route_candidate_remove after the pass cached the candidate
// set), nothing was written, and the caller counts candidate_revoked so the
// message retries next pass against the current set. current=true, wrote=false
// is the conflict: the message already carries its one route.
//
// The revalidation and the insert are ONE statement. FOR KEY SHARE blocks a
// concurrent DELETE of the candidate row until this transaction ends, and one
// that committed first is re-checked under READ COMMITTED and seen as gone, so
// a removal can never be overridden by a route decided from a stale cache.
//
// The ON CONFLICT restates the partial index's predicate
// (capture_decisions_route_uniq is PARTIAL; arbiter inference matches it only
// when the WHERE is repeated), and a conflict writes nothing: one route per
// message, forever.
func insertRouteDecision(ctx context.Context, pool *pgxpool.Pool, r routeRow, d RouteDecision) (wrote, current bool, err error) {
	var extraction any
	if d.Step == RouteStepModel {
		extraction = d.ExtractionID
	}
	var inserted int
	err = pool.QueryRow(ctx, `
		WITH cand AS (
		  SELECT EXISTS (SELECT 1 FROM source_account_projects sap
		                  WHERE sap.source_account_id = $7 AND sap.project_id = $3::bigint
		                  FOR KEY SHARE) AS current
		), ins AS (
		  INSERT INTO capture_decisions
		    (message_id, raw_source_item_id, mode, action, project_id, route_step, ai_extraction_id, reason)
		  SELECT $1, $2, 'route', 'attributed', $3::bigint, $4::text, $5::bigint, $6::text
		    FROM cand WHERE cand.current
		  ON CONFLICT (message_id) WHERE mode = 'route' DO NOTHING
		  RETURNING 1
		)
		SELECT (SELECT current FROM cand), (SELECT count(*) FROM ins)`,
		r.messageID, r.rawItemID, d.ProjectID, d.Step, extraction, routeReasonText(d), r.accountID).
		Scan(&current, &inserted)
	if err != nil {
		return false, false, fmt.Errorf("record route decision for message %d: %w", r.messageID, err)
	}
	return inserted == 1, current, nil
}

// routeReasonText is the row's human-readable reason.
func routeReasonText(d RouteDecision) string {
	switch d.Step {
	case RouteStepThread:
		return "route thread: the thread's other messages are attributed to exactly this one candidate"
	case RouteStepSingle:
		return "route single: the account has exactly one candidate"
	case RouteStepModel:
		return fmt.Sprintf("route model: a grounded classify_route verdict chose this candidate (extraction %d)", d.ExtractionID)
	default:
		return "route default: no grounded choice; the account's default candidate (O3)"
	}
}

// DecideRoute is B-D2, PURE: a function of (facts, candidates, verdict) with no
// I/O and no clock — the arming and verdict instants arrive as values. Kept
// LAST in this file so the structure test's body slice ends at EOF.
//
//  1. thread: the thread's other messages name exactly ONE project, and it is a
//     candidate (closed set: a non-candidate, or a candidate plus anything
//     else, is not a thread step);
//  2. single: the account has exactly one candidate;
//     — no verdict: pending_verdict, never the default;
//     — the verdict predates arming: verdict_before_arming, never applied and
//     never defaulted (B-D7; RecordedAt == ArmedAt counts as after);
//  3. model: a grounded verdict naming a candidate;
//  4. default: otherwise the account's default (O3); none → no_default.
func DecideRoute(facts RouteFacts, candidates []RouteCandidate, verdict *RouteVerdict) RouteDecision {
	isCandidate := map[int64]bool{}
	var def int64
	for _, c := range candidates {
		isCandidate[c.ProjectID] = true
		if c.IsDefault {
			def = c.ProjectID
		}
	}

	threadSet := map[int64]bool{}
	for _, p := range facts.ThreadProjects {
		threadSet[p] = true
	}
	if len(threadSet) == 1 {
		for p := range threadSet {
			if isCandidate[p] {
				return RouteDecision{Step: RouteStepThread, ProjectID: p}
			}
		}
	}

	if len(candidates) == 1 {
		return RouteDecision{Step: RouteStepSingle, ProjectID: candidates[0].ProjectID}
	}

	if verdict == nil {
		return RouteDecision{Reason: RouteReasonPendingVerdict}
	}
	if verdict.RecordedAt.Before(facts.ArmedAt) {
		return RouteDecision{Reason: RouteReasonBeforeArming}
	}
	if verdict.Grounded && verdict.ProjectID != 0 && isCandidate[verdict.ProjectID] {
		return RouteDecision{Step: RouteStepModel, ProjectID: verdict.ProjectID, ExtractionID: verdict.ExtractionID}
	}
	if def != 0 {
		return RouteDecision{Step: RouteStepDefault, ProjectID: def}
	}
	return RouteDecision{Reason: RouteReasonNoDefault}
}
