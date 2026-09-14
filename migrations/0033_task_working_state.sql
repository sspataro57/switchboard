-- 0033 board-status-lights (docs/tickets/board-status-lights_SPEC.md).
-- A Claude session's STATE SIGNAL on a human task: an annotation, not a status. Nothing
-- routes on it, no claim backs it, the orchestrator never reads it. Written ONLY by
-- internal/tools task_signal (set / refresh / clear), closeTransition (cleared on a
-- real close and on every reopen) and task_claim (cleared on a claim).
-- working_state_at = the last signal; a 'working' state older than
-- tools.WorkingLease is shown stale at READ time — no sweep writes it. Nullable, no
-- default, no backfill: fixtures that INSERT tasks without naming these get NULL = no
-- signal. No index: read by primary key and over ready tasks only.
-- Named CHECKs so a later ticket can widen the state set, or add a nullable link to a
-- feedback_requests row beside the pair (answer recording is deferred: SPEC D14).
-- Deploy order: apply BEFORE any image built with this file runs — closeTransition
-- writes these columns on every close, in every image that closes (the 0030 rule).
ALTER TABLE tasks
  ADD COLUMN working_state    TEXT,
  ADD COLUMN working_state_at TIMESTAMPTZ,
  ADD CONSTRAINT tasks_working_state_check CHECK (working_state IN ('working','needs_input')),
  ADD CONSTRAINT tasks_working_state_pair  CHECK ((working_state IS NULL) = (working_state_at IS NULL));
