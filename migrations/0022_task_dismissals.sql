-- 0022 task dismissals (SWT-31).
--
-- task_dismissals is the labelled-data store for board dismissals: a human's
-- judgement about a task that should not have existed, recorded as a TYPED row
-- a future precision ticket can GROUP BY — the capture_decisions (0015) and
-- classify_promotions (0021) precedent, and CLAUDE.md's own line ("Log every
-- dashboard correction as labeled data") made real. The status_changed
-- task_event still carries a PROSE reason for humans reading the task;
-- nothing may query that payload for labels.
--
-- This is a LOG, not a second tasks table (invariant 2): no status, no
-- assignee, no claim, and nothing ever "works" a row here — the dismissed item
-- stays a row in tasks with status='closed', and the board lane is a FILTER
-- (?status=closed), never a table.
--
-- ON DELETE CASCADE for capture_decisions' recorded reason: a dismissal
-- without its task means nothing, and the integration suites clear fixtures by
-- deleting tasks — without the cascade they fail inside cleanup, which reads
-- like the cross-pollution pact breaking rather than like a new FK.
--
-- dismissed_by duplicates the executor actor already in audit_events,
-- deliberately: the label is training data and must be readable with ONE typed
-- query, not by parsing audit_events.args.
CREATE TABLE task_dismissals (
  id           BIGSERIAL PRIMARY KEY,
  task_id      BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  reason_code  TEXT NOT NULL CHECK (reason_code IN
                 ('not_actionable','wrong_kind','duplicate','handled_elsewhere')),
  note         TEXT,
  dismissed_by TEXT NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- TOTAL unique index — the idempotency story itself: `ON CONFLICT (task_id)
-- DO NOTHING` needs no predicate restated (unlike capture_decisions_live_uniq
-- and task_events_outbound_observed_uniq, both partial), and a second dismiss
-- of the same task is a success that adds no second row. No other index,
-- deliberately: the join targets hold tens to hundreds of rows and every
-- reader reaches tasks by primary key; an index nothing uses is a permanent
-- claim that some query needs it (0018's recorded argument).
CREATE UNIQUE INDEX task_dismissals_task_uniq ON task_dismissals (task_id);
