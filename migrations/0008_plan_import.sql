-- 0008: plan import (SPEC 10-plan-import).
-- Plan-import proposal bookkeeping (NOT a task store — invariant 2: the
-- approved tree materializes as rows in tasks; this table only gates it).

CREATE TABLE plan_imports (
  id                 BIGSERIAL PRIMARY KEY,
  project_id         BIGINT NOT NULL REFERENCES projects(id),
  source_path        TEXT NOT NULL,
  content_hash       TEXT NOT NULL,
  raw_source_item_id BIGINT NOT NULL REFERENCES raw_source_items(id),
  ai_run_id          BIGINT NOT NULL REFERENCES ai_runs(id),
  ai_extraction_id   BIGINT NOT NULL REFERENCES ai_extractions(id),
  status             TEXT NOT NULL DEFAULT 'proposed'
                     CHECK (status IN ('proposed','approved','rejected','applied')),
  result             JSONB,          -- {"tasks":{ref:task_id}} once applied
  decided_by         TEXT,
  decided_at         TIMESTAMPTZ,
  applied_at         TIMESTAMPTZ,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX plan_imports_status_idx ON plan_imports (status);
-- One live proposal per exact plan content per project; re-import allowed
-- only after rejection (an applied plan's file is a stub — new hash anyway).
CREATE UNIQUE INDEX plan_imports_pending_uniq
  ON plan_imports (project_id, content_hash) WHERE status <> 'rejected';
