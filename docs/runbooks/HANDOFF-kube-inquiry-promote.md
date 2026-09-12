# Handoff to the kube session: SWT-40 inquiry-promote

The switchboard session builds and pushes the image. The kube session owns the manifests in `kube/switchboard`. This file lists exactly what changes, part by part. Parts E and D are listed; C and B add rows when they land.

## Part E: the event pipeline (ready)

**Image:** `192.168.50.20:5000/switchboard:<tag>`. The tag is filled in when Part E merges. The image now also contains `/usr/local/bin/pipelined`.

| workload | kind | change |
|---|---|---|
| connector-google, connector-jira, connector-slackweb, connector-upworkcrm | CronJob (stays) | image bump; add env `MQTT_BROKER=tcp://192.168.50.45:1883` |
| connector-gcal | CronJob (stays) | image bump only (it runs no capture pass) |
| classify-personal, classify-residue, classify-promote | CronJob (stays until the follow-up ticket) | image bump only |
| **pipelined** | **Deployment (new)**, `replicas: 1`, `strategy: Recreate`, pod label `app: pipelined` | command `/usr/local/bin/pipelined`; env below; no ports, no Service; `terminationGracePeriodSeconds: 120` (a stage pass gets 90 s to honour its cancellation before pipelined gives up) |

`pipelined` env:
- `MQTT_BROKER=tcp://192.168.50.45:1883`
- `PIPELINE_STAGES=` (empty in Part E: no stage exists yet; D, C and B add names)
- `DATABASE_URL`, from the same secret the connectors use (unused until a stage is enabled, harmless now)
- Later parts add `OPS_TOKEN_KEY` (D) and `OPS_LOCAL_PROVIDER_URL`/`OPS_LOCAL_MODEL` (C, B).

Add no new CronJob.

## Part E smoke (V6 step 2)

1. `kubectl -n ops logs deploy/pipelined` shows `pipelined serving stages=[]`.
2. `mosquitto_sub -h 192.168.50.45 -t 'ops/workers/+/status' -v | grep pipeline.daemon` shows a retained `idle`, republished every 60 s.
3. After the next connector tick, `mosquitto_sub -h 192.168.50.45 -t 'ops/pipeline/#' -v` shows a `captured` wake. It fires only when the pass decided ≥1 message, and the pipelined log shows the matching `wake` line.
4. `kubectl -n ops delete pod -l app=pipelined` shows `{"state":"dead"}` on `ops/workers/pipeline.daemon/status` (pipelined publishes it on a clean stop; a crash gets the broker's LWT instead), then `idle` again once the new pod connects.

## Part D: the capture-time assignee gate

**Image:** `192.168.50.20:5000/switchboard:<tag>`. The tag is filled in when Part D merges. The order below matters, one step at a time.

1. **Migration first.** Apply `migrations/0029_capture_ticket_gate.sql` to the `ops` db (the usual migrate Job/command) BEFORE any new image runs. Before applying, confirm the constraint names on prod: `SELECT conname FROM pg_constraint WHERE conrelid='capture_decisions'::regclass AND contype='c'` must list `capture_decisions_action_check` and `capture_decisions_mode_check`. Old images are unaffected by 0029.
2. **Connector image bump.** Capture starts writing `held` for gated projects (reengine) instead of creating tasks. Until this bump, old capture binaries keep creating reengine tasks as they do today.
3. **pipelined: enable the gate.**

| workload | kind | change |
|---|---|---|
| connector-google, connector-jira, connector-slackweb, connector-upworkcrm | CronJob (stays) | image bump (step 2): capture writes `held` for gated jira-keyed matches |
| connector-gcal, classify-* | CronJob (stays) | image bump only |
| **pipelined** | Deployment (Part E) | image bump; env `PIPELINE_STAGES=gate`; add env `OPS_TOKEN_KEY`, from the same secret key connector-jira reads it from |

Without `OPS_TOKEN_KEY` the gate still runs, but it looks nothing up: every hold stays `pending_lookup` and, after 72h, resolves `attributed (gate_unverified_expired)`, with no task created. pipelined logs a `gate: OPS_TOKEN_KEY is not set` warning at startup when the key is missing.

## Part D smoke (V6 step 3)

1. `kubectl -n ops logs deploy/pipelined` shows `pipelined serving stages=[gate]` and a `gate pass` line after each `captured` wake (and at least every 5 min).
2. `mosquitto_sub -h 192.168.50.45 -t 'ops/workers/+/status' -v | grep pipeline.gate` shows the stage heartbeat.
3. After the next LHH mention: `opsctl capture-rules report` shows a `GATE` section with a `held` line and, within one wake, a `gate …` resolution. A task exists only if the ticket is assigned to Salvador.
4. The reconciler (connector-jira's `ticket_status:` line) runs as before; it is the backstop.
