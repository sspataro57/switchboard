-- 0021 classify promotion (SWT-30).
--
-- projects.classify_promote_after is the per-project cutover for promoting
-- personal classify verdicts into tasks. NULL means "this project's verdicts
-- never promote" — the fail-closed side (D1): not promoting is a stall,
-- promoting by accident fills a board with rows nobody chose. Deliberately no
-- default and no backfill: the operator sets it in the psql statement the
-- runbook prints, which is what makes the cutover a decision with a timestamp
-- rather than a deploy side effect. A COLUMN, not a policies jsonb key —
-- 0016 (ai_locality) and 0018 (ai_classify) set that precedent.
ALTER TABLE projects ADD COLUMN classify_promote_after TIMESTAMPTZ;

-- classify_promotions is the promoter's append-only decision log — the
-- capture_decisions precedent: written directly by internal/promote, carrying
-- no status, no assignee and no claim, so it is not a second tasks table
-- (invariant 2). One row per message FOREVER: the row is inserted BEFORE the
-- executor call it describes (claim-before-act, criterion 12), then updated
-- with the resulting task_id; a crash between the two leaves task_id NULL —
-- visible and diagnosable, never a task nothing remembers.
--
-- ON DELETE CASCADE on normalized_message_id for capture_decisions' recorded
-- reason: a promotion decision without its message means nothing, and the
-- integration suites clear fixtures by deleting normalized_messages — without
-- the cascade they fail inside cleanup, which reads like the cross-pollution
-- pact breaking rather than like a new FK.
CREATE TABLE classify_promotions (
  id                    BIGSERIAL PRIMARY KEY,
  normalized_message_id BIGINT NOT NULL REFERENCES normalized_messages(id) ON DELETE CASCADE,
  raw_source_item_id    BIGINT REFERENCES raw_source_items(id),
  ai_extraction_id      BIGINT NOT NULL REFERENCES ai_extractions(id),
  project_id            BIGINT NOT NULL REFERENCES projects(id),
  kind                  TEXT NOT NULL,
  action                TEXT NOT NULL CHECK (action IN ('task','review','attached')),
  task_id               BIGINT REFERENCES tasks(id),
  reason                TEXT,
  created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- TOTAL unique index, not partial: `ON CONFLICT (normalized_message_id) DO
-- NOTHING` needs no predicate restated (unlike capture_decisions_live_uniq and
-- task_events_outbound_observed_uniq, both partial, whose ON CONFLICT callers
-- must each repeat the WHERE clause or hit a runtime error). The index IS the
-- idempotency story (criterion 11).
CREATE UNIQUE INDEX classify_promotions_message_uniq ON classify_promotions (normalized_message_id);
CREATE INDEX classify_promotions_created_idx ON classify_promotions (created_at);
