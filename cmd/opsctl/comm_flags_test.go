package main

// comms-inbox (SWT-74, docs/tickets/comms-inbox_SPEC.md) criteria 18, 19 and
// 28, the CLI half:
//
//   - `opsctl capture-rules add … --comm-task` rides capture_rule_add's INSERT
//     (the TOOL validates — criterion 18's refusals are internal/tools');
//   - `opsctl capture-rules try --comm-task --show all` shows what arming would
//     do BEFORE anything is armed (criterion 19);
//   - `opsctl task-match --message N | --task N | --text "…" [--project slug]
//     [--limit N]` parses to the executor call and prints its JSON (the
//     create-task path), and the usage lines name it (criterion 28).
//
// A flag-parse test plus a source scan: opsctl has no other test harness (the
// route-candidates, pr-review and slack-watch tests are the same shape).
//
// This is on the critical path of "usable alone": Verification Step 5.4 arms
// exactly ONE rule, and Step 4's smoke runs `swb match <id>` and
// `opsctl capture-rules try --comm-task` against a local database.
//
// ---- IMPOSED SURFACE (SPEC criteria 18, 19, 28) -------------------------------
//
//	opsctl capture-rules add … --comm-task        -> capture_rule_add {"comm_task":true}
//	opsctl capture-rules try  … --comm-task       -> DryRunConfig{Candidate:{CommTask:true}}
//	opsctl task-match --message N                 -> task_match {"message_id":N}
//	opsctl task-match --task N                    -> task_match {"task_id":N}
//	opsctl task-match --text "…"                  -> task_match {"text":"…"}
//	          [--project slug] [--limit N]        -> {"project":…,"limit":…}
//	// parseTaskMatch returns ("task_match", args, error) and run() prints the
//	// result JSON, exactly like create-task.
//
// GREENFIELD NOTE — EXPECTED RED: neither flag nor the subcommand exists, so
// the parser refuses the argv ("flag provided but not defined: -comm-task")
// and parseTaskMatch is undefined, which fails this file to COMPILE.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - --comm-task dropped from add's flag set -> CarriesCommTask.
//   - task-match missing from the usage lines -> TaskMatchCommand.

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// ---- criterion 18's CLI half ----------------------------------------------------

func commRuleArgv(extra ...string) []string {
	return append([]string{
		"--project", "collaboratory", "--type", "body_regex", "--pattern", `WEB-[0-9]+`,
		"--external-system", "jira", "--key-regex", `(WEB-[0-9]+)`, "--priority", "90",
		"--note", "comms-inbox: José's WEB-NNNNN mail becomes its own INCOMING row",
	}, extra...)
}

func TestParseCaptureRuleAdd_CarriesCommTask(t *testing.T) {
	tool, raw, err := parseCaptureRuleAdd(commRuleArgv("--comm-task"))
	if err != nil {
		t.Fatalf("parseCaptureRuleAdd(--comm-task): %v — criterion 18: `--comm-task` is an `opsctl "+
			"capture-rules add` flag and rides capture_rule_add's INSERT (the --pr-review precedent)", err)
	}
	if tool != "capture_rule_add" {
		t.Fatalf("tool = %q, want capture_rule_add", tool)
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("args are not JSON: %v", err)
	}
	if p["comm_task"] != true {
		t.Errorf("comm_task = %v, want true (criterion 18)", p["comm_task"])
	}
}

// Without the flag the argument is absent or false: arming is OPT-IN rule by
// rule, with a measurement behind each (D1, Verification Step 0a).
func TestParseCaptureRuleAdd_DefaultsToUnarmed(t *testing.T) {
	_, raw, err := parseCaptureRuleAdd(commRuleArgv())
	if err != nil {
		t.Fatalf("parseCaptureRuleAdd: %v", err)
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("args are not JSON: %v", err)
	}
	if v, ok := p["comm_task"]; ok && v != false {
		t.Errorf("comm_task = %v without --comm-task, want absent or false. D1: it defaults FALSE, so a db with "+
			"0040 applied and nothing armed behaves byte-identically to 0.7.39", v)
	}
}

// ---- criterion 19: the dry run's flag -------------------------------------------

func TestOpsctl_CaptureRulesTryCarriesCommTask(t *testing.T) {
	src := opsctlSources(t)
	if !strings.Contains(src, "comm-task") {
		t.Errorf("opsctl never defines a --comm-task flag. Criterion 19: `opsctl capture-rules try --comm-task " +
			"--show all` shows what arming would do BEFORE anything is armed — which is how Step 0a's gate is " +
			"read without writing a row")
	}
	if !regexp.MustCompile(`CommTask\s*:`).MatchString(src) {
		t.Errorf("opsctl never sets CandidateRule.CommTask; `capture-rules try --comm-task` would parse the " +
			"flag and simulate the UNARMED rule — a flag that changes nothing is worse than no flag " +
			"(criterion 19)")
	}
}

// ---- criterion 28: the task-match subcommand -------------------------------------

func TestOpsctl_TaskMatchCommand(t *testing.T) {
	src := opsctlSources(t)
	if !strings.Contains(src, `"capture-rules"`) {
		t.Fatalf("POSITIVE CONTROL FAILED: the opsctl sources no longer dispatch capture-rules, so this scan " +
			"is not reading the command table")
	}
	if !strings.Contains(src, `"task-match"`) {
		t.Errorf("opsctl does not dispatch a \"task-match\" command (criterion 28). It rides the create-task " +
			"path: parse the flags, call the executor, print the JSON")
	}
	if !regexp.MustCompile(`usage: opsctl <[^>]*task-match`).MatchString(src) {
		t.Errorf("opsctl's top-level usage line does not list task-match (criterion 28)")
	}
	if !regexp.MustCompile(`(?m)^//\s+opsctl task-match`).MatchString(src) {
		t.Errorf("cmd/opsctl/main.go's header comment does not document `opsctl task-match` (criterion 28: the " +
			"header comment AND both usage strings name it)")
	}
}

func TestParseTaskMatch_EachInputAndTheOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want map[string]any
	}{
		{"message", []string{"--message", "8801"}, map[string]any{"message_id": float64(8801)}},
		{"task", []string{"--task", "452"}, map[string]any{"task_id": float64(452)}},
		{"text", []string{"--text", "WEB-10469 still blocks the import"},
			map[string]any{"text": "WEB-10469 still blocks the import"}},
		{"message with project and limit", []string{"--message", "8801", "--project", "collaboratory", "--limit", "3"},
			map[string]any{"message_id": float64(8801), "project": "collaboratory", "limit": float64(3)}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			tool, raw, err := parseTaskMatch(tc.argv)
			if err != nil {
				t.Fatalf("parseTaskMatch(%v): %v — criterion 28: `opsctl task-match --message N | --task N | "+
					"--text \"…\" [--project slug] [--limit N]`", tc.argv, err)
			}
			if tool != "task_match" {
				t.Fatalf("tool = %q, want task_match", tool)
			}
			var p map[string]any
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatalf("args are not JSON: %v", err)
			}
			for k, v := range tc.want {
				if p[k] != v {
					t.Errorf("args[%q] = %v, want %v (args: %s)", k, p[k], v, raw)
				}
			}
			// The keys NOT given must be absent, or the tool's "exactly one of
			// three" validation refuses every call opsctl makes.
			for _, k := range []string{"message_id", "task_id", "text"} {
				if _, want := tc.want[k]; want {
					continue
				}
				if _, got := p[k]; got {
					t.Errorf("args carry %q (%s) although it was not passed; task_match validates EXACTLY ONE "+
						"of message_id, task_id and text (criterion 22)", k, raw)
				}
			}
		})
	}
}
