package fleet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

// ErrTaskFK is the sentinel a Store's Upsert returns when task_id fails the
// tasks FK — the pg store translates SQLSTATE 23503 into it so the mirror
// stays pgx-free.
var ErrTaskFK = errors.New("task_id violates tasks FK")

// Heartbeat is one worker_heartbeats row's writable state; last_seen is
// stamped now() by the store.
type Heartbeat struct {
	WorkerID string
	Client   string
	State    string
	TaskID   *int64
}

// Store is the mirror's persistence dependency.
type Store interface {
	// Existing returns the current row for a worker — used to preserve
	// task_id/client on a dead transition. found=false when absent.
	Existing(ctx context.Context, workerID string) (hb Heartbeat, found bool, err error)
	// Upsert writes ON CONFLICT (worker_id).
	Upsert(ctx context.Context, hb Heartbeat) error
}

type MirrorStats struct {
	Upserted, Skipped, Warnings int
}

// Mirror turns status messages into worker_heartbeats upserts. Telemetry
// discipline: malformed payloads and FK-bad task ids are non-fatal (skipped or
// degraded with a warning) — a telemetry daemon must not crash-loop on one bad
// publisher. Handle returns an error only for genuine store failures.
type Mirror struct {
	store Store

	mu    sync.Mutex
	stats MirrorStats
}

func NewMirror(store Store) *Mirror {
	return &Mirror{store: store}
}

func (m *Mirror) Stats() MirrorStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stats
}

func (m *Mirror) skip(topic, reason string) {
	m.mu.Lock()
	m.stats.Skipped++
	m.mu.Unlock()
	slog.Warn("skipping status message", "topic", topic, "reason", reason)
}

func (m *Mirror) warn(topic, msg string) {
	m.mu.Lock()
	m.stats.Warnings++
	m.mu.Unlock()
	slog.Warn(msg, "topic", topic)
}

// Handle processes one retained/live status message.
func (m *Mirror) Handle(ctx context.Context, topic string, payload []byte) error {
	workerID, err := ParseStatusTopic(topic)
	if err != nil {
		m.skip(topic, err.Error())
		return nil
	}

	// Zero-length retained payload = MQTT clear-retained convention.
	if len(payload) == 0 {
		m.skip(topic, "empty retained payload")
		return nil
	}

	status, err := ParseStatus(payload)
	if err != nil {
		m.skip(topic, err.Error())
		return nil
	}

	hb := Heartbeat{
		WorkerID: workerID,
		Client:   ClientFromWorkerID(workerID),
		State:    status.State,
		TaskID:   status.TaskID,
	}

	switch status.State {
	case StateIdle, StateWorking, StateNeedsFeedback, StateManual:
		// live states overwrite fully (idle legitimately NULLs task_id)
	case StateDead:
		// preserve "died holding task N" for step 5's recovery rules
		if prev, found, err := m.store.Existing(ctx, workerID); err != nil {
			return fmt.Errorf("read existing heartbeat for %s: %w", workerID, err)
		} else if found {
			hb.TaskID = prev.TaskID
			if prev.Client != "" {
				hb.Client = prev.Client
			}
		}
	default:
		// contract violation stays visible in the fleet view, not dropped
		m.warn(topic, fmt.Sprintf("unknown state %q stored verbatim", status.State))
	}

	if err := m.store.Upsert(ctx, hb); err != nil {
		if errors.Is(err, ErrTaskFK) {
			m.warn(topic, fmt.Sprintf("task_id %v not in tasks; upserting with NULL", hb.TaskID))
			hb.TaskID = nil
			if err := m.store.Upsert(ctx, hb); err != nil {
				return fmt.Errorf("upsert heartbeat for %s after FK retry: %w", workerID, err)
			}
		} else {
			return fmt.Errorf("upsert heartbeat for %s: %w", workerID, err)
		}
	}
	m.mu.Lock()
	m.stats.Upserted++
	m.mu.Unlock()
	return nil
}
