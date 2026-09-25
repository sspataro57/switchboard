-- 0045 always-task (swb 703): capture_rules.always_task.
--
-- Salvador, 2026-09-25: mail from Pines Property Management "should always pop
-- in incoming as personal". Rule 19 attributed it to personal, and the personal
-- lane's local classifier judged each notice "informational", so no task was
-- made. An owner-declared "always actionable" class is decided in CAPTURE,
-- before any model (the SWT-78 rule): a message an always_task rule attributes
-- becomes a task on its thread's open task (one task per thread), in INCOMING,
-- through the same path a Slack DM takes.
--
-- Keyless rules only: a rule with an external_system already makes or logs onto
-- its key's task, so the flag there would be inert (CHECK below).
--
-- Rollout BARRIER: apply this BEFORE rolling an image that reads it. loadRules
-- selects capture_rules.always_task, so a new image on a pre-0045 db fails
-- capture for every connector (the 0034/0035/0040 precedent). An old image
-- ignores the column, so applying first is always safe.
--
-- Defaults FALSE: nothing is armed by this migration. Idempotent.
ALTER TABLE capture_rules
  ADD COLUMN IF NOT EXISTS always_task BOOLEAN NOT NULL DEFAULT false;
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'capture_rules_always_task_keyless') THEN
    ALTER TABLE capture_rules ADD CONSTRAINT capture_rules_always_task_keyless
      CHECK (NOT always_task OR external_system IS NULL);
  END IF;
END $$;
