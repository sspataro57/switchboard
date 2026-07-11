package drafts

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
		var threadKey string
		err = s.pool.QueryRow(ctx,
			`SELECT thread_key FROM normalized_threads
			 WHERE thread_key LIKE 'upwork_crm:'||$1||':%'
			 ORDER BY id DESC LIMIT 1`, clientUUID).Scan(&threadKey)
		if errors.Is(err, pgx.ErrNoRows) {
			dt.TargetRef = "upwork_crm:" + clientUUID + ":upwork"
			return s.loadThreadContext(ctx, dt)
		}
		if err != nil {
			return fmt.Errorf("resolve upwork thread: %w", err)
		}
		dt.TargetRef = threadKey
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
