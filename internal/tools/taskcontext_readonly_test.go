package tools

// SWT-56 (docs/tickets/signal-session-name_SPEC.md) criterion 29, the unit half:
// task_context's handler flag require_read_only (S12 layer 2). ZERO I/O. The
// integration half (a claimed task stays claimed) is
// taskcontext_readonly_integration_test.go.
//
// IMPOSED SURFACE (S12):
//
//	type contextArgs struct { …; RequireReadOnly string `json:"require_read_only,omitempty"` }
//	// validateContext refuses any non-empty value other than "true", by name.
//
// EXPECTED RED: contextArgs has no RequireReadOnly, and validateContext accepts
// every value.

import (
	"reflect"
	"strings"
	"testing"
)

func TestContextArgs_RequireReadOnlyField(t *testing.T) {
	f, ok := reflect.TypeOf(contextArgs{}).FieldByName("RequireReadOnly")
	if !ok {
		t.Fatalf("contextArgs has no RequireReadOnly field (criterion 29 / S12 layer 2)")
	}
	if f.Type.Kind() != reflect.String {
		t.Errorf("contextArgs.RequireReadOnly is %s, want string (the SWT-38 pin shape)", f.Type)
	}
	if tag := f.Tag.Get("json"); tag != "require_read_only,omitempty" {
		t.Errorf("contextArgs.RequireReadOnly json tag = %q, want \"require_read_only,omitempty\"", tag)
	}
}

func TestValidateContext_RequireReadOnly(t *testing.T) {
	for _, args := range []string{
		`{"task_id":412}`,
		`{"task_id":412,"require_read_only":""}`,
		`{"task_id":412,"require_read_only":"true"}`,
		`{"task_id":412,"worker_id":"","require_read_only":"true"}`,
	} {
		if err := validateContext([]byte(args)); err != nil {
			t.Errorf("validateContext(%s) = %v, want nil", args, err)
		}
	}
	for _, bad := range []string{"false", "TRUE", "yes", "1", " true"} {
		args := `{"task_id":412,"require_read_only":"` + bad + `"}`
		err := validateContext([]byte(args))
		if err == nil {
			t.Errorf("validateContext(%s) = nil; S12: any value other than \"\" and \"true\" is refused", args)
			continue
		}
		if !strings.Contains(err.Error(), "require_read_only") || !strings.Contains(err.Error(), bad) {
			t.Errorf("validateContext(%s) = %q, want it to name require_read_only and the value %q", args, err, bad)
		}
	}
}
