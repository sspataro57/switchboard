package planimport

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AccountEmail is the synthetic provider='plan' source account (hooksd
// github@webhooks idiom): plans are files, not an external inbox, but raw-first
// still needs an owner row.
const AccountEmail = "plans@local"

// PGStore is the real Store over the ops db.
type PGStore struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool}
}

func (s *PGStore) EnsurePlanAccount(ctx context.Context) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, send_enabled)
		 VALUES ('plan', $1, false)
		 ON CONFLICT (provider, account_email) DO UPDATE SET send_enabled=false
		 RETURNING id`, AccountEmail).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("ensure plan account: %w", err)
	}
	return id, nil
}

// UpsertRaw stores the plan file raw-first. normalized_at is stamped
// immediately: plans never become normalized_messages, and a permanently-NULL
// normalized_at would read as "ingested, never processed" in raw-lag queries.
func (s *PGStore) UpsertRaw(ctx context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1, $2, $3, $4, now())
		 ON CONFLICT (source_account_id, external_id) DO UPDATE
		   SET raw_json = EXCLUDED.raw_json, content_hash = EXCLUDED.content_hash
		 RETURNING id`, accountID, externalID, raw, hash).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert raw %s: %w", externalID, err)
	}
	return id, nil
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

func (s *PGStore) RecordExtraction(ctx context.Context, aiRunID, rawSourceItemID int64, fields json.RawMessage) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields)
		 VALUES ($1,$2,$3) RETURNING id`, aiRunID, rawSourceItemID, fields).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert ai_extraction: %w", err)
	}
	return id, nil
}

func safeJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || !json.Valid(raw) {
		return json.RawMessage(`{}`)
	}
	return raw
}
