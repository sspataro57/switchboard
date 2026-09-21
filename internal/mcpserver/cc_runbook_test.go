package mcpserver_test

// gmail-delivery-cc (SWT-69) criteria 22 and 23: the two documents this ticket
// owes. A test on prose earns its place the same way
// TestRunbook_DocumentsUserScopeInstall does — nothing in code can stop the
// next session shipping a `cc` argument that no installed binary knows about,
// or rolling images before migration 0038 is applied (every live binary selects
// deliveries.*).
//
// EXPECTED RED: neither document mentions cc, and the handoff does not exist.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ccDoc(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// Criterion 22. The user-scope install is where a session in the collaboratory
// repo gets `cc` at all, and an OPEN session keeps the old binary and its old
// schema — so it will not know the argument exists.
func TestRunbook_UserScopeRecordsTheCcArgument(t *testing.T) {
	const rel = "docs/runbooks/ops-mcp-user-scope.md"
	doc := ccDoc(t, rel)
	lower := strings.ToLower(doc)

	for _, want := range []struct{ tok, why string }{
		{"cc", "the whole argument"},
		{"draft_delivery", "one of the two tools that takes it"},
		{"update_delivery", "the other, where [] clears it"},
		{"gmail", "cc is gmail only"},
		{"go install ./cmd/ops-mcp-user", "the re-install rule this ticket triggers (it touches internal/mcpserver and internal/tools)"},
		{"192.168.50.30", "the second workstation runs the same binary and needs the same re-install (IK)"},
	} {
		if !strings.Contains(lower, strings.ToLower(want.tok)) {
			t.Errorf("%s never mentions %q — %s", rel, want.tok, want.why)
		}
	}
	// The cc must be documented ON the drafting section, not merely as a word
	// somewhere: assert it appears within the same 1500 characters as
	// draft_delivery.
	i := strings.Index(lower, "draft_delivery")
	if i < 0 {
		t.Fatal("no draft_delivery section at all")
	}
	end := min(i+1500, len(lower))
	if !strings.Contains(lower[i:end], "cc") {
		t.Errorf("%s mentions cc nowhere near draft_delivery; a session reading the drafting section must learn "+
			"that it may pass one (and never on its own)", rel)
	}
}

// Criterion 23: merging is not applying. Migration 0038 goes to prod BEFORE the
// images roll, because every live binary — dashboard, orchestratord,
// pipelined, connectors, opsctl, ops-mcp-user — selects deliveries.*.
func TestHandoff_GmailDeliveryCcStatesTheOrder(t *testing.T) {
	const rel = "docs/runbooks/HANDOFF-kube-gmail-delivery-cc.md"
	doc := strings.ToLower(ccDoc(t, rel))
	steps := []struct{ frag, why string }{
		{"0038_delivery_cc.sql", "1. the 0038 migrate Job FIRST"},
		{"image", "2. the image build/tag, after the migration"},
		{"go install ./cmd/ops-mcp-user", "3. the user-scope MCP binary on both machines"},
	}
	at := 0
	for _, s := range steps {
		j := strings.Index(doc[at:], s.frag)
		if j < 0 {
			t.Errorf("%s does not mention %q after the previous step — %s", rel, s.frag, s.why)
			continue
		}
		at += j + len(s.frag)
	}
	for _, want := range []string{"192.168.50.30", "dashboard"} {
		if !strings.Contains(doc, want) {
			t.Errorf("%s never names %q", rel, want)
		}
	}
}
