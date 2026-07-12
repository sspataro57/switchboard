package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/chash"
	"github.com/sspataro57/switchboard/internal/executor"
)

// Provider and account identity of the webhook raw store.
const (
	Provider     = "github"
	AccountEmail = "github@webhooks"
)

// PGRawStore stores webhook deliveries raw-first under the synthetic
// provider='github' account.
type PGRawStore struct {
	pool *pgxpool.Pool
}

func NewPGRawStore(pool *pgxpool.Pool) *PGRawStore {
	return &PGRawStore{pool: pool}
}

func (s *PGRawStore) ensureAccount(ctx context.Context) (int64, error) {
	var accountID int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, send_enabled)
		 VALUES ($1, $2, false)
		 ON CONFLICT (provider, account_email) DO UPDATE SET send_enabled=false
		 RETURNING id`, Provider, AccountEmail).Scan(&accountID)
	if err != nil {
		return 0, fmt.Errorf("ensure github account: %w", err)
	}
	return accountID, nil
}

func (s *PGRawStore) StoreDelivery(ctx context.Context, guid, eventType string, body []byte) (bool, error) {
	accountID, err := s.ensureAccount(ctx)
	if err != nil {
		return false, err
	}

	// wrap in an envelope carrying the event type (the body alone lacks it)
	envelope, err := json.Marshal(map[string]any{"event": eventType, "payload": json.RawMessage(body)})
	if err != nil {
		return false, fmt.Errorf("marshal delivery envelope: %w", err)
	}
	hash, err := chash.ContentHash(envelope)
	if err != nil {
		return false, fmt.Errorf("hash delivery: %w", err)
	}

	tag, err := s.pool.Exec(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1, $2, $3, $4, now())
		 ON CONFLICT (source_account_id, external_id) DO NOTHING`,
		accountID, "delivery:"+guid, envelope, hash)
	if err != nil {
		return false, fmt.Errorf("insert delivery raw: %w", err)
	}
	return tag.RowsAffected() == 0, nil // already=true when the guid existed
}

// UpsertRaw stores poller-fetched JSON raw-first, hash-diffed (external_id
// pr:{owner}/{repo}#{n} or run:{owner}/{repo}:{id}).
func (s *PGRawStore) UpsertRaw(ctx context.Context, externalID string, raw json.RawMessage) error {
	accountID, err := s.ensureAccount(ctx)
	if err != nil {
		return err
	}
	hash, err := chash.ContentHash(raw)
	if err != nil {
		return fmt.Errorf("hash %s: %w", externalID, err)
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1, $2, $3, $4, now())
		 ON CONFLICT (source_account_id, external_id) DO UPDATE
		   SET raw_json = EXCLUDED.raw_json, content_hash = EXCLUDED.content_hash
		 WHERE raw_source_items.content_hash IS DISTINCT FROM EXCLUDED.content_hash`,
		accountID, externalID, raw, hash)
	if err != nil {
		return fmt.Errorf("upsert raw %s: %w", externalID, err)
	}
	return nil
}

// PGTaskResolver maps PRRefs to tasks: newest active external_ref for the PR,
// else the task-{N}-* head-branch fallback (linking itself in that case —
// through the executor's link_external_ref, never a direct write).
type PGTaskResolver struct {
	pool  *pgxpool.Pool
	ex    Dispatcher
	actor string
}

func NewPGTaskResolver(pool *pgxpool.Pool, ex Dispatcher, actor string) *PGTaskResolver {
	return &PGTaskResolver{pool: pool, ex: ex, actor: actor}
}

var branchTaskRe = regexp.MustCompile(`^task-(\d+)(-|$)`)

func (r *PGTaskResolver) Resolve(ctx context.Context, ref PRRef) (int64, bool, error) {
	key := fmt.Sprintf("%s#%d", ref.Repo, ref.PR)
	var taskID int64
	err := r.pool.QueryRow(ctx,
		`SELECT task_id FROM external_refs
		 WHERE system='github' AND external_key=$1
		 ORDER BY created_at DESC, id DESC LIMIT 1`, key).Scan(&taskID)
	if err == nil {
		return taskID, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, fmt.Errorf("resolve external ref %s: %w", key, err)
	}

	// task-{N}-* head-branch fallback: link + resolve.
	m := branchTaskRe.FindStringSubmatch(ref.HeadBranch)
	if m == nil {
		return 0, false, nil
	}
	taskID, err = strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false, nil
	}
	var exists bool
	if err := r.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM tasks WHERE id=$1)`, taskID).Scan(&exists); err != nil {
		return 0, false, fmt.Errorf("verify task %d: %w", taskID, err)
	}
	if !exists {
		return 0, false, nil
	}
	args, err := json.Marshal(map[string]any{
		"task_id": taskID, "system": "github", "external_key": key,
	})
	if err != nil {
		return 0, false, fmt.Errorf("marshal link args: %w", err)
	}
	if _, err := r.ex.Execute(ctx, executor.Call{Tool: "link_external_ref", Actor: r.actor, Args: args}); err != nil {
		return 0, false, fmt.Errorf("link fallback ref %s: %w", key, err)
	}
	return taskID, true, nil
}
