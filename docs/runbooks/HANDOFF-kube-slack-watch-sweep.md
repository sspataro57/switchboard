# Handoff to the kube session — Slack watch sweep (SWT-75)

**This one adds a resident workload. Salvador's go-ahead on the design comes first** (the kube
session said so, rightly): please take this to him before creating anything. The design in one
paragraph, for that conversation:

> A new `connector-slackweb-watch` Deployment (1 replica) runs `slackweb --watch`: every **minute**
> it asks the Mac mini's bridge for exactly the conversations in the `slack_watch` table (José's
> and Katie's DMs to start — ~18 s per read) and, every **30 minutes**, it runs today's full export.
> The two never overlap: one process, strictly sequential, so the mini's single browser sees one
> caller. Any bridge failure (503 busy, 500, a killed process) is a skipped pass, never a crash —
> restarting a pod cannot fix Chrome. The existing `connector-slackweb` CronJob stays as a
> **2-hourly safety net** and stands down by itself whenever the watcher is alive. Risk accepted
> and bounded: 2–4 extra navigations per minute on the mini; the levers are
> `SLACK_WATCH_INTERVAL=180s` or `=0` (targeted passes off, rotation only) — env only, no roll.

Spec: `docs/tickets/slack-watch-sweep_SPEC.md`. Image tag and digest are in the message that
accompanies this file (built from `main` after the merge).

## 0. Already done outside the cluster

- The leaf on the Mac mini is deployed (targeted `/export`, per-job queue estimate; bridge
  kickstarted 2026-09-22 12:15 EDT). An old switchboard sends no `targets`, so nothing changed
  for the CronJob.
- Migration **0041** (`slack_watch` table) — **applied to prod 2026-09-22 12:48 EDT** (verified:
  `schema_migrations` reads 0041). It had to precede the roll: the new dashboard reads the table on
  every `/sources` render and 500s the page without it.
- The watch rows are seeded by the switchboard session with `opsctl slack-watch add` (José
  `T0360B84U/DSAV4HJ2F`, Katie `T0HPR78RX/D04F7LXRB8B`) after 0041 is applied. The migration seeds
  nothing.

## 1. Roll the image tag to the 11 existing workloads as usual

Behaviour changes only in `connector-slackweb` (the stand-down probe at startup, the 503 send fix
in the dashboard's send path) — roll the same tag everywhere, keep the pins, check no Send is in
flight before replacing the dashboard pod.

## 2. The new Deployment: `connector-slackweb-watch`

Same image, command `["slackweb", "--watch"]`, **1 replica, `strategy: Recreate`**, no Service.

Env — **parity with the CronJob is a gate**, not advice: the same `DATABASE_URL`,
`SLACK_WEB_BRIDGE_URL=http://192.168.50.130:8787`, `SLACK_WEB_BRIDGE_TOKEN` (same secret),
`CAPTURE_RULES_MODE=live`, `CAPTURE_RULES_SINCE` (if the CronJob sets it), `MQTT_BROKER`, plus:

| env | value | meaning |
|---|---|---|
| `SLACK_WATCH_INTERVAL` | `60s` (default) | targeted cadence; `0` turns targeted passes off |
| `SLACK_ROTATION_INTERVAL` | `30m` (default) | full-export cadence |
| `SLACK_WATCH_BUDGET_MS` | `150000` (default) | the leaf's budget per targeted pass |
| `SLACK_BRIDGE_GRACE` | `120s` (default) | added to the budget for the Go context |
| `SLACK_WATCH_HEALTH_ADDR` | `:8093` (default) | liveness |

The watcher **refuses to start** without `SLACK_WEB_BRIDGE_URL` (the local CommandBridge would
run a full export every minute) and with `CAPTURE_RULES_MODE=live` under a `CAPTURE_RULES_SINCE`
below 2h. It prints one startup line:
`slack watch: interval=60s rotation=30m budget=150s targets=2 mode=live horizon=720h health=:8093`.

Probes: `livenessProbe` GET `/healthz` on **8093**, `initialDelaySeconds: 30`,
`periodSeconds: 30`, **`failureThreshold: 20`** (≈10 min). `/healthz` is 200 iff a pass
COMPLETED within 3 min AND the lock is held; a bridge answering 503 is a skipped pass and keeps
it green until that window lapses. A mini that is down for longer than ~13 min therefore does
restart the pod — deliberately rare, and harmless (it re-takes the lock and resumes); the
threshold is what keeps that from being a crash-loop. `terminationGracePeriodSeconds: 120` (an
in-flight pass finishes; SIGTERM exits 0).

Env parity check, both lists side by side:

```bash
kubectl -n ops get cronjob connector-slackweb -o jsonpath='{range .spec.jobTemplate.spec.template.spec.containers[0].env[*]}{.name}{"\n"}{end}' | sort > /tmp/cron.env
kubectl -n ops get deploy connector-slackweb-watch -o jsonpath='{range .spec.template.spec.containers[0].env[*]}{.name}{"\n"}{end}' | sort > /tmp/watch.env
diff /tmp/cron.env /tmp/watch.env   # only the SLACK_WATCH_* / SLACK_ROTATION_INTERVAL / SLACK_BRIDGE_GRACE lines may differ
```

Singleton: the pod takes advisory lock `0x5157_0011`. A second replica (a rolling overlap) logs
`standing by`, answers `/healthz` 503 "standby", and takes over within 15 s of the first dying —
do not treat "standby" as a crash.

## 3. Rollout order

1. Roll the tag (step 1).
2. Create the Deployment; wait for `/healthz` 200 and one `ingest_targeted:` line in its log.
3. Change `cronjob/connector-slackweb`'s schedule from `*/30 * * * *` to `0 */2 * * *`
   (`concurrencyPolicy: Forbid` stays). While the watcher lives, every tick logs
   `slack watch is live; skipping this pass` and exits 0.

## 4. Post-roll check

- Watcher log: one `ingest_targeted:` line per minute (most with `raw_inserted:0`); every ~30 min
  an `ingest:` line (the rotation) with the usual coverage block.
- A message in José's or Katie's DM appears in `normalized_messages` within ~2 minutes; the board
  shows it as a task or as activity on one.
- `/sources` has a "Slack watch" panel listing both rows.
- **24-hour gate (SPEC D10):** compare the mini's bridge restarts / stale-job kills / tab recycles
  per day with the baseline the leaf runbook records. If materially worse:
  `SLACK_WATCH_INTERVAL=180s`, or `=0`, on the Deployment. No image roll.

## 5. Rollback

Scale the Deployment to 0 and put the CronJob back on `*/30 * * * *`: today's behaviour returns on
the next tick, no data loss (same cursors, same raw rows). The table and the tools stay; a
disabled row (`opsctl slack-watch disable --id N`) is the mildest lever of all.
