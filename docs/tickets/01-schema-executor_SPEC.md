> Jira: SWT-1

# 01-schema-executor — Schema + migrations; executor + audit skeleton

## Source

Build-order step 1, quoted from CLAUDE.md:

> 1. Schema + migrations; executor + audit skeleton.

Constrained by the "Core schema (starting point — extend, don't fork)" section and
invariants 1–4 and 7 of CLAUDE.md.

## Goal

Bootstrap the Go module, ship the full core schema as forward-only numbered
migrations with a migrate tool, and build the executor pipeline
(validate → policy check → audit start → handler → audit complete) with one
internal tool (`create_task`) wired through it end to end.

**Usable alone means:** a real `ops` database carrying the complete canonical
schema, plus an audited, policy-gated way to create task rows from a small CLI —
the substrate every later step writes into, and immediately usable for manual
task capture (step 4 explicitly says "manual task creation is fine here"; this
step provides the mechanism).

## Acceptance criteria

1. `go build ./...` succeeds on a fresh clone (Go module
   `github.com/sspataro57/switchboard`, matching the git remote).
2. `go run ./cmd/tools/migrate --dir migrations` against a fresh Postgres
   applies `0001_initial.sql` cleanly; a second run is a no-op (every migration
   logged "skip applied"). `schema_migrations` records the applied version.
3. After migration, every table named in CLAUDE.md's core schema section exists
   (list under "Data model changes" below), with the `pgcrypto` and `vector`
   (pgvector) extensions enabled. Verifiable via `psql -c "\dt"`.
4. `go test ./...` passes. Executor unit tests run with **zero network / zero
   Postgres** (in-memory fakes for audit store and policy checker) and cover:
   a. Happy path: audit row written with status `started` before the handler
      runs, updated to `ok` after; handler result returned to caller.
   b. Policy denial: checker returns deny → handler is **never invoked**, audit
      row ends `denied`, a policy decision (deny + reason) is recorded.
   c. Validation failure: malformed args rejected **before** the policy check
      and handler; audit row ends `error` with the validation message.
   d. Handler error: audit row ends `error`, error wrapped with context.
   e. Unknown tool name: rejected, audited.
5. Integration test (build-tagged or env-gated, against the dockerized local
   Postgres) proves the pg-backed audit/policy stores write real
   `audit_events` and `policy_decisions` rows for one `Execute` call.
6. Smoke ("usable alone"): with a project row seeded via psql,
   `go run ./cmd/opsctl create-task --project <slug> --title "..."` inserts a
   `tasks` row (status `ready`) **through the executor**, and matching
   `audit_events` + `policy_decisions` rows are visible in psql.
7. No handler is callable except through `Executor.Execute` — handlers are
   unexported and reachable only via the tool registry (invariant 3, checked in
   review).
8. RESOLVED (Q2) — the real apply is part of done: the `ops` database exists on
   pg-main (CNPG), migrations applied to it cleanly, and the connection recipe
   (port-forward / connstring) is recorded in INSTITUTIONAL_KNOWLEDGE.md
   "Environment facts". The smoke check (criterion 6) runs against the real
   `ops` db, not only the local compose Postgres.

## Data model changes

Migration: `migrations/0001_initial.sql` (single file; the numbered forward-only
scheme and the `schema_migrations` bookkeeping table come from the job-agent
migrator, copied — see "Sibling patterns"). No down migrations.

DDL conventions (see "Decisions made unilaterally"): `BIGSERIAL` PKs,
`TIMESTAMPTZ` with `DEFAULT now()`, status/kind columns as `TEXT` + `CHECK`
constraints (not PG enums), `JSONB NOT NULL DEFAULT '{}'` for metadata bags.
Table names are exactly CLAUDE.md's vocabulary — no synonyms.

Tables to create (key columns / constraints that carry invariants; columns not
pinned by CLAUDE.md are minimal and will be extended forward-only by the step
that first uses them):

**Ingestion spine (invariant 1 — raw-first):**
- `source_accounts` — provider, account_email, refresh_token_encrypted (BYTEA,
  pgcrypto), scopes, domain_default, send_enabled BOOL, calendar_in_availability
  BOOL.
- `sync_runs` — source_account_id FK, started_at, finished_at, status, stats
  JSONB, error.
- `raw_source_items` — source_account_id FK, external_id, raw_json JSONB NOT
  NULL, content_hash TEXT NOT NULL, ingested_at, normalized_at NULLable.
  `UNIQUE (source_account_id, external_id)`; index on content_hash. The
  nullable `normalized_at` is what makes "raw before normalize" checkable.

**Canonical objects (invariant 2 — one funnel):**
- `normalized_messages`, `normalized_threads`, `normalized_events`,
  `normalized_documents` — each with `raw_source_item_id` FK (provenance) plus
  minimal type-specific columns (thread key/subject/participants; message
  direction/sent_at/body_text/external_message_id; event start/end/attendees;
  document title/uri). Step 2 (Upwork normalize) and step 7 (Gmail/Calendar)
  extend these; do not over-specify now.
- `people`, `person_identities` — person_id FK, provider, identity value;
  `UNIQUE (provider, value)`. No auto-merge machinery this step.

**Work spine:**
- `projects` — name/slug, client, execution `CHECK (execution IN
  ('auto','manual'))`, delivery `CHECK (delivery IN
  ('auto','dashboard','console'))`, repo_path, send_from_account FK →
  source_accounts (nullable), policies JSONB.
- `tasks` — the ONE task table. project_id FK, subproject, assignee_type
  `CHECK IN ('human','claude')`, worker_type, status `CHECK` over the full
  machine from CLAUDE.md: `holding, ready, claimed, in_progress,
  needs_feedback, pr_open, awaiting_ci, awaiting_merge, done_locally,
  delivered, closed, blocked`; autonomy, parent_id self-FK, plan_order,
  priority, execution_override (nullable per-task override), title, body,
  created_at/updated_at. Index on (status, project_id) for future claim scans.
- `task_dependencies` — (task_id, depends_on_task_id) composite PK, both FK →
  tasks.
- `task_events` — task_id FK, event_type, payload JSONB, created_at. Table
  only; the LISTEN/NOTIFY trigger is step 5's.
- `task_claims` — task_id FK, worker_id, claimed_at, expires_at, released_at.
  Table only; claim SQL (`FOR UPDATE SKIP LOCKED`) arrives with steps 4–5.
- `worker_heartbeats` — worker_id, client, state, task_id nullable, last_seen.
  Populated by step 3.
- `feedback_requests` — task_id FK, question, answer, status, created_at,
  answered_at.
- `external_refs` — task_id FK, system `CHECK IN ('jira','github',
  'upwork_crm')`, external_key, external_url, sync_cursor, direction.

**Outbound + governance (invariants 3, 4, 7):**
- `deliveries` — task_id FK, channel `CHECK IN ('gmail','jira_comment',
  'upwork_chat','calendar','github_review')`, target_ref, body, status
  `CHECK IN ('drafted','approved','sending','sent','failed')`, policy_result
  JSONB, sent_external_id TEXT nullable + partial unique index
  `(channel, sent_external_id) WHERE sent_external_id IS NOT NULL`
  (idempotency backstop for invariant 4). No send code this step.
- `decisions` — project_id FK, title, body, created_by, created_at.
- `ai_runs` — worker_type, provider, model, input JSONB, output JSONB, status,
  token/latency columns, created_at. `ai_extractions` — ai_run_id FK,
  raw_source_item_id FK nullable, fields JSONB (per-field confidence lands in
  step 6).
- `policy_decisions` — audit_event_id FK nullable, tool, action/channel,
  decision `CHECK IN ('allow','deny','needs_approval')`, rule, reason,
  created_at. Written by the executor's policy step.
- `approvals` — subject_type, subject_id, status, decided_by, decided_at.
- `audit_events` — id, actor, tool, args JSONB, status `CHECK IN ('started',
  'ok','error','denied')`, error TEXT, task_id nullable, started_at,
  completed_at. Written by the executor: one row per tool call, inserted
  `started`, updated to terminal status (see Decisions).

**RAG substrate:**
- `content_chunks` — source_table, source_id, chunk_index, text, created_at.
- `embeddings` — content_chunk_id FK → content_chunks, model TEXT, embedding
  `vector` (dimension left unpinned until the first embedder lands; pin it in
  the migration that adds the backfill). RESOLVED (Q1): create now — pg-main's
  CNPG image already ships pgvector; local compose uses `pgvector/pgvector`
  instead of stock `postgres`.

Also in 0001: `CREATE EXTENSION IF NOT EXISTS pgcrypto;` and
`CREATE EXTENSION IF NOT EXISTS vector;`.

## API / MCP tool changes

No MCP server this step (that is step 4). What ships is the **executor path**
every future tool will hook into:

- `internal/executor` — `Executor.Execute(ctx, Call) (Result, error)` where
  `Call{Tool string, Actor string, Args json.RawMessage, TaskID *int64}`.
  Pipeline, in order, no skips: registry lookup → per-tool validate →
  `policy.Checker.Check` → audit start row → handler → audit complete row.
  Denials and errors also complete the audit row. Dependencies (audit store,
  policy checker, tool registry) are interfaces so the pipeline is
  unit-testable with fakes (invariant-7 discipline applied to the executor).
- `internal/policy` — `Checker` interface + a static default for step 1:
  registered internal tools allow (decision recorded), everything else deny.
  The real policy matrix is later; the *hook and the recording* are now.
- One internal tool registered: **`create_task`** — args
  `{project (slug), title, body?, assignee_type? (default human),
  priority?, subproject?}`; resolves project slug → id, inserts a `tasks` row
  with status `ready`; returns the new task id. Name reuses CLAUDE.md's
  `create_task` vocabulary; step 4's MCP `create_child` will wrap the same
  registry path.
- `cmd/opsctl` — minimal CLI that builds an `Executor` with the pg-backed
  stores and calls `Execute`. It is a *client* of the executor, not a side
  door: no direct table writes for tool actions.
- `cmd/tools/migrate` — the migrator (adapted from job-agent), reads
  `DATABASE_URL`, `--dir migrations`.

## MQTT topics

None. (Heartbeat contract is step 3.)

## Files likely to touch

All new — greenfield repo (only `CLAUDE.md`, `.claude/`, `docs/` exist today):

- `go.mod` — module `github.com/sspataro57/switchboard`, Go 1.25; dependency:
  `github.com/jackc/pgx/v5` (pool + conn). No other runtime deps.
- `migrations/0001_initial.sql`
- `cmd/tools/migrate/main.go`
- `cmd/opsctl/main.go`
- `internal/executor/executor.go`, `internal/executor/registry.go`,
  `internal/executor/executor_test.go` (fakes live in the test file or
  `internal/executor/fakes_test.go`)
- `internal/policy/policy.go`, `internal/policy/policy_test.go`
- `internal/audit/audit.go` (Store interface), `internal/audit/pg.go`,
  `internal/audit/mem.go`
- `internal/store/pg.go` (pgxpool construction from `DATABASE_URL`)
- `internal/tools/createtask.go` (unexported handler + `Register(...)`)
- `docker-compose.yml` (local Postgres for integration tests — image
  `pgvector/pgvector`, not stock `postgres`, since 0001 needs the vector
  extension), `Makefile` (`db-up`, `migrate`, `test`) — record the compose
  usage in INSTITUTIONAL_KNOWLEDGE.md "Test infrastructure" once it exists.

## In scope

- Go module bootstrap, layout above.
- Migrator tool + `0001_initial.sql` covering the whole core schema section.
- Executor pipeline + static policy stub + audit store (pg + in-memory).
- `create_task` internal tool + `opsctl` CLI.
- Local Postgres compose + integration test wiring.
- Establishing access to pg-main from the workstation, creating the `ops`
  database, applying 0001 to it, and recording the connection recipe in
  INSTITUTIONAL_KNOWLEDGE.md (Q2: real apply is in step-1 scope).

## Out of scope (do not bundle)

- **Step 2**: Upwork CRM connector, any `raw_source_items` writes, normalizers.
- **Step 3**: MQTT anything (heartbeats, fleet view). No MQTT client dep.
- **Step 4**: MCP server, task claim SQL, worker wrapper. `create_task` here is
  an internal CLI proof of the executor path, not the MCP surface.
- **Step 5**: orchestrator loop, `task_events` NOTIFY trigger, claim expiry,
  cron templates.
- Real policy matrix rules, autonomy promotion, kill switch (table hooks exist;
  logic later).
- Dashboard, Keycloak, k8s manifests / Dockerfiles / registry pushes (deploy
  ships when there's a long-running service to deploy; `Dockerfile.migrate`
  pattern noted for then).
- Identity-merge machinery, embeddings backfill, pgcrypto encrypt/decrypt
  helpers (step 7 needs them for OAuth tokens).

## Invariants that apply

- **1. Raw-first** — no connector code this step, but the schema must make the
  invariant enforceable: `raw_source_items` ships with `raw_json` +
  `content_hash` NOT NULL, unique external id per account, and nullable
  `normalized_at` so "raw landed before normalize" is a queryable fact.
- **2. One funnel** — exactly one `tasks` table is created; no queue tables.
  `task_claims`/`task_events` are bookkeeping *about* tasks, not work tables.
- **3. Everything through the executor** — this step *builds* the path. The
  demand on this code: `create_task`'s handler is unexported and registered,
  never exported for direct call; `opsctl` goes through `Execute`; the pipeline
  cannot skip stages (denial/validation failures still audit). No raw_sql /
  raw_api tool is registered.
- **4. Nothing external without a delivery row** — schema-level only:
  `deliveries` status CHECK encodes the drafted→approved→sending→sent|failed
  machine and the partial unique index on `sent_external_id` backstops
  idempotency before any send adapter exists.
- **7. Orchestrator is pure** — no orchestrator yet, but the same discipline
  binds the executor: policy `Checker` is a pure function of (call, config);
  executor core depends only on interfaces; unit tests run with no network and
  no Postgres. Every decision — allow or deny — writes a `policy_decisions`
  row and every call writes an `audit_events` row.

(5 and 6 have no code surface this step; their schema hooks —
`sent_external_id`, delivery matching — are noted above.)

## Sibling patterns to copy

- **Migrator**: `~/GolandProjects/job-agent/cmd/tools/migrate/main.go` — copy
  nearly verbatim (pgx v5, `schema_migrations` version table, `--dir` flag,
  `DATABASE_URL`, per-migration transaction, numbered-file regex). Don't invent
  a second migration idiom and don't add a framework (goose/atlas) — this tool
  is 160 lines and already proven.
- **DDL style**: `~/GolandProjects/job-agent/migrations/0001_initial.sql` —
  BIGSERIAL PKs, TEXT statuses, JSONB defaults, UNIQUE(source, external_id),
  header comment explaining scope.
- **Claim idiom (reference only, NOT this step)**:
  `~/GolandProjects/job-agent/internal/queue/queue.go` — `FOR UPDATE SKIP
  LOCKED`; the `tasks`/`task_claims` columns above must not preclude that
  pattern (status + timestamps present).
- **Deploy-later reference**: `~/GolandProjects/job-agent/Dockerfile.migrate`
  and `deploy/overlays/itza/migrate-job.yaml` — the eventual cluster migration
  job shape; out of scope now.

## Verification protocol

1. `docker compose up -d` (local Postgres) — or `make db-up`.
2. `export DATABASE_URL=postgres://…local…/ops` then
   `go run ./cmd/tools/migrate --dir migrations` — twice; first run applies,
   second logs skips only.
3. `psql "$DATABASE_URL" -c "\dt"` — all core-schema tables +
   `schema_migrations` present.
4. `go test ./...` — unit green with no DB running is a bonus check for the
   fakes; integration tests need the compose DB (env-gated).
5. Smoke: `psql` insert one `projects` row →
   `go run ./cmd/opsctl create-task --project <slug> --title "hello"` →
   `psql` shows the `tasks` row (status `ready`), one `audit_events` row
   (`started_at` set, status `ok`), one `policy_decisions` row (`allow`).
6. Negative smoke: `opsctl` with an unregistered tool name exits non-zero and
   the failure is audited (unit test 4e covers the pipeline; smoke confirms
   the CLI surfaces it).
7. Real apply (Q2): establish pg-main access (port-forward to
   `pg-main-rw.cnpg.svc:5432`), create the `ops` database, run the migrator
   against it, re-run smoke step 5 there; record the connection recipe in
   INSTITUTIONAL_KNOWLEDGE.md "Environment facts".
8. Salvador reviews and commits — no auto-commit.

## Decisions made unilaterally (rationale attached; flag in review if wrong)

- **Module name** `github.com/sspataro57/switchboard` — matches the existing
  git remote; sibling repos use the same account path.
- **TEXT + CHECK instead of PG enums** — forward-only migrations make
  `ALTER TYPE ... ADD VALUE` ergonomics a liability; job-agent precedent is
  TEXT statuses.
- **BIGSERIAL ids** — job-agent precedent; nothing here needs UUIDs and the
  system is single-writer per table.
- **Audit as one row per call, `started` → terminal update** — keeps call
  identity in one place and makes "handler ran without completing" visible as
  a stuck `started` row; two-event append-only adds join complexity with no
  consumer yet.
- **`create_task` default status `ready`** — `holding` is the triage parking
  lane (step 6); a human deliberately creating a task via CLI means it's ready
  to route.
- **Minimal columns on `normalized_*`** — the steps that first write them
  (2 and 7) will extend forward-only; over-specifying now guarantees churn.
- **Single `0001_initial.sql`** — the core schema is one coherent unit; there
  is no partial-apply state worth naming with separate numbers.
- **Audit `args` stored verbatim (no redaction layer yet)** — no step-1 tool
  carries secrets; add redaction with the first secret-bearing tool (step 7
  OAuth) and record it in INSTITUTIONAL_KNOWLEDGE.md.

## Future work (not this SPEC)

- `task_events` NOTIFY trigger + orchestrator LISTEN (step 5).
- pgcrypto encrypt/decrypt helpers for `refresh_token_encrypted` (step 7).
- `Dockerfile.migrate` + k8s migrate job when the first service deploys.

---

Open questions resolved 2026-07-11 (answers recorded in
`docs/tickets/01-schema-executor_OPEN_QUESTIONS.md`): Q1 — `embeddings` ships
in 0001 with pgvector (CNPG image already has it); Q2 — the real apply to the
`ops` db on pg-main is part of step-1 done. This SPEC is final.
