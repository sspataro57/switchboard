-- 0003_orchestrator.sql — SWT-5 / docs/tickets/05-orchestrator-loop_SPEC.md.
-- NOTIFY trigger on task_events (wake-up only; the cursor drain is the sole
-- delivery path) + the orchestrator's cursor bookkeeping.

CREATE FUNCTION task_events_notify() RETURNS trigger AS $$
BEGIN
  -- Payload is the event id ONLY: the engine re-reads the row, keeping the
  -- payload untrusted bookkeeping and miles under the notify byte limit.
  PERFORM pg_notify('task_events', NEW.id::text);
  RETURN NEW;
END
$$ LANGUAGE plpgsql;

CREATE TRIGGER task_events_notify AFTER INSERT ON task_events
  FOR EACH ROW EXECUTE FUNCTION task_events_notify();

-- Engine bookkeeping (one integer), not a task-like table.
CREATE TABLE orchestrator_cursor (
  name          TEXT PRIMARY KEY,
  last_event_id BIGINT NOT NULL DEFAULT 0,
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Seed at current max(id): first deploy must NOT replay pre-orchestrator
-- history (it would spawn delivery tasks for old done_local events).
INSERT INTO orchestrator_cursor (name, last_event_id)
  VALUES ('orchestrator', COALESCE((SELECT max(id) FROM task_events), 0));
