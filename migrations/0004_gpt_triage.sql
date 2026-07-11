-- 0004_gpt_triage.sql — SWT-6 / docs/tickets/06-gpt-triage_SPEC.md.
-- The message→project hop for triage plus the queue-filter indexes.

-- One project per client person for triage-default purposes; populated
-- manually via psql (report's UNMAPPED lane flags missing mappings).
ALTER TABLE projects ADD COLUMN client_person_id BIGINT REFERENCES people(id);

-- Queue-filter probe (NOT EXISTS per message).
CREATE INDEX ai_extractions_raw_item_idx ON ai_extractions (raw_source_item_id);

-- The filter joins extractions to triage runs to stay robust against future
-- extraction writers (step 8 drafts, step 10 plan import).
CREATE INDEX ai_runs_worker_type_idx ON ai_runs (worker_type);
