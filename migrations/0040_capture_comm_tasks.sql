-- 0040 comms inbox (SWT-74, comms-inbox): capture_rules.comm_task and
-- capture_decisions.comm_task_id.
--
-- Rollout BARRIER: apply this BEFORE rolling an image built from SWT-74.
-- Every capture pass built from that branch selects capture_rules.comm_task
-- (loadRules), so a new image on a pre-0040 db fails capture for EVERY
-- connector (the 0034/0035 precedent) — and task_match (dashboard, ops-mcp-user)
-- reads the same rules, so every workload needs it. An old image ignores both columns, so
-- applying first is always safe and rolling first is not.
--
-- comm_task: this rule's matches from a PERSON, filed onto an OPEN task, become
-- their OWN task in INCOMING (a "comm" to answer, route or dismiss) instead of
-- a silent log line. Defaults FALSE: with nothing armed the funnel behaves
-- byte-identically to 0.7.39. Arming is capture_rule_add --comm-task or one
-- UPDATE by hand, never this migration. The CHECK is the data-level half of
-- the rule: a keyless rule can never reach the task_log branch (an armed
-- keyless rule would be an inert flag), and GitHub notification mail is a
-- notice stream with its own review tasks (SWT-54).
--
-- comm_task_id: the comm task a task_log decision created, recorded on
-- capture's own log BEFORE the follow-ups (the live claim is spent, so a later
-- failure must not lose the pointer). A comm decision IS a task_log — no new
-- action value, or every latest-decision reader would break.
--
-- No index, no backfill, no other table, no rule armed. Idempotent.
ALTER TABLE capture_rules
  ADD COLUMN IF NOT EXISTS comm_task BOOLEAN NOT NULL DEFAULT false;
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'capture_rules_comm_task_shape'
                   AND conrelid = 'capture_rules'::regclass) THEN
    ALTER TABLE capture_rules
      ADD CONSTRAINT capture_rules_comm_task_shape
      CHECK(NOT comm_task OR(external_system IS NOT NULL AND NOT pr_review));
  END IF;
END $$;
ALTER TABLE capture_decisions
  ADD COLUMN IF NOT EXISTS comm_task_id BIGINT REFERENCES tasks(id);
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'capture_decisions_comm_task_is_task_log'
                   AND conrelid = 'capture_decisions'::regclass) THEN
    ALTER TABLE capture_decisions
      ADD CONSTRAINT capture_decisions_comm_task_is_task_log
      CHECK(comm_task_id IS NULL OR action = 'task_log');
  END IF;
END $$;
