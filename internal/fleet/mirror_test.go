package fleet_test

// Unit tests for the mirror's message -> upsert decision logic (SPEC
// 03-mqtt-heartbeats, acceptance criterion 3 and criterion 6). Everything runs
// against a fake Store — ZERO network, ZERO Postgres. The upsert DECISION
// (which columns, preserve-on-dead, FK retry) lives in Mirror.Handle, not the
// store, so it is genuinely exercised here (invariant 7 discipline transfer:
// message -> params is a pure function).
//
// GREENFIELD NOTE: package internal/fleet does not exist yet; this file
// compile-FAILs under `go test ./...` until it is implemented. Exported surface
// imposed here (SPEC's mirror.go):
//
//   // ErrTaskFK is the sentinel a Store's Upsert returns when a task_id fails
//   // the tasks FK; the pg store translates pg SQLSTATE 23503 into it so the
//   // pure Mirror can react without importing pgx.
//   var ErrTaskFK error
//
//   type Heartbeat struct {
//       WorkerID string
//       Client   string
//       State    string
//       TaskID   *int64 // nil => column set NULL
//   }
//
//   type Store interface {
//       // Existing returns the current row for a worker (used to preserve
//       // task_id/client on a dead transition). found=false when absent.
//       Existing(ctx context.Context, workerID string) (hb Heartbeat, found bool, err error)
//       // Upsert writes ON CONFLICT (worker_id); last_seen is stamped now() by
//       // the store, not carried in Heartbeat.
//       Upsert(ctx context.Context, hb Heartbeat) error
//   }
//
//   type MirrorStats struct{ Upserted, Skipped, Warnings int }
//
//   func NewMirror(store Store) *Mirror
//   // Handle processes one retained/live status message. Malformed payloads and
//   // FK-bad task ids are non-fatal (never returns an error for them); it returns
//   // an error only for genuine store failures.
//   func (m *Mirror) Handle(ctx context.Context, topic string, payload []byte) error
//   func (m *Mirror) Stats() MirrorStats

import (
	"context"
	"testing"

	"github.com/sspataro57/switchboard/internal/fleet"
)

// ---- fake store -----------------------------------------------------------

type fakeStore struct {
	existing map[string]fleet.Heartbeat // workerID -> current row (for Existing)

	upserts []fleet.Heartbeat // every attempt, in order (including FK-failed ones)

	// failFKWhenTaskSet makes Upsert return fleet.ErrTaskFK whenever the row
	// carries a non-nil TaskID — models a task_id absent from tasks.
	failFKWhenTaskSet bool
}

func newFakeStore() *fakeStore { return &fakeStore{existing: map[string]fleet.Heartbeat{}} }

func (s *fakeStore) Existing(_ context.Context, workerID string) (fleet.Heartbeat, bool, error) {
	hb, ok := s.existing[workerID]
	return hb, ok, nil
}

func (s *fakeStore) Upsert(_ context.Context, hb fleet.Heartbeat) error {
	s.upserts = append(s.upserts, hb)
	if s.failFKWhenTaskSet && hb.TaskID != nil {
		return fleet.ErrTaskFK
	}
	return nil
}

func (s *fakeStore) lastUpsert(t *testing.T) fleet.Heartbeat {
	t.Helper()
	if len(s.upserts) == 0 {
		t.Fatalf("no upsert was performed")
	}
	return s.upserts[len(s.upserts)-1]
}

func statusTopic(worker string) string { return "ops/workers/" + worker + "/status" }

// Normal status → a full upsert: worker_id, derived client, state, task_id
// (criterion 3, "normal status → full upsert").
func TestMirror_StatusFullUpsert(t *testing.T) {
	store := newFakeStore()
	m := fleet.NewMirror(store)

	if err := m.Handle(context.Background(), statusTopic("acme.backend"),
		[]byte(`{"state":"working","task_id":42}`)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	up := store.lastUpsert(t)
	if up.WorkerID != "acme.backend" {
		t.Errorf("worker_id = %q, want acme.backend", up.WorkerID)
	}
	if up.Client != "acme" {
		t.Errorf("client = %q, want acme (derived from worker_id)", up.Client)
	}
	if up.State != fleet.StateWorking {
		t.Errorf("state = %q, want working", up.State)
	}
	if up.TaskID == nil || *up.TaskID != 42 {
		t.Errorf("task_id = %v, want 42", up.TaskID)
	}
	if got := m.Stats().Upserted; got != 1 {
		t.Errorf("Upserted = %d, want 1", got)
	}
}

// Status without task_id → the column is set NULL (criterion 3).
func TestMirror_StatusWithoutTaskID_SetsNull(t *testing.T) {
	store := newFakeStore()
	m := fleet.NewMirror(store)

	if err := m.Handle(context.Background(), statusTopic("acme"), []byte(`{"state":"idle"}`)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if up := store.lastUpsert(t); up.TaskID != nil {
		t.Errorf("task_id = %v, want nil (NULL column) when omitted", up.TaskID)
	}
}

// A dead message preserves the last known task_id (and client): the mirror reads
// the existing row and carries its task_id forward (criterion 3 + "dead upsert
// preserves task_id and client" decision — step 5 wants "died holding task N").
func TestMirror_DeadPreservesTaskID(t *testing.T) {
	store := newFakeStore()
	store.existing["acme"] = fleet.Heartbeat{
		WorkerID: "acme", Client: "acme", State: fleet.StateWorking, TaskID: ptrInt64(7),
	}
	m := fleet.NewMirror(store)

	if err := m.Handle(context.Background(), statusTopic("acme"), []byte(`{"state":"dead"}`)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	up := store.lastUpsert(t)
	if up.State != fleet.StateDead {
		t.Errorf("state = %q, want dead", up.State)
	}
	if up.TaskID == nil || *up.TaskID != 7 {
		t.Errorf("dead task_id = %v, want 7 preserved from prior row", up.TaskID)
	}
}

// FK-violating task_id: the store reports ErrTaskFK, the mirror retries the
// upsert with task_id=NULL and counts a warning — never crashes (criterion 6).
func TestMirror_FKViolation_RetriesWithNullAndWarns(t *testing.T) {
	store := newFakeStore()
	store.failFKWhenTaskSet = true
	m := fleet.NewMirror(store)

	err := m.Handle(context.Background(), statusTopic("acme"),
		[]byte(`{"state":"working","task_id":999}`))
	if err != nil {
		t.Fatalf("Handle must not return an error for an FK-bad task_id: %v", err)
	}
	if len(store.upserts) != 2 {
		t.Fatalf("upsert attempts = %d, want 2 (first with task_id, retry with NULL)", len(store.upserts))
	}
	if store.upserts[0].TaskID == nil || *store.upserts[0].TaskID != 999 {
		t.Errorf("first upsert task_id = %v, want 999", store.upserts[0].TaskID)
	}
	if store.upserts[1].TaskID != nil {
		t.Errorf("retry upsert task_id = %v, want nil (NULL)", store.upserts[1].TaskID)
	}
	if got := m.Stats().Warnings; got != 1 {
		t.Errorf("Warnings = %d, want 1", got)
	}
}

// A zero-length retained payload (MQTT's clear-retained convention) is silently
// skipped — no upsert, counted, never a parse error (criterion 3 + "empty
// retained" rule).
func TestMirror_EmptyPayload_Skipped(t *testing.T) {
	store := newFakeStore()
	m := fleet.NewMirror(store)

	if err := m.Handle(context.Background(), statusTopic("acme"), []byte{}); err != nil {
		t.Fatalf("Handle(empty): %v", err)
	}
	if len(store.upserts) != 0 {
		t.Errorf("empty payload produced %d upserts, want 0", len(store.upserts))
	}
	if got := m.Stats().Skipped; got != 1 {
		t.Errorf("Skipped = %d, want 1", got)
	}
}

// Malformed JSON is skipped and counted, never fatal (criterion 3, "malformed
// JSON ... never a crash").
func TestMirror_MalformedPayload_Skipped(t *testing.T) {
	store := newFakeStore()
	m := fleet.NewMirror(store)

	if err := m.Handle(context.Background(), statusTopic("acme"), []byte(`{not json`)); err != nil {
		t.Fatalf("Handle(malformed) must be non-fatal: %v", err)
	}
	if len(store.upserts) != 0 {
		t.Errorf("malformed payload produced %d upserts, want 0", len(store.upserts))
	}
	if got := m.Stats().Skipped; got != 1 {
		t.Errorf("Skipped = %d, want 1", got)
	}
}

// An unknown state (valid JSON, out-of-vocabulary state) is stored VERBATIM with
// a warning, not dropped — a contract violation must be visible in the fleet
// view (criterion 3 + "strict publish, lenient consume" decision).
func TestMirror_UnknownState_StoredVerbatimWithWarning(t *testing.T) {
	store := newFakeStore()
	m := fleet.NewMirror(store)

	if err := m.Handle(context.Background(), statusTopic("acme"), []byte(`{"state":"zombie"}`)); err != nil {
		t.Fatalf("Handle(unknown state): %v", err)
	}
	up := store.lastUpsert(t)
	if up.State != "zombie" {
		t.Errorf("state = %q, want it stored verbatim as zombie", up.State)
	}
	if got := m.Stats().Warnings; got != 1 {
		t.Errorf("Warnings = %d, want 1 (unknown state logged)", got)
	}
}
