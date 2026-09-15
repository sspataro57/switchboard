package policy_test

// SWT-56 (docs/tickets/signal-session-name_SPEC.md) criterion 32: task_context
// joins the user profile with NO policy change. Through the real matrix
// (mcpVerbsChecker: the real registry's static allow-list, a loader that fails
// the test if it runs) it stays allow/static-default for Salvador's sessions AND
// for worker consoles, which need it to start work. ZERO I/O (invariant 7).
//
// A GUARD, green before and after: the SPEC forbids an internal/policy
// production change, so this pins that the read-only-ness lives in the pin and
// the handler (criteria 28-30), not in a policy rule that would also cut off
// worker consoles. The map half is matrix_context_internal_test.go.

import (
	"context"
	"testing"

	"github.com/sspataro57/switchboard/internal/policy"
)

func TestMatrix_TaskContext_StaysStaticDefaultAllow(t *testing.T) {
	checker := mcpVerbsChecker(t)
	for _, actor := range []string{"mcp:manual:salvo", "mcp:acme", "mcp:acme.web"} {
		d, err := checker.Check(context.Background(), policy.Request{Tool: "task_context", Actor: actor})
		if err != nil {
			t.Fatalf("Check(task_context, %q): %v", actor, err)
		}
		if d.Decision != "allow" || d.Rule != "static-default" {
			t.Errorf("Check(task_context, %q) = %s/%s (%s), want allow/static-default (criterion 32: no policy change; "+
				"worker consoles need it)", actor, d.Decision, d.Rule, d.Reason)
		}
	}
}
