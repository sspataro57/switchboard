package mcpserver_test

// SWT-35 (docs/tickets/task-list-mcp_SPEC.md) criteria 21 and 22: the user-scope
// install runbook. A test on prose earns its place the way
// internal/ticketstatus's TestRunbook_DocumentsTheReconciler does — nothing in
// code can stop the next session installing ops-mcp with `go run` from a ticket
// branch, or installing the full ops-mcp instead of ops-mcp-read so that every
// repo's session gets the write surface.
//
// AMENDED after review (2026-09-10): the runbook first claimed that omitting
// OPS_TOKEN_KEY from the -e flags kept sends out of other repos. It does not — a
// stdio server inherits the launching shell's environment, and ~/.bashrc exports
// the key. The boundary is now the read-only binary cmd/ops-mcp-read (a
// separate binary, after Codex's re-review: a profile SETTING on ops-mcp would
// have had to default to full), and the runbook must say both.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestRunbook_DocumentsUserScopeInstall(t *testing.T) {
	const rel = "docs/runbooks/ops-mcp-user-scope.md"
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v. Criterion 21: the one-time user-scope install is the ticket's 'usable "+
			"alone' step, and its runbook is new", rel, err)
	}
	doc := string(b)
	lower := strings.ToLower(doc)

	// ---- criterion 22: the tokens, exactly as spelled -----------------------
	for _, want := range []struct{ tok, why string }{
		{"--scope user", "the install is USER scope, so every repo's session sees the `ops` server"},
		{"OPS_WORKER_ID=manual:salvo", "REQUIRED: ops-mcp refuses to start without it (fact 4), and a manual install is manual:salvo"},
		{"go install ./cmd/ops-mcp-read", "a BUILT binary from main, never `go run` of whatever branch is checked out (L13)"},
		{"OPS_TOKEN_KEY", "named, so the reader learns that omitting it is NOT the boundary (L13)"},
		{"ops-mcp-read", "the boundary: the read-only binary lists only the queue reads and wires no sender (L13)"},
		{"project_list", "the directory a session confirms a slug against"},
		{"task_list", "the queue read itself"},
		{"delivered", "the default hides delivered work, unlike the board (L4)"},
	} {
		if !strings.Contains(doc, want.tok) {
			t.Errorf("%s never mentions %q — %s (criterion 22)", rel, want.tok, want.why)
		}
	}

	// ---- criterion 21: the rest of the runbook's content --------------------
	for _, want := range []struct{ re, why string }{
		{`claude mcp add --scope user ops`, "the `claude mcp add` line, server name `ops` (L13: the name is what makes precedence work)"},
		{`-e DATABASE_URL=`, "the first of the two -e flags"},
		{`-e OPS_WORKER_ID=manual:salvo`, "the second -e flag"},
		{`-- "$(go env GOPATH)/bin/ops-mcp-read"`, "the registered command is the READ binary (criterion 25)"},
		{`mcp:manual:salvo`, "verification: the audit rows carry the manual actor"},
		{`claude mcp get ops`, "verification step 4"},
		{`/mcp`, "verification step 5: `/mcp` shows `ops` connected from another repo, and ONE `ops` in this one"},
	} {
		if !strings.Contains(doc, want.re) {
			t.Errorf("%s does not contain %q — %s (criterion 21)", rel, want.re, want.why)
		}
	}

	for _, want := range []struct{ re, why string }{
		{`\.mcp\.json`, "the precedence note names the project-scope entry that shadows the user one in this repo"},
		{`shadow|precedence|local\s*(→|->)\s*project\s*(→|->)\s*user`,
			"the precedence note: Claude Code resolves a same-name server local → project → user (L13)"},
		{`(?s)ops_token_key.{0,300}not a boundary.{0,400}inherit`,
			"WHY omitting OPS_TOKEN_KEY protects nothing: the server inherits the launching shell's environment"},
		{`(?s)ops-mcp-read. is the boundary.{0,600}wires no mail sender`,
			"WHAT the read binary does: whatever the environment holds, no sender is wired"},
		{`(?s)ops-mcp-read. is the boundary.{0,100}project_list.{0,40}task_list.{0,40}task_get_next`,
			"…and it lists exactly the three queue reads"},
		{`never install .ops-mcp. itself at user scope`, "the full binary is named as the thing NOT to install"},
		{`re-?run|re-?install`, "the re-install rule: re-run `go install` after any merge touching the server"},
		{`cmd/ops-mcp`, "…naming what triggers it: cmd/ops-mcp"},
		{`internal/mcpserver`, "…internal/mcpserver"},
		{`internal/tools`, "…and internal/tools"},
		{`remember|memori[sz]e`, "the memorise-a-slug usage line: the per-repo binding lives in Claude Code's memory (L2)"},
		{`(?s)closed.{0,300}delivered|delivered.{0,300}closed`,
			"one sentence: task_list hides closed AND delivered by default…"},
		{`board`, "…unlike the board, which hides only closed"},
	} {
		if !regexp.MustCompile(want.re).MatchString(lower) {
			t.Errorf("%s does not match /%s/ — %s (criterion 21)", rel, want.re, want.why)
		}
	}
}
