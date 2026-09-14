package policy

// SWT-52 (docs/tickets/board-status-lights_SPEC.md) criterion 21, the map
// shape. Internal because the maps are unexported (mcpverbs_internal_test.go's
// reason). ZERO I/O. EXPECTED RED until matrix.go's humanOnly gains task_signal.

import (
	"sort"
	"strings"
	"testing"
)

func TestPolicy_TaskSignalShape(t *testing.T) {
	if !humanOnly["task_signal"] {
		t.Errorf("humanOnly lacks task_signal (criterion 21 / D7)")
	}
	for name, m := range map[string]map[string]bool{
		"mcpHumanOnly": mcpHumanOnly, "sendShaped": sendShaped, "freezeGated": freezeGated, "snapshotGated": snapshotGated,
	} {
		if m["task_signal"] {
			t.Errorf("task_signal is in %s; criterion 21: humanOnly only — it sends nothing and no spine caller uses it", name)
		}
	}
	var got []string
	for tool, on := range mcpHumanOnly {
		if on {
			got = append(got, tool)
		}
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "task_close,task_mark_delivered" {
		t.Errorf("mcpHumanOnly = %v, want exactly [task_close task_mark_delivered] (criterion 21)", got)
	}
}
