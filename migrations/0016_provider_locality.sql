-- 0016 — provider locality (SWT-21)
--
-- Personal content may only be processed by a model running locally. This
-- migration adds the column that says so per project, and creates the `personal`
-- project itself. It does NOT seed capture rules: SWT-17 established that routing
-- is configuration with an enabled flag and an audit trail, and seeding it would
-- put production routing into every test database while making a rule edit a new
-- migration. The rules go in through `opsctl capture-rules add`, from a measured
-- sender list, per docs/runbooks/provider-locality.md.
--
-- The boundary does NOT depend on those rules being complete. Under SWT-21 an
-- unmatched message is RESTRICTED, so a personal message no rule happens to match
-- is still kept local. Rule completeness must not be load-bearing for a security
-- property; the rules are about routing quality, not about containment.

-- ---------------------------------------------------------------------------
-- STATEMENT ORDER BELOW IS LOAD-BEARING. Do not reorder.
--
--   1. ALTER   — add the column with the FAIL-CLOSED default
--   2. UPDATE  — existing rows, which are all real work, become 'any'
--   3. INSERT  — personal, explicitly local_only
--
-- Swap 2 and 3 and the blanket UPDATE catches the row it was never meant to
-- touch: `personal` silently becomes 'any', the boundary is open for exactly the
-- project it exists to protect, and NOTHING FAILS. No error, no failing test —
-- the migration succeeds and the property is gone. The verification step in the
-- runbook checks the resulting values directly for this reason.
-- ---------------------------------------------------------------------------

-- 1. The column. DEFAULT 'local_only' is the fail-closed choice (open question
--    Q1): a leak is irreversible, a stall is one UPDATE and shows up in the
--    skipped lane. When one direction is recoverable and the other is not, the
--    default belongs on the recoverable side — the same asymmetry that made
--    SWT-19 refuse rather than guess a room.
--
--    Consequence, stated because it will be met before it is read: 23 test
--    fixtures INSERT INTO projects without naming this column, and will now get
--    local_only. Those suites must set ai_locality='any' or they will start
--    SKIPPING rather than failing — passing while exercising nothing, which is
--    worse than a red test. No non-test code creates projects at all, so the
--    stall risk in production is limited to hand-created rows.
ALTER TABLE projects
  ADD COLUMN ai_locality TEXT NOT NULL DEFAULT 'local_only'
    CHECK (ai_locality IN ('local_only','any'));

-- 2. Every project that exists today is client work that already flows through a
--    hosted provider. Preserve that exactly; this ticket restricts, it never
--    widens, and it must not stall live work on the day it lands.
UPDATE projects SET ai_locality = 'any';

-- 3. Now, and only now, the personal project — explicitly local_only, so it is
--    unaffected by the blanket UPDATE above.
--
--    execution/delivery are 'manual'/'dashboard': nothing about personal mail is
--    automated by this ticket. It is attribution only until a classifier earns
--    the right to create tasks (SWT-22).
--    client is NULL, and that is LOAD-BEARING rather than cosmetic. This SPEC
--    dropped task_get_next's locality predicate on the reasoning that the
--    existing `p.client = $1` already excludes a NULL-client project. Give this
--    row a client name and that reasoning silently becomes false — a worker
--    could claim a personal task, and the guard that was deemed unnecessary
--    would have been necessary after all.
--
--    DO NOTHING, not DO UPDATE: re-running a migration must not overwrite a
--    value an operator has deliberately changed since.
INSERT INTO projects (name, slug, client, execution, delivery, ai_locality)
VALUES ('Personal', 'personal', NULL, 'manual', 'dashboard', 'local_only')
ON CONFLICT (slug) DO NOTHING;

-- NO INDEX on ai_locality, deliberately. Every read of this column reaches
-- `projects` by primary key already (triage joins through capture_decisions,
-- drafts through tasks.project_id), so a partial index on (id) would never be
-- chosen; and the table holds tens of rows. The first cut added one anyway. An
-- index nothing uses is not free in a forward-only migration — it is a
-- permanent claim that some query needs it, which the next reader has to
-- disprove.
