package main

// SWT-40 Part B criterion B8's CLI half (docs/tickets/inquiry-promote_SPEC.md,
// "API / MCP tool changes"): `opsctl route-candidates add|remove|list`. The two
// writes go through the executor tools (route_candidate_add / _remove,
// humanOnly, audited as opsctl:$USER) — never a direct INSERT or DELETE here
// (the repo-wide scan in internal/capture/route_structure_test.go also covers
// this file). A source scan: opsctl has no other test harness.
//
// GREENFIELD NOTE — EXPECTED RED: main.go has no route-candidates command.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestOpsctl_RouteCandidatesCommand(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var b strings.Builder
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		b.Write(raw)
	}
	src := b.String()
	if !strings.Contains(src, `"capture-rules"`) {
		t.Fatalf("POSITIVE CONTROL FAILED: the opsctl sources no longer dispatch capture-rules")
	}
	if !strings.Contains(src, `"route-candidates"`) {
		t.Errorf("opsctl does not dispatch a \"route-candidates\" command (B8)")
	}
	if !regexp.MustCompile(`usage: opsctl <[^>]*route-candidates`).MatchString(src) {
		t.Errorf("opsctl's usage line does not list route-candidates")
	}
	for _, tool := range []string{"route_candidate_add", "route_candidate_remove"} {
		if !strings.Contains(src, `"`+tool+`"`) {
			t.Errorf("opsctl never calls the %s tool; B8's writes go through the executor (validate → policy → audit)", tool)
		}
	}
	if !regexp.MustCompile(`usage: opsctl route-candidates <add\|remove\|list>`).MatchString(src) {
		t.Errorf("opsctl has no `usage: opsctl route-candidates <add|remove|list>` line (B8 names the three subcommands)")
	}
}
