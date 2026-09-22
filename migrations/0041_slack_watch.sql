-- 0041 slack_watch (SWT-75, slack-watch-sweep).
--
-- The Slack watch list: the conversations the resident watcher reads about
-- every minute (José's and Katie's DMs to start), as DATA — added with opsctl
-- through the executor, never in code. Keyed by (workspace, conversation);
-- a row is turned OFF, never deleted (the capture_rules shape), so a "why did
-- we stop watching this" question has an answer.
--
-- The two CHECKs are the leaf's own id rules (http-bridge.ts parseExportBody;
-- internal/connector/slackweb/export_request.go): the leaf turns these ids
-- straight into navigable URLs and BuildExportRequest silently DROPS a
-- malformed one, so the constraint that makes a dropped watch row impossible
-- belongs here, where the write fails loudly.
--
-- No rows are seeded: which conversations matter is Salvador's call.
-- Rollout ORDER: apply before rolling the image. The new dashboard reads this
-- table on every /sources render (and 500s the page on a missing table), and
-- the new watcher refuses to start without it; an old image never touches it,
-- so applying first is always safe. Idempotent.
CREATE TABLE IF NOT EXISTS slack_watch (
  id               BIGSERIAL PRIMARY KEY,
  workspace_id     TEXT NOT NULL CHECK (workspace_id ~ '^T[A-Z0-9]{5,}$'),
  conversation_id  TEXT NOT NULL CHECK (conversation_id ~ '^[CDG][A-Z0-9]{5,}$'),
  label            TEXT NOT NULL DEFAULT '',
  enabled          BOOLEAN NOT NULL DEFAULT true,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(workspace_id, conversation_id)
);
