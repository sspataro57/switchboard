package capture

// Structural tests for SWT-17 acceptance criteria 2, 16 and 21 — the three the
// SPEC asks to be enforced mechanically rather than by review. ZERO I/O beyond
// reading this repo's own source and docs.
//
// GREENFIELD NOTE: all three fail today because the files they check do not exist
// yet (rules.go, rules_store.go, docs/runbooks/capture-rules.md). That is
// deliberate: a source-scanning test that passes because there is nothing to scan
// is the "fixture that proves nothing" landmine wearing a lab coat, so each
// assertion below starts by REQUIRING its subject to exist.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// mustReadRepoFile reads a path relative to the repo root (this package sits at
// internal/capture, two levels down).
func mustReadRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// ---- criterion 2: Evaluate lives in a file with no I/O -------------------------

// "capture.Evaluate(msg, rules) is a pure Go function: no pgxpool, no context, no
// provider import in its file." Invariant 7 applied to this engine: the file is
// the boundary, so purity cannot rot into "pure except for one lookup".
func TestRulesGo_IsPure(t *testing.T) {
	src := mustReadRepoFile(t, "internal/capture/rules.go")

	banned := []struct{ token, why string }{
		{`"context"`, "a context parameter is the first thing an I/O call needs; its absence is what keeps Evaluate offline"},
		{"pgx", "no database: the pure evaluator must be testable with zero network (invariant 7)"},
		{"internal/provider", "no LLM: the orchestrator/engine never calls a model"},
		{"internal/connector/", "no provider adapter: vendor details live in adapters only"},
		{"net/http", "no network"},
		{"os.Getenv", "configuration is passed in (RulesConfig), not read from the environment inside the evaluator"},
	}
	for _, b := range banned {
		if strings.Contains(src, b.token) {
			t.Errorf("internal/capture/rules.go mentions %q — criterion 2 forbids it: %s", b.token, b.why)
		}
	}
	if !strings.Contains(src, "func Evaluate(") {
		t.Errorf("internal/capture/rules.go does not declare Evaluate; criterion 2 pins it to THIS file, " +
			"because the file is what makes the purity checkable")
	}
}

// ---- criterion 16: tasks / external_refs / task_events only via the executor ----

// "Every tasks / external_refs / task_events write made by this package goes
// through executor.Execute; a test greps or reflects that internal/capture
// contains no INSERT INTO tasks, INSERT INTO external_refs, or INSERT INTO
// task_events."
//
// Scoped to the capture-RULES files, and that scoping is not a loophole: SWT-16's
// observe.go deliberately writes `INSERT INTO task_events ... outbound_observed`
// directly, so criterion 16 taken literally over the whole package is
// unsatisfiable against code that shipped a ticket ago. The rule this test
// enforces is the one criterion 16 means — the ENGINE reaches tool-action tables
// only through create_task / link_external_ref / task_append_log.
func TestCaptureRules_NeverWritesToolActionTablesDirectly(t *testing.T) {
	files := ruleEngineSourceFiles(t)
	if len(files) == 0 {
		t.Fatalf("no rule-engine source files found in internal/capture (expected rules.go and rules_store.go); " +
			"a scan with nothing to scan proves nothing")
	}
	banned := regexp.MustCompile(`(?is)insert\s+into\s+(tasks|external_refs|task_events)\b`)
	updateBanned := regexp.MustCompile(`(?is)update\s+(tasks|external_refs|task_events)\b`)
	for _, f := range files {
		src := mustReadRepoFile(t, filepath.Join("internal/capture", f))
		if m := banned.FindString(src); m != "" {
			t.Errorf("internal/capture/%s contains %q — invariant 3: tasks/external_refs/task_events are "+
				"reached ONLY through create_task, link_external_ref and task_append_log on the executor", f, m)
		}
		if m := updateBanned.FindString(src); m != "" {
			t.Errorf("internal/capture/%s contains %q — same rule: a direct mutation skips validate → policy "+
				"→ audit and leaves no answer to 'who did this'", f, m)
		}
	}
}

// ruleEngineSourceFiles lists the non-test capture-rules sources. rules*.go is the
// SPEC's own naming (rules.go, rules_store.go, rulesreport.go).
func ruleEngineSourceFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/capture: %v", err)
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		if strings.HasPrefix(n, "rules") {
			out = append(out, n)
		}
	}
	return out
}

// ---- criterion 21: the go-live ordering is written down ------------------------

// "The go-live ordering constraint is documented in docs/runbooks/capture-rules.md:
// capture live before triage live, with the fall-through-both reason stated."
//
// A test on prose is unusual; this one earns it. The constraint exists because
// §8's answer creates a window in which a matched-but-shadow message is claimed by
// nobody — capture did not create the task (shadow) and triage will not (routed).
// Nothing in the code can detect that window; the only mitigation is that the
// person flipping the two env vars reads the order. If the sentence is not in the
// runbook, the mitigation does not exist.
func TestRunbook_DocumentsCaptureBeforeTriage(t *testing.T) {
	doc := mustReadRepoFile(t, "docs/runbooks/capture-rules.md")
	lower := strings.ToLower(doc)

	ordering := regexp.MustCompile(`(?s)capture[^.\n]{0,80}before[^.\n]{0,80}triage`)
	if !ordering.MatchString(lower) {
		t.Errorf("docs/runbooks/capture-rules.md does not state the ordering (a sentence saying capture goes " +
			"live BEFORE triage). Criterion 21: the two mode flips are ordered, not independent")
	}
	for _, want := range []string{"shadow", "live"} {
		if !strings.Contains(lower, want) {
			t.Errorf("docs/runbooks/capture-rules.md never mentions %q; the go-live checklist is the point of the file", want)
		}
	}
	if !strings.Contains(lower, "fall") && !strings.Contains(lower, "gap") {
		t.Errorf("docs/runbooks/capture-rules.md states the ordering but not the REASON (matched-in-shadow " +
			"messages fall through both capture and triage). An unexplained ordering is one someone reorders")
	}
}
