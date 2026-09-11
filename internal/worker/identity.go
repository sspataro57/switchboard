package worker

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sspataro57/switchboard/internal/policy"
)

// ValidateWorkerID refuses a worker identity that policy would read as a human
// session, or that already carries the MCP transport prefix (SWT-37, Codex
// review). The MCP adapter builds Actor = "mcp:" + OPS_WORKER_ID, and
// policy.HumanActor strips one "mcp:" and trusts manual:/dashboard:/opsctl:, so
// a console launched as --client manual:foo would otherwise pass every human
// gate (approve/send since SWT-11, the task verbs since SWT-37) as
// mcp:manual:foo. Client names are free text ("Saka Technologies (Mario
// Cruz)"), so only the reserved shapes are refused, each through the policy's
// own spelling so the two cannot drift.
func ValidateWorkerID(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("worker id is empty")
	}
	if policy.ViaMCPActor(id) {
		return fmt.Errorf("worker id %q carries the MCP transport prefix %q, which the adapter adds itself",
			id, policy.MCPTransportPrefix)
	}
	if policy.HumanActor(policy.MCPTransportPrefix + id) {
		return fmt.Errorf("worker id %q would read as a human session identity (manual:/dashboard:/opsctl:); "+
			"a worker console must never pass a human gate", id)
	}
	return nil
}
