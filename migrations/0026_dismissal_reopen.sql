-- 0026 — dismiss-reopen-on-activity (SWT-36, docs/tickets/dismiss-reopen-on-activity_SPEC.md).
--
-- A dismissed task reopens when a new INBOUND message is ingested for it. The
-- dismissal row is KEPT (labelled data, SWT-31) and stamped when overtaken, and
-- one task may now carry several dismissal rows over time: at most ONE open.
--
--   closed_from_status      D5: the status task_dismiss closed the task from; the
--                           reopen restores it (openStatuses) or falls back to
--                           ready. NULL = the task was already closed at dismiss
--                           time, or the row predates 0026 (no backfill: the only
--                           source is task_events jsonb, which SWT-31/32 forbid
--                           mining). No CHECK: the handler's fallback is the
--                           guard and openStatuses stays the one spelling.
--   reopened_at/_by         D6: NULL = the dismissal is OPEN. Set by every
--                           successful reopen of a task with an open dismissal.
--   reopened_by_message_id  D6: the inbound message that overtook it (activity
--                           reopen); NULL for a human plain reopen. ON DELETE SET
--                           NULL, not CASCADE: deleting a message must never
--                           delete a human label.
--
-- LANDMINE (IK partial-index rule): task_dismissals_task_uniq was TOTAL; its
-- replacement is PARTIAL. Every ON CONFLICT against task_dismissals must RESTATE
-- `WHERE reopened_at IS NULL`, or Postgres raises "no unique or exclusion
-- constraint matching the ON CONFLICT specification" at runtime.
--
-- Migrate runs this file in one transaction: the drop, the backfill and the
-- create are atomic.

ALTER TABLE task_dismissals
  ADD COLUMN closed_from_status     TEXT,
  ADD COLUMN reopened_at            TIMESTAMPTZ,
  ADD COLUMN reopened_by            TEXT,
  ADD COLUMN reopened_by_message_id BIGINT REFERENCES normalized_messages(id) ON DELETE SET NULL,
  ADD CONSTRAINT task_dismissals_reopen_pair CHECK ((reopened_at IS NULL) = (reopened_by IS NULL)),
  ADD CONSTRAINT task_dismissals_reopen_msg  CHECK (reopened_by_message_id IS NULL OR reopened_at IS NOT NULL);

-- D14: an open dismissal must imply a closed task. Rows on tasks reopened by
-- psql or by SWT-32's task_reopen before this ticket are stamped, so a
-- re-dismissal of such a task does not conflict on the partial index and lose
-- its label. Measured on prod before apply (SPEC Verification §4 query b).
UPDATE task_dismissals d SET reopened_at = now(), reopened_by = 'migration:0026'
  FROM tasks t WHERE t.id = d.task_id AND t.status <> 'closed' AND d.reopened_at IS NULL;

DROP INDEX task_dismissals_task_uniq;
CREATE UNIQUE INDEX task_dismissals_open_uniq ON task_dismissals (task_id) WHERE reopened_at IS NULL;
