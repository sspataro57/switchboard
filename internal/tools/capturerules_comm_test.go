package tools_test

// comms-inbox (SWT-74, docs/tickets/comms-inbox_SPEC.md) criterion 18:
// capture_rule_add accepts `comm_task` and refuses, NAMING THE FIELD, the two
// combinations migration 0040's CHECK also refuses — both ways round:
//
//   - comm_task with pr_review;
//   - comm_task without an external_system.
//
// Same harness as capturerules_prreview_test.go (refusedAt: executor.Execute
// with a NIL pool, so every assertion stops at Validate and a nil-pool panic is
// reported as "validation accepted it", the red state).
//
// WHY BOTH A CHECK AND A VALIDATOR (D1, the pr_review precedent): the CHECK is
// the data-level guarantee — a rule armed by hand with an UPDATE cannot slip
// past it — and the validator is what names the FIELD instead of a constraint,
// which matters because capture_rules cannot be edited (IK F8): the operator
// gets ONE chance at a rule.
//
// ---- IMPOSED SURFACE (SPEC D1, criterion 18) ---------------------------------
//
//	// internal/tools/capturerules.go
//	type captureRuleAddArgs struct { …; CommTask bool `json:"comm_task,omitempty"` }
//	// refusals, ahead of / beside the pr_review block so the error names both fields:
//	//   comm_task with pr_review          -> names comm_task AND pr_review
//	//   comm_task with no external_system -> names comm_task AND external_system
//	// and the INSERT carries comm_task.
//
// GREENFIELD NOTE, EXPECTED RED: `comm_task` is an unknown JSON field today,
// silently dropped (IK: nothing rejects unknown tool args), so every case here
// PASSES validation and reaches the handler — refusedAt turns that into a clean
// "capture_rule_add accepted …" failure.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - allow comm_task with pr_review (drop the validator refusal) -> RefusesBadCommRules.
//   - drop the external_system refusal -> the same (an armed keyless rule would
//     be an INERT flag: a keyless rule can never reach the task_log branch).

import (
	"strings"
	"testing"
)

func TestValidate_CaptureRuleAdd_RefusesBadCommRules(t *testing.T) {
	ex := captureRulesExecutor(t)
	const base = `"project":"collaboratory","criteria_type":"body_regex","pattern":"WEB-[0-9]+","priority":90`
	const jiraKey = `"external_system":"jira","key_regex":"(WEB-[0-9]+)"`
	const ghKey = `"external_system":"github","key_regex":"<(treetopllc/[A-Za-z0-9._-]+/pull/[0-9]+)@github\\.com>$"`

	for _, tc := range []struct {
		name string
		args string
		want []string // the error must name EVERY one of these
		why  string
	}{
		{
			name: "comm_task with pr_review",
			args: `{` + base + `,` + ghKey + `,"pr_review":true,"comm_task":true}`,
			want: []string{"comm_task", "pr_review"},
			why: "D1: GitHub notification mail is a NOTICE stream, it has its own review tasks (SWT-54), and " +
				"his own PR mail was 25 of the 14-day sample — a comm task for each of those is a worse board " +
				"than today's",
		},
		{
			name: "comm_task with pr_review, the flags the other way round",
			args: `{` + base + `,"comm_task":true,"pr_review":true,` + ghKey + `}`,
			want: []string{"comm_task", "pr_review"},
			why:  "criterion 18: refused BOTH WAYS (the pr_review+revive test's shape)",
		},
		{
			name: "comm_task without an external_system",
			args: `{` + base + `,"comm_task":true}`,
			want: []string{"comm_task", "external_system"},
			why: "D1: a keyless rule can never reach the task_log branch (decideMessage returns attribution " +
				"only), so an armed keyless rule would be an INERT flag — the constant-discriminator landmine " +
				"in one more costume",
		},
		{
			name: "comm_task with an empty external_system",
			args: `{` + base + `,"external_system":"","comm_task":true}`,
			want: []string{"comm_task", "external_system"},
			why:  "the same, spelled as an empty string",
		},
		{
			name: "comm_task with a key_regex but still no external_system",
			args: `{` + base + `,"key_regex":"(WEB-[0-9]+)","comm_task":true}`,
			want: []string{"comm_task", "external_system"},
			why:  "a key_regex without a system derives a key nothing can resolve",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err, accepted := refusedAt(ex, tc.args)
			if accepted {
				t.Fatalf("capture_rule_add accepted %s. Criterion 18: %s", tc.args, tc.why)
			}
			if strings.Contains(err.Error(), "unknown tool") {
				t.Fatalf("capture_rule_add is not registered (%v)", err)
			}
			for _, field := range tc.want {
				if !strings.Contains(err.Error(), field) {
					t.Errorf("error %q does not name %s — criterion 18: the refusal names the FIELD, not a "+
						"constraint, because capture_rules cannot be edited and the operator gets ONE chance "+
						"(IK F8). %s", err, field, tc.why)
				}
			}
		})
	}
	_ = jiraKey
}

// The positive half: a jira-keyed, non-pr_review rule armed for comms passes
// validation and reaches the handler — this is the ONE rule Verification Step 5
// arms (rule 75's shape). The persistence half (the column is written and
// loadRules reads it back) is internal/capture's criteria 42/43.
func TestValidate_CaptureRuleAdd_AcceptsAnArmedJiraRule(t *testing.T) {
	ex := captureRulesExecutor(t)
	args := `{"project":"collaboratory","criteria_type":"body_regex","pattern":"WEB-[0-9]+","priority":90,` +
		`"external_system":"jira","key_regex":"(WEB-[0-9]+)","url_template":"https://x.atlassian.net/browse/{key}",` +
		`"comm_task":true,"note":"comms-inbox: José's WEB-NNNNN mail becomes its own INCOMING row"}`
	err, accepted := refusedAt(ex, args)
	if accepted {
		return // past validation, which is the whole assertion
	}
	if strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("capture_rule_add is not registered (%v)", err)
	}
	if strings.Contains(err.Error(), "validate capture_rule_add") {
		t.Errorf("capture_rule_add REFUSED the one rule this ticket exists to arm: %v.\nargs: %s\n"+
			"Criterion 18 / \"Usable alone\": `opsctl capture-rules add … --comm-task` (or one UPDATE) on "+
			"rule 75's shape is the entire rollout", err, args)
	}
}
