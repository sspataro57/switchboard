package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/tools"
)

// swb 703 / SWT-93: `opsctl capture-rules add --always-task` rides
// capture_rule_add, and the tool refuses the flag on a keyed rule.
func TestParseCaptureRuleAdd_CarriesAlwaysTask(t *testing.T) {
	argv := []string{"--project", "personal", "--type", "sender", "--pattern", "@pinespropertymanagement.com",
		"--priority", "6", "--always-task"}
	_, raw, err := parseCaptureRuleAdd(argv)
	if err != nil {
		t.Fatalf("parseCaptureRuleAdd(--always-task): %v", err)
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if p["always_task"] != true {
		t.Errorf("always_task = %v, want true", p["always_task"])
	}
	if err := tools.ValidateCaptureRuleAdd(raw); err != nil {
		t.Errorf("a keyless always_task rule is refused: %v", err)
	}
	_, plain, err := parseCaptureRuleAdd(argv[:len(argv)-1])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "always_task") {
		t.Errorf("always_task sent without --always-task: %s", plain)
	}
}

func TestCaptureRuleAdd_RefusesAlwaysTaskOnAKeyedRule(t *testing.T) {
	raw := []byte(`{"project":"collaboratory","criteria_type":"body_regex","pattern":"WEB-[0-9]+",` +
		`"external_system":"jira","key_regex":"(WEB-[0-9]+)","priority":90,"always_task":true}`)
	err := tools.ValidateCaptureRuleAdd(raw)
	if err == nil || !strings.Contains(err.Error(), "always_task") {
		t.Errorf("a keyed always_task rule validated (err=%v); want a refusal naming always_task", err)
	}
}
