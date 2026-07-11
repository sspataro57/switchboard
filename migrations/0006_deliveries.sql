-- 0006_deliveries.sql — SWT-8 / docs/tickets/08-draft-deliveries_SPEC.md.

ALTER TABLE deliveries
  ADD COLUMN subject         TEXT,
  ADD COLUMN from_account_id BIGINT REFERENCES source_accounts(id),
  ADD COLUMN thread_id       BIGINT REFERENCES normalized_threads(id),
  ADD COLUMN created_by      TEXT,
  ADD COLUMN error           TEXT,
  ADD COLUMN sent_at         TIMESTAMPTZ,
  ADD COLUMN confirmed_at    TIMESTAMPTZ;
CREATE INDEX deliveries_status_idx ON deliveries (status);

-- Global runtime flags (kill switch lives here: row 'sending_frozen').
CREATE TABLE ops_flags (
  name       TEXT PRIMARY KEY,
  value      JSONB NOT NULL DEFAULT '{}',
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
