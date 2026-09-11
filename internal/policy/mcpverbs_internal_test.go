package policy

// SWT-37 (docs/tickets/mcp-task-verbs_SPEC.md) criterion 5: the SHAPE of the
// new rule's map. Internal (package policy) because the maps are unexported.
// ZERO I/O.
//
// IMPOSED SURFACE (SPEC V1):
//
//	var mcpHumanOnly = map[string]bool{"task_close": true, "task_mark_delivered": true}
//
// GREENFIELD NOTE — EXPECTED RED. mcpHumanOnly does not exist, so package
// policy's tests compile-FAIL until matrix.go declares it.
//
// WHY THE MAPS MUST STAY APART (V1 "Why not humanOnly", mutation M-d):
// task_close and task_mark_delivered are called outside MCP by the orchestrator
// (R2 closes the feedback-answer task, R8 marks work delivered and closes its
// Deliver task) and by the Jira reconciler (ticketstatus:jira closes dropped
// tickets' tasks). Folding either into humanOnly would stall those rules on a
// denial that reads like a permissions bug. And neither transmits anything, so
// neither belongs in sendShaped (the loader, the rate limit, the channel
// branch) or freezeGated (the kill switch).

import (
	"sort"
	"strings"
	"testing"
)

func TestPolicy_MCPHumanOnlyShape(t *testing.T) {
	var got []string
	for tool, on := range mcpHumanOnly {
		if on {
			got = append(got, tool)
		}
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "task_close,task_mark_delivered" {
		t.Errorf("mcpHumanOnly = %v, want exactly [task_close task_mark_delivered] (V1). task_dismiss "+
			"stays in humanOnly (V2); task_reopen is not on MCP at all", got)
	}

	for tool := range mcpHumanOnly {
		if humanOnly[tool] {
			t.Errorf("%q is in BOTH mcpHumanOnly and humanOnly: the maps must be disjoint. humanOnly means "+
				"every actor must be human, which would refuse the orchestrator and the reconciler", tool)
		}
	}

	for _, tool := range []string{"task_close", "task_mark_delivered"} {
		for _, m := range []struct {
			name string
			set  map[string]bool
		}{
			{"humanOnly", humanOnly},
			{"sendShaped", sendShaped},
			{"freezeGated", freezeGated},
			{"snapshotGated", snapshotGated},
		} {
			if m.set[tool] {
				t.Errorf("%s is in %s. It must not be: the orchestrator and the reconciler call it (humanOnly), "+
					"and it sends nothing (sendShaped / freezeGated / snapshotGated)", tool, m.name)
			}
		}
	}

	if !humanOnly["task_dismiss"] {
		t.Errorf("task_dismiss left humanOnly: V2 keeps it there, and since this ticket MCP-lists it in the " +
			"full profile, humanOnly is the ONLY thing that refuses a worker console")
	}
}
