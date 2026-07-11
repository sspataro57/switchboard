package audit

import (
	"context"
	"sync"
)

// MemRow is one in-memory audit row (exported for callers inspecting a MemStore).
type MemRow struct {
	Event  Event
	Status string
	ErrMsg string
}

// MemStore is the in-memory Store — for tools and tests that need a real audit
// trail without Postgres. The unit-test fakes in executor_test deliberately do
// NOT use it (they assert against their own recording); this exists for
// non-test wiring (dry runs, future REPLs).
type MemStore struct {
	mu     sync.Mutex
	nextID int64
	rows   map[int64]*MemRow
	policy map[int64][]PolicyDecision
}

func NewMemStore() *MemStore {
	return &MemStore{rows: map[int64]*MemRow{}, policy: map[int64][]PolicyDecision{}}
}

func (s *MemStore) Start(_ context.Context, ev Event) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	s.rows[s.nextID] = &MemRow{Event: ev, Status: "started"}
	return s.nextID, nil
}

func (s *MemStore) RecordPolicy(_ context.Context, auditEventID int64, d PolicyDecision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policy[auditEventID] = append(s.policy[auditEventID], d)
	return nil
}

func (s *MemStore) Complete(_ context.Context, id int64, status, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rows[id]; ok {
		r.Status = status
		r.ErrMsg = errMsg
	}
	return nil
}

// Rows returns a copy of all rows keyed by audit event id.
func (s *MemStore) Rows() map[int64]MemRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int64]MemRow, len(s.rows))
	for id, r := range s.rows {
		out[id] = *r
	}
	return out
}
