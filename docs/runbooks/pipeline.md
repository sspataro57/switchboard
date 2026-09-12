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
| `ops/pipeline/gated` | gate stage | inquiry |
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
