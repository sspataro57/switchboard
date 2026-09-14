package main

// SWT-54 (docs/tickets/treetop-pr-review-tasks_SPEC.md) criterion 3:
// `opsctl capture-rules add` gains `--pr-review` and a REPEATABLE
// `--exclude-pr-author`, carried into capture_rule_add's args as `pr_review`
// and `exclude_pr_authors`. The TOOL validates (criterion 2); this only builds
// the call, like --revive/--addressed.
//
// GREENFIELD NOTE — EXPECTED RED: neither flag is defined, so the flag parser
// refuses the argv ("flag provided but not defined: -pr-review").
//
// OQ-1 = (b): the seeded command carries NO --exclude-pr-author (Step 0b found
// no second login), so its args carry an empty or absent exclude list.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const prSeedKeyRegex = `<(treetopllc/[A-Za-z0-9._-]+/pull/[0-9]+)@github\.com>$`

func prSeedArgv(extra ...string) []string {
	return append([]string{
		"--project", "collaboratory", "--type", "thread_key_contains", "--pattern", "<treetopllc/",
		"--external-system", "github", "--key-regex", prSeedKeyRegex, "--priority", "91",
		"--note", "treetop-pr-review-tasks: one review task per treetopllc PR he did not author",
	}, extra...)
}

func TestParseCaptureRuleAdd_CarriesPRReviewAndARepeatableExcludeList(t *testing.T) {
	tool, raw, err := parseCaptureRuleAdd(prSeedArgv("--pr-review",
		"--exclude-pr-author", "sspataro-alt", "--exclude-pr-author", "*[bot]"))
	if err != nil {
		t.Fatalf("parseCaptureRuleAdd: %v — criterion 3: --pr-review and a repeatable --exclude-pr-author", err)
	}
	if tool != "capture_rule_add" {
		t.Fatalf("tool = %q, want capture_rule_add", tool)
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("args are not JSON: %v", err)
	}
	if p["pr_review"] != true {
		t.Errorf("pr_review = %v, want true", p["pr_review"])
	}
	if got, want := p["exclude_pr_authors"], []any{"sspataro-alt", "*[bot]"}; !reflect.DeepEqual(got, want) {
		t.Errorf("exclude_pr_authors = %#v, want %#v (repeatable, order kept)", got, want)
	}
	if p["key_regex"] != prSeedKeyRegex {
		t.Errorf("key_regex = %v, want the seeded %q byte for byte", p["key_regex"], prSeedKeyRegex)
	}
}

func TestParseCaptureRuleAdd_TheSeededCommandCarriesNoExcludeList(t *testing.T) {
	_, raw, err := parseCaptureRuleAdd(prSeedArgv("--pr-review"))
	if err != nil {
		t.Fatalf("parseCaptureRuleAdd(the seed command): %v", err)
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("args are not JSON: %v", err)
	}
	if p["pr_review"] != true {
		t.Errorf("pr_review = %v, want true", p["pr_review"])
	}
	if ex, ok := p["exclude_pr_authors"]; ok && !reflect.DeepEqual(ex, []any{}) {
		t.Errorf("exclude_pr_authors = %#v, want absent or empty — OQ-1 = (b): no *[bot] and no second login", ex)
	}
}

// Finding B (SWT-54 review): add's flag set carries --revive/--addressed, but
// try cannot simulate activity rules; it must refuse them by name, before any
// database is reached.
func TestRunCaptureRulesTry_RefusesActivityFlags(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://refused@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	base := []string{"--project", "collaboratory", "--type", "body_regex", "--pattern", `CRG-[0-9]+`,
		"--external-system", "jira", "--key-regex", `(CRG-[0-9]+)`, "--priority", "90"}
	for _, flags := range [][]string{{"--revive"}, {"--addressed"}, {"--revive", "--addressed"}} {
		err := runCaptureRulesTry(append(append([]string{}, base...), flags...))
		if err == nil || !strings.Contains(err.Error(), "try does not simulate activity rules") {
			t.Errorf("try %v: err = %v, want the refusal naming \"try does not simulate activity rules\"", flags, err)
		}
	}
}
