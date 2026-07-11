> Jira: SWT-2

# 02-upwork-crm-connector — Upwork CRM connector → raw → normalize

## Source

Build-order step 2, quoted from CLAUDE.md:

> 2. Upwork CRM connector (poll upwork_crm tables on pg-main OR extend its MQTT
>    topics) → raw → normalize.

Constrained by invariants 1 (raw-first), 2 (one funnel), 5 (own-message loop
closure, seam only this step), and the scope rule "established clients only —
unknown senders tagged prospect, stay CRM-side."

## Goal

Ship a one-shot Go connector binary that polls the live `upwork_crm` database
(clients + communications, read-only), lands every row in `raw_source_items`
first, then deterministically normalizes into `people` / `person_identities` /
`normalized_threads` / `normalized_messages` — idempotent, cursor-tracked,
re-runnable, no LLM anywhere.

**Usable alone means:** after one `go run ./cmd/connectors/upworkcrm` against
the real databases, the real `ops` db contains the full raw + normalized
history of every established-client Upwork conversation, queryable via psql,
re-derivable from raw alone, and safe to re-run on a cron cadence forever.
That corpus is exactly what steps 4 (task context) and 6 (triage) consume.

## Ground truth about the source (scouted live, 2026-07-11 — do not re-guess)

Database `upwork_crm` on pg-main, owner role `upwork_crm`. Relevant tables:

- `clients` (14 rows): `id uuid PK`, `name NOT NULL`, `email`, `company`,
  `upwork_profile_url`, `upwork_room_id` (partial unique), `lead_id` /
  `proposal_id` FKs, `ai_summary`, `created_at`, `updated_at`
  (trigger-maintained).
- `communications` (823 rows): `id uuid PK`, `client_id uuid NOT NULL FK
  clients (CASCADE)`, `contract_id uuid NULL`, `direction CHECK
  (inbound|outbound)`, `channel CHECK (upwork|email|slack|manual)`, `subject`,
  `body`, `communicated_at timestamptz NOT NULL` (indexed DESC), `created_at`,
  `sender`, `external_id` (`UNIQUE(client_id, external_id)`), `is_draft bool`,
  `instructions text`. **No `updated_at` column** — incremental pulls cannot
  see mutations of old rows (see Decisions).
- `leads` (12263), `proposals` (2266), `prospect_draft_revisions` (42): the
  prospect funnel. **Out of scope by CLAUDE.md policy** — prospects stay
  CRM-side.
- `contracts` (1), `milestones` (1): deferred (Future work).
- `sync_runs` / `sync_state` / `scraper_health_*`: the CRM's own bookkeeping —
  we never read or write these.

The `ops` role currently has **no rights** on the `upwork_crm` database; a
one-time superuser grant is part of this step's verification protocol.

## Acceptance criteria

1. `go build ./...` and `go test ./...` pass. Connector unit tests (hashing,
   cursor logic, normalize mapping, draft/prospect filtering) run with zero
   network / zero Postgres, against a fake source reader.
2. First real run: a `source_accounts` row (`provider='upwork_crm'`) exists
   (upserted by the connector), one `sync_runs` row ends `status='ok'` with
   stats (counts of clients/communications seen, raw inserted / updated /
   unchanged, normalized, suspected merges), and `raw_source_items` holds one
   row per source client and per non-draft communication, `raw_json` = the
   verbatim source row as JSON, `content_hash` set, external ids
   `clients:{uuid}` / `communications:{uuid}`.
3. Raw-first is observable: for every normalized row,
   `raw_source_items.ingested_at <= normalized_at`, and the write order in
   code is ingest phase fully before normalize phase (integration test asserts
   a raw row exists with `normalized_at IS NULL` mid-run via phase separation,
   or minimally: no normalized row references a nonexistent raw row —
   enforced by FKs — and `normalized_at` is set on exactly the raw rows that
   were normalized).
4. Normalization is complete and deterministic: every source client → one
   `people` row + `person_identities` (`upwork_crm:{client uuid}` always;
   `email:{email}` and `upwork_room:{room id}` when present); every ingested
   communication → one `normalized_messages` row (direction, sent_at =
   `communicated_at`, body_text, subject, sender, channel,
   external_message_id = source `external_id`) attached to a
   `normalized_threads` row keyed `upwork_crm:{client uuid}:{channel}` whose
   participants reference the client's person id.
5. Idempotency: an immediate second run inserts **zero** new rows anywhere
   (raw, normalized, people, identities, threads), updates nothing
   (`content_hash` match short-circuits), and its `sync_runs.stats` proves it
   (raw_inserted=0, raw_updated=0). Verified in integration test and in the
   real smoke.
6. Change handling: if a source row's content changes (integration test
   mutates a seeded communication, then runs with `--full`), the raw row is
   updated in place (new `raw_json` + `content_hash`, `normalized_at` reset to
   NULL) and re-normalization updates — not duplicates — the corresponding
   `normalized_messages` row.
7. Re-normalization from raw alone: `--normalize-only --all` with **no source
   DSN configured** (binary must not require `UPWORK_CRM_DATABASE_URL` in this
   mode) rebuilds/updates all normalized rows purely from `raw_source_items`.
   Integration test: truncate normalized tables, run normalize-only, assert
   identical row set.
8. Scope filters hold: no raw or normalized row derives from `leads`,
   `proposals`, `prospect_draft_revisions`, or `is_draft = true`
   communications. (Belt: query filters. Suspenders: the pg-main grant is
   SELECT on `clients` and `communications` only.)
9. Read-only source: the source DSN sets
   `default_transaction_read_only=on`; no statement other than SELECT is
   issued against the source pool (reviewable by grep; the grant enforces it
   mechanically).
10. Zero tasks: `SELECT count(*) FROM tasks` is unchanged by any run (no
    triage, no task creation — that is step 6).
11. Failure bookkeeping: a run that errors mid-way leaves `sync_runs` with
    `status='error'` + error text, does **not** advance the cursor, and exits
    non-zero. A subsequent run recovers (re-pulls from the old cursor).
12. Migration `0002` applies cleanly on top of 0001 (twice — second run
    skips), locally and on the real `ops` db.

## Data model changes

Migration: `migrations/0002_upwork_crm_connector.sql` (forward-only; 0001 is
never edited). Same DDL conventions as 0001 (TEXT + CHECK, JSONB defaults).

- `ALTER TABLE source_accounts ADD COLUMN sync_cursor JSONB NOT NULL DEFAULT
  '{}'` — per-account sync position (this step: communications cursor; step 7
  reuses it for Gmail historyId / Calendar syncToken).
- `ALTER TABLE normalized_messages ADD COLUMN subject TEXT, ADD COLUMN sender
  TEXT, ADD COLUMN channel TEXT` — carried from `communications`; step 1
  deliberately left `normalized_*` minimal for the first writer (this step) to
  extend.
- `CREATE UNIQUE INDEX normalized_messages_raw_item_idx ON
  normalized_messages (raw_source_item_id)` — one message per raw item; the
  upsert target that makes normalization idempotent.
- `CREATE UNIQUE INDEX normalized_threads_thread_key_idx ON normalized_threads
  (thread_key) WHERE thread_key IS NOT NULL` — thread upsert target.

No new tables. Rows written at runtime (not by migration):

- `source_accounts`: connector upserts `(provider='upwork_crm',
  account_email='upwork_crm@pg-main')` on startup (`ON CONFLICT DO NOTHING`),
  `send_enabled=false`. Synthetic account_email — see Decisions.
- `raw_source_items.external_id` convention: `{source_table}:{uuid}`, i.e.
  `clients:3f2a…`, `communications:9b1c…`. Table-qualified so one account can
  carry multiple source tables under the existing
  `UNIQUE (source_account_id, external_id)`.
- `content_hash`: sha256 hex over the row's canonical JSON. Canonical =
  Postgres `jsonb` text output of the full source row (jsonb key order is
  deterministic), hashed in Go before comparing to the stored hash.

## API / MCP tool changes

**None.** This step registers no executor tools and adds no MCP surface.
Where it stands relative to invariant 3: the connector is ingestion spine, not
an agent-facing action — it is a trusted service writing the ingestion tables
directly, exactly as invariant 1 describes connectors doing. Its audit trail
is `sync_runs` (one row per run, stats + error), not per-item `audit_events`.
No raw_sql/raw_api capability is exposed to any agent by this step; the
connector binary is not reachable from any tool registry.

`internal/store` grows a `NewPoolDSN(ctx, dsn)` (or equivalent) so the
connector can hold two pools: sink from `DATABASE_URL` (existing `NewPool`),
source from `UPWORK_CRM_DATABASE_URL`.

## MQTT topics

None. The CRM's own MQTT topics are not consumed (see Decisions — polling
chosen); switchboard's MQTT contract starts at step 3.

## Files likely to touch

Existing (verified in repo):
- `internal/store/pg.go` — add the DSN-parameterized pool constructor
  alongside `NewPool`.
- `Makefile` — optional `crm-sync` convenience target; `integration` target
  already covers the new build-tagged tests.
- `migrations/` — new `0002_upwork_crm_connector.sql` next to
  `0001_initial.sql`.
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` — record the upwork_crm grant recipe +
  `UPWORK_CRM_DATABASE_URL` under "Environment facts".

New:
- `cmd/connectors/upworkcrm/main.go` — flag parsing (`--full`,
  `--normalize-only`, `--all`, `--overlap`), env wiring, run-once entrypoint.
- `internal/connector/upworkcrm/source.go` — `SourceReader` interface
  (ListClients, ListCommunications(since cursor)) + pg implementation + fake
  for unit tests.
- `internal/connector/upworkcrm/ingest.go` — source_account upsert, sync_runs
  bookkeeping, raw upsert (hash compare, `normalized_at` reset on change),
  cursor read/advance.
- `internal/connector/upworkcrm/normalize.go` — deterministic raw →
  people/identities/threads/messages mapping; `normalized_at` stamping;
  suspected-merge skip counting.
- `internal/connector/upworkcrm/hash.go` — canonical-JSON sha256.
- `internal/connector/upworkcrm/{ingest,normalize,hash}_test.go` (unit, fakes)
  and `integration_test.go` (build tag `integration`, env-gated like
  `internal/executor/integration_test.go`).

## In scope

- Migration 0002 as specified.
- One-shot connector binary: ingest phase (clients full-scan — 14 rows;
  communications incremental by cursor with overlap; `--full` rescans
  everything) then normalize phase (raw rows with `normalized_at IS NULL`;
  `--all` reprocesses every raw row).
- Cursor persistence in `source_accounts.sync_cursor`
  (shape: `{"communications_created_at": "<ts>"}`), advanced only on
  successful runs; `--overlap` (default 24h) re-reads a trailing window,
  absorbed by idempotent upserts.
- Identity resolution writes with **no auto-merge**: person is found/created
  via the `(provider='upwork_crm', value=client_uuid)` identity; secondary
  identities (`email`, `upwork_room`) inserted `ON CONFLICT DO NOTHING`; a
  conflict where the identity already belongs to a *different* person is
  counted in stats as a suspected merge and skipped (dashboard approval
  machinery is a later step).
- Integration test infra: the test creates and seeds a simulated
  `clients`/`communications` pair (second database or dedicated schema on the
  compose Postgres — compose `ops` user is superuser locally), rerunnable
  against a persistent db (clean own leftovers first, FK order — see
  INSTITUTIONAL_KNOWLEDGE "Test infrastructure" landmine).
- One-time pg-main grant (superuser) + real end-to-end run, both in the
  verification protocol.

## Out of scope (do not bundle)

- **Step 3**: MQTT heartbeats/fleet view — the connector publishes nothing.
- **Step 4**: MCP task tools, claims, worker wrapper.
- **Step 5**: orchestrator, scheduling. The binary is one-shot; cron/k8s
  CronJob packaging lands when the first service deploys.
- **Step 6**: triage, task creation, `find_related_tasks`, ai_extractions —
  this connector creates **zero** `tasks` rows.
- **Step 8**: deliveries, delivery-row matching of outbound messages, Upwork
  assisted tier. This step only preserves the match keys (see invariant 5).
- Prospect funnel ingestion (`leads`, `proposals`,
  `prospect_draft_revisions`) — CRM-side by policy.
- `contracts` / `milestones` ingestion (1 row each today; Future work).
- Consuming or extending the CRM's MQTT topics.
- Any write to the `upwork_crm` database, including its `sync_state`.
- Embeddings/content_chunks backfill for the ingested corpus.

## Invariants that apply

- **1. Raw-first** — the load-bearing invariant of this step. The
  `raw_source_items` write happens in `internal/connector/upworkcrm/ingest.go`
  and completes for the whole batch **before** the normalize phase starts;
  normalization reads only `raw_source_items.raw_json` (never the source db),
  which is what makes criterion 7 (re-normalize from raw alone) possible.
  Content change ⇒ raw updated + `normalized_at` reset ⇒ reprocessing is
  always possible.
- **2. One funnel** — no new tables at all, let alone task-like ones. Source
  rows normalize into the existing canonical objects
  (Message/Thread/Person). Nothing actionable is minted here; that stays for
  triage (step 6) writing into the ONE `tasks` table.
- **3. Everything through the executor** — no tool is added; the connector
  exposes no agent-callable surface and registers nothing on the executor.
  Review check: no import of `internal/executor` from connector code, no new
  `Register` calls, no raw_sql-style capability.
- **5. Own-message loop closure** — seam only. `direction='outbound'`
  communications (the CRM scraper's confirmations of our sends) are ingested
  and normalized like any message, with `external_message_id` preserved
  verbatim from `communications.external_id`. Step 8's normalizer hook will
  match that id against `deliveries.sent_external_id`. This step's obligations:
  (a) never drop or rewrite the external id, (b) never create tasks from any
  message (criterion 10), so our own sends cannot be re-triaged.
- **7. Orchestrator purity (discipline transfer)** — no orchestrator here, but
  the same rule binds: normalize is a pure function of the raw row (unit-
  testable, zero network, no LLM — CLAUDE.md pins this normalizer as
  deterministic Go). The pg-facing edges live behind the `SourceReader`
  interface + sink store so `go test ./...` stays offline.

(4 and 6 have no surface: nothing is sent externally, nothing client-visible
is authored.)

## Sibling patterns to copy

- **Migration style**: `migrations/0001_initial.sql` in this repo and
  `~/GolandProjects/job-agent/migrations/` — numbered forward-only files, TEXT
  + CHECK, header comment naming the SPEC. The migrator
  (`cmd/tools/migrate/main.go`) already handles 0002 with zero changes.
- **Env-gated integration tests**: `internal/executor/integration_test.go` in
  this repo — build tag `integration`, skip when `DATABASE_URL` unset,
  rerunnable cleanup. Copy its shape.
- **Pg store style**: `internal/audit/pg.go` — small struct over `*pgxpool.Pool`,
  wrapped errors, context-first. The ingest/normalize stores follow it.
- **UNIQUE(source, external_id) upsert idiom**:
  `~/GolandProjects/job-agent/migrations/0001_initial.sql` precedent already
  cited in the step-1 SPEC; `raw_source_items` upserts use
  `ON CONFLICT (source_account_id, external_id)`.
- **Conceptual precedent**: the source db's own `sync_runs`/`sync_state`
  tables show the CRM solved the same bookkeeping problem the same way —
  reassurance, not code to copy.

## Verification protocol

1. `go test ./...` — unit tests green offline (no compose db running).
2. `make integration` — migrates 0001+0002 onto the compose Postgres, then the
   connector integration test: seed simulated source → run → assert raw +
   normalized rows (criteria 2–4) → run again → no-op (criterion 5) → mutate a
   source row → `--full` → raw updated, normalized updated not duplicated
   (criterion 6) → truncate normalized → `--normalize-only --all` without a
   source DSN → identical rows (criterion 7).
3. One-time grant on pg-main (superuser creds: k8s secret
   `cnpg/pg-main-superuser`), then record it in INSTITUTIONAL_KNOWLEDGE.md:
   ```sql
   -- connected to db upwork_crm as postgres
   GRANT CONNECT ON DATABASE upwork_crm TO ops;
   GRANT USAGE ON SCHEMA public TO ops;
   GRANT SELECT ON clients, communications TO ops;
   ```
4. Apply 0002 to the real `ops` db:
   `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/tools/migrate --dir migrations`
   (twice; second run all-skips).
5. Real smoke ("usable alone"):
   `DATABASE_URL="$OPS_DATABASE_URL"
   UPWORK_CRM_DATABASE_URL='postgres://ops:…@192.168.50.49:5432/upwork_crm?options=-c%20default_transaction_read_only%3Don'
   go run ./cmd/connectors/upworkcrm` — then psql against ops:
   - `SELECT status, stats FROM sync_runs ORDER BY id DESC LIMIT 1;` → ok,
     plausible counts (~14 clients, ≤823 communications after draft filter).
   - `SELECT count(*) FROM raw_source_items WHERE normalized_at IS NULL;` → 0.
   - Spot-check one thread: pick a known client, read its
     `normalized_messages` ordered by `sent_at`, compare against the CRM.
   - `SELECT count(*) FROM tasks;` unchanged.
6. Run again immediately → new `sync_runs` row with raw_inserted=0,
   raw_updated=0.
7. `psql -h 192.168.50.49 -U ops -d upwork_crm -c 'DELETE FROM clients'`
   → must FAIL with permission denied (read-only proof; nothing is deleted).
8. Commit via the `/ticket-deliver` flow after review.

## Decisions made unilaterally (rationale attached; flag in review if wrong)

- **Poll the tables; ignore the CRM's MQTT topics.** Polling is deterministic,
  supports the required backfill of 823 historical communications (MQTT has no
  replay), needs no topic catalog (none is documented), and gives natural
  crash recovery via cursor + idempotent upserts. MQTT would only buy latency
  we don't need at a 5–15 min cadence, at the cost of a second contract with
  the CRM. The CRM's topics remain its own concern.
- **Access via direct grant to the `ops` role (option a), two DSNs in the
  connector.** Simplest thing that is mechanically read-only and
  least-privilege (SELECT on exactly `clients` + `communications` — which also
  hard-enforces the prospects-stay-CRM-side rule at the grant level).
  postgres_fdw hides the second connection behind schema magic for no benefit;
  a dedicated reader role adds credential sprawl for a same-cluster,
  same-owner setup. Revisit only if upwork_crm ever leaves pg-main.
- **One-shot binary, scheduling external.** No daemon loop this step: run
  manually / via Makefile now, k8s CronJob when deploy packaging lands. Keeps
  the step small and testable; the cursor makes cadence irrelevant to
  correctness.
- **Skip `is_draft = true` communications at query time.** Drafts are CRM
  working state, not communications that happened. When the CRM marks one
  sent, it enters the next pull as a normal outbound row. No raw ingestion of
  drafts — "raw-first" governs what we capture, not an obligation to capture
  the source's scratch space.
- **Cursor on `communications.created_at` (insert time) with a 24h overlap,
  not `communicated_at`.** `communicated_at` is historical message time — the
  scraper can insert old messages late, which a `communicated_at` cursor would
  silently skip. `created_at` is monotone-enough with the overlap window;
  idempotent upserts make the overlap free. Because the table has no
  `updated_at`, mutations of old rows are invisible to incremental pulls —
  `--full` exists for that, is O(823 rows) ≈ free, and can simply be the
  scheduled mode until volume forces true incrementality. Clients (14 rows)
  are always full-scanned; their `updated_at` is not needed as a cursor.
- **Raw change = update in place + reset `normalized_at`.** The
  `UNIQUE (source_account_id, external_id)` constraint from 0001 pins one raw
  row per source row; raw history-keeping would need a schema fork for a
  source that barely mutates. The source itself retains everything — replay is
  always possible by re-polling.
- **Thread granularity: one per `(client, channel)`**, key
  `upwork_crm:{client uuid}:{channel}`. `communications` has no finer thread
  id; folding channels together would erase a distinction step 8's delivery
  routing needs (upwork_chat vs gmail are different delivery channels).
- **Synthetic `account_email = 'upwork_crm@pg-main'`.** `source_accounts`
  requires NOT NULL + UNIQUE(provider, account_email); the CRM is a system
  source, not a mailbox. A visibly-synthetic value beats overloading
  Salvador's real address, which step 7 will register for Gmail.
- **Connector-side upsert of the `source_accounts` row, not a seed
  migration.** Migrations stay pure DDL; the upsert is idempotent and keeps
  the account definition next to the code that owns it.
- **Two `sync_runs` rows per invocation (one per phase), not one combined.**
  Ingest and Normalize each open/close their own run: phase-level audit and
  clean failure isolation (criterion 11) beat a single combined row. Criterion
  2's "one sync_runs row ends ok" is satisfied by the ingest-phase row.
  (Accepted at review, 2026-07-11.)
- **`sync_runs` (not per-item `audit_events`) is the ingestion audit.**
  `audit_events` is the executor's tool-call trail; 800+ rows per poll would
  drown it. Per-run stats + the raw table itself (ingested_at, content_hash)
  give full traceability.

## Future work (not this SPEC)

- `contracts` / `milestones` ingestion when they carry signal (1 row each
  today).
- Suspected-merge queue surfaced on the dashboard (stats counter exists now).
- Delivery-row matching for outbound rows (step 8) — hooks: `direction`,
  `external_message_id`.
- Triage over the normalized corpus (step 6, shadow mode first).
- CronJob packaging + Dockerfile when the first long-running service deploys.
- Embeddings backfill over `normalized_messages`.
