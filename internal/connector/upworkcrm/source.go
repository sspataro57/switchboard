package upworkcrm

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SourceClient is one upwork_crm.clients row: typed fields for the ingest
// decisions plus the verbatim row JSON that gets hashed and stored.
type SourceClient struct {
	ID  string
	Raw json.RawMessage
}

// SourceCommunication is one upwork_crm.communications row.
type SourceCommunication struct {
	ID        string
	ClientID  string
	CreatedAt time.Time
	IsDraft   bool
	Raw       json.RawMessage
}

// SourceReader reads the upwork_crm source. Implementations must be read-only.
type SourceReader interface {
	ListClients(ctx context.Context) ([]SourceClient, error)
	ListCommunications(ctx context.Context, since time.Time) ([]SourceCommunication, error)
}

// PGSource reads the live upwork_crm tables. Table names are unqualified so
// the integration test can point search_path at a simulated schema.
type PGSource struct {
	pool *pgxpool.Pool
}

func NewSource(pool *pgxpool.Pool) *PGSource {
	return &PGSource{pool: pool}
}

func (s *PGSource) ListClients(ctx context.Context) ([]SourceClient, error) {
	rows, err := s.pool.Query(ctx, `SELECT c.id::text, to_jsonb(c) FROM clients c ORDER BY c.id`)
	if err != nil {
		return nil, fmt.Errorf("list clients: %w", err)
	}
	defer rows.Close()

	var out []SourceClient
	for rows.Next() {
		var c SourceClient
		if err := rows.Scan(&c.ID, &c.Raw); err != nil {
			return nil, fmt.Errorf("scan client: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate clients: %w", err)
	}
	return out, nil
}

func (s *PGSource) ListCommunications(ctx context.Context, since time.Time) ([]SourceCommunication, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT m.id::text, m.client_id::text, m.created_at, m.is_draft, to_jsonb(m)
		 FROM communications m
		 WHERE m.created_at > $1
		 ORDER BY m.created_at, m.id`, since)
	if err != nil {
		return nil, fmt.Errorf("list communications: %w", err)
	}
	defer rows.Close()

	var out []SourceCommunication
	for rows.Next() {
		var m SourceCommunication
		if err := rows.Scan(&m.ID, &m.ClientID, &m.CreatedAt, &m.IsDraft, &m.Raw); err != nil {
			return nil, fmt.Errorf("scan communication: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate communications: %w", err)
	}
	return out, nil
}
