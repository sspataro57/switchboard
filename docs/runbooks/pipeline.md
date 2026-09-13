# Pipeline wake-ups (SWT-40 Part E)

Only capture runs on cron. Everything downstream of it is event-driven:
- each connector's capture pass publishes a `captured` wake-up on MQTT;
- `pipelined`, a long-running Deployment, runs one loop per enabled stage;
- each stage is woken by its upstream events and by a 5-minute sweep.

Spec: `docs/tickets/inquiry-promote_SPEC.md`, Part E. Code: `internal/pipeline`, `cmd/pipelined`.

## The one rule: a wake-up, never work

Postgres is the queue of record. A wake carries no work: a stage woken for any reason re-queries its own SQL inbox, and only that query decides what it does.
- A lost message costs latency, up to one sweep.
- A duplicate costs one empty inbox query.
- The payload's `counts` and `max_id` are diagnostics; never branch on them.

Mosquitto has no ack or redelivery work-queue semantics. That is why nothing may depend on a wake arriving.

## Topics

Every `ops/pipeline/*` topic is QoS 1 and **never retained**: a retained wake would re-fire on every reconnect, and retained state is global on the production broker. `internal/pipeline/structure_test.go` scans the repo for a retained publish. The scan is lexical: it catches a topic that spells `ops/pipeline` or calls `pipeline.Topic` in the publish itself, not one passed through a variable outside `internal/pipeline`.

| topic | publisher | wakes |
|---|---|---|
| `ops/pipeline/captured` | each connector main, after a capture pass that committed ≥1 decision (the google IMAP IDLE loop included) | gate, route, inquiry |
| `ops/pipeline/gated` | gate stage, after a pass that resolved ≥1 hold | inquiry |
| `ops/pipeline/route_classified` | route stage | route_apply |
| `ops/pipeline/routed` | route_apply stage | inquiry |
| `ops/pipeline/inquiry_classified` | inquiry stage | inquiry_promote |
| `ops/pipeline/promoted` | inquiry_promote stage | nothing (the task boundary is the orchestrator's) |
| `ops/workers/pipeline.{stage}/status` | each stage: fleet heartbeat, **retained**, LWT `{"state":"dead"}` | fleetd mirror, you |
| `ops/workers/pipeline.daemon/status` | the `pipelined` process itself (heartbeat even with no stage) | fleetd mirror, you |

Payload: `{"event":"captured","source":"slackweb","counts":{…},"max_id":0,"ts":"…"}`.

The stage graph is the static table `pipeline.Subscribers`. Adding a consumer is a row there plus its test.

## Watching it

```bash
# wake-ups as they happen
mosquitto_sub -h 192.168.50.45 -t 'ops/pipeline/#' -v
# heartbeats: `+` must fill a whole topic level, so filter for the pipeline ids
mosquitto_sub -h 192.168.50.45 -t 'ops/workers/+/status' -v | grep 'workers/pipeline\.'
```

`pipelined`'s log has one `wake` line per wake-up on any `ops/pipeline` topic. It is the Part E smoke even with no stage enabled.

## Enabling a stage

`PIPELINE_STAGES` is a comma list, e.g. `gate,inquiry,inquiry_promote`.
- **Adding a name** turns the stage on at the next pod start. **Removing it** turns it off.
- **Empty** runs no stage: the daemon heartbeats and logs wake-ups.
- A name the build does not implement yet is refused at startup: `stage "x" is not implemented in this build`. It is never silently skipped.
- `DATABASE_URL` is needed as soon as any stage is on.

## Each stage's loop

- It runs one catch-up pass at start, so a restarted pod never sits on a backlog.
- After that, a pass runs on:
  - a wake (a burst coalesces to one pending run);
  - the sweep (`PipelineSweep`, 5 min; `--sweep` overrides);
  - a lock retry.
- A pass that fills its `--limit` repeats at once until one comes back short. At most `MaxDrainPasses` (50) in a row: an inbox that never empties costs one burst per wake or sweep, not a hot loop.
- A pass that runs longer than `PassTimeout` (15 min) is cancelled. One that ignores the cancellation for `PassWedgeGrace` (90 s) more makes `pipelined` exit non-zero, and the pod restarts. A stage pass must honour its context.
- If a stage's advisory lock is held elsewhere, it retries after 30 s and then on the next sweep. That is normal: the GPU stages share the classify lock with the classify CronJobs.
- A failing pass logs and waits for the next wake or sweep. A stage never crash-loops on a DB error.
- The heartbeat is `working` during a pass (republished every 60 s, even through long passes) and `idle` otherwise. It is published from its own goroutine, so a broker outage never stalls passes; the sweep keeps the pipeline moving with MQTT down.

## Downstream wakes

A stage's downstream wake is published by a wrapper around its pass, `pipeline.PublishAfterPass`, applied to every stage by `cmd/pipelined`'s `buildPass`. It is never published by the loop core in `stage.go`.
- A pass that reports `processed > 0` publishes ONE wake for its event (`pipeline.Downstream`), QoS 1, not retained, with `counts.processed`.
- A pass that moved nothing, lost its lock or failed before any row publishes nothing.
- A publish failure is logged and the pass's own result goes back to the loop unchanged: the sweep covers the lost wake.
- This is where the gate's `gated` wake comes from.

## The inquiry stages (SWT-40 Part C)

The Part C go-live value is `PIPELINE_STAGES=gate,inquiry,inquiry_promote`.

| stage | a pass | lock | woken by | publishes |
|---|---|---|---|---|
| `inquiry` | `classify.Run` on the inquiry lane, `--since 72h`, at most 25 messages. `processed` = verdicts written; a skipped message stays in the inbox and never counts | `0x5157_0022`, shared with the classify CronJobs | `captured`, `gated`, `routed`, sweep | `inquiry_classified` |
| `inquiry_promote` | `promote.Run` on the inquiry lane as `promote:inquiry`, at most 50 verdicts acted on. `processed` = verdicts acted on; a gated verdict never counts | `0x5157_0021`, shared with the classify-promote CronJob (personal lane) | `inquiry_classified`, sweep | `promoted` |

- **The sweep releases the grace.** A verdict on an ask younger than 1h is gated `pending` and writes nothing, and no event fires when the hour passes. The next sweep (5 min) or wake promotes it, so capture to Holding task is about 1h plus at most one sweep.
- **Nothing promotes until a project is armed.** `projects.inquiry_promote_after` NULL means off; the pass logs "inquiry promotion is off everywhere". Arming is a hand-run `UPDATE`, never a deploy step.
- **The inquiry stage needs `OPS_LOCAL_PROVIDER_URL` (an IP literal) and `OPS_LOCAL_MODEL`.** Unset, every message is skipped and recorded as a skip, never sent to a hosted model.
- **Nothing sends.** Promoted tasks are `holding` (O7) and `assignee_type=human`; neither stage creates a delivery. From the task on, the lifecycle is the orchestrator's (E-D2).
- **A non-human thread task gates the verdict.** If the thread's open or dismissed task is not `assignee_type=human`, the verdict is gated `claude_task`. It is never attached (no log on a worker's task) or reopened, and never shadowed by a second task (C-D13).
- **The locks are shared with CronJobs, so a run can fail on a collision.** pipelined takes `0x5157_0022` (the `inquiry` stage, GPU/classify) and `0x5157_0021` (`inquiry_promote`) on every sweep (5 min) and on every wake. The `classify run` CronJobs exit 1 when they lose `0x5157_0022`, and the classify-promote CronJob fails its run when it loses `0x5157_0021`. That run fails, and the next tick recovers. A stage that loses a lock retries after 30 s.
- **Run the hand backfill BEFORE enabling the `inquiry` stage.** `classify run --lane inquiry --since 336h` (SPEC V6.4.2) holds `0x5157_0022` for the whole backlog. With the stage already live, the two fight for the lock: the backfill exits 1 part-way whenever the stage holds it. Run it, read `classify report`, then add `inquiry` to `PIPELINE_STAGES`.

## The route stages (SWT-40 Part B)

The Part B value, in SHADOW, is `PIPELINE_STAGES=gate,route,route_apply,inquiry,inquiry_promote`.

| stage | a pass | lock | woken by | publishes |
|---|---|---|---|---|
| `route` | `classify.Run` on the route lane (`worker_type=classify_route`), `--since 168h`, at most 25 messages. `processed` = verdicts written; a skipped message stays in the inbox and never counts | `0x5157_0022`, shared with the `inquiry` stage and the classify CronJobs | `captured`, sweep | `route_classified` |
| `route_apply` | `capture.RunRouteApply`, window 720h, at most 500 messages. Writes one `mode='route'` `capture_decisions` row per routed message, directly (capture's own log: no task, no tool call). `processed` = rows written; an unrouted message (`pending_verdict`, `no_default`, `verdict_before_arming`, `candidate_revoked`) stays in the inbox and never counts | `0x5157_0015`, capture's own, shared with every connector's capture pass and the `gate` stage | `route_classified`, sweep | `routed` (wakes `inquiry`) |

- **Shadow until an account is armed.** The `route` stage classifies every live-unmatched message on an account with candidate rows (`opsctl route-candidates list`), armed or not. `route_apply` writes nothing for an account whose `source_accounts.route_after` is NULL. Arming is a hand-run `UPDATE` after the eval gate, never a deploy step (`docs/runbooks/local-classifier.md`, "Routing lane").
- **The `route` stage is local only.** It needs `OPS_LOCAL_PROVIDER_URL` (an IP literal) and `OPS_LOCAL_MODEL`, as the `inquiry` stage does. Unset, every message is skipped and recorded as a skip, never sent to a hosted model.
- **`route_apply` needs no model.** It reads the verdict the lane recorded (`fields.project_id`, the resolved candidate, and `fields.grounded`) and decides with the pure `capture.DecideRoute`.
- **Capture-lock collisions are expected.** `route_apply` takes `0x5157_0015` every sweep and on every `route_classified`. A connector capture pass that overlaps it logs "another pass holds advisory lock" and skips; its next tick (or the IMAP IDLE loop) recovers. A `route_apply` pass that loses the lock retries after 30 s.
- **The post-arming backfill competes for the GPU lock.** `classify run --lane route --since 720h` (SPEC V6.5) takes `0x5157_0022` for its whole run. If it exits at start with "another classify run holds the advisory lock", a stage pass holds it: rerun it. Once it holds the lock, the `route` and `inquiry` stages wait, retrying every sweep.
- **A route backlog drain can starve the `inquiry` stage.** `route` and `inquiry` share `0x5157_0022`. While a large route backlog drains (the post-arming backfill, or a first shadow pass over an account's history), a route pass holds the lock on most sweeps and wakes, and the `inquiry` stage keeps losing it: client asks wait behind routing until the backlog is gone. Watch the `inquiry pass` lines in the pipelined log during a drain. If asks are waiting, pause the drain (stop the hand backfill, or drop `route` from `PIPELINE_STAGES`) and let `inquiry` catch up. A structural fix is SPEC Future work.

## What a dead heartbeat means

A retained `{"state":"dead"}` on `ops/workers/pipeline.{stage}/status` or `pipeline.daemon` is the broker firing that client's last will: the pod was killed, OOMed, or lost its network without a clean disconnect.
- **Immediately:** nothing. Work is not lost: it sits in Postgres until a stage runs again.
- **Recovery:** check `kubectl -n ops get pods -l app=pipelined` and the pod's last log lines.
- A clean shutdown (a rollout, a `kubectl delete pod`) publishes `dead` too, deliberately: a clean DISCONNECT suppresses the will, so pipelined publishes it itself after its loops stop. `dead` therefore means "not running", however the process ended.

## Connectors

The connector CronJobs need `MQTT_BROKER` in their env.
- **Unset:** the publish is skipped with one log line, and the stages' sweep picks the work up within 5 min.
- **Broker down:** the connector logs a warning and exits normally; its decisions are already committed.
- Each connector connects only for its publish, with a distinct client id: `switchboard-capture-{connector}`.

## Scaling

Each stage is single-instance (replicas 1, strategy `Recreate`) and serialised by its advisory lock. A second replica would need `FOR UPDATE SKIP LOCKED` claims in each inbox (Future work).
