> Jira: SWT-3

# 03-mqtt-heartbeats — MQTT heartbeat contract + minimal fleet view

## Source

Build-order step 3, quoted from CLAUDE.md:

> 3. MQTT heartbeat contract + minimal fleet view.

Constrained by the Workers section ("Heartbeats: retained MQTT on
`ops/workers/{client}/status` {state: idle|working|needs_feedback|manual,
task}. LWT publishes {state: dead}. Commands on `ops/workers/{client}/cmd`
(resume, pause, dispatch)"), the Stack section (Mosquitto `192.168.50.45:1883`,
WS `:9001`), and the `worker_heartbeats` table shipped in 0001.

## Goal

Ship the MQTT fleet contract as a small Go package (payload types, topic
builders, paho client wrapper with LWT wiring), a long-running mirror daemon
(`cmd/fleetd`) that upserts `ops/workers/+/status` into `worker_heartbeats`,
and an `opsctl fleet` view over that table.

**Usable alone means:** with `fleetd` running, any process that publishes a
conforming heartbeat (today: `mosquitto_pub`; step 4: the worker wrapper)
shows up live in `worker_heartbeats` via psql and in `opsctl fleet`, and a
worker that dies without a clean disconnect flips to `dead` via LWT — real
fleet observability before any worker exists. The payload types and topic
constants in `internal/fleet` ARE the contract steps 4 (wrapper heartbeats,
cmd consumption) and 5 (orchestrator resume cmd publish) import.

## The contract (normative — this section is what steps 4/5 build against)

Topics (constants + builder functions in `internal/fleet`):

- `ops/workers/{worker_id}/status` — worker → fleet. **Retained, QoS 1.**
- `ops/workers/{worker_id}/cmd` — orchestrator/human → worker.
  **Not retained, QoS 1** (now-or-never triggers; same reasoning as the CRM's
  crm/* topics — a retained cmd would re-fire on every reconnect).
- Mirror subscription filter: `ops/workers/+/status`.

`worker_id` is the single topic segment. For single-console clients (all of
step 4) `worker_id == client`, exactly CLAUDE.md's `{client}`. Multi-console
projects later use `{client}.{subproject}` — a dot, not a slash, so the
segment stays one MQTT topic level and the `+` wildcard keeps matching. The
mirror derives the `client` column as the segment up to the first `.`.

Status payload (JSON):

```json
{"state": "idle|working|needs_feedback|manual", "task_id": 123, "ts": "RFC3339"}
```

- `state` — required. Publish-side vocabulary is exactly the four states
  above; `dead` is reserved for the LWT and is never published live.
- `task_id` — optional; the claimed switchboard task id (bigint). Omitted when
  idle.
- `ts` — optional, publisher clock, informational only (skew debugging). The
  mirror stamps `last_seen = now()` at receive time regardless.

LWT payload, fixed at connect time, registered on the worker's own status
topic, **retained, QoS 1**:

```json
{"state": "dead"}
```

No `ts` (an LWT payload is frozen at connect; a timestamp there would lie).

Command payload (JSON):

```json
{"action": "resume|pause|dispatch", "args": {}}
```

- `action` — required, from the CLAUDE.md verb set (constants exported).
- `args` — optional object, action-specific (e.g. resume:
  `{"task_id": N, "feedback": "..."}` — the exact args schema per action is
  pinned by steps 4/5 when the consumers/producers land; this step pins only
  the envelope).

Cadence: publishers re-publish status at least every 60s
(`fleet.HeartbeatInterval` constant) even when state is unchanged. The fleet
view flags a row stale after 3× that. Retained status + LWT means restarts of
either side recover current fleet state with no replay machinery.

Empty (zero-length) retained payloads are the MQTT convention for clearing a
retained message — the mirror must silently skip them, never parse-error.

## Acceptance criteria

1. `go build ./...` and `go test ./...` pass. All new unit tests run with
   zero network and zero Postgres.
2. Unit tests cover the contract surface: status/command payload marshal +
   unmarshal round-trips (required/optional fields, unknown-state rejection on
   the strict path), topic construction and parsing (`worker_id` extracted from
   `ops/workers/X/status`; non-matching topics rejected), `client` derivation
   from `worker_id` (plain and dotted forms).
3. Unit tests cover mirror semantics against a fake store: normal status →
   full upsert (state, client, task_id, last_seen); status without `task_id` →
   column set NULL; `dead` → state + last_seen updated, prior `task_id` and
   `client` preserved; zero-length payload → skipped; malformed JSON /
   unknown state → logged, state stored verbatim (see Decisions), never a
   crash.
4. The client wrapper cannot connect without an LWT when opened in worker
   mode: the worker-facing constructor takes `worker_id` and always registers
   the retained `{"state":"dead"}` will on that worker's status topic. The
   mirror-facing constructor (fleetd) connects without a will.
5. `cmd/fleetd` runs against `MQTT_BROKER` + `DATABASE_URL`: subscribes
   `ops/workers/+/status` (subscription re-established in the OnConnect
   handler so paho auto-reconnect survives broker restarts), upserts
   `worker_heartbeats` keyed on the existing `worker_id UNIQUE`
   (`ON CONFLICT (worker_id) DO UPDATE`). Because status is retained, a fresh
   fleetd against an empty table rebuilds current fleet state from the broker
   alone.
6. A heartbeat carrying a `task_id` that does not exist in `tasks` does not
   kill fleetd: the FK violation is caught, the row is upserted with
   `task_id = NULL`, and a warning is logged.
7. Integration test (build tag `integration`, skips unless **both**
   `MQTT_BROKER` and `DATABASE_URL` are set) against the compose
   Mosquitto + Postgres: publish status → row appears in `worker_heartbeats`;
   publish changed state for the same worker → row updated, count unchanged;
   abrupt connection drop of a will-carrying client → retained
   `{"state":"dead"}` fires and the row shows `state='dead'` with `task_id`
   preserved (poll with deadline — LWT delivery is asynchronous). Rerunnable
   against a persistent db and broker: test-owned worker_id prefix, cleanup
   deletes its rows AND clears its retained messages (publish zero-length
   retained) first.
8. `opsctl fleet` prints one line per `worker_heartbeats` row: worker_id,
   client, state, task_id, last_seen age; rows older than 3× the heartbeat
   interval and not already `dead` are marked stale. Read-only SELECT; exits 0
   on an empty table with a "no workers" notice.
9. Smoke ("usable alone") against the REAL broker (192.168.50.45) and the real
   `ops` db, per the verification protocol: mosquitto_pub heartbeat → psql row
   → `opsctl fleet` shows it; kill -9 a will-carrying subscriber → `dead`
   visible in both; retained smoke messages cleared afterwards.
10. Nothing else moves: no `tasks` / `task_claims` writes, no executor tool
    registered, no migration applied (`schema_migrations` unchanged).

## Data model changes

**None.** `worker_heartbeats` from `migrations/0001_initial.sql:159-166`
(worker_id TEXT UNIQUE, client TEXT, state TEXT, task_id BIGINT FK tasks,
last_seen TIMESTAMPTZ) already carries everything this step writes. No 0003.
The state vocabulary is enforced in Go on the publish side, not by a CHECK —
see Decisions.

Rows written at runtime: `worker_heartbeats` upserts by fleetd only.

## API / MCP tool changes

**None.** No executor tools are registered and no MCP surface is added (MCP is
step 4). Where this stands relative to invariant 3:

- `fleetd` is trusted telemetry ingestion, the same standing as the step-2
  connector — a service writing its own bookkeeping table directly, not an
  agent-callable action. It imports nothing from `internal/executor`.
- `opsctl fleet` is a read-only SELECT over `worker_heartbeats` — a view, not
  an action. It does not go through `Executor.Execute` (the executor pipeline
  gates tool *actions*; reads are not actions). It writes nothing.
- `internal/fleet.PublishCommand` is a typed publish helper (the envelope for
  steps 4/5); no command *handler* exists anywhere this step, so no action can
  be triggered through it against anything.

## MQTT topics

| topic                            | dir            | payload                        | retained | QoS |
|----------------------------------|----------------|--------------------------------|----------|-----|
| `ops/workers/{worker_id}/status` | worker → fleet | `{state, task_id?, ts?}`       | yes      | 1   |
| same topic, via LWT              | broker → fleet | `{"state":"dead"}`             | yes      | 1   |
| `ops/workers/{worker_id}/cmd`    | ctl → worker   | `{action, args?}`              | no       | 1   |

fleetd subscribes `ops/workers/+/status` only. Nothing subscribes `cmd` this
step. Verified 2026-07-11: the broker is reachable from the workstation and
`ops/#` currently carries no retained messages — this step is the first writer
under `ops/`.

## Files likely to touch

Existing (verified in repo):

- `go.mod` — add `github.com/eclipse/paho.mqtt.golang` (first and only new
  runtime dep).
- `cmd/opsctl/main.go` — add the `fleet` subcommand next to
  `create-task`/`call`; it needs only `store.NewPool` + a SELECT, not the
  executor wiring in `run()`.
- `docker-compose.yml` — add an `eclipse-mosquitto:2` service, host port
  **1884** (1883 stays free for anything local), mounting the conf below;
  healthcheck so `--wait` covers it.
- `Makefile` — `integration` target additionally exports
  `MQTT_BROKER=tcp://localhost:1884`; `db-up` (which is `docker compose up -d
  --wait`) now starts both services with zero changes.
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` — record: compose Mosquitto port +
  `MQTT_BROKER` env contract under "Test infrastructure"; the
  `worker_id`/dotted-subproject convention and retained-clear gotcha under
  "Environment facts" (MQTT entry already exists there).

New:

- `docker/mosquitto.conf` — `listener 1883` + `allow_anonymous true`
  (Mosquitto 2.x binds localhost-only and refuses anonymous without a conf;
  this is required, not optional).
- `internal/fleet/contract.go` — topic constants + `StatusTopic(workerID)` /
  `CmdTopic(workerID)` / `ParseStatusTopic(topic) (workerID, error)` /
  `ClientFromWorkerID`, `Status` + `Command` payload types with
  marshal/validate, state + action constants, `HeartbeatInterval`.
- `internal/fleet/client.go` — thin wrapper over paho: connect options
  (broker URL, stable client id, keepalive, auto-reconnect, OnConnect
  resubscribe hook), worker-mode constructor (mandatory LWT), mirror-mode
  constructor, `PublishStatus`, `PublishCommand`, `SubscribeStatus(handler)`.
- `internal/fleet/mirror.go` — `Mirror` (pure message → upsert-params logic
  behind a small store interface) + pg store implementation
  (`internal/audit/pg.go` style: struct over `*pgxpool.Pool`, wrapped errors,
  context-first).
- `internal/fleet/contract_test.go`, `internal/fleet/mirror_test.go` — unit,
  fakes, zero network.
- `internal/fleet/integration_test.go` — build tag `integration`, env-gated
  (criterion 7).
- `cmd/fleetd/main.go` — env wiring, signal handling, run-forever loop.

## In scope

- The `internal/fleet` contract package (types, topics, client wrapper).
- `cmd/fleetd` mirror daemon.
- `opsctl fleet` CLI view.
- Compose Mosquitto + conf + Makefile/env contract for integration tests.
- Real-broker smoke including LWT demonstration and retained cleanup.

## Out of scope (do not bundle)

- **Step 4**: the worker wrapper, any real heartbeat *publisher* loop, cmd
  *consumption*, MCP task tools, task claiming (`FOR UPDATE SKIP LOCKED`),
  session capture. This step's only live publisher is the smoke test.
- **Step 5**: orchestrator publishing `resume` commands, claim expiry driven
  by heartbeat staleness, dead-worker → task-release rules. Staleness here is
  display-only in `opsctl fleet`.
- **Step 10**: the dashboard fleet page (HTMX). No HTTP surface this step.
- Deploy packaging (Dockerfile, k8s manifests, registry push) — fleetd runs
  from the workstation until the deploy step for long-running services; note
  it in Future work.
- MQTT auth/TLS — the LAN broker is anonymous today (matches the CRM
  ecosystem's usage); revisit at deploy time.
- Worker command handling of any kind; `ops/workers/+/cmd` has no subscriber.
- Heartbeats for fleetd itself or other spine services.

## Invariants that apply

- **2. One funnel** — no new tables; `worker_heartbeats` is bookkeeping
  *about* workers (explicitly in CLAUDE.md's schema), not a task-like table.
  Nothing actionable is minted from a heartbeat: fleetd never inserts into
  `tasks` (criterion 10). Dead-worker reaction (a decision) belongs to the
  step-5 orchestrator reading this table.
- **3. Everything through the executor** — no tool is added. Review checks:
  no `Register` calls, no `internal/executor` import from `internal/fleet` /
  `cmd/fleetd`, `opsctl fleet` performs SELECTs only, and no agent-facing
  surface can publish commands (the helper exists; no tool or handler exposes
  it).
- **7. Orchestrator purity (discipline transfer)** — no orchestrator here, but
  the mirror follows the same rule: message → upsert-params is a pure function
  (unit-testable, zero network, no LLM anywhere in this step); the paho and pg
  edges live behind interfaces so `go test ./...` stays offline. Heartbeat
  mirroring is telemetry, not a decision — no `audit_events` per message
  (same reasoning as step 2's `sync_runs`-not-audit_events call: 1440+ rows
  per worker per day would drown the tool-call trail).
- **1. Raw-first (explicit non-application)** — heartbeats are our own fleet
  telemetry, not captured work signals from a source; they do not pass through
  `raw_source_items`. The retained broker state itself is the replayable
  "raw": a fresh mirror rebuilds current state from it (criterion 5).

(4, 5, 6 have no surface: nothing leaves the LAN, nothing is client-visible,
nothing is authored.)

## Sibling patterns to copy

- **MQTT conventions**: `~/WebstormProjects/crm/src/lib/mqtt-publish.ts` — the
  only sibling talking to this exact broker. Copy its *decisions*, not its
  code (it's Node): QoS 1 everywhere, retain=false for trigger events with the
  rationale spelled out, lazy singleton connection, rely on the client's
  auto-reconnect. No Go sibling uses MQTT (verified: no paho in any
  `~/GolandProjects/*/go.mod`) — `internal/fleet` is greenfield Go.
- **Pg store style**: `internal/audit/pg.go` in this repo — small struct over
  `*pgxpool.Pool`, context-first, `fmt.Errorf("...: %w", err)`.
- **Env-gated integration tests**: `internal/executor/integration_test.go` and
  `internal/connector/upworkcrm/integration_test.go` — build tag +
  env-var skip + rerunnable cleanup in FK order with a test-owned prefix
  (INSTITUTIONAL_KNOWLEDGE "Test infrastructure" landmine).
- **CLI shape**: `cmd/opsctl/main.go` — subcommand switch + flag.NewFlagSet;
  `fleet` slots into the existing switch.
- **Upsert idiom**: `ON CONFLICT ... DO UPDATE` as used in
  `internal/connector/upworkcrm/ingest.go` (raw upserts) — same shape for the
  `worker_id` conflict target.

## Verification protocol

1. `go test ./...` — unit green with no broker and no db running.
2. `make integration` — compose brings up Postgres AND Mosquitto (`--wait` on
   both healthchecks); migrations apply (all skips — no 0003); the fleet
   integration test runs criteria 7 end to end against
   `tcp://localhost:1884`.
3. Manual local smoke (compose broker): run
   `DATABASE_URL=… MQTT_BROKER=tcp://localhost:1884 go run ./cmd/fleetd`, then
   `mosquitto_pub -h localhost -p 1884 -r -q 1 -t ops/workers/smoketest/status
   -m '{"state":"idle"}'` → `psql` shows the row → `go run ./cmd/opsctl fleet`
   shows `smoketest idle`.
4. Real smoke ("usable alone"), broker 192.168.50.45 + real `ops` db:
   - `MQTT_BROKER=tcp://192.168.50.45:1883 DATABASE_URL="$OPS_DATABASE_URL"
     go run ./cmd/fleetd` in one terminal.
   - Publish: `mosquitto_pub -h 192.168.50.45 -r -q 1 -t
     ops/workers/smoketest/status -m '{"state":"working"}'` → row visible via
     psql and `opsctl fleet`.
   - LWT: `mosquitto_sub -h 192.168.50.45 -t dummy --will-topic
     ops/workers/smoketest/status --will-payload '{"state":"dead"}'
     --will-retain --will-qos 1` in another terminal, then `kill -9` that
     mosquitto_sub (SIGKILL — a clean Ctrl-C disconnect suppresses the will) →
     within the keepalive window `opsctl fleet` shows `smoketest dead`.
   - Restart-recovery: stop fleetd, `psql -c "DELETE FROM worker_heartbeats
     WHERE worker_id = 'smoketest'"`, start fleetd → the retained message
     repopulates the row.
   - **Cleanup (mandatory — this is the production broker):**
     `mosquitto_pub -h 192.168.50.45 -r -n -t ops/workers/smoketest/status`
     (zero-length retained clears it), delete the smoketest row, confirm
     `mosquitto_sub -h 192.168.50.45 -t 'ops/#' -v` goes quiet.
5. Criterion 10 checks: `SELECT count(*) FROM tasks;` unchanged;
   `SELECT max(version) FROM schema_migrations;` unchanged.
6. Commit via the `/ticket-deliver` flow after review.

## Decisions made unilaterally (rationale attached; flag in review if wrong)

- **Package name `internal/fleet`, not `internal/mqtt`.** The contents are the
  fleet contract (worker states, heartbeat semantics, cmd verbs), not a
  generic MQTT utility; CLAUDE.md's own phrase is "fleet nervous system". Also
  avoids the import-alias dance a package named `mqtt` forces alongside the
  paho import.
- **Dependency: `github.com/eclipse/paho.mqtt.golang` (v1, MQTT 3.1.1)** over
  `eclipse/paho.golang` (v5). The contract needs only retained + LWT + QoS 1 —
  all 3.1.1 features; the v1 client is the mature, battle-tested one with
  built-in auto-reconnect. Known sharp edge baked into the spec: with clean
  sessions, subscriptions die on reconnect, so subscribing happens in the
  OnConnect handler (criterion 5).
- **`worker_id` is the topic segment; dotted `{client}.{subproject}` reserved
  for multi-console.** CLAUDE.md's topic literal is `{client}` and its worker
  model is one wrapper per client console; multi-console projects need a
  per-console identity eventually. A dot keeps it one topic level (the `+`
  filter and the mirror's parser need zero changes when step 4+ first uses
  it); the mirror's `client` derivation (prefix before first `.`) is the only
  code that ever looks inside. Payload stays exactly CLAUDE.md's shape — no
  redundant `client` field.
- **`ts` is optional and informational; `last_seen` is mirror receive time.**
  The broker doesn't timestamp; trusting publisher clocks would make staleness
  detection hostage to clock skew. LWT payload omits `ts` because it is frozen
  at connect time and would be a lie.
- **`dead` upsert preserves `task_id` and `client`.** "Worker died while
  holding task N" is exactly what step 5's recovery rules will want to read.
  All live states overwrite fully (idle legitimately NULLs task_id).
- **Strict publish, lenient consume.** Publish helpers validate state/action
  against the constants (a worker can't emit garbage through the library). The
  mirror stores an unknown state verbatim with a warning instead of dropping
  the message — a contract violation should be *visible* in the fleet view,
  not silently discarded telemetry. Consequence: no CHECK constraint on
  `worker_heartbeats.state`, hence no migration this step.
- **FK-violating `task_id` → retry upsert with NULL + warn** (criterion 6). A
  telemetry daemon must not crash-loop on one bad publisher; losing the bogus
  task ref is correct, losing the liveness signal is not.
- **Mirror is a standalone `cmd/fleetd`, not embedded in a dashboard
  process.** There is no dashboard until step 10, and "usable alone" here
  means psql shows live fleet state with nothing else running. fleetd is
  ~equivalent in standing to the step-2 connector: a small trusted spine
  service. Step 10's dashboard reads the table fleetd maintains.
- **Fleet view is `opsctl fleet` (CLI), not a throwaway HTTP page.** opsctl
  already exists, the step's verification surface is terminal-native, and an
  HTTP page now would be scaffolding deleted by step 10's real HTMX board.
  It reads the DB, not the broker, so it works when the broker is down and
  matches what the dashboard will do.
- **Stable fleetd client id (`switchboard-fleetd`), clean session.** A second
  fleetd instance kicks the first (MQTT same-client-id takeover) — with both
  writing the same table by upsert that's flapping, not corruption, and the
  stable id makes the daemon identifiable in mosquitto logs. Retained statuses
  make clean sessions free (full state replays on every subscribe).
- **Cadence 60s, stale at 3×.** CLAUDE.md doesn't pin numbers. 60s keepalive
  territory means LWT dead-detection lands within ~90s of a silent death,
  fast enough for a fleet a human glances at; the constants live in
  `internal/fleet` so steps 4/5 inherit them from the contract, not from
  folklore.
- **Compose Mosquitto on host port 1884 + `MQTT_BROKER` env contract.**
  Integration tests must not touch the production broker (retained messages
  are global state there). Env var name `MQTT_BROKER` (full URL,
  `tcp://host:port`) — gated the same way as `DATABASE_URL`: build tag AND
  skip-if-unset. Port 1884 avoids clashing with any local broker.
- **LWT integration test forces an abrupt drop via
  `SetCustomOpenConnectionFn`** (paho ≥1.4 lets the test hold the raw
  `net.Conn` and slam it shut, which is what makes the broker fire the will —
  a clean `Disconnect()` suppresses it). Fallback if that fights the test
  harness: spawn a helper process and SIGKILL it, same trick as the manual
  smoke. test-author picks; the criterion is the observable `dead` row, not
  the mechanism.

## Future work (not this SPEC)

- Step 4: worker wrapper publishes real heartbeats through
  `internal/fleet`; workers subscribe their own `cmd` topic.
- Step 5: orchestrator publishes `resume` commands; claim-expiry /
  dead-worker rules read `worker_heartbeats.last_seen` and `state='dead'`.
- Step 10: dashboard fleet panel over the same table (WS `:9001` exists if a
  live-updating view ever wants MQTT-over-websocket directly).
- Deploy packaging for fleetd (Dockerfile, k8s Deployment in `ops` namespace,
  image to 192.168.50.20:5000) when long-running services get their deploy
  step.
- Broker auth/TLS if the fleet ever leaves the LAN.
- fleetd self-observability (its own status topic) if the spine grows enough
  daemons to need it.

---

No open questions arose — CLAUDE.md pins the topic shapes, payload vocabulary,
and broker; the remaining choices were implementation-detail and are recorded
under "Decisions made unilaterally". This SPEC is ready for `test-author`.
