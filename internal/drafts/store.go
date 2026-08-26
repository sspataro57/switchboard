package drafts

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/upworkcrm"
)

// PGStore resolves the Deliver-task queue deterministically: channel from
// project config, thread from the client's mail/chat history — never from
// the model.
type PGStore struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool}
}

// DeliverTasks lists open R3 Deliver tasks whose parent has no delivery row
// yet, with channel + thread resolved.
func (s *PGStore) DeliverTasks(ctx context.Context, cfg Config) ([]DeliverTask, error) {
	q := `SELECT t.id, t.parent_id, p.slug, p.id,
	             COALESCE(parent.title,''), COALESCE(parent.body,''),
	             COALESCE(pe.display_name, p.client, ''),
	             COALESCE(p.policies->>'delivery_channel',''),
	             p.send_from_account, p.client_person_id,
	             COALESCE((SELECT payload->>'summary' FROM task_events
	                WHERE task_id = t.parent_id AND event_type='done_local'
	                ORDER BY id DESC LIMIT 1),'')
	      FROM tasks t
	      JOIN tasks parent ON parent.id = t.parent_id
	      JOIN projects p ON p.id = t.project_id
	      LEFT JOIN people pe ON pe.id = p.client_person_id
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
	for rows.Next() {
		var dt DeliverTask
		var projectID int64
		var channelCfg string
		var sendFrom, clientPerson *int64
		if err := rows.Scan(&dt.DeliverTaskID, &dt.ParentTaskID, &dt.ProjectSlug, &projectID,
			&dt.ParentTitle, new(string), &dt.ClientName, &channelCfg, &sendFrom, &clientPerson,
			&dt.ParentSummary); err != nil {
			return nil, fmt.Errorf("scan deliver task: %w", err)
		}
		if err := s.resolve(ctx, &dt, channelCfg, sendFrom, clientPerson); err != nil {
			return nil, err
		}
		out = append(out, dt)
	}
	return out, rows.Err()
}

// resolve applies the deterministic channel/thread rules from the SPEC.
func (s *PGStore) resolve(ctx context.Context, dt *DeliverTask, channelCfg string, sendFrom, clientPerson *int64) error {
	channel := channelCfg
	if channel == "" {
		switch {
		case sendFrom != nil:
			channel = "gmail"
		case clientPerson != nil && s.hasUpworkIdentity(ctx, *clientPerson):
			channel = "upwork_chat"
		}
	}
	dt.Channel = channel

	switch channel {
	case "gmail":
		if clientPerson == nil {
			return nil // unresolvable thread
		}
		var threadID int64
		err := s.pool.QueryRow(ctx,
			`SELECT m.thread_id FROM normalized_messages m
			 JOIN person_identities pi ON pi.provider='email' AND lower(pi.value)=lower(split_part(replace(replace(m.sender,'>',''),'<',''),' ',array_length(string_to_array(m.sender,' '),1)))
			 WHERE pi.person_id=$1 AND m.channel='gmail' AND m.thread_id IS NOT NULL
			 ORDER BY m.sent_at DESC LIMIT 1`, *clientPerson).Scan(&threadID)
		if errors.Is(err, pgx.ErrNoRows) {
			// simpler fallback: latest gmail thread mentioning any email identity
			err = s.pool.QueryRow(ctx,
				`SELECT m.thread_id FROM normalized_messages m
				 WHERE m.channel='gmail' AND m.direction='inbound' AND m.thread_id IS NOT NULL
				   AND EXISTS (SELECT 1 FROM person_identities pi
				               WHERE pi.person_id=$1 AND pi.provider='email'
				                 AND m.sender ILIKE '%'||pi.value||'%')
				 ORDER BY m.sent_at DESC LIMIT 1`, *clientPerson).Scan(&threadID)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("resolve gmail thread: %w", err)
		}
		dt.ThreadID = &threadID
	case "upwork_chat":
		if clientPerson == nil {
			return nil
		}
		var clientUUID string
		err := s.pool.QueryRow(ctx,
			`SELECT value FROM person_identities WHERE person_id=$1 AND provider='upwork_crm' LIMIT 1`,
			*clientPerson).Scan(&clientUUID)
		if errors.Is(err, pgx.ErrNoRows) {
			dt.Channel = ""
			return nil
		}
		if err != nil {
			return fmt.Errorf("resolve upwork client: %w", err)
		}
		// Prefer a ROOMED thread, deliberately (SWT-19). `ORDER BY id DESC`
		// alone happened to do this after the re-key, because roomed threads are
		// created later and so carry higher ids — but accidental correctness is
		// not correctness, and it flips the first time a legacy thread is touched.
		//
		// The preference is computed in GO, not in SQL. An earlier cut of this
		// ordered by a LIKE that concatenated the provider literal with the room
		// tag inline. It worked, and it was wrong: it put a SECOND SPELLING of
		// the key format in a query string — the exact failure this ticket exists
		// to end, and the structural test caught it. The client filter below is a
		// bind parameter built by ClientThreadPrefix, so no SQL here knows the
		// format.
		// Ordered by the thread's MOST RECENT MESSAGE, not by thread id. Id order
		// is creation order, which during a --full --all re-key is raw external_id
		// order — deterministic but meaningless, and it decides which room we
		// reply into for the two production clients that have several. Since
		// SWT-19 a delivery aimed at the wrong room can NEVER confirm (a room
		// mismatch excludes) and only surfaces via the reconciler ~45 minutes
		// later, so "arbitrary but stable" is not good enough here.
		//
		// Threads with no messages sort last (NULLs last) rather than winning on
		// a NULL comparison — an empty legacy thread left behind by the re-key is
		// exactly the shape that would otherwise be picked.
		rows, err := s.pool.Query(ctx,
			`SELECT t.thread_key
			   FROM normalized_threads t
			   LEFT JOIN normalized_messages m ON m.thread_id = t.id
			  WHERE t.thread_key LIKE $1 || '%'
			  GROUP BY t.id, t.thread_key
			  ORDER BY max(m.sent_at) DESC NULLS LAST, t.id DESC`,
			upworkcrm.ClientThreadPrefix(clientUUID))
		if err != nil {
			return fmt.Errorf("resolve upwork thread: %w", err)
		}
		var roomed, legacy string
		roomedCount := 0
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				rows.Close()
				return fmt.Errorf("scan upwork thread candidate: %w", err)
			}
			ref, err := upworkcrm.ParseThreadKey(key)
			if err != nil || ref.ClientID != clientUUID {
				// Not ours, or unreadable. The prefix LIKE can only over-match,
				// never under-match, so re-checking the client id in Go is what
				// makes it safe. Over-match is NOT "one client id is a prefix of
				// another" — ClientThreadPrefix ends with ':', which rules that
				// out. The real sources are LIKE metacharacters (the '_' in
				// 'upwork_crm' matches any character, and a '%' or '_' inside a
				// client id would too) and a colon-bearing client id.
				continue
			}
			if ref.Roomed {
				roomedCount++
				if roomed == "" {
					roomed = key
				}
				continue
			}
			if legacy == "" {
				legacy = key
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate upwork thread candidates: %w", err)
		}
		switch {
		case roomedCount > 1:
			// AMBIGUOUS: this client has several Upwork rooms and the Deliver
			// task carries no record of which one it came from. Picking the most
			// recent is a GUESS, and since SWT-19 a guess is expensive in both
			// directions — the reply may land in the wrong conversation, and the
			// delivery can then never confirm, because a room mismatch excludes.
			// The miss surfaces only via the reconciler, ~45 minutes later.
			//
			// So refuse to target it. An empty Channel routes to drafts.go's
			// existing "unresolvable — tell the human on the Deliver task" path,
			// which is reversible and audited; a wrong-room send is neither.
			// Two production clients are in this state (3 rooms and 2 rooms).
			//
			// The real fix is task-level provenance — recording which thread the
			// task came from — which needs a schema change and is its own ticket.
			dt.Channel = ""
			return nil
		case roomed != "":
			dt.TargetRef = roomed
		case legacy != "":
			dt.TargetRef = legacy
		default:
			// No thread at all. SWT-17 removes this branch ("let it die"): it aims
			// at first-contact drafting, which the policy matrix forbids for
			// upwork ("existing threads only"), and 26 of 26 upwork identities
			// have a thread. Built through ThreadKey so it cannot become a third
			// spelling while it survives.
			dt.TargetRef = upworkcrm.ThreadKey(clientUUID, "", "upwork")
		}
	}
	return s.loadThreadContext(ctx, dt)
}

func (s *PGStore) hasUpworkIdentity(ctx context.Context, personID int64) bool {
	var exists bool
	_ = s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM person_identities WHERE person_id=$1 AND provider='upwork_crm')`,
		personID).Scan(&exists)
	return exists
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
