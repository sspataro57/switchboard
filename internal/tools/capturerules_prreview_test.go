package tools_test

// SWT-54 (docs/tickets/treetop-pr-review-tasks_SPEC.md) criterion 2:
// capture_rule_add accepts pr_review and exclude_pr_authors and refuses, NAMING
// THE FIELD:
//   - pr_review without external_system=github or without key_regex;
//   - pr_review with revive;
//   - exclude_pr_authors without pr_review;
//   - any exclude entry that is not a GitHub login or `*`+suffix
//     (^\*?[A-Za-z0-9][A-Za-z0-9-]*(\[bot\])?$ or ^\*\[bot\]$);
//   - url_template with external_system=github ({key} in the canonical form
//     contains '#').
//
// Same harness as capturerules_test.go: executor.Execute with a NIL pool, so
// every assertion stops at the Validate stage. capture_rules cannot be edited
// and the same pattern cannot be re-added (IK F8): a bad rule refused here is
// the only cheap moment to refuse it.
//
// GREENFIELD NOTE — EXPECTED RED: today the two args are unknown JSON fields,
// silently dropped, so most cases PASS validation and reach the handler, which
// dereferences the nil pool. refusedAt converts that panic into a clean
// failure: "validation accepted it" is the red state, not a crash.
//
// The positive half (a valid rule with both args is stored, both columns
// written) is in internal/capture/prreview_integration_test.go: it seeds every
// PR-review rule THROUGH this tool.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
)

// refusedAt runs capture_rule_add and returns its error, or a synthetic
// "accepted" error when the call got past validation (nil-pool panic).
func refusedAt(ex *executor.Executor, args string) (err error, accepted bool) {
	defer func() {
		if r := recover(); r != nil {
			err, accepted = fmt.Errorf("validation accepted the args; the handler ran and panicked on the nil pool: %v", r), true
		}
	}()
	_, err = ex.Execute(context.Background(), executor.Call{
		Tool: "capture_rule_add", Actor: "opsctl:salvo", Args: json.RawMessage(args)})
	return err, err == nil
}

func TestValidate_CaptureRuleAdd_RefusesBadPRReviewRules(t *testing.T) {
	ex := captureRulesExecutor(t)
	const base = `"project":"collaboratory","criteria_type":"thread_key_contains","pattern":"<treetopllc/","priority":91`
	const re = `"key_regex":"<(treetopllc/[A-Za-z0-9._-]+/pull/[0-9]+)@github\\.com>$"`

	cases := []struct {
		name string
		args string
		want []string // the error must name ONE of these fields
	}{
		{"pr_review on a jira rule", `{` + base + `,"external_system":"jira",` + re + `,"pr_review":true}`, []string{"pr_review"}},
		{"pr_review on an attribution-only rule", `{` + base + `,` + re + `,"pr_review":true}`, []string{"pr_review"}},
		{"pr_review without key_regex", `{` + base + `,"external_system":"github","pr_review":true}`, []string{"pr_review"}},
		{"pr_review with revive", `{` + base + `,"external_system":"github",` + re + `,"pr_review":true,"revive":true}`,
			[]string{"pr_review", "revive"}},
		{"exclude_pr_authors without pr_review", `{` + base + `,"external_system":"github",` + re + `,"exclude_pr_authors":["*[bot]"]}`,
			[]string{"exclude_pr_authors"}},
		{"url_template on a github rule", `{` + base + `,"external_system":"github",` + re + `,"url_template":"https://github.com/{key}"}`,
			[]string{"url_template"}},
		{"url_template on a pr_review rule", `{` + base + `,"external_system":"github",` + re + `,"pr_review":true,"url_template":"https://github.com/{key}"}`,
			[]string{"url_template"}},
	}
	for _, entry := range []string{
		"", "*", "joe smith", "joe@example.com", "-joe", "*-avviato", "[bot]", "**[bot]", "*[bot]x",
		"dependabot[bot][bot]", "joe/smith", "joe_smith",
	} {
		b, _ := json.Marshal([]string{entry})
		cases = append(cases, struct {
			name string
			args string
			want []string
		}{
			name: fmt.Sprintf("exclude entry %q is neither a login nor *suffix", entry),
			args: `{` + base + `,"external_system":"github",` + re + `,"pr_review":true,"exclude_pr_authors":` + string(b) + `}`,
			want: []string{"exclude_pr_authors"},
		})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err, accepted := refusedAt(ex, tc.args)
			if accepted {
				t.Fatalf("capture_rule_add accepted %s: %v", tc.args, err)
			}
			if strings.Contains(err.Error(), "unknown tool") {
				t.Fatalf("capture_rule_add is not registered (%v)", err)
			}
			named := false
			for _, f := range tc.want {
				named = named || strings.Contains(err.Error(), f)
			}
			if !named {
				t.Errorf("error %q names none of %v — criterion 2: the refusal names the field, because the "+
					"operator gets ONE chance at this rule (F8)", err, tc.want)
			}
		})
	}
}
