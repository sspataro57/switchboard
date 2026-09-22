-- 0042 slack send queue (SWT-76, slack-send-queue).
--
-- A Slack reply the leaf ACCEPTED while its browser was busy (HTTP 202) is a
-- `sending` row whose attempt has not settled — no new status (D4). These two
-- nullable columns record the acceptance so the dashboard can say "queued on
-- the bridge" with the leaf's job id, and so the row and the mini's log line
-- can be tied together by hand. NULL means "this send was never queued", which
-- is true of every row that exists today: no backfill, no default.
--
-- No index: rows are located by id or by the dashboard's bounded scan (the
-- 0012 argument). Nothing else is touched — deliveries.status' CHECK stays
-- as it is, and policy_result stays the policy matrix's verdict.
--
-- Rollout ORDER: apply before rolling the image. The new send path writes
-- these columns on a 202 and the dashboard reads them on every /deliveries
-- render; an old image never touches them, so applying first is always safe.
-- Idempotent.
ALTER TABLE deliveries ADD COLUMN IF NOT EXISTS send_queued_at TIMESTAMPTZ;
ALTER TABLE deliveries ADD COLUMN IF NOT EXISTS send_queue_job_id TEXT;
