-- Reproduction: slackweb-collab-export-stale (Jira SWT-39)
--
-- BUG: The Collaboratory/LlamaSite Slack workspace (T0HPR78RX,
-- source_accounts.id = 542) keeps exporting with status 'ok' while a message
-- already in Slack does not reach raw_source_items. The reported instance is
-- asunda45's DM (conversation D0AUD86LKGA) "Hey Salvador, it is now ready for
-- review and merge ..." (Screenshot_20260911_094927.png), with Slack timestamp
-- 2026-09-10T20:49:06.665Z. From 23:00Z on 2026-09-10 the 542 runs sat at
-- conversations_seen=7, messages_seen 166-168, raw_inserted=0 (30 runs, 33807 to
-- 34656). The message finally landed in run 34684 (2026-09-11 14:00Z) as raw
-- 77849, about 17h late. Avviato (539) is the contrast: its messages are picked
-- up by the next run or the one after.
--
-- The report said the message "never lands". Before 14:00Z on 2026-09-11 this
-- could be asserted as ABSENCE. Once 34684 ingested it, absence is no longer
-- true, so the re-runnable form of the bug is DELAY:
--
--   missed_runs(message) = number of status='ok' sync_runs of the SAME account
--                          that started >= 30 min after the message's Slack
--                          time (normalized_messages.sent_at) and FINISHED
--                          before the message was first normalized
--                          (normalized_messages.created_at, which sink upserts
--                          never rewrite, so it is a stable "first seen" time).
--
-- In words: ok runs that claimed a pass over the workspace while a message that
-- was already there stayed invisible. raw_source_items.ingested_at is NOT used,
-- because it is rewritten on every update.
--
-- WHAT THIS ASSERTS (expected behaviour, so it FAILS while the bug is present in
-- the window):
--   Every slack message of account 542 with sent_at >= :since has
--   missed_runs < :max_missed (default 3). Measured baseline on the healthy
--   workspace (539, since 2026-09-08): 46 messages, max missed_runs = 1.
--   The reported message (raw 77849) is also printed with its own missed_runs.
--
-- RUN (read-only; everything is in a READ ONLY transaction that rolls back):
--   psql -h 192.168.50.49 -U ops -d ops -v ON_ERROR_STOP=1 \
--        -f docs/bugs/slackweb-collab-export-stale_repro.sql
--   Optional: -v since='2026-09-12 00:00Z'  (default '2026-09-08 00:00Z')
--             -v max_missed=3                (default 3)
--   After a fix, re-run with :since set to the fix's deploy time to check that
--   new messages are no longer delayed. The default window contains the
--   incident, so with the defaults this script keeps failing on history.
--
--   Exit 3 + "REPRO FAILS (bug present)" = delayed messages in the window.
--   Exit 3 + "INCONCLUSIVE"              = no 542 slack messages in the window.
--   Exit 0 + "REPRO PASSES"              = nothing delayed in the window.

\set ON_ERROR_STOP 1
\pset pager off
\if :{?since}
\else
  \set since '2026-09-08 00:00Z'
\endif
\if :{?max_missed}
\else
  \set max_missed 3
\endif

BEGIN TRANSACTION READ ONLY;
SELECT set_config('repro.since', :'since', true)           AS since,
       set_config('repro.max_missed', :'max_missed', true) AS max_missed;

\echo '== 1. Latest 12 sync_runs for Collaboratory (542) =='
SELECT id, started_at, status,
       stats->>'conversations_seen' AS conv,
       stats->>'messages_seen'      AS msgs,
       stats->>'raw_inserted'       AS ins,
       stats->>'raw_updated'        AS upd,
       stats->>'raw_unchanged'      AS unch
FROM sync_runs
WHERE source_account_id = 542
ORDER BY id DESC
LIMIT 12;

\echo '== 2. Zero-insert streaks for 542 of >= 6 consecutive ok runs since :since =='
WITH runs AS (
  SELECT id, started_at, status,
         (stats->>'raw_inserted')::int       AS ins,
         (stats->>'conversations_seen')::int AS conv,
         (stats->>'messages_seen')::int      AS msgs,
         (stats->>'raw_updated')::int        AS upd,
         count(*) FILTER (WHERE (stats->>'raw_inserted')::int > 0)
           OVER (ORDER BY id) AS grp
  FROM sync_runs
  WHERE source_account_id = 542
    AND started_at >= current_setting('repro.since')::timestamptz
)
SELECT min(id) AS first_run, max(id) AS last_run,
       min(started_at) AS first_start, max(started_at) AS last_start,
       count(*) AS runs,
       min(conv) || '-' || max(conv) AS conv_range,
       min(msgs) || '-' || max(msgs) AS msgs_range,
       min(upd)  || '-' || max(upd)  AS upd_range
FROM runs
WHERE ins = 0 AND status = 'ok'
GROUP BY grp
HAVING count(*) >= 6
ORDER BY min(id);

\echo '== 3. Delay per account since :since (missed_runs = ok runs that should have seen it) =='
WITH m AS (
  SELECT r.id AS raw_id, r.source_account_id AS acct, r.external_id,
         nm.sent_at, nm.created_at
  FROM normalized_messages nm
  JOIN raw_source_items r ON r.id = nm.raw_source_item_id
  WHERE r.source_account_id IN (539, 542)
    AND nm.channel = 'slack'
    AND nm.sent_at >= current_setting('repro.since')::timestamptz
), l AS (
  SELECT m.*,
         (SELECT count(*) FROM sync_runs s
           WHERE s.source_account_id = m.acct AND s.status = 'ok'
             AND s.started_at >= m.sent_at + interval '30 minutes'
             AND s.finished_at <= m.created_at) AS missed_runs
  FROM m
)
SELECT acct,
       count(*)                                        AS messages,
       count(*) FILTER (WHERE missed_runs = 0)         AS on_time,
       count(*) FILTER (WHERE missed_runs BETWEEN 1 AND 2) AS missed_1_2,
       count(*) FILTER (WHERE missed_runs >= 3)        AS missed_3plus,
       max(missed_runs)                                AS max_missed_runs,
       max(created_at - sent_at)                       AS max_delay
FROM l
GROUP BY acct
ORDER BY acct;

\echo '== 4. The reported message (asunda45, D0AUD86LKGA, ~20:49Z 2026-09-10) =='
SELECT r.id AS raw_id, r.external_id, nm.sent_at, nm.created_at AS first_seen,
       nm.created_at - nm.sent_at AS delay,
       (SELECT count(*) FROM sync_runs s
         WHERE s.source_account_id = 542 AND s.status = 'ok'
           AND s.started_at >= nm.sent_at + interval '30 minutes'
           AND s.finished_at <= nm.created_at) AS missed_runs,
       (SELECT min(s.id) FROM sync_runs s
         WHERE s.source_account_id = 542
           AND nm.created_at BETWEEN s.started_at AND s.finished_at + interval '5 seconds') AS landed_in_run,
       left(nm.body_text, 60) AS text_prefix
FROM raw_source_items r
JOIN normalized_messages nm ON nm.raw_source_item_id = r.id
WHERE r.source_account_id = 542
  AND r.external_id LIKE 'message:D0AUD86LKGA:%'
  AND r.raw_json->'message'->>'text' ILIKE '%it is now ready for review and merge%';

\echo '== 5. Delayed 542 messages (missed_runs >= :max_missed) by conversation =='
WITH m AS (
  SELECT r.id AS raw_id, r.external_id, nm.sent_at, nm.created_at
  FROM normalized_messages nm
  JOIN raw_source_items r ON r.id = nm.raw_source_item_id
  WHERE r.source_account_id = 542
    AND nm.channel = 'slack'
    AND nm.sent_at >= current_setting('repro.since')::timestamptz
), l AS (
  SELECT m.*,
         (SELECT count(*) FROM sync_runs s
           WHERE s.source_account_id = 542 AND s.status = 'ok'
             AND s.started_at >= m.sent_at + interval '30 minutes'
             AND s.finished_at <= m.created_at) AS missed_runs
  FROM m
)
SELECT split_part(external_id, ':', 2) AS conversation,
       count(*)          AS delayed_messages,
       max(missed_runs)  AS max_missed_runs,
       min(sent_at)      AS first_sent,
       max(created_at)   AS last_first_seen
FROM l
WHERE missed_runs >= current_setting('repro.max_missed')::int
GROUP BY 1
ORDER BY 1;

\echo '== 6. Assertion =='
DO $$
DECLARE
  since_t     timestamptz := current_setting('repro.since')::timestamptz;
  max_missed  int         := current_setting('repro.max_missed')::int;
  n_msgs      bigint;
  n_delayed   bigint;
  worst_raw   bigint;
  worst_ext   text;
  worst_n     bigint;
BEGIN
  WITH m AS (
    SELECT r.id AS raw_id, r.external_id, nm.sent_at, nm.created_at
    FROM normalized_messages nm
    JOIN raw_source_items r ON r.id = nm.raw_source_item_id
    WHERE r.source_account_id = 542
      AND nm.channel = 'slack'
      AND nm.sent_at >= since_t
  ), l AS (
    SELECT m.*,
           (SELECT count(*) FROM sync_runs s
             WHERE s.source_account_id = 542 AND s.status = 'ok'
               AND s.started_at >= m.sent_at + interval '30 minutes'
               AND s.finished_at <= m.created_at) AS missed_runs
    FROM m
  )
  SELECT count(*), count(*) FILTER (WHERE missed_runs >= max_missed)
    INTO n_msgs, n_delayed
  FROM l;

  IF n_msgs = 0 THEN
    RAISE EXCEPTION 'INCONCLUSIVE: no slack messages for account 542 with sent_at >= %', since_t;
  END IF;

  IF n_delayed > 0 THEN
    WITH m AS (
      SELECT r.id AS raw_id, r.external_id, nm.sent_at, nm.created_at
      FROM normalized_messages nm
      JOIN raw_source_items r ON r.id = nm.raw_source_item_id
      WHERE r.source_account_id = 542 AND nm.channel = 'slack' AND nm.sent_at >= since_t
    )
    SELECT raw_id, external_id,
           (SELECT count(*) FROM sync_runs s
             WHERE s.source_account_id = 542 AND s.status = 'ok'
               AND s.started_at >= m.sent_at + interval '30 minutes'
               AND s.finished_at <= m.created_at)
      INTO worst_raw, worst_ext, worst_n
    FROM m ORDER BY 3 DESC, raw_id LIMIT 1;

    RAISE EXCEPTION 'REPRO FAILS (bug present): % of % slack messages for account 542 since % were invisible to >= % ok export runs (worst: raw % % missed % ok runs)',
      n_delayed, n_msgs, since_t, max_missed, worst_raw, worst_ext, worst_n;
  END IF;

  RAISE NOTICE 'REPRO PASSES: % slack messages for account 542 since %, none missed by >= % ok runs',
    n_msgs, since_t, max_missed;
END
$$;

ROLLBACK;
