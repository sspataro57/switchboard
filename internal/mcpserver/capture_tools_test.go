package mcpserver_test

// Unit test for SWT-17 acceptance criterion 15's MCP half: the two rule-management
// tools are NOT listed on the agent surface. "An agent must not be able to
// redirect the funnel" — and unlike mark_delivery_sent (listed but policy-gated),
// these are not listed at all, so there is nothing for a model to attempt.
// ZERO network.
//
// This test PASSES today, vacuously — the tools do not exist. It is here to fail
// the day someone adds them to internal/mcpserver/schemas.go, which is the only
// moment it could ever be wrong.

import (
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/mcpserver"
)

func TestListTools_ExcludesCaptureRuleTools(t *testing.T) {
	srv := mcpserver.New(&fakeExec{}, testWorkerID)
	for _, tl := range srv.ListTools() {
		for _, banned := range []string{"capture_rule_add", "capture_rule_set_enabled"} {
			if tl.Name == banned {
				t.Errorf("%q is MCP-listed; routing rules decide which project a message becomes work for, so "+
					"the tools that change them are dashboard:/opsctl:/manual: only and are not on the agent "+
					"surface at all (criterion 15)", banned)
			}
		}
		if strings.HasPrefix(tl.Name, "capture_") {
			t.Errorf("unexpected MCP-listed capture tool %q; SWT-17 adds no agent-facing surface", tl.Name)
		}
	}
}
