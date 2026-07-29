-- 0011_slack_send_promotion.sql — SWT-12 / docs/tickets/slack-send-promotion_SPEC.md.
--
-- Promoting slack_reply from assisted to approve creates two ways a message
-- leaves the system, with two different authorities deciding it may:
--
--   'switchboard' — switchboard drafted it, approve_delivery gated it, and
--                   policy_decisions carries that decision. The delivery row IS
--                   the approval of record.
--   'leaf_token'  — Salvador told an interactive session to post; the Slack Web
--                   connector's own short-lived token gated it and the message
--                   was already in the channel before switchboard heard. The row
--                   is a RECORD, not a decision, and policy never gated it.
--
-- Both end as status='sent', which is exactly the problem: without this column
-- the two are indistinguishable, and the safe-looking reading of the deliveries
-- table — that everything in it passed switchboard policy — would be false.
-- That is the question asked first when a message looks wrong, so the answer
-- lives in a plain indexed column rather than inside policy_result's jsonb,
-- where nothing currently writes and no existing query reads.
--
-- Nullable, and no CHECK constraint: a third authority is plausible later (an
-- earned 'auto' tier), and the SWT-8 convention validates delivery enums in the
-- handler rather than the schema.

ALTER TABLE deliveries ADD COLUMN approval_source TEXT;

-- Backfill is unambiguous: every delivery that reached the world before this
-- migration did so through approve_delivery, because send_delivery accepted
-- nothing else. Rows still in flight (drafted/failed) are left NULL — their
-- authority is not yet decided, and approve_delivery will stamp it.
UPDATE deliveries
   SET approval_source = 'switchboard'
 WHERE status IN ('sending', 'sent')
   AND approval_source IS NULL;

-- No index. deliveries is small and queried by status (deliveries_status_idx,
-- 0006) or by id; approval_source is read alongside a row already located, and
-- audit queries over it are interactive rather than hot.
