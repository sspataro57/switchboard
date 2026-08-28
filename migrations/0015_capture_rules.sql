-- 0015_capture_rules.sql — SWT-17 / docs/tickets/capture-rules_SPEC.md
-- ("Data model changes"). Deterministic project assignment for normalized
-- messages: rules as typed data, one decision row per evaluation, and the
-- retirement of the one special-case column that tried to do this job before.
--
-- Highest migration before this file is 0014_imap_mail.sql; production
-- schema_migrations was at 0014 when this was written, so 0015 is free.
--
-- Four statements' worth of change, in dependency order:
--   1. capture_rules      — the configuration the evaluator loads.
--   2. capture_decisions  — the append-only evaluation log (FKs capture_rules).
--   3. external_refs.system CHECK widened with 'slack' and 'gmail'.
--   4. projects.client_person_id DROPPED.
--
-- migrate (cmd/tools/migrate) runs each file inside ONE transaction, which is
-- what makes the CHECK drop/add in step 3 safe — the same reason 0009's
-- deliveries_channel_check swap was safe. Forward-only: there is no down
-- migration, and 0004's column is not recreated.

-- ---------------------------------------------------------------------------
-- 1. capture_rules — configuration, not work.
-- ---------------------------------------------------------------------------
-- No status, no assignee, no claim, nothing is ever "worked": this is not a
-- second tasks table (invariant 2). Rows are written only through the
-- capture_rule_add / capture_rule_set_enabled executor tools, so "who changed
-- the routing and when" is answerable from audit_events (invariant 3).
--
-- Deliberately NOT seeded here. Salvador's routing table is configuration with
-- an enabled flag and an audit trail, and migrations are forward-only and
-- unchecksummed — seeding it would put production routing into every test
-- database and make a rule edit a new migration. docs/runbooks/capture-rules.md
-- is the source of truth for the fixture rules.
--
-- criteria_type is the SQL spelling of the evaluator's rule kind; external_system
-- is the SQL spelling of its external source. Both enums are closed on purpose —
-- a criteria_type the evaluator does not implement would be accepted by the tool,
-- stored, listed by `opsctl capture-rules list`, and silently match nothing,
-- which is the failure this repo has already paid for three times.
--
-- external_system's enum tracks external_refs.system (widened in step 3 below);
-- keep the two lists in step if either ever changes again.
CREATE TABLE capture_rules (
  id              BIGSERIAL PRIMARY KEY,
  project_id      BIGINT NOT NULL REFERENCES projects(id),
  subproject      TEXT,
  criteria_type   TEXT NOT NULL CHECK (criteria_type IN
                    ('body_regex','sender','thread_key_prefix','thread_key_contains',
                     'source_slack_workspace','person')),
  pattern         TEXT NOT NULL,
  external_system TEXT CHECK (external_system IN
                    ('jira','github','upwork_crm','slack','gmail')),
  key_regex       TEXT,
  url_template    TEXT,
  priority        INTEGER NOT NULL DEFAULT 0,
  enabled         BOOLEAN NOT NULL DEFAULT true,
  note            TEXT,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (project_id, criteria_type, pattern)
);

-- The evaluator's load order, verbatim: ORDER BY priority DESC, id ASC over the
-- enabled rules. Priority is load-bearing — one Slack workspace feeds two
-- projects and nothing but this ordering separates them — so the index is not a
-- micro-optimisation, it is the query shape written down.
--
-- NOTE: regex patterns are compiled and REFUSED at insert time by
-- capture_rule_add, not here. Postgres cannot validate a Go RE2 pattern, and a
-- pattern that only fails to compile inside a CronJob is a silent routing outage.
CREATE INDEX capture_rules_eval_idx ON capture_rules (priority DESC, id) WHERE enabled;

-- ---------------------------------------------------------------------------
-- 2. capture_decisions — one row per evaluation, forever.
-- ---------------------------------------------------------------------------
-- `action` records the DECISION, `mode` records whether it was applied, so a
-- shadow row saying action='task' and a live row saying action='task' are
-- directly diffable. That is the whole point of shadow mode.
--
-- action='unmatched' is the only action with project_id IS NULL, and triage's
-- inbox keys on exactly that (SPEC §8b). The three states triage must keep apart
-- are: no row at all (unseen — SKIP), latest row unmatched (CONSUME), latest row
-- naming a project (NEVER re-triage). A missing row is not an unmatched row.
--
-- raw_source_item_id rides alongside message_id so a decision stays traceable to
-- the provider JSON and re-derivable after a re-normalization (invariant 1). It
-- is nullable: message_id is the identity, the raw link is the provenance.
--
-- Not policy_decisions: that table answers "was this TOOL CALL permitted", is
-- written only by audit.PGStore.RecordPolicy from inside Executor.Execute, and
-- has no column for a project, a message or a matched rule. This table is
-- written directly by internal/capture (the ai_runs / ai_extractions precedent);
-- tasks, external_refs and task_events are never written directly there.
CREATE TABLE capture_decisions (
  id                 BIGSERIAL PRIMARY KEY,
  -- ON DELETE CASCADE, deliberately. A decision is a record of evaluating a
  -- MESSAGE; without the message it means nothing, so cascading is the honest
  -- semantics rather than a convenience.
  --
  -- It also removes a live footgun. No production code deletes
  -- normalized_messages (verified: the only DELETEs are in tests), but 19 test
  -- suites clear their fixtures that way, and the capture pass writes decision
  -- rows over OTHER suites' fixture messages. Under `make integration`'s -p 1
  -- those suites would then fail inside cleanup with a foreign-key violation —
  -- which reads exactly like the cross-pollution pact breaking, sending the
  -- reader to the wrong place entirely.
  message_id         BIGINT NOT NULL REFERENCES normalized_messages(id) ON DELETE CASCADE,
  raw_source_item_id BIGINT REFERENCES raw_source_items(id),
  mode               TEXT NOT NULL CHECK (mode IN ('shadow','live')),
  matched_rule_id    BIGINT REFERENCES capture_rules(id),
  project_id         BIGINT REFERENCES projects(id),
  matched_rule_ids   BIGINT[] NOT NULL DEFAULT '{}',
  ambiguous          BOOLEAN NOT NULL DEFAULT false,
  action             TEXT NOT NULL CHECK (action IN
                       ('unmatched','attributed','task','task_log')),
  external_system    TEXT,
  external_key       TEXT,
  task_id            BIGINT REFERENCES tasks(id),
  reason             TEXT,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- The three-state contract in §8 rests entirely on this equivalence: an
  -- 'unmatched' decision is the one with no project, and every other action
  -- names one. Triage's inbox filter and its project lookup both assume it, and
  -- a row violating it would be served as inbox WHILE carrying a project —
  -- silently, with no error. It was a code-only invariant; one line makes it
  -- structural, and a hand-written row is exactly how this repo has been bitten.
  CONSTRAINT capture_decisions_unmatched_has_no_project
    CHECK ((action = 'unmatched') = (project_id IS NULL))
);

-- One live action per message, FOREVER. Structural, not advisory — this index is
-- the entire live dedup, and it is why `--all` (re-evaluate messages that already
-- have decision rows) is refused in live mode: task_append_log has no dedup of
-- its own, so a live replay would double-append silently.
--
-- LANDMINE: this is a PARTIAL unique index. Any ON CONFLICT against it MUST
-- restate the predicate —
--   ON CONFLICT (message_id) WHERE mode = 'live' DO NOTHING
-- — exactly as task_events_outbound_observed_uniq (0013) requires. Arbiter
-- inference matches a partial index only when the predicate is repeated;
-- omitting it raises "no unique or exclusion constraint matching the ON CONFLICT
-- specification" at RUNTIME, inside a CronJob, where nobody is watching.
CREATE UNIQUE INDEX capture_decisions_live_uniq
  ON capture_decisions (message_id) WHERE mode = 'live';

-- The pending filter's probe ("does this message already have a live decision")
-- and the latest-decision lookups triage does per message (SPEC §8a).
CREATE INDEX capture_decisions_message_idx ON capture_decisions (message_id, id DESC);

-- The report's per-project rollups over a time window (opsctl capture-rules
-- report --since).
CREATE INDEX capture_decisions_project_idx ON capture_decisions (project_id, created_at);

-- ---------------------------------------------------------------------------
-- 3b. external_refs gains ONE TASK PER TICKET, structurally.
--
-- Capture answers "does this ticket already have a task?" by reading this table
-- (rules_store.go's `SELECT task_id FROM external_refs WHERE system=$1 AND
-- external_key=$2`), and until now nothing guaranteed the answer was unique —
-- the table had no unique constraint of any kind. link_external_ref is on the
-- agent surface, so a worker could claim a ticket for a task of its choosing and
-- every later notification about it would append there instead, silently, with
-- capture simply believing what it read.
--
-- The SPEC made capture_rule_add and capture_rule_set_enabled humanOnly on the
-- reasoning that "an agent must not be able to redirect the funnel". This is the
-- same funnel through a different door, and this ticket is what turns the table
-- from inert (zero rows, no systematic reader) into the thing that decides where
-- a client's ticket traffic lands.
--
-- Free to add now, at zero rows. A second claim on the same ticket now fails
-- loudly at the executor instead of quietly re-routing it.
--
-- Accepted cost, stated so nobody is surprised: one ticket can no longer be
-- attached to two tasks — no task-plus-follow-up on the same key. If that is
-- ever wanted, it needs a deliberate design (a link type, or a nullable
-- "primary" flag), not the absence of a constraint.
CREATE UNIQUE INDEX external_refs_system_key_uniq ON external_refs (system, external_key);

-- 3. external_refs.system gains 'slack' and 'gmail'.
-- ---------------------------------------------------------------------------
-- The engine's dedup key is (system, external_key), and for non-ticket sources
-- the external_key is the thread_key verbatim. Widening the existing enum keeps
-- a thread-keyed dedup ref expressible in the existing vocabulary instead of
-- inventing a parallel table.
--
-- It also closes the gap internal/capture/observe.go documents ("external_refs
-- keys are agent-chosen free text that cannot be matched against a thread_key"):
-- for the rows THIS engine writes, they can be — and the drafts rework in §9
-- depends on exactly that.
--
-- The drop/add is safe because migrate wraps this file in one transaction; the
-- constraint is never absent to any other session. Constraint name verified
-- against production (0001 created it inline, so Postgres named it
-- external_refs_system_check).
ALTER TABLE external_refs DROP CONSTRAINT external_refs_system_check;
ALTER TABLE external_refs ADD CONSTRAINT external_refs_system_check
  CHECK (system IN ('jira','github','upwork_crm','slack','gmail'));

-- ---------------------------------------------------------------------------
-- 4. projects.client_person_id is DROPPED.
-- ---------------------------------------------------------------------------
-- Added by 0004 as "one project per client person for triage-default purposes,
-- populated manually via psql". It never was: nothing in the codebase writes it
-- outside test fixtures, and it is NULL on all four production project rows
-- (verified read-only before writing this migration, 4 rows / 0 non-null). The
-- lookup it fed resolved nothing, for anything, because it depended on
-- normalized_threads.participants, which is '[]' for 16,959 of 16,985 threads.
--
-- Project selection moves wholly into capture_rules. The criteria_type 'person'
-- above is what subsumes this column: one rule row does the job the special-case
-- column did, and it becomes live the day the three sinks start populating
-- participants — with no schema change.
--
-- NO BACKFILL IS NEEDED OR POSSIBLE. The column is the referencing side of
-- projects_client_person_id_fkey, so the drop takes that FK with it and orphans
-- nothing: no row in any table points AT this column. Verified against
-- production — the only constraints referencing projects are tasks.project_id,
-- decisions.project_id and plan_imports.project_id, none of which touch it, and
-- there is no index and no view over it.
--
-- What the drop DOES release: people rows that were previously pinned by this FK
-- become deletable. Two integration cleanups exploited that pin with
-- `... AND id NOT IN (SELECT client_person_id FROM projects ...)`; those
-- subqueries now fail at RUNTIME with "column does not exist" — a dropped column
-- does not fail a query at compile time. See SPEC criteria 17 and 27; the
-- affected files are named in the SPEC's "SPEC re-validated 2026-08-28" section.
--
-- On a development or compose database a fixture may hold a non-null value here;
-- it is discarded. That is deliberate — a guard refusing to drop a non-empty
-- column would wedge `make migrate` on leftover test fixtures with no way
-- forward, which is a worse failure than losing a fixture id.
--
-- Ordering constraint for the deploy, not for this file: apply 0015 and ship the
-- image carrying the reworked internal/drafts TOGETHER. An old image against the
-- new schema fails on `SELECT ... p.client_person_id` — though only in
-- cmd/drafts, which is not deployed, which is precisely why the drop is
-- survivable today.
ALTER TABLE projects DROP COLUMN client_person_id;
