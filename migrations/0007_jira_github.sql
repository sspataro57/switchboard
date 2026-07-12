-- 0007_jira_github.sql — SWT-9 / docs/tickets/09-jira-github-connectors_SPEC.md.
-- external_refs gets its first writers (link_external_ref, hooksd fallback).

ALTER TABLE external_refs ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE external_refs ADD CONSTRAINT external_refs_task_system_key_uniq
  UNIQUE (task_id, system, external_key);
CREATE INDEX external_refs_system_key_idx ON external_refs (system, external_key);
