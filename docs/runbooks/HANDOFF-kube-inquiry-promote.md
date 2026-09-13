# Handoff to the kube session: SWT-40 inquiry-promote

**After this branch merges, migration 0029 must be applied to prod BEFORE any image built from main runs. A new capture binary on a db without 0029 fails the action CHECK on the first gated match and stalls capture for every connector.**

The switchboard session builds and pushes the image. The kube session owns the manifests in `kube/switchboard`. This file lists exactly what changes, part by part. Parts E, D and C are listed; B adds rows when it lands.

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

## Part C: the inquiry lane as pipeline stages

**Image:** `192.168.50.20:5000/switchboard:<tag>`. The tag is filled in when Part C merges. One step at a time:

1. **Migration first.** Apply `migrations/0031_inquiry_promotion.sql` to the `ops` db. It adds `projects.inquiry_promote_after TIMESTAMPTZ` (NULL, no default); old images never read it. 0030 belongs to SWT-45; the migrate runner applies whatever is pending in number order.
2. **Pin the classify-promote CronJob** to `classify promote --lane personal`. Personal is the default, so its behaviour and log line are byte-identical, but the pin keeps the inquiry lane off cron for good (E5).
3. **Wait for the hand backfill.** The switchboard session runs `classify run --lane inquiry --since 336h` once by hand (SPEC V6.4.2) and reads `classify report`. Do this BEFORE step 4: the backfill and the live `inquiry` stage both take `0x5157_0022`, and with the stage enabled they fight for it (the backfill exits 1 part-way whenever the stage holds the lock).
4. **pipelined: enable the two stages.**

| workload | kind | change |
|---|---|---|
| connector-*, classify-personal, classify-residue | CronJob (stays) | image bump only |
| classify-promote | CronJob (stays) | image bump; command `classify promote --lane personal` |
| **pipelined** | Deployment (Part E) | image bump; env `PIPELINE_STAGES=gate,inquiry,inquiry_promote`; add env `OPS_LOCAL_PROVIDER_URL=http://192.168.50.55:11434` and `OPS_LOCAL_MODEL=qwen3:8b` (the classify CronJobs' values; an IP literal, never a hostname) |

Without the two `OPS_LOCAL_*` variables the inquiry stage still runs, but every message is skipped (recorded as a skip; there is no hosted fallback) and pipelined logs a warning at startup.

**Expect occasional CronJob failures from lock collisions.** Once the stages are on, pipelined takes `0x5157_0022` (GPU/classify) and `0x5157_0021` (promote) every 5 minutes and on every wake. The classify-personal and classify-residue CronJobs exit 1 when they lose `0x5157_0022`, and classify-promote fails its run when it loses `0x5157_0021`. That run fails, and the next tick recovers. A failed Job from a collision is not an incident; a CronJob failing on every tick is.

**Arming is not a kube step.** The switchboard session sets task #110's provenance, then runs `UPDATE projects SET inquiry_promote_after = now() WHERE slug = 'collaboratory'` by hand (C-D11, V6.4). Until then the promote stage logs "inquiry promotion is off everywhere" and creates nothing.

## Part C smoke (V6 step 4)

1. `kubectl -n ops logs deploy/pipelined` shows `pipelined serving stages=[gate inquiry inquiry_promote]`, then `inquiry pass` and `inquiry_promote pass` lines after each `captured` wake and at least every 5 min.
2. `mosquitto_sub -h 192.168.50.45 -t 'ops/workers/+/status' -v | grep -E 'pipeline\.inquiry'` shows both stage heartbeats.
3. `mosquitto_sub -h 192.168.50.45 -t 'ops/pipeline/#' -v` shows `inquiry_classified` after a pass that wrote a verdict, and `promoted` after a pass that created or attached.
4. After arming: an ask in a Slack DM or a mail appears at `/tasks?project=collaboratory&status=holding` about an hour after capture (plus at most one 5-minute sweep). `deliveries` is unchanged.
