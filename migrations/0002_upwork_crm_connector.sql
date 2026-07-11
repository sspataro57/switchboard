-- 0002_upwork_crm_connector.sql — SWT-2 / docs/tickets/02-upwork-crm-connector_SPEC.md.
-- Forward-only extension of 0001 for the first connector (upwork_crm poller).

-- Per-account sync position (this step: communications cursor; step 7 reuses it
-- for Gmail historyId / Calendar syncToken).
ALTER TABLE source_accounts ADD COLUMN sync_cursor JSONB NOT NULL DEFAULT '{}';

-- Carried from upwork_crm.communications; 0001 deliberately left normalized_*
-- minimal for the first writer (this step) to extend.
ALTER TABLE normalized_messages
  ADD COLUMN subject TEXT,
  ADD COLUMN sender  TEXT,
  ADD COLUMN channel TEXT;

-- One message per raw item: the upsert target that makes normalization idempotent.
CREATE UNIQUE INDEX normalized_messages_raw_item_idx
  ON normalized_messages (raw_source_item_id);

-- Thread upsert target.
CREATE UNIQUE INDEX normalized_threads_thread_key_idx
  ON normalized_threads (thread_key) WHERE thread_key IS NOT NULL;
