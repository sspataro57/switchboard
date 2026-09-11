package tools

// validateReopen's dismissal guard — SWT-36
// (docs/tickets/dismiss-reopen-on-activity_SPEC.md) criterion 1. ZERO network,
// ZERO Postgres.
//
// `package tools` for reopen_test.go's reason, unchanged: the accept cases can
// only be expressed against validateReopen directly (a nil-pool Execute that
// validates runs the handler and derefs the pool).
//
// IMPOSED SURFACE. The JSON names are the SPEC's ("API / MCP tool changes"):
//
//	args: {task_id, reason, status?, dismissal_id?, message_id?}
//	      dismissal_id and message_id: both or neither, both > 0;
//	      status forbidden with them (the dismissal decides the target, D5)
//
// The Go spelling below is this file's suggestion; nothing here names the
// fields in Go, ON PURPOSE — the cases drive JSON, so this file compiles today
// and every other test in the package keeps running:
//
//	type reopenArgs struct {
//	    TaskID      int64  `json:"task_id"`
//	    Status      string `json:"status,omitempty"`
//	    Reason      string `json:"reason"`
//	    DismissalID int64  `json:"dismissal_id,omitempty"`
//	    MessageID   int64  `json:"message_id,omitempty"`
//	}
//
// RED TODAY FOR THE RIGHT REASON: reopenArgs has no dismissal_id/message_id,
// json.Unmarshal silently drops unknown keys, so every refusal below returns
// nil — which is exactly the failure a guard that was never wired up would
// have in production (a caller's dismissal_id ignored, an unguarded reopen
// performed instead).

import (
	"strings"
	"testing"
)

// "It refuses: message_id without dismissal_id; dismissal_id without
// message_id; status together with dismissal_id. Each refusal names the
// offending field."
//
// The pair refusal must name BOTH fields: "both or neither" is the rule, and a
// message naming only one of them sends a script author to add the wrong key.
func TestValidateReopen_DismissalGuardIsBothOrNeither(t *testing.T) {
	for _, tc := range []struct {
		name, args string
		wantIn     []string
	}{
		{"message_id without dismissal_id",
			`{"task_id":7,"reason":"new inbound","message_id":9}`,
			[]string{"message_id", "dismissal_id"}},
		{"dismissal_id without message_id",
			`{"task_id":7,"reason":"new inbound","dismissal_id":3}`,
			[]string{"dismissal_id", "message_id"}},
		// Zero is indistinguishable from absent in an int64, so it reads as the
		// pair refusal — which is still a refusal naming the field.
		{"zero dismissal_id with a message_id",
			`{"task_id":7,"reason":"new inbound","dismissal_id":0,"message_id":9}`,
			[]string{"dismissal_id"}},
		{"negative dismissal_id",
			`{"task_id":7,"reason":"new inbound","dismissal_id":-3,"message_id":9}`,
			[]string{"dismissal_id"}},
		{"negative message_id",
			`{"task_id":7,"reason":"new inbound","dismissal_id":3,"message_id":-9}`,
			[]string{"message_id"}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := validateReopen([]byte(tc.args))
			if err == nil {
				t.Fatalf("validateReopen(%s) = nil, want a refusal. Criterion 1: the guard is both-or-neither. "+
					"A half-guarded call that validated would run as an UNGUARDED reopen — the handler would "+
					"skip the dismissal check and the ingest-time comparison entirely (D4)", tc.args)
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("validateReopen(%s) = %q, which does not name %q. Criterion 1: each refusal "+
						"names the offending field", tc.args, err, want)
				}
			}
		})
	}
}

// "status together with dismissal_id" — D5: the DISMISSAL decides the target
// (closed_from_status, falling back to ready). A caller-supplied status beside
// it would let a promote pass lift a review-lane task straight to `ready`, the
// autonomy widening D5 exists to refuse.
func TestValidateReopen_StatusIsForbiddenWithADismissal(t *testing.T) {
	for _, status := range []string{"ready", "holding", "delivered"} {
		args := `{"task_id":7,"reason":"new inbound","status":"` + status + `","dismissal_id":3,"message_id":9}`
		err := validateReopen([]byte(args))
		if err == nil {
			t.Errorf("validateReopen(%s) = nil, want a refusal. D5: a guarded reopen restores "+
				"closed_from_status; a status argument next to dismissal_id is two answers to one question", args)
			continue
		}
		for _, want := range []string{"status", "dismissal_id"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("validateReopen(%s) = %q, which does not name %q", args, err, want)
			}
		}
	}
}

// The accept half. A guarded call with both ids and no status is legal, and so
// is every SWT-32 shape (no ids at all) — criterion 4's "otherwise
// byte-identical". This half is GREEN today (unknown keys are ignored) and must
// stay green: a validator that refused the guarded call would make the whole
// feature unreachable, with every refusal test above passing.
func TestValidateReopen_AcceptsTheGuardedAndTheUnguardedShapes(t *testing.T) {
	for _, args := range []string{
		`{"task_id":7,"reason":"new inbound on the thread","dismissal_id":3,"message_id":9}`,
		`{"task_id":7,"reason":"ITS-1 left Done"}`,
		`{"task_id":7,"reason":"ITS-1 left Done","status":"delivered"}`,
	} {
		if err := validateReopen([]byte(args)); err != nil {
			t.Errorf("validateReopen(%s) = %v, want nil", args, err)
		}
	}
}

// The SWT-32 refusals still fire FIRST on a guarded call: a guarded call is
// still a task_reopen, and a missing task_id or reason is still a caller bug.
func TestValidateReopen_GuardedCallStillNeedsTaskIDAndReason(t *testing.T) {
	for _, tc := range []struct{ args, wantIn string }{
		{`{"reason":"r","dismissal_id":3,"message_id":9}`, "task_id"},
		{`{"task_id":7,"dismissal_id":3,"message_id":9}`, "reason"},
	} {
		err := validateReopen([]byte(tc.args))
		if err == nil || !strings.Contains(err.Error(), tc.wantIn) {
			t.Errorf("validateReopen(%s) = %v, want a refusal naming %q", tc.args, err, tc.wantIn)
		}
	}
}
