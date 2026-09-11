> Jira: SWT-41

# orchestrator-deploy — run orchestratord for real, start from now, and make its absence visible

**STATUS: PROVISIONAL — four owner questions open** (`docs/tickets/orchestrator-deploy_OPEN_QUESTIONS.md`).
Q1–Q3 change only cutover steps and one env value, not code. Q4 changes only the wording of the
"Stage contract" section, which ships as text.

## Source

Ad-hoc. Salvador, verbatim, 2026-09-11:

> "that is ok but I don't like all that being cronjobs everything if waiting cron jobs. All that
> dance should be a Q on mqtt"
>
> "the only thing on cron shoul be capture. after that should dance through orchestration and mqtt"
>
> "I don't think we have orchestration"
>
> "yes spec it, start from now" (answering: on first deploy, start from the current event rather
> than replaying the backlog)

Prod evidence (coordinator, read-only, 2026-09-11):
- `orchestrator_cursor.last_event_id = 75`, `updated_at` 2026-07-12 01:01Z. There are 17
  `audit_events` rows with actor `orchestrator`, the last one 2026-07-12 01:01Z (that was the SWT-5
  `--once` smoke).
- **866 `task_events` sit past the cursor** (2026-09-07..11): log 794, status_changed 69,
  delivery_sent 1, delivery_confirmed 1, priority_changed 1.
- No Deployment or CronJob for orchestratord, fleetd or hooksd exists in
  `~/projects/personal/kube/switchboard`. No process runs on the workstation or on 192.168.50.30.
- July leftovers in project `switchboard`: #4–#6 `done_locally`; #10–13, 15, 16, 18–20 `blocked`;
  #8 "Deliver #6" `ready`.

## What exists today (verified in code, this session)

- **The image cannot run it.** The `Dockerfile` build line is `./cmd/connectors/...
  ./cmd/tools/migrate ./cmd/dashboard ./cmd/google-auth ./cmd/classify`. `cmd/orchestratord`,
  `cmd/fleetd`, `cmd/hooksd` and `cmd/opsctl` are absent, so `switchboard:0.7.7` has no
  `/usr/local/bin/orchestratord`. "Never deployed" was also "never buildable into the deploy
  artifact".
- **"First deploy seeds at max(id)" is no longer true.** Migration 0003 seeded the cursor once, at
  apply time (2026-07). `Engine.DrainOnce` reads the existing row and drains `WHERE id > cursor`.
  It has no re-seed and no "skip" path. **A plain deploy today replays all 866 events.** Step 05's
  line ~227 describes the migration, not the binary.
- **What a replay would actually do** (rules × today's facts): `log`, `priority_changed` and
  `delivery_confirmed` match no rule (`Evaluate` default, pinned by
  `TestEvaluate_CaptureEventsFireNothing`). `status_changed` fires R5 only when `to ∈
  {delivered, closed}` and the task has blocked dependents whose deps are now all satisfied. The one
  `delivery_sent` fires R8: parent `done_locally → delivered`, its Deliver task closed. So the
  replay is small but not zero. Start-from-now is the owner's decision; P3/P4 below surface exactly
  what it forgoes.
- `cmd/orchestratord/main.go`:
  - **env:** `DATABASE_URL` (required, via `store.NewPool`), `MQTT_BROKER` (required),
    `ORCH_BRIEF_PROJECT` (optional, unset = R7 off), `ORCH_BRIEF_HOUR` (default 7, process-local
    TZ);
  - **flags:** `--tick 60s`, `--once`;
  - **wiring:** it calls **no** `tools.Set*` seam, so the gmail, jira, slack and calendar senders
    stay nil and every send-shaped handler errors (`no … adapter wired`);
  - **broker:** it connects as `switchboard-orchestratord` via `fleet.NewSpineClient` (no will,
    publish only, no subscription).
- The tools the rules call: `create_task`, `record_orchestration`, `task_close`,
  `task_mark_delivered`, `task_block`, `task_unblock`, `task_release`, `task_pr_transition`,
  `task_append_log`. **None reads `OPS_TOKEN_KEY`** (there are zero references in
  `internal/tools`).
- `TryAdvisoryLock` holds key `0x5157_0005` on one pooled connection for the process lifetime and
  never checks that connection again. A CNPG switchover kills it silently, and the process keeps
  draining unlocked.
- `opsctl fleet` is the only fleet view (a `worker_heartbeats` SELECT). The dashboard has none, and
  `funnel-view_SPEC` explicitly left it out.

## Goal

Ship orchestratord as a single-instance, always-on Deployment in `ops`. It starts from the current
head of `task_events`, and both the dashboard and Kubernetes can tell when it is not running.

**Usable alone means:** with this deployed and nothing else changed, a task reaching
`done_locally` gets its `Deliver #N` task within seconds. A worker parked on `request_feedback` gets
an answer task, and answering it publishes `resume` over MQTT. Expired claims are released within a
minute, and dependency gating runs. If the process is missing, crashed or wedged, `/tasks` shows a
red line saying so. That is the silent two-month gap, made loud.

## Decisions (unilateral, rationale attached; flag in review if wrong)

**D1 — Start-from-now is a new executor tool, `orchestrator_cursor_advance`, run once by hand.**
- **Not a hand-typed `UPDATE`.** Step 05 shipped `task_add_dependency` for exactly this reason:
  "psql-as-workflow would dodge the executor". Moving the cursor is how a human decides to discard
  lifecycle events, so it gets an audit row, a policy decision and a guard.
- **Not a migration.** A migration fires whenever migrate runs, including against a live engine,
  and would collide with SWT-40's 0027/0028.
- **Not an orchestratord flag.** A flag makes skipping one typo away on every restart.

The tool's contract:
- **Args:** `{expect_last_event_id: int ≥ 0, reason: non-empty string}`. It sets `last_event_id` to
  the current `max(task_events.id)` **iff** the row's value equals `expect_last_event_id`
  (compare-and-set). A mismatch is refused, naming the actual value, so a second or stale run can
  never skip events that arrived after the first.
- **Refuses while an orchestratord is running:** inside its transaction it takes
  `pg_try_advisory_xact_lock(<orchestrator lock key>)`. Transaction and session advisory locks
  share one key space, so a held engine lock makes this return false → refuse ("stop orchestratord
  first"). Because the transaction holds the key, no engine can start mid-advance.
- **Output:** `{from, to, skipped_total, skipped_by_type: {event_type: n}}`, computed in the same
  transaction.
- **Gating:** `humanOnly` (`internal/policy`), NOT in `internal/mcpserver/schemas.go`. It is
  reachable via `opsctl call --tool orchestrator_cursor_advance --args '{…}'`.
- Only forward. Reversing is `expect` = current and a lower target, which this tool does not
  offer. Replaying history is a different, deliberate act (Future work).
- **Lock key, one spelling:** `internal/orchestrator/integration_test.go` imports `internal/tools`,
  so `internal/tools` must NOT import `internal/orchestrator` (cycle in the test build). Put the
  constant in a leaf both can import, or keep two constants pinned equal by a unit test. Never two
  unpinned literals.

**D2 — Least privilege for the pod.**
- **Env:** `DATABASE_URL` and `MQTT_BROKER` only, plus `ORCH_HEALTH_ADDR` (D5), plus `ORCH_BRIEF_*`
  only if Q3 says yes.
- **No secrets beyond the db:** no `OPS_TOKEN_KEY`, no Slack bridge, no Pipedream, no
  `OPS_LOCAL_*`.
- **Senders stay unwired:** main keeps calling no `tools.Set*`. That is the structural guarantee
  that this process cannot send, even if a future rule named a send verb (invariant 4).
- **Cluster access:** `automountServiceAccountToken: false`, because it never talks to the
  Kubernetes API.
- **Role:** the db role stays the shared `ops` (every switchboard binary uses it; a narrower role
  is Future work).

**D3 — The lock connection is checked every tick; losing it exits the process.**
`TryAdvisoryLock` returns a handle with `Alive(ctx) error` (a `SELECT 1` on the held connection).
The main loop calls it on each tick, and an error → `os.Exit(1)` after logging. Kubernetes restarts
the pod, and the new process re-takes the lock or exits on contention. This turns "CNPG switchover →
unlocked engine" into a restart.

**D4 — fleetd is NOT deployed here.**
- orchestratord needs nothing from it: R2 resolves the resume target from `task_claims`, not
  `worker_heartbeats`, and publishing needs only the broker.
- Nothing consumes `worker_heartbeats` for a decision, and no worker console runs in-cluster.
- Heartbeats are retained on the broker, so a later fleetd rebuilds current state on first connect.
  Deferring loses nothing.
- fleetd ships with the first always-on worker console or a dashboard fleet view, whichever comes
  first.

**D5 — Health is judged from OUTSIDE the process, because "not running" has no process to ask.**
- **The dashboard reads it from Postgres** (`orchestrator.Health(ctx, pool, now)`):
  - `running` = the orchestrator lock is held by some session, read from `pg_locks`
    (`locktype='advisory'`; for a bigint key `classid` = high 32 bits, `objid` = low 32 bits,
    `objsubid = 1`, all computed in Go from the one lock constant);
  - `backlog` = `count(*) FROM task_events WHERE id > cursor`;
  - `oldest_unprocessed_at` = `min(created_at)` of those rows;
  - `cursor_updated_at`.
- **Verdict** from the pure `HealthVerdict(running, backlog, oldest, now)`:
  - `not_running`: lock not held;
  - `stalled`: running, backlog > 0 and oldest unprocessed older than `HealthStallAfter = 5m`
    (five ticks);
  - `ok`: otherwise.
  - A package constant, not env (the `funnelDisplayStaleAfter` precedent: it gates nothing).
  - "Cursor age" alone is deliberately NOT the signal. With no events the cursor legitimately
    never moves.
- **Where it shows:**
  - `/funnel` gains an "Orchestrator" section: verdict, backlog, oldest unprocessed, cursor,
    head;
  - `/tasks` (the board) shows one red line when the verdict is not `ok`, because the board is
    where Salvador looks and `/funnel` is where he investigates.
  - Neither page performs a tool call. A failing health query renders inline and never breaks the
    board (funnel criterion 17's shape).
- **Kubernetes liveness:** orchestratord serves `GET /healthz` on `ORCH_HEALTH_ADDR` (default
  `:8091`; hooksd owns `:8090`). It returns 200 iff a tick iteration completed (successfully or
  not) within `3 × tick` AND the lock handle is `Alive`, else 503. This catches a wedged loop, which
  the dashboard would show only as `stalled` later. It carries no data and has no Service.
- **No push alert in this ticket.** No notification channel exists in the codebase, and building
  one is its own ticket (Future work).

**D6 — The MQTT stage contract ships as TEXT in this SPEC, not code** (see "Stage contract").
- It has no publisher and no subscriber in this ticket.
- Step 05 declined to define `dispatch` for the same reason: a contract with no consumer binds
  future steps to a guess.
- The first code lands with its first consumer, the "convert the classify CronJobs" follow-up.

**D7 — The one skipped `delivery_sent` and any missed R5 unblocks are surfaced, not silently
dropped.** P3/P4 list them before the advance. The recommended handling is to apply R8's effect by
hand (`task_mark_delivered` + `task_close` of its Deliver task) only if the parent is still
`done_locally`. R5 is covered by Q2.

## What each rule does the moment it runs (today's data, new events only)

| Rule | Trigger | Day-one effect |
|---|---|---|
| R1 feedback task | new `feedback_requested` | Only when a console or manual session calls `request_feedback`. None is running, so nothing until one does. |
| R2 resume | new `feedback_answered` | Publishes `resume` to the claim holder's `ops/workers/{id}/cmd` (not retained, so it is lost if no wrapper listens) and closes the R1 answer task. With no live claim it records `skipped:no_active_claim`. |
| R3 Deliver task | new `done_local` | Fires on `mark_done_local` or `pr_merged → done_locally`, for projects with `delivery ≠ console`. **#4–#6 never re-fire:** a task cannot re-reach `done_locally` by an event already past the cursor. |
| R8 delivery lifecycle | new `delivery_sent` | Marks the parent delivered and closes its Deliver task. This is live behaviour the dashboard and assisted tier have been missing since SWT-8, so expect it on the next real send. |
| R4 block | new `dependency_added` / `released` | Only plan import or `task_add_dependency` add deps. Quiet today. |
| R5 unblock | new `done_local`, or `status_changed` to delivered/closed | **This is the one that touches the July leftovers.** Closing a plan root (#9, #14, #17) AFTER cutover flips its blocked dependents to `ready`. If they are `assignee_type='claude'`, a `switchboard` console could then claim them. Q2. |
| R6 claim expiry | every tick, **current state** | Releases any unreleased claim past `expires_at` on a `claimed`/`in_progress` task on the FIRST tick. Start-from-now does not protect against it, because it reads state, not events. P2 lists them. `needs_feedback` stays exempt. |
| R7 morning brief | every tick, **current state** | Only if `ORCH_BRIEF_PROJECT` is set (Q3). Then one "Morning brief YYYY-MM-DD" human task per day, which nothing closes. Hour is container-local, i.e. UTC unless `TZ` is set. |
| R9–R11 PR/CI | new `pr_*` / `ci_*` | hooksd is undeployed and the github poller is not scheduled, so these are quiet. |

**The July leftovers** (project `switchboard`): #4, #5, #6 `done_locally` (smoke; #8 "Deliver #6"
is `ready`); #10–13, 15, 16, 18–20 `blocked` (plan import 1, "switchboard follow-ups", roots #9 /
#14 / #17). Without a new event, none of them triggers a rule. Stale test and plan data is the risk
only through R5 (a human close after cutover), R6 (a stale claim) and the morning brief's counts.
**Recommendation:** close the smoke leftovers and whichever plan tasks are dead **before** the
advance, so their `status_changed` events land before the new cursor and are never evaluated (Q1,
Q2).

## Acceptance criteria

1. The `Dockerfile` build line includes `./cmd/orchestratord`. A plain unit test
   (`cmd/orchestratord/dockerfile_test.go` or a repo-level structural test) reads the `Dockerfile`
   and fails if `./cmd/orchestratord` is missing from the `go build` line. It exists because this
   bug class is exactly "built, never shipped".
2. `orchestrator_cursor_advance` (D1), integration-tested on the compose db:
   - with the cursor at X < head and `expect=X` → cursor = head, and the output's
     `skipped_by_type` equals an independent `GROUP BY` over `(X, head]`;
   - a second call with `expect=X` → refused, naming the current value, cursor unchanged;
   - with the orchestrator lock held on another connection → refused, cursor unchanged;
   - an `audit_events` row exists (`tool='orchestrator_cursor_advance'`, status `ok`/`error`) plus
     a `policy_decisions` row;
   - validation: `reason` empty → refused, and `expect < 0` → refused.
3. Policy: `orchestrator_cursor_advance` is in `humanOnly`. The test enumerates the IK's actor
   shapes:
   - `dashboard:x`, `opsctl:x`, `manual:x` → allowed;
   - `orchestrator`, `drafts:gpt`, `promote:inquiry`, `mcp:switchboard`, `mcp:worker:x`, bare
     `worker:x` → denied.

   `internal/mcpserver/schemas.go` is unchanged, and its adapter test still pins the listed set.
4. **Start-from-now, end to end** (integration): seed events past a cursor, advance, then
   `DrainOnce` processes **0** events. Insert a new `done_local` on a `delivery='dashboard'` task,
   and `DrainOnce` processes exactly 1 and creates exactly one `Deliver #N` task. Mutation: skip the
   advance and the drain creates tasks for the seeded events (red).
5. Lock liveness (D3):
   - `Alive` returns nil while held;
   - after `pg_terminate_backend(<lock conn pid>)` on the compose db, `Alive` returns an error
     (integration);
   - the main loop's reaction (exit non-zero) is unit-tested through an injected exit func. No
     `os.Exit` inside a testable function.
6. `/healthz` (D5), a unit test with an injected clock and lock handle: 200 when the last tick is
   within `3 × tick` and `Alive` is nil; 503 on either failure. The body carries no data beyond
   `ok` / the failing reason.
7. `HealthVerdict` is pure, with a table test: lock not held → `not_running` (whatever the backlog);
   held + backlog 0 → `ok`; held + oldest unprocessed ≤ 5m → `ok`; > 5m → `stalled`; boundary
   inclusive at 5m.
8. `orchestrator.Health` loader (integration, **test the column, not the fixture**):
   - holding `pg_try_advisory_lock(<key>)` on a separate connection → `running=true`, released →
     `false`;
   - the backlog and oldest match inserted events.

   Mutations: replace the `pg_locks` predicate with a literal `true` and the not-held case goes red;
   compare `objid` against the full 64-bit key instead of the split and the held case goes red.
9. Dashboard:
   - `/funnel` renders the Orchestrator section, and `/tasks` renders the red line only when the
     verdict ≠ `ok` (integration, both states);
   - a failing health query renders an inline error on `/funnel` and nothing on `/tasks` (the board
     never breaks);
   - no new POST route, no `s.execute` call;
   - the funnel's existing sections and suites stay untouched and green.
10. `cmd/orchestratord` still calls no `tools.Set*` seam (a structural test scanning `main.go`,
    like `ops-mcp-user`'s). `internal/orchestrator` still imports no provider adapter (existing
    check).
11. `go test ./...` and `make integration` are green. No migration is added (the ledger is
    unchanged).
12. `docs/runbooks/HANDOFF-kube-orchestrator-deploy.md` exists with the hand-off list below, and
    `docs/runbooks/orchestrator.md` covers:
    - the cutover sequence (P1–P5);
    - how to read the health section;
    - when `orchestrator_cursor_advance` is and is not appropriate (never as a routine restart
      step; downtime catch-up is the default and correct behaviour);
    - the pre-cutover SQL.
13. The IK entries in "Notes for the IK" are written by the delivering session.

## Data model changes

**None.** No migration.
- `orchestrator_cursor` (0003) is written by the engine and, now, by the one humanOnly tool.
- Health reads `pg_locks`, `task_events` and `orchestrator_cursor`.
- SWT-40's 0027/0028 are unaffected.

## API / MCP tool changes

- **New executor tool** `orchestrator_cursor_advance {expect_last_event_id, reason}` →
  `{from, to, skipped_total, skipped_by_type}`. It is registered in the `internal/tools` register
  table (`createtask.go`) and runs through `executor.Execute`: validate → policy (`humanOnly`) →
  audit start → handler (one transaction: xact lock → CAS update → histogram) → audit complete. It
  is off MCP.
- **Changed binary** `cmd/orchestratord`: `ORCH_HEALTH_ADDR` (default `:8091`), `/healthz`, lock
  liveness. The rules, facts, apply, drain and `--once` are unchanged.
- **Changed** `internal/orchestrator/engine.go`: `TryAdvisoryLock` returns a lock handle
  (`Alive`, `Release`); new `health.go` (`Health`, `HealthVerdict`, `HealthStallAfter`).
- **Dashboard**: read-only additions only (D5).

## MQTT topics

| Topic | Use in this ticket | Retained | QoS |
|---|---|---|---|
| `ops/workers/{worker_id}/cmd` | orchestratord publishes `resume {"task_id":N,"feedback_request_id":M}` (R2, unchanged from step 05) | no | 1 |

Client id is `switchboard-orchestratord`, publish only, no will. No new topic is implemented. The
future contract is below, as text.

## Stage contract (normative text for the follow-up; no code in SWT-41)

Salvador's target: **only capture stays on cron; everything after it is woken, not scheduled.**
Q4 decides one sentence of this section (who rings the bell).

1. **Postgres stays the queue of record** (CLAUDE.md, decided). Every stage's inbox is already a
   SQL query with a NOT EXISTS dedup:
   - capture's pending filter;
   - the classify lanes' inboxes keyed on `worker_type`;
   - promote's claim table.

   **MQTT is the doorbell, never the letter.** No payload field is ever read as work.
2. **Topic:** `ops/pipeline/{stage}/done`. It is **not retained**: a retained doorbell re-rings on
   every reconnect, the same reason `cmd` is not retained. QoS 1. Payload
   `{"stage":"capture","at":"<RFC3339>","count":N}`, where `count` is advisory (logging only).
   Stage vocabulary: `capture`, `classify_personal`, `classify_inquiry`, `classify_route`,
   `promote`. The spellings live in `internal/fleet` as one set of constants when the code lands.
3. **Publisher:** a stage publishes after its transaction COMMITS, and only if it wrote something
   the next stage reads (count > 0). Connectors publish `capture` at the end of their capture pass.
   A publish failure is logged and never fails the pass: the sweep covers it.
4. **Consumer:** a long-running Deployment per stage group, subscribed to its upstream topics.
   - On a message it runs one pass, debounced, so ten rings during a pass mean one more pass.
   - It **also runs a pass on a sweep timer** (e.g. 15 min). That is what makes a lost message
     harmless, exactly as the cursor drain makes a lost NOTIFY harmless (step 05's rule, restated
     for MQTT).
   - Single instance per stage keeps using the stage's existing advisory lock (classify
     `0x5157_0022` shared by lanes, promote `0x5157_0021`).
   - Client id `switchboard-{stage}` via `fleet.NewSpineClient`, never the mirror id.
5. **Where the orchestrator sits:** it owns the TASK lifecycle (`task_events`, LISTEN/NOTIFY +
   cursor). The message pipeline owns message → task. They meet at `tasks`: promote and capture
   create tasks through the executor, and those tasks' later events reach the orchestrator on
   their own. Q4: whether the orchestrator also rings the pipeline's bells.
6. **GPU stages stay fail-closed:** a ring while the z4 is down skips exactly as a CronJob tick
   does today. The shared classify lock means rings serialize the lanes; they never overlap.

**In SWT-41:** this text and the IK pointer to it. **Out:** constants, publish helpers,
subscribers, connector changes, and converting any CronJob. SWT-40 is unchanged and ships its
CronJobs as written. Its "Scheduling and cost" table is the input to the conversion follow-up.

## Hand-off to the kube session (spec only; the kube repo is not edited here)

1. **Image:** a new tag built from `main` after this merges, e.g. `0.7.8` (the next free tag at
   build time), with `/usr/local/bin/orchestratord` present. Verify each entrypoint starts and
   fails on its own missing config (the SWT-18 handoff's check), and that orchestratord in
   particular exits with `MQTT_BROKER is not set`. Bump the dashboard to the same tag (it carries
   the health section). Bumping the rest is uniformity, the kube session's call.
2. **Migrations:** SWT-41 adds none. Before pushing, still compare prod `schema_migrations` against
   `ls migrations/` (IK landmine). If SWT-40 merged first, 0027/0028 must be applied before any
   workload runs the new tag.
3. **New manifest `kube/switchboard/orchestrator.yaml`**, a Deployment `orchestratord` in `ops`:
   - `replicas: 1`, `strategy: Recreate`. Two replicas would fight over the advisory lock, and
     RollingUpdate would start the new pod while the old one holds the lock.
   - `command: ["/usr/local/bin/orchestratord"]`, `automountServiceAccountToken: false`,
     `terminationGracePeriodSeconds: 30` (SIGTERM is handled).
   - env:
     - `DATABASE_URL` from `secret/switchboard-db`;
     - `MQTT_BROKER=tcp://192.168.50.45:1883`;
     - `ORCH_HEALTH_ADDR=:8091`;
     - `ORCH_BRIEF_PROJECT` / `ORCH_BRIEF_HOUR` / `TZ=America/New_York` **only if Q3 = yes**.
       Distroless `static` ships tzdata; confirm with a one-off run printing `time.Local`.
   - **No** `OPS_TOKEN_KEY`, Slack, Pipedream or `OPS_LOCAL_*` env (D2).
   - `livenessProbe: httpGet /healthz :8091`, `initialDelaySeconds: 20`, `periodSeconds: 30`,
     `failureThreshold: 3`. No readinessProbe, no Service (nothing calls it).
   - resources `requests {cpu: 10m, memory: 32Mi}`, `limits {cpu: 200m, memory: 128Mi}`, and the
     dashboard's securityContext (non-root, read-only root fs, drop ALL).
   - Header comment: single-instance by advisory lock `0x5157_0005`. An exit with "another
     orchestratord holds the advisory lock" right after a rollout is expected, not an incident. It
     clears within one restart.
4. **Order:** do not apply the Deployment until Salvador reports P4 done (the cursor advanced). The
   advance refuses while an orchestratord runs, and a Deployment applied first would replay the
   backlog.
5. Update `connectors.yaml`'s header comment and `README.md` ("orchestrator … NOT deployed yet") to
   match.
6. **Rollback:** scale to 0. The cursor stays wherever it was, and restarting later catches up
   (the default behaviour). Nothing to undo in the db.

## Files likely to touch

- `Dockerfile` — add `./cmd/orchestratord` to the build line.
- `cmd/orchestratord/main.go` — health server, lock liveness per tick, last-tick timestamp.
- `internal/orchestrator/engine.go` — lock handle; new `internal/orchestrator/health.go`.
- `internal/tools/orchestratorcursor.go` (new) and `internal/tools/createtask.go` (register table
  line).
- `internal/policy/matrix.go` — `humanOnly` entry.
- `internal/dashboard/funnel.go`, `board.go`, `templates/funnel.html`, `templates/tasks.html`.
- Tests:
  - `internal/orchestrator/health_test.go` (pure),
    `internal/orchestrator/health_integration_test.go`, additions to `integration_test.go`
    (criterion 4);
  - `internal/tools/orchestratorcursor_integration_test.go`, `internal/policy` matrix test;
  - `cmd/orchestratord/main_test.go` (healthz, exit func, no-seam scan), the Dockerfile test;
  - dashboard integration additions.
- `docs/runbooks/orchestrator.md` (new), `docs/runbooks/HANDOFF-kube-orchestrator-deploy.md`
  (new), `.claude/INSTITUTIONAL_KNOWLEDGE.md` (at delivery).

## In scope / Out of scope

**In scope:**
- the image fix;
- `orchestrator_cursor_advance`;
- lock liveness, `/healthz`, and the dashboard health section plus board line;
- the runbook and kube hand-off;
- the cutover (P1–P5) and the smoke;
- the stage contract as text.

**Out of scope (do not bundle):**
- **fleetd and hooksd deployment** (D4; hooksd also needs public exposure, SWT-9).
- **The stage contract's code** and **converting the classify CronJobs** to MQTT-woken consumers.
  That is the follow-up, and it needs its own SPEC.
- **SWT-40** (inquiry-promote) in any part. Its CronJobs ship as it specifies.
- New orchestrator rules, changes to R1–R11, `dispatch` semantics, and a `delivery_failed` rule
  (IK: failing after R8 is its own analysis).
- A push alert channel (email/Slack/HA). Replaying history, or a backward cursor move.
- A narrower db role, OIDC for the dashboard, and the drafts/triage workers' deployment.
- Build-order step 8/9 work that R3/R8/R9 would make more useful.

## Invariants that apply

1. **Raw-first:** no surface. Nothing is ingested.
2. **One funnel:** Deliver, answer and brief tasks are `tasks` rows. `orchestrator_cursor` is one
   integer of bookkeeping. Health is a read, not a table.
3. **Everything through the executor:**
   - the cursor advance is a registered, humanOnly, audited tool, not psql;
   - the engine's own mutations are unchanged (executor calls as `orchestrator`, plus its
     cursor bookkeeping);
   - health and dashboard additions are SELECT-only with no tool call.
4. **Nothing external without a delivery row:** orchestratord wires no sender seam and gets no
   `OPS_TOKEN_KEY`, so it structurally cannot send (criterion 10). Its only broker write is internal
   `resume`.
5. **Own-message loop closure:** untouched. R8 is how a confirmed send closes the task side of
   that loop, and it starts working for the first time.
6. **Stealth attribution:** no client-visible surface.
7. **Orchestrator purity:** `rules.go` is unchanged. `HealthVerdict` is pure and tested offline.
   Every action the deployed engine takes writes executor audit rows plus `orchestrated` records,
   as step 05 built. The cursor advance is itself an audited decision.

## Sibling patterns to copy

- Deployment shape: `kube/switchboard/dashboard.yaml` (Recreate, pinned tag, securityContext,
  probes).
- Handoff format: `docs/runbooks/HANDOFF-kube-swt18.md` (image digest, preconditions, "deliberately
  NOT in this handoff", rollback).
- Pure freshness verdict with injected now and inclusive boundary:
  `internal/dashboard/funnel.go` `funnelFreshness`. Per-section degrade: the funnel's criterion 17.
- humanOnly spine tool off MCP: `capture_rule_add` (`internal/tools/capturerules.go`) plus its
  `internal/policy` entry. Status-precondition handler shape: `internal/tools/donelocal.go`.
- Structural "no seam wired" scan: `cmd/ops-mcp-user`'s test. Daemon skeleton: `cmd/fleetd`,
  `cmd/hooksd` (`/healthz`).
- Queue claims (`FOR UPDATE SKIP LOCKED`, jobagent): not applicable. The advance is a single-row
  CAS under an advisory lock.

## Verification protocol

**V1.** `go test ./...` (offline; health verdict, healthz, Dockerfile scan, policy).
**V2.** `make integration` on the compose db (`localhost:5433`, broker `:1884`), `-p 1`. Never
against 192.168.50.49. Mutations from criteria 4 and 8: apply, see red, revert.
**V3. Pre-cutover, prod, read-only** (`BEGIN READ ONLY … ROLLBACK`; record results in the runbook,
freeze none in a test):
- **P1** `SELECT last_event_id, updated_at FROM orchestrator_cursor` → expect 75. Take `max(id)`
  and the `event_type` histogram past it, to compare with the tool's output later.
- **P2** Expired unreleased claims on `claimed`/`in_progress` tasks (R6's own predicate from
  `loadTickFacts`) → R6 releases these on tick one. List them.
- **P3** The `delivery_sent` event(s) past the cursor: task id, its status, and whether its
  Deliver task is open (D7).
- **P4** `blocked` tasks whose dependencies are ALL in `done_locally|delivered|closed` right now.
  These are R5 unblocks the replay would have done and start-from-now will not.
- **P5** `SELECT slug, delivery FROM projects WHERE slug IN ('smoke','switchboard')`, to pick the
  smoke project (needs `delivery ≠ console`).

**V4. Cutover, in order:**
1. Q1/Q2 closes through the dashboard or `task_close`, BEFORE the advance. D7's by-hand R8 if P3
   says so.
2. From the merged `main`: `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/opsctl call --tool
   orchestrator_cursor_advance --args '{"expect_last_event_id":75,"reason":"SWT-41 start from
   now"}'`. Its `skipped_by_type` must match P1 plus whatever arrived since. Paste the output into
   the runbook.
3. Kube session applies the Deployment. Then check:
   - `kubectl -n ops logs deploy/orchestratord` shows `orchestratord running`;
   - `/funnel` shows `ok`, backlog 0;
   - `/tasks` shows no red line.

**V5. Usable-alone smoke** (prod db + prod broker, orchestratord running in-cluster; actor
`opsctl:$USER`, worker id `swt41-smoke`):
1. `mosquitto_sub -h 192.168.50.45 -t 'ops/workers/swt41-smoke/cmd' -v` in one terminal.
2. `opsctl create-task --project <P5> --title "SWT-41 smoke" --assignee claude`, then
   `opsctl call --tool task_claim --args '{"task_id":N,"worker_id":"swt41-smoke"}'`.
3. `request_feedback` on N → within seconds an `Answer feedback #M on task #N` task exists (R1).
4. `opsctl answer-feedback --id M --answer ok` (no `--resume`) → the subscriber prints `resume
   {"task_id":N,"feedback_request_id":M}`. That proves the pod reaches the broker. The answer task
   is `closed` (R2).
5. `mark_done_local` on N as `swt41-smoke` → `Deliver #N: SWT-41 smoke` exists within seconds (R3).
6. Then check:
   - `audit_events` rows with actor `orchestrator` for each action;
   - the cursor ≥ the smoke's last event id;
   - `/funnel` `ok`.
7. **Stall check:** `kubectl -n ops scale deploy/orchestratord --replicas=0` → `/tasks` shows the
   red `not_running` line on the next load. Scale back to 1 → `ok`, and any events written
   meanwhile drain (catch-up).
8. Cleanup: `task_close` the Deliver task and N. Nothing retained was published (cmd is not
   retained).

**V6.** `/ticket-review orchestrator-deploy`, go-reviewer plus the adversarial pass. This diff adds
a human verb that discards lifecycle events and turns on the first always-on spine writer.

## Notes for the IK (written by the delivering session, not here)

- **LANDMINE: built is not deployed, and the Dockerfile build line is the deploy list.**
  orchestratord shipped in SWT-5 (2026-07-11), ran once as a `--once` smoke, and then did not run
  for two months. Nothing noticed: its binary was not even in the image, R8 (SWT-8), R9–R11 (SWT-9)
  and every Deliver task silently never happened in production, and 866 events queued. A daemon a
  ticket depends on must be in the `Dockerfile`, have a manifest, and have a health signal that is
  judged from outside the process, or the ticket is not delivered.
- **The cursor row outlives its seed.** 0003's "seed at max(id)" happened once. Any later first
  start drains from wherever the row is. Skipping is `orchestrator_cursor_advance` (humanOnly,
  CAS, refuses while running). Downtime catch-up is the default and the right behaviour, so do not
  advance as a routine restart step.
- **Health = `pg_locks` + backlog age, not cursor age** (an idle system never moves the cursor).
- **Stage contract (SWT-41 text, not yet code):** Postgres is the letter, MQTT the doorbell, and a
  sweep timer the fallback. Pointer to this SPEC's section.
- Update "Still not deployed" in Environment facts: orchestrator deployed (date, tag). fleetd,
  hooksd, triage and drafts are still not deployed.

## Coordination with in-flight work

- **SWT-40 (inquiry-promote):** no shared migration. Both touch `internal/policy` `humanOnly`
  (merge-level only) and the IK. Its promote passes write `log` and `status_changed` (via
  `task_reopen`) events that the live orchestrator drains as no-ops or R5 no-ops, and its `holding`
  and `ready` human tasks match no rule. Its CronJobs are unchanged by this ticket. The stage
  contract above is written to absorb them later without changing SWT-40's code.

## Future work

- **The stage-contract implementation:** constants and helpers in `internal/fleet`, connector
  publishes, and classify/promote as MQTT-woken Deployments with sweep fallback. It replaces the
  `classify-*` CronJobs.
- fleetd deployment plus a dashboard fleet view. A spine heartbeat topic (`ops/spine/{service}/status`,
  retained, LWT) shared by orchestratord and the future stage consumers.
- A push alert when the health verdict ≠ `ok` for longer than N minutes (needs a notification
  channel decision).
- A narrower db role per workload. A deliberate history-replay verb with a dry-run (Evaluate is
  pure, so a read-only "what would fire" report over a range is cheap).
- Closing old morning briefs automatically, if Q3 turns them on.
