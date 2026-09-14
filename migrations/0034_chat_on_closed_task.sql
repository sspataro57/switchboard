-- 0034 chat-on-closed-task (SWT-53, docs/tickets/chat-on-closed-task_SPEC.md).
--
-- A human message that capture logs onto a task that is already CLOSED used to
-- disappear: the log line lands on a task nobody looks at, and the inquiry lane
-- read only `attributed` messages. This migration adds the two facts that fix it.
--
-- CC3: capture_decisions.resurface is CAPTURE'S RECORDED FACT. insertDecision
-- writes it from the pure resurfaces() (internal/capture/resurface.go): true iff
-- the action is task_log, the linked task was closed AT LOG TIME, the winner is
-- not an activity (SWT-45 revive) match, the task has no open dismissal (SWT-36),
-- the message is not the Jira connector's own copy (SWT-45 J3), the sender is
-- not on the project's notifier list, and the sender is not blank (CC4b: no
-- identity fails closed). The inquiry lanes only READ it, through
-- replyfold.InquiryEligibleLatestSQL, which re-reads "still closed" and reads
-- it from the message's latest LIVE decision only (CC5b). Status at
-- log time is a fact only capture sees, so it is recorded, never recomputed.
-- DEFAULT false: every writer that does not name it (the gate stage, the route
-- stage) records false, which is the CC8 residual. The CHECK pins it to task_log.
--
-- CC4: projects.notifier_senders is per-project CONFIGURATION, the 0025
-- ticket_delivered_statuses shape: exact identities of bot / notification
-- senders (a Slack display name such as Jira, or a mail ADDRESS such as
-- jira@treetopllc.jira.com). Matching is EQUALITY after trim + case-fold against
-- the whole stored sender or the address parsed from it, never a substring. It is
-- read only by capture's loadRules. DEFAULT '{}' is today's behaviour; NOT NULL
-- because of 0018's nullable trap. No seeding UPDATE here: seeding is an
-- operator act (docs/runbooks/capture-rules.md).
--
-- CC10: apply 0034 BEFORE any image built from this branch. New capture binaries
-- select p.notifier_senders and write resurface on every pass; on a db without
-- 0034 every connector's capture pass fails. Seed the list after 0034 and before
-- the images roll, then roll ONE tag to every capture writer and inquiry reader in
-- ONE apply: an old capture binary on this db records resurface=false for good
-- (docs/runbooks/HANDOFF-kube-chat-on-closed-task.md).
--
-- No index: projects is tens of rows read by primary key, and resurface is read
-- only on the one latest decision row per message. No backfill (CC9).
ALTER TABLE projects ADD COLUMN notifier_senders TEXT[] NOT NULL DEFAULT '{}';

ALTER TABLE capture_decisions ADD COLUMN resurface BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE capture_decisions ADD CONSTRAINT capture_decisions_resurface_is_task_log
  CHECK (NOT resurface OR action = 'task_log');
