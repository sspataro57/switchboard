package mcpserver_test

// SWT-40 Part B criterion B8's MCP half (docs/tickets/inquiry-promote_SPEC.md,
// "API / MCP tool changes": "Both humanOnly, off MCP"): route_candidate_add and
// route_candidate_remove are NOT on the agent surface in ANY profile — not
// listed, not callable. humanOnly would still refuse worker shapes, but
// `mcp:manual:*` is a human to policy, so the transport allowlist is the only
// thing keeping a prompt-injected interactive session from redirecting a
// mailbox's routing. ZERO network.
//
// GREEN TODAY, vacuously (the tools do not exist). It is here to fail the day
// someone adds them to internal/mcpserver/schemas.go — the cursor_advance_test.go
// precedent.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/mcpserver"
)

func TestMCP_RouteCandidateToolsAreOffEveryProfile(t *testing.T) {
	for _, p := range []mcpserver.Profile{mcpserver.ProfileFull, mcpserver.ProfileUser, mcpserver.ProfileRead} {
		p := p
		t.Run(string(p), func(t *testing.T) {
			listed := 0
			for _, tl := range mcpserver.NewWithProfile(&fakeExec{}, testWorkerID, p).ListTools() {
				listed++
				if strings.HasPrefix(tl.Name, "route_candidate") {
					t.Errorf("profile %s lists %q; the candidate set authorises routing into a project and is "+
						"opsctl/dashboard only (B8)", p, tl.Name)
				}
			}
			if listed == 0 {
				t.Fatalf("POSITIVE CONTROL FAILED: profile %s lists no tools at all", p)
			}
			for _, tool := range []string{"route_candidate_add", "route_candidate_remove"} {
				fx := &fakeExec{}
				srv := mcpserver.NewWithProfile(fx, testWorkerID, p)
				_, err := srv.CallTool(context.Background(), tool,
					json.RawMessage(`{"account_email":"salvador@handsonconnect.org","project":"reengine","description":"injected"}`))
				if err == nil {
					t.Errorf("CallTool(%s) on profile %s = nil error, want rejection at the MCP layer", tool, p)
				}
				if fx.called {
					t.Errorf("CallTool(%s) on profile %s reached the executor; the allowlist is the gate", tool, p)
				}
			}
		})
	}
}
