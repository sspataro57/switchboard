-- 0047_external_refs_job_agent.sql — external_refs.system gains 'job_agent'.
--
-- job-agent's analyzer opens a `fit %` task on this db for every job it scores
-- above threshold (cmd/analyzer/swb.go in the job-agent repo, writing with the
-- ops DSN). Those tasks now carry two external_refs rows so the task page's
-- "External refs" table links straight to the job:
--   job:<job id>      -> https://jobs.home.arpa/jobs/<job id>   (job-agent dashboard)
--   posting:<job id>  -> the posting's canonical URL
-- job-agent writes them directly, idempotently (ON CONFLICT (system,
-- external_key) DO NOTHING), and soft-fails if this value is missing.
--
-- capture_rules.external_system is deliberately NOT widened, despite 0015's
-- "keep the two lists in step" note. That column names a source the capture
-- evaluator can key a rule on; nothing ingests job-agent through capture, so a
-- 'job_agent' rule would be stored and silently match nothing — the failure
-- 0015 closed the enum to prevent. For the same reason link_external_ref's
-- validator (internal/tools/prci.go) and captureExternalSystems
-- (internal/tools/capturerules.go) stay as they are: job_agent refs are written
-- by job-agent, never through a capture rule or that tool. From here on
-- external_refs.system is a SUPERSET of capture_rules.external_system.
--
-- Same drop/add as 0015 step 3; migrate wraps the file in one transaction, so
-- the constraint is never absent to another session.
ALTER TABLE external_refs DROP CONSTRAINT external_refs_system_check;
ALTER TABLE external_refs ADD CONSTRAINT external_refs_system_check
  CHECK (system IN ('jira','github','upwork_crm','slack','gmail','job_agent'));
