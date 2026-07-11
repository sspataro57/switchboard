// Package policy holds the policy check hook the executor runs before every
// handler. Checkers are pure functions of the request and their configuration
// (invariant 7 discipline): no I/O, no clock, no network. Persisting the
// verdict is the executor's job, via audit.Store.RecordPolicy.
package policy

import (
	"context"
	"encoding/json"
)

// Request describes the call being gated. Args carries the tool args so
// delivery-gated policy can resolve the delivery's channel.
type Request struct {
	Tool   string
	Actor  string
	TaskID *int64
	Args   json.RawMessage
}

// Decision is the verdict. Decision is one of allow | deny | needs_approval;
// Rule names the rule that produced it, Reason says why (required on deny).
type Decision struct {
	Decision string
	Rule     string
	Reason   string
}

// Checker gates a call. Implementations must be pure.
type Checker interface {
	Check(ctx context.Context, req Request) (Decision, error)
}

// static is step 1's default policy: tools registered at construction are
// allowed, everything else is denied. The real policy matrix replaces this;
// the hook and the recording are what this step ships.
type static struct {
	registered map[string]struct{}
}

// NewStatic returns a Checker allowing exactly the named tools.
func NewStatic(registered ...string) Checker {
	s := &static{registered: make(map[string]struct{}, len(registered))}
	for _, name := range registered {
		s.registered[name] = struct{}{}
	}
	return s
}

func (s *static) Check(_ context.Context, req Request) (Decision, error) {
	if _, ok := s.registered[req.Tool]; ok {
		return Decision{Decision: "allow", Rule: "static-default", Reason: "registered internal tool"}, nil
	}
	return Decision{Decision: "deny", Rule: "static-default", Reason: "tool not in registered set"}, nil
}
