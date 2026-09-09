package tools

// validateReopen and the ONE shared transition list — SWT-32
// (docs/tickets/jira-status-sync_SPEC.md) criteria 35 and 37. ZERO network,
// ZERO Postgres.
//
// WHY `package tools` AND NOT `package tools_test`: dismiss_test.go's reason,
// unchanged. Driving Execute with a nil pool can only assert REFUSALS — an
// accepted call runs the handler and derefs the nil pool — and half of criterion
// 35 is that the five allowed statuses are ACCEPTED. So the accept cases call
// validateReopen directly, which is also the function the SPEC names. The
// registration half (that `task_reopen` reaches Validate at all rather than
// returning "unknown tool") stays in tools_unit_test.go, where allToolNames and
// toolsUnderTest already enumerate the registry.
//
// GREENFIELD NOTE — EXPECTED RED. `validateReopen`, `reopenTask` and the shared
// status list do not exist, so this file compile-FAILs internal/tools until
// close.go declares them.
//
// IMPOSED SURFACE (the SPEC fixes the tool's JSON contract —
// `{task_id: int, status?: string, reason: string}` -> `{task_id, status,
// reopened}` — and names reopenArgs / validateReopen / reopenTask; the Go
// spelling of the shared list is this file's, and it is the smallest thing that
// can be shared by two verbs):
//
//	type reopenArgs struct {
//	    TaskID int64  `json:"task_id"`
//	    Status string `json:"status,omitempty"`
//	    Reason string `json:"reason"`
//	}
//	func validateReopen(args []byte) error
//
//	// openStatuses is D6's set, spelled ONCE: exactly the statuses task_close
//	// accepts as a SOURCE and therefore exactly the ones task_reopen accepts as
//	// a TARGET. closeTransition and reopenTransition both read it; a second
//	// spelling is how the two verbs drift.
//	var openStatuses = []string{"holding", "ready", "blocked", "done_locally", "delivered"}

import (
	"os"
	"strings"
	"testing"
)

// ---- criterion 35: validation --------------------------------------------------

// "validateReopen rejects {}, a missing/zero task_id, an empty reason, and any
// status outside the allowed set (by name, both ways)."
//
// The reason is REQUIRED for the same purpose it is on task_close: the
// status_changed event carries it, and this verb's callers are the reconciler
// (which composes prose from the ticket key and its status) and a human undoing
// a mis-click. A reopen with no reason is a task that came back from the dead
// with no explanation on its own page.
func TestValidateReopen_RejectsIncompleteArgs(t *testing.T) {
	for _, tc := range []struct {
		name, args, wantIn string
	}{
		{"empty object", `{}`, "task_id"},
		{"zero task_id", `{"task_id":0,"reason":"ticket reopened"}`, "task_id"},
		{"negative task_id", `{"task_id":-3,"reason":"ticket reopened"}`, "task_id"},
		{"missing task_id", `{"reason":"ticket reopened"}`, "task_id"},
		{"empty reason", `{"task_id":7,"reason":""}`, "reason"},
		{"missing reason", `{"task_id":7}`, "reason"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := validateReopen([]byte(tc.args))
			if err == nil {
				t.Fatalf("validateReopen(%s) = nil, want a validation failure", tc.args)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("validateReopen(%s) = %q, want a message naming %q — a validation error that "+
					"does not say which field is missing is a support ticket", tc.args, err, tc.wantIn)
			}
		})
	}
}

// "...and any status outside the allowed set (by name, both ways)."
//
// Both ways = the message quotes the offending value AND the allowed set. The
// refusal matters more here than it looks: `tasks.status` has a CHECK listing
// twelve values, five of which are restorable. A reopen to `claimed` would put a
// task back into a claimed state with no task_claims row and no worker — a
// shape nothing else in the spine can produce.
func TestValidateReopen_StatusMustBeInTheSharedAllowedSet(t *testing.T) {
	t.Run("every allowed status is accepted", func(t *testing.T) {
		for _, status := range openStatuses {
			args := `{"task_id":7,"status":"` + status + `","reason":"ticket left Done"}`
			if err := validateReopen([]byte(args)); err != nil {
				t.Errorf("validateReopen(%s) = %v, want nil. D6: the allowed target set is exactly the "+
					"set task_close accepts as a SOURCE, so a task can always be restored to the "+
					"status it was closed from", args, err)
			}
		}
	})

	t.Run("status is optional", func(t *testing.T) {
		if err := validateReopen([]byte(`{"task_id":7,"reason":"ticket left Done"}`)); err != nil {
			t.Errorf("validateReopen with no status = %v, want nil — the SPEC's contract makes status "+
				"optional (`status?`), and the fall-back is `ready` (D6)", err)
		}
	})

	for _, tc := range []struct{ name, status string }{
		{"an active status", "claimed"},
		{"in_progress", "in_progress"},
		{"needs_feedback", "needs_feedback"},
		{"closed itself", "closed"},
		{"a PR lifecycle status", "pr_open"},
		{"not a status at all", "banana"},
		{"empty-but-present is not the same as absent", " "},
	} {
		tc := tc
		t.Run("rejects "+tc.name, func(t *testing.T) {
			args := `{"task_id":7,"status":"` + tc.status + `","reason":"r"}`
			err := validateReopen([]byte(args))
			if err == nil {
				t.Fatalf("validateReopen(%s) = nil, want a refusal. Reopening to %q produces a task "+
					"state nothing else in the spine can produce — a claimed task with no claim, or a "+
					"pr_open task with no PR", args, tc.status)
			}
			if !strings.Contains(err.Error(), tc.status) {
				t.Errorf("validateReopen(%s) = %q, which does not quote the offending value", args, err)
			}
			for _, allowed := range openStatuses {
				if !strings.Contains(err.Error(), allowed) {
					t.Errorf("validateReopen(%s) = %q, which does not list the allowed status %q. "+
						"'By name, both ways' (criterion 35): the caller is usually a script, and the "+
						"allowed set is the one thing it cannot guess", args, err, allowed)
					break
				}
			}
		})
	}
}

// ---- criterion 37: ONE transition helper, ONE allowed-status list ------------

// "closeTask and reopenTask share ONE unexported transition helper and ONE
// allowed-status list in internal/tools/close.go. Dropping the list from one
// verb must fail a test."
//
// The behavioural half is above (validateReopen consults openStatuses, and its
// message enumerates it) and in the integration suite (closeTask still refuses
// active work in the same words). This half is the one that catches the drift
// SWT-31 already had to guard against: two spellings of a status list stay
// identical for exactly as long as nobody edits one of them, and the divergence
// shows up as a verb that accepts a transition its sibling refuses.
func TestReopen_SharesOneStatusListWithClose(t *testing.T) {
	if len(openStatuses) != 5 {
		t.Errorf("openStatuses = %v, want the five statuses task_close accepts as a source "+
			"(holding, ready, blocked, done_locally, delivered). D6: spelled ONCE and shared by both "+
			"verbs — a second list is how the two drift", openStatuses)
	}
	for _, want := range []string{"holding", "ready", "blocked", "done_locally", "delivered"} {
		found := false
		for _, got := range openStatuses {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("openStatuses (%v) does not contain %q — a task closed from %q could then never "+
				"be restored to it, and D6's whole point is not flattening every restored task to "+
				"ready", openStatuses, want, want)
		}
	}
	// And the active-work statuses are NOT in it, in either direction: those are
	// the three task_close refuses, and a reopen target of `claimed` is the
	// mirror-image mistake.
	for _, banned := range []string{"claimed", "in_progress", "needs_feedback", "closed"} {
		for _, got := range openStatuses {
			if got == banned {
				t.Errorf("openStatuses contains %q. The set is exactly the SOURCE set of task_close "+
					"(fact 11: 'never close work out from under a holder'); if the two sets are not "+
					"the same set, they are not one list", banned)
			}
		}
	}
}

// The source half of criterion 37, complementing
// dismiss_structure_test.go's refusal-count scan: ONE lock, ONE event write.
//
// A generalised helper is the only shape that satisfies both verbs — `SELECT ...
// FOR UPDATE`, the refusal, the UPDATE and the status_changed event, with the
// target status as a parameter. If reopenTask grows its own copy, close.go ends
// up with two lock spellings and two event writers that agree until one is
// edited, which is precisely how a "reopen" that skipped the row lock would ship
// a lost update between the pass and a dashboard dismiss.
func TestReopen_SharesOneTransitionHelperWithClose(t *testing.T) {
	b, err := os.ReadFile("close.go")
	if err != nil {
		t.Fatalf("read internal/tools/close.go: %v", err)
	}
	src := string(b)

	if !strings.Contains(src, "func reopenTask(") {
		t.Fatalf("internal/tools/close.go does not declare reopenTask; the SPEC puts it HERE, beside " +
			"closeTask, precisely so the shared helper is unavoidable")
	}
	if n := strings.Count(src, "FOR UPDATE"); n != 1 {
		t.Errorf("close.go takes a row lock in %d places, want 1. Criterion 37: closeTask, dismissTask "+
			"and reopenTask share ONE transition helper — three verbs locking the same row three "+
			"different ways is how one of them stops locking it", n)
	}
	if n := strings.Count(src, `"status_changed"`); n != 1 {
		t.Errorf("close.go writes a status_changed event from %d places, want 1. The payload contract "+
			"({from,to,reason}, no fourth key — criterion 38) has to be written once or the "+
			"orchestrator's drain sees two shapes", n)
	}
}
