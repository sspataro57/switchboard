-- SWT-40 Part D (inquiry-promote): the capture-time Jira assignee gate.
--
-- capture records a new action, 'held', for a jira-keyed match on a project
-- whose ticket_assignee_gate is on, and creates nothing; the pipelined `gate`
-- stage resolves the hold into a SECOND row, mode='gate' (task | task_log |
-- attributed), after the ticket has been looked up. The live claim is spent by
-- the held row and held acted on nothing, so the gate row is the message's one
-- ACTION: "one live action per message" still holds and capture_decisions_live_uniq
-- is untouched.
--
-- 0015 declared both CHECKs inline; Postgres named them
-- capture_decisions_action_check and capture_decisions_mode_check (confirmed on
-- the compose db via pg_constraint; confirm against prod before applying, as
-- 0015 did). Widen them: every existing value kept. Additive — no row changes.
--
-- Old binaries are unaffected: they never write 'held' or 'gate', and their
-- conflict target (WHERE mode='live') is unchanged. Deploy order: apply BEFORE
-- the connector images that write 'held', or every gated match fails the CHECK.
--
-- Numbered 0029: the SPEC's 0027 went to SWT-39, and 0028 is claimed by SWT-43.
ALTER TABLE capture_decisions DROP CONSTRAINT capture_decisions_action_check;
ALTER TABLE capture_decisions ADD CONSTRAINT capture_decisions_action_check
  CHECK (action IN ('unmatched','attributed','task','task_log','held'));
ALTER TABLE capture_decisions DROP CONSTRAINT capture_decisions_mode_check;
ALTER TABLE capture_decisions ADD CONSTRAINT capture_decisions_mode_check
  CHECK (mode IN ('shadow','live','gate'));
ALTER TABLE capture_decisions ADD CONSTRAINT capture_decisions_gate_shape CHECK (
  mode <> 'gate' OR (action IN ('task','task_log','attributed')
                     AND matched_rule_id IS NOT NULL AND external_system IS NOT NULL
                     AND external_key IS NOT NULL));
ALTER TABLE capture_decisions ADD CONSTRAINT capture_decisions_held_is_not_gate CHECK (
  action <> 'held' OR mode IN ('shadow','live'));
-- The gate row's task_id: 'attributed' names no task. 'task' and 'task_log' are
-- left free: both are claimed with task_id NULL BEFORE the executor acts
-- (claim-before-act: create_task, or task_append_log plus the guarded reopen)
-- and completed with their task_id afterwards, so a NULL there is the in-flight
-- state (and, if it outlives the pass, the report's "claimed with no task" line).
ALTER TABLE capture_decisions ADD CONSTRAINT capture_decisions_gate_task_pin CHECK (
  mode <> 'gate' OR action <> 'attributed' OR task_id IS NULL);
-- One resolution per message, forever. PARTIAL: every ON CONFLICT restates WHERE mode = 'gate'.
CREATE UNIQUE INDEX capture_decisions_gate_uniq ON capture_decisions (message_id) WHERE mode = 'gate';
