-- 0023 ticket status sync (SWT-32).
--
-- The assignee gate: per project, default OFF (0016/0018 precedent — a typed
-- column, never a policies jsonb key; fail-closed here means "keep today's
-- behaviour", because gating by accident silently empties a client's board).
-- Armed by hand, per project, per the runbook. No backfill and no arming here.
ALTER TABLE projects ADD COLUMN ticket_assignee_gate BOOLEAN NOT NULL DEFAULT false;

-- ticket_status_syncs is the reconciler's typed STATE, one row per jira
-- external_ref, UPSERTed in place. It is state, not a log — the history of
-- every close and reopen is already append-only and typed in task_events
-- (status_changed) and audit_events — and it is NOT a second tasks table
-- (invariant 2): no title, no assignee_type, no priority, no claim, and
-- nothing ever "works" a row here. last_action='closed' is the only value that
-- authorises a later reopen: it is how the pass knows a close was its OWN and
-- not a human's dismissal, R8's delivery lifecycle, or a hand-run task_close.
--
-- ON DELETE CASCADE on both FKs for capture_decisions' recorded reason: the
-- integration suites clear fixtures by deleting tasks, and without the cascade
-- they fail inside cleanup — which reads like the cross-pollution pact breaking
-- rather than like a new FK.
CREATE TABLE ticket_status_syncs (
  id                  BIGSERIAL PRIMARY KEY,
  external_ref_id     BIGINT NOT NULL REFERENCES external_refs(id) ON DELETE CASCADE,
  task_id             BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  -- Jira's three statusCategory keys. The NAME is diagnostic only; nothing
  -- branches on it (D2).
  status_category     TEXT NOT NULL CHECK (status_category IN ('new','indeterminate','done')),
  status_name         TEXT,
  -- The ticket's assignee accountId as last observed, and whether it matched
  -- the storing account's own_account_id. Diagnostic + the report's join; the
  -- DECISION is recomputed every pass from the stored raw, never read back
  -- from here (a cached boolean would go stale the day the polling identity
  -- changes, silently).
  assignee_account_id TEXT,
  assigned_to_self    BOOLEAN,
  -- What this pass last DID.
  last_action         TEXT NOT NULL CHECK (last_action IN
                        ('none','closed','reopened','refused_active','suppressed_dismissed')),
  -- WHICH fact dropped it. NULL unless the task was dropped.
  drop_reason         TEXT CHECK (drop_reason IN ('ticket_done','not_assigned')),
  -- The status the task held when this pass closed it, so a reopen restores it
  -- instead of flattening everything to ready (D6). NULL unless
  -- last_action='closed'.
  closed_from_status  TEXT,
  reason              TEXT,
  observed_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  acted_at            TIMESTAMPTZ
);

-- TOTAL unique index — one row per ref, and what lets
-- `ON CONFLICT (external_ref_id) DO UPDATE` omit a restated predicate (unlike
-- capture_decisions_live_uniq and task_events_outbound_observed_uniq, both
-- partial). No other index, deliberately: one row per jira external ref — tens
-- today — and every read either scans the table whole or reaches it by
-- external_ref_id; an index nothing uses is a permanent claim that some query
-- needs it (0016's and 0018's recorded argument).
CREATE UNIQUE INDEX ticket_status_syncs_ref_uniq ON ticket_status_syncs (external_ref_id);
