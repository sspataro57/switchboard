package executor_test

// SWT-37 (docs/tickets/mcp-task-verbs_SPEC.md) criterion 6, executor half:
// executor.ViaMCP and policy.ViaMCPActor are ONE spelling of "arrived over MCP"
// (V1 "Transport test spelled once"). ZERO I/O.
//
// IMPOSED SURFACE (SPEC V1):
//
//	// internal/policy/matrix.go
//	func ViaMCPActor(actor string) bool
//	// internal/executor/executor.go
//	func ViaMCP(ctx context.Context) bool { return policy.ViaMCPActor(ActorFrom(ctx)) }
//
// GREENFIELD NOTE — EXPECTED RED. policy.ViaMCPActor does not exist, so this
// package's tests compile-FAIL until internal/policy exports it.
//
// Behavioural, not a source scan: for every input the two answers must agree
// AND match the SPEC's truth table. A second spelling that drifts (a TrimSpace,
// a case fold) fails the first input where the two differ.

import (
	"context"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
)

func TestViaMCP_IsPolicyViaMCPActor(t *testing.T) {
	for _, tc := range []struct {
		actor string
		want  bool
	}{
		{"mcp:x", true},
		{"mcp:", true},
		{"x", false},
		{"MCP:x", false},
		{"", false},
		{" mcp:x", false},
	} {
		ctx := executor.WithActor(context.Background(), tc.actor)
		got, one := executor.ViaMCP(ctx), policy.ViaMCPActor(tc.actor)
		if got != one {
			t.Errorf("executor.ViaMCP(actor %q) = %v but policy.ViaMCPActor = %v: two spellings of the "+
				"transport test have drifted", tc.actor, got, one)
		}
		if got != tc.want {
			t.Errorf("executor.ViaMCP(actor %q) = %v, want %v", tc.actor, got, tc.want)
		}
	}
}
