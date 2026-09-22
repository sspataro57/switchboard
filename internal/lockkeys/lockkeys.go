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

// SlackWatch is the resident Slack watcher's single-instance lock (SWT-75
// D9): connector-slackweb-watch holds it as a session lock for its lifetime
// (a second replica stands by rather than crashing), and the one-shot
// connector-slackweb CronJob probes it at startup and stands down while it is
// held (D4: the CronJob is a net, never a co-worker on the mini's one browser).
const SlackWatch int64 = 0x5157_0011
