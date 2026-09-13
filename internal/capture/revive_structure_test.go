package capture

// Structural tests for SWT-45 (docs/tickets/jira-activity-revive_SPEC.md) on
// the capture side: revive.go is pure (criterion 20's file), every capture
// counter line prints the two new counters (criterion 28), opsctl gains the two
// flags and lists them (criterion 17), and the capture-rules runbook gains its
// "Activity rules (SWT-45)" section (criteria 17 and 40). ZERO I/O beyond
// reading this repo. Reuses mustReadRepoFile from rules_structure_test.go.
//
// Each test pins a fact that holds once SWT-45 is implemented: revive.go
// declares overrides and imports no context, pgx, environment or provider;
// every printer of the capture counter line also prints "revived" and
// "surfaced_created"; opsctl's add declares both bool flags and its list
// selects both columns; the runbook's activity section names the flags, the
// truth table, the J2/J4 commands, F8 and J10's cost.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ---- criterion 20's file: overrides is pure --------------------------------

func TestReviveGo_IsPure(t *testing.T) {
	src := mustReadRepoFile(t, "internal/capture/revive.go")
	if !strings.Contains(src, "func overrides(") {
		t.Errorf("internal/capture/revive.go does not declare overrides — criterion 20 pins the truth table to " +
			"a pure function in its own file")
	}
	for _, b := range []struct{ token, why string }{
		{`"context"`, "a pure truth table takes no context"},
		{"pgx", "no database: invariant 7, Capture's overrides is a pure truth table"},
		{"os.Getenv", "the flags come from the capture_rules and projects COLUMNS, never the environment"},
		{"internal/provider", "no model"},
	} {
		if strings.Contains(src, b.token) {
			t.Errorf("internal/capture/revive.go mentions %q — %s", b.token, b.why)
		}
	}
}

// ---- criterion 28: every capture counter line prints the new counters ------

// "Every capture counter line prints "revived" and "surfaced_created", zeros
// included: cmd/connectors/{jira,slackweb,upworkcrm}/main.go,
// cmd/connectors/google/main.go and watch.go, and cmd/opsctl/main.go's
// capture-rules run. A structural scan fails a main that prints the counters
// without them."
//
// The scan finds the printers by what they already print ("appended", the
// SWT-17 counter) rather than from a list this test supplies, so a sixth
// printer added later is held to the same line.
func TestCaptureCounterLines_PrintRevivedAndSurfacedCreated(t *testing.T) {
	printer := regexp.MustCompile(`\\"appended\\":%d`)
	var printers []string
	err := filepath.Walk(filepath.Join("..", "..", "cmd"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		src := string(b)
		if !printer.MatchString(src) {
			return nil
		}
		rel := strings.TrimPrefix(filepath.ToSlash(path), "../../")
		printers = append(printers, rel)
		for _, want := range []string{`\"revived\":%d`, `\"surfaced_created\":%d`} {
			if !strings.Contains(src, want) {
				t.Errorf("%s prints the capture counters without %s. Criterion 28: zeros included — "+
					"a revive that happened and a revive that could not happen must not print the same line, "+
					"and Verification step 8 keys on these two counters", rel, want)
			}
		}
		// J17 (second review batch): the own-action guard's undecided and blind
		// outcomes are visible on the same line, zeros included. A deferral writes
		// no decision row and a blind decision fails open, so the log line is the
		// only place either shows up without a query. The gate line
		// (cmd/opsctl/gate.go) prints them as constant 0, as it does revived.
		for _, want := range []string{`\"deferred\":%d`, `\"blind\":%d`} {
			if !strings.Contains(src, want) {
				t.Errorf("%s prints the capture counters without %s (SPEC J17: RulesStats.Deferred and .Blind, "+
					"zeros included)", rel, want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk cmd/: %v", err)
	}
	// Control: the five printers the SPEC names must be among those found, or
	// the scan is looking at the wrong spelling.
	for _, want := range []string{
		"cmd/connectors/jira/main.go", "cmd/connectors/slackweb/main.go", "cmd/connectors/upworkcrm/main.go",
		"cmd/connectors/google/main.go", "cmd/opsctl/main.go",
	} {
		found := false
		for _, p := range printers {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s no longer prints the capture counter line (\"appended\":%%d); the scan cannot hold it "+
				"to criterion 28. Found printers: %v", want, printers)
		}
	}
}

// ---- criterion 17: opsctl flags and list ------------------------------------

func TestOpsctl_CaptureRulesAddAndListKnowTheActivityFlags(t *testing.T) {
	src := mustReadRepoFile(t, "cmd/opsctl/main.go")

	add := rvFuncSrc(src, "parseCaptureRuleAdd")
	if add == "" {
		t.Fatalf("cmd/opsctl/main.go no longer declares parseCaptureRuleAdd; criterion 17's subject moved")
	}
	for _, flag := range []string{`"revive"`, `"addressed"`} {
		if !regexp.MustCompile(`fs\.Bool\(\s*` + regexp.QuoteMeta(flag)).MatchString(add) {
			t.Errorf("opsctl capture-rules add declares no %s bool flag. Criterion 17: `--revive` and "+
				"`--addressed` are how J2 and J4 are seeded through the executor", flag)
		}
	}

	list := rvFuncSrc(src, "runCaptureRulesList")
	if list == "" {
		t.Fatalf("cmd/opsctl/main.go no longer declares runCaptureRulesList")
	}
	for _, col := range []string{"r.revive", "r.addressed"} {
		if !strings.Contains(list, col) {
			t.Errorf("runCaptureRulesList does not select %s. Criterion 17: `capture-rules list` prints revive / "+
				"addressed on the key line — Verification step 5 reads it to confirm J2 landed at 92 WITH revive", col)
		}
	}
}

// ---- criteria 17 + 40: the runbook's activity section -----------------------

func TestRunbook_DocumentsActivityRules(t *testing.T) {
	doc := strings.ToLower(mustReadRepoFile(t, "docs/runbooks/capture-rules.md"))
	i := strings.Index(doc, "activity rules (swt-45)")
	if i < 0 {
		t.Fatalf("docs/runbooks/capture-rules.md has no \"Activity rules (SWT-45)\" section (criterion 40)")
	}
	section := doc[i:]
	if j := strings.Index(section[1:], "\n## "); j > 0 {
		section = section[:j+1]
	}
	for _, want := range []struct{ frag, why string }{
		{"--revive", "the flag, and what it means (decision 1)"},
		{"--addressed", "the flag, and what it means (decision 3)"},
		{"overrides", "J1's overrides table: revive AND (NOT gate OR addressed)"},
		{"jira@treetopllc.jira.com", "the J2 command verbatim"},
		{"--key-regex", "the J2/J4 commands carry an explicit key regex (J1 requires one)"},
		{"lhh-", "the J4 command"},
		{"key_regex", "why revive needs an explicit key_regex (F1: rule 10 keys by PREFIX)"},
		{"unique", "F8: capture_rules is UNIQUE (project_id, criteria_type, pattern) — no re-add with the same pattern"},
		{"close", "J10's cost: a ticket-closed email arriving after the close revives the task until one hand close"},
		{"slack", "the owner's 2026-09-12 answer: a Slack or GitHub mention of a key counts as activity (the " +
			"rule-10 successor carries --revive); only NEW messages act, there is no backfill"},
	} {
		if !strings.Contains(section, want.frag) {
			t.Errorf("the runbook's activity section never mentions %q — %s", want.frag, want.why)
		}
	}
}

// rvFuncSrc returns `func name(` up to the next top-level func.
func rvFuncSrc(src, name string) string {
	i := strings.Index(src, "\nfunc "+name+"(")
	if i < 0 {
		return ""
	}
	body := src[i+1:]
	if j := strings.Index(body[1:], "\nfunc "); j > 0 {
		body = body[:j+1]
	}
	return body
}
