-- 0018: the ai_classify workload flag and the bulk project (SWT-23). Forward-only.
--
-- STATEMENT ORDER IS LOAD-BEARING (0016's lesson, restated):
--   1. ALTER  — fail-closed default
--   2. UPDATE — preserve SWT-22's lane exactly
--   3. INSERT — the bulk project, explicitly excluded
--
-- What ai_classify MEANS: "mail attributed to this project gets an actionability
-- verdict from the personal classify lane". It is a WORKLOAD flag, not a
-- boundary flag — ai_locality remains the boundary, and the personal lane's
-- filter keeps BOTH clauses (`p.ai_locality = 'local_only' AND p.ai_classify`)
-- because they answer different questions: where a message may be sent (a leak
-- is irreversible) and whether it is worth classifying (a stall is one UPDATE).
--
-- DEFAULT false is the fail-closed side. Not classifying is a stall,
-- recoverable with one UPDATE and visible as an empty lane; classifying by
-- accident costs GPU-hours at a measured 7.2 s per message and pollutes a
-- report. Same asymmetry 0016 used for ai_locality, opposite polarity, same
-- reasoning.

ALTER TABLE projects
  ADD COLUMN ai_classify BOOLEAN NOT NULL DEFAULT false;

UPDATE projects SET ai_classify = true WHERE slug = 'personal';

-- The bulk project: where the deterministically-claimed residue lands.
-- ai_locality = 'local_only' because 0016's reasoning applies unchanged: a rule
-- one character too wide would otherwise downgrade real personal mail to
-- hosted-eligible, and "a leak is irreversible, a stall is one UPDATE".
-- client is NULL and it is load-bearing (0016's words for `personal`):
-- task_get_next's `p.client = $1` is what excludes these projects from every
-- worker queue. ON CONFLICT DO NOTHING, not DO UPDATE: re-running a migration
-- must not overwrite a value an operator has deliberately changed since.
-- No index: every read reaches projects by primary key and the table holds
-- tens of rows.
INSERT INTO projects (name, slug, client, execution, delivery, ai_locality, ai_classify)
VALUES ('Bulk mail', 'bulk', NULL, 'manual', 'dashboard', 'local_only', false)
ON CONFLICT (slug) DO NOTHING;
