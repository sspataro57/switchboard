package executor_test

// Test doubles for the executor pipeline. Per the SPEC (criterion 4) these
// fakes live in the test file — production `mem.go` is NOT used here so that a
// unit test asserting audit behaviour cannot silently exercise the real store.
//
// They also encode the *interfaces the SPEC names* that the implementation must
// satisfy:
//
//   audit.Store   — Start (writes a `started` row, returns id) / RecordPolicy
//                   (a policy_decisions row FK'd to the audit row) / Complete
//                   (terminal status update).
//   policy.Checker — Check(ctx, Request) (Decision, error), a pure function.
//
// A shared *recorder threads through every stage so the ordering test can
// assert validate -> policy-check -> audit-start -> policy-record -> handler ->
// audit-complete.

import (
	"context"
	"sync"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/policy"
)

// recorder captures the order in which pipeline stages run.
type recorder struct {
	mu    sync.Mutex
	steps []string
}

func (r *recorder) add(step string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, step)
}

func (r *recorder) sequence() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.steps))
	copy(out, r.steps)
	return out
}

// fakeChecker is a policy.Checker whose verdict the test controls.
type fakeChecker struct {
	rec      *recorder
	decision policy.Decision
	err      error
	calls    int
	lastReq  policy.Request
}

func (c *fakeChecker) Check(ctx context.Context, req policy.Request) (policy.Decision, error) {
	c.calls++
	c.lastReq = req
	if c.rec != nil {
		c.rec.add("policy-check")
	}
	return c.decision, c.err
}

// auditRow mirrors one audit_events row's observable state.
type auditRow struct {
	ev     audit.Event
	status string // started -> ok|error|denied
	errMsg string
}

// fakeAudit is an in-memory audit.Store.
type fakeAudit struct {
	rec    *recorder
	nextID int64
	lastID int64
	rows   map[int64]*auditRow
	policy []audit.PolicyDecision
}

func newFakeAudit(rec *recorder) *fakeAudit {
	return &fakeAudit{rec: rec, rows: map[int64]*auditRow{}}
}

func (a *fakeAudit) Start(ctx context.Context, ev audit.Event) (int64, error) {
	if a.rec != nil {
		a.rec.add("audit-start")
	}
	a.nextID++
	a.lastID = a.nextID
	a.rows[a.nextID] = &auditRow{ev: ev, status: "started"}
	return a.nextID, nil
}

func (a *fakeAudit) RecordPolicy(ctx context.Context, auditEventID int64, d audit.PolicyDecision) error {
	if a.rec != nil {
		a.rec.add("policy-record")
	}
	a.policy = append(a.policy, d)
	return nil
}

func (a *fakeAudit) Complete(ctx context.Context, id int64, status, errMsg string) error {
	if a.rec != nil {
		a.rec.add("audit-complete")
	}
	if r, ok := a.rows[id]; ok {
		r.status = status
		r.errMsg = errMsg
	}
	return nil
}

// lastRow returns the most recently started row (tests create exactly one call).
func (a *fakeAudit) lastRow() *auditRow {
	return a.rows[a.lastID]
}

// lastStatus is the status of the most recent row at the moment of the call —
// used inside a handler to prove the row was `started` while the handler ran.
func (a *fakeAudit) lastStatus() string {
	if r := a.lastRow(); r != nil {
		return r.status
	}
	return ""
}

func ptrInt64(v int64) *int64 { return &v }
