-- receipts-become-tasks (SWT-68) — the reproduction set, read-only.
-- Run: psql "$OPS_DATABASE_URL" -f docs/bugs/receipts-become-tasks_repro.sql
-- Writes nothing. Re-derives the 19 `$…` personal tasks, their source message,
-- the stored classify verdict, the promotion row and how each was closed.

\pset pager off
\pset format aligned

SELECT t.id                                   AS task,
       t.title,
       t.status,
       t.created_at,
       sa.account_email                       AS mailbox,
       nm.sender,
       nm.subject,
       nm.sent_at::date                       AS sent,
       ae.fields->>'kind'                     AS verdict_kind,
       ae.fields->>'actionable'               AS verdict_actionable,
       left(ae.fields->>'reason', 100)        AS verdict_reason,
       cp.action                              AS promotion_action,
       coalesce(cp.reason, '(null)')          AS promotion_reason,
       cp.normalized_message_id,
       cp.ai_extraction_id,
       ae.ai_run_id,
       coalesce(td.reason_code, '(no dismissal)') AS dismissal,
       td.dismissed_by,
       td.created_at                          AS dismissed_at
FROM tasks t
JOIN projects p                 ON p.id = t.project_id
LEFT JOIN classify_promotions cp ON cp.task_id = t.id
LEFT JOIN normalized_messages nm ON nm.id = cp.normalized_message_id
LEFT JOIN raw_source_items rsi   ON rsi.id = nm.raw_source_item_id
LEFT JOIN source_accounts sa     ON sa.id = rsi.source_account_id
LEFT JOIN ai_extractions ae      ON ae.id = cp.ai_extraction_id
LEFT JOIN task_dismissals td     ON td.task_id = t.id
WHERE p.slug = 'personal'
  AND t.title LIKE '$%'
ORDER BY t.id;

-- Short body excerpts (receipt vs bill-due by eye).
SELECT t.id,
       regexp_replace(left(nm.body_text, 300), '[[:space:]]+', ' ', 'g') AS body
FROM tasks t
JOIN projects p                  ON p.id = t.project_id
JOIN classify_promotions cp      ON cp.task_id = t.id
JOIN normalized_messages nm      ON nm.id = cp.normalized_message_id
WHERE p.slug = 'personal' AND t.title LIKE '$%'
ORDER BY t.id;

-- How the six undismissed ones were closed.
SELECT te.task_id, te.event_type, te.created_at, te.payload
FROM task_events te
JOIN tasks t   ON t.id = te.task_id
JOIN projects p ON p.id = t.project_id
WHERE p.slug = 'personal' AND t.title LIKE '$%'
ORDER BY te.task_id, te.id;
