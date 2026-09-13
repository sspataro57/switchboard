package classify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/provider"
)

// PGStore is the Postgres side. It reads capture decisions and writes ai_runs /
// ai_extractions — nothing else. There is deliberately no task-write method
// anywhere on this type or on the Store interface: shadow mode is structural,
// and going live ADDS an executor create_task call rather than removing a guard
// here.
type PGStore struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// inboxWhere is criterion 11's filter, and it is THE predicate that protects
// this worker — not the class fold, which is constant here by construction.
//
// Four conditions, each load-bearing:
//
//   - `nm.direction = 'inbound'`. Asserted, but honestly: it cannot discriminate
//     anything, because the capture engine only ever decides inbound messages
//     (invariant 5) so an outbound message can never carry an 'attributed'
//     decision and the decision join already excludes it. It is here so a reader
//     does not have to know that to trust the query. It must NEVER be described
//     as the thing that keeps our own sends out.
//   - the LATEST decision, via `ORDER BY cd.id DESC LIMIT 1`. Not "any decision
//     that names a project": a rule narrowed after an attribution takes the
//     message back out, which is the ordinary way a rule set is tuned. Reading
//     any-project-bearing-decision would keep classifying against a project the
//     rules no longer assign.
//   - `latest.action = 'attributed'`. Note WHICH of these exclusions this clause
//     actually performs, because it is not the obvious one. Unmatched is already
//     excluded by the project join: the schema enforces
//     `(action='unmatched') = (project_id IS NULL)`, so an unmatched row has no
//     project to join to. Verified by mutation — adding 'unmatched' to this list
//     changes nothing. What the clause DOES exclude is `task` and `task_log`,
//     which name a project and would otherwise be pending: those messages already
//     produced a task, and classifying them again would double-count them. That
//     is a real trap rather than a hypothetical — give a personal capture rule an
//     `external_system` and its messages start arriving as 'task'. The
//     integration suite pins it with a 'task' fixture, added after mutation
//     testing showed the clause was untested.
//   - `p.ai_locality = 'local_only'`. THE BOUNDARY: where a message may be
//     sent, and a leak is irreversible. Drop this join and the worker starts
//     classifying client work; the integration test's ai_locality='any' case
//     is the assertion that goes red.
//   - `p.ai_classify` (SWT-23). THE WORKLOAD FLAG: whether mail attributed
//     here is worth an actionability verdict at 7.2 s of GPU each — a stall is
//     one UPDATE. The two clauses answer DIFFERENT questions and both stay:
//     collapsing them would make the boundary depend on a workload decision.
//     The `bulk` project is local_only + ai_classify=false, which is what
//     keeps ~5,700 claimed marketing messages out of this lane — and the
//     local_only+false control fixture is the assertion that keeps this
//     clause from going inert.
//
// The NOT EXISTS keys on worker_type='classify', so this worker's extractions
// and triage's cannot hide each other's messages.
const inboxWhere = `
	  FROM normalized_messages nm
	  JOIN LATERAL (SELECT cd.action, cd.project_id
	                  FROM capture_decisions cd
	                 WHERE cd.message_id = nm.id
	                 ORDER BY cd.id DESC LIMIT 1) latest ON true
	  JOIN projects p ON p.id = latest.project_id
	 WHERE nm.direction = 'inbound'
	   AND latest.action = 'attributed'
	   AND p.ai_locality = 'local_only'
	   AND p.ai_classify
	   AND NOT EXISTS (
	         SELECT 1 FROM ai_extractions e
	           JOIN ai_runs r ON r.id = e.ai_run_id AND r.worker_type = 'classify'
	          WHERE e.raw_source_item_id = nm.raw_source_item_id)`

// inboxWhereResidue is the RESIDUE lane's filter (SWT-23): the unmatched pile,
// which is triage's inbox in name and nothing's in practice. LEFT JOINs,
// because an unmatched decision has no project to join to (0015's CHECK makes
// (action='unmatched') = (project_id IS NULL) a schema fact) — an inner join
// here would silently empty the lane. `latest.action = 'unmatched'` is spelled
// as the positive on purpose: action has exactly four values and no "ignore"
// verb, so `<> 'attributed'` would catch 'task'/'task_log' by accident. The
// NOT EXISTS keys on worker_type='classify_residue' so this lane's verdicts
// and the personal lane's cannot hide each other's messages, and rows scanned
// from this filter carry Attribution = AttrUnmatched — which is what makes
// ClassOf restrict them through the non-AttrProject branch.
const inboxWhereResidue = `
	  FROM normalized_messages nm
	  JOIN LATERAL (SELECT cd.action, cd.project_id
	                  FROM capture_decisions cd
	                 WHERE cd.message_id = nm.id
	                 ORDER BY cd.id DESC LIMIT 1) latest ON true
	  LEFT JOIN projects p ON p.id = latest.project_id
	 WHERE nm.direction = 'inbound'
	   AND latest.action = 'unmatched'
	   AND NOT EXISTS (
	         SELECT 1 FROM ai_extractions e
	           JOIN ai_runs r ON r.id = e.ai_run_id AND r.worker_type = 'classify_residue'
	          WHERE e.raw_source_item_id = nm.raw_source_item_id)`

// inboxWhereInquiry is the INQUIRY lane's filter (SWT-33 criterion 9): inbound
// messages whose LATEST capture decision attributes them to a project armed
// with ai_inquiry, not yet given an inquiry verdict.
//
//   - `nm.direction = 'inbound'` — asserted for the reader, and it cannot
//     discriminate on its own, for the reason inboxWhere records: the capture
//     engine only ever decides inbound messages (invariant 5), so our own sends
//     are kept out by the decision join, never by this line.
//   - `latest.action = 'attributed'` on the LATEST decision, spelled as the
//     positive: 'task' and 'task_log' NAME a project and so survive the join,
//     and those messages already produced a task.
//   - `p.ai_inquiry` (0024). THE WORKLOAD FLAG, and the only clause that keeps an
//     unarmed project's client conversation out of this lane — the integration
//     suite's identical-but-unarmed project is what goes red when it is dropped.
//   - NOT EXISTS keyed on worker_type='classify_inquiry', so a personal- or
//     residue-lane verdict on the same raw item never hides a message from this
//     lane, or the other way round.
//
// DELIBERATELY NO ai_locality CLAUSE (criterion 10), and the absence is not an
// omission. `collaboratory`, the one armed project, is ai_locality='any', so the
// personal lane's `= 'local_only'` clause would return zero rows here and leave
// the lane silently inert. This lane is contained elsewhere, twice over:
// cmd/classify's buildRouter builds the router with general = nil — there is no
// hosted client to fall back TO — and Run pins this lane's routed class to
// ClassRestricted (criterion 11), so even a router that HAD a general client
// would never be offered these messages or their thread neighbours' bodies.
const inboxWhereInquiry = `
	  FROM normalized_messages nm
	  JOIN LATERAL (SELECT cd.action, cd.project_id
	                  FROM capture_decisions cd
	                 WHERE cd.message_id = nm.id
	                 ORDER BY cd.id DESC LIMIT 1) latest ON true
	  JOIN projects p ON p.id = latest.project_id
	 WHERE nm.direction = 'inbound'
	   AND latest.action = 'attributed'
	   AND p.ai_inquiry
	   AND NOT EXISTS (
	         SELECT 1 FROM ai_extractions e
	           JOIN ai_runs r ON r.id = e.ai_run_id AND r.worker_type = 'classify_inquiry'
	          WHERE e.raw_source_item_id = nm.raw_source_item_id)`

// routeAccountJoin is B-D9's NAMED CARVE-OUT (SWT-40 Part B), and the only
// place internal/classify touches the raw item table: the route inbox has to
// know which source account RECEIVED a message, because the closed candidate
// set is per account (B-D1), and that fact lives only on the raw item's
// source_account_id. It selects that one id column and nothing else — the
// provider payload is never read here (invariant 1; the structure test bans
// the payload column package-wide and allows this table name only inside this
// constant). Every query that needs the account reuses THIS constant, so the
// raw touch stays one line a reviewer can find. Requires the alias `nm` for
// normalized_messages; yields `acct.source_account_id`.
const routeAccountJoin = `
	  JOIN LATERAL (SELECT ri.source_account_id FROM raw_source_items ri
	                 WHERE ri.id = nm.raw_source_item_id) acct ON true`

// inboxWhereRoute is the ROUTE lane's filter (SWT-40 Part B, criterion B2):
//
//   - `nm.direction = 'inbound'` — asserted for the reader; capture never
//     decides an outbound message (invariant 5), so the decision clauses below
//     already exclude our own sends;
//   - EXISTS a mode='live' 'unmatched' decision — the rules tier ran on this
//     message live and left it unmatched (B-D2). A shadow-only message was never
//     decided live and is not the routing tier's to touch;
//   - the LATEST decision (ORDER BY id DESC, ANY mode) is 'unmatched' — a later
//     shadow re-pointing, a gate resolution or an existing route row takes the
//     message out. No mode predicate on purpose: every latest-decision reader
//     follows the newest row;
//   - the receiving account has >= 1 source_account_projects row (B-D1: only
//     accounts with candidates are routed at all);
//   - NOT EXISTS a CURRENT classify_route verdict. Current (SPEC amendment
//     2026-09-13, B7): with route_after NULL (unarmed, shadow) any route verdict
//     counts, so shadow classifies each message once; once armed, only a
//     verdict recorded at or after route_after counts, so the shadow period's
//     verdicts do not keep the backlog out of the post-arming backfill. One
//     fresh verdict is all a message ever needs. route_apply never applies a
//     pre-arming verdict (internal/capture/route.go), so B7 stands.
//
// LEFT JOIN projects: an unmatched decision has no project (0015's CHECK), and
// rows scanned from here carry Attribution = AttrUnmatched, which is what makes
// ClassOf restrict them (B-D8).
const inboxWhereRoute = `
	  FROM normalized_messages nm` + routeAccountJoin + `
	  JOIN source_accounts sa ON sa.id = acct.source_account_id
	  JOIN LATERAL (SELECT cd.action, cd.project_id
	                  FROM capture_decisions cd
	                 WHERE cd.message_id = nm.id
	                 ORDER BY cd.id DESC LIMIT 1) latest ON true
	  LEFT JOIN projects p ON p.id = latest.project_id
	 WHERE nm.direction = 'inbound'
	   AND latest.action = 'unmatched'
	   AND EXISTS (SELECT 1 FROM capture_decisions lv
	                WHERE lv.message_id = nm.id AND lv.mode = 'live' AND lv.action = 'unmatched')
	   AND EXISTS (SELECT 1 FROM source_account_projects sap
	                WHERE sap.source_account_id = acct.source_account_id)
	   AND NOT EXISTS (
	         SELECT 1 FROM ai_extractions e
	           JOIN ai_runs r ON r.id = e.ai_run_id AND r.worker_type = 'classify_route'
	          WHERE e.raw_source_item_id = nm.raw_source_item_id
	            AND (sa.route_after IS NULL OR r.created_at >= sa.route_after))`

// routeSelectAccount is the one extra column the route loaders select after
// inboxSelect's: the receiving account, from routeAccountJoin.
const routeSelectAccount = `, acct.source_account_id`

// inboxSelect is shared by every lane's loader. The last two columns (SWT-33
// criterion 13) are the thread identity an inquiry verdict records VERBATIM:
// the message's own provider id, and the thread key as normalized_threads
// stores it. Selected, never parsed — the one reading of the slack key's shape
// is slackweb.IsRootedThreadKey.
const inboxSelect = `
	SELECT nm.id, nm.raw_source_item_id, COALESCE(nm.thread_id, 0),
	       COALESCE(nm.sent_at, now()), COALESCE(nm.sender,''), COALESCE(nm.subject,''),
	       COALESCE(nm.channel,''), COALESCE(nm.body_text,''), COALESCE(nm.direction,''),
	       COALESCE(p.id, 0), COALESCE(p.slug, ''), COALESCE(p.ai_locality = 'local_only', false),
	       COALESCE(nm.links, '[]'::jsonb),
	       COALESCE(nm.external_message_id, ''),
	       COALESCE((SELECT nt.thread_key FROM normalized_threads nt WHERE nt.id = nm.thread_id), '')`

// PendingMessages returns one pass of the lane's inbox, oldest first.
func (s *PGStore) PendingMessages(ctx context.Context, cfg Config) ([]PendingMessage, error) {
	where, attr, sel := inboxWhere, attrProject, inboxSelect
	switch cfg.Lane.Name {
	case LaneResidue.Name:
		where, attr = inboxWhereResidue, attrUnmatched
	case LaneInquiry.Name:
		where = inboxWhereInquiry
	case LaneRoute.Name:
		// SWT-40 Part B: the unmatched pile on candidate accounts, with the
		// receiving account selected through B-D9's carve-out.
		where, attr, sel = inboxWhereRoute, attrUnmatched, inboxSelect+routeSelectAccount
	}
	q := sel + where
	args := []any{}
	if cfg.Since > 0 {
		args = append(args, cfg.Since.String())
		q += fmt.Sprintf(" AND nm.sent_at >= now() - $%d::interval", len(args))
	}
	q += " ORDER BY nm.sent_at, nm.id"
	if cfg.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", cfg.Limit)
	}
	return s.scanMessages(ctx, cfg.Lane, attr, q, args...)
}

// MessagesByID loads labelled messages for the eval harness. It deliberately
// does NOT apply the inbox filter: a labelled message that has already been
// classified must still be scoreable, or the eval set decays every time a pass
// runs.
//
// The RESIDUE loader (SWT-23 criterion 17) goes further: NO action and NO
// project predicate at all. Phase 1 exists to move messages out of the
// residue, so a loader that required `unmatched` would make the labelled set
// silently shrink every time a rule is added — label drift by another
// mechanism. Loading without the predicate is safe because Eval refuses any
// lane but the local one before it reads anything. Rows whose latest decision
// is no longer 'unmatched' come back with Attribution = AttrProject so the
// harness can SAY a rule has claimed them.
func (s *PGStore) MessagesByID(ctx context.Context, cfg Config, ids []int64) ([]PendingMessage, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	// The ROUTE loader (SWT-40 B10) loads labelled messages by id with their
	// receiving account and its candidates, and NO decision predicate: the
	// route eval scores messages the RULES attributed (the rules' answer is the
	// label), so an 'unmatched' requirement would load nothing. Safe for the
	// reason the residue branch is: Eval refuses any lane but the local one
	// before it reads anything, and it never shows the model the project.
	if cfg.Lane.Name == LaneRoute.Name {
		q := inboxSelect + routeSelectAccount + `
	  FROM normalized_messages nm` + routeAccountJoin + `
	  LEFT JOIN LATERAL (SELECT cd.action, cd.project_id
	                  FROM capture_decisions cd
	                 WHERE cd.message_id = nm.id
	                 ORDER BY cd.id DESC LIMIT 1) latest ON true
	  LEFT JOIN projects p ON p.id = latest.project_id
	 WHERE nm.id = ANY($1)
	 ORDER BY nm.id`
		return s.scanMessages(ctx, cfg.Lane, attrUnmatched, q, ids)
	}
	// The INQUIRY loader (SWT-33) takes the residue's shape: no ai_inquiry and no
	// action predicate, for the same reason — a labelled message whose project
	// was disarmed or re-routed since must stay scoreable, or the labelled set
	// decays by another mechanism. It loads the same PRIOR thread context the
	// run path does (scanMessages), so an eval scores the prompt that runs.
	// Safe for the reason the residue branch is: Eval refuses any lane but the
	// local one before it reads anything.
	if cfg.Lane.Name == LaneInquiry.Name {
		q := inboxSelect + `
	  FROM normalized_messages nm
	  LEFT JOIN LATERAL (SELECT cd.action, cd.project_id
	                  FROM capture_decisions cd
	                 WHERE cd.message_id = nm.id
	                 ORDER BY cd.id DESC LIMIT 1) latest ON true
	  LEFT JOIN projects p ON p.id = latest.project_id
	 WHERE nm.id = ANY($1)
	 ORDER BY nm.id`
		return s.scanMessages(ctx, cfg.Lane, attrProject, q, ids)
	}
	if cfg.Lane.Name == LaneResidue.Name {
		q := inboxSelect + `
	  FROM normalized_messages nm
	  LEFT JOIN LATERAL (SELECT cd.action, cd.project_id
	                  FROM capture_decisions cd
	                 WHERE cd.message_id = nm.id
	                 ORDER BY cd.id DESC LIMIT 1) latest ON true
	  LEFT JOIN projects p ON p.id = latest.project_id
	 WHERE nm.id = ANY($1)
	 ORDER BY nm.id`
		msgs, err := s.scanMessages(ctx, cfg.Lane, attrUnmatched, q, ids)
		if err != nil {
			return nil, err
		}
		claimed, err := s.claimedSince(ctx, ids)
		if err != nil {
			return nil, err
		}
		for i := range msgs {
			if claimed[msgs[i].MessageID] {
				msgs[i].Attribution = attrProject
			}
		}
		return msgs, nil
	}
	q := inboxSelect + `
	  FROM normalized_messages nm
	  JOIN LATERAL (SELECT cd.action, cd.project_id
	                  FROM capture_decisions cd
	                 WHERE cd.message_id = nm.id
	                 ORDER BY cd.id DESC LIMIT 1) latest ON true
	  JOIN projects p ON p.id = latest.project_id
	 WHERE nm.id = ANY($1)
	   AND p.ai_locality = 'local_only'
	 ORDER BY nm.id`
	return s.scanMessages(ctx, cfg.Lane, attrProject, q, ids)
}

// claimedSince names the labelled messages whose LATEST decision is no longer
// 'unmatched' — a rule has claimed them since the label was written.
func (s *PGStore) claimedSince(ctx context.Context, ids []int64) (map[int64]bool, error) {
	rows, err := s.pool.Query(ctx, `
	SELECT nm.id
	  FROM normalized_messages nm
	  JOIN LATERAL (SELECT cd.action FROM capture_decisions cd
	                 WHERE cd.message_id = nm.id ORDER BY cd.id DESC LIMIT 1) latest ON true
	 WHERE nm.id = ANY($1) AND latest.action <> 'unmatched'`, ids)
	if err != nil {
		return nil, fmt.Errorf("select claimed labelled messages: %w", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan claimed labelled message: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (s *PGStore) scanMessages(ctx context.Context, lane Lane, attr provider.AttributionState, q string, args ...any) ([]PendingMessage, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("select classify inbox: %w", err)
	}
	defer rows.Close()

	var out []PendingMessage
	for rows.Next() {
		var m PendingMessage
		var linksRaw []byte
		dest := []any{&m.MessageID, &m.RawSourceItemID, &m.ThreadID, &m.SentAt,
			&m.Sender, &m.Subject, &m.Channel, &m.BodyText, &m.Direction,
			&m.ProjectID, &m.ProjectSlug, &m.ProjectLocalOnly, &linksRaw,
			&m.ExternalMessageID, &m.ThreadKey}
		// The route loaders select one more column, the receiving account
		// (routeSelectAccount, through routeAccountJoin).
		if lane.Name == LaneRoute.Name {
			dest = append(dest, &m.SourceAccountID)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scan classify inbox row: %w", err)
		}
		// The links COLUMN is the contract with the normalizer (SWT-25): the
		// element shape is {"text","url"} and the array position is the
		// identity — the scan must not reorder it, and json.Unmarshal does not.
		if err := json.Unmarshal(linksRaw, &m.Links); err != nil {
			return nil, fmt.Errorf("parse links for message %d: %w", m.MessageID, err)
		}
		// Attribution is the LANE'S: AttrProject for the personal inbox (its
		// filter requires an 'attributed' decision) and AttrUnmatched for the
		// residue — which is what routes it through ClassOf's non-AttrProject
		// branch. Set explicitly rather than left at the zero value (AttrUnseen).
		m.Attribution = attr
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate classify inbox: %w", err)
	}

	// Neighbour classes, loaded per message in the SAME shape drafts uses.
	for i := range out {
		if out[i].ThreadID == 0 {
			continue
		}
		ns, err := s.neighbours(ctx, out[i].ThreadID, out[i].MessageID)
		if err != nil {
			return nil, err
		}
		out[i].Neighbours = ns
		// The inquiry lane's transcript (SWT-33 criterion 16). The other two
		// lanes classify one message alone and never pay for this query.
		if lane.Name == LaneInquiry.Name {
			thread, err := s.priorThread(ctx, out[i])
			if err != nil {
				return nil, err
			}
			out[i].ThreadContext = InquiryContext(out[i], thread)
		}
	}
	// The route lane's closed candidate sets (B-D1), one read for every account
	// in the pass.
	if lane.Name == LaneRoute.Name {
		if err := s.attachCandidates(ctx, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// attachCandidates fills each route row's Candidates: its receiving account's
// source_account_projects rows with the project's slug, name and client, in
// row order — the order the prompt numbers them and ResolveCandidate indexes.
func (s *PGStore) attachCandidates(ctx context.Context, msgs []PendingMessage) error {
	if len(msgs) == 0 {
		return nil
	}
	seen := map[int64]bool{}
	var accounts []int64
	for _, m := range msgs {
		if !seen[m.SourceAccountID] {
			seen[m.SourceAccountID] = true
			accounts = append(accounts, m.SourceAccountID)
		}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT sap.source_account_id, p.id, p.slug, p.name, COALESCE(p.client, ''), sap.description, sap.is_default
		  FROM source_account_projects sap
		  JOIN projects p ON p.id = sap.project_id
		 WHERE sap.source_account_id = ANY($1)
		 ORDER BY sap.source_account_id, sap.id`, accounts)
	if err != nil {
		return fmt.Errorf("load route candidates: %w", err)
	}
	defer rows.Close()
	byAccount := map[int64][]RouteCandidate{}
	for rows.Next() {
		var account int64
		var c RouteCandidate
		if err := rows.Scan(&account, &c.ProjectID, &c.Slug, &c.Name, &c.Client, &c.Description, &c.IsDefault); err != nil {
			return fmt.Errorf("scan route candidate: %w", err)
		}
		byAccount[account] = append(byAccount[account], c)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate route candidates: %w", err)
	}
	for i := range msgs {
		msgs[i].Candidates = byAccount[msgs[i].SourceAccountID]
	}
	return nil
}

// priorThread loads the inquiry prompt's transcript: the target's thread, BOTH
// directions, strictly before it by (sent_at, id), the newest
// inquiryContextMax rows. InquiryContext then applies the same rule in Go, so
// the SQL bound and the pure rule cannot disagree about what "prior" means.
//
// NOT neighbours(): that loader's direction='inbound' filter exists for
// capture-decision reasons (invariant 5), and a transcript with our own replies
// removed is a transcript in which nothing was ever answered — the one case
// this lane's context exists to get right.
func (s *PGStore) priorThread(ctx context.Context, m PendingMessage) ([]ThreadMessage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT nm.id, nm.sent_at, COALESCE(nm.direction, ''), COALESCE(nm.body_text, '')
		  FROM normalized_messages nm
		 WHERE nm.thread_id = $1
		   AND nm.sent_at IS NOT NULL
		   AND (nm.sent_at < $2::timestamptz OR (nm.sent_at = $2::timestamptz AND nm.id < $3))
		 ORDER BY nm.sent_at DESC, nm.id DESC
		 LIMIT $4`, m.ThreadID, m.SentAt, m.MessageID, inquiryContextMax)
	if err != nil {
		return nil, fmt.Errorf("load prior thread for message %d: %w", m.MessageID, err)
	}
	defer rows.Close()

	var out []ThreadMessage
	for rows.Next() {
		var t ThreadMessage
		if err := rows.Scan(&t.MessageID, &t.SentAt, &t.Direction, &t.BodyText); err != nil {
			return nil, fmt.Errorf("scan prior thread message: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// neighbours loads the INBOUND thread siblings' attribution.
//
// Inbound only, and the filter is load-bearing rather than tidy: the capture
// engine reads direction='inbound' (invariant 5), so an outbound message can
// never carry a decision on any pass in any mode. Folding one would read "no
// decision" as "unclassified" when it means "not applicable", and would restrict
// every thread the system has ever replied on, permanently. For the same reason
// it must not be reused as the inquiry transcript — see priorThread.
func (s *PGStore) neighbours(ctx context.Context, threadID, selfID int64) ([]NeighbourClass, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT COALESCE(p.ai_locality = 'local_only', false),
		        EXISTS (SELECT 1 FROM capture_decisions cd2 WHERE cd2.message_id = nm.id),
		        latest.project_id IS NOT NULL
		   FROM normalized_messages nm
		   LEFT JOIN LATERAL (SELECT cd.project_id FROM capture_decisions cd
		                       WHERE cd.message_id = nm.id ORDER BY cd.id DESC LIMIT 1) latest ON true
		   LEFT JOIN projects p ON p.id = latest.project_id
		  WHERE nm.thread_id = $1 AND nm.id <> $2
		    AND nm.direction = 'inbound'`, threadID, selfID)
	if err != nil {
		return nil, fmt.Errorf("resolve neighbour attribution for message %d: %w", selfID, err)
	}
	defer rows.Close()

	var out []NeighbourClass
	for rows.Next() {
		var localOnly, seen, hasProj bool
		if err := rows.Scan(&localOnly, &seen, &hasProj); err != nil {
			return nil, fmt.Errorf("scan neighbour attribution: %w", err)
		}
		st := attrUnseen
		switch {
		case hasProj:
			st = attrProject
		case seen:
			st = attrUnmatched
		}
		out = append(out, NeighbourClass{State: st, LocalOnly: localOnly})
	}
	return out, rows.Err()
}

func (s *PGStore) RecordRun(ctx context.Context, run AIRun) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO ai_runs (worker_type, provider, model, input, output, status,
		                      prompt_tokens, completion_tokens, latency_ms)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
		run.WorkerType, run.Provider, run.Model, safeJSON(run.Input), safeJSON(run.Output),
		run.Status, run.PromptTokens, run.CompletionTokens, run.LatencyMS).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert ai_run: %w", err)
	}
	return id, nil
}

func (s *PGStore) RecordExtraction(ctx context.Context, aiRunID, rawSourceItemID int64, fields json.RawMessage) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields)
		 VALUES ($1,$2,$3)`, aiRunID, rawSourceItemID, safeJSON(fields)); err != nil {
		return fmt.Errorf("insert ai_extraction: %w", err)
	}
	return nil
}

// TryLock serialises passes on AdvisoryLockKey.
//
// It UNLOCKS explicitly before returning the connection to the pool, following
// internal/capture/rules_store.go rather than triage's shape. A session-level
// advisory lock is held by the SESSION, and returning a pooled connection does
// not end the session — so releasing the connection alone leaks the lock for the
// life of the process, and the next pass in that process finds itself locked out
// by a run that already finished. Harmless in a one-shot CLI where pool.Close()
// ends the session; a silent deadlock the moment anything runs two passes.
//
// The unlock uses context.Background() deliberately: it must happen even when
// the run was cancelled, which is exactly when the lock most needs releasing.
func (s *PGStore) TryLock(ctx context.Context) (bool, func(), error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return false, nil, fmt.Errorf("acquire lock conn: %w", err)
	}
	var ok bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, AdvisoryLockKey).Scan(&ok); err != nil {
		conn.Release()
		return false, nil, fmt.Errorf("pg_try_advisory_lock: %w", err)
	}
	if !ok {
		conn.Release()
		return false, nil, nil
	}
	return true, func() {
		if _, err := conn.Exec(context.Background(),
			`SELECT pg_advisory_unlock($1)`, AdvisoryLockKey); err != nil {
			slog.Warn("releasing classify advisory lock", "key", fmt.Sprintf("0x%X", AdvisoryLockKey), "err", err)
		}
		conn.Release()
	}, nil
}
