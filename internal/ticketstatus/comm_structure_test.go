package ticketstatus_test

// comms-inbox (SWT-74, docs/tickets/comms-inbox_SPEC.md) criterion 41's
// structural half: the Jira reconciler is UNTOUCHED by this ticket. No file in
// internal/ticketstatus learns `comm_task`, `comm_task_id`, `task_attach` or
// `task_match`.
//
// Its SWT-72 neighbour (activity_structure_test.go) is the template and the
// reason: the reconciler's J11 hold reads tasks.surfaced_at, and every column
// this system teaches it becomes a reason to keep a done ticket's task open
// forever. A comm task carries NO external_refs row (D4) and is not a ticket
// task at all, so the reconciler must never see it as one.
//
// ZERO I/O beyond reading this repo's own source. The behavioural half is
// internal/capture's TestCaptureComm_Integration_ADoneTicketStillClosesItsTicketTask
// (criterion 41): a Done ticket still closes its ticket task after a pass that
// created a comm task from its mail, with surfaced_at untouched.
//
// GREEN TODAY BY DESIGN, AND REQUIRED TO STAY GREEN.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTicketStatus_LearnsNothingAboutComms(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/ticketstatus: %v", err)
	}
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src := string(b)
		scanned++
		for _, banned := range []struct{ tok, why string }{
			{"comm_task", "criterion 41: arming is capture's routing DATA; the reconciler decides from stored " +
				"Jira snapshots and nothing else"},
			{"comm_task_id", "criterion 41: capture_decisions is capture's own log"},
			{"task_attach", "criterion 41: routing a comm is a human act; the reconciler closes and reopens"},
			{"task_match", "criterion 41: the reconciler asks no questions about where a message belongs"},
		} {
			if strings.Contains(src, banned.tok) {
				t.Errorf("internal/ticketstatus/%s mentions %s — %s", name, banned.tok, banned.why)
			}
		}
	}
	if scanned == 0 {
		t.Fatalf("POSITIVE CONTROL FAILED: no non-test .go file scanned in internal/ticketstatus")
	}
}
