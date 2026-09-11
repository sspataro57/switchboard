-- SWT-39 (slackweb-collab-export-stale): a Slack export that deferred or could
-- not read conversations in scope finishes 'partial', not 'ok'. 0001 declared
-- the CHECK inline; Postgres named it sync_runs_status_check. Widen it: every
-- existing status kept, 'partial' added. Additive — no row changes.
--
-- Deploy order: apply BEFORE any image that writes 'partial', or every partial
-- run's FinishRun fails against the old CHECK.
ALTER TABLE sync_runs DROP CONSTRAINT sync_runs_status_check;
ALTER TABLE sync_runs ADD CONSTRAINT sync_runs_status_check
  CHECK (status IN ('running','ok','partial','error'));
