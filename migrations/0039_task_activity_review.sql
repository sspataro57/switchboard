-- 0039 tasks.activity_at / activity_by_message_id / reviewed_at (SWT-72, activity-resurfaces).
--
-- Rollout BARRIER: apply this BEFORE rolling an image built from SWT-72. The new
-- binaries select these columns on the board's first statement and in the
-- capture pass; an old image ignores them, so applying first is always safe and
-- rolling first is not.
--
-- What the columns mean, for the board only:
--   activity_at            the last inbound message a capture rule filed onto
--                          this OPEN task as a log line (a Jira comment, a direct
--                          email, a Slack message) — the fact that something new
--                          arrived that nobody has looked at.
--   activity_by_message_id that message, for the "from whom" line on the board.
--                          ON DELETE SET NULL, the surfaced_by_message_id spelling.
--   reviewed_at            the last human (or interactive Claude) review: the
--                          Requeue verb, or a close. A MONOTONE stamp, never a
--                          cleared activity_at, so "needs review" is simply
--                          activity_at > reviewed_at on an open task.
--
-- Deliberately NOT surfaced_at (0030): the Jira ticket-status reconciler reads
-- that one and would hold every commented-on closed ticket's task open forever.
-- These are a parallel, board-facing pair the reconciler never reads.
--
-- NO backfill, on purpose: every existing row is NULL, so nothing appears in
-- INCOMING at rollout — only activity that arrives after the roll surfaces.
-- No index: read by primary key and per displayed row. Idempotent.
ALTER TABLE tasks
  ADD COLUMN IF NOT EXISTS activity_at            TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS activity_by_message_id BIGINT REFERENCES normalized_messages(id) ON DELETE SET NULL,
  ADD COLUMN IF NOT EXISTS reviewed_at            TIMESTAMPTZ;
