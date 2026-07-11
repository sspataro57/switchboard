package policy_test

// Unit tests for the static default policy checker (SPEC 01-schema-executor,
// "internal/policy — Checker interface + a static default for step 1: registered
// internal tools allow, everything else deny"). Pure functions, zero I/O
// (invariant 7).
//
// GREENFIELD NOTE: the policy package does not exist yet; these tests define
// the contract the SPEC names and compile-FAIL until it is implemented.
// Expected exported surface:
//
//   type Request  struct { Tool, Actor string; TaskID *int64 }
//   type Decision struct { Decision, Rule, Reason string } // decision in allow|deny|needs_approval
//   type Checker  interface { Check(context.Context, Request) (Decision, error) }
//   func NewStatic(registered ...string) Checker
//
// The DB write of policy_decisions is the executor's job (and is covered by the
// integration test); here we pin only the pure verdict.

import (
	"context"
	"testing"

	"github.com/sspataro57/switchboard/internal/policy"
)

func TestStaticPolicy_Verdicts(t *testing.T) {
	ctx := context.Background()
	checker := policy.NewStatic("create_task")

	cases := []struct {
		name string
		tool string
		want string
	}{
		{"registered tool allows", "create_task", "allow"},
		{"unregistered tool denies", "raw_sql", "deny"},
		{"raw_api denies", "raw_api", "deny"},
		{"empty tool denies", "", "deny"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := checker.Check(ctx, policy.Request{Tool: tc.tool, Actor: "tester"})
			if err != nil {
				t.Fatalf("Check(%q): unexpected error: %v", tc.tool, err)
			}
			if d.Decision != tc.want {
				t.Errorf("Check(%q).Decision = %q, want %q", tc.tool, d.Decision, tc.want)
			}
			if tc.want == "deny" && d.Reason == "" {
				t.Errorf("Check(%q): deny must carry a reason, got empty", tc.tool)
			}
			if tc.want == "allow" && d.Rule == "" {
				t.Errorf("Check(%q): allow should record which rule allowed it, got empty Rule", tc.tool)
			}
		})
	}
}
