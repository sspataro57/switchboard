// Package audit records every executor tool call as one audit_events row
// (inserted `started`, updated to a terminal status) plus the policy verdict
// that gated it (policy_decisions). Invariant 3's paper trail lives here.
package audit

import (
	"context"
	"encoding/json"
)

// Event is the immutable identity of one tool call.
type Event struct {
	Actor  string
	Tool   string
	Args   json.RawMessage
	TaskID *int64
}

// PolicyDecision mirrors one policy_decisions row, FK'd to its audit event.
type PolicyDecision struct {
	Tool     string
	Decision string // allow | deny | needs_approval
	Rule     string
	Reason   string
}

// Store is the executor's audit dependency. Start writes a `started` row and
// returns its id; RecordPolicy attaches the policy verdict to that row;
// Complete sets the terminal status (ok | error | denied).
type Store interface {
	Start(ctx context.Context, ev Event) (int64, error)
	RecordPolicy(ctx context.Context, auditEventID int64, d PolicyDecision) error
	Complete(ctx context.Context, id int64, status, errMsg string) error
}
