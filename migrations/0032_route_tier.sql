-- SWT-40 Part B (inquiry-promote): the local-LLM routing tier, after the capture
-- rules fail.
--
-- The applied decision is a capture_decisions row with mode='route' (B-D5): a
-- second LIVE row per message is impossible under capture_decisions_live_uniq,
-- and changing that partial index would break every deployed binary's
-- `ON CONFLICT … WHERE mode='live'` (the SWT-36 landmine). A side table would
-- be invisible to every latest-decision reader, which already follows this
-- table. The shape CHECK pins a route row to an attribution: no rule, no task,
-- a typed step, and an extraction iff the step is 'model'.
--
-- source_account_projects is CONFIGURATION, not work (the capture_rules
-- precedent): the closed candidate set per receiving account (B-D1). A row is
-- the human authorisation to move a message into a project whose ai_locality
-- may be wider than its origin, so it is written only through the humanOnly
-- executor tools route_candidate_add / route_candidate_remove. No seeding here
-- (0015's recorded reason).
--
-- source_accounts.route_after is the per-account arming column (B-D7): NULL =
-- routing off (shadow: verdicts only). It is set by a hand-run UPDATE after the
-- shadow reads and the eval gate, never by code and never by this migration.
--
-- Additive. Old binaries never write 'route', and their conflict targets are
-- unchanged. But they do not HONOUR route rows either: a pre-Part-B capture
-- pendingMessages excludes only mode='gate', so an old binary's shadow `--all`
-- pass would write a newer 'unmatched' row above a route row, and every
-- latest-decision reader would then see unmatched. That is harmless while no
-- route row exists, and none does until an account is armed (route_after set)
-- and route_apply runs. Hence the hard precondition (SPEC B-D7 amendment,
-- 2026-09-13): arm an account only after EVERY capture binary runs the Part B
-- image, i.e. all connector CronJobs, pipelined, and any hand-run opsctl, rebuilt
-- from main. Numbered 0032: the SPEC's 0029 went to Part D, 0030 to SWT-45 and
-- 0031 to Part C.
ALTER TABLE capture_decisions DROP CONSTRAINT capture_decisions_mode_check;
ALTER TABLE capture_decisions ADD CONSTRAINT capture_decisions_mode_check
  CHECK (mode IN ('shadow','live','gate','route'));
ALTER TABLE capture_decisions
  ADD COLUMN route_step TEXT CHECK (route_step IN ('thread','single','model','default')),
  ADD COLUMN ai_extraction_id BIGINT REFERENCES ai_extractions(id),
  ADD CONSTRAINT capture_decisions_route_shape CHECK (
    (mode = 'route') = (route_step IS NOT NULL)
    AND (mode <> 'route' OR (action = 'attributed' AND matched_rule_id IS NULL AND task_id IS NULL))
    AND ((route_step = 'model') = (ai_extraction_id IS NOT NULL)));
-- One route per message, forever. PARTIAL: every ON CONFLICT restates WHERE mode = 'route'.
CREATE UNIQUE INDEX capture_decisions_route_uniq ON capture_decisions (message_id) WHERE mode = 'route';

CREATE TABLE source_account_projects (           -- configuration, not work (capture_rules precedent)
  id                BIGSERIAL PRIMARY KEY,
  source_account_id BIGINT NOT NULL REFERENCES source_accounts(id),
  project_id        BIGINT NOT NULL REFERENCES projects(id),
  is_default        BOOLEAN NOT NULL DEFAULT false,
  description       TEXT NOT NULL,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (source_account_id, project_id)
);
CREATE UNIQUE INDEX source_account_projects_default_uniq
  ON source_account_projects (source_account_id) WHERE is_default;
ALTER TABLE source_accounts ADD COLUMN route_after TIMESTAMPTZ;  -- NULL = routing off
