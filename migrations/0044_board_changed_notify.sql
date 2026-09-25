-- 0044 board-streaming (SWT-89): the board's change feed.
--
-- Every row change on a table whose rows change what /tasks shows NOTIFYs
-- the board-only channel `board_changed`, with payload '<table>:<id>'. The
-- dashboard holds one LISTEN per process and pushes a change signal to each
-- open board over SSE; the browser re-fetches its own server-rendered /tasks.
--
-- Why a trigger: the writers live in about ten processes (connectors, watchers,
-- orchestratord, the MCP servers, opsctl), and a trigger is the only place that
-- sees every write. Why not task_events: task_mark_activity, create_task, a
-- task_signal refresh and the promotion/ref/dismissal rows write no event, and
-- that channel is the orchestrator's wake-up (0003 is untouched).
--
-- The payload is a wake-up only; the dashboard ignores it. It names the row so
-- integration tests can wait for their own notification. An UPDATE that changes
-- nothing is silent (the WHEN clause). normalized_messages and projects are
-- deliberately NOT triggered: the board's 60 s tick covers them.

CREATE OR REPLACE FUNCTION board_changed_notify() RETURNS trigger AS $$
DECLARE
    row_id text;
BEGIN
    IF TG_OP = 'DELETE' THEN
        row_id := OLD.id::text;
    ELSE
        row_id := NEW.id::text;
    END IF;
    PERFORM pg_notify('board_changed', TG_TABLE_NAME || ':' || row_id);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS board_changed_tasks_insdel ON tasks;
CREATE TRIGGER board_changed_tasks_insdel
    AFTER INSERT OR DELETE ON tasks
    FOR EACH ROW EXECUTE FUNCTION board_changed_notify();
DROP TRIGGER IF EXISTS board_changed_tasks_upd ON tasks;
CREATE TRIGGER board_changed_tasks_upd
    AFTER UPDATE ON tasks
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*) EXECUTE FUNCTION board_changed_notify();

DROP TRIGGER IF EXISTS board_changed_task_dismissals_insdel ON task_dismissals;
CREATE TRIGGER board_changed_task_dismissals_insdel
    AFTER INSERT OR DELETE ON task_dismissals
    FOR EACH ROW EXECUTE FUNCTION board_changed_notify();
DROP TRIGGER IF EXISTS board_changed_task_dismissals_upd ON task_dismissals;
CREATE TRIGGER board_changed_task_dismissals_upd
    AFTER UPDATE ON task_dismissals
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*) EXECUTE FUNCTION board_changed_notify();

DROP TRIGGER IF EXISTS board_changed_classify_promotions_insdel ON classify_promotions;
CREATE TRIGGER board_changed_classify_promotions_insdel
    AFTER INSERT OR DELETE ON classify_promotions
    FOR EACH ROW EXECUTE FUNCTION board_changed_notify();
DROP TRIGGER IF EXISTS board_changed_classify_promotions_upd ON classify_promotions;
CREATE TRIGGER board_changed_classify_promotions_upd
    AFTER UPDATE ON classify_promotions
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*) EXECUTE FUNCTION board_changed_notify();

DROP TRIGGER IF EXISTS board_changed_external_refs_insdel ON external_refs;
CREATE TRIGGER board_changed_external_refs_insdel
    AFTER INSERT OR DELETE ON external_refs
    FOR EACH ROW EXECUTE FUNCTION board_changed_notify();
DROP TRIGGER IF EXISTS board_changed_external_refs_upd ON external_refs;
CREATE TRIGGER board_changed_external_refs_upd
    AFTER UPDATE ON external_refs
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*) EXECUTE FUNCTION board_changed_notify();
