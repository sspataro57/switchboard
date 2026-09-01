package drafts

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/upworkcrm"
	"github.com/sspataro57/switchboard/internal/provider"
	"github.com/sspataro57/switchboard/internal/store"
)

// PGStore resolves the Deliver-task queue deterministically: channel from
// project config or from the resolved conversation, thread from the TASK's own
// external_refs — never from the model, and (since SWT-17) never from a person.
//
// The person route is gone with `projects.client_person_id` (migration 0015).
// It never resolved anything in production anyway — the column is NULL on all
// four project rows — but the shape mattered: "who is this client" is a fuzzy
// identity question, while "which conversation did this task come from" is a
// recorded fact. The engine writes that fact as an `external_refs` row, so
// targeting reads it back instead of re-deriving it.
//
// gmailThreadKeyPrefix is the ONE thing here that restates a format another
// package owns (`google/normalize.go:106` builds `gmail:{account}:{id}`). Unlike
// the upwork key it has no constructor to call — this is a deliberate, single,
// named restatement rather than an inline literal, and it is only ever used as a
// prefix test in Go, never in SQL.
const gmailThreadKeyPrefix = "gmail:"

type PGStore struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool}
}

// DeliverTasks lists open R3 Deliver tasks whose parent has no delivery row
// yet, with channel + thread resolved.
func (s *PGStore) DeliverTasks(ctx context.Context, cfg Config) ([]DeliverTask, error) {
	// Site A: the client display name no longer comes through `people`.
	// NULLIF because `projects.client` is '' (not NULL) on the real rows and
	// `projects.name` is NOT NULL, so ClientName is now never empty — where an
	// unmapped project used to render as orDash "—" in the prompt.
	// p.ai_locality is read HERE, in the same row as the project it belongs to
	// (SWT-21 criterion 24). It is the whole reason the boundary in drafts.go is
	// not decorative: without this column DeliverTask.ProjectLocalOnly is false
	// for every task in production, `ClassOf(AttrProject, false)` returns
	// ClassGeneral, and a Deliver task on a local_only project is drafted by the
	// hosted model — with a locality test passing the whole time, because the
	// test sets the field on its own fixture. That is the repo's recurring
	// landmine (a predicate whose discriminating column is constant in
	// production), and it shipped inert in the first cut of this ticket.
	q := `SELECT t.id, t.parent_id, p.slug,
	             COALESCE(parent.title,''),
	             COALESCE(NULLIF(p.client,''), p.name),
	             COALESCE(p.policies->>'delivery_channel',''),
	             (p.send_from_account IS NOT NULL),
	             COALESCE((SELECT payload->>'summary' FROM task_events
	                WHERE task_id = t.parent_id AND event_type='done_local'
	                ORDER BY id DESC LIMIT 1),''),
	             (p.ai_locality = 'local_only')
	      FROM tasks t
	      JOIN tasks parent ON parent.id = t.parent_id
	      JOIN projects p ON p.id = t.project_id
	      WHERE t.title LIKE 'Deliver #%' AND t.status IN ('ready','holding')
	        AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.task_id = t.parent_id)
	      ORDER BY t.id`
	if cfg.Limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, cfg.Limit)
	}
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("select deliver tasks: %w", err)
	}
	defer rows.Close()

	var out []DeliverTask
	var pending []DeliverTask
	var channelCfgs []string
	var hasSendFroms []bool
	for rows.Next() {
		var dt DeliverTask
		var channelCfg string
		var hasSendFrom bool
		if err := rows.Scan(&dt.DeliverTaskID, &dt.ParentTaskID, &dt.ProjectSlug,
			&dt.ParentTitle, &dt.ClientName, &channelCfg, &hasSendFrom,
			&dt.ParentSummary, &dt.ProjectLocalOnly); err != nil {
			return nil, fmt.Errorf("scan deliver task: %w", err)
		}
		pending = append(pending, dt)
		channelCfgs = append(channelCfgs, channelCfg)
		hasSendFroms = append(hasSendFroms, hasSendFrom)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate deliver tasks: %w", err)
	}
	// Resolution runs AFTER the queue rows are drained: it issues further
	// queries on the same pool, and doing that while `rows` is open borrows a
	// second connection per Deliver task for the lifetime of the scan.
	for i := range pending {
		dt := pending[i]
		if err := s.resolve(ctx, &dt, channelCfgs[i], hasSendFroms[i]); err != nil {
			return nil, err
		}
		out = append(out, dt)
	}
	return out, nil
}

// resolve applies the deterministic channel/thread rules (SPEC §9 sites B-E).
//
// Site D's precedence:
//
//  1. `policies->>'delivery_channel'` when non-empty — explicit config wins.
//  2. otherwise the resolved thread's key PREFIX decides the channel.
//  3. otherwise `send_from_account IS NOT NULL` → gmail (with no thread; the
//     worker already skips that state).
//  4. otherwise "" — unresolvable.
//
// The prefix, not `normalized_messages.channel`: for upwork that column is
// CRM-supplied free text, while the thread key is constructed by the connector
// to a documented format.
func (s *PGStore) resolve(ctx context.Context, dt *DeliverTask, channelCfg string, hasSendFrom bool) error {
	// SWT-20: the upwork target is the TASK'S RECORDED SOURCE THREAD, read
	// through the one shared resolver, and nothing else. The walk inside
	// TaskSourceThread covers the Deliver child and its parent (premise 8), so
	// calling it on the Deliver task reaches the work task's observation.
	//
	// An external_refs row WITHOUT provenance no longer produces an upwork
	// channel: a ref is an agent-writable claim, not a recorded observation
	// (SPEC D1), and criterion 8 routes that state to the "unresolvable — tell
	// the human" log. The external_refs join below STAYS for gmail and jira —
	// it is their only route and nothing about them changes in this ticket.
	upworkKey := ""
	prov, provFound, err := store.TaskSourceThread(ctx, s.pool, dt.DeliverTaskID)
	if err != nil {
		return err
	}
	if provFound {
		// ParseThreadKey is the only reader of the upwork format; a prefix
		// literal here would be a second spelling of it. A provenance that is
		// not an upwork thread (a gmail-raised task) simply does not produce an
		// upwork channel.
		if _, perr := upworkcrm.ParseThreadKey(prov.ThreadKey); perr == nil {
			upworkKey = prov.ThreadKey
		}
	}

	threadID, threadKey, found, err := s.taskThread(ctx, dt.ParentTaskID, dt.DeliverTaskID)
	if err != nil {
		return err
	}

	// The channel the resolved conversation actually IS. Empty when nothing
	// resolved, or when it is a jira:/slack: thread — extending the draft
	// worker to those targets is step-9 work and deliberately not bundled here.
	threadChannel := ""
	switch {
	case upworkKey != "":
		threadChannel = "upwork_chat"
	case found && strings.HasPrefix(threadKey, gmailThreadKeyPrefix):
		threadChannel = "gmail"
	}

	channel := channelCfg
	switch {
	case channel != "":
		// explicit config
	case threadChannel != "":
		channel = threadChannel
	case hasSendFrom:
		channel = "gmail"
	}
	dt.Channel = channel

	// A thread only becomes the TARGET when it belongs to the channel being
	// delivered on. Explicit config that disagrees with the conversation (say
	// `delivery_channel: gmail` on a project whose task came from a jira thread)
	// yields the unresolvable state rather than a message aimed at the wrong
	// object — a gmail draft with someone else's thread_id would thread into a
	// conversation the client never had.
	if threadChannel != channel {
		if channel != "gmail" {
			// Site E: no upwork target and no way to invent one. The client uuid
			// used to be reachable through `client_person_id`, and the no-thread
			// fallback synthesized `upwork_crm:{uuid}:upwork` for a client with no
			// ingested conversation. That is REMOVED: the policy matrix restricts
			// upwork to "existing threads only, ≤2 touches", so drafting into a
			// conversation that does not exist was already outside policy.
			//
			// Zeroing the channel matters, and it is why this test is "not gmail"
			// rather than "upwork_chat": drafts.go's skip path is
			// `Channel == "" || (gmail && ThreadID == nil)`, so ANY other channel
			// with an empty TargetRef would NOT be skipped — it would reach
			// draft_delivery with `target_ref: ""`. gmail is the one channel whose
			// no-thread state the worker already handles, and criterion 25 pins it.
			dt.Channel = ""
		}
		return nil
	}

	switch channel {
	case "gmail":
		id := threadID
		dt.ThreadID = &id
	case "upwork_chat":
		// The recorded conversation, verbatim. Roomed provenance names the
		// exact room; LEGACY provenance names the legacy key — the truthful
		// statement "this client's conversation, room not recorded by the
		// source" (D6), which SameConversation's legacy tolerance keeps
		// confirmable. "Never to the most recent room" is satisfied
		// structurally: no code path in this package looks at any thread other
		// than the recorded one any more.
		dt.TargetRef = upworkKey
	}
	return s.loadThreadContext(ctx, dt)
}

// taskThread is Site B: the task→thread resolution that replaces both
// person-based queries.
//
// Tried on the PARENT task first — R3 creates the Deliver task as a child of the
// work task, and the capture engine links the ref to the work task — then on the
// Deliver task itself.
//
// The `OR` arm covers jira refs, whose `external_key` is a ticket key rather than
// a thread_key; every other system this engine writes stores the thread_key
// verbatim.
func (s *PGStore) taskThread(ctx context.Context, parentTaskID, deliverTaskID int64) (int64, string, bool, error) {
	for _, taskID := range []int64{parentTaskID, deliverTaskID} {
		var id int64
		var key string
		err := s.pool.QueryRow(ctx,
			`SELECT nt.id, COALESCE(nt.thread_key,'')
			   FROM external_refs er
			   JOIN normalized_threads nt
			     ON nt.thread_key = er.external_key
			     OR (er.system = 'jira' AND nt.thread_key LIKE 'jira:%:' || er.external_key)
			  WHERE er.task_id = $1
			  ORDER BY er.created_at DESC, er.id DESC, nt.id DESC
			  LIMIT 1`, taskID).Scan(&id, &key)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, "", false, fmt.Errorf("resolve thread for task %d: %w", taskID, err)
		}
		if key == "" {
			continue
		}
		return id, key, true, nil
	}
	return 0, "", false, nil
}

func (s *PGStore) loadThreadContext(ctx context.Context, dt *DeliverTask) error {
	// Each row carries the message AND its attribution (SWT-21). Deliberately ONE
	// query rather than a second pass: this is the set of bodies that actually
	// travels in the prompt, so classifying it here makes "what was sent" and
	// "what was classified" the same set by construction. A separate neighbour
	// query could drift — a different LIMIT, a different ordering, or simply
	// being forgotten when this one changes — and the drift would be silent and
	// in the unsafe direction.
	var rows pgx.Rows
	var err error
	switch {
	case dt.ThreadID != nil:
		rows, err = s.pool.Query(ctx,
			`SELECT sub.direction, COALESCE(sub.sender,''), COALESCE(sub.subject,''),
			        COALESCE(sub.body_text,''), sub.sent_at,
			        COALESCE(p.ai_locality = 'local_only', false),
			        EXISTS (SELECT 1 FROM capture_decisions cd2 WHERE cd2.message_id = sub.id),
			        latest.project_id IS NOT NULL
			 FROM (SELECT * FROM normalized_messages WHERE thread_id=$1 ORDER BY sent_at DESC LIMIT 6) sub
			 LEFT JOIN LATERAL (SELECT cd.project_id FROM capture_decisions cd
			                     WHERE cd.message_id = sub.id ORDER BY cd.id DESC LIMIT 1) latest ON true
			 LEFT JOIN projects p ON p.id = latest.project_id
			 ORDER BY sub.sent_at`, *dt.ThreadID)
	case dt.TargetRef != "":
		rows, err = s.pool.Query(ctx,
			`SELECT m.direction, COALESCE(m.sender,''), COALESCE(m.subject,''),
			        COALESCE(m.body_text,''), m.sent_at,
			        COALESCE(p.ai_locality = 'local_only', false),
			        EXISTS (SELECT 1 FROM capture_decisions cd2 WHERE cd2.message_id = m.id),
			        latest.project_id IS NOT NULL
			 FROM normalized_messages m JOIN normalized_threads t ON t.id=m.thread_id
			 LEFT JOIN LATERAL (SELECT cd.project_id FROM capture_decisions cd
			                     WHERE cd.message_id = m.id ORDER BY cd.id DESC LIMIT 1) latest ON true
			 LEFT JOIN projects p ON p.id = latest.project_id
			 WHERE t.thread_key=$1 ORDER BY m.sent_at DESC LIMIT 6`, dt.TargetRef)
	default:
		return nil
	}
	if err != nil {
		return fmt.Errorf("load thread context: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m ThreadMessage
		var localOnly, seen, hasProj bool
		if err := rows.Scan(&m.Direction, &m.Sender, &m.Subject, &m.BodyText, &m.SentAt,
			&localOnly, &seen, &hasProj); err != nil {
			return fmt.Errorf("scan thread message: %w", err)
		}
		dt.Thread = append(dt.Thread, m)

		// OUTBOUND messages are NOT folded (SWT-21, post-review correction).
		//
		// This is not an optimisation, it is a correctness fix, and the bug it
		// repairs was permanent rather than transient. The capture engine filters
		// `direction = 'inbound'` — that line IS invariant 5, and it means an
		// outbound message will NEVER have a capture_decisions row, in any mode,
		// on any pass. Measured: 21,194 outbound messages, zero decisions, 100%
		// of them. So classifying one as AttrUnseen does not mean "not yet
		// looked at", it means "not applicable", and folding it restricted the
		// whole request FOREVER on any thread with two-way traffic — which is
		// every thread a Deliver task exists to reply on, the moment its own
		// send re-enters through ingestion.
		//
		// What makes skipping them SAFE is inheritance, not approval. An outbound
		// message is structurally unclassifiable, so instead of defaulting it to
		// `unseen` we let it take its CONVERSATION's class — which is folded
		// anyway, from the Deliver task's own project and from every inbound
		// sibling on the thread. Excluding it therefore removes a value that was
		// never evidence, not a value that said "general".
		//
		// An earlier version of this comment justified it differently and WRONGLY:
		// that an outbound message is our own send and reached these participants
		// only by passing the delivery policy gate. Measured against production,
		// that is false — 21,194 outbound messages against 1 delivery row.
		// `direction='outbound'` is set by normalize.go when the From address is
		// one of the five own accounts, so it is overwhelmingly mail Salvador
		// typed in Gmail himself. The gate had seen ~0.005% of it. And the gate
		// answers disclosure to the RECIPIENT, which is a different question from
		// disclosure to a hosted API.
		//
		// RESIDUAL, stated because inheritance does not cover it: the outbound
		// body is still rendered into the prompt (see below — it stays in the
		// conversation). A hand-written reply that pastes personal material into
		// a client thread introduces content that no inbound sibling holds and no
		// project attribution describes, and it will travel to the hosted lane.
		// Dropping outbound from the prompt would break the draft worker's job,
		// and folding it restricted breaks every replied-on thread forever, so
		// this is an accepted gap rather than an oversight. SPEC deviation 10.
		if m.Direction == "outbound" {
			continue
		}

		// SWT-17 §8's three states, spelled exactly as triage spells them
		// (internal/triage/store.go) so the two workers cannot drift into two
		// vocabularies for one rule.
		//
		// Unseen and unmatched fold IDENTICALLY here — both are restricted, and
		// drafts has no report that tells them apart, so no test can distinguish
		// them either. Keeping the distinction anyway is deliberate: it costs one
		// EXISTS on a six-row query, it matches triage, and the moment drafts
		// grows a skip report the number it needs is already being read. Do not
		// mistake the missing test for missing coverage; there is nothing to
		// observe.
		st := provider.AttrUnseen
		switch {
		case hasProj:
			st = provider.AttrProject
		case seen:
			st = provider.AttrUnmatched
		}
		dt.NeighbourAttribution = append(dt.NeighbourAttribution,
			NeighbourClass{State: st, LocalOnly: localOnly})
	}
	return rows.Err()
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
