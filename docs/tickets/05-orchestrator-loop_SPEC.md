> Jira: SWT-5

# 05-orchestrator-loop — orchestrator event loop: dependencies, lifecycle rules, claim expiry, cron templates

## Source

Build-order step 5, quoted from CLAUDE.md:

> 5. Orchestrator event loop: dependencies, lifecycle rules (done_locally →
>    delivery task; needs_feedback → feedback task + resume cmd), claim
>    expiry, cron templates (morning brief).

Constrained by the Stack section ("Orchestration is a Postgres event loop +
cron ticker" — no LangGraph, no Temporal, no workflow engine), invariant 7
("Orchestrator is pure: rules are functions of (event, task, policy).
Unit-testable without any model. Every decision writes an audit row." The
orchestrator NEVER calls an LLM), the task status machine (`blocked ⇄ ready`
— "dependency resolution by orchestrator"), and the step-4 lifecycle contract
(event vocabulary, `resume` cmd args `{"task_id": N, "feedback_request_id": M}`,
`ClaimTTL = 2h` stamped in `internal/tools/claim.go` with enforcement
explicitly deferred to this step).

## Goal

Ship `cmd/orchestratord`: a long-running daemon that LISTENs on a new
`task_events` NOTIFY trigger plus a cron ticker, evaluates pure rules
(event, facts, config) → typed actions, and applies those actions exclusively
through `executor.Execute` and `fleet.PublishCommand` — closing the feedback
loop automatically, spawning delivery follow-up tasks, gating dependencies,
reaping expired claims, and emitting a daily morning-brief task.

**Usable alone means:** the manual glue left over from step 4 disappears.
With orchestratord running against the real ops db and broker: a worker that
parks on `request_feedback` gets a human "answer this" task in the funnel;
`opsctl answer-feedback` alone (no `--resume` flag) resumes the worker
automatically; `mark_done_local` surfaces a delivery follow-up task; a
crashed worker's claim is released back to `ready` within a sweep interval;
tasks with unmet dependencies sit `blocked` until their dependencies finish;
and a morning brief lands daily. All without any LLM call and without
touching a single task table outside the executor gate.

## Architecture (normative)

```
                       ┌──────────────────────────────────────────────┐
  migrations/0003      │  cmd/orchestratord                           │
  AFTER INSERT ON      │   pg_try_advisory_lock (single instance)     │
  task_events          │   LISTEN task_events ──┐                     │
  → pg_notify(id) ─────┼────────────────────────┤  wake-up only       │
                       │   60s ticker ──────────┤                     │
                       │                        ▼                     │
                       │   drain: task_events WHERE id > cursor       │
                       │          ORDER BY id (batch)                 │
                       │   per event: load Facts (read-only SQL)      │
                       │              rules(event, facts, cfg)        │
                       │              → []Action  (PURE, no I/O)      │
                       │   per tick:  expiry facts, brief facts       │
                       │              → []Action                      │
                       │   apply: executor.Execute (actor             │
                       │          "orchestrator") /                   │
                       │          fleet.PublishCommand(resume)        │
                       │   advance orchestrator_cursor                │
                       └──────────────────────────────────────────────┘
```

- **NOTIFY is a latency optimization, never a delivery mechanism.** The drain
  loop always reads `WHERE id > cursor ORDER BY id`; the ticker also drains.
  A lost or duplicated NOTIFY is therefore harmless by construction, and
  catch-up after downtime is the same code path as normal operation.
- **Purity boundary:** `internal/orchestrator/rules.go` contains only pure
  functions `func(ev Event, f Facts, cfg Config) []Action`. All SQL lives in
  the facts loader (read-only SELECTs), all mutation in the action applier
  (executor calls + MQTT publish). Rules import neither pgx nor paho nor any
  provider adapter.
- **At-least-once + idempotent actions:** cursor advances after an event's
  actions complete, so a crash mid-event replays it. Every rule is guarded by
  a dedup fact (see per-rule dedup below); replays are no-ops.
- **Single instance:** `pg_try_advisory_lock` on a constant key at startup;
  a second orchestratord logs and exits. (Same spirit as fleetd's stable
  client id, enforced at the db instead of the broker.)

## Rules shipped (the complete set — nothing else fires)

Vocabulary: "dep satisfied" = the depended-on task's status is one of
`done_locally | delivered | closed`.

### R1 — feedback task (`feedback_requested` event)

Create a HUMAN answer task in the parked task's project:
`create_task` equivalent via executor with `parent_id` = the parked task,
`assignee_type='human'`, title `Answer feedback #M on task #N`, body = the
question + the exact command to answer
(`opsctl answer-feedback --id M --answer "..."`). Then `record_orchestration`
on task N (payload `{rule:"feedback_task", trigger_event_id, feedback_request_id: M,
created_task_id}`).
**Dedup:** skip if an `orchestrated` event with `rule=feedback_task` and the
same `feedback_request_id` already exists (loaded in Facts).

### R2 — resume on answer (`feedback_answered` event)

1. Resolve the task's active (unreleased) claim → holder `worker_id`;
   `fleet.PublishCommand(worker_id, {action:"resume", args:{"task_id":N,
   "feedback_request_id":M}})` — the args schema pinned in step 4; the parked
   wrapper already consumes it (`internal/worker/loop.go` park path).
2. `task_close` the R1 answer task (found via R1's `orchestrated` payload in
   Facts), reason `feedback answered`.
3. `record_orchestration` (`rule:"feedback_resume"`, same dedup key shape).

Edge cases (pure branches, unit-tested): no active claim (worker died while
parked; LWT fired) → skip the publish, still close the answer task, record
the decision with `skipped:"no_active_claim"` — the task stays
`needs_feedback` for manual dispatch. No answer task found (feedback answered
before R1 ran, or created pre-orchestrator) → publish only.
**Dedup:** `orchestrated` `rule=feedback_resume` + `feedback_request_id`.
A duplicate resume publish is harmless anyway (the wrapper ignores resume
when not parked on that task), but the dedup keeps the record clean.

### R3 — delivery task (`done_local` event)

If `project.delivery != 'console'`: create a HUMAN task
`Deliver #N: <title>` (`parent_id` = N, same project), body = the
`done_local` summary payload + the project's delivery mode. `console` mode
skips (console-initiated delivery means the operator delivers as part of the
work). `record_orchestration` (`rule:"delivery_task"`).
The source task STAYS `done_locally` — the `done_locally → delivered`
transition belongs to step 8's real deliveries; until then completing the
human task is the delivery.
**Dedup:** `orchestrated` `rule=delivery_task` on task N (a task cannot
re-reach `done_locally` — release only covers claimed/in_progress/
needs_feedback — so task-scoped dedup is exact).

### R4 — block on unmet deps (`dependency_added` and `released` events)

If the task's status is `ready` and at least one dependency is unsatisfied →
`task_block`. Only `ready → blocked` (the status machine says `blocked ⇄
ready`; `holding` is triage's lane and stays untouched).
**Dedup:** status precondition — `task_block` only transitions from `ready`.

### R5 — unblock on satisfied deps (`done_local` events, and `status_changed` events whose payload `to ∈ {delivered, closed}`)

For each task that depends on the event's task, is `blocked`, and now has ALL
dependencies satisfied → `task_unblock` (→ `ready`; workers pick it up via
the untouched `task_get_next`).
**Dedup:** status precondition — `task_unblock` only transitions from
`blocked`; handler re-verifies all deps satisfied (defense in depth against
a dep added between snapshot and apply).

### R6 — claim expiry (tick)

Facts: `task_claims` rows with `released_at IS NULL AND expires_at < now()`
joined to tasks with `status IN ('claimed','in_progress')`. For each →
executor `task_release` with the HOLDER's `worker_id` read from the claim row
and reason `claim expired (orchestrator sweep)`. Same task returns to
`ready` — CLAUDE.md's retry shape, never a new task.
**Deliberately exempt:** `needs_feedback` tasks. A parked worker awaiting a
human answer is not a crashed worker; expiring its claim would orphan the
resume. (The 2h `ClaimTTL` from `internal/tools/claim.go` is inherited
unchanged — the sweep enforces, it does not redefine.)
**Dedup:** releasing sets `released_at`; the fact disappears. The `released`
task_event (reason carries the rule name) is the decision record; no separate
`orchestrated` row.

### R7 — morning brief (tick)

Enabled only when `ORCH_BRIEF_PROJECT` (project slug) is set. When local time
≥ `ORCH_BRIEF_HOUR` (default 7) and no task titled `Morning brief YYYY-MM-DD`
exists in the brief project → `create_task` (human): body is a deterministic
SQL snapshot — per project: counts of ready / blocked / needs_feedback /
done_locally tasks + open feedback_requests, rendered by a Go template. No
LLM, ever (invariant 7). Template is hardcoded in
`internal/orchestrator/brief.go`; dashboard-configurable templates are
step-10 work.
**Dedup:** the dated title existence check (in Facts).

`orchestrated` events themselves match no rule — the engine reads them (to
advance the cursor and load dedup facts) but never fires on them; no loops.

## Decision audit (invariant 7's "every decision writes an audit row")

Two layers, both through existing machinery:

1. **Every action is an executor call** → `audit_events` + `policy_decisions`
   rows with actor `orchestrator`, for free, via the existing pipeline.
2. **The decision itself** is a `task_events` row, `event_type
   'orchestrated'`, written on the TRIGGERING task via a new spine-facing
   tool `record_orchestration` (payload: rule name, trigger_event_id,
   rule-specific keys, created/affected task ids, or `skipped` reason).
   Chosen over a bare audit_events write because (a) it is task-scoped, so
   `task_context` surfaces orchestrator decisions to workers and humans where
   they matter, (b) it doubles as the replay-dedup key, and (c) going through
   a registered tool keeps the "no task_events writes outside
   internal/tools" grep-check intact.

No-op rule evaluations (event matched no rule, or preconditions failed with
nothing to skip-record) are slog-debug only, not persisted: rules are pure
and deterministic, so any historical no-op is exactly reproducible by
replaying the event log through the rule set — persisting them would only
flood `task_events` with noise. R2/R1-style "considered but skipped for
reason X" cases DO persist (they carry information replay can't infer from
the event alone, e.g. claim state at evaluation time).

## Data model changes — migration `0003_orchestrator.sql`

1. **NOTIFY trigger on `task_events`:**
   ```sql
   CREATE FUNCTION task_events_notify() RETURNS trigger AS $$
   BEGIN PERFORM pg_notify('task_events', NEW.id::text); RETURN NEW; END
   $$ LANGUAGE plpgsql;
   CREATE TRIGGER task_events_notify AFTER INSERT ON task_events
     FOR EACH ROW EXECUTE FUNCTION task_events_notify();
   ```
   Payload is the event id ONLY — the engine re-reads the row. Keeps us
   miles under the 8000-byte notify limit and makes the payload untrusted
   bookkeeping, not data.
2. **`orchestrator_cursor`** — engine bookkeeping, not a task-like table
   (invariant 2 concerns tables that hold things-to-act-on; this holds one
   integer):
   ```sql
   CREATE TABLE orchestrator_cursor (
     name          TEXT PRIMARY KEY,
     last_event_id BIGINT NOT NULL DEFAULT 0,
     updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
   );
   INSERT INTO orchestrator_cursor (name, last_event_id)
     VALUES ('orchestrator', COALESCE((SELECT max(id) FROM task_events), 0));
   ```
   Seeding at **current max(id)**, not 0: first deploy must NOT replay the
   step-4 smoke history (it would spawn delivery tasks for months-old
   done_local events). Catch-up covers downtime from this point forward.

No changes to `tasks`, `task_claims`, `task_dependencies`,
`feedback_requests` — the 0001 columns carry everything (verified: `blocked`
and `closed` already in the status CHECK; `expires_at`/`released_at` exist).

Event-type vocabulary additions (rows only, no schema): `dependency_added`
(on the dependent task, payload `{depends_on_task_id}`) and `orchestrated`
(above). `status_changed` payloads from the new tools carry
`{from, to, rule?, reason?}`.

## API / MCP tool changes

Five new executor tools, ALL spine-facing (registered in `tools.Register`,
absent from `internal/mcpserver/schemas.go`'s `agentTools` — that file does
not change; agents must not gate their own dependencies or close tasks).
Reachable manually via the existing `opsctl call <tool> '<json>'`.

- **`task_add_dependency`** `{task_id, depends_on_task_id}` — validate both
  exist and differ; `INSERT ... ON CONFLICT DO NOTHING` into
  `task_dependencies`; when actually inserted, `task_events` row
  `dependency_added` on `task_id`. Returns `{added: bool}`. No cycle
  detection this step (a cycle = two visibly-blocked tasks; see Future work).
- **`task_block`** `{task_id, reason?}` — `ready → blocked` only; handler
  re-verifies at least one unsatisfied dependency exists (defense in depth —
  the rule decides WHEN, the handler guarantees the transition is ever
  valid); `status_changed` event `{from:"ready", to:"blocked", ...}`.
- **`task_unblock`** `{task_id, reason?}` — `blocked → ready` only; handler
  re-verifies ALL deps satisfied; `status_changed` event.
- **`task_close`** `{task_id, reason}` — → `closed` from
  `holding | ready | blocked | done_locally | delivered`; refuses from
  `claimed | in_progress | needs_feedback` (never close work out from under
  a holder). `status_changed` event. Needed so R2 can retire answer tasks;
  also the missing terminal verb for any administrative task.
- **`record_orchestration`** `{task_id, rule, trigger_event_id, payload}` —
  inserts the `orchestrated` task_event. Validation: non-empty rule, task
  exists.

Task-creating actions (R1/R3/R7) reuse the existing `create_task` handler
path — `createtask.go`'s insert gains an internal `parent_id` route shared
with `create_child_task` (extend, don't duplicate; both already live in
`internal/tools`).

New binary: `cmd/orchestratord` — env `DATABASE_URL` (required),
`MQTT_BROKER` (required), `ORCH_BRIEF_PROJECT` (optional; unset disables R7),
`ORCH_BRIEF_HOUR` (default 7, process-local TZ), `--tick` (default 60s),
`--once` (single drain+tick pass, for smokes). Skeleton copies
`cmd/fleetd/main.go` (slog JSON, signal ctx, env wiring).

`internal/fleet/client.go` gains `NewSpineClient(ctx, brokerURL, clientID)`
(no will, command publisher). **Do NOT reuse `NewMirrorClient`** — it
hardcodes client id `switchboard-fleetd`; a second connection with the same
id would kick fleetd off the broker. orchestratord connects as
`switchboard-orchestratord`.

`opsctl answer-feedback --resume` keeps working as a manual fallback (a
duplicate resume is harmless), but the demo and docs drop the flag — the
orchestrator now owns the publish.

`dispatch` cmd semantics stay STUBBED (step-4 status quo): the orchestrator
publishes only `resume`. Nothing in this step's rules needs to push work at
an idle worker — workers pull via `task_get_next`. Defining dispatch now
would be speculation.

## MQTT topics

No new topics; step 3's frozen contract.

| topic                        | this step's use                                             | retained | QoS |
|------------------------------|-------------------------------------------------------------|----------|-----|
| `ops/workers/{worker_id}/cmd`| orchestratord publishes `resume` `{"task_id":N,"feedback_request_id":M}` to the parked claim holder | no | 1 |

orchestratord publishes no status/heartbeat (it is spine, not fleet; its
liveness shows in logs and the advancing cursor — fleet-view presence is
Future work if wanted).

## Files likely to touch

Existing (verified in repo):

- `internal/tools/createtask.go` — `Register` grows by five; `createTask`
  insert gains the internal parent_id route (shared with
  `createChildTask` in `childtask.go`).
- `internal/tools/helpers.go` — event-vocabulary comment gains
  `dependency_added`, `orchestrated`.
- `internal/fleet/client.go` — `NewSpineClient`.
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` — record: cursor-seeding rationale,
  advisory-lock key, orchestrated-event dedup idiom, needs_feedback expiry
  exemption, NewMirrorClient client-id landmine.

New:

- `migrations/0003_orchestrator.sql` — trigger + cursor (above).
- `internal/tools/dependency.go` — task_add_dependency, task_block,
  task_unblock (mirrors `donelocal.go`'s parse/validate/handler shape).
- `internal/tools/close.go` — task_close, record_orchestration.
- `internal/orchestrator/` — `rules.go` (pure rules + Event/Facts/Config/
  Action types), `facts.go` (read-only loaders), `apply.go` (Action →
  executor.Execute / PublishCommand, behind small interfaces for tests),
  `engine.go` (LISTEN conn, drain loop, ticker, cursor, advisory lock),
  `brief.go` (counts SQL + template); `rules_test.go` (the invariant-7
  heart: table-driven, zero network), `engine_test.go` (fake executor/
  publisher), `integration_test.go`.
- `cmd/orchestratord/main.go`.

## Acceptance criteria

1. `go build ./...` and `go test ./...` pass. `internal/orchestrator/rules_test.go`
   imports no pgx, no paho, no net — pure table-driven tests over
   (event, facts, config) → actions covering ALL rules including no-op and
   skip branches: R1 dedup skip; R2 no-active-claim and no-answer-task
   branches; R3 console skip + dedup; R4 met-deps no-op and holding no-op;
   R5 partial-deps no-op; R6 needs_feedback exemption and unexpired no-op;
   R7 disabled / before-hour / already-created no-ops. This is invariant 7's
   proof.
2. Migration 0003 applies cleanly via `make migrate` on the compose db, and
   on a db that already has task_events rows the cursor seeds at
   `max(task_events.id)` (verified in an integration test that inserts
   events before applying the seed logic — replay of pre-migration history
   is impossible).
3. Integration (tagged, env-gated as per test-infra conventions): a raw
   LISTEN connection receives a `task_events` notification whose payload is
   the inserted event's id.
4. The five new tools are reachable ONLY via `Executor.Execute`; every call
   audited by the existing pipeline; `internal/mcpserver/schemas.go`
   unchanged (adapter test still pins the step-4 agent-facing list).
   Grep-check holds: no task-table writes outside `internal/tools`; the
   engine's direct SQL is read-only facts + `orchestrator_cursor` bookkeeping.
5. Integration, feedback loop (compose db + compose broker): seed project +
   claude task, claim, `request_feedback` → engine drain creates the human
   answer task (correct parent, project, body contains the question) +
   `orchestrated` row; re-drain of the same event creates nothing;
   `answer_feedback` → a subscriber on the holder's cmd topic receives
   `resume` with exactly `{"task_id":N,"feedback_request_id":M}`, and the
   answer task is `closed`.
6. Integration, delivery rule: `done_local` on a `delivery='dashboard'`
   project yields one human `Deliver #N` task (replay: still one); on a
   `delivery='console'` project yields none.
7. Integration, dependencies: `task_add_dependency` on a `ready` task with
   an unmet dep → `blocked`; completing the dep (`mark_done_local`) →
   `ready`; with two deps the flip happens only after the second completes;
   `task_close` of a dep also unblocks (status_changed-to-closed path).
8. Integration, claim expiry: a claim backdated past `expires_at` on an
   `in_progress` task → tick sweep releases it (task `ready`, claim
   `released_at` set, `released` event reason names the sweep); an expired
   claim on a `needs_feedback` task is untouched.
9. Integration, catch-up + single instance: events inserted while no engine
   runs are processed exactly once on startup (cursor advanced); an
   immediate restart processes nothing new; a second engine against the same
   db fails to take the advisory lock and exits.
10. Morning brief: with a fixed clock/config in unit tests the rule fires
    exactly once per day; integration run creates one dated task with the
    deterministic counts body; second tick creates nothing.
11. Real smoke ("usable alone"), real ops db + real broker: seed a trivial
    task whose body instructs asking a question; `opsworker --client
    switchboard` parks `needs_feedback`; orchestratord (running throughout)
    has already created the answer task — visible via psql; `opsctl
    answer-feedback --id M --answer "..."` WITHOUT `--resume` → orchestrator
    publishes resume (watch `mosquitto_sub -t 'ops/#'`), worker resumes with
    the captured session and reaches `done_locally`; the `Deliver #N` task
    appears; the answer task is `closed`; audit_events rows exist for every
    orchestrator action with actor `orchestrator`. Cleanup: clear retained
    worker status; stop orchestratord (it may keep running — it is the
    product now).
12. Nothing else moves: no `deliveries` rows, no `ai_runs` rows (the
    orchestrator never calls an LLM — also enforced by imports: nothing
    under `internal/orchestrator` may import a provider adapter), no
    dashboard, no `dispatch` publisher, no get_next changes.

## In scope

- Migration 0003 (trigger + cursor).
- Five spine-facing executor tools (`task_add_dependency`, `task_block`,
  `task_unblock`, `task_close`, `record_orchestration`).
- `internal/orchestrator` (pure rules R1–R7, facts, applier, engine) +
  `cmd/orchestratord` + `fleet.NewSpineClient`.
- The real-smoke closed loop above.

## Out of scope (do not bundle)

- **Step 6 (GPT triage):** no automatic task creation from ingested content,
  no `find_related_tasks`, no shadow mode. The orchestrator's created tasks
  (answer/delivery/brief) are deterministic lifecycle artifacts, not triage.
- **Step 8 (deliveries):** no `deliveries` rows, no send adapters, no
  approve/edit flow. R3's output is a `tasks` row; `done_locally →
  delivered` stays untraversed.
- **Step 9 (Jira/GitHub, CI events):** no `pr_open`/`awaiting_ci`/
  `awaiting_merge` rules, no red-CI ×2 handling — only the rule SHAPE ships
  (release-to-ready-with-logs, exercised by R6), so step 9 adds a rule, not
  a mechanism.
- **Step 10 (dashboard, plan import):** brief template stays hardcoded; no
  board, no configurable cron templates.
- `dispatch` semantics; retry budgets / attempt counters; orchestrator
  heartbeat in the fleet view; dependency cycle detection.

## Invariants that apply

- **7. Orchestrator is pure — THE invariant of this step.** Rules are
  `func(Event, Facts, Config) []Action` with zero I/O; the package imports
  no provider adapter, no LLM client, no paho/pgx in `rules.go`. Every
  decision is recorded: actions via executor audit rows (actor
  `orchestrator`), the decision itself via `record_orchestration` /
  attributed `status_changed`/`released` payloads. Criterion 1 is the
  mechanical proof; criterion 12 the import-level check.
- **3. Everything through the executor.** The engine mutates task state ONLY
  via registered tools through `Executor.Execute` — including its own
  decision records. Direct SQL is confined to read-only facts loading and
  the `orchestrator_cursor` row (bookkeeping, same trusted-spine precedent
  as fleetd's `worker_heartbeats` writes). The five new tools are unexported
  handlers registered via `tools.Register`; none joins the MCP allowlist.
- **2. One funnel.** Answer tasks, delivery tasks, and briefs are rows in
  `tasks` — the funnel absorbs the orchestrator's output; no side table.
  `orchestrator_cursor` holds no work items. Queues remain filters:
  dependency gating flips `status`, it does not move rows.
- **4. Nothing external without a delivery row** — honored by absence: this
  step sends nothing external. The resume cmd is internal fleet plumbing on
  our own broker; R3 creates a task ABOUT delivering, never a send.
- **6. Stealth attribution** — no client-visible surface exists here; brief
  and answer-task bodies are internal. (Nothing to enforce, nothing
  violated.)
- (1 and 5 have no surface: nothing is ingested this step.)

## Sibling patterns to copy

- **Claim expiry shape:** `~/GolandProjects/job-agent/internal/queue/queue.go`
  `Reap` — the `WHERE status='claimed' AND claimed_at < now() - interval`
  sweep predicate transfers 1:1 to `expires_at < now() AND released_at IS
  NULL`. Ours deliberately differs in the apply: release goes through the
  `task_release` executor tool (audit + `released` event) instead of a bare
  UPDATE — invariant 3.
- **Daemon skeleton:** `cmd/fleetd/main.go` — slog JSON handler, signal
  NotifyContext, env validation, ticker + select loop, deferred
  pool/broker teardown.
- **Tool shape:** `internal/tools/donelocal.go` (status-precondition UPDATE
  with `RowsAffected()==0` → typed error, claim verification, event insert
  in the same tx) — `task_block`/`task_unblock`/`task_close` are the same
  skeleton with different predicates.
- **Executor client assembly:** `cmd/opsctl/main.go` `run()` — pool →
  registry → `policy.NewStatic(reg.Names()...)` → `audit.NewPGStore` →
  Execute; orchestratord reuses it with actor `orchestrator`.
- **Fleet publish:** `internal/fleet/client.go` `PublishCommand` (strict
  verb marshal, not retained, QoS 1) — the applier calls it verbatim.
- **Integration hygiene:** `internal/tools/lifecycle_integration_test.go` +
  `internal/fleet/integration_test.go` — build tag + env gates
  (`DATABASE_URL`, `MQTT_BROKER`), test-owned slug prefix, FK-ordered
  cleanup FIRST (the 2026-07-11 rerun landmine), retained-message cleanup.

## Verification protocol

1. `go test ./...` — unit green, offline; confirm `rules_test.go` runs with
   `-count=1` in <1s (no hidden I/O).
2. `make integration` — compose Postgres (5433) + Mosquitto (1884);
   migration 0003 applies; criteria 3–10.
3. Manual NOTIFY smoke: `psql -h localhost -p 5433 -U ops -d ops` →
   `LISTEN task_events;` in one session, `INSERT INTO task_events ...` in
   another, observe the notification.
4. Real smoke (criterion 11): `psql -h 192.168.50.49 -U ops -d ops` to apply
   0003 to the real db (via the migrate tool), then run orchestratord +
   opsworker against `MQTT_BROKER=tcp://192.168.50.45:1883` /
   `DATABASE_URL=$OPS_DATABASE_URL`; watch `mosquitto_sub -h 192.168.50.45
   -t 'ops/#' -v`; verify via psql: answer task created and closed, resume
   published (worker resumed), `Deliver #N` task, `orchestrated` events,
   audit trail; `mosquitto_pub -h 192.168.50.45 -r -n -t
   ops/workers/switchboard/status` cleanup.
5. Invariant greps: no pgx/paho import in `rules.go`; no task-table writes
   outside `internal/tools`; `schemas.go` diff empty.
6. Commit via `/ticket-deliver` after review.

## Decisions made unilaterally (rationale attached; flag in review if wrong)

- **NOTIFY as wake-up only; the cursor drain is the sole delivery path.**
  Makes lost/duplicate notifications structurally harmless and gives
  downtime catch-up for free — one code path instead of two.
- **Cursor in a new `orchestrator_cursor` table**, seeded at current
  `max(task_events.id)` in the migration. It is bookkeeping (one integer),
  not a task-like table; seeding at max prevents first-deploy replay of
  step-4 history (delivery tasks for ancient done_local events).
- **Decision records = `orchestrated` task_events via a registered
  `record_orchestration` tool** (task-scoped, surfaces in task_context,
  doubles as replay-dedup key, keeps writes inside internal/tools) — plus
  the executor's audit_events on every action. No-op evaluations are
  slog-only: pure deterministic rules make historical no-ops reproducible
  from the event log; persisting them is noise. Considered-but-skipped
  outcomes (e.g. resume with no live claim) DO persist.
- **Rules R4/R5 evaluate on events, not continuously**: `dependency_added` +
  `released` for blocking, dependency-terminal events for unblocking. The
  only unguarded window is a `ready` task claimed in the milliseconds before
  the block lands — acceptable (small, reversible, audited); `task_get_next`
  stays dependency-ignorant as decided in step 4 (one owner of the rule).
- **`task_add_dependency` ships now** (spine-facing). Step 5's mandate says
  "dependencies" but no write path exists — psql-as-workflow would dodge the
  executor. No cycle detection: a cycle is two visibly-blocked tasks, cheap
  to spot, and detection is graph code this step doesn't need.
- **`task_close` ships now** (spine-facing, refuses to close out from under
  an active holder). R2 must retire answer tasks or the funnel accumulates
  dead rows; it is also the status machine's missing terminal verb.
- **Claim expiry exempts `needs_feedback`** — parked-awaiting-human is not
  crashed; expiring it orphans the resume path. Expiry releases via
  `task_release` using the holder's worker_id from the claim row (the claim
  row IS the authority on who holds it; no new release variant needed).
- **R3 skips `delivery='console'` projects**; `auto` and `dashboard` both
  produce a human task until step 8 exists (the payload records the mode so
  step 8 can refine the rule, not the mechanism). Source task stays
  `done_locally`.
- **R1 answer task and R3 delivery task get `parent_id` = the source task**
  — linkage shows in `task_context` children with zero new columns.
- **Morning brief is env-configured (`ORCH_BRIEF_PROJECT`, `ORCH_BRIEF_HOUR`)
  and hardcoded-templated.** Cron-template CONFIGURATION is step 10; the
  build-order asks for the template working, not a template system.
  Disabled-when-unset keeps compose/integration runs quiet.
- **Single instance via `pg_try_advisory_lock`** — dedup guards make double
  processing mostly harmless but racey; the lock makes it impossible for one
  line of code.
- **orchestratord connects as `switchboard-orchestratord` via a new
  `fleet.NewSpineClient`** — reusing `NewMirrorClient`'s hardcoded
  `switchboard-fleetd` id would disconnect fleetd (MQTT same-client-id
  takeover).
- **`dispatch` stays stubbed; the orchestrator publishes only `resume`.**
  Workers pull work; nothing in R1–R7 needs push. Defining dispatch without
  a consumer need would bind future steps to a guess.
- **orchestratord publishes no fleet heartbeat** — it is spine (like fleetd),
  not fleet; the heartbeat contract is for workers. Liveness = logs + the
  advancing cursor. Revisit if the step-3 fleet view wants spine services.

## Future work (not this SPEC)

- Step 8: R3 upgraded to create real `deliveries` per policy matrix;
  `done_locally → delivered` transition wired to sends.
- Step 9: CI-event rules (red ×2 → ready with logs) reusing R6's
  release-to-ready mechanism; pr_open/awaiting_* lifecycle.
- Step 10: dashboard-configurable cron templates; board views of blocked/
  orchestrated state.
- Dependency cycle detection; retry budget / attempt counter on repeated
  releases (expiry or failure) before parking a task for human attention.
- `dispatch` cmd semantics once a push-shaped need exists.
- Orchestrator presence in the fleet view (spine heartbeat topic), if
  operating it blind proves annoying.

---

No open questions arose — the ambiguities the step carries (decision-record
mechanism, cursor storage and seeding, expiry-vs-parked semantics, dependency
write path, delivery-task shape, brief configuration, dispatch) were all
resolvable from CLAUDE.md plus the shipped step 1–4 code and are recorded
under "Decisions made unilaterally". This SPEC is ready for `test-author`.
