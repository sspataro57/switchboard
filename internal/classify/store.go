package classify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
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
//   - `p.ai_locality = 'local_only'`. THE DISCRIMINATOR, and the only column
//     here with two values in production. Drop this join and the worker starts
//     classifying client work; the integration test's ai_locality='any' case is
//     the assertion that goes red, and it is the only one that can.
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
	   AND NOT EXISTS (
	         SELECT 1 FROM ai_extractions e
	           JOIN ai_runs r ON r.id = e.ai_run_id AND r.worker_type = 'classify'
	          WHERE e.raw_source_item_id = nm.raw_source_item_id)`

const inboxSelect = `
	SELECT nm.id, nm.raw_source_item_id, COALESCE(nm.thread_id, 0),
	       COALESCE(nm.sent_at, now()), COALESCE(nm.sender,''), COALESCE(nm.subject,''),
	       COALESCE(nm.channel,''), COALESCE(nm.body_text,''), COALESCE(nm.direction,''),
	       p.id, p.slug, (p.ai_locality = 'local_only'),
	       COALESCE(nm.links, '[]'::jsonb)`

// PendingMessages returns one pass of the inbox, oldest first.
func (s *PGStore) PendingMessages(ctx context.Context, cfg Config) ([]PendingMessage, error) {
	q := inboxSelect + inboxWhere
	args := []any{}
	if cfg.Since > 0 {
		args = append(args, cfg.Since.String())
		q += fmt.Sprintf(" AND nm.sent_at >= now() - $%d::interval", len(args))
	}
	q += " ORDER BY nm.sent_at, nm.id"
	if cfg.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", cfg.Limit)
	}
	return s.scanMessages(ctx, q, args...)
}

// MessagesByID loads labelled messages for the eval harness. It deliberately
// does NOT apply the inbox filter: a labelled message that has already been
// classified must still be scoreable, or the eval set decays every time a pass
// runs.
func (s *PGStore) MessagesByID(ctx context.Context, ids []int64) ([]PendingMessage, error) {
	if len(ids) == 0 {
		return nil, nil
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
	return s.scanMessages(ctx, q, ids)
}

func (s *PGStore) scanMessages(ctx context.Context, q string, args ...any) ([]PendingMessage, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("select classify inbox: %w", err)
	}
	defer rows.Close()

	var out []PendingMessage
	for rows.Next() {
		var m PendingMessage
		var linksRaw []byte
		if err := rows.Scan(&m.MessageID, &m.RawSourceItemID, &m.ThreadID, &m.SentAt,
			&m.Sender, &m.Subject, &m.Channel, &m.BodyText, &m.Direction,
			&m.ProjectID, &m.ProjectSlug, &m.ProjectLocalOnly, &linksRaw); err != nil {
			return nil, fmt.Errorf("scan classify inbox row: %w", err)
		}
		// The links COLUMN is the contract with the normalizer (SWT-25): the
		// element shape is {"text","url"} and the array position is the
		// identity — the scan must not reorder it, and json.Unmarshal does not.
		if err := json.Unmarshal(linksRaw, &m.Links); err != nil {
			return nil, fmt.Errorf("parse links for message %d: %w", m.MessageID, err)
		}
		// Attribution is AttrProject by construction: the filter above requires a
		// latest decision of 'attributed' with a project. Set explicitly rather
		// than left at the zero value, which is AttrUnseen.
		m.Attribution = attrProject
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
	}
	return out, nil
}

// neighbours loads the INBOUND thread siblings' attribution.
//
// Inbound only, and the filter is load-bearing rather than tidy: the capture
// engine reads direction='inbound' (invariant 5), so an outbound message can
// never carry a decision on any pass in any mode. Folding one would read "no
// decision" as "unclassified" when it means "not applicable", and would restrict
// every thread the system has ever replied on, permanently.
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
