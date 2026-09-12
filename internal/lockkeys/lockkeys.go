// Package lockkeys holds advisory-lock keys that more than one package must
// agree on. It imports nothing, so any package may depend on it without
// widening its own dependency graph (SWT-41: internal/orchestrator must not
// reach the provider adapter or the connectors through internal/tools just to
// share one constant — invariant 7).
package lockkeys

// Orchestrator is orchestratord's single-instance lock. The engine holds it as
// a session lock for its lifetime; orchestrator_cursor_advance takes it as a
// transaction lock, so it refuses while an engine runs.
const Orchestrator int64 = 0x5157_0005 // "switchboard step 5"
