// Package executor is the single gate every tool call passes through
// (invariant 3): registry lookup → validate → policy check → audit start →
// policy record → handler → audit complete. No stage is skippable; denials,
// validation failures and handler errors still complete the audit row. The
// pipeline depends only on interfaces and is unit-testable with zero network.
package executor

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/policy"
)

// Call identifies one tool invocation.
type Call struct {
	Tool   string
	Actor  string
	Args   json.RawMessage
	TaskID *int64
}

// Result is what a successful handler returned.
type Result struct {
	Output json.RawMessage
}

type Executor struct {
	reg     *Registry
	checker policy.Checker
	store   audit.Store
}

func New(reg *Registry, checker policy.Checker, store audit.Store) *Executor {
	return &Executor{reg: reg, checker: checker, store: store}
}

// Execute runs the full pipeline for one call. Every path — unknown tool,
// validation failure, policy denial, handler error, success — writes an audit
// row with a terminal status.
func (e *Executor) Execute(ctx context.Context, call Call) (Result, error) {
	ev := audit.Event{Actor: call.Actor, Tool: call.Tool, Args: call.Args, TaskID: call.TaskID}

	tool, ok := e.reg.lookup(call.Tool)
	if !ok {
		err := fmt.Errorf("unknown tool %q", call.Tool)
		e.auditFailure(ctx, ev, "error", err)
		return Result{}, err
	}

	if tool.Validate != nil {
		if verr := tool.Validate(call.Args); verr != nil {
			err := fmt.Errorf("validate %s args: %w", call.Tool, verr)
			e.auditFailure(ctx, ev, "error", err)
			return Result{}, err
		}
	}

	decision, err := e.checker.Check(ctx, policy.Request{Tool: call.Tool, Actor: call.Actor, TaskID: call.TaskID, Args: call.Args})
	if err != nil {
		err = fmt.Errorf("policy check %s: %w", call.Tool, err)
		e.auditFailure(ctx, ev, "error", err)
		return Result{}, err
	}

	id, err := e.store.Start(ctx, ev)
	if err != nil {
		return Result{}, fmt.Errorf("audit start %s: %w", call.Tool, err)
	}

	if err := e.store.RecordPolicy(ctx, id, audit.PolicyDecision{
		Tool:     call.Tool,
		Decision: decision.Decision,
		Rule:     decision.Rule,
		Reason:   decision.Reason,
	}); err != nil {
		err = fmt.Errorf("record policy decision %s: %w", call.Tool, err)
		_ = e.store.Complete(ctx, id, "error", err.Error())
		return Result{}, err
	}

	if decision.Decision != "allow" {
		err := fmt.Errorf("tool %q denied by policy (%s): %s", call.Tool, decision.Rule, decision.Reason)
		if cerr := e.store.Complete(ctx, id, "denied", err.Error()); cerr != nil {
			return Result{}, fmt.Errorf("audit complete after denial: %w", cerr)
		}
		return Result{}, err
	}

	out, herr := tool.Handle(WithActor(ctx, call.Actor), call.Args)
	if herr != nil {
		werr := fmt.Errorf("tool %s: %w", call.Tool, herr)
		if cerr := e.store.Complete(ctx, id, "error", werr.Error()); cerr != nil {
			return Result{}, fmt.Errorf("audit complete after handler error: %w", cerr)
		}
		return Result{}, werr
	}

	if err := e.store.Complete(ctx, id, "ok", ""); err != nil {
		return Result{}, fmt.Errorf("audit complete %s: %w", call.Tool, err)
	}
	return Result{Output: out}, nil
}

type actorKey struct{}

// WithActor threads the executor Call's actor into the handler context —
// handlers that record who acted (created_by, decided_by) read it via
// ActorFrom instead of trusting caller-supplied args.
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// ActorFrom returns the acting identity, or "" outside an executor call.
func ActorFrom(ctx context.Context) string {
	if v, ok := ctx.Value(actorKey{}).(string); ok {
		return v
	}
	return ""
}

// auditFailure writes a start+terminal audit pair for calls that fail before
// the normal start point (unknown tool, validation, policy-check error). The
// call must still be audited; audit-store errors here cannot mask the original
// failure, so they are deliberately dropped.
func (e *Executor) auditFailure(ctx context.Context, ev audit.Event, status string, cause error) {
	id, err := e.store.Start(ctx, ev)
	if err != nil {
		return
	}
	_ = e.store.Complete(ctx, id, status, cause.Error())
}
