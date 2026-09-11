package orchestrator_test

// SWT-41 D1 "Lock key, one spelling". orchestrator_cursor_advance (in
// internal/tools) refuses while an orchestratord runs by taking
// pg_try_advisory_xact_lock on the ORCHESTRATOR's key. internal/tools must not
// import internal/orchestrator (this package's integration test imports
// internal/tools — a cycle in the test build), so the tool cannot read
// orch.AdvisoryLockKey directly. The SPEC allows a leaf both import, or two
// constants pinned equal by a unit test; either way tools exposes the key it
// uses, and this test pins the two equal. Never two unpinned literals: a drift
// makes the advance's guard check a key nobody holds, and it would then skip
// events under a RUNNING engine.
//
// The literal is deliberately NOT restated here (internal/classify's repo-wide
// collision scan reads a restated 0x5157 literal as a second owner).
//
// IMPOSED SURFACE (name chosen here):
//
//	// internal/tools — the key the cursor-advance guard locks. Either its own
//	// literal or an alias of a leaf-package constant.
//	const OrchestratorAdvisoryLockKey int64 = …
//
// Behavioural twin: internal/tools/orchestratorcursor_integration_test.go holds
// orch.AdvisoryLockKey on a separate connection and requires the advance to
// refuse.
//
// GREENFIELD NOTE — EXPECTED RED: tools.OrchestratorAdvisoryLockKey does not
// exist, so this package's unit test build compile-FAILS.

import (
	"testing"

	orch "github.com/sspataro57/switchboard/internal/orchestrator"
	"github.com/sspataro57/switchboard/internal/tools"
)

func TestOrchestratorLockKey_ToolsGuardUsesTheEngineKey(t *testing.T) {
	if int64(tools.OrchestratorAdvisoryLockKey) != int64(orch.AdvisoryLockKey) {
		t.Errorf("tools.OrchestratorAdvisoryLockKey = %#x, orchestrator.AdvisoryLockKey = %#x. The cursor "+
			"advance must lock the key a running orchestratord holds, or 'refuses while running' is a no-op",
			int64(tools.OrchestratorAdvisoryLockKey), int64(orch.AdvisoryLockKey))
	}
}
