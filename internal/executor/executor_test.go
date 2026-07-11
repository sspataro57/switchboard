package executor_test

// Unit tests for the executor pipeline (SPEC 01-schema-executor, acceptance
// criterion 4). These run with ZERO network and ZERO Postgres — every
// dependency is an in-memory fake (see fakes_test.go). Invariant 3 (everything
// through the executor) and invariant 7 (pure, unit-testable) are what these
// pin down.
//
// GREENFIELD NOTE: the executor package does not exist yet. These tests define
// the contract the SPEC names and therefore compile-FAIL until the package is
// implemented. Expected exported surface exercised below:
//
//   type Call struct { Tool, Actor string; Args json.RawMessage; TaskID *int64 }
//   type Result struct { Output json.RawMessage }
//   type Tool struct {
//       Name     string
//       Validate func(json.RawMessage) error
//       Handle   func(context.Context, json.RawMessage) (json.RawMessage, error)
//   }
//   func NewRegistry() *Registry
//   func (*Registry) Register(Tool)
//   func (*Registry) Names() []string
//   func New(*Registry, policy.Checker, audit.Store) *Executor
//   func (*Executor) Execute(context.Context, Call) (Result, error)
//
// Pipeline order (no skips): registry lookup -> validate -> policy.Check ->
// audit Start -> RecordPolicy -> handler -> audit Complete. Denials and
// validation/handler errors still complete the audit row.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
)

const testTool = "create_task"

var allowDecision = policy.Decision{Decision: "allow", Rule: "static-default", Reason: "registered internal tool"}

// registerTool wires a single tool into a fresh registry with recording
// validate/handle closures.
func registerTool(rec *recorder, validate func([]byte) error, handle func(context.Context, []byte) ([]byte, error)) *executor.Registry {
	reg := executor.NewRegistry()
	reg.Register(executor.Tool{
		Name: testTool,
		Validate: func(args []byte) error {
			if rec != nil {
				rec.add("validate")
			}
			if validate != nil {
				return validate(args)
			}
			return nil
		},
		Handle: func(ctx context.Context, args []byte) ([]byte, error) {
			if rec != nil {
				rec.add("handler")
			}
			return handle(ctx, args)
		},
	})
	return reg
}

// 4a. Happy path: audit row is `started` before the handler runs, `ok` after;
// handler result is returned to the caller.
func TestExecutor_HappyPath(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	audit := newFakeAudit(rec)
	checker := &fakeChecker{rec: rec, decision: allowDecision}

	want := []byte(`{"task_id":1}`)
	var statusAtHandler string
	reg := registerTool(rec, nil, func(ctx context.Context, args []byte) ([]byte, error) {
		statusAtHandler = audit.lastStatus() // must be "started" while running
		return want, nil
	})

	ex := executor.New(reg, checker, audit)
	res, err := ex.Execute(ctx, executor.Call{Tool: testTool, Actor: "tester", Args: []byte(`{"project":"p","title":"t"}`)})
	if err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}
	if !bytes.Equal(res.Output, want) {
		t.Errorf("Result.Output = %s, want %s", res.Output, want)
	}
	if statusAtHandler != "started" {
		t.Errorf("audit status while handler ran = %q, want %q", statusAtHandler, "started")
	}
	if got := audit.lastRow().status; got != "ok" {
		t.Errorf("final audit status = %q, want %q", got, "ok")
	}
	if len(audit.rows) != 1 {
		t.Errorf("audit rows = %d, want exactly 1", len(audit.rows))
	}
}

// 4b. Policy denial: handler is NEVER invoked, audit row ends `denied`, and a
// policy decision (deny + reason) is recorded.
func TestExecutor_PolicyDeny(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	audit := newFakeAudit(rec)
	checker := &fakeChecker{rec: rec, decision: policy.Decision{Decision: "deny", Rule: "static-default", Reason: "tool not permitted"}}

	handlerCalled := false
	reg := registerTool(rec, nil, func(ctx context.Context, args []byte) ([]byte, error) {
		handlerCalled = true
		return []byte(`{}`), nil
	})

	ex := executor.New(reg, checker, audit)
	_, err := ex.Execute(ctx, executor.Call{Tool: testTool, Actor: "tester", Args: []byte(`{}`)})
	if err == nil {
		t.Fatalf("Execute: expected an error on policy denial, got nil")
	}
	if handlerCalled {
		t.Errorf("handler was invoked despite policy denial")
	}
	if got := audit.lastRow().status; got != "denied" {
		t.Errorf("final audit status = %q, want %q", got, "denied")
	}
	if len(audit.policy) != 1 {
		t.Fatalf("recorded policy decisions = %d, want 1", len(audit.policy))
	}
	if d := audit.policy[0]; d.Decision != "deny" || d.Reason == "" {
		t.Errorf("recorded policy decision = %+v, want deny with non-empty reason", d)
	}
	if checker.calls != 1 {
		t.Errorf("checker calls = %d, want 1", checker.calls)
	}
}

// 4c. Validation failure: rejected BEFORE the policy check and handler; audit
// row ends `error` carrying the validation message.
func TestExecutor_ValidationFailure(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	audit := newFakeAudit(rec)
	checker := &fakeChecker{rec: rec, decision: allowDecision}

	handlerCalled := false
	reg := registerTool(rec,
		func(args []byte) error { return errors.New("missing title") },
		func(ctx context.Context, args []byte) ([]byte, error) {
			handlerCalled = true
			return nil, nil
		})

	ex := executor.New(reg, checker, audit)
	_, err := ex.Execute(ctx, executor.Call{Tool: testTool, Actor: "tester", Args: []byte(`{}`)})
	if err == nil {
		t.Fatalf("Execute: expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "missing title") {
		t.Errorf("error = %q, want it to contain the validation message %q", err.Error(), "missing title")
	}
	if checker.calls != 0 {
		t.Errorf("policy checker called %d times on validation failure, want 0", checker.calls)
	}
	if handlerCalled {
		t.Errorf("handler invoked on validation failure")
	}
	row := audit.lastRow()
	if row == nil {
		t.Fatalf("no audit row written for validation failure; the call must still be audited")
	}
	if row.status != "error" {
		t.Errorf("final audit status = %q, want %q", row.status, "error")
	}
	if !strings.Contains(row.errMsg, "missing title") {
		t.Errorf("audit errMsg = %q, want it to contain the validation message", row.errMsg)
	}
}

// 4d. Handler error: audit row ends `error`, and the returned error is wrapped
// with context while preserving the cause.
func TestExecutor_HandlerError(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	audit := newFakeAudit(rec)
	checker := &fakeChecker{rec: rec, decision: allowDecision}

	sentinel := errors.New("db down")
	reg := registerTool(rec, nil, func(ctx context.Context, args []byte) ([]byte, error) {
		return nil, sentinel
	})

	ex := executor.New(reg, checker, audit)
	_, err := ex.Execute(ctx, executor.Call{Tool: testTool, Actor: "tester", Args: []byte(`{}`)})
	if err == nil {
		t.Fatalf("Execute: expected handler error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want it to wrap sentinel (errors.Is)", err)
	}
	if err.Error() == sentinel.Error() {
		t.Errorf("error not wrapped with context: %q equals the bare cause", err.Error())
	}
	row := audit.lastRow()
	if row.status != "error" {
		t.Errorf("final audit status = %q, want %q", row.status, "error")
	}
	if !strings.Contains(row.errMsg, "db down") {
		t.Errorf("audit errMsg = %q, want it to contain the cause", row.errMsg)
	}
}

// 4e. Unknown tool: rejected and audited; policy/validate/handler never reached.
func TestExecutor_UnknownTool(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	audit := newFakeAudit(rec)
	checker := &fakeChecker{rec: rec, decision: allowDecision}

	reg := executor.NewRegistry() // nothing registered

	ex := executor.New(reg, checker, audit)
	_, err := ex.Execute(ctx, executor.Call{Tool: "raw_sql", Actor: "tester", Args: []byte(`{}`)})
	if err == nil {
		t.Fatalf("Execute: expected error for unknown tool, got nil")
	}
	if !strings.Contains(err.Error(), "raw_sql") {
		t.Errorf("error = %q, want it to name the unknown tool", err.Error())
	}
	if checker.calls != 0 {
		t.Errorf("policy checker called %d times for unknown tool, want 0", checker.calls)
	}
	row := audit.lastRow()
	if row == nil {
		t.Fatalf("unknown tool must still be audited; no audit row written")
	}
	if row.status != "error" {
		t.Errorf("final audit status = %q, want %q", row.status, "error")
	}
}

// Ordering: the pipeline runs stages in the exact SPEC order and never skips.
func TestExecutor_PipelineOrder(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	audit := newFakeAudit(rec)
	checker := &fakeChecker{rec: rec, decision: allowDecision}

	reg := registerTool(rec, nil, func(ctx context.Context, args []byte) ([]byte, error) {
		return []byte(`{"task_id":7}`), nil
	})

	ex := executor.New(reg, checker, audit)
	if _, err := ex.Execute(ctx, executor.Call{Tool: testTool, Actor: "tester", Args: []byte(`{}`), TaskID: ptrInt64(7)}); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}

	want := []string{"validate", "policy-check", "audit-start", "policy-record", "handler", "audit-complete"}
	got := rec.sequence()
	if len(got) != len(want) {
		t.Fatalf("pipeline sequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pipeline sequence = %v, want %v", got, want)
		}
	}
}
