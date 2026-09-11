package tools

// validateDismiss — SWT-31 (docs/tickets/board-dismissals_SPEC.md) criterion 8.
// ZERO network, ZERO Postgres.
//
// WHY `package tools` AND NOT `package tools_test`: the same reason
// createtask_status_test.go gives. Driving Execute with a nil pool can only
// assert REFUSALS — an accepted call runs the handler and panics on the nil
// pool — and half of criterion 8 is that the four enum values are ACCEPTED. So
// the accept cases call validateDismiss directly, which is also the function the
// SPEC names. The registration half (that `task_dismiss` reaches Validate at all
// rather than returning "unknown tool") stays in tools_unit_test.go, where
// allToolNames and toolsUnderTest already enumerate the registry.
//
// GREENFIELD NOTE — EXPECTED RED. `validateDismiss` does not exist, so this file
// compile-FAILs internal/tools until internal/tools/close.go declares it.
//
// IMPOSED SURFACE (SPEC "API / MCP tool changes"):
//
//	type dismissArgs struct {
//	    TaskID     int64  `json:"task_id"`
//	    ReasonCode string `json:"reason_code"`
//	    Note       string `json:"note,omitempty"`
//	}
//	func validateDismiss(args []byte) error

import (
	"encoding/json"
	"strings"
	"testing"
)

// D4's enum, in one place in this file so a widening shows up as a diff here and
// in migration 0022's CHECK together — never in only one of them.
var dismissReasonCodes = []string{"not_actionable", "wrong_kind", "duplicate", "handled_elsewhere"}

// "validateDismiss rejects {}, a missing/zero task_id, an empty reason_code".
//
// `{}` is the shape every other tool in this repo is asserted against, and it is
// not academic here: a dismissal with no task id that defaulted to anything at
// all would close a task nobody named, and the row it writes is training data.
func TestValidateDismiss_RejectsIncompleteArgs(t *testing.T) {
	for _, tc := range []struct {
		name, args, wantIn string
	}{
		{"empty object", `{}`, "task_id"},
		{"zero task_id", `{"task_id":0,"reason_code":"duplicate"}`, "task_id"},
		{"missing task_id", `{"reason_code":"duplicate"}`, "task_id"},
		{"empty reason_code", `{"task_id":7,"reason_code":""}`, "reason_code"},
		{"missing reason_code", `{"task_id":7}`, "reason_code"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := validateDismiss([]byte(tc.args))
			if err == nil {
				t.Fatalf("validateDismiss(%s) = nil, want a validation failure. The label is the whole "+
					"point of the verb (D2) — a dismissal with no reason is a close, and task_close "+
					"already exists", tc.args)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("validateDismiss(%s) failed with %q, which does not name %q; an error that does "+
					"not name the field sends the reader to the wrong one", tc.args, err, tc.wantIn)
			}
		})
	}
}

// "any reason_code outside the enum (by name, both ways — the create_task
// `status` precedent)". BY NAME, BOTH WAYS means the error quotes the offending
// value AND names the allowed set: a code that silently fell back to
// `not_actionable` would mint mislabelled training data, which is worse than
// refusing, because nothing downstream can tell the difference later.
func TestValidateDismiss_RejectsAnyCodeOutsideTheEnumByName(t *testing.T) {
	for _, code := range []string{
		// Plausible neighbours: the shapes a caller sends by accident.
		"not-actionable", "NOT_ACTIONABLE", "Duplicate", "dupe", "spam", "noise",
		"wontfix", "handled", "other",
		// And a task status, because the board's other verb is a close.
		"closed",
	} {
		code := code
		t.Run(code, func(t *testing.T) {
			args := []byte(`{"task_id":7,"reason_code":"` + code + `"}`)
			err := validateDismiss(args)
			if err == nil {
				t.Fatalf("validateDismiss(reason_code=%q) = nil; D4: the enum is a CHECK constraint with "+
					"exactly four values, and a free-text reason_code is a column nothing can GROUP BY", code)
			}
			msg := err.Error()
			if !strings.Contains(msg, code) {
				t.Errorf("validateDismiss(reason_code=%q) failed with %q, which does not quote the "+
					"offending value", code, msg)
			}
			for _, want := range dismissReasonCodes {
				if !strings.Contains(msg, want) {
					t.Errorf("validateDismiss(reason_code=%q) failed with %q, which does not name %q as "+
						"an allowed value; the error is the only place the enum is written down for a "+
						"caller (the dashboard's <select> is the other, and it will drift)", code, msg, want)
				}
			}
		})
	}
}

// The four values are ACCEPTED — the half a nil-pool Execute test cannot express.
// If the validator and migration 0022's CHECK ever disagree, the disagreement
// surfaces here or as a runtime constraint violation on a human's click; this is
// the cheaper of the two.
func TestValidateDismiss_AcceptsTheFourReasonCodes(t *testing.T) {
	for _, code := range dismissReasonCodes {
		code := code
		t.Run(code, func(t *testing.T) {
			if err := validateDismiss([]byte(`{"task_id":7,"reason_code":"` + code + `"}`)); err != nil {
				t.Fatalf("validateDismiss(reason_code=%q) = %v, want nil — these four are Salvador's "+
					"own values and are what migration 0022's CHECK allows", code, err)
			}
		})
	}
}

// `note` is OPTIONAL free text (D4) and must not be validated into a second
// required field: the board form submits an empty input when the human just
// wants the code.
func TestValidateDismiss_NoteIsOptionalAndFree(t *testing.T) {
	for _, args := range []string{
		`{"task_id":7,"reason_code":"duplicate"}`,
		`{"task_id":7,"reason_code":"duplicate","note":""}`,
		`{"task_id":7,"reason_code":"duplicate","note":"duplicate of the Tuesday thread"}`,
	} {
		if err := validateDismiss([]byte(args)); err != nil {
			t.Errorf("validateDismiss(%s) = %v, want nil (note is optional free text)", args, err)
		}
	}
}

// Sanity, the createtask_status_test.go precedent: the arguments are decoded
// from JSON at all. Without this, a json-tag typo would make every rejection
// above pass for the wrong reason — the value would never reach the validator —
// and the accept cases would be the only thing that noticed.
func TestValidateDismiss_ReadsTheArgumentsAsTheExecutorSendsThem(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"task_id": 7, "reason_code": "not_actionable", "note": "a note",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := validateDismiss(raw); err != nil {
		t.Fatalf("validateDismiss on marshalled args = %v, want nil. The dashboard marshals a map, not a "+
			"struct literal, so the json tags are the contract", err)
	}
	// A task_id sent as a JSON number string (the dashboard's json.Number shape
	// for path ids, jsonNum in internal/dashboard/server.go) must not be
	// silently read as zero.
	if err := validateDismiss([]byte(`{"task_id":7,"reason_code":"not_actionable"}`)); err != nil {
		t.Fatalf("validateDismiss with a numeric task_id = %v, want nil", err)
	}
}

// SWT-37 (docs/tickets/mcp-task-verbs_SPEC.md) criterion 19: the exported
// DismissReasonCodes is the ONE source of task_dismiss's MCP schema enum
// (internal/mcpserver TestTaskVerbSchemas asserts set-equality against it).
//
// IMPOSED SURFACE (SPEC V5):
//
//	func DismissReasonCodes() []string // a COPY of dismissCodes, in dismissCodes order
//
// GREENFIELD NOTE — EXPECTED RED. DismissReasonCodes does not exist, so
// internal/tools's tests compile-FAIL until close.go declares it.
//
// A COPY, because a caller that appended to or overwrote the returned slice
// would otherwise rewrite the validator's allowed set, and the error message
// that is the enum's only written-down form for a caller.
func TestDismissReasonCodes_ReturnsACopyInOrder(t *testing.T) {
	got := DismissReasonCodes()
	if strings.Join(got, ",") != strings.Join(dismissReasonCodes, ",") {
		t.Fatalf("DismissReasonCodes() = %v, want %v in dismissCodes order (migration 0022's CHECK)", got, dismissReasonCodes)
	}

	for i := range got {
		got[i] = "mutated"
	}
	if err := validateDismiss([]byte(`{"task_id":7,"reason_code":"not_actionable"}`)); err != nil {
		t.Errorf("after mutating DismissReasonCodes()'s result, validateDismiss(not_actionable) = %v: the "+
			"function returned dismissCodes itself, not a copy", err)
	}
	if err := validateDismiss([]byte(`{"task_id":7,"reason_code":"mutated"}`)); err == nil {
		t.Errorf("after mutating DismissReasonCodes()'s result, validateDismiss accepts reason_code \"mutated\": " +
			"the caller rewrote the validator's allowed set")
	}
	if again := DismissReasonCodes(); strings.Join(again, ",") != strings.Join(dismissReasonCodes, ",") {
		t.Errorf("a second DismissReasonCodes() = %v after the first result was mutated, want %v", again, dismissReasonCodes)
	}
}
