-- Reproduction — slack-messages-not-becoming-tasks (Jira SWT-78, swb #521)
--
-- Bug: "there are a bunch of slacks from yesterday that didn't come in as taks.
-- This has proven unreliable." (Salvador, 2026-09-23, about 2026-09-22 EDT.)
-- Expected (Salvador): "basically all DMs to me are actionable just messages on
-- the general forum need decision". So:
--   * DM / group DM (conversation id D… / G…): PASS only if the message produced a
--     task or was attached to a task. Anything else is FAIL, a classifier
--     "no reply needed" verdict included.
--   * Channel (C…): PASS if it produced a task/attachment OR carries a recorded
--     classifier (inquiry lane) verdict. No task and no verdict = FAIL.
--
-- READ-ONLY. SELECTs only. Run through the wrapper:
--   docs/bugs/slack-messages-not-becoming-tasks_repro.sh            (2026-09-22 EDT)
--   docs/bugs/slack-messages-not-becoming-tasks_repro.sh 2026-09-21 (another EDT day)
-- or directly:
--   psql -h 192.168.50.49 -U ops -d ops -X -v day=2026-09-22 \
--        -f docs/bugs/slack-messages-not-becoming-tasks_repro.sql
-- Every per-message line starts with PASS / FAIL; the wrapper exits 1 on any FAIL.
-- No message bodies are printed (ids, senders, conversation ids only).

\set ON_ERROR_STOP on
\pset pager off
\if :{?day}
\else
  \set day 2026-09-22
\endif

-- Window = the given calendar day in America/New_York.
CREATE TEMP VIEW win AS
SELECT (:'day'::date)::timestamp AT TIME ZONE 'America/New_York'                       AS ws,
       ((:'day'::date) + 1)::timestamp AT TIME ZONE 'America/New_York'                 AS we;

-- Every inbound (= not Salvador) Slack message sent in the window, whatever conversation.
CREATE TEMP VIEW msg AS
SELECT m.id, m.sender, m.sent_at, m.created_at AS normalized_at, m.raw_source_item_id,
       r.source_account_id AS acct,
       split_part(t.thread_key, ':', 3)                                   AS conv,
       CASE WHEN split_part(t.thread_key, ':', 3) ~ '^[DG]' THEN 'dm' ELSE 'channel' END AS kind
FROM normalized_messages m
JOIN raw_source_items   r ON r.id = m.raw_source_item_id
JOIN normalized_threads t ON t.id = m.thread_id
JOIN source_accounts    a ON a.id = r.source_account_id AND a.provider = 'slack_web'
CROSS JOIN win
WHERE m.channel = 'slack' AND m.direction = 'inbound'
  AND m.sent_at >= win.ws AND m.sent_at < win.we;

-- One funnel row per message: capture -> classifier (qwen, inquiry lane) -> promotion -> task.
CREATE TEMP VIEW funnel AS
SELECT msg.*,
       cd.action          AS cap_action,
       cd.matched_rule_id AS cap_rule,
       cd.task_id         AS cap_task,
       cd.comm_task_id    AS cap_comm,
       ae.id              AS ae_id,
       ar.provider || '/' || ar.model AS model,
       ae.fields->>'needs_reply' AS needs_reply,
       ae.fields->>'ask_kind'    AS ask_kind,
       cp.action          AS prom_action,
       cp.task_id         AS prom_task,
       (SELECT min(t.id) FROM tasks t WHERE t.activity_by_message_id = msg.id) AS activity_task,
       -- SWT-78 fix: the backfill verb logs one line per message on its
       -- conversation task (the live decision row stays `attributed`).
       (SELECT min(e.task_id) FROM task_events e
         WHERE e.event_type = 'log'
           AND e.payload->>'message' ~ ('^capture: DM backfill \(SWT-78\) \S+ — \S+ message ' || msg.id || ' from ')) AS backfill_task
FROM msg
LEFT JOIN capture_decisions  cd ON cd.message_id = msg.id AND cd.mode = 'live'
LEFT JOIN LATERAL (SELECT * FROM ai_extractions x WHERE x.raw_source_item_id = msg.raw_source_item_id
                   ORDER BY x.id DESC LIMIT 1) ae ON true
LEFT JOIN ai_runs            ar ON ar.id = ae.ai_run_id
LEFT JOIN LATERAL (SELECT * FROM classify_promotions p WHERE p.normalized_message_id = msg.id
                   ORDER BY p.id DESC LIMIT 1) cp ON true;

CREATE TEMP VIEW verdict AS
SELECT f.*,
  CASE
    WHEN f.cap_action = 'task' OR f.cap_comm IS NOT NULL OR f.prom_action = 'task'
                                                        THEN 'task'
    WHEN f.cap_action = 'task_log' AND f.cap_task IS NOT NULL THEN 'attached(capture)'
    WHEN f.prom_action = 'attached' AND f.prom_task IS NOT NULL THEN 'attached(promote)'
    WHEN f.activity_task IS NOT NULL                    THEN 'task(activity)'
    WHEN f.backfill_task IS NOT NULL                    THEN 'attached(backfill)'
    WHEN f.cap_action IS NULL                           THEN 'no_capture_decision'
    WHEN f.ae_id IS NULL                                THEN 'capture_only:' || f.cap_action || '_rule' || f.cap_rule || '_never_classified'
    WHEN f.needs_reply = 'false'                        THEN 'classifier_said_no_reply(' || coalesce(f.ask_kind,'?') || ')'
    WHEN f.needs_reply = 'true' AND f.prom_action IS NULL THEN 'classifier_said_reply_but_no_promotion_row'
    ELSE 'other:' || coalesce(f.prom_action,'')
  END AS outcome
FROM funnel f;

\echo
\echo '=== Per-message funnel (inbound Slack, ' :day ' EDT) ==='
SELECT CASE
         WHEN outcome LIKE 'task%' OR outcome LIKE 'attached%' THEN 'PASS'
         WHEN kind = 'channel' AND ae_id IS NOT NULL THEN 'PASS'
         ELSE 'FAIL'
       END AS result,
       kind, conv, id AS message_id, sender,
       to_char(sent_at AT TIME ZONE 'America/New_York', 'HH24:MI') AS sent_edt,
       cap_action, cap_rule, coalesce(cap_task, cap_comm) AS cap_task,
       ae_id, model, needs_reply, ask_kind, prom_action, prom_task, activity_task,
       outcome
FROM verdict ORDER BY kind DESC, sent_at;

\echo
\echo '=== Stage counts by conversation kind (jira_bot = DMs from the Jira app) ==='
SELECT kind, (sender = 'Jira') AS jira_bot, outcome, count(*) AS n,
       (array_agg(id ORDER BY sent_at))[1:4] AS sample_message_ids
FROM verdict GROUP BY 1, 2, 3 ORDER BY 1 DESC, 2, 4 DESC;

\echo
\echo '=== Classifier (qwen) verdicts on the window: model and ask_kind ==='
SELECT kind, model, needs_reply, ask_kind, count(*) AS n
FROM verdict WHERE ae_id IS NOT NULL AND sender <> 'Jira'
GROUP BY 1, 2, 3, 4 ORDER BY 1 DESC, 3, 5 DESC;

\echo
\echo '=== Capture coverage: rotation reads per conversation (DMs never read in the window) ==='
WITH runs AS (
  SELECT s.id, s.source_account_id AS acct, s.stats
  FROM sync_runs s JOIN source_accounts a ON a.id = s.source_account_id AND a.provider = 'slack_web'
  CROSS JOIN win
  WHERE s.stats->>'phase' = 'slack_web' AND s.started_at >= win.ws AND s.started_at < win.we),
en AS (SELECT r.acct, r.id, e->>'id' AS conv, (e->>'rank')::int AS rank FROM runs r,
       jsonb_array_elements(r.stats->'enumerated') e),
rd AS (SELECT r.id, x AS conv FROM runs r, jsonb_array_elements_text(r.stats->'read') x)
SELECT 'COVERAGE' AS tag, en.acct, en.conv, min(en.rank) AS dm_rank,
       count(*) AS runs_enumerated, count(rd.conv) AS runs_read
FROM en LEFT JOIN rd ON rd.id = en.id AND rd.conv = en.conv
GROUP BY 1, 2, 3 HAVING count(rd.conv) <= 1 ORDER BY 2, 5;

\echo
\echo '=== Run bookkeeping for the window (messages the ingester skipped for identity are not stored anywhere) ==='
SELECT s.source_account_id AS acct, s.stats->>'phase' AS phase, s.status, count(*) AS runs,
       sum((s.stats->>'raw_inserted')::int)              AS raw_inserted,
       sum((s.stats->>'messages_skipped_identity')::int) AS skipped_identity,
       sum(CASE WHEN (s.stats->'coverage'->>'budget_exhausted')::bool THEN 1 ELSE 0 END) AS budget_exhausted
FROM sync_runs s JOIN source_accounts a ON a.id = s.source_account_id AND a.provider = 'slack_web'
CROSS JOIN win
WHERE s.started_at >= win.ws AND s.started_at < win.we
GROUP BY 1, 2, 3 ORDER BY 1, 2, 3;
