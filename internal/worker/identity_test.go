package worker

// SWT-37 criterion 27 (Codex review): a worker console cannot be CONFIGURED to
// pose as a human session. The adapter's Actor is "mcp:" + OPS_WORKER_ID, and
// WriteMCPConfig is where a worker's ops-mcp gets it, so refusing the reserved
// shapes there closes `opsworker --client manual:foo`. It is an operator
// misconfiguration guard, not a boundary against a hostile model (a worker runs
// with full permissions and DATABASE_URL). ZERO I/O beyond a temp dir.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sspataro57/switchboard/internal/policy"
)

func TestValidateWorkerID(t *testing.T) {
	for _, tc := range []struct {
		id string
		ok bool
	}{
		{"acme", true},
		{"acme.main", true},
		{"Saka Technologies (Mario Cruz)", true}, // real clients are free text
		{"MANUAL:x", true},                       // HumanActor is case-sensitive: not human
		{"", false},
		{"   ", false},
		{"manual:foo", false},
		{"dashboard:salvo", false},
		{"opsctl:salvo", false},
		{"mcp:acme", false},
		{"mcp:manual:salvo", false},
	} {
		err := ValidateWorkerID(tc.id)
		if (err == nil) != tc.ok {
			t.Errorf("ValidateWorkerID(%q) = %v, want ok=%v", tc.id, err, tc.ok)
		}
		// The property that matters, stated directly: every id it accepts must
		// produce an actor the policy does NOT treat as human.
		if err == nil && policy.HumanActor(policy.MCPTransportPrefix+tc.id) {
			t.Errorf("ValidateWorkerID accepted %q, yet mcp:%s passes policy.HumanActor", tc.id, tc.id)
		}
	}
}

func TestWriteMCPConfig_RefusesAHumanShapedWorkerID(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteMCPConfig(dir, "/bin/ops-mcp", "postgres://x", "manual:foo"); err == nil {
		t.Fatal("WriteMCPConfig accepted worker id manual:foo: the console's actor would be mcp:manual:foo, " +
			"which passes every human gate")
	}
	if _, err := os.Stat(filepath.Join(dir, "opsworker-mcp.json")); !os.IsNotExist(err) {
		t.Errorf("WriteMCPConfig wrote a config for a refused worker id (stat err %v)", err)
	}
	if _, err := WriteMCPConfig(dir, "/bin/ops-mcp", "postgres://x", "acme"); err != nil {
		t.Errorf("WriteMCPConfig refused a plain client id: %v", err)
	}
}
