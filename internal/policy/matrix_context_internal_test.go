package policy

// SWT-56 (docs/tickets/signal-session-name_SPEC.md) criterion 32, the map shape:
// task_context is in none of the gating maps. Internal because the maps are
// unexported (matrix_signal_internal_test.go's reason). ZERO I/O. A guard: green
// before and after, red if a later change gates the read in policy.

import "testing"

func TestPolicy_TaskContextInNoGatingMap(t *testing.T) {
	for name, m := range map[string]map[string]bool{
		"humanOnly": humanOnly, "mcpHumanOnly": mcpHumanOnly, "sendShaped": sendShaped,
		"freezeGated": freezeGated, "snapshotGated": snapshotGated,
	} {
		if m["task_context"] {
			t.Errorf("task_context is in %s; criterion 32: it is in none — worker consoles fetch it, and the user "+
				"profile's read-only-ness is the pin plus the handler flag", name)
		}
	}
}
