package mcpserver_test

// SWT-41 (docs/tickets/orchestrator-deploy_SPEC.md) criterion 3's MCP half:
// `orchestrator_cursor_advance` is NOT on the agent surface in ANY profile —
// not listed, not callable. It discards lifecycle events; it is reached only
// via `opsctl call`. Asserted deliberately rather than by omission (the
// SWT-20 / SWT-32 precedent in adapter_test.go's spineTools), in a separate
// file so the pinned allowlist tests stay untouched. ZERO network.
//
// GREEN TODAY, vacuously (the tool does not exist). It is here to fail the day
// someone adds it to internal/mcpserver/schemas.go, the only moment it could be
// wrong. humanOnly (internal/policy) would still refuse worker shapes, but
// `mcp:manual:*` is a human to policy, so the transport allowlist is the only
// thing keeping a prompt-injected interactive session off this verb.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/mcpserver"
)

func TestMCP_OrchestratorCursorAdvanceIsOffEveryProfile(t *testing.T) {
	const banned = "orchestrator_cursor_advance"
	for _, p := range []mcpserver.Profile{mcpserver.ProfileFull, mcpserver.ProfileUser, mcpserver.ProfileRead} {
		p := p
		t.Run(string(p), func(t *testing.T) {
			listed := 0
			for _, tl := range mcpserver.NewWithProfile(&fakeExec{}, testWorkerID, p).ListTools() {
				listed++
				if tl.Name == banned || strings.HasPrefix(tl.Name, "orchestrator_") {
					t.Errorf("profile %s lists %q; moving the orchestrator cursor discards lifecycle events and "+
						"is opsctl-only (SWT-41 D1: NOT in schemas.go)", p, tl.Name)
				}
			}
			if listed == 0 {
				t.Fatalf("POSITIVE CONTROL FAILED: profile %s lists no tools at all", p)
			}

			fx := &fakeExec{}
			srv := mcpserver.NewWithProfile(fx, testWorkerID, p)
			_, err := srv.CallTool(context.Background(), banned,
				json.RawMessage(`{"expect_last_event_id":0,"reason":"injected"}`))
			if err == nil {
				t.Errorf("CallTool(%s) on profile %s = nil error, want rejection at the MCP layer", banned, p)
			}
			if fx.called {
				t.Errorf("CallTool(%s) on profile %s reached the executor; the allowlist is the gate", banned, p)
			}
		})
	}
}
