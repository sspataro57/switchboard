-- 0013_task_events_indexes.sql — SWT-16 review findings.
-- docs/tickets/outbound-capture_SPEC.md said "no migration"; review found two
-- costs in task_events that only grow, and both are index-shaped.
--
-- 1. task_events has carried NO index on task_id since 0001. Every reader that
--    asks "what happened to task N" is a sequential scan of a table that gains a
--    row for every claim, log line, status change, and confirmation and is never
--    pruned. The orchestrator's Facts query already pays it. SWT-16's capture pass
--    multiplies it: its duplicate check runs once per (linked task, candidate
--    message) on every connector poll, so one Gmail pass over a 30-day horizon
--    across five mailboxes is thousands of full scans.
--
-- 2. That duplicate check was INSERT ... SELECT ... WHERE NOT EXISTS, which is not
--    atomic: two passes on the same channel — a manual `--normalize-only --all`
--    while the CronJob fires — can both read "absent" and both insert. The guard
--    was advisory where confirmDelivery's (UPDATE ... WHERE sent_external_id IS
--    NULL, then check RowsAffected) is structural. A unique index makes it
--    structural here too, and lets the insert say ON CONFLICT DO NOTHING.
--
-- The unique index is PARTIAL, on event_type='outbound_observed' only. Other event
-- types legitimately repeat on a task (many delivery_confirmed rows, many
-- status_changed rows), so a table-wide uniqueness rule would be wrong; and a
-- partial index stays small, covering only the rows capture writes.

-- Defensive: the event type ships with this ticket and no deployment has written
-- one, but a development database may hold rows from an integration run against
-- the pre-index insert. Keep the earliest observation per (task, message) so the
-- unique index below can be created; ordering by id makes the survivor
-- deterministic rather than whichever row the planner happened to return.
DELETE FROM task_events e
 WHERE e.event_type = 'outbound_observed'
   AND EXISTS (
     SELECT 1 FROM task_events keep
      WHERE keep.event_type = 'outbound_observed'
        AND keep.task_id = e.task_id
        AND keep.payload->>'message_id' = e.payload->>'message_id'
        AND keep.id < e.id
   );

CREATE INDEX task_events_task_id_idx ON task_events (task_id);

CREATE UNIQUE INDEX task_events_outbound_observed_uniq
  ON task_events (task_id, (payload->>'message_id'))
  WHERE event_type = 'outbound_observed';
