package drafts

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/upworkcrm"
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
	q := `SELECT t.id, t.parent_id, p.slug,
	             COALESCE(parent.title,''),
	             COALESCE(NULLIF(p.client,''), p.name),
	             COALESCE(p.policies->>'delivery_channel',''),
	             (p.send_from_account IS NOT NULL),
	             COALESCE((SELECT payload->>'summary' FROM task_events
	                WHERE task_id = t.parent_id AND event_type='done_local'
	                ORDER BY id DESC LIMIT 1),'')
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
			&dt.ParentSummary); err != nil {
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
	threadID, threadKey, found, err := s.taskThread(ctx, dt.ParentTaskID, dt.DeliverTaskID)
	if err != nil {
		return err
	}

	// The channel the resolved conversation actually IS. Empty when no thread was
	// found, or when it is a jira:/slack: thread — extending the draft worker to
	// those targets is step-9 work and deliberately not bundled here.
	threadChannel := ""
	if found {
		switch {
		case strings.HasPrefix(threadKey, gmailThreadKeyPrefix):
			threadChannel = "gmail"
		default:
			// ParseThreadKey is the only reader of the upwork format; a prefix
			// literal here would be a second spelling of it.
			if _, perr := upworkcrm.ParseThreadKey(threadKey); perr == nil {
				threadChannel = "upwork_chat"
			}
		}
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
		target, err := s.upworkTarget(ctx, threadKey)
		if err != nil {
			return err
		}
		if target == "" {
			dt.Channel = ""
			return nil
		}
		dt.TargetRef = target
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

// upworkTarget turns the resolved upwork thread into the delivery target,
// preserving SWT-19's two load-bearing behaviours. Returns "" to REFUSE.
//
// SWT-19 had no task provenance at all: it found the client's threads by uuid
// (via the person) and had to choose among them. Site B gives provenance, which
// is the fix SWT-19's own comment named as "its own ticket" — but only where the
// ref names a ROOM. Both cases are handled here rather than collapsed:
//
//   - A ROOMED ref names the room outright. Target it; nothing to choose.
//   - A LEGACY ref (`upwork_crm:{client}:{channel}`) identifies the CLIENT and
//     says nothing about which room. That is exactly SWT-19's situation, so
//     SWT-19's rules still decide it, unchanged.
//
// Note where the client uuid comes from now: `ParseThreadKey` on the thread we
// already resolved. That is honest — it is read out of the conversation the task
// came from, not looked up through a person — so dropping `client_person_id`
// costs this branch nothing.
func (s *PGStore) upworkTarget(ctx context.Context, refKey string) (string, error) {
	ref, err := upworkcrm.ParseThreadKey(refKey)
	if err != nil {
		return "", fmt.Errorf("parse resolved upwork thread key %q: %w", refKey, err)
	}
	if ref.Roomed {
		return refKey, nil
	}

	// Prefer a ROOMED thread, deliberately (SWT-19). `ORDER BY id DESC` alone
	// happened to do this after the re-key, because roomed threads are created
	// later and so carry higher ids — but accidental correctness is not
	// correctness, and it flips the first time a legacy thread is touched.
	//
	// The preference is computed in GO, not in SQL. An earlier cut of this
	// ordered by a LIKE that concatenated the provider literal with the room tag
	// inline. It worked, and it was wrong: it put a SECOND SPELLING of the key
	// format in a query string, and the structural test caught it. The client
	// filter below is a bind parameter built by ClientThreadPrefix, so no SQL
	// here knows the format.
	//
	// Ordered by the thread's MOST RECENT MESSAGE, not by thread id. Id order is
	// creation order, which during a --full --all re-key is raw external_id order
	// — deterministic but meaningless, and it decides which room we reply into
	// for the two production clients that have several. Since SWT-19 a delivery
	// aimed at the wrong room can NEVER confirm (a room mismatch excludes) and
	// only surfaces via the reconciler ~45 minutes later, so "arbitrary but
	// stable" is not good enough here.
	//
	// Threads with no messages sort last (NULLs last) rather than winning on a
	// NULL comparison — an empty legacy thread left behind by the re-key is
	// exactly the shape that would otherwise be picked.
	rows, err := s.pool.Query(ctx,
		`SELECT t.thread_key
		   FROM normalized_threads t
		   LEFT JOIN normalized_messages m ON m.thread_id = t.id
		  WHERE t.thread_key LIKE $1 || '%'
		  GROUP BY t.id, t.thread_key
		  ORDER BY max(m.sent_at) DESC NULLS LAST, t.id DESC`,
		upworkcrm.ClientThreadPrefix(ref.ClientID))
	if err != nil {
		return "", fmt.Errorf("resolve upwork thread for client %q: %w", ref.ClientID, err)
	}
	var roomed string
	roomedCount := 0
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return "", fmt.Errorf("scan upwork thread candidate: %w", err)
		}
		cand, err := upworkcrm.ParseThreadKey(key)
		if err != nil || cand.ClientID != ref.ClientID {
			// Not ours, or unreadable. The prefix LIKE can only over-match, never
			// under-match, so re-checking the client id in Go is what makes it
			// safe. Over-match is NOT "one client id is a prefix of another" —
			// ClientThreadPrefix ends with ':', which rules that out. The real
			// sources are LIKE metacharacters (the '_' in the provider literal
			// matches any character, and a '%' or '_' inside a client id would
			// too) and a colon-bearing client id.
			continue
		}
		if !cand.Roomed {
			continue
		}
		roomedCount++
		if roomed == "" {
			roomed = key
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate upwork thread candidates for client %q: %w", ref.ClientID, err)
	}

	switch {
	case roomedCount > 1:
		// AMBIGUOUS: this client has several Upwork rooms and the ref this task
		// carries names only the client. Picking the most recent is a GUESS, and
		// since SWT-19 a guess is expensive in both directions — the reply may
		// land in the wrong conversation, and the delivery can then never confirm,
		// because a room mismatch excludes. The miss surfaces only via the
		// reconciler, ~45 minutes later.
		//
		// So refuse to target it. The caller turns "" into an empty Channel, which
		// routes to drafts.go's existing "unresolvable — tell the human on the
		// Deliver task" path: reversible and audited, where a wrong-room send is
		// neither. Two production clients are in this state (3 rooms and 2 rooms).
		//
		// This is SWT-20's shipped mitigation and it stays until a ROOM-level ref
		// exists for the task — which is the branch above, not a re-guess here.
		return "", nil
	case roomed != "":
		return roomed, nil
	default:
		// No roomed thread for this client: the legacy thread the task came from
		// is the target. §4's mismatch-only-excludes rule keeps an unroomed target
		// confirmable, and the pre-2026-07-21 corpus is most of the history.
		//
		// The ref itself, not the first legacy candidate the query returned: it is
		// the conversation this task is recorded against.
		return refKey, nil
	}
}

func (s *PGStore) loadThreadContext(ctx context.Context, dt *DeliverTask) error {
	var rows pgx.Rows
	var err error
	switch {
	case dt.ThreadID != nil:
		rows, err = s.pool.Query(ctx,
			`SELECT direction, COALESCE(sender,''), COALESCE(subject,''), COALESCE(body_text,''), sent_at
			 FROM (SELECT * FROM normalized_messages WHERE thread_id=$1 ORDER BY sent_at DESC LIMIT 6) sub
			 ORDER BY sent_at`, *dt.ThreadID)
	case dt.TargetRef != "":
		rows, err = s.pool.Query(ctx,
			`SELECT m.direction, COALESCE(m.sender,''), COALESCE(m.subject,''), COALESCE(m.body_text,''), m.sent_at
			 FROM normalized_messages m JOIN normalized_threads t ON t.id=m.thread_id
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
		if err := rows.Scan(&m.Direction, &m.Sender, &m.Subject, &m.BodyText, &m.SentAt); err != nil {
			return fmt.Errorf("scan thread message: %w", err)
		}
		dt.Thread = append(dt.Thread, m)
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
