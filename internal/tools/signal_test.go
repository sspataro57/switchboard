package tools

// SWT-52 (docs/tickets/board-status-lights_SPEC.md) criteria 6, 18 (validation)
// and D11: task_signal's validator, its one state set, the working lease, and
// the exported queue-order alias. ZERO network, ZERO Postgres.
//
// WHY `package tools`: priority_test.go's reason — an ACCEPTED call through
// Execute with a nil pool would run the handler and deref it, so the validator
// is called directly. The registration half is tools_unit_test.go
// (allToolNames, toolsUnderTest).
//
// IMPOSED SURFACE (SPEC criteria 6, 18, 23, D11; SignalStates is this file's
// name for the handler's set that criterion 23 pins the MCP enum to — the
// DismissReasonCodes precedent):
//
//	// internal/tools/signal.go (new)
//	const WorkingLease = 2 * time.Hour
//	type signalArgs struct {
//	    TaskID   int64  `json:"task_id"`
//	    State    string `json:"state"`
//	    WorkerID string `json:"worker_id,omitempty"`
//	}
//	func validateSignal(args []byte) error
//	func SignalStates() []string // a COPY of {"working","needs_input","clear"}, in that order
//	// Register gains {"task_signal", validateSignal, signalTask}.
//
//	// internal/tools/getnext.go
//	const TaskQueueOrder = taskQueueOrder
//
// GREENFIELD NOTE — EXPECTED RED: validateSignal, SignalStates, WorkingLease and
// TaskQueueOrder do not exist, so package tools' test binary compile-FAILS.

import (
	"go/ast"
	"strings"
	"testing"
	"time"
)

func TestValidateSignal_RefusesIncompleteArgs(t *testing.T) {
	for _, tc := range []struct{ name, args, wantIn string }{
		{"empty object", `{}`, "task_id"},
		{"missing task_id", `{"state":"working"}`, "task_id"},
		{"zero task_id", `{"task_id":0,"state":"working"}`, "task_id"},
		{"missing state", `{"task_id":412}`, "state"},
		{"empty state", `{"task_id":412,"state":""}`, "state"},
		{"not an object", `[]`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSignal([]byte(tc.args))
			if err == nil {
				t.Fatalf("validateSignal(%s) = nil, want a refusal (criterion 18)", tc.args)
			}
			if tc.wantIn != "" && !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("validateSignal(%s) = %q, which does not name %q", tc.args, err, tc.wantIn)
			}
		})
	}
}

// Criterion 18: a state outside the set is refused BY NAME — the value and the
// allowed set both appear.
func TestValidateSignal_RefusesUnknownStateByName(t *testing.T) {
	for _, bad := range []string{"done", "WORKING", "waiting", "needs-input", "closed"} {
		err := validateSignal([]byte(`{"task_id":412,"state":"` + bad + `"}`))
		if err == nil {
			t.Errorf("validateSignal(state=%q) = nil; D7: state ∈ working | needs_input | clear", bad)
			continue
		}
		for _, want := range []string{`"` + bad + `"`, "working", "needs_input", "clear"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("validateSignal(state=%q) = %q, want it to name %s (the value and the allowed set)", bad, err, want)
			}
		}
	}
}

// AMENDED — not deleted — by SWT-56 (signal-session-name) criterion 3: working and
// needs_input now carry "session":"kube-c7" (S2: required on the two setting
// states); clear still needs none. The new case below pins the refusal.
func TestValidateSignal_AcceptsTheThreeStates(t *testing.T) {
	for _, st := range []string{"working", "needs_input", "clear"} {
		sess := `,"session":"kube-c7"`
		if st == "clear" {
			sess = ""
		}
		for _, args := range []string{
			`{"task_id":412,"state":"` + st + `"` + sess + `}`,
			// what the MCP adapter forwards: worker_id injected, never authority
			`{"task_id":412,"state":"` + st + `","worker_id":"manual:salvo"` + sess + `}`,
		} {
			if err := validateSignal([]byte(args)); err != nil {
				t.Errorf("validateSignal(%s) = %v, want nil", args, err)
			}
		}
	}
	// SWT-56 criterion 3: the missing-session refusal.
	for _, st := range []string{"working", "needs_input"} {
		args := `{"task_id":412,"state":"` + st + `","worker_id":"manual:salvo"}`
		if err := validateSignal([]byte(args)); err == nil || !strings.Contains(err.Error(), "missing session") {
			t.Errorf("validateSignal(%s) = %v, want the S3 missing-session refusal", args, err)
		}
	}
}

// Criterion 23 pins the MCP enum to this set; it must be the one the
// validator uses, and callers must not be able to rewrite it.
func TestSignalStates_TheOneSet(t *testing.T) {
	got := SignalStates()
	if strings.Join(got, ",") != "working,needs_input,clear" {
		t.Errorf("SignalStates() = %v, want [working needs_input clear] (D7)", got)
	}
	got[0] = "hacked"
	if SignalStates()[0] != "working" {
		t.Errorf("SignalStates() returns the validator's own slice; return a copy (the DismissReasonCodes precedent)")
	}
	if err := validateSignal([]byte(`{"task_id":1,"state":"hacked"}`)); err == nil {
		t.Errorf("validateSignal accepted a state injected through SignalStates()' return value")
	}
}

// D11: one lease, spelled once in signal.go, equal to ClaimTTL but separate.
func TestWorkingLease_SpelledOnceInSignalGo(t *testing.T) {
	if WorkingLease != 2*time.Hour {
		t.Errorf("WorkingLease = %v, want 2h (D11)", WorkingLease)
	}
	src := parseToolsSource(t)
	if got := src.valueFile["WorkingLease"]; got != "signal.go" {
		t.Errorf("WorkingLease is declared in %q, want signal.go (D11)", got)
	}
	if vs, ok := src.values["WorkingLease"]; ok {
		ast.Inspect(vs, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "ClaimTTL" {
				t.Errorf("WorkingLease is defined from ClaimTTL. D11: a separate constant because it is a separate " +
					"lease — changing the claim TTL must not move the board's staleness")
			}
			return true
		})
	}
}

// Criterion 6: an EXPORTED alias, not a second literal.
func TestTaskQueueOrder_ExportedAlias(t *testing.T) {
	if TaskQueueOrder != taskQueueOrder {
		t.Errorf("TaskQueueOrder = %q, want exactly taskQueueOrder %q", TaskQueueOrder, taskQueueOrder)
	}
	src := parseToolsSource(t)
	if got := src.valueFile["TaskQueueOrder"]; got != "getnext.go" {
		t.Errorf("TaskQueueOrder is declared in %q, want getnext.go (criterion 6)", got)
	}
	vs, ok := src.values["TaskQueueOrder"]
	if !ok || len(vs.Values) != 1 {
		t.Fatalf("TaskQueueOrder has no single value")
	}
	if id, ok := vs.Values[0].(*ast.Ident); !ok || id.Name != "taskQueueOrder" {
		t.Errorf("TaskQueueOrder is not `= taskQueueOrder`; criterion 6: an alias, so the ordering literal still " +
			"occurs exactly once (TestTaskList_OrderingSpelledOnce)")
	}
}
