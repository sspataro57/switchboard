-- 0025 per-project delivered statuses (SWT-34).
--
-- projects.ticket_delivered_statuses is the set of Jira status NAMES that mean
-- "I delivered this; the ball is in someone else's court" for THAT project —
-- a client's QA column being the case that prompted it. It is the third clause
-- of internal/ticketstatus's existing `warranted` predicate, which is why the
-- return path comes free: a ticket that LEAVES the set is warranted again and
-- the reconciler's existing reopen restores the task to the status it held.
--
-- WHY NAMES HERE AND NOT IN CODE, given SWT-32's D2 bans a status-name list:
-- D2 answers "is this ticket finished", which Jira itself owns as a three-value
-- statusCategory. This answers "is the ball in my court", which no function of
-- statusCategory can express it: a client's QA column and their
-- work-in-progress column are both `indeterminate`, yet one means "I handed
-- it back" and the other means "I am mid-build".
-- `indeterminate`. A workflow-shaped fact belongs in CONFIGURATION, never in
-- the binary: a typed column, per project, hand-armed, with the same shape and
-- polarity as ai_locality (0016), ai_classify (0018), classify_promote_after
-- (0021) and ticket_assignee_gate (0023). When a client renames their column,
-- the failure is a task that stays on the board and an operator who edits one
-- row — not a silent stop needing a code change and a deploy.
--
-- DEFAULT '{}' is today's behaviour EXACTLY: the clause is inert for every
-- project until someone arms it, and disarming is the same one UPDATE back,
-- after which the next pass puts the tasks it dropped back on the board. NOT
-- NULL because 0018 recorded the nullable trap (`AND p.ai_classify` silently
-- excluding every row nobody set). No arming UPDATE here: arming is an operator
-- act recorded in the runbook — a migration that armed collaboratory would drop
-- eight tasks off a live board as a deploy side effect, at whatever hour the
-- rollout happened. No index: `projects` holds tens of rows and every reader
-- reaches it by primary key.
ALTER TABLE projects ADD COLUMN ticket_delivered_statuses TEXT[] NOT NULL DEFAULT '{}';

-- drop_reason gains its third value. 0023 wrote the CHECK inline, so Postgres
-- generated the name below; the DROP names it explicitly and the ADD re-uses
-- it, so a future reader greps ONE name and finds both halves (0009's
-- deliveries_channel_check precedent).
--
-- The drop/add pair leaves the column unconstrained BETWEEN the two statements,
-- and that is safe here ONLY because the migrate runner executes each file
-- inside one transaction. A reader copying this pattern into a runner without
-- that property would ship a window where any string is storable.
ALTER TABLE ticket_status_syncs DROP CONSTRAINT IF EXISTS ticket_status_syncs_drop_reason_check;
ALTER TABLE ticket_status_syncs ADD CONSTRAINT ticket_status_syncs_drop_reason_check
  CHECK (drop_reason IN ('ticket_done','ticket_delivered','not_assigned'));

-- SELF-CHECK, because the failure this guards against is SILENT. A DROP
-- CONSTRAINT against a name Postgres did not generate is a no-op: the migration
-- succeeds, every structural scan of the SQL text above passes, the old
-- two-value CHECK survives beside the new one, and the first real
-- ticket_delivered insert then fails at runtime — every tick, on the same ref,
-- months later. COUNTING is the assertion that catches it: "at least one CHECK
-- exists" is exactly what passes in the broken case.
DO $$
DECLARE n int;
BEGIN
  SELECT count(*) INTO n
    FROM pg_constraint
   WHERE conrelid = 'ticket_status_syncs'::regclass
     AND contype = 'c'
     AND pg_get_constraintdef(oid) LIKE '%drop_reason%';
  IF n <> 1 THEN
    RAISE EXCEPTION 'expected exactly 1 CHECK constraint mentioning drop_reason on ticket_status_syncs, found %', n;
  END IF;
END $$;

-- 0023 says of ticket_status_syncs.status_name: "DIAGNOSTIC only (D2) — nothing
-- branches on it." That file is applied and must not be edited, so the
-- correction lives here: FROM THIS MIGRATION ON, a per-project configured set
-- of status names MAY branch on it, through ticket_delivered_statuses above.
-- The name is still never a discriminator in code — only in data.
