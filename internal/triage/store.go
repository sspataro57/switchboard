package triage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AdvisoryLockKey guards single-instance triage runs.
const AdvisoryLockKey = 0x5157_0006

// PGStore is the triage worker's Postgres side: the pending filter, context
// assembly, find_related_tasks, and the ai_runs/ai_extractions bookkeeping.
// Deliberately NO task-write surface (shadow mode is structural).
type PGStore struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool}
}

// PendingMessages is the queue-as-filter: inbound messages with no triage
// extraction for their raw item, oldest first.
func (s *PGStore) PendingMessages(ctx context.Context, cfg Config) ([]PendingMessage, error) {
	q := `SELECT m.id, m.raw_source_item_id, COALESCE(m.thread_id, 0), m.sent_at,
	             COALESCE(m.sender,''), COALESCE(m.subject,''), COALESCE(m.channel,''),
	             COALESCE(m.body_text,''), m.direction
	      FROM normalized_messages m
	      WHERE m.direction = 'inbound'
	        AND NOT EXISTS (
	          SELECT 1 FROM ai_extractions e
	          JOIN ai_runs r ON r.id = e.ai_run_id AND r.worker_type = 'triage'
	          WHERE e.raw_source_item_id = m.raw_source_item_id)`
	args := []any{}
	if cfg.Since > 0 {
		args = append(args, cfg.Since.String())
		q += fmt.Sprintf(` AND m.sent_at >= now() - $%d::interval`, len(args))
	}
	q += ` ORDER BY m.sent_at, m.id`
	if cfg.Limit > 0 {
		args = append(args, cfg.Limit)
		q += fmt.Sprintf(` LIMIT $%d`, len(args))
	}

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("select pending messages: %w", err)
	}
	defer rows.Close()

	var out []PendingMessage
	for rows.Next() {
		var m PendingMessage
		if err := rows.Scan(&m.MessageID, &m.RawSourceItemID, &m.ThreadID, &m.SentAt,
			&m.Sender, &m.Subject, &m.Channel, &m.BodyText, &m.Direction); err != nil {
			return nil, fmt.Errorf("scan pending message: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AssembleContext gathers the deterministic per-message context.
func (s *PGStore) AssembleContext(ctx context.Context, m PendingMessage) (MessageContext, error) {
	mc := MessageContext{Message: m}

	// Thread context: up to 10 prior messages (both directions), oldest first.
	if m.ThreadID != 0 {
		rows, err := s.pool.Query(ctx,
			`SELECT direction, COALESCE(sender,''), COALESCE(subject,''), COALESCE(body_text,''), sent_at
			 FROM (SELECT * FROM normalized_messages
			       WHERE thread_id=$1 AND id <> $2 AND sent_at <= $3
			       ORDER BY sent_at DESC, id DESC LIMIT 10) sub
			 ORDER BY sent_at ASC, id ASC`, m.ThreadID, m.MessageID, m.SentAt)
		if err != nil {
			return mc, fmt.Errorf("load thread context: %w", err)
		}
		for rows.Next() {
			var t ThreadMessage
			if err := rows.Scan(&t.Direction, &t.Sender, &t.Subject, &t.BodyText, &t.SentAt); err != nil {
				rows.Close()
				return mc, fmt.Errorf("scan thread message: %w", err)
			}
			mc.Thread = append(mc.Thread, t)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return mc, fmt.Errorf("iterate thread: %w", err)
		}

		// Person: first participant of the thread.
		var participants []byte
		if err := s.pool.QueryRow(ctx,
			`SELECT participants FROM normalized_threads WHERE id=$1`, m.ThreadID).Scan(&participants); err == nil {
			var ids []int64
			if json.Unmarshal(participants, &ids) == nil && len(ids) > 0 {
				pid := ids[0]
				mc.PersonID = &pid
				var name *string
				if err := s.pool.QueryRow(ctx,
					`SELECT display_name FROM people WHERE id=$1`, pid).Scan(&name); err == nil && name != nil {
					mc.PersonName = *name
				}
			}
		}
	}

	// Project: people.id -> projects.client_person_id.
	if mc.PersonID != nil {
		var projectID int64
		var slug string
		err := s.pool.QueryRow(ctx,
			`SELECT id, slug FROM projects WHERE client_person_id=$1 ORDER BY id LIMIT 1`,
			*mc.PersonID).Scan(&projectID, &slug)
		if err == nil {
			mc.ProjectID = &projectID
			mc.ProjectSlug = slug
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return mc, fmt.Errorf("resolve project: %w", err)
		}
	}

	// Candidates: find_related_tasks — deterministic recency + project scope.
	if mc.ProjectID != nil {
		candidates, err := s.FindRelatedTasks(ctx, *mc.ProjectID)
		if err != nil {
			return mc, err
		}
		mc.Candidates = candidates
	}
	return mc, nil
}

// FindRelatedTasks supplies up to 10 open tasks for the project — the
// candidate set the model may attach to. Deterministic SQL, no LLM, no trgm.
func (s *PGStore) FindRelatedTasks(ctx context.Context, projectID int64) ([]Candidate, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, title, status, COALESCE(subproject,''), updated_at
		 FROM tasks
		 WHERE project_id = $1 AND status NOT IN ('closed')
		 ORDER BY updated_at DESC
		 LIMIT 10`, projectID)
	if err != nil {
		return nil, fmt.Errorf("find related tasks: %w", err)
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(&c.ID, &c.Title, &c.Status, &c.Subproject, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		out = append(out, c)
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
		`INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields) VALUES ($1,$2,$3)`,
		aiRunID, rawSourceItemID, safeJSON(fields)); err != nil {
		return fmt.Errorf("insert ai_extraction: %w", err)
	}
	return nil
}

// TryLock takes the single-instance advisory lock; the loser exits.
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
	return true, conn.Release, nil
}
