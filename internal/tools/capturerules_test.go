package tools_test

// Unit tests for the two SWT-17 rule-management tools (acceptance criteria 5 and
// 15's registration half), in the mold of tools_unit_test.go: driven through
// executor.Execute with a NIL *pgxpool.Pool. Every assertion stops BEFORE any
// handler runs — a validation failure returns from Execute after the Validate
// stage, so nothing dereferences the pool. ZERO network, ZERO Postgres.
//
// GREENFIELD NOTE: fails today — tools.Register wires neither name, so Execute
// returns "unknown tool", which every assertion below explicitly rejects as NOT a
// validation failure. Expected red state.
//
// IMPOSED, and worth arguing with if you disagree: criterion 5 says a rule with an
// uncompilable pattern is "REFUSED at insert time with an error naming the
// offending field; no row is written". This file puts that refusal in the tool's
// VALIDATE stage, which is the only place it can be reached without a database —
// and the only place where "no row is written" is guaranteed by construction
// rather than by remembering to return early. If the compile lands in the handler
// instead, these two cases panic on the nil pool rather than failing cleanly; that
// panic is the signal, not a flake.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

func captureRulesExecutor(t *testing.T) *executor.Executor {
	t.Helper()
	reg := executor.NewRegistry()
	tools.Register(reg, nil) // nil pool: Register only builds closures.
	return executor.New(reg, policy.NewStatic(reg.Names()...), audit.NewMemStore())
}

func TestRegister_CaptureRuleToolsRegistered(t *testing.T) {
	reg := executor.NewRegistry()
	tools.Register(reg, nil)
	got := map[string]bool{}
	for _, n := range reg.Names() {
		got[n] = true
	}
	for _, want := range []string{"capture_rule_add", "capture_rule_set_enabled"} {
		if !got[want] {
			t.Errorf("tool %q not registered by tools.Register — rule mutations are themselves tool calls, so "+
				"'who changed the routing and when' is answerable from audit_events (invariant 3)", want)
		}
	}
}

// Empty args are illegal for both: capture_rule_add needs project + criteria_type
// + pattern, capture_rule_set_enabled needs rule_id and an EXPLICIT enabled — a
// routing rule must never be switched off by omission.
func TestValidate_CaptureRuleTools_RejectEmptyArgs(t *testing.T) {
	ex := captureRulesExecutor(t)
	ctx := context.Background()
	for _, name := range []string{"capture_rule_add", "capture_rule_set_enabled"} {
		t.Run(name, func(t *testing.T) {
			_, err := ex.Execute(ctx, executor.Call{Tool: name, Actor: "opsctl:salvo", Args: json.RawMessage(`{}`)})
			if err == nil {
				t.Fatalf("%s with {} succeeded; want a validation failure", name)
			}
			if strings.Contains(err.Error(), "unknown tool") {
				t.Fatalf("%s is not registered (%v); that is not a validation failure", name, err)
			}
		})
	}
}

// Criterion 5: the pattern and the key_regex are compiled at insert time, and the
// error names which one. "A bad regex discovered inside a CronJob is a silent
// routing outage" — the rule that stops matching is invisible, because a rule
// matching nothing looks exactly like data nobody sent.
func TestValidate_CaptureRuleAdd_RefusesUncompilableRegexes(t *testing.T) {
	ex := captureRulesExecutor(t)
	ctx := context.Background()

	cases := []struct {
		name      string
		args      string
		wantField string
	}{
		{
			name:      "uncompilable pattern on a body_regex rule",
			args:      `{"project":"reengine","criteria_type":"body_regex","pattern":"LHH-[0-9","priority":100}`,
			wantField: "pattern",
		},
		{
			name:      "uncompilable key_regex",
			args:      `{"project":"reengine","criteria_type":"body_regex","pattern":"LHH-[0-9]+","key_regex":"(LHH-[0-9]+","priority":100}`,
			wantField: "key_regex",
		},
		{
			name: "uncompilable key_regex on a NON body_regex rule",
			// The key_regex is compiled whenever it is present — not only for
			// body_regex rules. Every thread-key rule in the fixture set carries
			// one.
			args:      `{"project":"collaboratory","criteria_type":"thread_key_prefix","pattern":"jira:treetopllc.jira.com:WEB-","key_regex":"[A-Z]+-[0-9+$"}`,
			wantField: "key_regex",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ex.Execute(ctx, executor.Call{
				Tool: "capture_rule_add", Actor: "opsctl:salvo", Args: json.RawMessage(tc.args)})
			if err == nil {
				t.Fatalf("capture_rule_add accepted an uncompilable %s", tc.wantField)
			}
			if strings.Contains(err.Error(), "unknown tool") {
				t.Fatalf("capture_rule_add is not registered (%v)", err)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("error %q does not name the offending field %q; the operator is at a CLI with nine "+
					"rules to seed and needs to know which one to fix (criterion 5)", err, tc.wantField)
			}
		})
	}
}

// The enum and the url_template are validated in the same stage, for the same
// reason: a criteria_type the evaluator does not implement, or a url_template with
// no {key}, both produce a rule that is silently useless.
func TestValidate_CaptureRuleAdd_RefusesBadEnumsAndTemplates(t *testing.T) {
	ex := captureRulesExecutor(t)
	ctx := context.Background()

	cases := []struct{ name, args, want string }{
		{
			name: "criteria_type outside the CHECK",
			args: `{"project":"reengine","criteria_type":"subject_regex","pattern":"LHH-[0-9]+"}`,
			want: "criteria_type",
		},
		{
			name: "external_system outside the extended external_refs enum",
			args: `{"project":"reengine","criteria_type":"body_regex","pattern":"LHH-[0-9]+","external_system":"linear"}`,
			want: "external_system",
		},
		{
			name: "url_template with no {key} placeholder",
			args: `{"project":"reengine","criteria_type":"body_regex","pattern":"LHH-[0-9]+","external_system":"jira","url_template":"https://avviato.atlassian.net/browse/"}`,
			want: "url_template",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ex.Execute(ctx, executor.Call{
				Tool: "capture_rule_add", Actor: "opsctl:salvo", Args: json.RawMessage(tc.args)})
			if err == nil {
				t.Fatalf("capture_rule_add accepted a bad %s", tc.want)
			}
			if strings.Contains(err.Error(), "unknown tool") {
				t.Fatalf("capture_rule_add is not registered (%v)", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}
}
