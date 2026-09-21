-- Spread query for SWT-70 / inquiry-reads-quoted-history.
--
-- How often does the inquiry lane answer needs_reply=false on an inbound gmail
-- message that carries reply-quoted history, versus one that does not? And which
-- of those messages had non-trivial NEW text above the first quote separator?
--
-- READ-ONLY. Run against production:
--   psql "$OPS_DATABASE_URL" -f docs/bugs/inquiry-reads-quoted-history_repro.sql
--
-- Quote separators recognised (the three shapes the report names):
--   "-----Original Message-----" / "-----Mensaje original-----"
--   a line starting "On " and containing "wrote:"
--   a line starting ">"
-- `cut` is the earliest of those (1-based, NULL when the body has none);
-- "new text" is everything before it.

\pset pager off

CREATE TEMP VIEW swt70_verdicts AS
WITH v AS (
    SELECT r.id                          AS run_id,
           r.created_at                  AS run_at,
           m.id                          AS message_id,
           m.sent_at,
           m.sender,
           m.subject,
           m.body_text,
           (e.fields->>'needs_reply')::bool AS needs_reply,
           e.fields->>'ask_kind'            AS ask_kind,
           e.fields->>'project_slug'        AS project_slug
      FROM ai_runs r
      JOIN ai_extractions e      ON e.ai_run_id = r.id
      JOIN normalized_messages m ON m.raw_source_item_id = e.raw_source_item_id
     WHERE r.worker_type = 'classify_inquiry'
       AND r.status = 'ok'
       AND r.created_at >= now() - interval '30 days'
       AND m.direction = 'inbound'
       AND m.channel = 'gmail'
)
SELECT v.*,
       NULLIF(LEAST(
         COALESCE(NULLIF(strpos(v.body_text, '-----Original Message-----'), 0), 2147483647),
         COALESCE(NULLIF(strpos(v.body_text, '-----Mensaje original-----'), 0), 2147483647),
         COALESCE(NULLIF(regexp_instr(v.body_text, '^On .*wrote:', 1, 1, 0, 'n'), 0), 2147483647),
         COALESCE(NULLIF(regexp_instr(v.body_text, '^>',           1, 1, 0, 'n'), 0), 2147483647)
       ), 2147483647) AS cut
  FROM v;

-- 1. quoted vs not, by verdict.
\echo '== inquiry verdicts on inbound gmail, last 30 days =='
SELECT (cut IS NOT NULL) AS has_quoted_history,
       needs_reply,
       count(*) AS n,
       count(*) FILTER (WHERE COALESCE(cut - 1, length(body_text)) > 40) AS new_text_gt_40_chars
  FROM swt70_verdicts
 GROUP BY 1, 2
 ORDER BY 1, 2;

-- 2. the candidates: needs_reply=false on a quoted message whose new text is
--    non-trivial. No bodies — ids, dates, sender domain, subject, ask_kind.
\echo '== needs_reply=false, quoted, new text > 40 chars =='
SELECT message_id,
       sent_at::date AS sent_on,
       lower(split_part(regexp_replace(sender, '^.*<|>.*$', '', 'g'), '@', 2)) AS sender_domain,
       project_slug,
       ask_kind,
       cut - 1            AS new_text_len,
       length(body_text)  AS body_len,
       left(subject, 70)  AS subject
  FROM swt70_verdicts
 WHERE needs_reply = false
   AND cut IS NOT NULL
   AND cut - 1 > 40
 ORDER BY sent_at DESC
 LIMIT 15;
