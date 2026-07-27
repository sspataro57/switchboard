-- 0009: Slack Web connector assisted reply delivery channel.
-- Slack replies remain delivery-governed but are never sent automatically:
-- prefill_delivery writes an approved body into the browser composer and a
-- human performs the send before mark_delivery_sent.

ALTER TABLE deliveries DROP CONSTRAINT deliveries_channel_check;
ALTER TABLE deliveries ADD CONSTRAINT deliveries_channel_check CHECK (channel IN
  ('gmail','jira_comment','upwork_chat','calendar','github_review','slack_reply'));
