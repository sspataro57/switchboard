package orchestrator_test

// SWT-41 review (go-reviewer, invariant 7): the orchestrator is pure and never
// reaches an LLM provider adapter or a connector — not directly, not through a
// dependency. Sharing the lock key through internal/tools once dragged in
// internal/provider (via planimport) and four connector packages; the key now
// lives in the import-free internal/lockkeys. This makes the rule a check, not
// a comment.
//
// MUTATION THAT MUST TURN THIS RED: import internal/tools from engine.go again.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestOrchestrator_DependsOnNoProviderOrConnector(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	out, err := exec.Command(goBin, "list", "-deps", "github.com/sspataro57/switchboard/internal/orchestrator").Output()
	if err != nil {
		t.Fatalf("go list -deps internal/orchestrator: %v", err)
	}
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(deps) < 5 {
		t.Fatalf("POSITIVE CONTROL: go list returned %d packages; the scan is not seeing the graph", len(deps))
	}
	for _, dep := range deps {
		for _, banned := range []string{
			"/internal/provider", "/internal/connector/", "/internal/planimport", "/internal/tools",
		} {
			if strings.Contains(dep+"/", banned) || strings.HasSuffix(dep, banned) {
				t.Errorf("internal/orchestrator depends on %s. Invariant 7: the orchestrator never reaches an LLM "+
					"provider adapter or a connector, even transitively", dep)
			}
		}
	}
}
