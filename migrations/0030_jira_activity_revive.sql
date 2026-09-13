-- 0030 jira-activity-revive (SWT-45, docs/tickets/jira-activity-revive_SPEC.md).
--
-- Deploy order: apply BEFORE any image built with this file runs. New code selects
-- capture_rules.revive/.addressed on every capture pass and writes tasks.closed_at on
-- every close, so a new image on a db without 0030 fails both. Old images are
-- unaffected by 0030 (docs/runbooks/HANDOFF-kube-jira-activity-revive.md).
--
-- (1) Activity is a RULE property (J1). revive: this rule's matches are Jira activity
-- (owner decision 1). addressed: they are addressed to Salvador (decision 3) and so
-- override a gated project's assignee check. overrides = revive AND (NOT gate OR
-- addressed), decided in Go (capture), never here. revive needs an explicit key_regex:
-- a key derived from a pattern's first group is how rule 10 keys by PREFIX, and a
-- reviving prefix rule would resurrect a catch-all task on every mention. The CHECK
-- asks only for SOME external_system; capture_rule_add refuses any but 'jira' (Part
-- D's hold keys on jira, so another system would bypass the gate), and capture treats
-- a non-jira reviving rule as inert.
-- Rules are armed by capture_rule_add (the executor), never by a migration.
ALTER TABLE capture_rules
  ADD COLUMN revive    BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN addressed BOOLEAN NOT NULL DEFAULT false,
  ADD CONSTRAINT capture_rules_revive_needs_key
    CHECK (NOT revive OR (external_system IS NOT NULL AND key_regex IS NOT NULL)),
  ADD CONSTRAINT capture_rules_addressed_implies_revive
    CHECK (NOT addressed OR revive);

-- (2) The close record and the surfacing record (J5). closed_at / closed_from_status
-- are written ONLY by internal/tools closeTransition (the one writer of
-- status='closed') and NULLed on reopen. No CHECK ties them to status: integration
-- fixtures INSERT closed tasks directly. No backfill: a NULL closed_at (pre-0030, or a
-- close by an old binary during rollout) makes the revive guard fall back to
-- updated_at, the task's last stamped write. On a closed task only closeTransition
-- stamps it (logs and surfacing do not), so it is the close instant for any close made
-- through the executor. It is LATER if a hand-run UPDATE touched updated_at after the
-- close (then a message ingested in between does not revive), and EARLIER if the task
-- reached 'closed' by a write that did not stamp it (hand SQL, a fixture INSERT; then a
-- message ingested in between DOES revive). surfaced_* = the last time something other than the reconciler
-- put this task on the board (activity revive, overriding-rule creation, a human's
-- plain reopen); message NULL = a human. The reconciler reads it; only executor
-- handlers write it. No index: read by primary key only.
ALTER TABLE tasks
  ADD COLUMN closed_at              TIMESTAMPTZ,
  ADD COLUMN closed_from_status     TEXT,
  ADD COLUMN surfaced_at            TIMESTAMPTZ,
  ADD COLUMN surfaced_by_message_id BIGINT REFERENCES normalized_messages(id) ON DELETE SET NULL;

-- (3) The reconciler's hold (J11). 'resurfaced' = not warranted, but activity (or a
-- human) surfaced the task after this pass last saw it; held open until status,
-- status name or assignee changes, or a human closes/dismisses it.
-- surfaced_seen_at = the tasks.surfaced_at value this pass last observed while the
-- task was open. SUPERSEDES 0023's "drop_reason NULL unless dropped": a resurfaced
-- row records the drop fact it is holding off. Drop/add is safe only because migrate
-- runs each file in one transaction (0009, 0025).
ALTER TABLE ticket_status_syncs ADD COLUMN surfaced_seen_at TIMESTAMPTZ;
ALTER TABLE ticket_status_syncs DROP CONSTRAINT ticket_status_syncs_last_action_check;
ALTER TABLE ticket_status_syncs ADD CONSTRAINT ticket_status_syncs_last_action_check
  CHECK (last_action IN ('none','closed','reopened','refused_active','suppressed_dismissed','resurfaced'));

-- Self-check (0025's): a DROP of a name Postgres did not generate would fail loudly
-- here rather than leave two CHECKs, one of which refuses 'resurfaced' at runtime.
DO $$
DECLARE n int;
BEGIN
  SELECT count(*) INTO n FROM pg_constraint
   WHERE conrelid = 'ticket_status_syncs'::regclass AND contype = 'c'
     AND pg_get_constraintdef(oid) LIKE '%last_action%';
  IF n <> 1 THEN
    RAISE EXCEPTION 'expected exactly 1 last_action CHECK on ticket_status_syncs, found %', n;
  END IF;
END $$;
