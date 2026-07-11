-- 0001_initial.sql — switchboard core schema (SWT-1 / docs/tickets/01-schema-executor_SPEC.md).
-- The full "Core schema" vocabulary from CLAUDE.md, forward-only: later steps
-- extend these tables with new numbered migrations, never edit this file.
-- Columns not pinned by CLAUDE.md are deliberately minimal.

CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS vector;

-- Ingestion spine (invariant 1: raw before normalize)

CREATE TABLE source_accounts (
  id                       BIGSERIAL PRIMARY KEY,
  provider                 TEXT NOT NULL,
  account_email            TEXT NOT NULL,
  refresh_token_encrypted  BYTEA,
  scopes                   TEXT[] NOT NULL DEFAULT '{}',
  domain_default           TEXT,
  send_enabled             BOOLEAN NOT NULL DEFAULT false,
  calendar_in_availability BOOLEAN NOT NULL DEFAULT true,
  created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (provider, account_email)
);

CREATE TABLE sync_runs (
  id                BIGSERIAL PRIMARY KEY,
  source_account_id BIGINT NOT NULL REFERENCES source_accounts(id),
  started_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at       TIMESTAMPTZ,
  status            TEXT NOT NULL DEFAULT 'running' CHECK (status IN ('running','ok','error')),
  stats             JSONB NOT NULL DEFAULT '{}',
  error             TEXT
);

CREATE TABLE raw_source_items (
  id                BIGSERIAL PRIMARY KEY,
  source_account_id BIGINT NOT NULL REFERENCES source_accounts(id),
  external_id       TEXT NOT NULL,
  raw_json          JSONB NOT NULL,
  content_hash      TEXT NOT NULL,
  ingested_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  normalized_at     TIMESTAMPTZ, -- NULL until normalized: "raw landed first" is queryable
  UNIQUE (source_account_id, external_id)
);
CREATE INDEX raw_source_items_content_hash_idx ON raw_source_items (content_hash);

-- Canonical objects (invariant 2: one funnel)

CREATE TABLE normalized_threads (
  id                 BIGSERIAL PRIMARY KEY,
  raw_source_item_id BIGINT REFERENCES raw_source_items(id),
  thread_key         TEXT,
  subject            TEXT,
  participants       JSONB NOT NULL DEFAULT '[]',
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE normalized_messages (
  id                  BIGSERIAL PRIMARY KEY,
  raw_source_item_id  BIGINT NOT NULL REFERENCES raw_source_items(id),
  thread_id           BIGINT REFERENCES normalized_threads(id),
  direction           TEXT CHECK (direction IN ('inbound','outbound')),
  external_message_id TEXT,
  sent_at             TIMESTAMPTZ,
  body_text           TEXT,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE normalized_events (
  id                 BIGSERIAL PRIMARY KEY,
  raw_source_item_id BIGINT NOT NULL REFERENCES raw_source_items(id),
  starts_at          TIMESTAMPTZ,
  ends_at            TIMESTAMPTZ,
  attendees          JSONB NOT NULL DEFAULT '[]',
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE normalized_documents (
  id                 BIGSERIAL PRIMARY KEY,
  raw_source_item_id BIGINT NOT NULL REFERENCES raw_source_items(id),
  title              TEXT,
  uri                TEXT,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE people (
  id           BIGSERIAL PRIMARY KEY,
  display_name TEXT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE person_identities (
  id        BIGSERIAL PRIMARY KEY,
  person_id BIGINT NOT NULL REFERENCES people(id),
  provider  TEXT NOT NULL,
  value     TEXT NOT NULL,
  UNIQUE (provider, value)
);

-- Work spine

CREATE TABLE projects (
  id                BIGSERIAL PRIMARY KEY,
  name              TEXT NOT NULL,
  slug              TEXT NOT NULL UNIQUE,
  client            TEXT,
  execution         TEXT NOT NULL DEFAULT 'manual' CHECK (execution IN ('auto','manual')),
  delivery          TEXT NOT NULL DEFAULT 'dashboard' CHECK (delivery IN ('auto','dashboard','console')),
  repo_path         TEXT,
  send_from_account BIGINT REFERENCES source_accounts(id),
  policies          JSONB NOT NULL DEFAULT '{}',
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE tasks (
  id                 BIGSERIAL PRIMARY KEY,
  project_id         BIGINT NOT NULL REFERENCES projects(id),
  subproject         TEXT,
  parent_id          BIGINT REFERENCES tasks(id),
  title              TEXT NOT NULL,
  body               TEXT,
  assignee_type      TEXT NOT NULL DEFAULT 'human' CHECK (assignee_type IN ('human','claude')),
  worker_type        TEXT,
  status             TEXT NOT NULL DEFAULT 'ready' CHECK (status IN
                       ('holding','ready','claimed','in_progress','needs_feedback',
                        'pr_open','awaiting_ci','awaiting_merge','done_locally',
                        'delivered','closed','blocked')),
  autonomy           TEXT,
  plan_order         INTEGER,
  priority           INTEGER NOT NULL DEFAULT 0,
  execution_override TEXT CHECK (execution_override IN ('auto','manual')),
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX tasks_status_project_idx ON tasks (status, project_id);

CREATE TABLE task_dependencies (
  task_id            BIGINT NOT NULL REFERENCES tasks(id),
  depends_on_task_id BIGINT NOT NULL REFERENCES tasks(id),
  PRIMARY KEY (task_id, depends_on_task_id)
);

CREATE TABLE task_events (
  id         BIGSERIAL PRIMARY KEY,
  task_id    BIGINT NOT NULL REFERENCES tasks(id),
  event_type TEXT NOT NULL,
  payload    JSONB NOT NULL DEFAULT '{}',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE task_claims (
  id          BIGSERIAL PRIMARY KEY,
  task_id     BIGINT NOT NULL REFERENCES tasks(id),
  worker_id   TEXT NOT NULL,
  claimed_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at  TIMESTAMPTZ,
  released_at TIMESTAMPTZ
);

CREATE TABLE worker_heartbeats (
  id        BIGSERIAL PRIMARY KEY,
  worker_id TEXT NOT NULL UNIQUE,
  client    TEXT,
  state     TEXT NOT NULL,
  task_id   BIGINT REFERENCES tasks(id),
  last_seen TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE feedback_requests (
  id          BIGSERIAL PRIMARY KEY,
  task_id     BIGINT NOT NULL REFERENCES tasks(id),
  question    TEXT NOT NULL,
  answer      TEXT,
  status      TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','answered','cancelled')),
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  answered_at TIMESTAMPTZ
);

CREATE TABLE external_refs (
  id           BIGSERIAL PRIMARY KEY,
  task_id      BIGINT NOT NULL REFERENCES tasks(id),
  system       TEXT NOT NULL CHECK (system IN ('jira','github','upwork_crm')),
  external_key TEXT NOT NULL,
  external_url TEXT,
  sync_cursor  TEXT,
  direction    TEXT
);

-- Outbound + governance (invariants 3, 4, 7)

CREATE TABLE deliveries (
  id               BIGSERIAL PRIMARY KEY,
  task_id          BIGINT NOT NULL REFERENCES tasks(id),
  channel          TEXT NOT NULL CHECK (channel IN
                     ('gmail','jira_comment','upwork_chat','calendar','github_review')),
  target_ref       TEXT,
  body             TEXT,
  status           TEXT NOT NULL DEFAULT 'drafted' CHECK (status IN
                     ('drafted','approved','sending','sent','failed')),
  policy_result    JSONB NOT NULL DEFAULT '{}',
  sent_external_id TEXT, -- set once, never resend while present (invariant 4)
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX deliveries_sent_external_idx
  ON deliveries (channel, sent_external_id) WHERE sent_external_id IS NOT NULL;

CREATE TABLE decisions (
  id         BIGSERIAL PRIMARY KEY,
  project_id BIGINT NOT NULL REFERENCES projects(id),
  title      TEXT NOT NULL,
  body       TEXT,
  created_by TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE ai_runs (
  id                BIGSERIAL PRIMARY KEY,
  worker_type       TEXT,
  provider          TEXT,
  model             TEXT,
  input             JSONB NOT NULL DEFAULT '{}',
  output            JSONB NOT NULL DEFAULT '{}',
  status            TEXT,
  prompt_tokens     INTEGER,
  completion_tokens INTEGER,
  latency_ms        INTEGER,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE ai_extractions (
  id                 BIGSERIAL PRIMARY KEY,
  ai_run_id          BIGINT NOT NULL REFERENCES ai_runs(id),
  raw_source_item_id BIGINT REFERENCES raw_source_items(id),
  fields             JSONB NOT NULL DEFAULT '{}',
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE audit_events (
  id           BIGSERIAL PRIMARY KEY,
  actor        TEXT NOT NULL,
  tool         TEXT NOT NULL,
  args         JSONB NOT NULL DEFAULT '{}',
  status       TEXT NOT NULL CHECK (status IN ('started','ok','error','denied')),
  error        TEXT,
  task_id      BIGINT REFERENCES tasks(id),
  started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  completed_at TIMESTAMPTZ
);

CREATE TABLE policy_decisions (
  id             BIGSERIAL PRIMARY KEY,
  audit_event_id BIGINT REFERENCES audit_events(id),
  tool           TEXT NOT NULL,
  action         TEXT,
  channel        TEXT,
  decision       TEXT NOT NULL CHECK (decision IN ('allow','deny','needs_approval')),
  rule           TEXT,
  reason         TEXT,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE approvals (
  id           BIGSERIAL PRIMARY KEY,
  subject_type TEXT NOT NULL,
  subject_id   BIGINT NOT NULL,
  status       TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','approved','rejected')),
  decided_by   TEXT,
  decided_at   TIMESTAMPTZ,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- RAG substrate

CREATE TABLE content_chunks (
  id           BIGSERIAL PRIMARY KEY,
  source_table TEXT NOT NULL,
  source_id    BIGINT NOT NULL,
  chunk_index  INTEGER NOT NULL DEFAULT 0,
  text         TEXT NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE embeddings (
  id               BIGSERIAL PRIMARY KEY,
  content_chunk_id BIGINT NOT NULL REFERENCES content_chunks(id),
  model            TEXT NOT NULL,
  -- dimension unpinned until the first embedder lands; pin it (and add the
  -- index) in the migration that ships the backfill
  embedding        vector,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
