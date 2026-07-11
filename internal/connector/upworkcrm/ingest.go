package upworkcrm

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Provider and account identity of this connector in source_accounts.
// account_email is visibly synthetic: the CRM is a system source, not a mailbox.
const (
	Provider     = "upwork_crm"
	AccountEmail = "upwork_crm@pg-main"
)

// DefaultOverlap is the trailing re-read window on the communications cursor.
// The scraper can insert rows late; idempotent upserts make the overlap free.
const DefaultOverlap = 24 * time.Hour

// Cursor is the per-account sync position stored in source_accounts.sync_cursor.
type Cursor struct {
	CommunicationsCreatedAt time.Time `json:"communications_created_at"`
}

// Stats is one run's bookkeeping, stored in sync_runs.stats.
type Stats struct {
	ClientsSeen        int `json:"clients_seen"`
	CommunicationsSeen int `json:"communications_seen"`
	RawInserted        int `json:"raw_inserted"`
	RawUpdated         int `json:"raw_updated"`
	RawUnchanged       int `json:"raw_unchanged"`
	Normalized         int `json:"normalized"`
	SuspectedMerges    int `json:"suspected_merges"`
}

// Config selects run modes: Full rescans communications from the zero time,
// All reprocesses every raw row in the normalize phase, Overlap is the cursor
// re-read window.
type Config struct {
	Full    bool
	All     bool
	Overlap time.Duration
}

// Sink is the ops-db side of ingestion. RawHash returns the stored
// content_hash for (account, external_id); Ingest compares and calls
// InsertRaw / UpdateRaw / neither. UpdateRaw resets normalized_at to NULL.
type Sink interface {
	EnsureAccount(ctx context.Context) (accountID int64, err error)
	Cursor(ctx context.Context, accountID int64) (Cursor, error)
	StartRun(ctx context.Context, accountID int64) (runID int64, err error)
	RawHash(ctx context.Context, accountID int64, externalID string) (hash string, exists bool, err error)
	InsertRaw(ctx context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) error
	UpdateRaw(ctx context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) error
	SaveCursor(ctx context.Context, accountID int64, c Cursor) error
	FinishRun(ctx context.Context, runID int64, status string, stats Stats, errMsg string) error
}

// Ingest is the raw-first phase: every source row lands in raw_source_items
// before any normalization happens (invariant 1). Drafts are skipped at query
// time; the cursor advances only on success.
func Ingest(ctx context.Context, src SourceReader, sink Sink, cfg Config) (Stats, error) {
	var stats Stats

	accountID, err := sink.EnsureAccount(ctx)
	if err != nil {
		return stats, fmt.Errorf("ensure source account: %w", err)
	}
	runID, err := sink.StartRun(ctx, accountID)
	if err != nil {
		return stats, fmt.Errorf("start sync run: %w", err)
	}

	// fail finishes the run as an error without advancing the cursor. A
	// FinishRun failure must not mask the original cause, so it is dropped.
	fail := func(cause error) (Stats, error) {
		_ = sink.FinishRun(ctx, runID, "error", stats, cause.Error())
		return stats, cause
	}

	var since time.Time
	if !cfg.Full {
		cur, err := sink.Cursor(ctx, accountID)
		if err != nil {
			return fail(fmt.Errorf("read cursor: %w", err))
		}
		if !cur.CommunicationsCreatedAt.IsZero() {
			since = cur.CommunicationsCreatedAt.Add(-cfg.Overlap)
		}
	}

	clients, err := src.ListClients(ctx)
	if err != nil {
		return fail(fmt.Errorf("list clients: %w", err))
	}
	stats.ClientsSeen = len(clients)
	for _, c := range clients {
		if err := upsertRaw(ctx, sink, accountID, "clients:"+c.ID, c.Raw, &stats); err != nil {
			return fail(fmt.Errorf("ingest client %s: %w", c.ID, err))
		}
	}

	comms, err := src.ListCommunications(ctx, since)
	if err != nil {
		return fail(fmt.Errorf("list communications: %w", err))
	}
	stats.CommunicationsSeen = len(comms)

	var maxCreated time.Time
	for _, m := range comms {
		if m.CreatedAt.After(maxCreated) {
			maxCreated = m.CreatedAt
		}
		if m.IsDraft {
			continue // CRM working state, not a communication that happened
		}
		if err := upsertRaw(ctx, sink, accountID, "communications:"+m.ID, m.Raw, &stats); err != nil {
			return fail(fmt.Errorf("ingest communication %s: %w", m.ID, err))
		}
	}

	if !maxCreated.IsZero() {
		if err := sink.SaveCursor(ctx, accountID, Cursor{CommunicationsCreatedAt: maxCreated}); err != nil {
			return fail(fmt.Errorf("save cursor: %w", err))
		}
	}
	if err := sink.FinishRun(ctx, runID, "ok", stats, ""); err != nil {
		return stats, fmt.Errorf("finish sync run: %w", err)
	}
	return stats, nil
}

// upsertRaw makes the insert / update / unchanged decision from the stored
// content_hash. The decision lives here — not in the sink — so it is exercised
// by unit tests.
func upsertRaw(ctx context.Context, sink Sink, accountID int64, externalID string, raw json.RawMessage, stats *Stats) error {
	h, err := ContentHash(raw)
	if err != nil {
		return fmt.Errorf("hash %s: %w", externalID, err)
	}
	stored, exists, err := sink.RawHash(ctx, accountID, externalID)
	if err != nil {
		return fmt.Errorf("read stored hash for %s: %w", externalID, err)
	}
	switch {
	case !exists:
		if err := sink.InsertRaw(ctx, accountID, externalID, raw, h); err != nil {
			return fmt.Errorf("insert raw %s: %w", externalID, err)
		}
		stats.RawInserted++
	case stored == h:
		stats.RawUnchanged++
	default:
		if err := sink.UpdateRaw(ctx, accountID, externalID, raw, h); err != nil {
			return fmt.Errorf("update raw %s: %w", externalID, err)
		}
		stats.RawUpdated++
	}
	return nil
}
