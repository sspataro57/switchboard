-- 0005_google_connector.sql — SWT-7 / docs/tickets/07-google-oauth-pollers_SPEC.md.

-- Calendar payload fields the first events writer needs (0001 left
-- normalized_events minimal for this step to extend).
ALTER TABLE normalized_events
  ADD COLUMN title        TEXT,
  ADD COLUMN status       TEXT,
  ADD COLUMN transparency TEXT,
  ADD COLUMN all_day      BOOLEAN NOT NULL DEFAULT false;

-- One event per raw item: upsert target (mirror of normalized_messages_raw_item_idx).
CREATE UNIQUE INDEX normalized_events_raw_item_idx
  ON normalized_events (raw_source_item_id);

-- Cross-account Message-ID dedup: the mechanical guarantee behind criterion 9.
CREATE UNIQUE INDEX normalized_messages_gmail_msgid_idx
  ON normalized_messages (external_message_id) WHERE channel = 'gmail';
