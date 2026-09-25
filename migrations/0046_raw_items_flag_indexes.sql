-- 0046 sources-slow (swb 709): two partial indexes for /sources' raw_json counters.
--
-- /sources counts, per account, the raw items whose capture was truncated and
-- the ones carrying MIME parts. Both predicates read raw_json, so every page
-- load de-TOASTed all of raw_source_items (98k rows, 2.2 GB on 2026-09-25):
-- ~10 s each, 22 s a page. A partial index on source_account_id WHERE <the
-- exact predicate> lets the planner count the few matching rows from the index
-- instead. The predicates are spelled EXACTLY as internal/dashboard/sources.go
-- spells them; a different spelling is not provable and silently falls back to
-- the full scan (pinned by internal/dashboard TestSources_CountersMatchTheirIndexes).
--
-- Runs inside the migrate runner's transaction, so no CONCURRENTLY: each build
-- holds writes to raw_source_items for its scan (seconds). Writers wait; none fail.
-- Idempotent.
CREATE INDEX IF NOT EXISTS raw_source_items_truncated_idx
  ON raw_source_items (source_account_id)
  WHERE raw_json->>'truncated' = 'true';
CREATE INDEX IF NOT EXISTS raw_source_items_has_parts_idx
  ON raw_source_items (source_account_id)
  WHERE jsonb_array_length(COALESCE(raw_json->'parts','[]'::jsonb)) > 0;
