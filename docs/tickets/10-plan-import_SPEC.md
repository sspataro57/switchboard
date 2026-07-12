> Jira: SWT-10

# 10-plan-import — Plan import (.md → approved task tree) + dashboard full board, briefs, exports

## Source

Build-order step 10, CLAUDE.md:

> 10. Plan import (one-way: .md → task tree via LLM-assisted parse + dashboard
>     approval; file replaced by stub; new discovered work =
>     create_child_task, never plan-file edits). Dashboard full board, briefs,
>     exports.

## Goal

Ship the one-way plan funnel — a `.md` plan file is captured raw-first, parsed
by a GPT provider call into a strictly-schema'd proposed task tree
(ai_runs/ai_extractions, triage idiom), approved or rejected in the dashboard,
and only on approval materialized through the executor as `tasks` rows with
parent ordering + dependencies, after which the source file is replaced by a
stub — plus the dashboard full board (queues = filters on the one tasks table),
brief surfacing, and deterministic CSV/JSON exports.

**Usable alone means:** today, on the workstation — Salvador writes a small
real plan .md for the switchboard project, runs `planimport propose`, reviews
the proposed tree at `/plans` in the dashboard (dev login), approves it, runs
`planimport apply`, and the tree appears as real ready/blocked tasks (R4 blocks
dependents after the orchestrator drain) on the new `/tasks` board, the file on
disk is now a stub pointing at the board, morning-brief tasks are browsable at
`/briefs`, and `/export/tasks.csv` downloads the board. No LLM call happens
anywhere in the executor path; nothing becomes a task without the dashboard
approval.

## Acceptance criteria

1. Migration `migrations/0008_plan_import.sql` applies on fresh and current
   schemas: one new table `plan_imports` (DDL below) + its indexes. No other
   DDL. Verify: `make db-up && make migrate` twice (idempotent runner),
   `psql ... -c '\d plan_imports'`.
2. **Raw-first capture (invariant 1):** `planimport propose --project <slug>
   --file <path>` first ensures the synthetic source account
   (`provider='plan'`, `account_email='plans@local'` — hooksd
   `github@webhooks` idiom) and writes ONE `raw_source_items` row
   (`external_id=plan:{project_slug}:{content_hash}`,
   `raw_json={"path":..., "content":<full file text>}`) BEFORE any LLM call.
   Re-proposing identical content is refused by the partial unique index on
   `plan_imports` (criterion 5), and the raw upsert is idempotent on
   (account, external_id). Verify: integration test asserts raw row exists
   with the full content even when the provider call is made to fail.
3. **LLM-assisted parse (triage idiom):** the propose command calls
   `provider.Client.Complete` (OpenAI adapter, `PLAN_MODEL` default
   `gpt-5-mini`, strict json_schema `plan_tree`) with the file content; the
   schema is pinned in "API changes" below. Every call records an `ai_runs`
   row (`worker_type='plan_import'`, provider/model/tokens/latency, input =
   prompt version + raw_source_item_id + rendered prompt) and, on success, one
   `ai_extractions` row (`raw_source_item_id` = the plan's raw item, `fields`
   = the validated tree document). Provider errors record an error ai_run and
   exit non-zero. Tests use a fake `provider.Client` — never a live LLM.
4. **Deterministic validation (agency at the leaves, determinism at the
   spine):** Go code — not the model — validates and normalizes the tree
   before anything is proposed: refs unique and non-empty, `parent_ref`
   resolves, no parent cycles, `depends_on_refs` resolve with no dependency
   cycles (topological check), `assignee_type ∈ {human,claude}`, titles
   non-empty, ≤ 200 tasks. `plan_order` is assigned by Go as the 1-based
   sibling array position — never model-chosen. Clamps/corrections are
   recorded in `fields.validation` (triage `buildFields` idiom); hard-invalid
   trees (cycle, unresolved ref) record the error ai_run, create NO
   plan_imports row, and exit non-zero with the reasons printed. Unit tests
   table-drive all of the above.
5. **Proposal row through the executor:** propose finishes with an executor
   call `propose_plan_import` `{project, source_path, content_hash,
   raw_source_item_id, ai_run_id, ai_extraction_id}` (actor
   `planimport:{os user}`) inserting a `plan_imports` row `status='proposed'`.
   The handler re-runs the criterion-4 validation on the stored extraction
   (defense in depth) and enforces the one-pending-proposal guard: partial
   unique index on `(project_id, content_hash) WHERE status <> 'rejected'`.
   A stub file (recognized by the pinned marker comment, criterion 9) is
   refused at the CLI before any write. Nothing in the propose path touches
   `tasks`/`task_events` — pinned by an integration assertion (shadow-lane
   discipline: AI proposed, nothing disposed yet).
6. **Dashboard approval:** `GET /plans` lists plan_imports (status filter);
   `GET /plans/{id}` renders the proposed tree (indentation from parent refs,
   plan_order, per-task assignee/priority/depends_on, confidence + validation
   notes). `POST /plans/{id}/approve` / `POST /plans/{id}/reject` go through
   the executor (`approve_plan_import` / `reject_plan_import`, actor
   `dashboard:{user}`): approve flips proposed→approved and inserts an
   `approvals` row (`subject_type='plan_import'`, decided_by = actor —
   approve_delivery idiom); reject flips proposed→rejected + approvals row.
   Both are humanOnly in the policy matrix (deny for a `worker:` actor —
   matrix unit test). Already-at-target is an idempotent no-op success;
   approve-after-reject (and vice versa) is an error.
7. **Apply creates the tree through the executor (nothing before approval):**
   `apply_plan_import` `{plan_import_id}` (humanOnly) requires
   `status='approved'`, reads the tree from `ai_extractions.fields`, and in
   ONE transaction inserts the tasks in topological (parents-first) order —
   `status='ready'`, project from the plan_imports row, `parent_id` resolved
   from refs, `plan_order` = sibling index, priority/subproject/worker_type/
   assignee_type from the tree — writes `task_events`: `child_created` on each
   parent (create_child_task idiom), `dependency_added` per task_dependencies
   row (so orchestrator R4 blocks dependents on the next drain — no new rule,
   no orchestrator change), and `plan_imported` `{plan_import_id}` on each
   root; then sets `status='applied'`, `applied_at`, and
   `result={"tasks":{ref:task_id,...}}`. Calling it on an already-applied
   plan is an idempotent no-op success returning the stored result (replay
   discipline). Integration test walks propose(fake provider)→approve→apply→
   orchestrator drain and asserts statuses (independent roots ready,
   dependents blocked), plan_order values, parent_ids, and that a second
   apply creates zero new rows.
8. **Apply before approval is impossible:** `apply_plan_import` on a
   `proposed` or `rejected` plan errors; the deny/error is visible in
   `audit_events`. Integration test pins it.
9. **Stub replacement (one-way import):** `planimport apply --id N` performs
   the executor apply and then — only after success — verifies the file at
   `source_path` still hashes to `content_hash` and overwrites it with the
   pinned stub:
   ```
   <!-- switchboard:imported plan_import={id} project={slug} date={YYYY-MM-DD} -->
   # Imported into switchboard

   This plan is tracked on the switchboard board (project "{slug}") — the
   board is the source of truth now. Do NOT add work here: new discovered
   work = create_child_task.
   ```
   If the file changed since propose (hash mismatch) or is missing, the tasks
   stand but the stub write is SKIPPED with a printed warning — never
   overwrite content that was never reviewed. `planimport propose` refuses
   files whose first line carries the marker. Unit tests: stub write, hash
   mismatch skip, stub detection.
10. **Full board:** `GET /tasks` renders every task as status-grouped columns
    over the ONE tasks table with filters as query params — `project` (slug
    dropdown), `status`, `assignee_type`, `subproject` — queues are filters,
    not tables. Default view hides `closed`; `?status=closed` shows them.
    Rows show id, title, project/subproject, assignee_type, priority,
    plan_order, parent id, updated_at, and link to `GET /tasks/{id}`: a
    read-only detail page (title/body/status/autonomy/worker_type, parent +
    children, dependencies with their statuses, last 50 task_events, open
    feedback_requests, deliveries, external_refs). Reads are direct SQL
    (dashboard idiom); there are NO mutation actions on the board this step.
11. **Briefs:** `GET /briefs` lists tasks titled `Morning brief %` (newest
    first — the same title predicate R7 itself dedups on), rendering the body
    preformatted. An R7-created brief task also appears on `/tasks` like any
    task (no special casing in the board query).
12. **Exports (deterministic, no LLM):** `GET /export/tasks.csv` and
    `GET /export/tasks.json`, session-gated like every dashboard route,
    honoring the same filters as `/tasks`, ordered by `id ASC`, pinned CSV
    header `id,project,subproject,parent_id,title,status,assignee_type,
    worker_type,priority,plan_order,created_at,updated_at` (JSON: array of
    objects, same fields). Golden-file unit test over a seeded fixture.
13. `go test ./...` green; `make integration` green — new suites join the
    mutual-cleanup pact (serialized `-p 1`, FK-ordered cleanup — task_events/
    task_dependencies/tasks children-first — test-owned slugs/actors).

## Data model changes

Migration `migrations/0008_plan_import.sql` (forward-only):

```sql
-- Plan-import proposal bookkeeping (NOT a task store — invariant 2: the
-- approved tree materializes as rows in tasks; this table only gates it).
CREATE TABLE plan_imports (
  id                 BIGSERIAL PRIMARY KEY,
  project_id         BIGINT NOT NULL REFERENCES projects(id),
  source_path        TEXT NOT NULL,
  content_hash       TEXT NOT NULL,
  raw_source_item_id BIGINT NOT NULL REFERENCES raw_source_items(id),
  ai_run_id          BIGINT NOT NULL REFERENCES ai_runs(id),
  ai_extraction_id   BIGINT NOT NULL REFERENCES ai_extractions(id),
  status             TEXT NOT NULL DEFAULT 'proposed'
                     CHECK (status IN ('proposed','approved','rejected','applied')),
  result             JSONB,          -- {"tasks":{ref:task_id}} once applied
  decided_by         TEXT,
  decided_at         TIMESTAMPTZ,
  applied_at         TIMESTAMPTZ,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX plan_imports_status_idx ON plan_imports (status);
-- One live proposal per exact plan content per project; re-import allowed
-- only after rejection (an applied plan's file is a stub — new hash anyway).
CREATE UNIQUE INDEX plan_imports_pending_uniq
  ON plan_imports (project_id, content_hash) WHERE status <> 'rejected';
```

Notes:
- The proposed tree is NOT duplicated here — approval and apply both read the
  immutable `ai_extractions.fields` via `ai_extraction_id` (what the human saw
  is exactly what gets applied).
- No tasks DDL: `parent_id`, `plan_order`, `priority`, `subproject`,
  `worker_type` (0001) already carry everything the tree needs.
- 0004's `ai_runs_worker_type_idx` comment explicitly anticipated this step:
  plan extractions carry `worker_type='plan_import'` and are invisible to the
  triage pending filter (which joins on `worker_type='triage'`).
- The plan's raw item gets `normalized_at` stamped when the extraction is
  recorded — plans never become normalized_messages, and a permanently-NULL
  normalized_at would read as "ingested, never processed" in raw-lag queries.
- New task_events vocabulary: `plan_imported` (payload `{plan_import_id}`, on
  root tasks at apply). `child_created` / `dependency_added` reused as-is.
- `approvals.subject_type` gains value `'plan_import'` (TEXT, no CHECK — no DDL).

## API / MCP tool changes

All four new tools register in `internal/tools.Register` (invariant 3),
**spine-facing — none is MCP-listed** (agents never import plans; their verb
for new work stays `create_child_task`, already agent-facing). No
`internal/mcpserver` change.

- `propose_plan_import` — `{project, source_path, content_hash,
  raw_source_item_id, ai_run_id, ai_extraction_id}` → `{plan_import_id}`.
  Re-validates the stored tree; enforces the pending-uniqueness guard.
- `approve_plan_import` — `{plan_import_id}` → `{status}`. proposed→approved +
  approvals row. Idempotent on approved; error from rejected/applied.
- `reject_plan_import` — `{plan_import_id, reason?}` → `{status}`.
  proposed→rejected + approvals row (rejected). Idempotent on rejected.
- `apply_plan_import` — `{plan_import_id}` → `{result}` (ref→task_id map).
  Approved-only; single-tx tree insert per criterion 7; idempotent no-op with
  stored result when already applied.

**Policy** (`internal/policy/matrix.go`): `approve_plan_import`,
`reject_plan_import`, `apply_plan_import` join the `humanOnly` map
(dashboard:/opsctl:/manual: actors). None is send-shaped — no snapshot loader
involvement, no kill-switch/rate-limit interaction (nothing leaves the
machine). `propose_plan_import` falls through to the static allow-list.

**Provider JSON schema** (`plan_tree`, strict — internal/planimport/prompt.go):

```json
{"summary": "string",
 "tasks": [{"ref": "string", "parent_ref": "string|null",
            "title": "string", "body": "string",
            "assignee_type": "human|claude",
            "subproject": "string|null", "worker_type": "string|null",
            "priority": "integer",
            "depends_on_refs": ["string"],
            "confidence": "number", "notes": "string"}]}
```

`plan_order` is deliberately absent — array position is authoritative (Go
assigns it). The system prompt forbids inventing work not in the file and
keeps titles/bodies in Salvador's terse register with no AI references.

**Dashboard routes** (`internal/dashboard/server.go`, all session-gated;
reads direct SQL, actions through the executor):

- `GET /tasks`, `GET /tasks/{id}` — board + detail (criterion 10)
- `GET /briefs` — criterion 11
- `GET /plans`, `GET /plans/{id}`, `POST /plans/{id}/approve`,
  `POST /plans/{id}/reject` — criterion 6
- `GET /export/tasks.csv`, `GET /export/tasks.json` — criterion 12
- `GET /` redirect target changes from `/deliveries` to `/tasks`; shared nav
  across templates.

**CLI** (`cmd/planimport`): `propose --project <slug> --file <path>`,
`apply --id N`, `list [--status s]`. Env: `DATABASE_URL`, `OPENAI_API_KEY`
(propose only), `PLAN_MODEL` (default `gpt-5-mini`). Executor actor
`planimport:{os user}` for propose/list, `manual:{os user}` for apply (passes
humanOnly — the CLI IS Salvador at a keyboard, same trust as opsctl).

## MQTT topics

None. planimport is a one-shot CLI; the dashboard is HTTP. Heartbeat/command
topics untouched.

## Files likely to touch

- `migrations/0008_plan_import.sql` — new
- `internal/planimport/{planimport.go,prompt.go,validate.go,store.go,stub.go}`
  (+ unit tests with a fake provider.Client, + integration test) — new;
  triage package layout copied
- `cmd/planimport/main.go` — new (cmd/drafts wiring shape)
- `internal/tools/planimport.go` — new (the four tools); register them in
  `internal/tools/createtask.go`'s Register list
- `internal/policy/matrix.go` + `matrix_test.go` — humanOnly additions
- `internal/dashboard/server.go` — board/briefs/plans/export handlers
- `internal/dashboard/templates/{tasks.html,task.html,briefs.html,plans.html,plan.html}`
  — new; `deliveries.html` — shared nav
- `internal/tools/lifecycle_integration_test.go` sibling: new
  `internal/tools/planimport_integration_test.go` (cleanup pact)
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` — `plan_imported` event vocabulary,
  planimport CLI env, stub marker format, dashboard route map

## In scope / Out of scope

**In scope:** everything above — 0008, planimport package + CLI, the four
executor tools, matrix humanOnly additions, dashboard plans review +
full board + task detail + briefs + exports, stub replacement, tests.

**Out of scope (do not bundle):**
- **Two-way plan sync / re-import of a partially-done plan** — the import is
  one-way by design; post-import work arrives via create_child_task only.
- **Dashboard file upload or in-dashboard tree editing** — the review loop for
  a wrong parse is reject → edit the .md → re-propose. Editing a proposed
  tree in the browser is a later slice if the reject loop proves painful.
- **Board mutation actions** (close/claim/reprioritize/answer-feedback from
  the UI) — the board is read-only this step; opsctl covers spine verbs.
- **Triage go-live / find_related_tasks wiring for plans** — plan import
  never attaches to existing tasks; it only creates.
- **Dashboard k8s/nginx/Keycloak packaging** — still workstation + dev login;
  deploy is operator work (Auth comment "step 10 revisits" is satisfied by
  OIDC already existing; hardening ships with deploy, not here).
- **Autonomy promotion machinery, delivery `kind`** — untouched (step-8/9
  future work).
- **Jira mirroring of imported tasks** — the product's tasks never sync to
  the personal SWT board (vocabulary split).

## Invariants that apply

- **1 — raw-first:** the plan file's full content lands in `raw_source_items`
  (synthetic `plan` account, content_hash) before the provider call; the
  extraction links to it via `ai_extractions.raw_source_item_id`. Reparse from
  raw alone stays possible forever — including after the file becomes a stub.
- **2 — one funnel:** the approved tree becomes rows in the ONE `tasks` table
  (parent_id/plan_order/task_dependencies); `plan_imports` is proposal
  bookkeeping in the ai_runs/sync_runs family — nothing consumes it as a work
  queue, and no board query reads anything but `tasks`. Queues on the board
  are filters (query params), not tables.
- **3 — everything through the executor:** propose/approve/reject/apply are
  registered tools with unexported handlers; the dashboard POSTs and the CLI
  both go through `Executor.Execute` (validate → policy → audit → handler).
  The board and exports are reads (dashboard idiom). No raw_sql/raw_api; none
  of the new tools is MCP-listed.
- **4 — nothing external without a delivery row:** nothing in this step sends
  anything anywhere — exports are session-gated HTTP responses, the stub write
  is a local file. No delivery path is added or touched.
- **5 — own-message loop closure:** N/A — no sends, no re-ingest. (The stub
  marker refusing re-propose is the analogous "don't re-triage your own
  output" guard for files.)
- **6 — stealth attribution:** imported task titles/bodies are internal, but
  the parse prompt still pins the terse register and forbids AI references —
  task bodies leak into client-visible drafts downstream. The stub file
  mentions switchboard, not any AI.
- **7 — orchestrator purity:** zero orchestrator changes. Apply emits the
  existing `dependency_added` events and R4 (pure, already tested) blocks
  dependents on the drain. The ONLY LLM call sits in cmd/planimport before any
  executor call; no executor handler, rule, or dashboard handler touches a
  provider adapter.

## Sibling patterns to copy

- **LLM worker shape:** `internal/triage/{triage.go,prompt.go,store.go}` —
  Store with RecordRun/RecordExtraction, strict-schema provider call,
  validation/clamp with `fields.validation`, fake-client tests
  (`worker_test.go`); `cmd/drafts/main.go` for the cmd wiring
  (executor + provider + env model default).
- **Synthetic source account + raw upsert:** hooksd's `github@webhooks`
  account in `internal/connector/github/receiver.go`; upworkcrm
  `sink.go` `upsertRaw` hash decision.
- **Approve verb:** `internal/tools/delivery.go` `approveDelivery` —
  status flip + approvals insert in one tx, humanOnly gating in
  `internal/policy/matrix.go`.
- **Tree insert SQL + events:** `internal/tools/childtask.go`
  (`child_created`, subproject inheritance) and
  `internal/tools/dependency.go` (`dependency_added`, dep predicate);
  `internal/tools/close.go` for idempotent no-op success shape.
- **Ordering semantics:** `internal/tools/getnext.go` (priority DESC,
  plan_order ASC NULLS LAST) + `getnext_ordering_integration_test.go` — the
  board should display siblings in the same order get_next would serve them.
- **Dashboard:** `internal/dashboard/server.go` + `templates/deliveries.html`
  — handler/template structure, `s.execute` executor bridge, dev login;
  rag-scv (`~/GolandProjects/rag-scv`) for any HTMX niceties.
- **Integration hygiene:** `internal/tools/delivery_lifecycle_integration_test.go`
  — FK-ordered cleanup, test-owned actors, `-p 1` pact.

## Verification protocol

1. `go test ./...` — tree validation table tests (cycles, unresolved refs,
   sibling plan_order assignment, 200-task cap, clamps recorded), stub
   write/detect/hash-mismatch, matrix humanOnly for the three gated tools,
   export golden files, planimport run against a fake provider (ai_runs/
   ai_extractions bookkeeping, error paths).
2. `make integration` — full walk: propose (fake provider) → raw+run+extraction
   +plan_imports rows, zero tasks; approve via executor (approvals row);
   apply → tree with parents/plan_order/deps; orchestrator drain → dependents
   blocked; re-apply no-op; apply-before-approve error; reject path; duplicate
   propose refused; dashboard handlers over httptest (plans list/detail,
   approve POST, board filters, briefs, CSV/JSON exports).
3. Manual smoke (real, today — the "usable alone" check):
   - `psql -h 192.168.50.49 -U ops -d ops -c "INSERT INTO projects (name,slug)
     VALUES ('switchboard','switchboard') ON CONFLICT (slug) DO NOTHING;"`
   - Write `~/plans/switchboard-followups.md` — a real 6–10 item plan (e.g.
     the Future work list below) with one nested subsection and one "after X"
     dependency phrase.
   - `eval "$(grep '^export OPENAI_API_KEY=' ~/.bashrc)"`;
     `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/planimport propose
     --project switchboard --file ~/plans/switchboard-followups.md` — prints
     the id + tree preview.
   - `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/dashboard` →
     `http://localhost:8085/dev/login` → `/plans` → review → Approve.
   - `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/planimport apply --id N` —
     prints created task ids; `cat ~/plans/switchboard-followups.md` shows the
     stub; re-running propose on it is refused.
   - Run `orchestratord` briefly (or one drain) → `/tasks?project=switchboard`
     shows roots ready, dependents blocked; `/tasks/{id}` shows the tree edges;
     `/briefs` lists existing morning briefs; `curl -b cookies.txt
     'http://localhost:8085/export/tasks.csv?project=switchboard'` matches the
     board.
4. Not a commit gate: nothing pending externally — no OAuth, no exposure, no
   broker. This step is fully verifiable on the workstation.

## Decisions made unilaterally (rationale inline)

1. **Import trigger is a CLI (`cmd/planimport`), not a dashboard upload.** The
   stub replacement needs write access to the plan file's filesystem; the
   dashboard is headed for k8s where that path does not exist, and a web
   handler writing arbitrary local paths is a bad surface. The dashboard's
   role is exactly the review/approve gate.
2. **Apply runs from the CLI too (dashboard only approves/rejects).** Same
   filesystem reason — apply and stub-write belong in one command so a plan is
   never applied with its file left editable silently. Approve-in-dashboard /
   act-from-CLI mirrors the deliveries split (approve vs send). The
   `manual:{user}` actor passes the existing humanOnly gate; no matrix
   loosening.
3. **GPT provider, triage/drafts idiom (`PLAN_MODEL` default gpt-5-mini).**
   Per-worker provider config per CLAUDE.md; parse-to-schema is exactly the
   triage workload (free tokens, strict json_schema). Claude Code is for
   execution workers, not extraction.
4. **Proposals live in ai_extractions + a `plan_imports` gate row; NO tasks
   (not even `holding`) exist before approval.** Mirrors triage shadow
   discipline structurally — the propose path has no code path that writes
   tasks — and keeps reject truly free (nothing to garbage-collect).
   0004 already anticipated plan import as an extraction writer.
5. **`plan_order` = sibling array position, assigned in Go.** Determinism at
   the spine: the model orders the array (it read the document), Go turns
   order into numbers; get_next's `plan_order ASC NULLS LAST` does the rest.
6. **Imported tasks are `ready`, dependents blocked by R4 via emitted
   `dependency_added` events** — reuses the shipped rule + handler semantics
   instead of apply computing blocked itself; one source of truth for
   dependency gating, and the drain is already idempotent.
7. **Apply is one transactional handler doing its own inserts** (mirroring
   createTask/createChildTask SQL) rather than N executor create_task calls
   from the dashboard — a 30-task tree must not half-exist on failure; one
   audit row carries the whole apply, and `result` stores the ref→id map.
8. **Hash-mismatch at stub time = skip the overwrite, warn.** The approved
   tree was applied, but a file edited since propose was never reviewed —
   silently clobbering it would eat unreviewed content. The warning tells
   Salvador to reconcile by hand (re-propose the delta if real).
9. **Briefs surfaced by the `Morning brief %` title predicate** — it is the
   exact key R7 itself dedups on, so it cannot drift from the producer without
   the producer changing first. A `worker_type` marker would be cleaner but is
   R7's change to make, not the board's.
10. **Exports are CSV + JSON only**, same filters as the board, `id ASC`,
    pinned header — deterministic and diffable; anything richer (xlsx, per-plan
    export) waits for a real consumer.
11. **No agent-facing surface at all.** Workers discovering work already have
    create_child_task ("never plan-file edits" is already their prompt rule);
    exposing plan tools to agents would let a model propose work for itself —
    against the grain of the whole gate.

No open questions — the remaining ambiguities were all decidable from shipped
conventions; the deviations worth flagging are Decisions 1, 2, and 9.

## Future work (not this step)

- Board mutation actions (close, reprioritize, answer feedback inline) once
  read-only proves insufficient.
- In-dashboard tree editing before approval (if the reject→edit→re-propose
  loop is too slow in practice).
- Plan-file delta re-import (diff against the applied tree, propose only new
  nodes).
- Dashboard k8s packaging + Keycloak in front (ingress, session store beyond
  in-memory).
- Brief delivery beyond the board (email/MQTT push of the morning brief — a
  deliveries-path feature).
- `worker_type='brief'` marker on R7 tasks to replace the title predicate.
