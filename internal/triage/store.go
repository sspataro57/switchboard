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
// extraction for their raw item AND whose latest capture decision says the
// deterministic rules did not cover them, oldest first.
//
// UNMATCHED IS TRIAGE'S INBOX (SPEC 17 §8b, Q3). Three states of
// capture_decisions must stay distinct:
//
//	no decision row at all              -> SKIP: the capture pass has not
//	                                       evaluated this message yet, so it is
//	                                       unseen, not unroutable. Triage runs
//	                                       can outrun the pass (it hitchhikes on
//	                                       the connector CronJobs); treating
//	                                       unseen as unmatched would hand every
//	                                       fresh message to the model before the
//	                                       deterministic rules ever looked at it.
//	latest action = 'unmatched'         -> CONSUME: this is the inbox.
//	latest action attributed/task/      -> NEVER: deterministically routed;
//	  task_log                             re-triaging it would double-process.
//
// So the guard is an EXISTS on an 'unmatched' LATEST decision, never a
// NOT EXISTS over decisions generally. "Latest" is id DESC, the same ordering
// AssembleContext's project lookup uses (SPEC §8a) — the two reads key on the
// same rows and must not drift apart.
func (s *PGStore) PendingMessages(ctx context.Context, cfg Config) ([]PendingMessage, error) {
	q := `SELECT m.id, m.raw_source_item_id, COALESCE(m.thread_id, 0), m.sent_at,
	             COALESCE(m.sender,''), COALESCE(m.subject,''), COALESCE(m.channel,''),
	             COALESCE(m.body_text,''), m.direction
	      FROM normalized_messages m
	      WHERE m.direction = 'inbound'
	        AND NOT EXISTS (
	          SELECT 1 FROM ai_extractions e
	          JOIN ai_runs r ON r.id = e.ai_run_id AND r.worker_type = 'triage'
	          WHERE e.raw_source_item_id = m.raw_source_item_id)
	        AND EXISTS (
	          SELECT 1 FROM (
	            SELECT cd.action
	              FROM capture_decisions cd
	             WHERE cd.message_id = m.id
	             ORDER BY cd.id DESC
	             LIMIT 1) latest
	          WHERE latest.action = 'unmatched')`
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

	// Project: the capture engine's decision for this message, and nothing else
	// (SPEC 17 §8a, Q3). projects.client_person_id is dropped in migration 0015,
	// so there is no person-based fallback: capture_rules is now the single
	// source of project assignment. Latest decision carrying a project wins,
	// ordered by id DESC as in the inbox filter above.
	//
	// nil ProjectID stays meaningful — it is the report's UNMAPPED lane, and it
	// is the normal state for the inbox, since an 'unmatched' decision has
	// project_id IS NULL by construction.
	var projectID int64
	var projectSlug string
	err := s.pool.QueryRow(ctx,
		// LATEST decision, then its project — NOT the latest decision that happens
		// to have one.
		//
		// The SPEC's §8a spells this `WHERE cd.project_id IS NOT NULL ORDER BY
		// cd.id DESC`, and that is a different query: it skips past a newer
		// decision to find an older project-bearing one. A message attributed by
		// an earlier pass and re-evaluated as unmatched after a rule was disabled
		// or narrowed would then be served to triage as inbox (the filter above
		// takes the latest row unconditionally) AND handed the project from the
		// decision that superseded it. Stale attribution, no error, and the two
		// reads drifting apart in exactly the way the filter's own comment
		// promises they will not.
		//
		// So both reads now mean the same thing by "latest": the newest row, full
		// stop. If that row carries no project, there is no project — which is
		// also what makes an unmatched message projectless, as §8b intends.
		// Deliberate deviation from §8a's literal SQL; recorded in the SPEC.
		`SELECT p.id, p.slug
		   FROM (SELECT cd.project_id FROM capture_decisions cd
		          WHERE cd.message_id = $1 ORDER BY cd.id DESC LIMIT 1) latest
		   JOIN projects p ON p.id = latest.project_id`,
		m.MessageID).Scan(&projectID, &projectSlug)
	if err == nil {
		mc.ProjectID = &projectID
		mc.ProjectSlug = projectSlug
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return mc, fmt.Errorf("resolve project for message %d: %w", m.MessageID, err)
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
