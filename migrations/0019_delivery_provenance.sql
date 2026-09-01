-- 0019 delivery provenance (SWT-20).
--
-- tasks.source_thread_id records WHICH CONVERSATION RAISED A TASK — a recorded
-- observation, written only by task_set_source_thread (spine tool, not
-- MCP-listed), read by drafts targeting and the draft_delivery binding. An FK
-- to normalized_threads survives a thread re-key and allows many tasks per
-- conversation, which external_refs (UNIQUE (system, external_key)) cannot.
-- NULL means "no provenance": the state of every pre-existing task, and no
-- backfill is possible — nothing recorded which message raised them.
ALTER TABLE tasks ADD COLUMN source_thread_id BIGINT REFERENCES normalized_threads(id);

-- deliveries.target_client_ref is the delivery's client identity, extracted in
-- GO by the same ParseThreadKey call that produced the stored target_ref (one
-- spelling; no SQL builds or dissects a key). It is what the upwork confirm
-- matcher shortlists on BEFORE locking anything.
ALTER TABLE deliveries ADD COLUMN target_client_ref TEXT;

-- The CHECK is what makes the shortlist safe rather than hopeful: an
-- upwork_chat row missing its identity would not error — it would silently
-- drop out of the candidate set and the delivery could never confirm. Free at
-- zero rows (production has never had an upwork_chat delivery; verify
-- `SELECT count(*) FROM deliveries WHERE channel='upwork_chat'` = 0 before
-- applying), impossible to add later without a backfill.
ALTER TABLE deliveries ADD CONSTRAINT deliveries_upwork_identity_check
  CHECK (channel <> 'upwork_chat' OR (target_client_ref IS NOT NULL AND thread_id IS NOT NULL));

-- The shortlist's partial index: unresolved upwork deliveries by client.
CREATE INDEX deliveries_upwork_unconfirmed_idx ON deliveries (target_client_ref)
  WHERE channel = 'upwork_chat' AND status = 'sent'
    AND sent_external_id IS NULL AND confirmed_at IS NULL;
